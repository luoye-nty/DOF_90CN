package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stalledWriteConn models the live 2026-09-25 stall measured on the production
// server: the client stopped reading (8 clients on one home connection, the
// machine busy), so the socket's send buffer filled and Write blocked.
//
// The write deadline is load-bearing for this double. gameConnWriteBudget is
// what turns "blocked" into "blocked for at most 5 seconds", and it is the only
// reason this test terminates on a build with no session writer: without it the
// blocked write would park the session's event loop for the whole test binary
// timeout instead of failing an assertion.
type stalledWriteConn struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	deadline time.Time
	// entered is signalled by the first Write that actually reaches the socket,
	// so the test knows the stall is in progress rather than merely queued.
	entered   chan struct{}
	release   chan struct{}
	releaseDo sync.Once
	writes    atomic.Int64
}

func newStalledWriteConn() *stalledWriteConn {
	return &stalledWriteConn{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (c *stalledWriteConn) releaseAll() {
	c.releaseDo.Do(func() { close(c.release) })
}

func (c *stalledWriteConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *stalledWriteConn) Read(_ []byte) (int, error)        { return 0, io.EOF }
func (c *stalledWriteConn) Close() error                      { return nil }
func (c *stalledWriteConn) LocalAddr() net.Addr               { return fakeAddr("local") }
func (c *stalledWriteConn) RemoteAddr() net.Addr              { return fakeAddr("remote") }
func (c *stalledWriteConn) SetDeadline(_ time.Time) error     { return nil }
func (c *stalledWriteConn) SetReadDeadline(_ time.Time) error { return nil }

func (c *stalledWriteConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *stalledWriteConn) Write(payload []byte) (int, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	var expiry <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expiry = timer.C
	}
	select {
	case <-c.release:
	case <-expiry:
		return 0, timeoutNetError{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes.Add(1)
	return c.buf.Write(payload)
}

// failFastWriteConn fails immediately, the way a closed socket does
// ("use of closed network connection"). The distinction from stalledWriteConn is
// the whole point of the session writer's design: a fast failure must keep
// reaching its caller synchronously, because production arms recovery work off
// that error (altar_runtime.go arms a server-side retry when the op38 write
// fails), while a stalled write must not park the session's event loop.
type failFastWriteConn struct {
	err    error
	writes atomic.Int64
}

func (c *failFastWriteConn) Read(_ []byte) (int, error)        { return 0, io.EOF }
func (c *failFastWriteConn) Close() error                      { return nil }
func (c *failFastWriteConn) LocalAddr() net.Addr               { return fakeAddr("local") }
func (c *failFastWriteConn) RemoteAddr() net.Addr              { return fakeAddr("remote") }
func (c *failFastWriteConn) SetDeadline(_ time.Time) error     { return nil }
func (c *failFastWriteConn) SetReadDeadline(_ time.Time) error { return nil }
func (c *failFastWriteConn) SetWriteDeadline(_ time.Time) error {
	return nil
}

func (c *failFastWriteConn) Write(_ []byte) (int, error) {
	c.writes.Add(1)
	return 0, c.err
}

func newGameSessionWriterTestSession(t *testing.T, ctx context.Context, conn net.Conn) (*Service, *gameSession) {
	t.Helper()
	service := &Service{}
	session := &gameSession{
		connID:              "stalled-client-write",
		ctx:                 ctx,
		conn:                conn,
		selectedCharacterID: 19,
		characterGeneration: 7,
	}
	if err := service.startGameSessionEvents(ctx, session); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopTestGameSessionEvents(t, service, session) })
	return service, session
}

// TestGameSessionWriteToStalledClientDoesNotParkTheSessionLoop pins the live
// 2026-09-25 failure that made 通关后返回城镇 look dead.
//
// Measured on the production server, connection game-000048 (character 2):
// the 返回城镇 click was ACKed at 07:40:29.958Z, the session then logged not one
// further line for 48 seconds -- not even the 3-second security-status
// heartbeat the client sends when it is idle -- and at 07:41:18.926Z the whole
// town rebuild (op584 x2, the town actor packets, the op9 party frames) arrived
// in one burst. So the route had not been lost; it was parked behind socket
// writes to a client that had stopped reading, at up to gameConnWriteBudget
// (5s) per frame.
//
// Because every write happens on the session's single event loop, that park
// also stops every unrelated request on the same character: the queued
// settlement exit, the consumable request behind it (the operator's "无色技能用
// 不了" is the same stall, as he suspected: "我感觉就是卡住了导致的"), and the
// chat line. Movement kept working because it is not routed through the loop in
// the same way.
func TestGameSessionWriteToStalledClientDoesNotParkTheSessionLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stalled := newStalledWriteConn()
	service, session := newGameSessionWriterTestSession(t, ctx, stalled)
	// Registered after the cleanup above, so it runs first and the shutdown in
	// the cleanup is never left waiting on a write that is still blocked.
	defer stalled.releaseAll()

	// The write has to run INSIDE an event-loop handler, which is where every
	// production write happens. Calling writeGameUpperPacket straight from the
	// test goroutine would block the test goroutine instead and prove nothing.
	firstWrite := make(chan error, 1)
	go func() {
		firstWrite <- service.callGameSession(ctx, session, "blocked-write", func() error {
			return service.writeGameUpperPacket(session, []byte{0x01, 0x02, 0x03})
		})
	}()
	waitTestSignal(t, stalled.entered, "the stalled write never reached the socket")

	// A second, unrelated request on the same session. It must be served even
	// though the client is not reading.
	served := make(chan error, 1)
	start := time.Now()
	go func() {
		served <- service.callGameSession(ctx, session, "unrelated-request", func() error { return nil })
	}()
	// 1s is the assertion window: far below the 5s a blocked write is allowed to
	// park the loop today, far above the sub-300ms a session writer may spend
	// before it hands the stall to its own goroutine.
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("unrelated request failed while the client was stalled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("session event loop was parked %.0fms behind a stalled client write; "+
			"every later request on this character (返回城镇, 物品/无色, 聊天) waits behind it",
			float64(time.Since(start).Milliseconds()))
	}

	stalled.releaseAll()
	select {
	case err := <-firstWrite:
		if err != nil {
			t.Fatalf("the write to the recovered client failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked write call did not return after the client resumed reading")
	}
	// The handler is allowed to return before its frame has crossed the socket --
	// that is the entire point of the writer -- so wait for the bytes to land
	// before asserting on them.
	deadline := time.Now().Add(2 * time.Second)
	for !bytes.Equal(stalled.written(), []byte{0x01, 0x02, 0x03}) {
		if time.Now().After(deadline) {
			t.Fatalf("stalled-then-released write body = %x, want 010203", stalled.written())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestGameSessionWriterKeepsFrameOrder is the ordering guard for moving socket
// writes off the event loop. The client parses an ordered stream, so a writer
// goroutine that reordered frames would corrupt every session it touched.
func TestGameSessionWriterKeepsFrameOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &Service{}
	connection := &bufferConn{}
	session := &gameSession{
		connID:              "writer-order",
		ctx:                 ctx,
		conn:                connection,
		selectedCharacterID: 19,
		characterGeneration: 7,
	}
	if err := service.startGameSessionEvents(ctx, session); err != nil {
		t.Fatal(err)
	}
	defer stopTestGameSessionEvents(t, service, session)

	var want []byte
	for index := 0; index < 64; index++ {
		frame := []byte{byte(index), byte(index >> 8), 0xAA}
		want = append(want, frame...)
		if err := service.writeGameUpperPacket(session, frame); err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
	}
	stream := connection.write.Bytes()
	if !bytes.Equal(stream, want) {
		t.Fatalf("writer reordered frames:\n got %x\nwant %x", stream, want)
	}
}

// TestGameSessionWriteFailureStillReachesItsCallerSynchronously guards the
// semantics the session writer must not break.
//
// A write error is load-bearing in production, not cosmetic: altar_runtime.go
// arms a server-side retry when the dynamic op38 write fails
// ("dynamic_monster_op38_write_failed_server_retry_armed"), and
// altar_runtime_test.go pins "failed write committed or lost retry". If moving
// writes to a goroutine made the error asynchronous, those recovery paths would
// silently stop arming and the safety net would be gone without a single test
// failing. A failure that is reported immediately must therefore still be
// reported immediately.
func TestGameSessionWriteFailureStillReachesItsCallerSynchronously(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wantErr := errors.New("use of closed network connection")
	failing := &failFastWriteConn{err: wantErr}
	service, session := newGameSessionWriterTestSession(t, ctx, failing)

	err := service.writeGameUpperPacket(session, []byte{0x07})
	if !errors.Is(err, wantErr) {
		t.Fatalf("write error = %v, want %v", err, wantErr)
	}
}
