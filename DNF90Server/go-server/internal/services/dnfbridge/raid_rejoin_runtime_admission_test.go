package dnfbridge

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRuntimeRaidRejoinNewAdmissionRefreshesOnlyExistingMembers(t *testing.T) {
	const raidKey = uint32(0x00c803e9)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038}
	leaderConnection := &bufferConn{}
	joiningConnection := &bufferConn{}
	leader := &gameSession{conn: leaderConnection, connID: "raid-admission-leader", channel: channel}
	joining := &gameSession{conn: joiningConnection, connID: "raid-admission-joining", channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(joining, 1002)
	state := &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{{
			CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0,
			UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
	}
	service.raids[raidKey] = state
	request := make([]byte, 4)
	binary.LittleEndian.PutUint32(request, raidKey)

	if err := service.handleOnlineRejoinRaid(joining, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
		t.Fatalf("new admission rejoin: %v", err)
	}
	wantRefreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*state))
	if err != nil {
		t.Fatalf("build mode3 refresh: %v", err)
	}
	assertRuntimeRaidRejoinPackets(t,
		runtimeRaidRejoinRaidPackets(t, leaderConnection.write.Bytes()),
		[]runtimeRaidRejoinExpectedPacket{
			{msgID: raid.RaidMemberRefreshMsgID, body: wantRefreshBody},
			{msgID: raid.RaidBuffMsgID, body: mustRuntimeRaidBuffRegistrationSnapshotBody(t, *state)},
		},
	)
	assertRuntimeRaidRejoinPackets(t,
		runtimeRaidRejoinRaidPackets(t, joiningConnection.write.Bytes()),
		runtimeRaidRejoinBaseExpectedPackets(t, service, *state),
	)
}

func TestRuntimeRaidRejoinPhaseTwoRecreatesSelectorBeforeSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 6, 6, 0, 0, 0, time.UTC)
	service, session, connection, otherConnection, queue, state := newRuntimeRaidRejoinTestService(t, now)
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	state.Lifecycle.ReadyDeadline = time.Time{}
	state.Lifecycle.PhaseStartedAt = now.Add(-30 * time.Second)
	state.Lifecycle.PhaseDeadline = now.Add(90 * time.Second)
	if err := initializeRuntimeRaidPhaseObjectives(&state.Lifecycle, state.AntonConfig, runtimeRaidDungeonPhaseTwo); err != nil {
		t.Fatalf("initialize phase-two objectives: %v", err)
	}
	request := make([]byte, 4)
	binary.LittleEndian.PutUint32(request, state.RaidKey)

	if err := service.handleOnlineRejoinRaid(session, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
		t.Fatalf("phase-two rejoin: %v", err)
	}
	symbolBody, dungeonBody, _, err := runtimeRaidPhaseSnapshotBodies(*state)
	if err != nil {
		t.Fatalf("phase-two snapshot bodies: %v", err)
	}
	want := runtimeRaidRejoinBaseExpectedPackets(t, service, *state)
	want = append(want,
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidSymbolMsgID, body: symbolBody},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidPhaseStartState, Argument: 1})},
		runtimeRaidRejoinExpectedPacket{
			msgID: raid.RaidTimerMsgID,
			body: raid.BuildRaidTimerBody(raid.RaidTimer{
				TimerType: 0,
				Value0:    runtimeRaidTopTimerKey,
				Value1:    uint32(state.Lifecycle.PhaseDeadline.Unix()),
				Value2:    120,
			}),
		},
		runtimeRaidRejoinExpectedPacket{
			msgID: raid.RaidRemainTimeMsgID,
			body:  raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: 90}),
		},
	)
	got := runtimeRaidRejoinRaidPackets(t, connection.write.Bytes())
	assertRuntimeRaidRejoinPackets(t, got, want)
	if otherConnection.write.Len() != 0 {
		t.Fatalf("phase-two rejoin wrote %d bytes to another member", otherConnection.write.Len())
	}
	if queue.scheduleCalls != 1 || queue.scheduledName != "raid-phase:13108201:2:7" || queue.scheduledDelay != 90*time.Second {
		t.Fatalf("phase-two schedule calls=%d name=%q delay=%s", queue.scheduleCalls, queue.scheduledName, queue.scheduledDelay)
	}
}

func TestRuntimeRaidRejoinCachedRewardsDoNotWriteInventory(t *testing.T) {
	now := time.Date(2026, 8, 6, 7, 0, 0, 0, time.UTC)
	service, session, connection, _, queue, state := newRuntimeRaidRejoinTestService(t, now)
	rewards := raid.RaidSettlementRewards{
		ResultType: 0,
		Members: []raid.RaidSettlementMemberRewards{{
			MemberKey: 1002,
			CardIndex: 2,
			Rewards: []raid.RaidSettlementReward{{
				Kind: 0, ItemIDOrValue: 12345, CountOrAux: 2,
			}},
		}},
	}
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	state.Lifecycle.RewardDelivery = &runtimeRaidRewardDeliveryCache{
		Phase: runtimeRaidDungeonPhaseOne, Token: 9, Rewards: rewards,
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	seed := dnfrepo.InventoryRecord{
		CharacterID: "1002",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 999, Count: 7},
		},
	}
	if err := repositories.Inventory.Save(context.Background(), seed); err != nil {
		t.Fatalf("seed inventory: %v", err)
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	before, found, err := repositories.Inventory.Load(context.Background(), "1002")
	if err != nil || !found {
		t.Fatalf("load inventory before replay found=%t err=%v", found, err)
	}
	request := make([]byte, 4)
	binary.LittleEndian.PutUint32(request, state.RaidKey)

	if err := service.handleOnlineRejoinRaid(session, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
		t.Fatalf("cached reward rejoin: %v", err)
	}
	after, found, err := repositories.Inventory.Load(context.Background(), "1002")
	if err != nil || !found || !reflect.DeepEqual(after, before) {
		t.Fatalf("cached reward replay changed inventory before=%+v after=%+v found=%t err=%v", before, after, found, err)
	}
	wantRewardBody, err := raid.BuildRaidSettlementRewardsBody(rewards)
	if err != nil {
		t.Fatalf("build cached reward body: %v", err)
	}
	want := runtimeRaidRejoinBaseExpectedPackets(t, service, *state)
	want = append(want,
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidStateMsgID, body: raid.BuildRaidStateBody(raid.RaidState{State: 5, Argument: 0})},
		runtimeRaidRejoinExpectedPacket{msgID: raid.RaidRewardListMsgID, body: wantRewardBody},
	)
	assertRuntimeRaidRejoinPackets(t, runtimeRaidRejoinRaidPackets(t, connection.write.Bytes()), want)
	if queue.scheduleCalls != 0 {
		t.Fatalf("cached reward replay scheduled lifecycle callback: %d", queue.scheduleCalls)
	}
}
