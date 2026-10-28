package dnfbridge

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestFiendWarEncounterSquadResolutionHasDefinedBehaviourForEveryDivergence pins the
// decision table of runtimeRaidFiendWarEncounterSquad.
//
// The two roster rows are NOT interchangeable and the disagreement between them used
// to be a silent dead end: the group was read from the FROZEN row (group 1), the LIVE
// groups were searched for it, nothing matched, and the encounter returned nil
// without a word. Live 2026-10-01, raid 12058625, char 1:
//
//	11:34:24 game-raid-squad-unassigned-on-solo-town-return char_id=1 previous_group=1
//	         group=0 slot=0 frozen_row=kept_for_phase_credit
//	11:35:55 game-town-set-user-area-blocked area_id=10
//	         reason=fiendwar source area requires combat
//
// The area's monster could never be cleared, and FiendWarBattle.Move refuses to let
// anyone walk past an uncleared area, so the member was locked in for good.
func TestFiendWarEncounterSquadResolutionHasDefinedBehaviourForEveryDivergence(t *testing.T) {
	// Two members, both frozen in squad 1 -- the phase roster the failure left alone.
	newState := func(liveSquads [2]byte, liveRows bool) runtimeRaidState {
		live := []runtimeRaidMemberState{
			{CharID: 1001, Name: "leader", GroupIndex: liveSquads[0], SlotOrder: 0},
			{CharID: 1002, Name: "member", GroupIndex: liveSquads[1], SlotOrder: 1},
		}
		if !liveRows {
			live = nil
		}
		return runtimeRaidState{
			RaidKey: 12058625,
			Members: live,
			FrozenMembers: []runtimeRaidMemberState{
				{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
				{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1},
			},
		}
	}

	cases := []struct {
		name        string
		state       runtimeRaidState
		characterID uint16
		wantGroup   byte
		wantSource  runtimeRaidFiendWarSquadSource
		wantRefusal string
		wantMembers int
	}{
		{
			name:        "live row names the squad",
			state:       newState([2]byte{1, 1}, true),
			characterID: 1001,
			wantGroup:   1,
			wantSource:  runtimeRaidFiendWarSquadSourceLive,
			wantMembers: 2,
		},
		{
			name: "a member re-assigned through the raid manager wins over the frozen row",
			// The frozen row still says squad 1; the live row says squad 2, and the
			// live roster really has a squad 2 for him.
			state: runtimeRaidState{
				RaidKey: 12058625,
				Members: []runtimeRaidMemberState{
					{CharID: 1001, Name: "leader", GroupIndex: 2, SlotOrder: 0},
					{CharID: 1002, Name: "member", GroupIndex: 2, SlotOrder: 1},
				},
				FrozenMembers: []runtimeRaidMemberState{
					{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
					{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1},
				},
			},
			characterID: 1001,
			wantGroup:   2,
			wantSource:  runtimeRaidFiendWarSquadSourceLive,
			wantMembers: 2,
		},
		{
			name:        "live row unassigned and frozen row still in squad 1 is a squadless member",
			state:       newState([2]byte{0, 1}, true),
			characterID: 1001,
			wantRefusal: "member_not_in_any_squad",
		},
		{
			name: "live row unassigned and no live squad at all is still squadless",
			// Nobody in the live roster carries squad 1: the absent squad the frozen
			// row names must not be resurrected into a party nobody is in.
			state:       newState([2]byte{0, 0}, true),
			characterID: 1001,
			wantRefusal: "member_not_in_any_squad",
		},
		{
			name:        "no live row for the member at all falls back to the frozen row",
			state:       newState([2]byte{0, 0}, false),
			characterID: 1001,
			wantGroup:   1,
			wantSource:  runtimeRaidFiendWarSquadSourceFrozen,
			wantMembers: 1,
		},
		{
			name: "no live row for the member joins the squad's live peers",
			// The live roster lost this member's row but still holds his squad, so the
			// frozen row puts him back into a party that really exists.
			state: runtimeRaidState{
				RaidKey: 12058625,
				Members: []runtimeRaidMemberState{
					{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1},
				},
				FrozenMembers: []runtimeRaidMemberState{
					{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
					{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1},
				},
			},
			characterID: 1001,
			wantGroup:   1,
			wantSource:  runtimeRaidFiendWarSquadSourceFrozen,
			wantMembers: 2,
		},
		{
			name:        "member absent from the frozen roster is refused by name",
			state:       newState([2]byte{1, 1}, true),
			characterID: 1099,
			wantRefusal: "member_not_in_frozen_roster",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runtimeRaidFiendWarEncounterSquad(tc.state, tc.characterID)
			if got.Refusal != tc.wantRefusal {
				t.Fatalf("refusal = %q, want %q", got.Refusal, tc.wantRefusal)
			}
			if tc.wantRefusal != "" {
				if len(got.Members) != 0 {
					t.Fatalf("refused resolution still carried %d members", len(got.Members))
				}
				return
			}
			if got.Group != tc.wantGroup || got.Source != tc.wantSource || len(got.Members) != tc.wantMembers {
				t.Fatalf("resolution = {group %d source %q members %d}, want {group %d source %q members %d}",
					got.Group, got.Source, len(got.Members), tc.wantGroup, tc.wantSource, tc.wantMembers)
			}
		})
	}
}

// TestRealPVFFiendWarSquadlessMemberIsNotSilentlyDeadlocked is the regression test for
// the live deadlock, driven through the real Script.pvf.
//
// It builds the exact divergent state the live log recorded -- LIVE row squad 0, FROZEN
// row squad 1, the member standing in an open area with the battle model still holding
// him under the frozen squad -- and asserts both halves of the fix:
//
//	(a) the encounter refuses EXPLICITLY (a named log row and a client notice) instead
//	    of silently returning nil, and
//	(b) after the member re-assigns himself in the raid manager the very same arrival
//	    fires the encounter normally.
func TestRealPVFFiendWarSquadlessMemberIsNotSilentlyDeadlocked(t *testing.T) {
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
	// Both members in squad 1, on both rows: the state a squad enters an area from.
	for i := range state.Members {
		state.Members[i].GroupIndex, state.Members[i].SlotOrder = 1, byte(i)
	}
	for i := range state.FrozenMembers {
		state.FrozenMembers[i].GroupIndex, state.FrozenMembers[i].SlotOrder = 1, byte(i)
	}
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
	// Area 14 owns dungeon 4962 and is open because a native defender stands in it.
	// The defender is seeded explicitly instead of relying on the PVF's random orc
	// spawn, so "this area requires combat" is a fact of the fixture.
	b.Areas[14] = raid.FiendWarAreaState{Monster: raid.FiendWarUnit{ID: 1, Type: 1}}
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
				"fatigue": 156, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 0,
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
		bindDungeonSelectorOriginForTestAt(t, s, peer, 98, 14, 450, 234)
		registerPartyPeerEndpointForTest(peer, byte(i+2), uint16(5100+i))
		seedPartyOwnerActorProjectionForTest(t, s, peer, peer.selectedCharacterID, 0)
	}
	if err := s.applyRuntimeRaidParties(*state); err != nil {
		t.Fatal(err)
	}
	b.GroupAreas[1] = 14
	syncRuntimeFiendWarObjectives(state)

	leader := f.leader
	// Both members stand in area 14, which owns dungeon 4962 and holds a native
	// defender: the exact state the live member was in when the encounter stopped
	// firing.
	//
	// peerArea 是**同伴物理上站在哪一格**。区域名单按客户端的实时坐标算
	// （runtimeRaidFiendWarAreaParty），所以这里必须连实时坐标一起搬 ——
	// 只改战场账本的话，同伴在坐标上还站在这一格里，"他一个人在区域里"这个前提
	// 就不成立了。
	reset := func(liveSquad byte, peerArea byte) {
		state.Members[0].GroupIndex, state.Members[0].SlotOrder = liveSquad, 0
		b.MemberAreas[leader.selectedCharacterID] = 14
		b.MemberAreas[f.member.selectedCharacterID] = int(peerArea)
		b.GroupAreas[1] = 14
		bindDungeonSelectorOriginForTestAt(t, s, leader, 98, 14, 450, 234)
		bindDungeonSelectorOriginForTestAt(t, s, f.member, 98, peerArea, 450, 234)
		// 进图名单按**实时位置**取自 area 实例表（runtimeRaidFiendWarAreaRoster →
		// onlinePlayers.GetAreaInstancePlayers）。真机上每次 op36 都会把人写进这张表
		// （town_move.go 的 EnterArea），fixture 必须照样登记，否则"同格的同伴"在
		// 服务端眼里根本不存在，(c) 就变成了在验单人进图。
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: leader.selectedCharacterID, ChannelID: townPresenceChannelID(leader),
			TownID: 98, AreaID: 14, InstanceKey: state.RaidKey,
			PositionX: 450, PositionY: 234, Session: leader,
		})
		s.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: f.member.selectedCharacterID, ChannelID: townPresenceChannelID(f.member),
			TownID: 98, AreaID: peerArea, InstanceKey: state.RaidKey,
			PositionX: 450, PositionY: 234, Session: f.member,
		})
		// 上一小段可能已经把人送进了副本；留着它，后面每一段的进图尝试都会在
		// "已经在副本里"那一支静默 return，什么都观察不到。
		//
		// **清会话侧还不算完**。进图同时会在**攻坚账本**上留下这一格的 run 与座位
		// 占位（beginRuntimeRaidDungeon 写的 Lifecycle.Dungeons[runKey] 与
		// GroupOccupancy[seat]），只清 session.dungeon.runtime 会造出一个在真机上
		// 不可能存在的状态 —— "拥有这条 run 的会话手上已经没有 run 了，攻坚却还
		// 认为这一格被占着"。于是后面每一段都在 beginRuntimeRaidDungeon 里被当成
		// **自己的 racing duplicate** 拒掉（真机同样两条错误的形状，见
		// raid_lifecycle.go:673/720/1019）：
		//
		//	game-dungeon-select-blocked reason=dungeon_runtime_commit_failed
		//	  error=runtime raid dungeon lifecycle token is invalid:
		//	        raid=13108201 phase=1 map=4962 token=3
		//	（兜底重试那一次）
		//	game-dungeon-select-blocked reason=dungeon_runtime_commit_failed
		//	  error=runtime raid group already occupies a dungeon:
		//	        group=1 char=0 map=4962
		//
		// 注意这两条与"本人 live 行是 0 队"**无关**：beginRuntimeRaidDungeon 解析
		// 座位用的是**冻结行**（raid_lifecycle.go:657 runtimeRaidFrozenMember），
		// 所以 seat 一直都是 {Group: 1}，char=0 就是座位本身没有个人字符。
		//
		// 真机上把"run 还在、拥有它的会话已经没了"收回的是
		// releaseRuntimeRaidStaleGateOccupancy（它判定这条 run 不再 live 之后调用的
		// 正是下面这个函数），所以 fixture 用同一个函数收回，而不是自己动手删 map：
		// 这一段要验的是**进图门禁**，不是"副本重进"。
		s.raidMu.Lock()
		s.releaseRuntimeRaidRunsOwnedByLocked(state, leader.selectedCharacterID)
		s.releaseRuntimeRaidRunsOwnedByLocked(state, f.member.selectedCharacterID)
		s.raidMu.Unlock()
		leader.dungeon.mu.Lock()
		leader.dungeon.runtime = nil
		leader.dungeon.mu.Unlock()
		leader.conn.(*bufferConn).write.Reset()
	}
	writtenMsgIDs := func() map[uint16]bool {
		raw := append([]byte(nil), leader.conn.(*bufferConn).write.Bytes()...)
		ids := map[uint16]bool{}
		for len(raw) != 0 {
			packet, rest := splitGameServerUpperPacket(t, raw)
			raw = rest
			ids[packet.Header.MsgID] = true
		}
		return ids
	}
	// The divergence, exactly as the live log recorded it: the solo town return
	// rewrote the LIVE row only (group 0) and the member walked into area 14. The 1 Hz
	// board tick is the pass that has to answer, and it is the pass that used to
	// return nil in silence.
	//
	// 2026-10-01 按**客户端原生规格**改写（逆向结论见
	// 超时空_同区域进图_客户端逆向规格_20261001.md）：
	//
	//	弹窗 sub_9AE390（VA 0x9AE390）第一件事是 `cmp eax,0x2a` —— 只在频道类型 42
	//	（超时空）弹；文案 dstr 75467「移动后，会与处于当前城镇的同伴一同开始战斗。
	//	确认要移动吗？」。弹窗分支只是暂停并返回，客户端不组队、不收集成员。
	//
	// 所以**小队编制不再影响超时空进图**：那条
	// game-fiendwar-encounter-blocked / currentFiendWarSquadlessNotice（"你不在任何
	// 小队里，请先在攻坚队管理器中重新编队"）已经删除，名单改由
	// runtimeRaidFiendWarAreaRoster 按"这一格里实时站着谁"现算。
	//
	// 这里 peer 站在区域 2、本人站在 14 —— 他一个人在这一格里，于是名单只有他自己，
	// 应当**单人照常进图**，而不是被拒之门外。
	reset(0, 2)
	if err := s.tryRuntimeFiendWarEncounter(leader, f.now.Add(7*time.Second)); err != nil {
		t.Fatalf("squadless encounter pass: %v", err)
	}
	leader.dungeon.mu.Lock()
	entered := leader.dungeon.runtime != nil
	leader.dungeon.mu.Unlock()
	if !entered {
		t.Fatalf("没编制的人独自站在有怪的区域里，应当照常进图；报文=%x",
			leader.conn.(*bufferConn).write.Bytes())
	}
	// 编队提示不该再出现 —— 小队编制已经不是超时空进图的条件。
	squadlessText, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("不在任何小队"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(leader.conn.(*bufferConn).write.Bytes(), squadlessText) {
		t.Fatalf("编队提示仍然出现（%q）：小队编制不该再影响超时空进图",
			currentFiendWarSquadlessNotice)
	}

	// (b) The operator's escape hatch: re-assigning himself in the raid manager (the
	// regroup command, type=669) makes the very same pass drive the encounter: the
	// squad projection (op11 member list + op9 party state) and the dungeon select
	// (op153) that carries the area's dungeon. In the squadless pass none of that is
	// written at all, which is what makes this a discriminating assertion.
	reset(1, 14)
	if err := s.tryRuntimeFiendWarEncounter(leader, f.now.Add(9*time.Second)); err != nil {
		t.Fatalf("regrouped encounter: %v", err)
	}
	ids := writtenMsgIDs()
	if len(ids) == 0 {
		t.Fatal("a re-assigned member still could not drive the encounter: nothing was written")
	}
	if !ids[11] || !ids[9] {
		t.Fatalf("the encounter did not project its squad into a party; msg ids=%v", ids)
	}
	if !ids[153] {
		t.Fatalf("the encounter did not drive the dungeon select; msg ids=%v", ids)
	}
	leader.dungeon.mu.Lock()
	runtime := leader.dungeon.runtime
	leader.dungeon.mu.Unlock()
	if runtime == nil {
		t.Fatalf("重新编队后进图没有提交：报文=%v", ids)
	}
	if runtime.raidDungeon.RaidKey != state.RaidKey {
		t.Fatalf("encounter entered outside the raid: binding=%+v, want raid %d",
			runtime.raidDungeon, state.RaidKey)
	}

	// (c) 队友站在**同一格**，而本人没有编制：按客户端原生规格，这正是"一起进图"
	// 的标准场景 —— 名单由 runtimeRaidFiendWarAreaRoster 按这一格里站着谁来算，
	// 编制不参与。所以两人都该进，且不该再出现任何编队提示。
	//
	// 注意：本人 live 行是 group 0（solo town return 造成的分歧），队友在 1 队；
	// 旧实现下这个分歧会让整场静默死锁，新实现下它只影响 battle 的 group 记法。
	reset(0, 14)
	leader.fiendWarEncounterAt = time.Time{}
	leader.fiendWarEncounterArea = 0
	if err := s.tryRuntimeFiendWarEncounter(leader, f.now.Add(10*time.Second)); err != nil {
		t.Fatalf("squadmate-in-area encounter: %v", err)
	}
	raw := append([]byte(nil), leader.conn.(*bufferConn).write.Bytes()...)
	leader.conn.(*bufferConn).write.Reset()
	squadlessWithPeer, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("不在任何小队"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, squadlessWithPeer) {
		t.Fatalf("同格有队友时仍然弹编队提示；报文=%x", raw)
	}
	leader.dungeon.mu.Lock()
	leaderRuntime := leader.dungeon.runtime
	leader.dungeon.mu.Unlock()
	if leaderRuntime == nil {
		t.Fatalf("同格有队友+自己没编制，进图没有发生；报文=%x", raw)
	}
	// 而"一起进图"的另一半：同格的队友也必须真的走进**同一场**副本 ——
	// 这才是这条规格的目的（客户端弹的 75467 就是「会与处于当前城镇的同伴一同
	// 开始战斗」），只验驱动者自己进去等于把功能验成了"单人进图"。
	f.member.dungeon.mu.Lock()
	peerRuntime := f.member.dungeon.runtime
	f.member.dungeon.mu.Unlock()
	if peerRuntime == nil {
		t.Fatalf("同格的队友没有被搬进副本：只有驱动者（char=%d）进去了",
			leader.selectedCharacterID)
	}
	// "同一场"按 (raid, phase, map) 判，**不能**比 LifecycleToken：
	// 超时空一个区域共用一条 run，但每个成员手里是**自己那份** token
	// （runtimeFiendWarPartyBinding 给的 binding 带的就是本人的 token），
	// 后到的那位靠 runtimeRaidDungeonLifecycleReportAccepted 的放松被静默接受
	// —— 那正是 raid_fiendwar_area_seat_test.go 钉住的设计。
	if peerRuntime.raidDungeon.RaidKey != leaderRuntime.raidDungeon.RaidKey ||
		peerRuntime.raidDungeon.Phase != leaderRuntime.raidDungeon.Phase ||
		peerRuntime.raidDungeon.MapID != leaderRuntime.raidDungeon.MapID {
		t.Fatalf("队友与驱动者进的不是同一场副本：peer=%+v leader=%+v",
			peerRuntime.raidDungeon, leaderRuntime.raidDungeon)
	}
	// 而且这一格在攻坚账本上只有**一条** run：后到的人采用先到者的那份，
	// 不是各开一份（各开一份会让战场记的"区域归属"与两个人的副本实例对不上）。
	s.raidMu.Lock()
	runs := 0
	for key := range state.Lifecycle.Dungeons {
		if key.MapID == int(leaderRuntime.raidDungeon.MapID) {
			runs++
		}
	}
	s.raidMu.Unlock()
	if runs != 1 {
		t.Fatalf("副本 %d 在攻坚账本上有 %d 条 run，want 1（一场只开一条）",
			leaderRuntime.raidDungeon.MapID, runs)
	}
}
