package inventory

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const (
	testCardBinder = int64(2600479)
	testCardInputA = int64(3619)
	testCardInputB = int64(3620)
	testCardOutput = int64(3610)
)

func monsterCardSynthesisCommand(binderSlot, firstSlot, secondSlot int16) Command {
	return NewMonsterCardSynthesisCommand(alignedcmd.Request{
		AccountID:           "acc",
		SelectedCharacterID: 77,
	}, MonsterCardSynthesisRequest{
		Candidates: []MonsterCardSynthesisSlotCandidate{{
			BinderSlot: binderSlot,
			BinderType: 0,
			CardSlots:  []int16{firstSlot, secondSlot},
			Layout:     "test",
		}},
		RawBody: []byte{
			byte(binderSlot), byte(uint16(binderSlot) >> 8),
			0,
			byte(firstSlot), byte(uint16(firstSlot) >> 8),
			byte(secondSlot), byte(uint16(secondSlot) >> 8),
		},
	})
}

func testMonsterCardSynthesisPlanner(
	binderType int,
	binder dnfrepo.ItemStack,
	cards []dnfrepo.ItemStack,
) (alignedcmd.MonsterCardSynthesisPlan, error) {
	if (binderType != 0 && binderType != 0xff) ||
		binder.ItemID != testCardBinder || len(cards) != 2 ||
		cards[0].ItemID != testCardInputA ||
		cards[1].ItemID != testCardInputB {
		return alignedcmd.MonsterCardSynthesisPlan{}, ErrMonsterCardSynthesisContract
	}
	return alignedcmd.MonsterCardSynthesisPlan{
		BinderItemID:  binder.ItemID,
		CardItemIDs:   []int64{cards[0].ItemID, cards[1].ItemID},
		BinderMinimum: 0,
		CardRarities:  []int{0, 0},
		ResultRarity:  1,
		Chance:        40000,
		Roll:          0,
		UpgradeRoll:   true,
		GoldCost:      10000,
		StackableType: "[material expert job]",
		Output: alignedcmd.MagicBoxRewardItem{
			ItemID:         testCardOutput,
			Kind:           "stackable",
			TargetListType: listTypeMain,
			StackLimit:     1000,
			SlotStart:      233,
			SlotEnd:        236,
			PVFPath:        "stackable/monstercard/result.stk",
		},
	}, nil
}

func TestDecodeMonsterCardSynthesisRequestAcceptsObservedBodies(t *testing.T) {
	tests := []struct {
		body  []byte
		count int
	}{
		{[]byte{98, 0, 2, 236, 0, 236, 0}, 2},
		{[]byte{98, 0, 3, 236, 0, 236, 0, 237, 0}, 3},
		{[]byte{98, 0, 4, 236, 0, 236, 0, 237, 0, 238, 0}, 4},
		{[]byte{98, 0, 5, 236, 0, 236, 0, 237, 0, 238, 0, 239, 0}, 5},
	}
	for _, test := range tests {
		body := test.body
		got, err := DecodeMonsterCardSynthesisRequest(body)
		if err != nil {
			t.Fatalf("body=% X: %v", body, err)
		}
		found := false
		for _, candidate := range got.Candidates {
			if candidate.BinderSlot == 98 && candidate.BinderType == 0xff &&
				len(candidate.CardSlots) == test.count &&
				candidate.CardSlots[0] == 236 && candidate.CardSlots[1] == 236 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("body=% X candidates=%+v do not contain 98/inferred-type/236/236", body, got.Candidates)
		}
	}
}

func TestOwnerMonsterCardSynthesisCommitsBinderTwoCardsAndOutput(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 50000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 230): testMonsterCardStack(testCardBinder, 2, 0),
		slotKey(listTypeMain, 231): testMonsterCardStack(testCardInputA, 1, 0),
		slotKey(listTypeMain, 232): testMonsterCardStack(testCardInputB, 1, 0),
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.SynthesizeMonsterCards(
		ctx,
		monsterCardSynthesisCommand(230, 231, 232),
		testMonsterCardSynthesisPlanner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.ResultItemID != testCardOutput || result.ResultSlot != 233 ||
		result.GoldCost != 10000 || result.UpdatedGold != 40000 {
		t.Fatalf("result=%+v", result)
	}
	inventory, found, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if binder := inventory.Slots[slotKey(listTypeMain, 230)]; binder.ItemID != testCardBinder || binder.Count != 1 {
		t.Fatalf("binder=%+v", binder)
	}
	if _, found := inventory.Slots[slotKey(listTypeMain, 231)]; found {
		t.Fatal("first input card was not consumed")
	}
	if _, found := inventory.Slots[slotKey(listTypeMain, 232)]; found {
		t.Fatal("second input card was not consumed")
	}
	output := inventory.Slots[slotKey(listTypeMain, 233)]
	if output.ItemID != testCardOutput || output.Count != 1 || output.Extra["pvf_path"] != "stackable/monstercard/result.stk" {
		t.Fatalf("output=%+v", output)
	}
	if got := loadMonsterCardGold(t, repos); got != 40000 {
		t.Fatalf("gold=%d want=40000", got)
	}
}

func TestOwnerMonsterCardSynthesisRollsBackOnInsufficientGold(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 230): testMonsterCardStack(testCardBinder, 1, 0),
		slotKey(listTypeMain, 231): testMonsterCardStack(testCardInputA, 1, 0),
		slotKey(listTypeMain, 232): testMonsterCardStack(testCardInputB, 1, 0),
	}
	saveMonsterCardFixture(t, repos, 9999, slots)
	owner, _ := NewOwner(repos)
	result, err := owner.SynthesizeMonsterCards(
		ctx,
		monsterCardSynthesisCommand(230, 231, 232),
		testMonsterCardSynthesisPlanner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success || result.ErrorCode != monsterCardSynthesisErrorGold {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	for _, slot := range []int16{230, 231, 232} {
		if _, found := inventory.Slots[slotKey(listTypeMain, slot)]; !found {
			t.Fatalf("slot %d changed after rejected synthesis", slot)
		}
	}
}

func TestOwnerMonsterCardSynthesisUsesOneStackAsTwoInputs(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 50000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 230): testMonsterCardStack(testCardBinder, 1, 0),
		slotKey(listTypeMain, 231): testMonsterCardStack(testCardInputA, 2, 0),
	})
	owner, _ := NewOwner(repos)
	planner := func(
		binderType int,
		binder dnfrepo.ItemStack,
		cards []dnfrepo.ItemStack,
	) (alignedcmd.MonsterCardSynthesisPlan, error) {
		if binderType != 0 || binder.ItemID != testCardBinder ||
			len(cards) != 2 || cards[0].ItemID != testCardInputA ||
			cards[1].ItemID != testCardInputA {
			return alignedcmd.MonsterCardSynthesisPlan{}, ErrMonsterCardSynthesisContract
		}
		plan, _ := testMonsterCardSynthesisPlanner(
			binderType,
			binder,
			[]dnfrepo.ItemStack{
				{ItemID: testCardInputA},
				{ItemID: testCardInputB},
			},
		)
		plan.CardItemIDs[1] = testCardInputA
		plan.CardRarities[1] = 0
		return plan, nil
	}
	result, err := owner.SynthesizeMonsterCards(
		ctx,
		monsterCardSynthesisCommand(230, 231, 231),
		planner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if _, found := inventory.Slots[slotKey(listTypeMain, 231)]; found {
		t.Fatal("the two-card source stack was not consumed")
	}
}

func TestOwnerMonsterCardSynthesisConsumesThreeCards(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 50000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 230): testMonsterCardStack(testCardBinder, 1, 0),
		slotKey(listTypeMain, 231): testMonsterCardStack(testCardInputA, 2, 0),
		slotKey(listTypeMain, 232): testMonsterCardStack(testCardInputB, 1, 0),
	})
	owner, _ := NewOwner(repos)
	cmd := monsterCardSynthesisCommand(230, 231, 232)
	cmd.MonsterCardSynthSlots[0].CardSlots = []int16{231, 231, 232}
	planner := func(
		binderType int,
		binder dnfrepo.ItemStack,
		cards []dnfrepo.ItemStack,
	) (alignedcmd.MonsterCardSynthesisPlan, error) {
		if binderType != 0 || binder.ItemID != testCardBinder || len(cards) != 3 {
			return alignedcmd.MonsterCardSynthesisPlan{}, ErrMonsterCardSynthesisContract
		}
		return alignedcmd.MonsterCardSynthesisPlan{
			BinderItemID:  binder.ItemID,
			CardItemIDs:   []int64{testCardInputA, testCardInputA, testCardInputB},
			CardRarities:  []int{0, 0, 0},
			BinderMinimum: 0,
			ResultRarity:  1,
			Chance:        64000,
			Roll:          0,
			UpgradeRoll:   true,
			GoldCost:      10000,
			StackableType: "[material expert job]",
			Output: alignedcmd.MagicBoxRewardItem{
				ItemID:         testCardOutput,
				Kind:           "stackable",
				TargetListType: listTypeMain,
				StackLimit:     1000,
				SlotStart:      233,
				SlotEnd:        236,
				PVFPath:        "stackable/monstercard/result.stk",
			},
		}, nil
	}
	result, err := owner.SynthesizeMonsterCards(ctx, cmd, planner)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	if _, found := inventory.Slots[slotKey(listTypeMain, 231)]; found {
		t.Fatal("three-card synthesis left the repeated input slot behind")
	}
	if _, found := inventory.Slots[slotKey(listTypeMain, 232)]; found {
		t.Fatal("three-card synthesis left the third input slot behind")
	}
}

func TestMonsterCardSynthesisSuccessAckMatchesObservedLayout(t *testing.T) {
	outputID := uint32(testCardOutput)
	body := buildMonsterCardSynthesisSuccessAck(MonsterCardSynthesisResult{
		BinderSlot:   230,
		FirstSlot:    231,
		SecondSlot:   232,
		ResultSlot:   233,
		ResultItemID: testCardOutput,
	})
	if len(body) != 20 {
		t.Fatalf("ack length=%d want=20 body=% X", len(body), body)
	}
	if body[0] != 1 ||
		body[1] != byte(230) || body[2] != 0 ||
		body[3] != 2 ||
		body[4] != byte(231) || body[5] != 0 ||
		body[6] != byte(232) || body[7] != 0 ||
		body[8] != 1 || body[9] != byte(233) || body[10] != 0 ||
		body[11] != byte(outputID) ||
		body[12] != byte(outputID>>8) ||
		body[13] != byte(outputID>>16) ||
		body[14] != byte(outputID>>24) ||
		body[15] != 1 || body[16] != 0 || body[17] != 0 || body[18] != 0 ||
		body[19] != 1 {
		t.Fatalf("ack=% X", body)
	}
}

func TestHandlerMonsterCardSynthesisReturnsResultAckAndSlotRefreshes(t *testing.T) {
	repos := dnfrepomemory.NewMemoryGroup()
	saveMonsterCardFixture(t, repos, 50000, map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 230): testMonsterCardStack(testCardBinder, 1, 0),
		slotKey(listTypeMain, 231): testMonsterCardStack(testCardInputA, 1, 0),
		slotKey(listTypeMain, 232): testMonsterCardStack(testCardInputB, 1, 0),
	})
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:                      uint16(dnfenum.CmdPacketMonstercardBind),
		Body:                        []byte{230, 0, 0, 231, 0, 232, 0},
		AccountID:                   "acc",
		SelectedCharacterID:         77,
		Repositories:                repos,
		MonsterCardSynthesisPlanner: testMonsterCardSynthesisPlanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed ||
		result.Operation != "synthesize_monster_card" ||
		len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	ack := result.UpperResponses[0]
	if ack.Classification != 1 ||
		ack.MsgID != uint16(dnfenum.CmdPacketMonstercardBind) ||
		!bytes.Equal(ack.Body, buildMonsterCardSynthesisSuccessAck(MonsterCardSynthesisResult{
			BinderSlot:   230,
			FirstSlot:    231,
			SecondSlot:   232,
			ResultSlot:   233,
			ResultItemID: testCardOutput,
		})) {
		t.Fatalf("ack=class%d msg=0x%04X body=% X", ack.Classification, ack.MsgID, ack.Body)
	}
	if len(result.ItemSlotRefreshes) != 4 ||
		result.CharacterGoldAfter == nil ||
		*result.CharacterGoldAfter != 40000 {
		t.Fatalf("refreshes=%+v gold=%v", result.ItemSlotRefreshes, result.CharacterGoldAfter)
	}
}
