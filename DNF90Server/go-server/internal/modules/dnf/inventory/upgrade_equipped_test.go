package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// The current client packs the op50 mode word as target list (high byte) +
// sub mode (low byte). 2026-08-28 live capture for a worn item at slot 14:
// mode word 0x0300, target 100080175, material slot 94.
func TestDecodeUpgradeItemRequestEquippedModeWord(t *testing.T) {
	body := []byte{
		0x00, 0x03,
		0x0E, 0x00,
		0x2F, 0x1A, 0xF7, 0x05,
		0x5E, 0x00,
		0xFF, 0xFF,
		0x02, 0x00, 0x00, 0x00,
		'x', 'y',
	}
	parsed, err := DecodeUpgradeItemRequest(body)
	if err != nil {
		t.Fatalf("DecodeUpgradeItemRequest error = %v", err)
	}
	if parsed.TargetListType != listTypeEquipment || parsed.Mode != "reinforce" ||
		parsed.RawMode != 0x0300 || parsed.TargetSlotIndex != 14 ||
		parsed.TargetItemTemplateID != 100080175 || parsed.MaterialSlotIndex != 94 {
		t.Fatalf("parsed = %+v", parsed)
	}

	ampBody := append([]byte(nil), body...)
	ampBody[0] = 0x01 // 0x0301: worn list-3 amplify
	parsedAmp, err := DecodeUpgradeItemRequest(ampBody)
	if err != nil {
		t.Fatalf("DecodeUpgradeItemRequest amplify error = %v", err)
	}
	if parsedAmp.TargetListType != listTypeEquipment || parsedAmp.Mode != "amplify" {
		t.Fatalf("parsed amplify = %+v", parsedAmp)
	}

	// The backpack wire is unchanged: 0x0000 / 0x0001 still decode to list 0.
	bagBody := append([]byte(nil), body...)
	bagBody[1] = 0x00
	parsedBag, err := DecodeUpgradeItemRequest(bagBody)
	if err != nil || parsedBag.TargetListType != listTypeMain || parsedBag.Mode != "reinforce" {
		t.Fatalf("parsed backpack = %+v err=%v", parsedBag, err)
	}

	// An unsupported list in the high byte is still refused.
	badList := append([]byte(nil), body...)
	badList[1] = 0x07
	if _, err := DecodeUpgradeItemRequest(badList); err == nil {
		t.Fatal("list-7 mode word unexpectedly accepted")
	}
	// Sub mode 2 is the 高级装备强化机 (live capture 2026-09-11 13:35:37); it
	// decodes as a reinforce carrying the high-upgrade flag. It was rejected
	// until then, which is why that machine never worked.
	highSub := append([]byte(nil), body...)
	highSub[0] = 0x02
	parsedHigh, err := DecodeUpgradeItemRequest(highSub)
	if err != nil || parsedHigh.Mode != "reinforce" || !parsedHigh.HighUpgrade {
		t.Fatalf("sub mode 2 = %+v err=%v", parsedHigh, err)
	}
	// Anything above 2 is still unknown and must fail closed.
	badSub := append([]byte(nil), body...)
	badSub[0] = 0x03
	if _, err := DecodeUpgradeItemRequest(badSub); err == nil {
		t.Fatal("sub mode 3 unexpectedly accepted")
	}
	if parsedBag.HighUpgrade {
		t.Fatal("ordinary reinforce must not carry the high-upgrade flag")
	}
}

// upgradeTicketEquippedBody is the live C2S layout for a ticket used on a worn
// item: mode word high byte 3.
func upgradeTicketEquippedBody(targetSlot int16, targetItemID int32, materialSlot int16) []byte {
	body := upgradeTicketRequestBody(targetSlot, targetItemID, materialSlot)
	body[0] = 0x00
	body[1] = 0x03
	return body
}

// newEquippedUpgradeTicketCommand targets a worn equipment slot (list 3).
func newEquippedUpgradeTicketCommand(mode string, targetSlot int16, targetItemID int32, materialSlot int16) Command {
	cmd := newUpgradeTicketCommand(mode, targetSlot, targetItemID, materialSlot)
	cmd.TargetListType = listTypeEquipment
	return cmd
}

// saveEquippedUpgradeTicketFixture puts the worn target into the equipment
// record (slot 14) and the ticket/material into the backpack.
func saveEquippedUpgradeTicketFixture(t *testing.T, ctx context.Context, repos dnfrepo.Group, targetLevel byte, ticketCount int64) {
	t.Helper()
	targetRaw := make([]byte, currentItemListEntrySize)
	targetRaw[0x0A] = targetLevel
	ticketRaw := make([]byte, currentItemListEntrySize)
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "77",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"14": {
				SlotIndex: 14,
				ItemID:    700,
				RawEntry:  targetRaw,
				Extra: map[string]string{
					"item_kind":                   "equipment",
					"durability":                  "45",
					"max_durability":              "45",
					"current_exe_inventory_count": "1",
				},
			},
		},
	}); err != nil {
		t.Fatalf("Save equipment error = %v", err)
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 121): {
				ItemID:   9100,
				Count:    ticketCount,
				RawEntry: ticketRaw,
				Extra: map[string]string{
					"item_kind": "stackable",
					"pvf_path":  "stackable/ticket/plus10.stk",
				},
			},
		},
	})
}

func TestOwnerUpgradeTicketOnEquippedTargetWritesEquipmentRepo(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveEquippedUpgradeTicketFixture(t, ctx, repos, 2, 2)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.UpgradeTicket(ctx, newEquippedUpgradeTicketCommand("reinforce", 14, 700, 121), reinforceTicketResolver(10, 100000))
	if err != nil {
		t.Fatalf("UpgradeTicket error = %v", err)
	}
	if !result.Success || !result.UpgradeSucceeded || result.TargetListType != listTypeEquipment || result.NewLevel != 10 {
		t.Fatalf("result = %+v", result)
	}
	equip, ok, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load equipment found=%t err=%v", ok, err)
	}
	entry := equip.Entries["14"]
	if got := entry.RawEntry[0x0A] & 0x1F; got != 10 {
		t.Fatalf("equipped raw level = %d, want 10", got)
	}
	if got := entry.Extra["reinforce"]; got != "10" {
		t.Fatalf("equipped extra reinforce = %q, want 10", got)
	}
	// The ticket is consumed from the backpack, and no list-3 key leaked in.
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 1 {
		t.Fatalf("ticket count = %d, want 1", got)
	}
	if _, leaked := loaded.Slots[slotKey(listTypeEquipment, 14)]; leaked {
		t.Fatalf("worn row leaked into the backpack record: %+v", loaded.Slots)
	}
	if result.TargetUpdatedStack.ItemID != 700 || result.TargetUpdatedStack.RawEntry[0x0A]&0x1F != 10 {
		t.Fatalf("target updated stack = %+v", result.TargetUpdatedStack)
	}
}

func TestOwnerUpgradeTicketOnEquippedTargetMissingEntryFailsInvalidTarget(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveEquippedUpgradeTicketFixture(t, ctx, repos, 2, 2)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.UpgradeTicket(ctx, newEquippedUpgradeTicketCommand("reinforce", 99, 700, 121), reinforceTicketResolver(10, 100000))
	if err != nil {
		t.Fatalf("UpgradeTicket error = %v", err)
	}
	if result.Success || result.ErrorCode != upgradeTicketErrorInvalidTarget || result.Changed {
		t.Fatalf("result = %+v, want invalid target without mutation", result)
	}
}

func TestOwnerUpgradeReinforceOnEquippedTargetWritesEquipmentRepo(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc", Stats: map[string]int64{"gold": 10_000_000}})
	saveEquippedUpgradeTicketFixture(t, ctx, repos, 2, 3)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	cmd := NewUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UpgradeItemRequest{
		Mode:                    "reinforce",
		TargetListType:          listTypeEquipment,
		TargetSlotIndex:         14,
		TargetItemTemplateID:    700,
		MaterialSlotIndex:       121,
		OptionalTicketSlotIndex: -1,
	})
	cmd.UpgradeSuccessWeight = 100000 // guaranteed success
	result, err := owner.Upgrade(ctx, cmd)
	if err != nil {
		t.Fatalf("Upgrade error = %v", err)
	}
	if !result.Success || !result.Changed || result.TargetListType != listTypeEquipment ||
		result.OldLevel != 2 || result.NewLevel != 3 || result.MaterialRemainingStackCount != 2 {
		t.Fatalf("result = %+v", result)
	}
	equip, ok, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load equipment found=%t err=%v", ok, err)
	}
	entry := equip.Entries["14"]
	if got := entry.RawEntry[0x0A] & 0x1F; got != 3 {
		t.Fatalf("equipped raw level = %d, want 3", got)
	}
	if got := entry.Extra["reinforce"]; got != "3" {
		t.Fatalf("equipped extra reinforce = %q, want 3", got)
	}
	loaded := loadTestInventory(t, ctx, repos, "77")
	if got := loaded.Slots[slotKey(listTypeMain, 121)].Count; got != 2 {
		t.Fatalf("material count = %d, want 2", got)
	}
	if _, leaked := loaded.Slots[slotKey(listTypeEquipment, 14)]; leaked {
		t.Fatalf("worn row leaked into the backpack record: %+v", loaded.Slots)
	}
}

func TestHandlerUpgradeTicketOnEquippedTargetSendsAckAndEquipmentRefresh(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveEquippedUpgradeTicketFixture(t, ctx, repos, 2, 2)

	got := handleUpgradeTicketRequest(t, repos, reinforceTicketResolver(10, 100000), upgradeTicketEquippedBody(14, 700, 121))
	if !got.Handled || !got.ResponseAllowed {
		t.Fatalf("result = %+v", got)
	}
	// ACK + the material row in list 0; the worn row is not in that refresh.
	if len(got.UpperResponses) != 2 {
		t.Fatalf("responses = %d, want ack + material refresh", len(got.UpperResponses))
	}
	ack := got.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketUpgradeItem) || ack.Classification != 1 {
		t.Fatalf("ack = msg 0x%04X class %d", ack.MsgID, ack.Classification)
	}
	if len(ack.Body) < 15 || ack.Body[14] != listTypeEquipment {
		t.Fatalf("ack body = %x, want byte[14] = listTypeEquipment(3)", ack.Body)
	}
	refresh := got.UpperResponses[1]
	if refresh.MsgID != msgItemListUpdate || refresh.Classification != 0 || len(refresh.Body) < 2 || refresh.Body[0] != listTypeMain || refresh.Body[1] != 1 {
		t.Fatalf("refresh = msg 0x%04X class %d body=%x, want one list-0 material row", refresh.MsgID, refresh.Classification, refresh.Body)
	}
	if len(got.EquipmentSlotRefreshes) != 1 || got.EquipmentSlotRefreshes[0] != 14 || len(got.ItemSlotRefreshes) != 0 {
		t.Fatalf("equipment refreshes = %v item refreshes = %+v, want the single worn slot 14", got.EquipmentSlotRefreshes, got.ItemSlotRefreshes)
	}
	equip, ok, err := repos.Equipment.Load(ctx, "77")
	if err != nil || !ok {
		t.Fatalf("load equipment found=%t err=%v", ok, err)
	}
	if got := equip.Entries["14"].RawEntry[0x0A] & 0x1F; got != 10 {
		t.Fatalf("equipped raw level = %d, want 10", got)
	}
}

// The backpack path must be byte-for-byte what it was: list 0 in the mode word
// still resolves the target in list 0, and byte[14] of the ACK stays 0.
func TestHandlerUpgradeTicketOnBackpackTargetKeepsByte14Zero(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveUpgradeTicketFixture(t, ctx, repos, nil, 2, 2)
	got := handleUpgradeTicketRequest(t, repos, reinforceTicketResolver(10, 100000), upgradeTicketRequestBody(9, 700, 121))
	if !got.Handled || len(got.UpperResponses) != 2 {
		t.Fatalf("result = %+v", got)
	}
	if ack := got.UpperResponses[0]; len(ack.Body) < 15 || ack.Body[14] != 0 {
		t.Fatalf("backpack ack body = %x, want byte[14] = 0", ack.Body)
	}
	if len(got.EquipmentSlotRefreshes) != 0 {
		t.Fatalf("backpack target must not request an equipment refresh: %v", got.EquipmentSlotRefreshes)
	}
}
