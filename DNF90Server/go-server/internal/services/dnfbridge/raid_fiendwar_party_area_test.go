package dnfbridge

import (
	"encoding/binary"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 「简易战况表」上那一行编制徽章 = **这一侧房间里所有编制**。
//
// 客户端那行徽章来自 op1455 成员行（{u32 op, u8 area, u32 charID, u32 复活时间}）
// 加上名册里每个人的编制。真机 2026-10-02 13:50（raid 12058625）操作员报的是：
//
//	「黑暗有 1、2 队进了 只显示 1 队（当前队伍队长编制是 1）；
//	  纯白 3、4 队进了 只显示 4 队（当前队伍队长编制是 4）」
//
// 那一侧只画得出一个徽章，是因为战场上只有**驱动者**的区域被记下来了：队友是
// prepareRuntimePartyDungeonEntry 备好、由驱动者按进图那一脚一起搬进去的，
// 他们**没有走过 op36**，battle.MemberAreas 里他们仍停在进图之前站的那一格
// （日志实证：上侧 c1 → 30、c20 停在 28；下侧 c22 → 31、c21 停在 29）。
//
// 官方资料图里那一行是 1/3/4 与 7/5/6/8 —— 四个编制都在，所以这一笔必须按
// "所有被带进去的人"补。
//
// 本用例按**客户端视角**判定：把 op1455 的行依次吃进去（首帧全量、之后只发变化），
// 最后客户端应当认为这一格里有两个人。
func TestFiendWarPartyAreaMarkPublishesEveryMemberRow(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	// 真机形状：c1（1 队）走进 30；c20（2 队）被一起带进 4978，但账本停在 28。
	battle.MemberAreas = map[uint16]int{1: 30, 20: 28}
	battle.GroupAreas = map[byte]int{1: 30, 2: 28}
	members := []runtimeRaidMemberState{
		{CharID: 1, Name: "top-leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 20, Name: "top-member", GroupIndex: 2, SlotOrder: 1},
	}
	const raidKey = uint32(0x00c80450)
	state := runtimeRaidState{
		RaidKey:       raidKey,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Started:       true,
		EntryCostPaid: true,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: &state}, gameSessions: map[uint16]*gameSession{}}
	now := time.Now().UTC()

	// 客户端视角：首帧（上一份快照是空的 → 全量）把两个人现在的区域吃进去。
	client := map[uint32]int{}
	first, err := runtimeFiendWarBoardPackets(state, runtimeRaidState{}, 0, now)
	if err != nil {
		t.Fatalf("board packets (first frame): %v", err)
	}
	applyOp1455(client, first)
	if client[1] != 30 || client[20] != 28 {
		t.Fatalf("夹具没造出问题：首帧后客户端认为 c1=%d c20=%d，want 30/28", client[1], client[20])
	}
	previous := cloneRuntimeRaidState(state)

	// 进图那一脚之后补的一笔：整队都在这一格里。
	if err := service.markRuntimeFiendWarPartyInArea(raidKey, members, 30, "test_encounter_entry"); err != nil {
		t.Fatalf("mark party area: %v", err)
	}
	marked := *service.raids[raidKey]
	after, err := runtimeFiendWarBoardPackets(marked, previous, 0, now)
	if err != nil {
		t.Fatalf("board packets (after mark): %v", err)
	}
	applyOp1455(client, after)

	for _, member := range members {
		if client[uint32(member.CharID)] != 30 {
			t.Fatalf("op1455 之后客户端认为 char %d 在区域 %d，want 30（整队都被带进这一格）\n"+
				"（真机 13:50：账本里被带进去的队友停在上一格，客户端那一侧就只画得出一个编制徽章）",
				member.CharID, client[uint32(member.CharID)])
		}
	}
}

// applyOp1455 按客户端语义把 op1455 的行吃进 client：op=1 记位置，op=2 清位置。
func applyOp1455(client map[uint32]int, packets []runtimeRaidWireNotification) {
	for _, packet := range packets {
		if packet.msgID != 1455 || len(packet.body) == 0 {
			continue
		}
		body := packet.body
		count := int(body[0])
		for row := 0; row < count; row++ {
			start := 1 + row*13
			if start+13 > len(body) {
				break
			}
			op := binary.LittleEndian.Uint32(body[start : start+4])
			area := int(body[start+4])
			charID := binary.LittleEndian.Uint32(body[start+5 : start+9])
			switch op {
			case 1:
				client[charID] = area
			case 2:
				delete(client, charID)
			}
		}
	}
}

// 账本里的个人区域一旦滞后，**玩家就走不动**：`MoveMember` 的相邻判定读的是账本
// （module 层 `b.GroupAreas[group] = b.MemberArea(character, group)` → `Move`），
// 于是"人在 11 点 7"会被按"16 与 7 不相邻"拒掉。
//
// 真机 2026-10-02 14:20（礼白666，raid 12058625，分片 20261002-016）：
//
//	06:18:58 game-fiendwar-party-area-marked area=16 char_ids=1   ← 被队友带进 area16 的副本
//	06:19:05 game-dungeon-full-town-route-send ... area_id=11     ← 打完回到 11
//	06:20:59 game-fiendwar-move-refused requested_area_id=7 keep_area_id=11
//	         reason=fiendwar areas are not adjacent
//
// PVF 的区域图：area 11 的 next = [7 10 12 16] **含 7**，area 16 的 next = [11 21] 不含 7。
// 所以这一脚本来就该放行 —— 挡它的是账本里的旧格子。
func TestFiendWarStaleLedgerAreaDoesNotBlockTheMove(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	// 先确认这份 PVF 的相邻关系（用例的判据全靠它）。
	nextOf := func(area int) []int {
		out := append([]int(nil), config.Battle.Areas[area].Next...)
		sort.Ints(out)
		return out
	}
	if !slices.Contains(nextOf(11), 7) || slices.Contains(nextOf(16), 7) {
		t.Fatalf("区域图与用例假设不符：next(11)=%v next(16)=%v", nextOf(11), nextOf(16))
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	// 真机形状：账本说 16（他刚被带进 area16 的副本），人其实已经回到 11。
	battle.MemberAreas = map[uint16]int{1: 16}
	battle.GroupAreas = map[byte]int{1: 16}
	members := []runtimeRaidMemberState{{CharID: 1, Name: "top-leader", GroupIndex: 1, SlotOrder: 0}}
	const raidKey = uint32(0x00c80451)
	state := runtimeRaidState{
		RaidKey:       raidKey,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Started:       true,
		EntryCostPaid: true,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: &state}, gameSessions: map[uint16]*gameSession{}}
	now := time.Now().UTC()

	// (1) 账本滞后时这一脚必须被拒 —— 复刻真机那句 move-refused。
	if err := state.Lifecycle.FiendWar.MoveMember(1, 1, 7, now); err == nil {
		t.Fatal("夹具没造出问题：账本停在 16 时 11→7 竟然放行了")
	}

	// (2) live-first 对齐之后必须放行（这就是 town_move 那一脚做的第一件事）。
	synced := service.syncRuntimeFiendWarMemberAreaFromLivePosition(1, 11, "test_fiendwar_move")
	if synced.RaidKey == 0 || synced.Lifecycle.FiendWar == nil {
		t.Fatal("live-first 对齐没有返回战场状态")
	}
	if got := synced.Lifecycle.FiendWar.MemberArea(1, 1); got != 11 {
		t.Fatalf("live-first 之后账本 = %d，want 11", got)
	}
	if err := synced.Lifecycle.FiendWar.MoveMember(1, 1, 7, now); err != nil {
		t.Fatalf("对齐之后 11→7 仍被拒: %v（真机 14:20 就是这样卡住的）", err)
	}
}

// 出副本那一脚（打完自动回城）必须把账本落到**目的地那一格**，否则下一脚移动会被
// 旧格子挡死 —— 这是上面那条用例的上游。
func TestFiendWarDungeonExitMarksTheDestinationArea(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := raid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	battle, err := raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatalf("battle: %v", err)
	}
	battle.MemberAreas = map[uint16]int{1: 16}
	battle.GroupAreas = map[byte]int{1: 16}
	members := []runtimeRaidMemberState{{CharID: 1, Name: "top-leader", GroupIndex: 1, SlotOrder: 0}}
	const raidKey = uint32(0x00c80452)
	state := runtimeRaidState{
		RaidKey:       raidKey,
		RaidMode:      runtimeRaidOwnerFiendWar,
		Started:       true,
		EntryCostPaid: true,
		Members:       append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
		Lifecycle: runtimeRaidLifecycleState{
			Status:   runtimeRaidLifecyclePhaseActive,
			Phase:    1,
			FiendWar: battle,
		},
	}
	service := &Service{raids: map[uint32]*runtimeRaidState{raidKey: &state}, gameSessions: map[uint16]*gameSession{}}
	if err := service.markRuntimeFiendWarMemberArea(1, 11, "test_fiendwar_clear_auto_return"); err != nil {
		t.Fatalf("mark member area: %v", err)
	}
	got := service.raids[raidKey].Lifecycle.FiendWar.MemberArea(1, 1)
	if got != 11 {
		t.Fatalf("出副本之后账本 = %d，want 11（回去的那一格）", got)
	}
}
