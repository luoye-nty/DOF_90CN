package raid

import (
	"bytes"
	"testing"
)

func TestBuildNativeRaidAvailabilityBodyUsesCurrentThreeRowLayout(t *testing.T) {
	got := BuildNativeRaidAvailabilityBody()
	want := []byte{
		0x01, 0x00, 0x01, 0x00, 0x00,
		0x02, 0x00, 0x01, 0x00, 0x00,
		0x03, 0x00, 0x01, 0x00, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("native raid availability body=%x, want %x", got, want)
	}
	if len(got) != RaidAvailabilityRowCount*5 {
		t.Fatalf("native raid availability length=%d, want %d", len(got), RaidAvailabilityRowCount*5)
	}
}

func TestBuildRaidAvailabilityBodyRejectsWrongRowCount(t *testing.T) {
	if _, err := BuildRaidAvailabilityBody(nil); err != errRaidAvailabilityRowCount {
		t.Fatalf("nil rows error=%v, want %v", err, errRaidAvailabilityRowCount)
	}
	if _, err := BuildRaidAvailabilityBody(make([]RaidAvailabilityRow, RaidAvailabilityRowCount+1)); err != errRaidAvailabilityRowCount {
		t.Fatalf("extra rows error=%v, want %v", err, errRaidAvailabilityRowCount)
	}
}
