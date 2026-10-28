package dungeon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerGrantCardRewardCommitsWalletAndInventoryTogether(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 100)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 7001, Count: 2},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	expireAt := time.Unix(2000, 0).UTC()
	updatedAt := time.Unix(1000, 0).UTC()
	result, err := owner.GrantCardReward(ctx, CardRewardCommand{
		CharacterID: "19",
		MainSlots:   3,
		Bundle: CardRewardBundle{
			Gold: 50,
			Items: []CardItemReward{{
				Stack:     dnfrepo.ItemStack{ItemID: 7001, Count: 3},
				Stackable: true,
				ExpireAt:  expireAt,
			}},
		},
		UpdatedAt: updatedAt,
		Project: func(stack dnfrepo.ItemStack, expiration time.Time) (dnfrepo.ItemStack, error) {
			stack.ExpireAt = expiration
			return stack, nil
		},
	})
	if err != nil {
		t.Fatalf("GrantCardReward: %v", err)
	}
	if result.GoldBefore != 100 || result.GoldAfter != 150 ||
		len(result.ItemSlots) != 1 || result.ItemSlots[0] != 0 {
		t.Fatalf("result = %+v", result)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if character.Stats["gold"] != 150 ||
		inventory.Slots["0:0"].Count != 5 ||
		!inventory.Slots["0:0"].ExpireAt.Equal(expireAt) {
		t.Fatalf("character=%+v inventory=%+v", character, inventory)
	}
}

// The paid card row charges the commission op35 printed before it credits the
// bundle, so the balance moves by reward-minus-cost in one transaction.
func TestOwnerGrantCardRewardSpendsPaidCardCommission(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 100)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	command := CardRewardCommand{
		CharacterID:   "19",
		CompletionKey: "paid-card-flip",
		MainSlots:     3,
		Cost:          30,
		Bundle:        CardRewardBundle{Gold: 50},
		UpdatedAt:     time.Unix(1000, 0).UTC(),
	}
	result, err := owner.GrantCardReward(ctx, command)
	if err != nil {
		t.Fatalf("GrantCardReward: %v", err)
	}
	if result.GoldBefore != 100 || result.GoldSpent != 30 || result.GoldAfter != 120 {
		t.Fatalf("result = %+v, want before=100 spent=30 after=120", result)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["gold"] != 120 {
		t.Fatalf("gold = %d, want 120", character.Stats["gold"])
	}
	// The receipt has to replay the charged amount, not just the credited one:
	// a retry that spends a second time charges the player twice for one flip.
	replayed, err := owner.GrantCardReward(ctx, command)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed.Replayed || replayed.GoldSpent != 30 || replayed.GoldAfter != 120 {
		t.Fatalf("replay = %+v, want replayed spent=30 after=120", replayed)
	}
	character, _, err = repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("reload character: %v", err)
	}
	if character.Stats["gold"] != 120 {
		t.Fatalf("replay charged twice: gold = %d, want 120", character.Stats["gold"])
	}
}

// A flip the character cannot pay for must be refused before anything mutates,
// matching 86JP's TryBuildRewardPlan, which rejects the plan up front.
func TestOwnerGrantCardRewardRefusesUnpayableCommission(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 20)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	if _, err = owner.GrantCardReward(ctx, CardRewardCommand{
		CharacterID: "19",
		MainSlots:   3,
		Cost:        30,
		Bundle:      CardRewardBundle{Gold: 50},
		UpdatedAt:   time.Unix(1000, 0).UTC(),
	}); !errors.Is(err, ErrCardRewardGoldShort) {
		t.Fatalf("err = %v, want ErrCardRewardGoldShort", err)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["gold"] != 20 {
		t.Fatalf("refused flip mutated gold: %d, want 20", character.Stats["gold"])
	}
}

func TestOwnerGrantCardRewardRoutesItemToMailboxWhenInventoryIsFull(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 100)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 1, Count: 1},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, _ := NewOwner(repos)
	result, err := owner.GrantCardReward(ctx, CardRewardCommand{
		CharacterID: "19",
		MainSlots:   1,
		Bundle: CardRewardBundle{
			Gold: 50,
			Items: []CardItemReward{{
				Stack: dnfrepo.ItemStack{ItemID: 2, Count: 1},
			}},
		},
	})
	if err != nil {
		t.Fatalf("GrantCardReward: %v", err)
	}
	if result.OverflowMailID == "" || len(result.ItemSlots) != 1 || result.ItemSlots[0] != -1 {
		t.Fatalf("result = %+v", result)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["gold"] != 150 {
		t.Fatalf("gold = %d, want 150", character.Stats["gold"])
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	mail := mailbox.Mails[result.OverflowMailID]
	if len(mail.Attachments) != 1 || mail.Attachments[0].ItemID != 2 || mail.Attachments[0].Count != 1 {
		t.Fatalf("overflow mail = %+v", mail)
	}
	if mail.SenderName != "系统" || mail.Title != "背包已满：通关奖励" ||
		mail.Body != "背包空间不足，通关奖励已通过邮件发送。请清理对应道具分页后领取。" {
		t.Fatalf("overflow mail prompt = %+v", mail)
	}
}

func TestOwnerGrantCardRewardDurableReceiptReplaysIndeterminateMailboxCommit(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 100)
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 1, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("mailbox transaction committed but result was lost")
	indeterminate := &commitThenErrorMailboxAssetUnitOfWork{
		base:     repos.MailboxAssets,
		failOnce: true,
		err:      wantErr,
	}
	repos.MailboxAssets = indeterminate
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	command := CardRewardCommand{
		CharacterID:   "19",
		CompletionKey: "altar-clear/run-1:card-plan:stable-plan-id",
		MainSlots:     2,
		Bundle: CardRewardBundle{
			Gold: 50,
			Items: []CardItemReward{
				{Stack: dnfrepo.ItemStack{ItemID: 2, Count: 1}},
				{Stack: dnfrepo.ItemStack{ItemID: 3, Count: 1}},
			},
		},
		UpdatedAt: time.Unix(1000, 0).UTC(),
	}
	if _, err := owner.GrantCardReward(ctx, command); !errors.Is(err, wantErr) {
		t.Fatalf("indeterminate first commit error=%v", err)
	}
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found || character.Stats["gold"] != 150 {
		t.Fatalf("committed character found=%t err=%v stats=%v", found, err, character.Stats)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "19")
	if err != nil || !found || inventory.Slots["0:1"].ItemID != 2 || inventory.Slots["0:1"].Count != 1 {
		t.Fatalf("committed inventory found=%t err=%v slots=%+v", found, err, inventory.Slots)
	}
	mailbox, found, err := repos.Mailbox.Load(ctx, "19")
	if err != nil || !found || len(mailbox.Mails) != 1 {
		t.Fatalf("committed mailbox found=%t err=%v mails=%+v", found, err, mailbox.Mails)
	}

	replayed, err := owner.GrantCardReward(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.GoldBefore != 100 || replayed.GoldAfter != 150 ||
		len(replayed.ItemSlots) != 2 || replayed.ItemSlots[0] != 1 || replayed.ItemSlots[1] != -1 ||
		replayed.OverflowMailID == "" {
		t.Fatalf("replayed result=%+v", replayed)
	}
	character, _, err = repos.Character.Load(ctx, "19")
	if err != nil || character.Stats["gold"] != 150 {
		t.Fatalf("replay duplicated gold: character=%+v err=%v", character, err)
	}
	inventory, _, err = repos.Inventory.Load(ctx, "19")
	if err != nil || inventory.Slots["0:1"].Count != 1 {
		t.Fatalf("replay duplicated inventory item: inventory=%+v err=%v", inventory, err)
	}
	mailbox, _, err = repos.Mailbox.Load(ctx, "19")
	if err != nil || len(mailbox.Mails) != 1 || indeterminate.calls != 2 {
		t.Fatalf("replay duplicated mailbox or skipped transaction: mails=%+v calls=%d err=%v",
			mailbox.Mails, indeterminate.calls, err)
	}
}

type commitThenErrorMailboxAssetUnitOfWork struct {
	base     dnfrepo.MailboxAssetUnitOfWork
	failOnce bool
	err      error
	calls    int
}

func (unit *commitThenErrorMailboxAssetUnitOfWork) WithinMailboxAssets(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(
		dnfrepo.AccountRepository,
		dnfrepo.AccountInventoryRepository,
		dnfrepo.CharacterRepository,
		dnfrepo.InventoryRepository,
		dnfrepo.MailboxRepository,
	) error,
) error {
	unit.calls++
	err := unit.base.WithinMailboxAssets(ctx, accountID, characterID, apply)
	if err == nil && unit.failOnce {
		unit.failOnce = false
		return unit.err
	}
	return err
}

var _ dnfrepo.MailboxAssetUnitOfWork = (*commitThenErrorMailboxAssetUnitOfWork)(nil)

func TestOwnerGrantPickupGoldUsesCharacterAssetTransaction(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 100)
	owner, _ := NewOwner(repos)
	result, err := owner.GrantPickupGold(ctx, GoldPickupCommand{
		CharacterID: "19",
		Amount:      25,
	})
	if err != nil {
		t.Fatalf("GrantPickupGold: %v", err)
	}
	if result.GoldBefore != 100 || result.GoldAfter != 125 {
		t.Fatalf("result = %+v", result)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["gold"] != 125 {
		t.Fatalf("gold = %d, want 125", character.Stats["gold"])
	}
}

func TestOwnerGrantPickupItemOwnsPlacementAndPersistence(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:3": {ItemID: 7001, Count: 2},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, _ := NewOwner(repos)
	result, err := owner.GrantPickupItem(ctx, PickupItemCommand{
		CharacterID: "19",
		Placement: PickupItemPlacement{
			Definition: PickupItemDefinition{
				ItemID:          7001,
				Kind:            PickupItemStackable,
				StackLimit:      10,
				SlotStart:       65,
				SlotEnd:         120,
				PreferQuickSlot: true,
			},
			Amount: 3,
			BuildNew: func(int16) (dnfrepo.ItemStack, error) {
				return dnfrepo.ItemStack{ItemID: 7001, Count: 3}, nil
			},
		},
		Finalize: func(_ int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			stack.RawEntry = []byte{5}
			return stack, nil
		},
	})
	if err != nil {
		t.Fatalf("GrantPickupItem: %v", err)
	}
	if result.Slot != 3 || result.Stack.Count != 5 {
		t.Fatalf("result = %+v", result)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if inventory.Slots["0:3"].Count != 5 ||
		len(inventory.Slots["0:3"].RawEntry) != 1 ||
		inventory.Slots["0:3"].RawEntry[0] != 5 {
		t.Fatalf("inventory = %+v", inventory)
	}
}

func TestPlacePickupItemTreatsZeroStackLimitAsUnlimited(t *testing.T) {
	record := dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:65": {ItemID: 7001, Count: 2},
	}}
	slot, stack, err := PlacePickupItem(&record, PickupItemPlacement{
		Definition: PickupItemDefinition{
			ItemID:     7001,
			Kind:       PickupItemStackable,
			StackLimit: 0,
			SlotStart:  65,
			SlotEnd:    120,
		},
		Amount: 3,
		BuildNew: func(int16) (dnfrepo.ItemStack, error) {
			return dnfrepo.ItemStack{ItemID: 7001, Count: 3}, nil
		},
	})
	if err != nil {
		t.Fatalf("PlacePickupItem: %v", err)
	}
	if slot != 65 || stack.Count != 5 || record.Slots["0:65"].Count != 5 || len(record.Slots) != 1 {
		t.Fatalf("slot=%d stack=%+v inventory=%+v", slot, stack, record.Slots)
	}
}

func TestOwnerGrantTutorialRewardIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	seedDungeonOwnerCharacter(t, ctx, repos, 0)
	expireAt := time.Unix(3000, 0).UTC()
	owner, _ := NewOwner(repos)
	command := TutorialRewardCommand{
		CharacterID: "19",
		Progress:    38,
		Rewards: []TutorialItemReward{{
			Progress:      38,
			ItemID:        8001,
			Count:         2,
			Consumable:    true,
			StackLimit:    10,
			SlotStart:     65,
			SlotEnd:       120,
			ExpireAt:      expireAt,
			PVFPath:       "stackable/test.stk",
			StackableType: "[waste]",
		}},
		Project: func(stack dnfrepo.ItemStack, expiration time.Time) (dnfrepo.ItemStack, error) {
			stack.ExpireAt = expiration
			return stack, nil
		},
	}
	result, err := owner.GrantTutorialReward(ctx, command)
	if err != nil {
		t.Fatalf("GrantTutorialReward: %v", err)
	}
	if !result.Granted || len(result.Rows) != 1 || result.Rows[0].Slot != 3 {
		t.Fatalf("result = %+v", result)
	}
	replay, err := owner.GrantTutorialReward(ctx, command)
	if err != nil {
		t.Fatalf("replay GrantTutorialReward: %v", err)
	}
	if replay.Granted || len(replay.Rows) != 0 {
		t.Fatalf("replay = %+v", replay)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if character.Stats[TutorialRewardMarker(38)] != 1 ||
		inventory.Slots["0:3"].Count != 2 ||
		!inventory.Slots["0:3"].ExpireAt.Equal(expireAt) {
		t.Fatalf("character=%+v inventory=%+v", character, inventory)
	}
}

func TestOwnerConsumeReviveCoinRetainsWalletSlotAtZero(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: 1, Count: 1},
			"0:5": {ItemID: 1, Count: 2},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, _ := NewOwner(repos)
	result, err := owner.ConsumeReviveCoin(ctx, ReviveCoinCommand{
		CharacterID: "19",
		ItemID:      1,
		WalletSlot:  1,
		AllowFree:   true,
		Project: func(_ int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			stack.RawEntry = []byte{byte(stack.Count)}
			return stack, nil
		},
	})
	if err != nil {
		t.Fatalf("ConsumeReviveCoin: %v", err)
	}
	if !result.Consumed || result.FreeRevive || result.Slot != 1 ||
		result.CountAfter != 0 || result.Removed {
		t.Fatalf("result = %+v", result)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if inventory.Slots["0:1"].Count != 0 ||
		inventory.Slots["0:5"].Count != 2 ||
		inventory.Slots["0:1"].Extra["last_consume_source"] != "dungeon_use_coin_revive" {
		t.Fatalf("inventory = %+v", inventory)
	}
}

func TestOwnerMutateOwnedInventoryValidatesCharacterAccount(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:5": {ItemID: 9001, Count: 2},
		},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, _ := NewOwner(repos)
	err := owner.MutateOwnedInventory(ctx, OwnedInventoryMutationCommand{
		AccountID:   "account-1",
		CharacterID: "19",
		Apply: func(slots map[string]dnfrepo.ItemStack) (bool, error) {
			stack := slots["0:5"]
			stack.Count--
			slots["0:5"] = stack
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("MutateOwnedInventory: %v", err)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if inventory.Slots["0:5"].Count != 1 {
		t.Fatalf("inventory = %+v", inventory)
	}
	err = owner.MutateOwnedInventory(ctx, OwnedInventoryMutationCommand{
		AccountID:   "account-2",
		CharacterID: "19",
		Apply: func(map[string]dnfrepo.ItemStack) (bool, error) {
			return true, nil
		},
	})
	if !errors.Is(err, ErrCharacterNotFound) {
		t.Fatalf("wrong account error = %v, want %v", err, ErrCharacterNotFound)
	}
}

func TestOwnerCompleteTutorialPersistsNextLoginState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		Stats: map[string]int64{
			"tutorial_completed": 0,
			"town_id":            99,
		},
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
	owner, _ := NewOwner(repos)
	command := TutorialCompletionCommand{
		CharacterID:  "19",
		CompletedKey: "tutorial_completed",
		Completed:    1,
		NextLogin: map[string]int64{
			"town_id": 1,
			"area_id": 2,
		},
	}
	result, err := owner.CompleteTutorial(ctx, command)
	if err != nil {
		t.Fatalf("CompleteTutorial: %v", err)
	}
	if result.Previous != 0 || !result.Changed {
		t.Fatalf("result = %+v", result)
	}
	replay, err := owner.CompleteTutorial(ctx, command)
	if err != nil {
		t.Fatalf("replay CompleteTutorial: %v", err)
	}
	if replay.Previous != 1 || replay.Changed {
		t.Fatalf("replay = %+v", replay)
	}
	character, _, err := repos.Character.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load character: %v", err)
	}
	if character.Stats["tutorial_completed"] != 1 ||
		character.Stats["town_id"] != 1 ||
		character.Stats["area_id"] != 2 {
		t.Fatalf("character = %+v", character)
	}
}

func TestOwnerCommitSettlementAdvancesAndReplaysReceipt(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatalf("load tables: %v", err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{LuckyStarMetadataKey: "398"},
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       1,
		Stats:       map[string]int64{"exp": 90},
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "19",
		Points: dnfrepo.SkillPointState{
			TotalSP: 10, RemainingSP: 10, SyncedLevel: 1,
		},
	}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	owner, _ := NewOwner(repos)
	result, err := owner.CommitSettlement(ctx, SettlementCommand{
		AccountID:               "account-1",
		CharacterID:             "19",
		CompletionKey:           "run-1",
		Tables:                  tables,
		Experience:              20,
		RecommendedDungeonClear: true,
		RentalPointAward:        1,
		RentalPointLimit:        400,
	})
	if err != nil {
		t.Fatalf("CommitSettlement: %v", err)
	}
	if result.Replayed || result.Character.Level != 2 ||
		result.Character.Stats["exp"] != 110 ||
		result.Character.Stats[adventuregroup.RecommendedDungeonClearStatKey] != 1 ||
		result.ExperienceGain != 20 || result.SPGain != 30 ||
		result.Skill.Points.SyncedLevel != 2 ||
		!result.LuckyStarAttempted || result.LuckyStarBefore != 398 ||
		result.LuckyStarApplied != 1 || result.LuckyStarAfter != 399 {
		t.Fatalf("result = %+v", result)
	}
	account, found, err := repos.Account.Load(ctx, "account-1")
	if err != nil || !found || account.Metadata[LuckyStarMetadataKey] != "399" {
		t.Fatalf("account after settlement=%+v found=%t err=%v", account, found, err)
	}
	replay, err := owner.CommitSettlement(ctx, SettlementCommand{
		AccountID:               "account-1",
		CharacterID:             "19",
		CompletionKey:           "run-1",
		Tables:                  tables,
		Experience:              999,
		RecommendedDungeonClear: true,
	})
	if err != nil {
		t.Fatalf("replay CommitSettlement: %v", err)
	}
	if !replay.Replayed || replay.ExperienceGain != 20 || replay.SPGain != 30 ||
		replay.Character.Level != 2 || replay.Character.Stats["exp"] != 110 ||
		replay.Character.Stats[adventuregroup.RecommendedDungeonClearStatKey] != 1 ||
		!replay.LuckyStarAttempted || replay.LuckyStarBefore != 398 ||
		replay.LuckyStarApplied != 1 || replay.LuckyStarAfter != 399 {
		t.Fatalf("replay = %+v", replay)
	}
	account, found, err = repos.Account.Load(ctx, "account-1")
	if err != nil || !found || account.Metadata[LuckyStarMetadataKey] != "399" {
		t.Fatalf("account after replay=%+v found=%t err=%v", account, found, err)
	}
	for _, test := range []struct {
		completionKey string
		before        uint32
		applied       uint32
		after         uint32
	}{
		{completionKey: "run-2", before: 399, applied: 1, after: 400},
		{completionKey: "run-3", before: 400, applied: 0, after: 400},
	} {
		capped, err := owner.CommitSettlement(ctx, SettlementCommand{
			AccountID:        "account-1",
			CharacterID:      "19",
			CompletionKey:    test.completionKey,
			Tables:           tables,
			Experience:       1,
			RentalPointAward: 1,
			RentalPointLimit: 400,
		})
		if err != nil || !capped.LuckyStarAttempted || capped.LuckyStarBefore != test.before ||
			capped.LuckyStarApplied != test.applied || capped.LuckyStarAfter != test.after {
			t.Fatalf("%s settlement=%+v err=%v", test.completionKey, capped, err)
		}
	}
	account, found, err = repos.Account.Load(ctx, "account-1")
	if err != nil || !found || account.Metadata[LuckyStarMetadataKey] != "400" {
		t.Fatalf("account at cap=%+v found=%t err=%v", account, found, err)
	}
}

func TestPlanSettlementProgressionRejectsCapAboveAbsoluteLevel90(t *testing.T) {
	tables, err := progression.Load(context.Background(), settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	_, err = PlanSettlementProgressionAtCap(
		tables,
		1,
		0,
		100,
		dnfrepo.SkillPointState{SyncedLevel: 1},
		progression.MaximumCharacterLevel+1,
	)
	if !errors.Is(err, ErrSettlementStateInvalid) {
		t.Fatalf("PlanSettlementProgressionAtCap() error = %v, want ErrSettlementStateInvalid", err)
	}
}

func TestParseLuckyStarsDistinguishesMissingFromCorruptMetadata(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want uint32
	}{
		{name: "missing", raw: "", want: 0},
		{name: "whitespace", raw: " \t\r\n", want: 0},
		{name: "zero", raw: "0", want: 0},
		{name: "value", raw: " 399 ", want: 399},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseLuckyStars(test.raw)
			if err != nil || got != test.want {
				t.Fatalf("parseLuckyStars(%q)=%d err=%v want=%d", test.raw, got, err, test.want)
			}
		})
	}
	for _, raw := range []string{"invalid", "-1", "4294967296"} {
		if got, err := parseLuckyStars(raw); err == nil {
			t.Fatalf("parseLuckyStars(%q)=%d, want error", raw, got)
		}
	}
}

func TestOwnerCommitSettlementCorruptLuckyStarMetadataRollsBackAndRetries(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{LuckyStarMetadataKey: "corrupt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 1,
		Stats: map[string]int64{"exp": 90},
	}); err != nil {
		t.Fatal(err)
	}
	initialPoints := dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: initialPoints}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	command := SettlementCommand{
		AccountID: "account-1", CharacterID: "19", CompletionKey: "corrupt-wallet-run",
		Tables: tables, Experience: 20, RecommendedDungeonClear: true,
		RentalPointAward: 1, RentalPointLimit: 400,
	}
	if _, err := owner.CommitSettlement(ctx, command); !errors.Is(err, ErrSettlementStateInvalid) {
		t.Fatalf("CommitSettlement corrupt metadata error=%v", err)
	}
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found || character.Level != 1 || character.Stats["exp"] != 90 ||
		character.Stats[adventuregroup.RecommendedDungeonClearStatKey] != 0 ||
		character.Stats[settlementReceiptPrefix+"version"] != 0 {
		t.Fatalf("rolled-back character=%+v found=%t err=%v", character, found, err)
	}
	skill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found || skill.Points != initialPoints {
		t.Fatalf("rolled-back skill=%+v found=%t err=%v", skill.Points, found, err)
	}
	account, found, err := repos.Account.Load(ctx, "account-1")
	if err != nil || !found || account.Metadata[LuckyStarMetadataKey] != "corrupt" {
		t.Fatalf("rolled-back account=%+v found=%t err=%v", account, found, err)
	}
	account.Metadata[LuckyStarMetadataKey] = "398"
	if err := repos.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	result, err := owner.CommitSettlement(ctx, command)
	if err != nil {
		t.Fatalf("retry CommitSettlement: %v", err)
	}
	if result.Replayed || result.ExperienceGain != 20 || result.LuckyStarBefore != 398 ||
		result.LuckyStarApplied != 1 || result.LuckyStarAfter != 399 {
		t.Fatalf("retry result=%+v", result)
	}
	account, _, _ = repos.Account.Load(ctx, "account-1")
	if account.Metadata[LuckyStarMetadataKey] != "399" {
		t.Fatalf("retry account=%+v", account)
	}
}

func TestOwnerCommitSettlementZeroExperienceStillCommitsRecommendedLuckyStarOnce(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 1,
		Stats: map[string]int64{"exp": 90},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "19",
		Points:      dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	command := SettlementCommand{
		AccountID: "account-1", CharacterID: "19", CompletionKey: "zero-exp-recommended-run",
		Tables: tables, RecommendedDungeonClear: true,
		RentalPointAward: 1, RentalPointLimit: 400,
	}
	result, err := owner.CommitSettlement(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExperienceGain != 0 || result.Character.Level != 1 || result.Character.Stats["exp"] != 90 ||
		result.Character.Stats[adventuregroup.RecommendedDungeonClearStatKey] != 1 ||
		!result.LuckyStarAttempted || result.LuckyStarBefore != 0 || result.LuckyStarApplied != 1 ||
		result.LuckyStarAfter != 1 {
		t.Fatalf("zero-exp result=%+v", result)
	}
	replay, err := owner.CommitSettlement(ctx, command)
	if err != nil || !replay.Replayed || replay.ExperienceGain != 0 || replay.LuckyStarAfter != 1 {
		t.Fatalf("zero-exp replay=%+v err=%v", replay, err)
	}
	account, found, err := repos.Account.Load(ctx, "account-1")
	if err != nil || !found || account.Metadata[LuckyStarMetadataKey] != "1" {
		t.Fatalf("zero-exp account=%+v found=%t err=%v", account, found, err)
	}
}

func TestOwnerCommitSettlementRejectsOrdinaryZeroExperienceWithoutReceipt(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 1,
		Stats: map[string]int64{"exp": 90},
	}); err != nil {
		t.Fatal(err)
	}
	initialPoints := dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: initialPoints}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.CommitSettlement(ctx, SettlementCommand{
		AccountID: "account-1", CharacterID: "19", CompletionKey: "ordinary-zero-exp-run",
		Tables: tables,
	})
	if !errors.Is(err, ErrSettlementStateInvalid) {
		t.Fatalf("ordinary zero-exp error=%v", err)
	}
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found || character.Stats["exp"] != 90 ||
		character.Stats[settlementReceiptPrefix+"version"] != 0 {
		t.Fatalf("ordinary zero-exp character=%+v found=%t err=%v", character, found, err)
	}
	skill, found, err := repos.Skill.Load(ctx, "19")
	if err != nil || !found || skill.Points != initialPoints {
		t.Fatalf("ordinary zero-exp skill=%+v found=%t err=%v", skill.Points, found, err)
	}
}

func TestOwnerCommitSettlementAcceptsOptedInZeroExperience(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 1,
		Stats: map[string]int64{"exp": 90},
	}); err != nil {
		t.Fatal(err)
	}
	initialPoints := dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "19", Points: initialPoints}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	// A raid gate is a legitimate zero-experience completion: its reward is the
	// raid phase settlement. Without the opt-in the zero-experience guard rejected
	// it, the whole settlement chain aborted, and every op72 afterwards answered
	// settlement_result_not_ready (live 2026-09-16T13:07Z).
	command := SettlementCommand{
		AccountID: "account-1", CharacterID: "19", CompletionKey: "raid-gate-zero-exp-run",
		Tables: tables, AllowZeroExperience: true,
	}
	result, err := owner.CommitSettlement(ctx, command)
	if err != nil {
		t.Fatalf("raid gate zero-exp settlement: %v", err)
	}
	if result.ExperienceGain != 0 || result.Character.Stats["exp"] != 90 {
		t.Fatalf("raid gate zero-exp result=%+v", result)
	}
	if result.Character.Stats[settlementReceiptPrefix+"version"] != settlementReceiptVersion {
		t.Fatalf("raid gate zero-exp receipt not written: %+v", result.Character.Stats)
	}
	replay, err := owner.CommitSettlement(ctx, command)
	if err != nil || !replay.Replayed || replay.ExperienceGain != 0 {
		t.Fatalf("raid gate zero-exp replay=%+v err=%v", replay, err)
	}
}

func TestSettlementReceiptValuesAcceptsPreLuckyStarReceipt(t *testing.T) {
	stats := map[string]int64{"exp": 110}
	writeSettlementReceipt(stats, sha256.Sum256([]byte("old-run")), 20, 30, 2, 110, false, 0, 0, 0)
	delete(stats, settlementReceiptPrefix+"lucky_star_attempted")
	delete(stats, settlementReceiptPrefix+"lucky_star_before")
	delete(stats, settlementReceiptPrefix+"lucky_star_applied")
	delete(stats, settlementReceiptPrefix+"lucky_star_after")

	receipt, err := settlementReceiptValues(
		dnfrepo.CharacterRecord{Level: 2, Stats: stats},
		dnfrepo.SkillRecord{Points: dnfrepo.SkillPointState{SyncedLevel: 2}},
	)
	if err != nil || receipt.ExperienceGain != 20 || receipt.SPGain != 30 || receipt.LuckyStarAttempted {
		t.Fatalf("legacy receipt=%+v err=%v", receipt, err)
	}
}

func TestOwnerCommitSettlementAccumulatesMaximumLevelAdventureExperienceOnce(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	honorTables, err := dnfhonor.LoadTables(settlementMemorySource{dnfhonor.TablePath: settlementHonorExpertTestDocument()})
	if err != nil {
		t.Fatal(err)
	}
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       5,
		Stats:       map[string]int64{"exp": 900},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "19",
		Points: dnfrepo.SkillPointState{
			TotalSP: 160, RemainingSP: 160, TotalTP: 4, RemainingTP: 4, SyncedLevel: 5,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	config := adventuregroup.RuntimeConfig{
		ShopCategories: []adventuregroup.ShopCategory{
			{
				Index:              adventuregroup.ShopPointBrave,
				MaxPoint:           250,
				ExperiencePerPoint: 10,
				PointPerExperience: 1,
			},
			{Index: adventuregroup.ShopPointGlory, MaxPoint: 9999},
			{Index: adventuregroup.ShopPointPure},
		},
		Capsule: adventuregroup.CapsuleConfig{
			MinimumExperience: 10,
			MaximumExperience: 100,
			MaximumCount:      10,
			MinimumLevel:      1,
			MaximumLevel:      4,
			GrantedExperience: 1,
		},
	}
	owner, _ := NewOwner(repos)
	command := SettlementCommand{
		AccountID:             "account-1",
		CharacterID:           "19",
		CompletionKey:         "max-level-run",
		Tables:                tables,
		Experience:            25,
		AdventureRuntime:      &config,
		HonorExpertTables:     honorTables,
		MaximumCharacterLevel: 5,
	}
	result, err := owner.CommitSettlement(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Character.Level != 5 || result.Character.Stats["exp"] != 900 ||
		result.HonorExpertGain != 25 ||
		result.Character.Stats[dnfhonor.ExpertLevelStatKey] != 1 ||
		result.Character.Stats[dnfhonor.ExpertProgressExperienceStatKey] != 15 {
		t.Fatalf("result=%+v", result)
	}
	account, found, err := repos.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%v err=%v", found, err)
	}
	runtime, err := adventuregroup.ParseRuntimeState(account, config, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if account.HonorExp != 25 || runtime.ShopPoints[adventuregroup.ShopPointBrave] != 2 ||
		runtime.BraveExperience != 5 || runtime.GrowthExperience != 25 {
		t.Fatalf("account=%+v runtime=%+v", account, runtime)
	}
	command.Experience = 99
	replay, err := owner.CommitSettlement(ctx, command)
	if err != nil || !replay.Replayed {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	account, _, _ = repos.Account.Load(ctx, "account-1")
	runtime, _ = adventuregroup.ParseRuntimeState(account, config, time.Now())
	if account.HonorExp != 25 || runtime.GrowthExperience != 25 ||
		replay.HonorExpertGain != 0 || replay.Character.Stats[dnfhonor.ExpertLevelStatKey] != 1 ||
		replay.Character.Stats[dnfhonor.ExpertProgressExperienceStatKey] != 15 {
		t.Fatalf("replay duplicated account=%+v runtime=%+v", account, runtime)
	}
}

// TestOwnerCommitSettlementRepairsAFullHonorExpertBar pins the 2026-10-03
// operator fix at the boundary it actually broke: the dungeon clear itself.
//
// A stored HonorExpert state whose bar is already full -- progress at or above
// the current level's requirement, or any progress at the final level -- is
// refused by honor.ResolveExpert, and this settlement used to propagate that
// refusal, rolling back the WHOLE clear. The player killed the boss, got no EXP
// and no reward row, and could not leave the instance until the client was
// force-closed:
//
//	"熟练度满级熟练度经验满了之后就无法再通关副本了，打完副本boss的时候
//	 没有经验结算，奖励结算，直接卡在副本里面"
//
// Each shape below used to fail CommitSettlement outright. They must now settle,
// apply the whole award, and land on a state the validator accepts.
func TestOwnerCommitSettlementRepairsAFullHonorExpertBar(t *testing.T) {
	// The honor test document is [1 10, 2 20]: level 0 is left at 10, level 1 at
	// 20, and level 2 is the final row.
	cases := []struct {
		name        string
		storedLevel int64
		storedExp   int64
	}{
		{name: "full bar at level zero", storedLevel: 0, storedExp: 10},
		{name: "bar holding more than one level", storedLevel: 0, storedExp: 304},
		{name: "final level holding progress", storedLevel: 2, storedExp: 1},
		{name: "level beyond the final row", storedLevel: 9, storedExp: 7},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			tables, err := progression.Load(ctx, settlementTestSource())
			if err != nil {
				t.Fatal(err)
			}
			honorTables, err := dnfhonor.LoadTables(settlementMemorySource{dnfhonor.TablePath: settlementHonorExpertTestDocument()})
			if err != nil {
				t.Fatal(err)
			}
			repos := dnfrepomemory.NewMemoryGroup()
			if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "account-1",
				Metadata:  map[string]string{},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19",
				AccountID:   "account-1",
				Level:       5,
				Stats: map[string]int64{
					"exp":                                    900,
					dnfhonor.ExpertLevelStatKey:              test.storedLevel,
					dnfhonor.ExpertProgressExperienceStatKey: test.storedExp,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
				CharacterID: "19",
				Points: dnfrepo.SkillPointState{
					TotalSP: 160, RemainingSP: 160, TotalTP: 4, RemainingTP: 4, SyncedLevel: 5,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "19",
				Slots:       map[string]dnfrepo.ItemStack{},
			}); err != nil {
				t.Fatal(err)
			}
			config := adventuregroup.RuntimeConfig{
				ShopCategories: []adventuregroup.ShopCategory{
					{Index: adventuregroup.ShopPointBrave, MaxPoint: 250, ExperiencePerPoint: 10, PointPerExperience: 1},
					{Index: adventuregroup.ShopPointGlory, MaxPoint: 9999},
					{Index: adventuregroup.ShopPointPure},
				},
				Capsule: adventuregroup.CapsuleConfig{
					MinimumExperience: 10, MaximumExperience: 100, MaximumCount: 10,
					MinimumLevel: 1, MaximumLevel: 4, GrantedExperience: 1,
				},
			}
			owner, _ := NewOwner(repos)
			result, err := owner.CommitSettlement(ctx, SettlementCommand{
				AccountID:             "account-1",
				CharacterID:           "19",
				CompletionKey:         "full-honor-expert-bar-run",
				Tables:                tables,
				Experience:            25,
				AdventureRuntime:      &config,
				HonorExpertTables:     honorTables,
				MaximumCharacterLevel: 5,
			})
			if err != nil {
				t.Fatalf("settlement refused a full expert bar, which is what leaves the player stuck in the dungeon: %v", err)
			}
			if result.HonorExpertGain != 25 {
				t.Fatalf("HonorExpertGain=%d, want the whole 25 post-cap award", result.HonorExpertGain)
			}
			progress := dnfhonor.ExpertProgress{
				Level:                  uint32(result.Character.Stats[dnfhonor.ExpertLevelStatKey]),
				CurrentLevelExperience: uint64(result.Character.Stats[dnfhonor.ExpertProgressExperienceStatKey]),
			}
			if _, err := honorTables.ResolveExpert(progress); err != nil {
				t.Fatalf("settlement persisted a state the validator still refuses: %+v: %v", progress, err)
			}
			persisted, found, err := repos.Character.Load(ctx, "19")
			if err != nil || !found {
				t.Fatalf("load character found=%t err=%v", found, err)
			}
			if persisted.Stats[dnfhonor.ExpertLevelStatKey] != result.Character.Stats[dnfhonor.ExpertLevelStatKey] ||
				persisted.Stats[dnfhonor.ExpertProgressExperienceStatKey] != result.Character.Stats[dnfhonor.ExpertProgressExperienceStatKey] {
				t.Fatalf("persisted expert=%d:%d, returned=%d:%d",
					persisted.Stats[dnfhonor.ExpertLevelStatKey], persisted.Stats[dnfhonor.ExpertProgressExperienceStatKey],
					result.Character.Stats[dnfhonor.ExpertLevelStatKey], result.Character.Stats[dnfhonor.ExpertProgressExperienceStatKey])
			}
			if result.Character.Level != 5 || result.Character.Stats["exp"] != 900 {
				t.Fatalf("the at-cap character moved: level=%d exp=%d",
					result.Character.Level, result.Character.Stats["exp"])
			}
		})
	}
}

// 未满级通关结算现在也要攒骁勇结晶（2026-09-24 口径），但胶囊槽与名誉经验不动；
// 账号行缺失时只跳过记账、结算照样落（角色经验/技能/背包都在后面写），
// 免得一行账本缺失把玩家的通关结算整个卡死。
func TestOwnerCommitSettlementCreditsCrystalsOnlyBelowCap(t *testing.T) {
	ctx := context.Background()
	tables, err := progression.Load(ctx, settlementTestSource())
	if err != nil {
		t.Fatal(err)
	}
	config := adventuregroup.RuntimeConfig{
		ShopCategories: []adventuregroup.ShopCategory{
			{
				Index:              adventuregroup.ShopPointBrave,
				MaxPoint:           250,
				ExperiencePerPoint: 10,
				PointPerExperience: 1,
			},
			{Index: adventuregroup.ShopPointGlory, MaxPoint: 9999},
			{Index: adventuregroup.ShopPointPure},
		},
		Capsule: adventuregroup.CapsuleConfig{
			MinimumExperience: 10,
			MaximumExperience: 100,
			MaximumCount:      10,
			MinimumLevel:      1,
			MaximumLevel:      4,
			GrantedExperience: 1,
		},
	}
	seed := func(t *testing.T, withAccount bool) dnfrepo.Group {
		t.Helper()
		repos := dnfrepomemory.NewMemoryGroup()
		if withAccount {
			if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "account-1",
				Metadata:  map[string]string{},
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: "19",
			AccountID:   "account-1",
			Level:       2,
			Stats:       map[string]int64{"exp": 0},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{
			CharacterID: "19",
			Points:      dnfrepo.SkillPointState{SyncedLevel: 2},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: "19",
			Slots:       map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
		return repos
	}

	repos := seed(t, true)
	owner, _ := NewOwner(repos)
	result, err := owner.CommitSettlement(ctx, SettlementCommand{
		AccountID:             "account-1",
		CharacterID:           "19",
		CompletionKey:         "below-cap-run",
		Tables:                tables,
		Experience:            25,
		AdventureRuntime:      &config,
		MaximumCharacterLevel: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Character.Level != 2 || result.ExperienceGain != 25 {
		t.Fatalf("below-cap result=%+v", result)
	}
	account, found, err := repos.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%v err=%v", found, err)
	}
	runtime, err := adventuregroup.ParseRuntimeState(account, config, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ShopPoints[adventuregroup.ShopPointBrave] != 2 || runtime.BraveExperience != 5 {
		t.Fatalf("below-cap crystals=%+v", runtime)
	}
	if runtime.GrowthExperience != 0 || account.HonorExp != 0 {
		t.Fatalf("below-cap settlement touched capsule/honor: runtime=%+v honor=%d", runtime, account.HonorExp)
	}

	// 账号行缺失：只跳过记账，结算本身必须照常落盘。
	repos = seed(t, false)
	owner, _ = NewOwner(repos)
	if _, err := owner.CommitSettlement(ctx, SettlementCommand{
		AccountID:             "account-1",
		CharacterID:           "19",
		CompletionKey:         "below-cap-run-no-account",
		Tables:                tables,
		Experience:            25,
		AdventureRuntime:      &config,
		MaximumCharacterLevel: 5,
	}); err != nil {
		t.Fatalf("missing account must not fail the settlement: %v", err)
	}
	character, found, err := repos.Character.Load(ctx, "19")
	if err != nil || !found || character.Stats["exp"] != 25 {
		t.Fatalf("settlement did not persist below-cap experience: %+v err=%v", character, err)
	}
}

func settlementHonorExpertTestDocument() string {
	return `[grade]
1 ` + "`effect.ani` `medal.img` 0 `icon.img` 0" + ` 1 0
[/grade]
[maxexp on maxlevel]
1
[expert info]
[grade info]
0 ` + "`challenger`" + `
[min lv]
0
[max lv]
0
[/grade info]
[grade info]
1 ` + "`veteran`" + `
[medal img]
` + "`expert-medal.img`" + ` 0
[icon img]
` + "`expert-icon.img`" + ` 0
[min lv]
1
[max lv]
-1
[/grade info]
[/expert info]
[honor expert exp table]
1 10 2 20
[/honor expert exp table]
`
}

func seedDungeonOwnerCharacter(t *testing.T, ctx context.Context, repos dnfrepo.Group, gold int64) {
	t.Helper()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		Stats:       map[string]int64{"gold": gold},
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
}

type settlementMemorySource map[string]string

func (s settlementMemorySource) ReadText(path string) (string, error) {
	value, ok := s[path]
	if !ok {
		return "", fmt.Errorf("missing test PVF path %s", path)
	}
	return value, nil
}

func settlementTestSource() settlementMemorySource {
	return settlementMemorySource{
		progression.ExperienceTablePath: "100 250 500 900\n",
		progression.SkillPointTablePath: "[sp table]\n1 10\n2 30\n3 30\n4 40\n5 50\n[/sp table]\n[tp table]\n3 2\n4 1\n5 1\n[/tp table]\n",
		progression.QuestParameterPath: `[difficulty]
` + "`N` 100\n`E` 200\n" + `[/difficulty]
[exp reward table]
100 -1
200 -1
300 -1
400 -1
500 -1
[gold reward table]
0 10
[green level penalty]
80
[grey level penalty]
30
`,
	}
}
