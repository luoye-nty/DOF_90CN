package dnfbridge

import (
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 超时空的翻牌金牌：**PVF 里 flag=1 的牌必须仍然是金牌**。
//
// 链路（证据 raid_reward_runtime.go:554-562、raid_reward_runtime_test.go:52-57）：
//
//	fiendwar.etc 的 [CLEAR REWARD ITEM] → [STATE REWARD] 行第 5 列 flag
//	→ anton_pvf.go:693/700  AntonStateReward.Flag
//	→ anton_reward_pvf.go:280  PoolEntry.Flag
//	→ anton_reward_pvf.go:559  Resolved.Flag          （ResolvePool 带出来）
//	→ raid_reward_runtime.go:564  RaidSettlementReward.Kind  ← 一条奖励行的**首字节**
//	→ result_flow_protocol.go:152  0x0258 行首字节
//
// 客户端把这一字节存在牌对象 `+0x44`，`sub_167FE00` 在**全队板（resultType 3）**
// 上按 `+0x44 == 1` 画金牌（0x0167FE1E）。
//
// 真机数据（tools/…/超时空_攻坚结束翻牌提交_根因与修复_20261002.md:56-67，PVF 实证）：
//
//	party_card  -1 800 490008037 0      ← 翻牌池四档，**全是 0**
//	party_card  -1 150 490008035 0
//	party_card  -1  35 490008036 0
//	party_card  -1  15 490008034 0
//	pcroom_card -1 100 10094733  0
//	squad_item  -1  90 490008038 0      ← 团队奖励五档，**后四档是 1**
//	squad_item  -1   5 490008672 1
//	squad_item  -1   3 490008039 1
//	squad_item  -1   1 490008040 1
//	squad_item  -1   1 490008818 1
//
// 也就是说超时空的金牌只可能从 `squad_item` 来；`party_card`（翻牌那四张）恒为 0。
// 本用例走**生产那条路**（`runtimeRaidRewardCatalogForState` → `resolveRuntimeRaidRewardPlan`
// → `runtimeRaidTeamSquadRewardBody`），并且用**真 Script.pvf**，所以：
//
//	把 raid_reward_runtime.go:564 的 `Kind: resolved.Flag` 改回写死的 0 → 必红；
//	把 runtimeRaidRewardPoolKinds 的 {"party_card","squad_item"} 调换顺序 → 必红
//	  （全队板取的是最后一行的位置启发式，调换后拿到的就是恒为 0 的 party_card）。
func TestRealPVFFiendWarGoldSquadItemRowStaysGoldOnTheWire(t *testing.T) {
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

	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, Name: "member", GroupIndex: 2, SlotOrder: 0},
	}
	lifecycle := newRuntimeRaidLifecycleState(now.Add(-time.Hour))
	lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	lifecycle.Phase = runtimeRaidDungeonPhaseOne
	lifecycle.RewardOpenedAt = now.Add(-time.Minute)
	lifecycle.RewardSelections[1001] = runtimeRaidRewardSelection{Mode: 0, Index: 2, ConfirmedAt: now.Add(-time.Minute)}
	lifecycle.RewardSelections[1002] = runtimeRaidRewardSelection{Mode: 0, Index: 3, ConfirmedAt: now.Add(-time.Minute)}
	lifecycle.RewardSkippedMembers = make(map[uint16]bool)
	// 结算只赔"清过门槛的人"：PhaseClearCounts 是战况表那一列，
	// FiendWarClearCounts 是超时空自己的 [MINIMAL DUNGEON CLEAR COUNT] 门（=1）。
	seedRuntimeRaidPhaseClearsForTest(&lifecycle, 1001, 1002)
	lifecycle.FiendWarClearCounts = map[uint16]int{1001: 1, 1002: 1}

	const raidKey = uint32(0x00c80452)
	state := runtimeRaidState{
		RaidKey:        raidKey,
		RaidMode:       runtimeRaidOwnerFiendWar,
		LeaderID:       1001,
		Started:        true,
		EntryCostPaid:  true,
		ChargedMembers: []uint16{1001, 1002},
		StartAttempt:   1,
		Members:        append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), members...),
		FiendWarConfig: config,
		Lifecycle:      lifecycle,
	}
	service := &Service{
		options:             options{gameUpperBodyCodec: gameUpperBodyCodecPlain, pvfPath: path},
		raids:               map[uint32]*runtimeRaidState{raidKey: &state},
		raidRewardCompleted: make(map[runtimeRaidRewardCompletionKey]runtimeRaidSettlementRewardCompletion),
		// 99 % 1000 = 99 → party_card 第一行（标记 0）；
		// 99 %  100 = 99 → squad_item 最后一行（权重 1、标记 1）。
		raidRewardRandom: func() uint64 { return 99 },
	}
	catalog, err := service.runtimeRaidRewardCatalogForState(state)
	if err != nil {
		t.Fatalf("超时空奖励目录加载失败: %v", err)
	}

	// (1) 先钉住 PVF 数据本身：金牌只能来自 squad_item。
	goldRows := 0
	for _, entry := range fixturePoolEntries(t, catalog, "squad_item") {
		if entry.Flag == 1 {
			goldRows++
		}
	}
	if goldRows == 0 {
		t.Fatal("fiendwar.etc 的 squad_item 池里一条 flag=1 的行都没有：真机上就不该有金牌了")
	}
	for _, entry := range fixturePoolEntries(t, catalog, "party_card") {
		if entry.Flag != 0 {
			t.Fatalf("party_card 出现 flag=%d 的行：翻牌那四张按 PVF 恒为 0", entry.Flag)
		}
	}

	// (2) 走生产的 plan：投出来的每一行都要带上它自己那条 PVF 行的 flag。
	plan, err := service.resolveRuntimeRaidRewardPlan(state, runtimeRaidDungeonPhaseOne, now, catalog)
	if err != nil {
		t.Fatalf("结算奖励 plan: %v", err)
	}
	if len(plan.Rewards.Members) != 2 {
		t.Fatalf("plan 的成员数 = %d, want 2", len(plan.Rewards.Members))
	}
	for _, member := range plan.Rewards.Members {
		if len(member.Rewards) != 2 {
			t.Fatalf("成员 %d 的奖励行 = %+v, want party_card + squad_item", member.MemberKey, member.Rewards)
		}
		if got := member.Rewards[0].Kind; got != 0 {
			t.Fatalf("成员 %d 的 party_card 首字节 = %d, want 0（PVF 标记 0）", member.MemberKey, got)
		}
		if got := member.Rewards[1].Kind; got != 1 {
			t.Fatalf("成员 %d 的 squad_item 首字节 = %d, want 1 —— "+
				"PVF 里那一条 flag=1 的稀有 squad_item 必须以 1 出现在 0x0258 行首字节上，"+
				"否则客户端 sub_167FE00 读到的 +0x44 不是 1，金牌永远画不出来",
				member.MemberKey, got)
		}
		if member.Rewards[1].ItemIDOrValue == 0 || member.Rewards[1].CountOrAux == 0 {
			t.Fatalf("成员 %d 的 squad_item 行 = %+v, want 有物品与数量", member.MemberKey, member.Rewards[1])
		}
	}

	// (3) 冻住的翻牌包体（resultType 1，每队一张揭示板）里，行首字节同样要是 1。
	// 第 0 行是 party_card（恒 0），第 1 行是 squad_item（这条 PVF 里是 1）。
	revealKinds := fixtureRewardRowKinds(t, plan.Body)
	if !fixtureEveryRowHasKind(t, revealKinds, 1, 1) {
		t.Fatalf("翻牌包体里各成员的第二行首字节 = %v, want 每一行都是 1\nbody=% X", revealKinds, plan.Body)
	}

	// (4) 全队板（resultType 3）——客户端就是在这张板上按 +0x44 == 1 画金牌的。
	boardBody, ok, err := runtimeRaidTeamSquadRewardBody(runtimeRaidSettlementRewardDelivery{Rewards: plan.Rewards})
	if err != nil || !ok {
		t.Fatalf("全队板 ok=%t err=%v", ok, err)
	}
	if boardBody[0] != runtimeRaidTeamRewardResultType {
		t.Fatalf("全队板 resultType = %d, want %d", boardBody[0], runtimeRaidTeamRewardResultType)
	}
	boardKinds := fixtureRewardRowKinds(t, boardBody)
	if !fixtureEveryRowHasKind(t, boardKinds, 0, 1) {
		t.Fatalf("全队板（resultType 3）里各成员那一行的首字节 = %v, want 每一行都是 1 —— "+
			"这一行必须是 squad_item（有 flag=1 的池），不是恒为 0 的 party_card\nbody=% X",
			boardKinds, boardBody)
	}
}

// fixturePoolEntries 读一个奖励池的原始行（含 flag）。
func fixturePoolEntries(t *testing.T, catalog *antonRewardPVFCatalog, kind string) []antonRewardPVFPoolEntry {
	t.Helper()
	pool, found := catalog.Pool(0, kind)
	if !found || len(pool.Entries) == 0 {
		t.Fatalf("超时空 phase0 %s 池为空 (found=%t)", kind, found)
	}
	return pool.Entries
}

// fixtureRewardRowKinds 把一条 0x0258 包体解成"每名成员的每一行首字节"。
// 布局：u8 resultType, u8 memberCount, 每人 {u16 key, u8 cardIndex, u8 rowCount,
// 行{u8 kind, u32 item, u16 count}}（BuildRaidSettlementRewardsBody）。
func fixtureRewardRowKinds(t *testing.T, body []byte) [][]byte {
	t.Helper()
	if len(body) < 2 {
		t.Fatalf("0x0258 包体太短: % X", body)
	}
	count := int(body[1])
	offset := 2
	out := make([][]byte, 0, count)
	for member := 0; member < count; member++ {
		if offset+4 > len(body) {
			t.Fatalf("0x0258 包体在成员 %d 处截断: % X", member, body)
		}
		rows := int(body[offset+3])
		offset += 4
		kinds := make([]byte, 0, rows)
		for row := 0; row < rows; row++ {
			if offset+7 > len(body) {
				t.Fatalf("0x0258 包体在成员 %d 第 %d 行截断: % X", member, row, body)
			}
			kinds = append(kinds, body[offset])
			offset += 7
		}
		out = append(out, kinds)
	}
	if offset != len(body) {
		t.Fatalf("0x0258 包体尾部多出 %d 字节: % X", len(body)-offset, body)
	}
	return out
}

// fixtureEveryRowHasKind 检查每名成员的第 index 行首字节都等于 want，且行数足够。
func fixtureEveryRowHasKind(t *testing.T, kinds [][]byte, index int, want byte) bool {
	t.Helper()
	for member, rows := range kinds {
		if len(rows) <= index {
			t.Fatalf("成员 %d 只有 %d 行，取不到第 %d 行", member, len(rows), index)
		}
		if rows[index] != want {
			return false
		}
	}
	return len(kinds) != 0
}
