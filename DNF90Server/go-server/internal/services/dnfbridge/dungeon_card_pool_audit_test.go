package dnfbridge

import (
	"os"
	"testing"
	"time"

	dnfdrops "longheng.io/server/internal/modules/dnf/drop"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentDungeonRandomClearCardRewardNeverFallsBackToStackable(t *testing.T) {
	const (
		dungeonLevel = 84
		equipmentID  = uint32(1001)
		projectileID = uint32(2001)
	)
	rule := dnfdrops.ClearRewardRule{
		EquipmentRate:    dnfdrops.ClearRewardItemRateDenominator,
		GradeUp:          1,
		RarityThresholds: [7]uint32{1, 1, 1, 1, 1, 1, 1},
		RarityScale:      1,
	}
	stackableOnly := &pvfDungeonDropCatalog{
		source:        dungeonDropCatalogTestSource{},
		itemCache:     map[uint32]dungeonDropItemDefinition{},
		genericLoaded: true,
		genericStackables: map[dungeonDropGradeRarityKey][]uint32{
			{Grade: dungeonLevel, Rarity: 0}: {projectileID},
		},
		genericEquipment: map[dungeonDropGradeRarityKey][]dungeonDropWeightedItem{},
	}
	bundle, err := currentDungeonRandomCardRewardBundle(
		newCurrentDungeonDropLCG(1),
		currentDungeonGoldReference{Level: dungeonLevel, Base: 1_000},
		currentDungeonCardGoldTerms{PartyMembers: 1, PartyRate: 1, WeightedKills: 40},
		stackableOnly,
		dungeonLevel,
		rule,
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Items) != 0 || bundle.Gold <= 0 {
		t.Fatalf("stackable fallback leaked into ordinary card: %+v", bundle)
	}

	withEquipment := &pvfDungeonDropCatalog{
		source: dungeonDropCatalogTestSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{
			equipmentID: {
				ItemID: equipmentID, Kind: dungeonDropItemEquipment,
				PVFPath: "equipment/test.equ", Grade: dungeonLevel,
			},
		},
		genericLoaded: true,
		genericStackables: map[dungeonDropGradeRarityKey][]uint32{
			{Grade: dungeonLevel, Rarity: 0}: {projectileID},
		},
		genericEquipment: map[dungeonDropGradeRarityKey][]dungeonDropWeightedItem{
			{Grade: dungeonLevel, Rarity: 0}: {{ItemID: equipmentID, Weight: 1}},
		},
	}
	bundle, err = currentDungeonRandomCardRewardBundle(
		newCurrentDungeonDropLCG(1),
		currentDungeonGoldReference{Level: dungeonLevel, Base: 1_000},
		currentDungeonCardGoldTerms{PartyMembers: 1, PartyRate: 1, WeightedKills: 40},
		withEquipment,
		dungeonLevel,
		rule,
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Items) != 1 || bundle.Items[0].ItemID != int64(equipmentID) || bundle.Items[0].Stackable {
		t.Fatalf("equipment clear-card reward=%+v", bundle)
	}
}

func TestCurrentDungeonClearCardEquipmentRarityUsesPVFThresholds(t *testing.T) {
	thresholds := [7]uint32{50, 80, 99, 100, 101, 102, 103}
	tests := []struct {
		roll uint32
		want int
	}{{0, 0}, {49, 0}, {50, 1}, {79, 1}, {80, 2}, {102, 6}, {103, 6}}
	for _, test := range tests {
		if got := currentDungeonClearCardEquipmentRarity(thresholds, test.roll); got != test.want {
			t.Fatalf("roll=%d rarity=%d want=%d", test.roll, got, test.want)
		}
	}
}

func TestRealScriptPVFDungeonCardEquipmentPoolAudit(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit real Script.pvf clear-card pools")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := dnfdrops.LoadClearRewardRules(archive)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := rules.Resolve(84, 0, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	// [gold card cost table] is the only source of the commission op35 prints
	// under 黄金卡牌 N (the 86JP reference's GetPaidCardCost reads the same
	// rows). Resolve already rejects a missing row, so a zero here would mean
	// the table collapsed to a free card and every gold card would render 0 金币.
	if rule.PaidCardCost == 0 {
		t.Fatalf("real level-84 gold card commission resolved to zero: rule=%+v", rule)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ensureGenericPools(); err != nil {
		t.Fatal(err)
	}
	if candidates := catalog.genericEquipmentCandidates(84, rule.GradeDown, rule.GradeUp, 0); len(candidates) == 0 {
		t.Fatal("real level-84 clear-card equipment pool is empty")
	}
	for seed := uint32(1); seed <= 256; seed++ {
		item, found, rewardErr := currentDungeonRandomClearCardEquipment(
			newCurrentDungeonDropLCG(seed), catalog, 84, rule, time.Now().UTC(),
		)
		if rewardErr != nil {
			t.Fatal(rewardErr)
		}
		if !found {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(uint32(item.ItemID))
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if definition.Kind != dungeonDropItemEquipment || item.Stackable {
			t.Fatalf("seed=%d ordinary card selected non-equipment: item=%+v definition=%+v", seed, item, definition)
		}
	}
}
