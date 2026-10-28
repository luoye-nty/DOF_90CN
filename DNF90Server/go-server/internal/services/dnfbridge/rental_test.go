package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrental "longheng.io/server/internal/modules/dnf/rental"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// rentalTestAttributes mirrors what the live 90CN [section] declares for every
// tier (`7 3`). Kept as a named value so the tests that only care about the
// wallet/asset commit still exercise a non-zero reinforcement.
var rentalTestAttributes = currentRentalItemAttributes{Reinforce: 7, Forge: 3}

func TestParseCurrentRentalCatalogPreservesJobTierPairs(t *testing.T) {
	source := initialEquipmentMemSource{currentRentalSystemPVFPath: `
[limit point]
400
[point charge]
` + "`pdungeon` 1 `gold` 100000" + `
[section]
0 1 7 3 1 1 1
1 20 7 3 1 1 1
[group]
` + "`[gun blader]` 0" + `
[package selection]
416000000 3
[/package selection]
[/group]
[group]
` + "`[gun blader]` 1" + `
[package selection]
416010000 5 416020000 5
[/package selection]
[/group]
`}
	catalog, err := parseCurrentRentalCatalog(source)
	if err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	if catalog.Limit != 400 || catalog.DungeonClearPoints != 1 || catalog.GoldPerPoint != 100000 {
		t.Fatalf("wallet config = limit %d clear %d gold %d", catalog.Limit, catalog.DungeonClearPoints, catalog.GoldPerPoint)
	}
	if tier, ok := catalog.tierForLevel(1); !ok || tier != 0 {
		t.Fatalf("level 1 tier = %d found=%t", tier, ok)
	}
	if tier, ok := catalog.tierForLevel(20); !ok || tier != 1 {
		t.Fatalf("level 20 tier = %d found=%t", tier, ok)
	}
	if cost, ok := catalog.itemCost("[gun blader]", 0, 416000000); !ok || cost != 3 {
		t.Fatalf("tier0 item cost = %d found=%t", cost, ok)
	}
	if _, ok := catalog.itemCost("[gun blader]", 0, 416010000); ok {
		t.Fatal("tier1 item leaked into tier0")
	}
	// Columns 3 and 4 of each [section] row are the tier's 强化/锻造 levels.
	// Dropping them is exactly the bug where the shop previews +7/+3 and the
	// granted weapon arrives at +0 with no way for the player to fix it.
	for _, want := range []struct {
		tier      int
		reinforce int64
		forge     int64
	}{{tier: 0, reinforce: 7, forge: 3}, {tier: 1, reinforce: 7, forge: 3}} {
		row, found := catalog.tierRow(want.tier)
		if !found {
			t.Fatalf("tier %d row missing", want.tier)
		}
		if row.Reinforce != want.reinforce || row.Forge != want.forge {
			t.Fatalf("tier %d attributes reinforce=%d forge=%d want %d/%d", want.tier, row.Reinforce, row.Forge, want.reinforce, want.forge)
		}
	}
	if _, found := catalog.tierRow(99); found {
		t.Fatal("unknown tier id resolved to a row")
	}
}

func TestParseCurrentRentalCatalogRejectsOutOfRangeTierAttributes(t *testing.T) {
	for _, test := range []struct {
		name string
		row  string
	}{
		{name: "reinforce past the packed field", row: "0 1 32 3 1 1 1"},
		{name: "negative reinforce", row: "0 1 -1 3 1 1 1"},
		{name: "negative forge", row: "0 1 7 -1 1 1 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initialEquipmentMemSource{currentRentalSystemPVFPath: `
[limit point]
400
[point charge]
` + "`pdungeon` 1 `gold` 100000" + `
[section]
` + test.row + `
[group]
` + "`[gun blader]` 0" + `
[package selection]
416000000 3
[/package selection]
[/group]
`}
			if _, err := parseCurrentRentalCatalog(source); !errors.Is(err, errRentalCatalogInvalid) {
				t.Fatalf("row=%q error=%v want errRentalCatalogInvalid", test.row, err)
			}
		})
	}
}

// The rental grant must hand out the grade the shop previews. [equipment grade]
// on the item PVF is the only declaration of it (ordinary equipment in this PVF
// has no such section), and every item in the live catalog says `superlative`.
// A catalog that ever says something else must fail loudly instead of quietly
// handing out an item that contradicts the preview.
func TestValidateCurrentRentalItemRequiresThePreviewedTopGrade(t *testing.T) {
	const (
		itemID  = uint32(416000000)
		pvfPath = "equipment/rental.equ"
		// Live rental items keep their ordinary equipment grade in [grade]
		// (14, 44, 89, ...), while the 24-hour term exists only in the
		// explanation. The old fixture incorrectly made [grade] the term.
		baseText = "[name]\n`test rental`\n[basic explain]\n`收回到期 24 小时`\n[grade]\n44\n[attach type]\n`[trade]`\n[usable job]\n`[gun blader]`\n[/usable job]\n[durability]\n45\n"
	)
	for _, test := range []struct {
		name    string
		grade   string
		wantErr bool
	}{
		{name: "superlative is the previewed grade", grade: "[equipment grade]\n`superlative`\n"},
		{name: "a different grade is refused", grade: "[equipment grade]\n`inferior`\n", wantErr: true},
		{name: "a missing grade is refused", grade: "", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := bridgePVFSource{pvfPath: baseText + test.grade}
			catalog := &pvfDungeonDropCatalog{source: source, itemCache: map[uint32]dungeonDropItemDefinition{
				itemID: {ItemID: itemID, Kind: dungeonDropItemEquipment, PVFPath: pvfPath, SlotStart: 9, SlotEnd: 64, Durability: 45},
			}}
			_, err := validateCurrentRentalItem(source, catalog, itemID, "[gun blader]")
			if test.wantErr && !errors.Is(err, errRentalItemInvalid) {
				t.Fatalf("error=%v want errRentalItemInvalid", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

// [grade] is equipment-grade metadata, not the rental term. The caller has
// already proved catalog membership; validation must not reject live catalog
// items merely because their ordinary grade is not 24.
func TestValidateCurrentRentalItemDoesNotTreatGradeAsRentalDuration(t *testing.T) {
	const (
		itemID  = uint32(416000001)
		pvfPath = "equipment/rental.equ"
	)
	itemText := func(grade string) string {
		return "[name]\n`test rental`\n[basic explain]\n`收回到期 24 小时`\n[grade]\n" + grade +
			"\n[attach type]\n`[trade]`\n[usable job]\n`[gun blader]`\n[/usable job]\n" +
			"[durability]\n45\n[equipment grade]\n`superlative`\n"
	}
	for _, grade := range []string{"14", "44", "89"} {
		t.Run("grade_"+grade, func(t *testing.T) {
			source := bridgePVFSource{pvfPath: itemText(grade)}
			catalog := &pvfDungeonDropCatalog{source: source, itemCache: map[uint32]dungeonDropItemDefinition{
				itemID: {ItemID: itemID, Kind: dungeonDropItemEquipment, PVFPath: pvfPath, SlotStart: 9, SlotEnd: 64, Durability: 45},
			}}
			if _, err := validateCurrentRentalItem(source, catalog, itemID, "[gun blader]"); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

// The reported field bug: the rental shop previews the catalog item at 最上级
// (90%) while the granted weapon landed in the bag as 最下级 (13%). The row's
// 品级 word is raw+0x06; without a seed itemquality.StackSeed returns 0 and the
// projector falls back to the stack count, so the grant must not take that path.
func TestCurrentRentalGrantedRowDoesNotFallBackToTheStackCountGrade(t *testing.T) {
	// The sentinel is real-machine verified (86JP labels 999999998 最上级) and is
	// shared with the gold grade box, so a change to it must be deliberate.
	if currentRentalQualitySeed != 999999998 {
		t.Fatalf("rental quality seed=%d want the top sentinel 999999998", currentRentalQualitySeed)
	}
	// Sanity-check the fallback the bug actually took, so this test cannot pass
	// by accident if the projector ever stops using the stack count.
	unseeded := dnfrepo.ItemStack{
		ItemID: 416000000,
		Count:  1,
		Extra:  map[string]string{"item_kind": "equipment", "source": currentRentalItemSource},
	}
	if got := currentItemListStackAmount(dnfrepo.MainInventoryListType, unseeded); got != 1 {
		t.Fatalf("unseeded equipment row no longer falls back to the count: %d", got)
	}

	stack := currentRentalStack(
		dnfrepo.ItemStack{},
		416000000,
		dungeonDropItemDefinition{ItemID: 416000000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/rental.equ", Durability: 45},
		rentalTestAttributes,
		time.Unix(1_790_224_057, 0).UTC(),
	)
	if got := currentItemListStackAmount(dnfrepo.MainInventoryListType, stack); got != currentRentalQualitySeed {
		t.Fatalf("granted row +0x06=%d want the top seed %d", got, currentRentalQualitySeed)
	}
	assertCurrentRentalQualitySeed(t, stack)
}

func TestCurrentRentalEffectiveLevelUsesOverEquipContract(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Unix(1_800_000_000, 0).UTC()
	catalog := &currentRentalCatalog{Tiers: []currentRentalTier{
		{Tier: 0, MinimumLevel: 1},
		{Tier: 1, MinimumLevel: 20},
		{Tier: 2, MinimumLevel: 30},
	}}
	account := dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			"premium_expire_22": strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		},
	}
	if err := repositories.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}

	effectiveLevel, active := currentRentalEffectiveLevel(ctx, repositories.Account, account.AccountID, 20, now)
	if !active || effectiveLevel != 30 {
		t.Fatalf("active contract effective level=%d active=%t, want 30/true", effectiveLevel, active)
	}
	if tier, ok := catalog.tierForLevel(effectiveLevel); !ok || tier != 2 {
		t.Fatalf("active contract tier=%d found=%t, want 2/true", tier, ok)
	}

	account.Metadata["premium_expire_22"] = strconv.FormatInt(now.Add(-time.Second).Unix(), 10)
	if err := repositories.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	effectiveLevel, active = currentRentalEffectiveLevel(ctx, repositories.Account, account.AccountID, 20, now)
	if active || effectiveLevel != 20 {
		t.Fatalf("expired contract effective level=%d active=%t, want 20/false", effectiveLevel, active)
	}
	if tier, ok := catalog.tierForLevel(effectiveLevel); !ok || tier != 1 {
		t.Fatalf("expired contract tier=%d found=%t, want 1/true", tier, ok)
	}

	effectiveLevel, active = currentRentalEffectiveLevel(ctx, nil, account.AccountID, 20, now)
	if active || effectiveLevel != 20 {
		t.Fatalf("missing repository effective level=%d active=%t, want 20/false", effectiveLevel, active)
	}
}

func TestCurrentRentalProtocolBodies(t *testing.T) {
	rentBody := make([]byte, currentRentalRequestWireSize)
	binary.LittleEndian.PutUint32(rentBody[13:17], 416000000)
	binary.LittleEndian.PutUint32(rentBody[17:21], uint32(11)|uint32(7)<<16)
	rent, err := decodeCurrentRentEquipmentRequest(rentBody)
	if err != nil || rent.ItemID != 416000000 || rent.PackedJobTier != uint32(11)|uint32(7)<<16 {
		t.Fatalf("decode rent = %+v err=%v", rent, err)
	}
	chargeBody := make([]byte, currentRentalRequestWireSize)
	binary.LittleEndian.PutUint32(chargeBody[13:17], 1)
	binary.LittleEndian.PutUint32(chargeBody[17:21], 10)
	charge, err := decodeCurrentChargeRentalPointRequest(chargeBody)
	if err != nil || charge.ChargeType != 1 || charge.Count != 10 {
		t.Fatalf("decode charge = %+v err=%v", charge, err)
	}
	state := buildCurrentRentalPointStateBody(97, []currentRentalActiveEntry{{ItemID: 416000000, ExpireUnix: 123456}})
	if len(state) != 16 || binary.LittleEndian.Uint32(state[0:4]) != 97 || binary.LittleEndian.Uint32(state[4:8]) != 1 || binary.LittleEndian.Uint32(state[8:12]) != 416000000 || binary.LittleEndian.Uint32(state[12:16]) != 123456 {
		t.Fatalf("rental state body = %x", state)
	}
	gold := buildCurrentGoldStateBody(1000000)
	if len(gold) != 3+currentItemListEntryWireSize || gold[0] != 0 || binary.LittleEndian.Uint16(gold[1:3]) != 1 || binary.LittleEndian.Uint32(gold[9:13]) != 1000000 {
		t.Fatalf("gold state body len=%d body=%x", len(gold), gold)
	}
	rentalExpire := time.Unix(1_800_000_000, 0).UTC()
	rentalStack := dnfrepo.ItemStack{
		ItemID:   401030031,
		Count:    1,
		ExpireAt: rentalExpire,
		Extra: map[string]string{
			"source":      currentRentalItemSource,
			"durability":  "45",
			"expire_time": strconv.FormatInt(rentalExpire.Unix(), 10),
		},
	}
	rentalEntry := currentItemListEntryFromStack(0, 9, rentalStack)
	rentalUpdate := buildCurrentItemUpdateBody(0, []currentItemListEntry{rentalEntry})
	if len(rentalUpdate) != 3+currentItemListEntryWireSize || rentalUpdate[0] != 0 || binary.LittleEndian.Uint16(rentalUpdate[1:3]) != 1 {
		t.Fatalf("rental op14 header len=%d body=%x", len(rentalUpdate), rentalUpdate)
	}
	row := rentalUpdate[3:]
	if binary.LittleEndian.Uint16(row[0:2]) != 9 || binary.LittleEndian.Uint32(row[2:6]) != 401030031 ||
		binary.LittleEndian.Uint32(row[6:10]) != 1 || binary.LittleEndian.Uint16(row[0x0b:0x0d]) != 45 ||
		binary.LittleEndian.Uint32(row[0x6e:0x72]) != 0 {
		t.Fatalf("rental op14 row=%x", row)
	}
}

func TestResetDungeonEntrySceneGatesClearsRentalWalletState(t *testing.T) {
	session := &gameSession{selectedRentalWalletStateSent: true}
	resetDungeonEntrySceneGates(session)
	if session.selectedRentalWalletStateSent {
		t.Fatal("rental wallet state gate survived scene reset")
	}
}

func TestSelectedRentalWalletStateDoesNotSendSyntheticGoldItemUpdate(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{currentRentalPointMetadataKey: "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{"gold": 1_000_000},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := Service{
		options: options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	if err := service.sendSelectedRentalWalletStateWithRefresh(session, "test_login", false); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentRentalStateMsgID {
		t.Fatalf("wallet packet class=%d msg=%d, want class0/op%d", packet.Header.Classification, packet.Header.MsgID, currentRentalStateMsgID)
	}
	if len(rest) != 0 {
		next, _ := splitGameServerUpperPacket(t, rest)
		t.Fatalf("wallet emitted an extra packet msg=%d; synthetic class0/op14 must not be sent", next.Header.MsgID)
	}
	if len(packet.Body) != 8 || binary.LittleEndian.Uint32(packet.Body[:4]) != 100 ||
		binary.LittleEndian.Uint32(packet.Body[4:8]) != 0 {
		t.Fatalf("rental wallet body=%x, want points=100 active_count=0", packet.Body)
	}
}

func TestSelectedRentalWalletStateDefersWhenActiveAssetLoadFails(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*dnfrepo.Group, error)
	}{
		{
			name: "inventory",
			inject: func(repositories *dnfrepo.Group, loadErr error) {
				repositories.Inventory = rentalFailingInventoryLoadRepository{
					InventoryRepository: repositories.Inventory,
					err:                 loadErr,
				}
			},
		},
		{
			name: "equipment",
			inject: func(repositories *dnfrepo.Group, loadErr error) {
				repositories.Equipment = rentalFailingEquipmentLoadRepository{
					EquipmentRepository: repositories.Equipment,
					err:                 loadErr,
				}
			},
		},
		{
			name: "inventory repository missing",
			inject: func(repositories *dnfrepo.Group, _ error) {
				repositories.Inventory = nil
			},
		},
		{
			name: "equipment repository missing",
			inject: func(repositories *dnfrepo.Group, _ error) {
				repositories.Equipment = nil
			},
		},
		{
			name: "account repository missing",
			inject: func(repositories *dnfrepo.Group, _ error) {
				repositories.Account = nil
			},
		},
		{
			name: "character repository missing",
			inject: func(repositories *dnfrepo.Group, _ error) {
				repositories.Character = nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "account-1",
				Metadata:  map[string]string{currentRentalPointMetadataKey: "100"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19",
				AccountID:   "account-1",
			}); err != nil {
				t.Fatal(err)
			}
			loadErr := errors.New("forced active rental load failure")
			test.inject(&repositories, loadErr)
			connection := &bufferConn{}
			service := Service{
				options: options{accountID: "account-1"},
				repositoryProvider: func() (dnfrepo.Group, bool) {
					return repositories, true
				},
			}
			session := &gameSession{conn: connection, selectedCharacterID: 19}
			sent, err := service.sendSelectedCurrentRentalPointState(session, "test_load_failure", true)
			if err != nil {
				t.Fatalf("deferred state returned error: %v", err)
			}
			if sent {
				t.Fatal("failed active rental load reported a completed send")
			}
			if connection.write.Len() != 0 {
				t.Fatalf("failed active rental load emitted forged state: %x", connection.write.Bytes())
			}
			if session.selectedRentalWalletStateSent {
				t.Fatal("failed active rental load closed the retry gate")
			}
		})
	}
}

type rentalFailingInventoryLoadRepository struct {
	dnfrepo.InventoryRepository
	err error
}

func (repository rentalFailingInventoryLoadRepository) Load(context.Context, string) (dnfrepo.InventoryRecord, bool, error) {
	return dnfrepo.InventoryRecord{}, false, repository.err
}

type rentalFailingEquipmentLoadRepository struct {
	dnfrepo.EquipmentRepository
	err error
}

func (repository rentalFailingEquipmentLoadRepository) Load(context.Context, string) (dnfrepo.EquipmentRecord, bool, error) {
	return dnfrepo.EquipmentRecord{}, false, repository.err
}

func TestCurrentRentalAssetsCommitPointsGoldAndPVFEquipmentAtomically(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{currentRentalPointMetadataKey: "5"}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Level: 1, Stats: map[string]int64{"gold": 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := currentRentalAssetOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	definition := dungeonDropItemDefinition{ItemID: 416000000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/rental.equ", SlotStart: 9, SlotEnd: 64, Durability: 45}
	rentResult, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, rentalTestAttributes, now)
	if err != nil {
		t.Fatalf("rent equipment: %v", err)
	}
	if rentResult.Points != 2 || rentResult.Slot != 9 || !rentResult.ExpireAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rent result = %+v", rentResult)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	stack, ok := inventory.Slots["0:9"]
	if !ok || stack.ItemID != 416000000 || len(stack.RawEntry) != currentItemListEntryWireSize || stack.Extra["durability"] != "45" || stack.Extra["source"] != currentRentalItemSource {
		t.Fatalf("rental stack = %+v", stack)
	}
	assertCurrentRentalAttributes(t, stack, rentalTestAttributes)
	chargeResult, err := purchaseCurrentRentalPoints(ctx, owner, "account-1", "19", 2, 400, 100000, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("purchase points: %v", err)
	}
	if chargeResult.Points != 4 || chargeResult.Gold != 800000 {
		t.Fatalf("charge result = %+v", chargeResult)
	}
	rerentAt := now.Add(2 * time.Minute)
	rerentResult, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, rentalTestAttributes, rerentAt)
	if err != nil {
		t.Fatalf("refresh existing rental: %v", err)
	}
	if rerentResult.Points != 1 || rerentResult.Slot != 9 || !rerentResult.ExpireAt.Equal(rerentAt.Add(24*time.Hour)) {
		t.Fatalf("rerent result = %+v", rerentResult)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if len(inventory.Slots) != 1 || !inventory.Slots["0:9"].ExpireAt.Equal(rerentResult.ExpireAt) {
		t.Fatalf("rerent inventory = %+v", inventory.Slots)
	}
	assertCurrentRentalAttributes(t, inventory.Slots["0:9"], rentalTestAttributes)
	if _, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 2, definition, rentalTestAttributes, rerentAt.Add(time.Minute)); !errors.Is(err, errCurrentRentalPoints) {
		t.Fatalf("insufficient point error = %v", err)
	}
	if _, err := purchaseCurrentRentalPoints(ctx, owner, "account-1", "19", 9, 400, 100000, rerentAt.Add(2*time.Minute)); !errors.Is(err, errCurrentRentalGold) {
		t.Fatalf("insufficient gold error = %v", err)
	}
	account, _, _ := repositories.Account.Load(ctx, "account-1")
	character, _, _ := repositories.Character.Load(ctx, "19")
	if account.Metadata[currentRentalPointMetadataKey] != "1" || character.Stats["gold"] != 800000 {
		t.Fatalf("persisted account=%+v character=%+v", account, character)
	}
	expired := inventory.Slots["0:9"]
	expired.ExpireAt = rerentAt.Add(-time.Minute)
	expired.Extra["expire_time"] = strconv.FormatInt(expired.ExpireAt.Unix(), 10)
	inventory.Slots["0:9"] = expired
	if err := repositories.Inventory.Save(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	removed, err := cleanupExpiredCurrentRentalEquipment(ctx, owner, "account-1", "19", rerentAt)
	if err != nil || removed != 1 {
		t.Fatalf("expired cleanup removed=%d err=%v", removed, err)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if len(inventory.Slots) != 0 {
		t.Fatalf("expired rental survived cleanup: %+v", inventory.Slots)
	}
}

func TestCurrentRentalDoesNotConvertPermanentSameItem(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{currentRentalPointMetadataKey: "5"}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Level: 1, Stats: map[string]int64{"gold": 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	permanentStack := dnfrepo.ItemStack{ItemID: 416000000, Count: 1, Bind: true, Extra: map[string]string{"source": "quest_reward", "marker": "keep"}}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{"0:9": permanentStack}}); err != nil {
		t.Fatal(err)
	}
	permanentEquipment := dnfrepo.EquipmentEntry{ItemID: 416000000, SlotIndex: 0, Extra: map[string]string{"source": "starter", "marker": "keep"}}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{"weapon": permanentEquipment}}); err != nil {
		t.Fatal(err)
	}
	owner, err := currentRentalAssetOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	definition := dungeonDropItemDefinition{ItemID: 416000000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/rental.equ", SlotStart: 9, SlotEnd: 64, Durability: 45}
	result, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, rentalTestAttributes, now)
	if err != nil {
		t.Fatalf("rent alongside permanent item: %v", err)
	}
	if result.Slot != 10 || result.Points != 2 || result.Equipped {
		t.Fatalf("rent result = %+v", result)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if got := inventory.Slots["0:9"]; got.ItemID != permanentStack.ItemID || got.Bind != permanentStack.Bind || got.Extra["source"] != "quest_reward" || got.Extra["marker"] != "keep" || !got.ExpireAt.IsZero() {
		t.Fatalf("permanent inventory item mutated: %+v", got)
	}
	if got := inventory.Slots["0:10"]; got.ItemID != 416000000 || got.Extra["source"] != currentRentalItemSource || !got.ExpireAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("rental copy = %+v", got)
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "19")
	if got := equipment.Entries["weapon"]; got.ItemID != permanentEquipment.ItemID || got.Extra["source"] != "starter" || got.Extra["marker"] != "keep" || !got.ExpireAt.IsZero() {
		t.Fatalf("permanent equipped item mutated: %+v", got)
	}
}

// assertCurrentRentalAttributes pins the rental row contract: the durable Extra
// keys every reader consults, the two bytes the client parses for 强化/锻造
// (packed reinforcement at +0x0A, forging at +0x57), and the 品级 seed word at
// +0x06. Asserting through currentItemListEntryFromStack keeps the check on the
// real projector rather than on the stored bytes alone.
func assertCurrentRentalAttributes(t *testing.T, stack dnfrepo.ItemStack, want currentRentalItemAttributes) {
	t.Helper()
	reinforce := strconv.FormatInt(want.Reinforce, 10)
	forge := strconv.FormatInt(want.Forge, 10)
	if stack.Extra["ext_data0"] != reinforce || stack.Extra["reinforce"] != reinforce || stack.Extra["forging_level"] != forge {
		t.Fatalf("rental attribute extras=%+v want reinforce=%s forge=%s", stack.Extra, reinforce, forge)
	}
	if len(stack.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("rental row length=%d want=%d", len(stack.RawEntry), currentItemListEntryWireSize)
	}
	if got := stack.RawEntry[0x0A]; got != byte(want.Reinforce) {
		t.Fatalf("rental stored +0x0A=%d want reinforce=%d", got, want.Reinforce)
	}
	if got := stack.RawEntry[currentItemListForgingLevelOffset]; got != byte(want.Forge) {
		t.Fatalf("rental stored +0x57=%d want forge=%d", got, want.Forge)
	}
	assertCurrentRentalQualitySeed(t, stack)
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
	if entry.data[0x0A] != byte(want.Reinforce) || entry.data[currentItemListForgingLevelOffset] != byte(want.Forge) {
		t.Fatalf("projected row +0x0A=%d +0x57=%d want %d/%d",
			entry.data[0x0A], entry.data[currentItemListForgingLevelOffset], want.Reinforce, want.Forge)
	}
}

// assertCurrentRentalQualitySeed pins the 品级 contract against the failure that
// was reported from the field: with no seed the list-row projector resolved
// raw+0x06 through itemquality.StackSeed (0) and fell back to the stack count of
// 1, so the client repainted the weapon as the grade of seed 1 (最下级) while
// the rental shop preview showed 最上级.
func assertCurrentRentalQualitySeed(t *testing.T, stack dnfrepo.ItemStack) {
	t.Helper()
	want := strconv.FormatUint(uint64(currentRentalQualitySeed), 10)
	if stack.Extra["quality_seed"] != want {
		t.Fatalf("rental quality_seed=%q want=%s (extras=%+v)", stack.Extra["quality_seed"], want, stack.Extra)
	}
	if got := currentEquipmentQualitySeedFromStack(stack); got != currentRentalQualitySeed {
		t.Fatalf("StackSeed resolved %d want %d", got, currentRentalQualitySeed)
	}
	if got := binary.LittleEndian.Uint32(stack.RawEntry[0x06:0x0A]); got != currentRentalQualitySeed {
		t.Fatalf("rental stored +0x06 seed=%d want=%d (a fallback to the stack count means the client shows 最下级)",
			got, currentRentalQualitySeed)
	}
	if got := currentItemListStackAmount(dnfrepo.MainInventoryListType, stack); got != currentRentalQualitySeed {
		t.Fatalf("projected +0x06 seed=%d want=%d", got, currentRentalQualitySeed)
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 0, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != currentRentalQualitySeed {
		t.Fatalf("projected row +0x06=%d want=%d", got, currentRentalQualitySeed)
	}
}

func newRentalGrantFixture(t *testing.T, ctx context.Context) (dnfrepo.Group, *dnfrental.Owner, dungeonDropItemDefinition) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{currentRentalPointMetadataKey: "20"}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1", Level: 1, Stats: map[string]int64{"gold": 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
		t.Fatal(err)
	}
	owner, err := currentRentalAssetOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	return repositories, owner, dungeonDropItemDefinition{ItemID: 416000000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/rental.equ", SlotStart: 9, SlotEnd: 64, Durability: 45}
}

// The granted row must carry the tier's 强化/锻造 from the first grant, and a
// re-rent of the same item must re-project them rather than keep the old row.
// The rental weapon's PVF lists `upgrade`/`separate upgrade` under
// [impossible contents], so a missing value can never be repaired in game.
func TestCurrentRentalGrantedRowCarriesTierReinforceAndForge(t *testing.T) {
	ctx := context.Background()
	repositories, owner, definition := newRentalGrantFixture(t, ctx)
	now := time.Unix(1_700_000_000, 0).UTC()

	first := currentRentalItemAttributes{Reinforce: 7, Forge: 3}
	if _, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, first, now); err != nil {
		t.Fatalf("first rent: %v", err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	assertCurrentRentalAttributes(t, inventory.Slots["0:9"], first)

	// A later catalog change must win over what the stored row already holds.
	second := currentRentalItemAttributes{Reinforce: 5, Forge: 1}
	if _, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, second, now.Add(time.Minute)); err != nil {
		t.Fatalf("re-rent: %v", err)
	}
	inventory, _, _ = repositories.Inventory.Load(ctx, "19")
	if len(inventory.Slots) != 1 {
		t.Fatalf("re-rent created a second row: %+v", inventory.Slots)
	}
	assertCurrentRentalAttributes(t, inventory.Slots["0:9"], second)
}

// A tier that declares zero must clear a previously granted level instead of
// leaving the stale value behind: the extras are overwritten, not merged.
func TestCurrentRentalGrantedRowClearsAttributesWhenTierDeclaresZero(t *testing.T) {
	ctx := context.Background()
	repositories, owner, definition := newRentalGrantFixture(t, ctx)
	now := time.Unix(1_700_000_000, 0).UTC()

	if _, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, currentRentalItemAttributes{Reinforce: 7, Forge: 3}, now); err != nil {
		t.Fatalf("first rent: %v", err)
	}
	zero := currentRentalItemAttributes{}
	if _, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, zero, now.Add(time.Minute)); err != nil {
		t.Fatalf("re-rent with zero attributes: %v", err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	assertCurrentRentalAttributes(t, inventory.Slots["0:9"], zero)
}

// Re-renting a rental that is currently worn goes down the equipment branch,
// which only refreshes Extra. That branch has to refresh the attributes too,
// because the worn projector reads them from Extra first.
func TestCurrentRentalEquippedRenewalRefreshesTierAttributes(t *testing.T) {
	ctx := context.Background()
	repositories, owner, definition := newRentalGrantFixture(t, ctx)
	now := time.Unix(1_700_000_000, 0).UTC()

	worn := dnfrepo.EquipmentEntry{
		ItemID:    416000000,
		SlotIndex: 11,
		Extra:     map[string]string{"source": currentRentalItemSource, "durability": "45"},
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: "19", Entries: map[string]dnfrepo.EquipmentEntry{"11": worn}}); err != nil {
		t.Fatal(err)
	}
	result, err := rentCurrentEquipment(ctx, owner, "account-1", "19", 416000000, 3, definition, rentalTestAttributes, now)
	if err != nil {
		t.Fatalf("renew worn rental: %v", err)
	}
	if !result.Equipped {
		t.Fatalf("renewal did not take the equipped branch: %+v", result)
	}
	equipment, _, _ := repositories.Equipment.Load(ctx, "19")
	renewed, ok := equipment.Entries["11"]
	if !ok {
		t.Fatal("worn rental entry disappeared")
	}
	reinforce := strconv.FormatInt(rentalTestAttributes.Reinforce, 10)
	forge := strconv.FormatInt(rentalTestAttributes.Forge, 10)
	if renewed.Extra["ext_data0"] != reinforce || renewed.Extra["forging_level"] != forge {
		t.Fatalf("worn rental extras=%+v want reinforce=%s forge=%s", renewed.Extra, reinforce, forge)
	}
	if got := currentItemListEquipmentExtData(renewed); got != byte(rentalTestAttributes.Reinforce) {
		t.Fatalf("worn projector reinforce=%d want=%d", got, rentalTestAttributes.Reinforce)
	}
	if got := currentEquipmentForgingLevel(renewed); got != byte(rentalTestAttributes.Forge) {
		t.Fatalf("worn projector forging=%d want=%d", got, rentalTestAttributes.Forge)
	}
	if got := currentItemListEquipmentInstance(renewed); got != currentRentalQualitySeed {
		t.Fatalf("worn projector 品级 word=%d want=%d", got, currentRentalQualitySeed)
	}
}

// The bag row the op14 update sends is built by the shared list-0 projector.
// Pin the two offsets on a fully assembled 119-byte row so a future change to
// either key name or offset fails here instead of in game.
func TestCurrentRentalSentRowCarriesReinforceAndForgeBytes(t *testing.T) {
	stack := currentRentalStack(
		dnfrepo.ItemStack{},
		416000000,
		dungeonDropItemDefinition{ItemID: 416000000, Kind: dungeonDropItemEquipment, PVFPath: "equipment/rental.equ", Durability: 45},
		rentalTestAttributes,
		time.Unix(1_790_224_057, 0).UTC(),
	)
	if stack.Count != 1 || stack.Bind {
		t.Fatalf("rental stack shape=%+v", stack)
	}
	if !stack.ExpireAt.Equal(time.Unix(1_790_224_057, 0).UTC()) {
		t.Fatalf("rental expire=%v", stack.ExpireAt)
	}
	assertCurrentRentalAttributes(t, stack, rentalTestAttributes)
	if got := binary.LittleEndian.Uint16(stack.RawEntry[0x0B:0x0D]); got != 45 {
		t.Fatalf("rental durability=%d want=45", got)
	}
	if got := binary.LittleEndian.Uint32(stack.RawEntry[0x02:0x06]); got != 416000000 {
		t.Fatalf("rental item id=%d want=416000000", got)
	}
}

// The reported bug: buying lucky stars (op1000, which charges gold per point)
// left the on-screen gold at the pre-purchase value until the next relog. The
// list-0 reserved wallet row is the client's only gold display, so the purchase
// has to republish it. Pin the exact bytes: list 0, one entry, slot 0, item id 0,
// and the post-charge balance at +0x06.
func TestCurrentRentalGoldRefreshBodyIsOneReservedWalletRow(t *testing.T) {
	const goldAfter int64 = 32_986_408

	body := buildCurrentItemUpdateBody(
		dnfrepo.MainInventoryListType,
		[]currentItemListEntry{currentGoldWalletItemListEntry(goldAfter)},
	)

	if want := 3 + currentItemListEntryWireSize; len(body) != want {
		t.Fatalf("body length = %d, want %d", len(body), want)
	}
	if body[0] != dnfrepo.MainInventoryListType {
		t.Fatalf("list type = %d, want %d", body[0], dnfrepo.MainInventoryListType)
	}
	if count := binary.LittleEndian.Uint16(body[1:3]); count != 1 {
		t.Fatalf("entry count = %d, want 1", count)
	}
	row := body[3:]
	if slot := int16(binary.LittleEndian.Uint16(row[0:2])); slot != currentGoldWalletReservedSlot {
		t.Fatalf("slot = %d, want %d", slot, currentGoldWalletReservedSlot)
	}
	if itemID := binary.LittleEndian.Uint32(row[2:6]); itemID != 0 {
		t.Fatalf("item id = %d, want 0 for the reserved wallet row", itemID)
	}
	if amount := binary.LittleEndian.Uint32(row[6:10]); int64(amount) != goldAfter {
		t.Fatalf("wallet amount = %d, want %d", amount, goldAfter)
	}
}

// sendSelectedCurrentRentalGoldRefresh must actually put that row on the wire as
// a class-0 op14, and must stay silent for a session with no selected character
// (it runs on the post-purchase path, where a missing selection is not fatal).
func TestCurrentRentalGoldRefreshSendsClassZeroOp14(t *testing.T) {
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "rental-gold-refresh", selectedCharacterID: 19}
	if err := (&Service{}).sendSelectedCurrentRentalGoldRefresh(session, "test", 1234); err != nil {
		t.Fatalf("gold refresh: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes = %x", rest)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) || packet.Header.Classification != 0 {
		t.Fatalf("header = %+v want class-0 op%d", packet.Header, dnfenum.CmdPacketWalkoutPartyMember)
	}
	if len(packet.Body) != 3+currentItemListEntryWireSize {
		t.Fatalf("body length = %d, want %d", len(packet.Body), 3+currentItemListEntryWireSize)
	}
	if amount := binary.LittleEndian.Uint32(packet.Body[3+6 : 3+10]); amount != 1234 {
		t.Fatalf("wallet amount = %d, want 1234", amount)
	}

	// No selected character: nothing to republish, and no error.
	empty := &bufferConn{}
	if err := (&Service{}).sendSelectedCurrentRentalGoldRefresh(&gameSession{conn: empty}, "test", 7); err != nil {
		t.Fatalf("unselected session: %v", err)
	}
	if got := empty.write.Bytes(); len(got) != 0 {
		t.Fatalf("unselected session sent %d bytes, want 0: %x", len(got), got)
	}
}

func TestRealScriptPVFCurrentRentalCatalog(t *testing.T) {
	pvfPath := os.Getenv("DNF_RENTAL_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_RENTAL_REAL_PVF_SMOKE to run the real Script.pvf rental smoke test")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatalf("open real Script.pvf: %v", err)
	}
	catalog, err := parseCurrentRentalCatalog(archive)
	if err != nil {
		t.Fatalf("parse real rental catalog: %v", err)
	}
	if catalog.Limit != 400 || catalog.DungeonClearPoints != 1 || catalog.GoldPerPoint != 100000 {
		t.Fatalf("real wallet config = %+v", catalog)
	}
	if cost, ok := catalog.itemCost("[gun blader]", 0, 416000000); !ok || cost != 3 {
		t.Fatalf("real gun blader tier0 cost=%d found=%t", cost, ok)
	}
	monsters, err := newPVFDungeonMonsterCatalog(archive)
	if err != nil {
		t.Fatalf("load real monster/item catalog: %v", err)
	}
	items, err := monsters.DropCatalog()
	if err != nil {
		t.Fatalf("load real item catalog: %v", err)
	}
	definition, err := validateCurrentRentalItem(archive, items, 416000000, "[gun blader]")
	if err != nil {
		t.Fatalf("validate real gun blader rental: %v", err)
	}
	if definition.Durability != 45 {
		t.Fatalf("real gun blader durability=%d want=45", definition.Durability)
	}
	// Swordman tier 3 uses chn_401040028 with [grade] 44, the exact item
	// rejected in the field after catalog validation had already succeeded.
	definition, err = validateCurrentRentalItem(archive, items, 401040028, "[swordman]")
	if err != nil {
		t.Fatalf("validate real swordman tier3 rental: %v", err)
	}
	if definition.Durability != 48 {
		t.Fatalf("real swordman tier3 durability=%d want=48", definition.Durability)
	}
	// The live 90CN [section] declares `7 3` for every tier, which is what the
	// client's rental shop preview shows. Pin it against the real PVF so a
	// catalog whose attributes silently stop parsing fails here.
	row, found := catalog.tierRow(0)
	if !found {
		t.Fatal("real catalog has no tier 0 row")
	}
	if row.Reinforce != 7 || row.Forge != 3 {
		t.Fatalf("real tier0 attributes reinforce=%d forge=%d want 7/3", row.Reinforce, row.Forge)
	}
}
