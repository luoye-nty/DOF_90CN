package dnfbridge

import (
	"context"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestGMSearchAvatarSetsUsesDeterministicPVFPackageContents(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                "",
		"stackable/stackable.lst":            "100 `cash/male.stk`\n101 `cash/female.stk`\n102 `cash/ambiguous.stk`\n103 `cash/short.stk`\n104 `cash/nested.stk`\n",
		"equipment/equipment.lst":            "200 `avatar/male_hair.equ`\n201 `avatar/male_coat.equ`\n202 `avatar/male_pants.equ`\n203 `avatar/male_hair_alt.equ`\n210 `avatar/female_hair.equ`\n211 `avatar/female_coat.equ`\n212 `avatar/female_pants.equ`\n",
		"stackable/cash/male.stk":            "[name] `Male Set`\n[stackable type] `[usable cera package]`\n[usable job] `[all]`\n[package data]\n200 1 201 1 202 1\n[/package data]\n",
		"stackable/cash/female.stk":          "[name] `Female Set`\n[stackable type] `[usable cera package]`\n[usable job] `[all]`\n[package data]\n210 1 211 1 212 1\n[/package data]\n",
		"stackable/cash/ambiguous.stk":       "[name] `Ambiguous Set`\n[stackable type] `[usable cera package]`\n[package data]\n200 1 203 1 201 1 202 1\n[/package data]\n",
		"stackable/cash/short.stk":           "[name] `Short Set`\n[stackable type] `[usable cera package]`\n[package data]\n200 1 201 1\n[/package data]\n",
		"stackable/cash/nested.stk":          "[name] `Nested Set`\n[stackable type] `[usable cera package]`\n[package data]\n100 1\n[/package data]\n",
		"equipment/avatar/male_hair.equ":     "[name] `Male Hair`\n[equipment type] `[hair avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_hair_alt.equ": "[name] `Male Hair Alt`\n[equipment type] `[hair avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_coat.equ":     "[name] `Male Coat`\n[equipment type] `[coat avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_pants.equ":    "[name] `Male Pants`\n[equipment type] `[pants avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/female_hair.equ":   "[name] `Female Hair`\n[equipment type] `[hair avatar]`\n[usable job] `[at swordman]`\n",
		"equipment/avatar/female_coat.equ":   "[name] `Female Coat`\n[equipment type] `[coat avatar]`\n[usable job] `[at swordman]`\n",
		"equipment/avatar/female_pants.equ":  "[name] `Female Pants`\n[equipment type] `[pants avatar]`\n[usable job] `[at swordman]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}

	page, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "swordman", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Sets) != 2 {
		t.Fatalf("male avatar set page = %+v, want direct and nested deterministic sets", page)
	}
	if cached := catalog.gmAvatarSetsByJob["swordman"]; len(cached) != 2 {
		t.Fatalf("cached male avatar sets = %+v, want 2", cached)
	}
	next, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "swordman", Limit: 1, Offset: 1})
	if err != nil || next.Total != 2 || len(next.Sets) != 1 || next.Sets[0].Package.ItemID != 104 {
		t.Fatalf("cached male avatar-set page = %+v, err=%v", next, err)
	}
	first := page.Sets[0]
	if first.Package.ItemID != 100 || len(first.Items) != 3 ||
		first.Items[0].Slot != "hair" || first.Items[1].Slot != "coat" || first.Items[2].Slot != "pants" {
		t.Fatalf("first avatar set = %+v", first)
	}
	for _, set := range page.Sets {
		if set.Package.ItemID == 102 || set.Package.ItemID == 103 {
			t.Fatalf("ambiguous or incomplete package leaked into sets: %+v", set)
		}
	}

	female, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "at swordman", Query: "Female", Limit: 20})
	if err != nil || female.Total != 1 || len(female.Sets) != 1 || female.Sets[0].Package.ItemID != 101 {
		t.Fatalf("female avatar sets = %+v, err=%v", female, err)
	}
	nested, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "swordman", Query: "104", Limit: 1})
	if err != nil || nested.Total != 1 || nested.Sets[0].Package.ItemID != 104 {
		t.Fatalf("nested id query = %+v, err=%v", nested, err)
	}
	if _, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Limit: 20}); err == nil {
		t.Fatal("avatar set query without a profession succeeded")
	}
}

func TestGMSearchAvatarSetsResolvesProfessionBoosterSelections(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "100 `cash/rare_selection.stk`\n101 `cash/rare_cera.stk`\n102 `cash/rare_inner.stk`\n103 `cash/ambiguous_selection.stk`\n",
		"equipment/equipment.lst": "200 `avatar/male_hair.equ`\n201 `avatar/male_coat.equ`\n202 `avatar/male_pants.equ`\n203 `avatar/male_hair_alt.equ`\n210 `avatar/female_hair.equ`\n211 `avatar/female_coat.equ`\n212 `avatar/female_pants.equ`\n",
		"stackable/cash/rare_selection.stk": "[name] `Rare Sky Set`\n[stackable type] `[booster selection]`\n[usable job] `[all]`\n[booster selection num] 0\n" +
			"[booster select category] 0 0\n[avatar] 200 1 3 0 201 1 3 0 202 1 3 0\n[/booster select category]\n" +
			"[booster select category] 1 0\n[avatar] 210 1 3 0 211 1 3 0 212 1 3 0\n[/booster select category]\n",
		"stackable/cash/rare_cera.stk": "[name] `Rare Cera Outer Set`\n[stackable type] `[cera booster]`\n[usable job] `[all]`\n[booster info]\n[cera] 1 102 1000 1\n[/cera]\n[/booster info]\n",
		"stackable/cash/rare_inner.stk": "[name] `Rare Cera Inner Set`\n[stackable type] `[booster selection]`\n[usable job] `[all]`\n[booster selection num] 0\n" +
			"[booster select category] 0 0\n[avatar] 200 1 3 0 201 1 3 0 202 1 3 0\n[/booster select category]\n" +
			"[booster select category] 1 0\n[avatar] 210 1 3 0 211 1 3 0 212 1 3 0\n[/booster select category]\n",
		"stackable/cash/ambiguous_selection.stk": "[name] `Ambiguous Rare Set`\n[stackable type] `[booster selection]`\n[booster selection num] 0\n" +
			"[booster select category] 0 0\n[avatar] 200 1 3 0 203 1 3 0 201 1 3 0 202 1 3 0\n[/booster select category]\n",
		"equipment/avatar/male_hair.equ":     "[name] `Male Hair`\n[equipment type] `[hair avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_hair_alt.equ": "[name] `Male Hair Alt`\n[equipment type] `[hair avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_coat.equ":     "[name] `Male Coat`\n[equipment type] `[coat avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/male_pants.equ":    "[name] `Male Pants`\n[equipment type] `[pants avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/female_hair.equ":   "[name] `Female Hair`\n[equipment type] `[hair avatar]`\n[usable job] `[at swordman]`\n",
		"equipment/avatar/female_coat.equ":   "[name] `Female Coat`\n[equipment type] `[coat avatar]`\n[usable job] `[at swordman]`\n",
		"equipment/avatar/female_pants.equ":  "[name] `Female Pants`\n[equipment type] `[pants avatar]`\n[usable job] `[at swordman]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}

	male, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "swordman", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if male.Total != 3 {
		t.Fatalf("male booster avatar sets = %+v, want selection, cera outer and cera inner", male)
	}
	for _, set := range male.Sets {
		if set.Package.ItemID == 103 {
			t.Fatalf("ambiguous booster selection leaked into sets: %+v", set)
		}
		if len(set.Items) != 3 || set.Items[0].Item.ItemID != 200 {
			t.Fatalf("male booster set chose the wrong profession category: %+v", set)
		}
	}

	female, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{Job: "at swordman", Query: "Rare Sky", Limit: 20})
	if err != nil || female.Total != 1 || len(female.Sets) != 1 || female.Sets[0].Items[0].Item.ItemID != 210 {
		t.Fatalf("female booster avatar sets = %+v, err=%v", female, err)
	}
}

func TestGMSearchAvatarSetsRealPVFDreamPackage(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real avatar packages")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	page, err := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{
		Job: "at swordman", Query: "2018绮幻童梦礼包", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total == 0 || len(page.Sets) == 0 {
		t.Fatal("real PVF returned no female-swordman dream avatar set")
	}
	for _, set := range page.Sets {
		if len(set.Items) < 3 {
			t.Fatalf("real avatar set %d has only %d pieces", set.Package.ItemID, len(set.Items))
		}
		for _, row := range set.Items {
			if !gmItemMatchesJob(row.Item, "at swordman") {
				t.Fatalf("real avatar set %d contains another job item %+v", set.Package.ItemID, row)
			}
		}
	}
}

func TestGMSearchAvatarSetsRealPVFRareAvatarBoosters(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify real rare-avatar boosters")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	for _, testCase := range []struct {
		name      string
		packageID uint32
	}{
		{name: "战灵天舞套装礼盒", packageID: 2683779},
		{name: "苍穹之翼套装礼盒", packageID: 2683778},
		{name: "第6套稀有装扮套装礼盒", packageID: 490701709},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page, searchErr := service.GMSearchAvatarSets(context.Background(), GMAvatarSetQuery{
				Job: "swordman", Query: testCase.name, Limit: 20,
			})
			if searchErr != nil {
				t.Fatal(searchErr)
			}
			matched := 0
			for _, set := range page.Sets {
				if set.Package.ItemID != testCase.packageID {
					continue
				}
				if len(set.Items) != 8 {
					t.Fatalf("rare-avatar package %d items=%d, want 8: %+v", testCase.packageID, len(set.Items), set)
				}
				matched++
			}
			if matched == 0 {
				t.Fatalf("rare-avatar package %d was not indexed: %+v", testCase.packageID, page)
			}
		})
	}
}
