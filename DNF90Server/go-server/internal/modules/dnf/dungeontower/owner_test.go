package dungeontower

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerAdvancesCharacterTowerFloorExactlyOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	route := dnfdungeonadmission.TowerRoute{
		Family:     dnfdungeonadmission.TowerFamilyGrief,
		DungeonIDs: []uint32{5115, 5116, 5117},
	}
	before, err := owner.Current(ctx, "7", route)
	if err != nil || before.Floor != 1 || before.DungeonID != 5115 || before.Completed {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	after, err := owner.Advance(ctx, "7", route, 5115, "clear-1", time.Now())
	if err != nil || after.Floor != 2 || after.DungeonID != 5116 || after.Replayed {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	replay, err := owner.Advance(ctx, "7", route, 5115, "clear-1", time.Now())
	if err != nil || replay.Floor != 2 || !replay.Replayed {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	current, err := owner.Current(ctx, "7", route)
	if err != nil || current.Floor != 2 || current.DungeonID != 5116 {
		t.Fatalf("current=%+v err=%v", current, err)
	}
}

func TestOwnerKeepsTowerFamiliesIndependentAndRejectsStaleClear(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "8", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	despair := dnfdungeonadmission.TowerRoute{Family: dnfdungeonadmission.TowerFamilyDespair, DungeonIDs: []uint32{11008, 11009}}
	grief := dnfdungeonadmission.TowerRoute{Family: dnfdungeonadmission.TowerFamilyGrief, DungeonIDs: []uint32{5115, 5116}}
	if _, err := owner.Advance(ctx, "8", despair, 11008, "despair-clear", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Advance(ctx, "8", despair, 11008, "different-stale-clear", time.Now()); !errors.Is(err, ErrProgressMismatch) {
		t.Fatalf("stale clear error=%v", err)
	}
	griefCurrent, err := owner.Current(ctx, "8", grief)
	if err != nil || griefCurrent.Floor != 1 {
		t.Fatalf("grief=%+v err=%v", griefCurrent, err)
	}
}

func TestOwnerAcceptsPVFProvedSkippedFloorAndCapsTopLayer(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "9", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	route := dnfdungeonadmission.TowerRoute{Family: dnfdungeonadmission.TowerFamilyDespair, DungeonIDs: []uint32{11008, 11009, 11010}}
	afterSkip, err := owner.Advance(ctx, "9", route, 11009, "skip-floor-one", time.Now())
	if err != nil || afterSkip.Floor != 3 || afterSkip.Completed {
		t.Fatalf("after skip=%+v err=%v", afterSkip, err)
	}
	top, err := owner.Advance(ctx, "9", route, 11010, "top-clear", time.Now())
	if err != nil || top.Floor != 3 || !top.Completed {
		t.Fatalf("top=%+v err=%v", top, err)
	}
}

func TestOwnerCommitsTowerRewardsAndFloorInOneReceipt(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "10", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	route := dnfdungeonadmission.TowerRoute{Family: dnfdungeonadmission.TowerFamilyDespair, DungeonIDs: []uint32{11008, 11009}}
	command := SettlementCommand{
		CharacterID: "10", Route: route, ClearedDungeonID: 11008, Receipt: "tower-settlement-1", MainSlots: 24,
		Rewards:   []RewardItem{{Stack: dnfrepo.ItemStack{ItemID: 1001, Count: 1, Extra: map[string]string{"pvf_path": "equipment/reward.equ"}}, SlotStart: 3, SlotEnd: 23}},
		UpdatedAt: time.Unix(100, 0).UTC(),
	}
	result, err := owner.CommitSettlement(ctx, command)
	if err != nil || result.Snapshot.Floor != 2 || result.Replayed || len(result.ItemSlots) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stack, found := result.Inventory.Slots["0:3"]
	if !found || stack.ItemID != 1001 || stack.Count != 1 {
		t.Fatalf("committed reward=%+v found=%t", stack, found)
	}
	replay, err := owner.CommitSettlement(ctx, command)
	if err != nil || !replay.Replayed || replay.Snapshot.Floor != 2 || len(replay.ItemSlots) != 1 || replay.ItemSlots[0] != 3 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	inventory, _, err := repositories.Inventory.Load(ctx, "10")
	if err != nil || inventory.Slots["0:3"].Count != 1 {
		t.Fatalf("inventory after replay=%+v err=%v", inventory, err)
	}
}
