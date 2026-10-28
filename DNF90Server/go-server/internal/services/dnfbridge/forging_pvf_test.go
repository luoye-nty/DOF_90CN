package dnfbridge

import (
	"os"
	"path"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func testForgingConfigText() string {
	return "[table] 2 1 10000 1 3 2 5000 1.11 [/table]\n" +
		"[separate upgrade max] 2\n[level] 8\n" +
		"[item weights by grade] 90 3326 45 [/item weights by grade]\n" +
		"[item weights by rarity] 0.4 0.7 1 1.25 1.4 1.1 1.3 [/item weights by rarity]\n" +
		"[stat weights by rarity] 0.5 0.75 1 1.25 1.5 1.75 2 [/stat weights by rarity]\n" +
		"[weapon weights] 1 1 1 1 1 [/weapon weights]\n"
}

func TestParseAndResolveCurrentForgingConfig(t *testing.T) {
	document, err := dnfpvf.Parse(currentForgingPVFPath, testForgingConfigText())
	if err != nil {
		t.Fatal(err)
	}
	config, err := parseCurrentForgingConfig(document)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxLevel != 2 || len(config.Levels) != 2 || config.MaterialsByGrade[90].ItemID != 3326 || config.ItemRarityWeights[4] != 1.4 {
		t.Fatalf("config = %+v", config)
	}
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                  "",
		"stackable/stackable.lst":              "3326 `material/strong_breath.stk`\n9100 `ticket/fixed2.stk`\n9101 `ticket/add1.stk`\n9102 `ticket/portable_forge.stk`\n9103 `ticket/random_forge.stk`\n9104 `ticket/random_enchant.stk`\n",
		"equipment/equipment.lst":              "700 `weapon/test.equ`\n701 `coat/test.equ`\n",
		"stackable/material/strong_breath.stk": "[stackable type] `[material]`\n[stack limit] 1000\n",
		"stackable/ticket/fixed2.stk":          "[stackable type] `[etc]`\n[equipment separate reinforcement ticket]\n2 75\n`fixed` -1\n",
		"stackable/ticket/add1.stk":            "[stackable type] `[etc]`\n[equipment separate reinforcement ticket]\n1 100\n`additional` 1\n",
		"stackable/ticket/portable_forge.stk":  "[stackable type] `[waste]`\n[action type] `[portable genuine damage upgrade upgrede]`\n[/action type]\n[check usable itemlevel] 1 85\n",
		"stackable/ticket/random_forge.stk":    "[stackable type] `[etc]`\n[enchant random]\n[er_grade]\n0 1 2 3 5 6\n[er_slot]\n1\n[er_seal]\n0\n[er_enchant]\n2 1 40000 2 60000\n[/er_enchant]\n[/enchant random]\n[action type] `[random upgrade separate]`\n[/action type]\n",
		"stackable/ticket/random_enchant.stk":  "[stackable type] `[etc]`\n[enchant random]\n[er_slot]\n1\n[er_enchant]\n1 10 100000\n[/er_enchant]\n[/enchant random]\n",
		"equipment/weapon/test.equ":            "[equipment type] `[weapon]`\n[grade] 90\n[rarity] 4\n[minimum level] 16\n[durability] 45\n",
		"equipment/coat/test.equ":              "[equipment type] `[coat]`\n[grade] 90\n[rarity] 4\n[durability] 45\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	first, err := resolveCurrentForgingMetadata(catalog, config, 700, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.TargetLevel != 1 || first.IndependentAttack != 9 || first.SuccessWeight != 10000 || first.MaterialItemID != 3326 || first.MaterialCount != 63 || first.TargetDurability != 45 {
		t.Fatalf("first resolution = %+v", first)
	}
	second, err := resolveCurrentForgingMetadata(catalog, config, 700, 1)
	if err != nil || second.TargetLevel != 2 || second.IndependentAttack != 13 || second.SuccessWeight != 5000 || second.MaterialCount != 69 {
		t.Fatalf("second resolution = %+v err=%v", second, err)
	}
	nonWeapon, err := resolveCurrentForgingMetadata(catalog, config, 701, 0)
	if err != nil || nonWeapon.TargetLevel != 0 || nonWeapon.MaterialItemID != 0 {
		t.Fatalf("non-weapon resolution = %+v err=%v", nonWeapon, err)
	}

	fixed, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9100, 700, 1)
	if err != nil || fixed.TicketMode != "fixed" || fixed.TargetLevel != 2 || fixed.SuccessWeight != 7500 || fixed.MaterialItemID != 9100 || fixed.MaterialCount != 1 || fixed.IndependentAttack != 13 {
		t.Fatalf("fixed ticket resolution = %+v err=%v", fixed, err)
	}
	additional, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9101, 700, 1)
	if err != nil || additional.TicketMode != "additional" || additional.TargetLevel != 2 || additional.TicketApplyValue != 1 || additional.MaterialItemID != 9101 || additional.MaterialCount != 1 {
		t.Fatalf("additional ticket resolution = %+v err=%v", additional, err)
	}
	capped, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9101, 700, 2)
	if err != nil || capped.TicketMode != "additional" || capped.TargetLevel != 0 {
		t.Fatalf("capped additional ticket resolution = %+v err=%v", capped, err)
	}
	ordinary, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 3326, 700, 1)
	if err != nil || ordinary.TicketMode != "" || ordinary.TargetLevel != 2 || ordinary.MaterialItemID != 3326 || ordinary.MaterialCount != 69 {
		t.Fatalf("ordinary request resolution = %+v err=%v", ordinary, err)
	}
	portable, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9102, 700, 1)
	if err != nil || portable.TicketMode != "" || portable.TargetLevel != 2 || portable.MaterialItemID != 9102 || portable.MaterialCount != 1 || portable.SuccessWeight != 5000 {
		t.Fatalf("portable forging resolution = %+v err=%v", portable, err)
	}

	random, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9103, 700, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !random.TicketRandom || random.TicketMode != "" || random.MaterialItemID != 9103 || random.MaterialCount != 1 {
		t.Fatalf("random forging ticket resolution = %+v", random)
	}
	// The domain owns the roll, so no fixed level or weight may leak through.
	if random.TargetLevel != 0 || random.SuccessWeight != 0 || random.IndependentAttack != 0 {
		t.Fatalf("random forging ticket carried a pre-picked level: %+v", random)
	}
	if random.RandomTicket.SlotRestriction != 1 || random.RandomTicket.SealRestriction != 0 ||
		len(random.RandomTicket.Rarities) != 6 || random.TargetRarity != 4 {
		t.Fatalf("random forging ticket restrictions = %+v rarity=%d", random.RandomTicket, random.TargetRarity)
	}
	wantCandidates := []alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 1, Weight: 40000}, {TargetLevel: 2, Weight: 60000}}
	if len(random.RandomTicket.Entries) != len(wantCandidates) {
		t.Fatalf("random forging candidates = %+v", random.RandomTicket.Entries)
	}
	for index, want := range wantCandidates {
		if random.RandomTicket.Entries[index] != want {
			t.Fatalf("random forging candidate %d = %+v, want %+v", index, random.RandomTicket.Entries[index], want)
		}
	}
	// Every candidate level carries its own independent attack, matching the
	// ordinary per-level resolutions above.
	if random.RandomIndependentAttacks[1] != 9 || random.RandomIndependentAttacks[2] != 13 {
		t.Fatalf("random forging independent attacks = %+v", random.RandomIndependentAttacks)
	}

	// An [enchant random] stackable without the forging [action type] belongs
	// to the op50 reinforcement ticket scene and must not be claimed here.
	reinforcement, err := resolveCurrentForgingRequestMetadata(catalog, config, source, 9104, 700, 1)
	if err != nil || reinforcement.TicketRandom || reinforcement.MaterialItemID != 3326 {
		t.Fatalf("op50 random ticket claimed by op438: %+v err=%v", reinforcement, err)
	}
}

// TestResolveCurrentUpgradeTicketMetadataSkipsForgingRandomTickets pins the
// other half of the split: the op50 resolver must leave a
// `[random upgrade separate]` coupon alone so it cannot move the
// reinforcement level.
func TestResolveCurrentUpgradeTicketMetadataSkipsForgingRandomTickets(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                 "",
		"stackable/stackable.lst":             "9103 `ticket/random_forge.stk`\n9104 `ticket/random_enchant.stk`\n",
		"equipment/equipment.lst":             "700 `weapon/test.equ`\n",
		"stackable/ticket/random_forge.stk":   "[stackable type] `[etc]`\n[enchant random]\n[er_slot]\n1\n[er_enchant]\n1 6 100000\n[/er_enchant]\n[/enchant random]\n[action type] `[random upgrade separate]`\n[/action type]\n",
		"stackable/ticket/random_enchant.stk": "[stackable type] `[etc]`\n[enchant random]\n[er_slot]\n1\n[er_enchant]\n1 10 100000\n[/er_enchant]\n[/enchant random]\n",
		"equipment/weapon/test.equ":           "[equipment type] `[weapon]`\n[grade] 90\n[rarity] 4\n[minimum level] 16\n[durability] 45\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	forging, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9103, 700)
	if err != nil {
		t.Fatal(err)
	}
	if forging.TicketRandom || forging.TicketMode != "" {
		t.Fatalf("forging random coupon claimed by op50: %+v", forging)
	}
	reinforcement, err := resolveCurrentUpgradeTicketMetadata(catalog, source, 9104, 700)
	if err != nil {
		t.Fatal(err)
	}
	if !reinforcement.TicketRandom || reinforcement.TicketMode != "reinforce" || len(reinforcement.RandomTicket.Entries) != 1 {
		t.Fatalf("op50 random ticket = %+v", reinforcement)
	}
}

func TestRealPVFCurrentForgingConfig(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime upgrade_separate.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := currentForgingConfigFromSource(archive)
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxLevel != 8 || len(config.Levels) != 10 || len(config.MaterialsByGrade) != 99 || len(config.ItemRarityWeights) != 7 || len(config.StatRarityWeights) != 7 || len(config.WeaponWeights) == 0 {
		t.Fatalf("runtime config max=%d levels=%d grades=%d itemRarities=%d statRarities=%d weaponWeights=%d", config.MaxLevel, len(config.Levels), len(config.MaterialsByGrade), len(config.ItemRarityWeights), len(config.StatRarityWeights), len(config.WeaponWeights))
	}
	if config.MaterialsByGrade[90].ItemID != 3326 || config.MaterialsByGrade[90].BaseCount != 45 || config.Levels[7].SuccessWeight != 750 {
		t.Fatalf("runtime config grade90=%+v level8=%+v", config.MaterialsByGrade[90], config.Levels[7])
	}
}

func TestRealPVFCurrentForgingTickets(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime forging tickets")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	listText, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		t.Fatal(err)
	}
	listDocument, err := dnfpvf.Parse("stackable/stackable.lst", listText)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range dnfpvf.ParseList(listDocument) {
		pvfPath := path.Join("stackable", entry.Path)
		text, readErr := archive.ReadText(pvfPath)
		if readErr != nil || !strings.Contains(strings.ToLower(text), "[equipment separate reinforcement ticket]") {
			continue
		}
		document, parseErr := dnfpvf.Parse(pvfPath, text)
		if parseErr != nil {
			t.Fatalf("parse forging ticket item=%d path=%s: %v", entry.ID, pvfPath, parseErr)
		}
		ticket, found, ticketErr := parseCurrentForgingTicket(document)
		if ticketErr != nil || !found {
			t.Fatalf("unsupported forging ticket item=%d path=%s ticket=%+v err=%v", entry.ID, pvfPath, ticket, ticketErr)
		}
		count++
	}
	if count == 0 {
		t.Fatal("current PVF contains no [equipment separate reinforcement ticket] documents")
	}
	t.Logf("current PVF forging tickets supported count=%d", count)
}

// TestRealPVFRandomForgingTicket pins the shipped +6锻造券 2683788: an
// [enchant random] stackable whose [action type] is `[random upgrade
// separate]`, with a single er_enchant candidate at forging level 6.
func TestRealPVFRandomForgingTicket(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the random forging ticket")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	config, err := currentForgingConfigFromSource(archive)
	if err != nil {
		t.Fatal(err)
	}
	const weaponItemID = 100240061
	got, err := resolveCurrentForgingRequestMetadata(catalog, config, archive, 2683788, weaponItemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.TicketRandom || got.MaterialItemID != 2683788 || got.MaterialCount != 1 {
		t.Fatalf("random forging ticket = %+v", got)
	}
	if got.RandomTicket.SlotRestriction != 1 || got.RandomTicket.SealRestriction != 0 || len(got.RandomTicket.Rarities) != 6 {
		t.Fatalf("random forging ticket restrictions = %+v", got.RandomTicket)
	}
	want := []alignedcmd.UpgradeTicketRandomEntry{{TargetLevel: 6, Weight: 100000}}
	if len(got.RandomTicket.Entries) != 1 || got.RandomTicket.Entries[0] != want[0] {
		t.Fatalf("random forging candidates = %+v, want %+v", got.RandomTicket.Entries, want)
	}
	if _, ok := got.RandomIndependentAttacks[6]; !ok {
		t.Fatalf("no independent attack resolved for candidate level 6: %+v", got.RandomIndependentAttacks)
	}
	// The same coupon must stay invisible to the op50 reinforcement scene.
	ticket, err := resolveCurrentUpgradeTicketMetadata(catalog, archive, 2683788, weaponItemID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.TicketRandom || ticket.TicketMode != "" {
		t.Fatalf("random forging coupon claimed by op50: %+v", ticket)
	}
}

func TestRealPVFPortableForgingDevicesUseTheirOwnMaterialContract(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the portable forging device")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	config, err := currentForgingConfigFromSource(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{10003912, 490702021, 490702282} {
		got, resolveErr := resolveCurrentForgingRequestMetadata(catalog, config, archive, itemID, 101040191, 0)
		if resolveErr != nil {
			t.Fatalf("item=%d: %v", itemID, resolveErr)
		}
		if got.TargetLevel != 1 || got.MaterialItemID != itemID || got.MaterialCount != 1 || got.SuccessWeight <= 0 {
			t.Fatalf("portable forging item=%d resolution=%+v", itemID, got)
		}
	}
}

func TestCurrentEquipmentForgingLevelForStatsOnlyAcceptsWornMainWeapon(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentItemListForgingLevelOffset] = 4
	for _, test := range []struct {
		name  string
		entry dnfrepo.EquipmentEntry
		want  byte
	}{
		{name: "main_weapon_raw", entry: dnfrepo.EquipmentEntry{SlotIndex: currentRepositoryMainWeaponSlot, RawEntry: raw}, want: 4},
		{name: "main_weapon_extra", entry: dnfrepo.EquipmentEntry{SlotIndex: currentRepositoryMainWeaponSlot, Extra: map[string]string{"forging_level": "6"}}, want: 6},
		{name: "non_weapon_slot", entry: dnfrepo.EquipmentEntry{SlotIndex: currentRepositoryMainWeaponSlot + 1, RawEntry: raw, Extra: map[string]string{"forging_level": "6"}}, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := currentEquipmentForgingLevelForStats(test.entry); got != test.want {
				t.Fatalf("forging level for stats = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCurrentItemListEquipmentProjectionUsesCurrentForgingOffset(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentItemListForgingLevelOffset] = 4
	raw[0x6D] = 0xC7
	projected, ok := currentItemListEntryFromEquipment(dnfrepo.EquipmentEntry{
		SlotIndex: 11,
		ItemID:    700,
		RawEntry:  raw,
	})
	if !ok {
		t.Fatal("equipment projection rejected main weapon")
	}
	if projected.data[currentItemListForgingLevelOffset] != 4 || projected.data[0x6D] != 0xC7 {
		t.Fatalf("projected forging=%d opaque6d=%02x", projected.data[currentItemListForgingLevelOffset], projected.data[0x6D])
	}
}
