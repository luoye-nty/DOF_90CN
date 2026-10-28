package inventory

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 现场 2026-10-03（服主报「塔罗牌用不了」）：真档塔罗牌的 `[int data]` 是**混合奖池**，
// 装备行按权重占大半（10159717：3 条装备 + 1 条材料）。而 grantRandomRewardItem 过去
// 只接受 kind=stackable —— 抽到装备行就等于"什么都没发生"。
//
// 这几条用例把装备行那条支路钉住，同时钉住"没破坏可叠加那一支"。
func TestGrantRandomRewardItemPlacesEquipmentOnePerRow(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{}
	slots, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         108040415,
		Kind:           "equipment",
		TargetListType: listTypeMain,
		EquipmentType:  "[weapon]",
		SlotStart:      9,
		SlotEnd:        64,
		Seal:           true,
		PVFPath:        "equipment/character/priest/weapon/axe/108040415.equ",
		Durability:     45,
		QualitySeed:    12345,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0] != 9 {
		t.Fatalf("slots=%v want [9]（装备落在 PVF 的 9..64 第一格）", slots)
	}
	stack := items[slotKey(listTypeMain, 9)]
	if stack.ItemID != 108040415 || stack.Count != 1 {
		t.Fatalf("stack=%+v want item 108040415 x1", stack)
	}
	if stack.Extra["item_kind"] != "equipment" || stack.Extra["equipment_type"] != "[weapon]" {
		t.Fatalf("装备行元数据不对: %v", stack.Extra)
	}
	if stack.Extra["seal_flag"] != "1" {
		t.Fatalf("Seal=true 必须写成 seal_flag=1: %v", stack.Extra)
	}
	if stack.Extra["durability"] != "45" || stack.Extra["max_durability"] != "45" {
		t.Fatalf("PVF [durability] 必须写进耐久字段: %v", stack.Extra)
	}
	if stack.Extra["quality_seed"] != "12345" {
		t.Fatalf("契约带来的品级种子没有被采用: %v", stack.Extra)
	}
	if len(stack.RawEntry) != 0x77 {
		t.Fatalf("RawEntry 长=%d want 0x77（当前行）", len(stack.RawEntry))
	}
}

// 装备**绝不合并**：容器里已经有同 id 的一行时，第二件必须另开一格，
// 而不是像可叠加物那样并进第一行（并进去客户端会把它显示成叠加物）。
func TestGrantRandomRewardItemNeverStacksEquipment(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 9): {
			ItemID: 108040415, Count: 1,
			Extra: map[string]string{"item_kind": "equipment"},
		},
	}
	slots, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         108040415,
		Kind:           "equipment",
		TargetListType: listTypeMain,
		EquipmentType:  "[weapon]",
		SlotStart:      9,
		SlotEnd:        64,
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0] != 10 {
		t.Fatalf("slots=%v want [10]（第二件必须另开一格）", slots)
	}
	if first := items[slotKey(listTypeMain, 9)]; first.Count != 1 {
		t.Fatalf("已有那一行被并了: %+v", first)
	}
}

// 契约上允许 count > 1（真档不会出现）：装备要一格一件地铺开。
func TestGrantRandomRewardItemSpreadsMultipleEquipmentAcrossRows(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{}
	slots, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         108040415,
		Kind:           "equipment",
		TargetListType: listTypeMain,
		SlotStart:      9,
		SlotEnd:        64,
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots=%v want 3 格", slots)
	}
	for _, slot := range slots {
		if stack := items[slotKey(listTypeMain, slot)]; stack.Count != 1 {
			t.Fatalf("slot=%d 数量=%d want 1", slot, stack.Count)
		}
	}
}

// 背包放不下时必须是既有的"干净失败"：调用方整笔事务回滚，不会出现半件装备。
func TestGrantRandomRewardItemFailsCleanlyWhenEquipmentHasNoRoom(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{}
	items[slotKey(listTypeMain, 9)] = dnfrepo.ItemStack{ItemID: 1, Count: 1}
	if _, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         108040415,
		Kind:           "equipment",
		TargetListType: listTypeMain,
		SlotStart:      9,
		SlotEnd:        9,
	}, 1); err == nil {
		t.Fatal("只有一格且被占，必须报 ErrRandomRewardInventoryFull")
	}
	if len(items) != 1 {
		t.Fatalf("失败时不该留下任何东西: %+v", items)
	}
}

// 「修 A 不坏 B」：kind 既不是 stackable 也不是 equipment 的，仍然一律拒绝。
// 这条是那道 guard 的钉子 —— 放宽装备行不等于放宽一切。
func TestGrantRandomRewardItemStillRefusesUnknownKinds(t *testing.T) {
	for _, kind := range []string{"", "creature", "avatar", "Equipment!"} {
		items := map[string]dnfrepo.ItemStack{}
		if _, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
			ItemID:         700,
			Kind:           kind,
			TargetListType: listTypeMain,
			SlotStart:      9,
			SlotEnd:        64,
		}, 1); err == nil {
			t.Fatalf("kind=%q 本该被拒", kind)
		}
		if len(items) != 0 {
			t.Fatalf("kind=%q 被拒却写了背包: %+v", kind, items)
		}
	}
}

// 回到别人身上：装备行的目标容器不是主背包（例如宠物 list 7）时仍然拒绝 ——
// 宠物装备那条路有自己的发奖函数（grantCurrentBoosterPetArtifact）。
func TestGrantRandomRewardItemRefusesEquipmentOutsideTheMainList(t *testing.T) {
	items := map[string]dnfrepo.ItemStack{}
	if _, err := grantRandomRewardItem(items, alignedcmd.MagicBoxRewardItem{
		ItemID:         702,
		Kind:           "equipment",
		TargetListType: 7,
		SlotStart:      0,
		SlotEnd:        139,
	}, 1); err == nil {
		t.Fatal("非主背包的装备行必须继续被拒")
	}
}
