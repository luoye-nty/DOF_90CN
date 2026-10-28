package inventory

import (
	"context"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const (
	testResetTargetSlot   int16 = 9
	testResetMaterialSlot int16 = 20
)

// testResetResolution mirrors the current randomizedoptionoverall2.etc
// [postfix grade modification] rows for a rare (rarity 2) target.
func testResetResolution() alignedcmd.RandomOptionResolution {
	resolution := testRandomOptionResolution()
	resolution.ResetGrades = []alignedcmd.RandomOptionGradeBand{
		{Grade: 1, MinPct: 0, MaxPct: 14, Weight: 330},
		{Grade: 2, MinPct: 14, MaxPct: 37, Weight: 520},
		{Grade: 3, MinPct: 37, MaxPct: 47, Weight: 100},
		{Grade: 4, MinPct: 47, MaxPct: 56, Weight: 50},
	}
	return resolution
}

func testResetCommand() Command {
	return Command{
		SelectedCharacterID: 77,
		Operation:           "reset_random_option",
		TargetSlotIndex:     testResetTargetSlot,
		MaterialSlotIndex:   testResetMaterialSlot,
		RandomOptionIndex:   0,
	}
}

func saveResetFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, options []RandomOptionValue, materialItemID int64, materialCount int64) {
	t.Helper()
	target := dnfrepo.ItemStack{ItemID: 700, Count: 1}
	if len(options) > 0 {
		setRandomOptions(&target, testResetTargetSlot, options)
	} else {
		target.RawEntry = make([]byte, currentItemListEntrySize)
		target.Extra = map[string]string{"item_kind": "equipment"}
	}
	material := dnfrepo.ItemStack{ItemID: materialItemID, Count: materialCount}
	material.Extra = map[string]string{"item_kind": "stackable"}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, testResetTargetSlot):   target,
			slotKey(listTypeMain, testResetMaterialSlot): material,
		},
	})
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{}})
}

func loadResetMaterialCount(t *testing.T, ctx context.Context, repos dnfrepo.Group) (int64, bool) {
	t.Helper()
	record, ok, err := repos.Inventory.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("Load inventory = %+v ok=%t err=%v", record, ok, err)
	}
	stack, found := record.Slots[slotKey(listTypeMain, testResetMaterialSlot)]
	if !found {
		return 0, false
	}
	return stack.Count, true
}

func TestOwnerResetRandomOptionRewritesGradePercentAndConsumesBox(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveResetFixture(t, ctx, repos, []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}}, 15, 3)

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	resolution := testResetResolution()
	result, err := owner.ResetRandomOption(ctx, testResetCommand(), staticRandomOptionResolver(resolution))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Failure != RandomOptionFailureNone {
		t.Fatalf("result = %+v", result)
	}
	if result.OptionType != 72 || result.OldPercent != 5 {
		t.Fatalf("selected option = type=%d old=%d", result.OptionType, result.OldPercent)
	}
	if !result.MaterialConsumed || result.MaterialItemID != 15 || result.UpdatedMaterial.Count != 2 {
		t.Fatalf("material = %+v", result)
	}
	raw := result.UpdatedTarget.RawEntry
	if len(raw) != currentItemListEntrySize {
		t.Fatalf("row length = %d", len(raw))
	}
	if raw[randomOptionCountOffset] != 1 || raw[randomOptionTypeOffset] != 72 {
		t.Fatalf("row header = % X", raw[randomOptionCountOffset:randomOptionCandidateOffset])
	}
	if raw[randomOptionValue1Offset] != result.NewPercent || raw[randomOptionValue2Offset] != result.NewPercent {
		t.Fatalf("row values = %d/%d want %d", raw[randomOptionValue1Offset], raw[randomOptionValue2Offset], result.NewPercent)
	}
	window, ok := resetGradeWindow(resolution.ResetGrades, result.Grade)
	if !ok {
		t.Fatalf("grade %d missing from resolution", result.Grade)
	}
	if int64(result.NewPercent) < window.MinPct || int64(result.NewPercent) > window.MaxPct {
		t.Fatalf("percent %d outside grade %d window %d..%d", result.NewPercent, result.Grade, window.MinPct, window.MaxPct)
	}
	if result.NewPercent > byte(currentRandomOptionResetMaxPercent) {
		t.Fatalf("percent %d above MAX_OPTION_VALUE", result.NewPercent)
	}
	if got := result.UpdatedTarget.Extra["random_option_1_value1"]; got != itoaByte(result.NewPercent) {
		t.Fatalf("extra value1 = %q want %d", got, result.NewPercent)
	}
	if got := result.UpdatedTarget.Extra["random_option_1_type"]; got != "72" {
		t.Fatalf("extra type = %q", got)
	}
	if count, ok := loadResetMaterialCount(t, ctx, repos); !ok || count != 2 {
		t.Fatalf("persisted material count = %d ok=%t, want 2", count, ok)
	}
}

func TestOwnerResetRandomOptionDropsEmptiedBoxSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveResetFixture(t, ctx, repos, []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}}, 15, 1)

	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.ResetRandomOption(ctx, testResetCommand(), staticRandomOptionResolver(testResetResolution()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.MaterialSlotEmpty {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := loadResetMaterialCount(t, ctx, repos); ok {
		t.Fatal("emptied material slot is still persisted")
	}
}

func TestOwnerResetRandomOptionRejectsWithoutConsuming(t *testing.T) {
	tests := []struct {
		name         string
		options      []RandomOptionValue
		materialID   int64
		materialQty  int64
		resolution   alignedcmd.RandomOptionResolution
		optionIndex  byte
		wantFailure  RandomOptionFailure
		wantErrorNum byte
	}{
		{
			name:         "no seal option in the selected slot",
			options:      nil,
			materialID:   15,
			materialQty:  3,
			resolution:   testResetResolution(),
			wantFailure:  RandomOptionFailureNoSealOption,
			wantErrorNum: randomOptionResetErrorNoSealOption,
		},
		{
			name:         "grade fixed by the PVF exemption list",
			options:      []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}},
			materialID:   15,
			materialQty:  3,
			resolution:   resetResolutionWithBlockedType(72),
			wantFailure:  RandomOptionFailureGradeFixed,
			wantErrorNum: randomOptionResetErrorGradeFixed,
		},
		{
			name:         "material slot holds an unrelated stackable",
			options:      []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}},
			materialID:   10015553,
			materialQty:  3,
			resolution:   testResetResolution(),
			wantFailure:  RandomOptionFailureMaterialMissing,
			wantErrorNum: randomOptionResetErrorInvalidMaterial,
		},
		{
			name:         "item rarity has no grade windows",
			options:      []RandomOptionValue{{Type: 72, Value1: 5, Value2: 39}},
			materialID:   15,
			materialQty:  3,
			resolution:   resetResolutionWithoutGrades(),
			wantFailure:  RandomOptionFailureGradesUnavailable,
			wantErrorNum: randomOptionResetErrorGradeFixed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repos := dnfrepomemory.NewMemoryGroup()
			saveResetFixture(t, ctx, repos, test.options, test.materialID, test.materialQty)
			owner, err := NewOwner(repos)
			if err != nil {
				t.Fatal(err)
			}
			cmd := testResetCommand()
			cmd.RandomOptionIndex = test.optionIndex
			result, err := owner.ResetRandomOption(ctx, cmd, staticRandomOptionResolver(test.resolution))
			if err != nil {
				t.Fatal(err)
			}
			if result.Success || result.Failure != test.wantFailure || result.ErrorCode != test.wantErrorNum {
				t.Fatalf("result = %+v want failure=%s code=%d", result, test.wantFailure, test.wantErrorNum)
			}
			if result.MaterialConsumed {
				t.Fatalf("rejected reset consumed material: %+v", result)
			}
			if count, ok := loadResetMaterialCount(t, ctx, repos); !ok || count != test.materialQty {
				t.Fatalf("material count = %d ok=%t, want untouched %d", count, ok, test.materialQty)
			}
		})
	}
}

func resetResolutionWithBlockedType(optionType byte) alignedcmd.RandomOptionResolution {
	resolution := testResetResolution()
	resolution.GradeUnmodifiable = map[byte]struct{}{optionType: {}}
	return resolution
}

func resetResolutionWithoutGrades() alignedcmd.RandomOptionResolution {
	resolution := testResetResolution()
	resolution.ResetGrades = nil
	return resolution
}

func resetGradeWindow(bands []alignedcmd.RandomOptionGradeBand, grade int64) (alignedcmd.RandomOptionGradeBand, bool) {
	for _, band := range bands {
		if band.Grade == grade {
			return band, true
		}
	}
	return alignedcmd.RandomOptionGradeBand{}, false
}

func itoaByte(value byte) string {
	return strconv.Itoa(int(value))
}
