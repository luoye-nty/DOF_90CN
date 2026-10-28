package dnfbridge

import (
	"context"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSwordmanJob                               = byte(0)
	currentSoulBenderFirstGrowType                   = byte(2)
	currentSoulBenderFirstAwakeningStage             = byte(1)
	currentSoulBenderFearAuraSkillID           int64 = 89
	currentActorPassiveThiefJob                      = byte(6)
	currentKunoichiFirstGrowType                     = byte(3)
	currentKunoichiLeaveTraceEXSkillID         int64 = 225
	currentActorPassiveATFighterJob                  = byte(7)
	currentMaleBrawlerFirstGrowType                  = byte(3)
	currentMaleBrawlerSecondAwakeningStage           = byte(2)
	currentMaleBrawlerRuleBreakSkillID         int64 = 228
	currentActorPassiveATMageJob                     = byte(8)
	currentDimensionWalkerFirstGrowType              = byte(5)
	currentDimensionWalkerSecondAwakeningStage       = byte(2)
	currentDimensionWalkerChaosStoneSkillID    int64 = 141
	currentATSwordmanJob                             = byte(11)
	currentSwordDemonFirstGrowType                   = byte(3)
	currentSwordDemonSnakeSwordSkillID         int64 = 31
	currentSwordDemonSnakeSwordEXSkillID       int64 = 234
	currentActorPassiveKnightJob                     = byte(12)
	currentPaladinFirstGrowType                      = byte(3)
	currentPaladinFirstAwakeningStage                = byte(1)
	currentPaladinGloryOfLightSkillID          int64 = 120
	currentPaladinCommandmentSkillID           int64 = 149
	currentDragonKnightFirstGrowType                 = byte(4)
	currentDragonKnightSecondAwakeningStage          = byte(2)
	currentDragonKnightHeirSkillID             int64 = 153
)

type currentDungeonActorPassivePrebindRule struct {
	job             byte
	firstGrowType   byte
	skillIDs        []int64
	requiredSkillID int64
	event           string
}

// A small set of current-PVF passives attach actor-owned controllers while
// full dungeon mode1 constructs the player actor. The ordinary dungeon route
// intentionally installs op19 after mode1, which is too late for those
// one-shot hooks. Keep this list restricted to current-PVF definitions that
// explicitly initialize on dungeon entry plus the reported Fear Aura actor
// symptom; the ordinary post-mode1 projection remains authoritative.
func currentDungeonActorPassivePrebindForCharacter(character dnfrepo.CharacterRecord) (currentDungeonActorPassivePrebindRule, bool) {
	job, valid := characterJobByte(character)
	if !valid {
		return currentDungeonActorPassivePrebindRule{}, false
	}
	growType := numericCharacterStatValue(character, "grow_type")
	if growType < 0 || growType > 0xff {
		return currentDungeonActorPassivePrebindRule{}, false
	}
	packedGrowType := byte(growType)
	firstGrowType := packedGrowType & 0x0f
	awakeningStage := (packedGrowType >> 4) & 0x0f
	switch {
	case job == currentATSwordmanJob && firstGrowType == currentSwordDemonFirstGrowType:
		return currentDungeonActorPassivePrebindRule{
			job:             currentATSwordmanJob,
			firstGrowType:   currentSwordDemonFirstGrowType,
			skillIDs:        []int64{currentSwordDemonSnakeSwordSkillID, currentSwordDemonSnakeSwordEXSkillID},
			requiredSkillID: currentSwordDemonSnakeSwordSkillID,
			event:           "game-sword-demon-snake-sword-prebind-send",
		}, true
	case job == currentSwordmanJob &&
		firstGrowType == currentSoulBenderFirstGrowType &&
		awakeningStage >= currentSoulBenderFirstAwakeningStage:
		return currentDungeonActorPassivePrebindRule{
			job:           currentSwordmanJob,
			firstGrowType: currentSoulBenderFirstGrowType,
			skillIDs:      []int64{currentSoulBenderFearAuraSkillID},
			event:         "game-soul-bender-fear-aura-prebind-send",
		}, true
	case job == currentActorPassiveThiefJob && firstGrowType == currentKunoichiFirstGrowType:
		return currentDungeonActorPassivePrebindRule{
			job:           currentActorPassiveThiefJob,
			firstGrowType: currentKunoichiFirstGrowType,
			skillIDs:      []int64{currentKunoichiLeaveTraceEXSkillID},
			event:         "game-kunoichi-leave-trace-prebind-send",
		}, true
	case job == currentActorPassiveATFighterJob &&
		firstGrowType == currentMaleBrawlerFirstGrowType &&
		awakeningStage >= currentMaleBrawlerSecondAwakeningStage:
		return currentDungeonActorPassivePrebindRule{
			job:           currentActorPassiveATFighterJob,
			firstGrowType: currentMaleBrawlerFirstGrowType,
			skillIDs:      []int64{currentMaleBrawlerRuleBreakSkillID},
			event:         "game-male-brawler-rule-break-prebind-send",
		}, true
	case job == currentActorPassiveATMageJob &&
		firstGrowType == currentDimensionWalkerFirstGrowType &&
		awakeningStage >= currentDimensionWalkerSecondAwakeningStage:
		return currentDungeonActorPassivePrebindRule{
			job:           currentActorPassiveATMageJob,
			firstGrowType: currentDimensionWalkerFirstGrowType,
			skillIDs:      []int64{currentDimensionWalkerChaosStoneSkillID},
			event:         "game-dimension-walker-chaos-stone-prebind-send",
		}, true
	case job == currentActorPassiveKnightJob &&
		firstGrowType == currentPaladinFirstGrowType &&
		awakeningStage >= currentPaladinFirstAwakeningStage:
		return currentDungeonActorPassivePrebindRule{
			job:           currentActorPassiveKnightJob,
			firstGrowType: currentPaladinFirstGrowType,
			skillIDs:      []int64{currentPaladinGloryOfLightSkillID, currentPaladinCommandmentSkillID},
			event:         "game-paladin-dungeon-entry-passive-prebind-send",
		}, true
	case job == currentActorPassiveKnightJob &&
		firstGrowType == currentDragonKnightFirstGrowType &&
		awakeningStage >= currentDragonKnightSecondAwakeningStage:
		return currentDungeonActorPassivePrebindRule{
			job:           currentActorPassiveKnightJob,
			firstGrowType: currentDragonKnightFirstGrowType,
			skillIDs:      []int64{currentDragonKnightHeirSkillID},
			event:         "game-dragon-knight-heir-prebind-send",
		}, true
	default:
		return currentDungeonActorPassivePrebindRule{}, false
	}
}

func currentNeedsSnakeSwordDungeonPrebind(character dnfrepo.CharacterRecord) bool {
	rule, found := currentDungeonActorPassivePrebindForCharacter(character)
	return found && currentSkillIDsContain(rule.skillIDs, currentSwordDemonSnakeSwordSkillID)
}

// currentMidSceneActorPassiveRiskSkillsByJob 登记**现场确认过**会被场景内 op19 打掉的
// "一次性 actor 被动"，按职业分组：女鬼剑士（含契魔者）的蛇腹剑。门禁要求"职业相符"和
// "该被动已启用"两条同时成立，所以别的职业一个技能 id 都不会被误判。
//
// 两轮现场教训：
//   - 第一版按 职业+转职阶段（job + firstGrowType）判定，漏掉了"未转职的女鬼剑士也能学
//     蛇腹剑"：线上角色 3 是 job=11、skill 31 已启用，但一张 grow_type 统计都没有，
//     算出来 firstGrowType=0 ≠ 3，门禁整体没命中，副本内升级照旧重发 op19、形态照掉。
//     所以转职阶段不再参与判定。
//   - 中间那版一度把 prebind 表里 7 个职业的被动一起纳入跳过范围（鬼泣/影舞者/男柔道/
//     次元行者/圣骑士/龙骑士），但那只是理论推导、没有现场证据，会平白改变这些职业的行
//     为。运营确认"被影响的就是蛇腹剑"，于是收窄回只有蛇腹剑。将来哪个职业现场复现，再
//     往这张表里加一行即可（判定逻辑不用动）。
var currentMidSceneActorPassiveRiskSkillsByJob = map[byte][]int64{
	currentATSwordmanJob: {
		currentSwordDemonSnakeSwordSkillID,
		currentSwordDemonSnakeSwordEXSkillID,
	},
}

// currentMidSceneActorPassiveRebindRisk 报告"在场景内再发一次 op19（技能信息）会不会
// 打掉这个角色的一次性 actor 被动"，并返回涉及的技能 id。
//
// 现场 2026-09-18：契魔者在副本内一升级，蛇腹剑形态就消失、普攻打不到怪——因为蛇腹剑
// 这类被动是**在副本 mode1 构造玩家角色时**挂上 actor 的（见本文件顶部的说明），场景内
// 重建技能运行时既不会重跑那个一次性钩子，又会把形态绑定的判定数据带掉。等级切换本身
// 只改 SP/TP（副本内升级的提交只动 skill.Points），而 SP/TP 已经由同一批的角色状态包
// （op37）下发，所以对这批被动跳过这次 op19 不丢任何信息。
func currentMidSceneActorPassiveRebindRisk(
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
) ([]int64, bool) {
	job, valid := characterJobByte(character)
	if !valid {
		return nil, false
	}
	candidates := currentMidSceneActorPassiveRiskSkillsByJob[job]
	atRisk := make([]int64, 0, len(candidates))
	for _, skillID := range candidates {
		if currentSceneSkillEnabled(character, skill, skillID) {
			atRisk = append(atRisk, skillID)
		}
	}
	if len(atRisk) == 0 {
		return nil, false
	}
	return atRisk, true
}

// currentSceneSkillEnabled 判断技能在客户端场景里是否真的"已学"：先看当前激活方案，再看
// 其它方案（op19 是整套快照，客户端会按它重建全部方案）。角色没有 skill_tree_index 统计
// 时（未转职角色很常见）默认就是 0 号方案，即顶层 Skills；激活方案取不到时也照样回落到
// 其它方案，避免"读不到方案 → 判成没风险 → 又发 op19"这种漏判。
func currentSceneSkillEnabled(
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	skillID int64,
) bool {
	if activeTree, err := currentSceneActiveSkillTree(character); err == nil {
		if state, found := dnfrepo.SkillTree(skill, int(activeTree)); found {
			if currentSkillStateEnabled(state.Skills[skillID]) {
				return true
			}
		}
	}
	if currentSkillStateEnabled(skill.Skills[skillID]) {
		return true
	}
	for _, state := range skill.Trees {
		if currentSkillStateEnabled(state.Skills[skillID]) {
			return true
		}
	}
	return false
}

func currentSkillStateEnabled(state dnfrepo.SkillState) bool {
	return state.Enabled && state.Level > 0
}

func currentNeedsSoulBenderFearAuraDungeonPrebind(character dnfrepo.CharacterRecord) bool {
	rule, found := currentDungeonActorPassivePrebindForCharacter(character)
	return found && currentSkillIDsContain(rule.skillIDs, currentSoulBenderFearAuraSkillID)
}

func currentSkillIDsContain(skillIDs []int64, skillID int64) bool {
	for _, candidate := range skillIDs {
		if candidate == skillID {
			return true
		}
	}
	return false
}

func currentProjectionHasEnabledSkill(projection currentSceneSkillInfoProjection, skillID int64) bool {
	for _, learnedSkillID := range projection.learnedSkillIDs {
		if learnedSkillID == skillID {
			return true
		}
	}
	return false
}

func currentProjectionHasAnyEnabledSkill(projection currentSceneSkillInfoProjection, skillIDs []int64) bool {
	for _, skillID := range skillIDs {
		if currentProjectionHasEnabledSkill(projection, skillID) {
			return true
		}
	}
	return false
}

func (s *Service) buildAndSendDungeonActorPassivePrebind(
	session *gameSession,
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	source string,
) (currentSceneSkillInfoProjection, bool, error) {
	rule, found := currentDungeonActorPassivePrebindForCharacter(character)
	if !found {
		return currentSceneSkillInfoProjection{}, false, nil
	}
	projection, ready, err := s.buildCurrentSceneSkillInfoProjection(session, ctx, character, source)
	if err != nil || !ready {
		return currentSceneSkillInfoProjection{}, false, err
	}
	if rule.requiredSkillID != 0 && !currentProjectionHasEnabledSkill(projection, rule.requiredSkillID) {
		return projection, true, fmt.Errorf(
			"sword demon dungeon prebind is missing enabled Snake Sword skill %d",
			rule.requiredSkillID,
		)
	}
	if !currentProjectionHasAnyEnabledSkill(projection, rule.skillIDs) {
		// Do not fabricate a paid/TP passive when the durable active tree does
		// not contain an enabled level. The already-built projection can still
		// be reused by the ordinary authoritative post-mode1 send.
		return projection, true, nil
	}
	if err := s.sendCurrentSceneSkillInfoProjection(session, projection, source); err != nil {
		return currentSceneSkillInfoProjection{}, false, err
	}
	s.logGameEvent(session, rule.event,
		"source", source,
		"character_id", character.CharacterID,
		"job", rule.job,
		"first_grow_type", rule.firstGrowType,
		"skill_ids", rule.skillIDs,
		"reason", "pvf_passive_must_exist_before_dungeon_mode1_actor_initialization")
	return projection, true, nil
}
