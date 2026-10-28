package fatigue

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerConsumeRoomPersistsRemainingAndDisplayUsed(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{"fatigue": 100, "fatigue_limit": DefaultLimit},
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	result, err := owner.ConsumeRoom(context.Background(), ConsumeRoomCommand{
		AccountID: "account-1", CharacterID: "19", Amount: 1, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Before != 100 || result.After != 99 || result.Consumed != 1 {
		t.Fatalf("result=%+v", result)
	}
	stored, found, err := repositories.Character.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load stored character found=%t err=%v", found, err)
	}
	if stored.Stats["fatigue"] != 99 || stored.Stats["fatigue_display_update"] != 57 ||
		!stored.UpdatedAt.Equal(now.UTC()) {
		t.Fatalf("stored stats=%+v updated_at=%v", stored.Stats, stored.UpdatedAt)
	}
}

func TestOwnerConsumeRoomConsumesBatteryAlongsideFatigue(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 89,
		Stats: map[string]int64{"fatigue": 100, "fatigue_limit": 156, FatigueBatteryStatKey: 12},
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ConsumeRoom(context.Background(), ConsumeRoomCommand{
		AccountID: "account-1", CharacterID: "19", Amount: 1,
	})
	if err != nil || result.Consumed != 1 || result.BatteryConsumed != 1 || result.BatteryAfter != 11 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stored, _, _ := repositories.Character.Load(context.Background(), "19")
	if stored.Stats[FatigueBatteryStatKey] != 11 {
		t.Fatalf("stored battery=%d, want 11", stored.Stats[FatigueBatteryStatKey])
	}
}

func TestOwnerConsumeRoomBatteryStopsAtLevelCapAndActualFatigue(t *testing.T) {
	for _, level := range []int{89, 90} {
		repositories := dnfrepomemory.NewMemoryGroup()
		ctx := context.Background()
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: "19", Level: level,
			Stats: map[string]int64{"fatigue": 1, FatigueBatteryStatKey: 12},
		}); err != nil {
			t.Fatal(err)
		}
		owner, _ := NewOwner(repositories)
		result, err := owner.ConsumeRoom(ctx, ConsumeRoomCommand{CharacterID: "19", Amount: 5})
		want := int64(11)
		if level == 90 {
			want = 12
		}
		if err != nil || result.BatteryAfter != want || result.Consumed != 1 {
			t.Fatalf("level=%d result=%+v err=%v", level, result, err)
		}
	}
}

func TestOwnerConsumeRoomClampsLastPointAndLeavesExhaustedStateStable(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1",
		Stats: map[string]int64{"fatigue": 1, "fatigue_limit": DefaultLimit},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	first, err := owner.ConsumeRoom(context.Background(), ConsumeRoomCommand{AccountID: "account-1", CharacterID: "19", Amount: 1})
	if err != nil || first.After != 0 || first.Consumed != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	updatedAt := first.Character.UpdatedAt
	second, err := owner.ConsumeRoom(context.Background(), ConsumeRoomCommand{AccountID: "account-1", CharacterID: "19", Amount: 1})
	if err != nil || second.After != 0 || second.Consumed != 0 || !second.Character.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestOwnerConsumeRoomRejectsAccountMismatch(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Stats: map[string]int64{"fatigue": 10},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	_, err := owner.ConsumeRoom(context.Background(), ConsumeRoomCommand{AccountID: "account-2", CharacterID: "19", Amount: 1})
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("error=%v", err)
	}
}
