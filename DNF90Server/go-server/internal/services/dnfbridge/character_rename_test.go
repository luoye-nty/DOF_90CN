package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func currentCharacterRenameRequestBody(selector byte, oldName, newName string, state byte) []byte {
	body := make([]byte, 1+4+len(oldName)+4+len(newName)+1)
	body[0] = selector
	offset := 1
	binary.LittleEndian.PutUint32(body[offset:offset+4], uint32(len(oldName)))
	offset += 4
	copy(body[offset:], oldName)
	offset += len(oldName)
	binary.LittleEndian.PutUint32(body[offset:offset+4], uint32(len(newName)))
	offset += 4
	copy(body[offset:], newName)
	body[len(body)-1] = state
	return body
}

func TestCurrentCharacterRenameParsersRequireExactWireShape(t *testing.T) {
	useCardBody := []byte{0x05, 0x00}
	request, err := parseCurrentUseCharacterRenameCard(useCardBody)
	if err != nil || request.Slot != 5 {
		t.Fatalf("parse op509 request=%+v err=%v", request, err)
	}
	if _, err := parseCurrentUseCharacterRenameCard(append(useCardBody, 0)); err == nil {
		t.Fatal("op509 accepted trailing bytes")
	}
	body := currentCharacterRenameRequestBody(0, "Old", "New", 1)
	request2, err := parseCurrentCharacterRename(body)
	if err != nil || request2.Selector != 0 || string(request2.OldRaw) != "Old" || string(request2.NewRaw) != "New" || request2.State != 1 {
		t.Fatalf("parse op135 request=%+v err=%v", request2, err)
	}
	if _, err := parseCurrentCharacterRename(append(body, 0)); err == nil {
		t.Fatal("op135 accepted trailing bytes")
	}
	legacy := append(append([]byte(nil), body...), 0, 0, 0, 0)
	if normalized := normalizeLegacyCharacterRenameBody(legacy); !bytes.Equal(normalized, body) {
		t.Fatalf("legacy normalization=%x want=%x", normalized, body)
	}
}

func TestCurrentCeraShopRenameProductAddsCharacterEntitlementWithoutInventoryRow(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 9000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Level:       50,
		Stats: map[string]int64{
			"cera":                                9000,
			currentCharacterRenameEntitlementStat: 0,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: connection, connID: "rename-purchase-test", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopTestRequestBody(100902)); err != nil {
		t.Fatal(err)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != uint16(dnfenum.CmdPacketBuyCerashopItem) {
		t.Fatalf("purchase ack header=%+v", first.Header)
	}
	balance, rest := splitGameServerUpperPacket(t, rest)
	if balance.Header.MsgID != currentCeraShopUpdateMsgID || !bytes.Equal(balance.Body, buildCurrentCeraShopBalanceBody(200)) {
		t.Fatalf("balance packet header=%+v body=%x", balance.Header, balance.Body)
	}
	renameAck, rest := splitGameServerUpperPacket(t, rest)
	if len(rest) != 0 || renameAck.Header.MsgID != uint16(dnfenum.CmdPacketUseRenameCard) || !bytes.Equal(renameAck.Body, []byte{1}) {
		t.Fatalf("rename entitlement ack header=%+v body=%x trailing=%d", renameAck.Header, renameAck.Body, len(rest))
	}
	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || character.Stats[currentCharacterRenameEntitlementStat] != 1 {
		t.Fatalf("character entitlement=%+v found=%t err=%v", character.Stats, found, err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found || len(inventory.Slots) != 0 {
		t.Fatalf("rename purchase created inventory row: %+v found=%t err=%v", inventory.Slots, found, err)
	}
	if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 200 {
		t.Fatalf("account cera=%d want=200", got)
	}
}

func TestCurrentCeraShopRenameProductRejectsBeforeChargingWhenUnavailable(t *testing.T) {
	now := time.Date(2026, time.August, 25, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		level int
		stats map[string]int64
	}{
		{name: "below_level_30", level: 29, stats: map[string]int64{}},
		{name: "entitlement_already_active", level: 90, stats: map[string]int64{currentCharacterRenameEntitlementStat: 1}},
		{name: "rename_cooldown_active", level: 90, stats: map[string]int64{currentCharacterRenameLastUnixStat: now.Add(-24 * time.Hour).Unix()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			catalog := mustCurrentCeraShopTestCatalog(t)
			repositories := dnfrepomemory.NewMemoryGroup()
			ctx := context.Background()
			seedCeraShopAccountCera(t, ctx, repositories, "account-1", 9000)
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19",
				AccountID:   "account-1",
				Level:       test.level,
				Stats:       test.stats,
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			service := newCeraShopPremiumTestService(repositories, catalog)
			service.gameplayTimers = &processGameplayTimeQueue{now: func() time.Time { return now }}
			request, err := parseCurrentCeraShopPurchaseRequest(currentCeraShopTestRequestBody(100902))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.commitCurrentCeraShopPurchase(ctx, &gameSession{selectedCharacterID: 19}, catalog, request); err == nil {
				t.Fatal("unavailable rename product purchase succeeded")
			}
			if got := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); got != 9000 {
				t.Fatalf("rejected rename purchase charged cera=%d, want 9000", got)
			}
			character, found, err := repositories.Character.Load(ctx, "19")
			if err != nil || !found || character.Stats[currentCharacterRenameEntitlementStat] != test.stats[currentCharacterRenameEntitlementStat] ||
				character.Stats[currentCharacterRenameLastUnixStat] != test.stats[currentCharacterRenameLastUnixStat] {
				t.Fatalf("rejected rename purchase mutated character=%+v found=%t err=%v", character, found, err)
			}
		})
	}
}

func TestCurrentUseCharacterRenameCardConsumesLegacyAndMisissuedRows(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	for _, itemID := range []int64{2660698, 2682290} {
		t.Run(strconv.FormatInt(itemID, 10), func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			ctx := context.Background()
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Level: 50, Stats: map[string]int64{currentCharacterRenameEntitlementStat: 0}}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
				"0:5": {ItemID: itemID, Count: 1},
			}}); err != nil {
				t.Fatal(err)
			}
			result, err := commitCurrentUseCharacterRenameCard(ctx, repositories, catalog, "account-1", "19", currentUseCharacterRenameCardRequest{Slot: 5}, time.Unix(1800000000, 0).UTC())
			if err != nil {
				t.Fatal(err)
			}
			if got := binary.LittleEndian.Uint32(result.Update.data[0x02:0x06]); got != ^uint32(0) {
				t.Fatalf("consumed update=%+v", result.Update)
			}
			character, _, _ := repositories.Character.Load(ctx, "19")
			if character.Stats[currentCharacterRenameEntitlementStat] != 1 {
				t.Fatalf("entitlement stats=%v", character.Stats)
			}
			inventory, _, _ := repositories.Inventory.Load(ctx, "19")
			if _, found := inventory.Slots["0:5"]; found {
				t.Fatalf("card row was not consumed: %+v", inventory.Slots["0:5"])
			}
		})
	}
}

func TestCurrentCharacterRenamePersistsNameEntitlementAndOldNameReservation(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "OldName", Level: 50,
		Stats: map[string]int64{currentCharacterRenameEntitlementStat: 6},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0).UTC()
	if err := commitCurrentCharacterRename(ctx, repositories, "account-1", "19", "OldName", "NewName", 7, now); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || character.Name != "NewName" || character.Stats[currentCharacterRenameEntitlementStat] != 0 || character.Stats[currentCharacterRenameLastUnixStat] != now.Unix() {
		t.Fatalf("persisted character=%+v found=%t err=%v", character, found, err)
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	reservationKey, err := characterRenameReservationMetadataKey("OldName")
	if err != nil || account.Metadata[reservationKey] == "" {
		t.Fatalf("old-name reservation key=%q metadata=%v err=%v", reservationKey, account.Metadata, err)
	}
}

func TestCharacterRenameEntitlementProjectsNativeRosterMarker(t *testing.T) {
	character := dnfrepo.CharacterRecord{Name: "Eligible", Stats: map[string]int64{currentCharacterRenameEntitlementStat: 1}}
	if got := currentCharacterRenameRosterName(character); got != "stdnfEligibl" {
		t.Fatalf("projected name=%q", got)
	}
	character.Stats[currentCharacterRenameEntitlementStat] = 0
	if got := currentCharacterRenameRosterName(character); got != "Eligible" {
		t.Fatalf("ordinary name=%q", got)
	}
}

func TestCharacterRenameTargetUsesNativeRosterSlotSelector(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Slot: 2, Name: "First"},
		{CharacterID: "20", AccountID: "account-1", Slot: 7, Name: "Target"},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	targetID, err := currentCharacterRenameTargetID(ctx, repositories.Character, "account-1", 7)
	if err != nil || targetID != "20" {
		t.Fatalf("target id=%q err=%v", targetID, err)
	}
	if _, err := currentCharacterRenameTargetID(ctx, repositories.Character, "account-1", 3); !errors.Is(err, errCurrentCharacterRenameOwnerInvalid) {
		t.Fatalf("missing selector error=%v", err)
	}
}

// rosterPendingRenameFlagOffset is the offset of the two pending-rename bytes
// inside one NoPack roster entry that starts at entryStart.
func rosterPendingRenameFlagOffset(entryStart int, character dnfrepo.CharacterRecord) int {
	return entryStart + 2 + 4 + len(rosterRawNameBytes(character))
}

func TestNoPackRosterCarriesPendingRenameFlagsWithoutMovingAnyOtherByte(t *testing.T) {
	ordinary := dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Slot: 0, Name: "1111", Job: "0", Level: 90,
	}
	pending := dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: "account-1", Slot: 1, Name: "2222", Job: "0", Level: 90,
		Stats: map[string]int64{currentCharacterRenameEntitlementStat: 1},
	}
	body := buildCSharpRosterBody([]dnfrepo.CharacterRecord{ordinary, pending})
	assertCSharpRosterBody(t, body, 2)

	firstStart := 15
	secondStart := firstStart + noPackRosterEntryLen(ordinary)
	if got, want := len(body), secondStart+noPackRosterEntryLen(pending)+10; got != want {
		t.Fatalf("roster body len=%d want=%d (rename flags must not change the row length)", got, want)
	}

	firstFlags := rosterPendingRenameFlagOffset(firstStart, ordinary)
	if got := body[firstFlags : firstFlags+noPackRosterPreJobBytes]; !bytes.Equal(got, []byte{0, 0}) {
		t.Fatalf("ordinary roster rename flags=%x want=0000", got)
	}
	secondFlags := rosterPendingRenameFlagOffset(secondStart, pending)
	if got := body[secondFlags : secondFlags+noPackRosterPreJobBytes]; !bytes.Equal(got, []byte{1, 1}) {
		t.Fatalf("pending roster rename flags=%x want=0101", got)
	}

	// Only the slot, the character name and these two flags may differ between
	// the two rows. Everything else has to stay identical, which is what keeps
	// every ordinary character card byte-for-byte compatible with the previous
	// projection.
	first := append([]byte(nil), body[firstStart:firstStart+noPackRosterEntryLen(ordinary)]...)
	second := append([]byte(nil), body[secondStart:secondStart+noPackRosterEntryLen(pending)]...)
	if len(first) != len(second) {
		t.Fatalf("roster rows differ in length: %d vs %d", len(first), len(second))
	}
	firstLocalFlags := firstFlags - firstStart
	for idx := range first {
		if idx < firstLocalFlags+noPackRosterPreJobBytes {
			first[idx] = 0
			second[idx] = 0
		}
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("roster rows differ outside the slot and rename flags:\n%x\n%x", first, second)
	}
}

// TestCurrentCharacterRenameCardCooldownWindow covers both sides of the restored
// 90-day rule (operator-confirmed retail behaviour: rename cooldown 90 days, and
// the old name stays reserved for 180 days).
//
//   - inside the window a card must not even be consumed, because the card would
//     be gone with nothing to show for it;
//   - just outside the window a second card arms and commits normally, which is
//     the flow the operator verified live on 2026-09-23.
func TestCurrentCharacterRenameCardCooldownWindow(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	cases := []struct {
		name       string
		lastRename time.Time
		wantErr    bool
	}{
		{name: "one hour after the previous rename", lastRename: now.Add(-time.Hour), wantErr: true},
		{name: "eighty-nine days after the previous rename", lastRename: now.Add(-89 * 24 * time.Hour), wantErr: true},
		{name: "ninety-one days after the previous rename", lastRename: now.Add(-91 * 24 * time.Hour)},
		{name: "never renamed", lastRename: time.Time{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			catalog := mustCurrentCeraShopTestCatalog(t)
			repositories := dnfrepomemory.NewMemoryGroup()
			ctx := context.Background()
			stats := map[string]int64{currentCharacterRenameEntitlementStat: 0}
			if !test.lastRename.IsZero() {
				stats[currentCharacterRenameLastUnixStat] = test.lastRename.Unix()
			}
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19", AccountID: "account-1", Name: "礼白", Level: 90, Stats: stats,
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
				"0:114": {ItemID: 2660698, Count: 1},
			}}); err != nil {
				t.Fatal(err)
			}
			_, err := commitCurrentUseCharacterRenameCard(ctx, repositories, catalog, "account-1", "19",
				currentUseCharacterRenameCardRequest{Slot: 114}, now)
			if test.wantErr {
				if !errors.Is(err, errCurrentCharacterRenameCooldownActive) {
					t.Fatalf("arming inside the cooldown err=%v want cooldown", err)
				}
				inventory, _, _ := repositories.Inventory.Load(ctx, "19")
				if _, kept := inventory.Slots["0:114"]; !kept {
					t.Fatal("a rejected card use consumed the card")
				}
				return
			}
			if err != nil {
				t.Fatalf("arming outside the cooldown: %v", err)
			}
			character, found, err := repositories.Character.Load(ctx, "19")
			if err != nil || !found || !currentCharacterRenamePending(character) {
				t.Fatalf("card armed=%+v found=%t err=%v", character.Stats, found, err)
			}
			if err := commitCurrentCharacterRename(ctx, repositories, "account-1", "19", "礼白", "礼黑", 1, now); err != nil {
				t.Fatalf("committing outside the cooldown: %v", err)
			}
			renamed, _, err := repositories.Character.Load(ctx, "19")
			if err != nil || renamed.Name != "礼黑" || renamed.Stats[currentCharacterRenameLastUnixStat] != now.Unix() {
				t.Fatalf("renamed character=%+v err=%v", renamed, err)
			}
		})
	}
}

// TestCurrentCharacterRenameCardRecognitionUsesThePVFActionType locks the widened
// contract: any stackable that declares
//
//	[action type]
//	`[character name change]`
//
// is a rename card wherever its PVF file lives. Recognition used to be a
// two-entry file-name allowlist, so every other card in the runtime PVF (18
// stackables declare the action type: the event cards, the account-bound
// 冒险家征集令 card, the localization-key variants) was answered with "card is
// invalid". A card that renames something else must stay rejected.
func TestCurrentCharacterRenameCardRecognitionUsesThePVFActionType(t *testing.T) {
	source := currentCeraShopTestSource()
	// 490007007 is an event card on a path the old allowlist never mentioned;
	// 9005 is the pet card, which must not rename a character.
	source["stackable/stackable.lst"] += "490007007 `490007001/chn_490007007.stk`\n9005 `cash/creature/rename_card.stk`\n"
	source["stackable/490007001/chn_490007007.stk"] = "[name]\n`角色更名卡`\n[stackable type]\n`[waste]`\n[action type]\n`[character name change]`\n[stack limit]\n1\n"
	catalog, err := newPVFCeraShopCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		itemID  uint32
		autoUse bool
		card    bool
		why     string
	}{
		{itemID: 2660698, autoUse: false, card: true, why: "cash/charcter_rename_card.stk bag card"},
		{itemID: 2682290, autoUse: true, card: true, why: "cash/chn_character_rename.stk Cera shop product"},
		{itemID: 490007007, autoUse: false, card: true, why: "event card outside the old allowlist"},
		{itemID: 9005, autoUse: false, card: false, why: "宠物更名卡 renames a creature, not the character"},
		{itemID: 37, autoUse: false, card: false, why: "ordinary consumable"},
	}
	for _, test := range cases {
		definition, resolveErr := catalog.items.ResolveItem(test.itemID)
		if resolveErr != nil {
			t.Fatalf("resolve item %d (%s): %v", test.itemID, test.why, resolveErr)
		}
		autoUse, card := catalog.characterRenameCardKind(definition)
		if card != test.card {
			t.Fatalf("item %d (%s) card=%t want %t (path=%s)", test.itemID, test.why, card, test.card, definition.PVFPath)
		}
		if card && autoUse != test.autoUse {
			t.Fatalf("item %d (%s) autoUse=%t want %t", test.itemID, test.why, autoUse, test.autoUse)
		}
	}
}

// TestCurrentUseCharacterRenameCardReturnsTheClientToCharacterSelect pins the
// server-owned return the card flow needs.
//
// The operator's report: using a bag 角色更名卡 consumed the card and set the
// pending-rename state, but the client sat in town doing nothing, and the
// character-select screen only appeared when the return button was clicked by
// hand. The 86-generation reference server returns the player itself after
// USE_RENAME_CARD; our client switches modules when it receives the class-1 op7
// ACK, so the server has to send the same sequence a client-requested return
// produces: op14 item update, op509 success ACK, op6 town-actor removal, op7 ACK.
func TestCurrentUseCharacterRenameCardReturnsTheClientToCharacterSelect(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Slot: 0, Name: "礼白", Level: 90,
		Stats: map[string]int64{currentCharacterRenameEntitlementStat: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:113": {ItemID: 2660698, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}

	service := newCeraShopPremiumTestService(repositories, catalog)
	service.options.gameUpperHeader = gameUpperHeaderServer16
	service.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	service.gameSessions = make(map[uint16]*gameSession)
	service.onlinePlayers = newOnlinePlayerManager()
	connection := &bufferConn{}
	session := &gameSession{
		conn:                         connection,
		connID:                       "rename-card-return-select",
		accountID:                    "account-1",
		selectedCharacterID:          19,
		initialTownRouteCharacterID:  19,
		townSceneReadyCharacterID:    19,
		sceneBootstrapTailSent:       true,
		selectedItemListRefreshSent:  true,
		selectPreviewObjectStateSent: true,
		townSelectorOriginBound:      true,
		townSelectorOriginSnapshot:   currentTownPositionSnapshot{CharacterID: 19, PositionValid: true},
	}
	service.gameSessions[19] = session
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 19, TownID: 38, AreaID: 0})

	if err := service.handleCurrentUseCharacterRenameCard(session, []byte{0x71, 0x00}); err != nil {
		t.Fatalf("use rename card: %v", err)
	}

	var msgIDs []uint16
	rest := connection.write.Bytes()
	for len(rest) > 0 {
		packet, trailing := splitGameServerUpperPacket(t, rest)
		msgIDs = append(msgIDs, packet.Header.MsgID)
		rest = trailing
	}
	want := []uint16{
		uint16(dnfenum.CmdPacketWalkoutPartyMember),    // op14: the consumed card leaves the bag
		uint16(dnfenum.CmdPacketUseRenameCard),         // op509: success ACK
		currentTownUserLeaveMsgID,                      // op6: the retained town actor is removed
		uint16(dnfenum.CmdPacketReturnSelectCharacter), // op7: the client switches modules on this ACK
	}
	if len(msgIDs) != len(want) {
		t.Fatalf("card use sent msg_ids=%v want %v", msgIDs, want)
	}
	for idx := range want {
		if msgIDs[idx] != want[idx] {
			t.Fatalf("card use sent msg_ids=%v want %v", msgIDs, want)
		}
	}
	if session.selectedCharacterID != 0 {
		t.Fatalf("return did not retire the character scope: selected=%d", session.selectedCharacterID)
	}
	if _, tracked := service.gameSessions[19]; tracked {
		t.Fatal("return left the character registered in gameSessions")
	}
	character, _, err := repositories.Character.Load(ctx, "19")
	if err != nil || !currentCharacterRenamePending(character) {
		t.Fatalf("character after return=%+v err=%v", character.Stats, err)
	}
}

func TestCeraShopRenamePurchaseTurnsTheRosterRowIntoPendingRename(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 9000)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Slot:        0,
		Name:        "1111",
		Job:         "0",
		Level:       90,
		Stats: map[string]int64{
			"cera":                                9000,
			currentCharacterRenameEntitlementStat: 0,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: connection, connID: "rename-roster-test", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopTestRequestBody(100902)); err != nil {
		t.Fatal(err)
	}

	character, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || !currentCharacterRenamePending(character) {
		t.Fatalf("purchased character stats=%+v found=%t err=%v", character.Stats, found, err)
	}
	body := buildCSharpRosterBody([]dnfrepo.CharacterRecord{character})
	assertCSharpRosterBody(t, body, 1)
	flags := rosterPendingRenameFlagOffset(15, character)
	if got := body[flags : flags+noPackRosterPreJobBytes]; !bytes.Equal(got, []byte{1, 1}) {
		t.Fatalf("post-purchase roster rename flags=%x want=0101", got)
	}
}
