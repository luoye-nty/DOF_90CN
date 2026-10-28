package dnfbridge

import (
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func newUpgradeTicketTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                   "",
		"stackable/stackable.lst":               "9100 `ticket/plus10.stk`\n9101 `ticket/amplify12.stk`\n9102 `ticket/random.stk`\n9103 `ticket/plain.stk`\n9104 `ticket/portable_amplify.stk`\n9105 `ticket/portable_upgrade.stk`\n",
		"equipment/equipment.lst":               "700 `weapon/test_sword.equ`\n701 `weapon/sealed.equ`\n",
		"stackable/ticket/plus10.stk":           "[name] `+10 Ticket`\n[stackable type] `[material]`\n[equipment reinforcement ticket] 10 100\n",
		"stackable/ticket/amplify12.stk":        "[name] `+12 Amplify Ticket`\n[stackable type] `[material]`\n[equipment amplify reinforcement ticket] 12 50\n",
		"stackable/ticket/random.stk":           "[name] `Random Ticket`\n[stackable type] `[material]`\n[enchant random]\n[er_grade]\n0 1 2 3 5 6\n[er_slot]\n1\n[er_seal]\n2\n[er_enchant]\n2 10 60000 12 40000\n[/er_enchant]\n[/enchant random]\n",
		"stackable/ticket/plain.stk":            "[name] `Plain Material`\n[stackable type] `[material]`\n",
		"stackable/ticket/portable_amplify.stk": "[name] `Portable Amplifier`\n[stackable type] `[waste]`\n[action type] `[portable amplify]` 0 50\n[/action type]\n[check usable itemlevel] 1 85\n",
		"stackable/ticket/portable_upgrade.stk": "[name] `Portable Reinforcer`\n[stackable type] `[town and dungeon]`\n[action type] `[portable upgrade]` 10 30\n[/action type]\n[check usable itemlevel] 85 90\n",
		"equipment/weapon/test_sword.equ":       "[name] `Test Sword`\n[equipment type] `[weapon]`\n[rarity] 3\n[minimum level] 85\n[durability] 57\n",
		"equipment/weapon/sealed.equ":           "[name] `Sealed`\n[equipment type] `[weapon]`\n[impossible contents] `[upgrade]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

func TestResolveCurrentUpgradeTicketMetadata(t *testing.T) {
	catalog, source := newUpgradeTicketTestCatalog(t)

	resolution, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9100, 700)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if resolution.TicketMode != "reinforce" || resolution.TargetLevel != 10 || resolution.SuccessWeight != 100000 {
		t.Fatalf("reinforce ticket = %+v", resolution)
	}
	if resolution.TicketRandom {
		t.Fatalf("regular ticket marked random")
	}
	if resolution.TargetKind != "equipment" || resolution.TargetEquipmentType != "[weapon]" || resolution.TargetUpgradeForbidden {
		t.Fatalf("target = %+v", resolution)
	}
	if resolution.TargetRarity != 3 {
		t.Fatalf("target rarity = %d, want 3", resolution.TargetRarity)
	}

	amplify, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9101, 700)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if amplify.TicketMode != "amplify" || amplify.TargetLevel != 12 || amplify.SuccessWeight != 50000 {
		t.Fatalf("amplify ticket = %+v", amplify)
	}

	random, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9102, 700)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if !random.TicketRandom || random.TicketMode != "reinforce" {
		t.Fatalf("random ticket = %+v", random)
	}
	if random.RandomTicket.SlotRestriction != 1 || random.RandomTicket.SealRestriction != 2 {
		t.Fatalf("random restrictions = %+v", random.RandomTicket)
	}
	if len(random.RandomTicket.Rarities) != 6 || random.RandomTicket.Rarities[0] != 0 || random.RandomTicket.Rarities[5] != 6 {
		t.Fatalf("random rarities = %+v", random.RandomTicket.Rarities)
	}
	wantEntries := []alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 10, Weight: 60000}, {TargetLevel: 12, Weight: 40000}}
	if len(random.RandomTicket.Entries) != len(wantEntries) {
		t.Fatalf("random entries = %+v, want leading count skipped", random.RandomTicket.Entries)
	}
	for index, want := range wantEntries {
		if random.RandomTicket.Entries[index] != want {
			t.Fatalf("random entry %d = %+v, want %+v", index, random.RandomTicket.Entries[index], want)
		}
	}

	forbidden, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9100, 701)
	if err != nil {
		t.Fatalf("resolve error = %v", err)
	}
	if !forbidden.TargetUpgradeForbidden {
		t.Fatalf("forbidden target not flagged: %+v", forbidden)
	}

	portable, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9104, 700)
	if err != nil {
		t.Fatal(err)
	}
	if !portable.PortableAmplifier || portable.TicketMode != "" || portable.PortableMinimumLevel != 0 || portable.PortableMaximumLevel != 50 ||
		portable.TargetMinimumLevel != 85 || portable.TargetLevelMinimum != 1 || portable.TargetLevelMaximum != 85 || portable.TargetKind != "equipment" {
		t.Fatalf("portable amplifier = %+v", portable)
	}

	reinforcer, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9105, 700)
	if err != nil {
		t.Fatal(err)
	}
	if !reinforcer.PortableReinforcer || reinforcer.TicketMode != "" || reinforcer.PortableMinimumLevel != 10 || reinforcer.PortableMaximumLevel != 30 ||
		reinforcer.TargetMinimumLevel != 85 || reinforcer.TargetLevelMinimum != 85 || reinforcer.TargetLevelMaximum != 90 || reinforcer.TargetKind != "equipment" {
		t.Fatalf("portable reinforcer = %+v", reinforcer)
	}
}

func TestResolveCurrentUpgradeTicketMetadataNonTickets(t *testing.T) {
	catalog, source := newUpgradeTicketTestCatalog(t)
	tests := []struct {
		name     string
		material int64
	}{
		{name: "plain stackable", material: 9103},
		{name: "unknown material", material: 9999},
		{name: "equipment as material", material: 700},
		{name: "zero material", material: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := resolveCurrentUpgradeTicketMetadata(catalog, source, tc.material, 700)
			if err != nil {
				t.Fatalf("resolve error = %v", err)
			}
			if resolution.TicketMode != "" {
				t.Fatalf("ticket mode = %q, want empty for non-ticket", resolution.TicketMode)
			}
		})
	}
}

// TestRealPVFBlessedEnchantRandomTickets pins the shipped definitions of
// 祝福强化券 (武器) 2681908 and 祝福强化券 (防具) 2681909: both carry an
// [enchant random] block with the same eight er_enchant candidates and differ
// only in er_slot (1 weapon vs 2 armor).
func TestRealPVFBlessedEnchantRandomTickets(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the blessed enchant tickets")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	wantEntries := []alignedcmd.UpgradeTicketRandomEntry{
		{TargetLevel: 6, Weight: 15285},
		{TargetLevel: 7, Weight: 30805},
		{TargetLevel: 8, Weight: 32150},
		{TargetLevel: 9, Weight: 11201},
		{TargetLevel: 10, Weight: 8600},
		{TargetLevel: 11, Weight: 1440},
		{TargetLevel: 12, Weight: 437},
		{TargetLevel: 13, Weight: 82},
	}
	for _, test := range []struct {
		itemID   int64
		wantSlot int64
	}{
		{itemID: 2681908, wantSlot: 1},
		{itemID: 2681909, wantSlot: 2},
	} {
		got, resolveErr := resolveCurrentUpgradeTicketMetadata(catalog, archive, test.itemID, 100240061)
		if resolveErr != nil {
			t.Fatalf("item=%d: %v", test.itemID, resolveErr)
		}
		if !got.TicketRandom || got.TicketMode != "reinforce" {
			t.Fatalf("item=%d resolution=%+v, want a reinforce random ticket", test.itemID, got)
		}
		if got.RandomTicket.SlotRestriction != test.wantSlot || got.RandomTicket.SealRestriction != 2 ||
			len(got.RandomTicket.Rarities) != 6 {
			t.Fatalf("item=%d restrictions=%+v", test.itemID, got.RandomTicket)
		}
		if len(got.RandomTicket.Entries) != len(wantEntries) {
			t.Fatalf("item=%d entries=%+v", test.itemID, got.RandomTicket.Entries)
		}
		for index, want := range wantEntries {
			if got.RandomTicket.Entries[index] != want {
				t.Fatalf("item=%d entry %d = %+v, want %+v", test.itemID, index, got.RandomTicket.Entries[index], want)
			}
		}
	}
}

func TestRealPVFPortableAmplifiersUseTheirOwnMaterialContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the portable amplifier")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		itemID        int64
		currentMax    int64
		targetMinimum int64
		targetMaximum int64
	}{
		{itemID: 10005313, currentMax: 50, targetMinimum: 1, targetMaximum: 85},
		{itemID: 490702022, currentMax: 50, targetMinimum: 55, targetMaximum: 90},
		{itemID: 490702253, currentMax: 30, targetMinimum: 55, targetMaximum: 90},
		{itemID: 490702334, currentMax: 30, targetMinimum: 55, targetMaximum: 90},
	} {
		got, resolveErr := resolveCurrentUpgradeTicketMetadata(catalog, archive, test.itemID, 100240061)
		if resolveErr != nil {
			t.Fatalf("item=%d: %v", test.itemID, resolveErr)
		}
		if !got.PortableAmplifier || got.PortableMinimumLevel != 0 || got.PortableMaximumLevel != test.currentMax ||
			got.TargetLevelMinimum != test.targetMinimum || got.TargetLevelMaximum != test.targetMaximum || got.TargetMinimumLevel != 85 ||
			got.TargetKind != "equipment" {
			t.Fatalf("portable amplifier item=%d resolution=%+v", test.itemID, got)
		}
	}
}

func TestRealPVFPortableReinforcersUseTheirOwnMaterialContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify portable reinforcers")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		itemID       int64
		minimumLevel int64
		maximumLevel int64
		targetMin    int64
		targetMax    int64
	}{
		{itemID: 490000362, minimumLevel: 0, maximumLevel: 9, targetMin: 1, targetMax: 86},
		{itemID: 490702163, minimumLevel: 10, maximumLevel: 30, targetMin: 85, targetMax: 90},
	} {
		got, resolveErr := resolveCurrentUpgradeTicketMetadata(catalog, archive, test.itemID, 100240061)
		if resolveErr != nil {
			t.Fatalf("item=%d: %v", test.itemID, resolveErr)
		}
		if !got.PortableReinforcer || got.PortableMinimumLevel != test.minimumLevel || got.PortableMaximumLevel != test.maximumLevel ||
			got.TargetLevelMinimum != test.targetMin || got.TargetLevelMaximum != test.targetMax || got.TargetKind != "equipment" {
			t.Fatalf("portable reinforcer item=%d resolution=%+v", test.itemID, got)
		}
	}
}
