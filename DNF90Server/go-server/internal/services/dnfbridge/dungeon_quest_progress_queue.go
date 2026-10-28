package dnfbridge

import (
	"context"
	"sync"
)

// reserveCurrentDungeonQuestProgress allocates a monotonically increasing
// persistence token. Every queued quest/inventory mutation for one connection
// runs in this order, preventing stale snapshots from overwriting newer state.
func reserveCurrentDungeonQuestProgress(session *gameSession) uint64 {
	if session == nil {
		return 0
	}
	session.questProgressMu.Lock()
	defer session.questProgressMu.Unlock()
	if session.questProgressCond == nil {
		session.questProgressCond = sync.NewCond(&session.questProgressMu)
	}
	session.questProgressQueued++
	if session.questProgressQueued == 0 {
		session.questProgressQueued++
	}
	session.questProgressWG.Add(1)
	return session.questProgressQueued
}

func runCurrentDungeonQuestProgressOrdered(session *gameSession, sequence uint64, run func()) {
	if session == nil || sequence == 0 {
		if run != nil {
			run()
		}
		return
	}
	defer session.questProgressWG.Done()
	session.questProgressMu.Lock()
	if session.questProgressCond == nil {
		session.questProgressCond = sync.NewCond(&session.questProgressMu)
	}
	for sequence != session.questProgressPersisted+1 {
		session.questProgressCond.Wait()
	}
	session.questProgressMu.Unlock()

	if run != nil {
		run()
	}

	session.questProgressMu.Lock()
	session.questProgressPersisted = sequence
	session.questProgressCond.Broadcast()
	session.questProgressMu.Unlock()
}

func waitGameSessionQuestProgress(ctx context.Context, session *gameSession) error {
	if session == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		session.questProgressWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
