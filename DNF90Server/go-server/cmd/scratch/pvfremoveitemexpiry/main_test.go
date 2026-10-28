package main

import (
	"encoding/binary"
	"testing"
)

func TestRemoveItemTimeLimitSectionsRemovesBothFieldsAndPreservesNeighbors(t *testing.T) {
	resolved := map[int]string{
		1: "[name]",
		2: "[expiration date]",
		3: "[usable period]",
		4: "[action type]",
		5: "[level up ticket]",
	}
	raw := encodeTokens(
		rawToken{typ: 3, value: 1},
		rawToken{typ: 6, value: 5},
		rawToken{typ: 3, value: 2},
		rawToken{typ: 6, value: 99},
		rawToken{typ: 3, value: 3},
		rawToken{typ: 0, value: 7},
		rawToken{typ: 3, value: 4},
		rawToken{typ: 6, value: 5},
	)
	patched, counts, err := removeItemTimeLimitSections(raw, func(value int) string { return resolved[value] })
	if err != nil {
		t.Fatal(err)
	}
	want := encodeTokens(
		rawToken{typ: 3, value: 1},
		rawToken{typ: 6, value: 5},
		rawToken{typ: 3, value: 4},
		rawToken{typ: 6, value: 5},
	)
	if string(patched) != string(want) {
		t.Fatalf("patched=%x want=%x", patched, want)
	}
	if counts["expiration date"] != 1 || counts["usable period"] != 1 {
		t.Fatalf("counts=%v", counts)
	}
}

func TestRemoveItemTimeLimitSectionsRemovesRepeatedAndEmptyFields(t *testing.T) {
	resolved := map[int]string{1: "[expiration date]", 2: "[name]"}
	raw := encodeTokens(
		rawToken{typ: 3, value: 1},
		rawToken{typ: 3, value: 1},
		rawToken{typ: 6, value: 10},
		rawToken{typ: 3, value: 2},
		rawToken{typ: 6, value: 11},
	)
	patched, counts, err := removeItemTimeLimitSections(raw, func(value int) string { return resolved[value] })
	if err != nil {
		t.Fatal(err)
	}
	want := encodeTokens(rawToken{typ: 3, value: 2}, rawToken{typ: 6, value: 11})
	if string(patched) != string(want) || counts["expiration date"] != 2 {
		t.Fatalf("patched=%x counts=%v want=%x", patched, counts, want)
	}
}

func TestRemoveItemTimeLimitSectionsRejectsPartialToken(t *testing.T) {
	if _, _, err := removeItemTimeLimitSections([]byte{3, 1, 2}, func(int) string { return "" }); err == nil {
		t.Fatal("partial token stream was accepted")
	}
}

func TestRewriteItemTimeLimitSectionsMakesEffectExpirationUnlimited(t *testing.T) {
	resolved := map[int]string{
		1: "[damage font info]",
		2: "[expiration info]",
		3: "[date]",
		4: "2048-12-31 06:00:00",
		5: "[/damage font info]",
		6: "[unlimit]",
	}
	raw := encodeTokens(
		rawToken{typ: 3, value: 1},
		rawToken{typ: 3, value: 2},
		rawToken{typ: 6, value: 3},
		rawToken{typ: 6, value: 4},
		rawToken{typ: 3, value: 5},
	)
	patched, counts, err := rewriteItemTimeLimitSections(raw, func(value int) string { return resolved[value] }, 6)
	if err != nil {
		t.Fatal(err)
	}
	want := encodeTokens(
		rawToken{typ: 3, value: 1},
		rawToken{typ: 3, value: 2},
		rawToken{typ: 6, value: 6},
		rawToken{typ: 3, value: 5},
	)
	if string(patched) != string(want) || counts.Transformed["expiration info"] != 1 {
		t.Fatalf("patched=%x counts=%+v want=%x", patched, counts, want)
	}
	rewritten, secondCounts, err := rewriteItemTimeLimitSections(patched, func(value int) string { return resolved[value] }, 6)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) != string(want) || countPatchSections(secondCounts) != 0 {
		t.Fatalf("second rewrite was not idempotent: patched=%x counts=%+v", rewritten, secondCounts)
	}
}

type rawToken struct {
	typ   byte
	value int32
}

func encodeTokens(tokens ...rawToken) []byte {
	out := make([]byte, len(tokens)*scriptTokenSize)
	for index, token := range tokens {
		offset := index * scriptTokenSize
		out[offset] = token.typ
		binary.LittleEndian.PutUint32(out[offset+1:offset+scriptTokenSize], uint32(token.value))
	}
	return out
}
