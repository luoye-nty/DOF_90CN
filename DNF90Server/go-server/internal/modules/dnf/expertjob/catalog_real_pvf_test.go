package expertjob

import (
	"context"
	"os"
	"reflect"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealScriptPVFAllExpertJobsCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify expert-job catalog")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, jobType := range []byte{EnchanterType, AlchemistType, DollControllerType} {
		config, ok := catalog.Config(jobType)
		if !ok || config.Level(0) != 1 || len(config.AutoRecipeIDs(0)) == 0 ||
			len(config.GiveUpCosts) == 0 || len(config.Recipes) == 0 || len(config.Extractors) == 0 || len(config.ExtractionRules) == 0 {
			t.Fatalf("expert job type=%d config=%+v found=%t", jobType, config, ok)
		}
		if !reflect.DeepEqual(config.GiveUpCosts, []int64{1000, 10_000, 100_000, 1_000_000}) {
			t.Fatalf("expert job type=%d give-up costs=%v", jobType, config.GiveUpCosts)
		}
		t.Logf("type=%d levels=%d recipes=%d extractors=%d extraction_rules=%d auto_level1=%v",
			jobType, len(config.ExperienceThresholds), len(config.Recipes), len(config.Extractors),
			len(config.ExtractionRules), config.AutoRecipeIDs(0))
	}
	enchanter, ok := catalog.Enchanter()
	if !ok || enchanter.MaximumStoreCharge != 10_000 || enchanter.InitialEndurance <= 0 ||
		enchanter.Recipes.ExtractionBaseConst != 500 || len(enchanter.CardRecipes) < 5 || len(enchanter.Cards) == 0 ||
		enchanter.CardRecipes[10015129].RequiredLevel != 1 || enchanter.Cards[3619].Qualification != 0 || enchanter.BeadByCard[3619] != 2600313 {
		t.Fatalf("enchanter config=%+v found=%t", enchanter, ok)
	}
	for _, test := range []struct {
		recipeID     int64
		essenceCount int64
	}{
		{recipeID: 10015132, essenceCount: 3},
		{recipeID: 10159564, essenceCount: 4},
	} {
		recipe, found := enchanter.CardRecipes[test.recipeID]
		if !found || expertJobRecipeMaterialCount(recipe.Materials, 2610041) != test.essenceCount {
			t.Fatalf("enchanter recipe=%d materials=%+v found=%t", test.recipeID, recipe.Materials, found)
		}
	}
	disjointer, ok := catalog.Disjointer()
	if !ok || disjointer.InitialEndurance != 300 || disjointer.MaximumStoreCharge != 10_000 || len(disjointer.GiveUpCosts) == 0 || disjointer.BaseConst != 150 ||
		len(disjointer.RepairRules) != 11 || disjointer.UpgradeCosts[2] != 30_000 || disjointer.CharacterLevelLimits[2] != 20 {
		t.Fatalf("disjointer config=%+v found=%t", disjointer, ok)
	}
	if !reflect.DeepEqual(disjointer.GiveUpCosts, []int64{1000, 10_000, 100_000, 1_000_000}) {
		t.Fatalf("disjointer give-up costs=%v", disjointer.GiveUpCosts)
	}
	disjointerMaterials := make(map[int64]bool)
	for _, rule := range disjointer.Results {
		disjointerMaterials[rule.ItemID] = true
	}
	for _, tables := range []map[int][]DisjointerSelectionRule{disjointer.AdditionalResults, disjointer.BigWinResults} {
		for _, rules := range tables {
			for _, rule := range rules {
				disjointerMaterials[rule.ItemID] = true
			}
		}
	}
	disjointerMaterials[disjointer.SelfServiceItemID] = true
	for _, required := range []int64{3037, 3166, 3167, 3228, 3242, 3245, 3262, 3299, 3300, 3332, 3336, 3337, 3338, 3339, 2610045, 10099777} {
		if !disjointerMaterials[required] {
			t.Fatalf("disjointer runtime tables are missing material=%d: %v", required, disjointerMaterials)
		}
	}
	for _, dollControllerSoul := range []int64{2610042, 2610043, 2610044, 2610048} {
		if disjointerMaterials[dollControllerSoul] {
			t.Fatalf("doll-controller soul=%d leaked into disjointer runtime tables", dollControllerSoul)
		}
	}
	t.Logf("enchanter card_recipes=%d cards=%d beads=%d repair_rules=%d; disjointer results=%d repair_rules=%d",
		len(enchanter.CardRecipes), len(enchanter.Cards), len(enchanter.BeadByCard), len(enchanter.RepairRules),
		len(disjointer.Results), len(disjointer.RepairRules))
}

func expertJobRecipeMaterialCount(materials []RecipeEntry, itemID int64) int64 {
	for _, material := range materials {
		if material.ItemID == itemID {
			return material.Count
		}
	}
	return 0
}
