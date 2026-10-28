package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	"longheng.io/server/internal/platform/timequeue"
)

func TestActivateRuntimeRaidPhaseBroadcastFailureAllowsSamePhaseRetry(t *testing.T) {
	preparedAt := time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)
	wantWriteErr := errors.New("raid phase announcement write failed")
	connection := &runtimeRaidPhaseTestConn{remainingFailures: 1, writeErr: wantWriteErr}
	queue := &runtimeRaidPhaseTestQueue{}
	service, raidKey, activationAt := newRuntimeRaidPhaseActivationTestService(t, preparedAt, connection, queue)
	queue.now = activationAt

	if _, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, activationAt, false); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first activation error=%v want=%v", err, wantWriteErr)
	}
	if queue.scheduleCalls != 0 {
		t.Fatalf("phase expiry scheduled after failed announcement: calls=%d", queue.scheduleCalls)
	}
	committed := cloneRuntimeRaidState(*service.raids[raidKey])
	if committed.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || committed.Lifecycle.Phase != runtimeRaidDungeonPhaseOne {
		t.Fatalf("committed lifecycle=%+v", committed.Lifecycle)
	}
	if !committed.Lifecycle.PhaseStartedAt.Equal(activationAt) {
		t.Fatalf("committed phase start=%s want=%s", committed.Lifecycle.PhaseStartedAt, activationAt)
	}

	retryAt := activationAt.Add(17*time.Second + 200*time.Millisecond)
	queue.now = retryAt
	retried, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, retryAt, false)
	if err != nil {
		t.Fatalf("same-phase retry: %v", err)
	}
	if !retried.Lifecycle.PhaseStartedAt.Equal(committed.Lifecycle.PhaseStartedAt) ||
		!retried.Lifecycle.PhaseDeadline.Equal(committed.Lifecycle.PhaseDeadline) ||
		retried.Lifecycle.Revision != committed.Lifecycle.Revision {
		t.Fatalf("retry reset phase lifecycle: before=%+v after=%+v", committed.Lifecycle, retried.Lifecycle)
	}
	wantRemaining := runtimeRaidDurationSeconds(committed.Lifecycle.PhaseDeadline.Sub(retryAt))
	assertRuntimeRaidPhaseActivationPackets(t, connection.write.Bytes(), runtimeRaidDungeonPhaseOne, wantRemaining)
	if queue.scheduleCalls != 1 || queue.scheduledName != "raid-phase:13108201:1:1" {
		t.Fatalf("phase expiry reconstruction calls=%d name=%q", queue.scheduleCalls, queue.scheduledName)
	}
	if wantDelay := committed.Lifecycle.PhaseDeadline.Sub(retryAt); queue.scheduledDelay != wantDelay {
		t.Fatalf("phase expiry delay=%s want=%s", queue.scheduledDelay, wantDelay)
	}
}

func TestActivateRuntimeRaidPhaseSchedulerFailureAllowsTimerReconstruction(t *testing.T) {
	preparedAt := time.Date(2026, 8, 6, 5, 0, 0, 0, time.UTC)
	wantScheduleErr := errors.New("raid phase schedule failed")
	connection := &runtimeRaidPhaseTestConn{}
	queue := &runtimeRaidPhaseTestQueue{remainingScheduleFailures: 1, scheduleErr: wantScheduleErr}
	service, raidKey, activationAt := newRuntimeRaidPhaseActivationTestService(t, preparedAt, connection, queue)
	queue.now = activationAt

	if _, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, activationAt, false); !errors.Is(err, wantScheduleErr) {
		t.Fatalf("first activation error=%v want=%v", err, wantScheduleErr)
	}
	committed := cloneRuntimeRaidState(*service.raids[raidKey])
	assertRuntimeRaidPhaseActivationPackets(
		t,
		connection.write.Bytes(),
		runtimeRaidDungeonPhaseOne,
		runtimeRaidDurationSeconds(committed.Lifecycle.PhaseDeadline.Sub(activationAt)),
	)
	connection.write.Reset()

	retryAt := activationAt.Add(9*time.Second + 500*time.Millisecond)
	queue.now = retryAt
	retried, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, retryAt, false)
	if err != nil {
		t.Fatalf("same-phase timer reconstruction: %v", err)
	}
	if !retried.Lifecycle.PhaseStartedAt.Equal(committed.Lifecycle.PhaseStartedAt) ||
		!retried.Lifecycle.PhaseDeadline.Equal(committed.Lifecycle.PhaseDeadline) ||
		retried.Lifecycle.Revision != committed.Lifecycle.Revision {
		t.Fatalf("timer reconstruction reset phase lifecycle: before=%+v after=%+v", committed.Lifecycle, retried.Lifecycle)
	}
	wantRemaining := runtimeRaidDurationSeconds(committed.Lifecycle.PhaseDeadline.Sub(retryAt))
	assertRuntimeRaidPhaseActivationPackets(t, connection.write.Bytes(), runtimeRaidDungeonPhaseOne, wantRemaining)
	if queue.scheduleCalls != 2 || queue.callback == nil {
		t.Fatalf("phase expiry reconstruction calls=%d callback_nil=%t", queue.scheduleCalls, queue.callback == nil)
	}
	if wantDelay := committed.Lifecycle.PhaseDeadline.Sub(retryAt); queue.scheduledDelay != wantDelay {
		t.Fatalf("reconstructed phase expiry delay=%s want=%s", queue.scheduledDelay, wantDelay)
	}
}

func TestPublishRuntimeRaidReadyOrdersCurrentEXEPacketsBeforeScheduling(t *testing.T) {
	preparedAt := time.Date(2026, 8, 6, 5, 30, 0, 0, time.UTC)
	wantWriteErr := errors.New("raid prepare-ui write failed")
	connection := &runtimeRaidPhaseTestConn{failAt: 4, writeErr: wantWriteErr}
	queue := &runtimeRaidPhaseTestQueue{now: preparedAt}
	service, raidKey, _ := newRuntimeRaidPhaseActivationTestService(t, preparedAt, connection, queue)
	preparing := cloneRuntimeRaidState(*service.raids[raidKey])

	if err := service.publishRuntimeRaidReady(preparing); !errors.Is(err, wantWriteErr) {
		t.Fatalf("prepare-start write failure=%v want=%v", err, wantWriteErr)
	}
	if queue.scheduleCalls != 0 {
		t.Fatalf("ready timer scheduled before all packets succeeded: calls=%d", queue.scheduleCalls)
	}
	prepareState, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if prepareState.Header.MsgID != raid.RaidStateMsgID ||
		!bytes.Equal(prepareState.Body, raid.BuildRaidStateBody(raid.RaidState{State: runtimeRaidPrepareState, Argument: 0})) {
		t.Fatalf("partial ready sequence state=%#04x body=%x", prepareState.Header.MsgID, prepareState.Body)
	}
	timer, rest := splitGameServerUpperPacket(t, rest)
	if timer.Header.MsgID != raid.RaidTimerMsgID {
		t.Fatalf("partial ready sequence timer=%#04x body=%x", timer.Header.MsgID, timer.Body)
	}
	remain, rest := splitGameServerUpperPacket(t, rest)
	if remain.Header.MsgID != raid.RaidRemainTimeMsgID || len(rest) != 0 {
		t.Fatalf("partial ready sequence remain=%#04x trailing=%d", remain.Header.MsgID, len(rest))
	}
	connection.write.Reset()

	if err := service.publishRuntimeRaidReady(preparing); err != nil {
		t.Fatalf("ready sequence retry: %v", err)
	}
	if rest := assertRuntimeRaidReadyTimer(t, connection.write.Bytes(), raidKey); len(rest) != 0 {
		t.Fatalf("ready sequence trailing bytes=%d", len(rest))
	}
	if queue.scheduleCalls != 1 || queue.scheduledDelay != 3*time.Second {
		t.Fatalf("ready scheduling calls=%d delay=%s", queue.scheduleCalls, queue.scheduledDelay)
	}
}

func TestActivateRuntimeRaidSinglePhaseBootstrapsCurrentClientStartNode(t *testing.T) {
	preparedAt := time.Date(2026, 8, 8, 4, 0, 0, 0, time.UTC)
	config := runtimeRaidBridgeTestAntonConfig()
	config.SingleMode = &raid.AntonSingleModeConfig{
		Name:             "single",
		TimeLimitSeconds: 2100,
		Phase: raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{
			{Index: 5314, InitState: raid.AntonDungeonOpen, HP: 1, PartyMax: 0},
			{Index: 5315, InitState: raid.AntonDungeonHide, HP: 1, PartyMax: 0},
		}},
	}
	lifecycle, err := newRuntimeRaidLifecycleStateFromAntonModeAndType(config, preparedAt, true, 0)
	if err != nil {
		t.Fatalf("create single lifecycle: %v", err)
	}
	member := runtimeRaidMemberState{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0}
	connection := &runtimeRaidPhaseTestConn{}
	queue := &runtimeRaidPhaseTestQueue{now: lifecycle.ReadyDeadline}
	const raidKey = uint32(0x00c803e9)
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameplayTimers: queue,
		raids: map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:        raidKey,
				LeaderID:       member.CharID,
				AntonConfig:    config,
				TailFlag:       1,
				Members:        []runtimeRaidMemberState{member},
				FrozenMembers:  []runtimeRaidMemberState{member},
				Started:        true,
				EntryCostPaid:  true,
				ChargedMembers: []uint16{member.CharID},
				StartAttempt:   1,
				Lifecycle:      lifecycle,
			},
		},
	}
	session := &gameSession{conn: connection, connID: "raid-single-phase-runtime-test", channel: channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038}}
	service.bindGameSessionCharacter(session, member.CharID)

	if _, err := service.activateRuntimeRaidPhase(raidKey, runtimeRaidDungeonPhaseOne, lifecycle.ReadyDeadline, false); err != nil {
		t.Fatalf("activate single phase: %v", err)
	}
	wantIDs := []uint16{
		raid.RaidObjectMsgID,
		raid.RaidBuffMsgID,
		raid.RaidSymbolMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidStateMsgID,
		raid.RaidDungeonChangeMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
	}
	wantDungeonBody, err := raid.BuildRaidDungeonStateBody(raid.RaidDungeonStateSnapshot{Rows: []raid.RaidDungeonStateRow{
		{DungeonID: 210, State: 0},
		{DungeonID: 211, State: 2},
		{DungeonID: 212, State: 2},
		{DungeonID: 213, State: 2},
		{DungeonID: 214, State: 2},
		{DungeonID: 215, State: 2},
		{DungeonID: 216, State: 2},
		{DungeonID: 218, State: 2},
		{DungeonID: 219, State: 2},
		{DungeonID: 220, State: 2},
		{DungeonID: 221, State: 2},
		{DungeonID: 222, State: 2},
		{DungeonID: 223, State: 2},
		{DungeonID: 224, State: 2},
		{DungeonID: 5314, State: 0},
		{DungeonID: 5315, State: 2},
	}})
	if err != nil {
		t.Fatalf("build expected single dungeon state: %v", err)
	}
	stream := connection.write.Bytes()
	for index, wantID := range wantIDs {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != wantID {
			t.Fatalf("single phase packet %d class=%d msg=%#04x want=%#04x", index, packet.Header.Classification, packet.Header.MsgID, wantID)
		}
		switch wantID {
		case raid.RaidDungeonStateMsgID:
			if !bytes.Equal(packet.Body, wantDungeonBody) {
				t.Fatalf("single dungeon state body=%x want=%x", packet.Body, wantDungeonBody)
			}
		case raid.RaidDungeonChangeMsgID:
			want := []byte{0xd2, 0, 0, 0, 0, 0}
			if !bytes.Equal(packet.Body, want) {
				t.Fatalf("single start node refresh=%x want=%x", packet.Body, want)
			}
		}
	}
	if len(stream) != 0 {
		t.Fatalf("single phase trailing bytes=%d", len(stream))
	}

	connection.write.Reset()
	if err := service.sendRuntimeRaidSelectorSnapshot(session, "single_selector_reopen"); err != nil {
		t.Fatalf("reopen single selector: %v", err)
	}
	wantSelectorIDs := []uint16{
		raid.RaidObjectMsgID,
		raid.RaidMemberRefreshMsgID,
		raid.RaidSymbolMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
	}
	stream = connection.write.Bytes()
	for index, wantID := range wantSelectorIDs {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 || packet.Header.MsgID != wantID {
			t.Fatalf("single selector packet %d class=%d msg=%#04x want=%#04x", index, packet.Header.Classification, packet.Header.MsgID, wantID)
		}
		if packet.Header.MsgID == raid.RaidStateMsgID {
			t.Fatalf("single selector replayed one-shot raid state=%x", packet.Body)
		}
		if packet.Header.MsgID == raid.RaidDungeonStateMsgID && !bytes.Equal(packet.Body, wantDungeonBody) {
			t.Fatalf("single selector dungeon state body=%x want=%x", packet.Body, wantDungeonBody)
		}
		if packet.Header.MsgID == raid.RaidParticipationMsgID || packet.Header.MsgID == raid.RaidDungeonChangeMsgID {
			t.Fatalf("single selector replayed one-shot bootstrap packet %#04x", packet.Header.MsgID)
		}
	}
	if len(stream) != 0 {
		t.Fatalf("single selector trailing bytes=%d", len(stream))
	}
}

func TestRuntimeRaidObjectMode0PreservesLifecycleForSelectorReplay(t *testing.T) {
	tests := []struct {
		name      string
		status    runtimeRaidLifecycleStatus
		phase     byte
		wantState byte
		wantPhase byte
	}{
		{name: "created", status: runtimeRaidLifecycleCreated, wantPhase: 0xff},
		{name: "preparing", status: runtimeRaidLifecyclePreparing, phase: runtimeRaidDungeonPhaseOne, wantPhase: 0xff},
		{name: "phase_one_active", status: runtimeRaidLifecyclePhaseActive, phase: runtimeRaidDungeonPhaseOne, wantState: 2, wantPhase: 0},
		{name: "phase_two_active", status: runtimeRaidLifecyclePhaseActive, phase: runtimeRaidDungeonPhaseTwo, wantState: 2, wantPhase: 1},
		{name: "awaiting_movie", status: runtimeRaidLifecycleAwaitingMovie, phase: runtimeRaidDungeonPhaseOne, wantState: 3, wantPhase: 0},
		{name: "awaiting_rewards", status: runtimeRaidLifecycleAwaitingRewards, phase: runtimeRaidDungeonPhaseOne, wantState: 5, wantPhase: 0},
		{name: "standby", status: runtimeRaidLifecycleStandby, phase: runtimeRaidDungeonPhaseOne, wantState: 5, wantPhase: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := runtimeRaidObjectMode0(runtimeRaidState{
				RaidKey:  0x00170001,
				LeaderID: 1001,
				TailFlag: 1,
				Members: []runtimeRaidMemberState{{
					CharID: 1001,
					Name:   "leader",
				}},
				Lifecycle: runtimeRaidLifecycleState{Status: test.status, Phase: test.phase},
			})
			if object.ObjectField28 != test.wantState || object.ObjectField30 != test.wantPhase {
				t.Fatalf("object lifecycle=%d/%d want=%d/%d", object.ObjectField28, object.ObjectField30, test.wantState, test.wantPhase)
			}
			if object.ObjectField94 != 1 || len(object.Members) != 1 {
				t.Fatalf("single/member projection field94=%d members=%d", object.ObjectField94, len(object.Members))
			}
		})
	}
}

func newRuntimeRaidPhaseActivationTestService(
	t *testing.T,
	preparedAt time.Time,
	connection *runtimeRaidPhaseTestConn,
	queue *runtimeRaidPhaseTestQueue,
) (*Service, uint32, time.Time) {
	t.Helper()
	const raidKey = uint32(0x00c803e9)
	config := runtimeRaidBridgeTestAntonConfig()
	lifecycle, err := newRuntimeRaidLifecycleStateFromAnton(config, preparedAt)
	if err != nil {
		t.Fatalf("prepare raid lifecycle: %v", err)
	}
	member := runtimeRaidMemberState{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameplayTimers: queue,
		raids: map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:        raidKey,
				LeaderID:       member.CharID,
				AntonConfig:    config,
				Members:        []runtimeRaidMemberState{member},
				FrozenMembers:  []runtimeRaidMemberState{member},
				Started:        true,
				EntryCostPaid:  true,
				ChargedMembers: []uint16{member.CharID},
				StartAttempt:   1,
				Lifecycle:      lifecycle,
			},
		},
	}
	session := &gameSession{
		conn:    connection,
		connID:  "raid-phase-runtime-test",
		channel: channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10038},
	}
	service.bindGameSessionCharacter(session, member.CharID)
	return service, raidKey, lifecycle.ReadyDeadline
}

// assertRuntimeRaidPhaseActivationPackets pins the raid wire sequence of a phase
// activation. The stream is filtered rather than read positionally, because
// activation also re-asserts the squads' ordinary-party projection
// (op9 -> op153) and installs each squad mate's actor record, and neither belongs
// to the raid sequence this helper owns.
func assertRuntimeRaidPhaseActivationPackets(t *testing.T, raw []byte, phase byte, wantSeconds uint32) {
	t.Helper()
	wantIDs := []uint16{
		raid.RaidObjectMsgID,
		raid.RaidBuffMsgID,
		raid.RaidSymbolMsgID,
		raid.RaidDungeonStateMsgID,
		raid.RaidStateMsgID,
		raid.RaidTimerMsgID,
		raid.RaidRemainTimeMsgID,
	}
	packets := make([]runtimeRaidRejoinExpectedPacket, 0, len(wantIDs))
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
	if len(packets) != len(wantIDs) {
		t.Fatalf("phase activation raid packets=%d want=%d", len(packets), len(wantIDs))
	}
	for index, wantID := range wantIDs {
		packet := packets[index]
		if packet.classification != 0 || packet.msgID != wantID {
			t.Fatalf("phase packet %d class=%d msg=%#04x want=%#04x", index, packet.classification, packet.msgID, wantID)
		}
		if wantID == raid.RaidObjectMsgID && len(packet.body) == 0 {
			t.Fatalf("phase activation emitted an empty raid object")
		}
		if packet.msgID == raid.RaidParticipationMsgID {
			t.Fatalf("phase activation emitted empty participation packet 0x0248")
		}
		switch wantID {
		case raid.RaidStateMsgID:
			wantBody := []byte{runtimeRaidPhaseStartState, phase - 1}
			if !bytes.Equal(packet.body, wantBody) {
				t.Fatalf("phase state body=%x want=%x", packet.body, wantBody)
			}
		case raid.RaidTimerMsgID:
			if len(packet.body) != 13 || packet.body[0] != 0 ||
				binary.LittleEndian.Uint32(packet.body[1:5]) != runtimeRaidTopTimerKey ||
				binary.LittleEndian.Uint32(packet.body[5:9]) == 0 ||
				binary.LittleEndian.Uint32(packet.body[9:13]) == 0 {
				t.Fatalf("phase timer body=%x", packet.body)
			}
		case raid.RaidRemainTimeMsgID:
			if len(packet.body) != 5 || packet.body[0] != 0 || binary.LittleEndian.Uint32(packet.body[1:]) != wantSeconds {
				t.Fatalf("phase remaining body=%x want_seconds=%d", packet.body, wantSeconds)
			}
		}
	}
}

type runtimeRaidPhaseTestConn struct {
	bufferConn
	remainingFailures int
	writeCalls        int
	failAt            int
	writeErr          error
}

func (connection *runtimeRaidPhaseTestConn) Write(data []byte) (int, error) {
	connection.writeCalls++
	if connection.failAt > 0 && connection.writeCalls == connection.failAt {
		return 0, connection.writeErr
	}
	if connection.remainingFailures > 0 {
		connection.remainingFailures--
		return 0, connection.writeErr
	}
	return connection.bufferConn.Write(data)
}

type runtimeRaidPhaseTestQueue struct {
	now                       time.Time
	remainingScheduleFailures int
	scheduleErr               error
	scheduleCalls             int
	scheduledName             string
	scheduledDelay            time.Duration
	callback                  timequeue.Callback
}

func (queue *runtimeRaidPhaseTestQueue) Now() time.Time {
	return queue.now
}

func (queue *runtimeRaidPhaseTestQueue) ScheduleAfter(
	name string,
	delay time.Duration,
	callback timequeue.Callback,
) error {
	queue.scheduleCalls++
	queue.scheduledName = name
	queue.scheduledDelay = delay
	if queue.remainingScheduleFailures > 0 {
		queue.remainingScheduleFailures--
		return queue.scheduleErr
	}
	queue.callback = callback
	return nil
}

func (queue *runtimeRaidPhaseTestQueue) RegisterDaily(string, int, int, timequeue.Callback) error {
	return nil
}

func (queue *runtimeRaidPhaseTestQueue) Cancel(string) bool {
	return false
}

func (queue *runtimeRaidPhaseTestQueue) Start(context.Context) {}
