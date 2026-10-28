package dnfbridge

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestCurrentExpertJobStoreMachineCommitClosesExactlyOnceAtZero(t *testing.T) {
	owner := &gameSession{selectedCharacterID: 19}
	visitorA := &gameSession{selectedCharacterID: 20}
	visitorB := &gameSession{selectedCharacterID: 21}
	store := &currentExpertJobStore{
		OwnerCharacterID: 19,
		OwnerSession:     owner,
		Kind:             currentExpertJobDisjointStoreKind,
		MachineGrade:     1,
		Endurance:        1,
	}
	service := &Service{
		expertJobStores: map[uint16]*currentExpertJobStore{19: store},
		expertJobVisitors: map[*gameSession]uint16{
			visitorA: 19,
			visitorB: 19,
		},
	}

	var closeCount atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			service.expertJobStoreOpMu.Lock()
			closed := service.applyCurrentExpertJobStoreMachineCommit(store, 0, 1)
			service.expertJobStoreOpMu.Unlock()
			if closed != nil {
				if closed.Endurance != 0 {
					t.Errorf("closed endurance = %d, want 0", closed.Endurance)
				}
				closeCount.Add(1)
			}
		}()
	}
	wait.Wait()

	if got := closeCount.Load(); got != 1 {
		t.Fatalf("close count = %d, want 1", got)
	}
	service.expertJobStoreMu.Lock()
	defer service.expertJobStoreMu.Unlock()
	if _, ok := service.expertJobStores[19]; ok {
		t.Fatal("zero-endurance machine remained open")
	}
	if len(service.expertJobVisitors) != 0 {
		t.Fatalf("visitor count = %d, want 0", len(service.expertJobVisitors))
	}
}

func TestCurrentExpertJobStoreMachineCommitKeepsPositiveEndurance(t *testing.T) {
	owner := &gameSession{selectedCharacterID: 19}
	visitor := &gameSession{selectedCharacterID: 20}
	store := &currentExpertJobStore{
		OwnerCharacterID: 19,
		OwnerSession:     owner,
		Kind:             currentExpertJobDisjointStoreKind,
		MachineGrade:     1,
		Endurance:        8,
	}
	service := &Service{
		expertJobStores:   map[uint16]*currentExpertJobStore{19: store},
		expertJobVisitors: map[*gameSession]uint16{visitor: 19},
	}

	service.expertJobStoreOpMu.Lock()
	closed := service.applyCurrentExpertJobStoreMachineCommit(store, 7, 2)
	service.expertJobStoreOpMu.Unlock()
	if closed != nil {
		t.Fatal("positive-endurance machine was closed")
	}
	service.expertJobStoreMu.Lock()
	defer service.expertJobStoreMu.Unlock()
	live := service.expertJobStores[19]
	if live == nil || live.Endurance != 7 || live.MachineGrade != 2 {
		t.Fatalf("live machine = %#v, want endurance 7 and grade 2", live)
	}
	if got := service.expertJobVisitors[visitor]; got != 19 {
		t.Fatalf("visitor owner = %d, want 19", got)
	}
}

func TestCurrentExpertJobStoreMachineCommitRejectsStaleOwnerSession(t *testing.T) {
	owner := &gameSession{selectedCharacterID: 19}
	live := &currentExpertJobStore{
		OwnerCharacterID: 19,
		OwnerSession:     owner,
		Kind:             currentExpertJobDisjointStoreKind,
		MachineGrade:     1,
		Endurance:        1,
	}
	stale := cloneCurrentExpertJobStore(live)
	stale.OwnerSession = &gameSession{selectedCharacterID: 19}
	service := &Service{expertJobStores: map[uint16]*currentExpertJobStore{19: live}}

	service.expertJobStoreOpMu.Lock()
	closed := service.applyCurrentExpertJobStoreMachineCommit(stale, 0, 1)
	service.expertJobStoreOpMu.Unlock()
	if closed != nil {
		t.Fatal("stale owner session closed the live machine")
	}
	service.expertJobStoreMu.Lock()
	defer service.expertJobStoreMu.Unlock()
	if service.expertJobStores[19] != live || live.Endurance != 1 {
		t.Fatalf("live machine changed through stale snapshot: %#v", service.expertJobStores[19])
	}
}
