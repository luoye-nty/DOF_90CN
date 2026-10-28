package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/text/encoding/simplifiedchinese"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

// 商城「赠送」的投递与下单判定。
//
// 口径（用户 2026-09-27 确认）：
//
//	A 在商城买一件东西送给 B  ⇒  A 付钱、商品**邮寄给 B**、活动奖励归 A。
//
// 与普通购买共用**同一套结账**（commitCurrentCeraShopPurchase）：价钱、限购、
// 多买多送计数、点券瀑布全都不变，唯一的区别是"商品落进谁的背包"——
// 赠送形态下这一类落格被改写成邮件附件（result.GiftAttachments），
// 由本文件的 deliverCurrentCeraShopGift 投递。
//
// 为什么不是"先买给自己再转寄"：那会在付款方背包里留下中间态，任何一步失败都会
// 产生"已扣钱但东西在别人背包/在邮件里/两边都有"的分支。这里只做一次结账，
// 目标容器从一开始就是收件人的邮箱。
//
// 为什么不做成一个跨库大事务：钱包/角色在 CeraShopAssets 的库里，邮箱在
// MailboxAssets 的库里（可以不同 shard），**不可能**同一个 SQL 事务。
// 所以顺序固定为"先扣钱、后投递"，投递失败就退款——绝不反过来
// （先给东西再扣钱，一旦扣钱失败就是白送，经济上不可逆）。

const (
	// currentCeraShopGiftMailTitle / Body 是收件人邮箱里显示的文字。
	currentCeraShopGiftMailTitle = "商城赠送"
	// currentCeraShopGiftMailBody 里的 %s 是赠送方角色名。
	currentCeraShopGiftMailBodyFormat = "%s 通过商城赠送给您的礼物，请查收。"
	// currentCeraShopGiftMailSource 写进邮件 Metadata 的 source，用于对账/追溯。
	currentCeraShopGiftMailSource = "cera_shop_gift"
	// currentCeraShopGiftSenderNameMaxBytes 是邮件摘要行里寄件人名字的字节预算，
	// 与 mail 包 wire builder 用的 maxMailboxRecipientBytes 保持一致（30 字节 GB18030）。
	currentCeraShopGiftSenderNameMaxBytes = 30
)

var (
	errCurrentCeraShopGiftNotGiftable     = errors.New("dnf cera shop gift item cannot be delivered by mail")
	errCurrentCeraShopGiftRecipientGone   = errors.New("dnf cera shop gift recipient is unavailable")
	errCurrentCeraShopGiftSenderMissing   = errors.New("dnf cera shop gift sender is unavailable")
	errCurrentCeraShopGiftMailUnavailable = errors.New("dnf cera shop gift mailbox is unavailable")
)

// currentCeraShopGiftDeliverySequence 给每封赠送邮件一个进程内唯一的序号。
//
// 和 now.UnixNano() 一起进 completion_key：序号防同一纳秒内的两次调用，
// 时间戳防进程重启后序号从头开始又撞上历史键。
var currentCeraShopGiftDeliverySequence atomic.Uint64

func currentCeraShopGiftDeliveryNonce() uint64 {
	return currentCeraShopGiftDeliverySequence.Add(1)
}

// currentCeraShopGiftConsumedAtCheckout 判定一件商品是不是"结账即消费"型。
//
// 这类商品没有可以邮寄的实物：契约/礼包时长直接写付款方账号，改名卡写成角色的
// 待用状态，各种扩容改的是付款方角色/账号的容器容量，装扮券兑换是扣付款方背包里的券。
// 赠送形态一旦放行它们，正好落进"送给别人 = 给自己买"这条红线，所以必须在
// **扣钱之前**整笔拒绝（见 commitCurrentCeraShopPurchase 里赠送形态的守卫）。
func currentCeraShopGiftConsumedAtCheckout(purchase currentCeraShopResolvedPurchase) bool {
	return purchase.PremiumType > 0 ||
		purchase.PremiumPackage ||
		purchase.NameTagDurationSeconds > 0 ||
		purchase.CharacterSlotExpansion ||
		purchase.CharacterRenameEntitlement ||
		purchase.MainInventoryUpgradeStage != 0 ||
		purchase.AvatarInventoryUpgradeStage != 0 ||
		purchase.PersonalCargoUpgradeTarget != 0 ||
		purchase.AccountCargoUpgrade ||
		purchase.AvatarCouponItemID != 0
}

// currentCeraShopGiftAttachments 把一件道具定义转成邮件附件（判定 + 构造）。
//
// 判定即投递能力：能被 mailbox 表达成实例行（装备/可叠加）的道具才可赠送。
// 判定不过就返回 errCurrentCeraShopGiftNotGiftable。
func currentCeraShopGiftAttachments(
	definition dungeonDropItemDefinition,
	count uint32,
	commodityID uint32,
) ([]dnfrepo.MailAttachment, error) {
	if count == 0 || definition.ItemID == 0 {
		return nil, fmt.Errorf("%w: commodity=%d item=%d count=%d", errCurrentCeraShopGiftNotGiftable, commodityID, definition.ItemID, count)
	}
	// 复活币是钱包（角色 1 号格），不是可邮寄的道具实例：邮寄出去会在领取时
	// 变成一行普通背包道具，等于把钱包值换成实物。直接判为不可赠送。
	if dnfrevivecoin.IsReward(int64(definition.ItemID)) {
		return nil, fmt.Errorf("%w: commodity=%d item=%d is a revive-coin wallet reward", errCurrentCeraShopGiftNotGiftable, commodityID, definition.ItemID)
	}
	built, err := currentCeraPackageOverflowAttachments(definition, count, commodityID)
	if err != nil {
		return nil, fmt.Errorf("%w: commodity=%d item=%d: %v", errCurrentCeraShopGiftNotGiftable, commodityID, definition.ItemID, err)
	}
	for index := range built {
		if built[index].Extra == nil {
			built[index].Extra = make(map[string]string, 4)
		}
		// 覆写来源标记：这是商城赠送，不是礼包溢出。
		built[index].Extra["source"] = currentCeraShopGiftMailSource
		built[index].Extra["last_grant_source"] = currentCeraShopGiftMailSource
		delete(built[index].Extra, "cera_package_overflow_mail")
	}
	return built, nil
}

// appendCurrentCeraShopGiftGrant 把一件"本该落进付款方背包"的商品转成邮件附件。
//
// 判定即投递能力：能被 mailbox 表达成实例行（装备/可叠加）的道具才可赠送。
// 判定不过就返回 errCurrentCeraShopGiftNotGiftable —— 它发生在结账事务**内部**，
// 所以整笔结账回滚、点券不会被扣（这正是"该商品无法赠送"应该有的行为）。
//
// ★ 时装走**真正的落格函数**，不走通用转换：`grantCurrentCeraShopAvatar` 还要写
// avatar_ability_no（玩家选的属性）、durability、avatar_duration_days（买的是 30 天
// 还是永久）、item_kind=avatar、以及作者化的装扮槽位。用 PVF 定义直接构造附件会把这些
// 全丢掉 —— 现场后果就是"买 30 天时装送人，对方领到一件永久时装"。所以在**临时背包**
// 上跑一遍真正的落格，再把它产出的那一行原样当附件：收件人拿到的 = 付款方本该拿到的。
func appendCurrentCeraShopGiftGrant(
	attachments *[]dnfrepo.MailAttachment,
	notes *[]string,
	source dnfpvf.Source,
	definition dungeonDropItemDefinition,
	product currentCeraShopProduct,
	attributeValue byte,
	count uint32,
) error {
	if attachments == nil || notes == nil {
		return fmt.Errorf("%w: commodity=%d item=%d has no collector", errCurrentCeraShopGiftNotGiftable, product.CommodityID, definition.ItemID)
	}
	if product.Section == "avatar" {
		built, err := currentCeraShopGiftAvatarAttachments(source, definition, product, attributeValue, count)
		if err != nil {
			return err
		}
		*attachments = append(*attachments, built...)
		*notes = append(*notes, fmt.Sprintf("commodity:%d:item:%d:x%d", product.CommodityID, definition.ItemID, count))
		return nil
	}
	built, err := currentCeraShopGiftAttachments(definition, count, product.CommodityID)
	if err != nil {
		return err
	}
	*attachments = append(*attachments, built...)
	*notes = append(*notes, fmt.Sprintf("commodity:%d:item:%d:x%d", product.CommodityID, definition.ItemID, count))
	return nil
}

// currentCeraShopGiftAvatarAttachments 在临时背包上跑真正的时装落格，把产出的行
// 原样转成邮件附件（含时长、玩家选的属性、装扮槽位、原始行）。
func currentCeraShopGiftAvatarAttachments(
	source dnfpvf.Source,
	definition dungeonDropItemDefinition,
	product currentCeraShopProduct,
	attributeValue byte,
	count uint32,
) ([]dnfrepo.MailAttachment, error) {
	if source == nil || count == 0 {
		return nil, fmt.Errorf("%w: commodity=%d item=%d has no PVF source", errCurrentCeraShopGiftNotGiftable, product.CommodityID, definition.ItemID)
	}
	scratch := dnfrepo.InventoryRecord{CharacterID: "-", Slots: make(map[string]dnfrepo.ItemStack, count)}
	attachments := make([]dnfrepo.MailAttachment, 0, count)
	for granted := uint32(0); granted < count; granted++ {
		slot, err := grantCurrentCeraShopAvatar(&scratch, source, definition, product, attributeValue)
		if err != nil {
			return nil, fmt.Errorf("%w: commodity=%d item=%d avatar grant: %v", errCurrentCeraShopGiftNotGiftable, product.CommodityID, definition.ItemID, err)
		}
		stack, found := scratch.Slots[currentCeraShopInventorySlotKey(1, slot)]
		if !found || stack.ItemID <= 0 {
			return nil, fmt.Errorf("%w: commodity=%d item=%d avatar grant produced no row", errCurrentCeraShopGiftNotGiftable, product.CommodityID, definition.ItemID)
		}
		if len(stack.RawEntry) == 0 {
			return nil, fmt.Errorf("%w: commodity=%d item=%d avatar row has no native bytes", errCurrentCeraShopGiftNotGiftable, product.CommodityID, definition.ItemID)
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 4)
		}
		stack.Extra["source"] = currentCeraShopGiftMailSource
		stack.Extra["last_grant_source"] = currentCeraShopGiftMailSource
		attachments = append(attachments, dnfrepo.MailAttachment{
			ItemID:   stack.ItemID,
			Count:    stack.Count,
			Bind:     stack.Bind,
			ExpireAt: stack.ExpireAt,
			RawEntry: append([]byte(nil), stack.RawEntry...),
			Extra:    stack.Extra,
		})
	}
	return attachments, nil
}

// currentCeraShopGiftFailureCode 把赠送判定/投递的错误翻成客户端的失败码。
//
// ⚠ op64 只能查**通用购买错误表**（`247 + code`，实测：code 11 → 258 点券不足），
// 那张表里没有赠送专用文案 —— 所以这里全部是"最接近的一条"，精确文案在 op136 那一步。
//
//	 6 → 253 无法购买的物品（兜底）
//	11 → 258 点券不足
//	18 → 265 未知错误
func currentCeraShopGiftFailureCode(err error) byte {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, dnfcerashop.ErrWalletInsufficient), errors.Is(err, dnfcerashop.ErrCeraInsufficient):
		// 支付瀑布（代币券→代币券→点券）整条都不够。**实测确认** 11 显示为
		// 「错误 : 点券不足」——这条是所有商城购买共用的判定，赠送也要走它。
		return currentCeraShopPurchaseFailureCodeInsufficientCera
	case errors.Is(err, errCurrentCeraShopGoldInsufficient):
		// 金币标价商品缺的是角色金币，不是点券：回 13「金币不足」。
		return currentCeraShopPurchaseFailureCodeInsufficientGold
	case errors.Is(err, errCurrentCeraShopGiftDailyLimit):
		// 每日赠送额度用完。用户要求用 11（在 `21969 + code` 表里是 21980
		//「超出赠送金额上限」）；真机若显示成别的句子，用试码文件定位后改常量。
		return currentCeraShopPurchaseFailureCodeGiftAmountCap
	case errors.Is(err, errCurrentCeraShopGiftMailUnavailable):
		// 邮件写不进去 = 瞬时故障（钱已经退回），回"未知错误"：至少不是一句
		// 指向玩家的假原因。真实原因在 game-cera-shop-gift-delivery-failed 日志里。
		return currentCeraShopPurchaseFailureCodeUnknown
	default:
		// 商品不可邮寄 / 职业不符 / 账号太新 / 每日额度用完 / 好友·大区·自身判定：
		// 通用表里都只能表达成"这一单买不成"。真实原因看日志里的 failure_code + reason。
		return currentCeraShopPurchaseFailureCodeCannotBuy
	}
}

// validateCurrentCeraShopGift 是 op64 的赠送判定（权威入口）。
//
// 不信任客户端走过的 op136：恶意客户端可以只发 op64。所以四条规则在这里全部重判，
// 通过的返回 nil。
func (s *Service) validateCurrentCeraShopGift(
	ctx context.Context,
	session *gameSession,
	request currentCeraShopPurchaseRequest,
) (currentCeraShopGiftRecipient, error) {
	recipientName := strings.TrimSpace(request.GiftRecipientName)
	if recipientName == "" {
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftRecipientGone
	}
	recipient, found, err := s.lookupCurrentCeraShopGiftRecipient(ctx, recipientName)
	if err != nil {
		return currentCeraShopGiftRecipient{}, err
	}
	if !found {
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftRecipientGone
	}
	// 规则 1（互为好友满 7 天）、规则 3（同大区）、不能送自己。
	code, err := s.evaluateCurrentCeraShopGiftQueryRules(ctx, session, recipientName, recipient)
	if err != nil {
		return currentCeraShopGiftRecipient{}, err
	}
	switch code {
	case 0:
	case currentCeraShopGiftQueryCodeSelf:
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftSelf
	case currentCeraShopGiftQueryCodeNotFound:
		// 查无此人或跨大区：op64 里没有对应文案，按"无法赠送"处理。
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftRecipientGone
	case currentCeraShopGiftQueryCodeFriendTooNew:
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftFriendTooNew
	case currentCeraShopGiftQueryCodeNotFriend:
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftNotFriend
	case currentCeraShopGiftQueryCodeDailyCap:
		// 今天的赠送额度已经用完了。这里必须映射到**赠送额度**那个错误，
		// 否则会掉进 default 变成"您无法使用赠送功能"——玩家看到的是"你被禁了"，
		// 而真实原因是"今天送满了"。op64 侧的文案是 11「超出赠送金额上限」。
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftDailyLimit
	default:
		return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftRecipientGone
	}
	// 规则 2（赠送方账号满 7 天）、规则 4（双方基础职业相同）。
	recipientJob, recipientHasJob := recipient.job, recipient.hasJob
	if purchaseCode, err := s.evaluateCurrentCeraShopGiftPurchaseRules(ctx, session, recipientName, recipientJob, recipientHasJob); err != nil {
		return currentCeraShopGiftRecipient{}, err
	} else if purchaseCode != 0 {
		switch purchaseCode {
		case currentCeraShopPurchaseFailureCodeGiftAccountTooNew:
			return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftAccountTooNew
		default:
			return currentCeraShopGiftRecipient{}, errCurrentCeraShopGiftJobMismatch
		}
	}
	return recipient, nil
}

// deliverCurrentCeraShopGift 把结账收集到的赠送商品投递到收件人邮箱。
//
// 调用时机：**结账已经提交之后**（付款方已经扣过点券），所以这里的任何失败都必须
// 由调用方退款并回失败包，绝不能"投递失败还回成功"。
//
// 幂等：completion_key 由 (赠送方, 收件人, 商品清单, 投递内容) 组成，同一笔结账
// 重复投递不会产生第二封邮件（AppendSystemMail 按 completion_key 去重）。
func (s *Service) deliverCurrentCeraShopGift(
	ctx context.Context,
	session *gameSession,
	request currentCeraShopPurchaseRequest,
	result currentCeraShopCommitResult,
	sender currentCeraShopGiftSender,
	recipient currentCeraShopGiftRecipient,
) (string, error) {
	if len(result.GiftAttachments) == 0 {
		// 赠送形态下一件商品都没收集到，一定是 bug（商品被静默吞掉）。
		// 必须报错让调用方退款，不能回成功。
		return "", fmt.Errorf("%w: no attachments were collected for recipient %s", errCurrentCeraShopGiftNotGiftable, recipient.characterID)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return "", errCurrentCeraShopGiftMailUnavailable
	}
	// 寄件人名字会原样进邮件**摘要行**，客户端那条 dstring 的预算是 30 字节
	// （mail.maxMailboxRecipientBytes）。超了的话不是"这封信显示不全"，而是**收件人
	// 打开邮箱时整包构建失败** —— 等于把对方的邮箱弄坏了。所以在这里挡住：
	// 用和 wire builder 完全同一个 GB18030 编码器量长度。
	senderNameRaw, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(sender.name))
	if err != nil {
		return "", fmt.Errorf("%w: sender name %q is not encodable: %v", errCurrentCeraShopGiftMailUnavailable, sender.name, err)
	}
	if len(senderNameRaw) > currentCeraShopGiftSenderNameMaxBytes {
		return "", fmt.Errorf("%w: sender name %q is %d bytes, limit %d",
			errCurrentCeraShopGiftMailUnavailable, sender.name, len(senderNameRaw), currentCeraShopGiftSenderNameMaxBytes)
	}
	body := fmt.Sprintf(currentCeraShopGiftMailBodyFormat, sender.name)
	now := s.gameplayNow().UTC()
	// ★ completion_key 必须**每次结账都不同**。
	//
	// AppendSystemMail 按 completion_key 去重：命中就把**原来那封**的 mailID 原样返回、
	// 不新建邮件。所以 key 里如果只有 (赠送方, 收件人, 商品, 投递内容)，同一件商品送给
	// 同一个人两次就会撞键：第二单照样扣点券、照样收到"成功"回包，而收件人只有第一封
	// 邮件 —— 玩家花了钱什么都没有。这个键是用来防"同一次结账被投递两次"的，不是用来
	// 防"同一个玩家送两次"的，因此必须带一个每次调用都变的一次性序号。
	nonce := currentCeraShopGiftDeliveryNonce()
	completionKey := fmt.Sprintf("cera-shop-gift:%d:%d:%s:%s:%s:%s",
		nonce, now.UnixNano(),
		sender.characterID, recipient.characterID, currentCeraShopCommoditySummary(result.Products),
		strings.Join(result.GiftGrantNotes, "|"))
	var mailID string
	err = repositories.MailboxAssets.WithinMailboxAssets(
		ctx,
		sender.characterID,
		recipient.characterID,
		func(
			_ dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			_ dnfrepo.CharacterRepository,
			_ dnfrepo.InventoryRepository,
			mailboxes dnfrepo.MailboxRepository,
		) error {
			if mailboxes == nil {
				return errCurrentCeraShopGiftMailUnavailable
			}
			id, appendErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: recipient.characterID,
				RecipientName:        request.GiftRecipientName,
				Title:                currentCeraShopGiftMailTitle,
				Body:                 body,
				Source:               currentCeraShopGiftMailSource,
				CompletionKey:        completionKey,
				Attachments:          result.GiftAttachments,
				CreatedAt:            now,
				SenderCharacterID:    sender.characterID,
				SenderName:           sender.name,
			})
			if appendErr != nil {
				return appendErr
			}
			mailID = id
			return nil
		},
	)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errCurrentCeraShopGiftMailUnavailable, err)
	}
	return mailID, nil
}

// refundCurrentCeraShopGift 把一次已经扣掉、但投递失败的**钱**退回付款方。
//
// 退的是本次结账实际扣掉的全部钱包：三个账号级点券钱包 + 角色金币（金币标价商品）。
// 金币那一路**必须一起退**：不改这里就会出现"投递失败 → 点券退了、金币没退"，
// 也就是玩家白丢几十万金币 —— 这正是"修 A 坏 B"的典型形态。
//
// Checkout 只接受 Cost >= 0，所以"退款"是 Cost=0 + 在投影里把扣掉的钱加回去。
// 账号记录在 Checkout 里是**无条件保存**的；金币改动则必须显式要求保存角色 stats
// （CheckoutChanges.Character），否则退款只改在内存里。
//
// 退款本身失败只能记日志：绝不能让"退款失败"把已经确定的失败回包变成成功回包。
func (s *Service) refundCurrentCeraShopGift(
	ctx context.Context,
	session *gameSession,
	result currentCeraShopCommitResult,
	sender currentCeraShopGiftSender,
) error {
	if result.PaidCera == 0 && result.PaidTokenCera == 0 && result.PaidHappyTokenCera == 0 && result.PaidGold == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CeraShopAssets == nil {
		return errCurrentCeraShopGiftSenderMissing
	}
	owner, err := dnfcerashop.NewOwner(repositories)
	if err != nil {
		return err
	}
	accountID := strings.TrimSpace(sender.accountID)
	if accountID == "" {
		accountID = strings.TrimSpace(s.accountIDForSession(session))
	}
	if accountID == "" || sender.characterID == "" {
		return errCurrentCeraShopGiftSenderMissing
	}
	_, err = owner.Checkout(ctx, dnfcerashop.CheckoutCommand{
		AccountID:     accountID,
		CharacterID:   sender.characterID,
		SettingsScope: dnfrepo.CharacterContainerStateScope(sender.characterID),
		Cost:          0,
		UpdatedAt:     s.gameplayNow().UTC(),
		PayPolicy:     dnfcerashop.PayPolicyWaterfall,
		Project: func(assets *dnfcerashop.CheckoutAssets) (dnfcerashop.CheckoutChanges, error) {
			if assets == nil || assets.Account == nil {
				return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopGiftSenderMissing
			}
			account := assets.Account
			if result.PaidCera != 0 {
				dnfcerashop.SetBalance(account, dnfcerashop.Balance(*account)+result.PaidCera)
			}
			if result.PaidTokenCera != 0 {
				dnfcerashop.SetTokenCeraBalance(account, dnfcerashop.TokenCeraBalance(*account)+result.PaidTokenCera)
			}
			if result.PaidHappyTokenCera != 0 {
				dnfcerashop.SetHappyTokenCeraBalance(account, dnfcerashop.HappyTokenCeraBalance(*account)+result.PaidHappyTokenCera)
			}
			// 金币标价商品：把扣掉的角色金币加回去。角色记录与账号记录在
			// WithinCeraShopAssets 的同一次 SQL 事务里，不会出现"退了一半"。
			if result.PaidGold != 0 {
				if assets.Character == nil || assets.Character.Stats == nil {
					return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopGoldInsufficient
				}
				assets.Character.Stats["gold"] += result.PaidGold
				return dnfcerashop.CheckoutChanges{Character: true}, nil
			}
			return dnfcerashop.CheckoutChanges{}, nil
		},
	})
	return err
}

// currentCeraShopGiftResultSummary 是投递成功后写进日志的摘要。
func currentCeraShopGiftResultSummary(result currentCeraShopCommitResult) string {
	return strings.Join(result.GiftGrantNotes, ",")
}

// currentCeraShopGiftPaidSummary 是赠送日志里的"这次扣了什么"摘要。
// 第 4 段是角色金币（金币标价商品）—— 没有它，赠送失败时无法判断该退多少金币。
func currentCeraShopGiftPaidSummary(result currentCeraShopCommitResult) string {
	return strconv.FormatInt(result.PaidCera, 10) + "/" +
		strconv.FormatInt(result.PaidTokenCera, 10) + "/" +
		strconv.FormatInt(result.PaidHappyTokenCera, 10) + "/" +
		strconv.FormatInt(result.PaidGold, 10)
}
