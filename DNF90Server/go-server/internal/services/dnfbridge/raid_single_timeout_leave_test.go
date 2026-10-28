package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

func TestActiveSingleRaidLeaveAbandonsRaidAndEndsClientProjection(t *testing.T) {
	now := time.Date(2026, 8, 18, 20, 0, 0, 0, time.UTC)
	queue := &runtimeRaidPhaseTestQueue{now: now}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameplayTimers: queue,
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	service.bindGameSessionCharacter(session, 1001)
	storeRuntimeRaidRejoinProjection(session, 4, 9)
	service.raids[4] = &runtimeRaidState{
		RaidKey:       4,
		LeaderID:      1001,
		TailFlag:      1,
		Started:       true,
		StartAttempt:  1,
		Members:       []runtimeRaidMemberState{{CharID: 1001}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:        runtimeRaidLifecyclePhaseActive,
			Phase:         runtimeRaidDungeonPhaseOne,
			PhaseDeadline: now.Add(time.Minute),
		},
	}
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 4)

	if err := service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("active single leave: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
		!bytes.Equal(packet.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("active leave packet class=%d msg=%d body=% X rest=%d", packet.Header.Classification, packet.Header.MsgID, packet.Body, len(rest))
	}
	if _, exists := service.raids[4]; exists {
		t.Fatal("active single Raid still exists after successful leave")
	}
	if !runtimeRaidEndedProjectionMatches(session, 4) || runtimeRaidRejoinProjectionMatches(session, 4, 9) {
		t.Fatal("single Raid leave did not finalize its session projection")
	}
}

func TestCompletedSingleRaidLeaveSucceedsAfterRuntimeFinalization(t *testing.T) {
	service := &Service{
		options:             options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:               make(map[uint32]*runtimeRaidState),
		raidRewardCompleted: make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion),
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	service.bindGameSessionCharacter(session, 1001)
	storeRuntimeRaidRejoinProjection(session, 4, 7)
	completed := runtimeRaidState{
		RaidKey:       4,
		LeaderID:      1001,
		TailFlag:      1,
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
		Lifecycle: runtimeRaidLifecycleState{
			Phase:  runtimeRaidDungeonPhaseOne,
			Status: runtimeRaidLifecycleFinished,
		},
	}
	key := runtimeRaidRewardCompletionKey{RaidKey: 4, Phase: runtimeRaidDungeonPhaseOne, Token: 9}
	service.raidRewardCompleted[key] = runtimeRaidSettlementRewardCompletion{Raid: completed}
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 4)

	if err := service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("completed single Raid leave: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
		!bytes.Equal(packet.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("completed leave packet class=%d msg=%d body=% X rest=%d", packet.Header.Classification, packet.Header.MsgID, packet.Body, len(rest))
	}
	if !runtimeRaidEndedProjectionMatches(session, 4) || runtimeRaidRejoinProjectionMatches(session, 4, 7) {
		t.Fatal("completed single Raid leave did not finalize its session projection")
	}
}

// TestCompletedTeamRaidLeaveSucceedsAfterRuntimeFinalization is the operator's
// 2026-09-20 report "二阶段通关 攻坚流程结束后 不能退出攻坚队 点击退出没反应": the
// leave request arrived at a raid the server had already released, matched no branch
// and was answered with the silent failure body {0,3} -- eleven times in the live log
// (11:30:55 … 11:47:06, e.g. SEND … msg_id=665 hex …150000000003).
func TestCompletedTeamRaidLeaveSucceedsAfterRuntimeFinalization(t *testing.T) {
	service := &Service{
		options:             options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:               make(map[uint32]*runtimeRaidState),
		raidRewardCompleted: make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion),
	}
	connection := &bufferConn{}
	leaver := &gameSession{conn: connection}
	service.bindGameSessionCharacter(leaver, 13)
	// Raid 1507329 is the live raid of the report; its key's low u16 is the
	// CREATOR's character id (1), not the leaver's (13).
	teamRaidKey := uint32(1507329)
	storeRuntimeRaidRejoinProjection(leaver, teamRaidKey, 7)
	// The leaver still holds the ordinary party the raid projected for his squad.
	storeRuntimePartyState(leaver, alignedcmd.PartyState{
		PartyID: 999, UserID: 13, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{{UserID: 13, UserState: 1, HPPercent: 100, MPPercent: 100}},
	})
	// The team raid was released by finalizeRuntimeRaid, so only the committed
	// completion record still knows the roster.
	completed := runtimeRaidState{
		RaidKey:  1507329,
		LeaderID: 1,
		TailFlag: 0,
		FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1, GroupIndex: 1, SlotOrder: 0, UserState: 1},
			{CharID: 13, GroupIndex: 1, SlotOrder: 1, UserState: 1},
		},
		Lifecycle: runtimeRaidLifecycleState{
			Phase:  runtimeRaidDungeonPhaseTwo,
			Status: runtimeRaidLifecycleFinished,
		},
	}
	key := runtimeRaidRewardCompletionKey{RaidKey: 1507329, Phase: runtimeRaidDungeonPhaseTwo, Token: 11}
	service.raidRewardCompleted[key] = runtimeRaidSettlementRewardCompletion{Raid: completed}
	// The live client sends its OWN character id for a self-leave (measured:
	// "0d 00" for char 13, "0a 00" for char 10), not the team raid key.
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 13)

	if err := service.handleOnlineLeaveRaid(leaver, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("completed team Raid leave: %v", err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
		!bytes.Equal(ack.Body, raid.BuildLeaveRaidResultBody(false)) {
		t.Fatalf("completed team leave ack class=%d msg=%d body=% X, want the success body % X",
			ack.Header.Classification, ack.Header.MsgID, ack.Body, raid.BuildLeaveRaidResultBody(false))
	}
	// The leaver's client drops the object it materialized, exactly like an ordinary
	// leave: the raid window must not stay up on a raid the player has left.
	removal, rest := splitGameServerUpperPacket(t, rest)
	if removal.Header.MsgID != raid.RaidObjectMsgID ||
		!bytes.Equal(removal.Body, raid.BuildRaidObjectRemoveBody(1507329)) {
		t.Fatalf("completed team leave sent msg=%d body=% X, want the raid object removal % X",
			removal.Header.MsgID, removal.Body, raid.BuildRaidObjectRemoveBody(1507329))
	}
	if len(rest) == 0 {
		t.Fatal("a completed team leave did not clear the party the raid projected")
	}
	if remaining := runtimePartyStateSnapshot(leaver); remaining.PartyID != 0 {
		t.Fatalf("the leaver still projects party %d after leaving the finished raid", remaining.PartyID)
	}
	if !runtimeRaidEndedProjectionMatches(leaver, uint16(teamRaidKey)) ||
		runtimeRaidRejoinProjectionMatches(leaver, teamRaidKey, 7) {
		t.Fatal("completed team Raid leave did not finalize its session projection")
	}

	// A character in no finished team raid still gets the failure body: the new
	// branch must not become a blanket success.
	stranger := &gameSession{conn: &bufferConn{}}
	service.bindGameSessionCharacter(stranger, 4242)
	if err := service.handleOnlineLeaveRaid(stranger, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("stranger leave: %v", err)
	}
	failure, _ := splitGameServerUpperPacket(t, stranger.conn.(*bufferConn).write.Bytes())
	if bytes.Equal(failure.Body, raid.BuildLeaveRaidResultBody(false)) {
		t.Fatalf("a character outside every finished raid was acknowledged as a leaver: body=% X", failure.Body)
	}
}

func TestActiveRaidLeaveKeepsTeamRaidAndMismatchedSingleKeyRejected(t *testing.T) {
	tests := []struct {
		name       string
		tailFlag   byte
		requestKey uint16
	}{
		{name: "team Raid", tailFlag: 0, requestKey: 4},
		{name: "single mismatched key", tailFlag: 1, requestKey: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{
				options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
				raids:   make(map[uint32]*runtimeRaidState),
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection}
			service.bindGameSessionCharacter(session, 1001)
			service.raids[4] = &runtimeRaidState{
				RaidKey:       4,
				LeaderID:      1001,
				TailFlag:      test.tailFlag,
				Started:       true,
				Members:       []runtimeRaidMemberState{{CharID: 1001}},
				FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
				Lifecycle:     runtimeRaidLifecycleState{Status: runtimeRaidLifecyclePhaseActive, Phase: 1},
			}
			body := make([]byte, 2)
			binary.LittleEndian.PutUint16(body, test.requestKey)

			if err := service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
				t.Fatalf("rejected active leave: %v", err)
			}
			packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(rest) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
				bytes.Equal(packet.Body, raid.BuildLeaveRaidResultBody(true)) {
				t.Fatalf("rejected leave packet msg=%d body=% X rest=%d", packet.Header.MsgID, packet.Body, len(rest))
			}
			if _, exists := service.raids[4]; !exists {
				t.Fatal("rejected active leave removed Raid")
			}
		})
	}
}

func TestSingleRaidPhaseExpiryPublishesEndedAndAcceptsLeaveRetry(t *testing.T) {
	now := time.Date(2026, 8, 18, 21, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Minute)
	queue := &runtimeRaidPhaseTestQueue{now: now}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameplayTimers: queue,
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	service.bindGameSessionCharacter(session, 1001)
	storeRuntimeRaidRejoinProjection(session, 4, 11)
	state := runtimeRaidState{
		RaidKey:       4,
		LeaderID:      1001,
		TailFlag:      1,
		Started:       true,
		StartAttempt:  3,
		Members:       []runtimeRaidMemberState{{CharID: 1001}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:         runtimeRaidLifecyclePhaseActive,
			Phase:          runtimeRaidDungeonPhaseOne,
			PhaseStartedAt: now,
			PhaseDeadline:  deadline,
		},
	}
	service.raids[4] = &state
	if err := service.scheduleRuntimeRaidPhaseExpiry(state); err != nil {
		t.Fatalf("schedule expiry: %v", err)
	}
	queue.callback(deadline)
	// An expired attack window is not a teardown. 86JP fails the phase first
	// (RunAttackTimeoutAsync -> TryFailPhase) and only hands the terminal object to
	// a ten-second remover afterwards, so the first packet is the closed gate table
	// and the raid must still be readable while the result window is open.
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != raid.RaidDungeonStateMsgID || len(rest) == 0 {
		t.Fatalf("expiry packet msg=%d body=% X rest=%d, want the failure presentation",
			packet.Header.MsgID, packet.Body, len(rest))
	}
	if failed := service.raids[4]; failed == nil || failed.Lifecycle.Status != runtimeRaidLifecycleFailed {
		t.Fatalf("expired single Raid = %+v, want a failed raid kept for its result window", failed)
	}
	if runtimeRaidEndedProjectionMatches(session, 4) || !runtimeRaidRejoinProjectionMatches(session, 4, 11) {
		t.Fatal("the failure presentation released the raid before its result window")
	}

	// The expiry callback armed the delayed removal, so the queue now holds that
	// callback instead of a second expiry: firing it again is what releases the
	// failed raid and tells the member it is over.
	connection.write.Reset()
	queue.callback(deadline)
	release, releaseRest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(releaseRest) != 0 || release.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
		!bytes.Equal(release.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("release packet msg=%d body=% X rest=%d", release.Header.MsgID, release.Body, len(releaseRest))
	}
	if _, exists := service.raids[4]; exists {
		t.Fatal("failed single Raid outlived its result window")
	}
	if !runtimeRaidEndedProjectionMatches(session, 4) || runtimeRaidRejoinProjectionMatches(session, 4, 11) {
		t.Fatal("release did not finalize its session projection")
	}

	connection.write.Reset()
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 4)
	if err := service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("leave retry after expiry: %v", err)
	}
	retry, retryRest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(retryRest) != 0 || !bytes.Equal(retry.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("expiry leave retry body=% X rest=%d", retry.Body, len(retryRest))
	}
}

func TestSingleRaidPhaseExpiryWriteFailureKeepsLeaveRetryProof(t *testing.T) {
	now := time.Date(2026, 8, 18, 22, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Minute)
	queue := &runtimeRaidPhaseTestQueue{now: now}
	connection := &runtimeRaidPhaseTestConn{
		remainingFailures: 1,
		writeErr:          errors.New("timeout notification write failed"),
	}
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		gameplayTimers: queue,
	}
	session := &gameSession{conn: connection}
	service.bindGameSessionCharacter(session, 1001)
	storeRuntimeRaidRejoinProjection(session, 4, 13)
	state := runtimeRaidState{
		RaidKey:       4,
		LeaderID:      1001,
		TailFlag:      1,
		Started:       true,
		StartAttempt:  5,
		Members:       []runtimeRaidMemberState{{CharID: 1001}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
		Lifecycle: runtimeRaidLifecycleState{
			Status:         runtimeRaidLifecyclePhaseActive,
			Phase:          runtimeRaidDungeonPhaseOne,
			PhaseStartedAt: now,
			PhaseDeadline:  deadline,
		},
	}
	service.raids[4] = &state
	if err := service.scheduleRuntimeRaidPhaseExpiry(state); err != nil {
		t.Fatalf("schedule expiry: %v", err)
	}
	queue.callback(deadline)
	// The failure presentation could not be written, but the expiry itself is
	// already committed and the delayed removal was armed before the write, so the
	// socket error can neither revive the phase nor strand the raid in the table.
	if connection.write.Len() != 0 {
		t.Fatalf("failed failure presentation wrote %d bytes", connection.write.Len())
	}
	if failed := service.raids[4]; failed == nil || failed.Lifecycle.Status != runtimeRaidLifecycleFailed {
		t.Fatalf("expired single Raid = %+v, want a failed raid still awaiting its removal", failed)
	}

	// Even with the presentation lost, the removal still carries the retry proof:
	// the release stores the ended projection, clears the rejoin projection and
	// answers op665 with the success body on the now-healthy socket.
	queue.callback(deadline.Add(runtimeRaidFailedResultWindow))
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketLeaveRaid) ||
		!bytes.Equal(packet.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("release packet msg=%d body=% X rest=%d", packet.Header.MsgID, packet.Body, len(rest))
	}
	if _, exists := service.raids[4]; exists {
		t.Fatal("expired single Raid survived its result window")
	}
	if !runtimeRaidEndedProjectionMatches(session, 4) || runtimeRaidRejoinProjectionMatches(session, 4, 13) {
		t.Fatal("release discarded the retry proof")
	}

	connection.write.Reset()
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 4)
	if err := service.handleOnlineLeaveRaid(session, uint16(dnfenum.CmdPacketLeaveRaid), body); err != nil {
		t.Fatalf("leave retry after write failure: %v", err)
	}
	retry, retryRest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(retryRest) != 0 || !bytes.Equal(retry.Body, raid.BuildLeaveRaidResultBody(true)) {
		t.Fatalf("write-failure retry body=% X rest=%d", retry.Body, len(retryRest))
	}
	if runtimeRaidRejoinProjectionMatches(session, 4, 13) {
		t.Fatal("successful retry retained the stale rejoin projection")
	}
}
