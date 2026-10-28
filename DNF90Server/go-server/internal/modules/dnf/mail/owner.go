package mail

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/carrygold"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

var (
	ErrMailboxRepoMissing         = errors.New("mailbox repository is missing")
	ErrMailboxCharacterMissing    = errors.New("mailbox character is missing")
	ErrMailboxRecipientMissing    = errors.New("mailbox recipient is missing")
	ErrMailboxInvalidRequest      = errors.New("mailbox request is invalid")
	ErrMailboxMailNotFound        = errors.New("mailbox mail is not found")
	ErrMailboxAlreadyClaimed      = errors.New("mailbox mail is already claimed")
	ErrMailboxExpired             = errors.New("mailbox mail expired")
	ErrMailboxInventoryFull       = errors.New("mailbox inventory is full")
	ErrMailboxAssetsUOW           = errors.New("mailbox asset unit of work is missing")
	ErrMailboxAssetMismatch       = errors.New("mailbox attachment does not match durable inventory")
	ErrMailboxGoldInsufficient    = errors.New("mailbox sender gold is insufficient")
	ErrMailboxDeleteClaimable     = errors.New("mailbox mail still has claimable assets")
	ErrMailboxStateUnsupported    = errors.New("mailbox state is unsupported")
	ErrMailboxSpecialUnsupported  = errors.New("special mailbox send is unsupported")
	ErrMailboxItemResolverMissing = errors.New("mailbox item price resolver is missing")
	ErrMailboxSelfRecipient       = errors.New("mailbox recipient is the sender")
	// ErrMailboxItemLocked marks a locked attachment. The equipment lock's whole
	// purpose is to keep an item out of destructive or irreversible moves, and a
	// letter leaves the character for good - live 2026-09-15: a locked piece of
	// equipment was mailed away because this path had no lock check at all.
	ErrMailboxItemLocked = errors.New("mailbox attachment is equipment locked")
	ErrMailboxSavedFull  = errors.New("mailbox saved-letter storage is full")
)

// mailboxStackLocked mirrors the lock-state contract the itemlock owner writes
// (equipment_lock_state: "1"/"2" or a named state). Selling, discarding, trading
// and every upgrade-style path each keep their own copy of that switch, so this
// one follows the same shape instead of inventing a shared abstraction.
func mailboxStackLocked(stack dnfrepo.ItemStack) bool {
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

const (
	mailboxNormalPageSize = 20
	mailboxSavedPageSize  = 10
	mailboxNormalLifetime = 15 * 24 * time.Hour

	// Mail attachments may target the character bag (3..353) or the
	// account-owned crystal/soul warehouses (354..365).  The resolver's PVF
	// range decides the durable owner; this upper bound only validates the
	// complete client-visible main-inventory namespace.
	mailboxMainFirstItemSlot int16 = 3
	mailboxMainLastItemSlot  int16 = dnfrepo.SoulWarehouseLastSlot

	// 主背包标准分类的解锁口径：每个分类 7 行 × 8 格 = 56 格，前 32 格随角色解锁，
	// 之后按 CharacterContainerState.MainSlotCount（0/8/16/24）逐行开放。
	// 与 dnfbridge.currentMainInventoryBaseSlotCount /
	// currentMainInventoryCategorySlotCount、inventory.moveDestinationUnlockedSlotRange
	// 是同一套数字；主背包扩展最多 3 行（24 格），超出说明状态不可信，按"未知"处理。
	mailboxMainInventoryBaseSlotCount     int16  = 32
	mailboxMainInventoryCategorySlotCount int16  = 56
	mailboxMainInventoryMaxExpansionSlots uint16 = 24

	// Current PVF stackable/cash/chn_Trade_Gold/trade_point_event.stk is an
	// account-currency voucher. It must never occupy a material inventory slot.
	HappyTokenCeraVoucherItemID int64 = 2681917
)

type Owner struct {
	repos            dnfrepo.Group
	itemResolver     alignedcmd.MailboxItemResolver
	premiumResolver  alignedcmd.PremiumContractResolver
	premiumAccountID string
	now              func() time.Time
}

// WithPremiumContracts lets the extraction path activate a 契约 attachment
// instead of demanding a backpack slot for it. Both the runtime-PVF resolver and
// the acting account are required: the resolver is the only authority for the
// premium type and its term, and the expiry is account-scoped state.
func (o Owner) WithPremiumContracts(resolver alignedcmd.PremiumContractResolver, accountID string) Owner {
	o.premiumResolver = resolver
	o.premiumAccountID = strings.TrimSpace(accountID)
	return o
}

// resolvePremiumContract reports whether one attachment is a runtime-PVF premium
// contract. Without a resolver or an acting account the attachment keeps its
// ordinary item behaviour instead of being silently consumed.
func (o Owner) resolvePremiumContract(itemID int64) (alignedcmd.PremiumContractResolution, bool, error) {
	if o.premiumResolver == nil || o.premiumAccountID == "" || itemID <= 0 {
		return alignedcmd.PremiumContractResolution{}, false, nil
	}
	resolution, err := o.premiumResolver(itemID)
	if err != nil {
		return alignedcmd.PremiumContractResolution{}, false, err
	}
	if resolution.PremiumType <= 0 || resolution.DurationSeconds <= 0 {
		return alignedcmd.PremiumContractResolution{}, false, nil
	}
	return resolution, true, nil
}

type OpenResult struct {
	CharacterID string
	Total       int
	Unread      int
	Claimable   int
	NotLoaded   int
	Mails       []dnfrepo.MailRecord
	ObservedAt  time.Time
}

type SendResult struct {
	MailID               string
	RecipientCharacterID string
}

type ClaimResult struct {
	MailIDs   []uint32
	Gold      int64
	GoldAfter int64
	// GoldDiscarded is the part of the attachment the carry cap refused.
	GoldDiscarded         int64
	ItemCount             int
	HappyTokenCeraGranted int64
	// TokenCeraGranted 是本次领取里直接入钱包的代币券数量（与欢乐代币券同一套语义）。
	TokenCeraGranted  int64
	Wallet                *dnfcerashop.Wallet
	InventoryOK           bool
	ItemSlotRefreshes     []alignedcmd.ItemSlotRefresh
	EpicBookBalances      []alignedcmd.EpicBookBalanceProjection
	// Claimed lists what op95 reports back: one row per attachment slot actually
	// handed over, carrying the same claim object the summary row gave it.
	Claimed          []ClaimedAttachment
	PremiumActivated bool
	// PremiumActivations is what a claimed 契约 granted, in premium-type order.
	// The account row already carries these expiries; the client does not, and
	// only learns about them from the activation notification.
	PremiumActivations []PremiumActivation
}

// PremiumActivation is one entitlement a claim extended. The absolute expiry is
// what the account row holds; the remaining seconds are what the activation
// notification carries, and both are taken from the claim's own clock.
type PremiumActivation struct {
	PremiumType      int64
	ExpireAt         int64
	RemainingSeconds int64
}

// ClaimedAttachment addresses one attachment the client must retire from an open
// letter. The client needs both halves: the letter id keys its mailbox container
// and the claim object selects the slot inside that letter.
type ClaimedAttachment struct {
	LetterID    uint32
	ClaimObject uint32
}

// pendingPremiumContract is one staged 契约 activation for a single letter.
type pendingPremiumContract struct {
	resolution alignedcmd.PremiumContractResolution
	count      int64
}

// ClearAllResult reports the durable rows and still-visible assets removed
// from one character's mailbox. Clearing is serialized with mail claiming so
// an attachment cannot be claimed while the GM operation removes its letter.
type ClearAllResult struct {
	Removed     int
	Unread      int
	Claimable   int
	Gold        int64
	Attachments int
}

func NewOwner(repos dnfrepo.Group, itemResolvers ...alignedcmd.MailboxItemResolver) Owner {
	var itemResolver alignedcmd.MailboxItemResolver
	if len(itemResolvers) > 0 {
		itemResolver = itemResolvers[0]
	}
	return Owner{repos: repos, itemResolver: itemResolver, now: time.Now}
}

// ClearAll permanently removes every normal, saved, deleted, and expired
// letter from the character's durable mailbox.
func (o Owner) ClearAll(ctx context.Context, characterID string) (ClearAllResult, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ClearAllResult{}, ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return ClearAllResult{}, ErrMailboxRepoMissing
	}
	if o.repos.MailboxAssets == nil {
		return ClearAllResult{}, ErrMailboxAssetsUOW
	}
	result := ClearAllResult{}
	err := o.repos.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		_ dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		record, found, err := mailboxes.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(record.Mails) == 0 {
			return nil
		}
		now := o.clock()
		result.Removed = len(record.Mails)
		for _, candidate := range record.Mails {
			visible := !candidate.Deleted && !mailExpired(candidate, now)
			if visible && !candidate.Read {
				result.Unread++
			}
			if visible && !candidate.Claimed && (candidate.Gold > 0 || len(candidate.Attachments) > 0) {
				result.Claimable++
			}
			if !candidate.Claimed {
				result.Gold += candidate.Gold
				result.Attachments += len(candidate.Attachments)
			}
		}
		record.Mails = nil
		record.UpdatedAt = now
		return dnfrepo.SaveMailboxFields(ctx, mailboxes, record, dnfrepo.MailboxFieldMails)
	})
	if err != nil {
		return ClearAllResult{}, err
	}
	return result, nil
}

func (o Owner) Open(ctx context.Context, characterID string) (OpenResult, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return OpenResult{}, ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return OpenResult{}, ErrMailboxRepoMissing
	}
	record, _, err := o.loadOrCreateMailbox(ctx, characterID)
	if err != nil {
		return OpenResult{}, err
	}
	// Older claim implementations retained transferred assets on a claimed
	// letter. A claimed mail is an empty, read receipt: repair those legacy
	// rows before projecting the mailbox so the client cannot render or select
	// a stale attachment again.
	if clearClaimedMailboxAssets(&record) {
		record.UpdatedAt = o.clock()
		if err := dnfrepo.SaveMailboxFields(ctx, o.repos.Mailbox, record, dnfrepo.MailboxFieldMails); err != nil {
			return OpenResult{}, err
		}
	}
	result := OpenResult{CharacterID: characterID}
	now := o.clock()
	result.ObservedAt = now
	normal := make([]dnfrepo.MailRecord, 0, len(record.Mails))
	saved := make([]dnfrepo.MailRecord, 0, len(record.Mails))
	for _, mail := range record.Mails {
		if mail.Deleted || mailExpired(mail, now) {
			continue
		}
		result.Total++
		if !mail.Read {
			result.Unread++
		}
		if !mail.Claimed && (mail.Gold > 0 || len(mail.Attachments) > 0) {
			result.Claimable++
		}
		if mailboxMailSaved(mail) {
			saved = append(saved, cloneMailboxMail(mail))
		} else {
			normal = append(normal, cloneMailboxMail(mail))
		}
	}
	sortMailboxMails(normal)
	sortMailboxMails(saved)
	// 分页窗口取**最新**的一页，不是最老的一页。
	//
	// 这一页就是玩家能看到的全部邮件：协议里没有任何"加载更多 / 翻页"请求
	// （request.go 只有 send / 领取 / 改状态 / 查角色 / mail-id 列表），
	// not_loaded 只是个计数，客户端拿它画一行提示、不会回请下一页。
	// 换句话说，谁被截掉，谁就等于不存在。
	//
	// sortMailboxMails 是**升序**（created_at 老的在前，同一时刻按 mail_id 升序），
	// 所以老的 normal[:mailboxNormalPageSize] 取的是**最老的 20 封**。一旦邮箱里
	// 活着的普通邮件超过 20 封，这个窗口就被最老的那一批永久钉死：此后进来的每一封
	// 信都只会让 not_loaded +1，永远挤不进窗口。而且它还锁死了自己——删信要求先领取
	// （见 ChangeState 的 ErrMailboxDeleteClaimable），领取又要求背包有空位，于是
	// "满背包 → 奖励转邮件 → 邮件领不出来也删不掉 → 窗口永不前进"。
	//
	// 实机证据（2026-09-23，角色 5「随便玩玩」）：451 封活着，窗口恒为 mail_id
	// 516..535；GM 新发一封 966 成功落库，窗口纹丝不动 —— 玩家那边看到的就是
	// "收不到邮件"。alert 也救不回来：class0/0x63 只弹一句"收到新邮件"、从不重载
	// 列表（见 dnfbridge/mailbox_notification.go），列表只由 class0/0x61 重建，
	// 而重建出来的还是那 20 封最老的。
	//
	// 截尾取最新即可：窗口跟着最新邮件走，Total/Unread/Claimable 仍是全量统计，
	// not_loaded 的口径（有多少封没装进这一页）也完全没变。
	//
	// 注意：不要把这里改成 sortMailboxMails 反向排序。发出去的顺序仍是升序，
	// 客户端 FUN_0270b7f0 自己会重排容器；改顺序既解决不了截断，又会动到
	// 已经对齐过的字节布局。真正要修的只有"截哪一头"。
	if len(normal) > mailboxNormalPageSize {
		result.NotLoaded += len(normal) - mailboxNormalPageSize
		normal = normal[len(normal)-mailboxNormalPageSize:]
	}
	if len(saved) > mailboxSavedPageSize {
		result.NotLoaded += len(saved) - mailboxSavedPageSize
		saved = saved[len(saved)-mailboxSavedPageSize:]
	}
	result.Mails = append(normal, saved...)
	return result, nil
}

func (o Owner) Send(ctx context.Context, senderCharacterID string, req SendRequest) (SendResult, error) {
	senderCharacterID = strings.TrimSpace(senderCharacterID)
	if senderCharacterID == "" {
		return SendResult{}, ErrMailboxCharacterMissing
	}
	if strings.TrimSpace(req.RecipientName) == "" {
		return SendResult{}, ErrMailboxInvalidRequest
	}
	// The current client sets the trailing global flag on an ordinary send.
	// Live 2026-09-15: a letter to a character on another account arrived as
	// special=0 global=1 and was refused as "special mailbox send is
	// unsupported", so no letter could be sent at all - locked or not. The flag
	// marks a recipient outside the sender's own account, not a server-wide
	// announcement, so only a non-zero Special still means an unimplemented
	// special mail.
	if req.Special != 0 {
		return SendResult{}, ErrMailboxSpecialUnsupported
	}
	if req.Gold > math.MaxInt32 {
		return SendResult{}, ErrMailboxInvalidRequest
	}
	if o.repos.Character == nil {
		return SendResult{}, ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return SendResult{}, ErrMailboxRepoMissing
	}
	if o.repos.MailboxAssets == nil {
		return SendResult{}, ErrMailboxAssetsUOW
	}

	recipientID, found, err := o.repos.Character.FindIDByName(ctx, req.RecipientName)
	if err != nil {
		return SendResult{}, err
	}
	if !found {
		return SendResult{}, ErrMailboxRecipientMissing
	}
	if recipientID == senderCharacterID {
		return SendResult{}, ErrMailboxSelfRecipient
	}
	var mailID string
	err = o.repos.MailboxAssets.WithinMailboxAssets(
		ctx,
		senderCharacterID,
		recipientID,
		func(
			_ dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			sender, senderFound, loadErr := characters.Load(ctx, senderCharacterID)
			if loadErr != nil {
				return loadErr
			}
			if !senderFound {
				return ErrMailboxCharacterMissing
			}
			if sender.Stats == nil {
				sender.Stats = make(map[string]int64)
			}
			gold := int64(req.Gold)

			var inventory dnfrepo.InventoryRecord
			var inventoryFound bool
			if len(req.Attachments) > 0 {
				inventory, inventoryFound, loadErr = inventories.Load(ctx, senderCharacterID)
				if loadErr != nil {
					return loadErr
				}
				if !inventoryFound || inventory.Slots == nil {
					return ErrMailboxAssetMismatch
				}
			}
			attachments := make([]dnfrepo.MailAttachment, 0, len(req.Attachments))
			var attachmentValue int64
			for idx, requested := range req.Attachments {
				if requested.Count > math.MaxInt32 {
					return ErrMailboxInvalidRequest
				}
				key := fmt.Sprintf("%d:%d", requested.ListType, requested.SlotIndex)
				stack, ok := inventory.Slots[key]
				if !ok || stack.ItemID != int64(requested.ItemID) || stack.Count < int64(requested.Count) {
					return fmt.Errorf("%w: attachment=%d list=%d slot=%d", ErrMailboxAssetMismatch, idx, requested.ListType, requested.SlotIndex)
				}
				if mailboxStackLocked(stack) {
					return fmt.Errorf("%w: attachment=%d list=%d slot=%d item=%d",
						ErrMailboxItemLocked, idx, requested.ListType, requested.SlotIndex, stack.ItemID)
				}
				if o.itemResolver == nil {
					return ErrMailboxItemResolverMissing
				}
				resolution, resolveErr := o.itemResolver(requested.ItemID)
				if resolveErr != nil || resolution.Price < 0 {
					return fmt.Errorf("%w: attachment=%d item=%d resolve=%v", ErrMailboxAssetMismatch, idx, requested.ItemID, resolveErr)
				}
				count := int64(requested.Count)
				if resolution.Price > 0 && count > math.MaxInt64/resolution.Price {
					return ErrMailboxInvalidRequest
				}
				value := resolution.Price * count
				if attachmentValue > math.MaxInt64-value {
					return ErrMailboxInvalidRequest
				}
				attachmentValue += value
				attachment := dnfrepo.MailAttachment{
					ItemID:   stack.ItemID,
					Count:    count,
					Bind:     stack.Bind,
					ExpireAt: stack.ExpireAt,
					RawEntry: append([]byte(nil), stack.RawEntry...),
					Extra:    cloneStringMap(stack.Extra),
				}
				if attachment.Extra == nil {
					attachment.Extra = make(map[string]string)
				}
				attachment.Extra["mailbox_pvf_kind"] = resolution.Kind
				attachment.Extra["mailbox_equipment_type"] = resolution.EquipmentType
				attachment.Extra["mailbox_pvf_path"] = resolution.PVFPath
				attachments = append(attachments, attachment)
				if stack.Count == count {
					delete(inventory.Slots, key)
				} else {
					stack.Count -= count
					inventory.Slots[key] = stack
				}
			}
			postage, postageErr := currentMailboxPostage(gold, len(attachments), attachmentValue)
			if postageErr != nil || gold > math.MaxInt64-postage {
				return ErrMailboxInvalidRequest
			}
			totalDebit := gold + postage
			if sender.Stats["gold"] < totalDebit {
				return ErrMailboxGoldInsufficient
			}

			record, recordFound, loadErr := mailboxes.Load(ctx, recipientID)
			if loadErr != nil {
				return loadErr
			}
			if !recordFound {
				record = dnfrepo.MailboxRecord{CharacterID: recipientID}
			}
			if record.Mails == nil {
				record.Mails = make(map[string]dnfrepo.MailRecord)
			}
			mailID, loadErr = nextMailID(record.Mails)
			if loadErr != nil {
				return loadErr
			}
			now := o.clock()
			record.Mails[mailID] = dnfrepo.MailRecord{
				MailID:               mailID,
				SenderCharacterID:    senderCharacterID,
				SenderName:           sender.Name,
				RecipientCharacterID: recipientID,
				RecipientName:        req.RecipientName,
				Body:                 req.Body,
				Gold:                 gold,
				Attachments:          attachments,
				CreatedAt:            now,
				ExpireAt:             now.Add(mailboxNormalLifetime),
				Metadata: map[string]string{
					"source":       mailboxSendSource(req),
					"special":      strconv.FormatUint(uint64(req.Special), 10),
					"global":       strconv.FormatBool(req.Global),
					"postage_gold": strconv.FormatInt(postage, 10),
				},
			}
			record.UpdatedAt = now
			sender.Stats["gold"] -= totalDebit
			if err := dnfrepo.SaveCharacterFields(ctx, characters, sender, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			if len(req.Attachments) > 0 {
				if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
					return err
				}
			}
			return dnfrepo.SaveMailboxFields(ctx, mailboxes, record, dnfrepo.MailboxFieldMails)
		},
	)
	if err != nil {
		return SendResult{}, err
	}
	return SendResult{MailID: mailID, RecipientCharacterID: recipientID}, nil
}

func (o Owner) Claim(ctx context.Context, characterID string, req ExtractRequest) (ClaimResult, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ClaimResult{}, ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return ClaimResult{}, ErrMailboxRepoMissing
	}
	if o.repos.MailboxAssets == nil {
		return ClaimResult{}, ErrMailboxAssetsUOW
	}
	if len(req.MailIDs) == 0 {
		return ClaimResult{}, ErrMailboxInvalidRequest
	}

	record, found, err := o.repos.Mailbox.Load(ctx, characterID)
	if err != nil {
		return ClaimResult{}, err
	}
	if !found {
		return ClaimResult{}, ErrMailboxMailNotFound
	}
	requiresItemResolver := false
	requiresEpicBook := false
	for _, claimObject := range req.MailIDs {
		id, _, _ := DecodeMailboxClaimObject(claimObject)
		mail, ok := record.Mails[strconv.FormatUint(uint64(id), 10)]
		if !ok || mail.Claimed {
			continue
		}
		for _, attachment := range mail.Attachments {
			if attachment.ItemID <= 0 || attachment.Count <= 0 || dnfcerashop.IsVoucher(attachment.ItemID) {
				continue
			}
			// A mailed 契约 is delivered by activation and never touches the item
			// resolver.
			if _, contractOK, contractErr := o.resolvePremiumContract(attachment.ItemID); contractErr != nil {
				return ClaimResult{}, contractErr
			} else if contractOK {
				continue
			}
			if !dnfrevivecoin.IsReward(attachment.ItemID) {
				requiresItemResolver = true
				if o.itemResolver != nil {
					resolution, resolveErr := o.itemResolver(uint32(attachment.ItemID))
					if resolveErr != nil {
						return ClaimResult{}, fmt.Errorf("%w: attachment item=%d resolve=%v", ErrMailboxAssetMismatch, attachment.ItemID, resolveErr)
					}
					if (resolution.EpicBookEquipmentID == 0) != (resolution.EpicBookFragmentID == 0) {
						return ClaimResult{}, fmt.Errorf("%w: attachment item=%d has incomplete epic-book mapping", ErrMailboxAssetMismatch, attachment.ItemID)
					}
					requiresEpicBook = requiresEpicBook || resolution.EpicBookEquipmentID != 0
				}
			}
		}
	}
	if requiresItemResolver && o.itemResolver == nil {
		return ClaimResult{}, ErrMailboxItemResolverMissing
	}
	result := ClaimResult{}
	resultEpicBookBalances := make(map[uint32]uint32)
	applyClaim := func(
		accounts dnfrepo.AccountRepository,
		accountInventories dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
		epicBooks dnfrepo.EpicBookRepository,
	) error {
		mailbox, mailboxFound, err := mailboxes.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !mailboxFound {
			return ErrMailboxMailNotFound
		}
		now := o.clock()
		selected := make([]claimCandidate, 0, len(req.MailIDs))
		byMail := make(map[uint32]int, len(req.MailIDs))
		for _, claimObject := range req.MailIDs {
			id, attachmentIndex, singleSlot := DecodeMailboxClaimObject(claimObject)
			key := strconv.FormatUint(uint64(id), 10)
			mail, ok := mailbox.Mails[key]
			if !ok || mail.Deleted {
				return ErrMailboxMailNotFound
			}
			if mail.Claimed {
				// The current mailbox UI keeps claimed letters visible and its
				// select-all action includes them again. Ignore those completed
				// rows so one old selection cannot block still-pending rewards in
				// the same op95 batch.
				continue
			}
			if mailExpired(mail, now) {
				return ErrMailboxExpired
			}
			if mail.Gold < 0 {
				return ErrMailboxInvalidRequest
			}
			if singleSlot && attachmentIndex >= len(mail.Attachments) {
				return ErrMailboxMailNotFound
			}
			// The client can select several slots of one letter in the same
			// request; they are resolved against a single copy of it.
			if position, seen := byMail[id]; seen {
				if !singleSlot {
					selected[position].slots = nil
					selected[position].wholeLetter = true
					continue
				}
				if !selected[position].wholeLetter {
					selected[position].slots = append(selected[position].slots, attachmentIndex)
				}
				continue
			}
			candidate := claimCandidate{id: id, key: key, mail: mail, claimObject: claimObject}
			if singleSlot {
				candidate.slots = []int{attachmentIndex}
			} else {
				candidate.wholeLetter = true
			}
			byMail[id] = len(selected)
			selected = append(selected, candidate)
		}
		if len(selected) == 0 {
			return ErrMailboxAlreadyClaimed
		}
		character, characterFound, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !characterFound {
			return ErrMailboxCharacterMissing
		}
		accountID := strings.TrimSpace(character.AccountID)

		selectedHasAttachments := false
		for _, candidate := range selected {
			if len(candidate.mail.Attachments) > 0 {
				selectedHasAttachments = true
				break
			}
		}
		inventory := dnfrepo.InventoryRecord{CharacterID: characterID}
		if selectedHasAttachments {
			loadedInventory, inventoryFound, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !inventoryFound {
				inventory = dnfrepo.InventoryRecord{CharacterID: characterID}
			} else {
				inventory = loadedInventory
			}
			if inventory.Slots == nil {
				inventory.Slots = make(map[string]dnfrepo.ItemStack)
			}
		}
		accountInventory := dnfrepo.AccountInventoryRecord{AccountID: accountID}
		accountInventoryLoaded := false
		accountInventoryChanged := false
		epicBook := dnfrepo.EpicBookRecord{AccountID: accountID}
		epicBookLoaded := false
		epicBookChanged := false

		// Select-all requests contain the current page's mail IDs in order. Each
		// letter remains an atomic asset transfer, but a full tab on one letter
		// must not roll back mail that already fit into the quick slots or its
		// real item tab earlier in the same request.
		// 契约 attachments activate on claim, so the account premium row joins this
		// transaction. It is loaded lazily: a letter without a contract never
		// touches account state, and it is saved before the happy-token block so
		// that block reloads the already extended row.
		var premiumAccount dnfrepo.AccountRecord
		premiumAccountLoaded := false
		premiumAccountDirty := false
		premiumExpiries := make(map[int64]int64)
		activatePremium := func(resolution alignedcmd.PremiumContractResolution, count int64, at time.Time) error {
			if accountID == "" || accounts == nil {
				return ErrMailboxCharacterMissing
			}
			if !premiumAccountLoaded {
				loaded, found, loadErr := accounts.Load(ctx, accountID)
				if loadErr != nil {
					return loadErr
				}
				if !found {
					return ErrMailboxAssetMismatch
				}
				premiumAccount = dnfrepo.CloneAccount(loaded)
				premiumAccountLoaded = true
			}
			premium.Upsert(&premiumAccount, resolution.PremiumType, resolution.DurationSeconds, count, at)
			// The client is told the new entitlement separately, so the resulting
			// expiry is read back here. Several contracts of one type in the same
			// request collapse onto the final value.
			premiumExpiries[resolution.PremiumType] = premium.ExpireAt(premiumAccount, resolution.PremiumType)
			premiumAccountDirty = true
			return nil
		}

		accepted := make([]claimCandidate, 0, len(selected))
		var totalGold int64
		var totalHappyTokenCera int64
		var totalTokenCera int64
		inventoryChanged := false
		inventoryFull := false
		for _, candidate := range selected {
			candidateInventory := dnfrepo.CloneInventory(inventory)
			if candidateInventory.Slots == nil {
				candidateInventory.Slots = make(map[string]dnfrepo.ItemStack)
			}
			candidateAccountInventory := dnfrepo.CloneAccountInventory(accountInventory)
			candidateEpicBook := dnfrepo.CloneEpicBook(epicBook)
			candidateAccountInventoryChanged := false
			candidateEpicBookChanged := false
			candidateEpicBookBalances := make(map[uint32]uint32)
			candidateCharacterInventoryChanged := false
			candidateRefreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(candidate.mail.Attachments))
			candidateItemCount := 0
			var candidateHappyTokenCera int64
			var candidateTokenCera int64
			candidateContracts := make([]pendingPremiumContract, 0, len(candidate.mail.Attachments))
			claimedSlots := make(map[int]struct{}, len(candidate.mail.Attachments))
			fits := true
			for _, index := range candidate.requestedSlots() {
				attachment := candidate.mail.Attachments[index]
				// An empty slot has already been handed over; it is skipped rather
				// than treated as a corrupt letter.
				if attachment.ItemID <= 0 || attachment.Count <= 0 {
					continue
				}
				// 代币券 / 欢乐代币券：账号级货币，不占背包格。
				// 它们在道具 PVF 里就是普通 `[stackable type] [material]`，
				// **没有 `[action type]`** —— 客户端根本没有任何途径把它们变成钱包
				// 余额，所以当道具发下去只会得到一件永远用不掉的"材料"。
				if voucher := dnfcerashop.VoucherKindOf(attachment.ItemID); voucher != dnfcerashop.VoucherNone {
					if voucher == dnfcerashop.VoucherTokenCera {
						if candidateTokenCera > math.MaxInt32-attachment.Count {
							return ErrMailboxInvalidRequest
						}
						candidateTokenCera += attachment.Count
					} else {
						if candidateHappyTokenCera > math.MaxInt32-attachment.Count {
							return ErrMailboxInvalidRequest
						}
						candidateHappyTokenCera += attachment.Count
					}
					claimedSlots[index] = struct{}{}
					continue
				}
				// A 契约 activates on claim: that is the delivery, and it needs no
				// inventory space at all. It is staged per letter and applied only
				// once the letter is accepted, so an attachment that does not fit can
				// never leave the contract granted.
				contract, contractOK, contractErr := o.resolvePremiumContract(attachment.ItemID)
				if contractErr != nil {
					return contractErr
				}
				if contractOK {
					candidateContracts = append(candidateContracts, pendingPremiumContract{
						resolution: contract,
						count:      attachment.Count,
					})
					claimedSlots[index] = struct{}{}
					claimedSlots[index] = struct{}{}
					candidateItemCount++
					continue
				}
				if dnfrevivecoin.IsReward(attachment.ItemID) {
					if _, grantErr := dnfrevivecoin.Grant(
						&candidateInventory,
						attachment.Count,
						"mailbox_attachment_revive_coin_wallet",
					); grantErr != nil {
						return fmt.Errorf("%w: revive-coin attachment item=%d: %v", ErrMailboxAssetMismatch, attachment.ItemID, grantErr)
					}
					candidateCharacterInventoryChanged = true
					candidateRefreshes = append(candidateRefreshes, alignedcmd.ItemSlotRefresh{
						ListType:  dnfrepo.MainInventoryListType,
						SlotIndex: dnfrevivecoin.WalletSlot,
					})
					claimedSlots[index] = struct{}{}
					candidateItemCount++
					continue
				}
				resolution, resolveErr := o.itemResolver(uint32(attachment.ItemID))
				if resolveErr != nil || !mailboxClaimSlotRangeValid(resolution) ||
					(resolution.EpicBookEquipmentID == 0) != (resolution.EpicBookFragmentID == 0) {
					return fmt.Errorf("%w: attachment item=%d resolve=%v", ErrMailboxAssetMismatch, attachment.ItemID, resolveErr)
				}
				if resolution.EpicBookEquipmentID != 0 {
					if accountID == "" || epicBooks == nil || attachment.Count > math.MaxUint32 {
						return ErrMailboxCharacterMissing
					}
					if !epicBookLoaded {
						loadedEpicBook, foundEpicBook, loadErr := epicBooks.Load(ctx, accountID)
						if loadErr != nil {
							return loadErr
						}
						if foundEpicBook {
							epicBook = dnfrepo.CloneEpicBook(loadedEpicBook)
						} else {
							epicBook = dnfrepo.EpicBookRecord{AccountID: accountID}
						}
						if epicBook.Balances == nil {
							epicBook.Balances = make(map[uint32]uint32)
						}
						epicBookLoaded = true
						candidateEpicBook = dnfrepo.CloneEpicBook(epicBook)
					}
					if candidateEpicBook.Balances == nil {
						candidateEpicBook.Balances = make(map[uint32]uint32)
					}
					amount := uint32(attachment.Count)
					before := candidateEpicBook.Balances[resolution.EpicBookEquipmentID]
					if before > math.MaxUint32-amount {
						return ErrMailboxInvalidRequest
					}
					balance := before + amount
					candidateEpicBook.Balances[resolution.EpicBookEquipmentID] = balance
					candidateEpicBookChanged = true
					candidateEpicBookBalances[resolution.EpicBookFragmentID] = balance
					claimedSlots[index] = struct{}{}
					candidateItemCount++
					continue
				}
				accountOwned, ownerErr := mailboxAttachmentUsesAccountInventory(resolution)
				if ownerErr != nil {
					return fmt.Errorf("%w: attachment item=%d owner=%v", ErrMailboxAssetMismatch, attachment.ItemID, ownerErr)
				}
				var changedSlots []int16
				var addErr error
				if accountOwned {
					if accountID == "" || accountInventories == nil {
						return ErrMailboxCharacterMissing
					}
					if !accountInventoryLoaded {
						loadedAccountInventory, foundAccountInventory, loadErr := accountInventories.Load(ctx, accountID)
						if loadErr != nil {
							return loadErr
						}
						if foundAccountInventory {
							accountInventory = dnfrepo.CloneAccountInventory(loadedAccountInventory)
						} else {
							accountInventory = dnfrepo.AccountInventoryRecord{AccountID: accountID}
						}
						if accountInventory.Slots == nil {
							accountInventory.Slots = make(map[string]dnfrepo.ItemStack)
						}
						accountInventoryLoaded = true
						candidateAccountInventory = dnfrepo.CloneAccountInventory(accountInventory)
					}
					changedSlots, addErr = addAttachmentToAccountInventory(&candidateAccountInventory, attachment, resolution)
					candidateAccountInventoryChanged = true
				} else {
					// One container-state read per attachment: the placement must stay
					// inside the rows the character has unlocked, or the attachment lands
					// in a locked bag slot the player cannot even click (live 2026-09-15).
					unlockedMainSlots := o.claimUnlockedMainSlots(ctx, characterID, resolution)
					changedSlots, addErr = addAttachmentToInventoryBounded(&candidateInventory, attachment, resolution, unlockedMainSlots)
					candidateCharacterInventoryChanged = true
				}
				if errors.Is(addErr, ErrMailboxItemLocked) {
					// 目标行被锁定：领取必须停下并报"锁定"，不能当成背包已满
					// （那会让玩家去清空背包，而问题根本不在空间上）。
					return fmt.Errorf("%w: attachment item=%d list=%d", ErrMailboxItemLocked, attachment.ItemID, resolution.ListType)
				}
				if errors.Is(addErr, ErrMailboxInventoryFull) {
					fits = false
					inventoryFull = true
					break
				}
				if addErr != nil {
					return addErr
				}
				for _, slot := range changedSlots {
					candidateRefreshes = append(candidateRefreshes, alignedcmd.ItemSlotRefresh{
						ListType:  resolution.ListType,
						SlotIndex: slot,
					})
				}
				claimedSlots[index] = struct{}{}
				candidateItemCount++
			}
			if !fits {
				continue
			}
			if len(claimedSlots) == 0 && !candidate.claimsGold() {
				// Every addressed slot was already empty and there is no gold left.
				continue
			}
			if totalGold > math.MaxInt64-candidate.mail.Gold {
				return ErrMailboxInvalidRequest
			}
			for _, contract := range candidateContracts {
				if err := activatePremium(contract.resolution, contract.count, now); err != nil {
					return err
				}
			}
			if totalHappyTokenCera > math.MaxInt32-candidateHappyTokenCera {
				return ErrMailboxInvalidRequest
			}
			if totalTokenCera > math.MaxInt32-candidateTokenCera {
				return ErrMailboxInvalidRequest
			}
			totalGold += candidate.mail.Gold
			totalHappyTokenCera += candidateHappyTokenCera
			totalTokenCera += candidateTokenCera
			inventory = candidateInventory
			if candidateAccountInventoryChanged {
				accountInventory = candidateAccountInventory
				accountInventoryChanged = true
			}
			if candidateEpicBookChanged {
				epicBook = candidateEpicBook
				epicBookChanged = true
				for fragmentID, balance := range candidateEpicBookBalances {
					resultEpicBookBalances[fragmentID] = balance
				}
			}
			inventoryChanged = inventoryChanged || candidateCharacterInventoryChanged
			result.ItemCount += candidateItemCount
			result.ItemSlotRefreshes = append(result.ItemSlotRefreshes, candidateRefreshes...)
			candidate.claimed = claimedSlots
			// The class0 remove notification retires whole letters only; a letter
			// with slots still pending stays in the client's list.
			if !mailboxHasAttachment(candidate.attachmentsAfterClaim()) {
				result.MailIDs = append(result.MailIDs, candidate.id)
			}
			result.Claimed = append(result.Claimed, candidate.acknowledgedRows()...)
			accepted = append(accepted, candidate)
		}
		if len(accepted) == 0 {
			if inventoryFull {
				return ErrMailboxInventoryFull
			}
			return ErrMailboxAlreadyClaimed
		}
		result.Gold = totalGold
		result.HappyTokenCeraGranted = totalHappyTokenCera
		result.TokenCeraGranted = totalTokenCera

		if premiumAccountDirty {
			premiumAccount.UpdatedAt = now
			if err := accounts.Save(ctx, premiumAccount); err != nil {
				return err
			}
			result.PremiumActivated = true
			types := make([]int64, 0, len(premiumExpiries))
			for premiumType := range premiumExpiries {
				types = append(types, premiumType)
			}
			sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
			result.PremiumActivations = result.PremiumActivations[:0]
			for _, premiumType := range types {
				expireAt := premiumExpiries[premiumType]
				remaining := expireAt - now.Unix()
				if remaining < 0 {
					remaining = 0
				}
				result.PremiumActivations = append(result.PremiumActivations, PremiumActivation{
					PremiumType:      premiumType,
					ExpireAt:         expireAt,
					RemainingSeconds: remaining,
				})
			}
		}

		if totalGold > 0 || totalHappyTokenCera > 0 || totalTokenCera > 0 {
			if totalGold > 0 {
				if character.Stats == nil {
					character.Stats = make(map[string]int64)
				}
				if character.Stats["gold"] > math.MaxInt64-totalGold {
					return ErrMailboxInvalidRequest
				}
				// The mail attachment is consumed by the claim, so the carry
				// cap discards the part that does not fit rather than failing
				// the whole claim and losing the attached items with it.
				after, discarded := carrygold.Credit(character, totalGold)
				character.Stats["gold"] = after
				result.Gold = totalGold - discarded
				result.GoldDiscarded = discarded
				result.GoldAfter = after
				if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
					return err
				}
			}
			if totalHappyTokenCera > 0 || totalTokenCera > 0 {
				if accountID == "" || accounts == nil {
					return ErrMailboxCharacterMissing
				}
				account, found, err := accounts.Load(ctx, accountID)
				if err != nil {
					return err
				}
				if !found {
					return ErrMailboxCharacterMissing
				}
				account = dnfrepo.CloneAccount(account)
				// 代币券与欢乐代币券都直接入账号钱包，不占背包格。
				// 全有或全无：任一项溢出就整笔拒绝，绝不写半截（见
				// cerashop.CreditVoucherCounts 的说明）。
				if _, err := dnfcerashop.CreditVoucherCounts(&account, dnfcerashop.VoucherCounts{
					TokenCera:      totalTokenCera,
					HappyTokenCera: totalHappyTokenCera,
				}); err != nil {
					return ErrMailboxInvalidRequest
				}
				account.UpdatedAt = now
				if err := accounts.Save(ctx, account); err != nil {
					return err
				}
				updated := dnfcerashop.WalletBalance(account)
				result.Wallet = &updated
			}
		}

		if inventoryChanged {
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		if accountInventoryChanged {
			accountInventory.AccountID = accountID
			accountInventory.UpdatedAt = now
			if err := accountInventories.Save(ctx, accountInventory); err != nil {
				return err
			}
		}
		if epicBookChanged {
			epicBook.AccountID = accountID
			epicBook.UpdatedAt = now
			if err := epicBooks.Save(ctx, epicBook); err != nil {
				return err
			}
		}

		for _, candidate := range accepted {
			mail := candidate.mail
			// Attachments and gold have already been committed to the recipient's
			// character/inventory in this same unit of work. Do not retain a second
			// claimable projection on the mail itself.
			mail.Gold = 0
			mail.Read = true
			// A slot claim empties exactly the slots it took and keeps every
			// position, so the claim object the client still holds for a pending
			// slot keeps addressing the same item. The letter closes once nothing
			// is left in it.
			mail.Attachments = candidate.attachmentsAfterClaim()
			mail.Claimed = !mailboxHasAttachment(mail.Attachments)
			if mail.Claimed {
				mail.Attachments = nil
			}
			mailbox.Mails[candidate.key] = mail
		}
		mailbox.UpdatedAt = now
		return dnfrepo.SaveMailboxFields(ctx, mailboxes, mailbox, dnfrepo.MailboxFieldMails)
	}
	var claimErr error
	if requiresEpicBook {
		claimAssets, ok := o.repos.MailboxAssets.(dnfrepo.MailboxClaimAssetUnitOfWork)
		if !ok {
			return ClaimResult{}, ErrMailboxAssetsUOW
		}
		claimErr = claimAssets.WithinMailboxClaimAssets(ctx, characterID, characterID, applyClaim)
	} else {
		claimErr = o.repos.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
			accounts dnfrepo.AccountRepository,
			accountInventories dnfrepo.AccountInventoryRepository,
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			return applyClaim(accounts, accountInventories, characters, inventories, mailboxes, nil)
		})
	}
	if claimErr != nil {
		return ClaimResult{}, claimErr
	}
	if len(resultEpicBookBalances) > 0 {
		fragmentIDs := make([]uint32, 0, len(resultEpicBookBalances))
		for fragmentID := range resultEpicBookBalances {
			fragmentIDs = append(fragmentIDs, fragmentID)
		}
		sort.Slice(fragmentIDs, func(i, j int) bool { return fragmentIDs[i] < fragmentIDs[j] })
		result.EpicBookBalances = make([]alignedcmd.EpicBookBalanceProjection, 0, len(fragmentIDs))
		for _, fragmentID := range fragmentIDs {
			result.EpicBookBalances = append(result.EpicBookBalances, alignedcmd.EpicBookBalanceProjection{
				FragmentID: fragmentID,
				Balance:    resultEpicBookBalances[fragmentID],
			})
		}
	}
	result.InventoryOK = true
	return result, nil
}

func (o Owner) ChangeState(ctx context.Context, characterID string, req ChangeStateRequest) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return ErrMailboxRepoMissing
	}
	if o.repos.MailboxAssets == nil {
		return ErrMailboxAssetsUOW
	}
	if req.Status != 0 && req.Status != 2 && req.Status != 3 {
		return fmt.Errorf("%w: %d", ErrMailboxStateUnsupported, req.Status)
	}
	return o.repos.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		_ dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		record, found, err := mailboxes.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || len(record.Mails) == 0 {
			return ErrMailboxMailNotFound
		}
		now := o.clock()
		savedCount := 0
		for _, candidate := range record.Mails {
			if !candidate.Deleted && !mailExpired(candidate, now) && mailboxMailSaved(candidate) {
				savedCount++
			}
		}
		for _, id := range req.MailIDs {
			key := strconv.FormatUint(uint64(id), 10)
			mail, ok := record.Mails[key]
			if !ok || mail.Deleted || mailExpired(mail, now) {
				return ErrMailboxMailNotFound
			}
			switch req.Status {
			case 0:
				if !mail.Claimed && (mail.Gold > 0 || len(mail.Attachments) > 0) {
					return ErrMailboxDeleteClaimable
				}
				mail.Deleted = true
			case 2:
				mail.Read = true
			case 3:
				if !mailboxMailSaved(mail) && savedCount >= mailboxSavedPageSize {
					return ErrMailboxSavedFull
				}
				if mail.Metadata == nil {
					mail.Metadata = make(map[string]string)
				}
				if !mailboxMailSaved(mail) {
					savedCount++
				}
				mail.Metadata["mailbox_saved"] = "true"
				mail.Read = true
			}
			record.Mails[key] = mail
		}
		record.UpdatedAt = now
		return dnfrepo.SaveMailboxFields(ctx, mailboxes, record, dnfrepo.MailboxFieldMails)
	})
}

func (o Owner) loadOrCreateMailbox(ctx context.Context, characterID string) (dnfrepo.MailboxRecord, bool, error) {
	record, found, err := o.repos.Mailbox.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.MailboxRecord{}, false, err
	}
	if found {
		if record.Mails == nil {
			record.Mails = make(map[string]dnfrepo.MailRecord)
		}
		return record, true, nil
	}
	record = dnfrepo.MailboxRecord{
		CharacterID: characterID,
		Mails:       make(map[string]dnfrepo.MailRecord),
		UpdatedAt:   o.clock(),
	}
	if err := o.repos.Mailbox.Save(ctx, record); err != nil {
		return dnfrepo.MailboxRecord{}, false, err
	}
	return record, false, nil
}

func clearClaimedMailboxAssets(record *dnfrepo.MailboxRecord) bool {
	if record == nil || len(record.Mails) == 0 {
		return false
	}
	changed := false
	for key, mail := range record.Mails {
		if !mail.Claimed || (mail.Gold == 0 && len(mail.Attachments) == 0) {
			continue
		}
		mail.Gold = 0
		mail.Attachments = nil
		record.Mails[key] = mail
		changed = true
	}
	return changed
}

// claimUnlockedMainSlots 返回这次领取在主背包里"最后一个已解锁格 + 1"，也就是
// mailboxClaimStackSlots 使用的排他上界：候选槽位必须 < 返回值。
//
// 口径与 inventory.moveDestinationUnlockedSlotRange（跨容器落点修正）逐字一致：
// 客户端主背包每个标准分类是 7 行 × 8 格 = 56 格，前 32 格随角色解锁，之后按
// CharacterContainerState.MainSlotCount（0/8/16/24，即已购买的行数）逐行开放；
// 因此已解锁区间是 [resolution.SlotStart, resolution.SlotStart + 32 - 1 + MainSlotCount]，
// 上界为 SlotStart + 32 + MainSlotCount。dnfbridge 侧同一口径见
// currentMainInventoryUnlockedEnd。
//
// fail-open：读不到容器状态、拿不到可证明的 PVF 范围起点、或不是主背包标准 56 格
// 分类（快捷栏 3..8、固定材料 345..359、宠物 7 / 装扮 1 / 守护石 38 页都不按行解锁）
// 时返回 0，即保持旧的无界落点。宁可"少修一次"，也不能因为算错窗口挡掉全部领取
// ——那比原 bug 更糟。
func (o Owner) claimUnlockedMainSlots(
	ctx context.Context,
	characterID string,
	resolution alignedcmd.MailboxItemResolution,
) int16 {
	if o.repos.Settings == nil || strings.TrimSpace(characterID) == "" ||
		resolution.ListType != dnfrepo.MainInventoryListType {
		return 0
	}
	state, found, err := dnfrepo.LoadCharacterContainerState(ctx, o.repos.Settings, characterID)
	if err != nil || !found {
		return 0
	}
	// PVF 范围的起点就是"这一页第 0 格"；起点都拿不到就不可能算出窗口。
	if resolution.SlotStart < 0 || resolution.SlotStart >= mailboxMainLastItemSlot {
		return 0
	}
	// 只有标准分类（65..120 材料 / 121..176 消耗品 / 177..232 任务 …）按行走，
	// 其它区段一律不设界。
	if resolution.SlotEnd-resolution.SlotStart+1 != mailboxMainInventoryCategorySlotCount {
		return 0
	}
	if state.MainSlotCount > mailboxMainInventoryMaxExpansionSlots {
		return 0
	}
	return resolution.SlotStart + mailboxMainInventoryBaseSlotCount + int16(state.MainSlotCount)
}

func (o Owner) clock() time.Time {
	if o.now == nil {
		return time.Now().UTC()
	}
	return o.now().UTC()
}

func addAttachmentToInventory(inventory *dnfrepo.InventoryRecord, attachment dnfrepo.MailAttachment, resolution alignedcmd.MailboxItemResolution) ([]int16, error) {
	return addAttachmentToInventoryBounded(inventory, attachment, resolution, 0)
}

// addAttachmentToInventoryBounded is the same placement with the character's
// unlocked main-inventory rows taken into account.
//
// A claim used to pick purely from the PVF slot range, so an attachment could
// land in a row the character has not bought yet - live 2026-09-15: mail
// attachments were extracted into locked bag slots, where the player cannot even
// click them. unlockedMainSlots here is an **exclusive upper bound** (the last
// unlocked slot + 1), not a count: see claimUnlockedMainSlots. Zero or less means
// "unknown", which keeps the previous unbounded behaviour for the account-cargo
// path and for item pages that are not row-expanded.
func addAttachmentToInventoryBounded(inventory *dnfrepo.InventoryRecord, attachment dnfrepo.MailAttachment, resolution alignedcmd.MailboxItemResolution, unlockedMainSlots int16) ([]int16, error) {
	accountOwned, ownerErr := mailboxAttachmentUsesAccountInventory(resolution)
	if inventory == nil || attachment.ItemID <= 0 || attachment.Count <= 0 ||
		!mailboxClaimSlotRangeValid(resolution) || ownerErr != nil || accountOwned {
		return nil, ErrMailboxInvalidRequest
	}
	return addAttachmentToInventoryRange(inventory, attachment, resolution, unlockedMainSlots)
}

// addAttachmentToAccountInventory reuses the stack/slot allocator while
// keeping account-shared list-0 slots under their account-scoped durable
// aggregate. The wire slot numbers deliberately stay unchanged.
func addAttachmentToAccountInventory(account *dnfrepo.AccountInventoryRecord, attachment dnfrepo.MailAttachment, resolution alignedcmd.MailboxItemResolution) ([]int16, error) {
	accountOwned, ownerErr := mailboxAttachmentUsesAccountInventory(resolution)
	if account == nil || !accountOwned || ownerErr != nil {
		return nil, ErrMailboxInvalidRequest
	}
	inventory := dnfrepo.InventoryRecord{Slots: account.Slots}
	// The account cargo is fully open, so no unlocked-slot bound applies.
	changedSlots, err := addAttachmentToInventoryRange(&inventory, attachment, resolution, 0)
	if err != nil {
		return nil, err
	}
	account.Slots = inventory.Slots
	return changedSlots, nil
}

func addAttachmentToInventoryRange(inventory *dnfrepo.InventoryRecord, attachment dnfrepo.MailAttachment, resolution alignedcmd.MailboxItemResolution, unlockedMainSlots int16) ([]int16, error) {
	if inventory == nil || attachment.ItemID <= 0 || attachment.Count <= 0 || !mailboxClaimSlotRangeValid(resolution) {
		return nil, ErrMailboxInvalidRequest
	}
	if inventory.Slots == nil {
		inventory.Slots = make(map[string]dnfrepo.ItemStack)
	}
	// 附件并进一行**锁定**的同类堆必须被拒绝：装备锁的语义是不可交易/强化/分解/丢弃，
	// 把锁定件数量悄悄加一等于绕过锁。同一容器里同一道具 id 只有一行
	// （CanOpenContainerItemRow 的口径），所以扫一遍候选槽位上的那一行就够。
	for _, slot := range mailboxClaimStackSlots(resolution, unlockedMainSlots) {
		stack, occupied := inventory.Slots[mailboxInventorySlotKey(resolution.ListType, slot)]
		if !occupied || stack.ItemID != attachment.ItemID {
			continue
		}
		if mailboxStackLocked(stack) {
			return nil, fmt.Errorf("%w: list=%d slot=%d item=%d", ErrMailboxItemLocked, resolution.ListType, slot, attachment.ItemID)
		}
	}
	remaining := attachment.Count
	stack := mailboxAttachmentStack(attachment, remaining, resolution)
	// PVF [stack limit] 是这个容器里该道具的总量上限，不是一个格子的上限：额度按容器
	// 里同 id 总量算，而且容器里已经有该道具的行时只能并进那一行（CanOpenContainerItemRow）。
	// 只有声明了 [stack limit] 的可叠加道具受这条约束；装备（每件一行）和没写
	// [stack limit] 的道具沿用旧行为。
	capped := mailboxCarryLimitCapped(resolution)
	newRowLimit := int64(0)
	space := int64(math.MaxInt32)
	if capped {
		newRowLimit = resolution.StackLimit
		space = dnfrepo.ContainerItemSpace(inventory.Slots, resolution.ListType, attachment.ItemID, resolution.StackLimit)
	}
	changedSlots := make([]int16, 0, 1)
	for _, slot := range mailboxClaimStackSlots(resolution, unlockedMainSlots) {
		if remaining <= 0 || space <= 0 {
			break
		}
		key := mailboxInventorySlotKey(resolution.ListType, slot)
		existing, occupied := inventory.Slots[key]
		if !occupied || !canMailStack(existing, stack, resolution) || existing.Count < 0 {
			continue
		}
		capacity := remaining
		stackLimit := mailboxEffectiveStackLimit(resolution)
		if stackLimit > 0 {
			if existing.Count >= stackLimit {
				continue
			}
			capacity = stackLimit - existing.Count
			if capacity > remaining {
				capacity = remaining
			}
		}
		if capacity > space {
			capacity = space
		}
		if capacity <= 0 || existing.Count > math.MaxInt64-capacity {
			return nil, ErrMailboxInvalidRequest
		}
		existing = normalizeMailboxStackClassification(existing, resolution)
		existing.Count += capacity
		updateMailboxStackRawAmount(&existing, resolution)
		inventory.Slots[key] = existing
		changedSlots = append(changedSlots, slot)
		remaining -= capacity
		space -= capacity
	}
	for _, slot := range mailboxClaimStackSlots(resolution, unlockedMainSlots) {
		if remaining <= 0 {
			break
		}
		key := mailboxInventorySlotKey(resolution.ListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		// 容器里已经有该道具的行时不能再开第二行：额度用尽/已有行都停在这里，
		// 余量交给调用方的既有处理（整封信保留，返回 ErrMailboxInventoryFull）。
		if !dnfrepo.CanOpenContainerItemRow(inventory.Slots, resolution.ListType, attachment.ItemID, newRowLimit) {
			break
		}
		count := remaining
		if stackLimit := mailboxEffectiveStackLimit(resolution); stackLimit > 0 && count > stackLimit {
			count = stackLimit
		}
		if capped && count > space {
			count = space
		}
		if count <= 0 {
			break
		}
		inventory.Slots[key] = mailboxAttachmentStack(attachment, count, resolution)
		changedSlots = append(changedSlots, slot)
		remaining -= count
		space -= count
	}
	if remaining > 0 {
		return nil, ErrMailboxInventoryFull
	}
	return changedSlots, nil
}

// mailboxCarryLimitCapped 报告该附件是否受"一个容器里同一道具 id 的总量不得超过
// PVF [stack limit]"约束。装备（每件一行）和 PVF 没写 [stack limit] 的道具不受限。
func mailboxCarryLimitCapped(resolution alignedcmd.MailboxItemResolution) bool {
	return strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") && resolution.StackLimit > 0
}

func mailboxClaimSlotRangeValid(resolution alignedcmd.MailboxItemResolution) bool {
	if resolution.SlotEnd < resolution.SlotStart {
		return false
	}
	switch resolution.ListType {
	case dnfrepo.MainInventoryListType:
		return resolution.SlotStart >= mailboxMainFirstItemSlot && resolution.SlotEnd <= mailboxMainLastItemSlot
	case 1: // avatar inventory
		return resolution.SlotStart >= 0 && resolution.SlotEnd <= 500
	case 7: // creature body/equipment/consumable inventory
		return resolution.SlotStart >= 0 && resolution.SlotEnd <= 238
	case 38: // guild medal and guardian-gem inventory
		return resolution.SlotStart >= 0 && resolution.SlotEnd <= 97
	default:
		return false
	}
}

func mailboxAttachmentUsesAccountInventory(resolution alignedcmd.MailboxItemResolution) (bool, error) {
	if !mailboxClaimSlotRangeValid(resolution) {
		return false, ErrMailboxInvalidRequest
	}
	if resolution.ListType == dnfrepo.MainInventoryListType &&
		resolution.SlotStart >= dnfrepo.CrystalWarehouseFirstSlot && resolution.SlotEnd <= dnfrepo.SoulWarehouseLastSlot {
		return true, nil
	}
	if resolution.ListType == dnfrepo.MainInventoryListType &&
		resolution.SlotStart <= dnfrepo.SoulWarehouseLastSlot && resolution.SlotEnd >= dnfrepo.CrystalWarehouseFirstSlot {
		// One PVF definition may never span a character-owned tab and an
		// account-owned warehouse: that would make a single item have two
		// durable owners.
		return false, ErrMailboxInvalidRequest
	}
	return false, nil
}

// mailboxClaimStackSlots lists the candidate slots one attachment may occupy.
// unlockedMainSlots is the exclusive upper bound computed by
// claimUnlockedMainSlots (the last unlocked slot + 1); the PVF range alone would
// also offer locked rows, which is how a claim used to fill a bag slot the player
// cannot reach. Zero or less keeps the unbounded behaviour. Quick slots 3..8 are
// never row-expanded, so the bound deliberately does not apply to them.
func mailboxClaimStackSlots(resolution alignedcmd.MailboxItemResolution, unlockedMainSlots int16) []int16 {
	slots := make([]int16, 0, 6+int(resolution.SlotEnd-resolution.SlotStart+1))
	accountOwned, _ := mailboxAttachmentUsesAccountInventory(resolution)
	prependQuickSlots := !accountOwned && resolution.ListType == dnfrepo.MainInventoryListType && resolution.PreferQuickSlots && resolution.StackLimit != 1
	mainBounded := !accountOwned && resolution.ListType == dnfrepo.MainInventoryListType && unlockedMainSlots > 0
	if prependQuickSlots {
		for slot := int16(3); slot <= 8; slot++ {
			slots = append(slots, slot)
		}
	}
	for slot := resolution.SlotStart; slot <= resolution.SlotEnd; slot++ {
		if prependQuickSlots && slot >= 3 && slot <= 8 {
			continue
		}
		if mainBounded && slot >= unlockedMainSlots {
			continue
		}
		slots = append(slots, slot)
	}
	return slots
}

func mailboxAttachmentStack(attachment dnfrepo.MailAttachment, count int64, resolution alignedcmd.MailboxItemResolution) dnfrepo.ItemStack {
	extra := cloneStringMap(attachment.Extra)
	// These three keys are mailbox-envelope framing evidence captured when a
	// player sends an item. They are not durable item state, so retaining them
	// would prevent a returned stackable from merging with the same item in the
	// recipient's real inventory page.
	delete(extra, "mailbox_pvf_kind")
	delete(extra, "mailbox_equipment_type")
	delete(extra, "mailbox_pvf_path")
	if extra == nil {
		extra = make(map[string]string)
	}
	// A mail envelope may be generated without the durable item metadata used
	// by the current op13/op14 projection and by item-specific handlers.  Never
	// trust or preserve stale envelope values here: the resolver has already
	// looked up the exact template in the active runtime PVF.
	if kind := strings.ToLower(strings.TrimSpace(resolution.Kind)); kind != "" {
		extra["item_kind"] = kind
	}
	if pvfPath := strings.TrimSpace(resolution.PVFPath); pvfPath != "" {
		extra["pvf_path"] = pvfPath
	}
	switch strings.ToLower(strings.TrimSpace(resolution.Kind)) {
	case "stackable":
		delete(extra, "equipment_type")
		if stackableType := strings.TrimSpace(resolution.StackableType); stackableType != "" {
			extra["stackable_type"] = stackableType
		} else {
			delete(extra, "stackable_type")
		}
		if resolution.StackLimit > 0 {
			extra["stack_limit"] = strconv.FormatInt(resolution.StackLimit, 10)
		} else {
			delete(extra, "stack_limit")
		}
	case "equipment":
		delete(extra, "stackable_type")
		delete(extra, "stack_limit")
		if equipmentType := strings.TrimSpace(resolution.EquipmentType); equipmentType != "" {
			extra["equipment_type"] = equipmentType
		} else {
			delete(extra, "equipment_type")
		}
		if resolution.Durability > 0 {
			extra["max_durability"] = strconv.FormatUint(uint64(resolution.Durability), 10)
			if strings.TrimSpace(extra["durability"]) == "" {
				currentDurability := resolution.Durability
				if len(attachment.RawEntry) == mailboxItemRawSize {
					if rawDurability := binary.LittleEndian.Uint16(attachment.RawEntry[0x0b:0x0d]); rawDurability > 0 {
						currentDurability = rawDurability
					}
				}
				extra["durability"] = strconv.FormatUint(uint64(currentDurability), 10)
			}
		}
	}
	stack := normalizeMailboxStackClassification(dnfrepo.ItemStack{
		ItemID:   attachment.ItemID,
		Count:    count,
		Bind:     attachment.Bind,
		ExpireAt: attachment.ExpireAt,
		RawEntry: append([]byte(nil), attachment.RawEntry...),
		Extra:    extra,
	}, resolution)
	updateMailboxStackRawAmount(&stack, resolution)
	return stack
}

func mailboxInventorySlotKey(listType byte, slot int16) string {
	return fmt.Sprintf("%d:%d", listType, slot)
}

func canMailStack(left, right dnfrepo.ItemStack, resolution alignedcmd.MailboxItemResolution) bool {
	return strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") &&
		resolution.StackLimit != 1 &&
		left.ItemID == right.ItemID &&
		left.Bind == right.Bind &&
		left.ExpireAt.Equal(right.ExpireAt)
}

func mailboxEffectiveStackLimit(resolution alignedcmd.MailboxItemResolution) int64 {
	if !strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") {
		return 1
	}
	if resolution.StackLimit > 0 {
		return resolution.StackLimit
	}
	return math.MaxInt32
}

func normalizeMailboxStackClassification(stack dnfrepo.ItemStack, resolution alignedcmd.MailboxItemResolution) dnfrepo.ItemStack {
	extra := cloneStringMap(stack.Extra)
	if extra == nil {
		extra = make(map[string]string, 4)
	}
	kind := strings.ToLower(strings.TrimSpace(resolution.Kind))
	if kind != "" {
		extra["item_kind"] = kind
	}
	if pvfPath := strings.TrimSpace(strings.ReplaceAll(resolution.PVFPath, "\\", "/")); pvfPath != "" {
		extra["pvf_path"] = pvfPath
	}
	if kind == "stackable" {
		delete(extra, "equipment_type")
		if stackableType := strings.TrimSpace(resolution.StackableType); stackableType != "" {
			extra["stackable_type"] = stackableType
		} else {
			delete(extra, "stackable_type")
		}
		if resolution.StackLimit > 0 {
			extra["stack_limit"] = strconv.FormatInt(resolution.StackLimit, 10)
		} else {
			delete(extra, "stack_limit")
		}
	}
	stack.Extra = extra
	return stack
}

func updateMailboxStackRawAmount(stack *dnfrepo.ItemStack, resolution alignedcmd.MailboxItemResolution) {
	// raw+0x06 is the quantity only for stackables.  For equipment the same
	// dword is the durable instance/quality seed; replacing it with Count makes
	// the current client decode the following reinforce/amplification fields as
	// a malformed multi-item row after a mailbox claim.
	if !strings.EqualFold(strings.TrimSpace(resolution.Kind), "stackable") ||
		stack == nil || stack.Count <= 0 || stack.Count > math.MaxUint32 || len(stack.RawEntry) != mailboxItemRawSize {
		return
	}
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	binary.LittleEndian.PutUint32(stack.RawEntry[0x06:0x0a], uint32(stack.Count))
}

func nextMailID(mails map[string]dnfrepo.MailRecord) (string, error) {
	var max uint64
	for key := range mails {
		value, err := strconv.ParseUint(strings.TrimSpace(key), 10, 32)
		if err != nil {
			continue
		}
		if value > max {
			max = value
		}
	}
	if max >= math.MaxUint32 {
		return "", ErrMailboxInvalidRequest
	}
	return strconv.FormatUint(max+1, 10), nil
}

func mailboxSendSource(req SendRequest) string {
	if len(req.Attachments) > 1 {
		return "op315"
	}
	return "op94"
}

func currentMailboxPostage(gold int64, attachmentCount int, attachmentValue int64) (int64, error) {
	if gold < 0 || attachmentCount < 0 || attachmentValue < 0 {
		return 0, ErrMailboxInvalidRequest
	}
	if attachmentCount > (math.MaxInt64-100)/1000 {
		return 0, ErrMailboxInvalidRequest
	}
	postage := int64(100 + attachmentCount*1000)
	for _, surcharge := range []int64{gold / 20, attachmentValue / 20} {
		if postage > math.MaxInt64-surcharge {
			return 0, ErrMailboxInvalidRequest
		}
		postage += surcharge
	}
	return postage, nil
}

func mailExpired(mail dnfrepo.MailRecord, now time.Time) bool {
	if mailboxMailSaved(mail) || mailboxMailUnlimited(mail) || now.IsZero() {
		return false
	}
	expiresAt := mail.ExpireAt
	if expiresAt.IsZero() && !mail.CreatedAt.IsZero() {
		expiresAt = mail.CreatedAt.Add(mailboxNormalLifetime)
	}
	return !expiresAt.IsZero() && now.After(expiresAt)
}

func mailboxMailSaved(mail dnfrepo.MailRecord) bool {
	return strings.EqualFold(strings.TrimSpace(mail.Metadata["mailbox_saved"]), "true")
}

func mailboxMailUnlimited(mail dnfrepo.MailRecord) bool {
	return strings.EqualFold(strings.TrimSpace(mail.Metadata["system_mail"]), "true") &&
		strings.EqualFold(strings.TrimSpace(mail.Metadata["unlimited"]), "true")
}

func sortMailboxMails(mails []dnfrepo.MailRecord) {
	sort.SliceStable(mails, func(i, j int) bool {
		left, right := mails[i], mails[j]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		leftID, leftErr := strconv.ParseUint(left.MailID, 10, 32)
		rightID, rightErr := strconv.ParseUint(right.MailID, 10, 32)
		if leftErr == nil && rightErr == nil {
			return leftID < rightID
		}
		return left.MailID < right.MailID
	})
}

func cloneMailboxMail(in dnfrepo.MailRecord) dnfrepo.MailRecord {
	out := in
	out.Attachments = append([]dnfrepo.MailAttachment(nil), in.Attachments...)
	for index := range out.Attachments {
		out.Attachments[index].RawEntry = append([]byte(nil), in.Attachments[index].RawEntry...)
		out.Attachments[index].Extra = cloneStringMap(in.Attachments[index].Extra)
	}
	out.Metadata = cloneStringMap(in.Metadata)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func characterIDFromRequest(reqCharacterID uint16) (string, error) {
	if reqCharacterID == 0 {
		return "", ErrMailboxCharacterMissing
	}
	return strconv.FormatUint(uint64(reqCharacterID), 10), nil
}

// claimCandidate is one letter an op95 request touches. A request addresses
// either the whole letter - which is what a gold-only row and every older client
// build sends - or a set of attachment slots.
type claimCandidate struct {
	id          uint32
	claimObject uint32
	key         string
	mail        dnfrepo.MailRecord
	slots       []int
	wholeLetter bool
	claimed     map[int]struct{}
}

// requestedSlots lists the attachment slots this claim addresses, in letter
// order.
func (c claimCandidate) requestedSlots() []int {
	if c.wholeLetter || len(c.slots) == 0 {
		out := make([]int, 0, len(c.mail.Attachments))
		for index := range c.mail.Attachments {
			out = append(out, index)
		}
		return out
	}
	out := append([]int(nil), c.slots...)
	sort.Ints(out)
	return out
}

// claimsGold reports whether this claim also hands over the letter's gold. A
// single-slot claim leaves the gold with the letter so the row stays claimable.
func (c claimCandidate) claimsGold() bool {
	return c.wholeLetter && c.mail.Gold > 0
}

// acknowledgedRows lists what op95 must report back for this letter: one row per
// slot actually handed over, each carrying the same claim object the summary row
// gave that slot, so the client can retire exactly those slots from an open
// letter. A letter that only gave up its gold is reported through the claim
// object the client sent, which is the letter id for such a row.
func (c claimCandidate) acknowledgedRows() []ClaimedAttachment {
	rows := make([]ClaimedAttachment, 0, len(c.claimed))
	seen := make(map[uint32]struct{}, len(c.claimed))
	for _, index := range c.requestedSlots() {
		if _, ok := c.claimed[index]; !ok {
			continue
		}
		// The same addressing the summary row used, including its fallback for a
		// slot that cannot be encoded: several such slots share the letter id, so
		// they are reported once.
		claimObject := mailboxSlotClaimObject(c.id, index)
		if _, duplicate := seen[claimObject]; duplicate {
			continue
		}
		seen[claimObject] = struct{}{}
		rows = append(rows, ClaimedAttachment{LetterID: c.id, ClaimObject: claimObject})
	}
	if len(rows) == 0 {
		rows = append(rows, ClaimedAttachment{LetterID: c.id, ClaimObject: c.claimObject})
	}
	return rows
}

// attachmentsAfterClaim empties the claimed slots while keeping every position,
// so a claim object the client still holds for a pending slot keeps addressing
// the same item.
func (c claimCandidate) attachmentsAfterClaim() []dnfrepo.MailAttachment {
	out := make([]dnfrepo.MailAttachment, len(c.mail.Attachments))
	copy(out, c.mail.Attachments)
	for index := range c.claimed {
		if index >= 0 && index < len(out) {
			out[index] = dnfrepo.MailAttachment{}
		}
	}
	return out
}

// mailboxHasAttachment reports whether any slot still holds an item.
func mailboxHasAttachment(attachments []dnfrepo.MailAttachment) bool {
	for _, attachment := range attachments {
		if attachment.ItemID > 0 && attachment.Count > 0 {
			return true
		}
	}
	return false
}

// mailboxClaimErrorCode maps a rejected op95 claim onto the codes the client
// actually renders. 86JP MailboxHandler.ToClientErrorCode keeps a separate table
// for ackType 0x005F: 4 means the attachment could not be inserted, after which
// the client runs its own inventory check and draws the no-space or carry-limit
// message; 10 is the gold carry limit, 19 a trade restriction and 21 a mail that
// no longer exists. The send-side table (mailboxErrorCode) is a different one and
// stays untouched - reusing it here made a full-tab rejection arrive as code 6,
// which is outside the claim table, so the client drew nothing and the button
// looked dead.
func mailboxClaimErrorCode(err error) byte {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrMailboxInventoryFull), errors.Is(err, ErrMailboxAssetMismatch),
		errors.Is(err, ErrMailboxItemLocked):
		// 码 4 在 op95 的失败表里是 dstr 2057「物品栏空间不足」——这正是背包放不下时
		// 该显示的那句。锁定附件目前共用这一格：op95 的表只有
		// {4,10,19,21,60,119,219,231} 有文案，其余（含 214）默认分支是 `pop ebp; ret`，
		// 也就是说锁定在 op95 上**没有**能渲染"物品已锁定"的码。
		// 现场证据（2026-09-15）：领取被拒时服务端回 0004，客户端什么都没画 ——
		// 玩家看到的仍是"没反应"，但安全性不受影响（附件留在信里）。
		return 4
	case errors.Is(err, ErrMailboxGoldInsufficient):
		return 10
	case errors.Is(err, ErrMailboxMailNotFound), errors.Is(err, ErrMailboxAlreadyClaimed), errors.Is(err, ErrMailboxExpired):
		return 21
	default:
		return mailboxErrorCode(err)
	}
}

func mailboxErrorCode(err error) byte {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrMailboxRecipientMissing):
		return 2
	case errors.Is(err, ErrMailboxMailNotFound):
		return 3
	case errors.Is(err, ErrMailboxAlreadyClaimed):
		return 4
	case errors.Is(err, ErrMailboxExpired):
		return 5
	case errors.Is(err, ErrMailboxInventoryFull):
		return 6
	case errors.Is(err, ErrMailboxGoldInsufficient), errors.Is(err, ErrMailboxAssetMismatch):
		return 7
	case errors.Is(err, ErrMailboxSelfRecipient):
		return 7
	case errors.Is(err, ErrMailboxSavedFull):
		return 22
	case errors.Is(err, ErrMailboxItemResolverMissing), errors.Is(err, ErrMailboxSpecialUnsupported):
		return 7
	default:
		return 1
	}
}

func isMissingRepositoryError(err error) bool {
	return errors.Is(err, ErrMailboxRepoMissing) ||
		errors.Is(err, ErrMailboxAssetsUOW) ||
		errors.Is(err, dnfrepo.ErrMailboxAssetTransactionUnavailable)
}
