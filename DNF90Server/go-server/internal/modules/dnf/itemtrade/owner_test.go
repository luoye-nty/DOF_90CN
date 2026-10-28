package itemtrade

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestOwnerExchangeAtomicallyMovesBothOffers(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.InventoryRecord{
		{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{"0:97": {ItemID: 1001, Count: 1, RawEntry: []byte{1, 2, 3}}}},
		{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{"0:57": {ItemID: 2002, Count: 3}}},
	} {
		if err := repositories.Inventory.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return time.Unix(100, 0) }

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 0, SourceList: 0, SourceSlot: 97, Count: 1, ExpectedItem: 1001}}},
		Participant{CharacterID: "5", Offers: []Offer{{TradeSlot: 0, SourceList: 0, SourceSlot: 57, Count: 2, ExpectedItem: 2002}}},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	first := result.Inventories["1"]
	second := result.Inventories["5"]
	if first.Slots["0:9"].ItemID != 2002 || first.Slots["0:9"].Count != 2 {
		t.Fatalf("first received = %+v", first.Slots)
	}
	if second.Slots["0:65"].ItemID != 1001 || second.Slots["0:65"].Count != 1 || second.Slots["0:57"].Count != 1 {
		t.Fatalf("second inventory = %+v", second.Slots)
	}
	if len(result.Received["1"]) != 1 || result.Received["1"][0].TradeSlot != 0 || result.Received["1"][0].DestinationSlot != 9 {
		t.Fatalf("first transfer = %+v", result.Received["1"])
	}
}

func TestOwnerExchangeRejectsBoundItemWithoutMutation(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:97": {ItemID: 1001, Count: 1, Bind: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	_, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 0, SourceList: 0, SourceSlot: 97, Count: 1, ExpectedItem: 1001}}},
		Participant{CharacterID: "5"},
	)
	if !errors.Is(err, ErrItemUnavailable) {
		t.Fatalf("Exchange() error = %v", err)
	}
	loaded, _, _ := repositories.Inventory.Load(ctx, "1")
	if loaded.Slots["0:97"].ItemID != 1001 {
		t.Fatalf("bound source mutated: %+v", loaded.Slots)
	}
}

func TestOwnerExchangeRejectsLockedItemWithoutMutation(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"1", "2", "locked", "unlocking"} {
		t.Run(state, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
				CharacterID: "1",
				Slots: map[string]dnfrepo.ItemStack{"0:97": {ItemID: 1001, Count: 1, Extra: map[string]string{
					"equipment_lock_state": state,
				}}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			owner, _ := NewOwner(repositories)
			_, err := owner.Exchange(ctx,
				Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 0, SourceList: 0, SourceSlot: 97, Count: 1, ExpectedItem: 1001}}},
				Participant{CharacterID: "5"},
			)
			if !errors.Is(err, ErrItemNotTradeable) {
				t.Fatalf("Exchange() error = %v, want ErrItemNotTradeable", err)
			}
			loaded, _, _ := repositories.Inventory.Load(ctx, "1")
			stack := loaded.Slots["0:97"]
			if stack.ItemID != 1001 || stack.Extra["equipment_lock_state"] != state {
				t.Fatalf("locked source mutated: %+v", loaded.Slots)
			}
		})
	}
}

func TestOwnerExchangeAcceptsOnlyExplicitlyAuthorizedSealedItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	stack := dnfrepo.ItemStack{ItemID: 1001, Count: 1, Bind: true}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{"0:97": stack},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	participant := Participant{CharacterID: "1", Offers: []Offer{{
		TradeSlot: 0, SourceList: 0, SourceSlot: 97, Count: 1, ExpectedItem: 1001, ExpectedBind: true,
	}}}
	if _, err := owner.Exchange(ctx, participant, Participant{CharacterID: "5"}); !errors.Is(err, ErrItemNotTradeable) {
		t.Fatalf("sealed item without authorization error=%v", err)
	}
	participant.Offers[0].AllowSealed = true
	result, err := owner.Exchange(ctx, participant, Participant{CharacterID: "5"})
	if err != nil {
		t.Fatalf("authorized sealed exchange: %v", err)
	}
	if got := result.Inventories["5"].Slots["0:65"]; got.ItemID != 1001 || !got.Bind {
		t.Fatalf("received sealed item=%+v", got)
	}
}

func TestOwnerExchangeValidatesEquipmentInstance(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	raw := make([]byte, 0x77)
	// A faithful 0x77 equipment row: raw+0x02 is the item id and item_kind says
	// equipment. Those are what make raw+0x06 the quality seed rather than the
	// projected stack count.
	binary.LittleEndian.PutUint32(raw[2:6], 100230483)
	binary.LittleEndian.PutUint32(raw[6:10], 0x0A48BC23)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{"0:9": {
			ItemID: 100230483, Count: 1, RawEntry: raw,
			Extra: map[string]string{"item_kind": "equipment"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	bad := Participant{CharacterID: "1", Offers: []Offer{{
		TradeSlot: 2, SourceList: 0, SourceSlot: 9, Count: 1, ExpectedItem: 100230483, ExpectedInstance: 0x01020304,
	}}}
	if _, err := owner.Exchange(ctx, bad, Participant{CharacterID: "5"}); !errors.Is(err, ErrItemUnavailable) {
		t.Fatalf("mismatched equipment instance error = %v", err)
	}
	good := bad
	good.Offers = append([]Offer(nil), bad.Offers...)
	good.Offers[0].ExpectedInstance = 0x0A48BC23
	result, err := owner.Exchange(ctx, good, Participant{CharacterID: "5"})
	if err != nil {
		t.Fatalf("matching equipment instance error = %v", err)
	}
	if got := result.Inventories["5"].Slots["0:9"]; got.ItemID != 100230483 || got.Count != 1 {
		t.Fatalf("received equipment = %+v", got)
	}
}

func TestOwnerExchangeValidatesEquipmentInstanceFromDurableQualitySeed(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{"0:9": {
			ItemID: 100230483,
			Count:  1,
			Extra:  map[string]string{"item_kind": "equipment", "quality_seed": "669297134"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	bad := Participant{CharacterID: "1", Offers: []Offer{{
		TradeSlot: 2, SourceList: 0, SourceSlot: 9, Count: 1, ExpectedItem: 100230483, ExpectedInstance: 0x01020304,
	}}}
	if _, err := owner.Exchange(ctx, bad, Participant{CharacterID: "5"}); !errors.Is(err, ErrItemUnavailable) {
		t.Fatalf("mismatched metadata instance error = %v", err)
	}
	good := bad
	good.Offers = append([]Offer(nil), bad.Offers...)
	good.Offers[0].ExpectedInstance = 0x27E4A9EE
	result, err := owner.Exchange(ctx, good, Participant{CharacterID: "5"})
	if err != nil {
		t.Fatalf("matching metadata instance error = %v", err)
	}
	if got := result.Inventories["5"].Slots["0:9"]; got.ItemID != 100230483 || got.Count != 1 || got.Extra["quality_seed"] != "669297134" {
		t.Fatalf("received metadata-only equipment = %+v", got)
	}
}

func TestOwnerExchangeAtomicallyMovesGoldAndItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Stats: map[string]int64{"gold": 250_000}},
		{CharacterID: "5", AccountID: "b", Stats: map[string]int64{"gold": 40_000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5",
		Slots:       map[string]dnfrepo.ItemStack{"0:57": {ItemID: 2002, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5", Gold: 10_000, Offers: []Offer{{TradeSlot: 1, SourceList: 0, SourceSlot: 57, Count: 1, ExpectedItem: 2002}}},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got := result.Characters["1"].Stats["gold"]; got != 160_000 {
		t.Fatalf("first gold = %d", got)
	}
	if got := result.Characters["5"].Stats["gold"]; got != 130_000 {
		t.Fatalf("second gold = %d", got)
	}
	if got := result.Inventories["1"].Slots["0:9"]; got.ItemID != 2002 || got.Count != 1 {
		t.Fatalf("first received item = %+v", got)
	}
}

func TestOwnerExchangePlacesExpandedSourceSlotAtVisiblePageStart(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:110": {ItemID: 490702514, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 1, SourceList: 0, SourceSlot: 110, Count: 1, ExpectedItem: 490702514}}},
		Participant{CharacterID: "7"},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got := result.Inventories["7"].Slots["0:65"]; got.ItemID != 490702514 || got.Count != 1 {
		t.Fatalf("recipient visible consumable slot = %+v, inventory = %+v", got, result.Inventories["7"].Slots)
	}
	if got := result.Received["7"][0].DestinationSlot; got != 65 {
		t.Fatalf("destination slot = %d, want 65", got)
	}
}

// TestOwnerExchangePlacesEmblemFromTheNinthRowAtEmblemPageStart 覆盖徽章页第 9 行（345..351）。
//
// 服主现场口径：徽章页是 9 行 × 7 格 = 63（289..351）。这里的分页常量原先写 289..344，
// 把 345..351 划给了 special 页；那会让落在最后一行上的徽章**交易不出去**
// （destinationSlotRange 找不到所属页 → 整笔拒绝）。同时钉住 special 页只剩 352..353。
func TestOwnerExchangePlacesEmblemFromTheNinthRowAtEmblemPageStart(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		// 345 = 徽章页第 9 行第一格。
		Slots: map[string]dnfrepo.ItemStack{"0:345": {ItemID: 2520037, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 1, SourceList: 0, SourceSlot: 345, Count: 1, ExpectedItem: 2520037}}},
		Participant{CharacterID: "7"},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v（徽章页第 9 行必须能交易）", err)
	}
	// 落点回到徽章页页首（与"扩展格物品收进可见页首"同一口径）。
	if got := result.Inventories["7"].Slots["0:289"]; got.ItemID != 2520037 || got.Count != 1 {
		t.Fatalf("recipient emblem page start = %+v, inventory = %+v", got, result.Inventories["7"].Slots)
	}
	if got := result.Received["7"][0].DestinationSlot; got != 289 {
		t.Fatalf("destination slot = %d, want 289", got)
	}
	for _, slot := range []int16{345, 351} {
		if first, last, ok := destinationSlotRange(mainListType, slot); !ok || first != emblemFirst || last != emblemLast {
			t.Fatalf("slot %d page = %d..%d ok=%t, want emblem %d..%d", slot, first, last, ok, emblemFirst, emblemLast)
		}
	}
	if first, last, ok := destinationSlotRange(mainListType, 352); !ok || first != specialFirst || last != specialLast {
		t.Fatalf("slot 352 page = %d..%d ok=%t, want special %d..%d", first, last, ok, specialFirst, specialLast)
	}
	if emblemFirst != 289 || emblemLast != 351 {
		t.Fatalf("徽章页常量漂了：%d..%d", emblemFirst, emblemLast)
	}
}

func TestOwnerExchangeRejectsInsufficientGoldWithoutMovingItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "a", Stats: map[string]int64{"gold": 99}},
		{CharacterID: "5", AccountID: "b", Stats: map[string]int64{"gold": 0}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5",
		Slots:       map[string]dnfrepo.ItemStack{"0:57": {ItemID: 2002, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, _ := NewOwner(repositories)
	_, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100},
		Participant{CharacterID: "5", Offers: []Offer{{TradeSlot: 1, SourceList: 0, SourceSlot: 57, Count: 1, ExpectedItem: 2002}}},
	)
	if !errors.Is(err, ErrGoldUnavailable) {
		t.Fatalf("Exchange() error = %v", err)
	}
	character, _, _ := repositories.Character.Load(ctx, "1")
	if got := character.Stats["gold"]; got != 99 {
		t.Fatalf("first gold mutated to %d", got)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "5")
	if got := inventory.Slots["0:57"]; got.ItemID != 2002 || got.Count != 1 {
		t.Fatalf("offered item mutated: %+v", inventory.Slots)
	}
}

func TestOwnerExchangeWithCompletionKeyReplaysIndeterminateCommit(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "seller", Stats: map[string]int64{"gold": 10}},
		{CharacterID: "5", AccountID: "buyer", Stats: map[string]int64{"gold": 100}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:65": {ItemID: 2002, Count: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	indeterminate := &commitThenErrorCharacterTrade{delegate: repositories.CharacterTrade, remainingErrors: 1}
	repositories.CharacterTrade = indeterminate
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	seller := Participant{CharacterID: "1", Offers: []Offer{{TradeSlot: 7, SourceList: 0, SourceSlot: 65, Count: 1, ExpectedItem: 2002}}}
	buyer := Participant{CharacterID: "5", Gold: 25}
	if _, err := owner.ExchangeWithCompletionKey(ctx, "private-store:1", "5", seller, buyer); err == nil {
		t.Fatal("first indeterminate commit returned nil error")
	}
	result, err := owner.ExchangeWithCompletionKey(ctx, "private-store:1", "5", seller, buyer)
	if err != nil {
		t.Fatalf("replay error = %v", err)
	}
	if !result.Replayed || len(result.Received["5"]) != 1 || result.Received["5"][0].Stack.ItemID != 2002 {
		t.Fatalf("replay result = %+v", result)
	}
	sellerCharacter, _, _ := repositories.Character.Load(ctx, "1")
	buyerCharacter, _, _ := repositories.Character.Load(ctx, "5")
	sellerInventory, _, _ := repositories.Inventory.Load(ctx, "1")
	buyerInventory, _, _ := repositories.Inventory.Load(ctx, "5")
	if sellerCharacter.Stats["gold"] != 35 || buyerCharacter.Stats["gold"] != 75 ||
		sellerInventory.Slots["0:65"].Count != 1 || buyerInventory.Slots["0:65"].Count != 1 {
		t.Fatalf("seller=%+v buyer=%+v seller_inventory=%+v buyer_inventory=%+v", sellerCharacter, buyerCharacter, sellerInventory, buyerInventory)
	}
	if _, err := owner.ExchangeWithCompletionKey(ctx, "private-store:1", "5", seller, Participant{CharacterID: "5", Gold: 26}); !errors.Is(err, ErrReceiptInvalid) {
		t.Fatalf("changed authority error = %v", err)
	}
}

type commitThenErrorCharacterTrade struct {
	delegate        dnfrepo.CharacterTradeUnitOfWork
	remainingErrors int
}

func (u *commitThenErrorCharacterTrade) WithinCharacterTrade(
	ctx context.Context,
	firstCharacterID string,
	secondCharacterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository) error,
) error {
	if err := u.delegate.WithinCharacterTrade(ctx, firstCharacterID, secondCharacterID, apply); err != nil {
		return err
	}
	if u.remainingErrors > 0 {
		u.remainingErrors--
		return errors.New("simulated commit outcome unknown")
	}
	return nil
}

var _ dnfrepo.CharacterTradeUnitOfWork = (*commitThenErrorCharacterTrade)(nil)

// The account-owned crystal/soul warehouse is a legal trade source. Its cells
// stay account-owned on both sides, so the debit and the credit both commit
// against the account record inside the wider account/character transaction.
func TestOwnerExchangeMovesAccountWarehouseStackWithinOneAccount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	const accountID = "dnf:acct"
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: accountID,
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []dnfrepo.InventoryRecord{
		{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{}},
		{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}},
	} {
		if err := repositories.Inventory.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: accountID, Stats: map[string]int64{"gold": 0}},
		{CharacterID: "5", AccountID: accountID, Stats: map[string]int64{"gold": 5000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return time.Unix(100, 0) }

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", AccountID: accountID, Offers: []Offer{
			{TradeSlot: 3, SourceList: 0, SourceSlot: 358, Count: 200, ExpectedItem: 3037},
		}},
		Participant{CharacterID: "5", AccountID: accountID, Gold: 1500},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	transfers := result.Received["5"]
	if len(transfers) != 1 || transfers[0].DestinationList != 0 || transfers[0].DestinationSlot != 358 {
		t.Fatalf("buyer transfers = %+v", transfers)
	}
	// One account owns one warehouse, so the cell keeps its total: the trade
	// moves gold between the two characters and leaves the shared stack intact.
	// Selling to your own alt is therefore a gold-only transfer.
	stored, found, err := repositories.AccountInventory.Load(ctx, accountID)
	if err != nil || !found {
		t.Fatalf("account inventory load found=%t err=%v", found, err)
	}
	if stored.Slots["0:358"].ItemID != 3037 || stored.Slots["0:358"].Count != 1000 {
		t.Fatalf("account warehouse = %+v", stored.Slots)
	}
	if result.Inventories["5"].Slots["0:358"].Count != 1000 {
		t.Fatalf("buyer merged view = %+v", result.Inventories["5"].Slots)
	}
	if result.Characters["1"].Stats["gold"] != 1500 || result.Characters["5"].Stats["gold"] != 3500 {
		t.Fatalf("gold seller=%d buyer=%d", result.Characters["1"].Stats["gold"], result.Characters["5"].Stats["gold"])
	}
}

// A cross-account sale is the real case: the crystal leaves the seller's
// warehouse and lands in the buyer's own warehouse cell, both inside one
// transaction.
func TestOwnerExchangeMovesAccountWarehouseStackAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.AccountInventoryRecord{
		{AccountID: "dnf:a", Slots: map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 1000}}},
		{AccountID: "dnf:b", Slots: map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 7}}},
	} {
		if err := repositories.AccountInventory.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []dnfrepo.InventoryRecord{
		{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{}},
		{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}},
	} {
		if err := repositories.Inventory.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:a", Stats: map[string]int64{"gold": 0}},
		{CharacterID: "5", AccountID: "dnf:b", Stats: map[string]int64{"gold": 5000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return time.Unix(100, 0) }

	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", AccountID: "dnf:a", Offers: []Offer{
			{TradeSlot: 3, SourceList: 0, SourceSlot: 358, Count: 200, ExpectedItem: 3037},
		}},
		Participant{CharacterID: "5", AccountID: "dnf:b", Gold: 1500},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	seller, found, err := repositories.AccountInventory.Load(ctx, "dnf:a")
	if err != nil || !found || seller.Slots["0:358"].Count != 800 {
		t.Fatalf("seller warehouse found=%t err=%v slots=%+v", found, err, seller.Slots)
	}
	buyer, found, err := repositories.AccountInventory.Load(ctx, "dnf:b")
	if err != nil || !found || buyer.Slots["0:358"].Count != 207 || buyer.Slots["0:358"].ItemID != 3037 {
		t.Fatalf("buyer warehouse found=%t err=%v slots=%+v", found, err, buyer.Slots)
	}
	if result.Inventories["5"].Slots["0:358"].Count != 207 || result.Inventories["1"].Slots["0:358"].Count != 800 {
		t.Fatalf("merged views seller=%+v buyer=%+v", result.Inventories["1"].Slots, result.Inventories["5"].Slots)
	}
	if result.Characters["1"].Stats["gold"] != 1500 || result.Characters["5"].Stats["gold"] != 3500 {
		t.Fatalf("gold seller=%d buyer=%d", result.Characters["1"].Stats["gold"], result.Characters["5"].Stats["gold"])
	}
}

// An account-owned offer without a resolved account owner must refuse: the
// transaction cannot decide which warehouse to debit.
func TestOwnerExchangeRefusesAccountWarehouseWithoutAccountOwner(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:a",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.Exchange(ctx,
		Participant{CharacterID: "1", AccountID: "dnf:a", Offers: []Offer{
			{TradeSlot: 0, SourceList: 0, SourceSlot: 358, Count: 1, ExpectedItem: 3037},
		}},
		Participant{CharacterID: "5"},
	)
	if !errors.Is(err, ErrItemNotTradeable) {
		t.Fatalf("Exchange() error = %v, want ErrItemNotTradeable", err)
	}
}

// A private-store row freezes ExpectedInstance from the list projection, which
// falls back to the stack count when the row is not a full 0x77 equipment
// entry. Reading RawEntry[6:10] for such a row answered 0 and every weapon
// purchase failed with ErrItemUnavailable.
func TestOwnerExchangeAcceptsLegacyEquipmentRowInstance(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	legacy := dnfrepo.ItemStack{
		ItemID: 101010912,
		Count:  1,
		Extra:  map[string]string{"item_kind": "equipment"},
		// The live 43-byte row: raw+0x06 is not the quality seed here.
		RawEntry: []byte{0x0b, 0xe0, 0x4d, 0x05, 0x06, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2c, 0x00, 0x00},
	}
	if got := itemTradeEquipmentInstance(legacy); got != 1 {
		t.Fatalf("legacy equipment instance = %d, want the projected count 1", got)
	}
	for _, record := range []dnfrepo.InventoryRecord{
		{CharacterID: "1", Slots: map[string]dnfrepo.ItemStack{"0:16": legacy}},
		{CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{}},
	} {
		if err := repositories.Inventory.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return time.Unix(100, 0) }
	result, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Offers: []Offer{
			{TradeSlot: 4, SourceList: 0, SourceSlot: 16, Count: 1, ExpectedItem: 101010912, ExpectedInstance: 1},
		}},
		Participant{CharacterID: "5"},
	)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if len(result.Received["5"]) != 1 || result.Received["5"][0].Stack.ItemID != 101010912 {
		t.Fatalf("buyer transfers = %+v", result.Received["5"])
	}
}
