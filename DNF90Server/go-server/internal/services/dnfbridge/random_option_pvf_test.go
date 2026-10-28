package dnfbridge

import (
	"fmt"
	"math"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestResolveCurrentRandomOptionValuesUsesFirstDungeonBlock(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		"etc/randomoption/options/test.etc": "[option]\n96\n[dungeon]\n[level]\n0\n[value]\n-1.60 -6.10\n[/level]\n[/dungeon]\n[pvp]\n[dungeon]\n[level]\n0\n[value]\n9.90 10.90\n[/level]\n[/dungeon]\n[/pvp]\n",
	}
	optionType, value1, value2, err := resolveCurrentRandomOptionValues(source, "etc/randomoption/options/test.etc", 80)
	if err != nil {
		t.Fatal(err)
	}
	if optionType != 96 || value1 != 0xFF || value2 != 0xFA {
		t.Fatalf("resolved option = type=%d values=%02X/%02X", optionType, value1, value2)
	}
}

func TestResolveCurrentRandomOptionValuesEncodesElementPropertyEndpoints(t *testing.T) {
	tests := []struct {
		optionID int
		property string
	}{
		{optionID: 244, property: "fire element"},
		{optionID: 245, property: "water element"},
		{optionID: 246, property: "dark element"},
		{optionID: 247, property: "light element"},
	}
	for _, test := range tests {
		t.Run(test.property, func(t *testing.T) {
			optionPath := "etc/randomoption/options/element.etc"
			source := dungeonDropCatalogTestSource{
				optionPath: fmt.Sprintf("[dungeon]\n[option]\n%d\n[level]\n1\n[elemental property]\n`[%s]`\n[/elemental property]\n`[%s]`\n[/elemental property]\n[/level]\n[/option]\n[/dungeon]\n", test.optionID, test.property, test.property),
			}
			optionType, value1, value2, err := resolveCurrentRandomOptionValues(source, optionPath, 40)
			if err != nil {
				t.Fatal(err)
			}
			if optionType != byte(test.optionID) || value1 != 1 || value2 != 1 {
				t.Fatalf("resolved option = type=%d values=%02X/%02X", optionType, value1, value2)
			}
		})
	}
}

func TestResolveCurrentRandomOptionValuesRejectsMismatchedElementProperty(t *testing.T) {
	const optionPath = "etc/randomoption/options/mismatch.etc"
	source := dungeonDropCatalogTestSource{
		optionPath: "[dungeon]\n[option]\n244\n[level]\n1\n[elemental property]\n`[water element]`\n[/elemental property]\n`[water element]`\n[/elemental property]\n[/level]\n[/option]\n[/dungeon]\n",
	}
	if _, _, _, err := resolveCurrentRandomOptionValues(source, optionPath, 40); err == nil {
		t.Fatal("mismatched categorical element property was accepted")
	}
}

func TestEncodeCurrentRandomOptionNumberUsesOneByteWireDomain(t *testing.T) {
	tests := []struct {
		value float64
		want  byte
	}{
		{value: -200, want: 0x80},
		{value: -6.10, want: 0xFA},
		{value: -1.60, want: 0xFF},
		{value: 109.90, want: 109},
		{value: 460, want: 0xFF},
	}
	for _, test := range tests {
		got, err := encodeCurrentRandomOptionNumber(currentRandomOptionNumber{value: test.value})
		if err != nil || got != test.want {
			t.Fatalf("encode(%v) = %02X, %v; want %02X", test.value, got, err, test.want)
		}
	}
	if _, err := encodeCurrentRandomOptionNumber(currentRandomOptionNumber{value: math.NaN()}); err == nil {
		t.Fatal("NaN was accepted")
	}
}

func TestCurrentRandomOptionEquipmentKeysSupportsArmorPathOrders(t *testing.T) {
	materials := map[string]string{
		"cloth":   "cl",
		"leather": "lt",
		"larmor":  "la",
		"harmor":  "ha",
		"plate":   "mt",
	}
	parts := []string{"coat", "pants", "shoulder", "waist", "shoes"}
	for material, prefix := range materials {
		for _, part := range parts {
			path := fmt.Sprintf("equipment/character/common/%s/%s/test.equ", part, material)
			want := prefix + part
			keys := currentRandomOptionEquipmentKeys(path, "["+part+"]")
			found := false
			for _, key := range keys {
				if key == want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("path=%q keys=%v, want %q", path, keys, want)
			}
		}
	}
	legacyKeys := currentRandomOptionEquipmentKeys("equipment/character/common/leather/shoes/legacy.equ", "[shoes]")
	for _, key := range legacyKeys {
		if key == "ltshoes" {
			return
		}
	}
	t.Fatalf("legacy material/part path keys=%v, want ltshoes", legacyKeys)
}

func TestParseCurrentRandomOptionSlotWindows(t *testing.T) {
	const document = "[different weight]\n" +
		"2 1 0 25 2 2 10 30 2 3 30 55 3 1 10 35 3 2 20 40 3 3 40 65\n" +
		"[/different weight]\n"
	parsed, err := dnfpvf.Parse("test/etc", document)
	if err != nil {
		t.Fatal(err)
	}
	windows := parseCurrentRandomOptionSlotWindows(parsed, currentRandomOptionDifferentWeightSection)
	if len(windows[2]) != 3 || len(windows[3]) != 3 {
		t.Fatalf("slot windows = %+v", windows)
	}
	want := []alignedcmd.RandomOptionPercentWindow{
		{Slot: 1, MinPct: 0, MaxPct: 25},
		{Slot: 2, MinPct: 10, MaxPct: 30},
		{Slot: 3, MinPct: 30, MaxPct: 55},
	}
	for index, window := range windows[2] {
		if window != want[index] {
			t.Fatalf("rare window[%d] = %+v, want %+v", index, window, want[index])
		}
	}
	if got := windows[3][2]; got.Slot != 3 || got.MinPct != 40 || got.MaxPct != 65 {
		t.Fatalf("unique slot 3 window = %+v", got)
	}
	if change := parseCurrentRandomOptionSlotWindows(parsed, currentRandomOptionChangedPostfixSection); len(change) != 0 {
		t.Fatalf("absent [changed postfix] should parse empty, got %+v", change)
	}
}

func TestParseCurrentRandomOptionGradeTables(t *testing.T) {
	const document = "[postfix grade modification]\n" +
		"2 1 0 14 330 2 2 14 37 520 2 3 37 47 100 2 4 47 56 50\n" +
		"3 1 10 24 330 3 2 24 47 520 3 3 47 57 100 3 4 57 66 50\n" +
		"[/postfix grade modification]\n" +
		"[unable to modify postfix grade]\n180 181 244 245 247\n[/unable to modify postfix grade]\n" +
		"[gradeless postfix]\n130 131 132 65 66 67 61 62\n[/gradeless postfix]\n"
	parsed, err := dnfpvf.Parse("test/etc", document)
	if err != nil {
		t.Fatal(err)
	}
	bands := parseCurrentRandomOptionGradeBands(parsed)
	if len(bands[2]) != 4 || len(bands[3]) != 4 {
		t.Fatalf("grade bands = %+v", bands)
	}
	if got := bands[2][3]; got.Grade != 4 || got.MinPct != 47 || got.MaxPct != 56 || got.Weight != 50 {
		t.Fatalf("rare S band = %+v", got)
	}
	if got := bands[3][0]; got.Grade != 1 || got.MinPct != 10 || got.MaxPct != 24 || got.Weight != 330 {
		t.Fatalf("unique C band = %+v", got)
	}
	unmodifiable := parseCurrentRandomOptionOptionIDs(parsed, currentRandomOptionUnmodifiableSection)
	if len(unmodifiable) != 5 {
		t.Fatalf("unmodifiable ids = %v", unmodifiable)
	}
	for _, id := range []byte{180, 181, 244, 245, 247} {
		if _, ok := unmodifiable[id]; !ok {
			t.Fatalf("unmodifiable ids missing %d: %v", id, unmodifiable)
		}
	}
	gradeless := parseCurrentRandomOptionOptionIDs(parsed, currentRandomOptionGradelessSection)
	if len(gradeless) != 8 {
		t.Fatalf("gradeless ids = %v", gradeless)
	}
	for _, id := range []byte{61, 62, 65, 66, 67, 130, 131, 132} {
		if _, ok := gradeless[id]; !ok {
			t.Fatalf("gradeless ids missing %d: %v", id, gradeless)
		}
	}
}

func TestRealPVFRandomOptionConfigAndEligibleEquipment(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime random-option policy")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := loadCurrentRandomOptionConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := resolveCurrentRandomOptionMetadata(catalog, archive, config, 100300005)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Eligible || resolution.TargetKind != "equipment" || resolution.TargetRarity != 2 || resolution.TargetMinimumLevel != 80 || resolution.TargetEquipmentKey != "amulet" {
		t.Fatalf("runtime random-option resolution = %+v", resolution)
	}
	if len(resolution.QuantityWeights) == 0 || len(resolution.InitialGroups) != 3 || len(resolution.ModifiedGroups) != 3 || resolution.BreakSealGoldCost <= 0 || resolution.ModificationGoldCost <= 0 {
		t.Fatalf("runtime random-option policy incomplete = %+v", resolution)
	}
	if len(resolution.ResetGrades) != 4 || len(resolution.GradeUnmodifiable) == 0 || len(resolution.GradeGradeless) == 0 {
		t.Fatalf("runtime seal-grade tables incomplete = %+v", resolution)
	}
	if top := resolution.ResetGrades[3]; top.Grade != 4 || top.MinPct != 47 || top.MaxPct != 56 {
		t.Fatalf("runtime rare S window = %+v", top)
	}
	// 解封/变更写的是百分位，窗口必须来自真 PVF 的 [different weight]。
	if len(resolution.InitialSlotWindows) != 3 {
		t.Fatalf("runtime slot windows = %+v", resolution.InitialSlotWindows)
	}
	if first := resolution.InitialSlotWindows[0]; first.Slot != 1 || first.MinPct != 0 || first.MaxPct != 25 {
		t.Fatalf("runtime rare slot 1 window = %+v", first)
	}
	if last := resolution.InitialSlotWindows[2]; last.Slot != 3 || last.MinPct != 30 || last.MaxPct != 55 {
		t.Fatalf("runtime rare slot 3 window = %+v", last)
	}
	if len(resolution.ChangeSlotWindows) != 0 {
		t.Fatalf("current PVF ships no [changed postfix], got %+v", resolution.ChangeSlotWindows)
	}

	sealedResolution, err := resolveCurrentRandomOptionMetadata(catalog, archive, config, 101030264)
	if err != nil {
		t.Fatal(err)
	}
	if !sealedResolution.Eligible || sealedResolution.TargetRarity != 2 || sealedResolution.TargetMinimumLevel != 40 || sealedResolution.TargetEquipmentKey != "lswd" {
		t.Fatalf("runtime sealed-equipment resolution = %+v", sealedResolution)
	}
	foundElements := make(map[byte]bool, 4)
	for _, groups := range [][][]alignedcmd.RandomOptionCandidate{sealedResolution.InitialGroups, sealedResolution.ModifiedGroups} {
		for _, candidates := range groups {
			for _, candidate := range candidates {
				if candidate.Type >= 244 && candidate.Type <= 247 {
					if candidate.Value1 != 1 || candidate.Value2 != 1 {
						t.Fatalf("runtime element option %d values = %02X/%02X", candidate.Type, candidate.Value1, candidate.Value2)
					}
					foundElements[candidate.Type] = true
				}
			}
		}
	}
	for optionType := byte(244); optionType <= 247; optionType++ {
		if !foundElements[optionType] {
			t.Fatalf("runtime element option %d was not resolved", optionType)
		}
	}
}

func TestRealPVFRandomOptionArmorEquipmentKeys(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify runtime random-option armor policy")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := loadCurrentRandomOptionConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, rarity := range []int64{2, 3} {
		for _, prefix := range []string{"cl", "lt", "la", "ha", "mt"} {
			for _, part := range []string{"coat", "pants", "shoulder", "waist", "shoes"} {
				key := prefix + part
				if _, ok := config.initialSelect[rarity][key]; !ok {
					t.Fatalf("runtime initial random-option selection missing rarity=%d armor key=%s", rarity, key)
				}
				if _, ok := config.modifiedSelect[rarity][key]; !ok {
					t.Fatalf("runtime modified random-option selection missing rarity=%d armor key=%s", rarity, key)
				}
			}
		}
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{100320649, 100310581, 100260138, 100210132, 100160110} {
		resolution, err := resolveCurrentRandomOptionMetadata(catalog, archive, config, itemID)
		if err != nil {
			t.Fatalf("resolve item=%d: %v", itemID, err)
		}
		if resolution.Eligible {
			continue
		}
		document, documentErr := parseCurrentRandomOptionDocument(archive, resolution.TargetPVFPath)
		equipmentType, _ := document.Text("equipment type")
		keys := currentRandomOptionEquipmentKeys(resolution.TargetPVFPath, equipmentType)
		_, initialKey, initialFound := currentRandomOptionSelectedGroups(config.initialSelect[resolution.TargetRarity], keys)
		_, modifiedKey, modifiedFound := currentRandomOptionSelectedGroups(config.modifiedSelect[resolution.TargetRarity], keys)
		t.Fatalf("item=%d resolution=%+v document_err=%v equipment_type=%q keys=%v initial=(%q,%t) modified=(%q,%t)",
			itemID, resolution, documentErr, equipmentType, keys, initialKey, initialFound, modifiedKey, modifiedFound)
	}
}
