package dnfbridge

import (
	"errors"
	"os"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonClearRankCatalogUsesExactPVFThresholds(t *testing.T) {
	catalog, err := loadCurrentDungeonClearRankCatalog(bridgePVFSource{
		currentDungeonRankSystemPVFPath: "[rank grade]\n99 90 80 60 50 30 20 10\n[/rank grade]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		point byte
		want  byte
	}{
		{point: 99, want: 99},
		{point: 98, want: 90},
		{point: 80, want: 80},
		{point: 73, want: 60},
		{point: 10, want: 10},
		{point: 9, want: 0},
	} {
		if got := catalog.GradeForPoint(test.point); got != test.want {
			t.Fatalf("point=%d grade=%d want=%d", test.point, got, test.want)
		}
	}
}

func TestLocalDebugSettlementRankPointUsesHighestPVFThreshold(t *testing.T) {
	catalog := currentDungeonClearRankCatalog{gradeThresholds: []byte{99, 90, 80, 60, 50, 30, 20, 10}}
	debugService := &Service{localDebugMode: true}
	rankPoint, forced := debugService.localDebugSettlementRankPoint(catalog, 73)
	if !forced || rankPoint != 99 {
		t.Fatalf("debug rank point=%d forced=%t want=99,true", rankPoint, forced)
	}
	if grade := catalog.GradeForPoint(rankPoint); grade != 99 {
		t.Fatalf("debug grade=%d want=99", grade)
	}
	resources := currentDungeonSettlementTestResources(t)
	if scoreBonus := currentDungeonScoreBonusExp(1000, resources, rankPoint); scoreBonus != 200 {
		t.Fatalf("debug score bonus=%d want=200", scoreBonus)
	}

	normalService := &Service{}
	rankPoint, forced = normalService.localDebugSettlementRankPoint(catalog, 73)
	if forced || rankPoint != 73 {
		t.Fatalf("normal rank point=%d forced=%t want=73,false", rankPoint, forced)
	}
}

func TestLocalDebugSettlementRewardKeepsOp34RankPointAndRequestBinding(t *testing.T) {
	catalog := currentDungeonClearRankCatalog{gradeThresholds: []byte{99, 90, 80, 60, 50, 30, 20, 10}}
	const clientRankPoint = byte(73)
	settlementRankPoint, forced := (&Service{localDebugMode: true}).localDebugSettlementRankPoint(catalog, clientRankPoint)
	if !forced || settlementRankPoint != 99 {
		t.Fatalf("debug settlement rank point=%d forced=%t want=99,true", settlementRankPoint, forced)
	}
	body, err := buildCurrentDungeonPlayResultNoticeBody(currentDungeonPlayResultNotice{
		RankGrade:       catalog.GradeForPoint(clientRankPoint),
		ClearTimeMS:     12345,
		RankPoint:       clientRankPoint,
		AllVisitedClear: true,
		Participants: []currentDungeonPlayResultParticipant{{
			ObjectKey: 99, ClearTimeMS: 12345,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := settlementProtocolReader{body: body}
	if reader.u8() != 60 || reader.u32() != 12345 || reader.u8() != 0 || reader.u8() != clientRankPoint {
		t.Fatalf("debug op34 does not preserve the client result state: %x", body)
	}

	character := dnfrepo.CharacterRecord{
		CharacterID: "99",
		Level:       5,
		Stats:       map[string]int64{"exp": 12345},
	}
	plan, err := buildCurrentDungeonSettlementPacketPlan(
		clientRankPoint,
		currentDungeonPlayResultNotice{
			RankGrade:       catalog.GradeForPoint(clientRankPoint),
			ClearTimeMS:     12345,
			RankPoint:       clientRankPoint,
			AllVisitedClear: true,
			Participants: []currentDungeonPlayResultParticipant{{
				ObjectKey:   99,
				ClearTimeMS: 12345,
			}},
		},
		currentDungeonClearRewardSnapshot{
			CharacterID:   99,
			CompletionKey: "debug-settlement",
			Source:        "committed_test",
			Committed:     true,
			Base:          [4]uint32{100},
			Tail:          currentDungeonClearRewardTail{ShowResult: true},
		},
		character,
		dnfrepo.SkillPointState{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ClientRankPoint != clientRankPoint {
		t.Fatalf("plan request rank=%d want=%d", plan.ClientRankPoint, clientRankPoint)
	}
}

func TestCurrentDungeonClearRankCatalogRejectsMissingAndInvalidPVFSections(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "missing", text: "[other]\n1\n[/other]\n"},
		{name: "ascending", text: "[rank grade]\n90 99\n[/rank grade]\n"},
		{name: "range", text: "[rank grade]\n256\n[/rank grade]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadCurrentDungeonClearRankCatalog(bridgePVFSource{currentDungeonRankSystemPVFPath: test.text})
			if !errors.Is(err, errCurrentDungeonClearRankCatalogInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRealScriptPVFCurrentDungeonClearRankCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify real settlement rank thresholds")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCurrentDungeonClearRankCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.GradeForPoint(73); got != 60 {
		t.Fatalf("real Script.pvf rank point 73 grade=%d want=60", got)
	}
}

func TestCurrentDungeonSettlementPresentationUsesFrozenRuntimeDomains(t *testing.T) {
	catalog, err := loadCurrentDungeonClearRankCatalog(bridgePVFSource{
		currentDungeonRankSystemPVFPath: "[rank grade]\n99 90 80 60 50 30 20 10\n[/rank grade]\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Unix(1700000000, 0)
	completedAt := startedAt.Add(12*time.Second + 345*time.Millisecond)
	run := worldmap.DungeonRunSnapshot{
		Status:  worldmap.DungeonRunCompleted,
		Visited: []worldmap.RoomCoordinate{{X: 0, Y: 0}, {X: 1, Y: 0}},
		Cleared: []worldmap.RoomCoordinate{{X: 0, Y: 0}, {X: 1, Y: 0}},
	}
	presentation, err := currentDungeonSettlementPresentationForRun(startedAt, completedAt, run, catalog, 73)
	if err != nil {
		t.Fatal(err)
	}
	if presentation.RankGrade != 60 || presentation.ClearTimeMS != 12345 ||
		presentation.TimeBonusPoint != 0 || !presentation.AllVisitedClear {
		t.Fatalf("presentation=%+v", presentation)
	}
}

func TestCurrentDungeonSettlementPresentationRejectsUnsafeClockOrIncompleteRun(t *testing.T) {
	catalog := currentDungeonClearRankCatalog{gradeThresholds: []byte{99}}
	startedAt := time.Unix(1700000000, 0)
	completedAt := startedAt.Add(-time.Millisecond)
	_, err := currentDungeonSettlementPresentationForRun(
		startedAt,
		completedAt,
		worldmap.DungeonRunSnapshot{Status: worldmap.DungeonRunCompleted},
		catalog,
		99,
	)
	if !errors.Is(err, errCurrentDungeonSettlementPresentationBad) {
		t.Fatalf("clock reversal error=%v", err)
	}
	_, err = currentDungeonSettlementPresentationForRun(
		startedAt,
		startedAt,
		worldmap.DungeonRunSnapshot{Status: worldmap.DungeonRunActive},
		catalog,
		99,
	)
	if !errors.Is(err, errCurrentDungeonSettlementPresentationBad) {
		t.Fatalf("incomplete run error=%v", err)
	}
}

func TestCurrentDungeonAllVisitedRoomsClearedRejectsDuplicateOrMissingRoom(t *testing.T) {
	run := worldmap.DungeonRunSnapshot{
		Status:  worldmap.DungeonRunCompleted,
		Visited: []worldmap.RoomCoordinate{{X: 0, Y: 0}, {X: 1, Y: 0}},
		Cleared: []worldmap.RoomCoordinate{{X: 0, Y: 0}, {X: 0, Y: 0}},
	}
	if currentDungeonAllVisitedRoomsCleared(run) {
		t.Fatal("duplicate/missing clear was accepted")
	}
}
