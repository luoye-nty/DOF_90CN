package dnfrobot

import (
	"encoding/binary"
	"testing"
)

func TestExpertStoreDefinitionsAndCreateBody(t *testing.T) {
	for _, test := range []struct {
		kind     string
		wireKind byte
		name     string
	}{
		{kind: "disjoint", wireKind: expertStoreDisjointWireKind, name: "分解机"},
		{kind: "enchant", wireKind: expertStoreEnchantWireKind, name: "附魔机"},
	} {
		kind, wireKind, encoded, err := expertStoreDefinition(test.kind)
		if err != nil {
			t.Fatalf("%s definition: %v", test.kind, err)
		}
		if kind != test.kind || wireKind != test.wireKind || len(encoded) == 0 {
			t.Fatalf("%s definition = kind %s wire %d name %x", test.kind, kind, wireKind, encoded)
		}
		body, err := buildExpertStoreCreateBody(wireKind, encoded, 500, 120, 240, 77)
		if err != nil {
			t.Fatalf("%s body: %v", test.kind, err)
		}
		if len(body) != 15+len(encoded) || body[0] != wireKind ||
			int(binary.LittleEndian.Uint32(body[1:5])) != len(encoded) {
			t.Fatalf("%s body header = %x", test.kind, body)
		}
		offset := 5 + len(encoded)
		if binary.LittleEndian.Uint32(body[offset:offset+4]) != 500 ||
			int16(binary.LittleEndian.Uint16(body[offset+4:offset+6])) != 120 ||
			int16(binary.LittleEndian.Uint16(body[offset+6:offset+8])) != 240 ||
			binary.LittleEndian.Uint16(body[offset+8:offset+10]) != 77 {
			t.Fatalf("%s body fields = %x", test.kind, body)
		}
	}
	if _, _, _, err := expertStoreDefinition("invalid"); err == nil {
		t.Fatal("invalid expert store kind was accepted")
	}
}
