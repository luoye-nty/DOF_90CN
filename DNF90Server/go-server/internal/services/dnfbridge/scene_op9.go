package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSceneOp9ActorDisplayKind byte   = 0
	currentSceneOp9ActorRemoveKind  byte   = 3
	currentSceneOp9StableSceneValue uint16 = 9999
)

func buildCurrentSceneOp9NoopBody() []byte {
	var writer packetWriter
	writer.writeUint16(0)
	writer.writeUint16(currentSceneOp9StableSceneValue)
	return writer.bytes()
}

func buildCurrentSceneOp9ActorRemovalBody(sceneObjectKey uint16) []byte {
	return buildCurrentSceneOp9ActorRemovalBodyInContext(
		sceneObjectKey,
		currentSceneObjectContext,
	)
}

func buildCurrentSceneOp9ActorRemovalBodyInContext(
	sceneObjectKey uint16,
	ownerChannel byte,
) []byte {
	var writer packetWriter
	writer.writeUint16(1)
	writer.writeUint16(currentSceneOp9StableSceneValue)
	writer.writeUint16(sceneObjectKey)
	writer.writeByte(0)
	writer.writeByte(ownerChannel)
	writer.writeByte(0)
	writer.writeByte(currentSceneOp9ActorRemoveKind)
	return writer.bytes()
}

func (s *Service) sendCurrentSceneOp9PreviewActorRemovalOnce(session *gameSession, source string) error {
	if session == nil || !session.selectPreviewObjectStateSent || session.selectPreviewActorRemoved {
		return nil
	}
	objectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	if objectKey == 0 {
		return nil
	}
	body := buildCurrentSceneOp9ActorRemovalBodyInContext(
		objectKey,
		currentSceneObjectContext,
	)
	s.logGameEvent(session, "game-upper-current-scene-op9-preview-actor-remove-send",
		"source", source,
		"char_id", session.selectedCharacterID,
		"object_key", objectKey,
		"msg_id", uint16(dnfenum.CmdPacketRecoverStamina),
		"classification", 0,
		"record_kind", currentSceneOp9ActorRemoveKind,
		"body_len", len(body),
		"body_source", "current_exe_sub_1D64CA0_kind3_clear_slots_and_remove_target",
		"reason", "evict_transition_preview_before_final_actor_mode0_mode1_bind")
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketRecoverStamina), body, 0); err != nil {
		return err
	}
	session.selectPreviewActorRemoved = true
	session.currentSceneObjectListSent = false
	return nil
}

func buildCurrentSceneOp9ActorDisplayBody(sceneObjectKey uint16, character dnfrepo.CharacterRecord, hasCharacter bool, fallbackName string) []byte {
	return buildCurrentSceneOp9ActorDisplayBodyInContext(sceneObjectKey, character, hasCharacter, fallbackName, currentSceneObjectContext)
}

func buildCurrentSceneOp9ActorDisplayBodyInContext(sceneObjectKey uint16, character dnfrepo.CharacterRecord, hasCharacter bool, fallbackName string, ownerChannel byte) []byte {
	return buildCurrentSceneOp9ActorPartyDisplayBodyInContext(
		sceneObjectKey,
		character,
		hasCharacter,
		fallbackName,
		ownerChannel,
		ownerChannel,
		nil,
		0,
	)
}

type currentSceneOp9PartyMemberProjection struct {
	State alignedcmd.PartyMemberState
	Name  string
	Job   byte
	Level byte
	Grow  byte
}

func buildCurrentSceneOp9ActorPartyDisplayBodyInContext(
	sceneObjectKey uint16,
	character dnfrepo.CharacterRecord,
	hasCharacter bool,
	fallbackName string,
	ownerChannel byte,
	localChannel byte,
	members []currentSceneOp9PartyMemberProjection,
	selectedMemberSlot byte,
) []byte {
	if sceneObjectKey == 0 {
		sceneObjectKey = currentSceneBootstrapObjectKey
	}
	name := fallbackName
	if name == "" && hasCharacter {
		name = character.Name
	}
	level := byte(1)
	job := byte(0)
	grow := byte(0)
	if hasCharacter {
		if character.Level > 0 && character.Level < 256 {
			level = byte(character.Level)
		}
		job = byte(numericCharacterStat(character.Job))
		grow = byte(numericCharacterStatValue(character, "grow_type"))
	}

	var writer packetWriter
	writer.writeUint16(1)
	writer.writeUint16(currentSceneOp9StableSceneValue)

	writer.writeUint16(sceneObjectKey)
	writer.writeByte(0)
	writer.writeByte(ownerChannel)
	writer.writeByte(0)
	writer.writeByte(currentSceneOp9ActorDisplayKind)

	writer.writeByte(currentSceneObjectRoute)
	// sub_1D64CA0 uses this nested byte as the inline-identity discriminator,
	// not as the actor-owner key. Zero is required for the following DSTR name
	// branch; the real actor owner is already carried by the outer record.
	writer.writeByte(currentSceneObjectContext)
	writer.writeRawDstr(rosterNameBytes(name))
	writer.writeByte(job)
	writer.writeByte(level)
	writer.writeUint32(0)
	writer.writeByte(grow)
	writer.writeUint16(0)
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeUint16(0)

	members = currentSceneOp9PartyMembers(members)
	writer.writeByte(byte(len(members)))
	for slot, member := range members {
		writer.writeByte(byte(slot))
		writer.writeUint16(member.State.UserID)
		if ownerChannel != localChannel {
			writer.writeByte(member.Job)
			writer.writeByte(member.Level)
			writer.writeByte(member.Grow)
			writer.writeByte(0)
			writer.writeRawDstr(rosterNameBytes(member.Name))
		}
		writer.writeByte(0)
		writer.writeByte(currentSceneOp9PartyMemberState(member.State.UserState))
		writer.writeByte(0)
	}
	writer.writeByte(0)
	writer.writeByte(selectedMemberSlot)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes()
}

// buildCurrentSceneOp9PartyInformationBodyInContext constructs the complete
// same-version kind-0 party aggregate consumed by NoPack sub_1D64CA0. The
// outer key is the stable party model ID shared by every observer, not a
// selected character's actor key. The byte after the member table is the
// authoritative leader slot; it must be identical on every member client.
func buildCurrentSceneOp9PartyInformationBodyInContext(
	partyModelKey uint16,
	ownerServer byte,
	ownerChannel byte,
	state alignedcmd.PartyState,
	members []currentSceneOp9PartyMemberProjection,
	leaderSlot byte,
) []byte {
	var writer packetWriter
	writer.writeUint16(1)
	writer.writeUint16(currentSceneOp9StableSceneValue)
	writer.writeUint16(partyModelKey)
	writer.writeByte(0)
	writer.writeByte(ownerChannel)
	writer.writeByte(0)
	writer.writeByte(currentSceneOp9ActorDisplayKind)

	writer.writeByte(ownerServer)
	writer.writeByte(state.RequestPrefix1)
	if state.RequestPrefix1 == 0 {
		writer.writeRawDstr(state.NameBytes)
	}
	writer.writeByte(ownerChannel)
	maxMembers := state.MaxMembers
	if maxMembers == 0 || maxMembers > 4 {
		maxMembers = 4
	}
	writer.writeByte(maxMembers)
	writer.writeUint32(state.SelectionID)
	writer.writeByte(state.SelectionCode)
	writer.writeUint16(state.SelectionValue)
	writer.writeByte(state.RecruitFlag)
	writer.writeByte(state.TargetMode)
	writer.writeUint16(state.TargetDungeonID)

	members = currentSceneOp9PartyMembers(members)
	writer.writeByte(byte(len(members)))
	for _, member := range members {
		writer.writeByte(member.State.Slot)
		writer.writeUint16(member.State.UserID)
		writer.writeByte(0)
		writer.writeByte(currentSceneOp9PartyMemberState(member.State.UserState))
		writer.writeByte(0)
	}
	writer.writeByte(0)
	writer.writeByte(leaderSlot)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes()
}

// buildCurrentSceneOp9PartyMembershipBodyInContext constructs the complete
// kind-2/kind-6 fixed-slot replacement used after leave, kick and area detach.
// Unlike kind 0 it carries no recruitment metadata.
func buildCurrentSceneOp9PartyMembershipBodyInContext(
	partyModelKey uint16,
	ownerChannel byte,
	members []alignedcmd.PartyMemberState,
	leaderSlot byte,
	kind byte,
) []byte {
	if kind != 2 && kind != 6 {
		return nil
	}
	var writer packetWriter
	writer.writeUint16(1)
	writer.writeUint16(currentSceneOp9StableSceneValue)
	writer.writeUint16(partyModelKey)
	writer.writeByte(0)
	writer.writeByte(ownerChannel)
	writer.writeByte(0)
	writer.writeByte(kind)
	writer.writeByte(byte(len(members)))
	for _, member := range members {
		writer.writeByte(member.Slot)
		writer.writeUint16(member.UserID)
		writer.writeByte(0)
		writer.writeByte(currentSceneOp9PartyMemberState(member.UserState))
		writer.writeByte(0)
	}
	writer.writeByte(0)
	writer.writeByte(leaderSlot)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes()
}

func currentSceneOp9SelectedPartySlot(members []currentSceneOp9PartyMemberProjection, selectedMemberID uint16) (byte, bool) {
	for _, member := range members {
		if member.State.UserID == selectedMemberID {
			return member.State.Slot, true
		}
	}
	return 0, false
}

func currentSceneOp9PartyMembers(members []currentSceneOp9PartyMemberProjection) []currentSceneOp9PartyMemberProjection {
	const maxCurrentScenePartySlots = 8
	capacity := len(members)
	if capacity > maxCurrentScenePartySlots {
		capacity = maxCurrentScenePartySlots
	}
	filtered := make([]currentSceneOp9PartyMemberProjection, 0, capacity)
	for _, member := range members {
		if member.State.UserID == 0 {
			continue
		}
		duplicate := false
		for _, existing := range filtered {
			if existing.State.UserID == member.State.UserID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		filtered = append(filtered, member)
		if len(filtered) == maxCurrentScenePartySlots {
			break
		}
	}
	return filtered
}

func currentSceneOp9PartyMemberState(value byte) byte {
	if value == 0 || value == 1 {
		return 100
	}
	return value
}

// currentSceneOp9PartySlotRow is one explicit-slot member row of a dungeon-room
// kind-0 record. The town display body numbers its rows densely by list
// position; a room record must instead carry every member's real fixed slot,
// because sub_269FB90 (IsLocalPlayerHost) compares the LOCAL actor's index in
// sceneActorManager+0x48 against the host slot at +0x68. A dense index would
// make every member land on slot 0 and therefore claim to be the host.
type currentSceneOp9PartySlotRow struct {
	Slot   byte
	Member currentSceneOp9PartyMemberProjection
}

// currentSceneOp9PartyMemberSlotPresent is the third trailing byte of a member
// row. sub_1D64CA0 stores it into an eight-byte stack array that is fully zero
// initialised, and after the row loop it walks all eight slots and calls
// sub_2FB5E50(slot, value == 0 ? 1 : 0), writing sceneRoot[0x20A09C + slot*4].
// A reader at 0x02FB67CE skips any slot whose entry is non-zero, so a row must
// send a NON-ZERO byte here to be counted: zero inverts to 1 and the slot is
// dropped, making real members indistinguishable from untouched empty slots.
// (Reverse-engineered on the old tree's party-dungeon-sync branch, 2026-08-19.)
const currentSceneOp9PartyMemberSlotPresent byte = 1

// currentSceneOp9PartyMemberCap resolves the member cap the party window shows
// as the denominator of its "x / y" label. sub_1D64CA0 stores the byte whole
// into party_info+0x4c and 0x00A07A87 reads that dword when formatting the
// label; the numerator is never a packet field.
func currentSceneOp9PartyMemberCap(state alignedcmd.PartyState) byte {
	const maxCurrentScenePartySlots = 8
	memberCap := state.MaxMembers
	if memberCap == 0 {
		return 0
	}
	if memberCap > maxCurrentScenePartySlots {
		memberCap = maxCurrentScenePartySlots
	}
	return memberCap
}

// currentSceneOp9PartyTypeAux carries the party type the list window
// categorises by (party_info+0x62). The current client reads op12's own order
// of normal, story, loop; anything above 2 means the create window never chose
// a type.
func currentSceneOp9PartyTypeAux(state alignedcmd.PartyState) byte {
	const maxPartyType = 2
	if state.TargetMode > maxPartyType {
		return 0
	}
	return state.TargetMode
}

// buildCurrentSceneOp9DungeonRoomSlotTableBodyInContext constructs the kind-0
// record a party member receives inside its initial dungeon room.
//
// The current EXE arbitrates who simulates the room from this record.
// sub_1D64CA0 first calls the scene actor manager's reset (0x01D66275),
// clearing the whole +0x48 slot table, then repopulates it through the by-id
// writer sub_26A2BB0 (its only caller, 0x01D66620) for all eight slots and sets
// the host slot through set_host (0x01D666AF, validated `slot <= 7`).
// sub_269FB90 - the predicate 46 combat, AI and UI call sites use - then
// answers "am I the host" by locating the LOCAL actor inside that slot table and
// comparing its index against the host slot. A member whose actor is absent
// scores -1 and answers false, so an empty table makes *every* member a
// non-host: nobody runs the authoritative simulation, and everything only the
// controller publishes over P2P - including the packed level/attributes of a
// party buff - is never sent.
//
// Two shape rules were paid for with live client faults on the old tree:
//   - every uid named here must resolve to an actor already created in the
//     receiver's room (mode0/mode1 first, slot table after), otherwise
//     sub_1D64CA0 dereferences a missing actor and walks off its stack buffer;
//   - a record whose owner context is the dungeon's 0 is parsed with the
//     EXTENDED member branch (job, level, grow, flag, name DSTR before the
//     three state bytes). The short town row makes sub_34E3AC0 read a garbage
//     DSTR length and memcpy past the stack.
//
// The record actor is the receiver's own; rows carry each member's real fixed
// slot; the tail's second byte is the leader slot and is identical on every
// member client, which is what makes exactly one of them the host.
func buildCurrentSceneOp9DungeonRoomSlotTableBodyInContext(
	sceneObjectKey uint16,
	character dnfrepo.CharacterRecord,
	hasCharacter bool,
	fallbackName string,
	ownerChannel byte,
	rows []currentSceneOp9PartySlotRow,
	leaderSlot byte,
	maxMembers byte,
	partyType byte,
) []byte {
	if sceneObjectKey == 0 {
		sceneObjectKey = currentSceneBootstrapObjectKey
	}
	name := fallbackName
	if name == "" && hasCharacter {
		name = character.Name
	}
	job := byte(0)
	grow := byte(0)
	if hasCharacter {
		job = byte(numericCharacterStat(character.Job))
		grow = byte(numericCharacterStatValue(character, "grow_type"))
	}

	var writer packetWriter
	writer.writeUint16(1)
	writer.writeUint16(currentSceneOp9StableSceneValue)

	writer.writeUint16(sceneObjectKey)
	writer.writeByte(0)
	writer.writeByte(ownerChannel)
	writer.writeByte(0)
	writer.writeByte(currentSceneOp9ActorDisplayKind)

	writer.writeByte(currentSceneObjectRoute)
	// sub_1D64CA0 reads this byte into party_info+0x04 and uses zero to select
	// the inline-identity name branch; the real actor owner is already carried
	// by the outer record.
	writer.writeByte(currentSceneObjectContext)
	writer.writeRawDstr(rosterNameBytes(name))
	writer.writeByte(job)
	writer.writeByte(maxMembers)
	writer.writeUint32(0)
	writer.writeByte(grow)
	writer.writeUint16(0)
	writer.writeByte(0)
	writer.writeByte(partyType)
	writer.writeUint16(0)

	writer.writeByte(byte(len(rows)))
	for _, row := range rows {
		member := row.Member
		writer.writeByte(row.Slot)
		writer.writeUint16(member.State.UserID)
		writer.writeByte(member.Job)
		writer.writeByte(member.Level)
		writer.writeByte(member.Grow)
		writer.writeByte(0)
		writer.writeRawDstr(rosterNameBytes(member.Name))
		writer.writeByte(0)
		writer.writeByte(currentSceneOp9PartyMemberState(member.State.UserState))
		writer.writeByte(currentSceneOp9PartyMemberSlotPresent)
	}
	// GameWorld::send_party_info_to_all closes the roster with
	// IsReturnUserParty, the leader slot, is_quick_party and IsEventCharacParty.
	writer.writeByte(0)
	writer.writeByte(leaderSlot)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes()
}
