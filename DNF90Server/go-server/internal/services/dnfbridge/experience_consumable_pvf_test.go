package dnfbridge

import (
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestExperienceConsumableResolverUsesRuntimePVFRateAndDuration(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":              "",
		"equipment/equipment.lst":          "",
		"stackable/stackable.lst":          "10005931 `event/super_elixir.stk`\n10005930 `event/normal_elixir.stk`\n100 `waste.stk`\n",
		"etc/variousbuffstackablelist.etc": "[index]\n10005930\n[buff list]\n180\n[/buff list]\n[/index]\n",
		"stackable/event/super_elixir.stk": "[name] `Super Elixir`\n[stackable type] `[waste]`\n" +
			"[stat change duration]\n1800000\n`myself`\n[exp bonus rate]\n2.0\n",
		"stackable/event/normal_elixir.stk": "[name] `Normal Elixir`\n[stackable type] `[waste]`\n" +
			"[stat change duration]\n1800000\n`myself`\n[exp bonus rate]\n1.0\n",
		"stackable/waste.stk": "[stackable type] `[waste]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedExperienceConsumableResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(10005931)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.BonusPercent != 200 || resolution.DurationSeconds != 1800 || resolution.ExperienceSourceKey != 181 || resolution.PVFPath != "stackable/event/super_elixir.stk" {
		t.Fatalf("resolution=%+v", resolution)
	}
	normal, err := resolver(10005930)
	if err != nil {
		t.Fatal(err)
	}
	if normal.BonusPercent != 100 || normal.ExperienceSourceKey != 180 {
		t.Fatalf("normal resolution=%+v", normal)
	}
	ordinary, err := resolver(100)
	if err != nil || ordinary.BonusPercent != 0 {
		t.Fatalf("ordinary=%+v err=%v", ordinary, err)
	}
}

func TestExperienceConsumableResolverPreservesUnlimitedWaste(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"equipment/equipment.lst": "",
		"stackable/stackable.lst": "490700690 `cash/unlimited_elixir.stk`\n",
		"stackable/cash/unlimited_elixir.stk": "[name] `Unlimited Elixir`\n[stackable type] `[unlimited waste]`\n" +
			"[stat change duration]\n3600000\n`myself`\n[exp bonus rate]\n1.0\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedExperienceConsumableResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(490700690)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Reusable || resolution.BonusPercent != 100 || resolution.DurationSeconds != 3600 {
		t.Fatalf("resolution=%+v", resolution)
	}
}

func TestRealScriptPVFExperienceConsumable(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf experience-consumable smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedExperienceConsumableResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolver(10005931)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.BonusPercent != 200 || resolution.DurationSeconds != 1800 ||
		resolution.ExperienceSourceKey != 181 || resolution.PVFPath != "stackable/event/chn_event_20150730_growup/chn_3exp_10005931.stk" {
		t.Fatalf("resolution=%+v", resolution)
	}
	for _, test := range []struct {
		itemID uint32
		key    uint32
	}{
		{itemID: 10151244, key: 180},
		{itemID: 10151245, key: 181},
	} {
		mapped, err := catalog.ResolveItem(test.itemID)
		if err != nil {
			t.Fatal(err)
		}
		if mapped.ExperienceSourceKey != test.key {
			t.Fatalf("item %d source key=%d, want %d", test.itemID, mapped.ExperienceSourceKey, test.key)
		}
	}
	resolved := 0
	for _, itemID := range catalog.stackableIDs {
		definition, err := catalog.ResolveItem(itemID)
		if err != nil {
			t.Fatalf("resolve item %d: %v", itemID, err)
		}
		normalizedType := normalizeDungeonDropStackableType(definition.StackableType)
		if definition.ExpBonusPercent <= 0 || definition.EffectDurationMS <= 0 ||
			(normalizedType != "[waste]" && normalizedType != "[unlimited waste]") {
			continue
		}
		resolution, err := resolver(int64(itemID))
		if err != nil {
			t.Fatalf("resolve experience consumable %d (%s): %v", itemID, definition.PVFPath, err)
		}
		if resolution.BonusPercent <= 0 || resolution.DurationSeconds <= 0 ||
			resolution.Reusable != (normalizedType == "[unlimited waste]") {
			t.Fatalf("incomplete experience resolution item=%d definition=%+v resolution=%+v", itemID, definition, resolution)
		}
		resolved++
	}
	if resolved != 569 {
		t.Fatalf("resolved %d current-PVF dungeon experience elixirs, want 569", resolved)
	}
}
