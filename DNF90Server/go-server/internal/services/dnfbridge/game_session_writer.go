package dnfbridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// This file moves socket writes off the session's single event loop.
//
// Why it exists (live measurement, 2026-09-25, production server):
//
// Connection game-000048 (character 2) ACKed a 返回城镇 click at 07:40:29.958Z
// and then logged not one further line for 48 seconds -- not even the
// 3-second security-status heartbeat this client sends while idle. At
// 07:41:18.926Z the entire town rebuild landed in one burst (op584 twice, the
// town actor packets, the op9 party frames). The route had not been lost: it was
// parked behind socket writes to a client that had stopped reading, at up to
// gameConnWriteBudget (5s) per frame. Squadmates on healthy connections kept
// playing through the whole window, so this was one character's session, not
// the server.
//
// Every one of those writes runs on the session's only event loop, so the park
// also stops every unrelated request queued behind it -- the settlement exit,
// the consumable request after it (the operator's 无色技能用不了 is this same
// stall, exactly as he suspected: "我感觉就是卡住了导致的"), and chat. Movement
// keeps working because it does not share that path, which is why the symptom
// looked like "点返回城镇完全没反应" while the character could still be seen
// running around.
//
// The design keeps two things the current code depends on:
//
//   - Writes stay ORDERED. One writer goroutine drains one FIFO queue, so the
//     byte stream is identical to the synchronous writes it replaces.
//   - A write failure that is reported immediately still reaches its caller
//     synchronously. A write error is load-bearing here, not cosmetic:
//     altar_runtime.go arms a server-side retry when the dynamic op38 write
//     fails, and altar_runtime_test.go pins "failed write committed or lost
//     retry". Only the stall case is handed to the writer goroutine, so those
//     recovery paths keep working.
type gameSessionWriter struct {
	service *Service
	session *gameSession

	queue  chan *gameWriterFrame
	stopCh chan struct{}
	exited chan struct{}

	stopOnce sync.Once
	// queued counts payload bytes currently waiting in queue.
	queued atomic.Int64
	closed atomic.Bool
	// stalling is set once a producer has spent its whole gameWriterSyncBudget
	// waiting, and cleared by the next completed write. While it is set, later
	// producers enqueue and return instead of each paying the budget again, so
	// a burst aimed at a stalled client costs the event loop nothing.
	stalling atomic.Bool

	mu sync.Mutex
	// stalledAt is when the current stall began, zero while healthy.
	stalledAt time.Time
	// terminalErr is latched when a stall has outlived gameWriterStallLimit.
	// Every later submit returns it, which is what lets the caller's existing
	// fatal path tear the session down exactly as the 5s write deadline does
	// today -- just without parking the event loop for every frame.
	terminalErr error
}

type gameWriterFrame struct {
	payload []byte
	result  chan error
}

const (
	// gameWriterQueueDepth bounds how many frames one client may have queued.
	gameWriterQueueDepth = 512
	// gameWriterQueueBytes bounds the queued payload bytes for one client. A
	// client that stopped reading must not be able to grow server memory.
	gameWriterQueueBytes = 1 << 20
	// gameWriterSyncBudget is how long a producer on the session event loop may
	// wait for its own frame to reach the socket. It is the whole point of this
	// file: the loop may be parked for this long, never for
	// gameConnWriteBudget per frame.
	gameWriterSyncBudget = 250 * time.Millisecond
	// gameWriterStallLimit is how long one frame may stay unwritten before the
	// session's write path reports the stall to its callers. It deliberately
	// equals gameConnWriteBudget: the write deadline it replaces also gave the
	// client 5 seconds before the session was torn down.
	gameWriterStallLimit = gameConnWriteBudget
	// gameWriterStallPoll is how often the writer checks that limit.
	gameWriterStallPoll = 100 * time.Millisecond
)

var (
	errGameWriterClosed  = errors.New("dnf game session writer is closed")
	errGameWriterBacklog = errors.New("dnf game session writer backlog is full")
	errGameWriterStalled = errors.New("dnf game session write stalled: the client stopped reading")
)

// startGameSessionWriter puts a writer goroutine behind one session's socket.
//
// Sessions with no connection are left on the direct write path: they cannot
// write anything, and the tests that build such sessions assert the exact
// "connection is unavailable" error this must not change.
func (s *Service) startGameSessionWriter(ctx context.Context, session *gameSession) {
	if s == nil || session == nil || session.conn == nil || session.writer != nil {
		return
	}
	writer := &gameSessionWriter{
		service: s,
		session: session,
		queue:   make(chan *gameWriterFrame, gameWriterQueueDepth),
		stopCh:  make(chan struct{}),
		exited:  make(chan struct{}),
	}
	session.writer = writer
	go writer.run(ctx)
}

// stopGameSessionWriter drains what is already queued and retires the writer.
//
// ctx bounds the whole drain, exactly like the event loop's own shutdown wait,
// so a session whose client has stopped reading cannot stall connection cleanup.
func (s *Service) stopGameSessionWriter(ctx context.Context, session *gameSession) {
	if session == nil || session.writer == nil {
		return
	}
	writer := session.writer
	writer.closed.Store(true)
	writer.stopOnce.Do(func() { close(writer.stopCh) })
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), gameWriterSyncBudget)
		defer cancel()
	}
	select {
	case <-writer.exited:
	case <-ctx.Done():
	}
}

// submit hands one already-encoded frame to the writer.
//
// It returns the frame's real write error whenever that error arrives inside
// gameWriterSyncBudget, which covers every fast failure (a closed socket, a
// rejected write, a test double that fails on demand). It returns nil once the
// budget expires, because the client -- not the server -- is what is behind;
// that is the case the event loop must stop paying for.
func (w *gameSessionWriter) submit(payload []byte) error {
	if w == nil || len(payload) == 0 {
		return nil
	}
	// terminalFailure is checked before closed so a stall reports its own
	// diagnostic error rather than the generic "writer is closed".
	if err := w.terminalFailure(); err != nil {
		return err
	}
	if w.closed.Load() {
		return errGameWriterClosed
	}
	size := int64(len(payload))
	if w.queued.Load()+size > gameWriterQueueBytes {
		return errGameWriterBacklog
	}
	// The caller's slice is not guaranteed to outlive this call -- writeGameConn
	// used to consume it before returning -- so the frame owns a copy.
	frame := &gameWriterFrame{
		payload: append([]byte(nil), payload...),
		result:  make(chan error, 1),
	}
	w.queued.Add(size)
	if err := w.enqueue(frame); err != nil {
		w.queued.Add(-size)
		return err
	}
	if w.stalling.Load() {
		// Already known to be behind. Enqueue and hand control straight back: a
		// burst must not pay gameWriterSyncBudget once per frame.
		return nil
	}
	timer := time.NewTimer(gameWriterSyncBudget)
	defer timer.Stop()
	select {
	case err := <-frame.result:
		return err
	case <-timer.C:
		w.noteStall()
		return nil
	case <-w.exited:
		return errGameWriterClosed
	}
}

func (w *gameSessionWriter) enqueue(frame *gameWriterFrame) error {
	select {
	case w.queue <- frame:
		return nil
	case <-w.exited:
		return errGameWriterClosed
	default:
	}
	timer := time.NewTimer(gameWriterSyncBudget)
	defer timer.Stop()
	select {
	case w.queue <- frame:
		return nil
	case <-w.exited:
		return errGameWriterClosed
	case <-timer.C:
		return errGameWriterBacklog
	}
}

func (w *gameSessionWriter) run(ctx context.Context) {
	defer close(w.exited)
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(gameWriterStallPoll)
	defer ticker.Stop()
	for {
		select {
		case frame := <-w.queue:
			w.queued.Add(-int64(len(frame.payload)))
			err := w.deliver(frame.payload)
			frame.deliver(err)
			// A completed attempt -- success or failure -- means the socket
			// answered, so the stall is over. The error is this frame's alone;
			// the writer keeps serving later frames, exactly like the
			// synchronous path where every write reports its own result.
			w.clearStall()
		case <-ticker.C:
			if err := w.expireStall(); err != nil {
				w.fail(err)
				return
			}
		case <-ctx.Done():
			w.fail(errGameWriterClosed)
			return
		case <-w.stopCh:
			w.drain()
			return
		}
	}
}

// deliver performs the one blocking socket write, with the same deadline and
// the same raw error the synchronous path returned.
func (w *gameSessionWriter) deliver(payload []byte) error {
	session := w.session
	if session == nil || session.conn == nil {
		return errors.New("dnf game connection is unavailable")
	}
	start := time.Now()
	if err := session.conn.SetWriteDeadline(time.Now().Add(gameConnWriteBudget)); err != nil {
		return err
	}
	defer func() { _ = session.conn.SetWriteDeadline(time.Time{}) }()
	_, err := session.conn.Write(payload)
	if elapsed := time.Since(start); elapsed >= gameConnWriteBudget {
		// Only reachable for a client that stopped reading. Nobody may be
		// waiting for this result any more, so it must not vanish silently:
		// it is the difference between "slow" and "gone".
		w.logStalledWrite(elapsed, len(payload), err)
	}
	return err
}

func (frame *gameWriterFrame) deliver(err error) {
	if frame == nil || frame.result == nil {
		return
	}
	select {
	case frame.result <- err:
	default:
	}
}

func (w *gameSessionWriter) noteStall() {
	w.mu.Lock()
	if w.stalledAt.IsZero() {
		w.stalledAt = time.Now()
	}
	w.mu.Unlock()
	w.stalling.Store(true)
}

func (w *gameSessionWriter) clearStall() {
	w.mu.Lock()
	w.stalledAt = time.Time{}
	w.mu.Unlock()
	w.stalling.Store(false)
}

// expireStall latches the terminal error once a stall has outlived
// gameWriterStallLimit, so the caller's existing fatal handling retires the
// session instead of the frames piling up behind a client that is gone.
func (w *gameSessionWriter) expireStall() error {
	w.mu.Lock()
	stalledAt := w.stalledAt
	w.mu.Unlock()
	if stalledAt.IsZero() || time.Since(stalledAt) < gameWriterStallLimit {
		return nil
	}
	return errGameWriterStalled
}

func (w *gameSessionWriter) terminalFailure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminalErr
}

func (w *gameSessionWriter) fail(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if w.terminalErr == nil {
		w.terminalErr = err
	}
	w.mu.Unlock()
	w.closed.Store(true)
	if errors.Is(err, errGameWriterStalled) || errors.Is(err, errGameWriterClosed) {
		return
	}
	w.logFailure(err)
}

// drain writes what is already queued so a closing session still gets its last
// frames out, then returns. Frames queued after the drain starts are refused by
// submit's closed check.
func (w *gameSessionWriter) drain() {
	for {
		select {
		case frame := <-w.queue:
			w.queued.Add(-int64(len(frame.payload)))
			frame.deliver(w.deliver(frame.payload))
		default:
			return
		}
	}
}

func (w *gameSessionWriter) logStalledWrite(elapsed time.Duration, size int, err error) {
	if w == nil || w.service == nil || w.session == nil {
		return
	}
	session := w.session
	w.service.logGameEvent(session, "game-write-stalled-on-client",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"elapsed_ms", elapsed.Milliseconds(),
		"frame_len", size,
		"error", err,
		"reason", "client_stopped_reading_write_hit_deadline")
	// A stalled write to a raid member is the server-side trace of THAT member's
	// own freeze, so it belongs in the raid cost summary (raid_cost_summary.go).
	if raidKey := w.service.runtimeRaidKeyForMember(session.selectedCharacterID); raidKey != 0 {
		w.service.raidCostMember(raidKey, session.selectedCharacterID)
		w.service.noteRaidCostWriteStall(raidKey, elapsed)
	}
}

func (w *gameSessionWriter) logFailure(err error) {
	if w == nil || w.service == nil || w.session == nil {
		return
	}
	session := w.session
	w.service.logGameEvent(session, "game-write-path-failed",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"error", err,
		"reason", "session_write_path_retired")
}
