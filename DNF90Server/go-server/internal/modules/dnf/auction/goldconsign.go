package auction

// 金币寄售 (gold consignment).
//
// A player consigns a fixed denomination of gold for a Cera price; a buyer pays
// Cera and receives the gold; the seller receives 代币券 (token Cera) less a
// commission. Every wire shape here comes from a live capture - see
// `金币寄售_客户端逆向规格与实现方案_20260921.md` at the repository root - except
// where a comment says the shape is inferred.
//
// # Why this is a separate ledger from Listing
//
// The item auction escrows an ITEM and charges a gold deposit; a consignment
// escrows GOLD ITSELF and charges no deposit (86's GoldConsignmentService does
// the same: TryRegister spends the seller's gold and nothing else). The two
// also differ in currency on both ends - the item auction settles in gold, a
// consignment takes Cera from the buyer and pays token Cera to the seller. The
// two therefore share only the escrow character and the storage idiom:
//
//   - escrowed gold lives in `system:auction:escrow`'s Stats["gold"], exactly
//     where the item auction parks its registration deposits, so the wallet is
//     conserved by construction;
//   - one consignment is one ItemStack in that escrow character's inventory,
//     keyed `goldconsign:listing:<id>`, with the terms in Extra.
//
// # Transactions
//
// No repository transaction boundary in this tree spans a buyer's ACCOUNT and
// the escrow character at once (Cera is account-scoped metadata; the escrow is
// a character). The buy is therefore a short saga whose every step is either
// atomic on its own or idempotent and retryable:
//
//	Reserve  (escrow   tx) - Active -> Reserved, so only one buyer can win
//	Pay      (buyer    tx) - debit the buyer's Cera and record a pay marker
//	Settle   (escrow   tx) - Reserved -> Sold, release the escrowed gold,
//	                         write the durable sold row (also the fee history)
//	Deliver  (mail     tx) - the buyer's gold letter and the seller's token
//	                         Cera letter, each keyed by CompletionKey
//
// A crash between Pay and Settle leaves a Reserved row plus a pay marker; the
// reconciler finishes it. A crash between Settle and Deliver leaves a sold row
// with its letters unsent; Deliver is idempotent and retried on the next
// command. Nothing can pay twice and nothing can be lost.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	goldConsignListingPrefix = "goldconsign:listing:"
	goldConsignSoldPrefix    = "goldconsign:sold:"

	// GoldConsignEscrowGoldKey is the escrow character stat holding consigned
	// gold, shared with the item auction's registration deposits.
	GoldConsignEscrowGoldKey = "gold"

	// GoldConsignSellerLevel is the client's own minimum: dstr 70615 renders
	// 「未满Lv30的角色不可出售金币。」
	GoldConsignSellerLevel = 30
	// GoldConsignBuyerLevel is dstr 70614 「未满Lv20的角色不可购买金币。」
	GoldConsignBuyerLevel = 20

	// GoldConsignMaxActiveListings bounds one character's live consignments.
	GoldConsignMaxActiveListings = 10
	// GoldConsignMaxVisibleRows bounds one list response.
	GoldConsignMaxVisibleRows = 100
	// GoldConsignMaxDurationHours is the longest lifetime the client offers.
	GoldConsignMaxDurationHours = 48
	// GoldConsignMinDurationHours is the shortest lifetime the client offers.
	GoldConsignMinDurationHours = 24

	// GoldConsignBaseFeePercent is the commission in the ordinary band
	// (40%..150% of the recent average).
	GoldConsignBaseFeePercent = 5
	// GoldConsignCouponFeePercent is that band's rate with 拍卖行优惠券. The
	// coupon only ever helps in this band; every higher band ignores it.
	GoldConsignCouponFeePercent = 1

	// goldConsignAverageSamples is how many recent sales feed the average.
	goldConsignAverageSamples = 20
	// goldConsignSoldHistoryPerTier caps the durable sold rows kept per tier.
	goldConsignSoldHistoryPerTier = 64

	// GoldConsignReservationTTL is how long a Reserved row may sit before the
	// reconciler decides whether its payment landed.
	GoldConsignReservationTTL = 2 * time.Minute

	// goldConsignPayMarkerPrefix is the per-account key the Pay step writes so
	// the reconciler can tell "the buyer already paid" from "nobody paid".
	goldConsignPayMarkerPrefix = "goldconsign_paid:"
)

// GoldConsignStatus is the live state of one consignment row.
type GoldConsignStatus string

const (
	// GoldConsignActive is a consignment anybody may buy.
	GoldConsignActive GoldConsignStatus = "active"
	// GoldConsignReserved is one a buyer has claimed but not yet settled.
	GoldConsignReserved GoldConsignStatus = "reserved"
	// GoldConsignSold is one whose sale committed; the row is kept only as the
	// delivery intent and fee history until its letters have gone out.
	GoldConsignSold GoldConsignStatus = "sold"
)

var (
	// ErrGoldConsignPriceRejected is the official table's first row: a price
	// below 40% of the recent average cannot be listed at all.
	ErrGoldConsignPriceRejected = errors.New("gold consignment price is too far below the average")
	// ErrGoldConsignSellerLevel is dstr 70615.
	ErrGoldConsignSellerLevel = errors.New("gold consignment seller level is too low")
	// ErrGoldConsignBuyerLevel is dstr 70614.
	ErrGoldConsignBuyerLevel = errors.New("gold consignment buyer level is too low")
	// ErrGoldConsignNotTier is an item id that is not one of the 17
	// denominations the client offers.
	ErrGoldConsignNotTier = errors.New("item is not a gold consignment denomination")
	// ErrGoldConsignBuyerIsSeller is 86's BuyerIsSeller.
	ErrGoldConsignBuyerIsSeller = errors.New("gold consignment buyer is the seller")
	// ErrGoldConsignNotReserved is a settle for a row this buyer does not hold.
	ErrGoldConsignNotReserved = errors.New("gold consignment row is not reserved by this buyer")
	// ErrGoldConsignNotActive is a cancel or a reserve on a row that is no
	// longer live - already sold, already reserved by somebody else, or run out.
	ErrGoldConsignNotActive = errors.New("gold consignment row is not active")
	// ErrGoldConsignLimit is the per-character live-consignment cap.
	ErrGoldConsignLimit = errors.New("gold consignment listing limit reached")
)

// ---------------------------------------------------------------------------
// Denomination table
// ---------------------------------------------------------------------------

// GoldConsignTier is one 档位: the client's own item id and the gold it stands
// for. The amounts live only in the stk file NAME (gold_200m.stk) and its
// `[name]` text (「2亿」); the .stk carries no amount field, so this table is
// the server's authoritative reading of the client's UI.
type GoldConsignTier struct {
	ItemID int64
	Amount int64
}

// goldConsignTiers is the client's table, read straight off NoPack.exe .rdata
// VA 0x4580AA8: 17 u32 entries, NUL terminated, consumed by FUN_02086d20 with
// a loop bound of `cmp esi,0x44` (0x44 = 68 = 17 x 4).
//
// The ids are NOT contiguous: the first twelve run 2681725..2681736 and the
// last five jump to 2683069..2683073. They are the same two batches the client
// strings came in (dstr 70491..70502 then 72048..72052), so a base+index
// formula is wrong and would silently misprice five of the seventeen tiers.
var goldConsignTiers = []GoldConsignTier{
	{ItemID: 2681725, Amount: 1_000_000},
	{ItemID: 2681726, Amount: 2_000_000},
	{ItemID: 2681727, Amount: 3_000_000},
	{ItemID: 2681728, Amount: 4_000_000},
	{ItemID: 2681729, Amount: 5_000_000},
	{ItemID: 2681730, Amount: 6_000_000},
	{ItemID: 2681731, Amount: 7_000_000},
	{ItemID: 2681732, Amount: 8_000_000},
	{ItemID: 2681733, Amount: 9_000_000},
	{ItemID: 2681734, Amount: 10_000_000},
	{ItemID: 2681735, Amount: 20_000_000},
	{ItemID: 2681736, Amount: 30_000_000},
	{ItemID: 2683069, Amount: 40_000_000},
	{ItemID: 2683070, Amount: 50_000_000},
	{ItemID: 2683071, Amount: 100_000_000},
	{ItemID: 2683072, Amount: 150_000_000},
	{ItemID: 2683073, Amount: 200_000_000},
}

// GoldConsignTiers returns the denomination table, in the client's own order.
func GoldConsignTiers() []GoldConsignTier {
	out := make([]GoldConsignTier, len(goldConsignTiers))
	copy(out, goldConsignTiers)
	return out
}

// GoldConsignTierAmount resolves one denomination's gold, and reports whether
// the id is a denomination at all.
func GoldConsignTierAmount(itemID int64) (int64, bool) {
	for _, tier := range goldConsignTiers {
		if tier.ItemID == itemID {
			return tier.Amount, true
		}
	}
	return 0, false
}

// IsGoldConsignTier reports whether an item id is one of the 17 denominations.
func IsGoldConsignTier(itemID int64) bool {
	_, ok := GoldConsignTierAmount(itemID)
	return ok
}

// ---------------------------------------------------------------------------
// Commission table
// ---------------------------------------------------------------------------

// ResolveGoldConsignFee is the official commission table, expressed on
// r = price / average:
//
//	r <  0.40                 -> not listable at all
//	0.40 <= r <= 1.50         ->  5%   (1% with 拍卖行优惠券)
//	1.50 <  r <= 1.60         -> 30%   (coupon has no effect)
//	1.60 <  r <= 1.70         -> 40%   (coupon has no effect)
//	1.70 <  r <= 1.80         -> 50%   (coupon has no effect)
//	r >  1.80                 -> 95%   (coupon has no effect)
//
// The bands are read as left-open / right-closed because the table's own first
// row is 「40% 以下」 - "below 40%" - which excludes 40% itself.
//
// With no sales history the average is 0 and the table has no meaning: a
// division by zero, or else every price falling into the 95% band. This
// deliberately degrades to the ordinary-band rate instead, which the operator
// chose: 「0不动 多成交几次 均价就下来了」 - the bands switch on by themselves
// once real sales exist.
func ResolveGoldConsignFee(price, average int64, hasAuctionCoupon bool) (feePercent int64, listable bool) {
	if price <= 0 {
		return 0, false
	}
	if average <= 0 {
		return goldConsignBaseFee(hasAuctionCoupon), true
	}
	// Compare with integer maths throughout: price*100 against average*threshold
	// avoids both floating point and an overflow on the 2亿 tier.
	switch {
	case price*100 < average*40:
		return 0, false
	case price*100 <= average*150:
		return goldConsignBaseFee(hasAuctionCoupon), true
	case price*100 <= average*160:
		return 30, true
	case price*100 <= average*170:
		return 40, true
	case price*100 <= average*180:
		return 50, true
	default:
		return 95, true
	}
}

// goldConsignBaseFee is the 40%..150% band, the only one the coupon helps in.
func goldConsignBaseFee(hasAuctionCoupon bool) int64 {
	if hasAuctionCoupon {
		return GoldConsignCouponFeePercent
	}
	return GoldConsignBaseFeePercent
}

// ---------------------------------------------------------------------------
// Row model
// ---------------------------------------------------------------------------

// GoldConsignListing is one consignment, decoded from its escrow row.
type GoldConsignListing struct {
	ID         uint64
	SellerID   string
	SellerName string
	GoldItemID int64
	GoldAmount int64
	PriceCera  int64
	// FeePercent is the commission frozen at registration, in percent.
	FeePercent int64
	// ProceedsCera is PriceCera less that commission; what the seller's letter
	// carries as token Cera.
	ProceedsCera int64
	Status       GoldConsignStatus
	BuyerID      string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ReservedAt   time.Time
	// Delivered marks a sold row whose two letters have gone out.
	Delivered bool
}

// GoldConsignProceeds is the seller's token Cera for a sale price under a
// frozen rate: 售价 x (1 - 费率), floored.
//
// The flooring direction matters and is 86's: GoldConsignmentPolicy.
// ResolveSellerProceeds computes `ceraPrice * 95 / 100`, which floors the
// PROCEEDS rather than the commission. Taking the obvious `price - price*fee/100`
// instead floors the commission and hands the seller the rounded-away unit -
// 111 Cera at 5% pays 105 that way, not 106. The stored fee is always
// `price - proceeds`, so the two never disagree.
func GoldConsignProceeds(priceCera, feePercent int64) int64 {
	if priceCera <= 0 {
		return 0
	}
	if feePercent <= 0 {
		return priceCera
	}
	if feePercent >= 100 {
		return 0
	}
	return priceCera * (100 - feePercent) / 100
}

// GoldConsignFee is the commission that goes with GoldConsignProceeds, kept
// next to it so no caller can round the two apart.
func GoldConsignFee(priceCera, feePercent int64) int64 {
	proceeds := GoldConsignProceeds(priceCera, feePercent)
	if priceCera <= 0 {
		return 0
	}
	return priceCera - proceeds
}

func goldConsignListingKey(id uint64) string {
	return goldConsignListingPrefix + strconv.FormatUint(id, 10)
}

func goldConsignSoldKey(id uint64) string {
	return goldConsignSoldPrefix + strconv.FormatUint(id, 10)
}

// goldConsignRowFromStack decodes one escrow row. Any malformed row retires
// itself rather than being guessed at, matching listingFromStack.
func goldConsignRowFromStack(key string, stack dnfrepo.ItemStack) (GoldConsignListing, error) {
	extra := stack.Extra
	if extra == nil {
		return GoldConsignListing{}, ErrListingMissing
	}
	listing := GoldConsignListing{
		SellerID:     strings.TrimSpace(extra["seller_id"]),
		SellerName:   strings.TrimSpace(extra["seller_name"]),
		GoldItemID:   stack.ItemID,
		Status:       GoldConsignStatus(strings.TrimSpace(extra["status"])),
		BuyerID:      strings.TrimSpace(extra["buyer_id"]),
		Delivered:    strings.TrimSpace(extra["delivered"]) == "1",
		GoldAmount:   0,
		PriceCera:    0,
		FeePercent:   0,
		ProceedsCera: 0,
	}
	if listing.Status == "" {
		listing.Status = GoldConsignActive
	}
	amount, err := goldConsignExtraInt(extra, "gold_amount", 1, math.MaxInt64)
	if err != nil {
		return GoldConsignListing{}, err
	}
	price, err := goldConsignExtraInt(extra, "price_cera", 1, math.MaxInt64)
	if err != nil {
		return GoldConsignListing{}, err
	}
	fee, err := goldConsignExtraInt(extra, "fee_percent", 0, 100)
	if err != nil {
		return GoldConsignListing{}, err
	}
	created, err := goldConsignExtraInt(extra, "created_at", 1, math.MaxInt64)
	if err != nil {
		return GoldConsignListing{}, err
	}
	expires, err := goldConsignExtraInt(extra, "expires_at", 1, math.MaxInt64)
	if err != nil {
		return GoldConsignListing{}, err
	}
	reserved, err := goldConsignExtraInt(extra, "reserved_at", 0, math.MaxInt64)
	if err != nil {
		return GoldConsignListing{}, err
	}
	listing.GoldAmount = amount
	listing.PriceCera = price
	listing.FeePercent = fee
	listing.ProceedsCera = GoldConsignProceeds(price, fee)
	listing.CreatedAt = time.Unix(created, 0).UTC()
	listing.ExpiresAt = time.Unix(expires, 0).UTC()
	if reserved > 0 {
		listing.ReservedAt = time.Unix(reserved, 0).UTC()
	}
	if listing.SellerID == "" {
		return GoldConsignListing{}, ErrListingMissing
	}
	if listing.ID == 0 {
		if parsed, parseErr := strconv.ParseUint(strings.TrimPrefix(key, goldConsignListingPrefix), 10, 64); parseErr == nil {
			listing.ID = parsed
		} else if parsed, parseErr := strconv.ParseUint(strings.TrimPrefix(key, goldConsignSoldPrefix), 10, 64); parseErr == nil {
			listing.ID = parsed
		}
	}
	return listing, nil
}

// goldConsignExtraInt reads one optional numeric term. A missing key is the
// pre-field default; a present but unparseable one retires the row.
func goldConsignExtraInt(extra map[string]string, key string, low, high int64) (int64, error) {
	raw, present := extra[key]
	if !present {
		return 0, nil
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < low || value > high {
		return 0, ErrListingMissing
	}
	return value, nil
}

// goldConsignRowStack builds the escrow row for a consignment.
func goldConsignRowStack(listing GoldConsignListing) dnfrepo.ItemStack {
	extra := map[string]string{
		"seller_id":    listing.SellerID,
		"seller_name":  listing.SellerName,
		"gold_amount":  strconv.FormatInt(listing.GoldAmount, 10),
		"price_cera":   strconv.FormatInt(listing.PriceCera, 10),
		"fee_percent":  strconv.FormatInt(listing.FeePercent, 10),
		"status":       string(listing.Status),
		"created_at":   strconv.FormatInt(listing.CreatedAt.Unix(), 10),
		"expires_at":   strconv.FormatInt(listing.ExpiresAt.Unix(), 10),
		"reserved_at":  "0",
		"delivered":    "0",
		"buyer_id":     "",
		"sold_at":      "0",
		"listing_kind": "goldconsign",
	}
	if listing.BuyerID != "" {
		extra["buyer_id"] = listing.BuyerID
	}
	if !listing.ReservedAt.IsZero() {
		extra["reserved_at"] = strconv.FormatInt(listing.ReservedAt.Unix(), 10)
	}
	if listing.Delivered {
		extra["delivered"] = "1"
	}
	return dnfrepo.ItemStack{ItemID: listing.GoldItemID, Count: 1, Extra: extra}
}

// ---------------------------------------------------------------------------
// Owner
// ---------------------------------------------------------------------------

// GoldConsignOwner owns the consignment ledger. It embeds the item auction's
// Owner purely to reuse EnsureEscrow, so both ledgers share one escrow
// character and one wallet.
type GoldConsignOwner struct {
	owner        *Owner
	repositories dnfrepo.Group
	now          func() time.Time
}

// NewGoldConsignOwner builds the ledger owner.
func NewGoldConsignOwner(repositories dnfrepo.Group) (*GoldConsignOwner, error) {
	base, err := NewOwner(repositories)
	if err != nil {
		return nil, err
	}
	return &GoldConsignOwner{owner: base, repositories: repositories, now: time.Now}, nil
}

func (o *GoldConsignOwner) clock() time.Time {
	if o == nil || o.now == nil {
		return time.Now().UTC()
	}
	return o.now().UTC()
}

// EnsureEscrow creates the shared escrow character and wallet if missing.
func (o *GoldConsignOwner) EnsureEscrow(ctx context.Context) error {
	if o == nil || o.owner == nil {
		return ErrUnavailable
	}
	return o.owner.EnsureEscrow(ctx)
}

// GoldConsignRegisterCommand is one op183 registration.
type GoldConsignRegisterCommand struct {
	ListingID    uint64
	SellerID     string
	AccountID    string
	SellerName   string
	GoldItemID   int64
	PriceCera    int64
	DurationHour int64
	HasCoupon    bool
}

// GoldConsignResult is what a register/cancel returns to the protocol layer.
type GoldConsignResult struct {
	Listing GoldConsignListing
	// GoldAfter is the seller's gold once the escrow debit committed.
	GoldAfter int64
	// RefundGold is the gold a cancel or an expiry sends home by letter.
	RefundGold int64
}

// Register escrows the consigned gold and writes the row. It is the gold
// equivalent of Owner.Register: same escrow character, same two-character
// transaction, no deposit on top of the escrowed amount.
func (o *GoldConsignOwner) Register(ctx context.Context, cmd GoldConsignRegisterCommand) (GoldConsignResult, error) {
	cmd.SellerID = strings.TrimSpace(cmd.SellerID)
	cmd.AccountID = strings.TrimSpace(cmd.AccountID)
	cmd.SellerName = strings.TrimSpace(cmd.SellerName)
	if o == nil || cmd.ListingID == 0 || cmd.SellerID == "" || cmd.AccountID == "" ||
		cmd.SellerID == EscrowCharacterID || cmd.PriceCera <= 0 ||
		cmd.DurationHour < GoldConsignMinDurationHours || cmd.DurationHour > GoldConsignMaxDurationHours {
		return GoldConsignResult{}, ErrInvalidRequest
	}
	goldAmount, ok := GoldConsignTierAmount(cmd.GoldItemID)
	if !ok || goldAmount <= 0 {
		return GoldConsignResult{}, ErrGoldConsignNotTier
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return GoldConsignResult{}, err
	}
	var result GoldConsignResult
	err := o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, cmd.SellerID, EscrowCharacterID,
		func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			seller, found, err := characters.Load(ctx, cmd.SellerID)
			if err != nil || !found {
				return errors.Join(ErrInvalidRequest, err)
			}
			if strings.TrimSpace(seller.AccountID) != cmd.AccountID {
				return ErrInvalidRequest
			}
			if seller.Level < GoldConsignSellerLevel {
				return ErrGoldConsignSellerLevel
			}
			escrowCharacter, found, err := characters.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			if escrow.Slots == nil {
				escrow.Slots = make(map[string]dnfrepo.ItemStack)
			}
			if _, exists := escrow.Slots[goldConsignListingKey(cmd.ListingID)]; exists {
				return ErrListingIDExists
			}
			now := o.clock()
			active := 0
			for key, stack := range escrow.Slots {
				if !strings.HasPrefix(key, goldConsignListingPrefix) {
					continue
				}
				if strings.TrimSpace(stack.Extra["seller_id"]) != cmd.SellerID {
					continue
				}
				if status := strings.TrimSpace(stack.Extra["status"]); status != "" && status != string(GoldConsignActive) {
					continue
				}
				expires, parseErr := strconv.ParseInt(stack.Extra["expires_at"], 10, 64)
				if parseErr == nil && expires > now.Unix() {
					active++
				}
			}
			if active >= GoldConsignMaxActiveListings {
				return ErrGoldConsignLimit
			}
			// The commission is frozen now, under the average this moment sees.
			// The client tells the seller so at registration (dstr 70628), and a
			// frozen rate cannot turn a 5% listing into a 30% one mid-flight.
			average := goldConsignAverageFromSlots(escrow.Slots, cmd.GoldItemID)
			feePercent, listable := ResolveGoldConsignFee(cmd.PriceCera, average, cmd.HasCoupon)
			if !listable {
				return ErrGoldConsignPriceRejected
			}

			sellerGold := seller.Stats[GoldConsignEscrowGoldKey]
			if seller.Stats == nil || sellerGold < goldAmount {
				return ErrGoldUnavailable
			}
			if escrowCharacter.Stats == nil {
				escrowCharacter.Stats = make(map[string]int64)
			}
			if escrowCharacter.Stats[GoldConsignEscrowGoldKey] > math.MaxInt64-goldAmount {
				return ErrInvalidRequest
			}
			seller.Stats[GoldConsignEscrowGoldKey] = sellerGold - goldAmount
			escrowCharacter.Stats[GoldConsignEscrowGoldKey] += goldAmount

			listing := GoldConsignListing{
				ID:         cmd.ListingID,
				SellerID:   cmd.SellerID,
				SellerName: strings.TrimSpace(seller.Name),
				GoldItemID: cmd.GoldItemID,
				GoldAmount: goldAmount,
				PriceCera:  cmd.PriceCera,
				FeePercent: feePercent,
				Status:     GoldConsignActive,
				CreatedAt:  now,
				ExpiresAt:  now.Add(time.Duration(cmd.DurationHour) * time.Hour),
			}
			if listing.SellerName == "" {
				listing.SellerName = cmd.SellerName
			}
			listing.ProceedsCera = GoldConsignProceeds(listing.PriceCera, feePercent)
			if listing.ProceedsCera <= 0 {
				return ErrInvalidRequest
			}
			escrow.Slots[goldConsignListingKey(cmd.ListingID)] = goldConsignRowStack(listing)
			escrow.UpdatedAt = now
			seller.UpdatedAt = now
			escrowCharacter.UpdatedAt = now
			if err := characters.Save(ctx, seller); err != nil {
				return err
			}
			if err := characters.Save(ctx, escrowCharacter); err != nil {
				return err
			}
			if err := inventories.Save(ctx, escrow); err != nil {
				return err
			}
			result = GoldConsignResult{Listing: listing, GoldAfter: seller.Stats[GoldConsignEscrowGoldKey]}
			return nil
		})
	if err != nil {
		return GoldConsignResult{}, err
	}
	return result, nil
}

// goldConsignAverageFromSlots derives the recent-average price for one tier
// from the durable sold rows that share the escrow inventory. It reads plain
// ints out of Extra so it can run inside a transaction without extra loads.
func goldConsignAverageFromSlots(slots map[string]dnfrepo.ItemStack, goldItemID int64) int64 {
	prices := make([]int64, 0, goldConsignAverageSamples)
	for key, stack := range slots {
		if !strings.HasPrefix(key, goldConsignSoldPrefix) {
			continue
		}
		if stack.ItemID != goldItemID {
			continue
		}
		price, err := strconv.ParseInt(strings.TrimSpace(stack.Extra["price_cera"]), 10, 64)
		if err != nil || price <= 0 {
			continue
		}
		prices = append(prices, price)
	}
	if len(prices) == 0 {
		return 0
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	// Trim to the most recent slice by value: sold rows carry no ordering that
	// survives a map iteration, and the newest N is what the operator means by
	// "多成交几次 均价就下来了". A median-ish trim keeps one absurd outlier from
	// moving the band on its own.
	if len(prices) > goldConsignAverageSamples {
		prices = prices[len(prices)-goldConsignAverageSamples:]
	}
	var total int64
	for _, price := range prices {
		if total > math.MaxInt64-price {
			return 0
		}
		total += price
	}
	return total / int64(len(prices))
}

// AverageCeraPrice is the public read of the same figure, for op182.
func (o *GoldConsignOwner) AverageCeraPrice(ctx context.Context, goldItemID int64) (average int64, samples int64, err error) {
	if o == nil || goldItemID <= 0 {
		return 0, 0, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return 0, 0, err
	}
	record, found, err := o.repositories.Inventory.Load(ctx, EscrowCharacterID)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, nil
	}
	samples = 0
	for key, stack := range record.Slots {
		if strings.HasPrefix(key, goldConsignSoldPrefix) && stack.ItemID == goldItemID {
			samples++
		}
	}
	return goldConsignAverageFromSlots(record.Slots, goldItemID), samples, nil
}

// Search lists live consignments, optionally filtered to some tier ids. An
// empty filter means every tier.
func (o *GoldConsignOwner) Search(ctx context.Context, itemIDs []int64) ([]GoldConsignListing, error) {
	return o.list(ctx, func(listing GoldConsignListing, now time.Time) bool {
		if listing.Status != GoldConsignActive || !listing.ExpiresAt.After(now) {
			return false
		}
		if len(itemIDs) == 0 {
			return true
		}
		for _, itemID := range itemIDs {
			if itemID == listing.GoldItemID {
				return true
			}
		}
		return false
	})
}

// ListOwned lists one character's live consignments plus any reserved row it
// still holds, which is what the 我寄售的金币 tab shows.
func (o *GoldConsignOwner) ListOwned(ctx context.Context, sellerID string) ([]GoldConsignListing, error) {
	sellerID = strings.TrimSpace(sellerID)
	if sellerID == "" {
		return nil, ErrInvalidRequest
	}
	return o.list(ctx, func(listing GoldConsignListing, now time.Time) bool {
		if listing.SellerID != sellerID {
			return false
		}
		switch listing.Status {
		case GoldConsignActive, GoldConsignReserved:
			return true
		default:
			return false
		}
	})
}

func (o *GoldConsignOwner) list(ctx context.Context, keep func(GoldConsignListing, time.Time) bool) ([]GoldConsignListing, error) {
	if o == nil {
		return nil, ErrUnavailable
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return nil, err
	}
	record, found, err := o.repositories.Inventory.Load(ctx, EscrowCharacterID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	now := o.clock()
	out := make([]GoldConsignListing, 0)
	for key, stack := range record.Slots {
		if !strings.HasPrefix(key, goldConsignListingPrefix) {
			continue
		}
		listing, err := goldConsignRowFromStack(key, stack)
		if err != nil {
			continue
		}
		if keep(listing, now) {
			out = append(out, listing)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PriceCera == out[j].PriceCera {
			return out[i].ID < out[j].ID
		}
		return out[i].PriceCera < out[j].PriceCera
	})
	if len(out) > GoldConsignMaxVisibleRows {
		out = out[:GoldConsignMaxVisibleRows]
	}
	return out, nil
}

// LoadListing reads one row without locking, for read-only decisions.
func (o *GoldConsignOwner) LoadListing(ctx context.Context, id uint64) (GoldConsignListing, error) {
	if o == nil || id == 0 {
		return GoldConsignListing{}, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return GoldConsignListing{}, err
	}
	record, found, err := o.repositories.Inventory.Load(ctx, EscrowCharacterID)
	if err != nil {
		return GoldConsignListing{}, err
	}
	if !found {
		return GoldConsignListing{}, ErrListingMissing
	}
	stack, exists := record.Slots[goldConsignListingKey(id)]
	if !exists {
		return GoldConsignListing{}, ErrListingMissing
	}
	return goldConsignRowFromStack(goldConsignListingKey(id), stack)
}

// Cancel pulls a live consignment and returns the gold by letter. A reserved
// row is refused: a buyer is mid-payment on it, and the reconciler owns it.
func (o *GoldConsignOwner) Cancel(ctx context.Context, sellerID, accountID string, listingID uint64) (GoldConsignResult, error) {
	sellerID = strings.TrimSpace(sellerID)
	accountID = strings.TrimSpace(accountID)
	if o == nil || sellerID == "" || accountID == "" || listingID == 0 {
		return GoldConsignResult{}, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return GoldConsignResult{}, err
	}
	var result GoldConsignResult
	err := o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, sellerID, EscrowCharacterID,
		func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			seller, found, err := characters.Load(ctx, sellerID)
			if err != nil || !found || strings.TrimSpace(seller.AccountID) != accountID {
				return errors.Join(ErrNotOwner, err)
			}
			escrowCharacter, found, err := characters.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrListingMissing, err)
			}
			key := goldConsignListingKey(listingID)
			stack, exists := escrow.Slots[key]
			if !exists {
				return ErrListingMissing
			}
			listing, err := goldConsignRowFromStack(key, stack)
			if err != nil {
				return err
			}
			if listing.SellerID != sellerID {
				return ErrNotOwner
			}
			if listing.Status != GoldConsignActive {
				return ErrGoldConsignNotActive
			}
			if err := goldConsignReleaseGold(escrowCharacter, listing.GoldAmount); err != nil {
				return err
			}
			delete(escrow.Slots, key)
			now := o.clock()
			escrowCharacter.UpdatedAt = now
			escrow.UpdatedAt = now
			if err := characters.Save(ctx, escrowCharacter); err != nil {
				return err
			}
			if err := inventories.Save(ctx, escrow); err != nil {
				return err
			}
			result = GoldConsignResult{Listing: listing, RefundGold: listing.GoldAmount}
			return nil
		})
	if err != nil {
		return GoldConsignResult{}, err
	}
	return result, nil
}

// goldConsignReleaseGold takes consigned gold back out of the escrow wallet.
func goldConsignReleaseGold(escrow dnfrepo.CharacterRecord, amount int64) error {
	if amount <= 0 {
		return ErrInvalidRequest
	}
	held := escrow.Stats[GoldConsignEscrowGoldKey]
	if escrow.Stats == nil || held < amount {
		return ErrUnavailable
	}
	escrow.Stats[GoldConsignEscrowGoldKey] = held - amount
	return nil
}

// ExpiredReturn is one consignment an expiry sweep pulled, plus the gold its
// letter must carry.
type ExpiredReturn struct {
	Listing GoldConsignListing
	Refund  int64
}

// ReturnExpired pulls every live consignment of one seller whose duration ran
// out, mirroring Owner.ReturnExpiredListings. The caller sends the letters.
func (o *GoldConsignOwner) ReturnExpired(ctx context.Context, sellerID, accountID string) ([]ExpiredReturn, error) {
	sellerID = strings.TrimSpace(sellerID)
	accountID = strings.TrimSpace(accountID)
	if o == nil || sellerID == "" || accountID == "" {
		return nil, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return nil, err
	}
	var out []ExpiredReturn
	err := o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, sellerID, EscrowCharacterID,
		func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			seller, found, err := characters.Load(ctx, sellerID)
			if err != nil || !found || strings.TrimSpace(seller.AccountID) != accountID {
				return errors.Join(ErrNotOwner, err)
			}
			escrowCharacter, found, err := characters.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return nil
			}
			now := o.clock()
			keys := make([]string, 0)
			for key, stack := range escrow.Slots {
				if !strings.HasPrefix(key, goldConsignListingPrefix) {
					continue
				}
				listing, err := goldConsignRowFromStack(key, stack)
				if err != nil {
					continue
				}
				if listing.SellerID != sellerID || listing.Status != GoldConsignActive {
					continue
				}
				if listing.ExpiresAt.After(now) {
					continue
				}
				keys = append(keys, key)
			}
			sort.Strings(keys)
			changed := false
			for _, key := range keys {
				stack := escrow.Slots[key]
				listing, err := goldConsignRowFromStack(key, stack)
				if err != nil {
					continue
				}
				if err := goldConsignReleaseGold(escrowCharacter, listing.GoldAmount); err != nil {
					return err
				}
				delete(escrow.Slots, key)
				out = append(out, ExpiredReturn{Listing: listing, Refund: listing.GoldAmount})
				changed = true
			}
			if !changed {
				return nil
			}
			escrowCharacter.UpdatedAt = now
			escrow.UpdatedAt = now
			if err := characters.Save(ctx, escrowCharacter); err != nil {
				return err
			}
			return inventories.Save(ctx, escrow)
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Buy saga
// ---------------------------------------------------------------------------

// GoldConsignReservation is what Reserve hands the protocol layer: everything
// the payment step needs, read under the escrow lock.
type GoldConsignReservation struct {
	Listing GoldConsignListing
}

// Reserve claims a live consignment for one buyer. It is the only place that
// decides who wins a contested row, so a double-click or two racing buyers can
// never both proceed to payment.
func (o *GoldConsignOwner) Reserve(ctx context.Context, buyerID string, buyerAccountID string, listingID uint64) (GoldConsignReservation, error) {
	buyerID = strings.TrimSpace(buyerID)
	if o == nil || buyerID == "" || listingID == 0 {
		return GoldConsignReservation{}, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return GoldConsignReservation{}, err
	}
	var reservation GoldConsignReservation
	err := o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, buyerID, EscrowCharacterID,
		func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			buyer, found, err := characters.Load(ctx, buyerID)
			if err != nil || !found {
				return errors.Join(ErrInvalidRequest, err)
			}
			if buyerAccountID != "" && strings.TrimSpace(buyer.AccountID) != strings.TrimSpace(buyerAccountID) {
				return ErrInvalidRequest
			}
			if buyer.Level < GoldConsignBuyerLevel {
				return ErrGoldConsignBuyerLevel
			}
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrListingMissing, err)
			}
			key := goldConsignListingKey(listingID)
			stack, exists := escrow.Slots[key]
			if !exists {
				return ErrListingMissing
			}
			listing, err := goldConsignRowFromStack(key, stack)
			if err != nil {
				return err
			}
			now := o.clock()
			if listing.Status != GoldConsignActive || !listing.ExpiresAt.After(now) {
				return ErrGoldConsignNotActive
			}
			if listing.SellerID == buyerID {
				return ErrGoldConsignBuyerIsSeller
			}
			listing.Status = GoldConsignReserved
			listing.BuyerID = buyerID
			listing.ReservedAt = now
			escrow.Slots[key] = goldConsignRowStack(listing)
			escrow.UpdatedAt = now
			if err := inventories.Save(ctx, escrow); err != nil {
				return err
			}
			reservation = GoldConsignReservation{Listing: listing}
			return nil
		})
	if err != nil {
		return GoldConsignReservation{}, err
	}
	return reservation, nil
}

// Release puts a reservation back when the payment step refused it. A row that
// has meanwhile been settled is left alone.
func (o *GoldConsignOwner) Release(ctx context.Context, buyerID string, listingID uint64) error {
	buyerID = strings.TrimSpace(buyerID)
	if o == nil || buyerID == "" || listingID == 0 {
		return ErrInvalidRequest
	}
	return o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, buyerID, EscrowCharacterID,
		func(_ dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return nil
			}
			key := goldConsignListingKey(listingID)
			stack, exists := escrow.Slots[key]
			if !exists {
				return nil
			}
			listing, err := goldConsignRowFromStack(key, stack)
			if err != nil || listing.Status != GoldConsignReserved || listing.BuyerID != buyerID {
				return nil
			}
			listing.Status = GoldConsignActive
			listing.BuyerID = ""
			listing.ReservedAt = time.Time{}
			escrow.Slots[key] = goldConsignRowStack(listing)
			escrow.UpdatedAt = o.clock()
			return inventories.Save(ctx, escrow)
		})
}

// Settle commits a reserved sale: the escrowed gold leaves the wallet and the
// row becomes the durable delivery intent plus the fee history.
func (o *GoldConsignOwner) Settle(ctx context.Context, buyerID string, listingID uint64) (GoldConsignListing, error) {
	buyerID = strings.TrimSpace(buyerID)
	if o == nil || buyerID == "" || listingID == 0 {
		return GoldConsignListing{}, ErrInvalidRequest
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return GoldConsignListing{}, err
	}
	var settled GoldConsignListing
	err := o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, buyerID, EscrowCharacterID,
		func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			escrowCharacter, found, err := characters.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrListingMissing, err)
			}
			key := goldConsignListingKey(listingID)
			stack, exists := escrow.Slots[key]
			if !exists {
				return ErrListingMissing
			}
			listing, err := goldConsignRowFromStack(key, stack)
			if err != nil {
				return err
			}
			if listing.Status != GoldConsignReserved || listing.BuyerID != buyerID {
				return ErrGoldConsignNotReserved
			}
			if err := goldConsignReleaseGold(escrowCharacter, listing.GoldAmount); err != nil {
				return err
			}
			now := o.clock()
			listing.Status = GoldConsignSold
			if escrow.Slots == nil {
				// A store that round-trips an empty slot map as nil would
				// otherwise panic on the assignment below.
				escrow.Slots = make(map[string]dnfrepo.ItemStack)
			}
			soldStack := goldConsignRowStack(listing)
			soldStack.Extra["sold_at"] = strconv.FormatInt(now.Unix(), 10)
			delete(escrow.Slots, key)
			escrow.Slots[goldConsignSoldKey(listingID)] = soldStack
			goldConsignPruneSold(escrow.Slots, listing.GoldItemID)
			escrowCharacter.UpdatedAt = now
			escrow.UpdatedAt = now
			if err := characters.Save(ctx, escrowCharacter); err != nil {
				return err
			}
			if err := inventories.Save(ctx, escrow); err != nil {
				return err
			}
			settled = listing
			return nil
		})
	if err != nil {
		return GoldConsignListing{}, err
	}
	return settled, nil
}

// goldConsignPruneSold keeps the durable fee history bounded per tier.
func goldConsignPruneSold(slots map[string]dnfrepo.ItemStack, goldItemID int64) {
	type entry struct {
		key    string
		soldAt int64
	}
	entries := make([]entry, 0)
	for key, stack := range slots {
		if !strings.HasPrefix(key, goldConsignSoldPrefix) || stack.ItemID != goldItemID {
			continue
		}
		soldAt, err := strconv.ParseInt(strings.TrimSpace(stack.Extra["sold_at"]), 10, 64)
		if err != nil {
			soldAt = 0
		}
		entries = append(entries, entry{key: key, soldAt: soldAt})
	}
	if len(entries) <= goldConsignSoldHistoryPerTier {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].soldAt < entries[j].soldAt })
	for _, item := range entries[:len(entries)-goldConsignSoldHistoryPerTier] {
		delete(slots, item.key)
	}
}

// ReservedListings returns every reserved row, across all sellers and buyers.
// The reconciler uses it to decide whether an abandoned reservation goes back
// on sale or has to be settled.
func (o *GoldConsignOwner) ReservedListings(ctx context.Context) ([]GoldConsignListing, error) {
	return o.list(ctx, func(listing GoldConsignListing, _ time.Time) bool {
		return listing.Status == GoldConsignReserved
	})
}

// UndeliveredSales lists sold rows whose two letters have not gone out yet.
func (o *GoldConsignOwner) UndeliveredSales(ctx context.Context) ([]GoldConsignListing, error) {
	return o.listSold(ctx, false)
}

func (o *GoldConsignOwner) listSold(ctx context.Context, delivered bool) ([]GoldConsignListing, error) {
	if o == nil {
		return nil, ErrUnavailable
	}
	if err := o.EnsureEscrow(ctx); err != nil {
		return nil, err
	}
	record, found, err := o.repositories.Inventory.Load(ctx, EscrowCharacterID)
	if err != nil || !found {
		return nil, err
	}
	out := make([]GoldConsignListing, 0)
	for key, stack := range record.Slots {
		if !strings.HasPrefix(key, goldConsignSoldPrefix) {
			continue
		}
		listing, err := goldConsignRowFromStack(key, stack)
		if err != nil || listing.Delivered != delivered {
			continue
		}
		out = append(out, listing)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// MarkDelivered stamps a sold row once both letters are committed, so the
// pending sweep stops offering it. counterpartID is any real character taking
// part in the sale - the escrow inventory lives on the escrow character, and
// the trade boundary needs a second, distinct owner to reach it. The buyer is
// the one its caller already has.
//
// freshlyDelivered reports whether THIS call is the one that flipped the flag.
// The caller needs it because "already delivered" and "just delivered" both
// return a nil error - the row that is already stamped is success, not a
// failure - and the gold-consignment record must be written exactly once, on
// the transition. Re-deriving it from the caller's own view (for example "the
// row came out of UndeliveredSales, so it must have been undelivered") would
// be a guess about state this function is the only authority on; the
// record side does no de-duplication, so a wrong guess shows up as one
// duplicate line per consignment command.
func (o *GoldConsignOwner) MarkDelivered(ctx context.Context, counterpartID string, listingID uint64) (freshlyDelivered bool, err error) {
	counterpartID = strings.TrimSpace(counterpartID)
	if o == nil || counterpartID == "" || listingID == 0 || counterpartID == EscrowCharacterID {
		return false, ErrInvalidRequest
	}
	err = o.repositories.CharacterTrade.WithinCharacterTrade(
		ctx, counterpartID, EscrowCharacterID,
		func(_ dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
			escrow, found, err := inventories.Load(ctx, EscrowCharacterID)
			if err != nil || !found {
				return errors.Join(ErrUnavailable, err)
			}
			key := goldConsignSoldKey(listingID)
			stack, exists := escrow.Slots[key]
			if !exists {
				// Both letters are keyed, so an already-removed row is success.
				return nil
			}
			listing, err := goldConsignRowFromStack(key, stack)
			if err != nil {
				return err
			}
			if listing.Delivered {
				return nil
			}
			listing.Delivered = true
			updated := goldConsignRowStack(listing)
			updated.Extra["sold_at"] = stack.Extra["sold_at"]
			escrow.Slots[key] = updated
			escrow.UpdatedAt = o.clock()
			if err := inventories.Save(ctx, escrow); err != nil {
				return err
			}
			// 只有这一条分支是真跃迁；Save 落库成功之后才算数，
			// 所以置位必须放在 Save 之后，失败路径一律留在 false。
			freshlyDelivered = true
			return nil
		})
	if err != nil {
		return false, err
	}
	return freshlyDelivered, nil
}

// ---------------------------------------------------------------------------
// Pay markers
// ---------------------------------------------------------------------------

// GoldConsignPayMarkerKey is the per-account key the Pay step writes so the
// reconciler can distinguish "buyer already paid" from "nobody paid".
func GoldConsignPayMarkerKey(listingID uint64) string {
	return goldConsignPayMarkerPrefix + strconv.FormatUint(listingID, 10)
}

// ReadPayMarker reports whether one account has already paid for a listing.
func GoldConsignReadPayMarker(account dnfrepo.AccountRecord, listingID uint64) (string, bool) {
	if account.Metadata == nil {
		return "", false
	}
	value, ok := account.Metadata[GoldConsignPayMarkerKey(listingID)]
	return strings.TrimSpace(value), ok && strings.TrimSpace(value) != ""
}

// BuildPayMarker encodes what the settle step needs after a restart.
func BuildPayMarker(buyerID string, listing GoldConsignListing) string {
	return fmt.Sprintf("%s|%d|%d|%d|%s", strings.TrimSpace(buyerID), listing.PriceCera,
		listing.ProceedsCera, listing.GoldAmount, listing.SellerID)
}
