package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestHandleCurrentPeerUserInfoSendsTargetMode3WithoutRosterBootstrap(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, account := range []dnfrepo.AccountRecord{
		{AccountID: "dnf:1", State: "active"},
		{AccountID: "dnf:2", State: "active"},
	} {
		if err := repositories.Account.Save(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "pouut", Job: "0", Level: 90},
		{CharacterID: "5", AccountID: "dnf:2", Name: "来来来", Job: "1", Level: 90, Stats: map[string]int64{
			"pvp_grade": 2, "pvp_rank_point": 3456, "expert_job_type": 3,
		}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	sourceConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 42}
	source := &gameSession{conn: sourceConn, channel: channel, accountID: "dnf:1", selectedCharacterID: 1, townActorOwnerChannel: 42}
	target := &gameSession{conn: &bufferConn{}, channel: channel, accountID: "dnf:2", selectedCharacterID: 5, townActorOwnerChannel: 42}
	service := &Service{
		options:             options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, TownID: 1, AreaID: 2, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, TownID: 1, AreaID: 2, Session: target})

	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{5, 0, currentPeerUserInfoMode},
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, sourceConn.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(rest) != 0 {
		t.Fatalf("peer userinfo header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) < 0x17 || packet.Body[0] != currentPeerUserInfoMode ||
		binary.LittleEndian.Uint16(packet.Body[0x0d:0x0f]) != 5 || packet.Body[4] != 42 {
		t.Fatalf("peer mode3 body head=%x", packet.Body[:min(len(packet.Body), 32)])
	}
	if source.rosterRequested {
		t.Fatal("peer mode3 request was misrouted to the character roster bootstrap")
	}
	// With no equipped create rows and no skill pages the current mode3 trailer
	// starts at 133 and reaches sub_2005520 at these fixed offsets: u32 cursor
	// field, u16 duel grade, u32 rank points, u32 length of the string
	// sub_3457C80 reads there, u8 expert-job type, u8 UI-state, then the
	// sub_2002DC0 count and the sub_2006BB0 / sub_20070C0 gates.
	//
	// The string's length field is not optional: sub_2005520 ->
	// sub_3457D20 -> sub_3457C80 reads one u32 length plus that many bytes
	// between the rank points and the u8 pair. Dropping it (length zero emitted
	// as "a string nobody reads") moved every following reader four bytes early
	// and made the peer mode-3 body crash the client - see
	// userinfo_mode3_trailer_test.go and userinfo_current_modes.go.
	if len(packet.Body) != 172 ||
		binary.LittleEndian.Uint32(packet.Body[150:154]) != 0 ||
		binary.LittleEndian.Uint16(packet.Body[154:156]) != 2 ||
		binary.LittleEndian.Uint32(packet.Body[156:160]) != 3456 ||
		binary.LittleEndian.Uint32(packet.Body[160:164]) != 0 ||
		packet.Body[164] != 3 || packet.Body[165] != 0 ||
		binary.LittleEndian.Uint32(packet.Body[166:170]) != 0 ||
		packet.Body[170] != 0 || packet.Body[171] != 0 {
		t.Fatalf("peer mode3 profession/rank tail=%x (len=%d)",
			packet.Body[150:min(len(packet.Body), 172)], len(packet.Body))
	}
}

func TestHandleCurrentPeerUserInfoSendsSelfMode3OnDemand(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:self", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "9", AccountID: "dnf:self", Name: "self", Job: "0", Level: 90,
		Stats: map[string]int64{"expert_job_type": 3},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn: connection, channel: channelcatalog.Channel{ID: 252},
		accountID: "dnf:self", selectedCharacterID: 9,
		townActorOwnerChannel: 252,
	}
	service := &Service{
		options:             options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.bindGameSessionCharacter(session, 9)

	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{9, 0, currentPeerUserInfoMode},
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(rest) != 0 {
		t.Fatalf("self userinfo header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) < 0x17 || packet.Body[0] != currentPeerUserInfoMode ||
		binary.LittleEndian.Uint16(packet.Body[0x0d:0x0f]) != 9 || packet.Body[4] != 252 {
		t.Fatalf("self mode3 body head=%x", packet.Body[:min(len(packet.Body), 32)])
	}
	if session.rosterRequested {
		t.Fatal("self mode3 request was misrouted to the character roster bootstrap")
	}
}

// A dungeon admission retires both characters from the town online-player
// directory, so PeerInSameArea can no longer answer the peer question there.
// The runtime party the two share must keep the information panel reachable.
func TestHandleCurrentPeerUserInfoAcceptsPartyMemberOutsideTownDirectory(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, account := range []dnfrepo.AccountRecord{
		{AccountID: "dnf:1", State: "active"},
		{AccountID: "dnf:2", State: "active"},
	} {
		if err := repositories.Account.Save(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "leader", Job: "0", Level: 90},
		{CharacterID: "5", AccountID: "dnf:2", Name: "member", Job: "1", Level: 90},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	sourceConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 42}
	source := &gameSession{conn: sourceConn, channel: channel, accountID: "dnf:1", selectedCharacterID: 1, townActorOwnerChannel: 42}
	target := &gameSession{conn: &bufferConn{}, channel: channel, accountID: "dnf:2", selectedCharacterID: 5, townActorOwnerChannel: 42}
	service := &Service{
		options:             options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)
	// Deliberately no EnterArea: this is the in-dungeon state.
	if service.onlinePlayers.PeerInSameArea(1, 5) {
		t.Fatal("test setup registered town co-presence")
	}

	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{5, 0, currentPeerUserInfoMode},
	); err != nil {
		t.Fatal(err)
	}
	if sourceConn.write.Len() != 0 {
		t.Fatal("peer userinfo answered a stranger outside the town directory")
	}

	storeRuntimePartyState(source, alignedcmd.PartyState{
		PartyID:    1,
		UserID:     1,
		IsLeader:   true,
		MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 1, Slot: 0},
			{UserID: 5, Slot: 1},
		},
	})
	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{5, 0, currentPeerUserInfoMode},
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, sourceConn.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(rest) != 0 {
		t.Fatalf("party peer userinfo header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) < 0x17 || packet.Body[0] != currentPeerUserInfoMode ||
		binary.LittleEndian.Uint16(packet.Body[0x0d:0x0f]) != 5 || packet.Body[4] != 42 {
		t.Fatalf("party peer mode3 body head=%x", packet.Body[:min(len(packet.Body), 32)])
	}
}

// The op7 prompt's two panels must open for anyone registered in the same
// channel even from another area of the town. The cross-area refusal is what the
// operator reported as dead buttons on 2026-09-26 (op1403 at 08:33:33 and op8 at
// 08:33:34, both answered with target_not_in_same_area_and_not_party_member).
func TestCurrentPeerInfoTargetVisibleAdmitsSameChannelAcrossAreas(t *testing.T) {
	channel := channelcatalog.Channel{ID: 42}
	otherChannel := channelcatalog.Channel{ID: 43}
	service := &Service{onlinePlayers: newOnlinePlayerManager()}
	newSession := func(id uint16, where channelcatalog.Channel) *gameSession {
		session := &gameSession{conn: &bufferConn{}, channel: where, selectedCharacterID: id}
		service.bindGameSessionCharacter(session, id)
		return session
	}
	leader := newSession(1, channel)
	applicant := newSession(5, channel)
	neighbour := newSession(6, channel)
	stranger := newSession(9, otherChannel)

	// Same channel, one area apart: the reported failure's exact shape.
	// ChannelID is set explicitly because EnterArea copies info.ChannelID rather
	// than deriving it from the session, and two registrations that both carry 0
	// compare as one channel -- which would make this test pass without ever
	// testing a real channel match.
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, ChannelID: 42, TownID: 19, AreaID: 2, Session: leader})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, ChannelID: 42, TownID: 19, AreaID: 3, Session: applicant})
	if service.onlinePlayers.PeerInSameArea(1, 5) {
		t.Fatal("fixture registered co-presence, so the cross-area case is not under test")
	}
	if !service.currentPeerInfoTargetVisible(leader, 1, 5) {
		t.Fatal("same-channel peer in another area was refused")
	}

	// The old area scope must not have regressed.
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 6, ChannelID: 42, TownID: 19, AreaID: 2, Session: neighbour})
	if !service.onlinePlayers.PeerInSameArea(1, 6) || !service.currentPeerInfoTargetVisible(leader, 1, 6) {
		t.Fatal("same-area peer was refused")
	}

	// Another channel is still out of scope.
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 9, ChannelID: 43, TownID: 19, AreaID: 2, Session: stranger})
	if service.currentPeerInfoTargetVisible(leader, 1, 9) {
		t.Fatal("a character in another channel was admitted")
	}

	// Never registered anywhere: the directory is the authority, so still out.
	if service.currentPeerInfoTargetVisible(leader, 1, 4242) {
		t.Fatal("an unregistered character was admitted")
	}
}

// The same relation end to end through the op8 handler the 查看信息 button sends:
// same channel, different areas, no party yet, and the panel must be answered
// rather than silently dropped.
func TestHandleCurrentPeerUserInfoAnswersAcrossAreasInTheSameChannel(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, account := range []dnfrepo.AccountRecord{
		{AccountID: "dnf:1", State: "active"},
		{AccountID: "dnf:2", State: "active"},
	} {
		if err := repositories.Account.Save(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:1", Name: "leader", Job: "0", Level: 90},
		{CharacterID: "5", AccountID: "dnf:2", Name: "applicant", Job: "1", Level: 90},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	sourceConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 42}
	source := &gameSession{conn: sourceConn, channel: channel, accountID: "dnf:1", selectedCharacterID: 1, townActorOwnerChannel: 42}
	target := &gameSession{conn: &bufferConn{}, channel: channel, accountID: "dnf:2", selectedCharacterID: 5, townActorOwnerChannel: 42}
	service := &Service{
		options:             options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)
	// ChannelID explicitly, for the same reason as the predicate test above:
	// EnterArea copies info.ChannelID, so leaving it unset would make this pass on
	// 0 == 0 instead of on a real same-channel registration.
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1, ChannelID: 42, TownID: 19, AreaID: 2, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 5, ChannelID: 42, TownID: 19, AreaID: 3, Session: target})
	if service.onlinePlayers.PeerInSameArea(1, 5) {
		t.Fatal("fixture registered co-presence, so the cross-area case is not under test")
	}

	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		[]byte{5, 0, currentPeerUserInfoMode},
	); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, sourceConn.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(rest) != 0 {
		t.Fatalf("cross-area peer userinfo header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) < 0x17 || packet.Body[0] != currentPeerUserInfoMode ||
		binary.LittleEndian.Uint16(packet.Body[0x0d:0x0f]) != 5 {
		t.Fatalf("cross-area peer mode3 body head=%x", packet.Body[:min(len(packet.Body), 32)])
	}
}
