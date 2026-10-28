package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const currentDungeonRoomFatigueCost int64 = 1

// currentDungeonTrainingRoomID is 修炼道场, the free training room. Live evidence
// 2026-09-12 13:25:33 (packet_log.txt): char 1 with fatigue=0 requested dungeon
// 5000 and the server answered
//
//	game-dungeon-select-blocked dungeon_id=5000 reason=dungeon_owner_validation_failed
//	  error=dnf dungeon character fatigue is exhausted: fatigue=0
//	game-dungeon-entry-fatigue-prompt-sent reason=255
//
// which is the client's 「疲劳值不足」 prompt. This data set's 5000 entry carries no
// "no fatigue" / "enter without fatigue" metadata, but the training room is free to
// enter and free to run, so the id is exempted explicitly the same way 11006/11007
// are special-cased for their solo party limit.
const currentDungeonTrainingRoomID int64 = 5000

// currentDungeonIsTrainingRoom reports 修炼道场, the free sandbox room.
func currentDungeonIsTrainingRoom(dungeon worldmap.Dungeon) bool {
	return dungeon.ID == currentDungeonTrainingRoomID
}

// currentDungeonIgnoresFatigue reports whether a dungeon must neither require nor
// consume fatigue: the training room, plus whatever the PVF data marks as free.
func currentDungeonIgnoresFatigue(dungeon worldmap.Dungeon) bool {
	if currentDungeonIsTrainingRoom(dungeon) {
		return true
	}
	return dungeon.Metadata.NoFatigue || dungeon.Metadata.Flags["enter without fatigue"]
}

var errCurrentDungeonFatigueCommitDeferred = errors.New("dungeon fatigue commit deferred")

func currentDungeonRuntimeConsumesFatigue(runtime *runtimeDungeonState) bool {
	if runtime == nil || currentDungeonRuntimeIsPractice(runtime) ||
		currentDungeonIgnoresFatigue(runtime.Dungeon) {
		return false
	}
	_, known := dnfdungeon.CharacterStat(runtime.Character.Stats, "fatigue", "fp", "疲劳")
	return known
}

// consumeCurrentDungeonRoomFatigue runs after op29 and the matching room
// commit. Callers already own the dungeon transition lock, so this function
// must not reacquire session.dungeon.mu.
func (s *Service) consumeCurrentDungeonRoomFatigue(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if s == nil || session == nil || runtime == nil || runtime.Session == nil || runtime.Room == nil {
		return errDungeonWorldMapUnavailable
	}
	// 签到弹珠的 29..32 号增益是"下一次通关消耗一次"的语义：必须在疲劳早退之前
	// 激活，否则练习模式/免疲劳副本里的通关不会消耗增益（也就永远发不出效果）。
	if err := s.activateAttendanceMarbleDungeonBuffs(session, runtime); err != nil {
		return err
	}
	if !currentDungeonRuntimeConsumesFatigue(runtime) {
		return nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return errDungeonWorldMapUnavailable
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	if err != nil {
		return err
	}
	if visit.FatigueConsumed {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return fmt.Errorf("%w: %v", errCurrentDungeonFatigueCommitDeferred, dnffatigue.ErrOwnerUnavailable)
	}
	owner, err := dnffatigue.NewOwner(repositories)
	if err != nil {
		return fmt.Errorf("%w: %v", errCurrentDungeonFatigueCommitDeferred, err)
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := owner.ConsumeRoom(ctx, dnffatigue.ConsumeRoomCommand{
		AccountID:   s.accountIDForSession(session),
		CharacterID: characterID,
		Amount:      currentDungeonRoomFatigueCost,
		UpdatedAt:   s.gameplayNow(),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errCurrentDungeonFatigueCommitDeferred, err)
	}

	runtime.Character = result.Character
	visit.FatigueConsumed = true
	visit.FatigueConsumedAmount = result.Consumed
	visit.FatigueBatteryConsumed = result.BatteryConsumed > 0
	s.logGameEvent(session, "game-dungeon-fatigue-consumed",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"maze_index", runtime.MazeIndex,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"before", result.Before,
		"after", result.After,
		"consumed", result.Consumed,
		"battery_before", result.BatteryBefore,
		"battery_after", result.BatteryAfter,
		"battery_consumed", result.BatteryConsumed,
		"source", source)
	state := currentCharacterFatigueState(runtime.Character, true)
	// The last paid room keeps its blessing even though no future charges remain.
	state.batteryBuff = dnffatigue.BatteryBuffCount(result.BatteryAfter, runtime.Character.Level, visit.FatigueBatteryConsumed)
	return s.sendGameUpperRawClass(session, currentFatigueMsgID, buildCurrentFatigueStateBody(state), 0)
}

// currentDungeonRoomUsesFatigueBattery reports whether the room's fatigue
// point was paid together with one fatigue-battery charge, which grants the
// 30% ancient-spirit monster EXP bonus for kills inside that room.
func currentDungeonRoomUsesFatigueBattery(runtime *runtimeDungeonState, scene worldmap.DungeonRoomScene) bool {
	if runtime == nil || runtime.Character.Level >= 90 {
		return false
	}
	visit, err := runtime.currentDungeonRoomVisit(scene)
	return err == nil && visit != nil && visit.FatigueBatteryConsumed
}

// consumeCurrentDungeonRoomFatigueAfterCommit distinguishes durable failures
// from socket failures after op29 has already committed the room. A durable
// failure is logged and deferred instead of aborting an irreversible scene
// transition; a transport failure still stops the packet stream.
func (s *Service) consumeCurrentDungeonRoomFatigueAfterCommit(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	err := s.consumeCurrentDungeonRoomFatigue(session, runtime, source)
	if err == nil || !errors.Is(err, errCurrentDungeonFatigueCommitDeferred) {
		return err
	}
	s.logGameEvent(session, "game-dungeon-fatigue-consume-deferred",
		"char_id", session.selectedCharacterID,
		"dungeon_id", runtime.Dungeon.ID,
		"source", source,
		"error", err)
	return nil
}

// currentDungeonRetryGatesOnFatigue reports whether THIS run's remaining fatigue
// decides whether the settlement's 再次挑战 stays available.
//
// Runs that never charge fatigue must not gate on it. Three of them are not covered
// by currentDungeonRuntimeConsumesFatigue and are excluded here explicitly:
//
//   - an 安徒恩 raid gate: the squad's run is owned by the raid binding, not by a
//     per-character fatigue budget (runtime.raidDungeon is only set on raid runs);
//   - 转职体验「领悟时间」 (7142), which the client enters straight from the 体验
//     button and which is already on the weakness-exempt whitelist for the same
//     "this run is free" reason (see weakness_state.go);
//
// while the rest are already covered there: 修炼道场 (5000) and any PVF dungeon
// flagged free go through currentDungeonIgnoresFatigue, and practice runs through
// currentDungeonRuntimeIsPractice.
func currentDungeonRetryGatesOnFatigue(runtime *runtimeDungeonState) bool {
	if runtime == nil {
		return false
	}
	if runtime.raidDungeon.RaidKey != 0 {
		return false
	}
	if runtime.Dungeon.ID == int64(currentJobChangeTrialDungeonID) {
		return false
	}
	return currentDungeonRuntimeConsumesFatigue(runtime)
}

// currentDungeonFatigueStatExhausted reads the REMAINING fatigue out of a character
// record; 0 means the character cannot enter or retry.
//
// No repository read is needed: every charged room writes the post-charge record
// back onto the connection's runtime (consumeCurrentDungeonRoomFatigue sets
// runtime.Character = result.Character), and a room charges on entry, so by the
// time the boss room is cleared this is already the final value.
func currentDungeonFatigueStatExhausted(stats map[string]int64) bool {
	fatigue, known := dnfdungeon.CharacterStat(stats, "fatigue", "fp", "疲劳")
	return known && fatigue <= 0
}

// currentDungeonSessionFatigueExhausted reads one session's live run fatigue.
// It takes that session's own lock and is therefore only called while no other
// session lock is held.
func currentDungeonSessionFatigueExhausted(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	exhausted := runtime != nil && currentDungeonFatigueStatExhausted(runtime.Character.Stats)
	session.dungeon.mu.Unlock()
	return exhausted
}

// currentPartyFatigueExhaustedSlots snapshots which party slots have run out of
// fatigue, for the card-layout packet that decides whether 再次挑战 is available.
//
// MUST be called WITHOUT holding any session.dungeon.mu. It borrows the leader's
// lock only to read the roster and the leader's own record and releases it before
// touching a member's -- this codebase only ever holds one session lock at a time
// (see party_dungeon_entry_presence.go and town_enter_select.go), so nesting the
// leader's lock around a member's would introduce a new lock order.
func (s *Service) currentPartyFatigueExhaustedSlots(leader *gameSession) [dungeonCardSlotsPerSide]bool {
	var slots [dungeonCardSlotsPerSide]bool
	if s == nil || leader == nil || leader.selectedCharacterID == 0 {
		return slots
	}
	leader.dungeon.mu.Lock()
	runtime := leader.dungeon.runtime
	gates := currentDungeonRetryGatesOnFatigue(runtime)
	leaderExhausted := runtime != nil && currentDungeonFatigueStatExhausted(runtime.Character.Stats)
	var roster []runtimePartyDungeonMember
	if runtime != nil && runtime.partyDungeonOwner != nil {
		roster = runtime.partyDungeonOwner.roster()
	}
	leader.dungeon.mu.Unlock()
	if !gates {
		return slots
	}
	// A solo run is a party of one: its own slot is present, the rest stay absent.
	if len(roster) == 0 {
		slots[0] = leaderExhausted
		return slots
	}
	for _, member := range roster {
		if member.slot >= dungeonCardSlotsPerSide {
			continue
		}
		if member.userID == leader.selectedCharacterID {
			slots[member.slot] = leaderExhausted
			continue
		}
		memberSession, ok := s.onlineGameSession(member.userID)
		if !ok || memberSession == nil {
			// A member with no live session cannot report its fatigue; leave the
			// slot at 1 so a disconnect never disables 再次挑战 for the others.
			continue
		}
		slots[member.slot] = currentDungeonSessionFatigueExhausted(memberSession)
	}
	return slots
}
