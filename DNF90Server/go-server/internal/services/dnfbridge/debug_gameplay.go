package dnfbridge

import (
	"context"
	"time"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	// Character speed values are encoded in tenths, so +500.0% is +5000 on
	// the existing UserInfo wire fields.
	localDebugSpeedBonusWire  int64 = 5000
	localDebugLightningDamage int64 = 9_999_999_999
	localDebugMaxStat         int64 = 1<<63 - 1
)

func (s *Service) localDebugModeEnabled() bool {
	return s != nil && s.localDebugMode
}

func (s *Service) localDebugStrikeOnSkillUseEnabled() bool {
	return s != nil && s.localDebugMode && s.localDebugStrikeOnSkillUse
}

// localDebugSettlementRankPoint returns the PVF-defined highest clear-rank
// threshold only for the local debug profile. It is a server-side reward
// input: op34 must continue to echo the client request byte exactly.
func (s *Service) localDebugSettlementRankPoint(
	catalog currentDungeonClearRankCatalog,
	clientRankPoint byte,
) (rankPoint byte, forced bool) {
	if !s.localDebugModeEnabled() || len(catalog.gradeThresholds) == 0 {
		return clientRankPoint, false
	}
	return catalog.gradeThresholds[0], true
}

func (r *csharpLegacyUserInfoReader) localDebugResolvedStat(
	column string,
	value int64,
	found bool,
) (int64, bool) {
	if r == nil || !r.service.localDebugModeEnabled() {
		return value, found
	}
	switch column {
	case "stat_move_speed", "stat_attack_speed", "stat_cast_speed":
		if value > localDebugMaxStat-localDebugSpeedBonusWire {
			return localDebugMaxStat, true
		}
		return value + localDebugSpeedBonusWire, true
	default:
		return value, found
	}
}

const currentLocalDebugUseSkillRequestSize = 12

// scheduleLocalDebugLightningAfterSkillUseLocked queues a normal room clear
// after the current client's validated C2S USE_SKILL request. There is no
// separate server-visible hit-damage packet in this protocol profile, so an
// attack is eligible only while the active room still has a live hostile. The
// strike is represented by the validated per-monster death transaction rather
// than an invented client effect frame.
//
// The caller owns session.dungeon.mu.
func (s *Service) scheduleLocalDebugLightningAfterSkillUseLocked(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) bool {
	if !s.localDebugStrikeOnSkillUseEnabled() || session == nil ||
		session.selectedCharacterID == 0 || scene.Cleared ||
		!currentLocalDebugRoomHasAnnouncedHostile(runtime) {
		return false
	}
	if session.dungeon.debugLightningPending {
		return true
	}

	characterID := session.selectedCharacterID
	session.dungeon.debugLightningPending = true
	go s.runLocalDebugLightning(session, characterID, scene)
	return true
}

func currentLocalDebugRoomHasAnnouncedHostile(runtime *runtimeDungeonState) bool {
	if runtime == nil || runtime.Room == nil {
		return false
	}
	snapshot := runtime.Room.Snapshot()
	for _, monster := range snapshot.Monsters {
		if monster.State == runtimeDungeonMonsterAnnounced {
			return true
		}
	}
	for _, actor := range snapshot.ExtendedActors {
		if actor.State == runtimeDungeonMonsterAnnounced && actor.HostileReference != nil {
			return true
		}
	}
	return false
}

func (s *Service) handleLocalDebugUseSkill(session *gameSession, body []byte) (bool, error) {
	if !s.localDebugStrikeOnSkillUseEnabled() || session == nil {
		return false, nil
	}
	if len(body) != currentLocalDebugUseSkillRequestSize {
		s.logGameEvent(session, "game-local-debug-lightning-blocked",
			"body_len", len(body),
			"reason", "current_exe_use_skill_body_shape_unproven")
		return false, nil
	}

	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.Room == nil {
		return false, nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return false, nil
	}
	return s.scheduleLocalDebugLightningAfterSkillUseLocked(session, runtime, scene), nil
}

func (s *Service) runLocalDebugLightning(
	session *gameSession,
	characterID uint16,
	scene worldmap.DungeonRoomScene,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := s.DebugKillCurrentRoom(ctx, characterID)

	session.dungeon.mu.Lock()
	session.dungeon.debugLightningPending = false
	session.dungeon.mu.Unlock()

	if err != nil {
		s.logGameEvent(session, "game-local-debug-lightning-deferred",
			"character_id", characterID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"damage", localDebugLightningDamage,
			"reason", "normal_room_death_transaction_failed",
			"error", err)
		return
	}
	s.logGameEvent(session, "game-local-debug-lightning-applied",
		"character_id", characterID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"damage", localDebugLightningDamage,
		"killed_enemies", result.KilledEnemies,
		"room_cleared", result.RoomCleared,
		"flow_source", "server_side_normal_monster_death_transactions")
}
