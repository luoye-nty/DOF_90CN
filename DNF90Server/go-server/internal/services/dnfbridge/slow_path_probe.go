package dnfbridge

import "time"

// slowStepProbeThreshold 是「单步耗时探针」的记录阈值。
//
// 背景：2026-09-25 的 8 人进图掉线里，慢事件告警（加 event_type 之后）能告诉我们
// "是哪条路径卡了多久"，但**不知道路径内部哪一步慢**。已知
// completeRuntimePartyDungeonLoading 每个成员实测要 3.79~5.95s，而它内部调用的
// sendCurrentFinishLoadingCharacterStateSnapshot 有 5 个候选步骤：
//
//	selectedCharacterForEnter        完整角色聚合读（每次都新建 loader，无缓存）
//	Skill.Load                       又一次仓储读
//	syncCurrentSceneSkillPointLedgers 技能点账本同步（可能含写）
//	currentFinishLoadingSnapshotPresentation 表现基线解析
//	sendGameUpperRawClass            建包 + 发包
//
// 没有分步耗时就只能靠猜，而按猜测改线上代码正是"修 A 坏 B"的来源。
// 这是继 event_type 之后第二支探针：先量准，再动手。
//
// 只在单步超过阈值时记录，正常运行不产生任何日志。
const slowStepProbeThreshold = 500 * time.Millisecond

// probeSlowStep 在单步耗时超过阈值时记录一条可直接定位的事件。
//
// 用法：
//
//	stepStarted := time.Now()
//	value, err := s.someStep(...)
//	s.probeSlowStep(session, source, "some-step", stepStarted)
func (s *Service) probeSlowStep(session *gameSession, source string, step string, started time.Time) {
	if s == nil {
		return
	}
	elapsed := time.Since(started)
	if elapsed < slowStepProbeThreshold {
		return
	}
	s.logGameEvent(session, "game-slow-step",
		"source", source,
		"step", step,
		"elapsed_ms", elapsed.Milliseconds())
}
