// 双开（多开）开关：给登录器的公开端点（免 ticket）。
//
// 背景与三端联动说明见主程序 multi_open.go（那里是配置字段的唯一事实源）。
//
// 登录器（**免费版与外网版都要**）在启动自检阶段拉一次这个接口，决定「双开」勾选框
// 与副账号登录按钮是否可用。免费版也要生效 —— 双开 UI 完全不在 `#if EXTERNAL` 里
// （免费版可以自由切到 "90" profile，同样能双开），所以这个端点与登录器侧的读取、
// 闸门都不能放进任何版本条件编译里。
//
// ── 实时推送：/launcher/v1/multi-open/wait（长轮询）────────────────────────
//
// 短端点只解决"登录器打开/点副登录那一刻"的取值；**已经开着的登录器**不会自己变灰，
// 运营关掉开关后得让玩家重开登录器 —— 服主不接受这个时机。
// 所以这里照抄「更新维护」那套已经在生产跑通的推送（maintenance.go + 本文件的长轮询）：
//
//	moApply（主程序 multi_open.go，运营点开关）
//	  → svc.MultiOpenChanged()（Broadcast 唤醒所有挂起的长轮询）
//	  → handleMultiOpenWait 立刻返回当前状态
//	  → 登录器 MultiOpenLoopAsync → ApplyMultiOpenStateAsync（置灰 + Toast）
//
// 短端点**保留不动**：老登录器只认它，而且它是"登录器刚打开、长轮询还没挂上"时的初值来源。
//
// 为什么必须免 ticket：自检阶段发生在玩家还没登录的时候，登录器手里根本没有票。
// 与 /launcher/v1/maintenance、/launcher/v1/registration-info、/launcher/v1/version-check
// 同一口径。
//
// ── ★ 三态：只有明确收到 enabled:false 才关闭 ★ ─────────────────────────────
//
// 响应 data 的形态（登录器侧用 `bool?` 接，缺失/为 null 都是 null）：
//
//	{"ok":true,"data":{"enabled":false}}   → 运营明确关掉了双开：登录器关闸；
//	{"ok":true,"data":{"enabled":true}}    → 运营明确允许：登录器开放双开；
//	{"ok":true,"data":{}}                  → 后台从未设置过（老 config.json）：
//	                                         登录器收到 null → **保持允许双开**；
//	（老运维工具根本没有这个路径 → 404 → 登录器拉取失败 → 同样保持允许）。
//
// 为什么"不下发该字段"这一态必须有：需求要求"双开现在默认打开"，也就是升级运维工具
// 不能让全服双开瞬间消失；同时要求"只有明确收到 false 才关闭"。把"没设置过"表达成
// 字段缺失（而不是 false），这两个要求才同时成立。
//
// 这个接口**永远返回 200**（只要认证服务活着）：双开被关掉不是"请求失败"，
// 而是一个正常业务结果（口径同 version-check，"拉不到也不报错"）。
package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// MultiOpenState 双开开关状态快照（由主程序在同一把锁下一次读出）。
type MultiOpenState struct {
	// Enabled 是否允许玩家双开。
	//
	// **三态**（本功能最容易出事的地方）：
	//	nil    = 后台从未设置过 / 未注入 → 端点**不下发** enabled 字段
	//	         → 登录器拿到 null → 保持"允许双开"（= 改造前的现有行为）；
	//	&true  = 明确允许双开；
	//	&false = 明确关闭双开 —— **只有这一种情况**登录器才会关闸。
	Enabled *bool
}

// MultiOpenFunc 返回双开开关实时快照（线程安全，由主程序注入）。
//
// nil = 未注入 = 端点下发空 data = 登录器保持允许双开。
// 这个缺省方向是**故意**的：宁可不关双开，也不能因为"忘了装配"就让全服玩家不能双开。
type MultiOpenFunc func() MultiOpenState

// SetMultiOpen 注入双开开关状态读取函数。
func (s *Service) SetMultiOpen(fn MultiOpenFunc) { s.multiOpenFn = fn }

// multiOpenState 当前双开开关快照；未注入时返回空快照（= 不下发 = 允许）。
func (s *Service) multiOpenState() MultiOpenState {
	if s.multiOpenFn == nil {
		return MultiOpenState{}
	}
	state := s.multiOpenFn()
	if state.Enabled == nil {
		return MultiOpenState{}
	}
	// 拷贝一份，避免把主程序内存里的指针泄漏出去（调用方只读，但仍要防写穿）。
	enabled := *state.Enabled
	return MultiOpenState{Enabled: &enabled}
}

// handleMultiOpen 公开端点（无需 ticket）：GET /launcher/v1/multi-open
//
// 返回 data.enabled；**未显式设置过时不下发该字段**（登录器收到 null = 保持允许双开）。
// 永远 200：这是"一个正常业务结果"，登录器不把它当错误分支处理。
func (s *Service) handleMultiOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	writeMultiOpenOK(w, s.multiOpenState())
}

// ============================================================================
//  实时推送：双开开关变化广播 + 长轮询端点
// ============================================================================

// multiOpenWaitTimeout 长轮询单次等待上限。与维护/GM 事件取同一个值（25 秒）：
// 比常见反代/网关的 30 秒空闲超时短一点，又能让"没有变化"时的心跳足够稀疏
// （每 25 秒一次请求），半开连接也总会断开重来。
//
// 用 var 而不是 const：单测要把它压到毫秒级。"后台从未设置过（data:{}）"那一路
// **不会**被 known 短路（见 multiOpenKnownStale 的 nil 分支），必须真挂满这个时长才返回，
// 25 秒会让那条测试没法跑。生产代码只读本变量，不写死字面量。
var multiOpenWaitTimeout = 25 * time.Second

// MultiOpenHub 双开开关变化广播：长轮询 handler 订阅，开关一改就唤醒。
//
// 与 MaintenanceHub 逐行同形（只广播"有变化"这一个信号，状态本身由各订阅者重新读取，
// 这样永远不会有陈旧快照）。**不做账号隔离**：与 GMEventHub 不同，双开开关是全服公开信息，
// 没有任何"只该给某个人看"的内容 —— 一个登录器挂着多条长轮询也没有意义，
// 登录器侧只挂一条（见 LoginWindow.MultiOpenLoopAsync）。
type MultiOpenHub struct {
	mu    sync.Mutex
	conns map[chan struct{}]struct{}
}

// NewMultiOpenHub 构造广播 hub。
func NewMultiOpenHub() *MultiOpenHub {
	return &MultiOpenHub{conns: make(map[chan struct{}]struct{})}
}

// Subscribe 注册一个订阅，返回接收 channel 与退订函数。
func (h *MultiOpenHub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if h == nil {
		return ch, func() {}
	}
	h.mu.Lock()
	if h.conns == nil {
		h.conns = make(map[chan struct{}]struct{})
	}
	h.conns[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.conns, ch)
		h.mu.Unlock()
	}
}

// Broadcast 唤醒所有等待中的长轮询，返回成功唤醒数量。
// 非阻塞投递（channel 容量 1）：已经有未处理信号的订阅者直接跳过，符合"不补发"语义
// —— 它由运营点开关的那条管理请求同步调用，绝不能因为某个客户端卡住而拖住管理端。
func (h *MultiOpenHub) Broadcast() int {
	if h == nil {
		return 0
	}
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
func (h *MultiOpenHub) SubscriberCount() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// MultiOpenChanged 双开开关变更后由主程序调用（见 main 包 multi_open.go 的 moApply）：
// 唤醒所有等待中的登录器长轮询，返回被唤醒的数量（仅用于日志/诊断）。
//
// 这是"后台一关，已经开着的登录器马上变灰"的**唯一源头** —— 少了这一步，
// 长轮询就退化成最多 25 秒的延迟，运营会以为开关没生效。
func (s *Service) MultiOpenChanged() int {
	if s == nil || s.multiOpenHub == nil {
		return 0
	}
	return s.multiOpenHub.Broadcast()
}

// MultiOpenWaiting 当前挂起等待双开开关变化的登录器数量（日志/诊断用）。
func (s *Service) MultiOpenWaiting() int { return s.multiOpenHub.SubscriberCount() }

// MultiOpen 返回双开开关广播 hub（形状同 Announce()）。
//
// 给"需要自己订阅"的调用方用：主程序侧的单测靠它验证 moApply **真的**把变化推了出去
// —— 那是"实时"的源头，静默漏掉的话表现是"后台关了，已打开的登录器最多要等 25 秒
// 才变灰"，在生产的日志里几乎看不出来（只是慢，不是错）。
func (s *Service) MultiOpen() *MultiOpenHub { return s.multiOpenHub }

// multiOpenKnownStale 客户端"当前认知"是否已经过期（= 服务端应当立刻回话，而不是挂起等）。
//
// 这是把 handleMaintenanceWait 的"版本号不一致就立刻返回"等价地搬到双开上的东西。
// 双开状态是一个三态布尔，**它自己就是版本**：客户端只关心"当前值"，
// true→false→true 这种来回切换不影响正确性（它拿到的永远是最新值），所以不需要额外的版本号。
//
// 短路掉的那段窗口是真实存在的：登录器启动自检拉短端点、到长轮询真正挂上之间有一小段
// （网络往返 + 窗口初始化）。运营刚好在这段时间点开关的话，光靠广播是收不到的
// （那会儿还没订阅），只能等 25 秒超时。带上 known 就没这个窗口了。
//
// knownSet=false（请求里没带 known，例如将来别家的客户端）：一律当作"不过期"。
// 缺参数绝不能变成"每次都立刻返回"，那会把长轮询打成忙轮询。
func multiOpenKnownStale(known *bool, knownSet bool, current MultiOpenState) bool {
	if !knownSet {
		return false
	}
	if current.Enabled == nil {
		// 后台从未设置过 = 服务端**没有**明确表态 = 客户端那份认知没有被推翻
		//（登录器把这一态同样按"允许双开"处理），不算过期。
		//
		// ⚠ 这一条同时是**防忙轮询**的关键：若把 nil 判成"不一致"，客户端每次都会
		// 立刻收到一个不带 enabled 的响应，而它的 known 也永远不会被更新，
		// 于是变成每秒上千次的死循环（比没推送严重得多）。
		return false
	}
	if known == nil {
		return true // 服务端有明确值、客户端还一无所知 → 立刻告诉它
	}
	return *known != *current.Enabled
}

// multiOpenStateDiffers 两份快照是否**语义不同**（三态逐位比较：nil ≠ true/false）。
// 用于长轮询里"订阅前/订阅后"的一致性检查，避免刚好卡在订阅那一瞬间的变化被漏掉。
func multiOpenStateDiffers(a, b MultiOpenState) bool {
	switch {
	case a.Enabled == nil && b.Enabled == nil:
		return false
	case a.Enabled == nil || b.Enabled == nil:
		return true // 一边"没设置过"、一边是明确值 → 变了
	default:
		return *a.Enabled != *b.Enabled
	}
}

// multiOpenData 双开状态 → 响应 data。
//
// **未显式设置过时返回空 map**（JSON 里就是 `{}`）—— 登录器收到"字段缺失" = null = 保持允许。
// 短端点与长轮询端点共用本函数，保证两条路的形态**逐字一致**（这是跨端契约，
// 见 launcher_version_test.go 的 TestHandleMultiOpenTriStateContract）。
func multiOpenData(state MultiOpenState) map[string]any {
	data := map[string]any{}
	if state.Enabled != nil {
		data["enabled"] = *state.Enabled
	}
	return data
}

// writeMultiOpenOK 短端点与长轮询端点共用的响应写出（**永远 200**）。
func writeMultiOpenOK(w http.ResponseWriter, state MultiOpenState) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": multiOpenData(state)})
}

// parseMultiOpenKnown 解析长轮询的 known 参数（客户端"当前认知"的三态）。
// 空串/无法识别 → knownSet=false = 不短路：宁可不省这一次往返，也不能瞎猜客户端的状态
// （猜错的后果是"该变灰的时候不返回"或"不该返回的时候打成忙轮询"）。
func parseMultiOpenKnown(raw string) (*bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		v := true
		return &v, true
	case "false", "0":
		v := false
		return &v, true
	default:
		return nil, false
	}
}

// handleMultiOpenWait 长轮询：GET /launcher/v1/multi-open/wait?known=
//
// ── 鉴权口径：免 ticket（与短端点、/launcher/v1/maintenance/wait 同源）──────────
//
// 它服务的是**还没登录的登录界面**，玩家手里根本没有票。对比两个先例：
//   - handleMaintenanceWait：免 ticket。它带 username 只是为了判维护白名单，
//     username 是明文查询参数、不是凭证，所以它并没有真的做鉴权；
//   - handleGMEventsWait：要 ticket，因为到账提示必须**按账号隔离**（store 按 ticket 取 accountID）。
//
// 双开开关是**全服公开信息、与账号无关**（短端点同样是免 ticket 的公开端点），
// 多带一张票既没有信息量，又会让"还没登录就收不到推送"这个最需要它的场景彻底失效
// —— 那正是本次需求要修的场景（已打开的登录器停在登录界面）。
//
// ── 语义与短端点**完全一致** ────────────────────────────────────────────────
//
// 同一个 multiOpenData（含"后台从未设置过 → data:{}"），区别只在"客户端认知没过期时挂住等"：
//   - 客户端带了 known 且与服务端**明确值**不同 → 立刻返回（覆盖"自检到挂长轮询之间的窗口"）；
//   - 否则订阅广播，**订阅后再比一次快照**（避免"订阅前刚好变了"被漏掉）；
//   - 然后等广播 / 客户端断开 / 25 秒超时，任一发生就返回当前状态。
//
// 返回**永远 200**，data 为空也不是错误（客户端据此重挂下一轮）。
func (s *Service) handleMultiOpenWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	known, knownSet := parseMultiOpenKnown(r.URL.Query().Get("known"))

	before := s.multiOpenState()
	if s.multiOpenHub == nil || multiOpenKnownStale(known, knownSet, before) {
		// hub 未装配（老装配路径/测试）：退化成"立刻返回当前状态"，行为与短端点一致，
		// 不会把登录器卡在这里。
		writeMultiOpenOK(w, before)
		return
	}

	ch, unsubscribe := s.multiOpenHub.Subscribe()
	defer unsubscribe()

	// 订阅后再读一次快照：广播只发一次、不补发，所以"订阅完成前刚好变了"必须在这里兜住。
	if after := s.multiOpenState(); multiOpenStateDiffers(before, after) {
		writeMultiOpenOK(w, after)
		return
	}

	select {
	case <-ch:
	case <-r.Context().Done():
		return // 客户端断开/超时：直接结束，不写响应
	case <-time.After(multiOpenWaitTimeout):
	}
	// 被唤醒或超时：读**当前**状态返回（订阅者永远看不到陈旧快照）。
	writeMultiOpenOK(w, s.multiOpenState())
}
