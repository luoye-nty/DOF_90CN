package inventory

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func testRandomOptionStack(itemID int64, raw []byte) dnfrepo.ItemStack {
	return dnfrepo.ItemStack{ItemID: itemID, Count: 1, RawEntry: append([]byte(nil), raw...)}
}

func testRandomOptionResolution() alignedcmd.RandomOptionResolution {
	return alignedcmd.RandomOptionResolution{
		TargetKind:         "equipment",
		TargetPVFPath:      "equipment/character/common/amulet/test.equ",
		TargetEquipmentKey: "amulet",
		TargetMinimumLevel: 80,
		TargetRarity:       2,
		Eligible:           true,
		QuantityWeights:    []alignedcmd.RandomOptionWeightedQuantity{{Quantity: 3, Weight: 1}},
		InitialGroups: [][]alignedcmd.RandomOptionCandidate{
			{{Type: 11, Value1: 21, Value2: 31, Weight: 1}},
			{{Type: 12, Value1: 22, Value2: 32, Weight: 1}},
			{{Type: 13, Value1: 23, Value2: 33, Weight: 1}},
		},
		ModifiedGroups: [][]alignedcmd.RandomOptionCandidate{
			{{Type: 21, Value1: 41, Value2: 51, Weight: 1}},
			{{Type: 22, Value1: 42, Value2: 52, Weight: 1}},
			{{Type: 23, Value1: 43, Value2: 53, Weight: 1}},
		},
		BreakSealGoldCost:    100,
		ModificationGoldCost: 25,
		// 当前 PVF [different weight] 的稀有度 2 行：第 1 条 0~25%、第 2 条 10~30%、
		// 第 3 条 30~55%。词条写的是百分位，所以这几个窗口决定断言。
		InitialSlotWindows: []alignedcmd.RandomOptionPercentWindow{
			{Slot: 1, MinPct: 0, MaxPct: 25},
			{Slot: 2, MinPct: 10, MaxPct: 30},
			{Slot: 3, MinPct: 30, MaxPct: 55},
		},
	}
}

// assertSealPercentile 断言某槽位的两个数值字节是同一个百分位且落在窗口内。
func assertSealPercentile(t *testing.T, raw []byte, slot int, windows []alignedcmd.RandomOptionPercentWindow, label string) {
	t.Helper()
	first := raw[randomOptionValue1Offset+slot-1]
	second := raw[randomOptionValue2Offset+slot-1]
	if first != second {
		t.Fatalf("%s slot %d percent = %d/%d, want one shared percentile", label, slot, first, second)
	}
	minPct, maxPct := int64(0), currentRandomOptionResetMaxPercent
	for _, window := range windows {
		if window.Slot == int64(slot) {
			minPct, maxPct = window.MinPct, window.MaxPct
			break
		}
	}
	if int64(first) < minPct || int64(first) > maxPct {
		t.Fatalf("%s slot %d percent = %d outside window %d..%d", label, slot, first, minPct, maxPct)
	}
}

func staticRandomOptionResolver(resolution alignedcmd.RandomOptionResolution) alignedcmd.RandomOptionResolver {
	return func(targetItemID int64) (alignedcmd.RandomOptionResolution, error) {
		return resolution, nil
	}
}

func saveRandomOptionFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, gold int64, raw []byte, extra map[string]string) {
	t.Helper()
	if raw == nil {
		raw = make([]byte, currentItemListEntrySize)
	}
	stack := testRandomOptionStack(700, raw)
	stack.Extra = extra
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9): stack,
		},
	})
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{CharacterID: "77", Stats: map[string]int64{"gold": gold}})
}

func unsealRandomOptionCommand() Command {
	return NewUnsealRandomOptionCommand(alignedcmd.Request{SelectedCharacterID: 77}, UnsealRandomOptionRequest{TargetSlotIndex: 9, InventoryManagerState: 0xFFFF})
}

func changeRandomOptionCommand(index byte) Command {
	return NewChangeRandomOptionCommand(alignedcmd.Request{SelectedCharacterID: 77}, ChangeRandomOptionRequest{TargetSlotIndex: 9, OptionIndex: index})
}

func TestOwnerUnsealRandomOptionMutatesOnlySealFieldsAndGold(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := currentRawEntryForStack(9, testRandomOptionStack(700, bytes.Repeat([]byte{0xA5}, currentItemListEntrySize)))
	for index := randomOptionCountOffset; index <= randomOptionCandidateOffset+3; index++ {
		raw[index] = 0
	}
	saveRandomOptionFixture(t, ctx, repos, 1000, raw, nil)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.UnsealRandomOption(ctx, unsealRandomOptionCommand(), staticRandomOptionResolver(testRandomOptionResolution()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.Changed || result.GoldCost != 100 || result.UpdatedGold != 900 || len(result.Options) != 3 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	got := loaded.Slots[slotKey(listTypeMain, 9)]
	// 类型仍来自 InitialGroups 的候选（11/12/13），数值是各槽位窗口内的百分位。
	if !bytes.Equal(got.RawEntry[randomOptionTypeOffset:randomOptionTypeOffset+3], []byte{11, 12, 13}) {
		t.Fatalf("seal types = % X", got.RawEntry[randomOptionTypeOffset:randomOptionTypeOffset+3])
	}
	for slot := 1; slot <= 3; slot++ {
		assertSealPercentile(t, got.RawEntry, slot, testRandomOptionResolution().InitialSlotWindows, "unseal")
	}
	if got.RawEntry[randomOptionStateOffset] != 0 || got.RawEntry[randomOptionChangedIndexOffset] != 0xFF ||
		!bytes.Equal(got.RawEntry[randomOptionCandidateOffset:randomOptionCandidateOffset+4], make([]byte, 4)) {
		t.Fatalf("transient fields = % X", got.RawEntry[randomOptionStateOffset:randomOptionCandidateOffset+4])
	}
	if result.PercentileFallback {
		t.Fatal("unseal fell back to the whole 0..100 domain although the PVF window is present")
	}
	for index := range raw {
		if index >= randomOptionCountOffset && index <= randomOptionCandidateOffset+3 {
			continue
		}
		if got.RawEntry[index] != raw[index] {
			t.Fatalf("unrelated raw[%02X] = %02X, want %02X", index, got.RawEntry[index], raw[index])
		}
	}
	character, found, err := repos.Character.Load(ctx, "77")
	if err != nil || !found || character.Stats["gold"] != 900 {
		t.Fatalf("character = %+v found=%t err=%v", character, found, err)
	}

	beforeRepeat := append([]byte(nil), got.RawEntry...)
	repeat, err := owner.UnsealRandomOption(ctx, unsealRandomOptionCommand(), staticRandomOptionResolver(testRandomOptionResolution()))
	if err != nil || repeat.Success || repeat.Changed {
		t.Fatalf("repeat result = %+v err=%v", repeat, err)
	}
	loaded = loadTestInventory(t, ctx, repos, "77")
	if !bytes.Equal(loaded.Slots[slotKey(listTypeMain, 9)].RawEntry, beforeRepeat) {
		t.Fatal("repeat unseal mutated target")
	}
	character, _, _ = repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 900 {
		t.Fatalf("repeat gold = %d, want 900", character.Stats["gold"])
	}
}

// 客户端右键「解封」走 op401；没有随机属性可开的贴膜物品（称号、传说、史诗）以前被拒，
// 服务端一直存着 seal_flag=1，重登 / 进称号簿又变回"封装"。现在只撕膜、不收金币。
//
// 移植上游 fc3d6d2（2026-09-22）。
func TestOwnerUnsealRandomOptionStripsSealFilmWhenNoRandomOptionEligibility(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntrySize)
	raw[0x0D] = 1
	saveRandomOptionFixture(t, ctx, repos, 1000, raw, map[string]string{
		"item_kind": "equipment", "equipment_type": "[title name]", "seal_flag": "1",
	})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	// 资格不足：TargetKind 与路径都对，但稀有度不是 2/3（称号这类没有随机属性可开）。
	ineligible := staticRandomOptionResolver(alignedcmd.RandomOptionResolution{TargetKind: "equipment", TargetPVFPath: "equipment/character/common/title/x.equ"})
	result, err := owner.UnsealRandomOption(ctx, unsealRandomOptionCommand(), ineligible)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || !result.Changed || result.Mode != "unseal_seal_film" || result.GoldCost != 0 || len(result.Options) != 0 {
		t.Fatalf("result = %+v", result)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	got := loaded.Slots[slotKey(listTypeMain, 9)]
	if got.RawEntry[0x0D] != 0 || got.Extra["seal_flag"] != "" || got.Extra["seal_removed_by_unseal"] != "1" {
		t.Fatalf("seal film not stripped: raw0d=%d extra=%v", got.RawEntry[0x0D], got.Extra)
	}
	if got.Extra["item_kind"] != "equipment" || got.Extra["equipment_type"] != "[title name]" {
		t.Fatalf("unrelated extra changed: %v", got.Extra)
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 1000 {
		t.Fatalf("plain unseal charged gold: %d", character.Stats["gold"])
	}

	// 已经没膜的物品：既无随机属性可开也无膜可撕，维持原来的"拒绝、不动"。
	again, err := owner.UnsealRandomOption(ctx, unsealRandomOptionCommand(), ineligible)
	if err != nil || again.Success || again.Changed {
		t.Fatalf("second unseal result = %+v err=%v", again, err)
	}
}

func TestOwnerChangeRandomOptionChangesOnlySelectedEntry(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, currentItemListEntrySize)
	raw[randomOptionCountOffset] = 3
	copy(raw[randomOptionTypeOffset:], []byte{11, 12, 13})
	copy(raw[randomOptionValue1Offset:], []byte{21, 22, 23})
	copy(raw[randomOptionValue2Offset:], []byte{31, 32, 33})
	raw[randomOptionStateOffset] = 7
	raw[randomOptionChangedIndexOffset] = 1
	copy(raw[randomOptionCandidateOffset:], []byte{9, 8, 7, 6})
	saveRandomOptionFixture(t, ctx, repos, 1000, raw, nil)
	owner, _ := NewOwner(repos)
	result, err := owner.ChangeRandomOption(ctx, changeRandomOptionCommand(1), staticRandomOptionResolver(testRandomOptionResolution()))
	if err != nil || !result.Success || result.UpdatedGold != 975 {
		t.Fatalf("result = %+v err=%v", result, err)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	got := loaded.Slots[slotKey(listTypeMain, 9)].RawEntry
	if got[randomOptionCountOffset] != 3 || !bytes.Equal(got[randomOptionTypeOffset:randomOptionTypeOffset+3], []byte{11, 22, 13}) {
		t.Fatalf("changed types = % X", got[randomOptionTypeOffset:randomOptionTypeOffset+3])
	}
	// 被变更的是第 2 条（下标 1）：类型来自 ModifiedGroups[1] 的候选（22），数值是
	// 第 2 槽位窗口内的百分位；其余两条必须原样不动（21/31 与 23/33）。
	if got[randomOptionValue1Offset] != 21 || got[randomOptionValue2Offset] != 31 ||
		got[randomOptionValue1Offset+2] != 23 || got[randomOptionValue2Offset+2] != 33 {
		t.Fatalf("untouched slots = % X", got[randomOptionValue1Offset:randomOptionValue2Offset+3])
	}
	assertSealPercentile(t, got, 2, testRandomOptionResolution().InitialSlotWindows, "change")
	if got[randomOptionStateOffset] != 0 || got[randomOptionChangedIndexOffset] != 0xFF || !bytes.Equal(got[randomOptionCandidateOffset:randomOptionCandidateOffset+4], make([]byte, 4)) {
		t.Fatalf("transient fields = % X", got[randomOptionStateOffset:randomOptionCandidateOffset+4])
	}
}

func TestOwnerRandomOptionValidationIsAtomic(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		gold        int64
		extra       map[string]string
		wantFailure RandomOptionFailure
	}{
		{name: "insufficient gold", gold: 99, wantFailure: RandomOptionFailureInsufficientGold},
		{name: "locked target", gold: 1000, extra: map[string]string{"equipment_lock_state": "locked"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repos := dnfrepomemory.NewMemoryGroup()
			raw := make([]byte, currentItemListEntrySize)
			raw[0x60] = 0x7B
			saveRandomOptionFixture(t, ctx, repos, test.gold, raw, test.extra)
			owner, _ := NewOwner(repos)
			result, err := owner.UnsealRandomOption(ctx, unsealRandomOptionCommand(), staticRandomOptionResolver(testRandomOptionResolution()))
			if err != nil || result.Success || result.Changed {
				t.Fatalf("result = %+v err=%v", result, err)
			}
			if result.Failure != test.wantFailure {
				t.Fatalf("failure = %q, want %q", result.Failure, test.wantFailure)
			}
			loaded := loadTestInventory(t, ctx, repos, "77")
			if !bytes.Equal(loaded.Slots[slotKey(listTypeMain, 9)].RawEntry, raw) {
				t.Fatal("rejected mutation changed target")
			}
			character, _, _ := repos.Character.Load(ctx, "77")
			if character.Stats["gold"] != test.gold {
				t.Fatalf("gold = %d, want %d", character.Stats["gold"], test.gold)
			}
		})
	}
}
