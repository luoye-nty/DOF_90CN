package server

// 更新维护闸门的语义测试（2026-09-24 新增）。
//
// 这几条锁的是"维护期间到底放行谁"：
//   - 未开维护 → 谁都不拦（行为必须与改动前完全一致）；
//   - 开维护 + 白名单为空 → 谁都拦（宁严不宽）；
//   - 开维护 + 账号在白名单 → 放行（管理员自测）；
//   - 账号名归一化后比对（大小写/前后空白都不影响）。
//
// 写反任何一条的后果都很直接：要么维护形同虚设，要么管理员自己也被关在门外。

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dnf90ops/internal/config"
)

func TestMaintenanceAllows(t *testing.T) {
	cases := []struct {
		name      string
		allow     []string
		username  string
		wantAllow bool
	}{
		{"白名单为空：谁都不放行", nil, "admin", false},
		{"命中白名单", []string{"admin"}, "admin", true},
		{"大小写不敏感", []string{"Admin"}, "aDmIn", true},
		{"前后空白要裁掉", []string{"  admin  "}, " admin ", true},
		{"不在名单里", []string{"admin", "ops"}, "player1", false},
		{"空用户名（取不到账号名）：不放行", []string{"admin"}, "", false},
	}
	for _, tc := range cases {
		state := MaintenanceState{Enabled: true, AllowUsers: tc.allow}
		if got := maintenanceAllows(state, tc.username); got != tc.wantAllow {
			t.Fatalf("%s: maintenanceAllows = %v, want %v", tc.name, got, tc.wantAllow)
		}
	}
}

func TestMaintenanceBlocks(t *testing.T) {
	cases := []struct {
		name      string
		enabled   bool
		allow     []string
		username  string
		wantBlock bool
	}{
		{"未开维护：一律不拦（行为与改动前一致）", false, nil, "player1", false},
		{"未开维护：连空账号名也不拦", false, []string{"admin"}, "", false},
		{"开维护：普通玩家拦", true, nil, "player1", true},
		{"开维护：白名单账号放行", true, []string{"admin"}, "admin", false},
		{"开维护：取不到账号名一律拦（宁严不宽）", true, []string{"admin"}, "", true},
	}
	for _, tc := range cases {
		s := &Service{
			cfg:           &config.Config{},
			maintenanceFn: func() MaintenanceState { return MaintenanceState{Enabled: tc.enabled, AllowUsers: tc.allow} },
		}
		if got := s.maintenanceBlocks(tc.username); got != tc.wantBlock {
			t.Fatalf("%s: maintenanceBlocks = %v, want %v", tc.name, got, tc.wantBlock)
		}
	}
	// 没注入状态函数（老装配路径 / 测试）时必须是"未维护"，绝不能默认拦人。
	bare := &Service{cfg: &config.Config{}}
	if bare.maintenanceBlocks("anyone") {
		t.Fatal("未注入维护状态时不应拦截任何人")
	}
}

// TestMaintenanceHubBroadcastWakesWaiters：保存维护配置后必须能唤醒等待中的长轮询，
// 否则"取消维护 → 按钮立刻亮起"就退化成了等超时。
func TestMaintenanceHubBroadcastWakesWaiters(t *testing.T) {
	hub := NewMaintenanceHub()
	ch, unsubscribe := hub.Subscribe()
	defer unsubscribe()
	if n := hub.Broadcast(); n != 1 {
		t.Fatalf("Broadcast 唤醒数 = %d, want 1", n)
	}
	select {
	case <-ch:
	default:
		t.Fatal("订阅者没有被唤醒")
	}
	if hub.SubscriberCount() != 1 {
		t.Fatalf("订阅者数 = %d, want 1", hub.SubscriberCount())
	}
	unsubscribe()
	if hub.SubscriberCount() != 0 {
		t.Fatalf("退订后订阅者数 = %d, want 0", hub.SubscriberCount())
	}
	// 没人订阅时广播不应 panic。
	if n := hub.Broadcast(); n != 0 {
		t.Fatalf("空 hub 广播唤醒数 = %d, want 0", n)
	}
}

// TestMaintenanceInfoEndpoint：免 ticket 的短请求要回 enabled/version/allowed，
// 版本变化后 allowed 必须跟着账号名走（登录器靠它决定按钮亮不亮）。
func TestMaintenanceInfoEndpoint(t *testing.T) {
	s := &Service{
		cfg:            &config.Config{},
		maintenanceHub: NewMaintenanceHub(),
		maintenanceFn: func() MaintenanceState {
			return MaintenanceState{Enabled: true, AllowUsers: []string{"admin"}}
		},
	}

	rec := httptest.NewRecorder()
	s.handleMaintenanceInfo(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/maintenance?username=admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"allowed":true`) {
		t.Fatalf("白名单账号应 enabled+allowed 都为 true，实际 %s", body)
	}

	rec = httptest.NewRecorder()
	s.handleMaintenanceInfo(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/maintenance?username=player1", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"allowed":false`) {
		t.Fatalf("普通玩家应 enabled=true / allowed=false，实际 %s", body)
	}

	// 长轮询：客户端版本**落后**（-1 ≠ 当前版本）→ 立刻返回当前状态。
	// 覆盖"登录器刚打开就已在维护中"这种情况（它带的版本号必然与服务器不同）。
	rec = httptest.NewRecorder()
	s.handleMaintenanceWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/maintenance/wait?version=-1&username=admin", nil))
	if body := rec.Body.String(); !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("版本不一致时应立刻返回当前状态，实际 %s", body)
	}
}

// TestMaintenanceWaitReturnsOnBroadcast：版本一致时挂起等待，收到广播后立刻返回。
// 这条就是"取消维护 → 已打开的登录器按钮立刻亮起"的服务端行为；写成超时才返回
// 就退化成最多 25 秒的延迟，玩家会以为维护还没结束。
func TestMaintenanceWaitReturnsOnBroadcast(t *testing.T) {
	s := &Service{
		cfg:            &config.Config{},
		maintenanceHub: NewMaintenanceHub(),
		maintenanceFn: func() MaintenanceState { return MaintenanceState{} },
	}
	version := s.maintenanceVer.Load() // 与客户端一致 → 会挂起等待

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleMaintenanceWait(rec, httptest.NewRequest(http.MethodGet,
			"/launcher/v1/maintenance/wait?version="+strconv.FormatInt(version, 10), nil))
		done <- rec
	}()

	// 等长轮询真正挂上（订阅数 > 0）再广播，避免"广播早于订阅"这种测试竞态。
	deadline := time.Now().Add(3 * time.Second)
	for s.MaintenanceWaiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.MaintenanceWaiting() == 0 {
		t.Fatal("长轮询没有挂起等待")
	}

	// 模拟"运维工具保存维护开关"：开关打开 + 版本 +1 + 广播。
	s.maintenanceFn = func() MaintenanceState {
		return MaintenanceState{Enabled: true, AllowUsers: []string{"admin"}}
	}
	s.MaintenanceChanged()

	select {
	case rec := <-done:
		if body := rec.Body.String(); !strings.Contains(body, `"enabled":true`) {
			t.Fatalf("广播后应立即返回最新状态，实际 %s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("广播后长轮询没有立刻返回（会退化成等超时）")
	}
}
