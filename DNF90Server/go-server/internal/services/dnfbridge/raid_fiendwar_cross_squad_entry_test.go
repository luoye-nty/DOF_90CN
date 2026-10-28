package dnfbridge

import (
	"context"
	"os"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 超时空：同格两个人**在不同小队**，一起进图。
//
// 这是规格 D 的原话 —— 「进图单位 = 区域里实时站着的所有人（≤4），**与小队编制无关**」——
// 也是 2026-10-01 22:48 真机那次失败的确切形状，报文原样（分片 030，本地时间）：
//
//	14:47:48.784 game-raid-manager-regrouped type=669 raid_key=12058625
//	             source_char_id=1 member_char_id=22 target_group=2
//	14:47:53.915 game-raid-squad-parties-projected squad_count=2 squads=1:2,2:3
//	14:48:00.695 char_id=1  -> town=98 area=4   （两人同格 4）
//	14:48:04.061 char_id=22 -> town=98 area=4
//	14:48:12.166 game-fiendwar-maze-selected char_id=1 dungeon_id=4955 area=7   ← char 1 走进有怪的 7
//	14:48:12.168 game-party-dungeon-entry-blocked char_id=1 dungeon_id=4955
//	             reason=party_entry_preflight_failed_before_any_member_commit
//	             error=prepare party member 22 for dungeon 4955: anton raid entry requires
//	                   a started, paid raid roster and an open current-phase objective
//	14:48:12.168 game-party-disbanded-by-single-member-left char_id=22 party_id=4 removed_id=1
//	14:48:12.514 game-fiendwar-encounter-solo-fallback entered=true   ← 触发者独自进去
//
// 原因是 validateAntonRaidDungeonEntry 的超时空分支只认两件事：
//
//	MemberArea(本人) == 目标区域   —— 队友按设计**留在原格**，永远对不上；
//	GroupAreas[本人所在小队] == 目标区域 —— [B] 那次放松加的，但它问的是**队友自己**的小队。
//
// 于是只要两个人不在同一个小队（这里 char 22 在 2 队、char 1 在 1 队），两条都不成立，
// 整场进图被拒 —— 「与小队编制无关」这条规格就没走通。
//
// 本用例就是那个形状：驱动者 1002 在 1 队，被搬的同伴 1001 在 2 队，两人同格 3，
// 目标区域 6。
func TestRealPVFFiendWarDifferentSquadsSharingOneCellEnterTogether(t *testing.T) {
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
		// 两个人都站在格子 3（98/3, 240,484）：这就是"同格的同伴"。
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
	// **不同小队**：驱动者 1002 在 1 队，被搬的同伴 1001 在 2 队。
	state.Members[0].GroupIndex, state.Members[0].SlotOrder = 2, 0 // 1001 -> 2 队
	state.Members[1].GroupIndex, state.Members[1].SlotOrder = 1, 1 // 1002 -> 1 队
	state.FrozenMembers[0].GroupIndex, state.FrozenMembers[0].SlotOrder = 2, 0
	state.FrozenMembers[1].GroupIndex, state.FrozenMembers[1].SlotOrder = 1, 1
	if state.Members[0].GroupIndex == state.Members[1].GroupIndex {
		t.Fatal("fixture 没造出「不同小队」的输入")
	}
	// 两个人**同格** 3，各自的账本也都在 3（这是"一起站在格子里"的事实）。
	b.MemberAreas[1001], b.MemberAreas[1002] = 3, 3
	b.GroupAreas[1], b.GroupAreas[2] = 3, 3
	syncRuntimeFiendWarObjectives(state)

	mover := f.member // 1002，1 队：按下进图的人
	peer := f.leader  // 1001，2 队：按设计留在原格、等着被搬进去的人
	dungeonID := int64(b.Config.Areas[6].Dungeon)

	// [反例 1] 小队还没走进区域 6 之前，谁都不许进 —— 放松不能变成"谁都能进"的洞。
	if err := s.validateAntonRaidDungeonEntry(peer, dungeonID); err == nil {
		t.Fatal("小队还没走进区域 6，门禁就放行了")
	}

	// 真机上这一脚由 town_move.go 落地：只有**移动的那个人**被记进目标区域，
	// 其余人（以及他们各自的小队）留在原格。
	if _, err := s.moveRuntimeFiendWarGroup(mover.selectedCharacterID, 6); err != nil {
		t.Fatalf("move ledger: %v", err)
	}
	b = state.Lifecycle.FiendWar
	if b.GroupAreas[1] != 6 || b.MemberAreas[mover.selectedCharacterID] != 6 {
		t.Fatalf("账本没把驱动者的小队列进区域 6：group=%v driver=%d",
			b.GroupAreas, b.MemberAreas[mover.selectedCharacterID])
	}
	if got := b.MemberAreas[peer.selectedCharacterID]; got != 3 {
		t.Fatalf("同伴的账本被一起搬走了：peer area = %d, want 3（按设计他留在原格）", got)
	}
	if got := b.GroupAreas[2]; got != 3 {
		t.Fatalf("同伴那个小队的账本被一起搬走了：GroupAreas[2] = %d, want 3", got)
	}

	// [本用例的核心] 同格但**不同小队**的同伴必须被放行。
	//
	// 这里就是 22:48 真机被拒的那一处：他本人不在目标区域（被钉在原格 3）、
	// 他自己那个小队也不在目标区域（GroupAreas[2] 还是 3），于是
	// errAntonRaidEntryBlocked → "prepare party member ...: anton raid entry requires
	// a started, paid raid roster and an open current-phase objective" → 整队被拆、
	// 触发者一个人进去。
	//
	// 两种形态**分别**判，别混成一个：
	//   * 他自己按 op16（partyFollower=false）：仍然拒绝 —— 另一半的门没被放松，
	//     "站在别处的人指哪进哪"这条口子没有开；
	//   * 被队长的 op16 带着走（partyFollower=true）：必须放行 —— 这才是规格 D
	//     「进图单位 = 区域里实时站着的所有人，与小队编制无关」。
	if err := s.validateAntonRaidDungeonEntry(peer, dungeonID); err == nil {
		t.Fatal("同伴自己按 op16 就能进超时空副本：非队友那一半被放松成了洞")
	}
	if err := s.validateAntonRaidDungeonEntryForEntry(peer, dungeonID, true); err != nil {
		t.Fatalf("同格但不同小队的同伴被门禁拒绝（这就是 22:48 真机那次）：%v", err)
	}

	// [反例] 未编队成员的两种形态也是分开的：
	//   * 自己按 op16 → 依旧拒绝（D2 的守卫，2026-09-17 那次事故的形状）；
	//   * 作为**区域名单里的队友** → 放行。这不是洞：规格 D/E 明确删掉了超时空的
	//     "没编制就拒绝"（同一个套件里的 (a) 段就钉着"没编制的人独自站在有怪的区域里
	//     照常进图"），决定权在"他此刻是不是真的站在这一格"。
	//     通用判定一条都没放松：本团冻结成员、团已开始并付过费、阶段有效、目标开着。
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
	liveGroup := setGroup(state.Members, peer.selectedCharacterID, 0)
	frozenGroup := setGroup(state.FrozenMembers, peer.selectedCharacterID, 0)
	if err := s.validateAntonRaidDungeonEntry(peer, dungeonID); err == nil {
		t.Fatal("未编队成员自己按 op16 被放进了超时空副本：GroupAreas[0] 应当是零值")
	}
	if err := s.validateAntonRaidDungeonEntryForEntry(peer, dungeonID, true); err != nil {
		t.Fatalf("未编队、但确实站在这一格的队友被拒了（规格 D/E 已删掉没编制拒绝）：%v", err)
	}
	setGroup(state.Members, peer.selectedCharacterID, liveGroup)
	setGroup(state.FrozenMembers, peer.selectedCharacterID, frozenGroup)

	// [反例] 放松只针对"区域"这一条：团没开始 / 没付入场费时，队友形态照样拒绝。
	state.Started = false
	if err := s.validateAntonRaidDungeonEntryForEntry(peer, dungeonID, true); err == nil {
		t.Fatal("团都没开始，队友形态就把人放进了超时空副本")
	}
	state.Started = true

	// 端到端：同格两个人**都**进同一个副本。
	mover.conn.(*bufferConn).write.Reset()
	peer.conn.(*bufferConn).write.Reset()
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
		t.Fatalf("area encounter pass: %v", err)
	}
	if !handled {
		t.Fatal("走进有怪的区域没有触发遭遇战")
	}
	mover.dungeon.mu.Lock()
	moverRuntime := mover.dungeon.runtime
	mover.dungeon.mu.Unlock()
	peer.dungeon.mu.Lock()
	peerRuntime := peer.dungeon.runtime
	peer.dungeon.mu.Unlock()
	if moverRuntime == nil {
		t.Fatalf("驱动者没有进图")
	}
	if peerRuntime == nil {
		t.Fatalf("同格但不同小队的同伴没有被搬进副本：只有驱动者 %d 进去了（22:48 真机的形状）",
			mover.selectedCharacterID)
	}
	if peerRuntime.raidDungeon.RaidKey != moverRuntime.raidDungeon.RaidKey ||
		peerRuntime.raidDungeon.Phase != moverRuntime.raidDungeon.Phase ||
		peerRuntime.raidDungeon.MapID != moverRuntime.raidDungeon.MapID {
		t.Fatalf("两个人进的不是同一场副本：peer=%+v mover=%+v",
			peerRuntime.raidDungeon, moverRuntime.raidDungeon)
	}
	// 这一格在攻坚账本上只有一条 run（一场只开一条）。
	s.raidMu.Lock()
	runs := 0
	for key := range state.Lifecycle.Dungeons {
		if key.MapID == int(moverRuntime.raidDungeon.MapID) {
			runs++
		}
	}
	s.raidMu.Unlock()
	if runs != 1 {
		t.Fatalf("副本 %d 在攻坚账本上有 %d 条 run，want 1",
			moverRuntime.raidDungeon.MapID, runs)
	}

	// 驱动者也必须拿到 op27（当前副本上下文），而且必须排在 op28 **之前**。
	//
	// 队伍进图会提前 return（runtime.partyDungeonOwner != nil），走不到单人的
	// op27 前奏；而 antuan/luke 的队长是从 op15 选图页进来的，op15 早就发过 op27 了，
	// 所以一直没暴露。超时空是**走进区域**触发的（op36），驱动者从没经过选图页 ——
	// 于是 op27 一次都没发。真机 2026-10-01 23:07 正是如此：
	//
	//	15:07:17.699 op27 -> char 22 (follower, source=leader_op16_party_follower_preflight_after_fatigue)
	//	（char 1 驱动者：整个会话一条 op27 都没有）
	//	15:07:17.890 char 1  start-committed（op5/op28/op29 都发了）
	//	15:07:19.906 char 22 RECV op37 FINISH_LOADING   ← 队友完成加载
	//	（char 1 再没有任何 C2S）                        ← 驱动者卡在进图
	//
	// 客户端拿不到 op27 就不会切进副本上下文，于是永远不发 op37，人就卡住了。
	seq := make([]uint16, 0, 64)
	for raw := append([]byte(nil), mover.conn.(*bufferConn).write.Bytes()...); len(raw) != 0; {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		seq = append(seq, packet.Header.MsgID)
	}
	indexOf := func(id uint16) int {
		for i, got := range seq {
			if got == id {
				return i
			}
		}
		return -1
	}
	at27, at28 := indexOf(27), indexOf(28)
	if at27 < 0 {
		t.Fatalf("驱动者没有收到 op27（当前副本上下文）——客户端会卡在进图、永远不发 op37；序列=%v", seq)
	}
	if at28 < 0 {
		t.Fatalf("驱动者没有收到 op28；序列=%v", seq)
	}
	if at27 > at28 {
		t.Fatalf("op27 排在 op28 之后：op27@%d op28@%d；序列=%v", at27, at28, seq)
	}
}
