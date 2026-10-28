package dnfbridge

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentBoosterContractRewardsActivateWithoutBackpackCapacity(t *testing.T) {
	catalog := mustCurrentBoosterSpecialRewardCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	slots := make(map[string]dnfrepo.ItemStack)
	for slot := int16(65); slot <= 120; slot++ {
		slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{ItemID: 700, Count: 1}
	}
	// Keep a second box in the source slot. The entire consumable category is
	// therefore occupied even after this one open is consumed.
	slots["0:95"] = dnfrepo.ItemStack{ItemID: 520, Count: 2}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	service := currentBoosterTestService(catalog, repositories)
	service.premiumCatalog.contractsByItem = map[int64]currentPremiumContractInfo{
		43:       {ItemID: 43, PremiumType: 27, DurationSeconds: 86400},
		30:       {ItemID: 30, PremiumType: 22, DurationSeconds: 86400},
		10000391: {ItemID: 10000391, PremiumType: premium.TypeCrystal, DurationSeconds: 86400},
		2660703:  {ItemID: 2660703, PremiumType: 84, DurationSeconds: 86400},
	}
	service.premiumCatalog.crystalCubeIDs = [6]int64{700, 701, 702, 703, 704, 705}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "booster-contract-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseBoosterItem),
		[]byte{95, 0},
	); err != nil {
		t.Fatal(err)
	}
	ack, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) == 0 || ack.Body[0] != 1 {
		t.Fatalf("op160 failure body=%x", ack.Body)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if source := inventory.Slots["0:95"]; source.ItemID != 520 || source.Count != 1 {
		t.Fatalf("source=%+v", source)
	}
	for _, itemID := range []int64{43, 30, 10000391, 2660703} {
		for _, stack := range inventory.Slots {
			if stack.ItemID == itemID {
				t.Fatalf("contract item %d leaked into backpack: %+v", itemID, stack)
			}
		}
	}
	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	for _, premiumType := range []int64{22, 27, 84, premium.TypeCrystal} {
		if !premium.Active(account, premiumType, time.Now().UTC()) {
			t.Fatalf("premium type %d was not activated: %+v", premiumType, account.Metadata)
		}
	}
}

func TestRealPVFAttendanceContractAndReviveGiftRouting(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify attendance gift routing")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	reviveGift, err := resolveCurrentBoosterDefinition(items, 490006575, currentBoosterRequestRandom)
	if err != nil {
		t.Fatal(err)
	}
	if reviveGift.MaterialItemID != 0 || reviveGift.MaterialCount != 0 ||
		reviveGift.Random.MaterialItemID != 0 || reviveGift.Random.MaterialCountPerUse != 0 {
		t.Fatalf("gift acquisition metadata leaked into opening cost: %+v", reviveGift)
	}
	reviveAmounts, _, _, err := resolveCurrentBoosterRewardAmounts(
		reviveGift,
		currentBoosterOpenRequest{Kind: currentBoosterRequestRandom},
		func(int64) (int64, error) { return 0, nil },
	)
	if err != nil || reviveAmounts[uint32(dnfrevivecoin.WalletItemID)] != 2 {
		t.Fatalf("revive reward amounts=%v err=%v", reviveAmounts, err)
	}

	contractGift, err := resolveCurrentBoosterDefinition(items, 490006574, currentBoosterRequestRandom)
	if err != nil {
		t.Fatal(err)
	}
	contractAmounts, _, _, err := resolveCurrentBoosterRewardAmounts(
		contractGift,
		currentBoosterOpenRequest{Kind: currentBoosterRequestRandom},
		func(int64) (int64, error) { return 0, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	premiumCatalog, err := buildCurrentPremiumCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{43, 30, 10000391, 2660703} {
		if contractAmounts[uint32(itemID)] != 1 {
			t.Fatalf("contract gift item=%d amount=%d", itemID, contractAmounts[uint32(itemID)])
		}
		contract, exists := premiumCatalog.contractsByItem[itemID]
		if !exists || contract.PremiumType <= 0 || contract.DurationSeconds != 86400 {
			t.Fatalf("premium contract item=%d mapping=%+v exists=%t", itemID, contract, exists)
		}
	}
	if crystal := premiumCatalog.contractsByItem[10000391]; crystal.PremiumType != premium.TypeCrystal {
		t.Fatalf("crystal contract mapping=%+v", crystal)
	}
}

func TestCurrentBoosterReviveCoinGiftIgnoresAcquisitionMaterialAndRefreshesWallet(t *testing.T) {
	catalog := mustCurrentBoosterSpecialRewardCatalog(t)
	definition, err := resolveCurrentBoosterDefinition(catalog, 521, currentBoosterRequestRandom)
	if err != nil {
		t.Fatal(err)
	}
	if definition.MaterialItemID != 0 || definition.MaterialCount != 0 ||
		definition.Random.MaterialItemID != 0 || definition.Random.MaterialCountPerUse != 0 {
		t.Fatalf("gift opening cost=%+v, want unconditional open", definition)
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:104": {ItemID: 521, Count: 1},
			"0:1":   {ItemID: dnfrevivecoin.WalletItemID, Count: 5},
			"0:121": {ItemID: 999, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := currentBoosterTestService(catalog, repositories)
	session := &gameSession{conn: connection, connID: "booster-revive-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketUseBoosterItem),
		[]byte{104, 0},
	); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(ack.Body) == 0 || ack.Body[0] != 1 {
		t.Fatalf("op160 failure body=%x", ack.Body)
	}
	rest = assertCurrentBoosterSourceRefresh(t, rest, 104, ^uint32(0), 0)
	walletRefresh, _ := splitGameServerUpperPacket(t, rest)
	if walletRefresh.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		len(walletRefresh.Body) < 3+currentItemListEntryWireSize ||
		walletRefresh.Body[0] != dnfrepo.MainInventoryListType ||
		!bytes.Equal(walletRefresh.Body[3:5], []byte{1, 0}) {
		t.Fatalf("wallet refresh header=%+v body=%x", walletRefresh.Header, walletRefresh.Body)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:104"]; exists {
		t.Fatalf("source box was not consumed: %+v", inventory.Slots["0:104"])
	}
	if material := inventory.Slots["0:121"]; material.ItemID != 999 || material.Count != 2 {
		t.Fatalf("gift acquisition material changed during open: %+v", material)
	}
	if wallet := inventory.Slots[dnfrevivecoin.WalletKey()]; wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 7 {
		t.Fatalf("wallet=%+v", wallet)
	}
}

func mustCurrentBoosterSpecialRewardCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                   "",
		"equipment/equipment.lst":               "",
		"stackable/stackable.lst":               "1 `coin.stk`\n30 `contract/30.stk`\n43 `contract/43.stk`\n520 `box/contracts.stk`\n521 `box/revive.stk`\n700 `material/filler.stk`\n999 `material/challenge_coin.stk`\n2660703 `contract/2660703.stk`\n10000391 `contract/10000391.stk`\n",
		"stackable/coin.stk":                    "[stackable type]\n`[etc]` 0\n",
		"stackable/contract/30.stk":             "[stackable type]\n`[etc]` 0\n[stack limit]\n1000\n",
		"stackable/contract/43.stk":             "[stackable type]\n`[etc]` 0\n[stack limit]\n1000\n",
		"stackable/contract/2660703.stk":        "[stackable type]\n`[etc]` 0\n[stack limit]\n1000\n",
		"stackable/contract/10000391.stk":       "[stackable type]\n`[etc]` 0\n[stack limit]\n1000\n",
		"stackable/material/filler.stk":         "[stackable type]\n`[waste]` 0\n[stack limit]\n1000\n",
		"stackable/material/challenge_coin.stk": "[stackable type]\n`[material]` 0\n[stack limit]\n1000\n",
		"stackable/box/contracts.stk":           "[stackable type]\n`[booster]` 0\n[stack limit]\n1000\n[booster info]\n[cera]\n1 43 1000 1\n[/cera]\n[cera]\n1 30 1000 1\n[/cera]\n[cera]\n1 10000391 1000 1\n[/cera]\n[cera]\n1 2660703 1000 1\n[/cera]\n[/booster info]\n",
		"stackable/box/revive.stk":              "[stackable type]\n`[booster]` 0\n[stack limit]\n1000\n[booster info]\n[cera]\n1 1 1000 2\n[/cera]\n[/booster info]\n[impossible contents]\ngift\n[/impossible contents]\n[need material]\n999 2\n[/need material]\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
