package equip

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 线上 Script.pvf 里 [item category] 为 `clear avatar` 的 2079 份装备文档中，只有
// 这 3 件是 [aurora avatar]：113590006 / 113590008 / 113590009（转职光环装扮）。
// 113590008 就是日志里角色 1 从 490004995 开出来的那一件。
const (
	testAuroraLookReplaceAvatar int64 = 113590008 // 转职光环装扮
	testOrdinaryAura            int64 = 101590068 // 被它覆盖的普通光环
	testCloneHat                int64 = 415500017 // 透明克隆时装（帽）
	testOrdinaryHat             int64 = 415500058 // 普通装扮（帽）
)

// newAuraLookReplaceTestOwner 按真实 PVF 分类装配 owner：
//   - 普通光环 / 普通装扮：是装扮，但不是 clear avatar；
//   - 转职光环 / 克隆帽：都是 clear avatar；
//   - 只有转职光环命中 auroraLookReplaceClassifier。
func newAuraLookReplaceTestOwner(t *testing.T, repos dnfrepo.Group, withAuroraClassifier bool) *Owner {
	t.Helper()
	owner, err := NewOwnerWithPlacementValidator(repos, &recordingPlacementValidator{})
	if err != nil {
		t.Fatal(err)
	}
	owner.SetAvatarClassifier(func(itemID uint32) (bool, bool) {
		switch int64(itemID) {
		case testOrdinaryAura, testOrdinaryHat:
			return true, false
		case testAuroraLookReplaceAvatar, testCloneHat:
			return true, true
		default:
			return false, false
		}
	})
	if withAuroraClassifier {
		owner.SetAuroraLookReplaceClassifier(func(itemID uint32) bool {
			return int64(itemID) == testAuroraLookReplaceAvatar
		})
	}
	return owner
}

// TestOwnerAuroraLookReplaceKeepsOwnAppearanceAndLinksCoveredAura 覆盖用户报的现象：
// 槽 9 已有普通光环时穿上转职光环，服务端旧代码把被覆盖光环的模板写进"外观" key，
// 于是 mode0 又渲染回原光环。现在必须是"外观保留自己 + 被覆盖光环只作为关联来源"。
func TestOwnerAuroraLookReplaceKeepsOwnAppearanceAndLinksCoveredAura(t *testing.T) {
	const (
		characterID       = "77"
		auraSlot    int16 = 9
		bagSlot     int16 = 31
	)
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			"1:31": {ItemID: testAuroraLookReplaceAvatar, Count: 0, RawEntry: []byte{0xA1}},
		},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"9": {SlotIndex: auraSlot, ItemID: testOrdinaryAura, RawEntry: []byte{0xC3}},
		},
	})
	owner := newAuraLookReplaceTestOwner(t, repos, true)

	result, err := owner.Move(ctx, MoveCommand{
		SelectedCharacterID:  77,
		SourceListType:       wireListAvatar,
		SourceSlotIndex:      bagSlot,
		MoveCount:            1,
		DestinationListType:  wireListEquipment,
		DestinationSlotIndex: auraSlot,
	})
	if err != nil {
		t.Fatalf("equip aurora look-replace: %v", err)
	}
	if result.Mode != "equip_swap" || result.ItemID != testAuroraLookReplaceAvatar ||
		result.SwappedItemID != testOrdinaryAura {
		t.Fatalf("result=%+v", result)
	}

	worn := loadTestEquipment(t, ctx, repos, characterID).Entries["9"]
	if worn.ItemID != testAuroraLookReplaceAvatar {
		t.Fatalf("worn item=%d want=%d", worn.ItemID, testAuroraLookReplaceAvatar)
	}
	// 红线：转职光环绝不能借用被覆盖光环的外观，否则 mode0 槽 9 又发回原光环。
	if got := worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey]; got != "" {
		t.Fatalf("转职光环 must not borrow the covered aura appearance, got %q", got)
	}
	if got := worn.Extra[dnfrepo.AuraLinkedTemplateExtraKey]; got != "101590068" {
		t.Fatalf("aura link=%q want=101590068", got)
	}
	// 关联来源必须记下被覆盖光环的落点：投影时据此实时取它的镶嵌字节。
	// 玩家完全可能在穿好转职光环之后才给那件光环打徽章，所以不能穿戴时快照。
	if got := worn.Extra[dnfrepo.AuraLinkedSourceExtraKey]; got != "1:31" {
		t.Fatalf("aura link source=%q want=1:31", got)
	}

	returned := loadTestInventory(t, ctx, repos, characterID).Slots["1:31"]
	if returned.ItemID != testOrdinaryAura {
		t.Fatalf("returned=%+v", returned)
	}
	if returned.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "" ||
		returned.Extra[dnfrepo.AuraLinkedTemplateExtraKey] != "" {
		t.Fatalf("returned aura kept residual keys=%+v", returned.Extra)
	}
}

// TestOwnerAuroraLookReplaceInIllusionSlotRecordsNoLink 证明放进光环幻化栏（槽 11）
// 的转职光环不带关联：客户端只在槽 9 读那个关联块，槽 9 的真光环本来就在提供属性。
func TestOwnerAuroraLookReplaceInIllusionSlotRecordsNoLink(t *testing.T) {
	const characterID = "77"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			"1:31": {ItemID: testAuroraLookReplaceAvatar, Count: 0, RawEntry: []byte{0xA1}},
		},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"9": {SlotIndex: 9, ItemID: testOrdinaryAura, RawEntry: []byte{0xC3}},
		},
	})
	owner := newAuraLookReplaceTestOwner(t, repos, true)

	if _, err := owner.Move(ctx, MoveCommand{
		SelectedCharacterID:  77,
		SourceListType:       wireListAvatar,
		SourceSlotIndex:      31,
		MoveCount:            1,
		DestinationListType:  wireListEquipment,
		DestinationSlotIndex: 11,
	}); err != nil {
		t.Fatalf("equip into illusion slot: %v", err)
	}

	worn := loadTestEquipment(t, ctx, repos, characterID).Entries["11"]
	if worn.ItemID != testAuroraLookReplaceAvatar {
		t.Fatalf("illusion slot item=%d want=%d", worn.ItemID, testAuroraLookReplaceAvatar)
	}
	if worn.Extra[dnfrepo.AuraLinkedTemplateExtraKey] != "" ||
		worn.Extra[dnfrepo.AuraLinkedSourceExtraKey] != "" ||
		worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "" {
		t.Fatalf("illusion slot must stay link-free, extra=%+v", worn.Extra)
	}
}

// TestOwnerAuroraLookReplaceWithoutClassifierKeepsLegacyBehaviour 钉住回退路径：
// classifier 未注入（隔离测试、宠物事务等）时必须完全等于改动前的行为，不能
// 悄悄改变任何东西。
func TestOwnerAuroraLookReplaceWithoutClassifierKeepsLegacyBehaviour(t *testing.T) {
	const characterID = "77"
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			"1:31": {ItemID: testAuroraLookReplaceAvatar, Count: 0, RawEntry: []byte{0xA1}},
		},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"9": {SlotIndex: 9, ItemID: testOrdinaryAura, RawEntry: []byte{0xC3}},
		},
	})
	owner := newAuraLookReplaceTestOwner(t, repos, false)

	if _, err := owner.Move(ctx, MoveCommand{
		SelectedCharacterID:  77,
		SourceListType:       wireListAvatar,
		SourceSlotIndex:      31,
		MoveCount:            1,
		DestinationListType:  wireListEquipment,
		DestinationSlotIndex: 9,
	}); err != nil {
		t.Fatalf("equip without aurora classifier: %v", err)
	}

	worn := loadTestEquipment(t, ctx, repos, characterID).Entries["9"]
	if worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] != "101590068" {
		t.Fatalf("legacy fallback must still borrow appearance, extra=%+v", worn.Extra)
	}
	if worn.Extra[dnfrepo.AuraLinkedTemplateExtraKey] != "" {
		t.Fatalf("legacy fallback must not write the aura link, extra=%+v", worn.Extra)
	}
}

// TestOwnerPlainCloneAvatarStillBorrowsAppearance 是"别修 A 坏 B"的直接护栏：
// 普通透明克隆时装的行为必须与改动前逐字节一致。
func TestOwnerPlainCloneAvatarStillBorrowsAppearance(t *testing.T) {
	const (
		characterID       = "77"
		hatSlot     int16 = 3
	)
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: characterID,
		Slots: map[string]dnfrepo.ItemStack{
			"1:19": {ItemID: testCloneHat, Count: 0, RawEntry: []byte{0xA1}},
		},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"3": {SlotIndex: hatSlot, ItemID: testOrdinaryHat, RawEntry: []byte{0xC3}},
		},
	})
	owner := newAuraLookReplaceTestOwner(t, repos, true)

	result, err := owner.Move(ctx, MoveCommand{
		SelectedCharacterID:  77,
		SourceListType:       wireListAvatar,
		SourceSlotIndex:      19,
		MoveCount:            1,
		DestinationListType:  wireListEquipment,
		DestinationSlotIndex: hatSlot,
	})
	if err != nil {
		t.Fatalf("equip clone hat: %v", err)
	}
	if result.Mode != "equip_swap" || result.ItemID != testCloneHat || result.SwappedItemID != testOrdinaryHat {
		t.Fatalf("result=%+v", result)
	}

	worn := loadTestEquipment(t, ctx, repos, characterID).Entries["3"]
	if worn.ItemID != testCloneHat {
		t.Fatalf("worn item=%d want=%d", worn.ItemID, testCloneHat)
	}
	if got := worn.Extra[dnfrepo.CloneAvatarAppearanceExtraKey]; got != "415500058" {
		t.Fatalf("clone appearance=%q want=415500058", got)
	}
	if worn.Extra[dnfrepo.AuraLinkedTemplateExtraKey] != "" {
		t.Fatalf("clone hat must not carry an aura link, extra=%+v", worn.Extra)
	}
}
