package dnfbridge

import (
	"context"
	"os"
	"runtime"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFCatalogRetainedMemory(t *testing.T) {
	archivePath := os.Getenv("DNF90_REAL_PVF_MEMORY")
	if archivePath == "" {
		t.Skip("set DNF90_REAL_PVF_MEMORY to run the real catalog memory probe")
	}

	runtime.GC()
	baseline := readHeapStats()
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: archivePath})
	if err != nil {
		t.Fatalf("load real archive: %v", err)
	}
	runtime.GC()
	afterArchive := readHeapStats()
	t.Logf("archive heap_bytes=%d heap_objects=%d", afterArchive.bytes-baseline.bytes, afterArchive.objects-baseline.objects)

	skillCatalog, err := buildSkillCatalogFromSource(context.Background(), archive)
	if err != nil {
		t.Fatalf("load real skill catalog: %v", err)
	}
	runtime.GC()
	afterSkills := readHeapStats()
	t.Logf("archive_and_skills heap_bytes=%d heap_objects=%d", afterSkills.bytes-baseline.bytes, afterSkills.objects-baseline.objects)

	questCatalog, err := loadQuestMemoryCatalog(context.Background(), archive)
	if err != nil {
		t.Fatalf("load real quest catalog: %v", err)
	}
	runtime.GC()
	afterQuests := readHeapStats()
	t.Logf("archive_skills_and_quests heap_bytes=%d heap_objects=%d", afterQuests.bytes-baseline.bytes, afterQuests.objects-baseline.objects)
	runtime.KeepAlive(archive)
	runtime.KeepAlive(skillCatalog)
	runtime.KeepAlive(questCatalog)
}

func TestRealPVFFullPreloadRetainedMemory(t *testing.T) {
	archivePath := os.Getenv("DNF90_REAL_PVF_MEMORY")
	if archivePath == "" {
		t.Skip("set DNF90_REAL_PVF_MEMORY to run the real full-preload memory probe")
	}
	service := &Service{options: options{pvfPath: archivePath, pvfMaxBytes: platformpvf.DefaultMaxBytes}}
	preloads := []struct {
		name string
		load func(context.Context) error
	}{
		{"initial equipment index", service.preloadInitialEquipmentIndex},
		{"item catalog", service.preloadPVFItemCatalog},
		{"pet catalog", service.preloadPetPVFCatalog},
		{"expert job catalog", service.preloadExpertJobCatalog},
		{"adventure group table", service.preloadAdventureGroupTable},
		{"honor table", service.preloadHonorTable},
		{"town catalog", service.preloadTownCatalog},
		{"skill catalog", service.preloadSkillCatalog},
		{"profession profiles", service.preloadProfessionProfiles},
		{"quest catalog", service.preloadQuestCatalog},
		{"dungeon world map", service.preloadDungeonWorldMap},
		{"character stat table", service.preloadCharacterStatTable},
		{"equipment stat index", service.preloadEquipmentStatIndex},
		{"combat power affix catalog", service.preloadCombatPowerAffixCatalog},
		{"epic book catalog", service.preloadEpicBookCatalog},
		{"hell party catalog", service.preloadHellPartyCatalog},
		{"hell party drop catalog", service.preloadHellPartyDropCatalog},
		{"dungeon drop catalog", service.preloadDungeonDropCatalog},
		{"gold carry limit table", service.preloadCarryGoldLevelLimits},
	}
	runtime.GC()
	baseline := readHeapStats()
	for _, preload := range preloads {
		if err := preload.load(context.Background()); err != nil {
			t.Fatalf("preload %s: %v", preload.name, err)
		}
		runtime.GC()
		current := readHeapStats()
		t.Logf("after=%q heap_bytes=%d heap_objects=%d", preload.name, current.bytes-baseline.bytes, current.objects-baseline.objects)
	}
	runtime.KeepAlive(service)
}

type heapStats struct {
	bytes   uint64
	objects uint64
}

func readHeapStats() heapStats {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return heapStats{bytes: stats.HeapAlloc, objects: stats.HeapObjects}
}

func loadQuestMemoryCatalog(ctx context.Context, source dnfpvf.Source) (*dnfquest.Catalog, error) {
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		return nil, err
	}
	return dnfquest.Load(ctx, index)
}
