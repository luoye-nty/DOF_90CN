package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestParseCurrentVendingMachineLiveRequests(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want currentVendingMachineRequest
	}{
		{
			name: "normal black-diamond vending machine",
			body: []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0},
			want: currentVendingMachineRequest{MachineID: 1, DrawCount: 1, MaterialSlot: 140},
		},
		{
			name: "advanced black-diamond vending machine",
			body: []byte{3, 0, 0, 0, 1, 0, 0, 0, 0x8e, 0},
			want: currentVendingMachineRequest{MachineID: 3, DrawCount: 1, MaterialSlot: 142},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCurrentVendingMachineRequest(test.body)
			if err != nil || got != test.want {
				t.Fatalf("request=%+v error=%v want=%+v", got, err, test.want)
			}
		})
	}
}

func TestParseCurrentVendingMachineRejectsForgedShape(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		{1, 0, 0, 0, 2, 0, 0, 0, 1, 0},
		{2, 0, 0, 0, 1, 0, 0, 0, 1, 0},
		{1, 0, 0, 0, 1, 0, 0, 0, 0xff, 0xff},
	} {
		if request, err := parseCurrentVendingMachineRequest(body); err == nil {
			t.Fatalf("body=%x unexpectedly parsed as %+v", body, request)
		}
	}
}

func TestRollCurrentVendingMachineOutputUsesRelativePositiveWeights(t *testing.T) {
	group := currentVendingMachineGroup{
		TotalWeight: 5,
		Outputs: []currentVendingMachineOutput{
			{ItemID: 10, Weight: 0, Count: 1},
			{ItemID: 11, Weight: 2, Count: 3},
			{ItemID: 12, Weight: 3, Count: 4},
		},
	}
	service := &Service{vendingMachineRoll: func(limit uint64) (uint64, error) {
		if limit != 5 {
			t.Fatalf("limit=%d want=5", limit)
		}
		return 2, nil
	}}
	got, err := service.rollCurrentVendingMachineOutput(group)
	if err != nil || got.ItemID != 12 || got.Count != 4 {
		t.Fatalf("output=%+v error=%v", got, err)
	}
}

func TestBuildCurrentVendingMachineSuccessBodyMatchesCurrentEXEReader(t *testing.T) {
	var entry currentItemListEntry
	entry.patchCore(77, 123456, 9)
	body, err := buildCurrentVendingMachineSuccessBody(currentVendingMachineSuccess{
		MaterialSlot:      140,
		MaterialRemaining: 6,
		RewardItemID:      123456,
		RewardCount:       9,
		RewardRows:        []currentVendingMachineRewardRow{{ListType: 0, Entry: entry}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 25+1+currentItemListEntryWireSize {
		t.Fatalf("body len=%d", len(body))
	}
	if body[0] != 1 || binary.LittleEndian.Uint16(body[1:3]) != 140 ||
		binary.LittleEndian.Uint32(body[3:7]) != 6 ||
		binary.LittleEndian.Uint32(body[7:11]) != 123456 ||
		binary.LittleEndian.Uint32(body[11:15]) != 9 ||
		binary.LittleEndian.Uint32(body[15:19]) != 0 ||
		binary.LittleEndian.Uint32(body[19:23]) != 0 ||
		binary.LittleEndian.Uint16(body[23:25]) != 1 || body[25] != 0 ||
		!bytes.Equal(body[26:], entry.data[:]) {
		t.Fatalf("success body=%x", body)
	}
	if failure := buildCurrentVendingMachineFailureBody(4); !bytes.Equal(failure, []byte{0, 4}) {
		t.Fatalf("failure=%x", failure)
	}
}
