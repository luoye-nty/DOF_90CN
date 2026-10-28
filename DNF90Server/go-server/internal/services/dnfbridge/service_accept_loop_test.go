package dnfbridge

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// 回归：频道热重载关闭一个游戏端口时只 Close listener、不取消 runCtx，
// acceptLoop 必须靠 closed 错误退出，否则会变成"一核 100% 空转 + 每圈一条 warn"。
func TestAcceptLoopStopsWhenGamePortListenerIsClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	service.wg.Add(1)
	go service.acceptLoop(context.Background(), listener, func(context.Context, net.Conn) {})
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		service.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop kept spinning after the listener was closed (CPU burn + log flood)")
	}
}

// 回归：永久性且非 closed 的 Accept 错误也不能无限重试。
func TestAcceptLoopStopsAfterRepeatedFailures(t *testing.T) {
	listener := &alwaysFailingListener{}
	service := &Service{}
	service.wg.Add(1)
	go service.acceptLoop(context.Background(), listener, func(context.Context, net.Conn) {})
	done := make(chan struct{})
	go func() {
		service.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("accept loop never gave up on a permanently failing listener")
	}
	if calls := listener.calls.Load(); calls == 0 || calls > currentAcceptLoopFailureLimit+2 {
		t.Fatalf("accept attempts=%d want 1..%d", calls, currentAcceptLoopFailureLimit+2)
	}
}

type alwaysFailingListener struct {
	calls atomic.Int64
}

func (l *alwaysFailingListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	return nil, errors.New("injected permanent accept failure")
}

func (l *alwaysFailingListener) Close() error   { return nil }
func (l *alwaysFailingListener) Addr() net.Addr { return fakeAddr("failing") }
