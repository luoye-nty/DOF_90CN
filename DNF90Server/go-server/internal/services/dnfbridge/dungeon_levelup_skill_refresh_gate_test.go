package dnfbridge

import (
	"strconv"
	"strings"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 契魔者（女鬼剑士 job 11，已转职）夹具：蛇腹剑技能 31 是**副本构造期**挂 actor 的一次性
// 被动，场景内再发 op19 会把武器形态与判定数据带掉。
func currentSwordDemonTestCharacter() dnfrepo.CharacterRecord {
	return dnfrepo.CharacterRecord{
		CharacterID: "41",
		AccountID:   "acc",
		Job:         "11",
		Level:       30,
		Stats:       map[string]int64{"grow_type": int64(currentSwordDemonFirstGrowType)},
	}
}

// currentUntransferredSwordDemonTestCharacter 是线上漏判的原始现场（角色 3）：**没转职**的
// 女鬼剑士也能学蛇腹剑，而且身上根本没有任何 grow_type 统计。仅按 职业+转职 判定会把它
// 当成"普通角色"，于是副本内升级照旧重发 op19、形态照掉。
func currentUntransferredSwordDemonTestCharacter() dnfrepo.CharacterRecord {
	return dnfrepo.CharacterRecord{
		CharacterID: "41",
		AccountID:   "acc",
		Job:         "11",
		Level:       22,
		Stats:       map[string]int64{"weakness_online_seconds": 3411},
	}
}

func currentSwordDemonTestSkill(enabled bool) dnfrepo.SkillRecord {
	entry := dnfrepo.SkillState{Enabled: enabled}
	if enabled {
		entry.Level = 1
	}
	return dnfrepo.SkillRecord{
		CharacterID: "41",
		Skills:      map[int64]dnfrepo.SkillState{currentSwordDemonSnakeSwordSkillID: entry},
		Points:      dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 30},
		Trees: map[int]dnfrepo.SkillTreeState{
			0: {
				Skills: map[int64]dnfrepo.SkillState{currentSwordDemonSnakeSwordSkillID: entry},
				Points: dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 30},
			},
		},
		Layouts: map[int]dnfrepo.SkillLayout{0: {1: uint16(currentSwordDemonSnakeSwordSkillID)}},
	}
}

// currentEveryPrebindPassiveSkillRecord 把规则表里所有一次性 actor 被动全部置为已启用。
// 它只用于一件事：证明**别的职业**即使（由于任何数据异常）带着这些技能 id，也不会被跳过。
func currentEveryPrebindPassiveSkillRecord(characterID string) dnfrepo.SkillRecord {
	build := func() map[int64]dnfrepo.SkillState {
		return map[int64]dnfrepo.SkillState{
			currentSwordDemonSnakeSwordSkillID:      {Level: 1, Enabled: true},
			currentSwordDemonSnakeSwordEXSkillID:    {Level: 1, Enabled: true},
			currentSoulBenderFearAuraSkillID:        {Level: 1, Enabled: true},
			currentKunoichiLeaveTraceEXSkillID:      {Level: 1, Enabled: true},
			currentMaleBrawlerRuleBreakSkillID:      {Level: 1, Enabled: true},
			currentDimensionWalkerChaosStoneSkillID: {Level: 1, Enabled: true},
			currentPaladinGloryOfLightSkillID:       {Level: 1, Enabled: true},
			currentPaladinCommandmentSkillID:        {Level: 1, Enabled: true},
			currentDragonKnightHeirSkillID:          {Level: 1, Enabled: true},
		}
	}
	return dnfrepo.SkillRecord{
		CharacterID: characterID,
		Skills:      build(),
		Points:      dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 30},
		Trees:       map[int]dnfrepo.SkillTreeState{0: {Skills: build()}},
		Layouts:     map[int]dnfrepo.SkillLayout{0: {}},
	}
}

// 门禁只保护蛇腹剑（女鬼剑士 job 11），并且不再看转职阶段。
func TestDecideCurrentMidSceneSkillRefreshOnlyProtectsSwordDemon(t *testing.T) {
	service := &Service{}
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "")

	// 风险表本身：只有女鬼剑士，只有蛇腹剑/蛇腹剑EX——这份断言就是"不影响其他职业"的根据。
	if len(currentMidSceneActorPassiveRiskSkillsByJob) != 1 {
		t.Fatalf("风险职业表必须只有女鬼剑士: %v", currentMidSceneActorPassiveRiskSkillsByJob)
	}
	riskSkills := currentMidSceneActorPassiveRiskSkillsByJob[currentATSwordmanJob]
	if len(riskSkills) != 2 || riskSkills[0] != currentSwordDemonSnakeSwordSkillID ||
		riskSkills[1] != currentSwordDemonSnakeSwordEXSkillID {
		t.Fatalf("风险技能表必须只有蛇腹剑/蛇腹剑EX: %v", riskSkills)
	}

	// 契魔者 + 蛇腹剑已启用 → 跳过场景内 op19。
	decision := service.decideCurrentMidSceneSkillRefresh(currentSwordDemonTestCharacter(), currentSwordDemonTestSkill(true))
	if decision.Send || decision.Mode != "auto" ||
		!strings.Contains(decision.Reason, "enabled_actor_passive") ||
		len(decision.AtRiskSkills) != 1 || decision.AtRiskSkills[0] != currentSwordDemonSnakeSwordSkillID {
		t.Fatalf("契魔者决策=%+v", decision)
	}

	// 线上漏判现场：未转职、没有任何 grow_type 统计，但蛇腹剑已启用 → 一样必须跳过。
	// （第一版门禁按 职业+转职 判定，这里会算成 firstGrowType=0，于是照旧发 op19。）
	untransferred := service.decideCurrentMidSceneSkillRefresh(currentUntransferredSwordDemonTestCharacter(), currentSwordDemonTestSkill(true))
	if untransferred.Send || !strings.Contains(untransferred.Reason, "enabled_actor_passive") ||
		len(untransferred.AtRiskSkills) != 1 || untransferred.AtRiskSkills[0] != currentSwordDemonSnakeSwordSkillID {
		t.Fatalf("未转职女鬼剑士决策=%+v", untransferred)
	}

	// 女鬼剑士但没学蛇腹剑：没有 actor 状态可丢，照常刷新。
	if decision := service.decideCurrentMidSceneSkillRefresh(currentSwordDemonTestCharacter(), currentSwordDemonTestSkill(false)); !decision.Send {
		t.Fatalf("未启用蛇腹剑时不应跳过: %+v", decision)
	}
	if decision := service.decideCurrentMidSceneSkillRefresh(currentUntransferredSwordDemonTestCharacter(), currentSwordDemonTestSkill(false)); !decision.Send {
		t.Fatalf("未转职且未启用蛇腹剑时不应跳过: %+v", decision)
	}

	// 只学了蛇腹剑EX（234）同样要跳过：它是同一套蛇腹剑形态。
	exOnly := currentSwordDemonTestSkill(false)
	exOnly.Skills = map[int64]dnfrepo.SkillState{currentSwordDemonSnakeSwordEXSkillID: {Level: 1, Enabled: true}}
	exOnly.Trees = map[int]dnfrepo.SkillTreeState{
		0: {Skills: map[int64]dnfrepo.SkillState{currentSwordDemonSnakeSwordEXSkillID: {Level: 1, Enabled: true}}},
	}
	exDecision := service.decideCurrentMidSceneSkillRefresh(currentUntransferredSwordDemonTestCharacter(), exOnly)
	if exDecision.Send || len(exDecision.AtRiskSkills) != 1 ||
		exDecision.AtRiskSkills[0] != currentSwordDemonSnakeSwordEXSkillID {
		t.Fatalf("蛇腹剑EX 决策=%+v", exDecision)
	}

	// 被动只存在于另一套技能方案（激活方案里没有）时同样要跳过：op19 是整套快照，
	// 客户端会按它重建全部方案。
	alternateOnly := currentSwordDemonTestSkill(false)
	alternateOnly.Skills = map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}}
	alternateOnly.Trees = map[int]dnfrepo.SkillTreeState{
		1: {Skills: map[int64]dnfrepo.SkillState{currentSwordDemonSnakeSwordSkillID: {Level: 1, Enabled: true}}},
	}
	alternateCharacter := currentUntransferredSwordDemonTestCharacter()
	alternateCharacter.Stats["skill_tree_index"] = 0
	if decision := service.decideCurrentMidSceneSkillRefresh(alternateCharacter, alternateOnly); decision.Send {
		t.Fatalf("备用方案里的蛇腹剑也应被判为风险: %+v", decision)
	}

	// 诊断开关：off 一律不发（用来验证客户端自身的等级切换是否也会掉形态）；
	// all 恢复修复前的旧行为。
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "off")
	if decision := service.decideCurrentMidSceneSkillRefresh(currentSwordDemonTestCharacter(), currentSwordDemonTestSkill(true)); decision.Send ||
		decision.Reason != "diagnostic_switch_off" {
		t.Fatalf("off 模式决策=%+v", decision)
	}
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "all")
	if decision := service.decideCurrentMidSceneSkillRefresh(currentSwordDemonTestCharacter(), currentSwordDemonTestSkill(true)); !decision.Send || decision.Mode != "all" {
		t.Fatalf("all 模式决策=%+v", decision)
	}
}

// 隔离证明：除女鬼剑士（job 11）以外的**每一个**职业，即使技能表里带着全部一次性 actor
// 被动 id，也必须照旧收到场景内 op19 刷新——门禁绝不能波及别的职业。
func TestCurrentMidSceneSkillRefreshNeverSkipsOtherJobs(t *testing.T) {
	service := &Service{}
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "")
	for job := 0; job <= 15; job++ {
		if byte(job) == currentATSwordmanJob {
			continue
		}
		character := dnfrepo.CharacterRecord{
			CharacterID: "42",
			AccountID:   "acc",
			Job:         strconv.Itoa(job),
			Level:       30,
			// 故意带上各种转职/觉醒组合，确认转职状态也不再参与判定。
			Stats: map[string]int64{"grow_type": int64((job%4)<<4 | (job % 6))},
		}
		decision := service.decideCurrentMidSceneSkillRefresh(character, currentEveryPrebindPassiveSkillRecord("42"))
		if !decision.Send {
			t.Fatalf("job=%d 必须继续收到技能刷新: %+v", job, decision)
		}
		if len(decision.AtRiskSkills) != 0 {
			t.Fatalf("job=%d 不应命中任何风险技能: %v", job, decision.AtRiskSkills)
		}
	}
}

// 副本内升级对契魔者不再发 op19（只留 op37/任务列表），并留下可核对的跳过日志。
// 未转职但学了蛇腹剑的女鬼剑士走同一条跳过路径——这正是线上漏判的角色形态。
func TestSendCurrentDungeonLevelUpSkillRefreshSkipsSwordDemon(t *testing.T) {
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "")
	connection := &bufferConn{}
	service := &Service{}
	session := &gameSession{conn: connection, connID: "sword-demon-levelup", selectedCharacterID: 41}
	if err := service.sendCurrentDungeonLevelUpSkillRuntimeRefresh(
		session,
		currentSwordDemonTestCharacter(),
		currentSwordDemonTestSkill(true),
		"unit_test_level_change",
	); err != nil {
		t.Fatalf("跳过路径不应报错: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("契魔者不应收到场景内 op19: %x", connection.write.Bytes())
	}
	// 线上现场：未转职、无 grow_type 统计，但蛇腹剑已启用 → 同样一发不发。
	connection.write.Reset()
	if err := service.sendCurrentDungeonLevelUpSkillRuntimeRefresh(
		session,
		currentUntransferredSwordDemonTestCharacter(),
		currentSwordDemonTestSkill(true),
		"unit_test_level_change_untransferred",
	); err != nil {
		t.Fatalf("未转职跳过路径不应报错: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("未转职女鬼剑士不应收到场景内 op19: %x", connection.write.Bytes())
	}
	// 对照：别的职业（job 2）带着同样的技能 id 时，必须真的收到 op19。
	otherCharacter := dnfrepo.CharacterRecord{CharacterID: "41", AccountID: "acc", Job: "2", Level: 30}
	connection.write.Reset()
	if err := service.sendCurrentDungeonLevelUpSkillRuntimeRefresh(
		session,
		otherCharacter,
		currentEveryPrebindPassiveSkillRecord("41"),
		"unit_test_other_job_still_refreshes",
	); err != nil {
		t.Fatalf("别的职业必须走原来的刷新路径: %v", err)
	}
	if connection.write.Len() == 0 {
		t.Fatalf("别的职业必须收到场景内 op19")
	}
	// 对照：把开关设成 all 时必须真的发包（恢复修复前的旧行为）。
	t.Setenv(currentMidSceneSkillRefreshSwitchEnv, "all")
	connection.write.Reset()
	if err := service.sendCurrentDungeonLevelUpSkillRuntimeRefresh(
		session,
		currentSwordDemonTestCharacter(),
		currentSwordDemonTestSkill(true),
		"unit_test_level_change_forced",
	); err != nil {
		t.Fatalf("all 模式应走原来的刷新路径: %v", err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("all 模式应发出 op19: header=%+v trailing=%x", packet.Header, trailing)
	}
}
