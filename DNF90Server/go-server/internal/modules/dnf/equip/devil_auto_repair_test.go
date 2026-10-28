package equip

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 魔王契约「自动修理」的服务端触发语义（2026-09-14）：
//   - 契约生效且该角色当天还有额度 -> 全身免费修满, 次数 +1;
//   - 没有契约 -> 空操作(ContractSkipped), 绝不落成收费修理;
//   - 当天 6 次用完 -> 同样空操作, 不修也不扣钱。

func devilAutoRepairRaw(itemID int64, durability uint16) []byte {
	raw := make([]byte, currentEquipmentRawEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], durability)
	return raw
}

func devilAutoRepairDurability(t *testing.T, ctx context.Context, repos dnfrepo.Group, key string) uint16 {
	t.Helper()
	record, found, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry, ok := record.Entries[key]
	if !ok {
		t.Fatalf("equipment key %q missing", key)
	}
	return binary.LittleEndian.Uint16(entry.RawEntry[0x0B:0x0D])
}

func devilAutoRepairDamage(t *testing.T, ctx context.Context, repos dnfrepo.Group, key string, durability uint16) {
	t.Helper()
	record, found, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry := record.Entries[key]
	entry.RawEntry = append([]byte(nil), entry.RawEntry...)
	binary.LittleEndian.PutUint16(entry.RawEntry[0x0B:0x0D], durability)
	record.Entries[key] = entry
	if err := repos.Equipment.Save(ctx, record); err != nil {
		t.Fatalf("save damaged equipment: %v", err)
	}
}

func TestRepairAllContractAutoRepairNeverChargesAndStopsAtDailyLimit(t *testing.T) {
	ctx := context.Background()
	const (
		characterID = "77"
		accountID   = "acc-devil-auto"
	)
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{
		CharacterID: characterID,
		AccountID:   accountID,
		Stats:       map[string]int64{"gold": 1234},
	})
	saveTestEquipment(t, ctx, repos, dnfrepo.EquipmentRecord{
		CharacterID: characterID,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: 700, RawEntry: devilAutoRepairRaw(700, 0)},
			"14": {SlotIndex: 14, ItemID: 701, RawEntry: devilAutoRepairRaw(701, 10)},
		},
	})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{}})
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: accountID}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	resolver := repairAllTestResolver(map[int64]string{700: "[weapon]", 701: "[coat]"})
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatalf("NewOwner error = %v", err)
	}
	command := RepairCommand{
		SelectedCharacterID: 77,
		AccountID:           accountID,
		SlotIndex:           -1,
		AutoRepair:          true,
		ContractAutoRepair:  true,
	}

	// 1) 没有契约: 空操作, 不修也不扣钱。
	skipped, err := owner.RepairAll(ctx, command, resolver)
	if err != nil {
		t.Fatalf("no-contract RepairAll error = %v", err)
	}
	if !skipped.ContractSkipped || skipped.FreeRepair || skipped.RepairedCount != 0 || skipped.UpdatedGold != 1234 {
		t.Fatalf("no-contract result = %+v, want ContractSkipped with untouched gold", skipped)
	}
	if got := devilAutoRepairDurability(t, ctx, repos, "14"); got != 10 {
		t.Fatalf("no-contract durability = %d, want untouched 10", got)
	}

	// 2) 契约生效: 全身修满, 免费, 次数 +1。
	account, found, err := repos.Account.Load(ctx, accountID)
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	premium.Upsert(&account, premium.DevilSlotType(premium.DevilSlotAutoRepair), 7*24*60*60, 1, time.Now().UTC())
	if err := repos.Account.Save(ctx, account); err != nil {
		t.Fatalf("save contract account: %v", err)
	}
	free, err := owner.RepairAll(ctx, command, resolver)
	if err != nil {
		t.Fatalf("contract RepairAll error = %v", err)
	}
	if !free.FreeRepair || free.ContractSkipped || free.Cost != 0 || free.UpdatedGold != 1234 || free.RepairedCount != 2 {
		t.Fatalf("contract result = %+v, want free repair of 2 targets at 1234 gold", free)
	}
	for _, key := range []string{"12", "14"} {
		if got := devilAutoRepairDurability(t, ctx, repos, key); got != 20 {
			t.Fatalf("slot %s durability = %d, want 20 (PVF max)", key, got)
		}
	}
	character, _, _ := repos.Character.Load(ctx, characterID)
	if used := premium.DailyUsage(character, premium.DevilSlotAutoRepair, time.Now().UTC()); used != 1 {
		t.Fatalf("daily usage = %d, want 1", used)
	}
	if character.Stats["gold"] != 1234 {
		t.Fatalf("gold = %d, want untouched 1234", character.Stats["gold"])
	}

	// 3) 把当天 6 次用完, 第 7 次必须是空操作: 不修、不收费。
	dailyLimit := int(premium.DailyLimit(premium.DevilSlotAutoRepair))
	for round := 1; round <= dailyLimit; round++ {
		devilAutoRepairDamage(t, ctx, repos, "14", 5)
		result, repairErr := owner.RepairAll(ctx, command, resolver)
		if repairErr != nil {
			t.Fatalf("round %d RepairAll error = %v", round, repairErr)
		}
		if round <= 5 {
			if !result.FreeRepair || result.RepairedCount != 1 {
				t.Fatalf("round %d result = %+v, want the remaining free repair", round, result)
			}
			continue
		}
		if !result.ContractSkipped || result.FreeRepair || result.RepairedCount != 0 {
			t.Fatalf("round %d (contract still active at the cap) result = %+v", round, result)
		}
	}
	if got := devilAutoRepairDurability(t, ctx, repos, "14"); got != 5 {
		t.Fatalf("daily-limit durability = %d, want untouched 5", got)
	}
	character, _, _ = repos.Character.Load(ctx, characterID)
	if used := premium.DailyUsage(character, premium.DevilSlotAutoRepair, time.Now().UTC()); used != premium.DailyLimit(premium.DevilSlotAutoRepair) {
		t.Fatalf("daily usage = %d, want %d", used, premium.DailyLimit(premium.DevilSlotAutoRepair))
	}
	if character.Stats["gold"] != 1234 {
		t.Fatalf("gold after the cap = %d, want untouched 1234 (never a paid fallback)", character.Stats["gold"])
	}
}
