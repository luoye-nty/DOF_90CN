package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentPremiumServiceBlobSize = 74

func premiumGameplayModule() gameplayModuleDefinition {
	serviceOpcode := uint16(dnfenum.CmdPacketPremiumService)
	recoverOpcode := uint16(dnfenum.CmdPacketRecoverStamina)
	crystalUpdateOpcode := uint16(dnfenum.CmdPacketUpdateContractOfCubeInfo)
	crystalConsumeOpcode := uint16(dnfenum.CmdPacketUseLimitCube)
	serviceHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentPremiumService(session, request.Body)
	}
	crystalUpdateHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentCrystalContractUpdate(session, request.Body)
	}
	crystalConsumeHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentLimitedCubeUseRequest(session, request.Body)
	}
	recoverHandler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentPremiumFreeWeakness(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "premium-service",
		LegacyHandlers: map[uint16]gameplayHandler{
			serviceOpcode: serviceHandler,
			// The 解除虚弱 NPC button sends op9 on the legacy path
			// (cmd=1 type=9). Registering it only under UpperHandlers meant
			// the request was received, logged and then dropped with no
			// dispatch at all -- the NPC simply did nothing, and no
			// weakness-recovery event was ever written.
			recoverOpcode:        recoverHandler,
			crystalUpdateOpcode:  crystalUpdateHandler,
			crystalConsumeOpcode: crystalConsumeHandler,
		},
		UpperHandlers: map[uint16]gameplayHandler{
			serviceOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(
					service,
					session,
					request,
					"game-premium-service-request-blocked",
					"current_exe_op903_command_class_mismatch",
				) {
					return nil
				}
				return serviceHandler(service, session, request)
			},
			recoverOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(
					service,
					session,
					request,
					"game-premium-free-weakness-blocked",
					"current_exe_op9_command_class_mismatch",
				) {
					return nil
				}
				return recoverHandler(service, session, request)
			},
			crystalUpdateOpcode: defaultClassGameplayHandler(
				"game-crystal-contract-selection-request-blocked",
				"current_exe_op535_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return crystalUpdateHandler(service, session, gameplayRequest{Body: body})
				},
			),
			crystalConsumeOpcode: defaultClassGameplayHandler(
				"game-limit-cube-use-request-blocked",
				"current_exe_op338_command_class_mismatch",
				func(service *Service, session *gameSession, body []byte) error {
					return crystalConsumeHandler(service, session, gameplayRequest{Body: body})
				},
			),
		},
	}
}

func (s *Service) handleCurrentPremiumService(session *gameSession, body []byte) error {
	// Current EXE action 1 carries a 74-byte blob. sub_206A240 sends slot 3
	// when the seven-buff service is actually applied; account for that daily
	// use before returning the complete authoritative state.
	if len(body) >= 2+currentPremiumServiceBlobSize && binary.LittleEndian.Uint16(body[:2]) == 1 {
		slot := int64(binary.LittleEndian.Uint16(body[2:4]))
		if slot == premium.DevilSlotSevenBuff {
			if err := s.consumeCurrentPremiumServiceDaily(session, slot, s.gameplayNow()); err != nil {
				s.logGameEvent(session, "game-premium-service-use-blocked",
					"slot", slot,
					"reason", err)
			}
		}
	}
	return s.sendCurrentPremiumServiceState(session, "op903_request", currentPremiumServiceTypeForSession(session))
}

// handleCurrentPremiumFreeWeakness answers op9, the 解除虚弱状态 NPC action
// (etc/popupmenu.etc entry 48).
//
// The client's u16 says which button was pressed - NoPack sub_30C6B10 sends 1
// for the 魔王契约 route and 0 for the paid one - but it is a request, not a
// verdict. The server decides whether the contract is active, whether its daily
// quota remains, and whether the gold is there; the previous implementation
// spent the quota purely because the client had asked, and cleared nothing.
//
// The answer shape is the current EXE's own: sub_1D11820 reads [ebp+0xC] as the
// status byte and, on success, calls the condition setter (vtable+0xAD4) with
// 100 - the only write of that value anywhere in the binary. The failure branch
// sub_1D11900 reads [ebp+0x10] as a reason code and decodes 0x12 (no weakness)
// and 0x16 (cannot afford) into their own messages.
func (s *Service) handleCurrentPremiumFreeWeakness(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	useContract := len(body) >= 2 && binary.LittleEndian.Uint16(body[:2]) == 1
	// The gameplay clock, not the wall clock: the daily quota, the contract
	// expiry and the weakness write must all agree on "now".
	now := s.gameplayNow()

	character, err := s.currentWeaknessCharacter(session)
	if err != nil {
		s.logGameEvent(session, "game-weakness-recovery-blocked", "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRecoverStamina), currentWeaknessReasonNotWeakened)
	}
	level := currentWeaknessLevel(character)
	if level == 0 {
		s.logGameEvent(session, "game-weakness-recovery-refused",
			"char_id", session.selectedCharacterID,
			"requested_contract", useContract,
			"reason_code", currentWeaknessReasonNotWeakened,
			"reason", "character_is_not_weakened")
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRecoverStamina), currentWeaknessReasonNotWeakened)
	}

	if useContract {
		// Server-side verdict: active contract plus remaining daily quota, and
		// the quota is spent only when the clear actually happens below.
		if err := s.consumeCurrentPremiumServiceDaily(session, premium.DevilSlotFreeWeakness, now); err != nil {
			s.logGameEvent(session, "game-weakness-recovery-refused",
				"char_id", session.selectedCharacterID,
				"slot", premium.DevilSlotFreeWeakness,
				"reason_code", currentWeaknessReasonCannotAfford,
				"reason", err)
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRecoverStamina), currentWeaknessReasonCannotAfford)
		}
		if _, _, _, err := s.clearCurrentWeakness(session, 0); err != nil {
			s.logGameEvent(session, "game-weakness-recovery-blocked", "reason", err)
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRecoverStamina), currentWeaknessReasonCannotAfford)
		}
		s.logGameEvent(session, "game-weakness-recovery-committed",
			"char_id", session.selectedCharacterID,
			"route", "devil_contract_slot5",
			"exemption", string(currentWeaknessExemptionDevilSlot),
			"cleared_level", level,
			"gold_cost", 0)
		if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRecoverStamina), nil); err != nil {
			return err
		}
		// The success ACK clears the weakness UI but says nothing about the
		// contract; without this the panel keeps showing the pre-use count.
		return s.sendCurrentPremiumServiceState(session, "devil_contract_free_weakness_after_op9", currentPremiumServiceTypeForSession(session))
	}

	cost := int64(0)
	if table, tableErr := s.currentWeaknessCostTable(); tableErr == nil {
		cost = table.recoveryCost(level, int64(character.Level))
	} else {
		s.logGameEvent(session, "game-weakness-recovery-cost-unavailable", "reason", tableErr)
	}
	clearedLevel, remainingGold, goldSource, err := s.clearCurrentWeakness(session, cost)
	if err != nil {
		reason := currentWeaknessReasonCannotAfford
		if errors.Is(err, errCurrentWeaknessNotWeakened) {
			reason = currentWeaknessReasonNotWeakened
		}
		s.logGameEvent(session, "game-weakness-recovery-refused",
			"char_id", session.selectedCharacterID,
			"gold_cost", cost,
			"reason_code", reason,
			"reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRecoverStamina), reason)
	}
	s.logGameEvent(session, "game-weakness-recovery-committed",
		"char_id", session.selectedCharacterID,
		"route", "gold",
		"cleared_level", clearedLevel,
		"gold_cost", cost,
		"gold_after", remainingGold,
		"gold_source", string(goldSource))
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRecoverStamina), nil); err != nil {
		return err
	}
	if cost > 0 {
		// The wallet is main-list slot 0, but on this server it is projected
		// from the character's gold column, so an incremental refresh of that
		// slot resolves nothing. Push the wallet the way every other gold
		// spender does. Only a payment that really came out of the legacy
		// inventory row needs that row re-sent instead.
		if goldSource == currentWeaknessGoldSourceLegacySlot {
			return s.sendSelectedIncrementalItemSlotRefreshes(session, "weakness_recovery_gold", []alignedcmd.ItemSlotRefresh{{
				ListType:  dnfrepo.MainInventoryListType,
				SlotIndex: 0,
			}})
		}
		return s.sendSelectedMainWalletsRefresh(session, "weakness_recovery_gold")
	}
	return nil
}

func (s *Service) currentWeaknessCharacter(session *gameSession) (dnfrepo.CharacterRecord, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, errCurrentWeaknessUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	if !found {
		return dnfrepo.CharacterRecord{}, errCurrentWeaknessUnavailable
	}
	return character, nil
}

func (s *Service) currentPremiumServiceRecords(
	ctx context.Context,
	session *gameSession,
) (dnfrepo.Group, dnfrepo.AccountRecord, dnfrepo.CharacterRecord, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("premium repositories are unavailable")
	}
	if session == nil || session.selectedCharacterID == 0 {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("premium selected character is missing")
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("load premium account: %w", err)
	}
	if !found {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("premium account %q is missing", accountID)
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("load premium character: %w", err)
	}
	if !found || strings.TrimSpace(character.AccountID) != accountID {
		return dnfrepo.Group{}, dnfrepo.AccountRecord{}, dnfrepo.CharacterRecord{}, fmt.Errorf("premium character ownership mismatch")
	}
	return repositories, account, character, nil
}

func (s *Service) consumeCurrentPremiumServiceDaily(session *gameSession, slot int64, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.CharacterAssets == nil {
		return fmt.Errorf("premium account/character transaction is unavailable")
	}
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("premium selected character is missing")
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load premium account: %w", err)
	}
	if !found {
		return fmt.Errorf("premium account %q is missing", accountID)
	}
	if !premium.Active(account, premium.DevilSlotType(slot), now) {
		return fmt.Errorf("premium slot %d is inactive", slot)
	}
	return repositories.CharacterAssets.WithinCharacterAssets(
		ctx,
		characterID,
		func(
			characters dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			character, characterFound, loadErr := characters.Load(ctx, characterID)
			if loadErr != nil {
				return fmt.Errorf("load premium character: %w", loadErr)
			}
			if !characterFound || strings.TrimSpace(character.AccountID) != accountID {
				return fmt.Errorf("premium character ownership mismatch")
			}
			if !premium.TryConsumeDaily(&character, slot, now) {
				return fmt.Errorf("premium slot %d daily limit %d reached", slot, premium.DailyLimit(slot))
			}
			return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
		},
	)
}

// currentPremiumServiceTypeForSession picks the op903 serviceType for a send that
// is NOT the login/entry scene tail.
//
// serviceType 1 makes the client apply the body to the contract singleton its UI
// reads and, while its own one-shot "show the contract panel" flag is still
// armed, open that panel (handler 0x01CFE1D0 -> 0x01CFE2AA). The flag is armed by
// the client's class1/op4 SELECT_CHARACTER handler and cleared by the first type-1
// body, so the intended flow is exactly one type-1 send per login: the scene
// tail's. A session whose tail never ran must not answer with 1, or the panel
// pops wherever the first such answer lands -- live 2026-09-16T14:23:54Z on conn
// game-000209 the tail stayed deferred ("wait_for_current_exe_legacy_type1345_u32_2_scene_ready")
// and an in-dungeon op903 request popped the panel. After the tail consumed the
// flag, 1 is safe again and keeps the contract UI data fresh.
func currentPremiumServiceTypeForSession(session *gameSession) uint16 {
	if session != nil && session.premiumPanelFlagConsumed.Load() {
		return premium.ServiceTypePanel
	}
	return premium.ServiceTypeSilent
}

func (s *Service) sendCurrentPremiumServiceState(session *gameSession, source string, serviceType uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil {
		return fmt.Errorf("premium repositories are unavailable")
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load premium account state: %w", err)
	}
	if !found {
		account = dnfrepo.AccountRecord{}
	}
	character := dnfrepo.CharacterRecord{}
	if session != nil && session.selectedCharacterID != 0 {
		characterID := strconv.Itoa(int(session.selectedCharacterID))
		loaded, characterFound, loadErr := repositories.Character.Load(ctx, characterID)
		if loadErr != nil {
			return fmt.Errorf("load premium character state: %w", loadErr)
		}
		if characterFound {
			if strings.TrimSpace(loaded.AccountID) != accountID {
				return fmt.Errorf("premium character ownership mismatch")
			}
			character = loaded
		}
	}
	// The gameplay clock, not the wall clock: the service-day bucket this body
	// reports usage against has to be the same one consumeCurrentPremiumServiceDaily
	// wrote into, or every fresh use reads back as unused.
	now := s.gameplayNow()
	body := buildCurrentPremiumServiceDataBody(account, character, now, serviceType)
	s.logGameEvent(session, "game-premium-service-state-send",
		"source", source,
		"msg_id", uint16(dnfenum.CmdPacketPremiumService),
		"classification", dnfproto.DefaultChannelClassification,
		"body_len", len(body),
		"service_type", serviceType,
		"slot_count", premium.DevilSlotCount,
		"state_source", "account_expiry_and_character_service_day_usage")
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketPremiumService),
		body,
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	if serviceType == premium.ServiceTypePanel && session != nil {
		// The client clears its one-shot panel flag on this body, so every later
		// type-1 send can no longer open the contract panel.
		session.premiumPanelFlagConsumed.Store(true)
	}
	return nil
}
