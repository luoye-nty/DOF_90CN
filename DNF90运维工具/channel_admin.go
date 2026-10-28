package main

// DNF90 频道管理 —— 主程序侧封装（免费版/赞助版共用，无 build tag）。
//
// 本文件只做"拿服务端根目录 + 调 internal/channelstore + 写日志"，
// 具体的 channel_info.etc 解析/改写/保护规则全部在 internal/channelstore（带单元测试）。
// 管理工具经 /manage/v1/channels* 走 remote_tab.go 注入的同名 Bridge 方法，两端行为一致。
//
// 生效方式（UI 必须原样告诉操作者）：改动只落盘，需要重启 DNF90Server。
// 原因见 internal/channelstore 包注释：频道目录与客户端频道表在服务端启动时一次性构建并缓存。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dnf90ops/internal/channelstore"
	"dnf90ops/internal/serverdir"
)

// currentServerRoot 返回当前服务端根目录（优先界面输入框，其次配置）。
//
// 注意这里**不能**用 a.uiSafe 包一层：walk 的 Synchronize 是异步投递（把回调排进队列
// 并由消息循环稍后执行），包在里面读到的只会是零值，函数永远退化成"只用 cfg.ServerRoot"。
// 直接跨线程读控件虽然会被编组到 UI 线程，但会阻塞到处理完——与 main.refreshNow 里
// 对同一控件的读法一致，且本函数调用频率极低（配置读写/手工刷新），代价可忽略。
func (a *App) currentServerRoot() string {
	if a == nil {
		return ""
	}
	root := ""
	if a.rootEdit != nil {
		root = strings.TrimSpace(a.rootEdit.Text())
	}
	if root == "" && a.cfg != nil {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	return root
}

// channelsStatus 读取当前频道状态。
func (a *App) channelsStatus() (channelstore.Status, error) {
	root := a.currentServerRoot()
	if root == "" {
		return channelstore.Status{}, fmt.Errorf("未配置服务端根目录，无法定位 channel_info.etc")
	}
	status, err := channelstore.Load(root)
	if err != nil {
		return channelstore.Status{}, err
	}
	return status, nil
}

// channelsApply 批量开启/关闭频道（落盘）。
func (a *App) channelsApply(channelIDs []int, enabled bool) ([]int, channelstore.Status, error) {
	root := a.currentServerRoot()
	if root == "" {
		return nil, channelstore.Status{}, fmt.Errorf("未配置服务端根目录，无法定位 channel_info.etc")
	}
	changed, status, err := channelstore.Apply(root, channelIDs, enabled)
	if err != nil {
		return nil, status, err
	}
	return changed, status, nil
}

// chanHotReload 请求服务端热生效（不重启）。
//
// 失败**不代表**改动丢失：channel_info.etc 已经落盘，服务端下次启动照样会用新表。
// 失败只说明"现在没法不重启就生效"（服务端没起、版本旧、端口被占用等），
// 所以调用方应当把它当成"降级到重启路径"的信号，而不是把整次操作报成失败。
func (a *App) chanHotReload(ctx context.Context) (acctGMTChannelReload, error) {
	return a.acctGMTReloadChannels(ctx)
}

// chanHotReloadSummary 把热重载结果压成一句给运维看的话。
func chanHotReloadSummary(result acctGMTChannelReload) string {
	parts := []string{fmt.Sprintf("服务端已热生效（当前频道表 %d 个频道，在线角色 %d）",
		result.TotalChannels, result.OnlineCharacters)}
	if len(result.GamePortsOpened) > 0 {
		parts = append(parts, fmt.Sprintf("新开监听端口 %s", channelIDListText(result.GamePortsOpened)))
	}
	if len(result.GamePortsClosed) > 0 {
		parts = append(parts, fmt.Sprintf("已关监听端口 %s", channelIDListText(result.GamePortsClosed)))
	}
	parts = append(parts, "新登录/重开的玩家立刻看到新频道表；停在选频道界面的玩家重开列表或重新登录即可")
	return strings.Join(parts, "；")
}

// serverRunningForChannels 判断 DNF90Server 当前是否在跑（决定频道改动后要不要提示重启）。
func (a *App) serverRunningForChannels() bool {
	if a == nil {
		return false
	}
	if running, _ := a.pm.Status(); running {
		return true
	}
	root := a.currentServerRoot()
	if root == "" {
		return false
	}
	ins := serverdir.Inspect(root)
	return ins != nil && ins.Running
}

// restartServerForChannels 触发一次"停→启"，用于让频道改动生效。
// 复用「一键启动/停止」同一条链路（stopServerAll / startServerAll），不另起一套进程管理。
//
// 独占性由调用方负责：本函数被 a.chanRun 包进 runAsync，而 runAsync 在进入 job 之前
// 就已经把 busy 置为 true。所以这里**不能**再查 a.isBusy()——那样每次都会立刻
// 返回"已有任务进行中"，重启永远不执行（初版写错、静态自查时发现）。
func (a *App) restartServerForChannels() error {
	if err := a.stopServerAll(); err != nil {
		// 没在跑时 stopServerAll 会返回"未发现正在运行的 DNF90 服务端"，属于预期情况。
		if !strings.Contains(err.Error(), "未发现正在运行的") {
			return fmt.Errorf("停止服务端失败: %w", err)
		}
		a.logf("频道改动生效：服务端未在运行，跳过停止步骤")
	}
	// 给端口/文件句柄一点回收时间，避免立刻启动时撞占用。
	time.Sleep(1500 * time.Millisecond)
	if err := a.startServerAll(); err != nil {
		return fmt.Errorf("重新启动服务端失败: %w", err)
	}
	return nil
}

// channelRestartHint 生成"改完要不要重启"的提示语。
// 热重载可用时不再需要重启，所以这里的措辞以"热生效优先、重启兜底"为准。
func (a *App) channelRestartHint() string {
	if !a.serverRunningForChannels() {
		return "改动已落盘。服务端当前未运行，下次启动即生效。"
	}
	return "改动已落盘。点「关闭/开启」会自动请求服务端热生效（不重启）；" +
		"若服务端版本不支持热重载，再用「重启服务端生效」。"
}

// channelApplyAndLog 供自检与批量操作复用：执行一次开关并写日志。
func (a *App) channelApplyAndLog(ctx context.Context, ids []int, enabled bool) error {
	_ = ctx
	changed, status, err := a.channelsApply(ids, enabled)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		a.logf("频道操作无变化：%s 已经是目标状态", channelIDListText(ids))
		return nil
	}
	action := "关闭"
	if enabled {
		action = "开启"
	}
	a.logf("已%s频道 %s；当前启用 %d / 共 %d。%s",
		action, channelIDListText(changed), status.Enabled, status.Total, a.channelRestartHint())
	return nil
}

// channelIDListText 频道号列表转可读文本。
func channelIDListText(ids []int) string {
	if len(ids) == 0 {
		return "(空)"
	}
	parts := make([]string, 0, len(ids))
	for index, id := range ids {
		if index >= 20 {
			parts = append(parts, fmt.Sprintf("…等 %d 个", len(ids)))
			break
		}
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return strings.Join(parts, ",")
}
