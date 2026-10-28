package dnfbridge

import (
	"context"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func equipmentDisjointGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketDisjointItem)
	return gameplayModuleDefinition{
		Name: "equipment-disjoint",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentDisjointItem(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-disjoint-item-blocked", "current_exe_op26_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentDisjointItem(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			opcode: func(body []byte) []byte {
				if len(body) == currentDisjointItemRequestSize+4 || len(body) == currentDisjointItemRequestSize+8 {
					return append([]byte(nil), body[:len(body)-4]...)
				}
				return body
			},
		},
	}
}

func (s *Service) handleCurrentDisjointItem(session *gameSession, body []byte) error {
	batch, isBatch, err := parseCurrentDisjointBatchRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-disjoint-item-batch-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDisjointItem), 4)
	}
	if isBatch {
		return s.handleCurrentDisjointItemBatch(session, batch)
	}
	request, err := parseCurrentDisjointItemRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-disjoint-item-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDisjointItem), 4)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	if err == nil {
		err = s.commitCurrentDisjointItem(ctx, session, catalog, request)
	}
	if err != nil {
		s.logGameEvent(session, "game-disjoint-item-rejected", "slot", request.SourceSlot, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDisjointItem), currentDisjointFailureResult(err))
	}
	return nil
}

func (s *Service) handleCurrentDisjointItemBatch(session *gameSession, request currentDisjointBatchRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	if err == nil {
		err = s.commitCurrentDisjointItemBatch(ctx, session, catalog, request)
	}
	if err != nil {
		s.logGameEvent(session, "game-disjoint-item-batch-rejected", "slot_count", len(request.Slots), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketDisjointItem), currentDisjointFailureResult(err))
	}
	return nil
}

// currentDisjointerRulesForDisjoint exposes the disjointer.exj tables to
// ordinary disassembly. A missing or unparsable expert-job catalog degrades to
// the pure etc/disjoint.etc behaviour instead of refusing the disassembly.
func (s *Service) currentDisjointerRulesForDisjoint(session *gameSession) currentDisjointerRules {
	catalog, err := s.currentExpertJobCatalog()
	if err != nil {
		s.logGameEvent(session, "game-disjoint-item-disjointer-rules-unavailable", "error", err)
		return nil
	}
	config, ok := catalog.Disjointer()
	if !ok || config == nil {
		return nil
	}
	return config
}

func (s *Service) commitCurrentDisjointItem(ctx context.Context, session *gameSession, catalog *pvfDungeonDropCatalog, request currentDisjointItemRequest) error {
	if s == nil || session == nil || catalog == nil {
		return errCurrentDisjointUnavailable
	}
	config, err := s.currentDisjointPVFConfigCached(catalog.source)
	if err != nil {
		return err
	}
	rules := s.currentDisjointerRulesForDisjoint(session)
	result, err := s.commitCurrentDisjoint(ctx, session, catalog, request.SourceSlot, 0, func(def dungeonDropItemDefinition, document *dnfpvf.Document, source dnfrepo.ItemStack) ([]currentDisjointReward, error) {
		if def.Kind != dungeonDropItemEquipment || currentDisjointDocumentIsAvatar(document) {
			return nil, errCurrentDisjointSourceInvalid
		}
		return currentEquipmentDisjointRewardsForSource(config, rules, def.ItemID, document, source)
	})
	if err != nil {
		return err
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "equipment_disjoint_source", []alignedcmd.ItemSlotRefresh{{
		ListType:  request.ListType,
		SlotIndex: request.SourceSlot,
	}}); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketDisjointItem), buildCurrentDisjointItemSuccessBody(request, result.Rewards), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(result.Rewards))
	for _, reward := range result.Rewards {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: reward.Slot,
		})
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "equipment_disjoint", refreshes); err != nil {
		return err
	}
	s.logGameEvent(session, "game-disjoint-item-committed", "slot", request.SourceSlot, "tool_slot", request.ToolSlot, "reward_rows", len(result.Rewards), "popup", "current_exe_op26_granted_amount", "inventory_refresh", "repository_backed_op14_final_stack_amount")
	return nil
}

func (s *Service) commitCurrentDisjointItemBatch(ctx context.Context, session *gameSession, catalog *pvfDungeonDropCatalog, request currentDisjointBatchRequest) error {
	if s == nil || session == nil || catalog == nil {
		return errCurrentDisjointUnavailable
	}
	config, err := s.currentDisjointPVFConfigCached(catalog.source)
	if err != nil {
		return err
	}
	rules := s.currentDisjointerRulesForDisjoint(session)
	results, err := s.commitCurrentDisjointBatch(ctx, session, catalog, request.Slots, request.ListType, func(def dungeonDropItemDefinition, document *dnfpvf.Document, source dnfrepo.ItemStack) ([]currentDisjointReward, error) {
		if def.Kind != dungeonDropItemEquipment || currentDisjointDocumentIsAvatar(document) {
			return nil, errCurrentDisjointSourceInvalid
		}
		return currentEquipmentDisjointRewardsForSource(config, rules, def.ItemID, document, source)
	})
	if err != nil {
		return err
	}
	mergedRewards, err := mergeCurrentDisjointBatchRewards(results)
	if err != nil {
		return err
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(results)*3)
	for index, result := range results {
		// The native per-item sender normally establishes client-side pending
		// source-slot state before each op26 response. The marked batch bypasses
		// those individual sends, so publish the authoritative empty/final source
		// rows explicitly after the single merged result body. If a reward reused a
		// consumed slot, the repository-backed refresh emits that final stack
		// instead of an empty row.
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  request.ListType,
			SlotIndex: request.Slots[index],
		})
		for _, reward := range result.Rewards {
			refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: dnfrepo.MainInventoryListType, SlotIndex: reward.Slot})
		}
	}
	itemRequest := currentDisjointItemRequest{
		SourceSlot: request.Slots[0],
		ListType:   request.ListType,
		ToolSlot:   request.ToolSlot,
		Context:    request.Context,
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketDisjointItem), buildCurrentDisjointItemSuccessBody(itemRequest, mergedRewards), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "equipment_disjoint_batch", refreshes); err != nil {
		return err
	}
	s.logGameEvent(session, "game-disjoint-item-batch-committed", "slot_count", len(results), "tool_slot", request.ToolSlot, "reward_rows", len(mergedRewards), "popup", "single_merged_op26_granted_amount", "inventory_refresh", "repository_backed_op14_source_slots_and_final_stack_amount")
	return nil
}

func mergeCurrentDisjointBatchRewards(results []currentDisjointResult) ([]currentDisjointRewardSlot, error) {
	merged := make([]currentDisjointRewardSlot, 0, len(results))
	byItem := make(map[uint32]int, len(results))
	for _, result := range results {
		for _, reward := range result.Rewards {
			if reward.ItemID == 0 || reward.Granted == 0 {
				return nil, errCurrentDisjointRewardInvalid
			}
			if index, found := byItem[reward.ItemID]; found {
				if merged[index].Granted > ^uint32(0)-reward.Granted {
					return nil, errCurrentDisjointRewardInvalid
				}
				merged[index].Granted += reward.Granted
				// Keep the latest/final repository slot for the popup row. The
				// following op14 refresh remains authoritative for every touched
				// slot and final stack amount.
				merged[index].Slot = reward.Slot
				merged[index].Count = reward.Count
				continue
			}
			if len(merged) >= currentDisjointMaxRewards {
				return nil, errCurrentDisjointRewardInvalid
			}
			byItem[reward.ItemID] = len(merged)
			merged = append(merged, reward)
		}
	}
	if len(merged) == 0 {
		return nil, errCurrentDisjointRewardInvalid
	}
	return merged, nil
}
