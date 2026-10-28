package dnfbridge

import "testing"

// A freshly created equipment instance is sealed (+0x0D = 1) only when the
// runtime PVF says `[attach type] [sealing]`; rarity plays no part. Untradeable
// legendaries from quests, guild exchange and NPC gift boxes therefore arrive
// unsealed and untradeable instead of showing 贴膜.
func TestCurrentDungeonEquipmentDropAndPickupSealOnlySealingAttachType(t *testing.T) {
	for _, rarity := range []int64{0, 1, 2, 3, 4, 5, 6} {
		sealing := dungeonDropItemDefinition{
			ItemID:     9000 + uint32(rarity),
			Kind:       dungeonDropItemEquipment,
			Rarity:     rarity,
			AttachType: "`[sealing]`",
		}
		entry, err := currentDungeonDeathDropItemState(1, sealing, 1, 123456789)
		if err != nil {
			t.Fatal(err)
		}
		if entry.data[0x0D] != 1 {
			t.Fatalf("rarity=%d [sealing] scene seal=%d want=1", rarity, entry.data[0x0D])
		}
		stack, err := buildCurrentDungeonPickupStack(10, sealing, 1, 123456789)
		if err != nil {
			t.Fatal(err)
		}
		if stack.Extra["seal_flag"] != "1" || len(stack.RawEntry) <= 0x0D || stack.RawEntry[0x0D] != 1 {
			t.Fatalf("rarity=%d [sealing] pickup extra=%+v raw=%x", rarity, stack.Extra, stack.RawEntry)
		}
		for _, attach := range []string{"`[trade delete]`", "`[trade]`", "`[free]`", "`[account]`", "`[sealing trade]`", ""} {
			plain := sealing
			plain.ItemID = 9100 + uint32(rarity)
			plain.AttachType = attach
			entry, err := currentDungeonDeathDropItemState(1, plain, 1, 123456789)
			if err != nil {
				t.Fatal(err)
			}
			if entry.data[0x0D] != 0 {
				t.Fatalf("rarity=%d attach=%q scene seal=%d want=0", rarity, attach, entry.data[0x0D])
			}
			stack, err := buildCurrentDungeonPickupStack(10, plain, 1, 123456789)
			if err != nil {
				t.Fatal(err)
			}
			if _, sealed := stack.Extra["seal_flag"]; sealed || (len(stack.RawEntry) > 0x0D && stack.RawEntry[0x0D] != 0) {
				t.Fatalf("rarity=%d attach=%q pickup extra=%+v raw=%x want unsealed", rarity, attach, stack.Extra, stack.RawEntry)
			}
		}
	}
	stackable := dungeonDropItemDefinition{ItemID: 3037, Kind: dungeonDropItemStackable, AttachType: "`[sealing]`"}
	if currentDungeonDroppedEquipmentShouldBeSealed(stackable) {
		t.Fatal("stackables are never sealed")
	}
}
