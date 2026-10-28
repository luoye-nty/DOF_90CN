package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentRaidRewardOverflowMailSource 是这些邮件的 source 标记，也是溢出行的
// extra 标记（currentOverflowMailAttachments 的 source 参数），排查时按它反查。
const currentRaidRewardOverflowMailSource = "raid_reward_overflow"

// deliverRuntimeRaidRewardOverflowMail 把结算时背包那一栏放不下的奖励行改成邮件发出去。
//
// 为什么需要它：`commitRuntimeRaidSettlementRewards` 用一个事务罩住**全队**，而
// `grantCurrentCeraShopProduct` 在放不下时返回 `errDungeonPickupInventoryFull`。
// 改动前那一行错误直接 `return` 出闭包 —— 一个人背包满，全队的奖励一起回滚、回执也不落，
// 客户端的牌就停在"翻不开"。现在放不下的**那一行**记进回执的 Overflow，走这里补发。
//
// 三个关键约束：
//
//  1. **必须在事务外**。`WithinRaidRewards` 只给出 Inventory + Settings，邮件走
//     `WithinMailboxAssets`，是两个工作单元，不能嵌套。奖励已经 durable，所以这里
//     失败只记日志、不回滚（与 currentUpgradeRefundMailer 同一口径）。
//  2. **幂等**。CompletionKey 由回执 scope + 成员拼出来，`AppendSystemMail` 撞到同一个
//     key 会直接返回既有邮件的 id，不会发第二封 —— 所以"回执已落、邮件没发完"这个
//     窗口靠下一次重投就能补上，且不会重复。
//  3. **不加玩家可见提示**。只落邮件，不发"背包已满"的弹窗/通知；玩家在邮箱里看到
//     附件即可。
func (s *Service) deliverRuntimeRaidRewardOverflowMail(
	ctx context.Context,
	receiptScope string,
	receipt runtimeRaidRewardReceipt,
) {
	if s == nil || len(receipt.Members) == 0 {
		return
	}
	pending := false
	for _, member := range receipt.Members {
		if len(member.Overflow) != 0 {
			pending = true
			break
		}
	}
	if !pending {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		s.logPacketEvent("game-raid-reward-overflow-mail-unavailable",
			"raid_key", receipt.RaidKey, "raid_phase", receipt.Phase,
			"error", "mailbox asset transaction is unavailable")
		return
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logPacketEvent("game-raid-reward-overflow-mail-catalog-unavailable",
			"raid_key", receipt.RaidKey, "raid_phase", receipt.Phase, "error", err)
		return
	}
	now := time.Now().UTC()
	for _, member := range receipt.Members {
		if len(member.Overflow) == 0 {
			continue
		}
		characterID := strconv.Itoa(int(member.CharacterID))
		attachments := make([]dnfrepo.MailAttachment, 0, len(member.Overflow))
		for _, row := range member.Overflow {
			if row.ItemID <= 0 || row.ItemID > math.MaxUint32 || row.Count <= 0 {
				s.logPacketEvent("game-raid-reward-overflow-row-skipped",
					"raid_key", receipt.RaidKey, "character_id", characterID,
					"item_id", row.ItemID, "count", row.Count,
					"reason", "overflow_row_out_of_range")
				continue
			}
			// 一行解不出来不能让这一位丢掉其它行。
			built, buildErr := currentOverflowMailAttachments(
				catalog, uint32(row.ItemID), row.Count, currentRaidRewardOverflowMailSource)
			if buildErr != nil {
				s.logPacketEvent("game-raid-reward-overflow-row-skipped",
					"raid_key", receipt.RaidKey, "character_id", characterID,
					"item_id", row.ItemID, "count", row.Count, "error", buildErr)
				continue
			}
			attachments = append(attachments, built...)
		}
		if len(attachments) == 0 {
			continue
		}
		completionKey := fmt.Sprintf("%s:overflow:%d", receiptScope, member.CharacterID)
		mailID := ""
		err := repositories.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
			_ dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			_ dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			id, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: characterID,
				Title:                "攻坚奖励（背包已满）",
				Body:                 "结算时背包对应道具分页没有空位，未能放入的攻坚奖励已随本邮件发送。请清理该分页后领取。",
				Source:               currentRaidRewardOverflowMailSource,
				CompletionKey:        completionKey,
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
			s.logPacketEvent("game-raid-reward-overflow-mail-failed",
				"raid_key", receipt.RaidKey, "raid_phase", receipt.Phase,
				"character_id", characterID, "rows", len(member.Overflow),
				"attachments", len(attachments), "error", err)
			continue
		}
		s.logPacketEvent("game-raid-reward-overflow-mailed",
			"raid_key", receipt.RaidKey,
			"raid_phase", receipt.Phase,
			"character_id", characterID,
			"mail_id", mailID,
			"rows", len(member.Overflow),
			"attachments", len(attachments),
			"reason", "the_bag_category_was_full_so_the_row_was_mailed_instead",
			"source", "raid_reward_overflow")
	}
}
