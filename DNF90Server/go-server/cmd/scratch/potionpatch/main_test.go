package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestPatchPotionRemovesClientGates(t *testing.T) {
	raw := make([]byte, 45*scriptTokenSize)
	for index := range 45 {
		raw[index*scriptTokenSize] = 3
	}
	for _, index := range []int{16, 22, 31, 39} {
		raw[index*scriptTokenSize] = 0
	}
	raw[30*scriptTokenSize] = 6
	putTokenInt(raw, 16, 6)
	putTokenInt(raw, 22, 10000)
	putTokenInt(raw, 31, 4)
	putTokenInt(raw, 39, 1)

	path := filepath.Join(t.TempDir(), "potion.stk")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchPotion(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 41*scriptTokenSize {
		t.Fatalf("patched length=%d", len(got))
	}
	if level := int32(binary.LittleEndian.Uint32(got[16*scriptTokenSize+1 : 16*scriptTokenSize+5])); level != 1 {
		t.Fatalf("minimum level=%d", level)
	}
	// Four removed tokens precede the original subtype token 31, so it is 29.
	if subtype := int32(binary.LittleEndian.Uint32(got[29*scriptTokenSize+1 : 29*scriptTokenSize+5])); subtype != 0 {
		t.Fatalf("waste subtype=%d", subtype)
	}
}
