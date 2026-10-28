package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

func TestRaidCreateAfterCharacterSelectionUsesBoundOriginalAccountCharacter(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.options.accountID = defaultAccountPrefix + "1"
	channel := channelcatalog.Channel{ServerID: 1, ID: 23, Type: 23, Name: "anton", Port: 10023}
	session.channel = channel
	session.residentChannel = channel
	session.selectedCharacterID = 0
	session.townSceneReadyCharacterID = 0
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	selectRequest := make([]byte, 16)
	binary.LittleEndian.PutUint32(selectRequest[:4], 0)
	if err := service.sendUpperCSharpSelectInit(session, selectRequest); err != nil {
		t.Fatalf("select character: %v", err)
	}
	if session.selectedCharacterID != 29 {
		t.Fatalf("selected character = %d, want 29", session.selectedCharacterID)
	}
	if bound, ok := service.onlineGameSession(29); !ok || bound != session {
		t.Fatalf("selected character 29 was not bound to the game session")
	}

	connection.write.Reset()
	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("anton"), 5),
	)
	if err != nil {
		t.Fatalf("create raid: %v", err)
	}
	if !handled {
		t.Fatal("create raid was not handled")
	}

	wantRaidKey := uint32(23)<<16 | 29
	state := service.raids[wantRaidKey]
	if state == nil || state.LeaderID != 29 || len(state.Members) != 1 || state.Members[0].CharID != 29 {
		t.Fatalf("runtime raid state = %+v, want character 29 as the sole leader", state)
	}

	object, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantObjectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(*state))
	if err != nil {
		t.Fatalf("build expected raid object: %v", err)
	}
	if object.Header.Classification != 0 ||
		object.Header.MsgID != raid.RaidObjectMsgID ||
		!bytes.Equal(object.Body, wantObjectBody) {
		t.Fatalf("raid object = class %d msg %#x body=% X", object.Header.Classification, object.Header.MsgID, object.Body)
	}

	buff, rest := splitGameServerUpperPacket(t, rest)
	wantBuffBody := mustRuntimeRaidBuffRegistrationSnapshotBody(t, *state)
	if buff.Header.Classification != 0 ||
		buff.Header.MsgID != raid.RaidBuffMsgID ||
		!bytes.Equal(buff.Body, wantBuffBody) {
		t.Fatalf("raid buff = class %d msg %#x body=% X", buff.Header.Classification, buff.Header.MsgID, buff.Body)
	}

	ack, rest := splitGameServerUpperPacket(t, rest)
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCreateRaid) ||
		!bytes.Equal(ack.Body, raid.BuildCreateRaidResultBody(wantRaidKey)) {
		t.Fatalf("raid create ACK = class %d msg %#x body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	refresh, rest := splitGameServerUpperPacket(t, rest)
	wantRefreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*state))
	if err != nil {
		t.Fatalf("build expected raid member refresh: %v", err)
	}
	if refresh.Header.Classification != 0 ||
		refresh.Header.MsgID != raid.RaidMemberRefreshMsgID ||
		!bytes.Equal(refresh.Body, wantRefreshBody) {
		t.Fatalf("raid member refresh = class %d msg %#x body=% X", refresh.Header.Classification, refresh.Header.MsgID, refresh.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("raid create trailing bytes = %d", len(rest))
	}
}

func TestRaidCreateWithoutCharacterBoundForConfiguredAccountReturnsReason3(t *testing.T) {
	service, session, _ := newTownMoveTest(t)
	service.options.accountID = "dnf:missing-account"
	channel := channelcatalog.Channel{ServerID: 1, ID: 23, Type: 23, Name: "anton", Port: 10023}
	session.channel = channel
	session.residentChannel = channel
	session.selectedCharacterID = 0
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	selectRequest := make([]byte, 16)
	if err := service.sendUpperCSharpSelectInit(session, selectRequest); err != nil {
		t.Fatalf("select character with mismatched account: %v", err)
	}
	if session.selectedCharacterID != 0 {
		t.Fatalf("mismatched account selected character = %d, want 0", session.selectedCharacterID)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("mismatched account wrote %d select bytes, want 0", connection.write.Len())
	}

	handled, err := service.handleAlignedGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("anton"), 5),
	)
	if err != nil {
		t.Fatalf("create raid without bound character: %v", err)
	}
	if !handled {
		t.Fatal("create raid without bound character was not handled")
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCreateRaid) ||
		!bytes.Equal(ack.Body, []byte{0, runtimeRaidFailureCode}) {
		t.Fatalf("unbound raid create ACK = class %d msg %#x body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("unbound raid create trailing bytes = %d", len(rest))
	}
	if len(service.raids) != 0 {
		t.Fatalf("unbound raid create produced %d runtime raids", len(service.raids))
	}
}

func TestRaidCreateModeMismatchRecoversExistingUnstartedSingleRaid(t *testing.T) {
	const (
		characterID = uint16(1001)
		raidKey     = uint32(0x00170001)
	)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:    connection,
		connID:  "raid-create-recover-existing",
		channel: channelcatalog.Channel{ServerID: 1, ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 10023},
	}
	service.bindGameSessionCharacter(session, characterID)
	existing := &runtimeRaidState{
		RaidKey:         raidKey,
		LeaderID:        characterID,
		RouteOrRaidType: 0,
		NameBytes:       []byte("fff"),
		TailFlag:        1,
		SingleRaidType:  0,
		Members: []runtimeRaidMemberState{{
			CharID: characterID, Name: "leader", GroupIndex: 1, SlotOrder: 0,
			UserState: 1, HPPercent: 100, MPPercent: 100,
		}},
		Lifecycle: runtimeRaidLifecycleState{Status: runtimeRaidLifecycleCreated, Revision: 1},
	}
	service.raids[raidKey] = existing

	err := service.handleOnlineCreateRaid(
		session,
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(1, []byte("different-normal-name"), 0),
	)
	if err != nil {
		t.Fatalf("recover existing raid on mismatched create: %v", err)
	}
	if len(service.raids) != 1 {
		t.Fatalf("mismatched create changed raid count: got=%d want=1", len(service.raids))
	}
	current := service.raids[raidKey]
	if current == nil || current.TailFlag != 1 || current.RouteOrRaidType != 0 || !bytes.Equal(current.NameBytes, []byte("fff")) {
		t.Fatalf("mismatched create mutated existing single raid: %+v", current)
	}

	object, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantObjectBody, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(*existing))
	if err != nil {
		t.Fatalf("build existing raid object: %v", err)
	}
	if object.Header.Classification != 0 || object.Header.MsgID != raid.RaidObjectMsgID || !bytes.Equal(object.Body, wantObjectBody) {
		t.Fatalf("recovered raid object = class %d msg %#x body=% X", object.Header.Classification, object.Header.MsgID, object.Body)
	}
	buff, rest := splitGameServerUpperPacket(t, rest)
	wantBuffBody := mustRuntimeRaidBuffRegistrationSnapshotBody(t, *existing)
	if buff.Header.Classification != 0 || buff.Header.MsgID != raid.RaidBuffMsgID || !bytes.Equal(buff.Body, wantBuffBody) {
		t.Fatalf("recovered raid buff = class %d msg %#x body=% X", buff.Header.Classification, buff.Header.MsgID, buff.Body)
	}
	ack, rest := splitGameServerUpperPacket(t, rest)
	if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
		ack.Header.MsgID != uint16(dnfenum.CmdPacketCreateRaid) ||
		!bytes.Equal(ack.Body, raid.BuildCreateRaidResultBody(raidKey)) {
		t.Fatalf("recovered raid ACK = class %d msg %#x body=% X", ack.Header.Classification, ack.Header.MsgID, ack.Body)
	}
	refresh, rest := splitGameServerUpperPacket(t, rest)
	wantRefreshBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(*existing))
	if err != nil {
		t.Fatalf("build existing raid refresh: %v", err)
	}
	if refresh.Header.Classification != 0 || refresh.Header.MsgID != raid.RaidMemberRefreshMsgID || !bytes.Equal(refresh.Body, wantRefreshBody) {
		t.Fatalf("recovered raid refresh = class %d msg %#x body=% X", refresh.Header.Classification, refresh.Header.MsgID, refresh.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("recovered raid trailing bytes = %d", len(rest))
	}
	if party := runtimePartyStateSnapshot(session); party.PartyID > 0 || len(party.Members) != 0 {
		t.Fatalf("unstarted single recovery created an ordinary party: %+v", party)
	}
}

func TestRaidCreateDoesNotRecoverAnUnstartedRaidOwnedByAnotherRaidChannel(t *testing.T) {
	const (
		characterID = uint16(1001)
		raidKey     = uint32(0x00170001)
	)
	service := &Service{raids: map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey: raidKey, LeaderID: characterID, RaidMode: runtimeRaidOwnerAnton,
			TailFlag: 1, NameBytes: []byte("single"),
			Members: []runtimeRaidMemberState{{CharID: characterID}},
		},
	}}
	session := &gameSession{channel: channelcatalog.Channel{Type: dnfenum.HiddenRaidType}}
	service.bindGameSessionCharacter(session, characterID)

	if state, created := service.createRuntimeRaid(session, characterID, raid.InfoRequest{
		NameBytes: []byte("single"), TailFlag: 1,
	}); created || state.RaidKey != 0 {
		t.Fatalf("Luke channel recovered Anton state: created=%t state=%+v", created, state)
	}
	if got := service.raids[raidKey]; got == nil || got.RaidMode != runtimeRaidOwnerAnton {
		t.Fatalf("cross-owner create mutated existing state: %+v", got)
	}
}
