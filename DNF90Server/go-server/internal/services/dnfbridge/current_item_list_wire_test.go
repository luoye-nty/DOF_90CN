package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestBuildCurrentSceneClass0FixedUpperPacketUsesZeroPrefixAndCurrentChecksum(t *testing.T) {
	body := []byte{2, 8, 0, 0, 0, 0}
	packet, err := buildCurrentSceneClass0FixedUpperPacket(13, body)
	if err != nil {
		t.Fatalf("build fixed scene upper packet: %v", err)
	}
	want, err := hex.DecodeString("000d00160000007afa29f90000000000020800000000")
	if err != nil {
		t.Fatalf("decode expected packet: %v", err)
	}
	if !bytes.Equal(packet, want) {
		t.Fatalf("fixed scene item list packet=%x want=%x", packet, want)
	}
	if got := binary.LittleEndian.Uint16(packet[11:13]); got != 0 || packet[13] != 0 || packet[14] != 0 || packet[15] != 0 {
		t.Fatalf("fixed scene header tail=%x want five zero bytes", packet[11:16])
	}
}

func TestBuildCurrentItemListBodyAvatarRowsIncludeTwoEmptyOptionalBlobs(t *testing.T) {
	entries := make([]currentItemListEntry, 2)
	entries[0].patchCore(7, 1001, 1)
	entries[1].patchCore(9, 1002, 1)

	body := buildCurrentItemListBody(1, entries, dnfrepo.CharacterContainerState{})
	const avatarRowSize = currentItemListEntryWireSize + 2*4
	if len(body) != 5+2*avatarRowSize {
		t.Fatalf("list1 body len=%d want=%d", len(body), 5+2*avatarRowSize)
	}
	if !bytes.Equal(body[:5], []byte{1, 0, 0, 2, 0}) {
		t.Fatalf("list1 header=%x", body[:5])
	}
	for index, wantSlot := range []uint16{7, 9} {
		row := body[5+index*avatarRowSize : 5+(index+1)*avatarRowSize]
		if got := binary.LittleEndian.Uint16(row[:2]); got != wantSlot {
			t.Fatalf("row %d slot=%d want=%d", index, got, wantSlot)
		}
		if !bytes.Equal(row[currentItemListEntryWireSize:], make([]byte, 8)) {
			t.Fatalf("row %d optional lengths=%x want zero", index, row[currentItemListEntryWireSize:])
		}
	}
}

func TestBuildCurrentItemUpdateBodyAvatarRowsIncludeTwoEmptyOptionalBlobs(t *testing.T) {
	entries := make([]currentItemListEntry, 2)
	entries[0].patchCore(7, 1001, 0)
	entries[1].patchCore(10, 1002, 0)

	body := buildCurrentItemUpdateBody(1, entries)
	const avatarRowSize = currentItemListEntryWireSize + 2*4
	if len(body) != 3+2*avatarRowSize {
		t.Fatalf("list1 update body len=%d want=%d", len(body), 3+2*avatarRowSize)
	}
	if !bytes.Equal(body[:3], []byte{1, 2, 0}) {
		t.Fatalf("list1 update header=%x", body[:3])
	}
	for index, wantSlot := range []uint16{7, 10} {
		row := body[3+index*avatarRowSize : 3+(index+1)*avatarRowSize]
		if got := binary.LittleEndian.Uint16(row[:2]); got != wantSlot {
			t.Fatalf("row %d slot=%d want=%d", index, got, wantSlot)
		}
		if !bytes.Equal(row[currentItemListEntryWireSize:], make([]byte, 8)) {
			t.Fatalf("row %d optional lengths=%x want zero", index, row[currentItemListEntryWireSize:])
		}
	}
}

func TestCurrentAvatarAbilityNoUsesUint16FieldAndRepairsLegacyGoGrant(t *testing.T) {
	current := currentItemListEntryFromStack(currentAvatarInventoryListType, 7, dnfrepo.ItemStack{
		ItemID: 112500217,
		Count:  1,
		Extra: map[string]string{
			"item_kind":         "avatar",
			"source":            "cera_shop_avatar",
			"avatar_ability_no": "6",
			"durability":        "6",
		},
	})
	if current.data[0x0A] != 0 || binary.LittleEndian.Uint16(current.data[0x0B:0x0D]) != 6 {
		t.Fatalf("current avatar attr/ability=%d/%d row=%x", current.data[0x0A], binary.LittleEndian.Uint16(current.data[0x0B:0x0D]), current.data[:0x10])
	}

	legacy := currentItemListEntryFromStack(currentAvatarInventoryListType, 8, dnfrepo.ItemStack{
		ItemID: 112500217,
		Count:  1,
		RawEntry: []byte{
			8, 0, 0xf9, 0x9d, 0xb4, 0x06, 0, 0, 0, 0, 6, 0, 0,
		},
		Extra: map[string]string{
			"item_kind": "avatar",
			"source":    "cera_shop_avatar",
			"ext_data0": "6",
		},
	})
	if legacy.data[0x0A] != 0 || binary.LittleEndian.Uint16(legacy.data[0x0B:0x0D]) != 6 {
		t.Fatalf("legacy avatar attr/ability=%d/%d row=%x", legacy.data[0x0A], binary.LittleEndian.Uint16(legacy.data[0x0B:0x0D]), legacy.data[:0x10])
	}

	legacyEquipped := dnfrepo.EquipmentEntry{Extra: map[string]string{
		"item_kind": "avatar",
		"source":    "cera_package",
		"ext_data0": "9",
	}}
	if got := currentItemListEquipmentExtData(legacyEquipped); got != 0 {
		t.Fatalf("legacy equipped avatar attr=%d want=0", got)
	}
	if got := currentItemListEquipmentDurability(legacyEquipped); got != 9 {
		t.Fatalf("legacy equipped avatar ability=%d want=9", got)
	}
}

func TestBuildCurrentItemListBodyKeepsOrdinaryRowsAndExactCargoLength(t *testing.T) {
	entry := currentItemListEntry{}
	entry.patchCore(2, 2001, 3)

	main := buildCurrentItemListBody(0, []currentItemListEntry{entry}, dnfrepo.CharacterContainerState{MainSlotCount: 24})
	if len(main) != 5+currentItemListEntryWireSize {
		t.Fatalf("list0 body len=%d want=%d", len(main), 5+currentItemListEntryWireSize)
	}
	cargo := buildCurrentItemListBody(2, []currentItemListEntry{entry}, dnfrepo.CharacterContainerState{PersonalCargoSlotCount: 8})
	if len(cargo) != 6+currentItemListEntryWireSize {
		t.Fatalf("list2 body len=%d want=%d", len(cargo), 6+currentItemListEntryWireSize)
	}
	if got := cargo[:5]; !bytes.Equal(got, []byte{2, 8, 0, 1, 0}) {
		t.Fatalf("list2 header=%x want=0208000100", got)
	}
	if got := binary.LittleEndian.Uint16(cargo[5:7]); got != 2 {
		t.Fatalf("list2 row slot=%d want=2", got)
	}
	if cargo[len(cargo)-1] != 0 {
		t.Fatalf("list2 group count=%d want=0", cargo[len(cargo)-1])
	}
}

func TestBuildCurrentItemListBodyPetAndAccountCargoHeaders(t *testing.T) {
	pet := buildCurrentItemListBody(7, nil, dnfrepo.CharacterContainerState{})
	if !bytes.Equal(pet, []byte{7, 0, 0}) {
		t.Fatalf("list7 body=%x want=070000", pet)
	}
	accountCargo := buildCurrentItemListBody(12, nil, dnfrepo.CharacterContainerState{
		AccountCargoSelectionKey: 0x1234,
		AccountCargoStateValue:   0x56789abc,
	})
	if !bytes.Equal(accountCargo, []byte{12, 0x34, 0x12, 0xbc, 0x9a, 0x78, 0x56, 0, 0}) {
		t.Fatalf("list12 body=%x", accountCargo)
	}
}

func TestCurrentPetItemRowDoesNotMirrorCreatureSerialIntoDurationField(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID: 63090,
		Count:  1,
		RawEntry: func() []byte {
			raw := make([]byte, currentItemListEntryWireSize)
			binary.LittleEndian.PutUint32(raw[0x0E:0x12], 19)
			binary.LittleEndian.PutUint32(raw[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4], 19)
			return raw
		}(),
		Extra: map[string]string{
			"creature_serial_or_handle": "19",
			"serial":                    "19",
		},
	}
	entry := currentItemListEntryFromStack(currentPetInventoryListType, 3, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 19 {
		t.Fatalf("pet serial=%d want=19", got)
	}
	if got := binary.LittleEndian.Uint32(entry.data[0x0E:0x12]); got != 0 {
		t.Fatalf("pet raw+0x0E=%d, serial must not become a time limit", got)
	}
	if got := binary.LittleEndian.Uint32(entry.data[currentPetRemainSecondsOffset : currentPetRemainSecondsOffset+4]); got != 0 {
		t.Fatalf("pet remaining seconds=%d want permanent", got)
	}
	if got := binary.LittleEndian.Uint32(entry.data[currentItemListExpireTimeOffset : currentItemListExpireTimeOffset+4]); got != 0 {
		t.Fatalf("pet expiration=%d want permanent", got)
	}

	stack.Extra["value_a"] = "77"
	entry = currentItemListEntryFromStack(currentPetInventoryListType, 3, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x0E:0x12]); got != 77 {
		t.Fatalf("explicit pet value_a=%d want=77", got)
	}
}

func TestCurrentPetItemRowClearsLegacyEquippedSerialFromRemainingSeconds(t *testing.T) {
	// This is the exact 47-byte equipped-pet row shape persisted for creature
	// serial 41. Padding it to the 0x77 item-list row places byte 0x29 at 0x18,
	// so the client would read 0x00290000 seconds and display 32 days.
	stack := dnfrepo.ItemStack{
		ItemID: 400990168,
		Count:  1,
		Extra: map[string]string{
			"creature_serial_or_handle": "41",
			"raw_entry_hex":             "1ad89fe617290000000000000000000000000000000000002900000000000000000000000000000000000000000000",
		},
	}
	entry := currentItemListEntryFromStack(currentPetInventoryListType, 39, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 41 {
		t.Fatalf("pet serial=%d want=41", got)
	}
	if got := binary.LittleEndian.Uint32(entry.data[currentPetRemainSecondsOffset : currentPetRemainSecondsOffset+4]); got != 0 {
		t.Fatalf("pet remaining seconds=%d want permanent", got)
	}
}

func TestCurrentPetRemainingSecondsAtPreservesRealExpiration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	if got := currentPetRemainingSecondsAt(uint32(now.Unix()+3_600), now); got != 3_600 {
		t.Fatalf("remaining seconds=%d want=3600", got)
	}
	if got := currentPetRemainingSecondsAt(uint32(now.Unix()-1), now); got != 0 {
		t.Fatalf("expired remaining seconds=%d want=0", got)
	}
}

func TestCurrentStackableItemRowSuppliesStableOp44Identity(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID: 10000388,
		Count:  82,
		Extra: map[string]string{
			"item_kind":       "stackable",
			"pvf_path":        "stackable/cash/contract_growth_3_10000388.stk",
			"amount":          "82",
			"amount_or_count": "82",
		},
	}
	entry := currentItemListEntryFromStack(0, 90, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x0E:0x12]); got != uint32(stack.ItemID) {
		t.Fatalf("stackable raw+0x0E identity=%d want item id %d", got, stack.ItemID)
	}

	stack.Extra["instance_value"] = "27521"
	entry = currentItemListEntryFromStack(0, 90, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x0E:0x12]); got != 27521 {
		t.Fatalf("explicit stackable identity=%d want 27521", got)
	}
}

func TestCurrentMainStackableItemRowPrefersDurableCountOverStaleLegacyAmount(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], 117)
	stack := dnfrepo.ItemStack{
		ItemID:   31,
		Count:    108,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":       "stackable",
			"amount":          "117",
			"amount_or_count": "117",
		},
	}

	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 69, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 108 {
		t.Fatalf("main stackable amount=%d want durable count 108", got)
	}
}

func TestSendSelectedCurrentContainerListsSkipsAfterSelectBootstrapWithoutRefresh(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"7:140": {ItemID: 10006784, Count: 1, Extra: map[string]string{"creature_serial_or_handle": "1001"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                                 connection,
		connID:                               "pet-list7-scene-rehydrate",
		selectedCharacterID:                  77,
		selectedItemListRefreshSent:          true,
		selectedItemListBootstrapCharacterID: 77,
	}

	if err := service.sendSelectedCurrentContainerListsWithRefresh(session, "initial_town_before_full_player_state", false); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("non-refresh path repeated selected containers after actor-bound bootstrap: %x", connection.write.Bytes())
	}
}

func TestSendSelectedCurrentContainerListsRefreshReplaysDurableSortLocks(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			"0:64": {ItemID: 490701734, Count: 1, Extra: map[string]string{"item_sort_lock_state": "1"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "container-refresh-sort-lock",
		selectedCharacterID: 77,
	}

	if err := service.sendSelectedCurrentContainerListsWithRefresh(session, "sort_after_ack", true); err != nil {
		t.Fatal(err)
	}
	stream := connection.write.Bytes()
	foundSortLock := false
	for len(stream) != 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.Classification == dnfproto.DefaultChannelClassification &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketRequestItemSortLock) {
			if !bytes.Equal(packet.Body, []byte{1, 0, 0x40, 0}) {
				t.Fatalf("sort-lock replay body=%x", packet.Body)
			}
			foundSortLock = true
		}
		stream = rest
	}
	if !foundSortLock {
		t.Fatal("container refresh did not replay durable sort lock")
	}
}

func TestBuildCurrentItemListBodyGuildMedalUsesGenericCurrentRows(t *testing.T) {
	entry := currentItemListEntry{}
	entry.patchCore(7, 100380060, 1)
	body := buildCurrentItemListBody(currentGuildMedalInventoryListType, []currentItemListEntry{entry}, dnfrepo.CharacterContainerState{})
	if len(body) != 3+currentItemListEntryWireSize {
		t.Fatalf("list38 body len=%d want=%d", len(body), 3+currentItemListEntryWireSize)
	}
	if !bytes.Equal(body[:3], []byte{currentGuildMedalInventoryListType, 1, 0}) {
		t.Fatalf("list38 header=%x", body[:3])
	}
	if got := binary.LittleEndian.Uint16(body[3:5]); got != 7 {
		t.Fatalf("list38 slot=%d want=7", got)
	}
	if got := binary.LittleEndian.Uint32(body[5:9]); got != 100380060 {
		t.Fatalf("list38 item=%d want=100380060", got)
	}
}

func TestBuildCurrentItemListBodyForSessionInitializesMissingContainerState(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots:       map[string]dnfrepo.ItemStack{},
		Warehouse:   map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{connID: "missing-container-state", selectedCharacterID: 88}

	body, source, count, ok := service.buildCurrentItemListBodyForSession(ctx, session, 2)
	if !ok || source != "inventory" || count != 0 {
		t.Fatalf("list2 ok=%v source=%q count=%d", ok, source, count)
	}
	if !bytes.Equal(body, []byte{2, 8, 0, 0, 0, 0}) {
		t.Fatalf("list2 body=%x want=020800000000", body)
	}
	state, found, err := dnfrepo.LoadCharacterContainerState(ctx, repositories.Settings, "88")
	if err != nil || !found {
		t.Fatalf("load initialized state found=%v err=%v", found, err)
	}
	if state.MainSlotCount != 0 || state.AvatarExpansion != 0 || state.PersonalCargoSlotCount != 8 {
		t.Fatalf("initialized state=%+v", state)
	}
}

func TestCurrentItemListKeepsPersistedGoldWalletSentinel(t *testing.T) {
	entries := currentItemListEntriesFromMap(map[string]dnfrepo.ItemStack{
		"0:0": {ItemID: 0, Count: 777},
		"0:3": {ItemID: 3227, Count: 2},
		"0:4": {ItemID: -1, Count: 9},
	}, dnfrepo.MainInventoryListType)
	sortCurrentItemListEntries(entries)
	if len(entries) != 2 {
		t.Fatalf("entries=%d want=2", len(entries))
	}
	if binary.LittleEndian.Uint16(entries[0].data[0:2]) != 0 ||
		binary.LittleEndian.Uint32(entries[0].data[2:6]) != 0 ||
		binary.LittleEndian.Uint32(entries[0].data[6:10]) != 777 {
		t.Fatalf("wallet entry=%x", entries[0].data)
	}
}

func TestBuildCurrentItemListBodyForSessionProjectsAuthoritativeCharacterGoldOverStaleWallet(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88",
		Stats:       map[string]int64{"gold": 12345},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 0, Count: 777},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountPrefix: defaultAccountPrefix},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	body, source, count, ok := service.buildCurrentItemListBodyForSession(ctx, &gameSession{selectedCharacterID: 88}, 0)
	if !ok || count != 1 || !bytes.Contains([]byte(source), []byte("character_gold_wallet_projection")) {
		t.Fatalf("list0 ok=%t source=%q count=%d", ok, source, count)
	}
	if len(body) != 5+currentItemListEntryWireSize {
		t.Fatalf("list0 body len=%d", len(body))
	}
	wallet := body[5:]
	if binary.LittleEndian.Uint16(wallet[0:2]) != 0 ||
		binary.LittleEndian.Uint32(wallet[2:6]) != 0 ||
		binary.LittleEndian.Uint32(wallet[6:10]) != 12345 {
		t.Fatalf("projected wallet=%x", wallet[:14])
	}
}

// TestReverseCurrentMainItemListRowsFlipsListZeroWireOrder pins the wire-order
// contract that reverseCurrentMainItemListRows exists for: main-inventory rows
// leave the server descending by slot, every other list passes through, and the
// list0 header (container parameter + row count) is preserved verbatim.
func TestReverseCurrentMainItemListRowsFlipsListZeroWireOrder(t *testing.T) {
	entries := make([]currentItemListEntry, 3)
	for index, slot := range []int16{0, 3, 9} {
		entries[index].patchCore(slot, uint32(1000+index), 1)
	}
	body := buildCurrentItemListBody(0, entries, dnfrepo.CharacterContainerState{MainSlotCount: 24})
	if !bytes.Equal(body[:5], buildCurrentItemListBody(0, entries, dnfrepo.CharacterContainerState{MainSlotCount: 24})[:5]) {
		t.Fatalf("builder header changed: %x", body[:5])
	}
	reversed := reverseCurrentMainItemListRows(body)
	if !bytes.Equal(reversed[:5], body[:5]) {
		t.Fatalf("reversed header=%x want=%x", reversed[:5], body[:5])
	}
	if len(reversed) != len(body) {
		t.Fatalf("reversed len=%d want=%d", len(reversed), len(body))
	}
	rows := decodeCurrentItemListTestRows(t, reversed[5:])
	for index, wantSlot := range []int16{9, 3, 0} {
		if got := binary.LittleEndian.Uint16(rows[index][0:2]); got != uint16(wantSlot) {
			t.Fatalf("reversed row %d slot=%d want=%d", index, got, wantSlot)
		}
		if got := binary.LittleEndian.Uint32(rows[index][2:6]); got != uint32(1000+2-index) {
			t.Fatalf("reversed row %d item=%d want=%d", index, got, 1000+2-index)
		}
	}
	avatar := buildCurrentItemListBody(1, entries, dnfrepo.CharacterContainerState{})
	if got := reverseCurrentMainItemListRows(avatar); !bytes.Equal(got, avatar) {
		t.Fatalf("list1 body was reordered: %x", got)
	}
	single := buildCurrentItemListBody(0, entries[:1], dnfrepo.CharacterContainerState{MainSlotCount: 24})
	if got := reverseCurrentMainItemListRows(single); !bytes.Equal(got, single) {
		t.Fatalf("single-row list0 body was reordered: %x", got)
	}
}
