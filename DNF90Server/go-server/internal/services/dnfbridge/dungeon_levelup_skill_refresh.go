package dnfbridge

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentMidSceneSkillRefreshSwitchEnv 是场景内技能信息（op19）刷新的诊断开关。
//
//	auto（默认）：按一次性 actor 被动风险决定（见下）；
//	off：场景内一律不发（诊断用：验证"客户端自身的等级切换会不会也掉形态"）；
//	all：一律发（即修复前的旧行为）。
const currentMidSceneSkillRefreshSwitchEnv = "DNF90_MID_SCENE_SKILL_REFRESH"

// currentMidSceneSkillRefreshMode 读取诊断开关；只认 off/all，其它（含空）都是 auto。
func currentMidSceneSkillRefreshMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(currentMidSceneSkillRefreshSwitchEnv))) {
	case "off":
		return "off"
	case "all":
		return "all"
	default:
		return "auto"
	}
}

// currentMidSceneSkillRefreshDecision 是"场景内要不要再发一次 op19"的判定结果。
type currentMidSceneSkillRefreshDecision struct {
	Send   bool
	Reason string
	Mode   string
	// AtRiskSkills 列出会因这次 op19 丢失 actor 状态的一次性被动技能。
	AtRiskSkills []int64
}

// decideCurrentMidSceneSkillRefresh 统一判定场景内 op19 刷新（副本内升级、副本内交任务）
// 是否可以安全下发。
//
// 背景：op19 是完整技能信息快照，客户端收到后重建技能运行时。对"一次性 actor 被动"
// （现场确认的是契魔者/女鬼剑士的蛇腹剑）来说，这类被动是在副本 mode1 构造玩家角色时挂
// 上去的，场景内重建不会重跑那次钩子，只会把形态/判定数据带掉——现场就是"副本内升级后
// 蛇腹剑外观消失、普攻打不到怪"。等级切换只改 SP/TP，而 SP/TP 已由同批的角色状态包
// （op37）下发，所以跳过不丢信息。
//
// 影响面被刻意压到最小：只有 currentMidSceneActorPassiveRiskSkillsByJob 里登记的职业 +
// 该被动已启用，才会跳过。其它职业（含女漫游、黑暗武士）一律照旧收到刷新，行为与本次修
// 复前完全一致。
func (s *Service) decideCurrentMidSceneSkillRefresh(
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
) currentMidSceneSkillRefreshDecision {
	mode := currentMidSceneSkillRefreshMode()
	switch mode {
	case "off":
		return currentMidSceneSkillRefreshDecision{Reason: "diagnostic_switch_off", Mode: mode}
	case "all":
		return currentMidSceneSkillRefreshDecision{Send: true, Mode: mode}
	}
	if atRisk, risky := currentMidSceneActorPassiveRebindRisk(character, skill); risky {
		return currentMidSceneSkillRefreshDecision{
			Reason:       "enabled_actor_passive_would_lose_weapon_form",
			Mode:         mode,
			AtRiskSkills: atRisk,
		}
	}
	return currentMidSceneSkillRefreshDecision{Send: true, Mode: mode}
}

// sendCurrentDungeonLevelUpSkillRuntimeRefresh rehydrates the current client's
// already-bound skill and passive runtime from the exact skill snapshot that
// committed with a monster EXP level change.  The client resets passive runtime
// controllers during a level transition; without this op19 replay, female
// ranger's Style passive (and its softening-count HUD) remains absent until a
// later scene rebuild.
//
// This is deliberately a projection only. It does not learn skills, grant
// profession rewards, alter layouts, or write any repository state. Dark
// Knight uses the same refresh, but its op19 is built with the persisted
// per-tree combo children before the adjacent editor projection is restored.
//
// 例外：只要**蛇腹剑（该职业登记在 currentMidSceneActorPassiveRiskSkillsByJob 里）已启用**
// （不分转职阶段，未转职的女鬼剑士也会学蛇腹剑），就**跳过**这次刷新——场景内重建技能运
// 行时会把这个一次 actor 被动的武器形态/判定数据带掉，而这次 op19 只重复 SP/TP（已由
// op37 下发）。
func (s *Service) sendCurrentDungeonLevelUpSkillRuntimeRefresh(
	session *gameSession,
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	source string,
) error {
	if session == nil {
		return fmt.Errorf("dungeon level-up skill refresh has no session")
	}
	decision := s.decideCurrentMidSceneSkillRefresh(character, skill)
	if !decision.Send {
		s.logGameEvent(session, "game-dungeon-levelup-skill-refresh-skipped",
			"source", source,
			"character_id", character.CharacterID,
			"job", character.Job,
			"level", character.Level,
			"mode", decision.Mode,
			"at_risk_skill_ids", fmt.Sprint(decision.AtRiskSkills),
			"reason", decision.Reason)
		return nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	if character.CharacterID != characterID || skill.CharacterID != characterID {
		return fmt.Errorf("dungeon level-up skill refresh character mismatch: selected=%s character=%s skill=%s", characterID, character.CharacterID, skill.CharacterID)
	}
	job, ok := characterJobByte(character)
	if !ok {
		return fmt.Errorf("dungeon level-up skill refresh has invalid job %q", character.Job)
	}
	activeTree, err := currentSceneActiveSkillTree(character)
	if err != nil {
		return fmt.Errorf("dungeon level-up skill refresh active tree: %w", err)
	}
	record := dnfrepo.CloneSkill(skill)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	body, activeSlots, err := s.buildCurrentSceneSkillInfoRuntimeBody(ctx, character, record, job, activeTree)
	if err != nil {
		return fmt.Errorf("dungeon level-up skill refresh body: %w", err)
	}
	activeLayout := record.Layouts[int(activeTree)]
	quickSkillIDs := make([]uint16, 0, len(activeSlots))
	for _, slot := range activeSlots {
		quickSkillIDs = append(quickSkillIDs, activeLayout[slot])
	}
	// learned_skill_ids 必须和场景构建路径一样填全：验证"场景内这一发 op19 到底带了什么"
	// 只能靠日志，缺字段会让日志看着像发了空快照。
	activeState, found := dnfrepo.SkillTree(record, int(activeTree))
	if !found {
		activeState = dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points}
	}
	learnedSkillIDs := make([]int64, 0, len(activeState.Skills))
	for skillID, state := range activeState.Skills {
		if state.Enabled && state.Level > 0 {
			learnedSkillIDs = append(learnedSkillIDs, skillID)
		}
	}
	sort.Slice(learnedSkillIDs, func(left, right int) bool {
		return learnedSkillIDs[left] < learnedSkillIDs[right]
	})
	projection := currentSceneSkillInfoProjection{
		body:            body,
		characterID:     character.CharacterID,
		job:             job,
		learnedCount:    len(activeState.Skills),
		learnedSkillIDs: learnedSkillIDs,
		activeSlots:     activeSlots,
		quickSkillIDs:   quickSkillIDs,
		activeTree:      activeTree,
	}
	return s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(session, projection, source)
}
