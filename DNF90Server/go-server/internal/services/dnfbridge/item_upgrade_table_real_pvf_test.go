package dnfbridge

import (
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFUpgradeTableCarriesNPCReinforceMaterial(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime upgrade.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	text, err := archive.ReadText(currentUpgradePVFPath)
	if err != nil {
		t.Fatalf("read %s: %v", currentUpgradePVFPath, err)
	}
	doc, err := dnfpvf.Parse(currentUpgradePVFPath, text)
	if err != nil {
		t.Fatalf("parse %s: %v", currentUpgradePVFPath, err)
	}
	table := parseCurrentUpgradeTable(doc, text)
	if len(table.Rows) < 13 {
		t.Fatalf("upgrade table rows = %d, want at least 13", len(table.Rows))
	}
	wantPenalties := map[int][2]int{
		8:  {1, 0},
		9:  {1, 0},
		10: {1, 0},
		11: {2, 3},
		12: {2, 3},
		13: {3, 0},
	}
	for _, level := range []int{1, 8, 9, 10, 11, 12, 13} {
		row, ok := table.RowForLevel(level)
		if !ok {
			t.Fatalf("missing row for target level %d", level)
		}
		t.Logf("target_level=%d failure=%d penalty=(%d,%d) material=%d x%d", level, row.FailureWeight, row.PenaltyType, row.PenaltyValue, row.MaterialItemID, row.MaterialCount)
		if row.MaterialItemID != 3037 || row.MaterialCount <= 0 {
			t.Fatalf("target level %d material = %d x%d, want clear cube material", level, row.MaterialItemID, row.MaterialCount)
		}
		if want, found := wantPenalties[level]; found && (row.PenaltyType != want[0] || row.PenaltyValue != want[1]) {
			t.Fatalf("target level %d raw penalty=(%d,%d), want (%d,%d)", level, row.PenaltyType, row.PenaltyValue, want[0], want[1])
		}
	}
	for _, currentLevel := range []int{7, 8, 9, 10} {
		row, ok := table.RowForLevel(currentLevel + 1)
		if !ok {
			t.Fatalf("missing effective reinforcement row for current level %d", currentLevel)
		}
		penaltyType, penaltyValue := currentUpgradePenaltyForAttempt("reinforce", currentLevel, row, table)
		wantType := 1
		if currentLevel >= 10 {
			wantType = 3
		}
		if penaltyType != wantType || penaltyValue != 0 {
			t.Fatalf("reinforce current=%d effective penalty=(%d,%d), want (%d,0)", currentLevel, penaltyType, penaltyValue, wantType)
		}
	}
}

func TestRealPVFAmplifyUpgradeTableCarriesFailurePolicy(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime amplifyupgrade.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	text, err := archive.ReadText(currentAmplifyUpgradePVFPath)
	if err != nil {
		t.Fatalf("read %s: %v", currentAmplifyUpgradePVFPath, err)
	}
	doc, err := dnfpvf.Parse(currentAmplifyUpgradePVFPath, text)
	if err != nil {
		t.Fatalf("parse %s: %v", currentAmplifyUpgradePVFPath, err)
	}
	table := parseCurrentUpgradeTable(doc, text)
	if len(table.Rows) < 13 {
		t.Fatalf("amplify table rows = %d, want at least 13", len(table.Rows))
	}
	for _, level := range []int{8, 9, 10, 11, 12, 13} {
		row, ok := table.RowForLevel(level)
		if !ok {
			t.Fatalf("missing amplify row for target level %d", level)
		}
		t.Logf("amplify target_level=%d failure=%d penalty=(%d,%d) material=%d x%d", level, row.FailureWeight, row.PenaltyType, row.PenaltyValue, row.MaterialItemID, row.MaterialCount)
	}
	t.Logf("amplify destroy levels=%v", table.DestroyLevelByRarity)
	for _, currentLevel := range []int{7, 8, 9, 10} {
		row, ok := table.RowForLevel(currentLevel + 1)
		if !ok {
			t.Fatalf("missing effective amplification row for current level %d", currentLevel)
		}
		penaltyType, penaltyValue := currentUpgradePenaltyForAttempt("amplify", currentLevel, row, table)
		wantType := 1
		if currentLevel >= 10 {
			wantType = 3
		}
		if penaltyType != wantType || penaltyValue != 0 {
			t.Fatalf("amplify current=%d effective penalty=(%d,%d), want (%d,0)", currentLevel, penaltyType, penaltyValue, wantType)
		}
	}
}

func TestRealPVFUpgradeGoldCostUsesTargetEquipmentMetadata(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime upgrade costs")
	}
	service := &Service{options: options{pvfPath: pvfPath}}
	// 魔战无双腰带 100240061: [grade] 91, [rarity] 4, [waist]. Each mode reads
	// its own table, and the price is
	//   [cost][grade] * [cost weights by rarity][rarity] * [type][slot]
	//     * [cost weight by upgrade level][currentLevel]
	// so for the first level, whose coefficient is 0.50 in upgrade.etc:
	//   reinforce 57075 * 8 * 1.00 * 0.50 = 228300
	// amplifyupgrade.etc carries no [cost weight by upgrade level] section, so
	// 增幅 has no level factor and prices at the base cost:
	//   amplify   68400 * 4 * 0.70 * 1 = 191520
	// The earlier expectations here were computed while the row coefficient was
	// being dropped and [cost] was indexed by [minimum level] 85 instead of
	// [grade] 91, which is what made the client popup and the charge disagree.
	// The amplify one then carried the retired [table type] weight * 25 term
	// (0.02 * 25 = 0.50) and read 95760 until 2026-09-25.
	for _, test := range []struct {
		mode string
		want int64
	}{
		{mode: "reinforce", want: 228_300},
		{mode: "amplify", want: 191_520},
	} {
		t.Run(test.mode, func(t *testing.T) {
			table, err := service.currentUpgradeTableForMode(test.mode)
			if err != nil {
				t.Fatalf("load %s table: %v", test.mode, err)
			}
			cost, err := service.currentUpgradeGoldCost(table, 100240061, 1)
			if err != nil || cost != test.want {
				t.Fatalf("%s gold cost=%d err=%v want=%d", test.mode, cost, err, test.want)
			}
		})
	}
}

func TestRealPVFPersonalCargoUpgradeTicketsRemainStackableItems(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime upgrade tickets")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		itemID uint32
		path   string
	}{
		{itemID: 7273, path: "stackable/event/tcg/deed_7equipmentstrengthening.stk"},
		{itemID: 10008121, path: "stackable/cash/chn_20150827_Blind_Purchase/deed_8equipmentamplify_10008121.stk"},
	} {
		definition, err := items.ResolveItem(test.itemID)
		if err != nil {
			t.Fatalf("resolve ticket %d: %v", test.itemID, err)
		}
		if definition.Kind != dungeonDropItemStackable || definition.PVFPath != test.path {
			t.Fatalf("ticket %d definition=%+v", test.itemID, definition)
		}
	}
}

// TestRealPVFUpgradeDestroyRefundIsTracesOnly pins what a destroyed target
// actually refunds, because the observable symptom ("强化失败什么都没返回")
// looks identical to a dropped reward.
//
// upgrade.etc's only destruction payout is [disjoint] -> [disjoint bonus item]
// (3243; amplifyupgrade.etc uses 3244). The full section list carries no
// 无色小晶块 and no 灵魂 row -- those belong to 分解 (disjoint.etc +
// disjointer.exj, granted by the 分解机), not to upgrade failure, and 86JP's
// CalculateDestroyBonuses agrees: it returns nothing but the [disjoint] bonus.
//
// The count is (level - [upgrade const for bonus item count])^2 *
// [upgrade failed bonus weight by rarity]. That weight table has no common or
// uncommon entry, so destroying a 普通/高级 item refunds zero at any level --
// which is the whole explanation for a failure that returns nothing.
func TestRealPVFUpgradeDestroyRefundIsTracesOnly(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect runtime upgrade tables")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	for _, expected := range []struct {
		path       string
		bonusItem  int
		equipConst int
		levelConst int
	}{
		{currentUpgradePVFPath, 3243, 40, 10},
		{currentAmplifyUpgradePVFPath, 3244, 40, 10},
	} {
		text, err := archive.ReadText(expected.path)
		if err != nil {
			t.Fatalf("read %s: %v", expected.path, err)
		}
		doc, err := dnfpvf.Parse(expected.path, text)
		if err != nil {
			t.Fatalf("parse %s: %v", expected.path, err)
		}
		table := parseCurrentUpgradeTable(doc, text)
		if table.DisjointBonusItemID != expected.bonusItem ||
			table.DisjointEquipLevelConst != expected.equipConst ||
			table.DisjointLevelConst != expected.levelConst {
			t.Fatalf("%s disjoint=(%d,%d,%d) want=(%d,%d,%d)", expected.path,
				table.DisjointBonusItemID, table.DisjointEquipLevelConst, table.DisjointLevelConst,
				expected.bonusItem, expected.equipConst, expected.levelConst)
		}
		// rarity 0/1 are absent from the weight table; 2..5 are rare, unique,
		// epic and chronicle at weights 1, 1.2, 1.5 and 1.
		for rarity, want := range map[int]int{0: 0, 1: 0, 2: 9, 3: 10, 4: 13, 5: 9} {
			if got := currentUpgradeDestroyBonusCount(table, 13, rarity, 90); got != want {
				t.Fatalf("%s level=13 rarity=%d count=%d want=%d", expected.path, rarity, got, want)
			}
		}
		// The threshold is strict: at or below [upgrade const for bonus item
		// count] nothing is refunded regardless of rarity.
		if got := currentUpgradeDestroyBonusCount(table, table.DisjointLevelConst, 4, 90); got != 0 {
			t.Fatalf("%s refunded %d at the level constant", expected.path, got)
		}
	}
}
