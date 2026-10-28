package dnfbridge

import (
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// testFiendWarArgs 把一个整数包成规则指令的参数，形状与 PVF 解析出来的 token 一致
// （matches/act 只读 Args[0].Int）。
func testFiendWarArgs(values ...int) []dnfpvf.Token {
	tokens := make([]dnfpvf.Token, 0, len(values))
	for _, value := range values {
		tokens = append(tokens, dnfpvf.Token{Kind: dnfpvf.TokenInt, Raw: strconv.Itoa(value), Int: int64(value)})
	}
	return tokens
}

// 超时空：走进 **BOSS 那张地图** 才把这一侧的 BOSS 机制推进到"四颗星球出场"，而
// 不是走进副本就算。
//
// 真机 2026-10-02（两两进罗什，分片 20261002-006）的形状：四个球一个都不出现、
// 罗什不给打，62 条 op1511 全是 kind=0 value=100（客户端压根没进机制阶段）。
// PVF 的状态机是：
//
//	[ON ENTER DUNGEON] 4978      → top boss state = 2   （还没进机制）
//	[ON ENTER MAP]     306000463 → top boss state = 5   （四颗星球出场）
//	客户端每打掉一颗报一条 kind=1 → 5→6→7→8 → 9（伤害窗口）→ BOSS 死 → 10
//
// 而服务端只在**副本 finish-loading** 那一次触发 on enter map，那一刻人在
// 0:0 起始房（306000461），于是带过去的是起始地图 ID，什么规则都没命中；
// 玩家随后走进 BOSS 房时没有任何代码再触发它 —— state 永远停在 2。
//
// 本用例用一条合成规则钉住"房间边"这件事：只有走进 306000463 才推进，
// 走起始房不推进、重复走不重复推进。
func TestFiendWarBossRoomEnterMapOpensTheBossPattern(t *testing.T) {
	const (
		raidKey         = uint32(0x00c803e8)
		startRoomMapID  = 306000461
		bossRoomMapID   = 306000463
		bossStateSymbol = 100
	)
	config := &raid.FiendWarBattleConfig{
		Raid:        runtimeRaidBridgeTestAntonConfig(),
		WaitingTown: 98,
		WaitingArea: 2,
		Areas: map[int]raid.FiendWarArea{
			2:  {Index: 2},
			30: {Index: 30, Dungeon: 4978, Kind: "top boss", Symbol: bossStateSymbol},
		},
		Symbols: map[string]uint32{"top boss state": bossStateSymbol, "boss pattern state": 101},
		// 与真 PVF 里 [ON ENTER MAP] 306000463 那一条同义：走进 BOSS 房 → state 5。
		Rules: []raid.FiendWarRule{{
			Conditions: []raid.FiendWarInstruction{{Op: "on enter map", Args: testFiendWarArgs(bossRoomMapID)}},
			Actions: []raid.FiendWarInstruction{{
				Op: "set", Symbol: bossStateSymbol, Args: testFiendWarArgs(5),
			}},
		}},
	}
	battle, err := raid.NewFiendWarBattle(config)
	if err != nil {
		t.Fatalf("fixture 战场: %v", err)
	}
	// 入场值：真机 2026-10-01 实测是 2（还没有任何机制）。
	battle.Symbols[bossStateSymbol] = 2
	state := &runtimeRaidState{
		RaidKey:       raidKey,
		Started:       true,
		EntryCostPaid: true,
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
		},
	}
	service := &Service{
		raids:        map[uint32]*runtimeRaidState{raidKey: state},
		gameSessions: map[uint16]*gameSession{},
	}
	session := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-boss-map-enter",
		channel:             channelcatalog.Channel{ID: 184, Type: 42},
		selectedCharacterID: 1,
		characterGeneration: 1,
	}
	runtime := &runtimeDungeonState{
		Request: dungeoncmd.SelectDungeonRequest{DungeonID: 4978},
		raidDungeon: runtimeRaidDungeonBinding{
			RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 1, loadedNotified: true,
		},
	}
	session.dungeon.runtime = runtime

	// 读回状态必须走 state.Lifecycle.FiendWar：切换路径是在**克隆**上跑规则、
	// 成功后才替换回去的，本地那个 battle 变量已经不是权威了。
	value := func() int { return state.Lifecycle.FiendWar.Value("top boss state") }

	// [反例] 起始房：一个字都不该改。真机上 finish-loading 带的就是这一张地图，
	// 它不推进机制正是这条缺陷的另一半。
	service.notifyRuntimeFiendWarMapEnteredLocked(session, runtime, startRoomMapID, "test")
	if got := value(); got != 2 {
		t.Fatalf("起始房推进了 BOSS 机制：state=%d，want 2", got)
	}

	// [本用例的核心] 走进 BOSS 房：必须推到 5 —— 客户端只有看到 5 才放那四颗星球。
	service.notifyRuntimeFiendWarMapEnteredLocked(session, runtime, bossRoomMapID, "test")
	if got := value(); got != 5 {
		t.Fatalf("走进 BOSS 房没有开启机制：top boss state=%d，want 5\n"+
			"（真机 2026-10-02 两两进罗什：四个球一个都不出现、罗什不给打）", got)
	}

	// 幂等：同一次进图里再走一遍不能重复推进（state != 2 那条守卫）。
	state.Lifecycle.FiendWar.Symbols[bossStateSymbol] = 3
	service.notifyRuntimeFiendWarMapEnteredLocked(session, runtime, bossRoomMapID, "test")
	if got := value(); got != 3 {
		t.Fatalf("重复进房把已经推进过的状态改了：state=%d，want 3", got)
	}
	state.Lifecycle.FiendWar.Symbols[bossStateSymbol] = 2

	// [反例] 不是超时空频道：一次都不触发。
	other := &gameSession{
		conn:                &bufferConn{},
		connID:              "fiendwar-boss-map-enter-not-fiendwar",
		channel:             channelcatalog.Channel{ID: 184, Type: 1},
		selectedCharacterID: 1,
		characterGeneration: 1,
	}
	other.dungeon.runtime = runtime
	service.notifyRuntimeFiendWarMapEnteredLocked(other, runtime, bossRoomMapID, "test")
	if got := value(); got != 2 {
		t.Fatalf("非超时空频道推进了 BOSS 机制：state=%d，want 2", got)
	}

	// [反例] 没有 loadedNotified（还没进副本）：不触发。
	notLoaded := *runtime
	notLoaded.raidDungeon.loadedNotified = false
	service.notifyRuntimeFiendWarMapEnteredLocked(session, &notLoaded, bossRoomMapID, "test")
	if got := value(); got != 2 {
		t.Fatalf("没进副本就推进了 BOSS 机制：state=%d，want 2", got)
	}

	// [反例] 别的副本（不在任何 BOSS 区域名下）：不触发。
	otherDungeon := *runtime
	otherDungeon.Request = dungeoncmd.SelectDungeonRequest{DungeonID: 4955}
	service.notifyRuntimeFiendWarMapEnteredLocked(session, &otherDungeon, bossRoomMapID, "test")
	if got := value(); got != 2 {
		t.Fatalf("不相干的副本推进了 BOSS 机制：state=%d，want 2", got)
	}
}

// 超时空：**队长**按下撤退（无包体 op42）时，整支临时队伍一起回城；队员自己按不算。
//
// 操作员 2026-10-02 定案；真机 10:29:10 只有队长一个人回了城（同队的 char 21 留在
// 格里，23 秒后连点 op72 被 settlement_result_not_ready 拒了 119 次）。
func TestFiendWarLeaderRetreatPullsTheWholeTemporaryParty(t *testing.T) {
	service := &Service{gameSessions: map[uint16]*gameSession{}}
	owner := &runtimePartyDungeonOwner{
		partyID:  22,
		leaderID: 1,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 1},
			{slot: 1, userID: 21},
		},
	}
	runtime := &runtimeDungeonState{
		raidDungeon:       runtimeRaidDungeonBinding{RaidKey: 12058625, Phase: 1, MapID: 4978, LifecycleToken: 20},
		partyDungeonOwner: owner,
	}
	fiendWar := &gameSession{
		conn:                &bufferConn{},
		channel:             channelcatalog.Channel{ID: 184, Type: 42},
		selectedCharacterID: 1,
	}
	fiendWar.dungeon.runtime = runtime
	// gameSession 里带锁：不能靠拷贝造变体（go vet 会拦），每条都用新会话。
	newSession := func(channelType byte, charID uint16) *gameSession {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "fiendwar-retreat-" + strconv.Itoa(int(charID)),
			channel:             channelcatalog.Channel{ID: 184, Type: channelType},
			selectedCharacterID: charID,
		}
		session.dungeon.runtime = runtime
		return session
	}

	// 队长：拉全队，名单就是这支临时队伍。
	plan := service.runtimeFiendWarPartyRetreatPlanFor(fiendWar, runtime)
	if !plan.armed {
		t.Fatal("队长的撤退没有拉全队（真机 10:29:10 就是这一脚，只有他一个人回了城）")
	}
	if plan.leader != 1 || plan.partyID != 22 || plan.raidKey != 12058625 {
		t.Fatalf("撤退计划抓错了人/队：%+v", plan)
	}
	if len(plan.members) != 2 || plan.members[1] != 21 {
		t.Fatalf("撤退计划的成员名单不对：%+v", plan.members)
	}

	// 队员：自己不带队，走原来的单人退出通道。
	if got := service.runtimeFiendWarPartyRetreatPlanFor(newSession(42, 21), runtime); got.armed {
		t.Fatalf("队员自己按撤退把全队拉走了：%+v", got)
	}

	// 非超时空：一个字不改（安东/卢克）。
	if got := service.runtimeFiendWarPartyRetreatPlanFor(newSession(1, 1), runtime); got.armed {
		t.Fatalf("非超时空也拉全队了：%+v", got)
	}

	// 没有临时队伍（单人进图）：没人可拉。
	soloRuntime := &runtimeDungeonState{raidDungeon: runtime.raidDungeon}
	solo := newSession(42, 1)
	solo.dungeon.runtime = soloRuntime
	if got := service.runtimeFiendWarPartyRetreatPlanFor(solo, soloRuntime); got.armed {
		t.Fatalf("没有临时队伍却立了撤退计划：%+v", got)
	}

	// 队员离线时那一趟是个空操作，不许崩、也不许发任何东西。
	service.runRuntimeFiendWarPartyRetreat(plan, "test")
	// 未立计划时同样是空操作。
	service.runRuntimeFiendWarPartyRetreat(runtimeFiendWarPartyRetreatPlan{}, "test")
}

// dungeonRuntimeOwnsCharacter 的语义在本用例里被间接依赖：撤退扇出只带"这一份
// runtime 真正属于他"的成员，免得把别人的副本算成他的。这条把它钉住，
// 免得将来"谁的副本"变了而撤退带错人。
func TestFiendWarPartyRetreatOwnershipPredicate(t *testing.T) {
	runtime := &runtimeDungeonState{
		raidDungeon: runtimeRaidDungeonBinding{RaidKey: 1},
	}
	if dungeonRuntimeOwnsCharacter(runtime, 1) {
		t.Fatal("没有角色绑定的 runtime 不该认领任何人")
	}
	runtime.Character = dnfrepo.CharacterRecord{CharacterID: "1"}
	if !dungeonRuntimeOwnsCharacter(runtime, 1) {
		t.Fatal("runtime 绑定的角色应当被认领")
	}
	if dungeonRuntimeOwnsCharacter(runtime, 2) {
		t.Fatal("别人的角色不该被这份 runtime 认领")
	}
	runtime.partyDungeonOwner = &runtimePartyDungeonOwner{leaderID: 1}
	if !dungeonRuntimeOwnsCharacter(runtime, 1) {
		t.Fatal("有队伍时队长本人仍应被认领")
	}
	_ = worldmap.DungeonRunActive
	_ = time.Second
}
