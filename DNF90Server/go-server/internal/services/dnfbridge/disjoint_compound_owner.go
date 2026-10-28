package dnfbridge

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	dnfdisjoint "longheng.io/server/internal/modules/dnf/disjoint"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentDisjointResolver turns one consumed source into its reward rows. The
// source stack is passed alongside the PVF definition because the rewards
// depend on instance state (an otherverse aura) and not only on the template.
type currentDisjointResolver func(dungeonDropItemDefinition, *dnfpvf.Document, dnfrepo.ItemStack) ([]currentDisjointReward, error)

func (s *Service) currentDisjointMutationOwner(session *gameSession) (string, string, *dnfdisjoint.Owner, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return "", "", nil, errCurrentDisjointUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.AccountAssets == nil {
		return "", "", nil, errCurrentDisjointUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return "", "", nil, errCurrentDisjointUnavailable
	}
	owner, err := dnfdisjoint.NewOwner(repositories)
	if err != nil {
		return "", "", nil, errCurrentDisjointUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	return accountID, characterID, owner, nil
}

func currentDisjointMutationError(err error) error {
	switch {
	case errors.Is(err, dnfdisjoint.ErrOwnerUnavailable),
		errors.Is(err, dnfdisjoint.ErrAccountRequired),
		errors.Is(err, dnfdisjoint.ErrCharacterRequired),
		errors.Is(err, dnfdisjoint.ErrCharacterNotFound),
		errors.Is(err, dnfdisjoint.ErrAccountMismatch),
		errors.Is(err, dnfdisjoint.ErrInventoryNotFound):
		return errors.Join(errCurrentDisjointUnavailable, err)
	default:
		return err
	}
}

// commitCurrentDisjoint consumes one equipment instance and grants its
// resolved PVF rows through the domain-owned account+character transaction.
func (s *Service) commitCurrentDisjoint(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	sourceSlot int16,
	listType byte,
	resolve currentDisjointResolver,
) (currentDisjointResult, error) {
	results, err := s.commitCurrentDisjointBatch(ctx, session, catalog, []int16{sourceSlot}, listType, resolve)
	if err != nil {
		return currentDisjointResult{}, err
	}
	if len(results) != 1 {
		return currentDisjointResult{}, errCurrentDisjointUnavailable
	}
	return results[0], nil
}

// commitCurrentDisjointBatch resolves, consumes, and grants every selected
// source inside one account+character transaction. Any invalid or stale source
// rolls the complete batch back; no partial disassembly is exposed.
func (s *Service) commitCurrentDisjointBatch(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	sourceSlots []int16,
	listType byte,
	resolve currentDisjointResolver,
) ([]currentDisjointResult, error) {
	if session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.source == nil || resolve == nil {
		return nil, errCurrentDisjointUnavailable
	}
	if len(sourceSlots) == 0 || len(sourceSlots) > currentDisjointBatchMaxItems {
		return nil, errCurrentDisjointRequestInvalid
	}
	accountID, characterID, owner, err := s.currentDisjointMutationOwner(session)
	if err != nil {
		return nil, err
	}

	results := make([]currentDisjointResult, 0, len(sourceSlots))
	command := dnfdisjoint.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		Project: func(assets *dnfdisjoint.Assets) (dnfdisjoint.Changes, error) {
			inventory := assets.Inventory
			account := assets.AccountInventory
			accountDirty := false
			overflowRewards := []currentDisjointReward(nil)
			for _, sourceSlot := range sourceSlots {
				key := currentCeraShopInventorySlotKey(listType, sourceSlot)
				source, found := inventory.Slots[key]
				validCount := source.Count == 1
				if listType == currentAvatarInventoryListType {
					// Durable avatar rows may keep zero in the wire count field
					// while the slot still owns one non-stackable instance.
					validCount = source.Count == 0 || source.Count == 1
				}
				if !found || source.ItemID <= 0 || source.ItemID > math.MaxUint32 || !validCount {
					return dnfdisjoint.Changes{}, errCurrentDisjointSourceInvalid
				}
				// 锁定（ALT 锁 / 解锁申请中）的装备不允许被分解：和出售、丢弃、
				// 交易、邮寄走同一条 equipment_lock_state 口径。这里必须单独报错，
				// 否则"被锁"会被当成"物品不存在"一样静默丢掉，玩家什么都看不到。
				if currentNPCShopItemLocked(source) {
					return dnfdisjoint.Changes{}, errCurrentDisjointSourceLocked
				}
				definition, err := catalog.ResolveItem(uint32(source.ItemID))
				if err != nil || (!definition.ExpirationDate.IsZero() && !time.Now().UTC().Before(definition.ExpirationDate)) {
					return dnfdisjoint.Changes{}, errCurrentDisjointSourceInvalid
				}
				document, err := parseDungeonCardPVFDocument(catalog.source, definition.PVFPath)
				if err != nil {
					return dnfdisjoint.Changes{}, errors.Join(errCurrentDisjointSourceInvalid, err)
				}
				rewards, err := resolve(definition, document, source)
				if err != nil {
					return dnfdisjoint.Changes{}, err
				}
				if len(rewards) == 0 || len(rewards) > currentDisjointMaxRewards {
					return dnfdisjoint.Changes{}, errCurrentDisjointRewardInvalid
				}
				delete(inventory.Slots, key)
				// The mailbox rides in this transaction, so a full bag no
				// longer has to abort the disassembly: rewards that do not fit
				// are posted as mail atomically with the source's removal.
				// Without a mailbox the old strict behaviour stands -- better
				// to refuse and let the player clear space than to consume the
				// item for nothing.
				var overflow *[]currentDisjointReward
				if assets.Mailbox != nil {
					overflow = &overflowRewards
				}
				rewardSlots, dirty, err := currentGrantDisjointRewards(inventory, account, catalog, rewards, overflow, s.currentInventoryPlacementMainSlotCount(ctx, inventory, characterID))
				if err != nil {
					return dnfdisjoint.Changes{}, err
				}
				accountDirty = accountDirty || dirty
				results = append(results, currentDisjointResult{Rewards: rewardSlots})
			}
			if len(overflowRewards) > 0 {
				if err := s.appendCurrentDisjointOverflowMail(ctx, assets.Mailbox, catalog, characterID, overflowRewards); err != nil {
					return dnfdisjoint.Changes{}, err
				}
			}
			return dnfdisjoint.Changes{AccountInventory: accountDirty, Inventory: true}, nil
		},
	}
	switch listType {
	case 0:
		err = owner.DisjointEquipment(ctx, command)
	case currentAvatarInventoryListType:
		err = owner.DisjointAvatar(ctx, command)
	default:
		err = errCurrentDisjointRequestInvalid
	}
	if err != nil {
		return nil, currentDisjointMutationError(err)
	}
	return results, nil
}

// appendCurrentDisjointOverflowMail posts the disassembly rewards the bag could
// not take. It runs inside the disjoint transaction (dnfdisjoint.Assets carries
// the mailbox for exactly this), so the source's removal and this letter commit
// or roll back together -- the disassembly consumes the item, and a two-phase
// delivery could otherwise take the equipment and lose the rewards.
func (s *Service) appendCurrentDisjointOverflowMail(
	ctx context.Context,
	mailboxes dnfrepo.MailboxRepository,
	catalog *pvfDungeonDropCatalog,
	characterID string,
	rewards []currentDisjointReward,
) error {
	if mailboxes == nil || catalog == nil || characterID == "" || len(rewards) == 0 {
		return errCurrentDisjointRewardInvalid
	}
	attachments := make([]dnfrepo.MailAttachment, 0, len(rewards))
	for _, reward := range rewards {
		built, err := currentOverflowMailAttachments(
			catalog, reward.ItemID, int64(reward.Count), "equipment_disjoint_refund")
		if err != nil {
			// Rolling back would cost the player the whole disassembly, so a
			// row we cannot express is dropped and logged instead.
			s.logPacketEvent("dnf-disjoint-overflow-mail-row-skipped",
				"character_id", characterID, "item_id", reward.ItemID,
				"count", reward.Count, "error", err)
			continue
		}
		attachments = append(attachments, built...)
	}
	if len(attachments) == 0 {
		return errCurrentDisjointRewardInvalid
	}
	mailID, err := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
		RecipientCharacterID: characterID,
		Title:                "背包已满：装备分解所得",
		Body:                 "分解装备时背包空间不足，未能放入的材料已通过邮件发送。请清理对应道具分页后领取。",
		Source:               "equipment_disjoint_inventory_full",
		Attachments:          attachments,
		CreatedAt:            time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	s.logPacketEvent("dnf-disjoint-overflow-mailed",
		"character_id", characterID, "mail_id", mailID,
		"rows", len(rewards), "attachments", len(attachments))
	return nil
}
