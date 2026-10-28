package dnfbridge

import (
	"fmt"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type dungeonMazeSelection struct {
	Index   int
	Reason  string
	QuestID int64
}

// selectDungeonMaze mirrors the PVF domain rule used by df_game_r's
// GetAppropriateMaze flow: an active quest connection wins first, then a
// cleared-quest connection, then one of the non-quest mazes. The current op16
// writer does not carry a proved maze index, so the choice is owned by the
// server's real PVF and persisted quest state rather than by an opaque request
// field.
func selectDungeonMaze(
	mazes []worldmap.Maze,
	difficulty byte,
	record dnfrepo.QuestRecord,
	choose func(int) (int, error),
) (dungeonMazeSelection, error) {
	if len(mazes) == 0 {
		return dungeonMazeSelection{}, worldmap.ErrMazeNotFound
	}
	active, cleared := dungeonQuestStateSets(record)
	if selected, ok, err := selectQuestConnectedMaze(mazes, active, 0, difficulty, choose); ok || err != nil {
		selected.Reason = "active_quest_connection"
		return selected, err
	}
	if selected, ok, err := selectQuestConnectedMaze(mazes, cleared, 1, difficulty, choose); ok || err != nil {
		selected.Reason = "cleared_quest_connection"
		return selected, err
	}

	defaults := make([]int, 0, len(mazes))
	for index := range mazes {
		if len(mazes[index].QuestConnection) == 0 {
			defaults = append(defaults, index)
		}
	}
	if len(defaults) == 0 {
		return dungeonMazeSelection{Index: 0, Reason: "pvf_first_maze_no_default"}, nil
	}
	index, err := chooseDungeonWeightedMaze(mazes, defaults, choose)
	if err != nil {
		return dungeonMazeSelection{}, fmt.Errorf("choose non-quest maze: %w", err)
	}
	return dungeonMazeSelection{Index: index, Reason: "pvf_non_quest_maze"}, nil
}

// chooseDungeonWeightedMaze follows a dungeon's optional PVF [maze chance
// rate] values.  Time Rift declares eight normal routes at 11875 each and two
// special routes at 2500 each, so treating the ten mazes as uniform makes an
// unsupported special route four times too likely.  Legacy dungeons without
// any rate keep the original candidate-count chooser shape.
func chooseDungeonWeightedMaze(
	mazes []worldmap.Maze,
	candidates []int,
	choose func(int) (int, error),
) (int, error) {
	weighted := false
	for _, index := range candidates {
		if index >= 0 && index < len(mazes) && mazes[index].ChanceRate.Set {
			weighted = true
			break
		}
	}
	if !weighted {
		return chooseDungeonCandidate(candidates, choose)
	}

	total := int64(0)
	weights := make([]int64, len(candidates))
	for position, index := range candidates {
		if index < 0 || index >= len(mazes) {
			return 0, fmt.Errorf("maze candidate index %d outside [0,%d)", index, len(mazes))
		}
		weight := int64(1)
		if mazes[index].ChanceRate.Set {
			weight = mazes[index].ChanceRate.Value
		}
		if weight <= 0 || weight > int64(^uint(0)>>1)-total {
			return 0, fmt.Errorf("invalid maze chance rate=%d maze=%d", weight, index)
		}
		weights[position] = weight
		total += weight
	}
	if total <= 0 || total > int64(^uint(0)>>1) {
		return 0, fmt.Errorf("invalid total maze chance rate=%d", total)
	}
	if choose == nil {
		return 0, fmt.Errorf("%w: weighted_candidates=%d total_weight=%d", errDungeonChoiceRequired, len(candidates), total)
	}
	roll, err := choose(int(total))
	if err != nil {
		return 0, err
	}
	if roll < 0 || int64(roll) >= total {
		return 0, fmt.Errorf("dungeon weighted maze roll %d outside [0,%d)", roll, total)
	}
	remaining := int64(roll)
	for position, weight := range weights {
		if remaining < weight {
			return candidates[position], nil
		}
		remaining -= weight
	}
	return 0, fmt.Errorf("dungeon weighted maze roll %d did not resolve", roll)
}

func selectQuestConnectedMaze(
	mazes []worldmap.Maze,
	questIDs map[int64]struct{},
	requiredType int64,
	difficulty byte,
	choose func(int) (int, error),
) (dungeonMazeSelection, bool, error) {
	if len(questIDs) == 0 {
		return dungeonMazeSelection{}, false, nil
	}
	candidates := make([]int, 0, 1)
	for index := range mazes {
		connection := mazes[index].QuestConnection
		if len(connection) < 2 || connection[0] != requiredType {
			continue
		}
		questID := connection[1]
		if _, exists := questIDs[questID]; !exists {
			continue
		}
		if requiredType == 0 && len(connection) >= 3 && connection[2] >= 0 && int64(difficulty) < connection[2] {
			continue
		}
		candidates = append(candidates, index)
	}
	if len(candidates) == 0 {
		return dungeonMazeSelection{}, false, nil
	}
	index, err := chooseDungeonCandidate(candidates, choose)
	if err != nil {
		return dungeonMazeSelection{}, true, fmt.Errorf("choose quest-connected maze: %w", err)
	}
	return dungeonMazeSelection{Index: index, QuestID: mazes[index].QuestConnection[1]}, true, nil
}

func chooseDungeonCandidate(candidates []int, choose func(int) (int, error)) (int, error) {
	if len(candidates) == 0 {
		return 0, worldmap.ErrMazeNotFound
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if choose == nil {
		return 0, fmt.Errorf("%w: candidates=%d", errDungeonChoiceRequired, len(candidates))
	}
	position, err := choose(len(candidates))
	if err != nil {
		return 0, err
	}
	if position < 0 || position >= len(candidates) {
		return 0, fmt.Errorf("dungeon candidate position %d outside [0,%d)", position, len(candidates))
	}
	return candidates[position], nil
}

func dungeonQuestStateSets(record dnfrepo.QuestRecord) (active map[int64]struct{}, cleared map[int64]struct{}) {
	active = make(map[int64]struct{})
	cleared = make(map[int64]struct{})
	collect := func(states map[int64]dnfrepo.QuestState) {
		for questID, state := range states {
			if questID <= 0 {
				continue
			}
			switch normalizeDungeonQuestStatus(state.Status) {
			case "active", "accepted", "inprogress", "progress":
				active[questID] = struct{}{}
			case "complete", "completed", "cleared", "finished", "done":
				cleared[questID] = struct{}{}
			}
		}
	}
	collect(record.States)
	collect(record.Progress)
	return active, cleared
}

func normalizeDungeonQuestStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	return value
}
