package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 魔王契约「自动修理」的桥接触发测试（2026-09-14）。
//
// 现场根因：op48(耐久上报) 是全客户端唯一的耐久消耗入口，它原先只减点并回一行，
// 从不查契约 —— 所以"耐久被消耗到 0 → 自动修理"这条官方行为在服务端完全没实现。
// 这组测试钉住四条行为：
//  1. >0 → 0 的那一刻触发全身免费修理，并回变更行；
//  2. 本来就为 0 的装备不触发（不是一次消耗性归零）；
//  3. 没有契约不修（也绝不收费）；
//  4. 该角色当天 6 次用完不修（也绝不收费）。

const (
	devilAutoRepairTestCharacter = "77"
	devilAutoRepairTestAccount   = "acc-devil-auto-repair"
	devilAutoRepairTestWeapon    = int64(700)
	devilAutoRepairTestCoat      = int64(701)
)

func devilAutoRepairBridgeRaw(itemID int64, durability uint16) []byte {
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], durability)
	return raw
}

func devilAutoRepairBridgeFixture(
	t *testing.T,
	contractDays int,
	weaponDurability uint16,
	coatDurability uint16,
	dailyUsesAlreadySpent int64,
) (dnfrepo.Group, *Service, *gameSession, *bufferConn) {
	t.Helper()
	ctx := context.Background()
	repos := testRepositoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: devilAutoRepairTestCharacter,
		AccountID:   devilAutoRepairTestAccount,
		Stats:       map[string]int64{"gold": 5555},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: devilAutoRepairTestCharacter,
		Entries: map[string]dnfrepo.EquipmentEntry{
			"12": {SlotIndex: 12, ItemID: devilAutoRepairTestWeapon, RawEntry: devilAutoRepairBridgeRaw(devilAutoRepairTestWeapon, weaponDurability)},
			"14": {SlotIndex: 14, ItemID: devilAutoRepairTestCoat, RawEntry: devilAutoRepairBridgeRaw(devilAutoRepairTestCoat, coatDurability)},
		},
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: devilAutoRepairTestCharacter,
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	account := dnfrepo.AccountRecord{AccountID: devilAutoRepairTestAccount}
	if contractDays > 0 {
		premium.Upsert(&account, premium.DevilSlotType(premium.DevilSlotAutoRepair), int64(contractDays)*24*60*60, 1, time.Now().UTC())
	}
	if err := repos.Account.Save(ctx, account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if dailyUsesAlreadySpent > 0 {
		character, found, err := repos.Character.Load(ctx, devilAutoRepairTestCharacter)
		if err != nil || !found {
			t.Fatalf("load character for daily seed found=%t err=%v", found, err)
		}
		now := time.Now().UTC()
		for use := int64(0); use < dailyUsesAlreadySpent; use++ {
			if !premium.TryConsumeDaily(&character, premium.DevilSlotAutoRepair, now) {
				t.Fatalf("seed daily use %d failed", use)
			}
		}
		if err := repos.Character.Save(ctx, character); err != nil {
			t.Fatalf("save daily-seeded character: %v", err)
		}
	}
	service := &Service{
		options:            options{accountID: devilAutoRepairTestAccount},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
		repairCostCatalog: &currentRepairCostCatalog{
			evidence: map[int64]alignedcmd.RepairCostEvidence{
				devilAutoRepairTestWeapon: {MaxDurability: 45, EquipmentType: "[weapon]"},
				devilAutoRepairTestCoat:   {MaxDurability: 45, EquipmentType: "[coat]"},
			},
		},
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "devil-auto-repair",
		selectedCharacterID: 77,
		accountID:           devilAutoRepairTestAccount,
		dungeon: dungeonSessionState{runtime: &runtimeDungeonState{
			Character: dnfrepo.CharacterRecord{CharacterID: devilAutoRepairTestCharacter},
			Session:   &worldmap.DungeonSession{},
			Room:      &runtimeDungeonRoom{},
		}},
	}
	return repos, service, session, connection
}

func devilAutoRepairBridgeDurability(t *testing.T, ctx context.Context, repos dnfrepo.Group, key string) uint16 {
	t.Helper()
	record, found, err := repos.Equipment.Load(ctx, devilAutoRepairTestCharacter)
	if err != nil || !found {
		t.Fatalf("load equipment found=%t err=%v", found, err)
	}
	entry, ok := record.Entries[key]
	if !ok {
		t.Fatalf("equipment key %q missing", key)
	}
	return binary.LittleEndian.Uint16(entry.RawEntry[0x0B:0x0D])
}

func TestDungeonDurabilityZeroTriggersDevilContractAutoRepair(t *testing.T) {
	ctx := context.Background()
	repos, service, session, connection := devilAutoRepairBridgeFixture(t, 7, 1, 30, 0)
	if err := service.handleDungeonDecreaseDurability(session, []byte{12}); err != nil {
		t.Fatalf("handle op48: %v", err)
	}

	// 第一包：op48 自己的一行 delta，武器 1 -> 0。
	first, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if first.Header.Classification != 0 || first.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("op48 refresh header class=%d msg=%d", first.Header.Classification, first.Header.MsgID)
	}
	if first.Body[0] != currentSocketListEquipment ||
		binary.LittleEndian.Uint16(first.Body[3+0x0B:3+0x0D]) != 0 {
		t.Fatalf("op48 refresh body=%x, want the zeroed weapon row", first.Body)
	}

	// 第二包：自动修理后的变更行（武器与上衣都修满）。
	repaired, trailing := splitCurrentGameServerUpperPacketAuto(t, rest)
	if repaired.Header.Classification != 0 || repaired.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("auto repair header class=%d msg=%d", repaired.Header.Classification, repaired.Header.MsgID)
	}
	if repaired.Body[0] != currentSocketListEquipment || binary.LittleEndian.Uint16(repaired.Body[1:3]) != 2 {
		t.Fatalf("auto repair body=%x, want 2 changed equipment rows", repaired.Body)
	}
	for row, wantSlot := range []int16{12, 14} {
		offset := 3 + row*currentItemListEntryWireSize
		if got := int16(binary.LittleEndian.Uint16(repaired.Body[offset : offset+2])); got != wantSlot {
			t.Fatalf("auto repair row %d slot=%d, want %d", row, got, wantSlot)
		}
		if got := binary.LittleEndian.Uint16(repaired.Body[offset+0x0B : offset+0x0D]); got != 45 {
			t.Fatalf("auto repair row %d durability=%d, want 45", row, got)
		}
	}
	_ = trailing // 之后还有一次魔王契约服务状态推送，这里不约束。

	if got := devilAutoRepairBridgeDurability(t, ctx, repos, "12"); got != 45 {
		t.Fatalf("persisted weapon durability=%d, want 45", got)
	}
	if got := devilAutoRepairBridgeDurability(t, ctx, repos, "14"); got != 45 {
		t.Fatalf("persisted coat durability=%d, want 45", got)
	}
	character, found, err := repos.Character.Load(ctx, devilAutoRepairTestCharacter)
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if character.Stats["gold"] != 5555 {
		t.Fatalf("gold=%d, want untouched 5555 (contract repair is free)", character.Stats["gold"])
	}
	if used := premium.DailyUsage(character, premium.DevilSlotAutoRepair, time.Now().UTC()); used != 1 {
		t.Fatalf("daily usage=%d, want 1", used)
	}
}

func TestDungeonDurabilityAlreadyZeroDoesNotTriggerDevilAutoRepair(t *testing.T) {
	ctx := context.Background()
	repos, service, session, connection := devilAutoRepairBridgeFixture(t, 7, 0, 30, 0)
	if err := service.handleDungeonDecreaseDurability(session, []byte{12}); err != nil {
		t.Fatalf("handle op48: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("already-zero report emitted %d bytes, want none", connection.write.Len())
	}
	if got := devilAutoRepairBridgeDurability(t, ctx, repos, "14"); got != 30 {
		t.Fatalf("coat durability=%d, want untouched 30 (no consumption-caused zero)", got)
	}
	character, _, _ := repos.Character.Load(ctx, devilAutoRepairTestCharacter)
	if used := premium.DailyUsage(character, premium.DevilSlotAutoRepair, time.Now().UTC()); used != 0 {
		t.Fatalf("daily usage=%d, want 0", used)
	}
}

func TestDungeonDurabilityZeroWithoutContractDoesNotRepair(t *testing.T) {
	repos, service, session, connection := devilAutoRepairBridgeFixture(t, 0, 1, 30, 0)
	if err := service.handleDungeonDecreaseDurability(session, []byte{12}); err != nil {
		t.Fatalf("handle op48: %v", err)
	}
	first, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if first.Body[0] != currentSocketListEquipment || len(rest) != 0 {
		t.Fatalf("no-contract op48 emitted first=%x rest=%d bytes, want only the zeroed row", first.Body, len(rest))
	}
	if got := devilAutoRepairBridgeDurability(t, context.Background(), repos, "12"); got != 0 {
		t.Fatalf("weapon durability=%d, want 0 (no contract, no repair)", got)
	}
	if got := devilAutoRepairBridgeDurability(t, context.Background(), repos, "14"); got != 30 {
		t.Fatalf("coat durability=%d, want untouched 30", got)
	}
}

func TestDungeonDurabilityZeroAtDailyLimitDoesNotRepairOrCharge(t *testing.T) {
	ctx := context.Background()
	limit := premium.DailyLimit(premium.DevilSlotAutoRepair)
	repos, service, session, connection := devilAutoRepairBridgeFixture(t, 7, 1, 30, limit)
	if err := service.handleDungeonDecreaseDurability(session, []byte{12}); err != nil {
		t.Fatalf("handle op48: %v", err)
	}
	first, rest := splitCurrentGameServerUpperPacketAuto(t, connection.write.Bytes())
	if first.Body[0] != currentSocketListEquipment || len(rest) != 0 {
		t.Fatalf("daily-limit op48 emitted first=%x rest=%d bytes, want only the zeroed row", first.Body, len(rest))
	}
	if got := devilAutoRepairBridgeDurability(t, ctx, repos, "12"); got != 0 {
		t.Fatalf("weapon durability=%d, want 0 (daily cap reached)", got)
	}
	character, _, _ := repos.Character.Load(ctx, devilAutoRepairTestCharacter)
	if character.Stats["gold"] != 5555 {
		t.Fatalf("gold=%d, want untouched 5555 (never a paid fallback)", character.Stats["gold"])
	}
	if used := premium.DailyUsage(character, premium.DevilSlotAutoRepair, time.Now().UTC()); used != limit {
		t.Fatalf("daily usage=%d, want %d", used, limit)
	}
}
