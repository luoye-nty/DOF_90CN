package dnfbridge

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 超时空：战况表 1 Hz 心跳不许把"计划中的整队进图"顶成单人进图。
//
// 真机 2026-10-01（本地 00:03:49~00:03:54，分片 002，UTC 16:03:xx）的形状：
//
//	16:03:49.334 char 22 committed town=98 area=4 instance=12058625   ← 两人同格 4
//	16:03:50.815 char 1  transition-send  area=4 row_count=2 peer_row_count=1
//	16:03:50.930 char 1  committed town=98 area=4 instance=12058625
//	16:03:53.743 char 1  RECV op36 {area 7, x419 y601, from 4}        ← 按下进图
//	16:03:53.761 op9 party_id=4 slot_count=1   ← 临时进图队伍只有 1 个人（铁证）
//	16:03:53.790 game-fiendwar-maze-selected area=7                  ← 单人副本开出来了
//	16:03:54.110 game-fiendwar-encounter-entry entered=true
//	（char 22 全程留在格 4，日志里连一行拒绝都没有：area-peer-skipped /
//	 encounter-peer-not-ready / solo-fallback **一条都不存在**）
//
// 42 秒后的第二脚（16:04:35.786 op36 → 16:04:35.811 slot_count=2 →
// 16:04:35.906 member-prepared char_id=22）却是整队进图。两脚的操作一模一样，
// 差别只有一个：**按键落在 1 Hz 心跳刻度之前还是之后**。
//
// 战况表 tick 的节奏是 raid-start(16:03:40.769) → 16:03:43.733 → 之后每 1 秒：
// 刻度落在 …53.73 / …35.73。第一脚的 op36 在 53.743（刻度之后 13ms）——
//
//	计划那一趟（plan != nil）开工前先把**本人的实时城镇坐标推到目标区域 7**
//	（raid_fiendwar_battle.go:setCurrentTownPositionSceneFromTransitionLocked），
//	而副本 runtime 要 ~300ms 后才落地（54.108）。心跳 53.777 正好落在这段窗口里，
//	plan==nil 那一路于是把"我站在 7"当成名单锚点，算出**空名单**、只带自己进图，
//	并把随后到达的整队那一趟顶掉（那时 runtime 已存在，计划那趟直接返回）。
//
// 第二脚的 op36 在 35.786（刻度之后 56ms，刻度先跑）—— 心跳跑的时候人还在格 4，
// 而格 4 没有怪（AreaOpen(4)=false），心跳自然放弃，整队进图才成功。
//
// 这个用例把那段时序按真机顺序摆出来：立牌之后心跳必须让位；牌落之后同一个心跳
// 就会单人开图 —— 后半段是**反证**，它证明"让位"这条守卫是唯一把两者分开的东西。
func TestFiendWarBoardHeartbeatCannotStealAPlannedAreaEntry(t *testing.T) {
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
	// 目标区域 6 拥有副本，且站着一只守军 —— 心跳那一趟的 AreaOpen(6) 就靠它。
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
				"fatigue": 156, "town_id": 38, "area_id": 3,
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
		// 两个人一起站在格子 3：这就是"同格的同伴"。
		bindDungeonSelectorOriginForTestAt(t, s, peer, 98, 3, 240, 484)
		registerPartyPeerEndpointForTest(peer, byte(i+2), uint16(5100+i))
		seedPartyOwnerActorProjectionForTest(t, s, peer, peer.selectedCharacterID, 0)
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
	state.Members[0].GroupIndex, state.Members[0].SlotOrder = 2, 0
	state.Members[1].GroupIndex, state.Members[1].SlotOrder = 1, 1
	state.FrozenMembers[0].GroupIndex, state.FrozenMembers[0].SlotOrder = 2, 0
	state.FrozenMembers[1].GroupIndex, state.FrozenMembers[1].SlotOrder = 1, 1
	b.MemberAreas[f.leader.selectedCharacterID] = 3
	b.MemberAreas[f.member.selectedCharacterID] = 3
	b.GroupAreas[1], b.GroupAreas[2] = 3, 3
	syncRuntimeFiendWarObjectives(state)

	leader := f.leader // 按下进图的人
	peer := f.member   // 同格、按设计等着被搬进去的人

	// 计划那一脚在开工前做的两件事，按真机的先后顺序摆出来：
	//   (1) 把本人的实时城镇坐标推到**目标区域 6**；
	//   (2) 在把计划进图投进本人信箱**之前**立牌。
	selfRow := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(leader.selectedCharacterID),
		Value1:              177,
		Value2:              597,
		Value3:              currentAntonRaidDirection,
		Value4:              currentAntonRaidAreaState,
	}
	leader.townMu.Lock()
	setCurrentTownPositionSceneFromTransitionLocked(leader, leader.selectedCharacterID, 98, 6, selfRow)
	leader.townMu.Unlock()
	s.beginRuntimeFiendWarPlannedEncounter(leader, f.now)

	// [本用例的核心] 心跳此刻落地（真机就是 53.777 那一下）：必须让位。
	//
	// 没有这条守卫时它会单人开图 —— 名单锚点已经是"目标区域 6"，而同格的同伴
	// 还留在格 3，于是名单里只剩触发者自己，日志上一行拒绝都不会有。
	if err := s.tryRuntimeFiendWarEncounter(leader, f.now.Add(time.Millisecond)); err != nil {
		t.Fatalf("heartbeat pass: %v", err)
	}
	leader.dungeon.mu.Lock()
	entered := leader.dungeon.runtime != nil
	leader.dungeon.mu.Unlock()
	if entered {
		t.Fatal("战况表心跳在计划进图还在飞的时候自己开了一场副本：它会单人开图，" +
			"并且把随后到达的整队进图顶掉（真机 2026-10-01 00:03:53 的形状）")
	}

	// [反证] 牌一落（=计划那一趟跑完后的正常状态），同一个心跳立刻就会单人开图。
	// 这一段是刻意的：它证明上一条断言不是"心跳本来就不会开图"这种空断言，
	// 而是这条守卫真的把两者分开了 —— 也正是 00:03:53 真机留下的
	// game-current-party-frame-op9-send party_id=4 slot_count=1。
	//
	// 先补上真机那一脚在遭遇战之前做的第三件事：战场账本也被挪到了目标区域
	// （town_move.go 的 moveRuntimeFiendWarGroup，门禁严格的那一半要求账本对齐 ——
	// 真机上这一步在 op36 落地时就已经跑过了）。
	s.endRuntimeFiendWarPlannedEncounter(leader)
	if _, err := s.moveRuntimeFiendWarGroup(leader.selectedCharacterID, 6); err != nil {
		t.Fatalf("move ledger: %v", err)
	}
	if err := s.tryRuntimeFiendWarEncounter(leader, f.now.Add(2*time.Second)); err != nil {
		t.Fatalf("unprotected heartbeat pass: %v", err)
	}
	leader.dungeon.mu.Lock()
	soloEntered := leader.dungeon.runtime != nil
	leader.dungeon.mu.Unlock()
	peer.dungeon.mu.Lock()
	peerRuntime := peer.dungeon.runtime
	peer.dungeon.mu.Unlock()
	if !soloEntered {
		t.Fatal("反证没成立：牌落下之后心跳应当能自己开图（fixture 太严会让上一条断言失去意义）")
	}
	if peerRuntime != nil {
		t.Fatal("反证没成立：心跳那一趟不该把同伴带走（它算的是目标区域 6 的名单）")
	}
}

// 计划那一趟跑完之后必须立刻落牌，否则战况表心跳会被白挡满整个安全网时长。
func TestFiendWarPlannedEncounterMarkIsClearedByTheEntry(t *testing.T) {
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
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
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
				"fatigue": 156, "town_id": 38, "area_id": 3,
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
		bindDungeonSelectorOriginForTestAt(t, s, peer, 98, 3, 240, 484)
		registerPartyPeerEndpointForTest(peer, byte(i+2), uint16(5100+i))
		seedPartyOwnerActorProjectionForTest(t, s, peer, peer.selectedCharacterID, 0)
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
	b.MemberAreas[f.leader.selectedCharacterID] = 3
	b.MemberAreas[f.member.selectedCharacterID] = 3
	b.GroupAreas[1] = 3
	syncRuntimeFiendWarObjectives(state)

	mover := f.leader
	mover.conn.(*bufferConn).write.Reset()
	f.member.conn.(*bufferConn).write.Reset()
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
	mover.townMu.Lock()
	handled, err := s.queueRuntimeFiendWarMoveEncounterLocked(mover, request, selfRow, body)
	mover.townMu.Unlock()
	if err != nil {
		t.Fatalf("planned encounter pass: %v", err)
	}
	if !handled {
		t.Fatal("计划那一脚没有被当成遭遇战处理")
	}
	if s.runtimeFiendWarPlannedEncounterPending(mover, f.now.Add(3*time.Second)) {
		t.Fatal("计划进图跑完了牌还立着：战况表心跳会被白挡（安全网 5 秒不是正常路径）")
	}
}
