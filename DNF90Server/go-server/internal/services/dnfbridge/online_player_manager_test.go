package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// TestAuxiliarySameCharacterSessionLeavesTheResidentParty keeps a second socket
// for a character the resident session still holds from taking the resident's
// town presence over -- and pins the operator rule for that same event
// (2026-09-27): 「被顶掉以后 客户端的连接就断开了 此时也按退队处理」.
//
// The party is deliberately NOT handed to the newcomer any more. It used to be
// inherited here, which made a takeover look like the same party continuing on
// another socket; now the resident is out of it, the newcomer starts partyless,
// and the survivors are told by the leave itself.
func TestAuxiliarySameCharacterSessionLeavesTheResidentParty(t *testing.T) {
	service := &Service{
		onlinePlayers: newOnlinePlayerManager(),
		gameSessions:  make(map[uint16]*gameSession),
	}
	partyState := alignedcmd.PartyState{
		PartyID: 7,
		UserID:  7,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 7, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	resident := &gameSession{selectedCharacterID: 7, party: partySessionState{state: cloneRuntimePartyState(partyState)}}
	resident.spendTime.characterID = 7
	resident.spendTime.anchor = time.Unix(1, 0)
	resident.spendTime.generation = 3
	resident.spendTime.catalog = &currentSpendTimeRuntimeCatalog{}
	auxiliary := &gameSession{}
	service.bindGameSessionCharacter(resident, 7)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 7, TownID: 38, AreaID: 0, Session: resident})

	service.bindGameSessionCharacter(auxiliary, 7)
	if got, ok := service.onlineGameSession(7); !ok || got != resident {
		t.Fatalf("auxiliary bind replaced resident session: got=%p ok=%t want=%p", got, ok, resident)
	}
	if got := runtimePartyStateSnapshot(auxiliary); got.PartyID != 0 {
		t.Fatalf("auxiliary session inherited the resident party: %+v", got)
	}
	if got := runtimePartyStateSnapshot(resident); got.PartyID != 0 {
		t.Fatalf("resident session kept a party the replacement left: %+v", got)
	}
	if resident.spendTime.characterID != 7 || resident.spendTime.anchor.IsZero() ||
		resident.spendTime.generation != 3 || resident.spendTime.catalog == nil {
		t.Fatalf("auxiliary bind stopped resident clock char=%d anchor=%s generation=%d catalog_nil=%t",
			resident.spendTime.characterID,
			resident.spendTime.anchor,
			resident.spendTime.generation,
			resident.spendTime.catalog == nil)
	}

	service.cleanupOnlinePlayer(auxiliary)
	service.unbindGameSession(auxiliary)
	if got := service.onlinePlayers.SessionForCharacter(7); got != resident {
		t.Fatalf("auxiliary disconnect evicted resident town presence: got=%p want=%p", got, resident)
	}
	if got := runtimePartyStateSnapshot(resident); got.PartyID != 0 {
		t.Fatalf("auxiliary disconnect re-attached a party to the resident: %+v", got)
	}
}

func TestChannelReconnectBindStopsResidentSpendTimeBeforeReplacementBootstrap(t *testing.T) {
	service := &Service{
		onlinePlayers: newOnlinePlayerManager(),
		gameSessions:  make(map[uint16]*gameSession),
	}
	resident := &gameSession{selectedCharacterID: 7}
	resident.spendTime.characterID = 7
	resident.spendTime.accountID = "account-1"
	resident.spendTime.anchor = time.Unix(1, 0)
	resident.spendTime.generation = 3
	resident.spendTime.catalog = &currentSpendTimeRuntimeCatalog{}
	service.bindGameSessionCharacter(resident, 7)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 7, TownID: 38, AreaID: 0, Session: resident})

	reconnect := &gameSession{channelReconnect: true}
	service.bindGameSessionCharacter(reconnect, 7)
	if resident.spendTime.characterID != 0 || !resident.spendTime.anchor.IsZero() ||
		resident.spendTime.catalog != nil {
		t.Fatalf("reconnect bind did not stop resident clock char=%d anchor=%s catalog_nil=%t",
			resident.spendTime.characterID,
			resident.spendTime.anchor,
			resident.spendTime.catalog == nil)
	}
}

func TestChannelReconnectBindArmsOp108ReplayFromTeamToOrdinaryChannel(t *testing.T) {
	for _, channelType := range []uint8{dnfenum.RaidChannelType, dnfenum.HiddenRaidType, 42} {
		t.Run(fmt.Sprintf("team_type_%d", channelType), func(t *testing.T) {
			service := &Service{
				onlinePlayers: newOnlinePlayerManager(),
				gameSessions:  make(map[uint16]*gameSession),
			}
			resident := &gameSession{
				selectedCharacterID: 7,
				channel:             channelcatalog.Channel{ID: 261, Type: channelType},
			}
			service.bindGameSessionCharacter(resident, 7)
			service.onlinePlayers.EnterArea(&onlinePlayerInfo{
				CharacterID: 7,
				TownID:      38,
				AreaID:      0,
				Session:     resident,
			})

			reconnect := &gameSession{
				channelReconnect: true,
				channel:          channelcatalog.Channel{ID: 19, Type: 11},
			}
			service.bindGameSessionCharacter(reconnect, 7)
			reconnect.spendTime.mu.Lock()
			armed := reconnect.spendTime.eventInfoReplayAfterTeamReturn
			reconnect.spendTime.mu.Unlock()
			if !armed {
				t.Fatalf("team type %d -> ordinary channel did not arm op108 replay", channelType)
			}
		})
	}
}

func TestChannelReconnectBindArmsOp108ReplayAfterTeamResidentAlreadyLeftTown(t *testing.T) {
	service := &Service{
		onlinePlayers:  newOnlinePlayerManager(),
		gameSessions:   make(map[uint16]*gameSession),
		clientAccounts: newClientAccountRegistry(),
	}
	if err := service.RegisterClientAccount(77, "dnf:1"); err != nil {
		t.Fatal(err)
	}
	team := &gameSession{
		clientPID: 77,
		channel:   channelcatalog.Channel{ID: 28, Type: dnfenum.HiddenRaidType},
	}
	service.bindGameSessionCharacter(team, 7)
	if resident := service.onlinePlayers.SessionForCharacter(7); resident != nil {
		t.Fatalf("test precondition has an unexpected online resident: %p", resident)
	}

	reconnect := &gameSession{
		clientPID:        77,
		channelReconnect: true,
		channel:          channelcatalog.Channel{ID: 19, Type: 11},
	}
	service.bindGameSessionCharacter(reconnect, 7)
	reconnect.spendTime.mu.Lock()
	armed := reconnect.spendTime.eventInfoReplayAfterTeamReturn
	reconnect.spendTime.mu.Unlock()
	if !armed {
		t.Fatal("team-to-ordinary reconnect did not arm op108 after the old town resident disappeared")
	}
}

func TestChannelReconnectBindDoesNotArmOp108ReplayOutsideTeamToOrdinaryTransition(t *testing.T) {
	tests := []struct {
		name         string
		residentType uint8
		targetType   uint8
	}{
		{name: "ordinary_to_ordinary", residentType: 11, targetType: 11},
		{name: "raid_to_raid", residentType: dnfenum.RaidChannelType, targetType: dnfenum.HiddenRaidType},
		{name: "ordinary_to_raid", residentType: 11, targetType: dnfenum.RaidChannelType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{
				onlinePlayers: newOnlinePlayerManager(),
				gameSessions:  make(map[uint16]*gameSession),
			}
			resident := &gameSession{
				selectedCharacterID: 7,
				channel:             channelcatalog.Channel{ID: 261, Type: test.residentType},
			}
			service.bindGameSessionCharacter(resident, 7)
			service.onlinePlayers.EnterArea(&onlinePlayerInfo{
				CharacterID: 7,
				TownID:      38,
				AreaID:      0,
				Session:     resident,
			})

			reconnect := &gameSession{
				channelReconnect: true,
				channel:          channelcatalog.Channel{ID: 19, Type: test.targetType},
			}
			service.bindGameSessionCharacter(reconnect, 7)
			reconnect.spendTime.mu.Lock()
			armed := reconnect.spendTime.eventInfoReplayAfterTeamReturn
			reconnect.spendTime.mu.Unlock()
			if armed {
				t.Fatalf("transition %d -> %d unexpectedly armed op108 replay", test.residentType, test.targetType)
			}
		})
	}
}

func TestCurrentTownRemoteActorOwnerContextUsesCommittedTownChannel(t *testing.T) {
	target := &gameSession{townActorOwnerChannel: 42}
	for _, characterID := range []uint16{0, 1, 42, 256, 298, 0xffff} {
		owner := currentTownRemoteActorOwnerContext(target, characterID)
		if owner != target.townActorOwnerChannel {
			t.Fatalf(
				"character %d projected owner %d, want committed town channel %d",
				characterID,
				owner,
				target.townActorOwnerChannel,
			)
		}
	}
}

func TestCurrentTownRemoteActorOwnerContextUsesConnectionChannelWhenLocalContextIsZero(t *testing.T) {
	target := &gameSession{
		channel:               channelcatalog.Channel{ID: 121},
		townActorOwnerChannel: currentSceneObjectContext,
	}
	if owner := currentTownRemoteActorOwnerContext(target, 1); owner != 121 {
		t.Fatalf("remote actor owner=%d want connected channel 121", owner)
	}
}

func TestBroadcastTownPlayerEnterCreatesRepositoryBackedRemoteActorsBeforePeerState(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{
			CharacterID: "17",
			AccountID:   "account-existing",
			Name:        "existing",
			Job:         "2",
			Level:       35,
			Stats:       map[string]int64{"grow_type": 1},
		},
		{
			CharacterID: "23",
			AccountID:   "account-newcomer",
			Name:        "newcomer",
			Job:         "5",
			Level:       48,
			Stats:       map[string]int64{"grow_type": 2},
		},
	} {
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
	}
	existingConn := &bufferConn{}
	newcomerConn := &bufferConn{}
	existingSession := &gameSession{
		conn:                  existingConn,
		connID:                "existing",
		selectedCharacterID:   17,
		accountID:             "account-existing",
		townActorOwnerChannel: 7,
	}
	newcomerSession := &gameSession{
		conn:                  newcomerConn,
		connID:                "newcomer",
		selectedCharacterID:   23,
		accountID:             "account-newcomer",
		townActorOwnerChannel: 8,
	}
	existing := onlinePlayerInfo{
		CharacterID: 17,
		AccountID:   "account-existing",
		Name:        "existing",
		Job:         2,
		GrowType:    1,
		Level:       35,
		TownID:      1,
		AreaID:      2,
		PositionX:   120,
		PositionY:   240,
		Direction:   1,
		AreaState:   3,
		Session:     existingSession,
	}
	newcomer := &onlinePlayerInfo{
		CharacterID: 23,
		AccountID:   "account-newcomer",
		Name:        "newcomer",
		Job:         5,
		GrowType:    2,
		Level:       48,
		TownID:      1,
		AreaID:      2,
		PositionX:   360,
		PositionY:   480,
		Direction:   0,
		AreaState:   4,
		Session:     newcomerSession,
	}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		onlinePlayers: newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	service.bindGameSessionCharacter(existingSession, 17)
	service.bindGameSessionCharacter(newcomerSession, 23)

	service.broadcastTownPlayerEnter(newcomer, []onlinePlayerInfo{existing})

	assertTownRemoteActorSequence(
		t,
		existingConn.write.Bytes(),
		existingSession,
		*newcomer,
	)
	assertTownRemoteActorSequence(
		t,
		newcomerConn.write.Bytes(),
		newcomerSession,
		existing,
	)
}

func TestPublishCurrentTownPresenceRegistersColdLoginAndSynchronizesExistingPeer(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	characters := []dnfrepo.CharacterRecord{
		{CharacterID: "17", AccountID: "account-existing", Name: "existing", Job: "2", Level: 35, Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "23", AccountID: "account-newcomer", Name: "newcomer", Job: "5", Level: 48, Stats: map[string]int64{"grow_type": 2}},
	}
	for _, character := range characters {
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
	}
	existingConn := &bufferConn{}
	newcomerConn := &bufferConn{}
	existingSession := &gameSession{conn: existingConn, connID: "existing", selectedCharacterID: 17, accountID: "account-existing", townActorOwnerChannel: 7}
	newcomerSession := &gameSession{conn: newcomerConn, connID: "newcomer", selectedCharacterID: 23, accountID: "account-newcomer", townActorOwnerChannel: 7}
	service := &Service{
		options:       options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
		gameSessions:  make(map[uint16]*gameSession),
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	service.bindGameSessionCharacter(existingSession, 17)
	service.bindGameSessionCharacter(newcomerSession, 23)
	existingRow := currentSceneTransitionRow{ObjectOrResourceKey: currentSceneActorObjectKey(17), Value1: 120, Value2: 240, Value3: 1, Value4: 3}
	newcomerRow := currentSceneTransitionRow{ObjectOrResourceKey: currentSceneActorObjectKey(23), Value1: 360, Value2: 480, Value3: 0, Value4: 4}

	service.publishCurrentTownPresence(existingSession, characters[0], 1, 2, existingRow, 0, "Town/Public/public.map", "test_first_cold_login")
	if presence, ok := service.onlinePlayers.PlayerForCharacter(17); !ok || presence.Session != existingSession {
		t.Fatalf("first cold-login presence missing: found=%t presence=%+v", ok, presence)
	}
	service.publishCurrentTownPresence(newcomerSession, characters[1], 1, 2, newcomerRow, 0, "Town/Public/public.map", "test_second_cold_login")
	if !service.onlinePlayers.PeerInSameArea(17, 23) {
		t.Fatal("cold-login players were not published into the same town scene")
	}

	assertTownRemoteActorSequence(t, existingConn.write.Bytes(), existingSession, onlinePlayerInfo{
		CharacterID: 23, TownID: 1, AreaID: 2, PositionX: 360, PositionY: 480,
		Direction: 0, AreaState: 4, Session: newcomerSession,
	})
	assertTownRemoteActorSequence(t, newcomerConn.write.Bytes(), newcomerSession, onlinePlayerInfo{
		CharacterID: 17, TownID: 1, AreaID: 2, PositionX: 120, PositionY: 240,
		Direction: 1, AreaState: 3, Session: existingSession,
	})

	existingBytes := existingConn.write.Len()
	newcomerBytes := newcomerConn.write.Len()
	service.publishCurrentTownPresence(newcomerSession, characters[1], 1, 2, newcomerRow, 0, "Town/Public/public.map", "test_duplicate_route_retry")
	if existingConn.write.Len() != existingBytes || newcomerConn.write.Len() != newcomerBytes {
		t.Fatalf("idempotent presence retry emitted duplicate actor packets: existing=%d/%d newcomer=%d/%d",
			existingConn.write.Len(), existingBytes, newcomerConn.write.Len(), newcomerBytes)
	}
}

func TestPublishCurrentTownPresenceSuppressesPrivateSeriaRoomAndRetiresPublicActor(t *testing.T) {
	privateConn := &bufferConn{}
	publicPeerConn := &bufferConn{}
	privateSession := &gameSession{
		conn: privateConn, connID: "private", selectedCharacterID: 17,
		townActorOwnerChannel: 7,
	}
	publicPeerSession := &gameSession{
		conn: publicPeerConn, connID: "public-peer", selectedCharacterID: 23,
		townActorOwnerChannel: 7,
	}
	service := &Service{
		onlinePlayers: newOnlinePlayerManager(),
		gameSessions:  make(map[uint16]*gameSession),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 17, TownID: 40, AreaID: 0, PositionX: 100, PositionY: 200,
		Direction: 5, AreaState: 3, Session: privateSession,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 23, TownID: 40, AreaID: 0, PositionX: 300, PositionY: 400,
		Direction: 5, AreaState: 3, Session: publicPeerSession,
	})
	privateRow := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(17),
		Value1:              450,
		Value2:              234,
		Value3:              5,
		Value4:              3,
	}
	service.publishCurrentTownPresence(
		privateSession,
		dnfrepo.CharacterRecord{CharacterID: "17", AccountID: "account-private", Name: "private"},
		38,
		1,
		privateRow,
		0,
		"Cataclysm/Town/Elvengard/new_seria_room.map",
		"test_private_seria_entry",
	)
	if _, found := service.onlinePlayers.PlayerForCharacter(17); found {
		t.Fatal("private Seria actor remained in the shared town directory")
	}
	if peer, found := service.onlinePlayers.PlayerForCharacter(23); !found || peer.Session != publicPeerSession {
		t.Fatalf("public peer was retired with private actor: found=%t peer=%+v", found, peer)
	}
	if got := privateConn.write.Bytes(); len(got) != 0 {
		t.Fatalf("private Seria owner received a remote actor projection: %x", got)
	}
	leave, trailing := splitGameServerUpperPacketWithHeader(
		t,
		publicPeerConn.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	if leave.Header.Classification != 0 || leave.Header.MsgID != currentTownUserLeaveMsgID ||
		!bytes.Equal(leave.Body, buildTownUserLeaveBody(17)) || len(trailing) != 0 {
		t.Fatalf("public observer Seria retirement header=%+v body=%x trailing=%x",
			leave.Header, leave.Body, trailing)
	}
}

func TestBroadcastTownPlayerEnterDoesNotReplaySoloSelfTownTransition(t *testing.T) {
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "solo",
		selectedCharacterID: 29,
	}
	service := &Service{onlinePlayers: newOnlinePlayerManager()}

	newcomer := &onlinePlayerInfo{
		CharacterID: 29,
		TownID:      38,
		AreaID:      0,
		PositionX:   768,
		PositionY:   238,
		Direction:   5,
		AreaState:   3,
		Session:     session,
	}
	service.broadcastTownPlayerEnter(newcomer, nil)

	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("solo co-presence replayed town transition: %x", got)
	}
}

// assertTownRemoteActorSequence 断言"不发 mode1"的城镇共现线序。
func assertTownRemoteActorSequence(
	t *testing.T,
	stream []byte,
	target *gameSession,
	actor onlinePlayerInfo,
) {
	t.Helper()
	assertTownRemoteActorSequenceWithMode1(t, stream, target, actor, false)
}

// assertTownRemoteActorSequenceWithMode1 断言 mode0 (+ 可选 mode1) -> op9 -> 0x0017 的线序。
func assertTownRemoteActorSequenceWithMode1(
	t *testing.T,
	stream []byte,
	target *gameSession,
	actor onlinePlayerInfo,
	wantMode1 bool,
) {
	t.Helper()
	wantOwner := currentTownRemoteActorOwnerContext(target, actor.CharacterID)
	mode0, rest := splitGameServerUpperPacketWithHeader(
		t,
		stream,
		dnfproto.GameServerUpperHeaderSize16,
	)
	if mode0.Header.Classification != 0 ||
		mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode0.Body) < 0x4e ||
		mode0.Body[0] != 0 ||
		mode0.Body[3] != currentSceneObjectRoute ||
		mode0.Body[4] != wantOwner ||
		binary.LittleEndian.Uint16(mode0.Body[0x4c:0x4e]) !=
			currentSceneActorObjectKey(actor.CharacterID) {
		t.Fatalf(
			"remote mode0 header=%+v body=%x owner=%d key=%d",
			mode0.Header,
			mode0.Body,
			wantOwner,
			currentSceneActorObjectKey(actor.CharacterID),
		)
	}

	// 城镇共现只发 mode0（2026-09-26）。mode1 描述的是**接收者自己**的属性/装备快照，
	// 给 peer 发一份会被客户端原生处理器当成自己的数据套到本地受控角色上；90CN 以前
	// 靠一道 !currentActorMatch 守卫把它吞掉。客户端已停用战斗力，mode1 现在没有任何
	// 消费者，服务端也就不再发。参考端同样只发最小 USERINFO(subtype0/occ0)。
	//
	// wantMode1：攻坚回城（assault.go）那条路本次**未改**，仍然发 mode1；其余城镇共现
	// 路径（broadcastTownPlayerEnter / sendChannelWidePeerActors）已不发。两者共用本助手，
	// 所以用参数区分，而不是把 mode1 那组断言删掉。
	if wantMode1 {
		mode1, mode1Rest := splitGameServerUpperPacketWithHeader(
			t,
			rest,
			dnfproto.GameServerUpperHeaderSize16,
		)
		if mode1.Header.Classification != 0 ||
			mode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
			len(mode1.Body) < currentMode1BaseWireSize ||
			mode1.Body[0] != 1 ||
			mode1.Body[3] != currentSceneObjectRoute ||
			mode1.Body[4] != wantOwner ||
			binary.LittleEndian.Uint16(mode1.Body[0x15:0x17]) !=
				currentSceneActorObjectKey(actor.CharacterID) {
			t.Fatalf(
				"remote mode1 header=%+v body=%x owner=%d key=%d",
				mode1.Header,
				mode1.Body,
				wantOwner,
				currentSceneActorObjectKey(actor.CharacterID),
			)
		}
		rest = mode1Rest
	}

	display, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	wantPartyClear := buildCurrentSceneOp9ActorRemovalBodyInContext(
		currentSceneActorObjectKey(actor.CharacterID),
		wantOwner,
	)
	if display.Header.Classification != 0 ||
		display.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		!bytes.Equal(display.Body, wantPartyClear) {
		t.Fatalf(
			"remote same-owner op9 party clear header=%+v body=%x want=%x owner=%d key=%d",
			display.Header,
			display.Body,
			wantPartyClear,
			wantOwner,
			currentSceneActorObjectKey(actor.CharacterID),
		)
	}

	state, trailing := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	wantState := buildCurrentTownUserAreaNotificationBody(
		currentSceneActorObjectKey(actor.CharacterID),
		actor.TownID,
		actor.AreaID,
		actor.PositionX,
		actor.PositionY,
		actor.Direction,
		actor.AreaState,
	)
	if state.Header.Classification != 0 ||
		state.Header.MsgID != currentTownUserAreaNotificationMsgID ||
		!bytes.Equal(state.Body, wantState) {
		t.Fatalf(
			"remote area state header=%+v body=%x want=%x",
			state.Header,
			state.Body,
			wantState,
		)
	}
	if len(trailing) != 0 {
		t.Fatalf("remote actor sequence replayed a town transition: %x", trailing)
	}
}
