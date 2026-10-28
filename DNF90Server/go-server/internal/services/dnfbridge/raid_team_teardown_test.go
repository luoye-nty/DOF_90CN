package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// TestRuntimeRaidPhaseExpiryReleasesEveryAttackPartyMember covers the attack
// party path of publishExpiredRuntimeRaid. expireRuntimeRaidPhase deletes the
// authoritative raid, so a twenty-member roster that is never told keeps its
// raid window, rejoin projection and ordinary-party projection forever. Only
// single raids used to be published here.
func TestRuntimeRaidPhaseExpiryReleasesEveryAttackPartyMember(t *testing.T) {
	channel := channelcatalog.Channel{ID: 200, Type: 23}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	now := time.Date(2026, 9, 8, 6, 0, 0, 0, time.UTC)

	members := make([]runtimeRaidMemberState, 0, raid.MaxAttackPartyMembers)
	sessions := make(map[uint16]*gameSession, raid.MaxAttackPartyMembers)
	for i := 0; i < raid.MaxAttackPartyMembers; i++ {
		charID := uint16(1001 + i)
		members = append(members, runtimeRaidMemberState{
			CharID:     charID,
			GroupIndex: byte(i/runtimeRaidGroupSize) + 1,
			SlotOrder:  byte(i % runtimeRaidGroupSize),
		})
		session := &gameSession{channel: channel, conn: &bufferConn{}}
		service.bindGameSessionCharacter(session, charID)
		sessions[charID] = session
	}

	const raidKey = uint32(0x00c80401)
	lifecycle := newRuntimeRaidLifecycleState(now.Add(-90 * time.Minute))
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.PhaseDeadline = now.Add(-time.Minute)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      members[0].CharID,
		TailFlag:      5,
		Started:       true,
		EntryCostPaid: true,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle:     lifecycle,
	}

	expired, err := service.expireRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, now)
	if err != nil {
		t.Fatalf("expire attack-party phase: %v", err)
	}
	if expired.TailFlag == 1 {
		t.Fatalf("fixture is not an attack party: tail_flag=%d", expired.TailFlag)
	}
	if err := service.publishExpiredRuntimeRaid(expired); err != nil {
		t.Fatalf("publish expired attack party: %v", err)
	}

	for _, member := range members {
		session := sessions[member.CharID]
		raw := session.conn.(*bufferConn).write.Bytes()
		if len(raw) == 0 {
			t.Fatalf("member %d was never released after the phase expired", member.CharID)
		}
		packet, _ := splitGameServerUpperPacket(t, raw)
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) {
			t.Fatalf("member %d terminal msg_id=%d want=%d",
				member.CharID, packet.Header.MsgID, uint16(dnfenum.CmdPacketLeaveRaid))
		}
		if !runtimeRaidEndedProjectionMatches(session, uint16(expired.RaidKey)) {
			t.Fatalf("member %d kept a live raid projection after expiry", member.CharID)
		}
	}
}

// A started team raid must still accept a leave.
//
// Live incident 2026-09-16 17:39: the raid leader picked 退出攻坚队 (C2S 665 with
// its own character id) and the server answered a refusal, because
// removeRuntimeRaidMemberLocked bailed out whenever StartPending/Started was set.
// The client had already closed its raid window, so the player sat inside a raid
// they could not leave and could not create another one beside -- the old object
// stayed on the roster listing them as its leader.
//
// The leave itself has no state gate in the 86JP reference either:
// RaidManager.LeaveLocked removes the member unconditionally.
func TestRuntimeRaidStartedTeamLeaveIsAccepted(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	fixture.service.raids[fixture.raidKey].Started = true
	fixture.service.raids[fixture.raidKey].StartPending = true

	// The leader leaves and the raid survives under the next member in roster
	// order, so the departing leader is free to create another one.
	remaining, removed, disbanded := fixture.service.removeRuntimeRaidMember(1001)
	if !removed || disbanded {
		t.Fatalf("started raid leader leave removed=%t disbanded=%t", removed, disbanded)
	}
	if remaining.LeaderID != 1002 || runtimeRaidMemberIndex(remaining, 1001) >= 0 {
		t.Fatalf("leader leave left the roster wrong: %+v", remaining)
	}

	// The last member leaving empties and disbands the raid.
	_, removed, disbanded = fixture.service.removeRuntimeRaidMember(1002)
	if !removed || !disbanded {
		t.Fatalf("last member leave removed=%t disbanded=%t", removed, disbanded)
	}
	if len(fixture.service.raids) != 0 {
		t.Fatalf("the started raid survived its last member's leave: %+v", fixture.service.raids)
	}
}

// TestRuntimeRaidManagerMoveRejectsFullSubgroup covers the native
// runtimeRaidMaxGroups x runtimeRaidGroupSize roster grid. Without the capacity
// guard the slot allocator fell back to the last slot and put two members on
// one grid cell.
func TestRuntimeRaidManagerMoveRejectsFullSubgroup(t *testing.T) {
	channel := channelcatalog.Channel{ID: 200, Type: 23}
	service := &Service{raids: make(map[uint32]*runtimeRaidState)}
	const raidKey = uint32(0x00c80402)

	members := []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, GroupIndex: 1, SlotOrder: 1},
		{CharID: 1003, GroupIndex: 1, SlotOrder: 2},
		{CharID: 1004, GroupIndex: 1, SlotOrder: 3},
		{CharID: 1005, GroupIndex: 2, SlotOrder: 0},
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:  raidKey,
		LeaderID: 1001,
		TailFlag: 5,
		Members:  append([]runtimeRaidMemberState(nil), members...),
	}
	leader := &gameSession{channel: channel, conn: &bufferConn{}}
	service.bindGameSessionCharacter(leader, 1001)

	if _, ok := service.moveRuntimeRaidMember(1001, 1005, 1); ok {
		t.Fatal("move into a full subgroup was accepted")
	}
	state := service.raids[raidKey]
	index := runtimeRaidMemberIndex(*state, 1005)
	if index < 0 || state.Members[index].GroupIndex != 2 || state.Members[index].SlotOrder != 0 {
		t.Fatalf("rejected move mutated the roster: %+v", state.Members[index])
	}
	assertRuntimeRaidGridUnique(t, *state)

	// Re-slotting inside the member's own subgroup must stay available: the
	// mover never counts against its own capacity.
	if _, ok := service.moveRuntimeRaidMember(1001, 1004, 1); !ok {
		t.Fatal("re-slotting inside the current subgroup was rejected")
	}
	assertRuntimeRaidGridUnique(t, *service.raids[raidKey])

	// A subgroup with a free slot still accepts the move.
	if _, ok := service.moveRuntimeRaidMember(1001, 1003, 2); !ok {
		t.Fatal("move into a subgroup with a free slot was rejected")
	}
	moved := service.raids[raidKey]
	movedIndex := runtimeRaidMemberIndex(*moved, 1003)
	if movedIndex < 0 || moved.Members[movedIndex].GroupIndex != 2 {
		t.Fatalf("accepted move did not land in subgroup 2: %+v", moved.Members[movedIndex])
	}
	assertRuntimeRaidGridUnique(t, *moved)
}

func assertRuntimeRaidGridUnique(t *testing.T, state runtimeRaidState) {
	t.Helper()
	type cell struct {
		group byte
		slot  byte
	}
	seen := make(map[cell]uint16, len(state.Members))
	for _, member := range state.Members {
		// Standby members (group 0) share the single "no squad" cell by design;
		// only a real squad seat has to be unique.
		if member.GroupIndex == 0 {
			continue
		}
		key := cell{group: member.GroupIndex, slot: member.SlotOrder}
		if other, clash := seen[key]; clash {
			t.Fatalf("members %d and %d share roster cell group=%d slot=%d",
				other, member.CharID, key.group, key.slot)
		}
		seen[key] = member.CharID
	}
}
