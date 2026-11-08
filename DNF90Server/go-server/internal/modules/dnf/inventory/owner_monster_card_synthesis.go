package inventory

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	monsterCardSynthesisErrorInventoryFull byte = 0x04
	monsterCardSynthesisErrorGold          byte = 0x0a
	monsterCardSynthesisErrorInvalid       byte = 0x13
)

type MonsterCardSynthesisResult struct {
	CharacterID  string
	Success      bool
	ErrorCode    byte
	BinderSlot   int16
	FirstSlot    int16
	SecondSlot   int16
	ResultSlot   int16
	BinderItemID int64
	BinderType   int
	FirstItemID  int64
	SecondItemID int64
	InputSlots   []int16
	ResultItemID int64
	FirstRarity  int
	SecondRarity int
	ResultRarity int
	Chance       int
	Roll         int
	UpgradeRoll  bool
	GoldCost     int64
	UpdatedGold  int64
	ChangedSlots []int16
	Changed      bool
}

// SynthesizeMonsterCards applies op414 as one character-assets transaction.
// The client body has appeared in multiple shapes, so the transaction tests
// each decoded slot interpretation against the durable binder/card identities.
// No random roll happens until a candidate has passed binder/card validation.
func (o *Owner) SynthesizeMonsterCards(
	ctx context.Context,
	cmd Command,
	planner alignedcmd.MonsterCardSynthesisPlanner,
) (MonsterCardSynthesisResult, error) {
	result := MonsterCardSynthesisResult{ErrorCode: monsterCardSynthesisErrorInvalid}
	if o == nil || o.assets == nil || planner == nil || cmd.SelectedCharacterID == 0 {
		return result, ErrMonsterCardSynthesisContract
	}
	if cmd.Operation != "synthesize_monster_card" || len(cmd.MonsterCardSynthSlots) == 0 {
		return result, ErrMonsterCardSynthesisContract
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	result.CharacterID = characterID

	err := o.withinCharacterAssetTransaction(ctx, characterID, func(txOwner *Owner) error {
		character, found, err := txOwner.characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: character=%s", ErrCharacterRequired, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			return fmt.Errorf("%w: character=%s has no stats", ErrWalletTxnRequired, characterID)
		}

		inventory, found, err := txOwner.repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrInventoryNotFound
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			return nil
		}

		var plan alignedcmd.MonsterCardSynthesisPlan
		var binderSlot, firstSlot, secondSlot int16
		var binderType int
		var binderStack dnfrepo.ItemStack
		var cardStacks []dnfrepo.ItemStack
		candidate := cmd.MonsterCardSynthSlots[0]
		if candidate.BinderSlot < 0 || len(candidate.CardSlots) < 2 || len(candidate.CardSlots) > 5 {
			return nil
		}
		needBySlot := make(map[int16]int64)
		for _, slot := range candidate.CardSlots {
			if slot < 0 || slot == candidate.BinderSlot {
				return nil
			}
			needBySlot[slot]++
		}
		binderStack, binderFound := inventory.Slots[slotKey(listTypeMain, candidate.BinderSlot)]
		if !binderFound || binderStack.Count <= 0 {
			return nil
		}
		for slot, count := range needBySlot {
			stack, found := inventory.Slots[slotKey(listTypeMain, slot)]
			if !found || stack.Count < count {
				return nil
			}
		}
		for _, slot := range candidate.CardSlots {
			cardStacks = append(cardStacks, inventory.Slots[slotKey(listTypeMain, slot)])
		}
		candidatePlan, planErr := planner(
			int(candidate.BinderType),
			binderStack,
			cardStacks,
		)
		if planErr != nil {
			return fmt.Errorf(
				"plan monster card synthesis binder=%d cards=%v: %w",
				binderStack.ItemID,
				candidate.CardSlots,
				planErr,
			)
		}
		if planErr == nil && validMonsterCardSynthesisPlan(candidatePlan, binderStack, cardStacks) {
			plan = candidatePlan
			binderSlot = candidate.BinderSlot
			firstSlot = candidate.CardSlots[0]
			secondSlot = candidate.CardSlots[1]
			binderType = int(candidate.BinderType)
		}
		if plan.BinderItemID == 0 {
			return fmt.Errorf(
				"monster card synthesis planner returned an invalid plan binder=%d cards=%v",
				binderStack.ItemID,
				candidate.CardSlots,
			)
		}

		result.BinderSlot = binderSlot
		result.FirstSlot = firstSlot
		result.SecondSlot = secondSlot
		result.ResultSlot = firstSlot
		result.BinderItemID = binderStack.ItemID
		result.BinderType = binderType
		result.FirstItemID = cardStacks[0].ItemID
		result.SecondItemID = cardStacks[1].ItemID
		result.InputSlots = append([]int16(nil), candidate.CardSlots...)
		result.ResultItemID = plan.Output.ItemID
		result.FirstRarity = plan.CardRarities[0]
		result.SecondRarity = plan.CardRarities[1]
		result.ResultRarity = plan.ResultRarity
		result.Chance = plan.Chance
		result.Roll = plan.Roll
		result.UpgradeRoll = plan.UpgradeRoll
		result.GoldCost = plan.GoldCost

		if plan.BinderMinimum > 0 && int64(character.Level) < plan.BinderMinimum {
			return nil
		}
		gold := character.Stats["gold"]
		if plan.GoldCost < 0 || gold < plan.GoldCost {
			result.ErrorCode = monsterCardSynthesisErrorGold
			return nil
		}

		working := cloneItemMap(inventory.Slots)
		if !consumeMonsterCardSynthesisStack(working, binderSlot, binderStack.ItemID) {
			return nil
		}
		for index, slot := range candidate.CardSlots {
			if !consumeMonsterCardSynthesisStack(working, slot, cardStacks[index].ItemID) {
				return nil
			}
		}
		resultSlot, err := placeMonsterCardSynthesisResult(working, plan.Output, plan.StackableType, firstSlot)
		if err != nil {
			result.ErrorCode = monsterCardSynthesisErrorInventoryFull
			return nil
		}

		now := time.Now().UTC()
		inventory.Slots = working
		inventory.UpdatedAt = now
		character.Stats["gold"] = gold - plan.GoldCost
		character.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, txOwner.characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveInventoryFields(ctx, txOwner.repo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}

		changed := map[int16]struct{}{binderSlot: {}, resultSlot: {}}
		for _, slot := range candidate.CardSlots {
			changed[slot] = struct{}{}
		}
		result.ResultSlot = resultSlot
		result.UpdatedGold = character.Stats["gold"]
		result.ChangedSlots = sortedMonsterCardSynthesisSlots(changed)
		result.Success = true
		result.ErrorCode = 0
		result.Changed = true
		return nil
	})
	if err != nil {
		return MonsterCardSynthesisResult{}, err
	}
	return result, nil
}

func validMonsterCardSynthesisPlan(
	plan alignedcmd.MonsterCardSynthesisPlan,
	binder dnfrepo.ItemStack,
	cards []dnfrepo.ItemStack,
) bool {
	reward := plan.Output
	if len(plan.CardItemIDs) != len(cards) || len(plan.CardRarities) != len(cards) {
		return false
	}
	for index, card := range cards {
		if plan.CardItemIDs[index] != card.ItemID {
			return false
		}
	}
	return plan.BinderItemID == binder.ItemID &&
		reward.ItemID > 0 &&
		reward.Kind == "stackable" &&
		reward.TargetListType == listTypeMain &&
		reward.PVFPath != "" &&
		reward.SlotStart >= 0 &&
		reward.SlotEnd >= reward.SlotStart &&
		reward.StackLimit >= 0
}

func consumeMonsterCardSynthesisStack(items map[string]dnfrepo.ItemStack, slot int16, expectedItemID int64) bool {
	key := slotKey(listTypeMain, slot)
	stack, found := items[key]
	if !found || stack.ItemID != expectedItemID || stack.Count <= 0 {
		return false
	}
	if stack.Count == 1 {
		delete(items, key)
		return true
	}
	stack = cloneStack(stack)
	stack.Count--
	updateStackRawAmount(&stack)
	items[key] = stack
	return true
}

func placeMonsterCardSynthesisResult(
	items map[string]dnfrepo.ItemStack,
	output alignedcmd.MagicBoxRewardItem,
	stackableType string,
	preferred int16,
) (int16, error) {
	limit := output.StackLimit
	if limit <= 0 {
		limit = math.MaxInt32
	}
	if limit <= 0 || dnfrepo.ContainerItemSpace(items, listTypeMain, output.ItemID, limit) <= 0 {
		return -1, fmt.Errorf("monster card synthesis result has no stack capacity")
	}

	for slot := output.SlotStart; slot <= output.SlotEnd; slot++ {
		if slot == preferred {
			continue
		}
		key := slotKey(listTypeMain, slot)
		stack, found := items[key]
		if !found || stack.ItemID != output.ItemID || stack.Count <= 0 || stack.Count >= limit ||
			stack.Bind != output.Seal || !stack.ExpireAt.Equal(output.ExpireAt) ||
			monsterCardUpgradeCountOf(stack) != 0 {
			continue
		}
		stack = cloneStack(stack)
		stack.Count++
		updateStackRawAmount(&stack)
		items[key] = stack
		return slot, nil
	}

	candidateSlots := []int16{preferred}
	for slot := output.SlotStart; slot <= output.SlotEnd; slot++ {
		if slot != preferred {
			candidateSlots = append(candidateSlots, slot)
		}
	}
	for _, slot := range candidateSlots {
		if slot < output.SlotStart || slot > output.SlotEnd {
			continue
		}
		key := slotKey(listTypeMain, slot)
		if _, occupied := items[key]; occupied {
			continue
		}
		stack := dnfrepo.ItemStack{
			ItemID:   output.ItemID,
			Count:    1,
			Bind:     output.Seal,
			ExpireAt: output.ExpireAt,
			Extra: map[string]string{
				"source":         "monster_card_synthesis",
				"item_kind":      "stackable",
				"pvf_path":       output.PVFPath,
				"stackable_type": stackableType,
			},
		}
		if !output.ExpireAt.IsZero() && output.ExpireAt.Unix() > 0 {
			expire := strconv.FormatInt(output.ExpireAt.Unix(), 10)
			stack.Extra["expire_time"] = expire
			stack.Extra["expire_unix"] = expire
		}
		if output.UsablePeriodDays > 0 {
			stack.Extra["usable_period_days"] = strconv.FormatInt(output.UsablePeriodDays, 10)
		}
		updateStackRawAmount(&stack)
		items[key] = stack
		return slot, nil
	}
	return -1, fmt.Errorf("monster card synthesis result has no free slot")
}

func sortedMonsterCardSynthesisSlots(values map[int16]struct{}) []int16 {
	result := make([]int16, 0, len(values))
	for slot := range values {
		result = append(result, slot)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
