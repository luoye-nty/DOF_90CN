// storygatematrix builds a read-only story/task gate regression report from a
// real Script.pvf. It never edits the archive or the server runtime state.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/storygate"
	"longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path (required)")
	startQuest := flag.Int64("start-quest", 3145, "starting quest ID")
	job := flag.Int("job", 2, "client job")
	growType := flag.Int("grow-type", 0, "client grow_type")
	level := flag.Int("level", 90, "client level")
	maxEntries := flag.Int("max-entries", 2048, "maximum reachable quest nodes")
	jsonPath := flag.String("json", "", "output JSON path")
	markdownPath := flag.String("markdown", "", "output Markdown path")
	flag.Parse()
	if *pvfPath == "" {
		fatalf("-pvf is required")
	}
	if *startQuest <= 0 || *job < 0 || *growType < 0 || *level <= 0 {
		fatalf("start-quest, job, grow-type, and level must be positive where applicable")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		fatalf("open PVF: %v", err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{
		quest.DefaultList,
		worldmap.DefaultMapList,
		worldmap.DefaultDungeonList,
		worldmap.DefaultWorldMapList,
		town.DefaultList,
	}})
	if err != nil {
		fatalf("build PVF index: %v", err)
	}
	quests, err := quest.Load(context.Background(), index)
	if err != nil {
		fatalf("load quest catalog: %v", err)
	}
	maps, err := worldmap.Load(context.Background(), index, worldmap.Options{})
	if err != nil {
		fatalf("load worldmap catalog: %v", err)
	}
	towns, err := town.Load(context.Background(), archive, town.Options{})
	if err != nil {
		fatalf("load town catalog: %v", err)
	}
	report, err := storygate.Build(context.Background(), quests, maps, towns, *startQuest, storygate.Character{Job: *job, GrowType: *growType, Level: *level}, storygate.Options{MaxEntries: *maxEntries})
	if err != nil {
		fatalf("build story-gate matrix: %v", err)
	}
	if *jsonPath != "" {
		if err := writeJSON(*jsonPath, report); err != nil {
			fatalf("write JSON: %v", err)
		}
	}
	if *markdownPath != "" {
		if err := writeMarkdown(*markdownPath, report); err != nil {
			fatalf("write Markdown: %v", err)
		}
	}
	fmt.Printf("story-gate matrix: quests=%d dungeons=%d gates=%d errors=%d warnings=%d hash=%s ready=%t\n", report.QuestCount, report.DungeonCount, report.GateCount, report.ErrorCount, report.WarningCount, report.InventoryHash, report.Ready)
}

func writeJSON(path string, report storygate.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return storygate.WriteJSON(file, report)
}

func writeMarkdown(path string, report storygate.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return storygate.WriteMarkdown(file, report)
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", values...)
	os.Exit(2)
}
