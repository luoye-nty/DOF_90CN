package dnfbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentAvatarOptionChangeParsesCurrentClientBodyAndBuildsAck(t *testing.T) {
	body, err := hex.DecodeString("0c0034ab1200170078cd56092a")
	if err != nil {
		t.Fatal(err)
	}
	request, err := parseCurrentAvatarOptionChangeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.SourceSlot != 12 || request.SourceItemID != 0x0012ab34 || request.TargetSlot != 23 || request.TargetItemID != 0x0956cd78 || request.AbilityNo != 42 {
		t.Fatalf("request=%+v", request)
	}
	if got := hex.EncodeToString(buildCurrentAvatarOptionChangeSuccessBody(request)); got != "0117002a" {
		t.Fatalf("success=%s", got)
	}
	if got := hex.EncodeToString(currentAvatarOptionChangeFailureBody()); got != "0000" {
		t.Fatalf("failure=%s", got)
	}
	if _, err := parseCurrentAvatarOptionChangeRequest(body[:12]); !errors.Is(err, errCurrentAvatarOptionChangeRequestInvalid) {
		t.Fatalf("short error=%v", err)
	}
	if _, err := parseCurrentAvatarOptionChangeRequest(append(body, 0)); !errors.Is(err, errCurrentAvatarOptionChangeRequestInvalid) {
		t.Fatalf("long error=%v", err)
	}
}

func TestCurrentAvatarOptionChangePublishesNativeOp459ResultThenSourceRefresh(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 9001, Count: 2},
		"1:23": {ItemID: 9002, Count: 1, Extra: map[string]string{"source": "cera_shop_avatar"}},
	}}); err != nil {
		t.Fatal(err)
	}
	source := avatarCompoundPVFTestSource{
		"stackable/tool.stk":   "[stackable type] `[material]`\n[avatar select ability change]\n`[all]` `[all]` 3 0\n",
		"equipment/avatar.equ": "[equipment type] `[coat avatar]`\n[grade]\n3\n",
	}
	catalog := &pvfDungeonDropCatalog{
		source: source,
		itemRefs: map[uint32]dungeonDropItemReference{
			9001: {kind: dungeonDropItemStackable, path: "stackable/tool.stk"},
			9002: {kind: dungeonDropItemEquipment, path: "equipment/avatar.equ"},
		},
		itemCache: map[uint32]dungeonDropItemDefinition{
			9001: {ItemID: 9001, Kind: dungeonDropItemStackable, PVFPath: "stackable/tool.stk"},
			9002: {ItemID: 9002, Kind: dungeonDropItemEquipment, PVFPath: "equipment/avatar.equ", EquipmentType: "[coat avatar]", Grade: 3},
		},
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountPrefix: defaultAccountPrefix},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog:     catalog,
	}
	session := &gameSession{conn: connection, accountID: "dnf:1", selectedCharacterID: 19}
	body, err := hex.DecodeString("0c002923000017002a2300000e")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleCurrentAvatarOptionChange(session, body); err != nil {
		t.Fatal(err)
	}

	result, rest := splitGameServerUpperPacketWithHeader(t, connection.write.Bytes(), service.gameUpperHeaderSize())
	if result.Header.Classification != dnfproto.DefaultChannelClassification ||
		result.Header.MsgID != uint16(dnfenum.CmdPacketAvatarOptionChange) ||
		!bytes.Equal(result.Body, []byte{1, 23, 0, 14}) {
		t.Fatalf("result class=%d msg=%d body=%x", result.Header.Classification, result.Header.MsgID, result.Body)
	}
	refresh, trailing := splitGameServerUpperPacketWithHeader(t, rest, service.gameUpperHeaderSize())
	if len(trailing) != 0 || refresh.Header.Classification != 0 ||
		refresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(refresh.Body) < 3 || refresh.Body[0] != byte(dnfrepo.MainInventoryListType) {
		t.Fatalf("refresh class=%d msg=%d body=%x trailing=%x", refresh.Header.Classification, refresh.Header.MsgID, refresh.Body, trailing)
	}
}

func TestCurrentAvatarOptionChangeCommitsToolAndAbilityAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 9001, Count: 2},
		"1:23": {ItemID: 9002, Count: 1, Extra: map[string]string{"source": "cera_shop_avatar"}},
	}}); err != nil {
		t.Fatal(err)
	}
	source := avatarCompoundPVFTestSource{
		"stackable/tool.stk":   "[stackable type] `[material]`\n[avatar select ability change]\n`[all]` `[all]` 3 0\n",
		"equipment/avatar.equ": "[equipment type] `[coat avatar]`\n[grade]\n3\n",
	}
	catalog := &pvfDungeonDropCatalog{
		source: source,
		itemRefs: map[uint32]dungeonDropItemReference{
			9001: {kind: dungeonDropItemStackable, path: "stackable/tool.stk"},
			9002: {kind: dungeonDropItemEquipment, path: "equipment/avatar.equ"},
		},
		itemCache: map[uint32]dungeonDropItemDefinition{
			9001: {ItemID: 9001, Kind: dungeonDropItemStackable, PVFPath: "stackable/tool.stk"},
			9002: {ItemID: 9002, Kind: dungeonDropItemEquipment, PVFPath: "equipment/avatar.equ", EquipmentType: "[coat avatar]", Grade: 3},
		},
	}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	request := currentAvatarOptionChangeRequest{SourceSlot: 12, SourceItemID: 9001, TargetSlot: 23, TargetItemID: 9002, AbilityNo: 14}
	result, err := service.commitCurrentAvatarOptionChange(ctx, session, catalog, request)
	if err != nil || result.SourceRemaining != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if got := inventory.Slots["0:12"].Count; got != 1 {
		t.Fatalf("source count=%d", got)
	}
	target := inventory.Slots["1:23"]
	if target.Extra["avatar_ability_no"] != "14" || target.Extra["durability"] != "14" || target.Extra["item_kind"] != "avatar" {
		t.Fatalf("target extra=%+v", target.Extra)
	}
	if len(target.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("target raw len=%d", len(target.RawEntry))
	}
}

func TestCurrentAvatarOptionChangeRejectsWrongPVFGradeWithoutConsume(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:12": {ItemID: 9001, Count: 1}, "1:23": {ItemID: 9002, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	source := avatarCompoundPVFTestSource{
		"stackable/tool.stk":   "[avatar select ability change]\n`[all]` `[all]` 2 1\n",
		"equipment/avatar.equ": "[equipment type] `[coat avatar]`\n[grade]\n3\n",
	}
	catalog := &pvfDungeonDropCatalog{source: source, itemRefs: map[uint32]dungeonDropItemReference{
		9001: {kind: dungeonDropItemStackable, path: "stackable/tool.stk"}, 9002: {kind: dungeonDropItemEquipment, path: "equipment/avatar.equ"},
	}, itemCache: map[uint32]dungeonDropItemDefinition{
		9001: {ItemID: 9001, Kind: dungeonDropItemStackable, PVFPath: "stackable/tool.stk"},
		9002: {ItemID: 9002, Kind: dungeonDropItemEquipment, PVFPath: "equipment/avatar.equ", EquipmentType: "[coat avatar]", Grade: 3},
	}}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	_, err := service.commitCurrentAvatarOptionChange(ctx, session, catalog, currentAvatarOptionChangeRequest{SourceSlot: 12, SourceItemID: 9001, TargetSlot: 23, TargetItemID: 9002, AbilityNo: 7})
	if !errors.Is(err, errCurrentAvatarOptionChangeToolInvalid) {
		t.Fatalf("error=%v", err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:12"].Count != 1 {
		t.Fatalf("tool was consumed: %+v", inventory.Slots["0:12"])
	}
}
