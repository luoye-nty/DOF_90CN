package dnfbridge

import (
	"context"
	"encoding/binary"
	"math"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentAdventureRepresentativeSetRequestLength = 7

type currentAdventureRepresentativeSetRequest struct {
	GroupID     byte
	CharacterID uint32
	Slot        adventuregroup.RepresentativeSlot
}

func decodeCurrentAdventureRepresentativeSlot(body []byte) (adventuregroup.RepresentativeSlot, bool) {
	if len(body) != 2 || body[1]&currentAdventureRepresentTypeMask != currentAdventureRepresentType {
		return adventuregroup.RepresentativeSlot{}, false
	}
	slot := adventuregroup.RepresentativeSlot{
		Job:      body[0],
		BaseGrow: body[1] & currentAdventureRepresentGrowMask,
	}
	return slot, adventuregroup.ValidRepresentativeSlot(slot)
}

func decodeCurrentAdventureRepresentativeSet(body []byte) (currentAdventureRepresentativeSetRequest, bool) {
	if len(body) != currentAdventureRepresentativeSetRequestLength {
		return currentAdventureRepresentativeSetRequest{}, false
	}
	slot, valid := decodeCurrentAdventureRepresentativeSlot(body[5:7])
	if !valid {
		return currentAdventureRepresentativeSetRequest{}, false
	}
	return currentAdventureRepresentativeSetRequest{
		GroupID:     body[0],
		CharacterID: binary.LittleEndian.Uint32(body[1:5]),
		Slot:        slot,
	}, true
}

func (s *Service) handleCurrentAdventureRepresentativeSet(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketSetRepresentCharacJob)
	request, valid := decodeCurrentAdventureRepresentativeSet(body)
	if !valid || request.CharacterID == 0 {
		return s.sendGameUpperFailure(session, opcode, 3)
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.adventureRepresentativeMu.Lock()
	defer s.adventureRepresentativeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil || state.Repositories.Settings == nil {
		s.logGameEvent(session, "game-adventure-representative-set-rejected",
			"candidate_id", request.CharacterID, "job", request.Slot.Job,
			"base_grow", request.Slot.BaseGrow, "error", err,
			"reason", "state_unavailable")
		return s.sendGameUpperFailure(session, opcode, 3)
	}

	selected, found := currentAdventureRepresentativeCharacter(state.Characters, request.CharacterID)
	if !found || currentAdventureCharacterServerGroup(selected) != request.GroupID ||
		!adventuregroup.RepresentativeCharacterEligible(selected, request.Slot, currentAdventureCharacterLevelCap) {
		s.logGameEvent(session, "game-adventure-representative-set-rejected",
			"candidate_id", request.CharacterID, "candidate_group", request.GroupID,
			"job", request.Slot.Job, "base_grow", request.Slot.BaseGrow,
			"reason", "candidate_not_owned_or_ineligible")
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	classification, found := state.Config.ProfessionClassification(int(request.Slot.Job), int(request.Slot.BaseGrow))
	if !found {
		s.logGameEvent(session, "game-adventure-representative-set-rejected",
			"candidate_id", request.CharacterID, "job", request.Slot.Job,
			"base_grow", request.Slot.BaseGrow,
			"reason", "pvf_profession_classification_missing")
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	count := currentAdventureProfessionCharacterCount(state.Characters, state.Config, classification)
	owner, err := adventuregroup.NewRepresentativeOwner(state.Repositories)
	if err == nil {
		err = owner.Select(ctx, state.AccountID, request.Slot, selected.CharacterID, s.gameplayNow())
	}
	if err != nil {
		s.logGameEvent(session, "game-adventure-representative-set-rejected",
			"candidate_id", request.CharacterID, "job", request.Slot.Job,
			"base_grow", request.Slot.BaseGrow, "error", err,
			"reason", "persistence_failed")
		return s.sendGameUpperFailure(session, opcode, 3)
	}

	s.logGameEvent(session, "game-adventure-representative-set",
		"candidate_id", request.CharacterID, "candidate_group", request.GroupID,
		"job", request.Slot.Job, "base_grow", request.Slot.BaseGrow,
		"profession_classification", classification, "profession_count", count)
	return s.sendGameUpperSuccess(session, opcode, buildCurrentAdventureRepresentativeSetSuccessBody(classification, count))
}

func buildCurrentAdventureRepresentativeSetSuccessBody(classification byte, count uint16) []byte {
	var response packetWriter
	response.writeUint32(3)
	response.writeByte(classification)
	response.writeUint16(count)
	return response.bytes()
}

func (s *Service) handleCurrentAdventureRepresentativeRemove(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketRemoveRepresentCharacJob)
	slot, valid := decodeCurrentAdventureRepresentativeSlot(body)
	if !valid {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.adventureRepresentativeMu.Lock()
	defer s.adventureRepresentativeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err == nil && state.Repositories.Settings != nil {
		var owner *adventuregroup.RepresentativeOwner
		owner, err = adventuregroup.NewRepresentativeOwner(state.Repositories)
		if err == nil {
			err = owner.Remove(ctx, state.AccountID, slot, s.gameplayNow())
		}
	}
	if err != nil || state.Repositories.Settings == nil {
		s.logGameEvent(session, "game-adventure-representative-remove-rejected",
			"job", slot.Job, "base_grow", slot.BaseGrow, "error", err,
			"reason", "persistence_failed")
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	s.logGameEvent(session, "game-adventure-representative-removed",
		"job", slot.Job, "base_grow", slot.BaseGrow)
	return s.sendGameUpperSuccess(session, opcode, nil)
}

func currentAdventureRepresentativeCharacter(
	characters []dnfrepo.CharacterRecord,
	characterID uint32,
) (dnfrepo.CharacterRecord, bool) {
	for _, character := range characters {
		value := numericCharacterID(character)
		if value > 0 && uint64(value) <= math.MaxUint32 && uint32(value) == characterID {
			return character, true
		}
	}
	return dnfrepo.CharacterRecord{}, false
}

func currentAdventureProfessionCharacterCount(
	characters []dnfrepo.CharacterRecord,
	config adventuregroup.RuntimeConfig,
	classification byte,
) uint16 {
	count := 0
	for _, character := range characters {
		job := numericCharacterStat(character.Job)
		packed := byte(numericCharacterStatValue(character, "grow_type"))
		candidate, found := config.ProfessionClassification(job, int(packed&0x0f))
		if found && candidate == classification {
			count++
		}
	}
	if count > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(count)
}
