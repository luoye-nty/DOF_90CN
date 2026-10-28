package dnfbridge

import "testing"

func testRaidRewardPool() antonRewardPVFPool {
	return antonRewardPVFPool{
		PhaseIndex: 0,
		Kind:       "party_card",
		Entries: []antonRewardPVFPoolEntry{
			{DefinitionItemID: 10, Weight: 100},
			{DefinitionItemID: 20, Weight: 100},
		},
		TotalWeight: 200,
	}
}

func TestRaidRewardOverrideEmptyTableKeepsPVFWeights(t *testing.T) {
	catalog := &antonRewardPVFCatalog{raid: "anton", overrides: newRaidRewardOverrides()}
	pool := testRaidRewardPool()
	if entry, total := catalog.selectPoolEntry(pool, 0); total != 200 || entry.DefinitionItemID != 10 {
		t.Fatalf("default selection at 0 = %+v total=%d", entry, total)
	}
	if entry, total := catalog.selectPoolEntry(pool, 150); total != 200 || entry.DefinitionItemID != 20 {
		t.Fatalf("default selection at 150 = %+v total=%d", entry, total)
	}
}

func TestRaidRewardOverridePoolPercentChangesSelection(t *testing.T) {
	catalog := &antonRewardPVFCatalog{raid: "anton", overrides: newRaidRewardOverrides()}
	pool := testRaidRewardPool()
	catalog.overrides.replace(map[string]uint32{
		"P|anton|0|party_card|10|0": 0,
	}, nil)
	entry, total := catalog.selectPoolEntry(pool, 0)
	if total != 100 || entry.DefinitionItemID != 20 {
		t.Fatalf("zeroed first entry selection = %+v total=%d", entry, total)
	}

	catalog.overrides.replace(map[string]uint32{
		"P|anton|0|party_card|10|0": 1000,
		"P|anton|0|party_card|20|1": 100,
	}, nil)
	entry, total = catalog.selectPoolEntry(pool, 0)
	if total != 1100 || entry.DefinitionItemID != 10 {
		t.Fatalf("weighted first entry selection = %+v total=%d", entry, total)
	}
	entry, total = catalog.selectPoolEntry(pool, 1050)
	if total != 1100 || entry.DefinitionItemID != 20 {
		t.Fatalf("weighted second entry selection = %+v total=%d", entry, total)
	}
}

func TestRaidRewardOverrideWildcardPoolKeyAppliesToDuplicateDefinition(t *testing.T) {
	catalog := &antonRewardPVFCatalog{raid: "luke", overrides: newRaidRewardOverrides()}
	pool := testRaidRewardPool()
	pool.Entries = append(pool.Entries, antonRewardPVFPoolEntry{DefinitionItemID: 10, Weight: 100})
	pool.TotalWeight = 300
	catalog.overrides.replace(map[string]uint32{
		"P|luke|0|party_card|10": 0,
	}, nil)
	entry, total := catalog.selectPoolEntry(pool, 0)
	if total != 100 || entry.DefinitionItemID != 20 {
		t.Fatalf("wildcard selection = %+v total=%d", entry, total)
	}
}

func TestRaidRewardOverrideItemPercentChangesDefinitionSelection(t *testing.T) {
	catalog := &antonRewardPVFCatalog{raid: "anton", overrides: newRaidRewardOverrides()}
	definition := antonRewardPVFDefinition{
		DefinitionItemID: 10,
		Entries: []antonRewardPVFEntry{
			{ItemID: 100, Weight: 1, Count: 1},
			{ItemID: 200, Weight: 1, Count: 1},
		},
		TotalWeight: 2,
	}
	entry, err := catalog.selectDefinitionEntry(definition, 0)
	if err != nil || entry.ItemID != 100 {
		t.Fatalf("default definition selection = %+v err=%v", entry, err)
	}
	catalog.overrides.replace(nil, map[string]uint32{
		"D|10|100": 0,
	})
	entry, err = catalog.selectDefinitionEntry(definition, 0)
	if err != nil || entry.ItemID != 200 {
		t.Fatalf("wildcard item override selection = %+v err=%v", entry, err)
	}
}

func TestRaidRewardOverrideRejectsAllZeroPool(t *testing.T) {
	catalog := &antonRewardPVFCatalog{
		raid:        "anton",
		overrides:   newRaidRewardOverrides(),
		pools:       map[antonRewardPVFPoolKey]antonRewardPVFPool{},
		poolOrder:   nil,
		definitions: map[uint32]antonRewardPVFDefinition{},
	}
	key := antonRewardPVFPoolKey{phaseIndex: 0, kind: "party_card"}
	catalog.pools[key] = testRaidRewardPool()
	catalog.poolOrder = []antonRewardPVFPoolKey{key}
	err := catalog.validateRaidRewardOverrides(map[string]uint32{
		"P|anton|0|party_card|10|0": 0,
		"P|anton|0|party_card|20|1": 0,
	}, nil)
	if err == nil {
		t.Fatal("all-zero pool override must be rejected")
	}
}

func TestRaidRewardOverrideSnapshotIsACopy(t *testing.T) {
	table := newRaidRewardOverrides()
	table.replace(map[string]uint32{"P|anton|0|party_card|10|0": 50}, map[string]uint32{"D|10|100": 200})
	weights, items := table.snapshot()
	weights["P|anton|0|party_card|10|0"] = 0
	items["D|10|100"] = 0
	againWeights, againItems := table.snapshot()
	if againWeights["P|anton|0|party_card|10|0"] != 50 || againItems["D|10|100"] != 200 {
		t.Fatalf("snapshot leaked mutable maps: %+v %+v", againWeights, againItems)
	}
}
