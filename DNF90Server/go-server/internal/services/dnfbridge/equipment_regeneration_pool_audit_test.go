package dnfbridge

import (
	"fmt"
	"os"
	"sort"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestEquipmentRegenerationRealPVFPoolsNeverCollapse walks every regeneration
// source the active PVF accepts and reports the size of each pool op407 can
// draw from. A pool of one whose only member is the source itself is the live
// bug: the player pays the soul cost and gets their own piece back.
func TestEquipmentRegenerationRealPVFPoolsNeverCollapse(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to audit op407 pools against the active PVF")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCurrentEquipmentRegenerationCatalog(items)
	if err != nil {
		t.Fatal(err)
	}

	groupPart := make(map[string]uint16, 128)
	partNames := make([]uint16, 0, len(catalog.parts))
	for part, groups := range catalog.parts {
		partNames = append(partNames, part)
		for group := range groups {
			groupPart[group] = part
		}
	}
	sort.Slice(partNames, func(i, j int) bool { return partNames[i] < partNames[j] })

	// The live filter splits on legacy too: a 传承 piece only ever draws other
	// 传承 pieces, and vice versa. Auditing without that dimension overstates
	// every pool.
	type key struct {
		part   uint16
		rarity int64
		level  int64
		legacy bool
	}
	sizes := make(map[key]int)
	members := make(map[key][]int64)
	for _, candidate := range catalog.byItemID {
		part, ok := groupPart[candidate.itemGroup]
		if !ok || candidate.attachType != "sealing" {
			continue
		}
		id := key{part: part, rarity: candidate.rarity, level: candidate.minimumLevel, legacy: candidate.legacy}
		sizes[id]++
		if len(members[id]) < 4 {
			members[id] = append(members[id], candidate.itemID)
		}
	}

	keys := make([]key, 0, len(sizes))
	for k := range sizes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].part != keys[j].part {
			return keys[i].part < keys[j].part
		}
		if keys[i].rarity != keys[j].rarity {
			return keys[i].rarity < keys[j].rarity
		}
		if keys[i].level != keys[j].level {
			return keys[i].level < keys[j].level
		}
		return !keys[i].legacy && keys[j].legacy
	})

	collapsed := make([]string, 0, 8)
	for _, k := range keys {
		size := sizes[k]
		if size <= 1 {
			collapsed = append(collapsed, fmt.Sprintf("part=%d rarity=%d level=%d legacy=%t size=%d members=%v", k.part, k.rarity, k.level, k.legacy, size, members[k]))
		}
	}
	t.Logf("pool keys=%d", len(keys))
	for _, k := range keys {
		t.Logf("  part=%d rarity=%d level=%-3d legacy=%-5t size=%d", k.part, k.rarity, k.level, k.legacy, sizes[k])
	}
	if len(collapsed) > 0 {
		t.Logf("collapsed pools (%d):", len(collapsed))
		for _, line := range collapsed {
			t.Logf("  %s", line)
		}
	}

	// The guarantee: no source may be charged for a draw that can only return
	// itself. Either its band holds another candidate, or the resolver refuses
	// the attempt outright.
	for _, k := range keys {
		if sizes[k] > 1 {
			continue
		}
		sole := members[k][0]
		resolution, resolveErr := catalog.resolve(sole, 0, k.part)
		if resolveErr != nil {
			t.Fatalf("resolve sole candidate %d: %v", sole, resolveErr)
		}
		if resolution.Eligible {
			t.Fatalf("item %d (part=%d rarity=%d level=%d legacy=%t) is the only candidate in its band yet regeneration was accepted: %+v",
				sole, k.part, k.rarity, k.level, k.legacy, resolution)
		}
	}

	// Weapons are the reported case: every band a player can actually reach
	// has to offer a real draw.
	for _, k := range keys {
		if k.part != 1 || sizes[k] <= 1 {
			continue
		}
		if sizes[k] < 3 {
			t.Fatalf("weapon band part=%d rarity=%d level=%d legacy=%t has only %d candidates", k.part, k.rarity, k.level, k.legacy, sizes[k])
		}
	}
}
