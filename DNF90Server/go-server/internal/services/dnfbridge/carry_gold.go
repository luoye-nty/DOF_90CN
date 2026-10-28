package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"

	dnfcarrygold "longheng.io/server/internal/modules/dnf/carrygold"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The gold-carry cap upgrade. The wire evidence lives next to the rule, in
// the package comment of internal/modules/dnf/carrygold; this file owns only
// the current EXE's request/response layout and the packet ordering.
const (
	// The client builder copies 14 bytes off its own stack and initializes
	// only the trailing byte, so bytes 0..12 are stack residue and ignored.
	currentCarryGoldRequestSize = 14
	currentCarryGoldStageOffset = currentCarryGoldRequestSize - 1

	// Login projection: class0/NOTI 947, a single stage byte, read by
	// sub_1D87BF0 into the same 0x052B5C04 global the op978 reply writes.
	// Without it the client shows stage 0 again after a relog and the player
	// could buy the same upgrade twice.
	currentCarryGoldStageMsgID = uint16(947)

	currentCarryGoldResponseSize = 5
	currentCarryGoldPrivateABI   = "current_exe_sub_1D23D60_op978_and_sub_1D87BF0_noti947"
)

// Result codes for the op978 reply. Zero takes the client's success branch;
// every other value takes its failure branch, so the button always resolves.
const (
	currentCarryGoldResultOK uint32 = iota
	currentCarryGoldResultInvalidStage
	currentCarryGoldResultInsufficientGold
	currentCarryGoldResultUnavailable
)

func buildCurrentCarryGoldUpgradeBody(stage byte, result uint32) []byte {
	body := make([]byte, currentCarryGoldResponseSize)
	body[0] = stage
	binary.LittleEndian.PutUint32(body[1:5], result)
	return body
}

func (s *Service) sendCurrentCarryGoldUpgradeResult(
	session *gameSession,
	stage byte,
	result uint32,
	source string,
) error {
	s.logGameEvent(session, "game-carry-gold-upgrade-result-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketUpgradeCarryGold),
		"classification", dnfproto.DefaultChannelClassification,
		"stage", stage,
		"displayed_limit", dnfcarrygold.DisplayedLimit(stage),
		"result", result,
		"body_source", currentCarryGoldPrivateABI)
	return s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketUpgradeCarryGold),
		buildCurrentCarryGoldUpgradeBody(stage, result),
	)
}

// sendCurrentCarryGoldStage republishes the durable stage so the client's cap
// display survives a relog.
func (s *Service) sendCurrentCarryGoldStage(session *gameSession, source string) error {
	if s == nil || session == nil || session.conn == nil || session.selectedCharacterID == 0 {
		return nil
	}
	owner, ok := s.currentCarryGoldOwner()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	state, err := owner.Load(ctx, session.selectedCharacterID)
	if err != nil {
		if errors.Is(err, dnfcarrygold.ErrCharacterNotFound) {
			return nil
		}
		return err
	}
	s.logGameEvent(session, "game-carry-gold-stage-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", currentCarryGoldStageMsgID,
		"classification", 0,
		"stage", state.Stage,
		"displayed_limit", dnfcarrygold.DisplayedLimit(state.Stage),
		"gold_limit", state.Limit,
		"body_source", currentCarryGoldPrivateABI)
	return s.sendCurrentSceneFixedClass0Packet(
		session,
		currentCarryGoldStageMsgID,
		[]byte{state.Stage},
		source,
	)
}

func (s *Service) handleCurrentUpgradeCarryGold(session *gameSession, body []byte) error {
	if session == nil || session.conn == nil {
		return nil
	}
	if session.selectedCharacterID == 0 {
		s.logCarryGoldUpgradeBlocked(session, 0, "selected_character_missing")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, 0, currentCarryGoldResultUnavailable, "selected_character_missing")
	}
	if len(body) != currentCarryGoldRequestSize {
		// Only the trailing byte is initialized by the client builder, so a
		// different length means a different builder and the request is not
		// understood. Answer anyway: a silent drop is what left the confirm
		// button dead in the first place.
		s.logCarryGoldUpgradeBlocked(session, 0, "current_exe_op978_request_length_mismatch")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, 0, currentCarryGoldResultInvalidStage, "request_length_mismatch")
	}
	requested := body[currentCarryGoldStageOffset]

	owner, ok := s.currentCarryGoldOwner()
	if !ok {
		s.logCarryGoldUpgradeBlocked(session, requested, "character_repository_unavailable")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, 0, currentCarryGoldResultUnavailable, "repository_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := owner.Upgrade(ctx, dnfcarrygold.Command{
		AccountID:           s.accountIDForSession(session),
		SelectedCharacterID: session.selectedCharacterID,
		RequestedStage:      requested,
	})
	switch {
	case err == nil:
	case errors.Is(err, dnfcarrygold.ErrStageOutOfSequence),
		errors.Is(err, dnfcarrygold.ErrStageExhausted):
		s.logCarryGoldUpgradeBlocked(session, requested, "requested_stage_is_not_the_next_stage")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, result.Stage, currentCarryGoldResultInvalidStage, "stage_out_of_sequence")
	case errors.Is(err, dnfcarrygold.ErrGoldInsufficient):
		s.logGameEvent(session, "game-carry-gold-upgrade-blocked",
			"char_id", session.selectedCharacterID,
			"stage", result.Stage,
			"requested_stage", requested,
			"gold", result.GoldBefore,
			"cost", result.Cost,
			"reason", "insufficient_gold")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, result.Stage, currentCarryGoldResultInsufficientGold, "insufficient_gold")
	case errors.Is(err, dnfcarrygold.ErrCharacterNotFound),
		errors.Is(err, dnfcarrygold.ErrAccountMismatch),
		errors.Is(err, dnfcarrygold.ErrCharacterRequired),
		errors.Is(err, dnfcarrygold.ErrOwnerUnavailable):
		s.logCarryGoldUpgradeBlocked(session, requested, "selected_character_unavailable")
		return s.sendCurrentCarryGoldUpgradeResult(
			session, 0, currentCarryGoldResultUnavailable, "character_unavailable")
	default:
		return err
	}

	s.logGameEvent(session, "game-carry-gold-upgrade-committed",
		"char_id", session.selectedCharacterID,
		"previous_stage", result.PreviousStage,
		"stage", result.Stage,
		"displayed_limit", dnfcarrygold.DisplayedLimit(result.Stage),
		"gold_limit", result.Limit,
		"cost", result.Cost,
		"gold_before", result.GoldBefore,
		"gold_after", result.GoldAfter)
	if err := s.sendCurrentCarryGoldUpgradeResult(
		session, result.Stage, currentCarryGoldResultOK, "upgrade_committed",
	); err != nil {
		return err
	}
	// The wallet row is the client's only gold display, so the charge has to be
	// republished with the same incremental update the shops use.
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		buildCurrentItemUpdateBody(
			dnfrepo.MainInventoryListType,
			[]currentItemListEntry{currentGoldWalletItemListEntry(result.GoldAfter)},
		),
		0,
	)
}

func (s *Service) currentCarryGoldOwner() (*dnfcarrygold.Owner, bool) {
	if s == nil {
		return nil, false
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, false
	}
	owner, err := dnfcarrygold.NewOwner(repositories)
	if err != nil {
		return nil, false
	}
	return owner, true
}

func (s *Service) logCarryGoldUpgradeBlocked(session *gameSession, requested byte, reason string) {
	s.logGameEvent(session, "game-carry-gold-upgrade-blocked",
		"char_id", session.selectedCharacterID,
		"requested_stage", requested,
		"reason", reason)
}
