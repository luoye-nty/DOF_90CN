package dungeonadmission

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestReconcileDailyResetResetsOnlyDailyPoliciesAndGrantsMissingInvitationsOnce(t *testing.T) {
	ctx := context.Background()
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "11", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "11",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 10161065, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	dailyCharacter := Policy{Key: "tower.character.daily", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	dailyAccount := Policy{Key: "tower.account.daily", Scope: ScopeAccount, Period: PeriodDaily, Limit: 3}
	weekly := Policy{Key: "raid.weekly", Scope: ScopeCharacter, Period: PeriodWeekly, Limit: 2, ResetWeekday: time.Thursday}
	before := time.Date(2026, 8, 31, 5, 59, 0, 0, serviceCalendar)
	after := time.Date(2026, 8, 31, 6, 0, 0, 0, serviceCalendar)
	if _, err := owner.Commit(ctx, "11", []Policy{dailyCharacter, dailyAccount, weekly}, "before-reset", before); err != nil {
		t.Fatal(err)
	}
	invitations := []DailyInvitation{
		{ItemID: 4473, Minimum: 1, ListType: dnfrepo.MainInventoryListType, CheckSlotStart: 121, CheckSlotEnd: 232, SlotStart: 177, SlotEnd: 232, PVFPath: "stackable/invitation_drapyus2.stk", StackableType: "[material]"},
		{ItemID: 10161065, Minimum: 1, ListType: dnfrepo.MainInventoryListType, CheckSlotStart: 121, CheckSlotEnd: 232, SlotStart: 177, SlotEnd: 232, PVFPath: "stackable/10161001/10161065.stk", StackableType: "[material]"},
	}
	result, err := owner.ReconcileDailyReset(ctx, "11", []Policy{dailyCharacter, dailyAccount, weekly}, invitations, after)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !result.CountersChanged || !result.InventoryChanged || !result.InvitationChecksChanged ||
		len(result.ResetPolicyKeys) != 2 || len(result.Snapshots) != 2 || len(result.Invitations) != 2 {
		t.Fatalf("result=%+v", result)
	}
	if result.Invitations[0].Granted != 1 || len(result.Invitations[0].ChangedSlots) != 1 || result.Invitations[0].ChangedSlots[0] != 177 {
		t.Fatalf("despair invitation=%+v", result.Invitations[0])
	}
	if result.Invitations[1].HeldBefore != 2 || result.Invitations[1].Granted != 0 || len(result.Invitations[1].ChangedSlots) != 0 {
		t.Fatalf("grief invitation=%+v", result.Invitations[1])
	}
	for _, policy := range []Policy{dailyCharacter, dailyAccount} {
		snapshot, err := owner.Preflight(ctx, "11", []Policy{policy}, after)
		if err != nil || len(snapshot) != 1 || snapshot[0].Used != 0 || snapshot[0].Remaining != 3 {
			t.Fatalf("daily policy=%s snapshot=%+v err=%v", policy.Key, snapshot, err)
		}
	}
	weeklySnapshot, err := owner.Preflight(ctx, "11", []Policy{weekly}, after)
	if err != nil || len(weeklySnapshot) != 1 || weeklySnapshot[0].Used != 1 {
		t.Fatalf("weekly snapshot=%+v err=%v", weeklySnapshot, err)
	}

	replay, err := owner.ReconcileDailyReset(ctx, "11", []Policy{dailyCharacter, dailyAccount, weekly}, invitations, after.Add(time.Hour))
	if err != nil || replay.Changed || replay.InventoryChanged || len(replay.Invitations) != 2 || !replay.Invitations[0].Replayed || !replay.Invitations[1].Replayed {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	delete(inventory.Slots, "0:177")
	if err := dnfrepo.SaveInventoryFields(ctx, repositories.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
		t.Fatal(err)
	}
	afterConsume, err := owner.ReconcileDailyReset(ctx, "11", []Policy{dailyCharacter, dailyAccount, weekly}, invitations, after.Add(2*time.Hour))
	if err != nil || afterConsume.InventoryChanged || !afterConsume.Invitations[0].Replayed {
		t.Fatalf("after consume=%+v err=%v", afterConsume, err)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "11")
	if _, regenerated := inventory.Slots["0:177"]; regenerated {
		t.Fatalf("same-day relog regenerated consumed invitation: %+v", inventory.Slots)
	}
}
