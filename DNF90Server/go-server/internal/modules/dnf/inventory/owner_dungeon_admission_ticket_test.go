package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerDimensionGateTicketConsumesDynamicSlotAndAddsFiveAdmissions(t *testing.T) {
	ctx := context.Background()
	repositories := dungeonAdmissionTicketRepositories(t, 90, 10158692, 147, 2, "stackable/10158001/10158692.stk")
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	owner.now = func() time.Time { return now }
	policy := dnfdungeonadmission.Policy{
		Key: "dimension_gate.character.daily", Scope: dnfdungeonadmission.ScopeCharacter,
		Period: dnfdungeonadmission.PeriodDaily, Limit: 5, DungeonIDs: []uint32{62, 64},
	}
	resolver := dungeonAdmissionTicketResolver(10158692, "stackable/10158001/10158692.stk", &alignedcmd.DungeonAdmissionTicketResolution{
		ActionType: "[add dimension dungeon enter count]", MinimumLevel: 70,
		VillageAllowed: true, SeriaRoomAllowed: true,
		Grants: []dnfdungeonadmission.AllowanceGrant{{Policy: policy, Amount: 5, MaximumBonus: 5}},
	})
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 147, ListType: listTypeMain, InstanceValue: 0x11223344, ItemCode: 10158692,
	})
	result, err := owner.UseStackableWithAllResolversAndSkillCatalog(ctx, cmd, nil, nil, resolver, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.DungeonAdmissionTicketApplied || result.DungeonAdmissionBonus != 5 || result.DungeonAdmissionPolicyCount != 1 || result.RemainingCount != 1 {
		t.Fatalf("result=%+v", result)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if stack := inventory.Slots[slotKey(listTypeMain, 147)]; stack.Count != 1 || stack.ItemID != 10158692 {
		t.Fatalf("inventory=%+v", inventory.Slots)
	}
	admissionOwner, _ := dnfdungeonadmission.NewOwner(repositories)
	snapshot, err := admissionOwner.Preflight(ctx, "77", []dnfdungeonadmission.Policy{policy}, now)
	if err != nil || len(snapshot) != 1 || snapshot[0].Bonus != 5 || snapshot[0].EffectiveLimit != 10 || snapshot[0].Remaining != 10 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	_, err = owner.UseStackableWithAllResolversAndSkillCatalog(ctx, cmd, nil, nil, resolver, nil, nil, nil, nil, nil, nil)
	if !errors.Is(err, dnfdungeonadmission.ErrAllowanceLimit) {
		t.Fatalf("second +5 err=%v", err)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "77")
	if inventory.Slots[slotKey(listTypeMain, 147)].Count != 1 {
		t.Fatalf("rejected grant consumed source inventory=%+v", inventory.Slots)
	}
}

func TestOwnerTowerOfGriefBookAddsCharacterAndAccountAdmissionsAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dungeonAdmissionTicketRepositories(t, 90, 490701703, 163, 3, "stackable/cash/chn_490701703.stk")
	owner, _ := NewOwner(repositories)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	owner.now = func() time.Time { return now }
	characterPolicy := dnfdungeonadmission.Policy{Key: "tower_of_grief.character.daily", Scope: dnfdungeonadmission.ScopeCharacter, Period: dnfdungeonadmission.PeriodDaily, Limit: 2, DungeonIDs: []uint32{5115}}
	accountPolicy := dnfdungeonadmission.Policy{Key: "tower_of_grief.account.daily", Scope: dnfdungeonadmission.ScopeAccount, Period: dnfdungeonadmission.PeriodDaily, Limit: 1, DungeonIDs: []uint32{5115}}
	resolver := dungeonAdmissionTicketResolver(490701703, "stackable/cash/chn_490701703.stk", &alignedcmd.DungeonAdmissionTicketResolution{
		ActionType: "[up layer despair dungeon]", TargetDungeonID: 5115, MinimumLevel: 90, VillageAllowed: true,
		Grants: []dnfdungeonadmission.AllowanceGrant{
			{Policy: characterPolicy, Amount: 1, MaximumBonus: 2},
			{Policy: accountPolicy, Amount: 1, MaximumBonus: 2},
		},
		UseLimit: dnfdungeonadmission.AllowanceUseLimit{Key: "tower_of_grief.recommendation.character.daily", Limit: 2},
	})
	cmd := NewUseStackableCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UseStackableRequest{
		SlotIndex: 163, ListType: listTypeMain, InstanceValue: 0x55667788, ItemCode: 490701703,
	})
	for remaining := int64(2); remaining >= 1; remaining-- {
		result, err := owner.UseStackableWithAllResolversAndSkillCatalog(ctx, cmd, nil, nil, resolver, nil, nil, nil, nil, nil, nil)
		if err != nil || result.RemainingCount != remaining || result.DungeonAdmissionPolicyCount != 2 || result.DungeonAdmissionRemainingUses != uint32(remaining-1) {
			t.Fatalf("remaining=%d result=%+v err=%v", remaining, result, err)
		}
	}
	if _, err := owner.UseStackableWithAllResolversAndSkillCatalog(ctx, cmd, nil, nil, resolver, nil, nil, nil, nil, nil, nil); !errors.Is(err, dnfdungeonadmission.ErrAllowanceLimit) {
		t.Fatalf("third use err=%v", err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if inventory.Slots[slotKey(listTypeMain, 163)].Count != 1 {
		t.Fatalf("third use consumed book inventory=%+v", inventory.Slots)
	}
	admissionOwner, _ := dnfdungeonadmission.NewOwner(repositories)
	for _, policy := range []dnfdungeonadmission.Policy{characterPolicy, accountPolicy} {
		snapshot, err := admissionOwner.Preflight(ctx, "77", []dnfdungeonadmission.Policy{policy}, now)
		if err != nil || len(snapshot) != 1 || snapshot[0].Bonus != 2 {
			t.Fatalf("policy=%s snapshot=%+v err=%v", policy.Key, snapshot, err)
		}
	}
}

func TestHandlerDungeonAdmissionTicketKeepsNativeOp44AndDynamicRefresh(t *testing.T) {
	ctx := context.Background()
	repositories := dungeonAdmissionTicketRepositories(t, 90, 10158692, 147, 2, "stackable/10158001/10158692.stk")
	policy := dnfdungeonadmission.Policy{
		Key: "dimension_gate.character.daily", Scope: dnfdungeonadmission.ScopeCharacter,
		Period: dnfdungeonadmission.PeriodDaily, Limit: 5, DungeonIDs: []uint32{62, 64},
	}
	resolver := dungeonAdmissionTicketResolver(10158692, "stackable/10158001/10158692.stk", &alignedcmd.DungeonAdmissionTicketResolution{
		ActionType: "[add dimension dungeon enter count]", MinimumLevel: 70,
		VillageAllowed: true, SeriaRoomAllowed: true,
		Grants: []dnfdungeonadmission.AllowanceGrant{{Policy: policy, Amount: 5, MaximumBonus: 5}},
	})
	body := make([]byte, 15)
	binary.LittleEndian.PutUint16(body[0:2], 147)
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[3:7], 0x11223344)
	binary.LittleEndian.PutUint32(body[7:11], 10158692)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackable), Body: body,
		AccountID: "acc", SelectedCharacterID: 77, Repositories: repositories,
		StackableUseResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{1, 147, 0, listTypeMain, 0x44, 0x33, 0x22, 0x11}
	itemBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(itemBytes, 10158692)
	wantAck = append(wantAck, itemBytes...)
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
		!bytes.Equal(result.UpperResponses[0].Body, wantAck) ||
		len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0] != (alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: 147}) ||
		!strings.Contains(result.Reason, "inventory/admission settlement owner applied") {
		t.Fatalf("handler result=%+v want_ack=%x", result, wantAck)
	}
}

func TestHandlerTowerRecommendationUsesExactOp515ActionOne(t *testing.T) {
	ctx := context.Background()
	repositories := dungeonAdmissionTicketRepositories(t, 90, 490701703, 163, 2, "stackable/cash/chn_490701703.stk")
	characterPolicy := dnfdungeonadmission.Policy{Key: "tower_of_grief.character.daily", Scope: dnfdungeonadmission.ScopeCharacter, Period: dnfdungeonadmission.PeriodDaily, Limit: 2, DungeonIDs: []uint32{5115}}
	accountPolicy := dnfdungeonadmission.Policy{Key: "tower_of_grief.account.daily", Scope: dnfdungeonadmission.ScopeAccount, Period: dnfdungeonadmission.PeriodDaily, Limit: 1, DungeonIDs: []uint32{5115}}
	resolver := dungeonAdmissionTicketResolver(490701703, "stackable/cash/chn_490701703.stk", &alignedcmd.DungeonAdmissionTicketResolution{
		ActionType: "[up layer despair dungeon]", ActionPacket: 1, TargetDungeonID: 5115,
		MinimumLevel: 90, VillageAllowed: true,
		Grants: []dnfdungeonadmission.AllowanceGrant{
			{Policy: characterPolicy, Amount: 1, MaximumBonus: 2},
			{Policy: accountPolicy, Amount: 1, MaximumBonus: 2},
		},
		UseLimit: dnfdungeonadmission.AllowanceUseLimit{Key: "tower_of_grief.recommendation.item.490701703.daily", Limit: 2},
	})
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 163)
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[7:11], dungeonAdmissionTicketActionIndex)
	result, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketUseStackableAction), Body: body,
		AccountID: "acc", SelectedCharacterID: 77, Repositories: repositories,
		StackableUseResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{1, 163, 0, 0, 0, 1, 0, 0, 0, 1}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
		!bytes.Equal(result.UpperResponses[0].Body, wantAck) ||
		len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0] != (alignedcmd.ItemSlotRefresh{ListType: listTypeMain, SlotIndex: 163}) ||
		!strings.Contains(result.Reason, "native op515 action-1 ACK") {
		t.Fatalf("handler result=%+v want_ack=%x", result, wantAck)
	}
	if remaining := loadTestInventory(t, ctx, repositories, "77").Slots[slotKey(listTypeMain, 163)].Count; remaining != 1 {
		t.Fatalf("remaining=%d want=1", remaining)
	}
}

func dungeonAdmissionTicketRepositories(t *testing.T, level int, itemID int64, slot int16, count int64, pvfPath string) dnfrepo.Group {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Level: level}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, slot): {
			ItemID: itemID, Count: count,
			Extra: map[string]string{"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[waste]"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	return repositories
}

func dungeonAdmissionTicketResolver(itemID int64, pvfPath string, ticket *alignedcmd.DungeonAdmissionTicketResolution) alignedcmd.StackableUseResolver {
	return func(candidate int64) (alignedcmd.StackableUseResolution, error) {
		if candidate != itemID {
			return alignedcmd.StackableUseResolution{}, nil
		}
		return alignedcmd.StackableUseResolution{
			ItemID: itemID, PVFPath: pvfPath, StackableType: "[waste]", DungeonAdmissionTicket: ticket,
		}, nil
	}
}
