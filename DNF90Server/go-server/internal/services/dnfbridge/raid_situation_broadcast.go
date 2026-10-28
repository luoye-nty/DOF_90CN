package dnfbridge

import (
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
)

// S2C 0x027A (RaidMonsterStateMsgID) is the battle-situation board's row packet:
// one row per squad, carrying how many revive coins that squad has spent, the
// record-set key the squad itself reports under, and the per-object values the
// board draws its bars from.
//
// Everything below is shaped by tools/_probe_634_report.md (client disassembly of
// DNF.exe sha256 8A7AF614...AA4D) and by the 86JP reference
// (RaidHandler.Situation.cs:433-456, RaidPacketBuilder.cs:477-501). Three of those
// findings are hard constraints rather than preferences:
//
//  1. The client REPLACES its whole row table with this body. Its
//     vector<Group>::operator= (0x17BF680) has no slot-preservation path, so a
//     squad omitted from the body loses its row. Every squad that should be drawn
//     must be in the same packet.
//  2. A body with GROUP COUNT 0 is not "nothing to say": it clears the table
//     (0x01D504CC-0x01D50502 on the 587 side, 0x17BF6AC on this one). This file
//     therefore never emits an empty body -- see runtimeRaidMonsterStateRows.
//  3. A group's dungeon is not a field in the packet. The client derives it by
//     looking the group's FIRST member up in its 0x0248 participation rows
//     (0x1D50BB6 -> 0x17B4280 -> [mgr+0xCC]) and skips the whole value phase when
//     that lookup fails. The member list must therefore be the real squad, and the
//     0x0248 row for that squad must already have been sent.

// runtimeRaidMonsterRuntimeReport is one squad's stored 0x02B6 report for one gate.
//
// RecordSetKey is the value the reporting client itself used (the leading dword of
// its 0x02B6 body). It has to be echoed unchanged: the client keeps its records in
// a (dungeon -> key -> records) map and treats an unknown key by CREATING a record
// there, which then persists and re-enters the client's own later reports
// (_probe_634_report.md section 6c Q4).
type runtimeRaidMonsterRuntimeReport struct {
	RecordSetKey uint32
	Values       []uint32
}

// runtimeRaidGatePlacement is where one online member currently stands.
//
// It deliberately carries the gate and nothing else. The board's V0 is a property
// of the GATE, not of the run a member happens to be in (see
// runtimeRaidReviveCoinKey), so it is read from the raid-scoped ledger instead of
// being scraped off whichever run is open.
type runtimeRaidGatePlacement struct {
	DungeonID int
}

// collectRuntimeRaidGatePlacements reads every online member's current raid gate.
//
// The member session's own run binding is the only authority for "this member is
// inside this gate": freezeRuntimeRaidDungeonSelection is what writes it, and it
// is cleared with the run. A binding that names another raid or another phase is
// not this raid's gate and is skipped.
//
// Lock order: session.dungeon.mu is a leaf here -- the critical section reads two
// fields and takes no other lock, so it cannot invert the raid lifecycle's
// dungeon.mu -> raidMu order.
func (s *Service) collectRuntimeRaidGatePlacements(
	state runtimeRaidState,
) map[uint16]runtimeRaidGatePlacement {
	if s == nil || state.RaidKey == 0 {
		return nil
	}
	placements := make(map[uint16]runtimeRaidGatePlacement, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		var binding runtimeRaidDungeonBinding
		if runtime != nil {
			binding = runtime.raidDungeon
		}
		session.dungeon.mu.Unlock()
		if binding.RaidKey != state.RaidKey ||
			binding.Phase != state.Lifecycle.Phase ||
			binding.MapID == 0 {
			continue
		}
		placements[member.CharID] = runtimeRaidGatePlacement{
			DungeonID: binding.MapID,
		}
	}
	return placements
}

// runtimeRaidMonsterStateRowIdentity names one board row. It is the same identity
// the 0x0248 participation rows and the stored 0x02B6 reports use: one row per
// non-zero squad, and one row per unassigned member (group 0).
type runtimeRaidMonsterStateRowIdentity struct {
	Group      byte
	SoloCharID uint16
}

// runtimeRaidMonsterStateRows builds the 0x027A body rows for one raid.
//
// Only squads actually standing in a gate get a row: a squad with no gate has
// nothing for the board to draw, and because the client replaces its whole table,
// leaving such a squad out is exactly how its stale row is removed.
//
// ok is false when no squad is in a gate. Callers MUST NOT send that case: a
// group count of 0 clears the client's table rather than being a no-op
// (_probe_634_report.md section 6c Q1), so "no rows" has to mean "send nothing",
// not "send an empty body".
func (s *Service) runtimeRaidMonsterStateRows(
	state runtimeRaidState,
) ([]raid.RaidMonsterStateGroup, bool) {
	if s == nil || state.RaidKey == 0 {
		return nil, false
	}
	placements := s.collectRuntimeRaidGatePlacements(state)
	if len(placements) == 0 {
		return nil, false
	}
	reports := s.runtimeRaidMonsterRuntimeValuesFor(state.RaidKey)

	identities := runtimeRaidMonsterStateRowIdentities(state)
	rows := make([]raid.RaidMonsterStateGroup, 0, len(identities))
	for _, identity := range identities {
		members := runtimeRaidMonsterStateRowMembers(state, identity)
		if len(members) == 0 {
			continue
		}
		// The row's gate is the gate its members stand in. Members of one squad
		// share a run, so the first member that is placed decides it; a member who
		// is between scenes simply contributes nothing.
		dungeonID := 0
		for _, member := range members {
			if placement, placed := placements[member]; placed {
				dungeonID = placement.DungeonID
				break
			}
		}
		if dungeonID == 0 {
			continue
		}
		// V0 is the squad's spend in THIS gate after the coins INCREASE COIN has
		// covered are taken off, which is 86JP's GetAntonReportedUsedCoinCount
		// (Situation.cs:458-463). The client renders `its own gate allowance - V0`,
		// so subtracting the grant is the ONLY way a granted coin can reach the
		// panel: the allowance itself never travels on the wire.
		//
		// Both numbers come from the gate-scoped ledger rather than from the run the
		// squad happens to be in: the client's allowance belongs to the GATE, so a
		// per-run counter would zero itself every re-entry.
		coinKey := runtimeRaidReviveCoinKey{
			RaidKey:    state.RaidKey,
			Phase:      state.Lifecycle.Phase,
			Group:      identity.Group,
			SoloCharID: identity.SoloCharID,
			DungeonID:  dungeonID,
		}
		used := s.runtimeRaidReviveCoinUses(coinKey)
		granted := s.runtimeRaidReviveCoinGranted(coinKey)
		report, reported := runtimeRaidMonsterRuntimeReportFrom(reports, identity, dungeonID)
		row := raid.RaidMonsterStateGroup{
			PartyIndex: uint16(identity.Group),
			Members:    members,
			Value0:     runtimeRaidReportedUsedCoinCount(used, granted),
		}
		if reported {
			row.Value1 = report.RecordSetKey
			row.Values = report.Values
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

// runtimeRaidMonsterStateRowIdentities lists the board's rows in a stable order:
// squads ascending, then unassigned members by character id.
func runtimeRaidMonsterStateRowIdentities(
	state runtimeRaidState,
) []runtimeRaidMonsterStateRowIdentity {
	identities := make([]runtimeRaidMonsterStateRowIdentity, 0, len(state.FrozenMembers))
	seenSquads := make(map[byte]bool, runtimeRaidMaxGroups)
	for _, member := range state.FrozenMembers {
		group := runtimeRaidMemberSquad(member.GroupIndex)
		if group == 0 {
			identities = append(identities, runtimeRaidMonsterStateRowIdentity{SoloCharID: member.CharID})
			continue
		}
		if seenSquads[group] {
			continue
		}
		seenSquads[group] = true
		identities = append(identities, runtimeRaidMonsterStateRowIdentity{Group: group})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Group != identities[j].Group {
			return identities[i].Group < identities[j].Group
		}
		return identities[i].SoloCharID < identities[j].SoloCharID
	})
	return identities
}

// runtimeRaidMonsterStateRowMembers lists one row's member keys. An unassigned
// member is its own row so it never appears inside a real squad's row, which is
// the same rule the 0x0248 rows follow.
//
// The squad list comes from the LIVE labels for the reason
// runtimeRaidMembersInSquadLiveRows records: the client classifies this row by
// looking each listed member up in its own 0x024F roster, and that roster is built
// from the live rows -- a member listed here whose live label is 0 makes the whole
// row draw as 单人 with an unresolved portrait.
func runtimeRaidMonsterStateRowMembers(
	state runtimeRaidState,
	identity runtimeRaidMonsterStateRowIdentity,
) []uint16 {
	if identity.Group == 0 {
		return []uint16{identity.SoloCharID}
	}
	return runtimeRaidMembersInSquadLiveRows(state, identity.Group)
}

// runtimeRaidMonsterRuntimeReportFrom picks one row's report out of an already
// copied snapshot, so building rows never holds s.raidMu while it works.
func runtimeRaidMonsterRuntimeReportFrom(
	reports map[runtimeRaidMonsterRuntimeKey][]uint32,
	identity runtimeRaidMonsterStateRowIdentity,
	dungeonID int,
) (runtimeRaidMonsterRuntimeReport, bool) {
	var (
		best  runtimeRaidMonsterRuntimeReport
		found bool
	)
	for key, values := range reports {
		if key.Group != identity.Group ||
			key.SoloCharID != identity.SoloCharID ||
			key.DungeonID != dungeonID {
			continue
		}
		if found && key.RecordSetKey >= best.RecordSetKey {
			continue
		}
		best = runtimeRaidMonsterRuntimeReport{
			RecordSetKey: key.RecordSetKey,
			Values:       append([]uint32(nil), values...),
		}
		found = true
	}
	return best, found
}

// runtimeRaidMonsterStateBody encodes the 0x027A body for one raid.
//
// ok is false when the raid has no squad in a gate, and that case must NOT be sent:
// see runtimeRaidMonsterStateRows.
func (s *Service) runtimeRaidMonsterStateBody(state runtimeRaidState) ([]byte, bool, error) {
	rows, ok := s.runtimeRaidMonsterStateRows(state)
	if !ok {
		return nil, false, nil
	}
	body, err := raid.BuildRaidMonsterStateBody(rows)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

// sendRuntimeRaidMonsterState answers one session's board refresh.
//
// The 0x0248 participation rows come FIRST by contract: the client resolves each
// row's dungeon through them, and drops the row's values when the lookup fails.
// The client asks for this refresh on a 7-15s timer, so a raid with nobody in a
// gate must answer with nothing at all rather than an empty table.
func (s *Service) sendRuntimeRaidMonsterState(session *gameSession, state runtimeRaidState) error {
	if s == nil || session == nil {
		return nil
	}
	body, ok, err := s.runtimeRaidMonsterStateBody(state)
	if err != nil || !ok {
		return err
	}
	return s.sendGameUpperRawClassCodec(session, raid.RaidMonsterStateMsgID, body, 0, true)
}

// broadcastRuntimeRaidMonsterState pushes the board's rows to every member of the
// raid. Every member gets the same body, because the client's table is global to
// the raid, not per squad.
func (s *Service) broadcastRuntimeRaidMonsterState(state runtimeRaidState) error {
	if s == nil {
		return nil
	}
	body, ok, err := s.runtimeRaidMonsterStateBody(state)
	if err != nil || !ok {
		return err
	}
	return s.broadcastRuntimeRaidWireSequence(state, runtimeRaidWireNotification{
		msgID: raid.RaidMonsterStateMsgID,
		body:  body,
	})
}

// scheduleRuntimeRaidMonsterStateBroadcast republishes the board from the gameplay
// timer queue instead of from the caller's own goroutine.
//
// The caller is the op41 use-coin path, which holds the spending member's
// session.dungeon.mu. Republishing inline would broadcast to every member, and the
// body's build reads each member's session lock to find out which gate that member
// stands in (collectRuntimeRaidGatePlacements), so two members spending a coin at
// the same instant would each wait for the other while holding their own lock --
// the live 2026-09-20 08:32:02Z deadlock, and the same reason
// scheduleRuntimeRaidForcedGroupReturn exists. Running the task on the queue means
// it starts with no session lock held.
//
// The raid state is looked up fresh inside the task rather than passed in, so the
// body reflects the ledger at the moment it is actually built: a spend or a gate
// clear that lands in between is included instead of being missed, and a raid that
// has already ended simply publishes nothing.
//
// A bare goroutine is the fallback when the queue is unavailable or refuses the
// task. Dropping the publish would freeze the 战况表 at its previous number -- the
// 2026-09-23 report was exactly that ("一直是10/12") -- so losing the refresh is
// worse than running it off-queue.
//
// A second spend cancels the first pending task of the same name (the queue
// replaces by name) and publishes once instead, which is what we want: the
// surviving task rebuilds the body from the ledger, so it carries both spends.
func (s *Service) scheduleRuntimeRaidMonsterStateBroadcast(raidKey uint32, source string) {
	if s == nil || raidKey == 0 {
		return
	}
	run := func(time.Time) {
		s.raidMu.Lock()
		current := s.raids[raidKey]
		if current == nil {
			s.raidMu.Unlock()
			return
		}
		state := cloneRuntimeRaidState(*current)
		s.raidMu.Unlock()
		if err := s.broadcastRuntimeRaidMonsterState(state); err != nil {
			s.logPacketEvent("game-raid-monster-state-broadcast-failed",
				"raid_key", raidKey, "source", source, "error", err)
		}
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		go run(time.Time{})
		return
	}
	name := fmt.Sprintf("raid-monster-state:%d:%s", raidKey, source)
	if err := queue.ScheduleAfter(name, 0, run); err != nil {
		s.logPacketEvent("game-raid-monster-state-schedule-failed",
			"raid_key", raidKey, "source", source, "error", err)
		go run(time.Time{})
	}
}

// runtimeRaidMonsterStateRowIdentityForMember names the board row a member belongs
// to: its squad's row, or its own row when it is unassigned.
func runtimeRaidMonsterStateRowIdentityForMember(
	state runtimeRaidState,
	memberKey uint16,
) (runtimeRaidMonsterStateRowIdentity, bool) {
	member, ok := runtimeRaidFrozenMember(state, memberKey)
	if !ok {
		return runtimeRaidMonsterStateRowIdentity{}, false
	}
	group := runtimeRaidMemberSquad(member.GroupIndex)
	if group == 0 {
		return runtimeRaidMonsterStateRowIdentity{SoloCharID: memberKey}, true
	}
	return runtimeRaidMonsterStateRowIdentity{Group: group}, true
}

// runtimeRaidReportedUsedCoinCount is the number 634 carries for a row: the spend
// that the squad's own grant does not cover.
//
// It is 86JP's GetAntonReportedUsedCoinCount (RaidHandler.Situation.cs:458-463)
// verbatim, clamp included: the client subtracts this from its own per-gate
// allowance, so a spend already covered by a granted coin must not be reported
// twice, and the value can never go negative.
func runtimeRaidReportedUsedCoinCount(used, granted uint32) uint32 {
	if used > granted {
		return used - granted
	}
	return 0
}

// runtimeRaidReviveCoinKey names one row's revive-coin ledger entry: the 634 row
// identity, the phase, and the gate.
//
// Phase and gate are both in the key on purpose, and both follow 86JP's own
// lifetime for this bookkeeping rather than a reset hook we would have to remember
// to call:
//
//   - The gate is in the key because the allowance the client subtracts from is the
//     GATE's own. 86JP keeps UsedCoinCount/GrantedCoinCount on
//     RaidDungeonParticipation, which is per (dungeon, party) and is NOT reset by
//     re-entering the gate. The counter this replaced lived on runtimeDungeonState,
//     which is allocated per RUN, so every re-entry silently zeroed it: live
//     2026-09-23, a squad spent two coins in 黑雾之源 (10/12), re-entered the same
//     gate, and the board went back to 12/12 -- the operator's report was
//     "当前可用复活币好像不会跌破10…一直是10/12".
//   - The phase is in the key because 86JP starts every phase with fresh
//     participation records, so a new phase starts with nothing spent.
type runtimeRaidReviveCoinKey struct {
	RaidKey    uint32
	Phase      byte
	Group      byte
	SoloCharID uint16
	DungeonID  int
}

// runtimeRaidReviveCoinUses is how many coins this gate has actually consumed for
// this row. It accumulates across re-entries within one phase and is dropped when
// the gate's own clear quota is met (see clearRuntimeRaidReviveCoinLedgerForGate).
func (s *Service) runtimeRaidReviveCoinUses(key runtimeRaidReviveCoinKey) uint32 {
	if s == nil || key.RaidKey == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	return s.raidReviveUses[key]
}

// runtimeRaidReviveCoinGranted is how many revive coins this row's INCREASE COIN
// grants currently cover.
func (s *Service) runtimeRaidReviveCoinGranted(key runtimeRaidReviveCoinKey) uint32 {
	if s == nil || key.RaidKey == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	return s.raidReviveGrants[key]
}

// recordRuntimeRaidReviveCoinUseLocked adds one consumed coin to a gate's ledger
// and returns the new total.
//
// The caller holds session.dungeon.mu (the op41 commit path does) and this takes
// s.raidMu, which is the order the raid lifecycle already uses -- the inverse is
// what would deadlock.
func (s *Service) recordRuntimeRaidReviveCoinUseLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
) (runtimeRaidReviveCoinKey, uint32, bool) {
	if s == nil || session == nil || runtime == nil {
		return runtimeRaidReviveCoinKey{}, 0, false
	}
	binding := runtime.raidDungeon
	if binding.RaidKey == 0 || binding.MapID == 0 {
		return runtimeRaidReviveCoinKey{}, 0, false
	}
	key := runtimeRaidReviveCoinKey{
		RaidKey:   binding.RaidKey,
		Phase:     binding.Phase,
		DungeonID: binding.MapID,
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[binding.RaidKey]
	if state == nil || state.Lifecycle.Phase != binding.Phase {
		return runtimeRaidReviveCoinKey{}, 0, false
	}
	member, ok := runtimeRaidFrozenMember(*state, session.selectedCharacterID)
	if !ok {
		return runtimeRaidReviveCoinKey{}, 0, false
	}
	key.Group = runtimeRaidMemberSquad(member.GroupIndex)
	if key.Group == 0 {
		key.SoloCharID = session.selectedCharacterID
	}
	if s.raidReviveUses == nil {
		s.raidReviveUses = make(map[runtimeRaidReviveCoinKey]uint32)
	}
	total := s.raidReviveUses[key] + 1
	s.raidReviveUses[key] = total
	return key, total, true
}

// grantRuntimeRaidReviveCoins adds to a row's grant ledger and returns the new
// total. Like 86JP's GrantedCoinCount it lives as long as the gate's own entry.
func (s *Service) grantRuntimeRaidReviveCoins(key runtimeRaidReviveCoinKey, added uint32) uint32 {
	if s == nil || key.RaidKey == 0 || added == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	if s.raidReviveGrants == nil {
		s.raidReviveGrants = make(map[runtimeRaidReviveCoinKey]uint32)
	}
	total := s.raidReviveGrants[key] + added
	s.raidReviveGrants[key] = total
	return total
}

// clearRuntimeRaidReviveCoinLedgerForGateLocked drops one gate's spent/granted
// counters for every squad.
//
// It is 86JP's participation lifetime: its UsedCoinCount/GrantedCoinCount live on
// the RaidDungeonParticipation for that (dungeon, party), and a cleared dungeon ends
// that record. Our "cleared" is the objective's own quota being met
// (`Objectives[mapID].RemainingHP == 0`), which is what the caller checks.
//
// Caller holds s.raidMu. It touches only Service maps, so it can run on the clear
// path -- which holds the completing member's session lock -- without taking any
// session lock, which is the rule that path must obey.
//
// It returns how many ledger entries it dropped, so the caller can log the reset
// only when there was something to drop.
func (s *Service) clearRuntimeRaidReviveCoinLedgerForGateLocked(raidKey uint32, phase byte, dungeonID int) int {
	if s == nil || raidKey == 0 || dungeonID == 0 {
		return 0
	}
	dropped := 0
	for key := range s.raidReviveUses {
		if key.RaidKey == raidKey && key.Phase == phase && key.DungeonID == dungeonID {
			delete(s.raidReviveUses, key)
			dropped++
		}
	}
	for key := range s.raidReviveGrants {
		if key.RaidKey == raidKey && key.Phase == phase && key.DungeonID == dungeonID {
			delete(s.raidReviveGrants, key)
			dropped++
		}
	}
	return dropped
}

// purgeRuntimeRaidReviveCoinLedgerLocked drops every revive-coin counter of one
// raid. Caller holds s.raidMu.
func (s *Service) purgeRuntimeRaidReviveCoinLedgerLocked(raidKey uint32) {
	if s == nil || raidKey == 0 {
		return
	}
	for key := range s.raidReviveUses {
		if key.RaidKey == raidKey {
			delete(s.raidReviveUses, key)
		}
	}
	for key := range s.raidReviveGrants {
		if key.RaidKey == raidKey {
			delete(s.raidReviveGrants, key)
		}
	}
}
