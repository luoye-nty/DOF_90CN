package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"longheng.io/server/internal/platform/eventloop"
)

const (
	gameSessionEventQueueSize = 512
	gameSessionShutdownWait   = 2 * createWriteTimeout
	// gameSessionIdlePriorityQueueSize 是"本包不会用到"的高/低优先级队列容量。
	//
	// 本包全部事件提交点（postGameSessionEvent / callGameSession /
	// postGameSessionCharacterEvent）都只用 eventloop.PriorityMedium —— 全仓 grep
	// PriorityHigh|PriorityLow 在 dnfbridge 下零匹配。而每条循环都会按 QueueSize
	// 预分配三条优先级通道，于是这两条 512 深的通道从创建到销毁一次都不会被写入。
	// 按 amd64 上 Event 约 96 字节算：2 × 512 × 96 ≈ 96 KiB/会话，1000 会话约 96 MB。
	//
	// 取 8 而不是 0：仍然保底容纳一次小突发；将来真要用高/低优先级时，也只是更早
	// 触发 SubmitBlocking 的 1ms 重试退避（不会死锁、不会丢事件），把这里调回
	// gameSessionEventQueueSize 即可恢复原容量。
	gameSessionIdlePriorityQueueSize = 8
	// slowEventProbeThreshold 是 2026-09-26「处处有延迟感」排查用的临时慢事件阈值。
	//
	// 原本该处用 createWriteTimeout（3s）。3s 只能抓到秒级以上的单次操作，而玩家
	// 感知的「处处卡」是 200ms~1s 量级的高频现象：同一份 packet_log.txt 里 200ms
	// 门限的 game-slow-upper-request 已经抓到选角耗时 1342ms，而 3s 门限一条都不报
	// （两个 server-*.stdout.log 全量 grep "slow threshold" 零命中）。
	//
	// 临时下调到 200ms 只为把这一段点名，纯诊断：只多打 WARN、只多加 slow 计数，
	// 不改变任何行为分支、不改变超时、不影响发包顺序。
	// 排查结束后把下面 Options 里的 SlowThreshold 改回 createWriteTimeout 即可。
	slowEventProbeThreshold = 200 * time.Millisecond
)

var errInvalidGameSessionEvent = errors.New("dnf game session event is invalid")

// errGameClientInputRejected marks a handler failure caused by the client's own
// packet - an unrecognised or unproven shape inside an already well-framed
// request - rather than by a server-side write, repository or transport failure.
//
// Inbound game packets are submitted as fatal events because a handler that
// failed in the middle of an ordered mutation may leave the client unable to
// follow the stream any more. A rejected input shape is not that case: nothing
// was mutated and the frame boundary came from the stream splitter, so the only
// effect of the fatal boundary is that one unproven request costs the player the
// entire connection. That is exactly the live 2026-09-12 17:40:04Z incident
// recorded in runtime/logs/packet_log.txt, where the party teleport's 8-byte
// legacy op434 reached the 18-byte native path-report parser, the returned
// validation error crossed this boundary and the game socket was closed - the
// player saw the "网络连接失败" popup plus "无法前往" and had to re-enter the game.
//
// Handlers therefore wrap input-shape rejections with this sentinel: the request
// stays visible in the packet log, the session survives it, and genuine write or
// repository failures keep the original fatal behaviour.
var errGameClientInputRejected = errors.New("game client input rejected")

type gameSessionEvents struct {
	loop   *eventloop.Loop
	active atomic.Bool
}

type gameSessionEventTask struct {
	source              string
	characterID         uint16
	characterGeneration uint64
	async               bool
	fatal               bool
	run                 func() error
}

func (s *Service) startGameSessionEvents(ctx context.Context, session *gameSession) error {
	if s == nil || session == nil {
		return errInvalidGameSessionEvent
	}
	if session.events != nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if session.characterGeneration == 0 {
		session.characterGeneration = 1
	}
	events := &gameSessionEvents{}
	events.loop = eventloop.New(
		"dnf-game-session-"+session.connID,
		eventloop.ProcessorFunc(func(_ context.Context, event eventloop.Event) eventloop.Result {
			task, ok := event.Data.(gameSessionEventTask)
			if !ok || task.run == nil {
				return eventloop.Result{Err: errInvalidGameSessionEvent}
			}
			if task.async {
				if !events.active.Load() {
					return eventloop.Result{}
				}
				if task.characterGeneration != 0 &&
					(task.characterGeneration != session.characterGeneration ||
						task.characterID == 0 ||
						task.characterID != session.selectedCharacterID) {
					s.logGameEvent(session, "game-session-event-stale",
						"source", task.source,
						"event_char_id", task.characterID,
						"selected_char_id", session.selectedCharacterID,
						"event_generation", task.characterGeneration,
						"current_generation", session.characterGeneration,
						"reason", "selected_character_or_generation_changed")
					return eventloop.Result{}
				}
			}
			err := task.run()
			if err != nil && task.fatal {
				if errors.Is(err, errGameClientInputRejected) {
					// The client's own packet was refused before anything was
					// mutated and the frame boundary is intact, so the ordered
					// session stays usable. Later queued packets are still
					// applied: they were framed independently and this rejection
					// did not consume part of their payload.
					s.logGameEvent(session, "game-session-client-input-rejected",
						"source", task.source,
						"char_id", session.selectedCharacterID,
						"character_generation", session.characterGeneration,
						"session_kept_alive", true,
						"error", err)
					return eventloop.Result{}
				}
				events.active.Store(false)
				s.logGameEvent(session, "game-session-async-event-failed",
					"source", task.source,
					"error", err)
				// The synchronous reader used to return the same handler error and
				// close the socket immediately. Preserve that failure boundary after
				// moving inbound packets to asynchronous submission: once one ordered
				// packet fails, later queued packets are stale and the blocked Read
				// call must be released so connection cleanup can run.
				if session.conn != nil {
					_ = session.conn.Close()
				}
			}
			return eventloop.Result{Err: err}
		}),
		eventloop.Options{
			QueueSize:         gameSessionEventQueueSize,
			HighQueueSize:     gameSessionIdlePriorityQueueSize,
			LowQueueSize:      gameSessionIdlePriorityQueueSize,
			CallbackQueueSize: gameSessionEventQueueSize,
			CallbackTimeout:   gameSessionShutdownWait,
			SlowThreshold:     slowEventProbeThreshold,
			Logger:            s.logger,
		},
	)
	if err := events.loop.Start(ctx); err != nil {
		return err
	}
	events.active.Store(true)
	session.events = events
	// The writer is what keeps a client that stopped reading from parking this
	// loop: every frame below is submitted to it instead of written inline.
	s.startGameSessionWriter(ctx, session)
	s.logGameEvent(session, "game-session-events-started",
		"character_generation", session.characterGeneration,
		"queue_size", gameSessionEventQueueSize)
	return nil
}

// postGameSessionEvent submits inbound network work without waiting for its
// repository I/O or response writes to complete. The per-session loop remains
// the single owner, so requests from one character stay ordered while other
// character sessions can execute concurrently. SubmitBlocking is intentional:
// the bounded queue provides TCP backpressure instead of dropping commands.
func (s *Service) postGameSessionEvent(
	ctx context.Context,
	session *gameSession,
	source string,
	run func() error,
) error {
	if run == nil || session == nil {
		return errInvalidGameSessionEvent
	}
	if session.events == nil || session.events.loop == nil {
		return run()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return session.events.loop.SubmitBlocking(ctx, eventloop.Event{
		Type: source,
		Data: gameSessionEventTask{
			source: source,
			async:  true,
			fatal:  true,
			run:    run,
		},
		Priority: eventloop.PriorityMedium,
		Context:  ctx,
	})
}

func (s *Service) beginGameSessionClose(session *gameSession) {
	if session == nil || session.events == nil {
		return
	}
	session.events.active.Store(false)
}

func (s *Service) stopGameSessionEvents(ctx context.Context, session *gameSession) error {
	if session == nil || session.events == nil || session.events.loop == nil {
		return nil
	}
	session.events.active.Store(false)
	if ctx == nil {
		ctx = context.Background()
	}
	return session.events.loop.Stop(ctx)
}

func (s *Service) callGameSession(
	ctx context.Context,
	session *gameSession,
	source string,
	run func() error,
) error {
	if run == nil {
		return errInvalidGameSessionEvent
	}
	if session == nil {
		return errInvalidGameSessionEvent
	}
	if session.events == nil || session.events.loop == nil {
		return run()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callback := make(chan eventloop.Result, 1)
	event := eventloop.Event{
		Type:     source,
		Data:     gameSessionEventTask{source: source, run: run},
		Priority: eventloop.PriorityMedium,
		Callback: callback,
		Context:  ctx,
	}
	if err := session.events.loop.SubmitBlocking(ctx, event); err != nil {
		return err
	}
	select {
	case result := <-callback:
		return result.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) postGameSessionCharacterEvent(
	session *gameSession,
	source string,
	characterID uint16,
	characterGeneration uint64,
	run func() error,
) error {
	if run == nil {
		return errInvalidGameSessionEvent
	}
	if session == nil {
		return errInvalidGameSessionEvent
	}
	if session.events == nil || session.events.loop == nil {
		return run()
	}
	ctx := session.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return session.events.loop.SubmitBlocking(ctx, eventloop.Event{
		Type: source,
		Data: gameSessionEventTask{
			source:              source,
			characterID:         characterID,
			characterGeneration: characterGeneration,
			async:               true,
			run:                 run,
		},
		Priority: eventloop.PriorityMedium,
		Context:  ctx,
	})
}

func (s *Service) shutdownGameSessionEvents(session *gameSession, closeDproto bool) {
	if session == nil {
		return
	}
	// The reader has stopped submitting new packet events at this boundary.
	// Drain acknowledged dungeon quest writes while their completion callbacks
	// can still be delivered to the session event loop.
	questCtx, questCancel := context.WithTimeout(context.Background(), gameSessionShutdownWait)
	questErr := waitGameSessionQuestProgress(questCtx, session)
	questCancel()
	if questErr != nil {
		s.logGameEvent(session, "game-session-quest-progress-drain-failed", "error", questErr)
	}
	s.beginGameSessionClose(session)

	ctx, cancel := context.WithTimeout(context.Background(), gameSessionShutdownWait)
	defer cancel()
	var cleanupCompleted atomic.Bool
	cleanup := func() error {
		if closeDproto {
			s.closeGameDprotoSession(session)
		}
		s.cleanupOnlinePlayer(session)
		s.unbindGameSession(session)
		cleanupCompleted.Store(true)
		return nil
	}
	cleanupErr := s.callGameSession(ctx, session, "game-session-shutdown", cleanup)
	stopErr := s.stopGameSessionEvents(ctx, session)
	// The loop has stopped, so nothing can submit another frame. Drain what the
	// client is still owed (its last notices) inside the same bounded ctx, then
	// retire the goroutine instead of leaking it.
	s.stopGameSessionWriter(ctx, session)
	if !cleanupCompleted.Load() && stopErr == nil {
		cleanupErr = errors.Join(cleanupErr, cleanup())
	}
	if cleanupErr != nil || stopErr != nil {
		s.logGameEvent(session, "game-session-events-shutdown-failed",
			"cleanup_error", cleanupErr,
			"stop_error", stopErr)
		return
	}
	s.logGameEvent(session, "game-session-events-stopped",
		"character_generation", session.characterGeneration)
}

func advanceGameSessionCharacterGeneration(session *gameSession) uint64 {
	if session == nil {
		return 0
	}
	session.characterGeneration++
	if session.characterGeneration == 0 {
		session.characterGeneration++
	}
	return session.characterGeneration
}

func gameSessionCharacterEventIdentity(session *gameSession, characterID uint16) (uint16, uint64, error) {
	if session == nil || characterID == 0 || characterID != session.selectedCharacterID {
		return 0, 0, fmt.Errorf("%w: selected character changed", errInvalidGameSessionEvent)
	}
	return characterID, session.characterGeneration, nil
}

func isClosedGameSessionEventError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, eventloop.ErrClosed) ||
		errors.Is(err, eventloop.ErrDraining) ||
		errors.Is(err, eventloop.ErrNotStarted)
}
