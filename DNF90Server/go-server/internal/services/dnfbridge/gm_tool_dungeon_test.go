package dnfbridge

import (
	"context"
	"testing"

	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	appkit "longheng.io/server/internal/platform/app"
)

func TestGMUnlocksAllDungeonDifficultiesAndHellPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "dnf:gm-dungeon", Name: "Dungeon GM", Job: "0", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Synthetic Dungeon`\n" +
		"[difficulty level]\n1 2 3 4 5\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	source[worldmap.DefaultWorldMapList] = "39 `test.wdm`\n"
	source["worldmap/test.wdm"] = "[name]\n`Synthetic Hell Area`\n" +
		"[dungeon]\n700 -1\n[/dungeon]\n" +
		"[hell dungeon]\n1\n" +
		"[hell quest]\n2900 2901\n[/hell quest]\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	service.worldMapTable = table
	service.worldMapResolver = resolver

	initial, err := service.GMDungeonAccess(ctx, "77")
	if err != nil || initial.DifficultyDungeons != 1 || initial.DifficultyDungeonsUnlocked != 0 ||
		initial.HellAreas != 1 || initial.HellPermissionQuests != 2 || initial.HellPermissionQuestsCleared != 0 {
		t.Fatalf("initial dungeon access=%+v err=%v", initial, err)
	}
	difficulties, err := service.GMUnlockAllDungeonDifficulties(ctx, "77")
	if err != nil || difficulties.DifficultyDungeonsUnlocked != 1 || difficulties.DifficultyDungeonsUpdated != 1 {
		t.Fatalf("difficulty unlock=%+v err=%v", difficulties, err)
	}
	permission, found, err := repositories.DungeonPermission.Load(ctx, "77")
	if err != nil || !found || len(permission.Entries) != 1 || permission.Entries[0].DungeonID != 700 || permission.Entries[0].ClearState != 4 {
		t.Fatalf("permission=%+v found=%t err=%v", permission, found, err)
	}
	hell, err := service.GMUnlockAllHellChallenges(ctx, "77")
	if err != nil || hell.HellPermissionQuestsCleared != 2 || hell.HellPermissionQuestsUpdated != 2 {
		t.Fatalf("hell unlock=%+v err=%v", hell, err)
	}
	quests, found, err := repositories.Quest.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("quest record found=%t err=%v", found, err)
	}
	for _, questID := range []int64{2900, 2901} {
		state := quests.States[questID]
		if state.Status != "completed" || state.Extra["gm_rewards_granted"] != "false" || state.Extra["gm_all_hell_unlocked"] != "true" {
			t.Fatalf("quest %d state=%+v", questID, state)
		}
	}
	hellPermission, found, err := repositories.HellPartyPermission.Load(ctx, "77")
	if err != nil || !found || len(hellPermission.Entries) != 1 || hellPermission.Entries[0].AreaID != 39 || hellPermission.Entries[0].QuestID != 2900 {
		t.Fatalf("hell permission=%+v found=%t err=%v", hellPermission, found, err)
	}
	session := &gameSession{selectedCharacterID: 77}
	if err := service.authorizeCurrentDungeonHellParty(ctx, session, table, 700); err != nil {
		t.Fatalf("hell entry after GM unlock error=%v", err)
	}
	repeated, err := service.GMUnlockAllHellChallenges(ctx, "77")
	if err != nil || repeated.HellPermissionQuestsUpdated != 0 {
		t.Fatalf("repeated hell unlock=%+v err=%v", repeated, err)
	}

	// A character whose quests were already completed by an earlier unlock that
	// never wrote the permission row is repaired by running the unlock again.
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "78", AccountID: "dnf:gm-dungeon", Name: "Legacy Unlock", Job: "0", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "78",
		States: map[int64]dnfrepo.QuestState{
			2900: {Status: "completed"},
			2901: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	legacy, err := service.GMUnlockAllHellChallenges(ctx, "78")
	if err != nil || legacy.HellPermissionQuestsUpdated != 0 || legacy.HellPermissionQuestsCleared != 2 {
		t.Fatalf("legacy hell unlock=%+v err=%v", legacy, err)
	}
	legacyPermission, found, err := repositories.HellPartyPermission.Load(ctx, "78")
	if err != nil || !found || len(legacyPermission.Entries) != 1 || legacyPermission.Entries[0].AreaID != 39 {
		t.Fatalf("legacy hell permission=%+v found=%t err=%v", legacyPermission, found, err)
	}
}

func TestGMHellEpicDropRatePersistsAndScalesPVFRarityRate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	newService := func() *Service {
		return NewWithOptions(&appkit.Env{}, ServiceOptions{
			RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		})
	}
	service := newService()
	settings, err := service.GMGameplaySettings(ctx)
	if err != nil || settings.HellEpicDropRatePercent != gmHellEpicDropRateDefaultPercent {
		t.Fatalf("default settings=%+v err=%v", settings, err)
	}
	settings, err = service.GMSetHellEpicDropRate(ctx, 250)
	if err != nil || settings.HellEpicDropRatePercent != 250 {
		t.Fatalf("saved settings=%+v err=%v", settings, err)
	}
	persisted, err := newService().GMGameplaySettings(ctx)
	if err != nil || persisted.HellEpicDropRatePercent != 250 {
		t.Fatalf("persisted settings=%+v err=%v", persisted, err)
	}
	for _, test := range []struct {
		base    int64
		percent int
		want    int64
	}{
		{base: 7_000, percent: 100, want: 7_000},
		{base: 7_000, percent: 1_000, want: 70_000},
		{base: 20_000, percent: 10_000, want: int64(dnfhellparty.RarityRollDenominator)},
		{base: 7_000, percent: 0, want: 0},
		{base: 0, percent: 10000, want: 0},
	} {
		if got := scaleGMHellEpicRarityRate(test.base, test.percent); got != test.want {
			t.Fatalf("scale base=%d percent=%d got=%d want=%d", test.base, test.percent, got, test.want)
		}
	}
	if _, err := service.GMSetHellEpicDropRate(ctx, gmHellEpicDropRateMaximumPercent+1); err == nil {
		t.Fatal("out-of-range hell epic drop rate was accepted")
	}
	if maximum, err := service.GMSetHellEpicDropRate(ctx, gmHellEpicDropRateMaximumPercent); err != nil || maximum.HellEpicDropRatePercent != gmHellEpicDropRateMaximumPercent {
		t.Fatalf("maximum setting=%+v err=%v", maximum, err)
	}
}

func TestGMAdjustHellEpicRarity(t *testing.T) {
	const base = int64(10_000)
	if rarity, keep := gmAdjustHellEpicRarity(4, base, 0, 0); keep || rarity != 4 {
		t.Fatalf("zero-percent epic = rarity %d keep %t", rarity, keep)
	}
	if rarity, keep := gmAdjustHellEpicRarity(3, base, 200, 0); !keep || rarity != 4 {
		t.Fatalf("boosted non-epic = rarity %d keep %t", rarity, keep)
	}
	if rarity, keep := gmAdjustHellEpicRarity(3, base, 100, 0); !keep || rarity != 3 {
		t.Fatalf("default non-epic = rarity %d keep %t", rarity, keep)
	}
}

// TestGMEpicPotionTestMultiplierPersistsAndDoesNotDisturbTheHellRate：
// 服主要"先改一下倍率来测药剂是否真生效，后面再改回从 PVF 读取"，所以这个旋钮必须
//
//	① 默认 0（= 关闭，按 PVF 表走）；
//	② 能存能读（换一个 Service 实例也读得到）；
//	③ 与 GM 全局掉落率**互不干扰**（改一个不覆盖另一个）；
//	④ 越界要被拒绝。
func TestGMEpicPotionTestMultiplierPersistsAndDoesNotDisturbTheHellRate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	newService := func() *Service {
		return NewWithOptions(&appkit.Env{}, ServiceOptions{
			RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		})
	}
	service := newService()
	settings, err := service.GMGameplaySettings(ctx)
	if err != nil || settings.EpicDropRatePotionTestMultiplierPercent != gmEpicDropRatePotionTestMultiplierDefaultPercent {
		t.Fatalf("default potion test multiplier=%+v err=%v", settings, err)
	}

	if _, err := service.GMSetHellEpicDropRate(ctx, 250); err != nil {
		t.Fatal(err)
	}
	settings, err = service.GMSetEpicDropRatePotionTestMultiplier(ctx, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if settings.EpicDropRatePotionTestMultiplierPercent != 2000 || settings.HellEpicDropRatePercent != 250 {
		t.Fatalf("after potion set=%+v want potion 2000 + hell 250 kept", settings)
	}

	persisted, err := newService().GMGameplaySettings(ctx)
	if err != nil || persisted.EpicDropRatePotionTestMultiplierPercent != 2000 ||
		persisted.HellEpicDropRatePercent != 250 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}

	// 改回全局掉落率不能把测试倍率冲掉。
	if _, err := newService().GMSetHellEpicDropRate(ctx, 300); err != nil {
		t.Fatal(err)
	}
	after, err := newService().GMGameplaySettings(ctx)
	if err != nil || after.EpicDropRatePotionTestMultiplierPercent != 2000 || after.HellEpicDropRatePercent != 300 {
		t.Fatalf("after hell set=%+v err=%v", after, err)
	}

	// 关掉旋钮 = 回到 PVF 表。
	off, err := newService().GMSetEpicDropRatePotionTestMultiplier(ctx, 0)
	if err != nil || off.EpicDropRatePotionTestMultiplierPercent != 0 {
		t.Fatalf("disabled=%+v err=%v", off, err)
	}
	for _, bad := range []int{-1, gmEpicDropRatePotionTestMultiplierMaximumPercent + 1} {
		if _, err := newService().GMSetEpicDropRatePotionTestMultiplier(ctx, bad); err == nil {
			t.Fatalf("out-of-range potion test multiplier %d was accepted", bad)
		}
	}
}
