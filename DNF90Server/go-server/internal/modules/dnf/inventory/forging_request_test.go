package inventory

import (
	"encoding/hex"
	"testing"
)

func TestDecodeSeparateUpgradeRequestCapturedCurrentClientBody(t *testing.T) {
	body, err := hex.DecodeString("030b00dd4c05068600160000002b313320e88d92e58fa4e98197e5b098e5a4aae5888000")
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeSeparateUpgradeRequest(body)
	if err != nil {
		t.Fatalf("DecodeSeparateUpgradeRequest error = %v", err)
	}
	if request.TargetListType != 3 || request.TargetSlotIndex != 11 || request.TargetItemTemplateID != 101010653 || request.MaterialSlotIndex != 134 || request.TargetItemName != "+13 荒古遗尘太刀" || request.TrailingFlag != 0 {
		t.Fatalf("request = %+v", request)
	}
}

func TestDecodeSeparateUpgradeRequestRejectsLengthMismatch(t *testing.T) {
	valid := separateUpgradeRequestBodyForTest(0, 25, 700, 121, "test")
	for _, body := range [][]byte{
		valid[:len(valid)-1],
		append(append([]byte(nil), valid...), 0),
		[]byte{0, 1, 0},
	} {
		if _, err := DecodeSeparateUpgradeRequest(body); err == nil {
			t.Fatalf("body %x unexpectedly decoded", body)
		}
	}
}

func separateUpgradeRequestBodyForTest(listType byte, targetSlot int16, targetItemID int32, materialSlot int16, name string) []byte {
	body := make([]byte, 0, 14+len(name))
	body = append(body, listType, byte(targetSlot), byte(targetSlot>>8))
	body = append(body, byte(targetItemID), byte(targetItemID>>8), byte(targetItemID>>16), byte(targetItemID>>24))
	body = append(body, byte(materialSlot), byte(materialSlot>>8))
	nameLength := uint32(len(name))
	body = append(body, byte(nameLength), byte(nameLength>>8), byte(nameLength>>16), byte(nameLength>>24))
	body = append(body, name...)
	body = append(body, 0)
	return body
}
