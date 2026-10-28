package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func newUpgradeProtectionTestCatalog(t *testing.T) (*pvfDungeonDropCatalog, dungeonDropCatalogTestSource) {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                  "",
		"equipment/equipment.lst":              "700 `weapon/test.equ`\n",
		"stackable/stackable.lst":              "7804 `ticket/reinforce.stk`\n10004511 `ticket/amplify.stk`\n10007840 `ticket/super_reinforce.stk`\n10007841 `ticket/super_amplify.stk`\n9000 `ticket/plain.stk`\n9001 `ticket/bad_super.stk`\n",
		"equipment/weapon/test.equ":            "[name] `test`\n[equipment type] `[weapon]`\n",
		"stackable/ticket/reinforce.stk":       "[stackable type] `[material]`\n[action type] `[protect equipment]`\n",
		"stackable/ticket/amplify.stk":         "[stackable type] `[material]`\n[action type] `[amplify protect equipment]`\n",
		"stackable/ticket/super_reinforce.stk": "[stackable type] `[material]`\n[action type] `[new protect equipment]` 10\n",
		"stackable/ticket/super_amplify.stk":   "[stackable type] `[material]`\n[action type] `[new amplify protect equipment]` 7\n",
		"stackable/ticket/plain.stk":           "[stackable type] `[material]`\n[action type] `[unrelated]`\n",
		"stackable/ticket/bad_super.stk":       "[stackable type] `[material]`\n[action type] `[new protect equipment]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, source
}

func TestResolveCurrentUpgradeProtectionMetadata(t *testing.T) {
	catalog, source := newUpgradeProtectionTestCatalog(t)
	tests := []struct {
		itemID int64
		mode   string
		retain int
	}{
		{itemID: 7804, mode: "reinforce", retain: 0},
		{itemID: 10004511, mode: "amplify", retain: 0},
		{itemID: 10007840, mode: "reinforce", retain: 10},
		{itemID: 10007841, mode: "amplify", retain: 7},
	}
	for _, tc := range tests {
		got, err := resolveCurrentUpgradeProtectionMetadata(catalog, source, tc.itemID)
		if err != nil {
			t.Fatalf("resolve item %d: %v", tc.itemID, err)
		}
		if got.TicketMode != tc.mode || got.FailureRetainLevel != tc.retain || got.TicketPVFPath == "" {
			t.Fatalf("resolve item %d = %+v", tc.itemID, got)
		}
	}

	plain, err := resolveCurrentUpgradeProtectionMetadata(catalog, source, 9000)
	if err != nil || plain.TicketMode != "" {
		t.Fatalf("plain resolution=%+v err=%v", plain, err)
	}
	if _, err := resolveCurrentUpgradeProtectionMetadata(catalog, source, 9001); err == nil {
		t.Fatal("malformed super protection ticket was accepted")
	}
}

func TestRealPVFUpgradeProtectionFamilies(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to inspect protection tickets")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open PVF: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, tc := range []struct {
		itemID int64
		mode   string
		retain int
	}{
		{7804, "reinforce", 0},
		{10004511, "amplify", 0},
		{10007840, "reinforce", 10},
		{10007841, "amplify", 7},
	} {
		got, resolveErr := resolveCurrentUpgradeProtectionMetadata(catalog, archive, tc.itemID)
		if resolveErr != nil {
			t.Fatalf("resolve real item %d: %v", tc.itemID, resolveErr)
		}
		if got.TicketMode != tc.mode || got.FailureRetainLevel != tc.retain {
			t.Fatalf("real item %d = %+v", tc.itemID, got)
		}
	}
}
