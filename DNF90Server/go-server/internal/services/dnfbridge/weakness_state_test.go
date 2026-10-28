package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// The live [stamina recovery cost] row, trimmed: nineteen free levels then the
// real curve. Index is level-1, so level 20 is the first charged level.
const currentWeaknessCostFixture = `
[stamina recovery cost]
0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 5000 5210 5420 5630 5840 6050
[/stamina recovery cost]
`

func prepareCurrentWeaknessTest(
	t *testing.T,
	characterLevel int,
	weakness int64,
	gold int64,
	premiums map[int64]int64,
) (*Service, *gameSession, dnfrepo.Group, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	account := dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{}}
	now := time.Unix(1_700_000_000, 0).UTC()
	for premiumType, seconds := range premiums {
		premium.Upsert(&account, premiumType, seconds, 1, now)
	}
	if err := repositories.Account.Save(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	stats := map[string]int64{}
	if weakness != 0 {
		stats[currentWeaknessStatKey] = weakness
	}
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "5",
		AccountID:   "account-1",
		Level:       characterLevel,
		Stats:       stats,
	}); err != nil {
		t.Fatal(err)
	}
	slots := map[string]dnfrepo.ItemStack{}
	if gold > 0 {
		// The legacy paid-recovery wallet: main-list slot 0. This server keeps
		// gold in the character's own `gold` stat, which the client's wallet is
		// projected from, so this fixture deliberately exercises the fallback
		// store that weakness_state.go still honours. A test that wants the
		// authoritative column calls seedCurrentWeaknessCharacterGold.
		slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 0)] =
			dnfrepo.ItemStack{ItemID: 0, Count: gold}
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "5", Slots: slots,
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = now
	service := &Service{
		options:            options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     queue,
	}
	document, err := dnfpvf.Parse(currentWeaknessCostArchivePath, currentWeaknessCostFixture)
	if err != nil {
		t.Fatal(err)
	}
	prices := document.Ints(currentWeaknessCostSection)
	if len(prices) == 0 {
		t.Fatal("cost fixture produced no prices")
	}
	service.weaknessCostTable = &currentWeaknessCostTable{basePrices: prices}
	session := &gameSession{conn: connection, connID: "weakness", selectedCharacterID: 5}
	return service, session, repositories, connection
}

func currentWeaknessStoredLevel(t *testing.T, repositories dnfrepo.Group) int64 {
	t.Helper()
	character, found, err := repositories.Character.Load(context.Background(), "5")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	return currentWeaknessLevel(character)
}

// seedCurrentWeaknessCharacterGold writes gold into the character's own column,
// which is the store this server actually spends from.
func seedCurrentWeaknessCharacterGold(t *testing.T, repositories dnfrepo.Group, gold int64) {
	t.Helper()
	character, found, err := repositories.Character.Load(context.Background(), "5")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = map[string]int64{}
	}
	character.Stats["gold"] = gold
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
}

// A forced exit weakens a character that has neither exemption, and it lands at
// full weight on the very first give-up rather than creeping down one level at
// a time. Repeating it changes nothing; only time does.
func TestForcedReturnAppliesTheWholePenaltyOnTheFirstGiveUp(t *testing.T) {
	service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, 0, 0, nil)
	for round := 1; round <= 3; round++ {
		service.applyCurrentForcedReturnWeakness(session, "test_forced_exit")
		if got := currentWeaknessStoredLevel(t, repositories); got != currentWeaknessForcedReturnLevel {
			t.Fatalf("round %d weakness=%d want %d", round, got, currentWeaknessForcedReturnLevel)
		}
	}
	// The operator specifies this penalty by what the player sees: 19% health.
	if got := currentWeaknessCondition(currentWeaknessForcedReturnLevel); got != 19 {
		t.Fatalf("forced-exit condition=%d want 19", got)
	}
	// The clamp still floors at 10 for any heavier level that reaches the wire.
	if got := currentWeaknessCondition(currentWeaknessMaximumLevel); got != 10 {
		t.Fatalf("condition at max weakness=%d want 10", got)
	}
}

// A forced exit must never heal: a character already carrying a heavier
// penalty keeps it.
func TestForcedReturnDoesNotReduceAHeavierPenalty(t *testing.T) {
	service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, currentWeaknessMaximumLevel, 0, nil)
	service.applyCurrentForcedReturnWeakness(session, "test_forced_exit")
	if got := currentWeaknessStoredLevel(t, repositories); got != currentWeaknessMaximumLevel {
		t.Fatalf("weakness=%d want %d (a forced exit must not heal)", got, currentWeaknessMaximumLevel)
	}
}

// 黑钻 is the only thing that skips the penalty, and a forced exit may never
// spend anything. The devil contract used to exempt here and silently burn a
// daily use; operator decision 2026-09-12 made the contract purely a player
// choice in the 解除虚弱 UI, so an active contract must now weaken like any
// other character and leave its quota untouched.
func TestForcedReturnIsExemptedOnlyByBlackDiamondAndSpendsNothing(t *testing.T) {
	devilType := premium.DevilSlotType(premium.DevilSlotFreeWeakness)
	for name, test := range map[string]struct {
		premiums      map[int64]int64
		wantLevel     int64
		wantExemption currentWeaknessExemption
		wantQuotaUsed bool
	}{
		"no premium at all": {
			premiums: nil, wantLevel: currentWeaknessForcedReturnLevel, wantExemption: currentWeaknessExemptionNone,
		},
		"black diamond": {
			premiums:      map[int64]int64{premium.TypeBlackDiamondEntitlement: 86400},
			wantLevel:     0,
			wantExemption: currentWeaknessExemptionBlackDiamond,
		},
		// An active devil contract no longer exempts and must not be charged.
		"devil contract": {
			premiums:      map[int64]int64{devilType: 86400},
			wantLevel:     currentWeaknessForcedReturnLevel,
			wantExemption: currentWeaknessExemptionNone,
		},
		// An unrelated active contract must not exempt anything.
		"over-equip contract": {
			premiums:  map[int64]int64{premium.TypeOverEquip: 86400},
			wantLevel: currentWeaknessForcedReturnLevel, wantExemption: currentWeaknessExemptionNone,
		},
		// An expired devil contract is not an exemption either.
		"expired devil contract": {
			premiums:  map[int64]int64{devilType: -1},
			wantLevel: currentWeaknessForcedReturnLevel, wantExemption: currentWeaknessExemptionNone,
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, 0, 0, test.premiums)
			level, exemption, err := service.commitCurrentForcedReturnWeakness(session)
			if err != nil {
				t.Fatal(err)
			}
			if exemption != test.wantExemption {
				t.Fatalf("exemption=%q want %q", exemption, test.wantExemption)
			}
			if got := currentWeaknessStoredLevel(t, repositories); got != test.wantLevel {
				t.Fatalf("stored weakness=%d want %d (returned %d)", got, test.wantLevel, level)
			}
			character, _, err := repositories.Character.Load(context.Background(), "5")
			if err != nil {
				t.Fatal(err)
			}
			used := premium.DailyUsage(character, premium.DevilSlotFreeWeakness, service.gameplayNow())
			if test.wantQuotaUsed && used == 0 {
				t.Fatal("the devil contract exemption must spend its daily quota")
			}
			if !test.wantQuotaUsed && used != 0 {
				t.Fatalf("no quota may be spent here, used=%d", used)
			}
		})
	}
}

// Repeated forced exits on an active contract must never touch its daily
// quota. The quota is the player's to spend in the 解除虚弱 UI; burning it on
// a give-up they did not choose to pay for is the behaviour this replaces.
func TestRepeatedForcedExitsNeverSpendTheDevilContractQuota(t *testing.T) {
	devilType := premium.DevilSlotType(premium.DevilSlotFreeWeakness)
	service, session, repositories, _ := prepareCurrentWeaknessTest(
		t, 40, 0, 0, map[int64]int64{devilType: 86400},
	)
	for round := 0; round < 3; round++ {
		_, exemption, err := service.commitCurrentForcedReturnWeakness(session)
		if err != nil {
			t.Fatal(err)
		}
		if exemption != currentWeaknessExemptionNone {
			t.Fatalf("round %d exemption=%q want none", round, exemption)
		}
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != currentWeaknessForcedReturnLevel {
		t.Fatalf("weakness=%d want %d", got, currentWeaknessForcedReturnLevel)
	}
	character, _, err := repositories.Character.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if used := premium.DailyUsage(character, premium.DevilSlotFreeWeakness, service.gameplayNow()); used != 0 {
		t.Fatalf("a forced exit spent %d contract uses; the quota is the player's own", used)
	}
}

// A cleared run never weakens; only the abandoned commit does.
func TestOnlyAnAbandonedRunAppliesWeakness(t *testing.T) {
	if currentForcedReturnAppliesWeakness(worldmap.DungeonRunCompleted) {
		t.Fatal("a cleared run must not apply weakness")
	}
	if !currentForcedReturnAppliesWeakness(worldmap.DungeonRunAbandoned) {
		t.Fatal("an abandoned run must apply weakness")
	}
}

// op9 with the contract flag must be judged by the server: an inactive contract
// is refused with the EXE's own reason code and nothing is cleared or spent.
func TestWeaknessRecoveryRefusesAContractTheServerCannotHonour(t *testing.T) {
	service, session, repositories, connection := prepareCurrentWeaknessTest(t, 40, 3, 0, nil)
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1)
	if err := service.handleCurrentPremiumFreeWeakness(session, body); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(packet.Body) != 2 || packet.Body[0] != 0 ||
		packet.Body[1] != currentWeaknessReasonCannotAfford || len(trailing) != 0 {
		t.Fatalf("refusal packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != 3 {
		t.Fatalf("a refused recovery must not clear weakness, got %d", got)
	}
}

// The contract route clears without charging gold and spends exactly one quota.
func TestWeaknessRecoveryByContractClearsWithoutGold(t *testing.T) {
	devilType := premium.DevilSlotType(premium.DevilSlotFreeWeakness)
	service, session, repositories, connection := prepareCurrentWeaknessTest(
		t, 40, 4, 100_000, map[int64]int64{devilType: 86400},
	)
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1)
	if err := service.handleCurrentPremiumFreeWeakness(session, body); err != nil {
		t.Fatal(err)
	}
	// The premium refresh follows the ACK; TestContractQuotaSpendRepublishesThePremiumState
	// pins its contents.
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(packet.Body) != 1 || packet.Body[0] != 1 || len(trailing) == 0 {
		t.Fatalf("success packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != 0 {
		t.Fatalf("weakness after contract recovery=%d want 0", got)
	}
	inventory, _, err := repositories.Inventory.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	wallet := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 0)]
	if wallet.Count != 100_000 {
		t.Fatalf("the contract route must not charge gold, wallet=%d", wallet.Count)
	}
	character, _, err := repositories.Character.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if premium.DailyUsage(character, premium.DevilSlotFreeWeakness, service.gameplayNow()) != 1 {
		t.Fatalf("the contract route must spend exactly one quota: %v", character.Stats)
	}
}

// The paid route prices the clear from the PVF table and refuses when the gold
// is short, without clearing anything.
func TestWeaknessRecoveryByGoldUsesThePVFPriceAndRefusesWhenShort(t *testing.T) {
	// Level 20 base price 5000, weakness 10 -> full 90 points lost -> 5000.
	service, session, repositories, connection := prepareCurrentWeaknessTest(t, 20, 10, 4_999, nil)
	if err := service.handleCurrentPremiumFreeWeakness(session, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(packet.Body) != 2 || packet.Body[0] != 0 || packet.Body[1] != currentWeaknessReasonCannotAfford {
		t.Fatalf("short-gold packet body=%x", packet.Body)
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != 10 {
		t.Fatalf("a refused paid recovery must not clear weakness, got %d", got)
	}

	service, session, repositories, connection = prepareCurrentWeaknessTest(t, 20, 10, 5_000, nil)
	if err := service.handleCurrentPremiumFreeWeakness(session, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	success, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(success.Body) != 1 || success.Body[0] != 1 {
		t.Fatalf("paid success body=%x", success.Body)
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != 0 {
		t.Fatalf("weakness after paid recovery=%d want 0", got)
	}
	inventory, _, err := repositories.Inventory.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	wallet := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 0)]
	if wallet.Count != 0 {
		t.Fatalf("paid recovery wallet=%d want 0", wallet.Count)
	}
}

// The paid route charges the character's gold column, which is what the client
// wallet is projected from and what every other gold spender in this tree
// spends.
func TestWeaknessRecoveryByGoldChargesTheCharacterGoldColumn(t *testing.T) {
	service, session, repositories, connection := prepareCurrentWeaknessTest(t, 20, 10, 0, nil)
	seedCurrentWeaknessCharacterGold(t, repositories, 7_500)
	if err := service.handleCurrentPremiumFreeWeakness(session, []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	character, _, err := repositories.Character.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if character.Stats["gold"] != 2_500 {
		t.Fatalf("gold after paid recovery=%d want 2500 (7500 - 5000)", character.Stats["gold"])
	}
	if got := currentWeaknessLevel(character); got != 0 {
		t.Fatalf("weakness after paid recovery=%d want 0", got)
	}
	// A cleared penalty drops its countdown so the next one starts fresh.
	if _, found := character.Stats[currentWeaknessRecoveryAtStatKey]; found {
		t.Fatalf("cleared weakness kept a recovery timestamp: %+v", character.Stats)
	}
	// And the client has to be told: the wallet is projected from this column,
	// so the balance keeps showing the pre-payment value unless a refresh
	// follows the success ACK.
	_, afterAck := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(afterAck) == 0 {
		t.Fatal("paid recovery sent the ACK but no wallet refresh after it")
	}
}

// The class-0 notification is the only way to publish a partial health cap; the
// op9 ACK can only ever say 100. Body is one byte, the 0..100 condition.
func TestWeaknessConditionRefreshPublishesTheSettledCondition(t *testing.T) {
	service, session, repositories, connection := prepareCurrentWeaknessTest(t, 20, 4, 0, nil)
	if err := service.sendCurrentWeaknessConditionRefresh(session, "test"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != currentWeaknessConditionMsg || packet.Header.Classification != 0 {
		t.Fatalf("condition packet msg=%d class=%d want %d/0",
			packet.Header.MsgID, packet.Header.Classification, currentWeaknessConditionMsg)
	}
	// Level 4 -> 100 - 4*9 = 64. Not 100: a partial cap must be sendable.
	if len(packet.Body) != 1 || packet.Body[0] != 64 || len(rest) != 0 {
		t.Fatalf("condition body=%x trailing=%d want 0x40 (64)", packet.Body, len(rest))
	}
	// Republishing must never itself reduce the penalty. This is the invariant
	// behind "re-selecting the character cured the weakness": selecting rebuilds
	// the local player at a healthy 100, so the route republishes the cap, and a
	// republish that healed would make the penalty evaporate on every relog.
	character, _, err := repositories.Character.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got := currentWeaknessLevel(character); got != 4 {
		t.Fatalf("stored weakness after a refresh=%d want 4 (a republish must not heal)", got)
	}
}

// Recovering while healthy is refused with the EXE's "not weakened" code rather
// than silently charging.
func TestWeaknessRecoveryRefusesAHealthyCharacter(t *testing.T) {
	for name, body := range map[string][]byte{
		"contract route": {1, 0},
		"gold route":     {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			service, session, _, connection := prepareCurrentWeaknessTest(t, 40, 0, 100_000, nil)
			if err := service.handleCurrentPremiumFreeWeakness(session, body); err != nil {
				t.Fatal(err)
			}
			packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
			if len(packet.Body) != 2 || packet.Body[0] != 0 ||
				packet.Body[1] != currentWeaknessReasonNotWeakened || len(trailing) != 0 {
				t.Fatalf("healthy refusal body=%x trailing=%x", packet.Body, trailing)
			}
		})
	}
}

// The cost curve must come from the table, not from a hard-coded formula.
func TestWeaknessRecoveryCostScalesWithLevelAndPenalty(t *testing.T) {
	document, err := dnfpvf.Parse(currentWeaknessCostArchivePath, currentWeaknessCostFixture)
	if err != nil {
		t.Fatal(err)
	}
	table := &currentWeaknessCostTable{basePrices: document.Ints(currentWeaknessCostSection)}
	if got := table.recoveryCost(10, 20); got != 5000 {
		t.Fatalf("full penalty at level 20=%d want the base price 5000", got)
	}
	if got := table.recoveryCost(5, 20); got != 2500 {
		t.Fatalf("half penalty at level 20=%d want 2500", got)
	}
	if got := table.recoveryCost(0, 20); got != 0 {
		t.Fatalf("no penalty must cost nothing, got %d", got)
	}
	// Below the charged band the table itself is zero.
	if got := table.recoveryCost(10, 5); got != 0 {
		t.Fatalf("level 5 cost=%d want 0", got)
	}
	// A level past the shipped row clamps into the last entry instead of failing.
	if got := table.recoveryCost(10, 999); got != 6050 {
		t.Fatalf("clamped level cost=%d want the last entry 6050", got)
	}
}

func TestCurrentWeaknessCostTableParsesTheLiveArchive(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime weakness cost table")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	table, err := parseCurrentWeaknessCostTable(archive)
	if err != nil {
		t.Fatal(err)
	}
	// The shipped row carries one entry per level, indexed level-1: nineteen
	// free levels then 5000 at level 20.
	if len(table.basePrices) < 20 {
		t.Fatalf("live [stamina recovery cost] entries=%d want at least 20", len(table.basePrices))
	}
	if table.basePrice(19) != 0 || table.basePrice(20) != 5000 {
		t.Fatalf("live curve level19=%d level20=%d", table.basePrice(19), table.basePrice(20))
	}
	// A level past the shipped row clamps rather than failing, and a full
	// penalty always costs exactly the level's base price.
	if got := table.recoveryCost(10, 90); got != table.basePrice(90) {
		t.Fatalf("a full penalty must cost the whole base price: %d vs %d", got, table.basePrice(90))
	}
	last := int64(len(table.basePrices))
	if table.basePrice(999) != table.basePrice(last) {
		t.Fatalf("an out-of-range level must clamp into the table")
	}
}

// Spending a contract quota without republishing the premium state is what the
// operator saw as "the contract was not deducted": the count is right in the
// database and stale on screen. Both consuming routes must refresh it.
func TestContractQuotaSpendRepublishesThePremiumState(t *testing.T) {
	devilType := premium.DevilSlotType(premium.DevilSlotFreeWeakness)
	service, session, repositories, connection := prepareCurrentWeaknessTest(
		t, 40, 4, 0, map[int64]int64{devilType: 86400},
	)
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1)
	if err := service.handleCurrentPremiumFreeWeakness(session, body); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(ack.Body) != 1 || ack.Body[0] != 1 {
		t.Fatalf("recovery ACK=%+v body=%x", ack.Header, ack.Body)
	}
	if len(rest) == 0 {
		t.Fatal("a spent contract quota must be followed by a premium state refresh")
	}
	state, trailing := splitGameServerUpperPacket(t, rest)
	if state.Header.MsgID != uint16(dnfenum.CmdPacketPremiumService) || len(trailing) != 0 {
		t.Fatalf("premium refresh=%+v trailing=%x", state.Header, trailing)
	}
	// The refresh must carry the post-use count, not the pre-use one.
	if got := premiumServicePacketUsage(t, rest, premium.DevilSlotFreeWeakness); got != 1 {
		t.Fatalf("republished free-weakness usage=%d want 1", got)
	}
	character, _, err := repositories.Character.Load(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got := premium.DailyUsage(character, premium.DevilSlotFreeWeakness, service.gameplayNow()); got != 1 {
		t.Fatalf("persisted usage=%d want 1", got)
	}
}

// The select-character projection is the only place a stored weakness level can
// currently reach the client. It preferred a legacy subtype0 row that does not
// exist on this runtime, so it always published 0 and the penalty was invisible
// even after a relog.
func TestSelectedUserInfoProjectsTheStoredWeaknessLevel(t *testing.T) {
	character := dnfrepo.CharacterRecord{
		CharacterID: "5",
		AccountID:   "account-1",
		Level:       90,
		Stats:       map[string]int64{currentWeaknessStatKey: 7, "fatigue_penalty": 3},
	}
	if got := rowStatU8(nil, character, currentWeaknessStatKey, 0); got != 7 {
		t.Fatalf("projected weakness=%d want the stored 7", got)
	}
	if got := rowStatU32(nil, character, "fatigue_penalty", 0); got != 3 {
		t.Fatalf("projected fatigue penalty=%d want 3", got)
	}
	// A healthy character still projects zero rather than inventing a penalty.
	healthy := dnfrepo.CharacterRecord{CharacterID: "6", Stats: map[string]int64{}}
	if got := rowStatU8(nil, healthy, currentWeaknessStatKey, 0); got != 0 {
		t.Fatalf("healthy weakness projection=%d want 0", got)
	}
}
