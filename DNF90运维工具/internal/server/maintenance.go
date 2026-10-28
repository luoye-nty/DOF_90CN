package server

// 更新维护：维护期间禁止玩家**登录**与**进游戏**（注册照常开放）。
//
// 为什么服务端要硬拦，而不只是让登录器把按钮置灰：
// 老版本登录器根本不知道"维护"这回事，界面置灰挡不住它；直接打接口也一样。
// 所以两处必须服务端拒绝：
//   - POST /launcher/v1/login          → 403 maintenance
//   - POST /internal/verify-game-ticket → 200 + ok:false reason=maintenance
//     （游戏服据此拒绝这条新开的游戏连接，见 dnfbridge launcher_auth.go）
// 其余接口一律放行：公告、GM 事件、心跳、注册、维护状态本身 —— 登录器必须活着，
// 才能实时收到"维护结束"的通知并把按钮重新点亮。
//
// 白名单（maintenance_allow_users）：维护期间这些账号仍可登录/进游戏，供管理员自测。
// 按用户名比对（NormalizeUsername 归一、不分大小写）。
//
// 推送：MaintenanceHub 广播 + 免 ticket 的长轮询 /launcher/v1/maintenance/wait。
// 登录器一打开（还没登录）就挂上这个长轮询，所以"勾选维护 → 所有已打开的登录器
// 按钮熄灭""取消维护 → 按钮立刻亮起"都是秒级生效。

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/store"
)

// maintenanceWaitTimeout 长轮询单次等待上限。取 25 秒：比常见反代/网关的 30 秒空闲超时短一点，
// 又能让"没有变化"时的心跳足够稀疏（每 25 秒一次请求）。
const maintenanceWaitTimeout = 25 * time.Second

// MaintenanceState 维护状态快照（由主程序在同一把锁下一次读出，避免混合快照）。
type MaintenanceState struct {
	Enabled    bool
	AllowUsers []string // 账号名白名单（配置里的原样写法，比对时归一化）
}

// MaintenanceFunc 返回维护状态实时快照（线程安全，由主程序注入）。nil = 始终"未维护"。
type MaintenanceFunc func() MaintenanceState

// MaintenanceHub 维护状态变化广播：长轮询 handler 订阅，状态变化时唤醒。
// 只广播"有变化"这一个信号，状态本身由各订阅者重新读取 —— 这样永远不会有陈旧快照。
type MaintenanceHub struct {
	mu    sync.Mutex
	conns map[chan struct{}]struct{}
}

// NewMaintenanceHub 构造广播 hub。
func NewMaintenanceHub() *MaintenanceHub {
	return &MaintenanceHub{conns: make(map[chan struct{}]struct{})}
}

// Subscribe 注册一个订阅，返回接收 channel 与退订函数。
func (h *MaintenanceHub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.conns[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.conns, ch)
		h.mu.Unlock()
	}
}

// Broadcast 唤醒所有等待中的长轮询，返回成功唤醒数量。
// 非阻塞投递（channel 容量 1）：已经有未处理信号的订阅者直接跳过，符合"不补发"语义。
func (h *MaintenanceHub) Broadcast() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for ch := range h.conns {
		select {
		case ch <- struct{}{}:
			n++
		default:
		}
	}
	return n
}

// SubscriberCount 当前挂起的订阅者数量（诊断/日志用）。
func (h *MaintenanceHub) SubscriberCount() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// SetMaintenance 注入维护状态读取函数（未注入 = 始终"未维护"，行为与改造前一致）。
func (s *Service) SetMaintenance(fn MaintenanceFunc) { s.maintenanceFn = fn }

// MaintenanceWaiting 当前挂起等待维护状态变化的登录器数量（日志/诊断用）。
func (s *Service) MaintenanceWaiting() int { return s.maintenanceHub.SubscriberCount() }

// maintenanceState 当前维护状态快照。
func (s *Service) maintenanceState() MaintenanceState {
	if s.maintenanceFn == nil {
		return MaintenanceState{}
	}
	return s.maintenanceFn()
}

// maintenanceAllows 该账号是否在维护白名单里（纯函数，便于单测）。
// 归一化后不分大小写比对；白名单为空 = 谁都不放行。
func maintenanceAllows(state MaintenanceState, username string) bool {
	want := store.NormalizeUsername(username)
	if want == "" {
		return false
	}
	for _, u := range state.AllowUsers {
		if store.NormalizeUsername(u) == want {
			return true
		}
	}
	return false
}

// maintenanceBlocks 维护中且该账号不在白名单 → 拦截。
// username 为空（例如取不到账号名）时按"不在白名单"处理：维护期间宁严不宽。
func (s *Service) maintenanceBlocks(username string) bool {
	state := s.maintenanceState()
	if !state.Enabled {
		return false
	}
	return !maintenanceAllows(state, username)
}

// MaintenanceChanged 维护配置变更后由主程序调用：版本 +1 并唤醒所有等待中的登录器。
// 返回新版本号（仅用于日志/诊断）。
func (s *Service) MaintenanceChanged() int64 {
	ver := s.maintenanceVer.Add(1)
	if s.maintenanceHub != nil {
		s.maintenanceHub.Broadcast()
	}
	return ver
}

// maintenanceInfo 维护状态响应体。
type maintenanceInfo struct {
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
	// Allowed：对本次传入的 username 是否放行（没传 username 或维护未开启时为 false）。
	// 登录器据此决定按钮是否对这个账号点亮（白名单里的管理员照样能登）。
	Allowed bool `json:"allowed"`
}

func (s *Service) maintenanceInfoFor(username string) maintenanceInfo {
	state := s.maintenanceState()
	return maintenanceInfo{
		Enabled: state.Enabled,
		Version: s.maintenanceVer.Load(),
		Allowed: state.Enabled && maintenanceAllows(state, username),
	}
}

// handleMaintenanceInfo 短请求：GET /launcher/v1/maintenance?username=
// 登录器自检阶段用它拿初始状态；用户改账号名时也用它重查一次（白名单判断依赖账号名）。
func (s *Service) handleMaintenanceInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	info := s.maintenanceInfoFor(strings.TrimSpace(r.URL.Query().Get("username")))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": info})
}

// handleMaintenanceWait 长轮询：GET /launcher/v1/maintenance/wait?version=&username=
//
// 免 ticket —— 登录器还停在登录界面时也必须能收到"开始/结束维护"。
// 版本号与传入不一致时立刻返回（覆盖"登录器刚打开就已在维护中"）；
// 否则挂起等待广播，超时后返回当前状态（客户端据此重试下一轮）。
func (s *Service) handleMaintenanceWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	clientVer, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("version")), 10, 64)

	current := s.maintenanceVer.Load()
	if clientVer != current || s.maintenanceHub == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.maintenanceInfoFor(username)})
		return
	}

	ch, unsubscribe := s.maintenanceHub.Subscribe()
	defer unsubscribe()

	// 订阅之后再读一次版本：避免"订阅前刚好变了"被漏掉。
	if s.maintenanceVer.Load() != current {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.maintenanceInfoFor(username)})
		return
	}

	select {
	case <-ch:
	case <-r.Context().Done():
		return // 客户端断开/超时：直接结束，不写响应
	case <-time.After(maintenanceWaitTimeout):
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.maintenanceInfoFor(username)})
}

// usernameForAccount 取账号名（维护白名单按用户名比对）。
// 查不到返回空串 = 不在白名单 = 维护期间拦下（宁严不宽）。
func (s *Service) usernameForAccount(ctx context.Context, accountID string) string {
	if s.store == nil || strings.TrimSpace(accountID) == "" {
		return ""
	}
	acc, err := s.store.GetAccountByID(ctx, accountID)
	if err != nil || acc == nil {
		return ""
	}
	return acc.Username
}
