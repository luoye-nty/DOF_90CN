// Command honorprobe dumps the real etc/honorlevel.etc tables and replays one
// character's durable HonorExpert state against them.
//
// Why it exists: the dungeon settlement feeds a run's post-cap experience into
// honor.Tables.AdvanceExpert, and AdvanceExpert begins with ResolveExpert -- a
// STRICT validator. A character whose stored (level, progress) pair violates the
// table's invariant therefore cannot settle a dungeon at all, which the player
// sees as "killed the boss, no result, stuck inside the dungeon".
//
// Usage:
//
//	go run ./cmd/scratch/honorprobe <Script.pvf> [level:progress ...]
//
// With no level:progress pairs it probes the boundaries (0, 1, the last rows,
// and the maximum level).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: honorprobe <Script.pvf> [level:progress ...]")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open pvf:", err)
		os.Exit(1)
	}
	tables, err := dnfhonor.LoadTables(archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load honor tables:", err)
		os.Exit(1)
	}
	fmt.Printf("snapshot: %+v\n", tables.Snapshot())

	rows := tables.ExpertExperienceRows()
	fmt.Printf("expert experience rows: %d  (maximum expert level = %d)\n", len(rows), len(rows))

	fmt.Println("-- grade info --")
	for _, grade := range tables.ExpertGrades() {
		fmt.Printf("   grade=%d name=%q min=%d max=%d\n", grade.Grade, grade.Name, grade.MinLevel, grade.MaxLevel)
	}

	fmt.Println("-- selected rows --")
	probe := []int{0, 1, 2, 90, 91, 92, 93, 94, len(rows) - 3, len(rows) - 2, len(rows) - 1}
	for _, level := range probe {
		if level < 0 || level >= len(rows) {
			continue
		}
		fmt.Printf("   row[%d] level=%d required=%d\n", level, rows[level].Level, rows[level].Experience)
	}

	// Cross-check: rows must be strictly increasing for the advance loop to
	// terminate sensibly; report any place where level N's requirement is not
	// greater than level N-1's.
	nonIncreasing := 0
	for i := 1; i < len(rows); i++ {
		if rows[i].Experience <= rows[i-1].Experience {
			if nonIncreasing < 10 {
				fmt.Printf("   ! non-increasing: level %d required=%d <= level %d required=%d\n",
					rows[i].Level, rows[i].Experience, rows[i-1].Level, rows[i-1].Experience)
			}
			nonIncreasing++
		}
	}
	fmt.Printf("   non-increasing transitions: %d\n", nonIncreasing)

	states := make([]dnfhonor.ExpertProgress, 0, len(os.Args)-2)
	for _, raw := range os.Args[2:] {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "bad level:progress %q\n", raw)
			os.Exit(2)
		}
		level, err1 := strconv.ParseUint(parts[0], 10, 32)
		progress, err2 := strconv.ParseUint(parts[1], 10, 64)
		if err1 != nil || err2 != nil {
			fmt.Fprintf(os.Stderr, "bad level:progress %q\n", raw)
			os.Exit(2)
		}
		states = append(states, dnfhonor.ExpertProgress{Level: uint32(level), CurrentLevelExperience: progress})
	}
	if len(states) == 0 {
		states = []dnfhonor.ExpertProgress{
			{Level: 0, CurrentLevelExperience: 0},
			{Level: 1, CurrentLevelExperience: 0},
			{Level: uint32(len(rows) - 1), CurrentLevelExperience: rows[len(rows)-2].Experience - 1},
			{Level: uint32(len(rows)), CurrentLevelExperience: 0},
			{Level: uint32(len(rows)), CurrentLevelExperience: 1},
			{Level: uint32(len(rows)) + 1, CurrentLevelExperience: 0},
		}
	}

	fmt.Println("-- replay --")
	for _, state := range states {
		resolved, resolveErr := tables.ResolveExpert(state)
		fmt.Printf("   ResolveExpert(%d:%d) -> %d:%d err=%v\n",
			state.Level, state.CurrentLevelExperience,
			resolved.Level, resolved.CurrentLevelExperience, resolveErr)

		advanced, advanceErr := tables.AdvanceExpert(state, 1_000_000)
		fmt.Printf("   AdvanceExpert(%d:%d, 1_000_000) -> %d:%d err=%v\n",
			state.Level, state.CurrentLevelExperience,
			advanced.Level, advanced.CurrentLevelExperience, advanceErr)

		if state.Level < uint32(len(rows)) {
			required := rows[state.Level].Experience
			fmt.Printf("      table requires %d at level %d; stored progress %d is %s\n",
				required, state.Level, state.CurrentLevelExperience,
				map[bool]string{true: "FULL (>= required) -- settlement will fail", false: "ok"}[state.CurrentLevelExperience >= required])
		}
	}

	// The cumulative experience needed to REACH each expert level, which is what
	// decides whether "满级" is even reachable in play, and where the visible
	// grade boundaries sit.
	fmt.Println("-- cumulative expert experience to reach a level --")
	cumulative := uint64(0)
	milestones := map[int]bool{
		1: true, 10: true, 49: true, 50: true, 92: true, 99: true, 100: true,
		125: true, 126: true, 150: true, 151: true, 199: true, 200: true, 289: true,
	}
	for index, row := range rows {
		cumulative += row.Experience
		level := index + 1
		if milestones[level] {
			fmt.Printf("   level %3d : cumulative %d\n", level, cumulative)
		}
	}

	fmt.Println("-- ordinary honor --")
	totals := []uint64{0, 885829639, 35000000, 17699999999}
	for _, total := range totals {
		progress, resolveErr := tables.Resolve(total)
		fmt.Printf("   Resolve(%d) -> total=%d level=%d/%d levelExp=%d grade=%d err=%v\n",
			total, progress.TotalExperience, progress.Level, tables.MaxOrdinaryLevel(),
			progress.CurrentLevelExperience, progress.Grade, resolveErr)
	}
}
