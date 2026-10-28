package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
)

// TestAdventureNativeIndependentOffsets pins the native adventure-group payload
// layout against the client: the six u32 content counters live at their own
// offsets, and the 20 descriptor rows at 7348 are type:u8 then count:u16.
// Getting either wrong makes the client read the wrong numbers, so these
// offsets are asserted directly rather than through our own constants.
func TestAdventureNativeIndependentOffsets(t *testing.T) {
	p := adventuregroup.Projection{ContentCounts: [6]uint32{70000, 11, 12, 13, 14, 15}}
	p.ProfessionCounts[0] = 4
	p.ProfessionCounts[1] = 2
	p.Runtime.ShopPoints = [3]uint32{10, 20, 30}
	p.Runtime.BraveExperience = 7500000
	body := buildCurrentAdventureInfoBodyWithState(1, adventuregroup.Summary{ManageLevel: 8}, nil, "", 0, p)
	raw := body[18 : 18+currentAdventureInfoRawLength]
	for i, offset := range []int{16, 32, 24, 28, 36, 40} {
		if got := binary.LittleEndian.Uint32(raw[offset:]); got != p.ContentCounts[i] {
			t.Fatalf("native counter at %d=%d, want %d", offset, got, p.ContentCounts[i])
		}
	}
	if binary.LittleEndian.Uint32(raw[5260:]) != 10 || binary.LittleEndian.Uint32(raw[5264:]) != 7500000 {
		t.Fatalf("native brave balance/progress=%x", raw[5260:5268])
	}
	// Row 0 = type 0 / count 4, row 1 = type 1 / count 2.
	if raw[7348] != 0 || binary.LittleEndian.Uint16(raw[7349:]) != 4 ||
		raw[7351] != 1 || binary.LittleEndian.Uint16(raw[7352:]) != 2 {
		t.Fatalf("native profession descriptors=%x", raw[7348:7354])
	}
}
