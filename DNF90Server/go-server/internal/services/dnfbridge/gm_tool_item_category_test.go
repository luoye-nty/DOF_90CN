package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestGMItemTypeTag(t *testing.T) {
	for input, want := range map[string]string{
		"[weapon]":        "weapon",
		"`[Coat]`":        "coat",
		" [magic stone] ": "magic stone",
	} {
		if got := gmItemTypeTag(input); got != want {
			t.Fatalf("gmItemTypeTag(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGMStackableSegment(t *testing.T) {
	for input, want := range map[string]string{
		"[material]":            "材料",
		"[material]4":           "特殊材料",
		"[quest]":               "任务品",
		"[material expert job]": "副职业材料",
		"[avatar emblem]":       "徽章",
		"[waste]":               "消耗品",
	} {
		if got := gmStackableSegment(input); got != want {
			t.Fatalf("gmStackableSegment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGMItemGroup(t *testing.T) {
	for name, test := range map[string]struct {
		item GMItemSummary
		want string
	}{
		"equipment": {GMItemSummary{Kind: "equipment", EquipmentType: "[coat]"}, "equipment"},
		"pet":       {GMItemSummary{Kind: "equipment", EquipmentType: "[artifact red]"}, "pet"},
		"avatar":    {GMItemSummary{Kind: "equipment", EquipmentType: "[hat avatar]"}, "avatar"},
		"stackable": {GMItemSummary{Kind: "stackable"}, "stackable"},
		"earring":   {GMItemSummary{Kind: "equipment", EquipmentType: "[earring]"}, "equipment"},
		"other":     {GMItemSummary{Kind: "equipment", EquipmentType: "[unknown]"}, "other"},
	} {
		if got := gmItemGroup(test.item); got != test.want {
			t.Fatalf("%s group = %q, want %q", name, got, test.want)
		}
	}
}

func TestGMItemMatchesJob(t *testing.T) {
	if !gmItemMatchesJob(GMItemSummary{Kind: "equipment", UsableJobs: []string{"[swordman]"}}, "swordman") {
		t.Fatal("specific usable job did not match")
	}
	if !gmItemMatchesJob(GMItemSummary{Kind: "equipment", UsableJobs: []string{"[all]"}}, "mage") {
		t.Fatal("all-job equipment did not match")
	}
	if !gmItemMatchesJob(GMItemSummary{Kind: "equipment"}, "gunner") {
		t.Fatal("unrestricted equipment did not match")
	}
	if gmItemMatchesJob(GMItemSummary{Kind: "equipment", UsableJobs: []string{"[mage]"}}, "priest") {
		t.Fatal("wrong job matched")
	}
	if !gmItemMatchesJob(GMItemSummary{Kind: "stackable"}, "swordman") {
		t.Fatal("unrestricted stackable did not match a job filter")
	}
	if !gmItemMatchesJob(GMItemSummary{Kind: "stackable", UsableJobs: []string{"[swordman]"}}, "swordman") {
		t.Fatal("job-restricted stackable did not match its usable job")
	}
	if gmItemMatchesJob(GMItemSummary{Kind: "stackable", UsableJobs: []string{"[mage]"}}, "swordman") {
		t.Fatal("job-restricted stackable matched the wrong job")
	}
}

func TestGMItemEffectiveJobsUsesPackageRewardsWhenOuterItemAllowsAll(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":               "",
		"stackable/stackable.lst":           "100 `cash/male_package.stk`\n101 `cash/female_package.stk`\n102 `cash/generic.stk`\n103 `cash/nested_package.stk`\n",
		"equipment/equipment.lst":           "200 `avatar/male.equ`\n201 `avatar/female.equ`\n",
		"stackable/cash/male_package.stk":   "[stackable type] `[usable cera package]`\n[usable job] `[all]`\n[package data]\n200 1 102 3\n[/package data]\n",
		"stackable/cash/female_package.stk": "[stackable type] `[usable cera package]`\n[usable job] `[all]`\n[package data]\n201 1 102 3\n[/package data]\n",
		"stackable/cash/generic.stk":        "[stackable type] `[waste]`\n[usable job] `[all]`\n",
		"stackable/cash/nested_package.stk": "[stackable type] `[usable cera package]`\n[usable job] `[all]`\n[package data]\n101 1\n[/package data]\n",
		"equipment/avatar/male.equ":         "[equipment type] `[hat avatar]`\n[usable job] `[swordman]`\n",
		"equipment/avatar/female.equ":       "[equipment type] `[hat avatar]`\n[usable job] `[at swordman]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}

	for itemID, want := range map[uint32][]string{
		100: {"swordman"},
		101: {"at swordman"},
		103: {"at swordman"},
	} {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		got := gmItemEffectiveJobs(catalog, definition)
		if len(got) != len(want) || !gmJobTagsContain(got, want[0]) {
			t.Fatalf("item %d effective jobs = %v, want %v", itemID, got, want)
		}
	}

	male, _ := catalog.ResolveItem(100)
	if gmJobTagsContain(gmItemEffectiveJobs(catalog, male), "at swordman") {
		t.Fatal("male swordman package matched female swordman")
	}
	generic, _ := catalog.ResolveItem(102)
	if got := gmItemEffectiveJobs(catalog, generic); len(got) != 0 {
		t.Fatalf("generic consumable unexpectedly inferred jobs: %v", got)
	}
}

func TestGMItemEffectiveJobsRealPVFDreamPackageVariants(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the 2018 dream-package profession variants")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	for itemID, wantJobs := range map[uint32][]string{
		490702213: {"demonic swordman", "swordman"},
		490702240: {"at swordman"},
	} {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		jobs := gmItemEffectiveJobs(catalog, definition)
		if len(jobs) != len(wantJobs) {
			t.Fatalf("item %d effective jobs = %v, want %v", itemID, jobs, wantJobs)
		}
		for _, wantJob := range wantJobs {
			if !gmJobTagsContain(jobs, wantJob) {
				t.Fatalf("item %d effective jobs = %v, missing %q", itemID, jobs, wantJob)
			}
		}
	}
	male, _ := catalog.ResolveItem(490702213)
	female, _ := catalog.ResolveItem(490702240)
	if gmJobTagsContain(gmItemEffectiveJobs(catalog, male), "at swordman") {
		t.Fatal("male dream package matched female swordman")
	}
	if gmJobTagsContain(gmItemEffectiveJobs(catalog, female), "swordman") {
		t.Fatal("female dream package matched male swordman")
	}
}

func TestGMItemSupportsEquipmentAttributes(t *testing.T) {
	for input, want := range map[string]bool{
		"[weapon]": true, "[coat]": true, "[magic stone]": true,
		"[title name]": false, "[name tag]": false, "[hat avatar]": false,
		"[earring]": true, "[creature]": false, "[artifact red]": false,
	} {
		if got := gmItemSupportsEquipmentAttributes(input); got != want {
			t.Fatalf("gmItemSupportsEquipmentAttributes(%q) = %t, want %t", input, got, want)
		}
	}
}
