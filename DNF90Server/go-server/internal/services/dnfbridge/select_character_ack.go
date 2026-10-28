package dnfbridge

import (
	"encoding/binary"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSelectAckResultOffset       = 0
	currentSelectAckCreatedTimeOffset  = 5
	currentSelectAckCharacterIDOffset  = 9
	currentSelectAckFatigueUsedOffset  = 11
	currentSelectAckFatigueLimitOffset = 13
	currentSelectAckFatigueAuxOffset   = 15
	currentSelectAckPremiumCountOffset = 17

	currentSelectAckFixedQuestCount = 30
	currentSelectAckQuestRowSize    = 6
	currentSelectAckStateU32Count   = 4
	currentSelectAckStateByteSize   = currentSelectAckStateU32Count * 4
	currentSelectAckMinimumBodyLen  = 233
	currentSelectAckPage1RouteIndex = 31

	// The generated enum still carries the old SET_USER_AREA name. The current
	// class-0/op36 handler is the five-u16 fatigue-state reader at sub_1D7ABE0.
	currentFatigueMsgID = uint16(dnfenum.CmdPacketSetUserArea)
)

type currentSelectAckQuestRow struct {
	questID      uint16
	triggerValue uint32
}

type currentFatigueState struct {
	used        uint16
	limit       uint16
	actorAux    uint16
	battery     uint16
	batteryBuff uint16
	actorExtra  uint16
}

func buildCurrentSelectCharacterAckBody(character dnfrepo.CharacterRecord, hasCharacter bool, quests dnfrepo.QuestRecord, hasQuests bool, characterID uint16, townID uint8, accountCera uint32, premiumEntries []byte) []byte {
	fatigue := currentCharacterFatigueState(character, hasCharacter)
	rows := currentSelectAckQuestRows(quests, hasQuests)
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint32(0) // Account registration time is not consumed by the current handler.
	writer.writeUint32(currentSelectAckCreatedTime(character, hasCharacter))
	writer.writeUint16(characterID)
	writer.writeUint16(fatigue.used)
	writer.writeUint16(fatigue.limit)
	writer.writeUint16(fatigue.actorAux)

	// Premium entries are type:u8 + remaining seconds:i64LE from the
	// account-owned premium contracts in account metadata. Devil slots are
	// folded to type 58 with their longest remaining duration. Cera is the
	// account-shared pool from account metadata, identical for every character
	// on the account.
	writer.writeBytes(premiumEntries)
	writer.writeUint32(accountCera)

	fixedCount := len(rows)
	if fixedCount > currentSelectAckFixedQuestCount {
		fixedCount = currentSelectAckFixedQuestCount
	}
	for idx := 0; idx < currentSelectAckFixedQuestCount; idx++ {
		if idx < fixedCount {
			writer.writeUint16(rows[idx].questID)
			writer.writeUint32(rows[idx].triggerValue)
			continue
		}
		writer.writeUint16(0xffff)
		writer.writeUint32(0)
	}

	overflowCount := len(rows) - fixedCount
	writer.writeUint32(uint32(overflowCount))
	for idx := fixedCount; idx < len(rows); idx++ {
		writer.writeUint16(rows[idx].questID)
		writer.writeUint32(rows[idx].triggerValue)
	}

	// Current sub_1A0C3E0 always consumes four u32 values after the overflow
	// quest rows and before the town ID. Fresh characters have no
	// persisted owner for these states, so their real initialized values are 0.
	for idx := 0; idx < currentSelectAckStateU32Count; idx++ {
		writer.writeUint32(0)
	}

	// sub_1D01C50 resolves this byte through dword_51B26D8 (town catalog 12),
	// then initializes that town's gate before mode0 creates the actor.
	//
	// It must be the town the initial-town transition sends afterwards. The
	// client builds the gate scene for this value first, so a mismatch makes it
	// load one town here and load the real one again on the transition.
	writer.writeByte(townID)

	// Current sub_33C28A0 consumes this first byte but never uses it. Tutorial
	// state is controlled by the following index list. A maximum index of 31 is
	// the current EXE's explicit page-1 route: it reaches sub_1CF2A70(1) and
	// sub_226D5F0 without synchronously entering the >38 sub_33C04C0 branch.
	// The later sub_33C6430 state dispatcher also maps state 31 to
	// sub_33C04C0, while state 0 maps to the observed null-actor crash owner
	// sub_33C5AB0.
	// Project only the durable server decision here; pending and unresolved
	// characters keep the empty list and follow the practice-dungeon route.
	writer.writeByte(0)
	if hasCharacter && !selectedCharacterStartsInTutorial(character, true) {
		writer.writeByte(1)
		writer.writeByte(currentSelectAckPage1RouteIndex)
	} else {
		writer.writeByte(0)
	}
	writer.writeUint16(fatigue.battery)
	writer.writeUint16(fatigue.batteryBuff)
	writer.writeByte(statU8(character, "trade_punish_flag", 0))
	writer.writeUint16(fatigue.actorExtra)
	writer.writeByte(0) // No typed source for the current secondary u16+u32 list yet.
	return writer.bytes()
}

// currentSelectAckTownID is the town the select ACK tells the client to build
// its gate scene for. The initial-town route must send the same town in its
// scene transition, otherwise the client loads twice.
//
// Temporary channel scenes (PvP, team raids) resolve through their own runtime
// PVF gate so the character's durable town stays untouched. A channel reconnect
// that kept the same character resumes where it was; one where the client
// switched characters is a fresh login and falls through to the ordinary town.
func currentSelectAckTownID(session *gameSession, character dnfrepo.CharacterRecord) (byte, error) {
	if session != nil && isPVPChannel(session.channel) {
		return byte(currentPVPTownID), nil
	}
	if spec, ok := currentRaidTownSpecForSession(session); ok {
		return byte(spec.townID), nil
	}
	if session != nil && session.channelReconnect && !session.channelReconnectNewCharacter &&
		character.Stats["town_id"] != currentAntonRaidTownID {
		return requiredCurrentInitialTownU8(character, "town_id")
	}
	return newCharacterInitialTownID, nil
}

func currentSelectAckCreatedTime(character dnfrepo.CharacterRecord, hasCharacter bool) uint32 {
	if !hasCharacter || character.CreatedAt.IsZero() {
		return 0
	}
	seconds := character.CreatedAt.Unix()
	if seconds <= 0 {
		return 0
	}
	if seconds > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(seconds)
}

func currentCharacterFatigueState(character dnfrepo.CharacterRecord, hasCharacter bool) currentFatigueState {
	if !hasCharacter {
		return currentFatigueState{}
	}
	limit := statU16(character, "fatigue_limit", 0)
	remaining, hasRemaining := statInt64OK(character, "fatigue")
	if hasRemaining {
		if remaining < 0 {
			remaining = 0
		}
		if remaining > int64(^uint16(0)) {
			remaining = int64(^uint16(0))
		}
		if limit == 0 {
			limit = uint16(remaining)
		}
	}

	used := statU16(character, "fatigue_used", 0)
	if hasRemaining && int64(limit) >= remaining {
		used = limit - uint16(remaining)
	}
	return currentFatigueState{
		used:        used,
		limit:       limit,
		actorAux:    statU16(character, "fatigue_update", 0),
		battery:     uint16(min(max(character.Stats[dnffatigue.FatigueBatteryChargeStatKey], 0), dnffatigue.FatigueBatteryChargeLimit)),
		batteryBuff: dnffatigue.BatteryBuffCount(character.Stats[dnffatigue.FatigueBatteryStatKey], character.Level, false),
		actorExtra:  statU16(character, "ack_fatigue_grownup_buff", 0),
	}
}

func buildCurrentFatigueBody(character dnfrepo.CharacterRecord, hasCharacter bool) []byte {
	return buildCurrentFatigueStateBody(currentCharacterFatigueState(character, hasCharacter))
}

func buildCurrentFatigueStateBody(state currentFatigueState) []byte {
	var writer packetWriter
	writer.writeUint16(state.used)
	writer.writeUint16(state.limit)
	writer.writeUint16(state.actorAux)
	// sub_26929D0/sub_2692A90 feed EXP-UP, not the consumed fatigue display.
	writer.writeUint16(state.batteryBuff)
	writer.writeUint16(state.actorExtra)
	return writer.bytes()
}

func currentSelectAckQuestRows(record dnfrepo.QuestRecord, hasRecord bool) []currentSelectAckQuestRow {
	if !hasRecord || (len(record.States) == 0 && len(record.Progress) == 0) {
		return nil
	}
	rows := make([]currentSelectAckQuestRow, 0, len(record.States)+len(record.Progress))
	seen := make(map[int64]struct{}, len(record.States)+len(record.Progress))
	collect := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if _, ok := seen[questID]; ok {
				continue
			}
			// Mark every canonical States entry before filtering by status. A
			// completed/abandoned canonical row must suppress a stale active
			// copy in the legacy Progress map below.
			seen[questID] = struct{}{}
			if !strings.EqualFold(strings.TrimSpace(state.Status), "active") || questID <= 0 || questID >= 0xffff {
				continue
			}
			triggerValue := uint32(0)
			if state.ProgressValue > 0 {
				triggerValue = uint32(state.ProgressValue)
				if state.ProgressValue > int64(^uint32(0)) {
					triggerValue = ^uint32(0)
				}
			}
			rows = append(rows, currentSelectAckQuestRow{questID: uint16(questID), triggerValue: triggerValue})
		}
	}
	// States is the canonical active-quest owner for new writes.  Progress is
	// still populated by older dungeon/quest code paths and by migrated rows,
	// so selection ACK / op574 must merge both or the client sees an empty task
	// tracker immediately after relogging or completing a chain step.
	collect(record.States)
	collect(record.Progress)
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].questID < rows[j].questID
	})
	return rows
}

func currentSelectAckQuestLayout(body []byte) (int, bool) {
	if len(body) <= currentSelectAckPremiumCountOffset {
		return 0, false
	}
	premiumCount := int(body[currentSelectAckPremiumCountOffset])
	offset := currentSelectAckPremiumCountOffset + 1 + premiumCount*9 + 4
	minimum := offset + currentSelectAckFixedQuestCount*currentSelectAckQuestRowSize + 4
	return offset, minimum <= len(body)
}

func currentSelectAckTailOffsets(body []byte) (int, int, bool) {
	questOffset, ok := currentSelectAckQuestLayout(body)
	if !ok {
		return 0, 0, false
	}
	overflowOffset := questOffset + currentSelectAckFixedQuestCount*currentSelectAckQuestRowSize
	if overflowOffset+4 > len(body) {
		return 0, 0, false
	}
	overflowCount := uint64(binary.LittleEndian.Uint32(body[overflowOffset : overflowOffset+4]))
	overflowRowsOffset := overflowOffset + 4
	availableRows := uint64((len(body) - overflowRowsOffset) / currentSelectAckQuestRowSize)
	if overflowCount > availableRows {
		return 0, 0, false
	}
	stateOffset := overflowRowsOffset + int(overflowCount)*currentSelectAckQuestRowSize
	if stateOffset+currentSelectAckStateByteSize > len(body) {
		return 0, 0, false
	}
	tailOffset := stateOffset + currentSelectAckStateByteSize
	if tailOffset+3 > len(body) {
		return 0, 0, false
	}
	tutorialCount := int(body[tailOffset+2])
	postTutorialOffset := tailOffset + 3 + tutorialCount
	if postTutorialOffset+8 > len(body) {
		return 0, 0, false
	}
	secondaryCount := int(body[postTutorialOffset+7])
	if secondaryCount > (len(body)-(postTutorialOffset+8))/currentSelectAckQuestRowSize {
		return 0, 0, false
	}
	return tailOffset, postTutorialOffset, true
}

func currentSelectAckIntermediateState(body []byte) ([currentSelectAckStateU32Count]uint32, int, bool) {
	var values [currentSelectAckStateU32Count]uint32
	questOffset, ok := currentSelectAckQuestLayout(body)
	if !ok {
		return values, 0, false
	}
	overflowOffset := questOffset + currentSelectAckFixedQuestCount*currentSelectAckQuestRowSize
	if overflowOffset+4 > len(body) {
		return values, 0, false
	}
	overflowCount := uint64(binary.LittleEndian.Uint32(body[overflowOffset : overflowOffset+4]))
	overflowRowsOffset := overflowOffset + 4
	availableRows := uint64((len(body) - overflowRowsOffset) / currentSelectAckQuestRowSize)
	if overflowCount > availableRows {
		return values, 0, false
	}
	stateOffset := overflowRowsOffset + int(overflowCount)*currentSelectAckQuestRowSize
	if stateOffset+currentSelectAckStateByteSize > len(body) {
		return values, 0, false
	}
	for idx := range values {
		offset := stateOffset + idx*4
		values[idx] = binary.LittleEndian.Uint32(body[offset : offset+4])
	}
	return values, stateOffset, true
}

func currentSelectAckQuestRowCounts(body []byte) (int, int, bool) {
	questOffset, ok := currentSelectAckQuestLayout(body)
	if !ok {
		return 0, 0, false
	}
	fixedCount := 0
	for idx := 0; idx < currentSelectAckFixedQuestCount; idx++ {
		offset := questOffset + idx*currentSelectAckQuestRowSize
		if binary.LittleEndian.Uint16(body[offset:offset+2]) != 0xffff {
			fixedCount++
		}
	}
	overflowOffset := questOffset + currentSelectAckFixedQuestCount*currentSelectAckQuestRowSize
	overflowCount := int(binary.LittleEndian.Uint32(body[overflowOffset : overflowOffset+4]))
	if _, _, ok := currentSelectAckTailOffsets(body); !ok {
		return fixedCount, 0, false
	}
	return fixedCount, overflowCount, true
}

func currentSelectAckCharacterID(body []byte) (uint16, bool) {
	if len(body) < currentSelectAckCharacterIDOffset+2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(body[currentSelectAckCharacterIDOffset : currentSelectAckCharacterIDOffset+2]), true
}

func currentSelectAckTownIDFromBody(body []byte) (uint8, bool) {
	tailOffset, _, ok := currentSelectAckTailOffsets(body)
	if !ok {
		return 0, false
	}
	return body[tailOffset], true
}

func currentSelectAckTutorialState(body []byte) (uint8, uint8, bool) {
	tailOffset, _, ok := currentSelectAckTailOffsets(body)
	if !ok {
		return 0, 0, false
	}
	return body[tailOffset+1], body[tailOffset+2], true
}

func currentSelectAckTutorialIndexes(body []byte) ([]byte, bool) {
	tailOffset, _, ok := currentSelectAckTailOffsets(body)
	if !ok {
		return nil, false
	}
	count := int(body[tailOffset+2])
	start := tailOffset + 3
	if count > len(body)-start {
		return nil, false
	}
	return append([]byte(nil), body[start:start+count]...), true
}

// selectedCharacterStartsInTutorial decides only the first scene after
// character selection. Newly created characters persist tutorial_completed=0;
// the completed tutorial path persists 1 before returning to town. Older
// records that predate the marker fall back to level 1 so an established
// character is not silently forced back into the tutorial.
func selectedCharacterStartsInTutorial(character dnfrepo.CharacterRecord, hasCharacter bool) bool {
	if !hasCharacter {
		return false
	}
	if character.Stats != nil {
		if completed, ok := character.Stats[currentDungeonTutorialCompletedKey]; ok {
			return completed != currentDungeonTutorialCompleteFlag
		}
	}
	return character.Level <= 1
}
