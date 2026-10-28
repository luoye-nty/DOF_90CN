package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonEndlessAltarAdmissionBridgeReservesAndRestoresGold(t *testing.T) {
	service, session, runtime := prepareCurrentDungeonEndlessAltarRoomMoveTest(t)
	reservation, err := service.reserveCurrentDungeonAltarAdmission(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if reservation == nil || reservation.snapshot.Fee != 22_000 || reservation.snapshot.GoldAfter != 78_000 ||
		reservation.snapshot.DailyEnterCount != 1 || reservation.snapshot.LottoPointEarned != 1 ||
		reservation.snapshot.LottoGoldSpent != 10_000 || runtime.altar.entryDailyCount != 1 {
		t.Fatalf("reservation=%+v altar=%+v", reservation, runtime.altar)
	}
	if err := service.restoreCurrentDungeonAltarAdmission(reservation); err != nil {
		t.Fatal(err)
	}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found || character.Stats["gold"] != 100_000 || runtime.altar.entryDailyCount != 0 {
		t.Fatalf("character=%+v found=%t err=%v entry_count=%d", character, found, err, runtime.altar.entryDailyCount)
	}
}

func TestCurrentDungeonUltimateAltarAdmissionBridgeCountsWithoutGoldFee(t *testing.T) {
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	reservation, err := service.reserveCurrentDungeonAltarAdmission(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if reservation == nil || reservation.kind != worldmap.BloodAltarDungeonKindUltimate ||
		reservation.snapshot.Fee != 0 || reservation.snapshot.DailyEnterCount != 1 ||
		runtime.altar.entryDailyCount != 1 {
		t.Fatalf("reservation=%+v altar=%+v", reservation, runtime.altar)
	}
	if err := service.restoreCurrentDungeonAltarAdmission(reservation); err != nil {
		t.Fatal(err)
	}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group is unavailable")
	}
	settings, found, err := repositories.Settings.Load(context.Background(), "character:99:dungeon_altar_v1")
	if err != nil || !found || settings.Values["ultimate_daily_count"] != "0" {
		t.Fatalf("settings=%+v found=%t err=%v", settings, found, err)
	}
}

func TestCurrentDungeonAltarAdmissionRuleMatchesServerParameterSemantics(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		currentDungeonAltarAdmissionDefinitionPath: `[price average]
1 50 8000 51 55 10000 56 99 12000
[lotto cost]
10000
[admission increase]
1 0 2 60 3 200
[lotto point]
1 1 2 3 3 5
`,
	}
	rule, err := loadCurrentDungeonAltarAdmissionRule(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.PriceBands) != 3 || rule.PriceBands[2].Price != 12_000 ||
		len(rule.AdmissionIncrease) != 3 || rule.AdmissionIncrease[1].Percent != 60 ||
		len(rule.LottoPoints) != 3 || rule.LottoPoints[1].Points != 3 || rule.LottoCost != 10_000 {
		t.Fatalf("rule=%+v", rule)
	}
}

func TestCurrentDungeonAltarExperienceUsesRoundAndEntryWeights(t *testing.T) {
	sources, err := progression.LoadMonsterExperienceSources(monsterRewardEvidenceTestSource())
	if err != nil {
		t.Fatal(err)
	}
	definition := currentDungeonAltarRewardDefinition{
		RewardExperienceWeights:    []float64{0, 1, 1.5},
		EntryCountExperienceWeight: []float64{1, 2, 3},
	}
	runtime := &runtimeDungeonState{
		Character: dnfrepo.CharacterRecord{CharacterID: "7", Level: 2},
		altar: &currentDungeonAltarRuntime{
			Kind: worldmap.BloodAltarDungeonKindEndless, completedRounds: 2, entryDailyCount: 1,
		},
	}
	resources := currentDungeonSettlementRewardResources{ExperienceSources: sources}
	got, err := currentDungeonAltarExperience(runtime, resources, definition)
	if err != nil || got != 360 {
		t.Fatalf("endless experience=%d err=%v", got, err)
	}
	runtime.altar.Kind = worldmap.BloodAltarDungeonKindUltimate
	got, err = currentDungeonAltarExperience(runtime, resources, definition)
	if err != nil || got != 1_800 {
		t.Fatalf("ultimate experience=%d err=%v", got, err)
	}
}

func TestRealScriptPVFCurrentDungeonAltarAdmissionRule(t *testing.T) {
	path := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify altar admission tables")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := loadCurrentDungeonAltarAdmissionRule(archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.PriceBands) != 3 || rule.PriceBands[0].MinimumLevel != 1 ||
		rule.PriceBands[2].MaximumLevel != 99 || rule.PriceBands[2].Price != 12_000 ||
		len(rule.AdmissionIncrease) != 12 || rule.AdmissionIncrease[0].Percent != 0 ||
		rule.AdmissionIncrease[1].Percent != 60 || rule.AdmissionIncrease[11].Percent != 4_550 ||
		len(rule.LottoPoints) != 13 || rule.LottoPoints[0].Points != 1 || rule.LottoPoints[12].Points != 10 ||
		rule.LottoCost != 10_000 {
		t.Fatalf("real rule=%+v", rule)
	}
}
