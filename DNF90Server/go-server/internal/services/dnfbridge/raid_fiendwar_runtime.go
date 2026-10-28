package dnfbridge

import "longheng.io/server/internal/modules/dnf/raid"

func (s *Service) runtimeRaidFiendWarConfig() (*raid.FiendWarConfig, error) {
	s.raidFiendWarMu.Lock()
	defer s.raidFiendWarMu.Unlock()
	if s.raidFiendWar != nil || s.raidFiendWarErr != nil {
		return s.raidFiendWar, s.raidFiendWarErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.raidFiendWarErr = err
		return nil, err
	}
	s.raidFiendWar, s.raidFiendWarErr = raid.LoadFiendWarConfig(archive)
	return s.raidFiendWar, s.raidFiendWarErr
}

func (s *Service) runtimeFiendWarSingleMonsterLevel(session *gameSession) uint8 {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return 0
	}
	state, ok := s.runtimeRaidForMember(session.selectedCharacterID)
	if !ok || state.RaidMode != runtimeRaidOwnerFiendWar || state.TailFlag != 1 ||
		state.Lifecycle.FiendWar == nil || state.Lifecycle.FiendWar.Single == nil ||
		state.FiendWarConfig == nil || state.FiendWarConfig.Single == nil {
		return 0
	}
	return state.FiendWarConfig.Single.MonsterLevel
}

func runtimeRaidMemberLimit(state runtimeRaidState) int {
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		if state.FiendWarConfig == nil {
			return 0
		}
		return state.FiendWarConfig.MemberMax
	}
	return raid.MaxAttackPartyMembers
}

// currentFiendWarSquadCount 是超时空的小队编制数。
//
// 操作员 2026-10-02 的定案：
//
//	「超时空总共有8个编制 跟其他攻坚战不一样的是 每个小队编制仅可放1人
//	  最有力的证明是 小队编制界面 每个小队后面三格都是锁住的」
//
// 副本侧也对得上：罗什那两张地图的 `[player number]` 是 `2 8`（最少 2 人、最多 8 人）。
const currentFiendWarSquadCount = 8

// runtimeRaidGroupMaxFor 返回"这个团有几个小队编制"（客户端小队编制界面画几行）。
//
// 超时空是 **8**；安东/卢克沿用既有的 runtimeRaidMaxGroups（10，40 人格子）。
func runtimeRaidGroupMaxFor(state runtimeRaidState) byte {
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return currentFiendWarSquadCount
	}
	return runtimeRaidMaxGroups
}

// runtimeRaidGroupSizeFor 返回"一个小队编制能站几个人"。
//
// 超时空是 **1** —— 客户端的小队编制界面每队后面三格是锁住的，团长把 B 拖进
// 已经有 A 的队时，**A 被挤成单人（未编队）、B 占这个位置**，而不是整脚拒绝；
// 见 moveRuntimeRaidMember 里那条挤人分支。其余团仍然是 4 人一队。
//
// 这条尺子同时管着"普通队伍 → 小队编号"的自动同步
// (runtimeRaidSquadForOrdinaryPartyLedBy)：超时空里两个人在镇上组了队，
// 他们会被分到**两个不同的编制**，而不是挤进同一队。
func runtimeRaidGroupSizeFor(state runtimeRaidState) int {
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return 1
	}
	return runtimeRaidGroupSize
}

// runtimeRaidGroupLimit 是这个团的小队编制数上限（按人数上限除以每队容量算）。
// 超时空每队 1 人 ⇒ 就是 8。
func runtimeRaidGroupLimit(state runtimeRaidState) byte {
	size := runtimeRaidGroupSizeFor(state)
	if size <= 0 {
		return 0
	}
	return byte((runtimeRaidMemberLimit(state) + size - 1) / size)
}
