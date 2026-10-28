package dnfbridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type vendingMachinePVFTestSource map[string]string

func (s vendingMachinePVFTestSource) ReadText(relativePath string) (string, error) {
	text, ok := s[relativePath]
	if !ok {
		return "", os.ErrNotExist
	}
	return text, nil
}

func TestCurrentVendingMachineCatalogParsesRelativeWeightsAndDummy(t *testing.T) {
	source := vendingMachinePVFTestSource{
		currentVendingMachineListPVFPath: "1 `normal.vm` 3 `high.vm` 5 `dummy.vm`",
		"etc/normal.vm": `[item group]
[group num]
1
[material]
7454 1
[output]
10007367 20 1 0
7451 0 1 1
7463 100 4 0
[/output]
[/item group]
[item group]
[group num]
2
[material]
7454 2
[output]
31 7 1 0
[/output]
[/item group]`,
		"etc/high.vm": `[item group]
[group num]
1
[material]
2749933 1
[output]
2749957 1 1 0
7464 2500000 10 0
[/output]
[/item group]`,
		"etc/dummy.vm": "[item group]\n[/item group]",
	}
	catalog, err := newCurrentVendingMachineCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.MachineCount(); got != 2 {
		t.Fatalf("machine count=%d want=2 (empty dummy must not be requestable)", got)
	}
	normal, err := catalog.Resolve(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if normal.MaterialItemID != 7454 || normal.MaterialCount != 1 || normal.TotalWeight != 120 || len(normal.Outputs) != 3 {
		t.Fatalf("normal group=%+v", normal)
	}
	if normal.Outputs[1].Weight != 0 || normal.Outputs[1].JackpotFlag != 1 {
		t.Fatalf("zero-weight/opaque row=%+v", normal.Outputs[1])
	}
	// Resolve returns a copy so a request path cannot mutate catalog authority.
	normal.Outputs[0].ItemID = 1
	again, err := catalog.Resolve(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if again.Outputs[0].ItemID != 10007367 {
		t.Fatalf("catalog output mutated through Resolve: %+v", again.Outputs[0])
	}
	if _, err := catalog.Resolve(5, 1); !errors.Is(err, errCurrentVendingMachineGroupUnavailable) {
		t.Fatalf("dummy resolve error=%v", err)
	}
	if _, err := catalog.Resolve(1, 99); !errors.Is(err, errCurrentVendingMachineGroupUnavailable) {
		t.Fatalf("missing group error=%v", err)
	}
}

func TestCurrentVendingMachineCatalogRejectsDuplicateMachineAndGroup(t *testing.T) {
	validGroup := `[item group]
[group num]
1
[material]
7454 1
[output]
31 1 1 0
[/output]
[/item group]`
	t.Run("machine", func(t *testing.T) {
		_, err := newCurrentVendingMachineCatalog(vendingMachinePVFTestSource{
			currentVendingMachineListPVFPath: "1 `a.vm` 1 `b.vm`",
			"etc/a.vm":                       validGroup,
			"etc/b.vm":                       validGroup,
		})
		if !errors.Is(err, errCurrentVendingMachineDefinitionInvalid) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("group", func(t *testing.T) {
		_, err := newCurrentVendingMachineCatalog(vendingMachinePVFTestSource{
			currentVendingMachineListPVFPath: "1 `a.vm`",
			"etc/a.vm":                       validGroup + "\n" + validGroup,
		})
		if !errors.Is(err, errCurrentVendingMachineDefinitionInvalid) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestCurrentVendingMachineCatalogRejectsMalformedAuthority(t *testing.T) {
	tests := []struct {
		name       string
		definition string
	}{
		{name: "group id", definition: vendingMachineDefinitionForTest("0", "7454 1", "31 1 1 0")},
		{name: "material id", definition: vendingMachineDefinitionForTest("1", "0 1", "31 1 1 0")},
		{name: "material count", definition: vendingMachineDefinitionForTest("1", "7454 0", "31 1 1 0")},
		{name: "output id", definition: vendingMachineDefinitionForTest("1", "7454 1", "0 1 1 0")},
		{name: "negative weight", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 -1 1 0")},
		{name: "zero count", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 1 0 0")},
		{name: "flag", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 1 1 2")},
		{name: "no positive weight", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 0 1 0")},
		{name: "output arity", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 1 1")},
		{name: "non integer", definition: vendingMachineDefinitionForTest("1", "7454 1", "31 1.5 1 0")},
		{name: "partial dummy", definition: "[item group]\n[group num]\n1\n[/item group]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newCurrentVendingMachineCatalog(vendingMachinePVFTestSource{
				currentVendingMachineListPVFPath: "1 `bad.vm`",
				"etc/bad.vm":                     test.definition,
			})
			if !errors.Is(err, errCurrentVendingMachineDefinitionInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentVendingMachineCatalogFailsClosedOnSourceErrors(t *testing.T) {
	if _, err := newCurrentVendingMachineCatalog(nil); !errors.Is(err, errCurrentVendingMachineCatalogUnavailable) {
		t.Fatalf("nil source error=%v", err)
	}
	_, err := newCurrentVendingMachineCatalog(vendingMachinePVFTestSource{
		currentVendingMachineListPVFPath: "1 `missing.vm`",
	})
	if !errors.Is(err, errCurrentVendingMachineCatalogUnavailable) {
		t.Fatalf("missing definition error=%v", err)
	}
	var catalog *currentVendingMachineCatalog
	if _, err := catalog.Resolve(1, 1); !errors.Is(err, errCurrentVendingMachineCatalogUnavailable) {
		t.Fatalf("nil catalog resolve error=%v", err)
	}
}

func vendingMachineDefinitionForTest(groupNum, material, output string) string {
	return "[item group]\n[group num]\n" + groupNum + "\n[material]\n" + material + "\n[output]\n" + output + "\n[/output]\n[/item group]"
}

func TestCurrentVendingMachineCatalogRealScriptPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
		if _, err := os.Stat(pvfPath); err != nil {
			t.Skipf("real Script.pvf unavailable: %v", err)
		}
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	catalog, err := newCurrentVendingMachineCatalog(archive)
	if err != nil {
		t.Fatalf("load real vending-machine catalog: %v", err)
	}

	normal, err := catalog.Resolve(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if normal.MaterialItemID != 7454 || normal.MaterialCount != 1 {
		t.Fatalf("machine 1 material=%d x%d", normal.MaterialItemID, normal.MaterialCount)
	}
	if !currentVendingMachineOutputMatches(normal, 10007367, 20000, 1) {
		t.Fatalf("machine 1 does not contain Lucky Hammer output: %+v", normal.Outputs)
	}
	assertCurrentVendingMachineRelativeTotal(t, normal)

	high, err := catalog.Resolve(3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if high.MaterialItemID != 2749933 || high.MaterialCount != 1 {
		t.Fatalf("machine 3 material=%d x%d", high.MaterialItemID, high.MaterialCount)
	}
	if !currentVendingMachineOutputMatches(high, 2749957, 1, 1) {
		t.Fatalf("machine 3 does not contain its weight-1 high-prize output: %+v", high.Outputs)
	}
	assertCurrentVendingMachineRelativeTotal(t, high)

	if _, err := catalog.Resolve(5, 1); !errors.Is(err, errCurrentVendingMachineGroupUnavailable) {
		t.Fatalf("real dummy machine is requestable: %v", err)
	}

	itemCatalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("load real item catalog: %v", err)
	}
	for _, group := range []currentVendingMachineGroup{normal, high} {
		for _, output := range group.Outputs {
			if output.Weight == 0 {
				continue
			}
			definition, resolveErr := itemCatalog.ResolveItem(output.ItemID)
			if resolveErr != nil {
				t.Fatalf("machine=%d unresolved output=%d: %v", group.MachineID, output.ItemID, resolveErr)
			}
			if isCurrentCeraShopCreatureItem(definition) || isCurrentCeraShopPetConsumable(definition) {
				t.Fatalf("machine=%d output=%d resolves outside the op215 main-inventory row contract", group.MachineID, output.ItemID)
			}
			candidate := dnfrepo.InventoryRecord{CharacterID: "smoke", Slots: make(map[string]dnfrepo.ItemStack)}
			touched, grantErr := grantCurrentCeraShopProduct(&candidate, definition, output.Count, 24)
			if grantErr != nil {
				t.Fatalf("machine=%d output=%d count=%d cannot be projected: %v", group.MachineID, output.ItemID, output.Count, grantErr)
			}
			rows := make([]currentVendingMachineRewardRow, 0, len(touched))
			for _, slot := range touched {
				if slot > 32767 {
					t.Fatalf("machine=%d output=%d projected slot out of wire range=%d", group.MachineID, output.ItemID, slot)
				}
				signedSlot := int16(slot)
				stack, found := candidate.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, signedSlot)]
				if !found {
					t.Fatalf("machine=%d output=%d missing projected slot=%d", group.MachineID, output.ItemID, slot)
				}
				rows = append(rows, currentVendingMachineRewardRow{
					ListType: dnfrepo.MainInventoryListType,
					Entry:    currentItemListEntryFromStack(dnfrepo.MainInventoryListType, signedSlot, stack),
				})
			}
			if _, buildErr := buildCurrentVendingMachineSuccessBody(currentVendingMachineSuccess{
				MaterialSlot:      140,
				MaterialRemaining: 1,
				RewardItemID:      output.ItemID,
				RewardCount:       output.Count,
				RewardRows:        rows,
			}); buildErr != nil {
				t.Fatalf("machine=%d output=%d count=%d cannot build native op215 rows: %v", group.MachineID, output.ItemID, output.Count, buildErr)
			}
		}
	}
}

func currentVendingMachineOutputMatches(group currentVendingMachineGroup, itemID uint32, weight uint64, count uint32) bool {
	for _, output := range group.Outputs {
		if output.ItemID == itemID && output.Weight == weight && output.Count == count {
			return true
		}
	}
	return false
}

func assertCurrentVendingMachineRelativeTotal(t *testing.T, group currentVendingMachineGroup) {
	t.Helper()
	var sum uint64
	for _, output := range group.Outputs {
		sum += output.Weight
	}
	if sum == 0 || group.TotalWeight != sum {
		t.Fatalf("machine=%d group=%d total=%d summed relative weights=%d", group.MachineID, group.GroupID, group.TotalWeight, sum)
	}
}
