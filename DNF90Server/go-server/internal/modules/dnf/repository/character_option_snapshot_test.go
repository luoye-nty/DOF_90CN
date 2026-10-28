package repository_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCharacterOptionSnapshotRoundTripAndCharacterIsolation(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	payload := bytes.Repeat([]byte{0xff}, dnfrepo.CharacterOptionSnapshotSize)
	payload[4] = 47
	payload[6] = 52
	payload[300] = 0x5a
	updatedAt := time.Date(2026, 8, 19, 1, 2, 3, 0, time.UTC)
	if err := dnfrepo.SaveCharacterOptionSnapshot(context.Background(), repositories.Settings, "19", payload, updatedAt); err != nil {
		t.Fatal(err)
	}
	payload[300] ^= 0xff

	loaded, found, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if loaded[4] != 47 || loaded[6] != 52 || loaded[300] != 0x5a {
		t.Fatalf("loaded payload was not preserved: %x", loaded)
	}
	loaded[300] ^= 0xff
	reloaded, _, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19")
	if err != nil || reloaded[300] != 0x5a {
		t.Fatalf("loaded snapshot aliases storage: value=%x err=%v", reloaded[300], err)
	}
	if other, otherFound, otherErr := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "20"); otherErr != nil || otherFound || other != nil {
		t.Fatalf("cross-character snapshot leaked: found=%t payload=%x err=%v", otherFound, other, otherErr)
	}
}

func TestCharacterOptionSnapshotRejectsMalformedPayloads(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, payload := range [][]byte{nil, make([]byte, dnfrepo.CharacterOptionSnapshotSize-1), make([]byte, dnfrepo.CharacterOptionSnapshotSize+1)} {
		if err := dnfrepo.SaveCharacterOptionSnapshot(context.Background(), repositories.Settings, "19", payload, time.Now()); err == nil {
			t.Fatalf("invalid payload length %d was accepted", len(payload))
		}
	}
	if err := repositories.Settings.Save(context.Background(), dnfrepo.SettingsRecord{
		Scope:  dnfrepo.CharacterOptionSnapshotScope("19"),
		Values: map[string]string{"format": "opaque-512-base64-v1", "payload_base64": "%%%"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19"); err == nil {
		t.Fatal("corrupt stored snapshot was accepted")
	}
}
