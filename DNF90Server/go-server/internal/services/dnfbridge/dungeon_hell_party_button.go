package dnfbridge

import (
	"context"
	"fmt"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The world map page's 挑战深渊派对 button is greyed by exactly one server-owned
// input: the first list of the class0/op27 ENTER_SELECT_DUNGEON notification.
//
// Client evidence (DNF.exe is the current EXE; the same binary the IDA database
// D:\ida_copy\NoPack.exe.i64 was built from -- the string-xref scan below was
// re-run on our own client-patch\bin\DNF.exe and matched every address):
//
//	op27 class0 reader            sub_1D868B0
//	0x01D86B84  call 0x34E3970    read u8  -> list0 count
//	0x01D86B98  lea edi,[ebp-24]  8 x u16 scratch array (0xFFFF filled)
//	0x01D86BA0  call 0x34E39E0    read u16 -> array, count times
//	0x01D86BCC  mov [ebp-450], 0  count > 0 => hell button disabled
//	0x01D86BD9  push 0x1537       5431 "因存在未完成深渊派对任务的队员，无法激活深渊派对按钮。"
//	0x01D86F79  call sub_341B6A0  publish (hellEnabled, dungeonSupportsHell)
//	0x0341B740  mov [esi+0x438],dl  the selection window's hell state
//
// sub_341B6A0 has exactly two callers: sub_1D868B0 above and the selector
// teardown at 0x01CF8EB8 (which clears it with (0,0)). Its two local gates
// (sub_3419C60: actor byte at +0x220 and a party word at +0x66) never consult
// the quest manager, so a quest-driven grey-out can only arrive through this
// packet. The list's *values* are read into a scratch array and never used
// again -- the client only counts entries -- so an entry list can only ever
// disable the button, never enable it.
//
// Wire layout inside the op27 body (verified against the reader, the C#
// reference and the live body):
//
//	+0 u32, +4 u32, +8 u8 count, +9 count x u16 slots
//
// The shipped body is 37 bytes; every added slot adds 2 bytes.
//
// The reference servers compute the list the same way (86JP and S4A21 are the
// same file): Network/Handlers/Dungeon/DungeonEntryHandler.cs
// BuildHellPartySelectionState + Network/Builders/Dungeon/
// EnterSelectDungeonStateBuilder.cs BuildA21EnterSelectDungeon(userIds,
// hellQuestBlockedSlots) -- for every party member (solo is a one-member party
// at slot 0), when the current world map area owns [hell dungeon] and the member
// fails CheckHellQuestRequirement (Game/Dungeon/DungeonEntryCostService.cs),
// that member's SlotIndex goes into the list.
const currentHellPartyBlockedSlotLimit = 8

// currentHellPartyButtonMember is one participant of the selector page whose
// eligibility the client renders. CharacterID is the runtime character id the
// quest and permission repositories are keyed by; Slot is the native party slot
// carried by class0/op9 (alignedcmd.PartyMemberState.Slot).
type currentHellPartyButtonMember struct {
	CharacterID uint16
	Slot        uint16
}

// currentHellPartyButtonBlockedSlots projects the party members who may not
// challenge the current area's hell party onto the op27 list0 entries.
//
// Fail-open contract: this runs on the select-page critical path, so every
// unexpected condition (no repositories, no world map, no town gate, repository
// error, timeout) returns nil and the packet stays byte-identical to what it
// was before this projection existed. Only a proven "member is not entitled"
// adds an entry.
func (s *Service) currentHellPartyButtonBlockedSlots(session *gameSession, source string) []uint16 {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	area, found := s.currentSelectorHellPartyArea(session)
	if !found {
		return nil
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Quest == nil || repositories.HellPartyPermission == nil {
		s.logGameEvent(session, "game-upper-current-dungeon-context-hell-blocked-skipped",
			"source", source,
			"char_id", session.selectedCharacterID,
			"area_id", area.ID,
			"reason", "repository_unavailable")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	started := time.Now()
	members := s.currentSelectorHellPartyMembers(session)
	blocked := make([]uint16, 0, len(members))
	for _, member := range members {
		if member.CharacterID == 0 {
			continue
		}
		granted, err := s.currentHellPartyAreaGranted(ctx, repositories, member.CharacterID, area)
		if err != nil {
			s.logGameEvent(session, "game-upper-current-dungeon-context-hell-blocked-skipped",
				"source", source,
				"char_id", session.selectedCharacterID,
				"area_id", area.ID,
				"member_char_id", member.CharacterID,
				"reason", "eligibility_unavailable",
				"error", err)
			return nil
		}
		if !granted {
			blocked = append(blocked, member.Slot)
		}
	}
	if len(blocked) > currentHellPartyBlockedSlotLimit {
		// The client reads this list into an eight entry array; over-long lists
		// would run past it.
		blocked = blocked[:currentHellPartyBlockedSlotLimit]
	}
	s.logGameEvent(session, "game-upper-current-dungeon-context-hell-blocked",
		"source", source,
		"char_id", session.selectedCharacterID,
		"area_id", area.ID,
		"hell_quest_ids", area.HellQuestIDs,
		"party_members", len(members),
		"blocked_slots", blocked,
		"elapsed_ms", time.Since(started).Milliseconds(),
		"sequence", "current_exe_class0_op27_list0_hell_quest_blocked_slots")
	return blocked
}

// currentSelectorHellPartyArea resolves the world map area the open select page
// belongs to, and reports whether that area owns a hell dungeon.
//
// The page is owned by the selection owner (the party leader), so a follower
// resolves the leader's frozen selector origin first and only falls back to its
// own. The town-area link is the PVF's own [dungeon gate] value, which is a
// world map area id -- evidence: town 22 area 3 (Elvengard_hendon.map) declares
// [dungeon gate] 114, CentralPark declares 39, and the hell-party permission
// rows written for those gates are keyed by exactly those area ids (area 39 in
// the GM unlock path). The reference server reads the same value as
// Town.cs CeraRoomInfo.WorldMapAreaId = area.LinkedId.
func (s *Service) currentSelectorHellPartyArea(session *gameSession) (worldmap.Area, bool) {
	owner := s.currentSelectorOwnerSession(session)
	if area, found := s.selectorHellPartyAreaForSession(owner); found {
		return area, true
	}
	if owner != nil && owner != session {
		if area, found := s.selectorHellPartyAreaForSession(session); found {
			return area, true
		}
	}
	return worldmap.Area{}, false
}

func (s *Service) selectorHellPartyAreaForSession(session *gameSession) (worldmap.Area, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return worldmap.Area{}, false
	}
	session.townMu.Lock()
	snapshot := session.townPositionSnapshot
	session.townMu.Unlock()
	if !snapshot.PositionValid || snapshot.CharacterID != session.selectedCharacterID {
		return worldmap.Area{}, false
	}
	gate, found := s.townArea(int64(snapshot.TownID), int64(snapshot.AreaID))
	if !found || gate.DungeonGate == nil || *gate.DungeonGate <= 0 {
		return worldmap.Area{}, false
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return worldmap.Area{}, false
	}
	area, found := table.FindArea(*gate.DungeonGate)
	if !found || !area.HellDungeon.Set || area.HellDungeon.Value != 1 || len(area.HellQuestIDs) == 0 {
		return worldmap.Area{}, false
	}
	return area, true
}

// currentSelectorOwnerSession returns the session that owns the open selection
// page: the party leader for a follower, otherwise the session itself. A page
// opened by a leader is shared by every member, but each member receives its own
// op27, so the followers resolve the leader's town gate rather than their own.
func (s *Service) currentSelectorOwnerSession(session *gameSession) *gameSession {
	if s == nil || session == nil {
		return nil
	}
	state := runtimePartyStateSnapshot(session)
	if state.PartyID <= 0 || state.UserID == 0 || state.UserID == session.selectedCharacterID {
		return session
	}
	if leader, found := s.onlineGameSession(state.UserID); found && leader != nil {
		return leader
	}
	return session
}

// currentSelectorHellPartyMembers lists the characters whose eligibility the
// client evaluates. A character without a runtime party is the one-member party
// the reference builder uses, at slot 0.
func (s *Service) currentSelectorHellPartyMembers(session *gameSession) []currentHellPartyButtonMember {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	state := runtimePartyStateSnapshot(session)
	if state.PartyID > 0 {
		members := make([]currentHellPartyButtonMember, 0, len(state.Members))
		for _, member := range runtimePartyMembers(state) {
			if member.UserID == 0 {
				continue
			}
			members = append(members, currentHellPartyButtonMember{
				CharacterID: member.UserID,
				Slot:        uint16(member.Slot),
			})
		}
		if len(members) != 0 {
			return members
		}
	}
	return []currentHellPartyButtonMember{{
		CharacterID: session.selectedCharacterID,
		Slot:        0,
	}}
}

// currentHellPartyAreaGranted is the single accepted proof that a character may
// challenge the hell party of one world map area. The dungeon entry gate
// (authorizeCurrentDungeonHellParty) and the selector button projection must
// never diverge: a greyed button with an accepted entry, or the reverse, is the
// bug this helper exists to prevent.
func (s *Service) currentHellPartyAreaGranted(
	ctx context.Context,
	repositories dnfrepo.Group,
	characterID uint16,
	area worldmap.Area,
) (bool, error) {
	if repositories.HellPartyPermission == nil || characterID == 0 {
		return false, nil
	}
	characterKey := fmt.Sprintf("%d", characterID)
	permission, found, err := repositories.HellPartyPermission.Load(ctx, characterKey)
	if err != nil {
		return false, err
	}
	if found {
		for _, entry := range permission.Entries {
			if entry.AreaID == area.ID {
				return true, nil
			}
		}
	}
	// The durable permission row is written only by the normal finish path of a
	// [hell challenge] quest. A quest that was marked completed any other way
	// (GM "unlock all Hell", records that predate the permission table) never
	// reaches that path again, so the persisted completed state of the area's
	// [hell quest] is the second accepted proof.
	if repositories.Quest == nil {
		return false, nil
	}
	quests, found, err := repositories.Quest.Load(ctx, characterKey)
	if err != nil {
		return false, err
	}
	if found && currentHellQuestRecordUnlocksArea(quests, area) {
		return true, nil
	}
	return false, nil
}
