package dnfbridge

// gm_channel_reload.go 提供"频道热重载"：不重启进程，重新读取 channel_info.etc
// 并把新的频道目录 + 客户端频道脚本换进内存，同时把游戏端口监听对账到新表。
//
// 为什么需要这个能力
//
//	频道目录（channelcatalog.Catalog）与下发给客户端的频道表（channelScript）原本
//	只在 Service.Start 里构建一次并缓存（loadChannelAssets），运行期没有任何 setter。
//	于是运维侧改完 channel_info.etc 必须重启 DNF90Server 才生效——重启会踢掉全部在线玩家，
//	而"关掉几个没人用的频道"这种小操作不值得整服重启。
//
// 热重载做的事（顺序有意为之）
//
//  1. 重新读文件、重建 catalog 与 script；任一步失败都直接返回，内存里的旧表**原样保留**
//     （宁可维持现状，也不能让一次坏改动把服务端搞成半死状态）。
//  2. 先校验新表仍然含有默认城镇频道（登录选频道的宿主），缺了直接拒绝。
//     引导频道由 Build90CNOnlineScript 自己强制，缺了在步骤 1 就会失败。
//  3. 先换内存里的目录与脚本，**再**对账监听。这个顺序保证：即使某个游戏端口因为被占用
//     而开不出来，新连进来的玩家看到的也已经是一致的频道表（不会出现"列表里有、端口没开完"
//     之外的更糟情况——反过来先对账监听就会出现一段"端口开着但表还是旧的"窗口）。
//  4. 对账游戏端口监听：新启用的频道开监听，被关闭的频道关监听。
//     关监听**不会**断开已建立的连接（net.Listener.Close 只影响 Accept），
//     所以频道里已经站着的玩家不会被踢；他们离开后该频道就进不去了。
//
// 生效范围（运维侧 UI 必须如实说明，不能让人以为"全服立刻刷新"）
//
//   - 新连入的登录/频道会话：立刻拿到新频道表（脚本与目录都是现读现发）。
//   - 已在线、停在选频道界面的玩家：客户端拿到目录后会本地缓存，需要重开频道列表
//     或重新登录才会刷新。服务端不主动推未请求的目录包——那会打乱客户端自身的
//     目录状态机，代价远大于收益。
//   - 已被关闭频道里的玩家：不受影响，直到他们自己离开该频道。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// GMChannelReloadResult 热重载结果（供 GM 工具/运维工具展示）。
type GMChannelReloadResult struct {
	Path             string    `json:"path"`
	TotalChannels    int       `json:"total_channels"`
	AddedChannels    []int     `json:"added_channels"`
	RemovedChannels  []int     `json:"removed_channels"`
	GamePortsOpened  []int     `json:"game_ports_opened"`
	GamePortsClosed  []int     `json:"game_ports_closed"`
	OnlineCharacters int       `json:"online_characters"`
	Warnings         []string  `json:"warnings,omitempty"`
	ReloadedAt       time.Time `json:"reloaded_at"`
}

// GMReloadChannels 重新读取 channel_info.etc 并热替换频道目录与客户端频道脚本。
//
// 与其它 GM 写操作一致：参数非法/环境不满足返回 ErrGMRequestInvalid 包装的错误，
// 调用方（GM 工具接口）据此回 400；失败时内存与监听都保持重载前的状态。
func (s *Service) GMReloadChannels(ctx context.Context) (GMChannelReloadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		return GMChannelReloadResult{}, ErrGMUnavailable
	}

	// 1) 重新构建（失败即回退，不动运行态）。
	catalog, script, err := s.loadChannelAssets()
	if err != nil {
		return GMChannelReloadResult{}, fmt.Errorf("%w: 重新读取频道表失败，已保留原表: %v", ErrGMRequestInvalid, err)
	}
	if catalog == nil {
		return GMChannelReloadResult{}, fmt.Errorf("%w: 频道表解析结果为空，已保留原表", ErrGMRequestInvalid)
	}
	// 2) 表能跑但玩家侧会受影响的状态：缺默认城镇频道 11（客户端那条"按记住的频道号找候选"
	//    的循环一个都命中不了，玩家停在选频道界面）、以及没有任何特殊区域频道（type 22）。
	//    默认只告警不拒绝——运维会主动只留少数频道甚至只留特殊区域频道；
	//    DNFBRIDGE_CHANNEL_TABLE_STRICT=1 恢复"缺 11 就拒绝热重载"的旧行为。
	if _, ok := catalog.Channel(dnfenum.DefaultGameChannelID); !ok && s.channelTableStrict() {
		return GMChannelReloadResult{}, fmt.Errorf(
			"%w: 新频道表缺少默认城镇频道 %d，拒绝热重载（已保留原表）",
			ErrGMRequestInvalid, dnfenum.DefaultGameChannelID)
	}
	preWarnings := currentChannelTableWarnings(catalog, !s.channelTableStrict())
	newPorts := catalog.GamePorts()
	if len(newPorts) == 0 {
		return GMChannelReloadResult{}, fmt.Errorf("%w: 新频道表没有任何游戏端口，拒绝热重载（已保留原表）", ErrGMRequestInvalid)
	}

	s.mu.Lock()
	oldCatalog := s.catalog
	runCtx := s.runCtx
	if s.cancel == nil || runCtx == nil {
		s.mu.Unlock()
		return GMChannelReloadResult{}, fmt.Errorf("%w: 服务未启动，无法热重载频道", ErrGMUnavailable)
	}
	// 3) 先换内存里的目录与脚本。
	s.catalog = catalog
	s.channelScript = append([]byte(nil), script...)
	s.mu.Unlock()

	oldPorts := []int(nil)
	if oldCatalog != nil {
		oldPorts = oldCatalog.GamePorts()
	}
	added, removed := diffChannelIDs(oldCatalog, catalog)

	// 4) 对账游戏端口监听。
	result := GMChannelReloadResult{
		Path:            s.options.channelInfoFile,
		TotalChannels:   len(catalog.Channels()),
		AddedChannels:   added,
		RemovedChannels: removed,
		ReloadedAt:      time.Now().UTC(),
		Warnings:        preWarnings,
	}
	for _, port := range oldPorts {
		if containsIntValue(newPorts, port) {
			continue
		}
		if err := s.closeGamePort(port); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("关闭已停用频道的游戏端口 %d 失败: %v", port, err))
			continue
		}
		result.GamePortsClosed = append(result.GamePortsClosed, port)
	}
	for _, port := range newPorts {
		if containsIntValue(oldPorts, port) {
			continue
		}
		if err := s.listenGamePort(runCtx, port); err != nil {
			// 端口开不出来不影响频道表已经生效：新玩家在选频道界面依然看得到这个频道，
			// 只是连进来会失败。把原因回报给运维侧，而不是让整次热重载失败。
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("启用频道的游戏端口 %d 监听失败（频道表已生效，但该频道连不进去）: %v", port, err))
			continue
		}
		result.GamePortsOpened = append(result.GamePortsOpened, port)
	}

	s.mu.Lock()
	result.OnlineCharacters = len(s.gameSessions)
	s.mu.Unlock()

	s.logInfo("dnfbridge channels hot-reloaded",
		"path", result.Path,
		"total_channels", result.TotalChannels,
		"added_channels", result.AddedChannels,
		"removed_channels", result.RemovedChannels,
		"game_ports_opened", result.GamePortsOpened,
		"game_ports_closed", result.GamePortsClosed,
		"online_characters", result.OnlineCharacters,
		"warnings", len(result.Warnings))
	for _, warning := range result.Warnings {
		s.logWarn("dnfbridge channel hot-reload warning", "warning", warning)
	}
	return result, nil
}

// diffChannelIDs 比较新旧目录的频道号集合，返回新增与被移除的频道号（升序）。
func diffChannelIDs(oldCatalog, newCatalog *channelcatalog.Catalog) (added, removed []int) {
	oldIDs := make(map[int]struct{})
	if oldCatalog != nil {
		for _, channel := range oldCatalog.Channels() {
			oldIDs[channel.ID] = struct{}{}
		}
	}
	newIDs := make(map[int]struct{})
	if newCatalog != nil {
		for _, channel := range newCatalog.Channels() {
			newIDs[channel.ID] = struct{}{}
		}
	}
	for id := range newIDs {
		if _, existed := oldIDs[id]; !existed {
			added = append(added, id)
		}
	}
	for id := range oldIDs {
		if _, still := newIDs[id]; !still {
			removed = append(removed, id)
		}
	}
	sort.Ints(added)
	sort.Ints(removed)
	return added, removed
}

func containsIntValue(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
