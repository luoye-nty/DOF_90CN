package dnfbridge

import (
	"context"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestDecodeCurrentNewbieGuideOption(t *testing.T) {
	tests := []struct {
		name  string
		body  []byte
		index uint32
		ok    bool
	}{
		{name: "village warp gate", body: []byte{0x14, 0, 0, 0}, index: 20, ok: true},
		{name: "pre town warp gate", body: []byte{0x15, 0, 0, 0}, index: 21, ok: true},
		{name: "short", body: []byte{0x15, 0, 0}, ok: false},
		{name: "out of range", body: []byte{0x00, 0x10, 0, 0}, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, ok := decodeCurrentNewbieGuideOption(tt.body)
			if ok != tt.ok || index != tt.index {
				t.Fatalf("decode = (%d, %v), want (%d, %v)", index, ok, tt.index, tt.ok)
			}
		})
	}
}

func TestCurrentNewbieGuideSeenIsAccountScopedPerGuide(t *testing.T) {
	ctx := context.Background()
	repositories := memory.NewMemoryGroup()
	now := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)

	for _, index := range []uint32{20, 21} {
		added, err := markCurrentNewbieGuideSeen(ctx, repositories.Settings, "dnf:test", index, now)
		if err != nil {
			t.Fatalf("mark guide %d: %v", index, err)
		}
		if !added {
			t.Fatalf("guide %d was not added", index)
		}
	}
	added, err := markCurrentNewbieGuideSeen(ctx, repositories.Settings, "dnf:test", 21, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("repeat mark guide 21: %v", err)
	}
	if added {
		t.Fatal("repeat mark must be idempotent")
	}

	seen, err := loadCurrentNewbieGuideSeen(ctx, repositories.Settings, "dnf:test")
	if err != nil {
		t.Fatalf("load seen: %v", err)
	}
	if want := []uint32{20, 21}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	otherAccount, err := loadCurrentNewbieGuideSeen(ctx, repositories.Settings, "dnf:other")
	if err != nil {
		t.Fatalf("load other account: %v", err)
	}
	if len(otherAccount) != 0 {
		t.Fatalf("other account inherited seen guides: %v", otherAccount)
	}
}
