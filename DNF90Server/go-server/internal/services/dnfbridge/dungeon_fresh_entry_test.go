package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func TestCommitDungeonRuntimeAlwaysCreatesFreshDungeonGeneration(t *testing.T) {
	service, session, first := newBackToVillageRuntime(t)
	if first.lifecycleToken == 0 || session.dungeon.runtime != first {
		t.Fatalf("initial runtime token=%d owner=%p", first.lifecycleToken, session.dungeon.runtime)
	}

	session.dungeon.mu.Lock()
	session.dungeon.debugLightningPending = true
	session.dungeon.deathTower = newDeathTowerRuntime([]int{30001}, 60)
	session.dungeon.mu.Unlock()
	if err := first.Session.Abandon(); err != nil {
		t.Fatal(err)
	}

	next, _, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, next); err != nil {
		t.Fatal(err)
	}
	oldToken := first.lifecycleToken
	if err := service.commitDungeonRuntime(session, next); err != nil {
		t.Fatal(err)
	}

	session.dungeon.mu.Lock()
	gotRuntime := session.dungeon.runtime
	gotToken := session.dungeon.runToken
	debugPending := session.dungeon.debugLightningPending
	deathTower := session.dungeon.deathTower
	session.dungeon.mu.Unlock()
	if gotRuntime != next || gotToken == 0 || gotToken != next.lifecycleToken || gotToken == oldToken {
		t.Fatalf("fresh runtime=%p want=%p old_token=%d new_token=%d session_token=%d", gotRuntime, next, oldToken, next.lifecycleToken, gotToken)
	}
	if !first.payloadReleased {
		t.Fatal("replaced dungeon payload was not released")
	}
	if debugPending || deathTower != nil {
		t.Fatalf("fresh state debug=%t death_tower=%p", debugPending, deathTower)
	}
}

func TestRetireCurrentDungeonRuntimeLockedClearsDetachedSessionOwners(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.dungeon.mu.Lock()
	session.dungeon.debugLightningPending = true
	session.dungeon.deathTower = newDeathTowerRuntime([]int{30001}, 60)
	service.retireCurrentDungeonRuntimeLocked(session, runtime, "test_force_or_completed_return")
	gotRuntime := session.dungeon.runtime
	gotToken := session.dungeon.runToken
	debugPending := session.dungeon.debugLightningPending
	deathTower := session.dungeon.deathTower
	session.dungeon.mu.Unlock()
	if gotRuntime != nil || gotToken != 0 || debugPending || deathTower != nil || !runtime.payloadReleased ||
		runtime.lifecycleState != currentDungeonRuntimeRetired || runtime.lifecycleEndReason != "test_force_or_completed_return" {
		t.Fatalf("retired runtime=%p token=%d debug=%t death_tower=%p released=%t lifecycle=%d reason=%q",
			gotRuntime, gotToken, debugPending, deathTower, runtime.payloadReleased,
			runtime.lifecycleState, runtime.lifecycleEndReason)
	}
}

func TestRetiredDungeonRuntimeGenerationCannotBeCommittedAgain(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.dungeon.mu.Lock()
	service.retireCurrentDungeonRuntimeLocked(session, runtime, "test_retired_generation")
	session.dungeon.mu.Unlock()

	err := service.commitDungeonRuntime(session, runtime)
	if !errors.Is(err, errDungeonRuntimeRetired) {
		t.Fatalf("commit retired runtime error=%v, want %v", err, errDungeonRuntimeRetired)
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != nil || session.dungeon.runToken != 0 {
		t.Fatalf("retired runtime became current runtime=%p token=%d",
			session.dungeon.runtime, session.dungeon.runToken)
	}
}

func TestDisconnectWithoutWorldmapRuntimeClearsDetachedDungeonOwners(t *testing.T) {
	service := &Service{}
	session := &gameSession{selectedCharacterID: 99}
	session.dungeon.runToken = 77
	session.dungeon.deathTower = newDeathTowerRuntime([]int{30001}, 60)
	session.dungeon.debugLightningPending = true

	service.releaseDisconnectedDungeonAttempt(session, "test_disconnect_without_worldmap_runtime")

	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != nil || session.dungeon.runToken != 0 ||
		session.dungeon.deathTower != nil || session.dungeon.debugLightningPending {
		t.Fatalf("detached dungeon scope leaked runtime=%p token=%d death_tower=%p debug=%t",
			session.dungeon.runtime, session.dungeon.runToken,
			session.dungeon.deathTower, session.dungeon.debugLightningPending)
	}
}

func TestFreshSoloDungeonFinishLoadingResetsConsumableGateWithoutInventingHellPartyData(t *testing.T) {
	service, session, _ := newBackToVillageRuntime(t)
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	// The actor is already created by the post-op29 chain. Keep the unrelated
	// post-loading projection complete so this test isolates the native
	// op37/op30 ordering and the absence of an invented Hell Party snapshot.
	session.postStartMapPlayerStateSent = true
	session.postFinishLoadingPlayerStateSent = true

	if err := service.sendFinishLoadingStatus(session, make([]byte, currentFinishLoadingLegacyRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	status, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if status.Header.Classification != dnfproto.DefaultChannelClassification ||
		status.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		!bytes.Equal(status.Body, []byte{1}) {
		t.Fatalf("finish status=%+v body=%x", status.Header, status.Body)
	}
	state, rest := splitGameServerUpperPacket(t, rest)
	if state.Header.Classification != 0 || state.Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		len(state.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("finish state=%+v body_len=%d", state.Header, len(state.Body))
	}
	commit, rest := splitGameServerUpperPacket(t, rest)
	if commit.Header.Classification != 0 || commit.Header.MsgID != currentIncreaseStatusResultMsgID ||
		!bytes.Equal(commit.Body, make([]byte, currentIncreaseStatusResultBodySize)) {
		t.Fatalf("finish commit=%+v body=%x", commit.Header, commit.Body)
	}
	reset, rest := splitGameServerUpperPacket(t, rest)
	if reset.Header.Classification != 0 ||
		reset.Header.MsgID != currentDungeonConsumableUseGateResetWireOpcode ||
		len(reset.Body) != 0 {
		t.Fatalf("consumable gate reset=%+v body=%x", reset.Header, reset.Body)
	}
	if len(rest) != 0 {
		extra, _ := splitGameServerUpperPacket(t, rest)
		t.Fatalf("ordinary finish-loading emitted extra packet after reset=%+v body=%x", extra.Header, extra.Body)
	}
}

func TestFreshSoloDungeonConsumableGateResetFollowsOp120ActorProjection(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	session.postStartMapPlayerStateSent = true

	if err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatal(err)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, connection.write.Bytes())
	placementIndex := -1
	resetIndex := -1
	for index, packet := range packets {
		if packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case uint16(dnfenum.CmdPacketRequestBlacklist):
			placementIndex = index
		case currentDungeonConsumableUseGateResetWireOpcode:
			resetIndex = index
		}
	}
	if placementIndex < 0 || resetIndex <= placementIndex || resetIndex != len(packets)-1 {
		t.Fatalf("finish-loading packet order placement=%d reset=%d count=%d packets=%+v",
			placementIndex, resetIndex, len(packets), packets)
	}
	reset := packets[resetIndex]
	if len(reset.Body) != 0 || !runtime.consumableUseGateResetSent {
		t.Fatalf("final reset body=%x sent=%t", reset.Body, runtime.consumableUseGateResetSent)
	}
}

func TestFreshSoloDungeonConsumableGateResetRetriesAfterOp37Op30WithoutReplay(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.postStartMapPlayerStateSent = true
	session.postFinishLoadingPlayerStateSent = true
	wantErr := errors.New("consumable gate reset write failed")
	failing := &failNthDungeonWriteConn{failAt: 4, err: wantErr}
	session.conn = failing

	err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("first finish-loading error=%v want=%v", err, wantErr)
	}
	if runtime.consumableUseGateResetSent {
		t.Fatal("failed class0/op42 write consumed the dungeon-generation reset")
	}
	first := splitAllCurrentGameServerUpperPackets(t, failing.write.Bytes())
	if len(first) != 3 ||
		first[0].Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		first[0].Header.Classification != dnfproto.DefaultChannelClassification ||
		first[1].Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		first[1].Header.Classification != 0 ||
		first[2].Header.MsgID != currentIncreaseStatusResultMsgID ||
		first[2].Header.Classification != 0 {
		t.Fatalf("first finish-loading writes=%+v", first)
	}

	resume := &bufferConn{}
	session.conn = resume
	if err := service.sendFinishLoadingStatus(
		session,
		make([]byte, currentFinishLoadingLegacyRequestBodySize),
	); err != nil {
		t.Fatalf("retry finish-loading: %v", err)
	}
	retried := splitAllCurrentGameServerUpperPackets(t, resume.write.Bytes())
	if len(retried) != 2 ||
		retried[0].Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) ||
		retried[0].Header.Classification != dnfproto.DefaultChannelClassification ||
		retried[1].Header.MsgID != currentDungeonConsumableUseGateResetWireOpcode ||
		retried[1].Header.Classification != 0 || len(retried[1].Body) != 0 {
		t.Fatalf("retry writes=%+v", retried)
	}
	if !runtime.consumableUseGateResetSent {
		t.Fatal("successful class0/op42 retry did not consume generation reset")
	}
}

func TestChangingSelectedCharacterRetiresOldDungeonScope(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.dungeon.mu.Lock()
	session.dungeon.deathTower = newDeathTowerRuntime([]int{30001}, 60)
	session.dungeon.debugLightningPending = true
	session.dungeon.mu.Unlock()

	service.bindGameSessionCharacter(session, 100)
	if session.selectedCharacterID != 100 {
		t.Fatalf("selected character=%d want=100", session.selectedCharacterID)
	}
	session.dungeon.mu.Lock()
	gotRuntime := session.dungeon.runtime
	gotToken := session.dungeon.runToken
	deathTower := session.dungeon.deathTower
	debugPending := session.dungeon.debugLightningPending
	session.dungeon.mu.Unlock()
	if gotRuntime != nil || gotToken != 0 || deathTower != nil || debugPending || !runtime.payloadReleased {
		t.Fatalf("changed-character dungeon state runtime=%p token=%d death_tower=%p debug=%t released=%t",
			gotRuntime, gotToken, deathTower, debugPending, runtime.payloadReleased)
	}
}
