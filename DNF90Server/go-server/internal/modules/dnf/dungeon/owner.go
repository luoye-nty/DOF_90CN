package dungeon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/carrygold"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

const (
	LuckyStarMetadataKey                  = "rental_lucky_stars"
	cardRewardReceiptPrefix               = "dungeon_card_reward_receipt_"
	cardRewardReceiptVersion        int64 = 1
	cardRewardReceiptOverflowMarker       = "durable-card-reward-receipt"
)

var (
	ErrOwnerUnavailable         = errors.New("dungeon asset owner is unavailable")
	ErrCharacterNotFound        = errors.New("dungeon character is not found")
	ErrAccountNotFound          = errors.New("dungeon account is not found")
	ErrRewardInvalid            = errors.New("dungeon reward is invalid")
	ErrInventoryFull            = errors.New("dungeon reward inventory is full")
	ErrGoldOverflow             = errors.New("dungeon gold would overflow")
	ErrStackProjectorRequired   = errors.New("dungeon expiring stack projector is required")
	ErrPremiumDailyLimit        = errors.New("dungeon premium service daily limit reached")
	ErrCardRewardReceiptInvalid = errors.New("dungeon card reward receipt is invalid")
	// ErrCardRewardGoldShort refuses a paid-card flip the character cannot pay
	// for. 86JP's TryBuildRewardPlan rejects the plan before any mutation when
	// the commission exceeds the carried gold, so the card is never revealed.
	ErrCardRewardGoldShort = errors.New("dungeon card reward gold is insufficient")
)

// StackProjector lets the transport compatibility layer project current-client
// raw-entry details while the dungeon owner retains the durable transaction and
// inventory rules.
type StackProjector func(dnfrepo.ItemStack, time.Time) (dnfrepo.ItemStack, error)

type CardItemReward struct {
	Stack      dnfrepo.ItemStack
	Stackable  bool
	StackLimit int64
	SlotStart  int16
	SlotEnd    int16
	ExpireAt   time.Time
}

type CardRewardBundle struct {
	Gold  int64
	Items []CardItemReward
}

type CardRewardCommand struct {
	CharacterID string
	// CompletionKey is optional. When present, the owner writes a compact
	// receipt into Character.Stats in the same asset/mailbox transaction and
	// replays it after an indeterminate commit outcome. Empty preserves the
	// legacy one-shot caller-owned behavior.
	CompletionKey string
	MainSlots     uint16
	// Cost is the gold spent before the bundle is credited. Only the paid card
	// row sets it: the amount is the commission op35 printed for that card.
	Cost                int64
	Bundle              CardRewardBundle
	UpdatedAt           time.Time
	Project             StackProjector
	ConsumePremiumDaily bool
	PremiumDailySlot    int64
}

type CardRewardResult struct {
	GoldBefore int64
	GoldAfter  int64
	// GoldSpent is the commission deducted before the bundle was credited.
	GoldSpent int64
	// GoldDiscarded is the part of the reward the carry cap refused.
	GoldDiscarded int64
	ItemSlots     []int16
	// OverflowMailID is set only when one or more card items could not fit in
	// the real main inventory and were committed as a system-mail attachment.
	OverflowMailID string
	Replayed       bool
}

type GoldPickupCommand struct {
	CharacterID string
	Amount      uint32
	UpdatedAt   time.Time
}

type GoldPickupResult struct {
	GoldBefore int64
	GoldAfter  int64
	// GoldDiscarded is the part of the pickup the carry cap refused.
	GoldDiscarded int64
}

// Owner is the durable mutation boundary for dungeon rewards. Packet parsing,
// runtime room state, packet ordering, and current-client raw projection stay
// in dnfbridge.
type Owner struct {
	assets        dnfrepo.CharacterAssetUnitOfWork
	accounts      dnfrepo.AccountRepository
	settlements   dnfrepo.CharacterSettlementUnitOfWork
	items         dnfrepo.CharacterItemUnitOfWork
	inventory     dnfrepo.InventoryRepository
	characters    dnfrepo.CharacterRepository
	mailboxAssets dnfrepo.MailboxAssetUnitOfWork
	accountAssets dnfrepo.AccountCharacterAssetUnitOfWork
}

func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.CharacterAssets == nil && repos.Account == nil && repos.CharacterSettlement == nil &&
		repos.CharacterItems == nil && repos.Inventory == nil && repos.Character == nil &&
		repos.AccountAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{
		assets:        repos.CharacterAssets,
		accounts:      repos.Account,
		settlements:   repos.CharacterSettlement,
		items:         repos.CharacterItems,
		inventory:     repos.Inventory,
		characters:    repos.Character,
		mailboxAssets: repos.MailboxAssets,
		accountAssets: repos.AccountAssets,
	}, nil
}

func (o *Owner) GrantCardReward(ctx context.Context, cmd CardRewardCommand) (CardRewardResult, error) {
	if o == nil || o.assets == nil || strings.TrimSpace(cmd.CharacterID) == "" {
		return CardRewardResult{}, ErrOwnerUnavailable
	}
	if err := validateCardReward(cmd); err != nil {
		return CardRewardResult{}, err
	}
	ctx = contextOrBackground(ctx)
	now := updatedAtOrNow(cmd.UpdatedAt)
	completionKey := strings.TrimSpace(cmd.CompletionKey)
	var receiptHash [sha256.Size]byte
	durableReceipt := completionKey != ""
	if durableReceipt {
		receiptHash = sha256.Sum256([]byte(completionKey))
	}

	var result CardRewardResult
	apply := func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
		accountInventories dnfrepo.AccountInventoryRepository,
	) error {
		character, found, err := characters.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterNotFound
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		if durableReceipt && cardRewardReceiptMatches(character.Stats, receiptHash) {
			replayed, receiptErr := cardRewardReceiptResult(character.Stats)
			if receiptErr != nil {
				return receiptErr
			}
			result = replayed
			result.Replayed = true
			return nil
		}
		if cmd.ConsumePremiumDaily && !premium.TryConsumeDaily(&character, cmd.PremiumDailySlot, now) {
			return ErrPremiumDailyLimit
		}
		currentGold := character.Stats["gold"]
		result.GoldBefore = currentGold
		if cmd.Cost < 0 {
			return ErrRewardInvalid
		}
		if cmd.Cost > 0 {
			if currentGold < cmd.Cost {
				return ErrCardRewardGoldShort
			}
			currentGold -= cmd.Cost
			character.Stats["gold"] = currentGold
			result.GoldSpent = cmd.Cost
		}
		if cmd.Bundle.Gold > 0 && currentGold > math.MaxInt64-cmd.Bundle.Gold {
			return ErrGoldOverflow
		}
		// The carry cap the character has bought bites here: a reward cannot be
		// refused without losing the whole bundle, so the excess is discarded
		// instead. carrygold.Credit never lowers a balance that is already
		// above the cap.
		result.GoldAfter, result.GoldDiscarded = carrygold.Credit(character, cmd.Bundle.Gold)
		character.Stats["gold"] = result.GoldAfter

		inventory, found, err := inventories.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			inventory = dnfrepo.InventoryRecord{CharacterID: cmd.CharacterID}
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		// 无色小晶块 / 灵魂 这两族通关奖励必须先落进账号共享仓库的固定格
		// （晶体仓库 354..359 + 灵魂仓库 360..365，权威表见
		// repository/account_shared_materials.go）。翻牌这条路过去直接 addCardItem
		// 写角色背包 —— 与 2026-09-21 NPC 商店金币购买落错槽是同一类漏点。
		accountInventory := dnfrepo.AccountInventoryRecord{}
		accountInventoryLoaded := false
		// accountInventoryReady 才代表"账号记录真的读到了"：读不到（没有账号仓库仓库、
		// 或角色记录里没有 AccountID）就必须整条退回角色背包那条既有路径，
		// 绝不能拿一个空 AccountID 去写账号记录。
		accountInventoryReady := false
		accountDirty := false
		loadAccountInventory := func() error {
			if accountInventoryLoaded || accountInventories == nil {
				return nil
			}
			accountInventoryLoaded = true
			accountID := strings.TrimSpace(character.AccountID)
			if accountID == "" {
				return nil
			}
			record, _, loadErr := accountInventories.Load(ctx, accountID)
			if loadErr != nil {
				return loadErr
			}
			accountInventory = dnfrepo.CloneAccountInventory(record)
			accountInventory.AccountID = accountID
			if accountInventory.Slots == nil {
				accountInventory.Slots = make(map[string]dnfrepo.ItemStack)
			}
			accountInventoryReady = true
			return nil
		}
		overflow := make([]dnfrepo.MailAttachment, 0)
		for _, reward := range cmd.Bundle.Items {
			pending := reward
			if accountInventories != nil {
				if err := loadAccountInventory(); err != nil {
					return err
				}
			}
			if accountInventoryReady {
				// 账号记录可用：无色/灵魂类奖励先落它的固定格，余量才交给角色背包。
				_, remainder, accountChanged := grantCardRewardAccountShared(&accountInventory, reward)
				if accountChanged {
					accountDirty = true
					if fixedSlot, member := dnfrepo.AccountSharedWarehouseSlotForItem(reward.Stack.ItemID); member {
						result.ItemSlots = append(result.ItemSlots, fixedSlot)
					}
				}
				if remainder <= 0 {
					continue
				}
				if accountChanged {
					// 只剩余量要落角色背包：数量换成余量，行本身沿用奖励的投影。
					pending.Stack = cloneStack(reward.Stack)
					pending.Stack.Count = remainder
				}
			}
			slot, err := addCardItem(&inventory, cmd.MainSlots, pending, cmd.Project)
			if err != nil {
				if !errors.Is(err, ErrInventoryFull) || mailboxes == nil {
					return err
				}
				attachment, attachmentErr := cardRewardMailAttachment(pending, cmd.Project)
				if attachmentErr != nil {
					return attachmentErr
				}
				overflow = append(overflow, attachment)
				result.ItemSlots = append(result.ItemSlots, -1)
				continue
			}
			result.ItemSlots = append(result.ItemSlots, slot)
		}

		if durableReceipt {
			writeCardRewardReceipt(character.Stats, receiptHash, result, len(overflow) != 0)
		}
		character.UpdatedAt = now
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if accountDirty {
			accountInventory.UpdatedAt = now
			if err := accountInventories.Save(ctx, accountInventory); err != nil {
				return err
			}
		}
		if len(overflow) == 0 {
			return nil
		}
		mailID, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
			RecipientCharacterID: cmd.CharacterID,
			Title:                "背包已满：通关奖励",
			Body:                 "背包空间不足，通关奖励已通过邮件发送。请清理对应道具分页后领取。",
			Source:               "dungeon_card_reward_inventory_full",
			Attachments:          overflow,
			CreatedAt:            now,
		})
		if mailErr != nil {
			return mailErr
		}
		result.OverflowMailID = mailID
		return nil
	}
	var err error
	if o.mailboxAssets != nil {
		err = o.mailboxAssets.WithinMailboxAssets(ctx, cmd.CharacterID, cmd.CharacterID, func(
			_ dnfrepo.AccountRepository,
			accountInventories dnfrepo.AccountInventoryRepository,
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			return apply(characters, inventories, mailboxes, accountInventories)
		})
	} else {
		// 这条兜底事务拿不到账号仓库记录，无色/灵魂类奖励只能照旧落角色背包。
		// 生产装配（memory / mysql）都提供 MailboxAssets，所以走的是上面那条。
		err = o.assets.WithinCharacterAssets(ctx, cmd.CharacterID, func(
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
		) error {
			return apply(characters, inventories, nil, nil)
		})
	}
	if err != nil {
		return CardRewardResult{}, err
	}
	return result, nil
}

func cardRewardMailAttachment(reward CardItemReward, project StackProjector) (dnfrepo.MailAttachment, error) {
	stack := cloneStack(reward.Stack)
	if reward.Stackable && !reward.ExpireAt.IsZero() {
		if project == nil {
			return dnfrepo.MailAttachment{}, ErrStackProjectorRequired
		}
		var err error
		stack, err = project(stack, reward.ExpireAt)
		if err != nil {
			return dnfrepo.MailAttachment{}, err
		}
	}
	return dnfrepo.MailAttachment{
		ItemID:   stack.ItemID,
		Count:    stack.Count,
		Bind:     stack.Bind,
		ExpireAt: stack.ExpireAt,
		RawEntry: append([]byte(nil), stack.RawEntry...),
		Extra:    cloneCardRewardExtra(stack.Extra),
	}, nil
}

func cloneCardRewardExtra(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (o *Owner) GrantPickupGold(ctx context.Context, cmd GoldPickupCommand) (GoldPickupResult, error) {
	if o == nil || o.assets == nil || strings.TrimSpace(cmd.CharacterID) == "" {
		return GoldPickupResult{}, ErrOwnerUnavailable
	}
	if cmd.Amount == 0 {
		return GoldPickupResult{}, ErrRewardInvalid
	}
	ctx = contextOrBackground(ctx)
	now := updatedAtOrNow(cmd.UpdatedAt)

	var result GoldPickupResult
	err := dnfrepo.WithinCharacterAssetsScoped(ctx, o.assets, cmd.CharacterID, func(
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterNotFound
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		result.GoldBefore = character.Stats["gold"]
		delta := int64(cmd.Amount)
		if result.GoldBefore < 0 || result.GoldBefore > math.MaxInt64-delta {
			return ErrGoldOverflow
		}
		// A gold drop the player walks over cannot be handed back, so the carry
		// cap discards what does not fit instead of refusing the pickup.
		result.GoldAfter, result.GoldDiscarded = carrygold.Credit(character, delta)
		character.Stats["gold"] = result.GoldAfter
		character.UpdatedAt = now
		return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
	})
	if err != nil {
		return GoldPickupResult{}, err
	}
	return result, nil
}

func validateCardReward(cmd CardRewardCommand) error {
	if cmd.Bundle.Gold < 0 ||
		cmd.ConsumePremiumDaily && (cmd.PremiumDailySlot < 0 || cmd.PremiumDailySlot >= premium.DevilSlotCount) ||
		strings.TrimSpace(cmd.CompletionKey) != "" && len(cmd.Bundle.Items) > math.MaxUint16 {
		return ErrRewardInvalid
	}
	for _, reward := range cmd.Bundle.Items {
		if reward.Stack.ItemID <= 0 || reward.Stack.Count <= 0 ||
			!reward.Stackable && reward.Stack.Count != 1 {
			return ErrRewardInvalid
		}
		hasPVFRange := reward.SlotStart >= 3 && reward.SlotEnd >= reward.SlotStart
		if !hasPVFRange && cmd.MainSlots == 0 {
			return ErrRewardInvalid
		}
		if reward.Stackable && !reward.ExpireAt.IsZero() && cmd.Project == nil {
			return ErrStackProjectorRequired
		}
		if reward.StackLimit < 0 || (reward.Stackable && reward.StackLimit > 0 && reward.Stack.Count > reward.StackLimit) {
			return ErrRewardInvalid
		}
	}
	return nil
}

func cardRewardReceiptMatches(stats map[string]int64, want [sha256.Size]byte) bool {
	if stats == nil || stats[cardRewardReceiptPrefix+"version"] != cardRewardReceiptVersion {
		return false
	}
	for index := 0; index < 4; index++ {
		if uint64(stats[fmt.Sprintf("%shash_%d", cardRewardReceiptPrefix, index)]) !=
			binary.LittleEndian.Uint64(want[index*8:(index+1)*8]) {
			return false
		}
	}
	return true
}

func writeCardRewardReceipt(
	stats map[string]int64,
	hash [sha256.Size]byte,
	result CardRewardResult,
	overflow bool,
) {
	stats[cardRewardReceiptPrefix+"version"] = cardRewardReceiptVersion
	for index := 0; index < 4; index++ {
		stats[fmt.Sprintf("%shash_%d", cardRewardReceiptPrefix, index)] =
			int64(binary.LittleEndian.Uint64(hash[index*8 : (index+1)*8]))
	}
	stats[cardRewardReceiptPrefix+"gold_before"] = result.GoldBefore
	stats[cardRewardReceiptPrefix+"gold_after"] = result.GoldAfter
	stats[cardRewardReceiptPrefix+"gold_spent"] = result.GoldSpent
	stats[cardRewardReceiptPrefix+"item_slot_count"] = int64(len(result.ItemSlots))
	for index, slot := range result.ItemSlots {
		stats[fmt.Sprintf("%sitem_slot_%d", cardRewardReceiptPrefix, index)] = int64(slot)
	}
	stats[cardRewardReceiptPrefix+"overflow"] = 0
	if overflow {
		stats[cardRewardReceiptPrefix+"overflow"] = 1
	}
}

func cardRewardReceiptResult(stats map[string]int64) (CardRewardResult, error) {
	goldBefore, beforeFound := stats[cardRewardReceiptPrefix+"gold_before"]
	goldAfter, afterFound := stats[cardRewardReceiptPrefix+"gold_after"]
	count, countFound := stats[cardRewardReceiptPrefix+"item_slot_count"]
	overflow, overflowFound := stats[cardRewardReceiptPrefix+"overflow"]
	// Receipts written before the paid-card commission existed carry no
	// gold_spent key; they describe a flip that spent nothing, so a missing key
	// reads as zero instead of invalidating an already-committed transaction.
	goldSpent, spentFound := stats[cardRewardReceiptPrefix+"gold_spent"]
	if !spentFound {
		goldSpent = 0
	}
	if !beforeFound || !afterFound || !countFound || !overflowFound ||
		goldSpent < 0 || goldSpent > math.MaxInt64/2 || goldAfter < goldBefore-goldSpent ||
		count < 0 || count > math.MaxUint16 || (overflow != 0 && overflow != 1) {
		return CardRewardResult{}, ErrCardRewardReceiptInvalid
	}
	result := CardRewardResult{
		GoldBefore: goldBefore,
		GoldAfter:  goldAfter,
		GoldSpent:  goldSpent,
		ItemSlots:  make([]int16, 0, int(count)),
	}
	for index := 0; index < int(count); index++ {
		value, found := stats[fmt.Sprintf("%sitem_slot_%d", cardRewardReceiptPrefix, index)]
		if !found || value < -1 || value > math.MaxInt16 {
			return CardRewardResult{}, ErrCardRewardReceiptInvalid
		}
		result.ItemSlots = append(result.ItemSlots, int16(value))
	}
	if overflow == 1 {
		result.OverflowMailID = cardRewardReceiptOverflowMarker
	}
	return result, nil
}

// grantCardRewardAccountShared 把一件通关奖励并进账号共享仓库的固定格。
//
// 六个晶块(354..359)与六个灵魂(360..365)在同一个账号里只有那一个家（权威表见
// repository/account_shared_materials.go）。翻牌这条路过去直接把这些奖励写进角色
// 背包 —— 2026-09-21 的同类漏点（NPC 商店金币购买）已经证明"每条路径自己判一次"
// 迟早会漏，所以这里不再自己判，交给唯一入库原语。
//
// 顺序上有个讲究：**先用奖励自带的那一行占住固定格**（保留 bridge 投影好的
// RawEntry / 绑定 / 到期，原语不认识线格式），再把数量交给原语加 —— 额度与并数归原语，
// 行本身仍是投影出来的那一条。数量变了以后由包内的 updateDungeonRewardRawAmount
// 补盖 0x77 行里的数量字节。
//
// 返回 (放进账号仓的数量, 需要交给角色背包的余量, 账号记录是否被改动)。
// 该道具不属于账号共享仓、或账号记录不可用时，原样退回给角色背包那条既有路径。
func grantCardRewardAccountShared(accountInventory *dnfrepo.AccountInventoryRecord, reward CardItemReward) (int64, int64, bool) {
	if accountInventory == nil || !reward.Stackable || reward.Stack.ItemID <= 0 || reward.Stack.Count <= 0 {
		return 0, reward.Stack.Count, false
	}
	fixedSlot, member := dnfrepo.AccountSharedWarehouseSlotForItem(reward.Stack.ItemID)
	if !member {
		return 0, reward.Stack.Count, false
	}
	key := dnfrepo.AccountSharedInventorySlotKey(fixedSlot)
	seeded := false
	if existing, exists := accountInventory.Slots[key]; !exists || existing.ItemID <= 0 {
		seededStack := cloneStack(reward.Stack)
		// 数量归零：全部交给原语去加，免得这里和原语各加一次。
		seededStack.Count = 0
		accountInventory.Slots[key] = seededStack
		seeded = true
	}
	grant := dnfrepo.GrantAccountSharedWarehouseStack(
		accountInventory,
		reward.Stack.ItemID,
		reward.StackLimit,
		reward.Stack.Count,
		dnfrepo.AccountSharedGrantMeta{
			Source:        "dungeon_card_reward",
			ItemKind:      reward.Stack.Extra["item_kind"],
			PVFPath:       reward.Stack.Extra["pvf_path"],
			StackableType: reward.Stack.Extra["stackable_type"],
		},
	)
	if !grant.Changed {
		// 固定格被别人占着、或容器里同 id 已到 [stack limit]：整件退回角色背包。
		// 绝不驱赶别人的行，也不留下一个空壳行。
		if seeded {
			delete(accountInventory.Slots, key)
		}
		return 0, reward.Stack.Count, false
	}
	finalKey := dnfrepo.AccountSharedInventorySlotKey(grant.Slot)
	stack := accountInventory.Slots[finalKey]
	updateDungeonRewardRawAmount(&stack)
	accountInventory.Slots[finalKey] = stack
	return grant.Placed, grant.Remainder, true
}

func addCardItem(
	record *dnfrepo.InventoryRecord,
	mainSlots uint16,
	reward CardItemReward,
	project StackProjector,
) (int16, error) {
	if record == nil {
		return 0, ErrRewardInvalid
	}
	// Card-flip revive coins belong in the fixed slot-1 wallet, like every
	// other channel (see dnfrevivecoin.Grant).
	if dnfrevivecoin.IsReward(reward.Stack.ItemID) && reward.Stack.Count > 0 {
		if _, _, err := dnfrevivecoin.GrantSlots(
			record.Slots, reward.Stack.Count, "revive_coin_card_reward"); err != nil {
			return 0, err
		}
		return dnfrevivecoin.WalletSlot, nil
	}
	slotStart := int16(0)
	slotEnd := int16(mainSlots) - 1
	if reward.SlotStart >= 3 && reward.SlotEnd >= reward.SlotStart {
		slotStart = reward.SlotStart
		slotEnd = reward.SlotEnd
	}
	// PVF [stack limit] 是这个容器里该道具的总量上限：额度按容器里同 id 总量算，
	// 容器里已经有该道具的行时不能再开第二行。装备（每件一行）与没写 [stack limit]
	// 的道具沿用旧行为。
	newRowStackLimit := int64(0)
	space := int64(math.MaxInt32)
	if reward.Stackable && reward.StackLimit > 0 {
		newRowStackLimit = reward.StackLimit
		space = dnfrepo.ContainerItemSpace(
			record.Slots,
			dnfrepo.MainInventoryListType,
			reward.Stack.ItemID,
			reward.StackLimit,
		)
	}
	if reward.Stackable {
		for slot := slotStart; slot <= slotEnd; slot++ {
			key := mainSlotKey(uint16(slot))
			stack, ok := record.Slots[key]
			if !ok || stack.ItemID != reward.Stack.ItemID || stack.Bind != reward.Stack.Bind {
				continue
			}
			if reward.ExpireAt.IsZero() {
				if !stack.ExpireAt.IsZero() {
					continue
				}
			} else {
				var err error
				stack, err = project(stack, reward.ExpireAt)
				if err != nil {
					return 0, err
				}
			}
			if reward.StackLimit == 1 || (reward.StackLimit > 0 && stack.Count > reward.StackLimit-reward.Stack.Count) ||
				reward.Stack.Count > space {
				continue
			}
			if stack.Count > math.MaxInt64-reward.Stack.Count {
				return 0, fmt.Errorf("%w: item=%d", ErrRewardInvalid, reward.Stack.ItemID)
			}
			stack.Count += reward.Stack.Count
			updateDungeonRewardRawAmount(&stack)
			record.Slots[key] = stack
			return int16(slot), nil
		}
	}
	for slot := slotStart; slot <= slotEnd; slot++ {
		key := mainSlotKey(uint16(slot))
		if _, occupied := record.Slots[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(record.Slots, dnfrepo.MainInventoryListType, reward.Stack.ItemID, newRowStackLimit) {
			break
		}
		stack := cloneStack(reward.Stack)
		if reward.Stackable && !reward.ExpireAt.IsZero() {
			var err error
			stack, err = project(stack, reward.ExpireAt)
			if err != nil {
				return 0, err
			}
		}
		record.Slots[key] = stack
		return int16(slot), nil
	}
	return 0, ErrInventoryFull
}

func updateDungeonRewardRawAmount(stack *dnfrepo.ItemStack) {
	if stack == nil || stack.Count <= 0 || stack.Count > math.MaxUint32 || len(stack.RawEntry) != 0x77 {
		return
	}
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	binary.LittleEndian.PutUint32(stack.RawEntry[0x06:0x0a], uint32(stack.Count))
}

func mainSlotKey(slot uint16) string {
	return "0:" + strconv.FormatUint(uint64(slot), 10)
}

func cloneStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if len(stack.Extra) == 0 {
		stack.Extra = nil
		return stack
	}
	extra := make(map[string]string, len(stack.Extra))
	for key, value := range stack.Extra {
		extra[key] = value
	}
	stack.Extra = extra
	return stack
}

func parseLuckyStars(raw string) (uint32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func updatedAtOrNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value
}
