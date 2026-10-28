package dnfbridge

import (
	"fmt"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

const (
	runtimeRaidOwnerAnton    byte = currentRaidAntonMode
	runtimeRaidOwnerLuke     byte = currentRaidLukeMode
	runtimeRaidOwnerFiendWar byte = currentRaidFiendWarMode

	runtimeRaidLukeLightRoute byte = 2
	runtimeRaidLukeDarkRoute  byte = 6
)

type runtimeRaidOwnerConfig struct {
	Mode byte
	Raid *raid.AntonConfig
	Luke *raid.LukeConfig
	// FiendWar 只在超时空频道非 nil。它的 Battle.Raid 是合成出来的
	// *raid.AntonConfig（PhaseMax=1），所以 Raid 字段对三个 owner 都非 nil，
	// 而 FiendWar 额外带着 30 区域地图 / 规则对 / 禁锢秒数 / 商人与雷达电池。
	FiendWar *raid.FiendWarConfig
}

func runtimeRaidModeForSession(session *gameSession) byte {
	if session == nil {
		return 0
	}
	switch session.channel.Type {
	case dnfenum.RaidChannelType:
		return runtimeRaidOwnerAnton
	case dnfenum.HiddenRaidType:
		return runtimeRaidOwnerLuke
	case dnfenum.FiendWarRaidChannelType:
		return runtimeRaidOwnerFiendWar
	default:
		return 0
	}
}

// runtimeRaidOwnerSupportsSingleRaid reports whether the selected owner loaded
// a native single (单人) raid declaration.
//
// Anton and Luke both do, and both go through the SAME Anton-side parser: the
// single raid is declared by [SINGLE RAID PHASE] plus
// [single raid start minium member], which anton_pvf.go turns into
// AntonConfig.SingleMode / SingleStartMinimumMembers and advertises as the
// AntonRaidModeSingle ("single") choice. Luke reuses that shape and even asserts
// it is present -- luke_pvf.go fails the load when base.SingleMode is nil, and
// validateLukeSingleRoutes then checks its single phase against the light and
// dark routes. Luke's single type is picked per weekday by
// runtimeRaidLukeSingleTypeForTime (2=light, 6=dark).
//
// Fiend War used to reject this before loading its config because no
// [SINGLE RAID PHASE] existed. It now has an optional server-side single
// overlay. Presence of AntonConfig.SingleMode is the single authority for all
// three owners, so a team-only archive still refuses the request exactly as
// before.
func runtimeRaidOwnerSupportsSingleRaid(config runtimeRaidOwnerConfig) bool {
	return config.Raid != nil && config.Raid.SingleMode != nil
}

// runtimeRaidSingleRequested reports whether a create request or an existing
// raid object asks for the single (单人) raid, which the wire format carries as
// the tail flag.
func runtimeRaidSingleRequested(tailFlag byte) bool {
	return tailFlag == 1
}

func (s *Service) runtimeRaidLukeConfig() (*raid.LukeConfig, error) {
	if s == nil {
		return nil, raid.ErrLukePVFSourceRequired
	}
	s.raidLukeMu.Lock()
	defer s.raidLukeMu.Unlock()
	if s.raidLuke != nil {
		return s.raidLuke, nil
	}
	if s.raidLukeErr != nil {
		return nil, s.raidLukeErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.raidLukeErr = fmt.Errorf("load luke runtime archive: %w", err)
		return nil, s.raidLukeErr
	}
	config, err := raid.LoadLukeConfig(archive)
	if err != nil {
		s.raidLukeErr = fmt.Errorf("load current luke runtime config: %w", err)
		return nil, s.raidLukeErr
	}
	s.raidLuke = config
	return config, nil
}

// runtimeRaidLukeCardSetConfig loads etc/raid/luke/LukeCardSet.etc, the tarot
// table the 1181 card display is built from. Same shape as runtimeRaidLukeConfig:
// one archive read, cached, and the failure cached too so a missing or invalid
// document is logged once instead of on every waiting-area entry.
func (s *Service) runtimeRaidLukeCardSetConfig() (*raid.LukeCardSetConfig, error) {
	if s == nil {
		return nil, raid.ErrLukeCardSetSourceRequired
	}
	s.raidLukeCardSetMu.Lock()
	defer s.raidLukeCardSetMu.Unlock()
	if s.raidLukeCardSet != nil {
		return s.raidLukeCardSet, nil
	}
	if s.raidLukeCardSetErr != nil {
		return nil, s.raidLukeCardSetErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.raidLukeCardSetErr = fmt.Errorf("load luke card set runtime archive: %w", err)
		return nil, s.raidLukeCardSetErr
	}
	config, err := raid.LoadLukeCardSet(archive)
	if err != nil {
		s.raidLukeCardSetErr = fmt.Errorf("load current luke card set config: %w", err)
		return nil, s.raidLukeCardSetErr
	}
	s.raidLukeCardSet = config
	return config, nil
}

// runtimeRaidLukeRaidMonsterListConfig loads etc/raid/luke/luke.etc.buff
// [RAID MONSTER LIST], the table that says which named monster and which boss
// each maze of each raid dungeon spawns. The 1181 card display derives its seven
// faces from it, so a failure here is logged once and makes that packet fail
// closed rather than fall back to an arbitrary face.
func (s *Service) runtimeRaidLukeRaidMonsterListConfig() (*raid.LukeRaidMonsterListConfig, error) {
	if s == nil {
		return nil, raid.ErrLukeRaidMonsterListSourceRequired
	}
	s.raidLukeMonsterListMu.Lock()
	defer s.raidLukeMonsterListMu.Unlock()
	if s.raidLukeMonsterList != nil {
		return s.raidLukeMonsterList, nil
	}
	if s.raidLukeMonsterListErr != nil {
		return nil, s.raidLukeMonsterListErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.raidLukeMonsterListErr = fmt.Errorf("load luke raid monster list runtime archive: %w", err)
		return nil, s.raidLukeMonsterListErr
	}
	config, err := raid.LoadLukeRaidMonsterList(archive)
	if err != nil {
		s.raidLukeMonsterListErr = fmt.Errorf("load current luke raid monster list config: %w", err)
		return nil, s.raidLukeMonsterListErr
	}
	s.raidLukeMonsterList = config
	return config, nil
}

func (s *Service) runtimeRaidOwnerConfigForSession(session *gameSession) (runtimeRaidOwnerConfig, error) {
	if session == nil {
		return runtimeRaidOwnerConfig{}, errRuntimeRaidLifecycleState
	}
	switch session.channel.Type {
	case dnfenum.RaidChannelType:
		config, err := s.runtimeRaidAntonConfig()
		return runtimeRaidOwnerConfig{Mode: runtimeRaidOwnerAnton, Raid: config}, err
	case dnfenum.HiddenRaidType:
		config, err := s.runtimeRaidLukeConfig()
		if err != nil {
			return runtimeRaidOwnerConfig{}, err
		}
		return runtimeRaidOwnerConfig{Mode: runtimeRaidOwnerLuke, Raid: config.Raid, Luke: config}, nil
	case dnfenum.FiendWarRaidChannelType:
		config, err := s.runtimeRaidFiendWarConfig()
		if err != nil {
			return runtimeRaidOwnerConfig{}, err
		}
		if config.Battle == nil {
			return runtimeRaidOwnerConfig{}, errRuntimeRaidLifecycleState
		}
		// Battle.Raid 是 parseFiendWarBattle 合成的 *AntonConfig（PhaseMax=1），
		// 所以 Raid 字段对超时空也非 nil —— 报名/名额/入场费走与安徒恩相同的骨架。
		return runtimeRaidOwnerConfig{Mode: runtimeRaidOwnerFiendWar, Raid: config.Battle.Raid, FiendWar: config}, nil
	default:
		return runtimeRaidOwnerConfig{}, errRuntimeRaidLifecycleState
	}
}

// Current PVF exposes the two native Luke single route symbols (2=light,
// 6=dark). The historical live schedule used Tuesday for light and Saturday
// for dark; the all-days local profile follows the official first-server split.
func runtimeRaidLukeSingleTypeForTime(at time.Time) byte {
	switch at.In(runtimeRaidChinaCalendar).Weekday() {
	case time.Monday, time.Tuesday, time.Wednesday:
		return runtimeRaidLukeLightRoute
	default:
		return runtimeRaidLukeDarkRoute
	}
}

func runtimeRaidBaseConfig(state runtimeRaidState) *raid.AntonConfig {
	if state.RaidMode == runtimeRaidOwnerLuke && state.LukeConfig != nil {
		return state.LukeConfig.Raid
	}
	// 超时空的 "base" 是 parseFiendWarBattle 合成的那个 *AntonConfig。它同时是
	// lifecycle 的 config 和 runtimeRaidBaseConfig 的返回值，所以路径/阶段/名额
	// 相关的每一处都不需要知道超时空的存在。Battle 为 nil 时退回 AntonConfig，
	// 让调用方拿到 nil 而不是 panic。
	if state.RaidMode == runtimeRaidOwnerFiendWar && state.FiendWarConfig != nil && state.FiendWarConfig.Battle != nil {
		return state.FiendWarConfig.Battle.Raid
	}
	return state.AntonConfig
}
