package inventory

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const (
	equipmentInheritanceTestCharacter = "77"
	equipmentInheritanceTestCoupon    = int64(202611001)
	equipmentInheritanceTestSourceID  = int64(9001)
	equipmentInheritanceTestTargetID  = int64(9002)
	equipmentInheritanceTestMaterial  = int64(3001)
)

type equipmentInheritanceFixture struct {
	repos      dnfrepo.Group
	owner      *Owner
	cmd        Command
	resolution alignedcmd.StackableUseResolution
	inventory  dnfrepo.InventoryRecord
	equipment  dnfrepo.EquipmentRecord
	character  dnfrepo.CharacterRecord
	metadata   map[int64]alignedcmd.EquipmentInheritanceEquipment
	source     dnfrepo.ItemStack
	target     dnfrepo.ItemStack
}

func TestEquipmentInheritanceMovesCompleteBuildClearsSourceAndConsumesConfiguredCost(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	result, err := fixture.owner.useEquipmentInheritance(context.Background(), fixture.cmd, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	if !result.EquipmentInheritanceApplied || result.EquipmentInheritanceRejected ||
		result.EquipmentInheritanceSourceSlot != 9 || result.EquipmentInheritanceTargetSlot != 14 {
		t.Fatalf("result=%+v", result)
	}

	inventory := loadTestInventory(t, context.Background(), fixture.repos, equipmentInheritanceTestCharacter)
	coupon := inventory.Slots[slotKey(listTypeMain, 10)]
	if coupon.ItemID != equipmentInheritanceTestCoupon || coupon.Count != 1 {
		t.Fatalf("coupon changed: %+v", coupon)
	}
	if material := inventory.Slots[slotKey(listTypeMain, 20)]; material.ItemID != equipmentInheritanceTestMaterial || material.Count != 1 {
		t.Fatalf("material=%+v", material)
	}
	source := inventory.Slots[slotKey(listTypeMain, 9)]
	if build, err := equipmentInheritanceBuildOf(source, 14); err != nil || build.hasAny() {
		t.Fatalf("source build=%+v err=%v", build, err)
	}

	equipmentRecord, found, err := fixture.repos.Equipment.Load(context.Background(), equipmentInheritanceTestCharacter)
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	targetEntry := equipmentRecord.Entries["14"]
	target := forgingStackFromEquipmentEntry(targetEntry)
	targetBuild, err := equipmentInheritanceBuildOf(target, 14)
	if err != nil {
		t.Fatal(err)
	}
	wantBuild, _ := equipmentInheritanceBuildOf(fixture.source, 14)
	if targetBuild != wantBuild {
		t.Fatalf("target build=%+v want=%+v", targetBuild, wantBuild)
	}
	if target.ItemID != equipmentInheritanceTestTargetID ||
		targetBuild.amplifyType != 2 || targetBuild.amplifyValue != 12 ||
		targetBuild.forging != 6 || targetBuild.enchantCard != 12345 ||
		targetBuild.enchantUpgrade != 4 || targetBuild.emblems[0] != 2 {
		t.Fatalf("target=%+v build=%+v", target, targetBuild)
	}
	character, found, err := fixture.repos.Character.Load(context.Background(), equipmentInheritanceTestCharacter)
	if err != nil || !found || character.Stats["gold"] != 900 {
		t.Fatalf("character found=%t stats=%+v err=%v", found, character.Stats, err)
	}
}

func TestEquipmentInheritanceAmplificationLevelZeroStillCountsAsBuild(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	fixture.source = equipmentInheritanceTestEquipment(9, equipmentInheritanceTestSourceID, 0)
	setAmplifyState(&fixture.source, 2, 0)
	fixture.inventory.Slots[slotKey(listTypeMain, 9)] = fixture.source
	fixture.resolution.EquipmentInheritance.Materials = nil
	fixture.save(t)

	result, err := fixture.owner.useEquipmentInheritance(context.Background(), fixture.cmd, fixture.resolution)
	if err != nil {
		t.Fatal(err)
	}
	if !result.EquipmentInheritanceApplied {
		t.Fatalf("result=%+v", result)
	}
	inventory := loadTestInventory(t, context.Background(), fixture.repos, equipmentInheritanceTestCharacter)
	sourceBuild, _ := equipmentInheritanceBuildOf(inventory.Slots[slotKey(listTypeMain, 9)], 14)
	if sourceBuild.amplifyType != 0 || sourceBuild.amplifyValue != 0 {
		t.Fatalf("source amplification not cleared: %+v", sourceBuild)
	}
	equipmentRecord, _, _ := fixture.repos.Equipment.Load(context.Background(), equipmentInheritanceTestCharacter)
	targetBuild, _ := equipmentInheritanceBuildOf(forgingStackFromEquipmentEntry(equipmentRecord.Entries["14"]), 14)
	if targetBuild.amplifyType != 2 || targetBuild.amplifyValue != 0 {
		t.Fatalf("target amplification=%+v", targetBuild)
	}
}

func TestTryUseEquipmentInheritanceSlotResolvesCouponFromDurableSlot(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	cmd := fixture.cmd
	cmd.Operation = "use_stackable_action"
	cmd.ActionIndex = favorLetterActionIndex
	cmd.ItemTemplateID = 0
	cmd.SourceInstanceValue = 0
	resolver := func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		if itemID != equipmentInheritanceTestCoupon {
			t.Fatalf("resolver item=%d", itemID)
		}
		return fixture.resolution, nil
	}

	result, matched, err := fixture.owner.TryUseEquipmentInheritanceSlot(context.Background(), cmd, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !matched || !result.EquipmentInheritanceApplied || result.EquipmentInheritanceRejected {
		t.Fatalf("matched=%t result=%+v", matched, result)
	}
}

func TestEquipmentInheritanceType251HandlerAppliesWithoutRandomRewardResolver(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	body := make([]byte, 6)
	binary.LittleEndian.PutUint32(body[0:4], uint32(equipmentInheritanceTestCoupon))
	body[4] = 10
	body[5] = listTypeMain
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseSharedEffectItem),
		Body:                body,
		AccountID:           fixture.cmd.AccountID,
		SelectedCharacterID: uint16(fixture.cmd.SelectedCharacterID),
		Repositories:        fixture.repos,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			if itemID != equipmentInheritanceTestCoupon {
				t.Fatalf("resolver item=%d", itemID)
			}
			return fixture.resolution, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || len(result.UpperResponses) != 1 ||
		len(result.UpperResponses[0].Body) != 1 || result.UpperResponses[0].Body[0] != 1 {
		t.Fatalf("type251 result=%+v", result)
	}
	if len(result.EquipmentSlotRefreshes) != 1 || result.EquipmentSlotRefreshes[0] != 14 {
		t.Fatalf("equipment refreshes=%v", result.EquipmentSlotRefreshes)
	}
}

func TestEquipmentInheritanceOp515FavorLetterCarrierAppliesBeforeFavorAction(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 10)
	binary.LittleEndian.PutUint32(body[7:11], favorLetterActionIndex)
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                body,
		AccountID:           fixture.cmd.AccountID,
		SelectedCharacterID: uint16(fixture.cmd.SelectedCharacterID),
		Repositories:        fixture.repos,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			if itemID != equipmentInheritanceTestCoupon {
				t.Fatalf("resolver item=%d", itemID)
			}
			return fixture.resolution, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || len(result.UpperResponses) != 1 ||
		len(result.UpperResponses[0].Body) == 0 || result.UpperResponses[0].Body[0] != 1 {
		t.Fatalf("op515 result=%+v", result)
	}
	if len(result.EquipmentSlotRefreshes) != 1 || result.EquipmentSlotRefreshes[0] != 14 {
		t.Fatalf("equipment refreshes=%v", result.EquipmentSlotRefreshes)
	}
}

func TestEquipmentInheritanceOp30GoldenCapsuleCarrierAppliesBeforeExperienceBook(t *testing.T) {
	fixture := newEquipmentInheritanceFixture(t)
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 10)
	result, err := NewHandler().Handle(context.Background(), alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketIncreaseStatus),
		Body:                body,
		AccountID:           fixture.cmd.AccountID,
		SelectedCharacterID: uint16(fixture.cmd.SelectedCharacterID),
		Repositories:        fixture.repos,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			if itemID != equipmentInheritanceTestCoupon {
				t.Fatalf("resolver item=%d", itemID)
			}
			return fixture.resolution, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || len(result.UpperResponses) != 1 ||
		len(result.UpperResponses[0].Body) != increaseStatusAckBodySize ||
		result.UpperResponses[0].Body[0] != 1 {
		t.Fatalf("op30 result=%+v", result)
	}
	if len(result.EquipmentSlotRefreshes) != 1 || result.EquipmentSlotRefreshes[0] != 14 {
		t.Fatalf("equipment refreshes=%v", result.EquipmentSlotRefreshes)
	}
}

func TestEquipmentInheritanceRejectionsUseExactMessagesAndDoNotMutate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*equipmentInheritanceFixture)
		message string
	}{
		{
			name: "source missing",
			mutate: func(f *equipmentInheritanceFixture) {
				delete(f.inventory.Slots, slotKey(listTypeMain, 9))
			},
			message: equipmentInheritanceMissingSourceText,
		},
		{
			name: "target missing",
			mutate: func(f *equipmentInheritanceFixture) {
				delete(f.equipment.Entries, "14")
			},
			message: equipmentInheritanceMissingTargetText,
		},
		{
			name: "unsupported type",
			mutate: func(f *equipmentInheritanceFixture) {
				meta := f.metadata[equipmentInheritanceTestSourceID]
				meta.EquipmentType = "`[hat avatar]`"
				f.metadata[equipmentInheritanceTestSourceID] = meta
			},
			message: equipmentInheritanceForbiddenText,
		},
		{
			name: "excluded id",
			mutate: func(f *equipmentInheritanceFixture) {
				f.resolution.EquipmentInheritance.ExcludedEquipmentIDs = []int64{equipmentInheritanceTestTargetID}
			},
			message: equipmentInheritanceForbiddenText,
		},
		{
			name: "locked source",
			mutate: func(f *equipmentInheritanceFixture) {
				f.source.Extra["equipment_lock_state"] = "locked"
				f.inventory.Slots[slotKey(listTypeMain, 9)] = f.source
			},
			message: equipmentInheritanceForbiddenText,
		},
		{
			name: "expired target",
			mutate: func(f *equipmentInheritanceFixture) {
				f.target.ExpireAt = time.Now().Add(-time.Minute)
			},
			message: equipmentInheritanceForbiddenText,
		},
		{
			name: "rental source",
			mutate: func(f *equipmentInheritanceFixture) {
				f.source.Extra["source"] = "pvf_rental"
				f.inventory.Slots[slotKey(listTypeMain, 9)] = f.source
			},
			message: equipmentInheritanceForbiddenText,
		},
		{
			name: "rarity below artifact",
			mutate: func(f *equipmentInheritanceFixture) {
				meta := f.metadata[equipmentInheritanceTestSourceID]
				meta.Rarity = 2
				f.metadata[equipmentInheritanceTestSourceID] = meta
			},
			message: equipmentInheritanceRarityLevelText,
		},
		{
			name: "minimum level below 60",
			mutate: func(f *equipmentInheritanceFixture) {
				meta := f.metadata[equipmentInheritanceTestTargetID]
				meta.MinimumLevel = 59
				f.metadata[equipmentInheritanceTestTargetID] = meta
			},
			message: equipmentInheritanceRarityLevelText,
		},
		{
			name: "level interval above five",
			mutate: func(f *equipmentInheritanceFixture) {
				meta := f.metadata[equipmentInheritanceTestTargetID]
				meta.MinimumLevel = 66
				f.metadata[equipmentInheritanceTestTargetID] = meta
			},
			message: equipmentInheritanceLevelRangeText,
		},
		{
			name: "source has no build",
			mutate: func(f *equipmentInheritanceFixture) {
				f.source = equipmentInheritanceTestEquipment(9, equipmentInheritanceTestSourceID, 0)
				f.inventory.Slots[slotKey(listTypeMain, 9)] = f.source
			},
			message: equipmentInheritanceNoBuildText,
		},
		{
			name: "material insufficient",
			mutate: func(f *equipmentInheritanceFixture) {
				material := f.inventory.Slots[slotKey(listTypeMain, 20)]
				material.Count = 1
				f.inventory.Slots[slotKey(listTypeMain, 20)] = material
			},
			message: equipmentInheritanceMaterialText,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEquipmentInheritanceFixture(t)
			test.mutate(fixture)
			fixture.save(t)
			beforeInventory := loadTestInventory(t, context.Background(), fixture.repos, equipmentInheritanceTestCharacter)
			beforeEquipment, _, _ := fixture.repos.Equipment.Load(context.Background(), equipmentInheritanceTestCharacter)
			beforeCharacter, _, _ := fixture.repos.Character.Load(context.Background(), equipmentInheritanceTestCharacter)

			result, err := fixture.owner.useEquipmentInheritance(context.Background(), fixture.cmd, fixture.resolution)
			if err != nil {
				t.Fatal(err)
			}
			if !result.EquipmentInheritanceRejected || result.EquipmentInheritanceApplied ||
				result.EquipmentInheritanceMessage != test.message {
				t.Fatalf("result=%+v want_message=%q", result, test.message)
			}
			afterInventory := loadTestInventory(t, context.Background(), fixture.repos, equipmentInheritanceTestCharacter)
			afterEquipment, _, _ := fixture.repos.Equipment.Load(context.Background(), equipmentInheritanceTestCharacter)
			afterCharacter, _, _ := fixture.repos.Character.Load(context.Background(), equipmentInheritanceTestCharacter)
			if !itemMapsEqualForEquipmentInheritance(beforeInventory.Slots, afterInventory.Slots) ||
				afterCharacter.Stats["gold"] != beforeCharacter.Stats["gold"] ||
				afterEquipment.Entries["14"].ItemID != beforeEquipment.Entries["14"].ItemID {
				t.Fatalf("rejected inheritance mutated assets: inventory=%+v equipment=%+v gold=%d",
					afterInventory.Slots, afterEquipment.Entries, afterCharacter.Stats["gold"])
			}
		})
	}
}

func newEquipmentInheritanceFixture(t *testing.T) *equipmentInheritanceFixture {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	source := equipmentInheritanceTestEquipment(9, equipmentInheritanceTestSourceID, 1)
	setUpgradeLevel(&source, 12)
	setAmplifyState(&source, 2, 12)
	setForgingLevel(&source, 6)
	setEnchantFields(&source, 12345, 4)
	equipmentInheritanceApplyEmblems(&source, [equipmentInheritanceEmblemBytes]byte{
		2, 0xFF, 0xFF, 0xFF, 0xFF, 0x39, 0x30, 0, 0,
	}, 0x04)

	target := equipmentInheritanceTestEquipment(14, equipmentInheritanceTestTargetID, 1)
	setUpgradeLevel(&target, 7)
	setAmplifyState(&target, 3, 7)
	setForgingLevel(&target, 2)
	setEnchantFields(&target, 999, 1)
	equipmentInheritanceApplyEmblems(&target, [equipmentInheritanceEmblemBytes]byte{
		1, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0,
	}, 0x04)

	coupon := dnfrepo.ItemStack{
		ItemID: equipmentInheritanceTestCoupon, Count: 1,
		Extra: map[string]string{"item_kind": "stackable", "pvf_path": "stackable/cash/equipment_inheritance_coupon_202611001.stk", "stackable_type": "[etc]"},
	}
	coupon.RawEntry = currentRawEntryForStack(10, coupon)
	material := dnfrepo.ItemStack{
		ItemID: equipmentInheritanceTestMaterial, Count: 3,
		Extra: map[string]string{"item_kind": "stackable", "pvf_path": "stackable/material/inheritance_material.stk", "stackable_type": "[material]"},
	}
	material.RawEntry = currentRawEntryForStack(20, material)
	inventory := dnfrepo.InventoryRecord{
		CharacterID: equipmentInheritanceTestCharacter,
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 9):  source,
			slotKey(listTypeMain, 10): coupon,
			slotKey(listTypeMain, 20): material,
		},
	}
	equipment := dnfrepo.EquipmentRecord{
		CharacterID: equipmentInheritanceTestCharacter,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"14": {
				SlotIndex: 14, ItemID: target.ItemID, RawEntry: append([]byte(nil), target.RawEntry...),
				Extra: cloneStack(target).Extra,
			},
		},
	}
	character := dnfrepo.CharacterRecord{
		CharacterID: equipmentInheritanceTestCharacter,
		AccountID:   "dnf:inheritance",
		Stats:       map[string]int64{"gold": 1000},
	}
	metadata := map[int64]alignedcmd.EquipmentInheritanceEquipment{
		equipmentInheritanceTestSourceID: {
			ItemID: equipmentInheritanceTestSourceID, PVFPath: "equipment/character/common/coat_9001.equ",
			EquipmentType: "`[coat]`", Rarity: 3, MinimumLevel: 60,
		},
		equipmentInheritanceTestTargetID: {
			ItemID: equipmentInheritanceTestTargetID, PVFPath: "equipment/character/common/coat_9002.equ",
			EquipmentType: "`[coat]`", Rarity: 4, MinimumLevel: 65,
		},
	}
	resolution := alignedcmd.StackableUseResolution{
		ItemID:        equipmentInheritanceTestCoupon,
		PVFPath:       "stackable/cash/equipment_inheritance_coupon_202611001.stk",
		StackableType: "[etc]",
		EquipmentInheritance: &alignedcmd.EquipmentInheritanceResolution{
			Materials: []alignedcmd.EquipmentInheritanceMaterial{
				{ItemID: 0, Count: 100},
				{ItemID: equipmentInheritanceTestMaterial, Count: 2},
			},
			ResolveEquipment: func(itemID int64) (alignedcmd.EquipmentInheritanceEquipment, error) {
				return metadata[itemID], nil
			},
		},
	}
	fixture := &equipmentInheritanceFixture{
		repos: repos, owner: owner,
		cmd: Command{
			AccountID: "dnf:inheritance", SelectedCharacterID: 77,
			Operation: "use_stackable", SourceListType: listTypeMain, SourceSlotIndex: 10,
			SourceInstanceValue: 0x12345678, ItemTemplateID: int32(equipmentInheritanceTestCoupon),
		},
		resolution: resolution, inventory: inventory, equipment: equipment, character: character,
		metadata: metadata, source: source, target: target,
	}
	fixture.save(t)
	return fixture
}

func (fixture *equipmentInheritanceFixture) save(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, exists := fixture.inventory.Slots[slotKey(listTypeMain, 9)]; exists {
		fixture.inventory.Slots[slotKey(listTypeMain, 9)] = fixture.source
	}
	if _, exists := fixture.equipment.Entries["14"]; exists {
		fixture.equipment.Entries["14"] = dnfrepo.EquipmentEntry{
			SlotIndex: 14, ItemID: fixture.target.ItemID,
			ExpireAt: fixture.target.ExpireAt,
			RawEntry: append([]byte(nil), fixture.target.RawEntry...),
			Extra:    cloneStack(fixture.target).Extra,
		}
	}
	if err := fixture.repos.Inventory.Save(ctx, fixture.inventory); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repos.Equipment.Save(ctx, fixture.equipment); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repos.Character.Save(ctx, fixture.character); err != nil {
		t.Fatal(err)
	}
}

func equipmentInheritanceTestEquipment(slot int16, itemID int64, rarity int64) dnfrepo.ItemStack {
	stack := dnfrepo.ItemStack{
		ItemID: itemID, Count: 1,
		RawEntry: make([]byte, currentItemListEntrySize),
		Extra: map[string]string{
			"item_kind":      "equipment",
			"pvf_path":       "equipment/character/common/coat_test.equ",
			"equipment_type": "[coat]",
			"rarity":         "3",
		},
	}
	stack.Extra["rarity"] = string(rune('0' + rarity))
	return stack
}

func itemMapsEqualForEquipmentInheritance(left, right map[string]dnfrepo.ItemStack) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftStack := range left {
		rightStack, found := right[key]
		if !found || leftStack.ItemID != rightStack.ItemID || leftStack.Count != rightStack.Count {
			return false
		}
	}
	return true
}
