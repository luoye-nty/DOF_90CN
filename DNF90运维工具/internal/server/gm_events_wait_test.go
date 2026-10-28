package server

// GM 事件长轮询的测试（2026-09-28 新增）。
//
// 背景：到账绿字（「通过泡点获得 100 点券」）以前只能靠登录器每 10 秒轮询一次，
// 玩家平均要等 5 秒、最坏 10 秒以上才看到提示 —— 线上反馈的"泡点不实时通知"。
// 现在改成"落库即唤醒对应账号的长轮询"。
//
// 这里锁住两件写错后果最严重的事：
//  1. **账号隔离**：A 的点券到账只能唤醒 A 的长轮询。写错就是把 B 的点券通知推给 A，
//     玩家会看到"我没收到点券却弹了到账提示"，属于最恶劣的一类 bug；
//  2. **不阻塞**：GMEventAdded 由 store 在 INSERT 成功后同步调用，
//     它一旦会阻塞（比如往满 channel 上硬写），写事件的主路径就会被拖住。

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// discardLogger 一个丢弃全部输出的 logger：测试里 Service 直接构造时 log 是 nil，
// 走日志的那一步会空指针；生产环境里由 New() 注入。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestGMEventHubIsolatesAccounts 只唤醒目标账号，别的账号一个都不能被惊动。
func TestGMEventHubIsolatesAccounts(t *testing.T) {
	hub := NewGMEventHub()
	aCh, unsubA := hub.Subscribe("account-a")
	defer unsubA()
	bCh, unsubB := hub.Subscribe("account-b")
	defer unsubB()

	if woke := hub.Broadcast("account-a"); woke != 1 {
		t.Fatalf("唤醒账号 A 的数量 = %d, want 1", woke)
	}
	select {
	case <-aCh:
	default:
		t.Fatal("账号 A 的长轮询没被唤醒")
	}
	select {
	case <-bCh:
		t.Fatal("账号 B 的长轮询被 A 的事件唤醒了 —— 账号隔离漏了，玩家会看到别人的到账提示")
	default:
	}

	// 没人等的账号：广播不能报错、也不能阻塞。
	if woke := hub.Broadcast("account-nobody"); woke != 0 {
		t.Fatalf("无人订阅时唤醒数 = %d, want 0", woke)
	}
}

// TestGMEventHubMultipleWaitersOneAccount 同一账号可能同时挂着多个登录器
//（双开/多机登录），全部都要被唤醒。
func TestGMEventHubMultipleWaitersOneAccount(t *testing.T) {
	hub := NewGMEventHub()
	first, unsubFirst := hub.Subscribe("account-a")
	defer unsubFirst()
	second, unsubSecond := hub.Subscribe("account-a")
	defer unsubSecond()

	if got := hub.SubscriberCount(); got != 2 {
		t.Fatalf("订阅数 = %d, want 2", got)
	}
	if woke := hub.Broadcast("account-a"); woke != 2 {
		t.Fatalf("唤醒数 = %d, want 2（同账号多个登录器都要收到）", woke)
	}
	for name, ch := range map[string]<-chan struct{}{"first": first, "second": second} {
		select {
		case <-ch:
		default:
			t.Fatalf("%s 没被唤醒", name)
		}
	}

	// 退订之后不能再被唤醒（否则会往已关闭的 channel 上投递）。
	// 注意：上面的 select 已经把两个信号消费掉了，这里**不能**再读 channel ——
	// 空 channel 上的读会永久阻塞（这里踩过一次：整包测试挂到 10 分钟超时）。
	unsubFirst()
	unsubSecond()
	if woke := hub.Broadcast("account-a"); woke != 0 {
		t.Fatalf("退订后唤醒数 = %d, want 0", woke)
	}
	if got := hub.SubscriberCount(); got != 0 {
		t.Fatalf("退订后订阅数 = %d, want 0（不能泄漏连接）", got)
	}
	// 退订后 channel 不应再收到任何东西（非阻塞确认）。
	for name, ch := range map[string]<-chan struct{}{"first": first, "second": second} {
		select {
		case <-ch:
			t.Fatalf("退订后 %s 仍然收到了信号", name)
		default:
		}
	}
}

// TestGMEventHubBroadcastNeverBlocks 同一个订阅者在信号没被消费前重复广播，
// 必须立刻返回（非阻塞投递），否则写事件的主路径会被拖死。
func TestGMEventHubBroadcastNeverBlocks(t *testing.T) {
	hub := NewGMEventHub()
	_, unsub := hub.Subscribe("account-a")
	defer unsub()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 不消费第一次的信号，看后续广播会不会卡住。
		for i := 0; i < 100; i++ {
			hub.Broadcast("account-a")
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("GMEventAdded 阻塞了 —— 它是旁路，绝不能拖住写事件的主路径")
	}
}

// TestGMEventAddedToleratesNilHub 未装配 hub（老装配路径/测试）时调用不能 panic。
func TestGMEventAddedToleratesNilHub(t *testing.T) {
	bare := &Service{}
	bare.GMEventAdded("account-a") // 不应 panic
	if bare.GMEventWaiting() != 0 {
		t.Fatal("未装配 hub 时等待数应为 0")
	}
	nilService := (*Service)(nil)
	nilService.GMEventAdded("account-a") // 不应 panic
}

// TestHandleGMEventsWaitRejectsBadRequests 参数/方法校验：这些路径不碰数据库，
// 可以在单测里直接覆盖。
func TestHandleGMEventsWaitRejectsBadRequests(t *testing.T) {
	s := &Service{log: discardLogger()}

	rec := httptest.NewRecorder()
	s.handleGMEventsWait(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/gm-events/wait", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.handleGMEventsWait(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/gm-events/wait", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 ticket 状态码 = %d, want 400", rec.Code)
	}
}
