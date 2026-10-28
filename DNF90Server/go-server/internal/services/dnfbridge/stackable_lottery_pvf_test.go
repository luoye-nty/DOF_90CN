package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The tarot card's own PVF numbers, read from the shipped archive.
//
// 真档原文 `stackable/10159001/10159697.stk`（「占星师罗赛洛  塔罗牌」）:
//
//	[stackable type]    `[monster tarot card]` 19
//	[lottery use cost]  100000
//	[int data]          100250574 116666 1 102040539 116666 1 106030594 116666 1 10159145 350000 1 10159142 300000 250
//	[cool time]         1000
//	[explain]           `使用后， 可以获得卢克地下城的神器及以上怪物卡片、 传说装备、 光之根源等。`
//
// Run with the real archive:
//
//	DNF_WORLDMAP_REAL_PVF_SMOKE=<Script.pvf> go test ./internal/services/dnfbridge/ \
//	    -run TestRealScriptPVFTarotCardLotteryTable -v
func TestRealScriptPVFTarotCardLotteryTable(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to read the tarot card's lottery table")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	const tarotCardItemID = uint32(10159697)
	definition, err := catalog.ResolveItem(tarotCardItemID)
	if err != nil {
		t.Fatalf("resolve tarot card %d: %v", tarotCardItemID, err)
	}
	if got := normalizeDungeonDropStackableType(definition.StackableType); got != "[monster tarot card]" {
		t.Fatalf("stackable type=%q want [monster tarot card] (path=%s)", got, definition.PVFPath)
	}
	if definition.LotteryUseCost != 100000 {
		t.Fatalf("lottery use cost=%d want 100000", definition.LotteryUseCost)
	}
	want := []dungeonDropLotteryEntry{
		{ItemID: 100250574, Weight: 116666, Count: 1},
		{ItemID: 102040539, Weight: 116666, Count: 1},
		{ItemID: 106030594, Weight: 116666, Count: 1},
		{ItemID: 10159145, Weight: 350000, Count: 1},
		{ItemID: 10159142, Weight: 300000, Count: 250},
	}
	if len(definition.LotteryEntries) != len(want) {
		t.Fatalf("lottery entries=%+v want %d rows", definition.LotteryEntries, len(want))
	}
	for index, entry := range want {
		if definition.LotteryEntries[index] != entry {
			t.Fatalf("lottery row %d=%+v want %+v", index, definition.LotteryEntries[index], entry)
		}
	}
	if got := dungeonDropLotteryTotalWeight(definition.LotteryEntries); got != 999998 {
		t.Fatalf("total weight=%d want 999998 (116666*3 + 350000 + 300000)", got)
	}
	// The pool must be drawable end to end: roll 0 lands on the first row, and the
	// last unit of weight lands on the last row.
	first, ok := dungeonDropLotteryPick(definition.LotteryEntries, 0)
	if !ok || first != want[0] {
		t.Fatalf("roll 0 -> %+v ok=%t want %+v", first, ok, want[0])
	}
	last, ok := dungeonDropLotteryPick(definition.LotteryEntries, 999997)
	if !ok || last != want[len(want)-1] {
		t.Fatalf("roll 999997 -> %+v ok=%t want %+v", last, ok, want[len(want)-1])
	}
	if _, ok := dungeonDropLotteryPick(definition.LotteryEntries, 999998); ok {
		t.Fatal("a roll at the total weight must miss")
	}
	t.Logf("path=%s stackable=%q cost=%d entries=%+v total_weight=%d",
		definition.PVFPath, definition.StackableType, definition.LotteryUseCost,
		definition.LotteryEntries, dungeonDropLotteryTotalWeight(definition.LotteryEntries))

	// The class is not a single item: 22 stackables declare `[monster tarot card]`
	// (the card ids the 1181 tarot display names as arcana). Assert the shape holds
	// for a second and third card too, so nothing here is hard-coded to 10159697.
	for _, secondCardID := range []uint32{10159698, 10159700} {
		second, err := catalog.ResolveItem(secondCardID)
		if err != nil {
			t.Fatalf("resolve second tarot card %d: %v", secondCardID, err)
		}
		if got := normalizeDungeonDropStackableType(second.StackableType); got != "[monster tarot card]" {
			t.Fatalf("card %d stackable type=%q want [monster tarot card]", secondCardID, got)
		}
		if second.LotteryUseCost <= 0 || len(second.LotteryEntries) == 0 {
			t.Fatalf("card %d declares cost=%d entries=%d",
				secondCardID, second.LotteryUseCost, len(second.LotteryEntries))
		}
		if total := dungeonDropLotteryTotalWeight(second.LotteryEntries); total <= 0 {
			t.Fatalf("card %d has no drawable weight: %+v", secondCardID, second.LotteryEntries)
		}
		if _, ok := dungeonDropLotteryPick(second.LotteryEntries, 0); !ok {
			t.Fatalf("card %d cannot be drawn at roll 0", secondCardID)
		}
		t.Logf("card=%d cost=%d rows=%d total_weight=%d first=%+v",
			secondCardID, second.LotteryUseCost, len(second.LotteryEntries),
			dungeonDropLotteryTotalWeight(second.LotteryEntries), second.LotteryEntries[0])
	}
}

// TestDungeonDropLotteryEntriesFailClosed pins the parser's own boundaries: a
// table that is empty, partial or carries unusable rows is not drawable.
func TestDungeonDropLotteryEntriesFailClosed(t *testing.T) {
	if entries := parseDungeonDropLotteryEntries(nil); entries != nil {
		t.Fatalf("empty table -> %+v", entries)
	}
	if entries := parseDungeonDropLotteryEntries([]int64{1, 2}); entries != nil {
		t.Fatalf("partial triple -> %+v", entries)
	}
	// A trailing partial triple is dropped, the whole rows survive.
	entries := parseDungeonDropLotteryEntries([]int64{10, 5, 1, 20, 5, 2, 30, 5})
	if len(entries) != 2 {
		t.Fatalf("entries=%+v want the two whole rows", entries)
	}
	if total := dungeonDropLotteryTotalWeight(entries); total != 10 {
		t.Fatalf("total weight=%d want 10", total)
	}
	// Rows with a zero/negative item, weight or count are not drawable at all.
	unusable := []dungeonDropLotteryEntry{
		{ItemID: 0, Weight: 5, Count: 1},
		{ItemID: 10, Weight: 0, Count: 1},
		{ItemID: 10, Weight: 5, Count: 0},
	}
	if total := dungeonDropLotteryTotalWeight(unusable); total != 0 {
		t.Fatalf("total weight of unusable rows=%d want 0", total)
	}
	if _, ok := dungeonDropLotteryPick(unusable, 0); ok {
		t.Fatal("an unusable table produced a winner")
	}
}
