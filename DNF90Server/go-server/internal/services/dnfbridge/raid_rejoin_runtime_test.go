package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

// A joiner lands in standby (group 0) rather than being slotted into the first
// squad with a free seat. The client stores the roster's squad byte verbatim, so
// zero is what makes the member show as unassigned until the leader moves it.
func TestRuntimeRaidRejoinLeavesTheJoinerUnassigned(t *testing.T) {
	state := runtimeRaidState{
		RaidKey:  0x00c803e9,
		LeaderID: 1001,
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1},
			{CharID: 1003, GroupIndex: 1, SlotOrder: 2},
			{CharID: 1004, GroupIndex: 1, SlotOrder: 3},
			{CharID: 1005, GroupIndex: 2, SlotOrder: 1},
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{state.RaidKey: &state}}

	joined, ok := service.rejoinRuntimeRaid(state.RaidKey, 2000)
	if !ok {
		t.Fatal("rejoin rejected an available raid position")
	}
	index := runtimeRaidMemberIndex(joined, 2000)
	if index < 0 {
		t.Fatalf("joined member missing from roster: %+v", joined.Members)
	}
	if member := joined.Members[index]; member.GroupIndex != 0 || member.SlotOrder != 0 {
		t.Fatalf("joined position group=%d slot=%d want group=0 slot=0 (standby)", member.GroupIndex, member.SlotOrder)
	}
	// Standby must not consume a squad seat: squad 2 still has three free slots
	// after the join, so the leader can move someone there.
	if count := runtimeRaidGroupCountExcluding(joined, 2, 0); count != 1 {
		t.Fatalf("squad 2 occupancy=%d want 1 (only 1005)", count)
	}
	if !runtimeRaidGroupsEditable(joined) {
		t.Fatal("fixture raid is not in an editable state")
	}
	moved, ok := service.moveRuntimeRaidMember(1001, 2000, 2)
	if !ok {
		t.Fatal("leader could not move the standby member into squad 2")
	}
	member := moved.Members[runtimeRaidMemberIndex(moved, 2000)]
	if member.GroupIndex != 2 {
		t.Fatalf("after the move group=%d want 2", member.GroupIndex)
	}
}

func TestRuntimeRaidRejoinRejectsFullAndCrossRaidMembership(t *testing.T) {
	t.Run("full", func(t *testing.T) {
		const raidKey = uint32(0x00c803e9)
		members := make([]runtimeRaidMemberState, 0, raid.MaxAttackPartyMembers)
		// Fill the wire-level member cap inside a valid group grid. The grid is
		// wider than the cap on purpose (runtimeRaidMaxGroups groups of
		// runtimeRaidGroupSize), so the roster is built from the cap, not from
		// the grid size.
		for len(members) < raid.MaxAttackPartyMembers {
			members = append(members, runtimeRaidMemberState{
				CharID:     uint16(len(members) + 1),
				GroupIndex: byte(len(members)/runtimeRaidGroupSize + 1),
				SlotOrder:  byte(len(members) % runtimeRaidGroupSize),
			})
		}
		state := runtimeRaidState{RaidKey: raidKey, LeaderID: 1, Members: members}
		service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: &state}}

		if _, ok := service.rejoinRuntimeRaid(raidKey, 3000); ok {
			t.Fatal("full 20-member raid accepted another member")
		}
		if got := len(service.raids[raidKey].Members); got != raid.MaxAttackPartyMembers {
			t.Fatalf("full raid roster changed: members=%d", got)
		}
	})

	t.Run("cross_raid", func(t *testing.T) {
		const firstKey = uint32(0x00c803e9)
		const secondKey = uint32(0x00c803ea)
		first := runtimeRaidState{
			RaidKey:  firstKey,
			LeaderID: 1001,
			Members:  []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
		}
		second := runtimeRaidState{
			RaidKey:  secondKey,
			LeaderID: 2001,
			Members:  []runtimeRaidMemberState{{CharID: 2001, GroupIndex: 1, SlotOrder: 0}},
		}
		service := &Service{raids: map[uint32]*runtimeRaidState{firstKey: &first, secondKey: &second}}

		if _, ok := service.rejoinRuntimeRaid(secondKey, 1001); ok {
			t.Fatal("member already owned by another raid joined a second raid")
		}
		if runtimeRaidMemberIndex(*service.raids[secondKey], 1001) >= 0 {
			t.Fatal("cross-raid rejection still mutated the target roster")
		}
	})
}

func TestRuntimeRaidRejoinRestoresExactLifecyclePacketSequence(t *testing.T) {
	now := time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)
	rewardBody := []byte{0x72, 0x61, 0x69, 0x64}
	tests := []struct {
		name              string
		configure         func(*runtimeRaidState)
		lifecyclePackets  func(runtimeRaidState) []runtimeRaidRejoinExpectedPacket
		wantScheduleName  string
		wantScheduleDelay time.Duration
	}{
		{
			name: "preparing",
			configure: func(state *runtimeRaidState) {
				state.Lifecycle.Status = runtimeRaidLifecyclePreparing
				state.Lifecycle.ReadyDeadline = now.Add(3 * time.Second)
				state.Lifecycle.PhaseStartedAt = time.Time{}
			},
			lifecyclePackets: func(state runtimeRaidState) []runtimeRaidRejoinExpectedPacket {
				seconds := uint32(3)
				return []runtimeRaidRejoinExpectedPacket{
					{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(raid.RaidTimer{
						TimerType: 0,
						Value0:    runtimeRaidTopTimerKey,
						Value1:    uint32(state.Lifecycle.ReadyDeadline.Unix()),
						Value2:    seconds,
					})},
					{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: seconds})},
				}
			},
			wantScheduleName:  "raid-ready:13108201:7",
			wantScheduleDelay: 3 * time.Second,
		},
		{
			name: "phase_active",
			configure: func(state *runtimeRaidState) {
				state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
				state.Lifecycle.ReadyDeadline = time.Time{}
				state.Lifecycle.PhaseStartedAt = now.Add(-20 * time.Second)
				state.Lifecycle.PhaseDeadline = now.Add(120 * time.Second)
			},
			lifecyclePackets: func(state runtimeRaidState) []runtimeRaidRejoinExpectedPacket {
				symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(state)
				if err != nil {
					t.Fatalf("phase snapshot bodies: %v", err)
				}
				return []runtimeRaidRejoinExpectedPacket{
					{msgID: raid.RaidSymbolMsgID, body: symbolBody},
					{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
					{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidPhaseStartState, Argument: 0})},
					{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(raid.RaidTimer{
						TimerType: 0,
						Value0:    runtimeRaidTopTimerKey,
						Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
						Value2:    140,
					})},
					{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: 120})},
				}
			},
			wantScheduleName:  "raid-phase:13108201:1:7",
			wantScheduleDelay: 120 * time.Second,
		},
		{
			name: "awaiting_movie",
			configure: func(state *runtimeRaidState) {
				state.Lifecycle.Status = runtimeRaidLifecycleAwaitingMovie
				state.Lifecycle.ReadyDeadline = time.Time{}
				state.Lifecycle.PhaseStartedAt = now.Add(-50 * time.Second)
				state.Lifecycle.PhaseDeadline = now.Add(100 * time.Second)
				state.Lifecycle.MovieOpenedAt = now.Add(-10 * time.Second)
			},
			lifecyclePackets: func(runtimeRaidState) []runtimeRaidRejoinExpectedPacket {
				resultBody, err := raid.BuildRaidSettlementResultBody(raid.RaidSettlementResult{
					ResultType:   0,
					PhaseIndex:   0,
					ClearSeconds: 40,
				})
				if err != nil {
					t.Fatalf("settlement result body: %v", err)
				}
				return []runtimeRaidRejoinExpectedPacket{
					{msgID: raid.RaidResultMsgID, body: resultBody},
					{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateResult, Argument: 0})},
				}
			},
		},
		{
			name: "awaiting_rewards",
			configure: func(state *runtimeRaidState) {
				state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
				state.Lifecycle.ReadyDeadline = time.Time{}
				state.Lifecycle.RewardDelivery = &runtimeRaidRewardDeliveryCache{
					Phase:      runtimeRaidDungeonPhaseOne,
					Token:      11,
					RewardBody: append([]byte(nil), rewardBody...),
				}
				state.Lifecycle.RewardDeliveryCommitted = true
			},
			lifecyclePackets: func(runtimeRaidState) []runtimeRaidRejoinExpectedPacket {
				return []runtimeRaidRejoinExpectedPacket{
					{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateCardsOpened, Argument: 0})},
					{msgID: raid.RaidRewardListMsgID, body: rewardBody},
				}
			},
		},
		{
			name: "standby",
			configure: func(state *runtimeRaidState) {
				state.Lifecycle.Status = runtimeRaidLifecycleStandby
				state.Lifecycle.ReadyDeadline = time.Time{}
				state.Lifecycle.StandbyStartedAt = now.Add(-10 * time.Second)
				state.Lifecycle.StandbyDeadline = now.Add(90 * time.Second)
				state.Lifecycle.RewardDelivery = &runtimeRaidRewardDeliveryCache{
					Phase:      runtimeRaidDungeonPhaseOne,
					Token:      12,
					RewardBody: append([]byte(nil), rewardBody...),
				}
				state.Lifecycle.RewardDeliveryCommitted = true
			},
			lifecyclePackets: func(state runtimeRaidState) []runtimeRaidRejoinExpectedPacket {
				seconds := uint32(90)
				return []runtimeRaidRejoinExpectedPacket{
					{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidSettlementStateCardsOpened, Argument: 0})},
					{msgID: raid.RaidRewardListMsgID, body: rewardBody},
					{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(raid.RaidTimer{
						TimerType: 1,
						Value0:    state.RaidKey,
						Value1:    uint32(state.Lifecycle.StandbyDeadline.Unix()),
						Value2:    seconds,
					})},
					{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 1, Seconds: seconds})},
				}
			},
			wantScheduleName:  "raid-standby:13108201:7",
			wantScheduleDelay: 90 * time.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, session, connection, otherConnection, queue, state := newRuntimeRaidRejoinTestService(t, now)
			test.configure(state)
			request := make([]byte, 4)
			binary.LittleEndian.PutUint32(request, state.RaidKey)

			if err := service.handleOnlineRejoinRaid(session, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
				t.Fatalf("handle rejoin: %v", err)
			}
			if otherConnection.write.Len() != 0 {
				t.Fatalf("rejoin projected %d bytes to another session", otherConnection.write.Len())
			}

			want := runtimeRaidRejoinBaseExpectedPackets(t, service, *state)
			want = append(want, test.lifecyclePackets(*state)...)
			got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
			assertRuntimeRaidRejoinPackets(t, got, want)

			partyState := runtimePartyStateSnapshot(session)
			if len(partyState.Members) != 2 ||
				partyState.Members[0].UserID != 1001 || partyState.Members[1].UserID != 1002 {
				t.Fatalf("rejoin restored wrong group party: %+v", partyState)
			}
			if test.wantScheduleName == "" {
				if queue.scheduleCalls != 0 {
					t.Fatalf("unexpected lifecycle schedule: calls=%d name=%q", queue.scheduleCalls, queue.scheduledName)
				}
			} else if queue.scheduleCalls != 1 || queue.scheduledName != test.wantScheduleName || queue.scheduledDelay != test.wantScheduleDelay {
				t.Fatalf("lifecycle schedule calls=%d name=%q delay=%s want name=%q delay=%s",
					queue.scheduleCalls, queue.scheduledName, queue.scheduledDelay, test.wantScheduleName, test.wantScheduleDelay)
			}
		})
	}
}

func TestRuntimeRaidRejoinStopsWhenCharacterGenerationChanges(t *testing.T) {
	now := time.Date(2026, 8, 6, 5, 0, 0, 0, time.UTC)
	service, session, _, otherConnection, queue, state := newRuntimeRaidRejoinTestService(t, now)
	connection := &runtimeRaidRejoinGenerationConn{}
	session.conn = connection
	connection.afterFirstWrite = func() {
		service.bindGameSessionCharacter(session, 9000)
	}
	request := make([]byte, 4)
	binary.LittleEndian.PutUint32(request, state.RaidKey)

	if err := service.handleOnlineRejoinRaid(session, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
		t.Fatalf("generation-changing rejoin: %v", err)
	}
	got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
	want := []runtimeRaidRejoinExpectedPacket{{
		msgID:          uint16(dnfenum.CmdPacketRejoinRaid),
		classification: dnfproto.DefaultChannelClassification,
		body:           raid.BuildRaidCommandSuccessBody(),
	}}
	assertRuntimeRaidRejoinPackets(t, got, want)
	if otherConnection.write.Len() != 0 {
		t.Fatalf("generation-changing rejoin wrote %d bytes to another session", otherConnection.write.Len())
	}
	if queue.scheduleCalls != 0 {
		t.Fatalf("stale rejoin scheduled lifecycle callback: calls=%d", queue.scheduleCalls)
	}
}

func TestRuntimeRaidSceneRejoinRestoresCreatedRaidAtPostSelectBoundaryOnce(t *testing.T) {
	const raidKey = uint32(0x00170001)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                   connection,
		connID:                 "raid-scene-rejoin-created",
		channel:                channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType},
		sceneBootstrapTailSent: true,
	}
	service.bindGameSessionCharacter(session, 1001)
	state := &runtimeRaidState{
		RaidKey:         raidKey,
		LeaderID:        1001,
		RouteOrRaidType: 0,
		NameBytes:       []byte("single-anton"),
		TailFlag:        1,
		SingleRaidType:  0,
		Members: []runtimeRaidMemberState{{
			CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0,
			UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecycleCreated,
			Revision: 3,
		},
	}
	service.raids[raidKey] = state
	staleParty := runtimeRaidGroupPartyState(*state, state.Members)
	storeRuntimePartyState(session, staleParty)
	if managed, restored := service.bootstrapManagedRuntimePartyForSession(session); !restored || len(managed.Members) != 1 {
		t.Fatalf("seed stale single-member party: restored=%t party=%+v", restored, managed)
	}
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgFollowUpStatus),
		nil,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build Cera request: %v", err)
	}

	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("post-select Cera boundary: %v", err)
	}
	want := runtimeRaidRejoinBaseExpectedPackets(t, service, *state)[1:]
	got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
	assertRuntimeRaidRejoinPackets(t, got, want)
	if !session.raidRejoinProjectionSent ||
		session.raidRejoinProjectionRaidKey != raidKey ||
		session.raidRejoinProjectionRevision != state.Lifecycle.Revision {
		t.Fatalf("scene rejoin cursor not committed: sent=%t key=%#x revision=%d",
			session.raidRejoinProjectionSent,
			session.raidRejoinProjectionRaidKey,
			session.raidRejoinProjectionRevision)
	}
	if !session.raidDirectoryReadySent {
		t.Fatal("raid directory readiness did not follow scene rejoin recovery")
	}
	partyState := runtimePartyStateSnapshot(session)
	if partyState.PartyID > 0 || len(partyState.Members) != 0 {
		t.Fatalf("unstarted single raid retained ordinary party projection: %+v", partyState)
	}
	if managed, found := service.runtimePartyManagerForService().SnapshotByUser(1001, session.characterGeneration); found {
		t.Fatalf("unstarted single raid retained central party membership: party=%+v", managed)
	}

	connection.write.Reset()
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("repeated post-select Cera boundary: %v", err)
	}
	if got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes()); len(got) != 0 {
		t.Fatalf("repeated boundary replayed raid packets: %v", runtimeRaidRejoinPacketIDs(got))
	}
}

func TestRuntimeRaidSceneRejoinPromotesReconnectSessionBeforeRecovery(t *testing.T) {
	const raidKey = uint32(0x00170001)
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:         make(map[uint32]*runtimeRaidState),
		onlinePlayers: newOnlinePlayerManager(),
	}
	resident := &gameSession{
		conn:    &bufferConn{},
		connID:  "raid-rejoin-resident",
		channel: channelcatalog.Channel{ID: 151, Type: 11},
	}
	service.bindGameSessionCharacter(resident, 1001)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1001, Session: resident})

	connection := &bufferConn{}
	replacement := &gameSession{
		conn:                   connection,
		connID:                 "raid-rejoin-replacement",
		channel:                channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType},
		channelReconnect:       true,
		sceneBootstrapTailSent: true,
	}
	service.bindGameSessionCharacter(replacement, 1001)
	if _, current := service.boundGameSessionCharacterSnapshot(replacement); current {
		t.Fatal("replacement unexpectedly became canonical before post-select boundary")
	}
	state := &runtimeRaidState{
		RaidKey:         raidKey,
		LeaderID:        1001,
		RouteOrRaidType: 0,
		NameBytes:       []byte("single-anton"),
		TailFlag:        1,
		SingleRaidType:  0,
		Members: []runtimeRaidMemberState{{
			CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0,
			UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
		Lifecycle: runtimeRaidLifecycleState{Status: runtimeRaidLifecycleCreated, Revision: 3},
	}
	service.raids[raidKey] = state
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgFollowUpStatus),
		nil,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(replacement, request); err != nil {
		t.Fatalf("post-select Cera boundary: %v", err)
	}
	if identity, current := service.boundGameSessionCharacterSnapshot(replacement); !current || identity.character != 1001 {
		t.Fatalf("replacement was not promoted before recovery: current=%t identity=%+v", current, identity)
	}
	if !replacement.raidRejoinProjectionSent {
		t.Fatal("promoted replacement did not receive unfinished raid recovery")
	}
	if got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes()); len(got) == 0 {
		t.Fatal("promoted replacement received no raid packets")
	}
	if resident.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("raid recovery was written to the retired resident socket")
	}
}

func TestRuntimeRaidSceneRejoinRestoresActiveProgressAtPostSelectBoundary(t *testing.T) {
	now := time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
	service, session, connection, otherConnection, queue, state := newRuntimeRaidRejoinTestService(t, now)
	session.sceneBootstrapTailSent = true
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.ReadyDeadline = time.Time{}
	state.Lifecycle.PhaseStartedAt = now.Add(-30 * time.Second)
	state.Lifecycle.PhaseDeadline = now.Add(90 * time.Second)
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.UpperMsgFollowUpStatus),
		nil,
		1,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build Cera request: %v", err)
	}

	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("active progress post-select boundary: %v", err)
	}
	symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(*state)
	if err != nil {
		t.Fatalf("phase snapshot bodies: %v", err)
	}
	want := runtimeRaidRejoinBaseExpectedPackets(t, service, *state)[1:]
	want = append(want,
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidSymbolMsgID, body: symbolBody},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidPhaseStartState, Argument: 0})},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidTimerMsgID, body: raid.BuildRaidTimerBody(raid.RaidTimer{
			TimerType: 0,
			Value0:    runtimeRaidTopTimerKey,
			Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
			Value2:    120,
		})},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
			TimerType: 0,
			Seconds:   90,
		})},
	)
	got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
	assertRuntimeRaidRejoinPackets(t, got, want)
	if otherConnection.write.Len() != 0 {
		t.Fatalf("scene rejoin projected %d bytes to another session", otherConnection.write.Len())
	}
	if managed, found := service.runtimePartyManagerForService().SnapshotByUser(1002, session.characterGeneration); !found || len(managed.Members) != 2 {
		t.Fatalf("active scene rejoin did not restore central party membership: found=%t party=%+v", found, managed)
	}
	if queue.scheduleCalls != 1 || queue.scheduledName != "raid-phase:13108201:1:7" || queue.scheduledDelay != 90*time.Second {
		t.Fatalf("restored phase schedule calls=%d name=%q delay=%s", queue.scheduleCalls, queue.scheduledName, queue.scheduledDelay)
	}
}

func TestRuntimeRaidSceneRejoinRestoresActiveSingleMapWithoutFakeEntry(t *testing.T) {
	now := time.Date(2026, 8, 17, 1, 30, 0, 0, time.UTC)
	config := runtimeRaidBridgeTestAntonConfig()
	config.SingleMode = &raid.AntonSingleModeConfig{
		Name:             "single",
		TimeLimitSeconds: 2100,
		Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
			{Index: 5314, InitState: raid.AntonDungeonOpen, HP: 1},
			{Index: 5315, InitState: raid.AntonDungeonHide, HP: 1},
		}},
	}
	lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, now.Add(-time.Minute), true, 0)
	if err != nil {
		t.Fatalf("create single lifecycle: %v", err)
	}
	lifecycle.Status = runtimeRaidLifecyclePhaseActive
	lifecycle.ReadyDeadline = time.Time{}
	lifecycle.PhaseStartedAt = now.Add(-30 * time.Second)
	lifecycle.PhaseDeadline = now.Add(90 * time.Second)
	member := runtimeRaidMemberState{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100}
	state := &runtimeRaidState{
		RaidKey: 0x00170001, LeaderID: member.CharID, TailFlag: 1,
		Members: []runtimeRaidMemberState{member}, FrozenMembers: []runtimeRaidMemberState{member},
		Started: true, EntryCostPaid: true, ChargedMembers: []uint16{member.CharID}, StartAttempt: 1,
		AntonConfig: config, Lifecycle: lifecycle,
	}
	connection := &bufferConn{}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          map[uint32]*runtimeRaidState{state.RaidKey: state},
		gameplayTimers: &runtimeRaidPhaseTestQueue{now: now},
	}
	session := &gameSession{conn: connection, channel: channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType}}
	service.bindGameSessionCharacter(session, member.CharID)
	identity, current := service.boundGameSessionCharacterSnapshot(session)
	if !current {
		t.Fatal("single rejoin session is not current")
	}
	if err := service.sendRuntimeRaidRejoinRecovery(identity, *state, now); err != nil {
		t.Fatalf("restore active single raid: %v", err)
	}
	got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
	wantIDs := []uint16{
		raid.RaidObjectMsgID, raid.RaidMemberRefreshMsgID, raid.RaidBuffMsgID,
		raid.RaidWaitingListMsgID, raid.RaidEntryCostMsgID,
		raid.RaidSymbolMsgID, raid.RaidDungeonStateMsgID, raid.RaidStateMsgID, raid.RaidTimerMsgID, raid.RaidRemainTimeMsgID,
	}
	if ids := runtimeRaidRejoinPacketIDs(got); !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("active single rejoin packets=%v want=%v", ids, wantIDs)
	}
	if partyState := runtimePartyStateSnapshot(session); partyState.PartyID > 0 || len(partyState.Members) != 0 {
		t.Fatalf("active single rejoin created ordinary party projection: %+v", partyState)
	}
	if managed, found := service.runtimePartyManagerForService().SnapshotByUser(member.CharID, session.characterGeneration); found {
		t.Fatalf("active single rejoin created central ordinary party: %+v", managed)
	}
}

func TestRuntimeRaidSceneRejoinSkipsNonRaidChannel(t *testing.T) {
	const raidKey = uint32(0x00170001)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids: map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:  raidKey,
				LeaderID: 1001,
				Members:  []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
			},
		},
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		channel: channelcatalog.Channel{ID: 151, Type: 11},
	}
	service.bindGameSessionCharacter(session, 1001)

	if err := service.sendRuntimeRaidSceneRejoinOnce(session, "ordinary_channel"); err != nil {
		t.Fatalf("ordinary-channel scene rejoin: %v", err)
	}
	if connection.write.Len() != 0 || session.raidRejoinProjectionSent {
		t.Fatalf("ordinary channel projected raid state: bytes=%d sent=%t", connection.write.Len(), session.raidRejoinProjectionSent)
	}
}

// A raid member who is still unassigned (standby group 0) must survive the scene
// rejoin.
//
// Live incident 2026-09-16 17:36: char 2 re-entered the raid channel after a
// re-login, the server restored their presence, and the session died 10 ms later
// with no other log line in between: sendRuntimeRaidRejoinPartySnapshot answered
// errRuntimeRaidNotMember because the member sat in the standby group, and that
// error travelled up through handleGameUpper into the connection read loop, which
// closes the socket -- the client showed "网络连接中断". An unassigned member is a
// first-class state since joiners land in standby, so it must not be an error.
func TestRuntimeRaidSceneRejoinOfAStandbyMemberKeepsTheSession(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 36, 38, 0, time.UTC)
	service, session, connection, _, _, state := newRuntimeRaidRejoinTestService(t, now)
	for index := range state.Members {
		if state.Members[index].CharID == 1002 {
			state.Members[index].GroupIndex = 0
			state.Members[index].SlotOrder = 0
		}
	}
	connection.write.Reset()

	if err := service.sendRuntimeRaidSceneRejoinOnce(session, "standby_member_rejoin"); err != nil {
		t.Fatalf("standby member scene rejoin returned a fatal error: %v", err)
	}
	if connection.write.Len() == 0 {
		t.Fatal("a standby member received no raid rejoin projection at all")
	}
	if !session.raidRejoinProjectionSent {
		t.Fatal("the standby member's raid projection was not recorded")
	}

	// Nothing reached the party projection: the standby member has no squad, and
	// the ordinary town route already published the partyless op9 row.
	for _, packet := range runtimeRaidRejoinRaidPackets(t, connection.write.Bytes()) {
		if packet.msgID == uint16(dnfenum.CmdPacketRecoverStamina) {
			t.Fatalf("standby member received a party projection: %+v", packet)
		}
	}
}

type runtimeRaidRejoinExpectedPacket struct {
	msgID          uint16
	classification byte
	body           []byte
}

func newRuntimeRaidRejoinTestService(
	t *testing.T,
	now time.Time,
) (*Service, *gameSession, *bufferConn, *bufferConn, *runtimeRaidPhaseTestQueue, *runtimeRaidState) {
	t.Helper()
	const raidKey = uint32(0x00c803e9)
	config := runtimeRaidBridgeTestAntonConfig()
	lifecycle, err := newRuntimeRaidLifecycleStateFromAnton(config, now)
	if err != nil {
		t.Fatalf("create lifecycle: %v", err)
	}
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
		{CharID: 1002, Name: "rejoin", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		{CharID: 1003, Name: "other-group", GroupIndex: 2, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
	}
	state := &runtimeRaidState{
		RaidKey:        raidKey,
		LeaderID:       1001,
		NameBytes:      []byte("anton"),
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002, 1003},
		StartAttempt:   7,
		AntonConfig:    config,
		Lifecycle:      lifecycle,
	}
	queue := &runtimeRaidPhaseTestQueue{now: now}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameplayTimers: queue,
		raids:          map[uint32]*runtimeRaidState{raidKey: state},
	}
	connection := &bufferConn{}
	otherConnection := &bufferConn{}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038}
	session := &gameSession{conn: connection, connID: "raid-rejoin-current", channel: channel}
	otherSession := &gameSession{conn: otherConnection, connID: "raid-rejoin-other", channel: channel}
	service.bindGameSessionCharacter(session, 1002)
	service.bindGameSessionCharacter(otherSession, 1001)
	return service, session, connection, otherConnection, queue, state
}

func runtimeRaidRejoinBaseExpectedPackets(
	t *testing.T,
	service *Service,
	state runtimeRaidState,
) []runtimeRaidRejoinExpectedPacket {
	t.Helper()
	objectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(state))
	if err != nil {
		t.Fatalf("build raid object mode0: %v", err)
	}
	refreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		t.Fatalf("build raid member mode3: %v", err)
	}
	buffBody := mustRuntimeRaidBuffRegistrationSnapshotBody(t, state)
	return []runtimeRaidRejoinExpectedPacket{
		{
			msgID:          uint16(dnfenum.CmdPacketRejoinRaid),
			classification: dnfproto.DefaultChannelClassification,
			body:           raid.BuildRaidCommandSuccessBody(),
		},
		{msgID: raid.RaidObjectMsgID, body: objectBody},
		{msgID: raid.RaidMemberRefreshMsgID, body: refreshBody},
		{msgID: raid.RaidBuffMsgID, body: buffBody},
		{msgID: raid.RaidWaitingListMsgID, body: runtimeRaidWaitingListBody(state)},
		{msgID: raid.RaidEntryCostMsgID, body: service.runtimeRaidEntryCostSnapshotBody(state)},
	}
}

func runtimeRaidRejoinRaidPackets(t *testing.T, raw []byte) []runtimeRaidRejoinExpectedPacket {
	t.Helper()
	packets := make([]runtimeRaidRejoinExpectedPacket, 0)
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if !runtimeRaidRejoinIsRaidPacket(packet.Header.MsgID) {
			continue
		}
		packets = append(packets, runtimeRaidRejoinExpectedPacket{
			msgID:          packet.Header.MsgID,
			classification: packet.Header.Classification,
			body:           append([]byte(nil), packet.Body...),
		})
	}
	return packets
}

func runtimeRaidRejoinIsRaidPacket(msgID uint16) bool {
	if msgID == uint16(dnfenum.CmdPacketRejoinRaid) {
		return true
	}
	switch msgID {
	case raid.RaidSymbolMsgID,
		raid.RaidParticipationMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidDungeonChangeMsgID,
		raid.RaidStateMsgID,
		raid.RaidMessageMsgID,
		raid.RaidObjectMsgID,
		raid.RaidWaitingListMsgID,
		raid.RaidWaitingChangeMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
		raid.RaidEntryCostMsgID,
		raid.RaidRewardListMsgID,
		raid.RaidResultMsgID,
		raid.RaidBuffMsgID,
		raid.RaidMonsterStateMsgID:
		return true
	default:
		return false
	}
}

func assertRuntimeRaidRejoinPackets(
	t *testing.T,
	got []runtimeRaidRejoinExpectedPacket,
	want []runtimeRaidRejoinExpectedPacket,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("raid packet count=%d want=%d\ngot=%v\nwant=%v", len(got), len(want), runtimeRaidRejoinPacketIDs(got), runtimeRaidRejoinPacketIDs(want))
	}
	for index := range want {
		if got[index].msgID != want[index].msgID ||
			got[index].classification != want[index].classification ||
			!bytes.Equal(got[index].body, want[index].body) {
			t.Fatalf("raid packet[%d] class=%d msg=%#04x body=%x want class=%d msg=%#04x body=%x",
				index,
				got[index].classification,
				got[index].msgID,
				got[index].body,
				want[index].classification,
				want[index].msgID,
				want[index].body)
		}
	}
}

// mustRuntimeRaidBuffRegistrationSnapshotBody is the 0x0279 body an untouched raid
// sends. Production builds it from the live activation table, which is
// byte-identical while nothing is running, so the packet assertions that use this
// helper keep pinning the same wire shape.
func mustRuntimeRaidBuffRegistrationSnapshotBody(t *testing.T, state runtimeRaidState) []byte {
	t.Helper()
	_ = state
	body, err := runtimeRaidBuffIdleStatusBody()
	if err != nil {
		t.Fatalf("build raid Buff registration snapshot: %v", err)
	}
	return body
}

func assertRuntimeRaidBuffPacket(t *testing.T, raw []byte, state runtimeRaidState) {
	t.Helper()
	packet, rest := splitGameServerUpperPacket(t, raw)
	if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != raid.RaidBuffMsgID ||
		!bytes.Equal(packet.Body, mustRuntimeRaidBuffRegistrationSnapshotBody(t, state)) {
		t.Fatalf("raid Buff packet class=%d msg=%#04x body=%x trailing=%d", packet.Header.Classification, packet.Header.MsgID, packet.Body, len(rest))
	}
}

func runtimeRaidRejoinPacketIDs(packets []runtimeRaidRejoinExpectedPacket) []uint16 {
	ids := make([]uint16, 0, len(packets))
	for _, packet := range packets {
		ids = append(ids, packet.msgID)
	}
	return ids
}

type runtimeRaidRejoinGenerationConn struct {
	bufferConn
	writes          int
	afterFirstWrite func()
}

func (connection *runtimeRaidRejoinGenerationConn) Write(data []byte) (int, error) {
	written, err := connection.bufferConn.Write(data)
	connection.writes++
	if connection.writes == 1 && connection.afterFirstWrite != nil {
		connection.afterFirstWrite()
	}
	return written, err
}
