package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/logcrypt"
)

// ===========================================================================
// 超时空之战：8 个模拟客户端的全流程仿真
// ===========================================================================
//
// 目的：把真实客户端会走的每一步**用生产入口**跑一遍（不是直接摆状态），
// 每一步都断言账本/战况表/目标计数，覆盖正常流程与异常分支。
//
// 走的是这些生产入口：
//
//	op36 区域移动      handleTownSetUserArea         （含移动后的遭遇/进图那一段）
//	op1511 BOSS 机制   handleRuntimeFiendWarBoss
//	op42 放弃/撤退     handleDungeonGiveupGame（及战况表撤退的整队通道）
//	名册/编制          handleOnlineRaidCommand 之外，编制移动走 moveRuntimeRaidMember
//	副本生命周期       applyRuntimeRaidDungeonLifecycleEvent（selected/loaded/completed）
//	结算与翻牌         applyRuntimeRaidJoinedSquadClear / autoFlipRuntimeRaidPendingCards
//
// 每个客户端 = 一个 gameSession（真 PVF 的城镇/区域/副本数据 + 内存仓库）。
type fiendWarSimClient struct {
	id      uint16
	name    string
	session *gameSession
}

type fiendWarSim struct {
	t           *testing.T
	service     *Service
	raidKey     uint32
	config      *raid.FiendWarConfig
	clients     []*fiendWarSimClient
	now         time.Time
	logger      *packetLogger
	privateKey  []byte
	logErr      string
	bindings    map[uint16]runtimeRaidDungeonBinding
	localTokens map[uint16]uint64
}

// startLog 把服务端的包日志落到临时文件 —— 模拟失败时能直接看"服务端说了什么"，
// 与真机排查同一口径（game-fiendwar-* 那些行）。
//
// 这里**自己生成一对密钥**并把 flush 间隔压到 5ms：日志文件是加密的，模拟结束前
// 要回读（logcrypt.ReadFilePlaintext），用工作区那把私钥会引入外部依赖。
func (sim *fiendWarSim) startLog() {
	sim.t.Helper()
	publicKey, privateKey, err := logcrypt.GenerateKeyPair()
	if err != nil {
		sim.t.Fatalf("generate log key: %v", err)
	}
	recipient, err := logcrypt.ParseRecipientPublicKey(publicKey)
	if err != nil {
		sim.t.Fatalf("parse log key: %v", err)
	}
	writer, err := logcrypt.Open(logcrypt.Config{
		Dir:           filepath.Join(sim.t.TempDir(), "packet"),
		Prefix:        "sim",
		Ext:           ".log.enc",
		Recipient:     recipient,
		FlushInterval: 5 * time.Millisecond,
		QueueSize:     4096,
		MaxBytes:      1 << 20,
		RetentionDays: 1,
		Now:           func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		sim.t.Fatalf("open log writer: %v", err)
	}
	logger := &packetLogger{writer: writer}
	sim.service.packetLog.Store(logger)
	sim.logger, sim.privateKey = logger, privateKey
	sim.t.Cleanup(func() {
		sim.service.packetLog.Store(nil)
		_ = writer.Close()
	})
}

// events 返回日志里所有 EVENT 行（按时间）。
func (sim *fiendWarSim) events() []string {
	if sim.logger == nil || len(sim.privateKey) == 0 {
		return nil
	}
	// 写满 MaxBytes 会滚动到下一个分片，所以要把目录里所有 .enc 按名字顺序读全。
	dir := filepath.Dir(sim.logger.Path())
	files, globErr := filepath.Glob(filepath.Join(dir, "*.enc"))
	if globErr != nil || len(files) == 0 {
		sim.logErr = fmt.Sprintf("glob %s: %v (%d files)", dir, globErr, len(files))
		return nil
	}
	sort.Strings(files)
	out := make([]string, 0, 128)
	for _, file := range files {
		plain, records, truncated, readErr := logcrypt.ReadFilePlaintext(sim.privateKey, file)
		sim.logErr = fmt.Sprintf("%s records=%d truncated=%t plain=%d err=%v",
			filepath.Base(file), records, truncated, len(plain), readErr)
		for _, line := range strings.Split(string(plain), "\n") {
			if strings.Contains(line, "EVENT") {
				out = append(out, strings.TrimSpace(strings.TrimRight(line, "\r")))
			}
		}
	}
	return out
}

// eventsWith 只取某一种事件（子串匹配）。
func (sim *fiendWarSim) eventsWith(kind string) []string {
	out := make([]string, 0, 8)
	for _, line := range sim.events() {
		if strings.Contains(line, kind) {
			out = append(out, line)
		}
	}
	return out
}

// trace 给断言失败用：把最后 n 条超时空相关事件拼成一段人能读的文字。
func (sim *fiendWarSim) trace(n int) string {
	lines := make([]string, 0, n)
	all := sim.events()
	for _, line := range all {
		if !strings.Contains(line, "fiendwar") && !strings.Contains(line, "party-dungeon") &&
			!strings.Contains(line, "dungeon-entry") && !strings.Contains(line, "state-user-area") {
			continue
		}
		if index := strings.Index(line, "EVENT "); index >= 0 {
			line = line[index:]
		}
		if len(line) > 200 {
			line = line[:200]
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		path := ""
		if sim.logger != nil {
			path = sim.logger.Path()
		}
		return fmt.Sprintf("\n      (trace 不可用：logger=%t path=%q 已读事件=%d %s)",
			sim.logger != nil, path, len(all), sim.logErr)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "\n      " + strings.Join(lines, "\n      ")
}

// diagnose 给断言失败用：把"为什么没进图"的证据一次列全 —— 账本区域、客户端坐标、
// 选图页是否就绪、这一格的开关与占位、以及心跳那一趟能看到的名单。
func (sim *fiendWarSim) diagnose(area int) string {
	lines := make([]string, 0, 16)
	battle := sim.battle()
	lines = append(lines, fmt.Sprintf("area %d: dungeon=%d open=%t group=%d monster=%+v deadline=%s pattern=%d",
		area, sim.config.Battle.Areas[area].Dungeon, battle.AreaOpen(area), battle.Areas[area].Group,
		battle.Areas[area].Monster, battle.Areas[area].Deadline.Format(time.RFC3339), battle.Value("boss pattern state")))
	for _, client := range sim.clients {
		session := client.session
		position := session.townPositionSnapshot
		ready, reason := sim.service.currentTownEnterSelectReady(session)
		session.dungeon.mu.Lock()
		inDungeon := session.dungeon.runtime != nil
		session.dungeon.mu.Unlock()
		planned := sim.service.runtimeFiendWarPlannedEncounterPending(session, sim.now)
		lines = append(lines, fmt.Sprintf("char %d: 账本=%d 坐标=(%d,%d) selectReady=%t/%s 副本运行中=%t plannedPending=%t",
			client.id, battle.MemberArea(client.id, sim.groupOf(client.id)),
			position.TownID, position.AreaID, ready, reason, inDungeon, planned))
	}
	if state, ok := sim.service.runtimeRaidForMember(sim.clients[0].id); ok {
		objective := state.Lifecycle.Objectives[sim.config.Battle.Areas[area].Dungeon]
		lines = append(lines, fmt.Sprintf("status=%v started=%t entryPaid=%t objective=%v hp=%d 目标数=%d",
			state.Lifecycle.Status, state.Started, state.EntryCostPaid,
			objective.State, objective.RemainingHP, len(state.Lifecycle.Objectives)))
	}
	return "\n      " + strings.Join(lines, "\n      ")
}

// traceAll 不挑事件种类，只取最后 n 条 —— 排查"这一步根本没进到 Fiend War 代码"时用。
func (sim *fiendWarSim) traceAll(n int) string {
	all := sim.events()
	if len(all) == 0 {
		return fmt.Sprintf("\n      (没有任何事件：%s)", sim.logErr)
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	out := make([]string, 0, len(all))
	for _, line := range all {
		if index := strings.Index(line, "EVENT "); index >= 0 {
			line = line[index:]
		}
		if len(line) > 200 {
			line = line[:200]
		}
		out = append(out, line)
	}
	return "\n      " + strings.Join(out, "\n      ")
}

// traceKeys 取"含任一关键字"的最后 n 条事件 —— 排查进图被拒时用（blocked/failed/rejected）。
func (sim *fiendWarSim) traceKeys(keys []string, n int) string {
	lines := make([]string, 0, n)
	for _, line := range sim.events() {
		matched := false
		for _, key := range keys {
			if strings.Contains(line, key) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if index := strings.Index(line, "EVENT "); index >= 0 {
			line = line[index:]
		}
		if len(line) > 500 {
			line = line[:500]
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("\n      (没有匹配 %v 的事件；%s)", keys, sim.logErr)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "\n      " + strings.Join(lines, "\n      ")
}

func (sim *fiendWarSim) state() *runtimeRaidState {
	sim.service.raidMu.Lock()
	defer sim.service.raidMu.Unlock()
	state := sim.service.raids[sim.raidKey]
	if state == nil {
		sim.t.Fatal("攻坚状态不见了")
	}
	snapshot := cloneRuntimeRaidState(*state)
	return &snapshot
}

func (sim *fiendWarSim) battle() *raid.FiendWarBattle {
	if b := sim.state().Lifecycle.FiendWar; b != nil {
		return b
	}
	sim.t.Fatal("战场不见了")
	return nil
}

func (sim *fiendWarSim) client(id uint16) *fiendWarSimClient {
	for _, client := range sim.clients {
		if client.id == id {
			return client
		}
	}
	sim.t.Fatalf("没有 char %d 这个模拟客户端", id)
	return nil
}

func (sim *fiendWarSim) ids() []uint16 {
	out := make([]uint16, 0, len(sim.clients))
	for _, client := range sim.clients {
		out = append(out, client.id)
	}
	return out
}

// newFiendWarSim 建一个 memberCount 人的超时空攻坚（默认 8 个编制 × 1 人）。
func newFiendWarSim(t *testing.T, memberCount int) *fiendWarSim {
	t.Helper()
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	if memberCount <= 0 || memberCount > 8 {
		t.Fatalf("超时空最多 8 个编制，memberCount=%d", memberCount)
	}
	fixture := newRuntimeRaidRewardTestFixture(t, true)
	service := fixture.service
	service.options.pvfPath = path
	if err := service.preloadDungeonWorldMap(context.Background()); err != nil {
		t.Fatalf("preload world map: %v", err)
	}
	if err := service.preloadTownCatalog(context.Background()); err != nil {
		t.Fatalf("preload town catalog: %v", err)
	}
	config, err := service.runtimeRaidFiendWarConfig()
	if err != nil {
		t.Fatalf("真 PVF 超时空配置: %v", err)
	}
	state := service.raids[fixture.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.AntonConfig, state.FiendWarConfig = nil, config
	state.Lifecycle, err = newRuntimeRaidLifecycleStateFromModeAndType(
		config.Battle.Raid, runtimeRaidOwnerFiendWar, fixture.now, false, 0)
	if err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("战场: %v", err)
	}
	state.Lifecycle.FiendWar = battle
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	// **阶段开始那一趟**：PVF 里没有条件的规则就是"开局初始化"（设施/区域状态），
	// 生产端建完战场就会 Apply 一次；不跑它，兵营/前哨/雷达的状态全是 0，
	// 于是"这一格开着吗"永远是否 —— 真机上表现为"设施进不去、BOSS 门永远不开"。
	if err := battle.Apply(raid.FiendWarEvent{}, time.Now().UTC()); err != nil {
		t.Fatalf("阶段开局规则: %v", err)
	}
	battle.GroupAreas[1] = int(currentFiendWarWaitingAreaID)
	battle.GroupAreas[2] = int(currentFiendWarWaitingAreaID)

	// 8 个成员：每人一个编制（操作员定案：超时空 8 编制，每个编制只能放 1 人）。
	members := make([]runtimeRaidMemberState, 0, memberCount)
	charged := make([]uint16, 0, memberCount)
	for index := 0; index < memberCount; index++ {
		charID := uint16(1001 + index)
		members = append(members, runtimeRaidMemberState{
			CharID: charID, Name: fmt.Sprintf("c%d", charID), GroupIndex: byte(index + 1), SlotOrder: 0,
		})
		charged = append(charged, charID)
	}
	state.Members = append([]runtimeRaidMemberState(nil), members...)
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), members...)
	state.ChargedMembers = charged
	state.LeaderID = members[0].CharID
	service.onlinePlayers = newOnlinePlayerManager()

	sim := &fiendWarSim{
		t: t, service: service, raidKey: fixture.raidKey, config: config, now: time.Now().UTC(),
	}
	reused := map[uint16]*gameSession{1001: fixture.leader, 1002: fixture.member}
	for index := range members {
		charID := members[index].CharID
		session, ok := reused[charID]
		if !ok {
			session = &gameSession{conn: &bufferConn{}}
		}
		service.bindGameSessionCharacter(session, charID)
		session.channel = channelcatalog.Channel{ID: 261, Type: 42}
		// 注意 ID 要 > 0xff：currentPartySceneOwnerContext 用 channel.ID(<=0xff) 当场景归属，
		// 投影是按 townActorOwnerChannel(=0) 建的，ID 落在低字节就会判成 actor projection owner is stale。
		// 副本选择会核对"会话的账号 == 角色记录里的账号"，两边必须写成同一个。
		session.accountID = "account-" + strconv.Itoa(int(charID))
		session.selectPreviewObjectStateSent, session.currentSceneObjectListSent = true, true
		if err := fixture.repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: strconv.Itoa(int(charID)),
			AccountID:   "account-" + strconv.Itoa(int(charID)),
			Name:        members[index].Name,
			Level:       90,
			Stats: map[string]int64{
				"fatigue": 156, "town_id": 98, "area_id": int64(currentFiendWarWaitingAreaID),
				"pos_x": 240, "pos_y": 484, "direction": 5, "area_state": 0,
			},
		}); err != nil {
			t.Fatalf("save character %d: %v", charID, err)
		}
		if err := fixture.repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: strconv.Itoa(int(charID)),
			Slots:       make(map[string]dnfrepo.ItemStack),
		}); err != nil {
			t.Fatalf("seed inventory %d: %v", charID, err)
		}
		quests := dnfrepo.QuestRecord{
			CharacterID: strconv.Itoa(int(charID)),
			States:      map[int64]dnfrepo.QuestState{},
		}
		for _, areaID := range []int64{2, 3, 4, 5} {
			area, found := service.townArea(98, areaID)
			if !found {
				t.Fatalf("待机城镇 98 的区域 %d 在 PVF 里不存在", areaID)
			}
			for _, quest := range area.NeedQuests {
				quests.States[quest] = dnfrepo.QuestState{Status: "completed"}
			}
		}
		if err := fixture.repositories.Quest.Save(context.Background(), quests); err != nil {
			t.Fatalf("save quests %d: %v", charID, err)
		}
		bindDungeonSelectorOriginForTestAt(t, service, session, 98, currentFiendWarWaitingAreaID, 240, 484)
		registerPartyPeerEndpointForTest(session, byte(index+2), uint16(5200+index))
		seedPartyOwnerActorProjectionForTest(t, service, session, charID, 0)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: charID,
			ChannelID:   townPresenceChannelID(session),
			TownID:      98,
			AreaID:      currentFiendWarWaitingAreaID,
			InstanceKey: fixture.raidKey,
			PositionX:   240,
			PositionY:   484,
			Session:     session,
		})
		battle.MemberAreas[charID] = int(currentFiendWarWaitingAreaID)
		sim.clients = append(sim.clients, &fiendWarSimClient{id: charID, name: members[index].Name, session: session})
	}
	if err := service.applyRuntimeRaidParties(*state); err != nil {
		t.Fatalf("投影小队: %v", err)
	}
	syncRuntimeFiendWarObjectives(state)
	sim.startLog()
	return sim
}

// ---------------------------------------------------------------------------
// 动作（全部走生产入口）
// ---------------------------------------------------------------------------

// moveTo 走 op36（客户端"走到某一格"），顺带触发移动后的遭遇/进图那一段。
func (sim *fiendWarSim) moveTo(charID uint16, area int) error {
	sim.t.Helper()
	client := sim.client(charID)
	return sim.service.handleTownSetUserArea(client.session,
		buildTownMoveRequest(98, byte(area), 240+int16(area), 484, 5))
}

// reportPosition 走 op35（客户端报"我现在站在这儿"）。真机每次移动/场景加载完都会
// 发一条；超时空的遭遇战要求 selectReady（= 这一格报过坐标）才会把人带进副本，
// 所以移动之后必须补这一条 —— 少发它就是"站在格子里但永远不进图"。
func (sim *fiendWarSim) reportPosition(charID uint16) error {
	sim.t.Helper()
	client := sim.client(charID)
	area := sim.battle().MemberArea(charID, sim.groupOf(charID))
	return sim.service.handleTownSetUserPosition(client.session,
		buildTownPositionRequest(240+uint16(area), 484, 6, 100))
}

// beat 模拟服务端 1 Hz 的超时空心跳（生产实现是 scheduleRuntimeFiendWarTick）：
// 推进战场 Tick（区域时限/计时器/撤退），广播战况表，然后对每个在线成员尝试一次
// 遭遇战 —— 真机上"走进一格"之后最多一秒就会被这一趟带进副本。
func (sim *fiendWarSim) beat(times int) {
	sim.t.Helper()
	for index := 0; index < times; index++ {
		sim.now = sim.now.Add(time.Second)
		sim.service.raidMu.Lock()
		state := sim.service.raids[sim.raidKey]
		battle := state.Lifecycle.FiendWar.Clone()
		tickErr := battle.Tick(sim.now)
		if tickErr == nil {
			captureRuntimeFiendWarRetreats(state, battle)
			state.Lifecycle.FiendWar = battle
			syncRuntimeFiendWarObjectives(state)
			state.Lifecycle.Revision++
		}
		snapshot := cloneRuntimeRaidState(*state)
		sim.service.raidMu.Unlock()
		if tickErr != nil {
			sim.t.Fatalf("战场 Tick 失败: %v", tickErr)
		}
		if err := sim.service.broadcastRuntimeFiendWarBoard(snapshot, false, sim.now); err != nil {
			sim.t.Fatalf("战况表广播失败: %v", err)
		}
		for _, client := range sim.clients {
			if err := sim.service.tryRuntimeFiendWarEncounter(client.session, sim.now); err != nil {
				sim.t.Fatalf("char %d 的心跳遭遇返回错误: %v%s", client.id, err, sim.trace(10))
			}
		}
	}
}

// placeGuard 在某一格放一只守军（真机上是 PVF 的 [ON ENTER DUNGEON]/随机中立怪布下的），
// 让这一格成为"有副本可打"的格子，并把目标表同步成 open。
func (sim *fiendWarSim) placeGuard(area int) {
	sim.t.Helper()
	sim.service.raidMu.Lock()
	state := sim.service.raids[sim.raidKey]
	battle := state.Lifecycle.FiendWar
	current := battle.Areas[area]
	current.Monster = raid.FiendWarUnit{ID: 1, Type: 1}
	battle.Areas[area] = current
	syncRuntimeFiendWarObjectives(state)
	sim.service.raidMu.Unlock()
}

// walkTo 沿区域图走一条合法路径（客户端也只能一格一格走），返回走过的格子。
func (sim *fiendWarSim) walkTo(charID uint16, target int) error {
	sim.t.Helper()
	from := sim.battle().MemberArea(charID, sim.groupOf(charID))
	if from == target {
		return nil
	}
	path := sim.areaPath(from, target)
	if len(path) == 0 {
		sim.t.Fatalf("区域图里 %d 到 %d 没有通路", from, target)
	}
	for _, area := range path[1:] {
		if err := sim.moveTo(charID, area); err != nil {
			return fmt.Errorf("走到 %d（从 %d 出发，路径 %v）: %w", area, from, path, err)
		}
	}
	return nil
}

// areaPath 是区域图上的最短路（客户端眼里的"相邻才能走"）。
func (sim *fiendWarSim) areaPath(from, target int) []int {
	type node struct {
		area int
		path []int
	}
	queue := []node{{area: from, path: []int{from}}}
	seen := map[int]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.area == target {
			return current.path
		}
		next := append([]int(nil), sim.config.Battle.Areas[current.area].Next...)
		sort.Ints(next)
		for _, area := range next {
			if seen[area] {
				continue
			}
			seen[area] = true
			step := append(append([]int(nil), current.path...), area)
			queue = append(queue, node{area: area, path: step})
		}
	}
	return nil
}

// openBossRooms 走 PVF 那条路：三个兵站本营清掉之后两只 BOSS 才开门。
func (sim *fiendWarSim) openBossRooms() {
	sim.t.Helper()
	sim.service.raidMu.Lock()
	battle := sim.service.raids[sim.raidKey].Lifecycle.FiendWar
	for _, name := range []string{"top command center state", "mid command center state", "bot command center state"} {
		battle.Symbols[battle.Config.Symbols[name]] = 0
	}
	sim.service.raidMu.Unlock()
	if err := battle.Apply(raid.FiendWarEvent{}, sim.now); err != nil {
		sim.t.Fatalf("清掉兵站本营之后跑规则: %v", err)
	}
}

// reportBoss 走 op1511。
func (sim *fiendWarSim) reportBoss(charID uint16, kind, value int) error {
	payload := make([]byte, 8)
	binary.LittleEndian.PutUint32(payload[:4], uint32(kind))
	binary.LittleEndian.PutUint32(payload[4:], uint32(value))
	return sim.service.handleRuntimeFiendWarBoss(sim.client(charID).session, payload)
}

// moveSquad 走生产路径的编制移动（团长把某人拖进某一队）。
func (sim *fiendWarSim) moveSquad(sourceID, memberID uint16, group byte) (runtimeRaidState, bool) {
	sim.t.Helper()
	return sim.service.moveRuntimeRaidMember(sourceID, memberID, group)
}

// dungeonIDForArea 返回这一格的副本号。
func (sim *fiendWarSim) dungeonIDForArea(area int) int {
	sim.t.Helper()
	a, ok := sim.config.Battle.Areas[area]
	if !ok || a.Dungeon <= 0 {
		sim.t.Fatalf("区域 %d 没有副本", area)
	}
	return a.Dungeon
}

// areaForDungeon 反向查副本在哪一格。
func (sim *fiendWarSim) areaForDungeon(dungeon int) int {
	sim.t.Helper()
	area, ok := sim.battle().AreaForDungeon(dungeon)
	if !ok {
		sim.t.Fatalf("副本 %d 不在区域表里", dungeon)
	}
	return area.Index
}

// soloEnter 一个人走进某一格并进副本（真机上单人也能打设施：兵营/前哨/雷达/炮塔）。
func (sim *fiendWarSim) soloEnter(driver uint16, area int) {
	sim.t.Helper()
	if err := sim.walkTo(driver, area); err != nil {
		sim.t.Fatalf("char %d 走到 %d：%v%s", driver, area, err,
			sim.traceKeys([]string{"move-refused", "blocked"}, 4))
	}
	if err := sim.reportPosition(driver); err != nil {
		sim.t.Fatalf("char %d 报坐标：%v", driver, err)
	}
	sim.beat(2)
	sim.captureBindings()
	if sim.client(driver).session.dungeon.runtime == nil && sim.bindings[driver].RaidKey == 0 {
		sim.t.Fatalf("char %d 没有进 %d 这一格的副本%s", driver, area,
			sim.diagnose(area)+sim.traceKeys([]string{"dungeon-entry", "blocked", "fiendwar-encounter"}, 8))
	}
}

// enterArea 把整队带进一个有守军的区域：每个人先走到中转格并报坐标（op35），
// 驱动者再走进目标格、报坐标，然后跑两拍心跳 —— 这就是真机上"一起进图"的完整动作。
func (sim *fiendWarSim) enterArea(driver uint16, roster []uint16, staging, area int) {
	sim.t.Helper()
	for _, charID := range roster {
		if err := sim.moveTo(charID, staging); err != nil {
			sim.t.Fatalf("char %d 走到 %d：%v%s", charID, staging, err, sim.traceKeys([]string{"move-refused", "blocked"}, 4))
		}
		if err := sim.reportPosition(charID); err != nil {
			sim.t.Fatalf("char %d 报坐标：%v", charID, err)
		}
	}
	if err := sim.moveTo(driver, area); err != nil {
		sim.t.Fatalf("驱动者 char %d 走进 %d：%v%s", driver, area, err, sim.traceKeys([]string{"move-refused", "blocked"}, 4))
	}
	if err := sim.reportPosition(driver); err != nil {
		sim.t.Fatalf("驱动者报坐标：%v", err)
	}
	sim.beat(2)
	sim.captureBindings()
	for _, charID := range roster {
		if sim.client(charID).session.dungeon.runtime == nil && sim.bindings[charID].RaidKey == 0 {
			sim.t.Fatalf("同格 %d 的 char %d 没有进副本%s", area, charID,
				sim.diagnose(area)+sim.traceKeys([]string{"dungeon-entry", "blocked", "fiendwar-encounter"}, 10))
		}
	}
}

// captureBindings 记下每个人手上的副本绑定（token 是各自的那一份）。
//
// 真机上每个成员的 run token 不同（一个区域一条 run，adopt 进来的人带的是开局人
// 的 token、本地 token 是另一个），而通关处理会把 run 从账本里删掉、也会把当事人的
// runtime 收走 —— 所以"报通关"要用**进图时抓下来的那份绑定**，不能事后回查。
func (sim *fiendWarSim) captureBindings() map[uint16]runtimeRaidDungeonBinding {
	sim.t.Helper()
	out := make(map[uint16]runtimeRaidDungeonBinding, len(sim.clients))
	locals := make(map[uint16]uint64, len(sim.clients))
	for _, client := range sim.clients {
		client.session.dungeon.mu.Lock()
		if runtime := client.session.dungeon.runtime; runtime != nil {
			out[client.id] = runtime.raidDungeon
			// 本地 token 是**每个成员自己那一份**：副本生命周期上报走的是它。
			locals[client.id] = runtime.lifecycleToken
		}
		client.session.dungeon.mu.Unlock()
	}
	sim.bindings, sim.localTokens = out, locals
	return out
}

// leaveDungeon 模拟"这一格打完了"：按真实顺序走一遍副本生命周期
// （selected → loaded → completed）；生命周期那本账要求顺序，跳步会报 out of order。
func (sim *fiendWarSim) leaveDungeon(charID uint16, dungeonID int) {
	sim.t.Helper()
	client := sim.client(charID)
	client.session.dungeon.mu.Lock()
	runtime := client.session.dungeon.runtime
	token := uint64(0)
	if runtime != nil {
		token = runtime.raidDungeon.LifecycleToken
	}
	client.session.dungeon.mu.Unlock()
	if runtime == nil {
		// run 已经被处理过（别人先报完）：用进图时抓下来的那份绑定。
		if binding, ok := sim.bindings[charID]; ok {
			token = binding.LifecycleToken
			dungeonID = binding.MapID
		} else {
			sim.t.Fatalf("char %d 手上没有副本运行时，也没抓过绑定，没法收尾副本 %d", charID, dungeonID)
		}
	}
	sim.service.raidMu.Lock()
	phase := sim.service.raids[sim.raidKey].Lifecycle.Phase
	sim.service.raidMu.Unlock()
	for _, kind := range []runtimeRaidDungeonLifecycleKind{
		runtimeRaidDungeonSelected, runtimeRaidDungeonLoaded, runtimeRaidDungeonCompleted,
	} {
		event := runtimeRaidDungeonLifecycleEvent{
			Kind:           kind,
			RaidKey:        sim.raidKey,
			Phase:          phase,
			MapID:          dungeonID,
			LifecycleToken: token,
			CharacterID:    charID,
		}
		if err := sim.service.applyRuntimeRaidDungeonLifecycleEvent(event); err != nil {
			sim.t.Fatalf("char %d 报副本 %d %s: %v%s", charID, dungeonID, kind, err,
				sim.traceKeys([]string{"dungeon-lifecycle", "publish-failed", "joined-clear"}, 6))
		}
	}
}

// enterDungeonRoom 模拟"在副本里走到某一张房间地图"（真机是 op45 提交之后的房间边钩子）。
// BOSS 房的 [ON ENTER MAP] 规则就挂在这一脚上：两队都进屋才会 [BOSS PATTERN INIT]。
func (sim *fiendWarSim) enterDungeonRoom(charID uint16, roomMapID int64) {
	sim.t.Helper()
	client := sim.client(charID)
	client.session.dungeon.mu.Lock()
	runtime := client.session.dungeon.runtime
	if runtime == nil {
		client.session.dungeon.mu.Unlock()
		sim.t.Fatalf("char %d 不在副本里，进不了房间 %d", charID, roomMapID)
	}
	sim.service.notifyRuntimeFiendWarMapEnteredLocked(client.session, runtime, roomMapID, "sim_room_transition")
	client.session.dungeon.mu.Unlock()
}

// clearCounts 返回战况表上每个人的通关数。
func (sim *fiendWarSim) clearCounts() map[uint16]uint32 {
	sim.service.raidMu.Lock()
	defer sim.service.raidMu.Unlock()
	state := sim.service.raids[sim.raidKey]
	out := make(map[uint16]uint32, len(state.Lifecycle.PhaseClearCounts))
	for id, count := range state.Lifecycle.PhaseClearCounts {
		out[id] = count
	}
	return out
}

// ---------------------------------------------------------------------------
// 断言辅助
// ---------------------------------------------------------------------------

func (sim *fiendWarSim) requireArea(charID uint16, area int, what string) {
	sim.t.Helper()
	if got := sim.battle().MemberArea(charID, sim.groupOf(charID)); got != area {
		sim.t.Fatalf("%s：char %d 的账本区域 = %d，want %d", what, charID, got, area)
	}
}

func (sim *fiendWarSim) groupOf(charID uint16) byte {
	sim.service.raidMu.Lock()
	defer sim.service.raidMu.Unlock()
	state := sim.service.raids[sim.raidKey]
	member, ok := runtimeRaidFrozenMember(*state, charID)
	if !ok {
		sim.t.Fatalf("名册里没有 char %d", charID)
	}
	return runtimeRaidNormalizedGroup(member.GroupIndex)
}

func (sim *fiendWarSim) memberState(charID uint16) runtimeRaidMemberState {
	sim.service.raidMu.Lock()
	defer sim.service.raidMu.Unlock()
	state := sim.service.raids[sim.raidKey]
	for _, member := range state.Members {
		if member.CharID == charID {
			return member
		}
	}
	sim.t.Fatalf("名册里没有 char %d", charID)
	return runtimeRaidMemberState{}
}

// sortedIDs 便于稳定输出。
func sortedIDs(values []uint16) []uint16 {
	out := append([]uint16(nil), values...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
