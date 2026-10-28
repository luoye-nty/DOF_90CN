package dnfbridge

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 组队通关 → 队长点「选择其他地下城」（不返回城镇）→ 队长直接点新副本：服务端必须把全队
// 带进新副本，而不是回一个失败码让客户端原地重发。
//
// 现场日志（packet_log.txt，UTC）：
//
//	2026-09-19T13:19:21.574 game-dungeon-completed-other-selector-committed char_id=1
//	2026-09-19T13:19:23.220 game-dungeon-select-request-decoded dungeon_id=3   ← 队长点新副本
//	2026-09-19T13:19:23.236 game-dungeon-select-entry-policy / -maze-selected / -monster-rank-policy
//	2026-09-19T13:19:23.236 game-party-dungeon-entry-blocked
//	    reason=party_entry_preflight_failed_before_any_member_commit
//	    error=party member 22 is not present in the leader town area
//	…同一条 op16 在 5 秒里被客户端重发了 16 次，每次都只回一个 3 字节失败码 3
//	（body=00 03 00）。客户端既不提示也不换页，玩家看到的就是「点不动」。
//
// 真因不在入场链本身：进场时 leaveTownPresenceForDungeon 把每个队员都从城镇名册上摘掉并
// 消费掉选图备忘，而「选择其他地下城」这条路由只退役运行时、重建队员的选图页，从没给任何
// 一个人留下城镇锚点。于是共存判定 currentPartyEntryMembersShareTownArea 两个人都解不出来，
// 全队入场在第一名队员上就被拒。
func TestPartyDungeonEntryAfterCompletedRunSelectOtherDungeonReachesStartMap(t *testing.T) {
	service, leader, follower, logPath := prepareCompletedPartySelectorEntryTest(t)
	leaderConn := leader.conn.(*bufferConn)
	followerConn := follower.conn.(*bufferConn)

	// 1) 全队真的进过一次副本，两个成员各自打完一局。
	leaderCompleted, followerCompleted := enterAndCompletePartyRunForTest(t, service, leader, follower)

	// 2) 队长点「选择其他地下城」（op72 {1,1}）。队员的客户端镜像同一个请求，服务端的
	//    队员侧动作就是 dungeon_card_flow.go 里 peer 循环的那两步。
	retirePartyIntoOtherSelectorForTest(t, service, leader, follower, followerCompleted)

	leaderConn.write.Reset()
	followerConn.write.Reset()

	// 3) 队长点新副本。入场链的第一道闸门就是共存判定：两个人都要有可解析的城镇锚点
	//    （活名册，或选图页留下的退役记录），否则第一名队员上就以 reason 3 收场。
	if !service.currentPartyEntryMembersShareTownArea(leader, follower) {
		t.Fatal("completed-run selector leaves the party without a resolvable town anchor: " +
			"every following op16 is refused with the silent reason-3 rejection")
	}
	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[0:4], 700)
	if err := service.handleDungeonSelectUpper(leader, body); err != nil {
		t.Fatalf("select dungeon after 选择其他地下城: %v", err)
	}
	assertNoSelectDungeonFailureForTest(t, "leader", leaderConn, logPath)
	assertNoSelectDungeonFailureForTest(t, "follower", followerConn, logPath)

	leaderRuntime := currentRuntimeAfterEntryForTest(t, leader)
	followerRuntime := currentRuntimeAfterEntryForTest(t, follower)
	if leaderRuntime == leaderCompleted || followerRuntime == followerCompleted {
		t.Fatalf("party entry reused the retired completed run: leader=%p follower=%p",
			leaderRuntime, followerRuntime)
	}
	if leaderRuntime.Dungeon.ID != followerRuntime.Dungeon.ID ||
		leaderRuntime.Seed != followerRuntime.Seed {
		t.Fatalf("party members entered different runs: leader=%d/%d follower=%d/%d",
			leaderRuntime.Dungeon.ID, leaderRuntime.Seed,
			followerRuntime.Dungeon.ID, followerRuntime.Seed)
	}
	assertStartMapReachedForTest(t, "leader", leaderConn, leaderRuntime)
	assertStartMapReachedForTest(t, "follower", followerConn, followerRuntime)
}

// 通关后「再次挑战」：队长先点，队员的客户端还没点 —— 现场 2026-09-20 16:35:07Z
// （char 1 / char 22，安徒恩 210）就是这个顺序：
//
//	16:35:07.842 game-dungeon-card-exit-retry-enter char_id=1 → op16 重进 210
//	16:35:07.863 game-party-dungeon-entry-copresence-refused char_id=22
//	             reason=member_has_no_resolvable_town_anchor
//	16:35:07.898 game-upper-current-dungeon-permission-op5-snapshot-send
//	             source=current_exe_op72_retry_entry_not_committed_op5_permissions
//	16:35:07.899 game-dungeon-card-exit-ack-sent char_id=22 {1,0}   ← 队员的再次挑战这时才被处理
//
// 队长这一次退役只给自己留了城镇锚点，队员还在自己那把「已完成」的 run 里（进城时就被摘出
// 城镇名册），共存判定两个人必须都能解出来，于是整队入场在队员上被拒；再次挑战又不像「选择其他
// 地下城」那样有队员自己的镜像退役，客户端只能拿到城镇选图页 —— 玩家看到的「再次挑战直接返回城镇」。
// 组队「再次挑战」必须整队重进，队长先点也不能把队员落下。
//
// 现场 2026-09-20（安徒恩 210，char 1 队长 / char 22 队员）：
//
//	16:52:46.112 game-dungeon-card-exit-retry-enter char_id=1 → op16 重进
//	16:52:46.128 game-party-dungeon-entry-blocked
//	             error=party member 22 selector is not ready: town_scene_player_state_not_finalized
//	16:52:46.156 op5/op27 → op24 回城：客户端被塞回城镇选图页
//
// 队长的客户端先点，队员自己的「再次挑战」还在路上：队长退役了自己那把已完成 run，队员还站在
// 自己那把里、城镇场景没 finalize，入场前置在队员上被拒，再次挑战又不像「选择其他地下城」那样
// 有队员自己的镜像退役 —— 玩家看到的就是「组队再次挑战直接返回城镇」。
// 所以队长这一下要顺手把队员那把已完成 run 也退役进选图页（等于替它做它自己马上要做的那一步）。
func TestPartyDungeonRetryDrivesTheMemberFromItsCompletedRun(t *testing.T) {
	service, leader, follower, logPath := prepareCompletedPartySelectorEntryTest(t)
	leaderConn := leader.conn.(*bufferConn)
	followerConn := follower.conn.(*bufferConn)
	leaderCompleted, followerCompleted := enterAndCompletePartyRunForTest(t, service, leader, follower)

	// 只有队长有「再次挑战」；队员这时还停在自己那把已完成 run 的结算页上，城镇场景没
	// finalize。86 的模型是服务端把整队推进去（PARTY_LOADING_GATE 下发预载 + 等各人
	// FINISH_LOADING），所以队长这一下必须带全队，而不是因为队员没就绪就回城。
	if err := service.handleDungeonEplpCommand(leader, []byte{1, 0}); err != nil {
		t.Fatalf("leader 再次挑战: %v", err)
	}
	assertNoSelectDungeonFailureForTest(t, "leader", leaderConn, logPath)
	assertNoSelectDungeonFailureForTest(t, "follower", followerConn, logPath)
	leaderRuntime := currentRuntimeAfterEntryForTest(t, leader)
	followerRuntime := currentRuntimeAfterEntryForTest(t, follower)
	if leaderRuntime == leaderCompleted || followerRuntime == followerCompleted {
		t.Fatalf("the retry reused the completed runs: leader=%p follower=%p", leaderRuntime, followerRuntime)
	}
	if leaderRuntime.Seed != followerRuntime.Seed {
		t.Fatalf("the party retried into different runs: leader=%d follower=%d",
			leaderRuntime.Seed, followerRuntime.Seed)
	}
	// 这个放宽只对「队员正处在自己那把已完成 run 的结算页」生效，实际命中哪一道检查取决
	// 于是队员的城镇场景还是它的选图页没就绪；两种说明行任取其一即证明走了这条路，但在
	// 整套测试一起跑时队员可能恰好是就绪的（那也是允许的），所以这里只做记录不判定。
	if text, err := packetLogPlaintextSoft(logPath); err == nil {
		if strings.Contains(text, "game-party-dungeon-retry-member-driven-by-server") ||
			strings.Contains(text, "game-party-dungeon-retry-member-page-skipped") {
			t.Logf("the retry drove the member through the scoped relaxation")
		}
	}
}

// 这条锚点只回答「这个队员的选图页落在哪里」，不能变成绕过区域检查的后门：通关原点在
// 另一个区域/城镇/频道的队员，仍然必须在第一名队员上被拒（与
// TestPartyDungeonEntryStillRefusesAMemberFromAnotherArea 同一条产品约束）。
func TestCompletedRunSelectorAnchorStillRefusesAMemberFromAnotherArea(t *testing.T) {
	for name, move := range map[string]func(t *testing.T, service *Service, follower *gameSession){
		"另一座城镇": func(t *testing.T, service *Service, follower *gameSession) {
			moveCompletedRunTestMemberLocation(t, service, follower, 39, 1)
		},
		"另一个区域": func(t *testing.T, service *Service, follower *gameSession) {
			moveCompletedRunTestMemberLocation(t, service, follower, 38, 2)
		},
		"另一个频道": func(_ *testing.T, _ *Service, follower *gameSession) {
			follower.channel = channelcatalog.Channel{ServerID: 1, ID: 43, Type: 11, Name: "ch.43", Port: 10043}
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, leader, follower, _ := prepareCompletedPartySelectorEntryTest(t)
			// 队员这一局是从别处开的：冻结的进城原点就是那个别处。
			move(t, service, follower)
			_, followerCompleted := enterAndCompletePartyRunForTest(t, service, leader, follower)
			retirePartyIntoOtherSelectorForTest(t, service, leader, follower, followerCompleted)

			prepared, err := currentPartySelectorEntryPreflight(t, service, leader)
			if err == nil || !strings.Contains(err.Error(), "is not present in the leader town area") {
				t.Fatalf("%s: preflight err=%v prepared=%d, want the co-presence refusal",
					name, err, len(prepared))
			}
			if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
				t.Fatalf("%s: entry must be refused before any member commits", name)
			}
		})
	}
}

// moveCompletedRunTestMemberLocation moves one member's persisted and bound town scene. The
// persisted stats, not the session, own the frozen pre-entry origin of its next run
// (currentDungeonTownReturnOriginOwner reads them), so a member that really started its run
// elsewhere has to be moved here.
func moveCompletedRunTestMemberLocation(
	t *testing.T,
	service *Service,
	session *gameSession,
	townID, areaID byte,
) {
	t.Helper()
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	record, found, err := repositories.Character.Load(context.Background(), characterID)
	if err != nil || !found {
		t.Fatalf("load member location record found=%t err=%v", found, err)
	}
	record.Stats["town_id"] = int64(townID)
	record.Stats["area_id"] = int64(areaID)
	if err := repositories.Character.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, townID, areaID, 500, 240)
}

// 补锚点只能在角色没有任何活登记时发生：活名册优先，且 op15 那条路由自己会用活登记写
// 备忘。否则一次通关退役就会把城镇里真实站着的记录换成副本原点。
func TestCompletedRunSelectorAnchorSkipsALiveTownRegistration(t *testing.T) {
	service, leader, _, _ := prepareCompletedPartySelectorEntryTest(t)
	origin := currentTownPositionSnapshot{
		CharacterID: 99, TownID: 38, AreaID: 2, PositionX: 700, PositionY: 300, PositionValid: true,
	}
	if service.bindCompletedRunSelectorPresence(leader, origin, "test_live_registration") {
		t.Fatal("a live town registration must not be replaced by a synthesized selector anchor")
	}
	if presence, ok := service.currentTownActorAreaDetachPresence(leader); !ok ||
		presence.TownID != 38 || presence.AreaID != 1 {
		t.Fatalf("live anchor changed: presence=%+v resolved=%t", presence, ok)
	}

	service.onlinePlayers.LeaveArea(leader.selectedCharacterID)
	if !service.bindCompletedRunSelectorPresence(leader, origin, "test_without_registration") {
		t.Fatal("a character that holds no town registration must receive the selector anchor")
	}
	presence, ok := service.currentTownActorAreaDetachPresence(leader)
	if !ok || presence.CharacterID != 99 || presence.TownID != 38 || presence.AreaID != 2 ||
		presence.PositionX != 700 || presence.PositionY != 300 {
		t.Fatalf("selector anchor not resolvable: presence=%+v resolved=%t", presence, ok)
	}
	if _, registered := service.onlinePlayers.PlayerForCharacter(leader.selectedCharacterID); registered {
		t.Fatal("the synthesized anchor must stay invisible in town (no roster registration)")
	}
}

// enterAndCompletePartyRunForTest takes the party through one full run: entry removes every
// member from the town roster and consumes the selector memo (leaveTownPresenceForDungeon),
// then each member holds its own settlement-exit-ready completed run.
func enterAndCompletePartyRunForTest(
	t *testing.T,
	service *Service,
	leader *gameSession,
	follower *gameSession,
) (leaderRun *runtimeDungeonState, followerRun *runtimeDungeonState) {
	t.Helper()
	service.leaveTownPresenceForDungeon(leader)
	service.leaveTownPresenceForDungeon(follower)
	for name, session := range map[string]*gameSession{"leader": leader, "follower": follower} {
		if _, registered := service.onlinePlayers.PlayerForCharacter(session.selectedCharacterID); registered {
			t.Fatalf("fixture: %s must be off the town roster while inside the dungeon", name)
		}
	}
	return commitCompletedPartyRunForTest(t, service, leader, 700),
		commitCompletedPartyRunForTest(t, service, follower, 700)
}

// retirePartyIntoOtherSelectorForTest mirrors the 选择其他地下城 route on both clients: the
// leader's own op72 {1,1} and the peer-side retire + follower selector page the
// dungeon_card_flow.go peer loop performs.
func retirePartyIntoOtherSelectorForTest(
	t *testing.T,
	service *Service,
	leader *gameSession,
	follower *gameSession,
	followerCompleted *runtimeDungeonState,
) {
	t.Helper()
	if err := service.handleDungeonEplpCommand(leader, []byte{1, 1}); err != nil {
		t.Fatalf("leader 选择其他地下城: %v", err)
	}
	if leader.dungeon.runtime != nil {
		t.Fatalf("leader completed run was not retired into the selector: %+v", leader.dungeon.runtime)
	}
	if followerCompleted == nil {
		t.Fatal("fixture: follower completed runtime missing")
	}
	if follower.dungeon.runtime != followerCompleted {
		t.Fatal("fixture: follower run changed before the mirror request")
	}
	retired, err := service.retireCompletedDungeonForTownSelect(follower)
	if err != nil || !retired {
		t.Fatalf("follower mirror retire: retired=%t err=%v", retired, err)
	}
	if follower.dungeon.runtime != nil {
		t.Fatalf("follower completed run was not retired into the selector: %+v", follower.dungeon.runtime)
	}
	ready, err := service.prepareRuntimePartyFollowerEnterSelect(
		follower,
		leader.selectedCharacterID,
		int(runtimePartyStateSnapshot(leader).PartyID),
		"test_party_dungeon_settlement_peer_follows_other_selector",
	)
	if err != nil || !ready {
		t.Fatalf("follower mirror selector page: ready=%t err=%v", ready, err)
	}
}

// prepareCompletedPartySelectorEntryTest is the ordinary-dungeon twin of
// prepareCurrentPartySelectorEntryTest: a two-member party standing in one town
// area with the selector origin bound, the state an actual op16 entry starts
// from. Dungeon 700 here is an ordinary dungeon on purpose - a tutorial dungeon
// keeps its own persisted birth-town return owner instead of the frozen op35
// origin the reported flow uses (see freezeCurrentDungeonTownReturnOrigin).
func prepareCompletedPartySelectorEntryTest(t *testing.T) (*Service, *gameSession, *gameSession, string) {
	t.Helper()
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(
		source["dungeon/test.dgn"],
		"[limit party count]\n1\n",
		"[limit party count]\n4\n",
		1,
	)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, id := range []string{"99", "100"} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed := uint32(0)
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { seed++; return 0x3000 + seed, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      newFakeCurrentDungeonDeathTimerQueue(),
	}
	// 选图页与入场链都会重装技能管理器（op19），这是真实客户端路径的一部分。
	enableTownMoveSkillProjection(t, service, repositories, "99")
	enableTownMoveSkillProjection(t, service, repositories, "100")
	// 现场诊断用的服务器端事件日志：失败时测试会把最后几条 *_blocked 打进失败信息，
	// 因为客户端对 reason-3 拒绝是完全静默的（现场就是靠 packet_log 才定位到的）。
	logPath := filepath.Join(t.TempDir(), "party-completed-selector.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	service.packetLog.Store(logger)
	t.Cleanup(func() {
		service.packetLog.Store(nil)
		if err := logger.Close(); err != nil {
			t.Logf("close party entry event log: %v", err)
		}
	})

	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	leader := &gameSession{conn: &bufferConn{}, channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: &bufferConn{}, channel: channel, residentChannel: channel, selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state := alignedcmd.PartyState{
		PartyID: 99, UserID: 99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, state)
	storeRuntimePartyState(follower, state)
	// 共享副本的 op2 投影是队长在城镇里建立 actor 时冻结的；真实客户端在进图前就已经
	// 持有它，所以 fixture 也要先把两个人的投影种进去（见 dungeon_entry_packets_test.go）。
	seedPartyOwnerActorProjectionForTest(t, service, leader, 99, byte(channel.ID))
	seedPartyOwnerActorProjectionForTest(t, service, follower, 100, byte(channel.ID))
	// 共享副本要求两名队员的原生 UDP 端点表齐全（客户端进队时会互报），fixture 照实登记。
	registerPartyPeerEndpointForTest(leader, 3, 5063)
	registerPartyPeerEndpointForTest(follower, 3, 60359)
	service.registerTownPresenceForTest(t, leader, 99, 38, 1, 450, 234)
	service.registerTownPresenceForTest(t, follower, 100, 38, 1, 500, 240)
	bindDungeonSelectorOriginForTestAt(t, service, leader, 38, 1, 450, 234)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 38, 1, 500, 240)
	return service, leader, follower, logPath
}

// commitCompletedPartyRunForTest gives one session a real, fully gated completed run: the
// same runtime an actual entry builds (real PVF maze, frozen pre-entry town origin) taken
// through the settlement barriers the 选择其他地下城 exit requires.
func commitCompletedPartyRunForTest(
	t *testing.T,
	service *Service,
	session *gameSession,
	dungeonID uint32,
) *runtimeDungeonState {
	t.Helper()
	request := dungeoncmd.SelectDungeonRequest{DungeonID: dungeonID, Difficulty: 1}
	runtime, _, err := service.prepareDungeonRuntime(context.Background(), session, request)
	if err != nil {
		t.Fatalf("prepare completed party run for %d: %v", session.selectedCharacterID, err)
	}
	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("freeze completed party run origin for %d: %v", session.selectedCharacterID, err)
	}
	if err := service.commitDungeonRuntime(session, runtime); err != nil {
		t.Fatalf("commit completed party run for %d: %v", session.selectedCharacterID, err)
	}
	// 打完这一局。生产里终点是 boss 死亡检查（completeSettlementRuntimeForTest 走的那条），
	// 这里只需要同一个状态：Run=Completed + 已进入结算。CompleteCurrentRoom 是
	// worldmap 自己的「权威脚本完成」入口，不依赖这个 fixture 里有没有怪物。
	if err := runtime.Session.CompleteCurrentRoom(); err != nil {
		t.Fatalf("complete party run for %d: %v", session.selectedCharacterID, err)
	}
	runtime.settlementEntrySent = true
	// 结算门槛用的就是其它「已完成」用例那一套字段：op46/op123 的重放分支不置
	// settlementPlayResultReceived，所以这里直接把结算状态写成客户端已经走完的样子。
	runtime.settlementPlayResultReceived = true
	runtime.settlementStatisticReceived = true
	markCurrentDungeonCardRewardCommittedForTest(runtime)
	if !service.completedDungeonSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("fixture: run for %d is not settlement-exit ready", session.selectedCharacterID)
	}
	return runtime
}

func currentRuntimeAfterEntryForTest(t *testing.T, session *gameSession) *runtimeDungeonState {
	t.Helper()
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	session.dungeon.mu.Unlock()
	if runtime == nil {
		t.Fatalf("character %d has no dungeon runtime after the entry request", session.selectedCharacterID)
	}
	return runtime
}

// assertNoSelectDungeonFailureForTest pins the shape the player actually sees: a 3-byte op16
// failure body {0, reason, 0}. The client neither shows a notice nor leaves the selector for
// it, so every reason here means 点不动 regardless of which gate produced it. The server-side
// refusal is quoted back from the event log, because that log is the only oracle the live
// report had.
func assertNoSelectDungeonFailureForTest(t *testing.T, name string, conn *bufferConn, logPath string) {
	t.Helper()
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, conn.write.Bytes()) {
		if packet.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) ||
			packet.Header.Classification == 0 {
			continue
		}
		// The success ACK starts with its compatibility byte 1; a refusal starts
		// with the failure byte 0 (buildSelectDungeonFailureBody).
		if len(packet.Body) == 0 || packet.Body[0] != 0 {
			continue
		}
		reason := byte(0)
		if len(packet.Body) > 1 {
			reason = packet.Body[1]
		}
		t.Fatalf("%s received an op16 failure reply body=%x reason=%d\nlast refusals:\n%s",
			name, packet.Body, reason, strings.Join(lastDungeonRefusalsForTest(t, logPath), "\n"))
	}
}

// lastDungeonRefusalsForTest returns the trailing server-side refusal events so a failure
// names the gate instead of only the wire symptom.
func lastDungeonRefusalsForTest(t *testing.T, logPath string) []string {
	t.Helper()
	// The packet logger hands lines to a background writer; give it one flush
	// interval so the refusal that just happened is on disk.
	time.Sleep(packetLogFlushInterval + 50*time.Millisecond)
	text, err := packetLogPlaintextSoft(logPath)
	if err != nil {
		return []string{"<no event log: " + err.Error() + ">"}
	}
	lines := strings.Split(text, "\n")
	refusals := make([]string, 0, 8)
	for _, line := range lines {
		if strings.Contains(line, "-blocked") || strings.Contains(line, "entry-failed") {
			refusals = append(refusals, line)
		}
	}
	if len(refusals) > 8 {
		refusals = refusals[len(refusals)-8:]
	}
	return refusals
}

// assertStartMapReachedForTest is the behavioural half of the report: the member's client
// must receive the start-map notification for the new run. An op16 failure reply (body
// {0, reason, 0}) instead is exactly the silent refusal the player sees as 点不动.
func assertStartMapReachedForTest(
	t *testing.T,
	name string,
	conn *bufferConn,
	runtime *runtimeDungeonState,
) {
	t.Helper()
	wantMsgID := currentDungeonStartMapNotificationForRuntime(runtime)
	startMapSeen := false
	for _, packet := range splitAllCurrentGameServerUpperPackets(t, conn.write.Bytes()) {
		if packet.Header.Classification != 0 {
			continue
		}
		if packet.Header.MsgID == wantMsgID {
			startMapSeen = true
		}
	}
	if !startMapSeen {
		t.Fatalf("%s never received start-map op%d for dungeon %d (wrote %d bytes)",
			name, wantMsgID, runtime.Dungeon.ID, conn.write.Len())
	}
}
