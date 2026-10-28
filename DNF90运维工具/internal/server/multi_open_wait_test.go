package server

// 双开（多开）开关**实时推送**的测试（长轮询 + 广播 hub）。
//
// 背景：双开开关原来只有"拉取式"生效时机（登录器启动自检读一次 + 点副账号登录前复查一次），
// 所以**已经开着的登录器窗口不会自动变灰**，运营关掉开关后必须让玩家重开登录器。
// 服主要求"后台一关，已经开着的登录器马上变灰"，做法与「更新维护」完全一致：
// 主程序 moApply 落盘成功后 Broadcast，挂起的长轮询立刻返回。
//
// 这里钉住四件写错后果最严重的事：
//  1. **三态语义在长轮询端点上一字不差地成立**：后台从未设置过 → data:{}（不是 enabled:false）。
//     写错就是"升级运维工具那一刻全服双开消失"；
//  2. **广播能唤醒等待者**：写成"只能等 25 秒超时"就退化成最多 25 秒的延迟，
//     运营会以为开关没生效（这正是本次要修的问题）；
//  3. **广播非阻塞**：它由运营点开关的那条管理请求同步调用，卡住就是管理端卡住；
//  4. **不发散**：客户端认知与服务端一致时**必须挂起**（不能每次都立刻返回，
//     那会把长轮询打成每秒上千次的忙轮询）；"没带 known" 的老式客户端同理。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shortMultiOpenWait 把服务端挂起上限压到毫秒级（生产 25 秒），并在用例结束时还原。
//
// 为什么必须能改：`data:{}`（后台从未设置过）这一路**故意**不会被 known 短路，
// 只能真挂满超时才返回 —— 用 25 秒的话这些用例根本没法跑。
func shortMultiOpenWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := multiOpenWaitTimeout
	multiOpenWaitTimeout = d
	t.Cleanup(func() { multiOpenWaitTimeout = old })
}

// decodeMultiOpenData 取出响应体里的 data 段，并断言 ok:true / 200。
func decodeMultiOpenData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（永远 200：开关状态是业务结果，不是请求失败）", rec.Code)
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	if !envelope.OK {
		t.Fatalf("ok 应为 true，body=%s", rec.Body.String())
	}
	return envelope.Data
}

// newMultiOpenTestService 造一个注入了状态 + 广播 hub 的 Service（长轮询要真的能挂起）。
func newMultiOpenTestService(fn MultiOpenFunc) *Service {
	s := &Service{log: discardLogger(), multiOpenHub: NewMultiOpenHub()}
	if fn != nil {
		s.SetMultiOpen(fn)
	}
	return s
}

func moBoolPtr(v bool) *bool { return &v }

// ============================================================================
//  广播 hub
// ============================================================================

// TestMultiOpenHubWakesAllSubscribers 多订阅者（多台机器/多个登录器窗口）必须一起被唤醒。
// 双开是**全服**开关，没有账号隔离 —— 少唤醒一个，那台机器上的登录器就得多等 25 秒。
func TestMultiOpenHubWakesAllSubscribers(t *testing.T) {
	hub := NewMultiOpenHub()
	first, unsubFirst := hub.Subscribe()
	defer unsubFirst()
	second, unsubSecond := hub.Subscribe()
	defer unsubSecond()
	third, unsubThird := hub.Subscribe()
	defer unsubThird()

	if got := hub.SubscriberCount(); got != 3 {
		t.Fatalf("订阅数 = %d, want 3", got)
	}
	if woke := hub.Broadcast(); woke != 3 {
		t.Fatalf("唤醒数 = %d, want 3（每个挂起的登录器都要收到）", woke)
	}
	for name, ch := range map[string]<-chan struct{}{"first": first, "second": second, "third": third} {
		select {
		case <-ch:
		default:
			t.Fatalf("%s 没被唤醒", name)
		}
	}

	// 退订之后不能再被唤醒（否则会往已关闭的 channel 上投递）。
	// 注意：上面的 select 已经把信号消费掉了，这里**不能**再读 channel（空 channel 读会永久阻塞）。
	unsubFirst()
	unsubSecond()
	unsubThird()
	if woke := hub.Broadcast(); woke != 0 {
		t.Fatalf("退订后唤醒数 = %d, want 0", woke)
	}
	if got := hub.SubscriberCount(); got != 0 {
		t.Fatalf("退订后订阅数 = %d, want 0（不能泄漏连接）", got)
	}
}

// TestMultiOpenHubBroadcastNeverBlocks 同一个订阅者在信号没被消费前重复广播，
// 必须立刻返回：Broadcast 由运营点开关的那条管理请求同步调用，
// 一旦会阻塞（比如往满 channel 上硬写），管理端就会卡住。
func TestMultiOpenHubBroadcastNeverBlocks(t *testing.T) {
	hub := NewMultiOpenHub()
	_, unsub := hub.Subscribe()
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 不消费第一次的信号，看后续广播会不会卡住。
		for i := 0; i < 100; i++ {
			hub.Broadcast()
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("MultiOpenChanged 阻塞了 —— 它由管理端的开关请求同步调用，绝不能拖住管理端")
	}
}

// TestMultiOpenHubWithoutSubscribers 没人挂着的时候（绝大多数时间）广播必须立刻返回 0；
// 而"没人等"绝不能变成错误或阻塞 —— 运营点开关时通常一个订阅者都没有。
func TestMultiOpenHubWithoutSubscribers(t *testing.T) {
	hub := NewMultiOpenHub()
	done := make(chan int, 1)
	go func() { done <- hub.Broadcast() }()
	select {
	case n := <-done:
		if n != 0 {
			t.Fatalf("无订阅者时唤醒数 = %d, want 0", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("无订阅者时广播阻塞了")
	}
	if hub.SubscriberCount() != 0 {
		t.Fatalf("订阅数 = %d, want 0", hub.SubscriberCount())
	}

	// 未装配 hub（老装配路径/测试）：不能 panic，诊断数按 0 报。
	var nilHub *MultiOpenHub
	if nilHub.Broadcast() != 0 || nilHub.SubscriberCount() != 0 {
		t.Fatal("nil hub 必须安全返回 0")
	}
	ch, unsub := nilHub.Subscribe()
	if unsub == nil {
		t.Fatal("nil hub 的退订函数不能是 nil（defer 会崩）")
	}
	unsub()
	select {
	case <-ch:
		t.Fatal("nil hub 不应产生任何信号")
	default:
	}

	bare := &Service{}
	if bare.MultiOpenChanged() != 0 || bare.MultiOpenWaiting() != 0 {
		t.Fatal("未装配 hub 时 MultiOpenChanged/MultiOpenWaiting 必须安全返回 0")
	}
	var nilService *Service
	if nilService.MultiOpenChanged() != 0 {
		t.Fatal("nil Service 调用 MultiOpenChanged 必须安全返回 0")
	}
}

// ============================================================================
//  长轮询端点：与短端点**逐字一致**的三态语义
// ============================================================================

// TestHandleMultiOpenWaitTriStateSameAsShort 长轮询端点的 data 必须与短端点**完全同形**，
// 尤其是"后台从未设置过 → data:{}"这一态。
//
// 这条是整份文件里最要命的：长轮询一旦把"没设置过"发成 enabled:false，
// 所有挂着长轮询的登录器会在升级运维工具那一刻同时关掉双开。
func TestHandleMultiOpenWaitTriStateSameAsShort(t *testing.T) {
	shortMultiOpenWait(t, 25*time.Millisecond)

	cases := []struct {
		name string
		fn   MultiOpenFunc
		// wantPresent=false 表示 data 里**不能**出现 enabled 键（登录器收到 null = 保持允许）。
		wantPresent bool
		want        bool
	}{
		{"未注入（老装配路径/测试）→ 不下发字段 = 允许", nil, false, false},
		{"后台从未设置过（老 config.json）→ 不下发字段 = 允许", func() MultiOpenState { return MultiOpenState{} }, false, false},
		{"明确允许 → enabled:true", func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(true)} }, true, true},
		{"明确关闭 → enabled:false", func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(false)} }, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMultiOpenTestService(tc.fn)

			shorter := httptest.NewRecorder()
			s.handleMultiOpen(shorter, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open", nil))
			shortData := decodeMultiOpenData(t, shorter)

			// 长轮询端点：带一个与服务端**明确值**相反的 known，逼它立刻回话；
			// "从未设置过"那一态不会被短路（故意的），所以只能靠缩短的挂起上限收尾。
			rec := httptest.NewRecorder()
			s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet,
				"/launcher/v1/multi-open/wait?known=true", nil))
			waitData := decodeMultiOpenData(t, rec)

			if len(shortData) != len(waitData) {
				t.Fatalf("长轮询 data 与短端点不同形：short=%v wait=%v", shortData, waitData)
			}
			got, present := waitData["enabled"]
			if present != tc.wantPresent {
				t.Fatalf("data.enabled 是否存在 = %v, want %v（body=%s）—— "+
					"「没设置过」必须表达成字段缺失，不能表达成 false", present, tc.wantPresent, rec.Body.String())
			}
			if !tc.wantPresent {
				if len(waitData) != 0 {
					t.Fatalf("「没设置过」时 data 必须是空对象 {}，实际 %v", waitData)
				}
				return
			}
			val, ok := got.(bool)
			if !ok {
				t.Fatalf("data.enabled 类型 = %T, want bool", got)
			}
			if val != tc.want {
				t.Fatalf("data.enabled = %v, want %v", val, tc.want)
			}
		})
	}

	// 只接受 GET：其余方法 405（与短端点/其它公开端点一致）。
	s := newMultiOpenTestService(nil)
	rec := httptest.NewRecorder()
	s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/multi-open/wait", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d, want 405", rec.Code)
	}
}

// ============================================================================
//  长轮询端点：实时性（本次需求的核心）
// ============================================================================

// TestHandleMultiOpenWaitReturnsOnBroadcast 客户端认知与服务端一致时挂起等待，
// 收到广播（= 运营点了开关）后**立刻**返回最新状态。
//
// 这条就是"后台一关，已经开着的登录器马上变灰"的服务端行为；写成只能等超时
// 就退化成最多 25 秒的延迟 —— 那正是本次要修的问题。
func TestHandleMultiOpenWaitReturnsOnBroadcast(t *testing.T) {
	// 故意留一个很长的挂起上限：如果广播没生效，本用例会直接超时失败（而不是假通过）。
	shortMultiOpenWait(t, 30*time.Second)

	state := MultiOpenState{Enabled: moBoolPtr(true)}
	s := newMultiOpenTestService(func() MultiOpenState { return state })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		// known=true 与服务端当前值一致 → 会挂起等待。
		s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet,
			"/launcher/v1/multi-open/wait?known=true", nil))
		done <- rec
	}()

	// 等长轮询真正挂上（订阅数 > 0）再广播，避免"广播早于订阅"这种测试竞态。
	deadline := time.Now().Add(3 * time.Second)
	for s.MultiOpenWaiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.MultiOpenWaiting() == 0 {
		t.Fatal("长轮询没有挂起等待")
	}

	// 模拟"运营在后台关闭双开"：状态变 false + 广播（= moApply 的两步）。
	state = MultiOpenState{Enabled: moBoolPtr(false)}
	if woke := s.MultiOpenChanged(); woke < 1 {
		t.Fatalf("广播唤醒数 = %d, want ≥1", woke)
	}

	select {
	case rec := <-done:
		data := decodeMultiOpenData(t, rec)
		if data["enabled"] != false {
			t.Fatalf("广播后应立刻返回最新状态 enabled=false，实际 %v", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("广播后长轮询没有立刻返回（会退化成等超时，运营会以为开关没生效）")
	}
}

// TestHandleMultiOpenWaitHangsWhenInSync 认知一致时**必须挂住**，直到超时才返回。
//
// 反向锁死"忙轮询"：如果实现改成"每次都立刻返回"，登录器循环就会以每秒几百次的频率
// 猛打认证服务（比没有推送严重得多）。这里用挂起上限来区分"挂住了"和"立刻返回了"。
func TestHandleMultiOpenWaitHangsWhenInSync(t *testing.T) {
	shortMultiOpenWait(t, 150*time.Millisecond)
	s := newMultiOpenTestService(func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(true)} })

	start := time.Now()
	rec := httptest.NewRecorder()
	s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet,
		"/launcher/v1/multi-open/wait?known=true", nil))
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("认知一致时应当挂起等待，实际 %v 就返回了 —— 会打成忙轮询", elapsed)
	}
	if data := decodeMultiOpenData(t, rec); data["enabled"] != true {
		t.Fatalf("超时后应返回当前状态 enabled=true，实际 %v", data)
	}

	// "没带 known"（将来别家的客户端）：一律挂起，绝不立刻返回。
	// 缺参数变成"每次都立刻返回"是最危险的一种退化。
	start = time.Now()
	rec = httptest.NewRecorder()
	s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open/wait", nil))
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("不带 known 的请求也必须挂起，实际 %v 就返回了", elapsed)
	}

	// known 取值无法识别（乱填）：同样按"不短路"处理。
	start = time.Now()
	rec = httptest.NewRecorder()
	s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open/wait?known=maybe", nil))
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("known 无法识别时必须按不短路处理（挂起），实际 %v 就返回了", elapsed)
	}
}

// TestHandleMultiOpenWaitShortCircuitsStaleKnown 客户端认知已过期 → 立刻返回，不挂起。
//
// 覆盖"启动自检拉短端点"与"长轮询真正挂上"之间的那个窗口：运营刚好在那几百毫秒里
// 点了开关，广播是收不到的（还没订阅），只能靠这个短路把状态补齐。
func TestHandleMultiOpenWaitShortCircuitsStaleKnown(t *testing.T) {
	shortMultiOpenWait(t, 30*time.Second) // 留长：如果没短路，本用例会挂到超时

	cases := []struct {
		name        string
		fn          MultiOpenFunc
		known       string
		wantEnabled bool
	}{
		{"服务端明确关闭 + 客户端还以为允许 → 立刻回 false",
			func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(false)} }, "true", false},
		{"服务端明确允许 + 客户端以为是关闭（运营刚改回来）→ 立刻回 true",
			func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(true)} }, "false", true},
		{"known 写成数字 1 也要认（与服务端解析口径一致）",
			func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(false)} }, "1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newMultiOpenTestService(tc.fn)
			start := time.Now()
			rec := httptest.NewRecorder()
			s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet,
				"/launcher/v1/multi-open/wait?known="+tc.known, nil))
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("认知已过期（客户端与服务端明确值不同）时必须立刻返回，实际挂了 %v", elapsed)
			}
			if data := decodeMultiOpenData(t, rec); data["enabled"] != tc.wantEnabled {
				t.Fatalf("应返回 enabled=%v，实际 %v", tc.wantEnabled, data)
			}
		})
	}
}

// TestHandleMultiOpenWaitKnownLessClientStillGetsState 不带 known 的老式客户端：
// 虽然不会被短路，但"广播 → 立刻返回"这条路照样通，状态永远是最新的。
func TestHandleMultiOpenWaitKnownLessClientStillGetsState(t *testing.T) {
	shortMultiOpenWait(t, 30*time.Second)
	state := MultiOpenState{Enabled: moBoolPtr(false)}
	s := newMultiOpenTestService(func() MultiOpenState { return state })

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open/wait", nil))
		done <- rec
	}()
	deadline := time.Now().Add(3 * time.Second)
	for s.MultiOpenWaiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.MultiOpenWaiting() == 0 {
		t.Fatal("不带 known 的长轮询没有挂起")
	}
	state = MultiOpenState{Enabled: moBoolPtr(true)} // 运营改回"允许"
	s.MultiOpenChanged()

	select {
	case rec := <-done:
		if data := decodeMultiOpenData(t, rec); data["enabled"] != true {
			t.Fatalf("广播后应返回最新状态 enabled=true，实际 %v", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("不带 known 的长轮询收到广播后也必须立刻返回")
	}
}

// TestHandleMultiOpenWaitNilHubDegradesToShort 未装配 hub（老装配路径/测试）时，
// 长轮询必须退化成"立刻返回当前状态"（行为同短端点），而不是把登录器卡在这里。
func TestHandleMultiOpenWaitNilHubDegradesToShort(t *testing.T) {
	shortMultiOpenWait(t, 30*time.Second)
	s := &Service{log: discardLogger()} // 没有 hub
	s.SetMultiOpen(func() MultiOpenState { return MultiOpenState{Enabled: moBoolPtr(false)} })

	start := time.Now()
	rec := httptest.NewRecorder()
	// 故意不带 known：即便这样也必须立刻返回（hub 都没有，挂起没有意义）。
	s.handleMultiOpenWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open/wait", nil))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("未装配 hub 时必须立刻返回，实际挂了 %v", elapsed)
	}
	if data := decodeMultiOpenData(t, rec); data["enabled"] != false {
		t.Fatalf("应返回 enabled=false，实际 %v", data)
	}
}

// ============================================================================
//  纯函数（三态比较 / known 解析）
// ============================================================================

// TestMultiOpenKnownStale 短路判定表。每一条写反的后果都很直接：
//   - nil（后台没设置过）被判成"过期" → 客户端每次都立刻拿到一个不带 enabled 的响应，
//     而它的 known 又不会被更新 → **忙轮询**（每秒上千次请求）；
//   - 明确值不同却没被判成"过期" → 已打开的登录器要多等一整个 25 秒周期才变灰。
func TestMultiOpenKnownStale(t *testing.T) {
	cases := []struct {
		name     string
		known    *bool
		knownSet bool
		current  MultiOpenState
		want     bool
	}{
		{"没带 known：一律不短路（缺参数不能变成忙轮询）", nil, false, MultiOpenState{Enabled: moBoolPtr(true)}, false},
		{"没带 known + 没设置过：不短路", nil, false, MultiOpenState{}, false},
		{"后台从未设置过：不算过期（这一条是防忙轮询的关键）", moBoolPtr(true), true, MultiOpenState{}, false},
		{"后台从未设置过 + 客户端一无所知：不算过期", nil, true, MultiOpenState{}, false},
		{"明确 false vs 客户端以为 true：过期", moBoolPtr(true), true, MultiOpenState{Enabled: moBoolPtr(false)}, true},
		{"明确 true vs 客户端以为 false：过期", moBoolPtr(false), true, MultiOpenState{Enabled: moBoolPtr(true)}, true},
		{"明确 true vs 客户端一无所知：过期（立刻告知）", nil, true, MultiOpenState{Enabled: moBoolPtr(true)}, true},
		{"明确 false vs 客户端一无所知：过期（立刻告知）", nil, true, MultiOpenState{Enabled: moBoolPtr(false)}, true},
		{"明确 true vs 客户端以为 true：不过期（挂起等）", moBoolPtr(true), true, MultiOpenState{Enabled: moBoolPtr(true)}, false},
		{"明确 false vs 客户端以为 false：不过期（挂起等）", moBoolPtr(false), true, MultiOpenState{Enabled: moBoolPtr(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := multiOpenKnownStale(tc.known, tc.knownSet, tc.current); got != tc.want {
				t.Fatalf("multiOpenKnownStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMultiOpenStateDiffers 三态逐位比较：nil 与 true/false 是**不同的**状态
// （订阅前后用它对账，避免"订阅那一瞬间刚好变了"被漏掉）。
func TestMultiOpenStateDiffers(t *testing.T) {
	cases := []struct {
		name string
		a, b MultiOpenState
		want bool
	}{
		{"都是没设置过：没变", MultiOpenState{}, MultiOpenState{}, false},
		{"都是 true：没变", MultiOpenState{Enabled: moBoolPtr(true)}, MultiOpenState{Enabled: moBoolPtr(true)}, false},
		{"都是 false：没变", MultiOpenState{Enabled: moBoolPtr(false)}, MultiOpenState{Enabled: moBoolPtr(false)}, false},
		{"true → false：变了", MultiOpenState{Enabled: moBoolPtr(true)}, MultiOpenState{Enabled: moBoolPtr(false)}, true},
		{"false → true：变了", MultiOpenState{Enabled: moBoolPtr(false)}, MultiOpenState{Enabled: moBoolPtr(true)}, true},
		{"没设置过 → 明确值：变了", MultiOpenState{}, MultiOpenState{Enabled: moBoolPtr(true)}, true},
		{"明确值 → 没设置过：变了", MultiOpenState{Enabled: moBoolPtr(false)}, MultiOpenState{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := multiOpenStateDiffers(tc.a, tc.b); got != tc.want {
				t.Fatalf("multiOpenStateDiffers = %v, want %v", got, tc.want)
			}
			if got := multiOpenStateDiffers(tc.b, tc.a); got != tc.want {
				t.Fatalf("multiOpenStateDiffers 必须对称，反向 = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseMultiOpenKnown 参数解析：只认 true/false（含 1/0 与大小写、空白），
// 其余一律"不短路"。
func TestParseMultiOpenKnown(t *testing.T) {
	cases := []struct {
		raw     string
		wantSet bool
		wantVal bool
	}{
		{"true", true, true},
		{" TRUE ", true, true},
		{"1", true, true},
		{"false", true, false},
		{"False", true, false},
		{"0", true, false},
		{"", false, false},
		{"maybe", false, false},
		{"null", false, false},
	}
	for _, tc := range cases {
		got, set := parseMultiOpenKnown(tc.raw)
		if set != tc.wantSet {
			t.Fatalf("parseMultiOpenKnown(%q) set = %v, want %v", tc.raw, set, tc.wantSet)
		}
		if !set {
			if got != nil {
				t.Fatalf("parseMultiOpenKnown(%q) 应为 nil 指针", tc.raw)
			}
			continue
		}
		if got == nil || *got != tc.wantVal {
			t.Fatalf("parseMultiOpenKnown(%q) = %v, want %v", tc.raw, got, tc.wantVal)
		}
	}
}

// TestMultiOpenDataNeverSendsFalseForUnset 直击三态契约的**最小**断言：
// "没设置过"序列化出来必须**没有** enabled 这个键（而不是 enabled:false）。
func TestMultiOpenDataNeverSendsFalseForUnset(t *testing.T) {
	raw, err := json.Marshal(multiOpenData(MultiOpenState{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "{}" {
		t.Fatalf("「从未设置过」必须是 {}, 实际 %s", raw)
	}
	if strings.Contains(string(raw), "enabled") {
		t.Fatalf("「从未设置过」绝不能下发 enabled（哪怕是 false）：%s", raw)
	}
	// 与短端点共用同一个函数，所以短端点也不可能漂移。
	short := httptest.NewRecorder()
	s := newMultiOpenTestService(nil)
	s.handleMultiOpen(short, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open", nil))
	if !strings.Contains(short.Body.String(), `"data":{}`) {
		t.Fatalf("短端点「从未设置过」应为空 data，实际 %s", short.Body.String())
	}
}
