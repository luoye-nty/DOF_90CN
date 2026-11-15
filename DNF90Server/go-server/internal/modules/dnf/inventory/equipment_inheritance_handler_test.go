package inventory

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestEquipmentInheritanceFailureReturnsOp44ErrorAndExactClientNotice(t *testing.T) {
	cmd := Command{
		Operation: "use_stackable", SourceListType: listTypeMain, SourceSlotIndex: 10,
		SourceInstanceValue: 0x12345678, ItemTemplateID: int32(equipmentInheritanceTestCoupon),
	}
	result := rejectedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketUseStackable), cmd, UseStackableResult{
		ItemID:                         equipmentInheritanceTestCoupon,
		EquipmentInheritanceSourceSlot: 9,
		EquipmentInheritanceTargetSlot: 14,
		EquipmentInheritanceRejected:   true,
		EquipmentInheritanceMessage:    equipmentInheritanceMaterialText,
	})
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if result.ClientNotice == nil || result.ClientNotice.Text != equipmentInheritanceMaterialText {
		t.Fatalf("client notice=%+v", result.ClientNotice)
	}
	if body := result.UpperResponses[0].Body; len(body) == 0 || body[0] != 0 || body[1] != useStackableSilentFailureCode {
		t.Fatalf("failure body=%x", body)
	}
}

func TestEquipmentInheritanceFailureReturnsOp515ErrorAndExactClientNotice(t *testing.T) {
	cmd := Command{
		Operation: "use_stackable_action", SourceListType: listTypeMain, SourceSlotIndex: 10,
		SourceInstanceValue: 0x12345678, ItemTemplateID: int32(equipmentInheritanceTestCoupon),
	}
	result := rejectedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketUseStackableAction), cmd, UseStackableResult{
		ItemID:                         equipmentInheritanceTestCoupon,
		EquipmentInheritanceSourceSlot: 9,
		EquipmentInheritanceTargetSlot: 14,
		EquipmentInheritanceRejected:   true,
		EquipmentInheritanceMessage:    equipmentInheritanceMaterialText,
	})
	if result.ClientNotice == nil || result.ClientNotice.Text != equipmentInheritanceMaterialText {
		t.Fatalf("client notice=%+v", result.ClientNotice)
	}
	want := []byte{0, useStackableActionFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, want) {
		t.Fatalf("op515 failure responses=%+v want=%x", result.UpperResponses, want)
	}
}

func TestEquipmentInheritanceFailureReturnsType251ErrorAndExactClientNotice(t *testing.T) {
	cmd := Command{
		Operation: "use_shared_effect_item", SourceListType: listTypeMain, SourceSlotIndex: 10,
		ItemTemplateID: int32(equipmentInheritanceTestCoupon),
	}
	result := rejectedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketUseSharedEffectItem), cmd, UseStackableResult{
		ItemID:                         equipmentInheritanceTestCoupon,
		EquipmentInheritanceSourceSlot: 9,
		EquipmentInheritanceTargetSlot: 14,
		EquipmentInheritanceRejected:   true,
		EquipmentInheritanceMessage:    equipmentInheritanceNoBuildText,
	})
	if result.ClientNotice == nil || result.ClientNotice.Text != equipmentInheritanceNoBuildText {
		t.Fatalf("client notice=%+v", result.ClientNotice)
	}
	want := []byte{0, useStackableSilentFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, want) {
		t.Fatalf("type251 failure responses=%+v want=%x", result.UpperResponses, want)
	}
}

func TestEquipmentInheritanceFailureReturnsOp30Error(t *testing.T) {
	cmd := Command{
		Operation: "increase_status_book", SourceListType: listTypeMain, SourceSlotIndex: 10,
	}
	result := rejectedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketIncreaseStatus), cmd, UseStackableResult{
		ItemID:                         equipmentInheritanceTestCoupon,
		EquipmentInheritanceSourceSlot: 9,
		EquipmentInheritanceTargetSlot: 14,
		EquipmentInheritanceRejected:   true,
		EquipmentInheritanceMessage:    equipmentInheritanceMaterialText,
	})
	if result.ClientNotice == nil || result.ClientNotice.Text != equipmentInheritanceMaterialText {
		t.Fatalf("client notice=%+v", result.ClientNotice)
	}
	want := []byte{0, increaseStatusFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, want) {
		t.Fatalf("op30 failure responses=%+v want=%x", result.UpperResponses, want)
	}
}

func TestEquipmentInheritanceSuccessRefreshesBackpackWornSlotAndGold(t *testing.T) {
	cmd := Command{
		Operation: "use_stackable", SourceListType: listTypeMain, SourceSlotIndex: 10,
		SourceInstanceValue: 0x12345678, ItemTemplateID: int32(equipmentInheritanceTestCoupon),
	}
	result := ownerAppliedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketUseStackable), cmd, UseStackableResult{
		ListType:                         listTypeMain,
		SlotIndex:                        10,
		InstanceValue:                    0x12345678,
		ItemID:                           equipmentInheritanceTestCoupon,
		RemainingCount:                   1,
		EquipmentInheritanceApplied:      true,
		EquipmentInheritanceSourceSlot:   9,
		EquipmentInheritanceTargetSlot:   14,
		EquipmentInheritanceChangedSlots: []int16{9, 20},
		EquipmentInheritanceGoldBefore:   1000,
		EquipmentInheritanceGoldAfter:    900,
		EquipmentInheritanceGoldChanged:  true,
	})
	if result.ClientNotice == nil || result.ClientNotice.Text != "装备继承成功" {
		t.Fatalf("client notice=%+v", result.ClientNotice)
	}
	if len(result.EquipmentSlotRefreshes) != 1 || result.EquipmentSlotRefreshes[0] != 14 {
		t.Fatalf("equipment refreshes=%v", result.EquipmentSlotRefreshes)
	}
	if len(result.ItemListRebuilds) != 1 || result.ItemListRebuilds[0] != listTypeMain {
		t.Fatalf("item list rebuilds=%v", result.ItemListRebuilds)
	}
	if len(result.ItemSlotRefreshes) != 3 {
		t.Fatalf("item refreshes=%+v", result.ItemSlotRefreshes)
	}
	if result.CharacterGoldAfter == nil || *result.CharacterGoldAfter != 900 {
		t.Fatalf("gold after=%v", result.CharacterGoldAfter)
	}
}

func TestEquipmentInheritanceSuccessUsesType251ResultByte(t *testing.T) {
	cmd := Command{
		Operation: "use_shared_effect_item", SourceListType: listTypeMain, SourceSlotIndex: 10,
		ItemTemplateID: int32(equipmentInheritanceTestCoupon),
	}
	result := ownerAppliedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketUseSharedEffectItem), cmd, UseStackableResult{
		ListType:                         listTypeMain,
		SlotIndex:                        10,
		ItemID:                           equipmentInheritanceTestCoupon,
		RemainingCount:                   1,
		EquipmentInheritanceApplied:      true,
		EquipmentInheritanceSourceSlot:   9,
		EquipmentInheritanceTargetSlot:   14,
		EquipmentInheritanceChangedSlots: []int16{9, 20},
	})
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, []byte{1}) {
		t.Fatalf("type251 success responses=%+v", result.UpperResponses)
	}
	if len(result.EquipmentSlotRefreshes) != 1 || result.EquipmentSlotRefreshes[0] != 14 {
		t.Fatalf("equipment refreshes=%v", result.EquipmentSlotRefreshes)
	}
}

func TestEquipmentInheritanceSuccessUsesOp30SilentAck(t *testing.T) {
	cmd := Command{
		Operation: "increase_status_book", SourceListType: listTypeMain, SourceSlotIndex: 10,
	}
	result := ownerAppliedEquipmentInheritanceResult(uint16(dnfenum.CmdPacketIncreaseStatus), cmd, UseStackableResult{
		ListType:                         listTypeMain,
		SlotIndex:                        10,
		ItemID:                           equipmentInheritanceTestCoupon,
		RemainingCount:                   1,
		EquipmentInheritanceApplied:      true,
		EquipmentInheritanceSourceSlot:   9,
		EquipmentInheritanceTargetSlot:   14,
		EquipmentInheritanceChangedSlots: []int16{9, 20},
	})
	if len(result.UpperResponses) != 1 || len(result.UpperResponses[0].Body) != increaseStatusAckBodySize ||
		result.UpperResponses[0].Body[0] != 1 || result.UpperResponses[0].Body[1] != 10 ||
		result.UpperResponses[0].Body[3] != increaseStatusKindSilent {
		t.Fatalf("op30 success responses=%+v", result.UpperResponses)
	}
}
