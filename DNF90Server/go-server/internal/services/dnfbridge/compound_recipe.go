package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCompoundRecipeOpcode             = uint16(dnfenum.CmdPacketCompoundItem)
	currentCompoundRecipeSuccessBase        = int64(10000)
	currentCompoundRecipeErrorInventoryFull = byte(4)
	currentCompoundRecipeErrorGold          = byte(14)
	currentCompoundRecipeErrorMaterial      = byte(19)
	currentCompoundRecipeErrorInvalid       = byte(179)
	currentCompoundRecipeMaxRows            = 255
)

var (
	errCurrentCompoundRecipeInvalid       = errors.New("compound recipe request or PVF definition is invalid")
	errCurrentCompoundRecipeInventoryFull = errors.New("compound recipe output inventory is full")
	errCurrentCompoundRecipeGold          = errors.New("compound recipe gold is insufficient")
	errCurrentCompoundRecipeMaterial      = errors.New("compound recipe material is insufficient")
)

type currentCompoundRecipeRequest struct {
	SourceValue    uint32
	SourceIsID     bool
	RequestedCount uint16
}

type currentCompoundRecipeItem struct {
	ItemID     uint32
	Count      uint32
	Definition dungeonDropItemDefinition
}

type currentCompoundRecipe struct {
	ItemID          uint32
	PVFPath         string
	CatalogDirect   bool
	Inputs          []currentCompoundRecipeItem
	Outputs         []currentCompoundRecipeItem
	SuccessRate     int64
	MaintainAbility string
	NeedMaterial    []currentCompoundRecipeItem
	NeedSkill       []int64
}

type currentCompoundConsumedRow struct {
	ListType     byte
	Slot         int16
	DeletedCount uint32
}

type currentCompoundOutputRow struct {
	Action byte
	Entry  currentItemListEntry
}

type currentCompoundRecipeResult struct {
	RecipeItemID uint32
	Consumed     []currentCompoundConsumedRow
	Outputs      []currentCompoundOutputRow
	Refreshes    []alignedcmd.ItemSlotRefresh
	GoldAfter    int64
	GoldChanged  bool
}

func decodeCurrentCompoundRecipeRequest(body []byte) (currentCompoundRecipeRequest, error) {
	if len(body) != 7 && len(body) != 9 {
		return currentCompoundRecipeRequest{}, fmt.Errorf("%w: body length=%d", errCurrentCompoundRecipeInvalid, len(body))
	}
	request := currentCompoundRecipeRequest{
		SourceValue:    uint32(body[0]) | uint32(body[1])<<8 | uint32(body[2])<<16 | uint32(body[3])<<24,
		SourceIsID:     body[4] != 0,
		RequestedCount: uint16(body[5]) | uint16(body[6])<<8,
	}
	if body[4] > 1 || request.SourceValue == 0 || request.RequestedCount == 0 {
		return currentCompoundRecipeRequest{}, errCurrentCompoundRecipeInvalid
	}
	if len(body) == 9 && (body[7] != 0 || body[8] != 1) {
		return currentCompoundRecipeRequest{}, fmt.Errorf("%w: unsupported 9-byte tail=%d,%d", errCurrentCompoundRecipeInvalid, body[7], body[8])
	}
	if !request.SourceIsID && request.SourceValue > math.MaxUint16 {
		return currentCompoundRecipeRequest{}, fmt.Errorf("%w: source slot=%d", errCurrentCompoundRecipeInvalid, request.SourceValue)
	}
	return request, nil
}

func parseCurrentCompoundRecipe(catalog *pvfDungeonDropCatalog, recipeItemID uint32) (currentCompoundRecipe, error) {
	if catalog == nil || catalog.source == nil || recipeItemID == 0 {
		return currentCompoundRecipe{}, errCurrentCompoundRecipeInvalid
	}
	recipeDefinition, err := catalog.ResolveItem(recipeItemID)
	if err != nil {
		return currentCompoundRecipe{}, fmt.Errorf("%w: recipe item=%d: %v", errCurrentCompoundRecipeInvalid, recipeItemID, err)
	}
	if recipeDefinition.Kind != dungeonDropItemStackable || normalizeDungeonDropStackableType(recipeDefinition.StackableType) != "[recipe]" {
		return currentCompoundRecipe{}, fmt.Errorf("%w: item=%d stackable_type=%q", errCurrentCompoundRecipeInvalid, recipeItemID, recipeDefinition.StackableType)
	}
	text, err := catalog.source.ReadText(recipeDefinition.PVFPath)
	if err != nil {
		return currentCompoundRecipe{}, fmt.Errorf("%w: read %s: %v", errCurrentCompoundRecipeInvalid, recipeDefinition.PVFPath, err)
	}
	document, err := dnfpvf.Parse(recipeDefinition.PVFPath, text)
	if err != nil {
		return currentCompoundRecipe{}, fmt.Errorf("%w: parse %s: %v", errCurrentCompoundRecipeInvalid, recipeDefinition.PVFPath, err)
	}
	inputs, err := parseCurrentCompoundRecipePairs(catalog, document.Ints("input item"), true)
	if err != nil {
		return currentCompoundRecipe{}, fmt.Errorf("%w: [input item]: %v", errCurrentCompoundRecipeInvalid, err)
	}
	outputs, err := parseCurrentCompoundRecipePairs(catalog, document.Ints("output item"), false)
	if err != nil {
		return currentCompoundRecipe{}, fmt.Errorf("%w: [output item]: %v", errCurrentCompoundRecipeInvalid, err)
	}
	needMaterial, err := parseCurrentCompoundRecipePairs(catalog, document.Ints("need material"), false)
	if err != nil && len(document.Ints("need material")) != 0 {
		return currentCompoundRecipe{}, fmt.Errorf("%w: [need material]: %v", errCurrentCompoundRecipeInvalid, err)
	}
	successRate := currentCompoundRecipeSuccessBase
	if value, found := document.Int("success rate"); found {
		if value < 0 || value > currentCompoundRecipeSuccessBase {
			return currentCompoundRecipe{}, fmt.Errorf("%w: success rate=%d", errCurrentCompoundRecipeInvalid, value)
		}
		successRate = value
	}
	maintainAbility, _ := document.Text("maintain ability")
	stackableTypeValues := document.Ints("stackable type")
	return currentCompoundRecipe{
		ItemID:          recipeItemID,
		PVFPath:         recipeDefinition.PVFPath,
		CatalogDirect:   len(stackableTypeValues) > 0 && stackableTypeValues[0] == 0,
		Inputs:          inputs,
		Outputs:         outputs,
		SuccessRate:     successRate,
		MaintainAbility: normalizeCurrentCompoundToken(maintainAbility),
		NeedMaterial:    needMaterial,
		NeedSkill:       append([]int64(nil), document.Ints("need skill")...),
	}, nil
}

func parseCurrentCompoundRecipePairs(catalog *pvfDungeonDropCatalog, values []int64, allowGold bool) ([]currentCompoundRecipeItem, error) {
	if len(values) == 0 || len(values)%2 != 0 {
		return nil, fmt.Errorf("pair count=%d", len(values))
	}
	order := make([]uint32, 0, len(values)/2)
	totals := make(map[uint32]uint64, len(values)/2)
	definitions := make(map[uint32]dungeonDropItemDefinition, len(values)/2)
	for offset := 0; offset < len(values); offset += 2 {
		itemID, count := values[offset], values[offset+1]
		if itemID < 0 || itemID > math.MaxUint32 || count <= 0 || count > math.MaxUint32 || (itemID == 0 && !allowGold) {
			return nil, fmt.Errorf("invalid pair item=%d count=%d", itemID, count)
		}
		id := uint32(itemID)
		if _, exists := totals[id]; !exists {
			order = append(order, id)
			if id != 0 {
				definition, err := catalog.ResolveItem(id)
				if err != nil {
					return nil, fmt.Errorf("resolve item=%d: %w", id, err)
				}
				definitions[id] = definition
			}
		}
		total := totals[id] + uint64(count)
		if total > math.MaxUint32 {
			return nil, fmt.Errorf("count overflow item=%d", id)
		}
		totals[id] = total
	}
	items := make([]currentCompoundRecipeItem, 0, len(order))
	for _, id := range order {
		items = append(items, currentCompoundRecipeItem{ItemID: id, Count: uint32(totals[id]), Definition: definitions[id]})
	}
	return items, nil
}

func normalizeCurrentCompoundToken(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(value, "`[]")))
}

func resolveCurrentCompoundRecipeID(ctx context.Context, repo dnfrepo.InventoryRepository, characterID string, request currentCompoundRecipeRequest) (uint32, int16, error) {
	// The current client uses source_is_id=1 for recipes selected from the
	// town production catalog. Those rows identify a PVF [recipe] directly;
	// there is no recipe scroll in the character inventory to find or consume.
	// source_is_id=0 is the separate inventory-scroll route and still resolves
	// the recipe through its authoritative source slot below.
	if request.SourceIsID {
		return request.SourceValue, -1, nil
	}
	record, found, err := repo.Load(ctx, characterID)
	if err != nil || !found {
		return 0, -1, fmt.Errorf("%w: inventory load found=%t: %v", errCurrentCompoundRecipeInvalid, found, err)
	}
	slot := int16(request.SourceValue)
	stack, ok := record.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)]
	if !ok || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 {
		return 0, -1, fmt.Errorf("%w: recipe slot=%d", errCurrentCompoundRecipeInvalid, slot)
	}
	return uint32(stack.ItemID), slot, nil
}

func commitCurrentCompoundRecipe(
	ctx context.Context,
	repositories dnfrepo.Group,
	accountID string,
	characterID string,
	request currentCompoundRecipeRequest,
	recipeSourceSlot int16,
	recipe currentCompoundRecipe,
) (currentCompoundRecipeResult, error) {
	if repositories.AccountAssets == nil || recipe.ItemID == 0 || request.RequestedCount == 0 {
		return currentCompoundRecipeResult{}, errCurrentCompoundRecipeInvalid
	}
	if request.SourceIsID && !recipe.CatalogDirect {
		return currentCompoundRecipeResult{}, fmt.Errorf("%w: recipe item=%d is not a direct production-catalog row", errCurrentCompoundRecipeInvalid, recipe.ItemID)
	}
	if recipe.SuccessRate != currentCompoundRecipeSuccessBase {
		return currentCompoundRecipeResult{}, fmt.Errorf("%w: success rate=%d is not yet proven", errCurrentCompoundRecipeInvalid, recipe.SuccessRate)
	}
	if len(recipe.NeedSkill) != 0 {
		return currentCompoundRecipeResult{}, fmt.Errorf("%w: non-empty [need skill] is not yet proven", errCurrentCompoundRecipeInvalid)
	}
	preserveSourceID, preserveOutputID, preserve := currentCompoundPreservedTransform(recipe)
	if preserve && request.RequestedCount != 1 {
		return currentCompoundRecipeResult{}, fmt.Errorf("%w: preserved equipment count=%d", errCurrentCompoundRecipeInvalid, request.RequestedCount)
	}
	var result currentCompoundRecipeResult
	err := repositories.AccountAssets.WithinAccountCharacterAssets(ctx, accountID, characterID, func(
		accountRepo dnfrepo.AccountInventoryRepository,
		characterRepo dnfrepo.CharacterRepository,
		inventoryRepo dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		account, _, err := accountRepo.Load(ctx, accountID)
		if err != nil {
			return err
		}
		account = dnfrepo.CloneAccountInventory(account)
		account.AccountID = accountID
		if account.Slots == nil {
			account.Slots = make(map[string]dnfrepo.ItemStack)
		}
		character, found, err := characterRepo.Load(ctx, characterID)
		if err != nil || !found {
			return fmt.Errorf("%w: character load found=%t: %v", errCurrentCompoundRecipeInvalid, found, err)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		inventory, found, err := inventoryRepo.Load(ctx, characterID)
		if err != nil || !found {
			return fmt.Errorf("%w: inventory load found=%t: %v", errCurrentCompoundRecipeInvalid, found, err)
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		consumed := make([]currentCompoundConsumedRow, 0, len(recipe.Inputs)+2)
		refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(recipe.Inputs)+len(recipe.Outputs)+2)
		if !request.SourceIsID {
			if recipeSourceSlot < 0 {
				return fmt.Errorf("%w: missing inventory recipe source", errCurrentCompoundRecipeInvalid)
			}
			recipeKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, recipeSourceSlot)
			recipeStack, ok := inventory.Slots[recipeKey]
			if !ok || recipeStack.ItemID != int64(recipe.ItemID) || recipeStack.Count < int64(request.RequestedCount) {
				return fmt.Errorf("%w: recipe source changed slot=%d", errCurrentCompoundRecipeInvalid, recipeSourceSlot)
			}
			recipeStack.Count -= int64(request.RequestedCount)
			if recipeStack.Count == 0 {
				delete(inventory.Slots, recipeKey)
			} else {
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, recipeSourceSlot, recipeStack)
				recipeStack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[recipeKey] = recipeStack
			}
			consumed = append(consumed, currentCompoundConsumedRow{
				ListType:     dnfrepo.MainInventoryListType,
				Slot:         recipeSourceSlot,
				DeletedCount: uint32(request.RequestedCount),
			})
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: recipeSourceSlot})
		}
		var preserved dnfrepo.ItemStack
		var preservedSlot int16 = -1
		accountChanged := false
		characterChanged := false
		goldChanged := false
		for _, input := range recipe.Inputs {
			need, ok := scaleCurrentCompoundCount(input.Count, request.RequestedCount)
			if !ok {
				return errCurrentCompoundRecipeInvalid
			}
			if input.ItemID == 0 {
				if character.Stats == nil || character.Stats["gold"] < int64(need) {
					return errCurrentCompoundRecipeGold
				}
				character.Stats["gold"] -= int64(need)
				characterChanged = true
				goldChanged = true
				continue
			}
			if sharedSlot, shared := currentDisjointWarehouseFixedSlots[input.ItemID]; shared {
				key := dnfrepo.AccountSharedInventorySlotKey(sharedSlot)
				stack, found := account.Slots[key]
				if !found || stack.ItemID != int64(input.ItemID) || stack.Count < int64(need) {
					return errCurrentCompoundRecipeMaterial
				}
				stack.Count -= int64(need)
				remaining := stack.Count
				if remaining == 0 {
					delete(account.Slots, key)
				} else {
					entry := currentItemListEntryFromStack(0, sharedSlot, stack)
					stack.RawEntry = append([]byte(nil), entry.data[:]...)
					account.Slots[key] = stack
				}
				accountChanged = true
				consumed = append(consumed, currentCompoundConsumedRow{ListType: 0, Slot: sharedSlot, DeletedCount: need})
				refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: sharedSlot})
				continue
			}
			slots := currentCompoundSlotsForItem(inventory.Slots, int64(input.ItemID))
			available := int64(0)
			for _, slot := range slots {
				available += inventory.Slots[currentCeraShopInventorySlotKey(0, slot)].Count
			}
			if available < int64(need) {
				return errCurrentCompoundRecipeMaterial
			}
			left := int64(need)
			for _, slot := range slots {
				if left == 0 {
					break
				}
				key := currentCeraShopInventorySlotKey(0, slot)
				stack := inventory.Slots[key]
				take := stack.Count
				if take > left {
					take = left
				}
				if preserve && input.ItemID == preserveSourceID && preservedSlot < 0 {
					preserved = cloneCurrentCompoundStack(stack)
					preservedSlot = slot
				}
				stack.Count -= take
				left -= take
				remaining := stack.Count
				if remaining == 0 {
					delete(inventory.Slots, key)
				} else {
					entry := currentItemListEntryFromStack(0, slot, stack)
					stack.RawEntry = append([]byte(nil), entry.data[:]...)
					inventory.Slots[key] = stack
				}
				consumed = append(consumed, currentCompoundConsumedRow{ListType: 0, Slot: slot, DeletedCount: uint32(take)})
				refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: slot})
			}
		}

		outputs := make([]currentCompoundOutputRow, 0, len(recipe.Outputs))
		if preserve {
			if preservedSlot < 0 || preserved.ItemID != int64(preserveSourceID) {
				return errCurrentCompoundRecipeMaterial
			}
			output := recipe.Outputs[0]
			preserved.ItemID = int64(preserveOutputID)
			preserved.Count = 1
			preserved.Extra = cloneCurrentCompoundExtra(preserved.Extra)
			preserved.Extra["item_kind"] = string(dungeonDropItemEquipment)
			preserved.Extra["pvf_path"] = output.Definition.PVFPath
			preserved.Extra["equipment_type"] = output.Definition.EquipmentType
			entry := currentItemListEntryFromStack(0, preservedSlot, preserved)
			preserved.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[currentCeraShopInventorySlotKey(0, preservedSlot)] = preserved
			outputs = append(outputs, currentCompoundOutputRow{Action: 1, Entry: entry})
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: preservedSlot})
		} else {
			for _, output := range recipe.Outputs {
				count, ok := scaleCurrentCompoundCount(output.Count, request.RequestedCount)
				if !ok {
					return errCurrentCompoundRecipeInvalid
				}
				if fixedSlot, sharedItem := dnfrepo.AccountSharedWarehouseSlotForItem(int64(output.ItemID)); sharedItem {
					sharedKey := dnfrepo.AccountSharedInventorySlotKey(fixedSlot)
					_, existed := account.Slots[sharedKey]
					// 统一入口（account_shared_grant.go）。
					grant := grantCurrentAccountSharedFirst(&account, output.Definition, count, "compound_recipe")
					if grant.Changed {
						accountChanged = true
						stack := account.Slots[sharedKey]
						entry := currentItemListEntryFromStack(0, fixedSlot, stack)
						action := byte(0)
						if !existed {
							action = 1
						}
						outputs = append(outputs, currentCompoundOutputRow{Action: action, Entry: entry})
						refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: fixedSlot})
					}
					count = grant.Remainder
				}
				if count == 0 {
					continue
				}
				before := currentCompoundOccupiedSlots(inventory.Slots)
				slots, grantErr := grantCurrentCeraShopProduct(&inventory, output.Definition, count, currentInventoryPlacementMainSlotCountFor(ctx, repositories.Settings, characterID))
				if grantErr != nil {
					return fmt.Errorf("%w: %v", errCurrentCompoundRecipeInventoryFull, grantErr)
				}
				for _, rawSlot := range slots {
					slot := int16(rawSlot)
					stack := inventory.Slots[currentCeraShopInventorySlotKey(0, slot)]
					entry := currentItemListEntryFromStack(0, slot, stack)
					action := byte(0)
					if _, existed := before[slot]; !existed {
						action = 1
					}
					outputs = append(outputs, currentCompoundOutputRow{Action: action, Entry: entry})
					refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: 0, SlotIndex: slot})
				}
			}
		}
		if len(consumed) > currentCompoundRecipeMaxRows || len(outputs) > currentCompoundRecipeMaxRows {
			return errCurrentCompoundRecipeInvalid
		}
		sort.Slice(consumed, func(i, j int) bool {
			if consumed[i].ListType != consumed[j].ListType {
				return consumed[i].ListType < consumed[j].ListType
			}
			return consumed[i].Slot < consumed[j].Slot
		})
		now := time.Now().UTC()
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if accountChanged {
			account.UpdatedAt = now
			if err := accountRepo.Save(ctx, account); err != nil {
				return err
			}
		}
		if characterChanged {
			character.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, characterRepo, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		result = currentCompoundRecipeResult{
			RecipeItemID: recipe.ItemID,
			Consumed:     consumed,
			Outputs:      outputs,
			Refreshes:    refreshes,
			GoldAfter:    character.Stats["gold"],
			GoldChanged:  goldChanged,
		}
		return nil
	})
	return result, err
}

func currentCompoundPreservedTransform(recipe currentCompoundRecipe) (uint32, uint32, bool) {
	if recipe.MaintainAbility != "normal" || len(recipe.Outputs) != 1 || recipe.Outputs[0].Definition.Kind != dungeonDropItemEquipment || recipe.Outputs[0].Count != 1 {
		return 0, 0, false
	}
	var source uint32
	for _, input := range recipe.Inputs {
		if input.Definition.Kind != dungeonDropItemEquipment {
			continue
		}
		if source != 0 || input.Count != 1 {
			return 0, 0, false
		}
		source = input.ItemID
	}
	return source, recipe.Outputs[0].ItemID, source != 0
}

func scaleCurrentCompoundCount(unit uint32, multiplier uint16) (uint32, bool) {
	value := uint64(unit) * uint64(multiplier)
	return uint32(value), value > 0 && value <= math.MaxUint32
}

func currentCompoundSlotsForItem(items map[string]dnfrepo.ItemStack, itemID int64) []int16 {
	slots := make([]int16, 0, 4)
	for key, stack := range items {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || listType != 0 || dnfrepo.IsAccountSharedInventorySlot(0, slot) || stack.ItemID != itemID || stack.Count <= 0 {
			continue
		}
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	return slots
}

func currentCompoundOccupiedSlots(items map[string]dnfrepo.ItemStack) map[int16]struct{} {
	out := make(map[int16]struct{})
	for key := range items {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if ok && listType == 0 {
			out[slot] = struct{}{}
		}
	}
	return out
}

func cloneCurrentCompoundStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = cloneCurrentCompoundExtra(stack.Extra)
	return stack
}

func cloneCurrentCompoundExtra(source map[string]string) map[string]string {
	out := make(map[string]string, len(source)+3)
	for key, value := range source {
		out[key] = value
	}
	return out
}

func buildCurrentCompoundRecipeSuccessBody(result currentCompoundRecipeResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(byte(len(result.Consumed)))
	for _, row := range result.Consumed {
		writer.writeByte(row.ListType)
		writer.writeUint16(uint16(row.Slot))
		writer.writeUint32(row.DeletedCount)
	}
	writer.writeByte(byte(len(result.Outputs)))
	for _, row := range result.Outputs {
		writer.writeByte(row.Action)
		writer.writeBytes(row.Entry.data[:])
	}
	return writer.bytes()
}

func buildCurrentCompoundRecipeFailureBody(code byte) []byte {
	return []byte{0, code}
}

func currentCompoundRecipeErrorCode(err error) byte {
	switch {
	case errors.Is(err, errCurrentCompoundRecipeInventoryFull):
		return currentCompoundRecipeErrorInventoryFull
	case errors.Is(err, errCurrentCompoundRecipeGold):
		return currentCompoundRecipeErrorGold
	case errors.Is(err, errCurrentCompoundRecipeMaterial):
		return currentCompoundRecipeErrorMaterial
	default:
		return currentCompoundRecipeErrorInvalid
	}
}

func (s *Service) handleCurrentCompoundRecipe(session *gameSession, body []byte) error {
	request, err := decodeCurrentCompoundRecipeRequest(body)
	if err != nil || session == nil || session.selectedCharacterID == 0 {
		if err == nil {
			err = errCurrentCompoundRecipeInvalid
		}
		s.logGameEvent(session, "game-compound-recipe-rejected", "reason", err, "body_len", len(body))
		return s.sendGameUpperRawClass(session, currentCompoundRecipeOpcode, buildCurrentCompoundRecipeFailureBody(currentCompoundRecipeErrorCode(err)), dnfproto.DefaultChannelClassification)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return fmt.Errorf("compound recipe repository group unavailable")
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	recipeID, recipeSlot, err := resolveCurrentCompoundRecipeID(context.Background(), repositories.Inventory, characterID, request)
	if err != nil {
		s.logGameEvent(session, "game-compound-recipe-rejected", "reason", err, "source", request.SourceValue, "source_is_id", request.SourceIsID)
		return s.sendGameUpperRawClass(session, currentCompoundRecipeOpcode, buildCurrentCompoundRecipeFailureBody(currentCompoundRecipeErrorCode(err)), dnfproto.DefaultChannelClassification)
	}
	catalog, err := s.currentPVFItemCatalog()
	var recipe currentCompoundRecipe
	if err == nil {
		recipe, err = parseCurrentCompoundRecipe(catalog, recipeID)
		if err == nil {
			var result currentCompoundRecipeResult
			result, err = commitCurrentCompoundRecipe(context.Background(), repositories, s.accountIDForSession(session), characterID, request, recipeSlot, recipe)
			if err == nil {
				s.logGameEvent(session, "game-compound-recipe-committed",
					"recipe_item_id", result.RecipeItemID,
					"pvf_path", recipe.PVFPath,
					"recipe_slot", recipeSlot,
					"requested_count", request.RequestedCount,
					"consumed_rows", len(result.Consumed),
					"output_rows", len(result.Outputs),
					"refresh_rows", len(result.Refreshes),
					"need_material_rows_ignored_for_craft", len(recipe.NeedMaterial))
				if sendErr := s.sendGameUpperRawClass(session, currentCompoundRecipeOpcode, buildCurrentCompoundRecipeSuccessBody(result), dnfproto.DefaultChannelClassification); sendErr != nil {
					return sendErr
				}
				if sendErr := s.sendSelectedIncrementalItemSlotRefreshes(session, "compound_recipe_commit", result.Refreshes); sendErr != nil {
					return sendErr
				}
				if result.GoldChanged {
					return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(result.GoldAfter), 0)
				}
				return nil
			}
		}
	}
	s.logGameEvent(session, "game-compound-recipe-rejected", "recipe_item_id", recipeID, "recipe_slot", recipeSlot, "reason", err)
	return s.sendGameUpperRawClass(session, currentCompoundRecipeOpcode, buildCurrentCompoundRecipeFailureBody(currentCompoundRecipeErrorCode(err)), dnfproto.DefaultChannelClassification)
}
