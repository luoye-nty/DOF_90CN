package dungeon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// The tower clear, its ten frozen reward slots and the consumed entry pool all
// commit in one transaction, and a retry replays the same receipt.
func TestTowerSettlementProgressAndRewardsCommitAndReplayTogether(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{AccountID: "a", CharacterID: "19", Level: 1, Stats: map[string]int64{"exp": 0}}
	for _, err := range []error{
		repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a"}),
		repos.Character.Save(ctx, character),
		repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: dnfrepo.SkillPointState{SyncedLevel: 1}}),
		repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	owner, _ := NewOwner(repos)
	grantFailure := errors.New("inventory write failed")
	fail := true
	clear := &TowerClear{Kind: TowerGrief, Floor: 1, Grant: func(inv *dnfrepo.InventoryRecord, id, count uint32) error {
		if inv.Slots == nil {
			inv.Slots = make(map[string]dnfrepo.ItemStack)
		}
		stack := inv.Slots["0:121"]
		stack.ItemID = int64(id)
		stack.Count += int64(count)
		inv.Slots["0:121"] = stack
		if fail {
			return grantFailure
		}
		return nil
	}}
	clear.Rewards[2] = TowerReward{10160931, 1}
	cmd := SettlementCommand{AccountID: "a", CharacterID: "19", CompletionKey: "tower-1", Tables: tables, Tower: clear, UpdatedAt: time.Unix(1800000000, 0)}
	if _, err := owner.CommitSettlement(ctx, cmd); !errors.Is(err, grantFailure) {
		t.Fatal(err)
	}
	saved, _, _ := repos.Character.Load(ctx, "19")
	inv, _, _ := repos.Inventory.Load(ctx, "19")
	if TowerCharacterDailyRemaining(saved, TowerGrief, cmd.UpdatedAt) != 1 {
		t.Fatal("failed settlement consumed the character entry")
	}
	if floor, _ := TowerFloor(saved, TowerGrief); floor != 1 || len(inv.Slots) != 0 {
		t.Fatalf("failed transaction advanced state: %+v %+v", saved, inv)
	}
	fail = false
	for attempt := 0; attempt < 2; attempt++ {
		res, err := owner.CommitSettlement(ctx, cmd)
		if err != nil {
			t.Fatal(err)
		}
		floor, _ := TowerFloor(res.Character, TowerGrief)
		other, _ := TowerFloor(res.Character, TowerDespair)
		if floor != 2 || other != 1 || res.Replayed != (attempt == 1) || res.Inventory.Slots["0:121"].Count != 1 {
			t.Fatalf("result=%+v floor=%d other=%d", res, floor, other)
		}
		account, _, _ := repos.Account.Load(ctx, "a")
		used, err := TowerDailyUsed(account, TowerGrief, cmd.UpdatedAt)
		if err != nil || used != 0 || TowerCharacterDailyRemaining(res.Character, TowerGrief, cmd.UpdatedAt) != 0 {
			t.Fatal("first clear/replay did not consume exactly one character entry")
		}
	}
	// A new owner/session sees the durable floor and frozen reward snapshot.
	owner, _ = NewOwner(repos)
	clear.Rewards[2].Count = 9
	res, err := owner.CommitSettlement(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	rewards, err := TowerLastRewards(res.Character, TowerGrief)
	if err != nil || rewards[2].Count != 1 || res.Inventory.Slots["0:121"].Count != 1 {
		t.Fatalf("replayed reward changed: %+v %v", rewards, err)
	}
	cmd.CompletionKey = "despair-top"
	cmd.Tower = &TowerClear{Kind: TowerDespair, Floor: 100}
	res, err = owner.CommitSettlement(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if floor, _ := TowerFloor(res.Character, TowerDespair); floor != 1 {
		t.Fatal(floor)
	}
	if floor, _ := TowerFloor(res.Character, TowerGrief); floor != 2 {
		t.Fatal(floor)
	}
}

// Each tower has one character entry on top of the shared account pool, and the
// 06:00 service-day boundary resets them independently.
func TestTowerEntryPoolsConsumeCharacterBeforeAccountAndResetIndependently(t *testing.T) {
	reset := time.Date(2026, 9, 15, 6, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	now := reset.Add(-time.Hour)
	account := dnfrepo.AccountRecord{AccountID: "a"}
	character := dnfrepo.CharacterRecord{CharacterID: "19", Stats: map[string]int64{
		"tower_despair_last_clear_at": now.Unix(), "tower_grief_last_clear_at": now.Unix(),
	}}
	for _, row := range []struct {
		kind  TowerKind
		extra int
	}{{TowerDespair, 6}, {TowerGrief, 3}} {
		for clear := 1; clear <= row.extra+1; clear++ {
			if err := recordTowerEntryClear(&account, &character, row.kind, now); err != nil {
				t.Fatal(err)
			}
			used, err := TowerDailyUsed(account, row.kind, now)
			if err != nil || used != uint32(clear-1) || TowerCharacterDailyRemaining(character, row.kind, now) != 0 {
				t.Fatalf("%s clear %d charged the wrong pool: shared=%d err=%v", row.kind, clear, used, err)
			}
		}
	}
	other := dnfrepo.CharacterRecord{CharacterID: "20"}
	for _, kind := range []TowerKind{TowerDespair, TowerGrief} {
		if TowerCharacterDailyRemaining(other, kind, now) != 1 || TowerCharacterDailyRemaining(character, kind, reset.Add(-time.Second)) != 0 || TowerCharacterDailyRemaining(character, kind, reset) != 1 {
			t.Fatal("character isolation or 06:00 reset is incorrect")
		}
	}
	if err := recordTowerEntryClear(&account, &character, TowerDespair, reset); err != nil {
		t.Fatal(err)
	}
	if TowerCharacterDailyRemaining(character, TowerGrief, reset) != 1 || character.Stats["tower_despair_character_entry_day"] != premium.ServiceDayID(reset) {
		t.Fatal("next-day despair clear consumed the grief entry")
	}
}

func TestTowerDailyUsageRecordsAcceptedClearsAndResets(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	account := dnfrepo.AccountRecord{AccountID: "a"}
	if used, err := TowerDailyUsed(account, TowerDespair, now); err != nil || used != 0 {
		t.Fatalf("new account usage=%d err=%v", used, err)
	}
	if err := RecordTowerDailyClear(&account, TowerDespair, now); err != nil {
		t.Fatal(err)
	}
	if err := RecordTowerDailyClear(&account, TowerDespair, now); err != nil {
		t.Fatal(err)
	}
	if err := RecordTowerDailyClear(&account, TowerDespair, now); err != nil {
		t.Fatalf("third clear err=%v", err)
	}
	if used, err := TowerDailyUsed(account, TowerDespair, now.Add(24*time.Hour)); err != nil || used != 0 {
		t.Fatalf("next-day usage=%d err=%v", used, err)
	}
	if err := RecordTowerDailyClear(&account, TowerGrief, now); err != nil {
		t.Fatal(err)
	}
	if used, err := TowerDailyUsed(account, TowerDespair, now); err != nil || used != 3 {
		t.Fatalf("despair usage changed by grief clear=%d err=%v", used, err)
	}
}

func TestTowerDailyEnterableLimitReadsPVF(t *testing.T) {
	source := settlementMemorySource{
		"etc/towerofdespair.etc": "[account enterable max count]\n3\n[adventure group add enterable count]\n2 6 1 7 11 3 12 30 5 31 99 7\n[/adventure group add enterable count]",
		"etc/towerofgrief.etc":   "[account enterable max count]\n1\n[adventure group add enterable count]\n2 6 1 7 11 2 12 30 3 31 99 4\n[/adventure group add enterable count]",
	}
	for _, row := range []struct {
		level          int
		despair, grief uint32
	}{{1, 3, 1}, {2, 4, 2}, {6, 4, 2}, {7, 6, 3}, {8, 6, 3}, {11, 6, 3}, {12, 8, 4}, {30, 8, 4}, {31, 10, 5}, {99, 10, 5}} {
		for i, kind := range []TowerKind{TowerDespair, TowerGrief} {
			want := []uint32{row.despair, row.grief}[i]
			if got, err := TowerDailyEnterableLimit(source, kind, row.level); err != nil || got != want {
				t.Fatalf("kind=%s level=%d limit=%d want=%d err=%v", kind, row.level, got, want, err)
			}
		}
	}
}

func TestTowerDailyUsageDayChangeClearsBothTowers(t *testing.T) {
	now := time.Date(2026, 9, 13, 5, 59, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	account := dnfrepo.AccountRecord{AccountID: "a"}
	if err := RecordTowerDailyClear(&account, TowerGrief, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := RecordTowerDailyClear(&account, TowerDespair, now); err != nil {
		t.Fatal(err)
	}
	if used, err := TowerDailyUsed(account, TowerGrief, now); err != nil || used != 0 {
		t.Fatalf("new day restored yesterday's grief usage: used=%d err=%v", used, err)
	}
}

// The grief draw is deterministic per reward slot from the server-owned
// completion key, so a retry cannot re-roll the ten cards.
func TestGriefTowerPVFUsesFloorRangesAndIndependentCardSlots(t *testing.T) {
	source := settlementMemorySource{
		"etc/towerofgrief.etc":                "[each layer dungeon clear reward]\n1 49 `normal` 50 50 `special` 51 99 `normal` 100 100 `special`\n[/each layer dungeon clear reward]",
		"etc/itemdropinfo_togclearreward.etc": "[cardslot specific reward table]\n[table]\n`normal` `card 3` 10160931 10000 1\n[/table]\n[table]\n`special` `card 10` 10160930 10000 1\n[/table]\n[/cardslot specific reward table]",
	}
	for _, floor := range []uint16{1, 49, 50, 51, 99, 100} {
		rewards, err := GriefTowerRewards(source, floor, "server-owned-run")
		if err != nil {
			t.Fatal(err)
		}
		var want [TowerRewardSlots]TowerReward
		if floor == 50 || floor == 100 {
			want[9] = TowerReward{10160930, 1}
		} else {
			want[2] = TowerReward{10160931, 1}
		}
		if rewards != want {
			t.Fatalf("floor %d: %+v", floor, rewards)
		}
		again, err := GriefTowerRewards(source, floor, "server-owned-run")
		if err != nil || !reflect.DeepEqual(rewards, again) {
			t.Fatal("unstable reward draw")
		}
	}
	source["etc/itemdropinfo_togclearreward.etc"] = "[table]\n`normal` `card 3` 10160931 10001 1\n[/table]"
	if _, err := GriefTowerRewards(source, 1, "run"); err == nil {
		t.Fatal("accepted invalid probability")
	}
}
