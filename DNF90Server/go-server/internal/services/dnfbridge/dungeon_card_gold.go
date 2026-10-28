package dnfbridge

import (
	"context"
)

// 本文件保存工作区原有的"通关卡片金币"计算机制，与 DNF_NEW 的翻牌同步逻辑无关，
// 因此在移植翻牌逻辑时被单独隔离出来保留，避免静默改变地下城与血之祭坛的金币产出。
//
// DNF_NEW 的公式只保留等级参考与难度倍率：
//
//	floor(base * 175/1000 * difficultyRate)
//
// 工作区在此基础上补齐了 86JP CClearReward::generateSpecificItem 的其余因子
// （加权击杀数、队伍倍率、通关评价倍率、两段队伍拆分）。以 base=1338、
// 难度 2、40 次加权击杀、单人通关为例，两者分别给出 374 与 5990 金币。
//
// 如需完全对齐 DNF_NEW，删除本文件并改回：
//
//	func currentDungeonCardGoldAmount(rng *currentDungeonDropLCG, reference currentDungeonGoldReference, difficulty int) int64
//
// 同时把 dungeon_card_reward_runtime.go 里 currentDungeonCardGoldTerms 参数改回
// difficulty int，并删除 freeze 里的 goldTerms 构造。
// currentDungeonClearKillActorType classifies one accounted death for the
// clear-card gold tally, following 86JP DungeonInstance.RecordKill: actor
// types 3 and 8 are bosses, type 1 is a champion, and everything else -
// including super champions - counts as normal.
func currentDungeonClearKillActorType(monster runtimeDungeonMonster, bossDeath bool, bossActorType byte) byte {
	if bossDeath {
		if currentDungeonIsBossActorType(bossActorType) {
			return bossActorType
		}
		return 3
	}
	actorType, err := currentDungeonMonsterActorType(monster.Spawn)
	if err != nil {
		return 0
	}
	return actorType
}

func (runtime *runtimeDungeonState) recordCurrentDungeonClearKill(actorType byte) {
	if runtime == nil {
		return
	}
	switch {
	case currentDungeonIsBossActorType(actorType):
		runtime.clearKillBoss++
	case actorType == 1:
		runtime.clearKillChampion++
	default:
		runtime.clearKillNormal++
	}
}

// currentDungeonClearWeightedKills is 86JP's
// normal + 2*champion + 4*boss weighting of one run's accounted deaths.
func (runtime *runtimeDungeonState) currentDungeonClearWeightedKills() int64 {
	if runtime == nil {
		return 0
	}
	return runtime.clearKillNormal + 2*runtime.clearKillChampion + 4*runtime.clearKillBoss
}

// currentDungeonClearRankGoldRate is the additive [clear rank exp bonusrate]
// term the gold formula shares with the settlement's score bonus. The client
// reports its rank point in the op35 play result, which the settlement owner
// already stored before it freezes the card plan; a card frozen earlier - the
// op70 layout path - simply has no rank yet and contributes no bonus.
func (s *Service) currentDungeonClearRankGoldRate(runtime *runtimeDungeonState) float64 {
	if s == nil || runtime == nil ||
		len(runtime.settlementPlayResultBody) <= currentDungeonPlayResultClientRankPointOffset {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	resources, err := s.currentDungeonSettlementRewardResources(ctx)
	if err != nil {
		return 0
	}
	clientRankPoint := runtime.settlementPlayResultBody[currentDungeonPlayResultClientRankPointOffset]
	rate, ok := currentDungeonClearRankBonusRate(resources, clientRankPoint)
	if !ok {
		return 0
	}
	return rate
}

// currentDungeonCardGoldTerms carries everything outside the level reference
// that scales one clear card's gold. Every factor is read from the active PVF
// or counted by the run itself; none of them is a tuning constant.
type currentDungeonCardGoldTerms struct {
	Difficulty    int
	PartyMembers  int
	PartyRate     float64
	ClearRankRate float64
	WeightedKills int64
}

// currentDungeonCardGoldAmount reproduces CClearReward::generateSpecificItem
// as ported in 86JP ClearRewardGenerator.GenerateGoldCard.
//
// The old implementation kept only the level reference and the difficulty
// rate: `floor(base * 175/1000 * difficultyRate)`. That drops the run's
// weighted kill count, the party rate, the clear-rank rate and the two-stage
// party split, and it is why a level-85 clear paid a few hundred gold instead
// of tens of thousands - reference 1338 * 175/1000 = 234, times at most 2.0,
// is ~470 no matter how much of the dungeon was killed.
//
//	goldBase     = reference.Base * 175 / 1000
//	combined     = difficultyRate * partyRate * (1 + clearRankRate)
//	intermediate = int64(weightedKills * goldBase / 2 * combined)
//	amount       = int64(intermediate / 2 / partyMembers * combined)
//
// The two integer truncations are the reference's, not rounding noise: the
// clear factors are applied once on each side of the party split.
func currentDungeonCardGoldAmount(
	rng *currentDungeonDropLCG,
	reference currentDungeonGoldReference,
	terms currentDungeonCardGoldTerms,
) int64 {
	if rng == nil || reference.Base <= 0 {
		return 1
	}
	goldBase := reference.Base * 175 / 1000
	if goldBase < 1 {
		goldBase = 1
	}
	diffBonus := 1.0
	if terms.Difficulty >= 0 && terms.Difficulty < len(currentDungeonCardGoldDifficultyBonus) {
		diffBonus = currentDungeonCardGoldDifficultyBonus[terms.Difficulty]
	}
	partyMembers := terms.PartyMembers
	if partyMembers < 1 {
		partyMembers = 1
	} else if partyMembers > 4 {
		partyMembers = 4
	}
	partyRate := terms.PartyRate
	if partyRate <= 0 {
		partyRate = 1.0
	}
	rankRate := 1.0 + terms.ClearRankRate
	if rankRate <= 0 {
		rankRate = 1.0
	}
	combined := diffBonus * partyRate * rankRate
	weightedKills := terms.WeightedKills
	if weightedKills < 0 {
		weightedKills = 0
	}
	intermediate := int64(float64(weightedKills) * float64(goldBase) / 2.0 * combined)
	amount := int64(float64(intermediate) / 2.0 / float64(partyMembers) * combined)
	if amount < 1 {
		amount = 1
	}
	if reference.VariancePct > 0 {
		rangeSize := reference.VariancePct*2 + 1
		variance := (int64(rng.Next(int(rangeSize))) - reference.VariancePct) * amount / 100
		amount += variance
	}
	if amount < 1 {
		return 1
	}
	return amount
}
