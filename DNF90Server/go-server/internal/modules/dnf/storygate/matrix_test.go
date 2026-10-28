package storygate

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/quest"
	"longheng.io/server/internal/modules/dnf/town"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const realPVFStoryGateBaseline3145Hash = "44df0b9ced5b68bce3f4b39a3b3e4630bed2d6df7aff8cf683036ede67e8974b"

func TestObjectiveFamilyAndFingerprintAreDeterministic(t *testing.T) {
	definition := quest.Definition{ID: 10, Type: "[clear map]", IntData: []int64{123}}
	objective := buildObjective(definition, nil)
	if objective.Family != "clear-objective" || objective.Type != "clear map" {
		t.Fatalf("objective=%+v", objective)
	}
	if checks := stateChecks(objective); len(checks) != 3 || checks[0].State != "available" || checks[1].Transition != "clear-objective" {
		t.Fatalf("state checks=%+v", checks)
	}
	entry := Entry{QuestID: 10, Objective: objective}
	first := fingerprint(entry)
	second := fingerprint(entry)
	if first == "" || first != second {
		t.Fatalf("fingerprints are not stable: %q %q", first, second)
	}
}

func TestWriteReports(t *testing.T) {
	report := Report{Version: MatrixVersion, StartQuestID: 10, Character: Character{Job: 2, GrowType: 0, Level: 90}, Entries: []Entry{{Sequence: 0, QuestID: 10, QuestType: "clear map", Fingerprint: "abc"}}, Ready: true}
	var json bytes.Buffer
	if err := WriteJSON(&json, report); err != nil || !strings.Contains(json.String(), `"start_quest_id": 10`) {
		t.Fatalf("json=%q err=%v", json.String(), err)
	}
	var markdown bytes.Buffer
	if err := WriteMarkdown(&markdown, report); err != nil || !strings.Contains(markdown.String(), "Story Gate Regression Matrix") || !strings.Contains(markdown.String(), "| 0 | 10 | `clear map` |") {
		t.Fatalf("markdown=%q err=%v", markdown.String(), err)
	}
}

func TestRealPVFStoryGateMatrix(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run the real story-gate matrix")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{quest.DefaultList, worldmap.DefaultMapList, worldmap.DefaultDungeonList, worldmap.DefaultWorldMapList, town.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	quests, err := quest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	maps, err := worldmap.Load(context.Background(), index, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	towns, err := town.Load(context.Background(), archive, town.Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Build(context.Background(), quests, maps, towns, 3145, Character{Job: 2, GrowType: 0, Level: 90}, Options{MaxEntries: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if report.QuestCount == 0 || report.InventoryHash == "" {
		t.Fatalf("empty real matrix: %+v", report)
	}
	if report.Entries[0].QuestID != 3145 {
		t.Fatalf("first entry=%+v", report.Entries[0])
	}
	if len(report.Entries[0].Dungeons) == 0 || len(report.Entries[0].Gates) == 0 {
		t.Fatalf("q3145 topology evidence missing: %+v", report.Entries[0])
	}
	if report.InventoryHash != realPVFStoryGateBaseline3145Hash {
		t.Fatalf("story-gate PVF baseline drift: got %s want %s; update the reviewed baseline only with an intentional PVF change", report.InventoryHash, realPVFStoryGateBaseline3145Hash)
	}
}
