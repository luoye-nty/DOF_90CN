package dnfbridge

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

// currentDungeonProtocolMode is shared by C2S op16 offset 8 and S2C op28
// offset 12. It is independent from the selected maze route.
type currentDungeonProtocolMode byte

const (
	currentDungeonProtocolModeNormal   currentDungeonProtocolMode = 0
	currentDungeonProtocolModePractice currentDungeonProtocolMode = 1
)

// currentDungeonProtocolModeForWire maps the C2S selector's raw state marker
// **without the room**. Three shapes are proven by 90CN live traffic
// (runtime/logs/packet_log.txt, 2026-09-15..09-17, 481 select requests):
//
//	state=0 token=UINT16_MAX  ordinary 进入 into the selector's dungeon
//	                          (155x dungeon 210, plus 213/215/218/220/7115/…)
//	state=1 token=0           the isolated practice policy
//	state=1 token=UINT16_MAX  练习模式 on an ordinary dungeon, and the two
//	                          guided rooms' own entry buttons (修炼场 5000,
//	                          转职体验「领悟时间」7142)
//
// This supersedes the older reading that state=1/UINT16_MAX was itself "the
// current client's ordinary selection": 480 of the 481 captured ordinary
// entries carry state=0, and the only state=1/UINT16_MAX requests outside the
// guided rooms are the operator's 练习模式 attempts (dungeon 190, 2026-09-17
// 16:08:52, blocked by the fatigue policy before this mapping was room-aware).
//
// The pair alone therefore cannot decide the last shape, so this function keeps
// the conservative ordinary reading for it and callers that know the room must
// use currentDungeonProtocolModeForRoom. It stays the validity check for call
// sites that only need "is this pair one the client can send" (see
// validateCurrentDungeonEntryRequest). Every supported pair projects its mode
// back through op28.
func currentDungeonProtocolModeForWire(runtimeState byte, runtimeToken uint16) (currentDungeonProtocolMode, bool) {
	switch runtimeState {
	case byte(currentDungeonProtocolModeNormal):
		switch runtimeToken {
		case 0, 0xffff:
			return currentDungeonProtocolModeNormal, true
		}
	case byte(currentDungeonProtocolModePractice):
		switch runtimeToken {
		case 0:
			return currentDungeonProtocolModePractice, true
		case 0xffff:
			return currentDungeonProtocolModeNormal, true
		}
	}
	return currentDungeonProtocolModeNormal, false
}

func (mode currentDungeonProtocolMode) valid() bool {
	return mode == currentDungeonProtocolModeNormal || mode == currentDungeonProtocolModePractice
}

// currentDungeonProtocolModeForRoom resolves the raw C2S op16 pair into the
// runtime policy with the requested room taken into account. It is the entry
// point for every path that commits a run (prepareDungeonRuntimePlanned, the
// 再次挑战 preflight).
//
// The added rule is the 练习模式 marker: state=1 + token=UINT16_MAX means "run
// this dungeon in practice" for an ordinary room, which is how the client lets a
// character enter with fatigue=0 and without paying entry items. Operator report
// 2026-09-17 (fatigue was deliberately zeroed to test it): 练习模式 on 倒悬的瞭望台
// (190) was answered with the 「疲劳值不足」 prompt
//
//	game-dungeon-select-blocked dungeon_id=190 reason=dungeon_owner_validation_failed
//	  error=dnf dungeon character fatigue is exhausted: fatigue=0
//	game-dungeon-entry-fatigue-prompt-sent reason=255
//
// because the pair was read as an ordinary entry. The two guided rooms keep the
// ordinary policy even though their buttons send the same pair - see
// currentDungeonGuidedDirectEntryRoom.
func currentDungeonProtocolModeForRoom(
	runtimeState byte,
	runtimeToken uint16,
	dungeonID int64,
) (currentDungeonProtocolMode, bool) {
	mode, supported := currentDungeonProtocolModeForWire(runtimeState, runtimeToken)
	if !supported {
		return mode, false
	}
	if runtimeState != byte(currentDungeonProtocolModePractice) || runtimeToken != 0xffff {
		return mode, true
	}
	if currentDungeonGuidedDirectEntryRoom(dungeonID) {
		return currentDungeonProtocolModeNormal, true
	}
	return currentDungeonProtocolModePractice, true
}

func (mode currentDungeonProtocolMode) String() string {
	switch mode {
	case currentDungeonProtocolModeNormal:
		return "normal"
	case currentDungeonProtocolModePractice:
		return "practice"
	default:
		return fmt.Sprintf("unknown-%d", byte(mode))
	}
}

func currentDungeonRuntimeIsPractice(runtime *runtimeDungeonState) bool {
	return runtime != nil && runtime.ProtocolMode == currentDungeonProtocolModePractice
}

func currentDungeonPracticeProhibited(dungeon worldmap.Dungeon) bool {
	return dungeon.Metadata.IntegerValues != nil && dungeon.Metadata.IntegerValues["prohibit practice"] != 0
}

func selectPracticeDungeonMaze(
	mazes []worldmap.Maze,
	choose func(int) (int, error),
) (dungeonMazeSelection, error) {
	candidates := make([]int, 0, len(mazes))
	for index := range mazes {
		if len(mazes[index].QuestConnection) == 0 {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return dungeonMazeSelection{}, fmt.Errorf("practice mode requires a non-quest maze: %w", worldmap.ErrMazeNotFound)
	}
	index, err := chooseDungeonCandidate(candidates, choose)
	if err != nil {
		return dungeonMazeSelection{}, err
	}
	return dungeonMazeSelection{Index: index, Reason: "practice_pvf_non_quest_maze"}, nil
}

func validatePracticeDungeonRoute(dungeon worldmap.Dungeon) error {
	if dungeon.ID == 11006 || dungeon.ID == 11007 || dungeon.BloodAltarKind != 0 {
		return fmt.Errorf("%w: blood/ultimate altar does not support reward-free practice ownership", errDungeonEntryModeUnsupported)
	}
	if currentDungeonPracticeProhibited(dungeon) {
		return fmt.Errorf("practice mode prohibited by PVF dungeon metadata")
	}
	if dungeon.Metadata.TutorialDungeon.Set && dungeon.Metadata.TutorialDungeon.Value == 1 {
		return fmt.Errorf("practice mode does not admit tutorial dungeons")
	}
	if dungeon.Metadata.Flags != nil && dungeon.Metadata.Flags["tournament dungeon"] {
		return fmt.Errorf("practice mode does not admit tournament dungeons")
	}
	return nil
}
