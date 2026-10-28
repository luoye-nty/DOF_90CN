// questcontinuityaudit prints PVF-backed epic-quest level gates. It is a
// read-only diagnostic and never edits the archive or repository state.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/quest"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type transition struct {
	fromID      int64
	toID        int64
	fromLevel   int
	toLevel     int
	fromPath    string
	toPath      string
	prereqs     string
	fromMatches bool
	toMatches   bool
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path (required)")
	job := flag.Int("job", 0, "client job")
	growType := flag.Int("grow-type", 0, "client grow_type")
	minLevel := flag.Int("min-level", 1, "minimum source quest level")
	maxLevel := flag.Int("max-level", 99, "maximum target quest level")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" {
		fatalf("-pvf is required")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		fatalf("open PVF: %v", err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{quest.DefaultList}})
	if err != nil {
		fatalf("build PVF index: %v", err)
	}
	catalog, err := quest.Load(context.Background(), index)
	if err != nil {
		fatalf("load quest catalog: %v", err)
	}
	character := quest.CharacterEligibility{Level: 99, Job: *job, GrowType: *growType}
	var transitions []transition
	for _, definition := range catalog.Definitions() {
		if !strings.EqualFold(strings.Trim(definition.Grade, " []\t\r\n"), "epic") ||
			definition.LevelMin < *minLevel || definition.LevelMin > *maxLevel {
			continue
		}
		for _, successor := range catalog.Successors(definition.ID) {
			if !strings.EqualFold(strings.Trim(successor.Grade, " []\t\r\n"), "epic") ||
				successor.LevelMin <= definition.LevelMin || successor.LevelMin > *maxLevel {
				continue
			}
			transitions = append(transitions, transition{
				fromID: definition.ID, toID: successor.ID,
				fromLevel: definition.LevelMin, toLevel: successor.LevelMin,
				fromPath: definition.Path, toPath: successor.Path,
				prereqs:     formatGroups(successor.PreRequiredGroups),
				fromMatches: catalog.MatchesCharacter(definition.ID, character),
				toMatches:   catalog.MatchesCharacter(successor.ID, character),
			})
		}
	}
	sort.Slice(transitions, func(i, j int) bool {
		if transitions[i].fromLevel != transitions[j].fromLevel {
			return transitions[i].fromLevel < transitions[j].fromLevel
		}
		if transitions[i].fromID != transitions[j].fromID {
			return transitions[i].fromID < transitions[j].fromID
		}
		return transitions[i].toID < transitions[j].toID
	})
	for _, entry := range transitions {
		fmt.Printf("L%02d q%-5d -> L%02d q%-5d match=%t/%t prereq=%s\n  %s\n  %s\n",
			entry.fromLevel, entry.fromID, entry.toLevel, entry.toID,
			entry.fromMatches, entry.toMatches, entry.prereqs, entry.fromPath, entry.toPath)
	}
	fmt.Printf("epic level-gate transitions=%d job=%d grow_type=%d range=%d..%d\n",
		len(transitions), *job, *growType, *minLevel, *maxLevel)
}

func formatGroups(groups [][]int64) string {
	if len(groups) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		values := make([]string, 0, len(group))
		for _, value := range group {
			values = append(values, fmt.Sprint(value))
		}
		parts = append(parts, "["+strings.Join(values, ",")+"]")
	}
	return strings.Join(parts, "|")
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(2)
}
