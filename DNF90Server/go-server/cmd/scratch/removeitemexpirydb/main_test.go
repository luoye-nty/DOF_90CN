package main

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestMakePermanentClearsStackableExpirationProjection(t *testing.T) {
	expiresAt := time.Unix(1_849_989_600, 0).UTC()
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[usePeriodOffset:usePeriodOffset+2], 0xFFFF)
	binary.LittleEndian.PutUint32(raw[legacyExpireOffset:legacyExpireOffset+4], uint32(expiresAt.Unix()))
	binary.LittleEndian.PutUint32(raw[itemExpireOffset:itemExpireOffset+4], uint32(expiresAt.Unix()))
	extra := map[string]string{
		"item_kind":          "stackable",
		"expire_time":        "1849989600",
		"expire_unix":        "1849989600",
		"usable_period_days": "7",
		"expiration_source":  "runtime_pvf_usable_period_grant",
	}
	patched, result := makePermanent(raw, sql.NullTime{Time: expiresAt, Valid: true}, extra, itemKinds{Stackable: true})
	if !result.changed || result.preserved {
		t.Fatalf("result=%+v", result)
	}
	if binary.LittleEndian.Uint16(patched[usePeriodOffset:usePeriodOffset+2]) != 0 ||
		binary.LittleEndian.Uint32(patched[legacyExpireOffset:legacyExpireOffset+4]) != 0 ||
		binary.LittleEndian.Uint32(patched[itemExpireOffset:itemExpireOffset+4]) != 0 {
		t.Fatalf("expiration projection was not cleared: %x", patched)
	}
	if binary.LittleEndian.Uint16(raw[usePeriodOffset:usePeriodOffset+2]) != 0xFFFF {
		t.Fatal("source raw entry was mutated")
	}
}

func TestMakePermanentPreservesEquipmentDurabilityAndUnrelatedLegacyWord(t *testing.T) {
	expiresAt := time.Unix(1_849_989_600, 0).UTC()
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint16(raw[usePeriodOffset:usePeriodOffset+2], 45)
	binary.LittleEndian.PutUint32(raw[legacyExpireOffset:legacyExpireOffset+4], 0x11223344)
	binary.LittleEndian.PutUint32(raw[itemExpireOffset:itemExpireOffset+4], uint32(expiresAt.Unix()))
	patched, result := makePermanent(raw, sql.NullTime{Time: expiresAt, Valid: true}, map[string]string{"item_kind": "equipment"}, itemKinds{Equipment: true})
	if !result.changed || binary.LittleEndian.Uint16(patched[usePeriodOffset:usePeriodOffset+2]) != 45 {
		t.Fatalf("equipment durability changed: result=%+v raw=%x", result, patched)
	}
	if binary.LittleEndian.Uint32(patched[legacyExpireOffset:legacyExpireOffset+4]) != 0x11223344 {
		t.Fatal("unrelated legacy word was cleared")
	}
	if binary.LittleEndian.Uint32(patched[itemExpireOffset:itemExpireOffset+4]) != 0 {
		t.Fatal("equipment expiration timestamp remains")
	}
}

func TestMakePermanentPreservesIndependentRuntimeDurations(t *testing.T) {
	expiresAt := time.Unix(1_849_989_600, 0).UTC()
	for _, extra := range []map[string]string{
		{"source": "pvf_rental"},
		{"expiration_source": "runtime_pvf_cera_shop_duration_grant"},
	} {
		raw := make([]byte, 0x77)
		binary.LittleEndian.PutUint32(raw[itemExpireOffset:itemExpireOffset+4], uint32(expiresAt.Unix()))
		patched, result := makePermanent(raw, sql.NullTime{Time: expiresAt, Valid: true}, extra, itemKinds{Equipment: true})
		if !result.preserved || result.changed || binary.LittleEndian.Uint32(patched[itemExpireOffset:itemExpireOffset+4]) == 0 {
			t.Fatalf("runtime duration was not preserved: result=%+v raw=%x", result, patched)
		}
	}
}

func TestPatchSQLiteInventoryPurgesOnlyDefinitionExpiration(t *testing.T) {
	expiresAt := time.Unix(1_849_989_600, 0).UTC()
	raw := make([]byte, 0x77)
	binary.LittleEndian.PutUint32(raw[itemExpireOffset:itemExpireOffset+4], uint32(expiresAt.Unix()))
	record := dnfrepo.InventoryRecord{
		CharacterID: "7",
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: 100, Count: 1, ExpireAt: expiresAt, RawEntry: raw, Extra: map[string]string{"item_kind": "stackable", "expire_time": "1849989600"}},
			"0:2": {ItemID: 200, Count: 1, ExpireAt: expiresAt, RawEntry: append([]byte(nil), raw...), Extra: map[string]string{"source": "pvf_rental"}},
		},
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	patched, targets, changed, preserved, err := patchSQLitePayload("inventory", payload, map[int64]itemKinds{100: {Stackable: true}, 200: {Equipment: true}})
	if err != nil {
		t.Fatal(err)
	}
	if targets != 2 || changed != 1 || preserved != 1 {
		t.Fatalf("targets=%d changed=%d preserved=%d", targets, changed, preserved)
	}
	var got dnfrepo.InventoryRecord
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Slots["0:1"].ExpireAt.IsZero() || got.Slots["0:1"].Extra["expire_time"] != "" ||
		binary.LittleEndian.Uint32(got.Slots["0:1"].RawEntry[itemExpireOffset:itemExpireOffset+4]) != 0 {
		t.Fatalf("definition expiration remains: %+v", got.Slots["0:1"])
	}
	if !got.Slots["0:2"].ExpireAt.Equal(expiresAt) {
		t.Fatal("runtime rental expiration changed")
	}
}
