package dnfbridge

// gm_channel_reload_test.go 覆盖频道热重载：目录/脚本的热替换、保护性拒绝、
// 以及游戏端口监听的按频道开关对账。

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	appkit "longheng.io/server/internal/platform/app"
)

// writeChannelInfoFixture 写一份最小可用的 channel_info.etc。
// rows 是 `[server]` 段的原始行文本。
func writeChannelInfoFixture(t *testing.T, rows []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "channel_info.etc")
	body := "[dungeon]\r\n`[town]`\r\n<4::dname_1>\r\n1\r\n[/dungeon]\r\n" +
		"[server] 1\r\n" + strings.Join(rows, "\r\n") + "\r\n[/server]\r\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写频道档 fixture: %v", err)
	}
	return path
}

// channelRow 生成一条 `[server]` 段频道行（字段顺序与真实文件一致）。
func channelRow(id int, typeID int, group string) string {
	return fmt.Sprintf("   %d   `<chn_channel_info_%03d>`   %d   `[%s]`   0   0   ``", id, id, typeID, group)
}

// newChannelReloadTestService 造一个只加载了频道资源、没有真正开监听的服务实例。
func newChannelReloadTestService(t *testing.T, path string) *Service {
	t.Helper()
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{})
	service.options.channelInfoFile = path
	catalog, script, err := service.loadChannelAssets()
	if err != nil {
		t.Fatalf("loadChannelAssets: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.mu.Lock()
	service.catalog = catalog
	service.channelScript = append([]byte(nil), script...)
	service.runCtx = runCtx
	service.cancel = cancel
	service.mu.Unlock()
	return service
}

// TestGMReloadChannelsSwapsCatalogAndScript 频道号不变、只改频道类型时，
// 热重载必须换掉内存里的目录与客户端脚本，且不触碰任何监听。
func TestGMReloadChannelsSwapsCatalogAndScript(t *testing.T) {
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
		channelRow(50, 11, "granfloris"),
	})
	service := newChannelReloadTestService(t, path)

	before, ok := service.currentCatalog().Channel(50)
	if !ok || before.Type != 11 {
		t.Fatalf("fixture 频道 50 = %+v, want type 11", before)
	}
	beforeScript := string(service.currentChannelScript())

	// 只改 50 的类型：频道号集合完全不变 → 不应该有任何端口开/关。
	if err := os.WriteFile(path, []byte("[server] 1\r\n"+
		channelRow(11, 11, "granfloris")+"\r\n"+
		channelRow(19, 40, "crack")+"\r\n"+
		channelRow(50, 22, "ancient")+"\r\n"+
		"[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}

	result, err := service.GMReloadChannels(context.Background())
	if err != nil {
		t.Fatalf("GMReloadChannels: %v", err)
	}
	if len(result.AddedChannels) != 0 || len(result.RemovedChannels) != 0 {
		t.Fatalf("频道号集合没变却报告增删: +%v -%v", result.AddedChannels, result.RemovedChannels)
	}
	if len(result.GamePortsOpened) != 0 || len(result.GamePortsClosed) != 0 {
		t.Fatalf("频道号集合没变却动了监听: +%v -%v", result.GamePortsOpened, result.GamePortsClosed)
	}
	after, ok := service.currentCatalog().Channel(50)
	if !ok || after.Type != 22 || after.Group != "ancient" {
		t.Fatalf("热重载后频道 50 = %+v, want type 22 group ancient", after)
	}
	// 客户端下载的那份频道表也要跟着变（脚本里带类型），否则玩家看到的还是旧类型。
	if afterScript := string(service.currentChannelScript()); afterScript == beforeScript {
		t.Fatal("客户端频道脚本没有跟着热重载更新")
	}
	if !strings.Contains(string(service.currentChannelScript()), "`[ancient]`") {
		t.Fatalf("客户端频道脚本里没有新分组: %s", service.currentChannelScript())
	}
}

// TestGMReloadChannelsAllowsCatalogWithoutDefaultTownChannel 缺默认城镇频道 11 的表
// 默认只告警、照常重载：运维会主动只留特殊区域频道（2026-09-30 需求）。
// 告警仍然要出现在结果里，否则运维不知道有多少玩家会停在选频道界面。
func TestGMReloadChannelsAllowsCatalogWithoutDefaultTownChannel(t *testing.T) {
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
		channelRow(50, 11, "granfloris"),
	})
	service := newChannelReloadTestService(t, path)

	// 去掉 11，保留引导频道 19 与另一个频道。
	if err := os.WriteFile(path, []byte("[server] 1\r\n"+
		channelRow(19, 40, "crack")+"\r\n"+
		channelRow(50, 11, "granfloris")+"\r\n"+
		"[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}

	result, err := service.GMReloadChannels(context.Background())
	if err != nil {
		t.Fatalf("缺少默认城镇频道的表默认应被放行（只告警），却被拒绝: %v", err)
	}
	if _, ok := service.currentCatalog().Channel(11); ok {
		t.Fatal("频道 11 应从新表里消失")
	}
	if _, ok := service.currentCatalog().Channel(50); !ok {
		t.Fatal("新表里的 50 必须生效")
	}
	warned := false
	for _, warning := range result.Warnings {
		if strings.Contains(warning, "默认城镇频道 11") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("缺少默认城镇频道时必须留下告警，实际 warnings=%v", result.Warnings)
	}
}

// TestGMReloadChannelsRefusesCatalogWithoutDefaultTownChannelInStrictMode
// DNFBRIDGE_CHANNEL_TABLE_STRICT=1 时恢复旧行为：拒绝且**保留旧表**。
func TestGMReloadChannelsRefusesCatalogWithoutDefaultTownChannelInStrictMode(t *testing.T) {
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
		channelRow(50, 11, "granfloris"),
	})
	service := newChannelReloadTestService(t, path)
	service.options.channelTableStrict = true

	// 去掉 11，保留引导频道 19（引导频道在严格模式下由 Build90CNOnlineScript 强制，
	// 先去掉它会在解析阶段就失败，测不到"缺默认城镇频道"这条分支）。
	if err := os.WriteFile(path, []byte("[server] 1\r\n"+
		channelRow(19, 40, "crack")+"\r\n"+
		channelRow(50, 11, "granfloris")+"\r\n"+
		"[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}

	if _, err := service.GMReloadChannels(context.Background()); err == nil {
		t.Fatal("严格模式下缺少默认城镇频道的表应被拒绝热重载")
	}
	if _, ok := service.currentCatalog().Channel(11); !ok {
		t.Fatal("拒绝热重载后旧表必须原样保留，但频道 11 已经不见了")
	}
}

// TestGMReloadChannelsKeepsOldTableWhenFileIsBroken 文件坏掉时不能把服务端搞成半死状态。
func TestGMReloadChannelsKeepsOldTableWhenFileIsBroken(t *testing.T) {
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
	})
	service := newChannelReloadTestService(t, path)
	beforeScript := string(service.currentChannelScript())

	if err := os.WriteFile(path, []byte("[server] 1\r\n这行根本不是频道记录\r\n[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}
	if _, err := service.GMReloadChannels(context.Background()); err == nil {
		t.Fatal("坏掉的频道档应被拒绝")
	}
	if _, ok := service.currentCatalog().Channel(11); !ok {
		t.Fatal("拒绝后旧目录必须保留")
	}
	if string(service.currentChannelScript()) != beforeScript {
		t.Fatal("拒绝后旧客户端脚本必须保留")
	}
}

// TestGMReloadChannelsRequiresRunningService 服务没起来时不能假装重载成功。
func TestGMReloadChannelsRequiresRunningService(t *testing.T) {
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
	})
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{})
	service.options.channelInfoFile = path
	if _, err := service.GMReloadChannels(context.Background()); err == nil {
		t.Fatal("未启动的服务应拒绝热重载")
	}
}

// TestGMReloadChannelsReconcilesGamePorts 端到端验证端口对账：
// 关掉的频道必须停掉监听，新启用的频道必须开监听。
//
// 游戏端口 = GamePortBase + 频道号（dnfenum.GamePortBase = 10000），
// 所以这里先借 OS 分配一个空闲端口，再反推一个映射到它的频道号，
// 避免撞上机器上真实占用的端口。
func TestGMReloadChannelsReconcilesGamePorts(t *testing.T) {
	channelID := reserveFreeChannelID(t)

	// 初始状态：11/19/新频道 都在表里，并且新频道的端口已经被本服务监听。
	path := writeChannelInfoFixture(t, []string{
		channelRow(11, 11, "granfloris"),
		channelRow(19, 40, "crack"),
		channelRow(channelID, 11, "granfloris"),
	})
	service := newChannelReloadTestService(t, path)
	runCtx := service.runCtxForTest()
	if err := service.listenGamePort(runCtx, 10000+channelID); err != nil {
		t.Skipf("测试环境无法监听端口 %d，跳过端到端端口对账: %v", 10000+channelID, err)
	}
	port := 10000 + channelID
	if _, ok := service.gameListeners[port]; !ok {
		t.Fatalf("listenGamePort 之后端口 %d 未登记", port)
	}

	// 关掉那个频道 → 端口监听必须被关掉。
	if err := os.WriteFile(path, []byte("[server] 1\r\n"+
		channelRow(11, 11, "granfloris")+"\r\n"+
		channelRow(19, 40, "crack")+"\r\n"+
		"[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}
	result, err := service.GMReloadChannels(context.Background())
	if err != nil {
		t.Fatalf("GMReloadChannels(close): %v", err)
	}
	if len(result.RemovedChannels) != 1 || result.RemovedChannels[0] != channelID {
		t.Fatalf("RemovedChannels = %v, want [%d]", result.RemovedChannels, channelID)
	}
	if len(result.GamePortsClosed) != 1 || result.GamePortsClosed[0] != port {
		t.Fatalf("GamePortsClosed = %v, want [%d]", result.GamePortsClosed, port)
	}
	if _, still := service.gameListeners[port]; still {
		t.Fatalf("端口 %d 的监听未解除登记", port)
	}
	// 端口真的被释放了：现在能重新绑上。
	probe, probeErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if probeErr != nil {
		t.Fatalf("关闭频道后端口 %d 仍被占用，监听没真正关掉: %v", port, probeErr)
	}
	_ = probe.Close()

	// 再把频道加回来 → 端口监听必须重新开出来。
	if err := os.WriteFile(path, []byte("[server] 1\r\n"+
		channelRow(11, 11, "granfloris")+"\r\n"+
		channelRow(19, 40, "crack")+"\r\n"+
		channelRow(channelID, 11, "granfloris")+"\r\n"+
		"[/server]\r\n"), 0o644); err != nil {
		t.Fatalf("改写频道档: %v", err)
	}
	result, err = service.GMReloadChannels(context.Background())
	if err != nil {
		t.Fatalf("GMReloadChannels(open): %v", err)
	}
	if len(result.AddedChannels) != 1 || result.AddedChannels[0] != channelID {
		t.Fatalf("AddedChannels = %v, want [%d]", result.AddedChannels, channelID)
	}
	if len(result.GamePortsOpened) != 1 || result.GamePortsOpened[0] != port {
		t.Fatalf("GamePortsOpened = %v（warnings=%v）, want [%d]", result.GamePortsOpened, result.Warnings, port)
	}
	if _, ok := service.gameListeners[port]; !ok {
		t.Fatalf("重新启用后端口 %d 未登记", port)
	}
}

// TestCloseGamePortRemovesOnlyTargetListener 关闭单个端口不能误伤别的监听。
func TestCloseGamePortRemovesOnlyTargetListener(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = first.Close()
		t.Fatalf("listen: %v", err)
	}
	defer second.Close()

	service := &Service{
		listeners:     []net.Listener{first, second},
		gameListeners: map[int]net.Listener{10050: first, 10051: second},
	}
	if err := service.closeGamePort(10050); err != nil {
		t.Fatalf("closeGamePort: %v", err)
	}
	if _, still := service.gameListeners[10050]; still {
		t.Fatal("被关闭的端口仍留在 gameListeners 里")
	}
	if len(service.listeners) != 1 || service.listeners[0] != second {
		t.Fatalf("listeners = %v，期望只剩第二个", service.listeners)
	}
	if _, err := first.Accept(); err == nil {
		t.Fatal("被关闭的监听仍在 Accept")
	}
	// 未知端口是幂等空操作，不能报错。
	if err := service.closeGamePort(19999); err != nil {
		t.Fatalf("关闭未知端口应是无操作: %v", err)
	}
}

// TestDiffChannelIDs 比较函数本身（升序、去重语义）。
func TestDiffChannelIDs(t *testing.T) {
	oldCatalog := channelCatalogForTest(t, []int{11, 19, 50, 51})
	newCatalog := channelCatalogForTest(t, []int{11, 19, 51, 52})
	added, removed := diffChannelIDs(oldCatalog, newCatalog)
	if fmt.Sprint(added) != "[52]" || fmt.Sprint(removed) != "[50]" {
		t.Fatalf("added=%v removed=%v, want [52] / [50]", added, removed)
	}
}

func channelCatalogForTest(t *testing.T, ids []int) *channelcatalog.Catalog {
	t.Helper()
	rows := make([]string, 0, len(ids)+1)
	hasBootstrap := false
	for _, id := range ids {
		rows = append(rows, channelRow(id, 11, "granfloris"))
		if id == 19 {
			hasBootstrap = true
		}
	}
	// 引导频道 19 是频道脚本构建的硬要求（Build90CNOnlineScript 会在缺失时直接失败），
	// 每次都补一条——但显式列了 19 时不能再补，重复行会让解析器报 "duplicate server channel"。
	if !hasBootstrap {
		rows = append(rows, channelRow(19, 40, "crack"))
	}
	path := writeChannelInfoFixture(t, rows)
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{})
	service.options.channelInfoFile = path
	catalog, _, err := service.loadChannelAssets()
	if err != nil {
		t.Fatalf("loadChannelAssets: %v", err)
	}
	return catalog
}

// reserveFreeChannelID 借 OS 分配一个空闲端口，反推出映射到它的频道号并立即释放端口。
// 若拿到的端口不落在游戏端口基址之上（理论上不会），就反复重试。
func reserveFreeChannelID(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 32; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("探测空闲端口失败: %v", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		id := port - 10000
		// 避开保护频道与过大的号（频道号要能放进 int16 槽位语义之外也无妨，这里只要求 > 2）。
		if id <= 2 || id == 11 || id == 19 {
			continue
		}
		// 确认现在真的空闲。
		probe, probeErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if probeErr != nil {
			continue
		}
		_ = probe.Close()
		time.Sleep(0)
		return id
	}
	t.Skip("32 次都没能拿到可用的空闲端口，跳过")
	return 0
}

// runCtxForTest 读出租约上下文（仅供同包用例使用）。
func (s *Service) runCtxForTest() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runCtx
}
