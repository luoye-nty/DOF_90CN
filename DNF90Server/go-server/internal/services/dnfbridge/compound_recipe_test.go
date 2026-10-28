package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestDecodeCurrentCompoundRecipeRequestUsesCurrentEXELayouts(t *testing.T) {
	request, err := decodeCurrentCompoundRecipeRequest([]byte{0x49, 0, 0, 0, 0, 1, 0, 0, 1})
	if err != nil || request.SourceValue != 73 || request.SourceIsID || request.RequestedCount != 1 {
		t.Fatalf("nine-byte request=%+v err=%v", request, err)
	}
	request, err = decodeCurrentCompoundRecipeRequest([]byte{0xE8, 3, 0, 0, 1, 2, 0})
	if err != nil || request.SourceValue != 1000 || !request.SourceIsID || request.RequestedCount != 2 {
		t.Fatalf("seven-byte request=%+v err=%v", request, err)
	}
	for _, body := range [][]byte{
		{0x49, 0, 0, 0, 0, 1, 0, 1, 1},
		{0x49, 0, 0, 0, 2, 1, 0},
		{0x49, 0, 0, 0, 0, 0, 0},
		make([]byte, 8),
	} {
		if _, err := decodeCurrentCompoundRecipeRequest(body); err == nil {
			t.Fatalf("malformed request accepted: %x", body)
		}
	}
}

func TestParseCurrentCompoundRecipeSeparatesPurchaseMaterialFromCraftInputs(t *testing.T) {
	catalog := mustCurrentCompoundRecipeCatalog(t)
	preserved, err := parseCurrentCompoundRecipe(catalog, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.MaintainAbility != "normal" || len(preserved.NeedMaterial) != 1 ||
		preserved.NeedMaterial[0].ItemID != 4000 || preserved.NeedMaterial[0].Count != 3 {
		t.Fatalf("preserved recipe=%+v", preserved)
	}
	for _, input := range preserved.Inputs {
		if input.ItemID == 4000 {
			t.Fatalf("purchase material leaked into craft inputs: %+v", preserved)
		}
	}
}

func TestResolveCurrentCompoundRecipeIDDirectCatalogDoesNotRequireRecipeScroll(t *testing.T) {
	repositories := mustCurrentCompoundRecipeRepositories(t, 90, map[string]int64{"gold": 5000}, map[string]dnfrepo.ItemStack{
		"0:74": {ItemID: 2000, Count: 1},
		"0:75": {ItemID: 3000, Count: 800},
	})
	recipeID, recipeSlot, err := resolveCurrentCompoundRecipeID(
		context.Background(),
		repositories.Inventory,
		"19",
		currentCompoundRecipeRequest{SourceValue: 1000, SourceIsID: true, RequestedCount: 1},
	)
	if err != nil || recipeID != 1000 || recipeSlot != -1 {
		t.Fatalf("direct production recipe id=%d slot=%d err=%v", recipeID, recipeSlot, err)
	}
}

func TestCommitCurrentCompoundRecipeDirectCatalogConsumesOnlyInputs(t *testing.T) {
	catalog := mustCurrentCompoundRecipeCatalog(t)
	recipe, err := parseCurrentCompoundRecipe(catalog, 1000)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentCompoundRecipeRepositories(t, 90, map[string]int64{"gold": 5000}, map[string]dnfrepo.ItemStack{
		"0:74": {ItemID: 2000, Count: 1},
		"0:75": {ItemID: 3000, Count: 800},
	})
	request := currentCompoundRecipeRequest{SourceValue: 1000, SourceIsID: true, RequestedCount: 1}
	result, err := commitCurrentCompoundRecipe(context.Background(), repositories, "account-1", "19", request, -1, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Consumed) != 2 || len(result.Outputs) != 1 || len(result.Refreshes) != 3 {
		t.Fatalf("direct production result=%+v", result)
	}
	for _, row := range result.Consumed {
		if row.Slot == -1 || row.DeletedCount == 0 {
			t.Fatalf("catalog recipe appeared as consumed inventory row: %+v", result.Consumed)
		}
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:-1"]; exists {
		t.Fatalf("direct production fabricated recipe source slot: %+v", inventory.Slots["0:-1"])
	}
	if output := inventory.Slots["0:74"]; output.ItemID != 2001 || output.Count != 1 {
		t.Fatalf("direct production output=%+v", output)
	}
}

func TestCommitCurrentCompoundRecipeRejectsDirectUseOfPhysicalBlueprint(t *testing.T) {
	catalog := mustCurrentCompoundRecipeCatalog(t)
	recipe, err := parseCurrentCompoundRecipe(catalog, 1000)
	if err != nil {
		t.Fatal(err)
	}
	recipe.CatalogDirect = false
	repositories := mustCurrentCompoundRecipeRepositories(t, 90, map[string]int64{"gold": 5000}, map[string]dnfrepo.ItemStack{
		"0:74": {ItemID: 2000, Count: 1},
		"0:75": {ItemID: 3000, Count: 800},
	})
	request := currentCompoundRecipeRequest{SourceValue: 1000, SourceIsID: true, RequestedCount: 1}
	_, err = commitCurrentCompoundRecipe(context.Background(), repositories, "account-1", "19", request, -1, recipe)
	if !errors.Is(err, errCurrentCompoundRecipeInvalid) {
		t.Fatalf("physical blueprint direct-use error=%v", err)
	}
	inventory, found, loadErr := repositories.Inventory.Load(context.Background(), "19")
	if loadErr != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, loadErr)
	}
	if source := inventory.Slots["0:74"]; source.ItemID != 2000 || source.Count != 1 {
		t.Fatalf("rejected physical blueprint changed source=%+v", source)
	}
}

func TestCommitCurrentCompoundRecipeConsumesRecipePreservesEquipmentAndBuildsRefreshes(t *testing.T) {
	catalog := mustCurrentCompoundRecipeCatalog(t)
	recipe, err := parseCurrentCompoundRecipe(catalog, 1000)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, currentItemListEntryWireSize)
	for index := range raw {
		raw[index] = byte(index)
	}
	source := dnfrepo.ItemStack{
		ItemID:   2000,
		Count:    1,
		RawEntry: raw,
		Extra: map[string]string{
			"reinforce_level": "12",
			"amplify_stat":    "3",
			"forging_level":   "7",
			"enchant_card_id": "9876",
		},
	}
	repositories := mustCurrentCompoundRecipeRepositories(t, 90, map[string]int64{"gold": 5000}, map[string]dnfrepo.ItemStack{
		"0:73": {ItemID: 1000, Count: 1},
		"0:74": source,
		"0:75": {ItemID: 3000, Count: 4399},
	})
	result, err := commitCurrentCompoundRecipe(context.Background(), repositories, "account-1", "19", currentCompoundRecipeRequest{SourceValue: 73, RequestedCount: 1}, 73, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Consumed) != 3 || len(result.Outputs) != 1 || result.Outputs[0].Action != 1 || len(result.Refreshes) != 4 {
		t.Fatalf("compound result=%+v", result)
	}
	refreshed := make(map[int16]bool)
	for _, refresh := range result.Refreshes {
		if refresh.ListType != dnfrepo.MainInventoryListType {
			t.Fatalf("unexpected refresh=%+v", refresh)
		}
		refreshed[refresh.SlotIndex] = true
	}
	for _, slot := range []int16{73, 74, 75} {
		if !refreshed[slot] {
			t.Fatalf("changed slot %d missing from refreshes=%+v", slot, result.Refreshes)
		}
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if recipeStack, found := inventory.Slots["0:73"]; found {
		t.Fatalf("recipe stack was not consumed: %+v", recipeStack)
	}
	output, found := inventory.Slots["0:74"]
	if !found || output.ItemID != 2001 || output.Count != 1 || output.Extra["reinforce_level"] != "12" ||
		output.Extra["amplify_stat"] != "3" || output.Extra["forging_level"] != "7" || output.Extra["enchant_card_id"] != "9876" {
		t.Fatalf("preserved output=%+v found=%t", output, found)
	}
	if material := inventory.Slots["0:75"]; material.ItemID != 3000 || material.Count != 3599 {
		t.Fatalf("input material remaining=%+v want count=3599", material)
	}
	body := buildCurrentCompoundRecipeSuccessBody(result)
	if len(body) != 1+1+3*7+1+1+currentItemListEntryWireSize || body[0] != 1 || body[1] != 3 ||
		binary.LittleEndian.Uint32(body[5:9]) != 1 || binary.LittleEndian.Uint32(body[12:16]) != 1 ||
		binary.LittleEndian.Uint32(body[19:23]) != 800 || body[23] != 1 || body[24] != 1 ||
		binary.LittleEndian.Uint32(body[27:31]) != 2001 {
		t.Fatalf("success body len=%d body=%x", len(body), body)
	}
}

func TestCommitCurrentCompoundRecipeRollsBackRecipeConsumptionWhenInputIsMissing(t *testing.T) {
	catalog := mustCurrentCompoundRecipeCatalog(t)
	recipe, err := parseCurrentCompoundRecipe(catalog, 1000)
	if err != nil {
		t.Fatal(err)
	}
	repositories := mustCurrentCompoundRecipeRepositories(t, 90, map[string]int64{"gold": 5000}, map[string]dnfrepo.ItemStack{
		"0:73": {ItemID: 1000, Count: 1},
		"0:74": {ItemID: 2000, Count: 1},
	})
	_, err = commitCurrentCompoundRecipe(context.Background(), repositories, "account-1", "19", currentCompoundRecipeRequest{SourceValue: 73, RequestedCount: 1}, 73, recipe)
	if !errors.Is(err, errCurrentCompoundRecipeMaterial) {
		t.Fatalf("commit error=%v want material error", err)
	}
	inventory, found, loadErr := repositories.Inventory.Load(context.Background(), "19")
	if loadErr != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, loadErr)
	}
	if stack := inventory.Slots["0:73"]; stack.ItemID != 1000 || stack.Count != 1 {
		t.Fatalf("failed commit consumed recipe: %+v", stack)
	}
	if stack := inventory.Slots["0:74"]; stack.ItemID != 2000 || stack.Count != 1 {
		t.Fatalf("failed commit changed equipment input: %+v", stack)
	}
}

func TestCurrentCompoundRecipeRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current recipe PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := parseCurrentCompoundRecipe(catalog, 10303386)
	if err != nil || preserved.MaintainAbility != "normal" || len(preserved.NeedMaterial) != 1 ||
		preserved.NeedMaterial[0].ItemID != 10158964 || preserved.NeedMaterial[0].Count != 3 {
		t.Fatalf("real preserved recipe=%+v err=%v", preserved, err)
	}
	ring, err := parseCurrentCompoundRecipe(catalog, 10093312)
	if err != nil || len(ring.Inputs) == 0 || len(ring.Outputs) == 0 {
		t.Fatalf("real ring recipe=%+v err=%v", ring, err)
	}
	cloth, err := parseCurrentCompoundRecipe(catalog, 5057)
	if err != nil || len(cloth.Inputs) != 2 || cloth.Inputs[0].ItemID != 3030 || cloth.Inputs[0].Count != 1 ||
		cloth.Inputs[1].ItemID != 0 || cloth.Inputs[1].Count != 50 || len(cloth.Outputs) != 1 ||
		cloth.Outputs[0].ItemID != 3134 || cloth.Outputs[0].Count != 1 {
		t.Fatalf("real production cloth recipe=%+v err=%v", cloth, err)
	}
	t.Logf("real recipe refs=%d weapon_ref=%+v ring_ref=%+v", len(catalog.itemRefs), catalog.itemRefs[10303386], catalog.itemRefs[10093312])
}

func TestRealPVFTownProductionCatalogRecipesResolve(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify current recipe PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	// Current-client production tabs (cloth, leather, organic material,
	// metal and other) project the contiguous PVF catalog 5057..5080.
	for itemID := uint32(5057); itemID <= 5080; itemID++ {
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve production recipe item=%d: %v", itemID, resolveErr)
		}
		if definition.Kind != dungeonDropItemStackable || normalizeDungeonDropStackableType(definition.StackableType) != "[recipe]" {
			t.Fatalf("production item=%d path=%s kind=%s type=%s", itemID, definition.PVFPath, definition.Kind, definition.StackableType)
		}
		text, readErr := archive.ReadText(definition.PVFPath)
		if readErr != nil {
			t.Fatalf("read recipe item=%d path=%s: %v", itemID, definition.PVFPath, readErr)
		}
		document, documentErr := dnfpvf.Parse(definition.PVFPath, text)
		if documentErr != nil {
			t.Fatalf("parse recipe item=%d path=%s: %v", itemID, definition.PVFPath, documentErr)
		}
		stackableTypeValues := document.Ints("stackable type")
		if len(stackableTypeValues) == 0 || stackableTypeValues[0] != 0 {
			t.Fatalf("production recipe item=%d path=%s subtype=%v", itemID, definition.PVFPath, stackableTypeValues)
		}
		checked++
		if _, parseErr := parseCurrentCompoundRecipe(catalog, itemID); parseErr != nil {
			t.Fatalf("recipe item=%d path=%s: %v", itemID, definition.PVFPath, parseErr)
		}
	}
	if checked != 24 {
		t.Fatalf("town production recipe count=%d want=24", checked)
	}
	t.Logf("town production recipe audit definitions=%d", checked)
}

func mustCurrentCompoundRecipeCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := bridgePVFSource{
		"monster/monster.lst":             "",
		"stackable/stackable.lst":         "1000 `preserve_recipe.stk`\n3000 `material.stk`\n4000 `purchase_material.stk`\n",
		"equipment/equipment.lst":         "2000 `source_weapon.equ`\n2001 `output_weapon.equ`\n",
		"stackable/preserve_recipe.stk":   "[stackable type]\n`[recipe]` 0\n[input item]\n2000 1 3000 800\n[/input item]\n[output item]\n2001 1\n[/output item]\n[success rate]\n10000\n[maintain ability]\n`normal`\n[need material]\n4000 3\n[/need material]\n",
		"stackable/material.stk":          "[stackable type]\n`[material]`\n[stack limit]\n1000\n",
		"stackable/purchase_material.stk": "[stackable type]\n`[material]`\n[stack limit]\n1000\n",
		"equipment/source_weapon.equ":     "[equipment type]\n`[weapon]` 1\n[durability]\n40\n",
		"equipment/output_weapon.equ":     "[equipment type]\n`[weapon]` 1\n[durability]\n40\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func mustCurrentCompoundRecipeRepositories(t *testing.T, level int, stats map[string]int64, slots map[string]dnfrepo.ItemStack) dnfrepo.Group {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Level: level, Stats: stats}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	return repositories
}
