package dnfbridge

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// The two reported symptoms share one cause: the receiving client resolves a
// remote character through its own user table, which only actor creation fills,
// and the server left two whole classes of peer out of that table.

func newPeerActorResolutionService() *Service {
	return &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:         make(map[uint32]*runtimeRaidState),
		onlinePlayers: newOnlinePlayerManager(),
	}
}

// A raid channel's waiting area registers with InstanceKey == raidKey, so the
// public-town pass cannot see it. Before the fix the member standing on the deck
// never received the leader's record and the roster lost its portrait.
func TestChannelRaidInstancePlayersReachesInstanceKeyedAreas(t *testing.T) {
	manager := newOnlinePlayerManager()
	channelID := uint16(23)
	raidKey := uint32(23)<<16 | 1

	manager.EnterArea(&onlinePlayerInfo{
		CharacterID: 1001, ChannelID: channelID, TownID: 19, AreaID: 2, InstanceKey: raidKey,
		Session: &gameSession{conn: &bufferConn{}},
	})
	manager.EnterArea(&onlinePlayerInfo{
		CharacterID: 1002, ChannelID: channelID, TownID: 19, AreaID: 1, InstanceKey: 0,
		Session: &gameSession{conn: &bufferConn{}},
	})
	// A member of a different raid instance must not leak in.
	manager.EnterArea(&onlinePlayerInfo{
		CharacterID: 1003, ChannelID: channelID, TownID: 19, AreaID: 2, InstanceKey: raidKey + 1,
		Session: &gameSession{conn: &bufferConn{}},
	})

	if got := manager.GetChannelTownInstancePlayers(channelID, 0); len(got) != 1 || got[0].CharacterID != 1002 {
		t.Fatalf("public-town pass returned %+v, want only 1002", got)
	}
	got := manager.GetChannelRaidInstancePlayers(channelID, raidKey, 0)
	if len(got) != 1 || got[0].CharacterID != 1001 {
		t.Fatalf("raid-instance pass returned %+v, want only 1001", got)
	}
	if got := manager.GetChannelRaidInstancePlayers(channelID, raidKey, 1001); len(got) != 0 {
		t.Fatalf("exclusion ignored: %+v", got)
	}
	if got := manager.GetChannelRaidInstancePlayers(channelID, 0, 0); got != nil {
		t.Fatalf("instance key 0 must stay with the public pass: %+v", got)
	}
}

// A character inside a Seria room is deliberately absent from the presence map,
// but the op7 prompt still needs a record for it.
func TestPeerActorInfoFallsBackToBoundSessionOutsidePresence(t *testing.T) {
	service := newPeerActorResolutionService()
	channel := channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 1023}
	session := &gameSession{conn: &bufferConn{}, channel: channel}
	service.bindGameSessionCharacter(session, 1003)

	// Nothing was registered in the presence map: this is the private-room state.
	if _, present := service.onlinePlayers.PlayerForCharacter(1003); present {
		t.Fatal("fixture character is unexpectedly in the public presence map")
	}
	info, ok := service.peerActorInfoForCharacter(1003)
	if !ok {
		t.Fatal("no actor record for a bound character outside public presence")
	}
	if info.CharacterID != 1003 || info.Session != session || info.ChannelID != 23 {
		t.Fatalf("fallback record=%+v", info)
	}
	if _, ok := service.peerActorInfoForCharacter(4242); ok {
		t.Fatal("an unknown character produced an actor record")
	}
}

// The public-presence record must still win when it exists, so the send path keeps
// the peer's real coordinates.
func TestPeerActorInfoPrefersPublicPresence(t *testing.T) {
	service := newPeerActorResolutionService()
	channel := channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 1023}
	session := &gameSession{conn: &bufferConn{}, channel: channel}
	service.bindGameSessionCharacter(session, 1001)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1001, ChannelID: 23, TownID: 19, AreaID: 1, PositionX: 819, PositionY: 172, Session: session,
	})

	info, ok := service.peerActorInfoForCharacter(1001)
	if !ok || info.PositionX != 819 || info.AreaID != 1 {
		t.Fatalf("public presence not preferred: ok=%t info=%+v", ok, info)
	}
}

// The browse window's own roster push must go through the same fallback: reading
// the presence map directly meant a member standing in a Seria room produced no
// actor record, and the browsing window rendered that row with an empty Lv cell
// while the roster packet itself was fine.
func TestRuntimeRaidBrowseListKeepsGoingWhenAMemberIsOutsidePresence(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	// Move the leader out of the public presence map without unbinding it: this is
	// the Seria-room state the browse window renders wrong.
	fixture.service.onlinePlayers.LeaveArea(1001)
	if _, present := fixture.service.onlinePlayers.PlayerForCharacter(1001); present {
		t.Fatal("fixture member is still in the public presence map")
	}
	if _, ok := fixture.service.peerActorInfoForCharacter(1001); !ok {
		t.Fatal("presence-free member no longer resolves to an actor record")
	}

	fixture.outside.conn.(*bufferConn).write.Reset()
	if err := fixture.service.sendRuntimeRaidBrowseList(
		fixture.outside,
		raid.RequestRaidInfoRequest{RaidKey: fixture.raidKey, Mode: 1},
	); err != nil {
		t.Fatalf("browse list: %v", err)
	}
	packets := runtimeRaidPeerTake(t, fixture.outside)
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("browsing client received no roster")
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidOtherChannelUserinfoMsgID); !ok {
		t.Fatal("browsing client received no 819 key list")
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidEntryCostMsgID); !ok {
		t.Fatal("browsing client received no material snapshot")
	}
}

// A browsing outsider's op658 must be answered read-only instead of refused: the
// client learns which item the raid charges from that ACK, while the 0x0256 rows
// only say who has it. Verified on the wire that 0x0256 already carried
// field=1/value=1 for a browsed raid while the 入场材料 column still read "-".
func TestRuntimeRaidBrowseEntryCostInfoAnswersOutsiderReadOnly(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	fixture.browse(t)
	fixture.outside.conn.(*bufferConn).write.Reset()

	typ := uint16(dnfenum.CmdPacketRaidEntryCostInfo)
	if err := fixture.service.handleOnlineRaidEntryCostInfo(fixture.outside, typ, []byte{1}); err != nil {
		t.Fatalf("browsing 658: %v", err)
	}

	packets := runtimeRaidPeerTake(t, fixture.outside)
	ack, ok := runtimeRaidPeerFind(packets, typ)
	if !ok {
		t.Fatal("browsing outsider received no 658 answer")
	}
	if len(ack.Body) != 1 || ack.Body[0] != 1 {
		t.Fatalf("658 body=%x want the single success byte", ack.Body)
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidWaitingListMsgID); !ok {
		t.Fatal("browsing outsider received no 0x0250 waiting list")
	}
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidEntryCostMsgID); !ok {
		t.Fatal("browsing outsider received no 0x0256 material snapshot")
	}
	// Nothing in the answer may claim the raid: the object and the binding stay out.
	for _, msgID := range []uint16{raid.RaidObjectMsgID, uint16(dnfenum.CmdPacketCreateRaid)} {
		if _, claimed := runtimeRaidPeerFind(packets, msgID); claimed {
			t.Fatalf("read-only 658 answer sent claim packet %d", msgID)
		}
	}
}

func TestRuntimeRaidEntryCostInfoStillRefusesAnUnrelatedOutsider(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	// No browse window was opened, so there is no raid to describe.
	fixture.outside.conn.(*bufferConn).write.Reset()

	typ := uint16(dnfenum.CmdPacketRaidEntryCostInfo)
	if err := fixture.service.handleOnlineRaidEntryCostInfo(fixture.outside, typ, []byte{1}); err != nil {
		t.Fatalf("unrelated 658: %v", err)
	}
	ack, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.outside), typ)
	if !ok {
		t.Fatal("no 658 answer")
	}
	if len(ack.Body) != 2 || ack.Body[0] != 0 {
		t.Fatalf("658 body=%x want the failure body", ack.Body)
	}
}

func TestRuntimeRaidKeyForMemberReportsRaidKey(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	if got := fixture.service.runtimeRaidKeyForMember(1001); got != fixture.raidKey {
		t.Fatalf("member key=%d want %d", got, fixture.raidKey)
	}
	if got := fixture.service.runtimeRaidKeyForMember(4242); got != 0 {
		t.Fatalf("non-member key=%d want 0", got)
	}
}

// A prompt whose responder is in a private scene and whose requester is in the
// public town must still reach the responder. The actor records go out first, so
// the client can resolve the op7 source when the packet lands.
func TestRuntimeRaidPeerPromptSurvivesAPrivateSceneRequester(t *testing.T) {
	fixture := newRuntimeRaidPeerFixture(t)
	// The outsider is the applicant; drop it out of the public presence map to
	// model standing inside its own Seria room.
	fixture.service.onlinePlayers.LeaveArea(1003)
	if _, present := fixture.service.onlinePlayers.PlayerForCharacter(1003); present {
		t.Fatal("applicant is still registered in the public presence map")
	}

	if err := fixture.service.handleOnlineRequestPeer(
		fixture.outside,
		uint16(dnfenum.CmdPacketRequestPeer),
		runtimeRaidPeerRequestBody(1001),
	); err != nil {
		t.Fatalf("application from a private scene: %v", err)
	}

	packets := runtimeRaidPeerTake(t, fixture.leader)
	if _, ok := runtimeRaidPeerFind(packets, 0x0007); !ok {
		t.Fatal("leader received no op7 prompt")
	}
	// The admission itself is unchanged and must still complete.
	runtimeRaidPeerTake(t, fixture.outside)
	if err := fixture.service.handleOnlineResponsePeer(
		fixture.leader,
		uint16(dnfenum.CmdPacketResponsePeer),
		runtimeRaidPeerResponseBody(1003, false),
	); err != nil {
		t.Fatalf("leader accept: %v", err)
	}
	if runtimeRaidMemberIndex(*fixture.service.raids[fixture.raidKey], 1003) < 0 {
		t.Fatal("applicant was not admitted")
	}
}

// newRaidApplicationActorFixture 给申请链路准备一个**有仓库**的夹具：actor 投影要读角色记录，
// 没有仓库时投影会静默失败（老测试只断言 op7，所以一直没暴露）。
func newRaidApplicationActorFixture(t *testing.T) (*Service, *gameSession, *gameSession, uint32) {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1003"} {
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
			CharacterID: characterID,
			AccountID:   "dnf:1",
			Name:        "char" + characterID,
			Level:       90,
		}); err != nil {
			t.Fatalf("seed character %s: %v", characterID, err)
		}
	}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		onlinePlayers:      newOnlinePlayerManager(),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10238}
	leader := &gameSession{conn: &bufferConn{}, channel: channel}
	applicant := &gameSession{conn: &bufferConn{}, channel: channel}
	// 场景属于本频道 → "绑定"形态可用；申请那颗记录现在必须走"仅记录"形态。
	leader.townActorOwnerChannel = byte(channel.ID)
	applicant.townActorOwnerChannel = byte(channel.ID)
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(applicant, 1003)
	const raidKey = uint32(0x00c80902)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      1001,
		TailFlag:      5,
		NameBytes:     []byte("anton"),
		Members:       []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0)},
		FrozenMembers: []runtimeRaidMemberState{runtimeRaidSquadProjectionMember(1001, 1, 0)},
	}
	service.gameSessions = map[uint16]*gameSession{1001: leader, 1003: applicant}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1001, ChannelID: 200, TownID: 19, AreaID: 1, Session: leader})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 1003, ChannelID: 200, TownID: 19, AreaID: 1, Session: applicant})
	return service, leader, applicant, raidKey
}

// 申请只是申请：申请那一刻只该给团长装"申请人能被我解析"所需的那一条 mode0 记录，
// 不该把申请人的 mode1 绑定半、更不该把团长自己的记录建给申请人。
//
// 服主实测（2026-09-26 13:56:43，raid 1507329）：申请后只有团长卡一下。服务端在提示框
// 之前串行做了这些活，合计 121ms（提示框 43.440 才发出去，申请包 43.319 到达）：
//
//	43.321 读申请人角色记录 → 43.334 建申请人 mode0（读装备仓库 14 槽）→ 43.346 PVF 属性
//	→ 43.349 建申请人 mode1 装备行(3) → 43.353 发给团长
//	→ 43.376 建【团长自己】mode0（14 槽）→ 43.436 建【团长自己】mode1 装备行(15 行，带耐久/品级)
//	→ 43.440 才把 13 字节的申请提示框发给团长
//
// 也就是说 87ms 花在"把团长自己的记录装给申请人"上，而申请人 1.3 秒前浏览攻坚队列表时
// 就已经拿到团长的记录了（13:56:42.030，且是 163 字节的轻量形态）。这条测试钉住新的形状：
// 团长侧恰好一条 msg2（mode0，不再有 mode1），申请人侧一条都没有；提示框照旧在。
func TestRaidApplicationInstallsOnlyTheApplicantsRecordForThePrompt(t *testing.T) {
	service, leader, applicant, _ := newRaidApplicationActorFixture(t)
	if err := service.handleOnlineRequestPeer(
		applicant,
		uint16(dnfenum.CmdPacketRequestPeer),
		runtimeRaidPeerRequestBody(1001),
	); err != nil {
		t.Fatalf("application: %v", err)
	}

	leaderPackets := runtimeRaidPeerTake(t, leader)
	if len(leaderPackets) == 0 {
		t.Fatal("the leader received nothing for an application")
	}
	if _, ok := runtimeRaidPeerFind(leaderPackets, 0x0007); !ok {
		t.Fatal("the leader received no op7 prompt; the applicant's record must still be installed first")
	}
	recordPackets := 0
	for _, packet := range leaderPackets {
		if packet.Header.MsgID == 2 {
			recordPackets++
		}
	}
	// mode0 一条（记录）+ mode1 一条（绑定半）= 改前的两条；现在只发 mode0。
	if recordPackets != 1 {
		t.Fatalf("the leader received %d actor packets for one application, want exactly the mode0 record", recordPackets)
	}
	for _, packet := range runtimeRaidPeerTake(t, applicant) {
		if packet.Header.MsgID == 2 {
			t.Fatalf("a mere application built the leader's actor record for the applicant: msg=%d body=% X",
				packet.Header.MsgID, packet.Body)
		}
	}
}

// 入队被接受之后，双方仍然互相拿到对方的 actor 记录（名册头像/名字靠它解析），
// 但**每个方向恰好一条、且只有 mode0 那一条**：不再带 mode1 绑定半，也不再有重复的第二遍。
//
// 改前的现场（raid 1507329，2026-09-26 17:18:06Z）：每个方向两条、字节数完全相同 ——
// 申请人侧 472+1373 各出现两次（.404 与 .529），团长侧 459+679 各出现两次（.434 与 .463）；
// 6674 字节的一次"通过"里 2983 字节是原样重发，另有两次 14 格装备读取（mode1 投影）。
// 原因：syncRuntimeRaidMateActors 一次调用就已经装了两个方向（online_player_manager.go
// 的循环），而这条路径为申请人和团长各调了一次。
//
// 这里刻意让申请人对共现表不可见（赛丽亚房间那种私有场景）：同区域时 sync 有"共现已经装过
// 就别重复"的短路，测不到这一次安装；不可见时它才会真的装。
func TestRaidAdmissionStillInstallsBothMateRecords(t *testing.T) {
	service, leader, applicant, raidKey := newRaidApplicationActorFixture(t)
	service.onlinePlayers.LeaveArea(1003)
	if _, present := service.onlinePlayers.PlayerForCharacter(1003); present {
		t.Fatal("applicant is still registered in the public presence map")
	}
	if err := service.handleOnlineRequestPeer(
		applicant,
		uint16(dnfenum.CmdPacketRequestPeer),
		runtimeRaidPeerRequestBody(1001),
	); err != nil {
		t.Fatalf("application: %v", err)
	}
	runtimeRaidPeerTake(t, leader)
	runtimeRaidPeerTake(t, applicant)

	if err := service.handleOnlineResponsePeer(
		leader,
		uint16(dnfenum.CmdPacketResponsePeer),
		runtimeRaidPeerResponseBody(1003, false),
	); err != nil {
		t.Fatalf("leader accept: %v", err)
	}
	if runtimeRaidMemberIndex(*service.raids[raidKey], 1003) < 0 {
		t.Fatal("applicant was not admitted")
	}
	for _, session := range []*gameSession{leader, applicant} {
		records := 0
		for _, packet := range runtimeRaidPeerTake(t, session) {
			if packet.Header.MsgID == 2 {
				records++
			}
		}
		// 一条 = 装到了（这条断言原来的用意）+ 没有 mode1（mode1 是第二条 msg2）+ 没有重发。
		if records != 1 {
			t.Fatalf("admission installed %d actor packets for one member, want exactly one mode0 record per direction", records)
		}
	}
}
