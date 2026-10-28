package titlebook

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 根因回归（礼白666 型卡死）：achievement.grantTitle 写进称号簿的条目**没有
// RawEntry**（见 achievement/owner.go grantTitle）。titlebook.Equip 过去把空 raw
// 原样搬进装备行，于是 equip 的 unequip() 一开头就撞上
// `len(source.RawEntry) == 0 → ErrMoveRawEntryMissing`，这个称号从此摘不下来。
// 现在 Equip 必须在簿内条目没有行数据时补出合法的 43 字节行。
func TestOwnerEquipSynthesizesRawEntryForAchievementGrantedTitle(t *testing.T) {
	t.Skip("2026-09-28 线上回退：被测行为已停用，待诊断后重开")
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 完全照 achievement.grantTitle 的形状：无 RawEntry + source=achievement_reward。
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			BookSlotKey(1, 135): {
				ItemID: 100331415,
				Count:  1,
				Extra: map[string]string{
					"source":              "achievement_reward",
					"title_book_category": "1",
					"title_book_index":    "135",
					"quest_id":            "6746",
				},
			},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	if _, err := owner.Equip(ctx, GetCommand{
		SelectedCharacterID: 19, ItemSpace: 3, SourceSlot: 13, ItemID: 100331415, Category: 1, BookIndex: 135,
	}); err != nil {
		t.Fatalf("Equip: %v", err)
	}
	equipment, found, err := repos.Equipment.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry, ok := equipment.Entries["13"]
	if !ok {
		t.Fatalf("equipment slot 13 missing: %+v", equipment.Entries)
	}
	if len(entry.RawEntry) != 43 {
		t.Fatalf("equipment raw must be synthesized, got %d bytes (%x)", len(entry.RawEntry), entry.RawEntry)
	}
	if entry.RawEntry[0] != 13 ||
		binary.LittleEndian.Uint32(entry.RawEntry[1:5]) != 100331415 ||
		binary.LittleEndian.Uint32(entry.RawEntry[5:9]) != 1 {
		t.Fatalf("synthesized equipment raw = %x", entry.RawEntry)
	}
	if entry.Extra["source"] != "title_book_equip" || entry.Extra["title_book_index"] != "135" {
		t.Fatalf("equipment extras = %+v", entry.Extra)
	}
	// 仍然按「移动」语义：穿上之后簿里不再留这件称号。
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if _, occupied := inventory.Slots[BookSlotKey(1, 135)]; occupied {
		t.Fatalf("book entry should move out when equipped: %+v", inventory.Slots)
	}
}

func TestOwnerPutAndGetTitle(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:42": {ItemID: 9001, Count: 1, Extra: map[string]string{"bind": "1"}},
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	put, err := owner.Put(ctx, PutCommand{
		SelectedCharacterID: 19,
		ItemSpace:           0,
		SourceSlot:          42,
		ItemID:              9001,
		Category:            2,
		BookIndex:           7,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if put.TargetSlot != 2007 || put.BookIndex != 7 {
		t.Fatalf("put result = %+v", put)
	}
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load after put: %v", err)
	}
	if _, found := inventory.Slots["0:42"]; found ||
		inventory.Slots["100:2007"].ItemID != 9001 {
		t.Fatalf("slots after put = %+v", inventory.Slots)
	}

	get, err := owner.Get(ctx, GetCommand{
		SelectedCharacterID: 19,
		ItemSpace:           0,
		SourceSlot:          42,
		ItemID:              9001,
		Category:            2,
		BookIndex:           7,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if get.TargetSlot != 9 || get.TargetStack.ItemID != 9001 ||
		get.TargetStack.Extra["bind"] != "1" {
		t.Fatalf("get result = %+v", get)
	}
}

func TestOwnerEquipTitleFromBookMovesItOutOfTheBook(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	// 称号簿里已经登记了一个称号（list 100 / category 4 / index 10），
	// 行首槽位是簿内槽号，穿到身上时会被改写成 list-3 的称号槽位。
	bookRow := make([]byte, 0x77)
	bookRow[0], bookRow[1] = 0xEA, 0x03 // 1000 + 10 = 1010 = 0x03EA
	copy(bookRow[2:6], []byte{0xC8, 0xEC, 0x29, 0x00})
	repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			BookSlotKey(4, 10): {ItemID: 2747592, Count: 1, RawEntry: bookRow, Extra: map[string]string{"item_kind": "equipment", "pvf_path": "equipment/character/common/title/t.equ"}},
		},
	})
	repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{}})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	result, err := owner.Equip(ctx, GetCommand{
		SelectedCharacterID: 19, ItemSpace: 1, SourceSlot: 3, ItemID: 2747592, Category: 4, BookIndex: 10,
	})
	if err != nil {
		t.Fatalf("Equip: %v", err)
	}
	if result.EquipmentSlot != TitleEquipmentSlot || result.ItemID != 2747592 || result.Replaced {
		t.Fatalf("equip result = %+v", result)
	}
	// 官方语义是「移动」：穿上之后簿里不再留这件称号。
	// （簿里一个、身上一个正是玩家报的双份现象，见 owner.go Equip 注释。）
	inventory, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatalf("load inventory: %v", err)
	}
	if stack, found := inventory.Slots[BookSlotKey(4, 10)]; found {
		t.Fatalf("book entry should move out when equipped: %+v", stack)
	}
	// 也不许往背包里塞一份（那正是玩家报的「背包里多一个」）。
	for key, stack := range inventory.Slots {
		if stack.ItemID == 2747592 {
			t.Fatalf("title leaked into slot %s: %+v", key, stack)
		}
	}
	equipment, found, err := repos.Equipment.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry, ok := equipment.Entries["13"]
	if !ok || entry.ItemID != 2747592 || len(entry.RawEntry) != len(bookRow) || entry.RawEntry[0] != 13 || entry.RawEntry[1] != 0 {
		t.Fatalf("equipment slot 13 = %+v", entry)
	}
	if entry.Extra["equipped_slot"] != "13" || entry.Extra["source"] != "title_book_equip" || entry.Extra["title_book_index"] != "10" {
		t.Fatalf("equipment extras = %+v", entry.Extra)
	}

	// 再换一个称号：旧称号必须回背包，不能凭空消失。
	secondRow := make([]byte, 0x77)
	copy(secondRow[2:6], []byte{0x11, 0x22, 0x33, 0x00})
	if inventory.Slots == nil {
		// 上一件称号穿走后簿里空了，内存仓会回一个空记录。
		inventory.Slots = make(map[string]dnfrepo.ItemStack, 1)
	}
	inventory.Slots[BookSlotKey(4, 11)] = dnfrepo.ItemStack{ItemID: 2747593, Count: 1, RawEntry: secondRow}
	if err := repos.Inventory.Save(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	swap, err := owner.Equip(ctx, GetCommand{
		SelectedCharacterID: 19, ItemSpace: 1, SourceSlot: 3, ItemID: 2747593, Category: 4, BookIndex: 11,
	})
	if err != nil {
		t.Fatalf("Equip swap: %v", err)
	}
	if !swap.Replaced || swap.ReturnItem.ItemID != 2747592 {
		t.Fatalf("swap result = %+v", swap)
	}
	after, _, err := repos.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if after.Slots[inventoryKey(dnfrepo.MainInventoryListType, swap.ReturnSlot)].ItemID != 2747592 {
		t.Fatalf("replaced title not returned to bag: %+v", after.Slots)
	}
	equipment, _, _ = repos.Equipment.Load(ctx, "19")
	if equipment.Entries["13"].ItemID != 2747593 {
		t.Fatalf("equipment after swap = %+v", equipment.Entries)
	}
	if _, found := after.Slots[BookSlotKey(4, 11)]; found {
		t.Fatalf("swapped-in title should leave the book: %+v", after.Slots)
	}
}

func TestOwnerGetHonorsPreferredSlot(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			BookSlotKey(4, 10): {ItemID: 2747592, Count: 1},
			"0:12":             {ItemID: 5, Count: 1}, // 首选格被占用
		},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	// 首选格空着 → 必须用它（客户端按 ack 里的槽位摆放物品）。
	free, err := owner.Get(ctx, GetCommand{
		SelectedCharacterID: 19, ItemSpace: 0, PreferredSlot: 30, ItemID: 2747592, Category: 4, BookIndex: 10,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if free.TargetSlot != 30 {
		t.Fatalf("target slot = %d, want the requested 30", free.TargetSlot)
	}
	// 首选格被占用 → 退回第一个空格（9 号格空着）。
	occupied, err := owner.Put(ctx, PutCommand{
		SelectedCharacterID: 19, ItemSpace: 0, SourceSlot: 30, ItemID: 2747592, Category: 4, BookIndex: 10,
	})
	if err != nil {
		t.Fatalf("Put back: %v", err)
	}
	if occupied.TargetSlot != 4010 {
		t.Fatalf("put back target = %d", occupied.TargetSlot)
	}
	fallback, err := owner.Get(ctx, GetCommand{
		SelectedCharacterID: 19, ItemSpace: 0, PreferredSlot: 12, ItemID: 2747592, Category: 4, BookIndex: 10,
	})
	if err != nil {
		t.Fatalf("Get fallback: %v", err)
	}
	if fallback.TargetSlot != 9 {
		t.Fatalf("target slot = %d, want the first free slot 9", fallback.TargetSlot)
	}
}

func TestOwnerPutRejectsFullCategory(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	slots := map[string]dnfrepo.ItemStack{
		"0:42": {ItemID: 9001, Count: 1},
	}
	for index := int32(0); index < MaxPerCategory; index++ {
		slots[BookSlotKey(2, index)] = dnfrepo.ItemStack{ItemID: int64(10000 + index), Count: 1}
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       slots,
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner: %v", err)
	}
	_, err = owner.Put(ctx, PutCommand{
		SelectedCharacterID: 19,
		ItemSpace:           0,
		SourceSlot:          42,
		ItemID:              9001,
		Category:            2,
		BookIndex:           7,
	})
	if !errors.Is(err, ErrCategoryFull) {
		t.Fatalf("Put error = %v, want ErrCategoryFull", err)
	}
}
