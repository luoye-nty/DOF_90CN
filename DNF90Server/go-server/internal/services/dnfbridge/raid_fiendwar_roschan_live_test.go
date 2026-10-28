package dnfbridge

import (
	"encoding/binary"
	"os"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
)

// 真机 2026-10-02 12:34~12:38（raid 12058625，分片 012）的**原样复刻**：
//
//	04:34:58 char 21 走进 306000460（下侧 BOSS 房）→ bot boss state = 4
//	04:35:02 char 20 走进 306000463（上侧 BOSS 房）→ top = 4，且 bot 已是 4
//	          ⇒ PVF rule 131/132 触发 boss pattern init ⇒ 两侧各分到一颗球、pattern = 1111
//	04:35:35 起 char 1（上侧队长）与 char 22（下侧队长）各自反复报 kind=1 value=5
//	          服务端：rejected 0、dropped 0 —— 但机制**不推进**，球永远是同一颗。
//
// 本用例用**真 PVF 配置**把这段摆出来，然后要求：两队各自打掉**自己手上那颗球**
// 之后，机制条按颗数掉位（1111 → 111 → 11），而且打自己那颗不影响对面。
func TestRealPVFFiendWarRoshanPatternAdvancesFromBothSidesReports(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	service := &Service{gameSessions: map[uint16]*gameSession{}}
	service.options.pvfPath = path
	config, err := service.runtimeRaidFiendWarConfig()
	if err != nil {
		t.Fatalf("真 PVF 配置: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("战场: %v", err)
	}
	// 真机顺序：先下侧进 BOSS 房，再上侧。
	for _, mapID := range []int{306000460, 306000463} {
		if err := battle.Apply(raid.FiendWarEvent{Kind: "on enter map", ID: mapID}, time.Now().UTC()); err != nil {
			t.Fatalf("on enter map %d: %v", mapID, err)
		}
	}
	topClone := battle.Value("top boss state")
	botClone := battle.Value("bot boss state")
	// 客户端 0x9D80A0 那张表：5=宝石/虚无 6=火 7=冰 8=风。
	if topClone < 5 || topClone > 8 {
		t.Fatalf("上侧 state=%d，want 5..8（两侧都进 BOSS 房之后应当 pattern init 分球）", topClone)
	}
	if botClone < 5 || botClone > 8 || botClone == topClone {
		t.Fatalf("两侧应当拿到两颗**不同**的球：top=%d bot=%d", topClone, botClone)
	}
	if got := battle.Value("boss pattern state"); got != 1111 {
		t.Fatalf("机制条=%d，want 1111", got)
	}
	// 真机上这两个区域此刻是"被占"的（Enter 成功过）。dnfbridge 侧没有 set，
	// 所以直接按战场语义摆好 —— BossReport 的第一道门就是这一条。
	battle.Areas[30] = raid.FiendWarAreaState{Group: 1}
	battle.Areas[31] = raid.FiendWarAreaState{Group: 2}

	const raidKey = uint32(0x00c80440)
	topSeat := runtimeRaidDungeonSeat{Group: 1}
	botSeat := runtimeRaidDungeonSeat{Group: 2}
	members := []runtimeRaidMemberState{
		{CharID: 1, Name: "top-leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "top-member", GroupIndex: 2, SlotOrder: 0},
		{CharID: 22, Name: "bot-leader", GroupIndex: 3, SlotOrder: 0},
		{CharID: 21, Name: "bot-member", GroupIndex: 4, SlotOrder: 0},
	}
	// 真机：上侧的 run 是 char 20 开的（token=11），下侧是 char 21 开的（token=13）；
	// 两个队长（char 1 / char 22）都是 adopt 进来的。
	topProgress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: 4978, Seat: topSeat, CharacterID: 20,
		LifecycleToken: 11, Status: runtimeRaidDungeonProgressLoaded,
	}
	botProgress := runtimeRaidDungeonProgress{
		Phase: 1, MapID: 4953, Seat: botSeat, CharacterID: 21,
		LifecycleToken: 13, Status: runtimeRaidDungeonProgressLoaded,
	}
	state := &runtimeRaidState{
		RaidKey: raidKey, LeaderID: 1, Started: true, EntryCostPaid: true,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive, Phase: 1, FiendWar: battle,
			GroupOccupancy: map[runtimeRaidDungeonSeat]runtimeRaidDungeonBinding{
				topSeat: {RaidKey: raidKey, Phase: 1, MapID: 4978, LifecycleToken: 11},
				botSeat: {RaidKey: raidKey, Phase: 1, MapID: 4953, LifecycleToken: 13},
			},
			Dungeons: map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
				{MapID: 4978, Seat: topSeat}: topProgress,
				{MapID: 4953, Seat: botSeat}: botProgress,
			},
		},
	}
	service.raids = map[uint32]*runtimeRaidState{raidKey: state}

	// 两个队长各带一个队员；队长是 char 1（上）/ char 22（下）。
	newLeaderSession := func(charID, memberID uint16, dungeonID int, localToken, runToken uint64) *gameSession {
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "roschan-report-" + strconv.Itoa(int(charID)),
			channel:             channelcatalog.Channel{ID: 184, Type: 42},
			selectedCharacterID: charID,
			characterGeneration: 1,
		}
		session.dungeon.runtime = &runtimeDungeonState{
			Request:        dungeoncmd.SelectDungeonRequest{DungeonID: uint32(dungeonID)},
			lifecycleToken: localToken,
			raidDungeon: runtimeRaidDungeonBinding{
				RaidKey: raidKey, Phase: 1, MapID: dungeonID,
				LifecycleToken: runToken,
				// **真机就是这个形状**：adopt 进来的成员，本地 loadedNotified 是 false
				// （notifyRuntimeRaidDungeonLifecycleLocked 对"加入者"跳过 loaded，
				//   只有 completed 才在本地留一笔）。
				// 真机 12:34~12:38 的两个队长（char 1 / char 22）全都是 false，
				// 而 kind=1 机制上报只有他们在发 ⇒ 每一条都被静默丢掉。
				loadedNotified: false,
			},
			partyDungeonOwner: &runtimePartyDungeonOwner{
				partyID:  uint16(100 + dungeonID%100),
				leaderID: charID,
				members: []runtimePartyDungeonMember{
					{slot: 0, userID: charID, generation: 1},
					{slot: 1, userID: memberID, generation: 1},
				},
			},
		}
		service.gameSessions[charID] = session
		return session
	}
	topLeader := newLeaderSession(1, 20, 4978, 9, 11)
	botLeader := newLeaderSession(22, 21, 4953, 12, 13)

	body := func(kind, value int) []byte {
		payload := make([]byte, 8)
		binary.LittleEndian.PutUint32(payload[:4], uint32(kind))
		binary.LittleEndian.PutUint32(payload[4:], uint32(value))
		return payload
	}

	// 客户端 0x9D80A0 那张表：5=宝石/虚无 6=火 7=冰 8=风；球池 boss pattern state
	// 的四个十进制位就是这四颗球（1=还在）。真 PVF 的分球是随机的，所以期望值
	// 必须按"这一侧实际拿到哪颗球"算出来，不能写死 111/11。
	cloneWeight := func(clone int) int { return []int{1000, 100, 10, 1}[clone-5] }

	// 上侧先报：只清掉**它自己**那颗球那一位，并给它补一颗新的；下侧一点不动。
	if err := service.handleRuntimeFiendWarBoss(topLeader, body(1, topClone)); err != nil {
		t.Fatalf("上侧机制上报: %v", err)
	}
	if got, want := state.Lifecycle.FiendWar.Value("boss pattern state"), 1111-cloneWeight(topClone); got != want {
		t.Fatalf("上侧打掉自己那颗球（%d）之后机制条 = %d，want %d", topClone, got, want)
	}
	if got := state.Lifecycle.FiendWar.Value("bot boss state"); got != botClone {
		t.Fatalf("上侧打自己的球却动了对面：bot boss state=%d，want %d", got, botClone)
	}
	topSecond := state.Lifecycle.FiendWar.Value("top boss state")
	if topSecond < 5 || topSecond > 8 || topSecond == botClone {
		t.Fatalf("上侧补到的球 = %d，want 5..8 且不等于对面那颗 %d", topSecond, botClone)
	}
	// 下侧报它手上那颗：池子里只剩上侧没拿的那一颗。
	if err := service.handleRuntimeFiendWarBoss(botLeader, body(1, botClone)); err != nil {
		t.Fatalf("下侧机制上报: %v", err)
	}
	wantAfterBot := 1111 - cloneWeight(topClone) - cloneWeight(botClone)
	if got := state.Lifecycle.FiendWar.Value("boss pattern state"); got != wantAfterBot {
		t.Fatalf("下侧打掉自己那颗球（%d）之后机制条 = %d，want %d\n"+
			"（真机 12:35~12:38：两侧都在报 kind=1 value=5，rejected 0 / dropped 0，"+
			"但球一直停在第一颗）", botClone, got, wantAfterBot)
	}
	botSecond := state.Lifecycle.FiendWar.Value("bot boss state")
	if botSecond < 5 || botSecond > 8 || botSecond == topSecond {
		t.Fatalf("下侧补到的球 = %d，want 5..8 且不等于上侧那颗 %d", botSecond, topSecond)
	}
	// 现在各自手上那颗也必须能被报掉 —— 这就是"球会切换"。
	if err := service.handleRuntimeFiendWarBoss(topLeader, body(1, topSecond)); err != nil {
		t.Fatalf("上侧第二颗球的机制上报: %v", err)
	}
	if got, want := state.Lifecycle.FiendWar.Value("boss pattern state"), wantAfterBot-cloneWeight(topSecond); got != want {
		t.Fatalf("上侧第二颗球（%d）之后机制条 = %d，want %d", topSecond, got, want)
	}
}
