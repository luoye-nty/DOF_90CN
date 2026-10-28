package dnfbridge

// 攻坚阶段死亡计数 —— 0x0259 (601 RAID_RESULT) 的 deaths 字段。
//
// 为什么要有这个文件
//
//	客户端的阶段结算条把 0x0259 的第 4 个字段画成「死亡次数」（`raid.RaidSettlementResult`
//	的 `DeathCount`，u16，`raid/result_flow_protocol.go:44` + `:117`），而
//	`publishRuntimeRaidPhaseSettlement` 一直只填 ResultType/PhaseIndex/ClearSeconds
//	—— 这个 u16 恒为 0。服主 2026-09-29 实测单人卢克：「我确定死亡了并且用复活币了的」
//	但结算条写着「死亡次数 0」。
//
//	luke.etc 的 `[CONDITION FOR RANK]` 正是按 `[DEAD COUNT]` 分档
//	（`{0,1}→0 / {2,5}→1 / {6,9}→2 / {10,14}→3`，`luke_pvf_test.go:136-141`），
//	86JP 也是 `RAID_RESULT(0, phase, clearTime, deaths, rank+2, rewardOption)`
//	（`86raid_flow_report.md:100`）—— 所以这个字段本来就是"本阶段全团死亡次数"。
//
// 口径（本实现的约定，写明以免后人猜）
//
//	计数边沿 = **服务端接受的 C2S op40 `DIE_CHARACTER`**
//	（`handleDungeonDieCharacterLocal` 通过全部守卫、真正发出 op32 的那一步）。
//	一次死亡一条，与"是否用复活币站起来"无关：两种死亡都该被统计。
//	粒度 = (RaidKey, Phase)，即本阶段整团；阶段结束时随攻坚一起清零。
//
// 适用范围：**所有团本**（卢克 + 安徒恩，团队与单人阶段都是）。
//
//	2026-09-29 服主拍板：这个值本来就是该有值的字段，恒 0 是缺陷。依据是
//	86JP 的 `RAID_RESULT(0, phase, clearTime, deaths, rank+2, rewardOption)`
//	（`86raid_flow_report.md:100`）——第 4 个参数就是死亡次数；而 luke.etc 的
//	`[CONDITION FOR RANK]` 也正是按 `[DEAD COUNT]` 分档
//	（`{0,1}→0 / {2,5}→1 / {6,9}→2 / {10,14}→3`，`luke_pvf_test.go:136-141`），
//	两者都说明这个 u16 是"本阶段全团死亡次数"，与阵营无关。
//
//	在 2026-09-29 之前这里有一道 `runtimeRaidSettlementCarriesDeathCount` 的
//	owner 判断，只有卢克写这个字段（当时安徒恩 0x0259 要求逐字节不变）。服主
//	拍板后该判断已删除：**安徒恩本次唯一有意的行为变化就是这个 u16**，其它字节
//	（header / ClearSeconds / Score / Flags / ResultType / PhaseIndex）逐字节不变，
//	由 TestAntonPhaseSettlementCarriesThePhasesDeathCount 逐字节对照钉住。

// runtimeRaidPhaseDeathKey names one raid phase's death counter.
type runtimeRaidPhaseDeathKey struct {
	RaidKey uint32
	Phase   byte
}

// recordRuntimeRaidPhaseDeath counts one accepted character death inside a raid
// gate for its phase.
//
// The caller (handleDungeonDieCharacterLocal) holds session.dungeon.mu and this
// takes s.raidMu, which is the order the raid lifecycle already uses -- the
// inverse is what would deadlock (same rule as recordRuntimeRaidReviveCoinUseLocked).
func (s *Service) recordRuntimeRaidPhaseDeath(session *gameSession, runtime *runtimeDungeonState) {
	if s == nil || session == nil || runtime == nil {
		return
	}
	binding := runtime.raidDungeon
	if binding.RaidKey == 0 || binding.MapID == 0 {
		return
	}
	key := runtimeRaidPhaseDeathKey{RaidKey: binding.RaidKey, Phase: binding.Phase}
	s.raidMu.Lock()
	state := s.raids[binding.RaidKey]
	if state == nil || state.Lifecycle.Phase != binding.Phase {
		// The gate belongs to a phase that is no longer running: a death that
		// arrives after the settlement is not part of any result body.
		s.raidMu.Unlock()
		return
	}
	if s.raidPhaseDeaths == nil {
		s.raidPhaseDeaths = make(map[runtimeRaidPhaseDeathKey]uint32, 4)
	}
	s.raidPhaseDeaths[key]++
	total := s.raidPhaseDeaths[key]
	s.raidMu.Unlock()
	s.logGameEvent(session, "game-raid-phase-death-counted",
		"char_id", session.selectedCharacterID,
		"raid_key", binding.RaidKey,
		"raid_phase", binding.Phase,
		"raid_map_id", binding.MapID,
		"phase_deaths", total)
}

// runtimeRaidPhaseDeathCount is the u16 the 0x0259 result body carries for this
// raid and phase.
func (s *Service) runtimeRaidPhaseDeathCount(raidKey uint32, phase byte) uint16 {
	if s == nil || raidKey == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	count := s.raidPhaseDeaths[runtimeRaidPhaseDeathKey{RaidKey: raidKey, Phase: phase}]
	if count > 0xffff {
		return 0xffff
	}
	return uint16(count)
}

// purgeRuntimeRaidPhaseDeathsLocked drops one raid's phase death counters.
// Caller holds s.raidMu.
func (s *Service) purgeRuntimeRaidPhaseDeathsLocked(raidKey uint32) {
	if s == nil || raidKey == 0 {
		return
	}
	for key := range s.raidPhaseDeaths {
		if key.RaidKey == raidKey {
			delete(s.raidPhaseDeaths, key)
		}
	}
}
