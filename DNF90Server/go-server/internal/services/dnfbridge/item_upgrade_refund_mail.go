package dnfbridge

import (
	"context"
	"errors"
	"math"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	errCurrentUpgradeRefundMailUnavailable = errors.New("current upgrade destroy refund mail unavailable")
)

// currentUpgradeRefundMailer delivers the destruction refund rows the bag had
// no room for. Only the disassembly refund this server adds on top of the PVF
// compensation is routed here (see currentUpgradeDestroyDisjointRewards); the
// upgrade.etc [disjoint bonus item] compensation keeps its strict behaviour.
//
// It cannot share the upgrade's transaction: WithinCharacterAssets covers
// characters, inventory and equipment, mailboxes have their own unit of work,
// and the destroyed target may sit on the equipment page. The owner therefore
// calls this once the upgrade is durable. A failure here is logged and
// swallowed -- undoing a committed destruction to save a bonus row would be
// the worse outcome.
func (s *Service) currentUpgradeRefundMailer() alignedcmd.UpgradeRefundMailer {
	return func(ctx context.Context, characterID string, rows []alignedcmd.UpgradeDestroyReward) error {
		if s == nil || characterID == "" || len(rows) == 0 {
			return nil
		}
		repositories, ok := s.repositoryGroup()
		if !ok || repositories.MailboxAssets == nil {
			s.logPacketEvent("dnf-upgrade-refund-mail-unavailable",
				"character_id", characterID, "rows", len(rows))
			return errCurrentUpgradeRefundMailUnavailable
		}
		catalog, err := s.currentPVFItemCatalog()
		if err != nil {
			s.logPacketEvent("dnf-upgrade-refund-mail-catalog-unavailable",
				"character_id", characterID, "rows", len(rows), "error", err)
			return err
		}
		attachments := make([]dnfrepo.MailAttachment, 0, len(rows))
		for _, row := range rows {
			if row.ItemID <= 0 || row.ItemID > math.MaxUint32 || row.Count <= 0 {
				continue
			}
			built, buildErr := currentOverflowMailAttachments(
				catalog, uint32(row.ItemID), int64(row.Count), "upgrade_destroy_refund")
			if buildErr != nil {
				// One unresolvable row must not cost the player the others.
				s.logPacketEvent("dnf-upgrade-refund-mail-row-skipped",
					"character_id", characterID, "item_id", row.ItemID,
					"count", row.Count, "error", buildErr)
				continue
			}
			attachments = append(attachments, built...)
		}
		if len(attachments) == 0 {
			return errCurrentUpgradeRefundMailUnavailable
		}
		now := time.Now().UTC()
		mailID := ""
		err = repositories.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
			_ dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			_ dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			id, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID,
				Title:                "背包已满：装备损坏返还",
				Body:                 "强化/增幅失败导致装备损坏，背包空间不足未能放入的返还物品已通过邮件发送。请清理对应道具分页后领取。",
				Source:               "upgrade_destroy_refund_inventory_full",
				Attachments:          attachments,
				CreatedAt:            now,
			})
			if mailErr != nil {
				return mailErr
			}
			mailID = id
			return nil
		})
		if err != nil {
			s.logPacketEvent("dnf-upgrade-refund-mail-failed",
				"character_id", characterID, "rows", len(rows),
				"attachments", len(attachments), "error", err)
			return err
		}
		s.logPacketEvent("dnf-upgrade-refund-mailed",
			"character_id", characterID, "mail_id", mailID,
			"rows", len(rows), "attachments", len(attachments))
		return nil
	}
}
