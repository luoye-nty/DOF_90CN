package itemtrade

import (
	"context"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 2026-09-11 12:00 北京时间 => 服务日 20260911（06:00 换日）。
var currentItemTradeFeeTestNow = time.Date(2026, 9, 11, 12, 0, 0, 0,
	time.FixedZone("Asia/Shanghai", 8*60*60))

func currentItemTradeFeeTestOwner(t *testing.T, first, second dnfrepo.CharacterRecord) (*Owner, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{first, second} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return currentItemTradeFeeTestNow }
	return owner, repositories
}

func currentItemTradeFeeCharacter(t *testing.T, repositories dnfrepo.Group, id string) dnfrepo.CharacterRecord {
	t.Helper()
	record, found, err := repositories.Character.Load(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("load character %s found=%t err=%v", id, found, err)
	}
	return record
}

func currentItemTradeFeeSmallTradeRecord(id string, gold int64, count int64) dnfrepo.CharacterRecord {
	stats := map[string]int64{"gold": gold}
	if count > 0 {
		stats[currentItemTradeSmallTradeDayStat] = currentItemTradeServiceDay(currentItemTradeFeeTestNow)
		stats[currentItemTradeSmallTradeCountStat] = count
	}
	return dnfrepo.CharacterRecord{CharacterID: id, AccountID: "acc", Name: "c" + id, Level: 90, Stats: stats}
}

// 小额（≤100万）且双方当日次数都没到 101：不收手续费，双方次数各 +1。
func TestOwnerExchangeSmallTradeUnderDailyLimitIsFree(t *testing.T) {
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 500_000, 100),
		currentItemTradeFeeSmallTradeRecord("5", 50_000, 7),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	// 给钱方 500000-100000；收钱方 50000+100000，都不扣手续费。
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 400_000 {
		t.Fatalf("sender gold = %d, want 400000", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 150_000 {
		t.Fatalf("receiver gold = %d, want 150000", got)
	}
	sender := currentItemTradeFeeCharacter(t, repositories, "1")
	receiver := currentItemTradeFeeCharacter(t, repositories, "5")
	if sender.Stats[currentItemTradeSmallTradeCountStat] != 101 {
		t.Fatalf("sender small count = %d, want 101", sender.Stats[currentItemTradeSmallTradeCountStat])
	}
	if receiver.Stats[currentItemTradeSmallTradeCountStat] != 8 {
		t.Fatalf("receiver small count = %d, want 8", receiver.Stats[currentItemTradeSmallTradeCountStat])
	}
}

// 收钱方当日次数已到 101：收钱方自己付 3%，给钱方完全不受影响。
func TestOwnerExchangeSmallTradeChargesReceiverOverDailyLimit(t *testing.T) {
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 500_000, 0),
		currentItemTradeFeeSmallTradeRecord("5", 50_000, 101),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 400_000 {
		t.Fatalf("sender gold = %d, want 400000 (unaffected)", got)
	}
	// 50000 + 100000 - 3% * 100000 = 147000
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 147_000 {
		t.Fatalf("receiver gold = %d, want 147000", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats[currentItemTradeSmallTradeCountStat]; got != 102 {
		t.Fatalf("receiver small count = %d, want 102", got)
	}
}

// 给钱方当日次数已到 101：由给钱方额外承担 3%，收钱方拿到全额。
func TestOwnerExchangeSmallTradeChargesSenderOverDailyLimit(t *testing.T) {
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 500_000, 101),
		currentItemTradeFeeSmallTradeRecord("5", 50_000, 0),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	// 500000 - 100000 - 3000 = 397000
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 397_000 {
		t.Fatalf("sender gold = %d, want 397000", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 150_000 {
		t.Fatalf("receiver gold = %d, want 150000 (unaffected)", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats[currentItemTradeSmallTradeCountStat]; got != 102 {
		t.Fatalf("sender small count = %d, want 102", got)
	}
}

// 超过 100万：不看次数，收钱方在到账前被扣 3%；小额计数不增加。
func TestOwnerExchangeLargeGoldAlwaysChargesReceiver(t *testing.T) {
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 5_000_000, 0),
		currentItemTradeFeeSmallTradeRecord("5", 0, 0),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 1_000_001},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	// 1000001 * 3 / 100 = 30000（向下取整）
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 970_001 {
		t.Fatalf("receiver gold = %d, want 970001", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 3_999_999 {
		t.Fatalf("sender gold = %d, want 3999999", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats[currentItemTradeSmallTradeCountStat]; got != 0 {
		t.Fatalf("large trade must not touch the small-trade counter, got %d", got)
	}
}

// 恰好 100万算小额；100万零1 算大额。
func TestOwnerExchangeSmallTradeCeilingIsOneMillion(t *testing.T) {
	if currentItemTradeFee(1_000_000) != 30_000 {
		t.Fatalf("fee(1000000) = %d", currentItemTradeFee(1_000_000))
	}
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 5_000_000, 0),
		currentItemTradeFeeSmallTradeRecord("5", 0, 0),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 1_000_000},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 1_000_000 {
		t.Fatalf("receiver gold = %d, want 1000000 (exactly one million is a free small trade)", got)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats[currentItemTradeSmallTradeCountStat]; got != 1 {
		t.Fatalf("receiver small count = %d, want 1", got)
	}
}

// 前一天的服务日计数在 06:00 换日后归零。
func TestOwnerExchangeSmallTradeCounterResetsAtBeijingSix(t *testing.T) {
	ctx := context.Background()
	sender := currentItemTradeFeeSmallTradeRecord("1", 500_000, 0)
	sender.Stats[currentItemTradeSmallTradeDayStat] = 20260910 // 昨天
	sender.Stats[currentItemTradeSmallTradeCountStat] = 999
	receiver := currentItemTradeFeeSmallTradeRecord("5", 50_000, 0)
	owner, repositories := currentItemTradeFeeTestOwner(t, sender, receiver)

	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5"},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 400_000 {
		t.Fatalf("sender gold = %d, want 400000 (yesterday's count must not charge)", got)
	}
	stored := currentItemTradeFeeCharacter(t, repositories, "1")
	if stored.Stats[currentItemTradeSmallTradeDayStat] != 20260911 ||
		stored.Stats[currentItemTradeSmallTradeCountStat] != 1 {
		t.Fatalf("sender counter day=%d count=%d, want 20260911/1",
			stored.Stats[currentItemTradeSmallTradeDayStat],
			stored.Stats[currentItemTradeSmallTradeCountStat])
	}
}

// 双方都超限：各自为自己的方向付 3%，互不牵连。
func TestOwnerExchangeSmallTradeBothSidesOverLimitPaySeparately(t *testing.T) {
	ctx := context.Background()
	owner, repositories := currentItemTradeFeeTestOwner(t,
		currentItemTradeFeeSmallTradeRecord("1", 500_000, 101),
		currentItemTradeFeeSmallTradeRecord("5", 500_000, 101),
	)
	if _, err := owner.Exchange(ctx,
		Participant{CharacterID: "1", Gold: 100_000},
		Participant{CharacterID: "5", Gold: 200_000},
	); err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	// 角色1：-100000（给出）+200000（收到）-6000（收到那份的3%）-3000（给出那份的3%）
	if got := currentItemTradeFeeCharacter(t, repositories, "1").Stats["gold"]; got != 591_000 {
		t.Fatalf("character 1 gold = %d, want 591000", got)
	}
	// 角色5：-200000 +100000 -3000 -6000
	if got := currentItemTradeFeeCharacter(t, repositories, "5").Stats["gold"]; got != 391_000 {
		t.Fatalf("character 5 gold = %d, want 391000", got)
	}
}

// 回归保护：个人商店（摆摊）收购走的是 ExchangeWithCompletionKey，不是玩家交易，
// 即使金额远超 100万 也绝对不能收手续费、不能写小额次数。
// 金额特意取大，否则 3% 向下取整会变成 0，测不出问题。
func TestOwnerExchangeWithCompletionKeyNeverChargesTradeFee(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, record := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "seller", Name: "seller", Stats: map[string]int64{"gold": 0}},
		{CharacterID: "5", AccountID: "buyer", Name: "buyer", Stats: map[string]int64{"gold": 5_000_000}},
	} {
		if err := repositories.Character.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots:       map[string]dnfrepo.ItemStack{"0:65": {ItemID: 2002, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "5", Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	owner.now = func() time.Time { return currentItemTradeFeeTestNow }

	seller := Participant{CharacterID: "1", Offers: []Offer{
		{TradeSlot: 7, SourceList: 0, SourceSlot: 65, Count: 1, ExpectedItem: 2002},
	}}
	buyer := Participant{CharacterID: "5", Gold: 2_000_000}
	if _, err := owner.ExchangeWithCompletionKey(ctx, "private-store:9", "5", seller, buyer); err != nil {
		t.Fatalf("ExchangeWithCompletionKey() error = %v", err)
	}
	stall := currentItemTradeFeeCharacter(t, repositories, "1")
	purchaser := currentItemTradeFeeCharacter(t, repositories, "5")
	if stall.Stats["gold"] != 2_000_000 {
		t.Fatalf("stall owner gold = %d, want 2000000 (no 3%% trade fee on a private store)", stall.Stats["gold"])
	}
	if purchaser.Stats["gold"] != 3_000_000 {
		t.Fatalf("buyer gold = %d, want 3000000", purchaser.Stats["gold"])
	}
	for _, id := range []string{"1", "5"} {
		record := currentItemTradeFeeCharacter(t, repositories, id)
		if _, found := record.Stats[currentItemTradeSmallTradeCountStat]; found {
			t.Fatalf("character %s small-trade counter was written by a private store purchase", id)
		}
	}
}
