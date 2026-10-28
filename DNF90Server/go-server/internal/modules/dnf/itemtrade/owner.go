// Package itemtrade owns the durable, two-character inventory exchange used
// by the current client's town item-trade flow. Wire/session state remains in
// dnfbridge; this package validates offers and commits both inventories.
package itemtrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/itemquality"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrRepositoryUnavailable = errors.New("item trade repository is unavailable")
	ErrInvalidParticipants   = errors.New("item trade participants are invalid")
	ErrInvalidOffer          = errors.New("item trade offer is invalid")
	ErrItemUnavailable       = errors.New("item trade item is unavailable")
	ErrItemNotTradeable      = errors.New("item is not tradeable")
	ErrInventoryFull         = errors.New("item trade destination inventory is full")
	ErrGoldUnavailable       = errors.New("item trade gold is unavailable")
	ErrGoldOverflow          = errors.New("item trade gold would overflow")
	ErrReceiptInvalid        = errors.New("item trade completion receipt is invalid")
)

const (
	itemTradeReceiptVersion = int64(3)
	itemTradeReceiptPrefix  = "item_trade_completion_receipt_"
)

const (
	mainListType   byte = 0
	avatarListType byte = 1

	// currentItemRowSize is the fixed projected item row the current client
	// reads; only a row of exactly this size keeps the equipment quality seed
	// at +0x06.
	currentItemRowSize = 0x77

	equipmentFirst  int16 = 9
	equipmentLast   int16 = 64
	consumableFirst int16 = 65
	consumableLast  int16 = 120
	materialFirst   int16 = 121
	materialLast    int16 = 176
	questFirst      int16 = 177
	questLast       int16 = 232
	professionFirst int16 = 233
	professionLast  int16 = 288
	// 徽章页 289..351 是 9 行 × 7 格 = 63（与 dnfbridge 的 currentAvatarEmblemPageSlotStart/End
	// 同一口径）。这里原先是 289..344，把最后一行 345..351 划给了 special 页；那会让
	// 落在徽章页第 9 行的徽章交易不出去（destinationSlotRange 找不到所属页 → 直接拒收）。
	emblemFirst  int16 = 289
	emblemLast   int16 = 351
	specialFirst int16 = 352
	specialLast  int16 = 353

	avatarFirst int16 = 0
	avatarLast  int16 = 500
)

type Offer struct {
	TradeSlot        uint16
	SourceList       byte
	SourceSlot       int16
	Count            int64
	ExpectedItem     int64
	ExpectedInstance uint32
	ExpectedBind     bool
	AllowSealed      bool
}

type Participant struct {
	CharacterID string
	// AccountID is only required when one of this participant's offers sources
	// the account-owned crystal/soul warehouse (list-0 slots 354..365). That
	// storage is not part of the character inventory record, so the exchange
	// has to run inside the wider account/character transaction instead.
	AccountID string
	Offers    []Offer
	Gold      int64
}

type Transfer struct {
	FromCharacterID string
	ToCharacterID   string
	TradeSlot       uint16
	DestinationList byte
	DestinationSlot int16
	Stack           dnfrepo.ItemStack
}

type Result struct {
	Inventories map[string]dnfrepo.InventoryRecord
	Characters  map[string]dnfrepo.CharacterRecord
	Received    map[string][]Transfer
	Replayed    bool
}

type Owner struct {
	repositories dnfrepo.Group
	now          func() time.Time
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterTrade == nil {
		return nil, ErrRepositoryUnavailable
	}
	return &Owner{repositories: repositories, now: time.Now}, nil
}

func (o *Owner) Exchange(ctx context.Context, first Participant, second Participant) (Result, error) {
	// 玩家对玩家的在线交易：收金币交易手续费、并累计小额交易次数。
	return o.exchange(ctx, "", "", true, first, second)
}

// ExchangeWithCompletionKey performs the same atomic two-character exchange,
// but also records one durable receipt on receiptCharacterID. A caller may
// repeat the exact command after an indeterminate commit result; the second
// call returns the committed transfer without charging or moving the item a
// second time.
func (o *Owner) ExchangeWithCompletionKey(
	ctx context.Context,
	completionKey string,
	receiptCharacterID string,
	first Participant,
	second Participant,
) (Result, error) {
	completionKey = strings.TrimSpace(completionKey)
	receiptCharacterID = strings.TrimSpace(receiptCharacterID)
	if completionKey == "" || len(completionKey) > 256 || receiptCharacterID == "" {
		return Result{}, ErrReceiptInvalid
	}
	// 个人商店（摆摊）收购不是玩家交易，不收交易手续费、也不计小额交易次数。
	return o.exchange(ctx, completionKey, receiptCharacterID, false, first, second)
}

func (o *Owner) exchange(
	ctx context.Context,
	completionKey string,
	receiptCharacterID string,
	applyGoldTradeFee bool,
	first Participant,
	second Participant,
) (Result, error) {
	if o == nil || o.repositories.CharacterTrade == nil {
		return Result{}, ErrRepositoryUnavailable
	}
	first.CharacterID = strings.TrimSpace(first.CharacterID)
	second.CharacterID = strings.TrimSpace(second.CharacterID)
	if first.CharacterID == "" || second.CharacterID == "" || first.CharacterID == second.CharacterID {
		return Result{}, ErrInvalidParticipants
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	participants := []Participant{first, second}
	if first.Gold < 0 || second.Gold < 0 {
		return Result{}, ErrInvalidOffer
	}
	sort.Slice(participants, func(i, j int) bool {
		return participants[i].CharacterID < participants[j].CharacterID
	})
	if completionKey != "" && receiptCharacterID != participants[0].CharacterID && receiptCharacterID != participants[1].CharacterID {
		return Result{}, ErrReceiptInvalid
	}
	receiptKeyHash := sha256.Sum256([]byte(completionKey))
	receiptAuthorityHash := itemTradeAuthorityHash(participants)

	// An offer that sources the account-owned crystal/soul warehouse needs the
	// account inventory inside the same transaction as both characters. Only
	// then can the debit and the credit commit together.
	accountShared := false
	for _, participant := range participants {
		for _, offer := range participant.Offers {
			if dnfrepo.IsAccountSharedInventorySlot(offer.SourceList, offer.SourceSlot) {
				accountShared = true
			}
		}
	}
	accountIDs := map[string]string{}
	if accountShared {
		if o.repositories.AccountPairTrade == nil {
			return Result{}, ErrRepositoryUnavailable
		}
		// Each side keeps its own warehouse. A cross-account sale therefore
		// locks two account records plus both characters; a same-account one
		// resolves to a single shared record.
		for _, participant := range participants {
			id := strings.TrimSpace(participant.AccountID)
			if id == "" {
				return Result{}, ErrItemNotTradeable
			}
			accountIDs[participant.CharacterID] = id
		}
	}

	result := Result{}
	commit := func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		accountInventories dnfrepo.AccountInventoryRepository,
	) error {
		accountRecords := make(map[string]dnfrepo.AccountInventoryRecord, 2)
		if accountInventories != nil {
			for _, accountID := range accountIDs {
				if _, already := accountRecords[accountID]; already {
					continue
				}
				record, found, err := accountInventories.Load(ctx, accountID)
				if err != nil {
					return err
				}
				if !found {
					record = dnfrepo.AccountInventoryRecord{AccountID: accountID}
				}
				if record.Slots == nil {
					record.Slots = make(map[string]dnfrepo.ItemStack)
				}
				accountRecords[accountID] = record
			}
		}
		loaded := make(map[string]dnfrepo.InventoryRecord, 2)
		for _, participant := range participants {
			record, found, err := inventories.Load(ctx, participant.CharacterID)
			if err != nil {
				return err
			}
			if !found {
				record = dnfrepo.InventoryRecord{CharacterID: participant.CharacterID}
			}
			if record.Slots == nil {
				record.Slots = make(map[string]dnfrepo.ItemStack)
			}
			loaded[participant.CharacterID] = record
		}
		loadedCharacters := make(map[string]dnfrepo.CharacterRecord, 2)
		if participants[0].Gold > 0 || participants[1].Gold > 0 || completionKey != "" {
			for _, participant := range participants {
				record, found, err := characters.Load(ctx, participant.CharacterID)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("character %s: %w", participant.CharacterID, ErrGoldUnavailable)
				}
				if record.Stats == nil {
					record.Stats = make(map[string]int64)
				}
				loadedCharacters[participant.CharacterID] = record
			}
			if completionKey != "" {
				receiptOwner := loadedCharacters[receiptCharacterID]
				replayed, replayResult, err := replayItemTradeReceipt(
					receiptOwner,
					participants,
					loadedCharacters,
					loaded,
					receiptKeyHash,
					receiptAuthorityHash,
				)
				if err != nil {
					return err
				}
				if replayed {
					result = replayResult
					return nil
				}
			}
			// 金币结算：基础转移 + 交易手续费 + 小额交易次数累计。
			// 手续费必须和钱包变更落在同一笔事务里，所以先算出每个参与者
			// 的净变化量，再统一写回 loadedCharacters（结尾的
			// characters.Save 会把它和物品一起提交）。
			deltas := currentItemTradeGoldDeltas(participants, loadedCharacters, o.clock(), applyGoldTradeFee)
			for _, participant := range participants {
				record := loadedCharacters[participant.CharacterID]
				balance := record.Stats["gold"]
				delta := deltas[participant.CharacterID]
				if delta < 0 && balance < -delta {
					return fmt.Errorf("character %s balance=%d required=%d: %w", participant.CharacterID, balance, -delta, ErrGoldUnavailable)
				}
				if delta > 0 && balance > math.MaxInt64-delta {
					return fmt.Errorf("character %s balance=%d incoming=%d: %w", participant.CharacterID, balance, delta, ErrGoldOverflow)
				}
				record.Stats["gold"] = balance + delta
				loadedCharacters[participant.CharacterID] = record
			}
		}

		// Validation reads the same list-0 view the client was shown, which
		// overlays the account warehouse on top of the character record.
		viewFor := func(characterID string) dnfrepo.InventoryRecord {
			if accountInventories == nil {
				return loaded[characterID]
			}
			return dnfrepo.MergeAccountSharedInventory(loaded[characterID], accountRecords[accountIDs[characterID]])
		}
		staged := make(map[string][]stagedOffer, 2)
		for _, participant := range participants {
			rows, err := validateOffers(viewFor(participant.CharacterID), participant.Offers, o.clock())
			if err != nil {
				return fmt.Errorf("character %s: %w", participant.CharacterID, err)
			}
			staged[participant.CharacterID] = rows
		}

		// Remove every outgoing quantity before assigning incoming slots. This
		// lets a full inventory complete a one-for-one exchange without a
		// transient capacity failure.
		for _, participant := range participants {
			record := loaded[participant.CharacterID]
			for _, row := range staged[participant.CharacterID] {
				slots := record.Slots
				if row.accountShared {
					slots = accountRecords[accountIDs[participant.CharacterID]].Slots
				}
				stack := slots[row.sourceKey]
				if row.offer.Count == stack.Count {
					delete(slots, row.sourceKey)
				} else {
					stack.Count -= row.offer.Count
					slots[row.sourceKey] = stack
				}
			}
			loaded[participant.CharacterID] = record
		}

		received := make(map[string][]Transfer, 2)
		for index, participant := range participants {
			recipient := participants[1-index]
			record := loaded[recipient.CharacterID]
			for _, row := range staged[participant.CharacterID] {
				// A cube/soul family is pinned to one warehouse cell in the
				// current client, so an account-owned row can only be received
				// back into that same cell - stacked onto whatever is already
				// there rather than allocated a free bag slot.
				if row.accountShared {
					stack := cloneStack(row.stack)
					stack.Count = row.offer.Count
					destination := accountRecords[accountIDs[recipient.CharacterID]]
					if existing, present := destination.Slots[row.sourceKey]; present {
						if existing.ItemID != stack.ItemID {
							return fmt.Errorf("account warehouse %s: %w", row.sourceKey, ErrItemUnavailable)
						}
						if existing.Count > math.MaxInt64-stack.Count {
							return fmt.Errorf("account warehouse %s: %w", row.sourceKey, ErrItemUnavailable)
						}
						stack = cloneStack(existing)
						stack.Count = existing.Count + row.offer.Count
					}
					destination.Slots[row.sourceKey] = stack
					// Transfer.Stack describes what arrived, not the cell's new
					// total: the recipient's client prints its count as
					// "购买了[...] N个". The merged total stays reachable through
					// Result.Inventories.
					delivered := cloneStack(row.stack)
					delivered.Count = row.offer.Count
					received[recipient.CharacterID] = append(received[recipient.CharacterID], Transfer{
						FromCharacterID: participant.CharacterID,
						ToCharacterID:   recipient.CharacterID,
						TradeSlot:       row.offer.TradeSlot,
						DestinationList: row.offer.SourceList,
						DestinationSlot: row.offer.SourceSlot,
						Stack:           delivered,
					})
					continue
				}
				destinationSlot, err := allocateDestinationSlot(record.Slots, row.offer.SourceList, row.offer.SourceSlot)
				if err != nil {
					return fmt.Errorf("character %s receiving trade slot %d: %w", recipient.CharacterID, row.offer.TradeSlot, err)
				}
				stack := cloneStack(row.stack)
				stack.Count = row.offer.Count
				destinationKey := slotKey(row.offer.SourceList, destinationSlot)
				record.Slots[destinationKey] = stack
				received[recipient.CharacterID] = append(received[recipient.CharacterID], Transfer{
					FromCharacterID: participant.CharacterID,
					ToCharacterID:   recipient.CharacterID,
					TradeSlot:       row.offer.TradeSlot,
					DestinationList: row.offer.SourceList,
					DestinationSlot: destinationSlot,
					Stack:           cloneStack(stack),
				})
			}
			loaded[recipient.CharacterID] = record
		}

		for _, participant := range participants {
			record := loaded[participant.CharacterID]
			record.UpdatedAt = o.clock().UTC()
			if err := inventories.Save(ctx, record); err != nil {
				return err
			}
			loaded[participant.CharacterID] = record
		}
		if accountInventories != nil {
			for accountID, record := range accountRecords {
				record.UpdatedAt = o.clock().UTC()
				accountRecords[accountID] = record
				if err := accountInventories.Save(ctx, record); err != nil {
					return err
				}
			}
			// Callers project op14 rows straight out of Result.Inventories,
			// so the returned view has to carry the committed warehouse cells
			// as the client addresses them: ordinary list-0 slots.
			for _, participant := range participants {
				loaded[participant.CharacterID] = dnfrepo.MergeAccountSharedInventory(
					loaded[participant.CharacterID], accountRecords[accountIDs[participant.CharacterID]])
			}
		}
		if completionKey != "" {
			record := loadedCharacters[receiptCharacterID]
			writeItemTradeReceipt(
				&record,
				participants,
				received,
				receiptKeyHash,
				receiptAuthorityHash,
			)
			loadedCharacters[receiptCharacterID] = record
		}
		for _, participant := range participants {
			record, changed := loadedCharacters[participant.CharacterID]
			if !changed {
				continue
			}
			record.UpdatedAt = o.clock().UTC()
			if err := characters.Save(ctx, record); err != nil {
				return err
			}
			loadedCharacters[participant.CharacterID] = record
		}
		result = Result{Inventories: loaded, Characters: loadedCharacters, Received: received}
		return nil
	}

	var err error
	if accountShared {
		err = o.repositories.AccountPairTrade.WithinAccountPairCharacterTrade(
			ctx,
			accountIDs[participants[0].CharacterID],
			accountIDs[participants[1].CharacterID],
			participants[0].CharacterID,
			participants[1].CharacterID,
			func(
				accountInventories dnfrepo.AccountInventoryRepository,
				characters dnfrepo.CharacterRepository,
				inventories dnfrepo.InventoryRepository,
			) error {
				if accountInventories == nil {
					return ErrRepositoryUnavailable
				}
				return commit(characters, inventories, accountInventories)
			},
		)
	} else {
		err = o.repositories.CharacterTrade.WithinCharacterTrade(
			ctx,
			participants[0].CharacterID,
			participants[1].CharacterID,
			func(characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository) error {
				return commit(characters, inventories, nil)
			},
		)
	}
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// 金币交易手续费与小额交易次数。
//
// 规则（运营定义）：
//   - 金额 > 100万（含 100万零1）：不看次数，收钱方在到账前被扣 3%。
//   - 金额 ≤ 100万：小额金币交易。给钱方与收钱方**各自**按自己角色的当日累计
//     次数独立判定 —— 谁在本次交易之前已经 ≥101 次，谁就为这笔金币付 3%；
//     未超限的另一方不受对方状态牵连。
//   - 先判后加，所以每个角色每天（北京时间 06:00 换日）前 101 次小额交易免费。
//   - 手续费系统回收，不进入任何人的钱包。
//
// 次数与服务日一起存在角色 Stats 里（trade_small_count / trade_small_day），
// 由 exchange 结尾的 characters.Save 与钱包变更同事务落库。
// ---------------------------------------------------------------------------
const (
	currentItemTradeFeePercent           = int64(3)
	currentItemTradeLargeGoldCeiling     = int64(1_000_000)
	currentItemTradeSmallTradeDailyLimit = int64(101)
	currentItemTradeSmallTradeCountStat  = "trade_small_count"
	currentItemTradeSmallTradeDayStat    = "trade_small_day"
)

var currentItemTradeCalendar = time.FixedZone("Asia/Shanghai", 8*60*60)

// currentItemTradeServiceDay 把时刻折算成"北京时间 06:00 换日"的服务日序号
// （与 quest / dungeonadmission 的既有约定一致）。
func currentItemTradeServiceDay(now time.Time) int64 {
	local := now.In(currentItemTradeCalendar).Add(-6 * time.Hour)
	return int64(local.Year())*10000 + int64(local.Month())*100 + int64(local.Day())
}

// currentItemTradeFee 沿用拍卖行的取整约定：amount * percent / 100（向下取整）。
func currentItemTradeFee(amount int64) int64 {
	if amount <= 0 || currentItemTradeFeePercent <= 0 {
		return 0
	}
	if amount > math.MaxInt64/currentItemTradeFeePercent {
		return amount / 100 * currentItemTradeFeePercent
	}
	return amount * currentItemTradeFeePercent / 100
}

func currentItemTradeSmallTradeCount(record dnfrepo.CharacterRecord, day int64) int64 {
	if record.Stats == nil || record.Stats[currentItemTradeSmallTradeDayStat] != day {
		return 0
	}
	count := record.Stats[currentItemTradeSmallTradeCountStat]
	if count < 0 {
		return 0
	}
	return count
}

func currentItemTradeWithSmallTradeCount(record dnfrepo.CharacterRecord, day int64, count int64) dnfrepo.CharacterRecord {
	if record.Stats == nil {
		record.Stats = make(map[string]int64, 2)
	}
	record.Stats[currentItemTradeSmallTradeDayStat] = day
	record.Stats[currentItemTradeSmallTradeCountStat] = count
	return record
}

// currentItemTradeGoldDeltas 返回每个参与者钱包的净变化量（含手续费），并把新的
// 小额交易次数写回 characters。判定统一使用"本次交易之前"的计数快照，因此结果
// 与参与者的遍历顺序无关；同一角色在同一笔交易里既收又给时，计数按金币流各自累加。
//
// applyGoldTradeFee=false 时（个人商店收购等非玩家交易）退化为纯转移：不收手续费、
// 不写次数，与加入手续费之前的语义逐字一致。
func currentItemTradeGoldDeltas(
	participants []Participant,
	characters map[string]dnfrepo.CharacterRecord,
	now time.Time,
	applyGoldTradeFee bool,
) map[string]int64 {
	day := currentItemTradeServiceDay(now)
	deltas := make(map[string]int64, len(participants))
	before := make(map[string]int64, len(participants))
	smallFlows := make(map[string]int64, len(participants))
	for _, participant := range participants {
		deltas[participant.CharacterID] = -participant.Gold
		before[participant.CharacterID] = currentItemTradeSmallTradeCount(characters[participant.CharacterID], day)
	}
	for index, payer := range participants {
		recipient := participants[1-index]
		amount := payer.Gold
		if amount <= 0 {
			continue
		}
		deltas[recipient.CharacterID] += amount
		if !applyGoldTradeFee {
			continue
		}
		if amount > currentItemTradeLargeGoldCeiling {
			// 大额：收钱方在到账前被扣 3%。
			deltas[recipient.CharacterID] -= currentItemTradeFee(amount)
			continue
		}
		// 小额：双方各自独立判定，谁超限谁付。
		for _, side := range []string{payer.CharacterID, recipient.CharacterID} {
			if before[side] >= currentItemTradeSmallTradeDailyLimit {
				deltas[side] -= currentItemTradeFee(amount)
			}
			smallFlows[side]++
		}
	}
	for _, participant := range participants {
		if smallFlows[participant.CharacterID] == 0 {
			continue
		}
		characters[participant.CharacterID] = currentItemTradeWithSmallTradeCount(
			characters[participant.CharacterID],
			day,
			before[participant.CharacterID]+smallFlows[participant.CharacterID],
		)
	}
	return deltas
}

func itemTradeAuthorityHash(participants []Participant) [32]byte {
	var canonical bytes.Buffer
	for _, participant := range participants {
		_ = binary.Write(&canonical, binary.LittleEndian, uint32(len(participant.CharacterID)))
		canonical.WriteString(participant.CharacterID)
		_ = binary.Write(&canonical, binary.LittleEndian, uint64(participant.Gold))
		offers := append([]Offer(nil), participant.Offers...)
		sort.Slice(offers, func(i, j int) bool {
			if offers[i].TradeSlot != offers[j].TradeSlot {
				return offers[i].TradeSlot < offers[j].TradeSlot
			}
			if offers[i].SourceList != offers[j].SourceList {
				return offers[i].SourceList < offers[j].SourceList
			}
			return offers[i].SourceSlot < offers[j].SourceSlot
		})
		_ = binary.Write(&canonical, binary.LittleEndian, uint32(len(offers)))
		for _, offer := range offers {
			_ = binary.Write(&canonical, binary.LittleEndian, offer.TradeSlot)
			canonical.WriteByte(offer.SourceList)
			_ = binary.Write(&canonical, binary.LittleEndian, offer.SourceSlot)
			_ = binary.Write(&canonical, binary.LittleEndian, uint64(offer.Count))
			_ = binary.Write(&canonical, binary.LittleEndian, uint64(offer.ExpectedItem))
			_ = binary.Write(&canonical, binary.LittleEndian, offer.ExpectedInstance)
			canonical.WriteByte(boolByte(offer.ExpectedBind))
			canonical.WriteByte(boolByte(offer.AllowSealed))
		}
	}
	return sha256.Sum256(canonical.Bytes())
}

func itemTradeReceiptHashMatches(stats map[string]int64, suffix string, want [32]byte) bool {
	for index := 0; index < 4; index++ {
		key := fmt.Sprintf("%s%s_%d", itemTradeReceiptPrefix, suffix, index)
		if uint64(stats[key]) != binary.LittleEndian.Uint64(want[index*8:(index+1)*8]) {
			return false
		}
	}
	return true
}

func writeItemTradeReceiptHash(stats map[string]int64, suffix string, value [32]byte) {
	for index := 0; index < 4; index++ {
		stats[fmt.Sprintf("%s%s_%d", itemTradeReceiptPrefix, suffix, index)] =
			int64(binary.LittleEndian.Uint64(value[index*8 : (index+1)*8]))
	}
}

func writeItemTradeReceipt(
	record *dnfrepo.CharacterRecord,
	participants []Participant,
	received map[string][]Transfer,
	keyHash [32]byte,
	authorityHash [32]byte,
) {
	if record.Stats == nil {
		record.Stats = make(map[string]int64)
	}
	stats := record.Stats
	stats[itemTradeReceiptPrefix+"version"] = itemTradeReceiptVersion
	writeItemTradeReceiptHash(stats, "key", keyHash)
	writeItemTradeReceiptHash(stats, "authority", authorityHash)
	transfers := make([]Transfer, 0)
	for _, participant := range participants {
		transfers = append(transfers, received[participant.CharacterID]...)
	}
	sort.Slice(transfers, func(i, j int) bool {
		if transfers[i].ToCharacterID != transfers[j].ToCharacterID {
			return transfers[i].ToCharacterID < transfers[j].ToCharacterID
		}
		return transfers[i].TradeSlot < transfers[j].TradeSlot
	})
	stats[itemTradeReceiptPrefix+"transfer_count"] = int64(len(transfers))
	for index, transfer := range transfers {
		prefix := fmt.Sprintf("%stransfer_%d_", itemTradeReceiptPrefix, index)
		fromIndex, toIndex := -1, -1
		for participantIndex, participant := range participants {
			if participant.CharacterID == transfer.FromCharacterID {
				fromIndex = participantIndex
			}
			if participant.CharacterID == transfer.ToCharacterID {
				toIndex = participantIndex
			}
		}
		stats[prefix+"from"] = int64(fromIndex)
		stats[prefix+"to"] = int64(toIndex)
		stats[prefix+"trade_slot"] = int64(transfer.TradeSlot)
		stats[prefix+"destination_list"] = int64(transfer.DestinationList)
		stats[prefix+"destination_slot"] = int64(transfer.DestinationSlot)
		stats[prefix+"item_id"] = transfer.Stack.ItemID
		stats[prefix+"count"] = transfer.Stack.Count
	}
}

func replayItemTradeReceipt(
	receiptOwner dnfrepo.CharacterRecord,
	participants []Participant,
	characters map[string]dnfrepo.CharacterRecord,
	inventories map[string]dnfrepo.InventoryRecord,
	keyHash [32]byte,
	authorityHash [32]byte,
) (bool, Result, error) {
	stats := receiptOwner.Stats
	if stats == nil || !itemTradeReceiptHashMatches(stats, "key", keyHash) {
		return false, Result{}, nil
	}
	if stats[itemTradeReceiptPrefix+"version"] != itemTradeReceiptVersion ||
		!itemTradeReceiptHashMatches(stats, "authority", authorityHash) {
		return false, Result{}, ErrReceiptInvalid
	}
	count := stats[itemTradeReceiptPrefix+"transfer_count"]
	if count < 0 || count > 64 {
		return false, Result{}, ErrReceiptInvalid
	}
	received := make(map[string][]Transfer, 2)
	for index := 0; index < int(count); index++ {
		prefix := fmt.Sprintf("%stransfer_%d_", itemTradeReceiptPrefix, index)
		fromIndex := stats[prefix+"from"]
		toIndex := stats[prefix+"to"]
		if fromIndex < 0 || fromIndex >= int64(len(participants)) || toIndex < 0 || toIndex >= int64(len(participants)) || fromIndex == toIndex {
			return false, Result{}, ErrReceiptInvalid
		}
		destinationList := byte(stats[prefix+"destination_list"])
		destinationSlot := int16(stats[prefix+"destination_slot"])
		itemID := stats[prefix+"item_id"]
		itemCount := stats[prefix+"count"]
		toCharacterID := participants[toIndex].CharacterID
		stack, ok := inventories[toCharacterID].Slots[slotKey(destinationList, destinationSlot)]
		if !ok || itemID <= 0 || itemCount <= 0 || stack.ItemID != itemID || stack.Count < itemCount {
			return false, Result{}, ErrReceiptInvalid
		}
		stack = cloneStack(stack)
		stack.Count = itemCount
		received[toCharacterID] = append(received[toCharacterID], Transfer{
			FromCharacterID: participants[fromIndex].CharacterID,
			ToCharacterID:   toCharacterID,
			TradeSlot:       uint16(stats[prefix+"trade_slot"]),
			DestinationList: destinationList,
			DestinationSlot: destinationSlot,
			Stack:           stack,
		})
	}
	return true, Result{Inventories: inventories, Characters: characters, Received: received, Replayed: true}, nil
}

type stagedOffer struct {
	offer         Offer
	sourceKey     string
	stack         dnfrepo.ItemStack
	accountShared bool
}

func validateOffers(record dnfrepo.InventoryRecord, offers []Offer, now time.Time) ([]stagedOffer, error) {
	rows := make([]stagedOffer, 0, len(offers))
	tradeSlots := make(map[uint16]struct{}, len(offers))
	sourceCounts := make(map[string]int64, len(offers))
	ordered := append([]Offer(nil), offers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TradeSlot < ordered[j].TradeSlot })
	for _, offer := range ordered {
		if _, duplicate := tradeSlots[offer.TradeSlot]; duplicate || offer.Count <= 0 || !supportedSourceList(offer.SourceList) || offer.SourceSlot < 0 {
			return nil, ErrInvalidOffer
		}
		// The crystal/soul warehouse is a legitimate source: the current client
		// offers those cells as its own container (list 36) in the private-store
		// and trade windows. They stay account-owned on both sides, so the row is
		// marked here and committed against the account record instead of the
		// character inventory.
		accountShared := dnfrepo.IsAccountSharedInventorySlot(offer.SourceList, offer.SourceSlot)
		tradeSlots[offer.TradeSlot] = struct{}{}
		key := slotKey(offer.SourceList, offer.SourceSlot)
		stack, found := record.Slots[key]
		if !found || stack.ItemID <= 0 || stack.Count <= 0 || (offer.ExpectedItem > 0 && stack.ItemID != offer.ExpectedItem) {
			return nil, ErrItemUnavailable
		}
		if offer.ExpectedInstance != 0 && itemTradeEquipmentInstance(stack) != offer.ExpectedInstance {
			return nil, ErrItemUnavailable
		}
		if stack.Bind != offer.ExpectedBind {
			return nil, ErrItemUnavailable
		}
		if (stack.Bind && !offer.AllowSealed) || (!stack.ExpireAt.IsZero() && !stack.ExpireAt.After(now)) || isEquipmentLocked(stack) {
			return nil, ErrItemNotTradeable
		}
		sourceCounts[key] += offer.Count
		if sourceCounts[key] > stack.Count {
			return nil, ErrItemUnavailable
		}
		rows = append(rows, stagedOffer{offer: offer, sourceKey: key, stack: cloneStack(stack), accountShared: accountShared})
	}
	return rows, nil
}

// isEquipmentLocked mirrors the lock-state contract written by the itemlock
// owner (Alt lock, equipment_lock_state): both the active lock and a pending
// unlock keep the item untradeable, matching the client's own panel rule.
func isEquipmentLocked(stack dnfrepo.ItemStack) bool {
	if stack.Extra == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Extra["equipment_lock_state"])) {
	case "1", "2", "active", "locked", "unlocking", "pending_unlock":
		return true
	default:
		return false
	}
}

// itemTradeEquipmentInstance mirrors the durable identity used by the current
// inventory projection. New equipment stores its quality seed in Extra even
// when a legacy/current 119-byte raw row is unavailable; older imported rows
// may only have raw+0x06. Extra is authoritative when both are present because
// it is also what the client sees and returns in its trade/store request.
//
// itemTradeEquipmentInstance must return the same dword the list projection put
// at +0x06 of the row the client was shown, because that is the value the
// client replays as ExpectedInstance. A full 0x77 equipment row carries the
// quality seed there; a legacy 43-byte row (or none at all) carries something
// else, and the projection then falls back to the stack count. Reading
// RawEntry[6:10] unconditionally answered 0 for those rows, so buying a weapon
// out of a private store failed with "item trade item is unavailable" and the
// client showed 不存在的物品.
func itemTradeEquipmentInstance(stack dnfrepo.ItemStack) uint32 {
	if value, err := strconv.ParseUint(strings.TrimSpace(stack.Extra["quality_seed"]), 10, 32); err == nil {
		seed := uint32(value)
		if itemquality.Valid(seed) {
			return seed
		}
	}
	if len(stack.RawEntry) == currentItemRowSize && stack.ItemID > 0 &&
		binary.LittleEndian.Uint32(stack.RawEntry[2:6]) == uint32(stack.ItemID) &&
		strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "equipment") {
		if seed := binary.LittleEndian.Uint32(stack.RawEntry[6:10]); itemquality.Valid(seed) {
			return seed
		}
	}
	if stack.Count > 0 && stack.Count <= math.MaxUint32 {
		return uint32(stack.Count)
	}
	return 0
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func allocateDestinationSlot(slots map[string]dnfrepo.ItemStack, listType byte, preferred int16) (int16, error) {
	if !supportedSourceList(listType) {
		return 0, ErrInvalidOffer
	}
	start, end, ok := destinationSlotRange(listType, preferred)
	if !ok {
		return 0, ErrInvalidOffer
	}
	for slot := start; slot <= end; slot++ {
		if _, occupied := slots[slotKey(listType, slot)]; !occupied {
			return slot, nil
		}
	}
	return 0, ErrInventoryFull
}

// destinationSlotRange keeps an incoming item in the same current-client bag
// page as its source. The source index is not a portable destination: copying
// a sender's expanded high slot into a recipient with only the base capacity
// persists the item but leaves it outside the recipient's visible cells.
func destinationSlotRange(listType byte, sourceSlot int16) (int16, int16, bool) {
	if listType == avatarListType {
		if sourceSlot < avatarFirst || sourceSlot > avatarLast {
			return 0, 0, false
		}
		return avatarFirst, avatarLast, true
	}
	for _, page := range [...]struct{ first, last int16 }{
		{equipmentFirst, equipmentLast},
		{consumableFirst, consumableLast},
		{materialFirst, materialLast},
		{questFirst, questLast},
		{professionFirst, professionLast},
		{emblemFirst, emblemLast},
		{specialFirst, specialLast},
	} {
		if sourceSlot >= page.first && sourceSlot <= page.last {
			return page.first, page.last, true
		}
	}
	return 0, 0, false
}

func supportedSourceList(listType byte) bool {
	return listType == mainListType || listType == avatarListType
}

func validDestinationSlot(listType byte, slot int16) bool {
	_, _, ok := destinationSlotRange(listType, slot)
	return ok
}

func slotKey(listType byte, slot int16) string {
	return strconv.Itoa(int(listType)) + ":" + strconv.FormatInt(int64(slot), 10)
}

func cloneStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if stack.Extra != nil {
		extra := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			extra[key] = value
		}
		stack.Extra = extra
	}
	return stack
}

func (o *Owner) clock() time.Time {
	if o != nil && o.now != nil {
		return o.now()
	}
	return time.Now()
}
