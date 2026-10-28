package dnfbridge

import (
	"errors"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func TestCurrentBoosterSelectionCatalogAllowsUnselectedEmptyCategory(t *testing.T) {
	document, err := dnfpvf.Parse("stackable/test_select.stk", `
[booster select category]
1 0
[stackable]
10095199 1
[/booster select category]
[booster select category]
1 4
[/booster select category]
`)
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}
	candidates, categories, err := parseCurrentBoosterSelectionCatalog(document)
	if err != nil {
		t.Fatalf("parseCurrentBoosterSelectionCatalog error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].ItemID != 10095199 {
		t.Fatalf("candidates = %+v", candidates)
	}
	if populated := categories[1]; len(populated) != 1 || populated[0].ItemID != 10095199 {
		t.Fatalf("populated category = %+v", populated)
	}
	emptyCategory := uint16(1 | 4<<8)
	if empty, exists := categories[emptyCategory]; !exists || len(empty) != 0 {
		t.Fatalf("empty placeholder exists=%t rows=%+v", exists, empty)
	}

	definition := currentBoosterDefinition{
		SelectionRequired: 1,
		SelectionCategory: categories,
	}
	selected, err := currentBoosterSelectedCandidates(definition, currentBoosterOpenRequest{
		Kind:             currentBoosterRequestSelection,
		SelectionContext: 1,
		Selections: []currentBoosterSelectionRequest{
			{ItemID: 10095199},
		},
	})
	if err != nil || len(selected) != 1 || selected[0].ItemID != 10095199 {
		t.Fatalf("populated selection = %+v error=%v", selected, err)
	}
	_, err = currentBoosterSelectedCandidates(definition, currentBoosterOpenRequest{
		Kind:             currentBoosterRequestSelection,
		SelectionContext: emptyCategory,
		Selections: []currentBoosterSelectionRequest{
			{ItemID: 10095199},
		},
	})
	if !errors.Is(err, errCurrentBoosterSelectionInvalid) {
		t.Fatalf("empty category error = %v, want %v", err, errCurrentBoosterSelectionInvalid)
	}
}

func TestCurrentBoosterSelectionCatalogIgnoresHistoricalCategoryMetadata(t *testing.T) {
	document, err := dnfpvf.Parse("stackable/historical_select.stk", `
[booster select category]
0 0
[default select]
2 3330 2749982
[booster equipment upgrade]
12
[booster equipment grade]
4
[booster equipment separate]
3
[random probability]
1 20 2 20
[stackable]
3330 10 2749982 10
[equipment]
2747675 1 2747676 1 2747675 1
[/booster select category]
[booster select category]
1 0
[stackable]
3330 20
[/booster select category]
[booster select category]
7 2
[/booster select category]
[booster select category]
7 2
[/booster select category]
[/booster select category]
[booster select category]
8 0
[booster category num]
1 9
[booster selection num]
1
[cera]
34 1
[booster select category]
9 0
[avatar]
101550666 1 4 -1
[/booster select category]
[booster select category]
10 0
[stackble]
490004325 1
[/booster select category]
`)
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}
	candidates, categories, err := parseCurrentBoosterSelectionCatalog(document)
	if err != nil {
		t.Fatalf("parseCurrentBoosterSelectionCatalog error = %v", err)
	}
	if len(candidates) != 7 {
		t.Fatalf("candidates = %+v", candidates)
	}
	for index, itemID := range []uint32{3330, 2749982, 2747675, 2747676, 34, 101550666, 490004325} {
		if candidates[index].ItemID != itemID {
			t.Fatalf("candidate[%d] = %+v, want item=%d", index, candidates[index], itemID)
		}
	}
	if category := categories[0]; len(category) != 4 {
		t.Fatalf("category 0 = %+v", category)
	}
	if category := categories[1]; len(category) != 1 || category[0].ItemID != 3330 || category[0].Count != 20 {
		t.Fatalf("category 1 = %+v", category)
	}
	emptyID := uint16(7 | 2<<8)
	if empty, exists := categories[emptyID]; !exists || len(empty) != 0 {
		t.Fatalf("repeated empty category exists=%t rows=%+v", exists, empty)
	}
	if category := categories[8]; len(category) != 1 || category[0].ItemID != 34 {
		t.Fatalf("EOF-closed cera category = %+v", category)
	}
	if category := categories[9]; len(category) != 1 || category[0].CategoryKind != 4 || category[0].Option != 0 {
		t.Fatalf("negative-default avatar category = %+v", category)
	}
	if category := categories[10]; len(category) != 1 || category[0].ItemID != 490004325 {
		t.Fatalf("historical stackble category = %+v", category)
	}
}
