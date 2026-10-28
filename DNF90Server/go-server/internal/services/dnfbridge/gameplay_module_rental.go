package dnfbridge

import (
	"context"
	"errors"
	"math"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func rentalGameplayModule() gameplayModuleDefinition {
	rentOpcode := uint16(dnfenum.CmdPacketRentEquipmentItem)
	chargeOpcode := uint16(dnfenum.CmdPacketChargeRentpoint)
	return gameplayModuleDefinition{
		Name: "equipment-rental",
		LegacyHandlers: map[uint16]gameplayHandler{
			rentOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentRentEquipment(session, request.Body)
			},
			chargeOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentChargeRentalPoint(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			rentOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-rental-equipment-request-blocked", "current_exe_op999_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentRentEquipment(session, request.Body)
			},
			chargeOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-upper-rental-point-charge-request-blocked", "current_exe_op1000_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentChargeRentalPoint(session, request.Body)
			},
		},
		LegacyNormalizers: map[uint16]gameplayLegacyNormalizer{
			rentOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentRentalRequestWireSize)
			},
			chargeOpcode: func(body []byte) []byte {
				return stripLegacyTransportTrailer(body, currentRentalRequestWireSize)
			},
		},
	}
}

func (s *Service) handleCurrentRentEquipment(session *gameSession, body []byte) error {
	s.logInfo("rental equipment request received", "conn_id", rentalSessionConnID(session), "body_len", len(body))
	request, err := decodeCurrentRentEquipmentRequest(body)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "body_len", len(body), "reason", err)
		return nil
	}
	s.logInfo("rental equipment request decoded", "conn_id", rentalSessionConnID(session), "item_id", request.ItemID, "packed_job_tier", request.PackedJobTier)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, characterID, character, err := s.currentRentalSelectedCharacter(ctx, session)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "reason", err)
		return nil
	}
	catalog, source, items, err := s.currentRentalSources(ctx)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "reason", err)
		return nil
	}
	jobValue := numericCharacterStat(character.Job)
	if jobValue < 0 || jobValue > math.MaxUint16 {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "job", character.Job, "reason", "job_out_of_u16_range")
		return nil
	}
	jobTag, err := currentRentalCharacterJobTag(source, byte(jobValue))
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "job", jobValue, "reason", err)
		return nil
	}
	now := time.Now().UTC()
	effectiveLevel, overEquipContract := currentRentalEffectiveLevel(
		ctx,
		repositories.Account,
		s.accountIDForSession(session),
		character.Level,
		now,
	)
	tier, ok := catalog.tierForLevel(effectiveLevel)
	if !ok {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked",
			"item_id", request.ItemID,
			"level", character.Level,
			"effective_level", effectiveLevel,
			"over_equip_contract", overEquipContract,
			"reason", "no_pvf_tier")
		return nil
	}
	// 这一档声明的强化/锻造必须随发放行一起写下去。少了它，客户端租赁商店预览
	// 显示 +7/+3 而到手是 +0，且租赁武器 PVF 把强化与锻造都列为不可自行操作，
	// 玩家没有任何补救途径。
	tierRow, ok := catalog.tierRow(tier)
	if !ok {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked",
			"item_id", request.ItemID,
			"tier", tier,
			"reason", "no_pvf_tier_row")
		return nil
	}
	attributes := currentRentalItemAttributes{Reinforce: tierRow.Reinforce, Forge: tierRow.Forge}
	expectedPacked := uint32(uint16(jobValue)) | uint32(uint16(tier))<<16
	if request.PackedJobTier != expectedPacked {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked",
			"item_id", request.ItemID,
			"packed_job_tier", request.PackedJobTier,
			"expected_packed_job_tier", expectedPacked,
			"job", jobValue,
			"tier", tier,
			"level", character.Level,
			"effective_level", effectiveLevel,
			"over_equip_contract", overEquipContract,
			"reason", "current_exe_job_tier_key_mismatch")
		return nil
	}
	pointCost, ok := catalog.itemCost(jobTag, tier, request.ItemID)
	if !ok {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "job_tag", jobTag, "tier", tier, "reason", "item_not_in_current_pvf_group")
		return nil
	}
	definition, err := validateCurrentRentalItem(source, items, request.ItemID, jobTag)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "job_tag", jobTag, "tier", tier, "reason", err)
		return nil
	}
	owner, err := currentRentalAssetOwner(repositories)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-equipment-request-blocked", "item_id", request.ItemID, "reason", err)
		return nil
	}
	result, err := rentCurrentEquipment(ctx, owner, s.accountIDForSession(session), characterID, request.ItemID, pointCost, definition, attributes, now)
	if err != nil {
		resultCode := uint32(1)
		if errors.Is(err, errCurrentRentalInventoryFull) {
			resultCode = 2
		}
		s.rentalBlockedLog(session, "game-upper-rental-equipment-transaction-blocked",
			"character_id", characterID,
			"item_id", request.ItemID,
			"point_cost", pointCost,
			"result_code", resultCode,
			"reason", err)
		return s.sendCurrentRentEquipmentResult(session, resultCode)
	}
	if err := s.sendCurrentRentEquipmentResult(session, 0); err != nil {
		return err
	}
	if _, err := s.sendSelectedCurrentRentalPointState(session, "op999_after_atomic_rent", true); err != nil {
		return err
	}
	if err := s.sendSelectedCurrentRentalInventoryItemUpdate(session, repositories.Inventory, characterID, request.ItemID, result); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-rental-equipment-applied",
		"character_id", characterID,
		"item_id", request.ItemID,
		"job", jobValue,
		"job_tag", jobTag,
		"tier", tier,
		"point_cost", pointCost,
		"remaining_points", result.Points,
		"slot", result.Slot,
		"equipped", result.Equipped,
		"expire_unix", result.ExpireAt.Unix(),
		"pvf_path", definition.PVFPath,
		"durability", definition.Durability,
		"reinforce", attributes.Reinforce,
		"forge", attributes.Forge)
	s.logInfo("rental equipment applied",
		"conn_id", rentalSessionConnID(session),
		"character_id", characterID,
		"item_id", request.ItemID,
		"job_tag", jobTag,
		"tier", tier,
		"point_cost", pointCost,
		"remaining_points", result.Points)
	return nil
}

func (s *Service) handleCurrentChargeRentalPoint(session *gameSession, body []byte) error {
	s.logInfo("rental point charge request received", "conn_id", rentalSessionConnID(session), "body_len", len(body))
	request, err := decodeCurrentChargeRentalPointRequest(body)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-point-charge-request-blocked", "body_len", len(body), "reason", err)
		return nil
	}
	s.logInfo("rental point charge request decoded", "conn_id", rentalSessionConnID(session), "count", request.Count, "charge_type", request.ChargeType)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, characterID, _, err := s.currentRentalSelectedCharacter(ctx, session)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-point-charge-request-blocked", "count", request.Count, "reason", err)
		return nil
	}
	catalog, _, _, err := s.currentRentalSources(ctx)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-point-charge-request-blocked", "count", request.Count, "reason", err)
		return nil
	}
	owner, err := currentRentalAssetOwner(repositories)
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-point-charge-request-blocked", "count", request.Count, "reason", err)
		return nil
	}
	result, err := purchaseCurrentRentalPoints(ctx, owner, s.accountIDForSession(session), characterID, request.Count, catalog.Limit, catalog.GoldPerPoint, time.Now().UTC())
	if err != nil {
		s.rentalBlockedLog(session, "game-upper-rental-point-charge-transaction-blocked",
			"character_id", characterID,
			"count", request.Count,
			"point_limit", catalog.Limit,
			"gold_per_point", catalog.GoldPerPoint,
			"reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChargeRentpoint), 1)
	}
	var response packetWriter
	response.writeUint32(request.ChargeType)
	response.writeUint32(0)
	response.writeUint32(result.Points)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketChargeRentpoint), response.bytes()); err != nil {
		return err
	}
	if _, err := s.sendSelectedCurrentRentalPointState(session, "op1000_after_atomic_charge", true); err != nil {
		return err
	}
	// op1000 charges gold (gold_per_point), and the list-0 gold wallet row is the
	// client's only gold display. Without republishing it the balance stays at
	// the pre-purchase value until the next full list load, which is the reported
	// "not refreshed until I relog".
	if err := s.sendSelectedCurrentRentalGoldRefresh(session, "op1000_after_atomic_charge", result.Gold); err != nil {
		return err
	}
	s.logGameEvent(session, "game-upper-rental-point-charge-applied",
		"character_id", characterID,
		"count", request.Count,
		"new_points", result.Points,
		"remaining_gold", result.Gold,
		"gold_per_point", catalog.GoldPerPoint,
		"point_limit", catalog.Limit)
	s.logInfo("rental point charge applied",
		"conn_id", rentalSessionConnID(session),
		"character_id", characterID,
		"count", request.Count,
		"new_points", result.Points,
		"remaining_gold", result.Gold)
	return nil
}

// rentalSessionConnID keeps the plain-log fields nil-safe: the packet log
// tolerates a nil session, and diagnostics should never panic on one.
func rentalSessionConnID(session *gameSession) string {
	if session == nil {
		return ""
	}
	return session.connID
}

// rentalBlockedLog mirrors a blocked rental request into the ordinary server
// log. Every pre-transaction rejection used to reach only the encrypted packet
// event log, so an in-game "confirm does nothing" left no trace in
// dnf90server.log; the plain line names the exact reason instead.
func (s *Service) rentalBlockedLog(session *gameSession, kind string, fields ...any) {
	s.logGameEvent(session, kind, fields...)
	args := make([]any, 0, len(fields)+2)
	args = append(args, "event", kind)
	args = append(args, fields...)
	s.logInfo("rental request blocked", args...)
}
