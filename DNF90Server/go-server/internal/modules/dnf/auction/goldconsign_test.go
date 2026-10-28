package auction

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// seedGoldConsignCharacter writes a character with the level the consignment
// gates look at, plus a gold wallet.
func seedGoldConsignCharacter(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID, accountID, name string, level int, gold int64) {
	t.Helper()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: characterID, AccountID: accountID, Name: name, Level: level,
		Stats: map[string]int64{"gold": gold},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: characterID, Slots: map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID, Metadata: map[string]string{},
	}); err != nil {
		t.Fatal(err)
	}
}

func goldConsignCharGold(t *testing.T, ctx context.Context, repositories dnfrepo.Group, characterID string) int64 {
	t.Helper()
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load character %s: found=%t err=%v", characterID, found, err)
	}
	return character.Stats["gold"]
}

func goldConsignEscrowGold(t *testing.T, ctx context.Context, repositories dnfrepo.Group) int64 {
	t.Helper()
	character, found, err := repositories.Character.Load(ctx, EscrowCharacterID)
	if err != nil || !found {
		t.Fatalf("load escrow: found=%t err=%v", found, err)
	}
	return character.Stats[GoldConsignEscrowGoldKey]
}

func mustGoldConsignOwner(t *testing.T, repositories dnfrepo.Group) *GoldConsignOwner {
	t.Helper()
	owner, err := NewGoldConsignOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

// The table is the client's own, read off NoPack.exe .rdata VA 0x4580AA8. The
// ids are deliberately NOT contiguous, so this test exists to stop anybody
// "simplifying" it into a base+index formula that would misprice five tiers.
func TestGoldConsignTierTableMatchesTheClientImage(t *testing.T) {
	expected := []GoldConsignTier{
		{2681725, 1_000_000}, {2681726, 2_000_000}, {2681727, 3_000_000},
		{2681728, 4_000_000}, {2681729, 5_000_000}, {2681730, 6_000_000},
		{2681731, 7_000_000}, {2681732, 8_000_000}, {2681733, 9_000_000},
		{2681734, 10_000_000}, {2681735, 20_000_000}, {2681736, 30_000_000},
		{2683069, 40_000_000}, {2683070, 50_000_000}, {2683071, 100_000_000},
		{2683072, 150_000_000}, {2683073, 200_000_000},
	}
	got := GoldConsignTiers()
	if len(got) != 17 || len(got) != len(expected) {
		t.Fatalf("tier count = %d, want 17", len(got))
	}
	for index := range expected {
		if got[index] != expected[index] {
			t.Fatalf("tier %d = %+v, want %+v", index, got[index], expected[index])
		}
	}
	// The gap is the whole point: 2681737 is NOT a tier, and neither is
	// 2681741, which a contiguous formula would happily resolve.
	for _, absent := range []int64{2681724, 2681737, 2681741, 2683068, 2683074, 0, -1} {
		if IsGoldConsignTier(absent) {
			t.Fatalf("item %d must not be a gold tier", absent)
		}
	}
	if amount, ok := GoldConsignTierAmount(2683073); !ok || amount != 200_000_000 {
		t.Fatalf("top tier = %d ok=%t, want 200000000", amount, ok)
	}
}

// The official commission table, including its boundaries: the first row reads
// 「40% 以下」, so exactly 40% is still listable.
func TestResolveGoldConsignFeeFollowsTheOfficialTable(t *testing.T) {
	const average = 1000
	for _, test := range []struct {
		name       string
		price      int64
		coupon     bool
		wantFee    int64
		wantListed bool
	}{
		{"below 40% is refused", 399, false, 0, false},
		{"below 40% is refused with a coupon too", 399, true, 0, false},
		{"exactly 40% lists", 400, false, 5, true},
		{"exactly 40% lists at the coupon rate", 400, true, 1, true},
		{"mid band", 900, false, 5, true},
		{"exactly 150% is still the base band", 1500, false, 5, true},
		{"exactly 150% takes the coupon", 1500, true, 1, true},
		{"just over 150% jumps to 30%", 1501, false, 30, true},
		{"the coupon cannot help above 150%", 1501, true, 30, true},
		{"exactly 160%", 1600, false, 30, true},
		{"just over 160%", 1601, false, 40, true},
		{"exactly 170%", 1700, false, 40, true},
		{"just over 170%", 1701, false, 50, true},
		{"exactly 180%", 1800, false, 50, true},
		{"just over 180%", 1801, false, 95, true},
		{"the coupon cannot help in the 95% band either", 9999, true, 95, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fee, listable := ResolveGoldConsignFee(test.price, average, test.coupon)
			if listable != test.wantListed || fee != test.wantFee {
				t.Fatalf("fee=%d listable=%t, want fee=%d listable=%t",
					fee, listable, test.wantFee, test.wantListed)
			}
		})
	}
}

// With no sales the average is 0 and the table has no meaning. The operator's
// decision is to keep selling at the ordinary band rather than divide by zero
// or dump every listing into the 95% band.
func TestGoldConsignNoSalesFallsBackToTheBaseFee(t *testing.T) {
	if fee, listable := ResolveGoldConsignFee(1, 0, false); !listable || fee != GoldConsignBaseFeePercent {
		t.Fatalf("no-history fee=%d listable=%t, want %d/true", fee, listable, GoldConsignBaseFeePercent)
	}
	if fee, listable := ResolveGoldConsignFee(999_999, 0, true); !listable || fee != GoldConsignCouponFeePercent {
		t.Fatalf("no-history coupon fee=%d listable=%t, want %d/true", fee, listable, GoldConsignCouponFeePercent)
	}
	// A non-positive price is never listable, average or not.
	if _, listable := ResolveGoldConsignFee(0, 0, false); listable {
		t.Fatal("a zero price must not be listable")
	}
}

func TestGoldConsignProceedsApplyTheFrozenCommission(t *testing.T) {
	for _, test := range []struct{ price, fee, want int64 }{
		{111, 5, 105},
		{111, 1, 109},
		{100, 30, 70},
		{100, 95, 5},
		{100, 100, 0},
		{0, 5, 0},
	} {
		if got := GoldConsignProceeds(test.price, test.fee); got != test.want {
			t.Fatalf("proceeds(%d,%d) = %d, want %d", test.price, test.fee, got, test.want)
		}
	}
}

// Register escrows the consigned gold itself - no extra deposit - and writes
// one row whose frozen fee is what the seller was told at registration.
func TestGoldConsignRegisterEscrowsGoldAndFreezesTheFee(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "卖家", 40, 50_000_000)
	owner := mustGoldConsignOwner(t, repositories)

	result, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 1, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Listing.GoldAmount != 1_000_000 {
		t.Fatalf("escrowed amount = %d, want 1000000", result.Listing.GoldAmount)
	}
	if result.Listing.SellerName != "卖家" {
		t.Fatalf("seller name = %q, want the character row's own", result.Listing.SellerName)
	}
	// No sales yet, so the ordinary band applies and the coupon is irrelevant.
	if result.Listing.FeePercent != GoldConsignBaseFeePercent {
		t.Fatalf("frozen fee = %d, want %d", result.Listing.FeePercent, GoldConsignBaseFeePercent)
	}
	if result.Listing.ProceedsCera != 105 {
		t.Fatalf("proceeds = %d, want 105", result.Listing.ProceedsCera)
	}
	if gold := goldConsignCharGold(t, ctx, repositories, "seller"); gold != 49_000_000 {
		t.Fatalf("seller gold = %d, want 49000000", gold)
	}
	if gold := goldConsignEscrowGold(t, ctx, repositories); gold != 1_000_000 {
		t.Fatalf("escrow gold = %d, want 1000000", gold)
	}
	if result.Listing.ExpiresAt.Sub(result.Listing.CreatedAt) != 24*time.Hour {
		t.Fatalf("lifetime = %v, want 24h", result.Listing.ExpiresAt.Sub(result.Listing.CreatedAt))
	}
	// The gold is conserved: seller + escrow is what the seller started with.
	if goldConsignCharGold(t, ctx, repositories, "seller")+goldConsignEscrowGold(t, ctx, repositories) != 50_000_000 {
		t.Fatal("gold was created or destroyed by the escrow move")
	}
}

func TestGoldConsignRegisterRefusesWhatItCannotHonour(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		cmd   GoldConsignRegisterCommand
		gold  int64
		level int
		want  error
	}{
		{
			name: "seller below level 30",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "account-seller", GoldItemID: 2681725, PriceCera: 111, DurationHour: 24},
			gold: 50_000_000, level: 29, want: ErrGoldConsignSellerLevel,
		},
		{
			name: "item is not a denomination",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "account-seller", GoldItemID: 2681741, PriceCera: 111, DurationHour: 24},
			gold: 50_000_000, level: 40, want: ErrGoldConsignNotTier,
		},
		{
			name: "not enough gold to consign",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "account-seller", GoldItemID: 2681736, PriceCera: 111, DurationHour: 24},
			gold: 29_999_999, level: 40, want: ErrGoldUnavailable,
		},
		{
			name: "lifetime outside 24/48",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "account-seller", GoldItemID: 2681725, PriceCera: 111, DurationHour: 12},
			gold: 50_000_000, level: 40, want: ErrInvalidRequest,
		},
		{
			name: "non-positive price",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "account-seller", GoldItemID: 2681725, PriceCera: 0, DurationHour: 24},
			gold: 50_000_000, level: 40, want: ErrInvalidRequest,
		},
		{
			name: "wrong account",
			cmd:  GoldConsignRegisterCommand{ListingID: 1, SellerID: "seller", AccountID: "someone-else", GoldItemID: 2681725, PriceCera: 111, DurationHour: 24},
			gold: 50_000_000, level: 40, want: ErrInvalidRequest,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", test.level, test.gold)
			owner := mustGoldConsignOwner(t, repositories)
			if _, err := owner.Register(ctx, test.cmd); !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
			// A refusal must not move any gold.
			if gold := goldConsignCharGold(t, ctx, repositories, "seller"); gold != test.gold {
				t.Fatalf("refused register moved gold: %d, want %d", gold, test.gold)
			}
		})
	}
}

func TestGoldConsignCancelReturnsTheGoldAndDropsTheRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 50_000_000)
	owner := mustGoldConsignOwner(t, repositories)
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 7, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681735, PriceCera: 900, DurationHour: 48,
	}); err != nil {
		t.Fatal(err)
	}
	if gold := goldConsignEscrowGold(t, ctx, repositories); gold != 20_000_000 {
		t.Fatalf("escrow before cancel = %d, want 20000000", gold)
	}
	result, err := owner.Cancel(ctx, "seller", "account-seller", 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.RefundGold != 20_000_000 {
		t.Fatalf("refund = %d, want 20000000", result.RefundGold)
	}
	if gold := goldConsignEscrowGold(t, ctx, repositories); gold != 0 {
		t.Fatalf("escrow after cancel = %d, want 0", gold)
	}
	// The refund goes home by letter, so the seller's wallet is untouched here
	// and the gold sits in no wallet at all until the letter is claimed.
	if gold := goldConsignCharGold(t, ctx, repositories, "seller"); gold != 30_000_000 {
		t.Fatalf("seller gold after cancel = %d, want 30000000 (still unredeemed)", gold)
	}
	if _, err := owner.LoadListing(ctx, 7); !errors.Is(err, ErrListingMissing) {
		t.Fatalf("cancelled row still loads: %v", err)
	}
	// Cancelling twice is a missing row, not a second refund.
	if _, err := owner.Cancel(ctx, "seller", "account-seller", 7); !errors.Is(err, ErrListingMissing) {
		t.Fatalf("second cancel err = %v, want ErrListingMissing", err)
	}
	// Somebody else's cancel is refused.
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 8, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Cancel(ctx, "someone-else", "someone-else", 8); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("foreign cancel err = %v, want ErrNotOwner", err)
	}
}

// The buy saga: reserve, settle, and the sold row that becomes both the
// delivery intent and the fee history.
func TestGoldConsignBuySagaSettlesAndFeedsTheAverage(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 10_000_000)
	seedGoldConsignCharacter(t, ctx, repositories, "buyer", "account-buyer", "Buyer", 20, 0)
	owner := mustGoldConsignOwner(t, repositories)
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 3, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if average, samples, err := owner.AverageCeraPrice(ctx, 2681725); err != nil || average != 0 || samples != 0 {
		t.Fatalf("average before any sale = %d/%d err=%v, want 0/0", average, samples, err)
	}

	reservation, err := owner.Reserve(ctx, "buyer", "account-buyer", 3)
	if err != nil {
		t.Fatal(err)
	}
	if reservation.Listing.Status != GoldConsignReserved || reservation.Listing.BuyerID != "buyer" {
		t.Fatalf("reservation = %+v", reservation.Listing)
	}
	// The row is out of the shop while reserved, so nobody else can take it.
	live, err := owner.Search(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("a reserved row must not stay on sale: %+v", live)
	}
	if _, err := owner.Reserve(ctx, "buyer", "account-buyer", 3); !errors.Is(err, ErrGoldConsignNotActive) {
		t.Fatalf("double reserve err = %v, want ErrGoldConsignNotActive", err)
	}

	sold, err := owner.Settle(ctx, "buyer", 3)
	if err != nil {
		t.Fatal(err)
	}
	if sold.Status != GoldConsignSold || sold.ProceedsCera != 105 {
		t.Fatalf("settled = %+v, want sold with 105 proceeds", sold)
	}
	// The gold has left the escrow wallet for the buyer's letter.
	if gold := goldConsignEscrowGold(t, ctx, repositories); gold != 0 {
		t.Fatalf("escrow after settle = %d, want 0", gold)
	}
	// And it is now the fee history the next registration reads.
	average, samples, err := owner.AverageCeraPrice(ctx, 2681725)
	if err != nil || average != 111 || samples != 1 {
		t.Fatalf("average after one sale = %d/%d err=%v, want 111/1", average, samples, err)
	}
	pending, err := owner.UndeliveredSales(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != 3 || pending[0].Delivered {
		t.Fatalf("undelivered sales = %+v", pending)
	}
	// 第一次盖章才算跃迁（freshlyDelivered=true）——记录侧全靠这一位保证只写一条。
	freshlyDelivered, err := owner.MarkDelivered(ctx, "buyer", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !freshlyDelivered {
		t.Fatal("首次 MarkDelivered 必须报告 freshlyDelivered=true")
	}
	// 重放同一次盖章：行已经盖过，必须报告 false（否则记录会多出一条成交收款）。
	if again, err := owner.MarkDelivered(ctx, "buyer", 3); err != nil || again {
		t.Fatalf("重放 MarkDelivered = %t/%v, want false/nil", again, err)
	}
	pending, err = owner.UndeliveredSales(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("stamped sale still pending: %+v", pending)
	}
	// Releasing a settled row must not resurrect it.
	if err := owner.Release(ctx, "buyer", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Settle(ctx, "buyer", 3); !errors.Is(err, ErrListingMissing) {
		t.Fatalf("re-settle err = %v, want ErrListingMissing", err)
	}
}

// Once a sale exists the bands switch on by themselves, which is the whole
// point of starting the average at zero.
func TestGoldConsignBandsActivateOnceSalesExist(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 100_000_000)
	seedGoldConsignCharacter(t, ctx, repositories, "buyer", "account-buyer", "Buyer", 20, 0)
	owner := mustGoldConsignOwner(t, repositories)
	// Seed the history directly: one 1百万 sale at 1000 Cera.
	box := GoldConsignListing{
		ID: 900, SellerID: "seller", GoldItemID: 2681725, GoldAmount: 1_000_000,
		PriceCera: 1000, FeePercent: 5, ProceedsCera: 950, Status: GoldConsignSold,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := owner.EnsureEscrow(ctx); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Inventory.Load(ctx, EscrowCharacterID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if stored.Slots == nil {
		stored.Slots = map[string]dnfrepo.ItemStack{}
	}
	stack := goldConsignRowStack(box)
	stack.Extra["sold_at"] = "1"
	stored.Slots[goldConsignSoldKey(box.ID)] = stack
	if err := repositories.Inventory.Save(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if average, samples, err := owner.AverageCeraPrice(ctx, 2681725); err != nil || average != 1000 || samples != 1 {
		t.Fatalf("seeded average = %d/%d err=%v, want 1000/1", average, samples, err)
	}
	// 300 is below 40% of 1000, so the table refuses the listing outright.
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 11, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 300, DurationHour: 24,
	}); !errors.Is(err, ErrGoldConsignPriceRejected) {
		t.Fatalf("below-band register err = %v, want ErrGoldConsignPriceRejected", err)
	}
	// 400 is exactly 40% and lists at the base rate.
	result, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 12, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 400, DurationHour: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Listing.FeePercent != GoldConsignBaseFeePercent {
		t.Fatalf("40%% band fee = %d, want %d", result.Listing.FeePercent, GoldConsignBaseFeePercent)
	}
	// 2000 is 200% of the average, which is the 95% band.
	steep, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 13, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 2000, DurationHour: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if steep.Listing.FeePercent != 95 {
		t.Fatalf("200%% band fee = %d, want 95", steep.Listing.FeePercent)
	}
}

func TestGoldConsignExpiryReturnsTheGoldAndClearsTheRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 5_000_000)
	owner := mustGoldConsignOwner(t, repositories)
	// A past expiry is produced by moving the ledger's clock back far enough
	// that Register's own 24h lifetime has already run out: 30h ago plus 24h
	// leaves the row six hours stale.
	owner.now = func() time.Time { return time.Now().UTC().Add(-30 * time.Hour) }
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 21, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	}); err != nil {
		t.Fatal(err)
	}
	owner.now = time.Now
	expired, err := owner.ReturnExpired(ctx, "seller", "account-seller")
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].Refund != 1_000_000 {
		t.Fatalf("expired = %+v", expired)
	}
	if gold := goldConsignEscrowGold(t, ctx, repositories); gold != 0 {
		t.Fatalf("escrow after expiry = %d, want 0", gold)
	}
	again, err := owner.ReturnExpired(ctx, "seller", "account-seller")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second sweep re-returned %+v", again)
	}
}

// A buyer may not take their own consignment, and a buyer below level 20 is
// refused before any money moves.
func TestGoldConsignBuyRefusesSelfDealingAndLowLevel(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 5_000_000)
	seedGoldConsignCharacter(t, ctx, repositories, "lowbie", "account-lowbie", "Lowbie", 19, 0)
	owner := mustGoldConsignOwner(t, repositories)
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 31, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Reserve(ctx, "seller", "account-seller", 31); !errors.Is(err, ErrGoldConsignBuyerIsSeller) {
		t.Fatalf("self-buy err = %v, want ErrGoldConsignBuyerIsSeller", err)
	}
	if _, err := owner.Reserve(ctx, "lowbie", "account-lowbie", 31); !errors.Is(err, ErrGoldConsignBuyerLevel) {
		t.Fatalf("low-level buy err = %v, want ErrGoldConsignBuyerLevel", err)
	}
	// Neither refusal may have taken the row off sale.
	live, err := owner.Search(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("refused reserves changed the listing set: %+v", live)
	}
}

func TestGoldConsignActiveListingLimit(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedGoldConsignCharacter(t, ctx, repositories, "seller", "account-seller", "Seller", 40, 50_000_000)
	owner := mustGoldConsignOwner(t, repositories)
	for index := 0; index < GoldConsignMaxActiveListings; index++ {
		if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
			ListingID: uint64(100 + index), SellerID: "seller", AccountID: "account-seller",
			GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
		}); err != nil {
			t.Fatalf("listing %d: %v", index, err)
		}
	}
	if _, err := owner.Register(ctx, GoldConsignRegisterCommand{
		ListingID: 200, SellerID: "seller", AccountID: "account-seller",
		GoldItemID: 2681725, PriceCera: 111, DurationHour: 24,
	}); !errors.Is(err, ErrGoldConsignLimit) {
		t.Fatalf("over-limit register err = %v, want ErrGoldConsignLimit", err)
	}
}
