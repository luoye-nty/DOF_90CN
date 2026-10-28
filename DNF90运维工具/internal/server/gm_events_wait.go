// GM 事件长轮询（给登录器的实时推送通道）。
//
// ── 为什么需要它（2026-09-28 修真实线上反馈）────────────────────────────────
// 到账绿字（「通过泡点获得 100 点券」）原本只能靠登录器**每 10 秒轮询一次**
// `/launcher/v1/gm-events` 拿到。泡点本身一秒一跳、发放几乎瞬时完成，但玩家要等
// 下一次轮询才看到提示 —— 反馈里的"不实时通知"就是这 0~10 秒（叠加发放耗时更久）。
//
// 这里照搬「更新维护」那套长轮询的做法（见 maintenance.go）：
//   - 主程序每写一条 GM 事件，就按账号唤醒正在等它的长轮询（store 层的 notifier → GMEventAdded）；
//   - 登录器挂着这个长轮询，事件一落库立刻返回，**延迟从"平均 5 秒"降到"毫秒级"**；
//   - 服务端最多挂 25 秒就返回（客户端据此重挂），保证半开连接总会断开重来。
//
// 原来的短轮询**保留不动**：它同时负责心跳续期与兜底拉取，而且老登录器只认它。
// 两条路共用同一个事件游标，长轮询只是让"有事件"这件事更快被知道。
package server

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/store"
)

// gmEventWaitTimeout 长轮询单次等待上限。与维护长轮询同值：比常见反代/网关的
// 30 秒空闲超时短一点，又能让"没有事件"时的心跳足够稀疏。
const gmEventWaitTimeout = 25 * time.Second

// gmEventWaitLimit 单次返回的事件条数上限。短轮询那条走的是 store 的默认 20 条，
// 长轮询一次唤醒后要能把积压尽量清掉，所以给大一些。
const gmEventWaitLimit = 200

// GMEventHub 按账号广播：某个账号有新事件时，只唤醒正在等这个账号的长轮询。
//
// 与 MaintenanceHub 的差别：那个是"有变化"的全局信号，这个是账号定向的
// —— 到账提示必须只给本人，不能把 A 的点券通知推给 B。
type GMEventHub struct {
	mu    sync.Mutex
	conns map[string]map[chan struct{}]struct{}
}

// NewGMEventHub 构造广播 hub。
func NewGMEventHub() *GMEventHub {
	return &GMEventHub{conns: make(map[string]map[chan struct{}]struct{})}
}

// Subscribe 注册某个账号的订阅，返回接收 channel 与退订函数。
func (h *GMEventHub) Subscribe(accountID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if h == nil {
		return ch, func() {}
	}
	h.mu.Lock()
	if h.conns == nil {
		h.conns = make(map[string]map[chan struct{}]struct{})
	}
	set := h.conns[accountID]
	if set == nil {
		set = make(map[chan struct{}]struct{})
		h.conns[accountID] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if set := h.conns[accountID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.conns, accountID)
			}
		}
		h.mu.Unlock()
	}
}

// Broadcast 唤醒某个账号下所有等待中的长轮询，返回唤醒数量。
// 非阻塞投递（channel 容量 1）：已有未处理信号的订阅者直接跳过，符合"不补发"语义。
func (h *GMEventHub) Broadcast(accountID string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for ch := range h.conns[accountID] {
		select {
		case ch <- struct{}{}:
			n++
		default:
		}
	}
	return n
}

// SubscriberCount 当前挂起的订阅者数量（诊断/日志用）。
func (h *GMEventHub) SubscriberCount() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, set := range h.conns {
		total += len(set)
	}
	return total
}

// GMEventAdded 主程序写入一条 GM 事件后调用：唤醒该账号正在长轮询的登录器。
//
// **必须非阻塞**：它由 store 层在 INSERT 成功之后同步调用，
// 没人在等的时候就是一次查表，不能让写事件的人为此付出等待代价。
func (s *Service) GMEventAdded(accountID string) {
	if s == nil || s.gmEventHub == nil {
		return
	}
	s.gmEventHub.Broadcast(strings.TrimSpace(accountID))
}

// GMEventWaiting 当前挂起等待事件推送的登录器数量（日志/诊断用）。
func (s *Service) GMEventWaiting() int { return s.gmEventHub.SubscriberCount() }

// handleGMEventsWait 长轮询：GET /launcher/v1/gm-events/wait?ticket=&after_id=
//
// 语义与短轮询完全一致（同一个账号、同一个 after_id 游标），只是"没有事件时挂住等"：
//   - 一进来先查一次：已经有事件就立刻返回（覆盖"登录器刚打开、事件早就到了"）；
//   - 否则订阅该账号，**订阅后再查一次**（避免"订阅前刚好写了事件"被漏掉）；
//   - 然后等广播 / 客户端断开 / 25 秒超时，任一发生就返回当前事件列表（可能为空）。
//
// 返回空列表**不代表出错**：客户端据此重挂下一轮即可。
// 鉴权与短轮询同源（ticket 决定只能看到本账号的事件）。
func (s *Service) handleGMEventsWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	since := parseSinceParam(r.URL.Query().Get("since"))
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "ticket required")
		return
	}
	accountID, err := s.resolveAccountIDByTicket(r.Context(), ticket)
	if err != nil {
		if err == store.ErrTicketNotFound {
			// 保持与短轮询一致的形状：401 + 同一个错误码，登录器按它重新登录。
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("gm-events wait resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}

	writeEvents := func() bool {
		events, listErr := s.store.ListGMKickEventsAfter(r.Context(), accountID, afterID, since, gmEventWaitLimit)
		if listErr != nil {
			s.log.Error("gm-events wait list failed", "error", listErr)
			writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
			return true
		}
		if len(events) == 0 {
			return false
		}
		writeJSON(w, http.StatusOK, gmEventsResponse{OK: true, Events: gmEventItems(events)})
		return true
	}

	// ① 先查一次：已经有事件就别挂了。
	if writeEvents() {
		return
	}
	// ② 没事件且 hub 不可用（老装配路径/测试）：退化成"立刻返回空"，
	//    行为与短轮询一致，不会把登录器卡在这里。
	if s.gmEventHub == nil {
		writeJSON(w, http.StatusOK, gmEventsResponse{OK: true, Events: []gmEventItem{}})
		return
	}

	ch, unsubscribe := s.gmEventHub.Subscribe(accountID)
	defer unsubscribe()

	// ③ 订阅后再查一次：避免"订阅前刚好写了事件"被漏掉。
	if writeEvents() {
		return
	}

	select {
	case <-ch:
	case <-r.Context().Done():
		return // 客户端断开/超时：直接结束，不写响应
	case <-time.After(gmEventWaitTimeout):
	}
	// 唤醒或超时：返回当前事件（可能为空，客户端会重挂）。
	writeEvents()
}

// gmEventItems 把 store 行转成响应项（短轮询与长轮询共用，避免两处字段漂移）。
func gmEventItems(events []store.GMKickEvent) []gmEventItem {
	items := make([]gmEventItem, 0, len(events))
	for _, e := range events {
		items = append(items, gmEventItem{
			ID:        e.ID,
			AccountID: e.AccountID,
			Username:  e.Username,
			EventType: e.EventType,
			Reason:    e.Reason,
			Operator:  e.Operator,
			CreatedAt: e.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	return items
}

// parseSinceParam 解析登录器带来的 since 参数（RFC3339）。空串/格式错 → 零值（不过滤时间）。
//
// 为什么要有这个参数：见 store.ListGMKickEventsAfter 的注释 ——
// 它让"登录器带着一个被毒化的大游标"这种情况能自愈，是修
// "泡点到账提示永远不弹、且毫无报错"那类故障的关键。
func parseSinceParam(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts.Local()
	}
	// 兼容 Unix 秒（登录器时钟格式将来若变化也不至于直接失效）
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	return time.Time{}
}
