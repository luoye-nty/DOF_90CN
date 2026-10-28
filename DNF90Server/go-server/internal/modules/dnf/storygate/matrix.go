// Package storygate provides a read-only, PVF-backed regression matrix for
// story quests. It deliberately lives outside dnfbridge: building a matrix
// must never mutate quest state, dungeon runs, or wire protocol behavior.
package storygate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const MatrixVersion = 1

var (
	ErrQuestCatalogRequired = errors.New("story-gate quest catalog is required")
	ErrWorldMapRequired     = errors.New("story-gate worldmap table is required")
	ErrStartQuestMissing    = errors.New("story-gate start quest is missing")
)

type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Detail   string `json:"detail"`
}

type Options struct {
	// MaxEntries prevents a malformed prerequisite graph from producing an
	// unbounded report. Zero uses the current story-catalog default of 2048 nodes.
	MaxEntries int
}

type Character struct {
	Job      int `json:"job"`
	GrowType int `json:"grow_type"`
	Level    int `json:"level"`
}

type Objective struct {
	Family       string   `json:"family"`
	Type         string   `json:"type"`
	SubType      int      `json:"sub_type"`
	Owner        string   `json:"owner"`
	IntData      []int64  `json:"int_data,omitempty"`
	CheckCount   []int64  `json:"check_count,omitempty"`
	Targets      []Target `json:"targets,omitempty"`
	Prerequisite []int64  `json:"prerequisite,omitempty"`
}

type Target struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	Path     string `json:"path,omitempty"`
	Dungeon  int64  `json:"dungeon,omitempty"`
	Resolved bool   `json:"resolved"`
}

type Maze struct {
	Index         int      `json:"index"`
	Rooms         int      `json:"rooms"`
	Start         *Point   `json:"start,omitempty"`
	Boss          *Point   `json:"boss,omitempty"`
	LayeredStages int      `json:"layered_stages"`
	Layered       bool     `json:"layered"`
	QuestConnect  []int64  `json:"quest_connection,omitempty"`
	Diagnostics   []string `json:"diagnostics,omitempty"`
}

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Dungeon struct {
	ID          int64    `json:"id"`
	Path        string   `json:"path"`
	AreaID      int64    `json:"area_id,omitempty"`
	AreaPath    string   `json:"area_path,omitempty"`
	Mazes       []Maze   `json:"mazes,omitempty"`
	Diagnostics []string `json:"diagnostics,omitempty"`
}

type Gate struct {
	TownID     int64   `json:"town_id"`
	AreaID     int64   `json:"area_id"`
	MapPath    string  `json:"map_path"`
	Kind       string  `json:"kind"`
	DungeonID  int64   `json:"dungeon_id,omitempty"`
	MinLevel   int64   `json:"min_level,omitempty"`
	NeedQuests []int64 `json:"need_quests,omitempty"`
	HasGate    bool    `json:"has_gate"`
	ExitNPCIDs []int64 `json:"exit_npc_ids,omitempty"`
}

type StateCheck struct {
	State      string  `json:"state"`
	Requires   []int64 `json:"requires,omitempty"`
	Transition string  `json:"transition"`
}

type Entry struct {
	Sequence     int          `json:"sequence"`
	QuestID      int64        `json:"quest_id"`
	QuestPath    string       `json:"quest_path"`
	QuestType    string       `json:"quest_type"`
	MainQuestID  int64        `json:"main_quest_id,omitempty"`
	Objective    Objective    `json:"objective"`
	States       []StateCheck `json:"states"`
	Successors   []int64      `json:"successors,omitempty"`
	Dungeons     []Dungeon    `json:"dungeons,omitempty"`
	Gates        []Gate       `json:"gates,omitempty"`
	ReturnRoutes []Gate       `json:"return_routes,omitempty"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
	Fingerprint  string       `json:"fingerprint"`
}

type Report struct {
	Version       int       `json:"version"`
	StartQuestID  int64     `json:"start_quest_id"`
	Character     Character `json:"character"`
	Entries       []Entry   `json:"entries"`
	QuestCount    int       `json:"quest_count"`
	DungeonCount  int       `json:"dungeon_count"`
	GateCount     int       `json:"gate_count"`
	ErrorCount    int       `json:"error_count"`
	WarningCount  int       `json:"warning_count"`
	InventoryHash string    `json:"inventory_hash"`
	Ready         bool      `json:"ready_for_manual_regression"`
}

// Build walks the PVF prerequisite graph from startQuestID and joins every
// reachable objective with dungeon, town gate, and return-route evidence.
// The graph is filtered using the same job/grow predicate as quest acceptance.
func Build(ctx context.Context, quests *quest.Catalog, maps *worldmap.Table, towns *town.Table, startQuestID int64, character Character, options Options) (Report, error) {
	if quests == nil {
		return Report{}, ErrQuestCatalogRequired
	}
	if maps == nil {
		return Report{}, ErrWorldMapRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	start, ok := quests.Find(startQuestID)
	if !ok {
		return Report{}, fmt.Errorf("%w: %d", ErrStartQuestMissing, startQuestID)
	}
	limit := options.MaxEntries
	if limit <= 0 {
		limit = 2048
	}
	report := Report{Version: MatrixVersion, StartQuestID: startQuestID, Character: character}
	queue := []int64{start.ID}
	visited := make(map[int64]struct{})
	resolver, resolverErr := worldmap.NewResolver(maps)
	for len(queue) > 0 && len(report.Entries) < limit {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		questID := queue[0]
		queue = queue[1:]
		if _, seen := visited[questID]; seen {
			continue
		}
		visited[questID] = struct{}{}
		definition, found := quests.Find(questID)
		if !found {
			continue
		}
		entry := Entry{
			Sequence: len(report.Entries), QuestID: definition.ID, QuestPath: definition.Path,
			QuestType: normalize(definition.Type), MainQuestID: definition.MainQuestID,
			Objective: buildObjective(definition, maps),
		}
		entry.States = stateChecks(entry.Objective)
		for _, successor := range quests.Successors(definition.ID) {
			if !quests.MatchesCharacter(successor.ID, quest.CharacterEligibility{Job: character.Job, GrowType: character.GrowType, Level: character.Level}) {
				continue
			}
			entry.Successors = append(entry.Successors, successor.ID)
			queue = append(queue, successor.ID)
		}
		sort.Slice(entry.Successors, func(i, j int) bool { return entry.Successors[i] < entry.Successors[j] })
		entry.Dungeons, entry.Gates, entry.ReturnRoutes = joinTopology(definition, maps, towns, resolver, &entry.Diagnostics)
		if dungeonObjective(entry.Objective.Type) && len(entry.Objective.Targets) == 0 {
			entry.Diagnostics = append(entry.Diagnostics, Diagnostic{Severity: "error", Code: "clear_target_unresolved", Detail: fmt.Sprintf("quest %d declares %q but no target map or dungeon was found in the loaded PVF", definition.ID, entry.Objective.Type)})
		}
		entry.Fingerprint = fingerprint(entry)
		report.Entries = append(report.Entries, entry)
	}
	if len(queue) > 0 {
		report.WarningCount++
	}
	if resolverErr != nil {
		for index := range report.Entries {
			if len(report.Entries[index].Dungeons) > 0 {
				report.Entries[index].Diagnostics = append(report.Entries[index].Diagnostics, Diagnostic{Severity: "warning", Code: "topology_resolver_unavailable", Detail: resolverErr.Error()})
			}
		}
	}
	for _, entry := range report.Entries {
		report.QuestCount++
		report.DungeonCount += len(entry.Dungeons)
		report.GateCount += len(entry.Gates)
		for _, diagnostic := range entry.Diagnostics {
			if diagnostic.Severity == "error" {
				report.ErrorCount++
			} else {
				report.WarningCount++
			}
		}
	}
	report.InventoryHash = reportHash(report.Entries)
	report.Ready = report.ErrorCount == 0 && len(report.Entries) > 0
	return report, nil
}

func buildObjective(definition quest.Definition, maps *worldmap.Table) Objective {
	tag := normalize(definition.Type)
	owner := "standalone"
	if definition.MainQuestID > 0 {
		owner = "parent-bound-child"
	}
	if tag == "quest clear" || tag == "clear quest" {
		owner = "quest-clear-parent"
	}
	result := Objective{
		Family: objectiveFamily(tag), Type: tag, SubType: definition.SubType, Owner: owner,
		IntData: append([]int64(nil), definition.IntData...), CheckCount: append([]int64(nil), definition.CheckCount...),
		Prerequisite: flattenPrerequisites(definition),
	}
	if !dungeonObjective(tag) {
		return result
	}
	for _, value := range definition.IntData {
		if value <= 0 {
			continue
		}
		if dungeonValue, ok := maps.FindDungeon(value); ok {
			result.Targets = append(result.Targets, Target{ID: value, Kind: "dungeon", Path: dungeonValue.Path, Dungeon: value, Resolved: true})
			continue
		}
		if mapValue, ok := maps.FindMap(value); ok {
			target := Target{ID: value, Kind: "map", Path: mapValue.Path, Resolved: true}
			if mapValue.DungeonID.Set {
				target.Dungeon = mapValue.DungeonID.Value
			}
			result.Targets = append(result.Targets, target)
		}
	}
	return result
}

func dungeonObjective(tag string) bool {
	switch tag {
	case "clear map", "condition under clear", "condition under clear2", "normal clear":
		return true
	default:
		return false
	}
}

func stateChecks(objective Objective) []StateCheck {
	requires := append([]int64(nil), objective.Prerequisite...)
	transition := "finish_reward"
	if objective.Family == "linked-objective-parent" {
		transition = "finish_linked_children_then_reward"
	}
	return []StateCheck{
		{State: "available", Requires: requires, Transition: "accept"},
		{State: "active", Requires: requires, Transition: objective.Family},
		{State: "completed", Requires: requires, Transition: transition},
	}
}

func flattenPrerequisites(definition quest.Definition) []int64 {
	var ids []int64
	for _, group := range definition.PreRequiredGroups {
		ids = append(ids, group...)
	}
	for _, requirement := range definition.PreRequiredAnswers {
		ids = append(ids, requirement.QuestID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func objectiveFamily(tag string) string {
	switch tag {
	case "clear map", "condition under clear", "condition under clear2", "normal clear":
		return "clear-objective"
	case "hunt monster", "hunt enemy":
		return "kill-objective"
	case "quest clear", "clear quest":
		return "linked-objective-parent"
	case "seeking":
		return "collection-objective"
	case "seek n meet npc":
		return "collection-and-dialogue"
	case "meet npc":
		return "dialogue-objective"
	case "look cinematic":
		return "cinematic-objective"
	case "reach the range":
		return "range-objective"
	case "question":
		return "question-objective"
	default:
		return tag
	}
}

func joinTopology(definition quest.Definition, maps *worldmap.Table, towns *town.Table, resolver *worldmap.Resolver, diagnostics *[]Diagnostic) ([]Dungeon, []Gate, []Gate) {
	if !dungeonObjective(normalize(definition.Type)) {
		return nil, nil, nil
	}
	var dungeons []Dungeon
	seenDungeons := make(map[int64]struct{})
	for _, value := range definition.IntData {
		if value <= 0 {
			continue
		}
		if dungeonValue, ok := maps.FindDungeon(value); ok {
			if _, seen := seenDungeons[dungeonValue.ID]; seen {
				continue
			}
			seenDungeons[dungeonValue.ID] = struct{}{}
			dungeons = append(dungeons, summarizeDungeon(dungeonValue, maps, resolver, diagnostics))
			continue
		}
		if mapValue, ok := maps.FindMap(value); ok {
			ids := append([]int64(nil), mapValue.DungeonIDs...)
			if mapValue.DungeonID.Set {
				ids = append(ids, mapValue.DungeonID.Value)
			}
			for _, dungeonID := range ids {
				if dungeonID <= 0 {
					continue
				}
				if _, seen := seenDungeons[dungeonID]; seen {
					continue
				}
				if dungeonValue, ok := maps.FindDungeon(dungeonID); ok {
					seenDungeons[dungeonID] = struct{}{}
					dungeons = append(dungeons, summarizeDungeon(dungeonValue, maps, resolver, diagnostics))
				}
			}
		}
	}
	sort.Slice(dungeons, func(i, j int) bool { return dungeons[i].ID < dungeons[j].ID })
	var gates, routes []Gate
	if towns != nil {
		for _, value := range towns.Towns() {
			for _, area := range value.Areas {
				needsQuest := contains(area.NeedQuests, definition.ID)
				for _, dungeon := range dungeons {
					if area.DungeonGate == nil || *area.DungeonGate != dungeon.ID {
						continue
					}
					gate := gateFromArea(value.ID, area)
					gates = append(gates, gate)
					if len(gate.ExitNPCIDs) > 0 {
						routes = append(routes, gate)
					}
				}
				if needsQuest {
					gate := gateFromArea(value.ID, area)
					if area.Kind == "gate" || area.Kind == "dungeon_gate" {
						gates = append(gates, gate)
					} else {
						routes = append(routes, gate)
					}
				}
			}
		}
	}
	uniqueGates(&gates)
	uniqueGates(&routes)
	if len(dungeons) > 0 && len(gates) == 0 {
		*diagnostics = append(*diagnostics, Diagnostic{Severity: "warning", Code: "dungeon_gate_not_found", Detail: fmt.Sprintf("quest %d has dungeon topology but no PVF town gate was joined", definition.ID)})
	}
	return dungeons, gates, routes
}

func summarizeDungeon(value worldmap.Dungeon, maps *worldmap.Table, resolver *worldmap.Resolver, diagnostics *[]Diagnostic) Dungeon {
	result := Dungeon{ID: value.ID, Path: value.Path}
	if area, ok := maps.FindAreaByDungeon(value.ID); ok {
		result.AreaID, result.AreaPath = area.ID, area.Path
	}
	for _, sourceMaze := range value.Mazes {
		maze := Maze{Index: sourceMaze.Index, QuestConnect: append([]int64(nil), sourceMaze.QuestConnection...), Layered: len(sourceMaze.LayeredSpecifications) > 0 || len(sourceMaze.LayeredMapSpecifications) > 0}
		if sourceMaze.Start != nil {
			maze.Start = &Point{X: sourceMaze.Start.X, Y: sourceMaze.Start.Y}
		}
		if sourceMaze.Boss != nil {
			maze.Boss = &Point{X: sourceMaze.Boss.X, Y: sourceMaze.Boss.Y}
		}
		maze.Rooms = len(sourceMaze.MapSpecifications) + len(sourceMaze.BossSpecifications) + len(sourceMaze.LayeredSpecifications)
		if resolver != nil {
			if stages, err := resolver.StoryStages(value.ID, sourceMaze.Index); err == nil {
				maze.LayeredStages = len(stages)
				maze.Layered = maze.Layered || len(stages) > 0
			}
		}
		for _, item := range sourceMaze.Diagnostics {
			maze.Diagnostics = append(maze.Diagnostics, item.Message)
		}
		result.Mazes = append(result.Mazes, maze)
	}
	for _, item := range value.Diagnostics {
		result.Diagnostics = append(result.Diagnostics, item.Message)
	}
	if len(value.Mazes) == 0 {
		*diagnostics = append(*diagnostics, Diagnostic{Severity: "error", Code: "dungeon_has_no_maze", Detail: fmt.Sprintf("dungeon %d (%s) has no [maze info]", value.ID, value.Path)})
	}
	return result
}

func gateFromArea(townID int64, area town.Area) Gate {
	gate := Gate{TownID: townID, AreaID: area.ID, MapPath: area.MapPath, Kind: area.Kind, MinLevel: area.MinLevel, NeedQuests: append([]int64(nil), area.NeedQuests...), ExitNPCIDs: append([]int64(nil), area.TownExitTargetNPCIDs...)}
	gate.HasGate = area.Gate != nil
	if area.DungeonGate != nil {
		gate.DungeonID = *area.DungeonGate
	}
	return gate
}

func uniqueGates(values *[]Gate) {
	if values == nil {
		return
	}
	seen := make(map[string]struct{})
	out := (*values)[:0]
	for _, value := range *values {
		key := fmt.Sprintf("%d:%d:%s:%d", value.TownID, value.AreaID, value.Kind, value.DungeonID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	*values = out
}

func contains(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func normalize(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "[]"))
}

func fingerprint(entry Entry) string {
	shape := struct {
		Objective Objective `json:"objective"`
		Dungeons  []Dungeon `json:"dungeons,omitempty"`
		Gates     []Gate    `json:"gates,omitempty"`
		Routes    []Gate    `json:"routes,omitempty"`
	}{entry.Objective, entry.Dungeons, entry.Gates, entry.ReturnRoutes}
	b, _ := json.Marshal(shape)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func reportHash(entries []Entry) string {
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry.Fingerprint)
	}
	sum := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(sum[:])
}

// WriteJSON emits an indented report for CI artifacts.
func WriteJSON(w io.Writer, report Report) error {
	if w == nil {
		return errors.New("story-gate JSON writer is nil")
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// WriteMarkdown emits a compact checklist suitable for reviewing a PVF update.
func WriteMarkdown(w io.Writer, report Report) error {
	if w == nil {
		return errors.New("story-gate Markdown writer is nil")
	}
	fmt.Fprintf(w, "# Story Gate Regression Matrix\n\n")
	fmt.Fprintf(w, "- Version: `%d`\n- Start quest: `%d`\n- Character: job `%d`, grow_type `%d`, level `%d`\n- Inventory hash: `%s`\n- Ready: `%t`\n\n", report.Version, report.StartQuestID, report.Character.Job, report.Character.GrowType, report.Character.Level, report.InventoryHash, report.Ready)
	fmt.Fprintf(w, "| # | Quest | Type | Dungeons | Gates | Routes | Fingerprint |\n|---:|---:|---|---:|---:|---:|---|\n")
	for _, entry := range report.Entries {
		fmt.Fprintf(w, "| %d | %d | `%s` | %d | %d | %d | `%s` |\n", entry.Sequence, entry.QuestID, entry.QuestType, len(entry.Dungeons), len(entry.Gates), len(entry.ReturnRoutes), entry.Fingerprint)
	}
	fmt.Fprintln(w, "\n## Diagnostics")
	for _, entry := range report.Entries {
		for _, diagnostic := range entry.Diagnostics {
			fmt.Fprintf(w, "- `%s` q%d: %s (%s)\n", diagnostic.Severity, entry.QuestID, diagnostic.Code, diagnostic.Detail)
		}
	}
	return nil
}
