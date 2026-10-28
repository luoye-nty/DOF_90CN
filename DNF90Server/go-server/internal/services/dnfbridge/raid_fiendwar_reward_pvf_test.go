package dnfbridge

import (
	"os"
	"testing"

	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 超时空的「攻坚结束翻牌」必须在**提交**那一步成功 —— 那一步曾经被安东的阶段数
// 硬编码挡掉，四个人的牌翻了却被整批回滚。
//
// 真机 2026-10-02 13:52:48（raid 12058625，第 2 局 5 分 39 秒通关，四人）：
//
//	game-raid-reward-auto-flip-commit-failed raid_key=12058625 raid_phase=1 flipped=4
//	  error=anton reward PVF definition set is invalid: phases=1 want=2
//
// 原因：超时空的 fiendwar.etc 只有**一个** [PHASE]（`[PHASE MAX] 1`），而
// buildRaidRewardPVFPools 拿安东的 2 当通用常量；操作员看到的「翻牌翻不开」
// （8 张牌 / 一个「0」点不动）就是这条提交失败之后的客户端状态。
//
// 超时空的翻牌本来就是**攻坚结束**那一次（与卢克/安徒恩二阶段结束同一条路：
// 结算电影 → 倒计时 → 自动翻开 → 提交发奖），不是每个区域打完翻一次。
//
// 本用例走**生产那条路**（runtimeRaidRewardCatalogForState），不是直接调 loader
// —— 反向验证时改的是生产调用点的阶段数，测试必须跟着红。
func TestRealPVFFiendWarRaidEndRewardCatalogLoads(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	config, err := dnfraid.LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatalf("load Fiend War config: %v", err)
	}
	if config.Battle == nil || config.Battle.Raid == nil {
		t.Fatal("Fiend War config carries no synthesized raid config")
	}
	if got := len(config.Battle.Raid.Phases); got != currentFiendWarRewardPhaseCount {
		t.Fatalf("超时空的阶段数 = %d，want %d（fiendwar.etc 是 [PHASE MAX] 1）", got, currentFiendWarRewardPhaseCount)
	}
	service := &Service{}
	service.options.pvfPath = path
	state := runtimeRaidState{
		RaidMode:       runtimeRaidOwnerFiendWar,
		TailFlag:       1,
		FiendWarConfig: config,
	}
	catalog, err := service.runtimeRaidRewardCatalogForState(state)
	if err != nil {
		t.Fatalf("超时空结算奖励目录加载失败（真机 13:52 就是在这里失败）: %v", err)
	}
	// [CLEAR REWARD ITEM] 里的四行 party_card 就是攻坚结束那次翻牌的池子。
	for _, kind := range []string{"party_card", "pcroom_card", "squad_item"} {
		pool, found := catalog.Pool(0, kind)
		if !found || len(pool.Entries) == 0 || pool.TotalWeight == 0 {
			t.Fatalf("超时空 phase0 %s 池 = %+v found=%t", kind, pool, found)
		}
		resolved, resolveErr := catalog.ResolvePool(0, kind, &antonRewardPVFSequenceRNG{})
		if resolveErr != nil {
			t.Fatalf("resolve 超时空 %s: %v", kind, resolveErr)
		}
		definition, definitionErr := catalog.ResolveFinalItem(resolved.ItemID)
		if definitionErr != nil {
			t.Fatalf("resolve 超时空 %s final item %d: %v", kind, resolved.ItemID, definitionErr)
		}
		if definition.ItemID != resolved.ItemID || resolved.Count == 0 || resolved.ItemPVFPath == "" {
			t.Fatalf("超时空 %s resolved=%+v definition=%+v", kind, resolved, definition)
		}
	}
	rewardPhase, ok := runtimeRaidRewardPhaseIndex(state, runtimeRaidDungeonPhaseOne)
	if !ok || rewardPhase != len(config.Battle.Raid.Phases) {
		t.Fatalf("Fiend War single reward phase = %d ok=%t, want appended phase %d", rewardPhase, ok, len(config.Battle.Raid.Phases))
	}
	if single, found := catalog.Pool(rewardPhase, "party_card"); !found || len(single.Entries) == 0 {
		t.Fatalf("Fiend War single party_card pool missing at appended phase %d", rewardPhase)
	}
}

// 反向保护：安东仍然是**严格两个阶段**，被改坏成 1 个阶段必须继续报错，
// 不能因为放宽超时空而把安东的完整性检查一起放掉。
func TestAntonRewardPoolsStillRefuseASinglePhase(t *testing.T) {
	config := &dnfraid.AntonConfig{
		Phases: []dnfraid.AntonPhase{
			{Rewards: []dnfraid.AntonStateReward{
				{Kind: "party_card", Rank: -1, Weight: 100, DefinitionItemID: 10094735},
			}},
		},
	}
	if _, _, err := buildRaidRewardPVFPools(config, nil, false, currentAntonRewardPhaseCount); err == nil {
		t.Fatal("安东只有一个阶段时没有报错：阶段数完整性检查被放宽了")
	}
}
