package dnfbridge

// C2S 695 `ENUM_CMDPACKET_RAID_MONSTER_HP` is the client's 战况表 entry point.
//
// Live 2026-09-17 the current EXE sent it twice within 25-31 ms of the only two
// client faults recorded for this build, both after clearing a hatchery gate
// (221-224) and returning to town:
//
//	03:49:22.291Z conn=game-000567 type=695 body_len=5  -> 11:49:22.316 local
//	08:52:12.668Z conn=game-000178 type=695 body_len=5  -> 16:52:12.699 local
//	90CN_trace.log: exception 0xC0000005 eip=0x01F95C63 module=DNF.exe
//	offset=0x01B95C63 access_address=0x00000B98 ecx=0x00000000 (this=null)
//
// Those are the same fault address twice, and over the whole log every other
// raid return to town (0 of 126) left the process alive. The server had no
// handler at all: `dnfenum.CmdPacketRaidMonsterHp` was referenced only by its
// own name table, so the request was dropped silently and the client rendered
// the window from whatever it already had.
//
// The 86JP reference serves the same request in
// `RaidHandler.Situation.cs:137-192`: count==0 means "the window just opened (or
// is refreshing), resend the situation", count>=1 reports the runtime monster
// values of the sender's own gate. This file implements that handshake with the
// bodies this client already uses; it never invents a byte layout and never
// replays the one-shot op587 state=2 announcement.
//
// This is deliberately a pure addition: the opcode was unhandled before, so no
// existing packet is removed, reordered or altered by serving it.

import "fmt"

// The current EXE prefixes the value list with a dword and puts the count at +4,
// not +0:
//
//	body_len=5  -> 00000000 00              -> count 0
//	body_len=9  -> 00000000 01 64000000     -> count 1, value 100
//	body_len=21 -> 00000000 04 64000000 x4  -> count 4
//
// The 86JP reader (`TryReadRaidMonsterRuntimeValues`) starts at body[0], which
// would read every one of these as count 0 and misclassify every report as a
// window refresh, so its offset is not copied.
//
// The leading dword is NOT reserved padding. tools/_probe_634_report.md §6c
// traces the client's own 695 sender: 0x00F17A5E-0x00F17A65 feeds the SAME value
// both into 0x17C2B80 (the per-dungeon record lookup) and into this body
// (0x00F17A7B). It is therefore the client's record-set key inside its own
// (dungeon -> key -> records) map, and 634 must echo it back unchanged: an
// unknown key makes the client create a default record that persists and then
// re-enters its own later 695 reports.
const (
	raidSituationKeyBytes  = 4
	raidSituationMaxValues = 5
)

// decodeRaidSituationRequest splits C2S 695 into its record-set key and value
// list. A shape the current EXE has never produced is reported as invalid
// instead of guessed at.
func decodeRaidSituationRequest(body []byte) (recordSetKey uint32, values []uint32, err error) {
	if len(body) < raidSituationKeyBytes+1 {
		return 0, nil, fmt.Errorf("body too short for the leading key: len=%d", len(body))
	}
	count := int(body[raidSituationKeyBytes])
	if count > raidSituationMaxValues {
		return 0, nil, fmt.Errorf("value count out of range: count=%d", count)
	}
	if len(body) != raidSituationKeyBytes+1+count*4 {
		return 0, nil, fmt.Errorf("body length does not match the value count: len=%d count=%d", len(body), count)
	}
	recordSetKey = uint32(body[0]) | uint32(body[1])<<8 | uint32(body[2])<<16 | uint32(body[3])<<24
	values = make([]uint32, 0, count)
	for index := 0; index < count; index++ {
		offset := raidSituationKeyBytes + 1 + index*4
		values = append(values, uint32(body[offset])|uint32(body[offset+1])<<8|
			uint32(body[offset+2])<<16|uint32(body[offset+3])<<24)
	}
	return recordSetKey, values, nil
}

// handleOnlineRaidSituationMonsterHp serves C2S 695 while the session is on a
// raid channel.
//
// A malformed body is logged and swallowed: the 86JP handler answers neither, so
// returning an error here would only tear down the connection's read loop for a
// packet the client sends on a timer.
func (s *Service) handleOnlineRaidSituationMonsterHp(session *gameSession, typ uint16, body []byte) (bool, error) {
	if s == nil || session == nil {
		return false, nil
	}
	recordSetKey, values, err := decodeRaidSituationRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-situation-monster-hp-body-invalid",
			"type", typ, "body_len", len(body), "error", err)
		return true, nil
	}
	if len(values) != 0 {
		// count>=1 is the sender's own gate reporting runtime values. 86JP records
		// them per (raid, squad, dungeon) and then broadcasts its 0x027B
		// (RaidHandler.cs:82 `_raidMonsterRuntimeValues`, built at Situation.cs:433-456);
		// this is the same handshake on this client's 0x027A. Nothing here may fail
		// the caller: the request is telemetry from the client's HUD.
		key, recorded := s.recordRuntimeRaidMonsterRuntimeValues(session, recordSetKey, values)
		fields := []any{
			"type", typ,
			"char_id", session.selectedCharacterID,
			"record_set_key", recordSetKey,
			"values", fmt.Sprintf("%v", values),
			"value_count", len(values),
			"recorded", recorded,
			"raid_object_replay", false,
		}
		if recorded {
			fields = append(fields,
				"raid_key", key.RaidKey,
				"raid_group", key.Group,
				"raid_dungeon", key.DungeonID,
				"raid_solo_char_id", key.SoloCharID)
		} else {
			// A report that cannot be attributed to a started raid gate is dropped
			// like 86JP's non-participant branch (Situation.cs:233-236): the client
			// polls this opcode on a timer, so it also arrives outside a raid.
			fields = append(fields, "reason", "not_inside_an_attributable_raid_gate")
		}
		// A recorded report changes what every squad's row should draw, so the whole
		// raid is refreshed -- the client's row table is global to the raid, not per
		// squad (_probe_634_report.md section 6c Q1).
		if recorded {
			if state, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok {
				if broadcastErr := s.broadcastRuntimeRaidMonsterState(state); broadcastErr != nil {
					fields = append(fields, "broadcast_error", broadcastErr.Error())
				} else {
					fields = append(fields, "broadcast", true)
				}
			}
		}
		s.logGameEvent(session, "game-raid-situation-monster-hp-reported", fields...)
		return true, nil
	}
	// count==0: the 战况表 is open (or refreshing) and the client asked for the
	// situation. This is the state the client was left in when it faulted, so
	// answer it with the same persistent projection the selector reopen uses:
	// raid object mode0, roster mode3, symbol table, gate state table and the
	// phase timers. sendRuntimeRaidSelectorSnapshot is guarded (raid mode, started,
	// active phase) and deliberately does not replay the one-shot op587 state=2
	// announcement, which makes it idempotent for a request the client repeats
	// every 7-15s.
	if runtimeRaidModeForSession(session) == 0 {
		return true, nil
	}
	if err := s.sendRuntimeRaidSelectorSnapshot(session, "raid_situation_window"); err != nil {
		s.logGameEvent(session, "game-raid-situation-refresh-failed",
			"type", typ, "char_id", session.selectedCharacterID, "error", err)
	}
	// The records the board's cells are drawn from go BEFORE the rows: a cell whose
	// member has no record in this client's user table draws as 单人 with a black
	// avatar even though the row itself arrived, so repairing the records first is
	// what makes the repaint correct on the first frame.
	if state, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok {
		s.ensureRuntimeRaidBoardRowActors(session, state, "raid_situation_window_board_row_actors")
	}
	// The board's rows follow the snapshot: the snapshot is what re-sends the
	// 0x0248 participation rows this packet's rows are resolved through, so the
	// order matters (see raid_situation_broadcast.go). A raid with nobody in a
	// gate sends nothing here rather than an empty table.
	if state, ok := s.runtimeRaidForMember(session.selectedCharacterID); ok {
		if err := s.sendRuntimeRaidMonsterState(session, state); err != nil {
			s.logGameEvent(session, "game-raid-situation-rows-failed",
				"type", typ, "char_id", session.selectedCharacterID, "error", err)
		}
	}
	return true, nil
}

// ensureRuntimeRaidBoardRowActors installs, record-only, the actor record of every
// member the board's rows are keyed by.
//
// A board cell is resolved through the client's OWN user table: the client looks
// the row's first member key up in it (sub_F14C40 -> sub_2699A30) and hides the
// whole cell on a miss -- which is the operator's "看不到是哪队进图 就显示个单
// 队长头像也是黑的". That table is per scene, and every push that fills it (this
// file's boundary fan-out, co-presence, admission) is edge-triggered, so a client
// that was mid-transition at the wrong instant keeps an unresolvable cell until
// something happens to re-install that member.
//
// The 战况表 window asks for this refresh every time it is opened or repainted
// (C2S 695 count==0, measured on the live server at a 0.3-3 min cadence per
// window), and that request is the one moment the client has just proved its scene
// is alive AND is about to draw the board. Re-installing here makes a lost record
// self-heal within one repaint instead of waiting for the next raid-wide sweep.
//
// Scope and form are deliberately cheap: one install per row (the row's first
// member, which is the key the cell resolves), never for the requesting character
// itself, and record-only (mode0 + the op9 party frame, no mode1 equipment
// binding) because a portrait, name and level are all that come from mode0
// (sendRuntimeRaidPeerActorRecord). Re-sending a record the client already has is
// harmless: it repaints the same actor, which is what the boundary fan-out has
// always done for its repeats.
func (s *Service) ensureRuntimeRaidBoardRowActors(session *gameSession, state runtimeRaidState, source string) {
	if s == nil || session == nil || session.conn == nil ||
		state.RaidKey == 0 || session.selectedCharacterID == 0 {
		return
	}
	rows, ok := s.runtimeRaidMonsterStateRows(state)
	if !ok {
		return
	}
	installed := 0
	for _, row := range rows {
		if installed >= currentRuntimeRaidBoardRowActorInstallLimit {
			// A raid with more squads standing in gates than the limit converges
			// over successive refreshes rather than blocking one client loop for
			// the whole set.
			break
		}
		if len(row.Members) == 0 {
			continue
		}
		identity := row.Members[0]
		if identity == 0 || identity == session.selectedCharacterID {
			continue
		}
		s.sendRuntimeRaidPeerActorRecord(state, session, identity, source)
		installed++
	}
}

// currentRuntimeRaidBoardRowActorInstallLimit is how many row records one board
// refresh is allowed to install.
//
// The scope is one install per row, so this only bites a raid whose squads are
// spread over more gates than this at once (runtimeRaidMaxGroups is 10). The
// bound exists because each record-only install costs about 50ms of the receiving
// loop (measured on this fan-out: seven installs 0.35s without the mode1 half), and
// this path runs inside the client's own refresh request -- the common Anton board
// (a handful of squads in gates) is unaffected, and a wider one repairs over the
// next refresh or two (windows ask every 0.3-3 min).
const currentRuntimeRaidBoardRowActorInstallLimit = 4

// runtimeRaidMonsterRuntimeKey identifies one squad's reported runtime values.
//
// The squad is the client's own roster byte (runtimeRaidMemberSquad) and the
// dungeon is the gate that squad is standing in. That pair is what 634's rows are
// keyed by, and keying by the pair is what stops a value reported inside 擎天之柱
// from being drawn in 舰炮.
//
// SoloCharID exists for the same reason 86JP's GetRaidMonsterRuntimeKey carries a
// SoloMemberKey (Situation.cs:485-492): an unassigned member renders as its own
// row with group 0, so the group alone cannot tell two unassigned members apart.
type runtimeRaidMonsterRuntimeKey struct {
	RaidKey    uint32
	Group      byte
	SoloCharID uint16
	DungeonID  int
	// RecordSetKey is the leading dword of the sender's own C2S 695 body. The
	// client keeps its per-dungeon records in a (dungeon -> key -> records) map
	// and uses this value as the inner key, so a report and the 634 row that
	// answers it must carry the same number. Live bodies have always sent 0, but
	// inventing a different value is not harmless: an unknown key makes the
	// client create a default record that then persists and re-enters its own
	// later reports (tools/_probe_634_report.md §6c Q4).
	RecordSetKey uint32
}

// recordRuntimeRaidMonsterRuntimeValues attributes one C2S 695 report to the raid
// gate the sender is standing in and stores it.
//
// recorded=false covers every report that is not inside an attributable raid gate.
// That is the same drop 86JP performs for a stale or non-participant report
// (Situation.cs:233-236), and it matters here because the client sends this opcode
// on a 7-15s timer, so it also arrives in town and in ordinary dungeons.
//
// Lock order: session.dungeon.mu is taken only to read the binding and is released
// before s.raidMu is taken. That is the order the raid lifecycle already uses --
// notifyRuntimeRaidDungeonLifecycleLocked runs under dungeon.mu and then calls into
// the raid state -- so this never inverts it.
func (s *Service) recordRuntimeRaidMonsterRuntimeValues(
	session *gameSession,
	recordSetKey uint32,
	values []uint32,
) (runtimeRaidMonsterRuntimeKey, bool) {
	if s == nil || session == nil || len(values) == 0 {
		return runtimeRaidMonsterRuntimeKey{}, false
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	var binding runtimeRaidDungeonBinding
	if runtime != nil {
		binding = runtime.raidDungeon
	}
	session.dungeon.mu.Unlock()
	if binding.RaidKey == 0 || binding.MapID == 0 {
		return runtimeRaidMonsterRuntimeKey{}, false
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok ||
		state.RaidKey != binding.RaidKey ||
		!state.Started ||
		state.Lifecycle.Phase != binding.Phase {
		return runtimeRaidMonsterRuntimeKey{}, false
	}
	member, ok := runtimeRaidFrozenMember(state, session.selectedCharacterID)
	if !ok {
		return runtimeRaidMonsterRuntimeKey{}, false
	}
	key := runtimeRaidMonsterRuntimeKey{
		RaidKey:      binding.RaidKey,
		Group:        runtimeRaidMemberSquad(member.GroupIndex),
		DungeonID:    binding.MapID,
		RecordSetKey: recordSetKey,
	}
	if key.Group == 0 {
		key.SoloCharID = session.selectedCharacterID
	}
	stored := append([]uint32(nil), values...)

	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	if s.raidSituationValues == nil {
		s.raidSituationValues = make(map[runtimeRaidMonsterRuntimeKey][]uint32)
	}
	s.raidSituationValues[key] = stored
	return key, true
}

// runtimeRaidMonsterRuntimeValuesFor returns a copy of every stored report of one
// raid so a caller can build 634 without holding the lock while it encodes.
//
// The backing store (Service.raidSituationValues) holds the runtime values each
// squad reported in C2S 695, keyed by (raid, squad, solo member, gate). It is the
// equivalent of 86JP's _raidMonsterRuntimeValues (RaidHandler.cs:82, key built at
// Situation.cs:479-492), and one 634 row is exactly "one squad plus the values of
// the gate it stands in". It is guarded by s.raidMu -- the same lock as s.raids --
// and is purged when the raid itself is removed.
func (s *Service) runtimeRaidMonsterRuntimeValuesFor(
	raidKey uint32,
) map[runtimeRaidMonsterRuntimeKey][]uint32 {
	if s == nil || raidKey == 0 {
		return nil
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	out := make(map[runtimeRaidMonsterRuntimeKey][]uint32)
	for key, values := range s.raidSituationValues {
		if key.RaidKey != raidKey {
			continue
		}
		out[key] = append([]uint32(nil), values...)
	}
	return out
}

// purgeRuntimeRaidMonsterRuntimeValuesLocked drops every stored report of one raid.
// A report is only meaningful while the raid that produced it is alive, so keeping
// it would leak one entry per gate per squad for every raid the process has run.
// Caller holds s.raidMu.
func (s *Service) purgeRuntimeRaidMonsterRuntimeValuesLocked(raidKey uint32) {
	if s == nil || raidKey == 0 {
		return
	}
	for key := range s.raidSituationValues {
		if key.RaidKey == raidKey {
			delete(s.raidSituationValues, key)
		}
	}
}
