package dnfbridge

import (
	"context"
	"os"
	"slices"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 超时空：走进一个有怪的区域时，**这一格里的所有人都要一起进副本**，
// 与攻坚小队编制无关；编制本身一个字不动（进去的是一支临时普通队伍）。
//
// 官方口径（九游 2018-06-15《超时空之战机制解读》）：
//
//	「攻坚开始后，处在同一地图的玩家进入副本时自动组队，第一个进入新副本的玩家为队长」
//	「每个房间上限4人，达到人数上限后其他人无法进入」 —— 就是 PVF 的 [MEMBER MAX]。
//
// 客户端自己的提示也是这句：「移动后，会与处于当前城镇的同伴一同开始战斗」。
//
// 真机报文（2026-10-01 10:36:10，char 22 按下进图）：
//
//	RECV op36 town=98 area=6 x=177 y=597 dir=5 opaque=(98, 3)
//	              ^^^^^^ 要打的那个区域（副本 4954）
//	                                       ^^^^^^^^^^^^^ 他**按下进图时站的格子**
//
// 所以这份用例的输入就是真机那份：两个人站在格子 3（Town01），
// 其中一个人请求进 area 6（3 → 6 是 PVF 里真实的相邻关系）。
func TestRealPVFFiendWarMoveEncounterTakesTheWholeArea(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	f := newRuntimeRaidRewardTestFixture(t, true)
	s := f.service
	s.options.pvfPath = path
	if err := s.preloadDungeonWorldMap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.preloadTownCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, err := s.runtimeRaidFiendWarConfig()
	if err != nil {
		t.Fatal(err)
	}
	state := s.raids[f.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.AntonConfig, state.FiendWarConfig = nil, config
	state.Lifecycle, err = newRuntimeRaidLifecycleStateFromModeAndType(
		config.Battle.Raid, runtimeRaidOwnerFiendWar, f.now, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	state.Lifecycle.FiendWar, err = raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatal(err)
	}
	b := state.Lifecycle.FiendWar
	b.GroupAreas[1] = int(currentFiendWarWaitingAreaID)
	b.GroupAreas[2] = int(currentFiendWarWaitingAreaID)
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	// 区域 6 拥有副本 4954，且站着一只守军 —— "这个区域要打"是 fixture 的事实。
	b.Areas[6] = raid.FiendWarAreaState{Monster: raid.FiendWarUnit{ID: 1, Type: 1}}
	syncRuntimeFiendWarObjectives(state)

	s.onlinePlayers = newOnlinePlayerManager()
	for i, peer := range []*gameSession{f.leader, f.member} {
		peer.accountID = "account-" + strconv.Itoa(i+1)
		peer.channel = channelcatalog.Channel{ID: 261, Type: 42}
		peer.selectPreviewObjectStateSent, peer.currentSceneObjectListSent = true, true
		if err := f.repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: strconv.Itoa(int(peer.selectedCharacterID)),
			AccountID:   peer.accountID,
			Name:        "member",
			Level:       90,
			Stats: map[string]int64{
				"fatigue": 156, "town_id": 98, "area_id": 3,
				"pos_x": 240, "pos_y": 484, "direction": 5, "area_state": 0,
			},
		}); err != nil {
			t.Fatal(err)
		}
		quests := dnfrepo.QuestRecord{
			CharacterID: strconv.Itoa(int(peer.selectedCharacterID)),
			States:      map[int64]dnfrepo.QuestState{},
		}
		for _, id := range []int64{2, 3, 6, 10, 9, 14} {
			area, found := s.townArea(98, id)
			if !found {
				t.Fatalf("waiting town area %d missing from PVF", id)
			}
			for _, quest := range area.NeedQuests {
				quests.States[quest] = dnfrepo.QuestState{Status: "completed"}
			}
		}
		if err := f.repositories.Quest.Save(context.Background(), quests); err != nil {
			t.Fatal(err)
		}
		// 两个人都站在格子 3（98/3, 240,484）：这就是"同格的同伴"。
		bindDungeonSelectorOriginForTestAt(t, s, peer, 98, 3, 240, 484)
		registerPartyPeerEndpointForTest(peer, byte(i+2), uint16(5100+i))
		seedPartyOwnerActorProjectionForTest(t, s, peer, peer.selectedCharacterID, 0)
		// 进图名单按**实时位置**取自 area 实例表（runtimeRaidFiendWarAreaRoster →
		// onlinePlayers.GetAreaInstancePlayers）。真机上每一次 op36 都会把人写进
		// 这张表（town_move.go 的 EnterArea），所以 fixture 必须照样登记，否则
		// "同格的同伴"在服务端眼里根本不存在。
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: peer.selectedCharacterID,
			ChannelID:   townPresenceChannelID(peer),
			TownID:      98,
			AreaID:      3,
			InstanceKey: state.RaidKey,
			PositionX:   240,
			PositionY:   484,
			Session:     peer,
		})
	}
	if err := s.applyRuntimeRaidParties(*state); err != nil {
		t.Fatal(err)
	}
	// 进图单位是**编制小队**（操作员 2026-10-01 定案：不做附近自动组队，按小队编制走）。
	// 所以这份输入就是"两个人同一小队、站在同一格"：1001 和 1002 都在 1 队。
	state.Members[0].GroupIndex, state.Members[0].SlotOrder = 1, 0
	state.Members[1].GroupIndex, state.Members[1].SlotOrder = 1, 1
	state.FrozenMembers[0].GroupIndex, state.FrozenMembers[0].SlotOrder = 1, 0
	state.FrozenMembers[1].GroupIndex, state.FrozenMembers[1].SlotOrder = 1, 1
	if got := runtimeRaidNormalizedGroup(state.Members[1].GroupIndex); got != 1 {
		t.Fatalf("fixture 没造出同队输入：1002 在 %d 队", got)
	}
	b.MemberAreas[1001], b.MemberAreas[1002] = 3, 3
	b.GroupAreas[1] = 3
	syncRuntimeFiendWarObjectives(state)

	// **按下进图的是 1002**（小队里的第二个人）：他才是这一场的驱动者
	// （官方口径「第一个进入新副本的玩家为队长」）。
	mover := f.member
	leader := f.leader

	// [B] 队友被搬进副本前，每个人都要过 raid_current_compat.go 的
	// validateAntonRaidDungeonEntry。它原来只认 MemberArea(**本人**)，于是
	// "两个人同队同格、一起进图"里的第二个人必被拒：同伴按设计留在原格，
	// 本人的账本永远对不上目标区域。现在"本人所在小队已经走进目标区域"
	// （GroupAreas[小队] == 目标区域）也算数。
	dungeonID := int64(b.Config.Areas[6].Dungeon)
	if err := s.validateAntonRaidDungeonEntry(leader, dungeonID); err == nil {
		t.Fatal("小队还没走进区域 6，门禁就放行了：这条放松变成了任何人都能进的洞")
	}
	// 真机上这一脚由 town_move.go 先落地：moveRuntimeFiendWarGroup 只把**移动的人**
	// 和小队记进目标区域，其他队员的 MemberAreas 留在他们当时站的格子上。
	if _, err := s.moveRuntimeFiendWarGroup(mover.selectedCharacterID, 6); err != nil {
		t.Fatalf("move ledger: %v", err)
	}
	b = state.Lifecycle.FiendWar
	if b.GroupAreas[1] != 6 || b.MemberAreas[mover.selectedCharacterID] != 6 {
		t.Fatalf("账本没把小队列进区域 6：group=%v driver=%d",
			b.GroupAreas, b.MemberAreas[mover.selectedCharacterID])
	}
	if got := b.MemberAreas[leader.selectedCharacterID]; got != 3 {
		t.Fatalf("同伴的账本被一起搬走了：peer area = %d, want 3（按设计他留在原格）", got)
	}
	if err := s.validateAntonRaidDungeonEntry(leader, dungeonID); err != nil {
		t.Fatalf("同队同伴站在原格、小队已走进区域 6，门禁仍然拒绝：%v", err)
	}
	// D2 的决定不受影响：没编制的人（Seat.Group == 0）依旧进不来。
	setGroup := func(members []runtimeRaidMemberState, id uint16, group byte) byte {
		for i := range members {
			if members[i].CharID != id {
				continue
			}
			previous := members[i].GroupIndex
			members[i].GroupIndex = group
			return previous
		}
		return 0
	}
	liveGroup := setGroup(state.Members, leader.selectedCharacterID, 0)
	frozenGroup := setGroup(state.FrozenMembers, leader.selectedCharacterID, 0)
	if err := s.validateAntonRaidDungeonEntry(leader, dungeonID); err == nil {
		t.Fatal("未编队成员被放进了超时空副本：GroupAreas[0] 应当是零值")
	}
	setGroup(state.Members, leader.selectedCharacterID, liveGroup)
	setGroup(state.FrozenMembers, leader.selectedCharacterID, frozenGroup)

	// [A] 小队队长由操作员排定（名册序第一的 1001）。这一场按下进图的却是 1002：
	// 临时队伍必须跟着驱动者走，但小队队长标志不许跟着换。
	state.SquadPartyLeaders = map[byte]uint16{1: leader.selectedCharacterID}

	// 进图前的编制快照：整段流程一个字都不许改它。
	beforeSquads := []byte{state.Members[0].GroupIndex, state.Members[1].GroupIndex}

	leader.conn.(*bufferConn).write.Reset()
	mover.conn.(*bufferConn).write.Reset()

	// 真机报文：目标 area 6，站的格子 3 在 opaque 里。
	request := currentTownSetUserAreaRequest{
		TownID: 98, AreaID: 6, PositionX: 177, PositionY: 597, OpaqueU16A: 98, OpaqueU16B: 3,
	}
	selfRow := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(mover.selectedCharacterID),
		Value1:              177,
		Value2:              597,
		Value3:              currentAntonRaidDirection,
		Value4:              currentAntonRaidAreaState,
	}
	body, err := buildCurrentSceneTransitionBody(98, 6, []currentSceneTransitionRow{selfRow})
	if err != nil {
		t.Fatal(err)
	}

	// queueRuntimeFiendWarMoveEncounterLocked 的约定：调用者持 townMu，
	// 它自己会先放锁、返回前再拿回来。
	mover.townMu.Lock()
	handled, err := s.queueRuntimeFiendWarMoveEncounterLocked(mover, request, selfRow, body)
	mover.townMu.Unlock()
	if err != nil {
		t.Fatalf("area encounter pass: %v", err)
	}
	if !handled {
		t.Fatal("走进有怪的区域没有触发遭遇战：根本进不了副本")
	}

	// 同一个小队的同伴被一起带进副本：临时普通队伍（op11/op9）+ 副本选择（op153）。
	// 按编制走之后，编出来的就是这一支队伍本身。
	written := map[uint16]bool{}
	for raw := append([]byte(nil), leader.conn.(*bufferConn).write.Bytes()...); len(raw) != 0; {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		written[packet.Header.MsgID] = true
	}
	if !written[11] || !written[9] {
		t.Fatalf("同队的同伴没有拿到临时队伍（op11/op9）；msg ids=%v", written)
	}
	if !written[153] {
		t.Fatalf("同队的同伴没有被带进副本（没有 op153 副本选择）；msg ids=%v", written)
	}
	// 按下进图的那个人自己也必须真的被带着走 —— 否则就是"同伴进去了、我留在原地"。
	// （这里只看报文：本用例的 fixture 没有真实副本场景，提交 runtime 不是它的职责。）
	moverWritten := map[uint16]bool{}
	for raw := append([]byte(nil), mover.conn.(*bufferConn).write.Bytes()...); len(raw) != 0; {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		moverWritten[packet.Header.MsgID] = true
	}
	if !moverWritten[11] || !moverWritten[9] || !moverWritten[153] {
		t.Fatalf("按下进图的人没有被带进副本；msg ids=%v", moverWritten)
	}

	// 编制不变：攻坚名册还是"1001 在 1 队、1002 在 2 队"，谁都没被并队。
	after := s.raids[f.raidKey]
	if after.Members[0].GroupIndex != beforeSquads[0] || after.Members[1].GroupIndex != beforeSquads[1] {
		t.Fatalf("攻坚编制被改了：live=%d/%d want %d/%d；同一个 state 对象=%t；本地 state=%d/%d",
			after.Members[0].GroupIndex, after.Members[1].GroupIndex, beforeSquads[0], beforeSquads[1],
			after == state, state.Members[0].GroupIndex, state.Members[1].GroupIndex)
	}
	if len(after.Members) != 2 || len(after.FrozenMembers) != 2 {
		t.Fatalf("名册行数变了：live=%d frozen=%d", len(after.Members), len(after.FrozenMembers))
	}

	// [A]+[D] 临时队伍：**由按下进图的人领队，并且把同格里站着的人一起带上**。
	//
	// 这是整条链路的验收点。修好之前这里是空的（solo）。现在应该是
	// party.UserID == 驱动者、成员恰好是这一格里登记的两个人。
	//
	// 纪律说明：进图提交成功时队伍要**留着**（真机上一直留到队友进图为止，
	// packet log 2026-10-01 20:56:48 的 party_id=4 slot_count=2 就是它）；
	// 只有提交失败才允许被拆掉，而且必须拆干净。
	mover.dungeon.mu.Lock()
	committed := mover.dungeon.runtime != nil
	mover.dungeon.mu.Unlock()
	party := runtimePartyStateSnapshot(mover)
	if committed {
		if party.PartyID <= 0 {
			t.Fatalf("进图已提交，临时队伍却不见了：%+v", party)
		}
		if party.UserID != mover.selectedCharacterID {
			t.Fatalf("临时队伍的队长是 %d，want 按下进图的 %d；party=%+v",
				party.UserID, mover.selectedCharacterID, party)
		}
		if len(party.Members) != 2 {
			t.Fatalf("同格里的两个人没有一起进临时队伍：%+v", party.Members)
		}
		var ids []uint16
		for _, m := range party.Members {
			ids = append(ids, m.UserID)
		}
		if !slices.Contains(ids, mover.selectedCharacterID) || !slices.Contains(ids, leader.selectedCharacterID) {
			t.Fatalf("临时队伍的成员不是同格的两个人：%v", ids)
		}
	} else {
		if party.PartyID != 0 {
			t.Fatalf("副本没提交却留下了临时队伍：%+v", party)
		}
		if peer := runtimePartyStateSnapshot(leader); peer.PartyID != 0 {
			t.Fatalf("同伴身上留下了临时队伍：%+v", peer)
		}
	}
	// 临时队伍的队长让给了驱动者，但**攻坚队的小队队长标志不许跟着换**：
	// 小队队长是操作员在攻坚队管理器里排的编制事实，不该因为"这次是谁按的进图"
	// 而改手。（这条同时钉住 restoreRuntimeFiendWarSquadPartyLeader：去掉它，
	// 名册序重排会把 1002 写进 SquadPartyLeaders，这里就会看到 1002。）
	if got := after.SquadPartyLeaders[1]; got != leader.selectedCharacterID {
		t.Fatalf("小队队长标志被这一场进图改手了：squad 1 leader = %d, want %d (table=%v)",
			got, leader.selectedCharacterID, after.SquadPartyLeaders)
	}
}

// [A] 的机制面：进图这一场的名册序必须由**驱动者**领队。
//
// runtimeRaidGroupPartyState 拿 members[0] 当队伍的 UserID 与 0 号座位，而
// prepareRuntimePartyDungeonEntry 只在"驱动进图的 session 就是队伍队长"时才把队友
// 搬进同一个副本（dungeon_navigation_runtime.go:753：
// `state.UserID != leader.selectedCharacterID → return nil, nil`）；
// RuntimePartySnapshot.StateFor 又给**每个**成员的 UserID 都是 snapshot.Leader
// （runtime_manager.go:652）。
//
// 所以"谁驱动这一场"必须排在名册最前面。原来它总是名册序第一（SlotOrder 最小），
// 只要按下进图的不是那个人，整支临时队伍就会在那道守卫上被**静默**丢掉：
// 队友一个都不搬、驱动者一个人进图，日志里连 game-party-dungeon-entry-blocked
// 都不会有 —— 真机 2026-10-01 20:56:48 正是如此。
func TestFiendWarEncounterRosterIsLedByTheDriver(t *testing.T) {
	members := []runtimeRaidMemberState{
		{CharID: 1001, SlotOrder: 0},
		{CharID: 1002, SlotOrder: 1},
		{CharID: 1003, SlotOrder: 2},
	}
	led := runtimeRaidRosterLedBy(members, 1002)
	if len(led) != 3 || led[0].CharID != 1002 || led[1].CharID != 1001 || led[2].CharID != 1003 {
		t.Fatalf("驱动者没有领队、或其余顺序没逐字保留：%+v", led)
	}
	// 入参不许被就地重排：squad.Members 还要用于"这一趟该不该 defer 给名册序第一位"
	// 的判断（tryRuntimeFiendWarEncounterPlanned 里 plan == nil 那条），动它就等于
	// 顺手改了那一处的语义。
	if members[0].CharID != 1001 || members[1].CharID != 1002 || members[2].CharID != 1003 {
		t.Fatalf("入参被就地重排了：%+v", members)
	}
	// 这支名册序真的决定队伍的队长与 0 号座位 —— 也就是决定谁能触发队友 fan-out。
	party := runtimeRaidGroupPartyState(
		runtimeRaidState{RaidKey: 12058625, NameBytes: []byte("raid")}, led)
	if party.UserID != 1002 {
		t.Fatalf("临时队伍的队长 = %d, want 驱动者 1002", party.UserID)
	}
	if len(party.Members) != 3 || party.Members[0].UserID != 1002 || party.Members[0].Slot != 0 {
		t.Fatalf("0 号座位不是驱动者：%+v", party.Members)
	}
	// 驱动者本来就在最前面 / 不在名单里 / 未知：原样返回，不造一个不存在的成员。
	for _, tc := range []struct {
		name   string
		leader uint16
	}{{"already first", 1001}, {"absent", 1099}, {"unknown", 0}} {
		got := runtimeRaidRosterLedBy(members, tc.leader)
		if len(got) != 3 || got[0].CharID != 1001 {
			t.Fatalf("%s：名册被改动了 %+v", tc.name, got)
		}
	}
}

// 同伴没好（离线/还在切场景/坐标没落）时，整格都不进 —— 但要**说出原因**。
// 原来这里全是静默 return，真机上只能看到"走进了区域却没开打"。
func TestFiendWarEncounterPeerRefusalNamesTheReason(t *testing.T) {
	if got := (&Service{}).fiendWarEncounterPeerRefusal(runtimeRaidMemberState{CharID: 4242}, 98, 14); got != "peer_offline" {
		t.Fatalf("离线同伴的拒绝理由 = %q，want peer_offline", got)
	}
}
