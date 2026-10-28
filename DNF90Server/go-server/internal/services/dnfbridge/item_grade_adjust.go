package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemgrade "longheng.io/server/internal/modules/dnf/itemgrade"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentResetItemAttrOpcode is CMD 81 (0x0051): equipment grade adjustment box.
const (
	currentResetItemAttrOpcode      uint16 = uint16(dnfenum.CmdPacketResetItemAttr)
	currentResetItemAttrGradeResult uint16 = 2
)

type currentResetItemAttrRequest struct {
	TargetSlot   int16
	TargetItemID int32
	MaterialSlot int16
	Mode         uint16
}

func decodeCurrentResetItemAttrRequest(body []byte) (currentResetItemAttrRequest, error) {
	if len(body) < 8 {
		return currentResetItemAttrRequest{}, fmt.Errorf("reset_item_attr body too short: %d", len(body))
	}
	request := currentResetItemAttrRequest{
		TargetSlot:   int16(binary.LittleEndian.Uint16(body[0:2])),
		TargetItemID: int32(binary.LittleEndian.Uint32(body[2:6])),
		MaterialSlot: int16(binary.LittleEndian.Uint16(body[6:8])),
	}
	if len(body) >= 10 {
		request.Mode = binary.LittleEndian.Uint16(body[8:10])
	}
	return request, nil
}

func (s *Service) handleCurrentResetItemAttr(session *gameSession, body []byte) error {
	request, err := decodeCurrentResetItemAttrRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-reset-item-attr-parse-failed", "body_len", len(body), "err", err)
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 1)
	}
	if len(body) >= 10 {
		reseal, routeErr := s.currentResetItemAttrUsesReseal(session, request)
		if routeErr != nil {
			s.logGameEvent(session, "game-reset-item-attr-route-rejected",
				"mode", request.Mode,
				"target_slot", request.TargetSlot,
				"material_slot", request.MaterialSlot,
				"reason", routeErr)
			return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 3)
		}
		if reseal {
			return s.handleCurrentEquipmentReseal(session, request)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}

	var catalog dnfitemgrade.ItemCatalog
	if pvfCatalog, catalogErr := s.currentPVFItemCatalog(); catalogErr == nil {
		catalog = currentItemGradeCatalog{catalog: pvfCatalog}
	}
	owner, err := dnfitemgrade.NewOwner(repos, catalog, nil)
	if err != nil {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}
	result, err := owner.Adjust(ctx, dnfitemgrade.Command{
		SelectedCharacterID: session.selectedCharacterID,
		TargetSlot:          request.TargetSlot,
		TargetItemID:        request.TargetItemID,
		MaterialSlot:        request.MaterialSlot,
	})
	switch {
	case errors.Is(err, dnfitemgrade.ErrTargetMissing):
		s.logGameEvent(session, "game-reset-item-attr-target-missing",
			"target_slot", request.TargetSlot,
			"target_item_id", request.TargetItemID)
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	case errors.Is(err, dnfitemgrade.ErrMaterialMissing):
		s.logGameEvent(session, "game-reset-item-attr-material-missing",
			"material_slot", request.MaterialSlot)
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 3)
	case err != nil:
		s.logGameEvent(session, "game-reset-item-attr-save-failed", "err", err)
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 4)
	}

	s.logGameEvent(session, "game-reset-item-attr-success",
		"char_id", result.CharacterID,
		"target_slot", result.TargetSlot,
		"target_item_id", result.TargetItemID,
		"material_slot", result.MaterialSlot,
		"material_item_id", result.MaterialItemID,
		"new_seed", result.NewSeed,
		"is_gold", result.GoldKaleido)

	// Current EXE sub_1D16480 reads u16 slot + u32 amount + u16 resultType.
	// Those fields describe the consumed adjustment material, not the target
	// equipment: update/remove the material object at its real slot, then select
	// the native grade-adjust result. Result type 1 is the unrelated item-seal
	// announcement ("item packaged successfully") in this client build.
	var w packetWriter
	w.writeUint16(uint16(result.MaterialSlot))
	w.writeUint32(uint32(result.MaterialRemaining))
	w.writeUint16(currentResetItemAttrGradeResult)
	if err := s.sendGameUpperSuccess(session, currentResetItemAttrOpcode, w.bytes()); err != nil {
		return err
	}

	// The target remains present while op81 consumes only the material. A
	// single-row op14 therefore updates the existing equipment object with the
	// committed full quality seed; unlike the former full op13 refresh it does
	// not rebuild every inventory object or destroy the grade-adjust UI state.
	targetEntry := currentItemListEntryFromStack(0, result.TargetSlot, result.TargetStack)
	refreshBody := buildCurrentItemUpdateBody(0, []currentItemListEntry{targetEntry})
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		refreshBody,
		0,
	)
}

// currentResetItemAttrUsesReseal separates the two native op81 operations.
// The active PVF's ordinary grade box is item 15 with stackable type [etc], so
// op81 must retain the older working rule: mode zero is grade adjustment unless
// the selected material is positively identified as wax. Non-zero modes are
// the account-soul reseal variants and do not carry an ordinary box item.
func (s *Service) currentResetItemAttrUsesReseal(
	session *gameSession,
	request currentResetItemAttrRequest,
) (bool, error) {
	if request.Mode != 0 {
		return true, nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return false, dnfitemgrade.ErrCharacterRequired
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return false, dnfitemgrade.ErrOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	inventory, found, err := repositories.Inventory.Load(ctx, selectedCharacterIDString(session))
	if err != nil {
		return false, err
	}
	if !found {
		return false, dnfitemgrade.ErrOwnerUnavailable
	}
	material, found := inventory.Slots[currentMainInventorySlotKey(request.MaterialSlot)]
	if !found || material.ItemID <= 0 || material.Count <= 0 {
		return false, dnfitemgrade.ErrMaterialMissing
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		// The known-good implementation did not require PVF classification for
		// ordinary boxes. A missing catalog must not turn a valid box into native
		// error code 3; it only prevents the optional wax route from matching.
		return false, nil
	}
	definition, err := catalog.ResolveItem(uint32(material.ItemID))
	if err != nil {
		return false, nil
	}
	if _, wax := currentPVFWaxKind(definition); wax {
		return true, nil
	}
	return false, nil
}

func (s *Service) handleCurrentEquipmentReseal(session *gameSession, request currentResetItemAttrRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}
	resealConfig, err := loadCurrentResealConfig(catalog)
	if err != nil {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}
	owner, err := dnfitemgrade.NewOwner(repositories, nil, nil)
	if err != nil {
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 2)
	}
	characterID := selectedCharacterIDString(session)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	committed, err := owner.Reseal(ctx, currentResealCatalog{catalog: catalog}, currentDomainResealConfig(resealConfig), dnfitemgrade.ResealCommand{
		AccountID: accountID, CharacterID: characterID, TargetSlot: request.TargetSlot,
		TargetItemID: uint32(request.TargetItemID), MaterialSlot: request.MaterialSlot,
		Mode: request.Mode, UpdatedAt: s.gameplayNow(),
	})
	if err != nil {
		s.logGameEvent(session, "game-equipment-reseal-blocked", "mode", request.Mode, "target_slot", request.TargetSlot, "target_item_id", request.TargetItemID, "err", err)
		return s.sendGameUpperFailure(session, currentResetItemAttrOpcode, 3)
	}

	// The op81 success ACK is a **row mutation**, not a result descriptor:
	// sub_1916480 (current EXE) reads u16 slot + u32 amount + u16 resultType and
	// then edits the inventory row named by slot -- amount != 0 sets that row's
	// count, amount == 0 REMOVES the row -- before it branches on resultType to
	// pick the 公告 text (1 = dstr 1699 已成功封装该物品, 2/3 = dstr 1700
	// 已成功重调该物品的品级).
	//
	// So the slot/amount pair must name the CONSUMED MATERIAL, exactly like the
	// grade-adjust path above; the target keeps its slot and is repainted by the
	// row/list packet sent below.
	//
	// 2026-10-04 20:08 crash: this path used to send (TargetSlot, 0, 1). The
	// client read that as "the stack in the equipment's own slot is now empty",
	// deleted the just-packaged equipment object, then dereferenced it while
	// finishing the 封装 flow -> C0000005 at DNF.exe+0x311484C (`mov edx,[esi]`,
	// esi = 0) right after 公告窗 0x298 was opened. Both reseal modes (wax and
	// account-soul) were affected; only the account-soul one had been tried.
	var response packetWriter
	response.writeUint16(uint16(committed.MaterialSlot))
	response.writeUint32(uint32(committed.MaterialRemaining))
	response.writeUint16(1)
	if err := s.sendGameUpperSuccess(session, currentResetItemAttrOpcode, response.bytes()); err != nil {
		return err
	}
	if committed.AccountMaterial {
		body, _, _, built := s.buildCurrentItemListBodyForSession(ctx, session, dnfrepo.MainInventoryListType)
		if !built {
			return errors.New("build reseal main inventory refresh")
		}
		return s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), body)
	}
	entries := []currentItemListEntry{
		currentItemListEntryFromStack(dnfrepo.MainInventoryListType, committed.TargetSlot, committed.TargetStack),
	}
	if inventory, found, loadErr := repositories.Inventory.Load(ctx, characterID); loadErr == nil && found {
		material := inventory.Slots[currentMainInventorySlotKey(committed.MaterialSlot)]
		entries = append(entries, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, committed.MaterialSlot, material))
	}
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, entries), 0)
}

func currentResealCount(stack dnfrepo.ItemStack) byte {
	return dnfitemgrade.ResealCount(stack)
}

func currentItemSealFlag(stack dnfrepo.ItemStack) byte {
	return dnfitemgrade.SealFlag(stack)
}

type currentResealCatalog struct{ catalog *pvfDungeonDropCatalog }

func (c currentResealCatalog) ResolveResealItem(itemID uint32) (dnfitemgrade.ResealItemDefinition, error) {
	definition, err := c.catalog.ResolveItem(itemID)
	if err != nil {
		return dnfitemgrade.ResealItemDefinition{}, err
	}
	oneShot, wax := currentPVFWaxKind(definition)
	return dnfitemgrade.ResealItemDefinition{
		Equipment: definition.Kind == dungeonDropItemEquipment, MinimumLevel: definition.MinimumLevel,
		Rarity: definition.Rarity, Wax: wax, WaxOneShot: oneShot,
		ResealLevelMin: definition.ResealLevelMin, ResealLevelMax: definition.ResealLevelMax,
	}, nil
}

func currentDomainResealConfig(config currentResealConfig) dnfitemgrade.ResealConfig {
	convert := func(rows []currentPVFRangeWeight) []dnfitemgrade.ResealRangeWeight {
		out := make([]dnfitemgrade.ResealRangeWeight, 0, len(rows))
		for _, row := range rows {
			out = append(out, dnfitemgrade.ResealRangeWeight{Minimum: row.Minimum, Maximum: row.Maximum, Weight: row.Weight})
		}
		return out
	}
	return dnfitemgrade.ResealConfig{
		GradeWeights: convert(config.GradeWeights), RarityWeights: config.RarityWeights,
		ExtendedWeights: config.ExtendedWeights, SoulItemIDs: config.SoulItemIDs,
		SoulCostWeights: config.SoulCostWeights, ResealConstants: config.ResealConstants,
		ResealCountLimits: convert(config.ResealCountLimits),
	}
}

func currentMainInventorySlotKey(slot int16) string {
	return fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
}

func consumeCurrentItemByID(slots map[string]dnfrepo.ItemStack, itemID uint32, count int64) (int16, int64, error) {
	if count <= 0 {
		return 0, 0, errors.New("invalid consume count")
	}
	keys := make([]string, 0)
	available := int64(0)
	for key, stack := range slots {
		if stack.ItemID == int64(itemID) && stack.Count > 0 {
			keys = append(keys, key)
			available += stack.Count
		}
	}
	if available < count {
		return 0, 0, errors.New("material insufficient")
	}
	sort.Strings(keys)
	remainingNeed := count
	var lastSlot int16
	var lastRemaining int64
	for _, key := range keys {
		stack := slots[key]
		parts := strings.Split(key, ":")
		if len(parts) == 2 {
			if parsed, parseErr := strconv.ParseInt(parts[1], 10, 16); parseErr == nil {
				lastSlot = int16(parsed)
			}
		}
		used := stack.Count
		if used > remainingNeed {
			used = remainingNeed
		}
		stack.Count -= used
		remainingNeed -= used
		lastRemaining = stack.Count
		if stack.Count == 0 {
			delete(slots, key)
		} else {
			slots[key] = stack
		}
		if remainingNeed == 0 {
			break
		}
	}
	return lastSlot, lastRemaining, nil
}

func firstNonNilError(err error, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

type currentItemGradeCatalog struct {
	catalog *pvfDungeonDropCatalog
}

func (c currentItemGradeCatalog) ResolveItem(itemID uint32) (dnfitemgrade.ItemDefinition, error) {
	if c.catalog == nil {
		return dnfitemgrade.ItemDefinition{}, dnfitemgrade.ErrOwnerUnavailable
	}
	definition, err := c.catalog.ResolveItem(itemID)
	if err != nil {
		return dnfitemgrade.ItemDefinition{}, err
	}
	return dnfitemgrade.ItemDefinition{StackableType: definition.StackableType}, nil
}
