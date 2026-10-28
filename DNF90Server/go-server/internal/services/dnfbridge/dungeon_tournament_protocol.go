package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentTournamentInfoNotification        uint16 = 386
	currentTournamentMapInfoNotification     uint16 = 387
	currentTournamentClearRewardNotification uint16 = 388
	currentTournamentRewardSelectState       uint16 = 457
	currentTournamentRewardSelect            uint16 = 458
	currentTournamentEmptyRewardSentinel     uint32 = ^uint32(0)
	currentTournamentUnselectedPartySlot     byte   = 0xff
	currentTournamentSoloPartySlot           byte   = 0
)

var currentTournamentRewardSelectOpenBody = []byte{
	1,
	1, currentTournamentUnselectedPartySlot, currentTournamentUnselectedPartySlot, currentTournamentUnselectedPartySlot,
	1, currentTournamentUnselectedPartySlot, currentTournamentUnselectedPartySlot, currentTournamentUnselectedPartySlot,
}

var (
	errTournamentProtocolRuntime       = errors.New("dnf tournament protocol runtime is required")
	errTournamentProtocolRoundCount    = errors.New("dnf tournament protocol requires four rounds")
	errTournamentProtocolTeamCount     = errors.New("dnf tournament protocol team count exceeds current EXE range")
	errTournamentProtocolActorCount    = errors.New("dnf tournament protocol actor count exceeds current EXE range")
	errTournamentProtocolMapOperation  = errors.New("dnf tournament map-info operation is invalid")
	errTournamentProtocolRewardCount   = errors.New("dnf tournament reward count exceeds current EXE range")
	errTournamentProtocolRewardRequest = errors.New("dnf tournament reward request is invalid")
)

func buildCurrentTournamentInfoBody(runtime *worldmap.TournamentRuntime, difficulty byte) ([]byte, error) {
	if runtime == nil {
		return nil, errTournamentProtocolRuntime
	}
	if len(runtime.Rounds) != 4 {
		return nil, fmt.Errorf("%w: count=%d", errTournamentProtocolRoundCount, len(runtime.Rounds))
	}
	partyCount := int(runtime.Definition.PartyCount)
	if partyCount < 1 || partyCount > 0xff || len(runtime.PathActors) != len(runtime.Rounds)*partyCount {
		return nil, fmt.Errorf("%w: party=%d path_actors=%d", errTournamentProtocolActorCount, partyCount, len(runtime.PathActors))
	}
	firstSequence := runtime.FirstActorSequence()
	if firstSequence == 0 {
		return nil, worldmap.ErrTournamentSequenceUnbound
	}
	if int(firstSequence)+len(runtime.PathActors)-1 > int(^uint16(0)) {
		return nil, fmt.Errorf("%w: first=%d count=%d", errTournamentProtocolActorCount, firstSequence, len(runtime.PathActors))
	}
	var writer packetWriter
	writer.writeUint32(runtime.Definition.DungeonID)
	writer.writeByte(difficulty)
	writer.writeByte(byte(partyCount))
	for _, round := range runtime.Rounds {
		if len(round.Teams) > 0xff {
			return nil, fmt.Errorf("%w: round=%d count=%d", errTournamentProtocolTeamCount, round.Number, len(round.Teams))
		}
		writer.writeByte(round.Number)
		writer.writeByte(byte(len(round.Teams)))
		for _, team := range round.Teams {
			if len(team.Members) != partyCount {
				return nil, fmt.Errorf("%w: round=%d team=%d members=%d party=%d", errTournamentProtocolActorCount, round.Number, team.Position, len(team.Members), partyCount)
			}
			writer.writeByte(team.Position)
			for _, actor := range team.Members {
				writer.writeUint32(actor.Code)
				writer.writeUint16(actor.Strength)
			}
		}
	}
	actorIndex := 0
	for _, round := range runtime.Rounds {
		writer.writeByte(round.Number)
		for member := 0; member < partyCount; member++ {
			actor := runtime.PathActors[actorIndex]
			writer.writeUint16(firstSequence + uint16(actorIndex))
			writer.writeUint32(actor.Code)
			writer.writeByte(actor.Level)
			writer.writeByte(actor.Type)
			actorIndex++
		}
	}
	return writer.bytes(), nil
}

type currentTournamentMapActor struct {
	Sequence uint16
	Code     uint32
	FieldA   uint16
	FieldB   uint16
	FieldC   byte
	FieldD   byte
	FieldE   byte
	FieldF   byte
}

type currentTournamentMapExtra struct {
	Type   byte
	ValueA uint32
	ValueB uint32
	ValueC uint32
	ValueD uint16
	ValueE byte
	ValueF uint16
	ValueG uint16
}

type currentTournamentMapInfo struct {
	RoomX     byte
	RoomY     byte
	Seed      uint32
	RoomState byte
	Operation byte
	MapID     uint32
	Actors    []currentTournamentMapActor
	Extras    []currentTournamentMapExtra
	FinalFlag byte
}

func (packet currentTournamentMapInfo) Build() ([]byte, error) {
	if packet.Operation > 2 {
		return nil, fmt.Errorf("%w: operation=%d", errTournamentProtocolMapOperation, packet.Operation)
	}
	if packet.Operation == 0 && (packet.MapID != 0 || len(packet.Actors) != 0 || len(packet.Extras) != 0 || packet.FinalFlag != 0) {
		return nil, fmt.Errorf("%w: operation=0 has payload", errTournamentProtocolMapOperation)
	}
	if packet.Operation != 0 && packet.MapID == 0 {
		return nil, fmt.Errorf("%w: operation=%d map=0", errTournamentProtocolMapOperation, packet.Operation)
	}
	if len(packet.Actors) > 0xff || len(packet.Extras) > 0xff {
		return nil, fmt.Errorf("%w: actors=%d extras=%d", errTournamentProtocolActorCount, len(packet.Actors), len(packet.Extras))
	}
	var writer packetWriter
	writer.writeByte(packet.RoomX)
	writer.writeByte(packet.RoomY)
	writer.writeUint32(packet.Seed)
	writer.writeByte(packet.RoomState)
	writer.writeByte(packet.Operation)
	if packet.Operation == 0 {
		return writer.bytes(), nil
	}
	writer.writeUint32(packet.MapID)
	writer.writeByte(byte(len(packet.Actors)))
	for _, actor := range packet.Actors {
		writer.writeUint16(actor.Sequence)
		writer.writeUint32(actor.Code)
		writer.writeUint16(actor.FieldA)
		writer.writeUint16(actor.FieldB)
		writer.writeByte(actor.FieldC)
		writer.writeByte(actor.FieldD)
		writer.writeByte(actor.FieldE)
		writer.writeByte(actor.FieldF)
	}
	writer.writeByte(byte(len(packet.Extras)))
	for _, extra := range packet.Extras {
		writer.writeByte(extra.Type)
		writer.writeUint32(extra.ValueA)
		writer.writeUint32(extra.ValueB)
		writer.writeUint32(extra.ValueC)
		writer.writeUint16(extra.ValueD)
		writer.writeByte(extra.ValueE)
		writer.writeUint16(extra.ValueF)
		writer.writeUint16(extra.ValueG)
	}
	writer.writeByte(packet.FinalFlag)
	return writer.bytes(), nil
}

type currentTournamentClearRewardExtended struct {
	Prefix  uint16
	ValueA  uint32
	ValueB  uint32
	ValueC  uint16
	SuffixA byte
	SuffixB byte
}

type currentTournamentClearRewardEntry struct {
	ItemOrSentinel uint32
	CountOrGold    uint32
	Reserved       uint16
}

type currentTournamentClearReward struct {
	CompletedRounds byte
	Experience      uint32
	Extended        []currentTournamentClearRewardExtended
	GroupA          []currentTournamentClearRewardEntry
	GroupB          []currentTournamentClearRewardEntry
}

func (packet currentTournamentClearReward) Build() ([]byte, error) {
	if len(packet.Extended) > 0xff || len(packet.GroupA) > 0xff || len(packet.GroupB) > 0xff {
		return nil, fmt.Errorf("%w: extended=%d group_a=%d group_b=%d", errTournamentProtocolRewardCount, len(packet.Extended), len(packet.GroupA), len(packet.GroupB))
	}
	var writer packetWriter
	writer.writeByte(packet.CompletedRounds)
	writer.writeUint32(packet.Experience)
	writer.writeByte(byte(len(packet.Extended)))
	for _, row := range packet.Extended {
		writer.writeUint16(row.Prefix)
		writer.writeUint32(row.ValueA)
		writer.writeUint32(row.ValueB)
		writer.writeUint16(row.ValueC)
		writer.writeByte(row.SuffixA)
		writer.writeByte(row.SuffixB)
	}
	for _, group := range [][]currentTournamentClearRewardEntry{packet.GroupA, packet.GroupB} {
		writer.writeByte(byte(len(group)))
		for _, row := range group {
			writer.writeUint32(row.ItemOrSentinel)
			writer.writeUint32(row.CountOrGold)
			writer.writeUint16(row.Reserved)
		}
	}
	return writer.bytes(), nil
}

type currentTournamentRewardSelectRequest struct {
	Side      byte
	CardIndex byte
}

func decodeCurrentTournamentRewardSelectRequest(body []byte) (currentTournamentRewardSelectRequest, error) {
	if len(body) != 2 {
		return currentTournamentRewardSelectRequest{}, fmt.Errorf("%w: body_len=%d want=2", errTournamentProtocolRewardRequest, len(body))
	}
	return currentTournamentRewardSelectRequest{Side: body[0], CardIndex: body[1]}, nil
}

// currentTournamentRewardSelectionState is the class-1 op458 response. The
// current EXE consumes the leading success byte before its handler, then reads
// one count and one selected-party byte per card for each of the two groups.
// Counts must therefore be derived from the same frozen plan used by op388.
type currentTournamentRewardSelectionState struct {
	Plan worldmap.TournamentRewardPlan
}

func (state currentTournamentRewardSelectionState) build() ([]byte, error) {
	var writer packetWriter
	writer.writeByte(1)
	for side := byte(0); side < 2; side++ {
		count := state.Plan.CardCount(side)
		writer.writeByte(count)
		selected, selectedOK := state.Plan.SelectedCard(side)
		for index := byte(0); index < count; index++ {
			partySlot := currentTournamentUnselectedPartySlot
			if selectedOK && selected == index {
				partySlot = currentTournamentSoloPartySlot
			}
			writer.writeByte(partySlot)
		}
	}
	return writer.bytes(), nil
}
