package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestDropExpiredStacksUsesClientWireExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	slots := map[string]dnfrepo.ItemStack{
		"future": {
			ExpireAt: time.Unix(2000, 0),
		},
		"extra-expired": {
			ExpireAt: time.Unix(2000, 0),
			Extra:    map[string]string{"expire_time": "900"},
		},
		"extra-future": {
			Extra: map[string]string{"expire_unix": "2000"},
		},
		"expireat-expired": {
			ExpireAt: time.Unix(900, 0),
		},
		"no-expiry": {},
	}

	if removed := dropExpiredStacks(slots, now); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, key := range []string{"future", "extra-future", "no-expiry"} {
		if _, found := slots[key]; !found {
			t.Fatalf("future/no-expiry row %q was removed", key)
		}
	}
	for _, key := range []string{"extra-expired", "expireat-expired"} {
		if _, found := slots[key]; found {
			t.Fatalf("expired row %q survived", key)
		}
	}
}

func TestDropExpiredEquipmentEntriesHandlesCostumesAndNameTag(t *testing.T) {
	now := time.Unix(2000, 0)
	entries := map[string]dnfrepo.EquipmentEntry{
		"costume-expired": {
			SlotIndex: 14,
			Extra:     map[string]string{"expire_time": "1900"},
		},
		"name-tag-expired": {
			SlotIndex: 30,
			ExpireAt:  time.Unix(1900, 0),
		},
		"weapon-future": {
			SlotIndex: 12,
			Extra:     map[string]string{"expire_unix": "2100"},
		},
		"permanent": {
			SlotIndex: 11,
		},
	}

	if removed := dropExpiredEquipmentEntries(entries, now); removed != 2 {
		t.Fatalf("removed=%d want=2", removed)
	}
	for _, key := range []string{"weapon-future", "permanent"} {
		if _, found := entries[key]; !found {
			t.Fatalf("non-expired equipment %q was removed", key)
		}
	}
	for _, key := range []string{"costume-expired", "name-tag-expired"} {
		if _, found := entries[key]; found {
			t.Fatalf("expired equipment %q survived", key)
		}
	}
}

func TestSweepExpiredEquipmentRecordClearsExpiredNameTagState(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	now := time.Unix(2000, 0)
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1",
		Stats: map[string]int64{
			"level":                90,
			"name_tag_item_id":     2747693,
			"name_tag_expire_time": 1900,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "1",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"14": {
				SlotIndex: 14,
				ItemID:    490000100,
				ExpireAt:  time.Unix(1900, 0),
			},
			"30": {
				SlotIndex: 30,
				ItemID:    2747693,
				ExpireAt:  time.Unix(1900, 0),
				Extra:     map[string]string{"equipment_type": "[name tag]"},
			},
			"12": {
				SlotIndex: 12,
				ItemID:    101000100,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed, nameTagCleared, err := sweepExpiredEquipmentRecord(
		ctx, repos.Character, repos.Equipment, "1", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || !nameTagCleared {
		t.Fatalf("removed=%d nameTagCleared=%t", removed, nameTagCleared)
	}
	equipment, found, err := repos.Equipment.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	if _, found := equipment.Entries["14"]; found {
		t.Fatal("expired costume survived")
	}
	if _, found := equipment.Entries["30"]; found {
		t.Fatal("expired name tag survived")
	}
	if _, found := equipment.Entries["12"]; !found {
		t.Fatal("permanent weapon was removed")
	}
	character, found, err := repos.Character.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if character.Stats["name_tag_item_id"] != 0 ||
		character.Stats["name_tag_expire_time"] != 0 ||
		character.Stats["level"] != 90 {
		t.Fatalf("character stats=%+v", character.Stats)
	}
}
