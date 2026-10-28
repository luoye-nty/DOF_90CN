package main

// 双开（多开）开关：配置读写与**三态语义**的纯函数测试。
//
// 这个功能最容易出事的地方只有两个，本文件逐个钉住：
//
//  1. **"关"必须来自运营的一次明确点击**。升级运维工具 / 老 config.json / 网络异常
//     都不能让全服双开瞬间消失。所以配置字段是 `*bool`：
//     nil（没设过）→ 对外端点不下发 enabled → 登录器保持允许双开；
//     &false（显式关闭）→ 端点下发 enabled:false → 登录器才关闸。
//     TestMultiOpenUnconfiguredStaysAllowed / TestMultiOpenApplyPersistsExplicitFalse
//  2. **显式关闭必须落得住盘**。如果这里用 bool + omitempty，false 会被 json 省略，
//     重启后开关自己就变回"开"了（关不掉）。指针不会被省略，所以关得掉。
//     TestMultiOpenApplyPersistsExplicitFalse
//
// 另外覆盖：配置未加载时报错不 panic、落盘失败时内存整体回滚。
// TestMultiOpenApplyRollsBackOnSaveFailure

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
	"dnf90ops/internal/server"
)

// newMultiOpenTestApp 起一个指向临时 config.json 的 App（与 maintApply 的测试同形）。
func newMultiOpenTestApp(t *testing.T) (*App, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return &App{cfg: cfg}, cfgPath
}

// TestMultiOpenUnconfiguredStaysAllowed "从未设置过"必须是**允许双开**。
//
// 这是整个功能的降级基石：老 config.json 里没有 multi_open_enabled 键，
// 升级运维工具后如果这一态被判成"关闭"，全服玩家的双开会在升级那一刻同时消失。
func TestMultiOpenUnconfiguredStaysAllowed(t *testing.T) {
	a, _ := newMultiOpenTestApp(t)

	// 配置里没设过 → 管理端快照报"允许"，但标记为"未显式设置"。
	st := a.moStatus()
	if !st.Enabled {
		t.Fatalf("从未设置过时玩家侧应视为允许双开，实际 %+v", st)
	}
	if st.Configured {
		t.Fatalf("从未设置过时 Configured 应为 false，实际 %+v", st)
	}

	// 对外端点：不下发 enabled（登录器收到 null = 保持允许双开）。
	pub := a.moPublicState()
	if pub.Enabled != nil {
		t.Fatalf("从未设置过时不应下发 enabled（应保持 nil），实际 %v", *pub.Enabled)
	}

	// 配置指针为 nil 的 App 也必须安全：认证服务可能在配置加载前被调用。
	empty := &App{}
	if p := empty.moPublicState(); p.Enabled != nil {
		t.Fatalf("无配置时对外应视为未设置，实际 %v", *p.Enabled)
	}
	if s := empty.moStatus(); !s.Enabled || s.Configured {
		t.Fatalf("无配置时管理端快照应报「允许但未设置」，实际 %+v", s)
	}
}

// TestMultiOpenApplyRejectsUnloadedConfig 配置没加载时必须报错（而不是 panic 或静默成功）。
func TestMultiOpenApplyRejectsUnloadedConfig(t *testing.T) {
	empty := &App{}
	if _, err := empty.moApply(context.Background(), false); err == nil {
		t.Fatal("配置未加载时 moApply 必须返回错误")
	}

	// 同一个 App 也要能吃住 nil cfg 的读取（认证服务可能在配置加载前被调用）。
	if st := empty.moStatus(); !st.Enabled {
		t.Fatalf("无配置时读取不应 panic 且应报允许，实际 %+v", st)
	}
}

// TestMultiOpenApplyPersistsExplicitFalse 正常值：开启 → 关闭 → 落盘 → 重新加载，
// **两次都必须与本次入参逐字一致**，特别是 false 不能"自己变回 true"。
func TestMultiOpenApplyPersistsExplicitFalse(t *testing.T) {
	a, cfgPath := newMultiOpenTestApp(t)

	// 关闭（第一次显式设置）
	st, err := a.moApply(context.Background(), false)
	if err != nil {
		t.Fatalf("moApply(false): %v", err)
	}
	if st.Enabled || !st.Configured {
		t.Fatalf("关闭后快照应为 enabled=false configured=true，实际 %+v", st)
	}
	pub := a.moPublicState()
	if pub.Enabled == nil || *pub.Enabled {
		t.Fatalf("关闭后对外必须下发 enabled=false，实际 %v", pub.Enabled)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.MultiOpenEnabled == nil {
		t.Fatal("显式关闭必须落盘（指针非 nil）—— 否则重启后开关会自己变回「允许」")
	}
	if *reloaded.MultiOpenEnabled {
		t.Fatal("重新加载后开关应为 false")
	}
	// 落盘的 JSON 里必须真的写出这个键（omitempty 对非 nil 指针不省略）。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), `"multi_open_enabled"`) {
		t.Fatalf("config.json 里必须写出 multi_open_enabled，实际内容：\n%s", raw)
	}

	// 再打开
	if st, err = a.moApply(context.Background(), true); err != nil {
		t.Fatalf("moApply(true): %v", err)
	}
	if !st.Enabled || !st.Configured {
		t.Fatalf("开启后快照应为 enabled=true configured=true，实际 %+v", st)
	}
	reloaded2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload2: %v", err)
	}
	if reloaded2.MultiOpenEnabled == nil || !*reloaded2.MultiOpenEnabled {
		t.Fatalf("重新加载后开关应为 true，实际 %v", reloaded2.MultiOpenEnabled)
	}
}

// TestMultiOpenApplyRollsBackOnSaveFailure 落盘失败时内存必须整体回滚：
// 绝不能出现"界面上显示已关闭、配置里其实还是开着"（或反过来）的分裂状态。
//
// 制造失败的手法：把 config.json 换成一个**同名目录** —— Mutate 内部的临时文件写得进去，
// 最后的 os.Rename(tmp, config.json) 必然失败。
func TestMultiOpenApplyRollsBackOnSaveFailure(t *testing.T) {
	a, cfgPath := newMultiOpenTestApp(t)

	// 先显式开启并落盘，拿到一个确定的旧值。
	if _, err := a.moApply(context.Background(), true); err != nil {
		t.Fatalf("准备阶段 moApply(true): %v", err)
	}
	old := a.cfg.MultiOpenEnabled
	if old == nil || !*old {
		t.Fatalf("准备阶段旧值应为 &true，实际 %v", old)
	}

	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatalf("mkdir config(as dir): %v", err)
	}

	if _, err := a.moApply(context.Background(), false); err == nil {
		t.Fatal("落盘必然失败时 moApply 必须返回错误")
	}
	// 内存必须回到旧值（&true），而不是停在失败的那次入参（&false）。
	got := a.cfg.MultiOpenEnabled
	if got == nil {
		t.Fatal("回滚后指针不应变成 nil")
	}
	if !*got {
		t.Fatal("落盘失败后内存没有回滚：开关停在了未生效的 false 上")
	}
	if st := a.moStatus(); !st.Enabled {
		t.Fatalf("落盘失败后管理端快照也必须还是旧值，实际 %+v", st)
	}
}

// newMultiOpenBroadcastApp 造一个**接了认证服务**的 App：moApply 的广播那一步
// （a.svc.MultiOpenChanged()）只有在 svc 非 nil 时才走得到，而它正是"实时"的源头。
func newMultiOpenBroadcastApp(t *testing.T) (*App, *server.Service) {
	t.Helper()
	a, _ := newMultiOpenTestApp(t)
	svc := server.New(a.cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil)
	svc.SetMultiOpen(a.moPublicState)
	a.svc = svc
	return a, svc
}

// TestMultiOpenApplyBroadcasts 运营点一下开关，**必须**唤醒所有挂起的长轮询。
//
// 这条锁的是"实时"的源头：少了 svc.MultiOpenChanged()，功能表面完全正常
// （开关存得住、短端点也报新值、重新打开登录器也生效），只是**已经开着的登录器
// 最多要等 25 秒才变灰** —— 正是需求要修的那个时机问题，而且生产日志里看不出来。
func TestMultiOpenApplyBroadcasts(t *testing.T) {
	a, svc := newMultiOpenBroadcastApp(t)

	ch, unsubscribe := svc.MultiOpen().Subscribe()
	defer unsubscribe()
	if got := svc.MultiOpenWaiting(); got != 1 {
		t.Fatalf("订阅数 = %d, want 1", got)
	}

	// ① 关闭：广播 + 快照必须立刻变成"明确 false"（长轮询醒来后读到的就是这个）。
	if _, err := a.moApply(context.Background(), false); err != nil {
		t.Fatalf("moApply(false): %v", err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("moApply(false) 没有广播 —— 已打开的登录器就只能等 25 秒超时，开关等于不实时")
	}
	pub := a.moPublicState()
	if pub.Enabled == nil || *pub.Enabled {
		t.Fatalf("关闭后对外快照必须是明确的 enabled=false，实际 %v", pub.Enabled)
	}

	// ② 改回来：同样要广播（否则登录器会一直灰着）。
	if _, err := a.moApply(context.Background(), true); err != nil {
		t.Fatalf("moApply(true): %v", err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("moApply(true) 没有广播 —— 玩家要等 25 秒才能重新双开")
	}
	if pub := a.moPublicState(); pub.Enabled == nil || !*pub.Enabled {
		t.Fatalf("开启后对外快照必须是明确的 enabled=true，实际 %v", pub.Enabled)
	}
}

// TestMultiOpenApplyDoesNotBroadcastOnSaveFailure 落盘失败时**不能**广播。
//
// 那一次改动已经整体回滚：广播出去会让所有登录器按一个并未生效的值置灰
// （运营看到开关弹回去、玩家侧却已经变过了，两边对不上，而且没有任何报错）。
func TestMultiOpenApplyDoesNotBroadcastOnSaveFailure(t *testing.T) {
	a, svc := newMultiOpenBroadcastApp(t)
	cfgPath := a.cfg.Path()
	if strings.TrimSpace(cfgPath) == "" {
		t.Skip("配置未暴露路径，无法制造落盘失败")
	}
	if _, err := a.moApply(context.Background(), true); err != nil {
		t.Fatalf("准备阶段 moApply(true): %v", err)
	}

	ch, unsubscribe := svc.MultiOpen().Subscribe()
	defer unsubscribe()

	// 把 config.json 换成同名目录 → Mutate 的 os.Rename 必然失败。
	if err := os.Remove(cfgPath); err != nil {
		t.Fatalf("remove config: %v", err)
	}
	if err := os.Mkdir(cfgPath, 0o755); err != nil {
		t.Fatalf("mkdir config(as dir): %v", err)
	}
	if _, err := a.moApply(context.Background(), false); err == nil {
		t.Fatal("落盘必然失败时 moApply 必须返回错误")
	}
	select {
	case <-ch:
		t.Fatal("落盘失败（改动已回滚）时不能广播：登录器会按一个并未生效的值置灰")
	default:
	}
}
