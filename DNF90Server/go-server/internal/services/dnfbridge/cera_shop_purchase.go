package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfcargo "longheng.io/server/internal/modules/dnf/cargo"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/purchaselimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentCeraShopRequestHeaderSize = 4
	currentCeraShopRequestItemHeader = 7
	currentCeraShopRequestItemStride = 15
	currentCeraShopRequestItemTail   = 6
	currentCeraShopRequestMaxItems   = 64
	currentCeraShopUpdateMsgID       = 0x0035

	currentCeraShopPaymentModeCera         byte = 0
	currentCeraShopPaymentModeAvatarCoupon byte = 1

	// The current client consumes class1/op415 only after a successful
	// character-slot ticket checkout. Persist the newly unlocked limit with
	// the account transaction so a selector transition cannot lose the effect.
	currentCharacterSlotExtendEffectPendingMetadataKey = "character_slot_extend_effect_pending_limit"

	// Current NoPack sub_232A9F0 groups list 7 as pet bodies 0..139,
	// pet equipment 140..188, and pet consumables 189..238 (inclusive).
	currentCeraShopPetConsumableSlotStart int16 = 189
	currentCeraShopPetConsumableSlotEnd   int16 = 238
)

var (
	errCurrentCeraShopRequestMalformed   = errors.New("dnf cera shop purchase request is malformed")
	errCurrentCeraShopPaymentUnsupported = errors.New("dnf cera shop payment mode is unsupported")
	errCurrentCeraShopCatalogUnavailable = errors.New("dnf cera shop catalog is unavailable")
	errCurrentCeraShopProductUnavailable = errors.New("dnf cera shop product is unavailable")
	errCurrentCeraShopOwnerUnavailable   = errors.New("dnf cera shop selected character is unavailable")
	errCurrentCeraShopCeraInsufficient   = errors.New("dnf cera shop cera balance is insufficient")
	// errCurrentCeraShopGoldInsufficient：金币标价商品的角色金币不够。整笔结账回滚，
	// 客户端收到码 13「金币不足」（见 cera_shop_protocol.go 的码表，那是这张表里
	// 唯一一句"金币不足"，不需要另造文案）。
	errCurrentCeraShopGoldInsufficient = errors.New("dnf cera shop character gold is insufficient")
	// errCurrentCeraShopMixedPaymentUnsupported：一辆购物车里同时有金币标价商品和
	// 点券标价商品。**整笔拒绝，不猜**。
	//
	// 为什么不"两边各扣各的"：客户端的支付方式位只有一个（现场恒为 0 = 点券），混合
	// 车在客户端本来就不是一个自洽的状态；而猜错任何一种口径都会变成经济事故
	// （少扣 = 白送，多扣 = 玩家花了钱没拿到东西）。拒绝既不会白送也不会多扣，
	// 日志里有 commodities 可查，真出现再按现场数据决定口径。
	errCurrentCeraShopMixedPaymentUnsupported = errors.New("dnf cera shop cart mixes gold-priced and cera-priced commodities")
	// errCurrentCeraShopProductPriceAmbiguous：同一行同时写了金币与 Cera 两个非 0 标价。
	// 这是 PVF 数据坏，同样是拒绝而不是二选一。
	errCurrentCeraShopProductPriceAmbiguous = errors.New("dnf cera shop product row carries both a gold and a cera price")
	// errCurrentCeraShopGiftRequestMalformed：赠送前缀（形态位 + DSTR 名字）本身畸形。
	// 与普通购买解析失败分开，日志里才能一眼看出"是赠送包没解析对"。
	errCurrentCeraShopGiftRequestMalformed = errors.New("dnf cera shop gift request prefix is malformed")
	// errCurrentCeraShopGiftDeliveryUnsupported：赠送形态解析成功，但本服还不能把
	// 商品投递给对方。**必须显式拒绝**，绝不能让这样一个包掉进普通购买分支——
	// 那会把"送给别人"变成"买给自己"，是真正的经济事故。
	errCurrentCeraShopGiftDeliveryUnsupported = errors.New("dnf cera shop gift delivery is not implemented")
)

// currentCeraShopGiftRecipientMaxNameBytes 与角色名上限（30 字节 GB18030）取同一个量级；
// 这里只是防畸形长度，真正的名字合法性由 decodeCharacterName + 查库决定。
const currentCeraShopGiftRecipientMaxNameBytes = 30

// currentCeraShopPurchaseRequest follows the current client's sendBuyPacket
// body. Each row starts with seven fixed bytes (payment/category, attribute,
// purchase type and commodity), then two variable tables: u8+5*n avatar
// choices and u8+8*n [package data selection] choices. A six-byte linked-item
// tail closes the row. Fifteen bytes is therefore only the zero-choice minimum,
// not a fixed stride. The first row's first byte remains the checkout payment
// selector used by this account-local implementation.
type currentCeraShopPurchaseRequest struct {
	PaymentMode byte
	Items       []currentCeraShopPurchaseRequestItem
	// GiftRecipientName 非空表示这是**赠送形态**的结账：现场抓包
	// （2026-09-27 16:14:02，legacy cmd=1 type=64 body_len=28）的 body 是
	//
	//     b0 | 01 | u32 名字长度 | 名字(GB18030) | u8 赠送类型 | b2 b3 ... 原样
	//     ^^   ^^   ^^^^^^^^^^^   ^^^^^^^^^^^^   ^^^^^^^^^^   ^^^^^^^^^^^^^^
	//     \___/ \___________________________________________/ \_____________/
	//     普通 body 的头两字节      赠送前缀                      普通 body 从第 3 字节起
	//
	// 也就是"把 DSTR+类型 插在普通购物车 body 的第 2 个字节之后，并把第 2 个字节
	// 置 1"。这与客户端 CashShopBuyWindow::sendBuyPacket（RVA 0x02E7471A）完全一致：
	// 它先写 [edi+0x282C]、再写 [edi+0x2828]，后者 == 1 时才追加 WriteDstr(收件人)
	// + WriteU8(类型)。普通购买那一包 body[1] = 0（现场 3 包全是 0），所以**形态位在
	// 下标 1，不是下标 0**。
	//
	// 名字与类型已被剥掉，Items/PaymentMode 仍由同一个解析器从**还原后的**普通
	// body 里读出来。
	GiftRecipientName string
	GiftRecipientRaw  []byte
	// GiftKind 是前缀尾部的 u8（客户端 [edi+0x2834]）。现场那一包 = 0。
	GiftKind byte
}

// currentCeraShopGiftPrefixFixedSize 是赠送前缀的固定部分：
// [形态位 0][赠送位 1][u32 名字长度][u8 类型] —— 长度字段之外的 7 个字节。
const currentCeraShopGiftPrefixFixedSize = 7

// currentCeraShopGiftPrefix 是拆出来的赠送前缀 + 还原后的普通 body。
type currentCeraShopGiftPrefix struct {
	Gift    bool
	Name    string
	NameRaw []byte
	Kind    byte
	Cart    []byte
}

// splitCurrentCeraShopGiftPrefix 把赠送前缀从 body 上剥下来，并**还原**出一份与普通
// 购买逐字节同形的 body 交给原来的解析器。
//
// 前缀畸形时返回错误——**绝不能让畸形前缀落进普通购买解析器**：那样 body[2] 会被当成
// 行数、commodity 会从一个错位的地方读出来，最后变成一次跟真实原因无关的失败，正是
// 现场 16:14 那次「购买后提示错误」。
func splitCurrentCeraShopGiftPrefix(body []byte) (currentCeraShopGiftPrefix, error) {
	// 形态位在**下标 1**。body[0] 两种形态都是 0，不能拿它当判据。
	if len(body) < currentCeraShopGiftPrefixFixedSize || body[1] != 1 {
		return currentCeraShopGiftPrefix{Cart: body}, nil
	}
	length := int(binary.LittleEndian.Uint32(body[2:6]))
	prefix := currentCeraShopGiftPrefixFixedSize + length
	if length <= 0 || length > currentCeraShopGiftRecipientMaxNameBytes || prefix >= len(body) {
		return currentCeraShopGiftPrefix{Gift: true}, errCurrentCeraShopGiftRequestMalformed
	}
	raw := body[6 : 6+length]
	if bytes.IndexByte(raw, 0) >= 0 {
		return currentCeraShopGiftPrefix{Gift: true}, errCurrentCeraShopGiftRequestMalformed
	}
	name, err := decodeCharacterName(raw)
	// 注意：x/text 的 GB18030 解码器很宽松（0x80/0xFF 这类单字节也能解出来），所以
	// name/reject 这一层实际拦不下"乱名字"；乱名字会在查库那一步落到"查无此人"，
	// 由赠送查询那条路径给客户端一个明确回执。
	if err != nil || strings.TrimSpace(name) == "" {
		return currentCeraShopGiftPrefix{Gift: true}, errCurrentCeraShopGiftRequestMalformed
	}
	cart := make([]byte, 0, 2+len(body)-prefix)
	cart = append(cart, body[0], 0) // 还原普通头的头两字节：赠送位清零
	cart = append(cart, body[prefix:]...)
	return currentCeraShopGiftPrefix{
		Gift:    true,
		Name:    name,
		NameRaw: append([]byte(nil), raw...),
		Kind:    body[6+length],
		Cart:    cart,
	}, nil
}

type currentCeraShopPurchaseRequestItem struct {
	CommodityID       uint32
	AttributeValue    byte
	AvatarChoices     []currentCeraPackageChoice
	PackageSelections []currentCeraShopPackageSelection
	LinkedItemID      uint32
	InventorySlot     uint16
}

type currentCeraShopPackageSelection struct {
	PackageItemID uint32
	GroupIndex    uint16
	ChoiceIndex   uint16
}

type currentCeraShopCommitResult struct {
	Products                 []currentCeraShopProduct
	Updates                  []currentCeraShopItemUpdate
	MainInventoryExpansion   uint16
	AvatarInventoryExpansion uint16
	PersonalCargoSlotCount   uint16
	AccountCargoSelectionKey uint16
	CharacterSlotLimit       uint16
	RenameEntitlementsAdded  uint16
	PetInventoryChanged      bool
	CeraAfter                int64
	TokenCera                int64
	HappyTokenCera           int64
	PremiumActivations       []currentCeraShopPremiumActivation
	NameTagActivation        *currentNameTagCardActivation
	// BuyRestrictNotes 记录本次结账消费掉的商城限购（“维度:键:已购/上限”）。
	// 商城道具的限购在道具 PVF 里没有字段，只能看这条日志确认计数真的在走。
	BuyRestrictNotes []string
	// BuyCountGiftCount 是本次结账后角色的多买多送累计次数（活动未启用时为 0）。
	// 仅供 committed 日志核对；真正下发给客户端的是
	// sendCurrentCeraShopBuyCountGiftProjection（它重新读一次角色，拿的是权威值）。
	BuyCountGiftCount int64
	// BuyCountGiftNotes 记录本次跨档发奖（“tier:档位:item:道具号:x数量”）。
	BuyCountGiftNotes []string
	// BuyCountGiftRewards 是本次跨档发奖的原始数据，供结账收尾时逐条推
	// `class0/0x02D2` 奖励弹窗（参考实现就是每拿到一件赠品发一包）。
	BuyCountGiftRewards []currentCeraShopBuyCountGiftTier
	// PaidHappyTokenCera / PaidTokenCera / PaidCera 是本次结账各钱包实际扣掉的数量
	// （支付瀑布的结果）。点券之外的两种是"代币券真的能花掉"的凭据。
	PaidHappyTokenCera int64
	PaidTokenCera      int64
	PaidCera           int64
	// PaidGold 是本次结账从**角色金币**实际扣掉的数量（金币标价商品走这条）。
	// 它与上面三个点券钱包并列：是"金币真的被扣了"的唯一凭据，也是赠送投递失败时
	// 退款金额的唯一来源。点券标价商品恒为 0。
	PaidGold int64
	// GoldAfter 是扣完金币之后的角色余额，直接用来发 op14 金币包；PaidGold 为 0 时
	// 无意义（不发包）。
	GoldAfter int64
	// GiftAttachments 是**赠送**结账里本该落进付款方背包、改为邮寄给收件人的商品。
	// 只有赠送形态会填，普通购买永远是 nil（所以"赠送变成买给自己"这条红线
	// 在类型层面就能被断言到）。
	//
	// 注意口径：这里只有**买到的商品**（含礼包自动展开出的奖励）。
	// 一比一赠品（multibonus）与多买多送的档位奖励**不进这里** —— 活动奖励归付款方。
	GiftAttachments []dnfrepo.MailAttachment
	// GiftGrantNotes 记录这次赠送投递了什么（"commodity:item:xN"），只用于日志核对。
	GiftGrantNotes []string
}

// currentCeraShopPremiumActivation is one premium contract activated at
// checkout: the premium type (580+slot for devil perks) and its remaining
// seconds, used for the class0/op66 activation notification.
type currentCeraShopPremiumActivation struct {
	PremiumType      int64
	RemainingSeconds int64
}

// currentCeraShopItemUpdate is an actual changed inventory container.  A
// Cera package belongs in list 0, while a direct avatar product belongs in
// list 1.  Keep the container with the changed rows so a list-0 item update
// can never make a client interpret an avatar entry as a normal bag row.
type currentCeraShopItemUpdate struct {
	ListType byte
	Entries  []currentItemListEntry
}

type currentCeraShopResolvedPurchase struct {
	Product                     currentCeraShopProduct
	AttributeValue              byte
	MainInventoryUpgradeStage   uint16
	AvatarInventoryUpgradeStage uint16
	PersonalCargoUpgradeTarget  uint16
	AccountCargoUpgrade         bool
	CharacterSlotExpansion      bool
	CharacterRenameEntitlement  bool
	AvatarCouponItemID          int64
	// PremiumType > 0 means the purchase is consumed at checkout and activates
	// this account-level premium contract instead of entering any container
	// (86JP ConsumedOnPurchase). PremiumPackage activates every 魔王契约 devil
	// slot for the same duration ([charac premium package]).
	PremiumType            int64
	PremiumDurationSeconds int64
	PremiumPackage         bool
	NameTagDurationSeconds int64
	AutoOpenRewards        []currentCeraShopAutoOpenReward
}

type currentCeraShopChangedSlot struct {
	ListType byte
	Slot     int16
	Product  currentCeraShopProduct
	// Bonus marks a slot filled by a one-plus-one IPG gift
	// (etc/chn_cerashop_multibonus.etc) rather than by the purchased product
	// itself; Product keeps the parent commodity for provenance while
	// Product.ItemID is the granted bonus item.
	Bonus bool
}

func parseCurrentCeraShopPurchaseRequest(body []byte) (currentCeraShopPurchaseRequest, error) {
	giftPrefix, err := splitCurrentCeraShopGiftPrefix(body)
	if err != nil {
		return currentCeraShopPurchaseRequest{}, err
	}
	body = giftPrefix.Cart
	if len(body) < currentCeraShopRequestHeaderSize+currentCeraShopRequestItemStride {
		return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
	}
	count := int(body[2])
	if count == 0 {
		count = 1
	}
	if count < 1 || count > currentCeraShopRequestMaxItems ||
		len(body) < currentCeraShopRequestHeaderSize+count*currentCeraShopRequestItemStride {
		return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
	}
	request := currentCeraShopPurchaseRequest{Items: make([]currentCeraShopPurchaseRequestItem, 0, count)}
	cursor := currentCeraShopRequestHeaderSize
	for index := 0; index < count; index++ {
		if cursor > len(body)-currentCeraShopRequestItemHeader {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		itemOffset := cursor
		if index == 0 {
			request.PaymentMode = body[itemOffset]
		}
		commodityID := binary.LittleEndian.Uint32(body[itemOffset+3 : itemOffset+7])
		if commodityID == 0 {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		item := currentCeraShopPurchaseRequestItem{
			CommodityID:    commodityID,
			AttributeValue: body[itemOffset+1],
		}
		cursor += currentCeraShopRequestItemHeader

		if cursor >= len(body) {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		avatarCount := int(body[cursor])
		cursor++
		if avatarCount > currentCeraPackageMaxChoices || avatarCount > (len(body)-cursor)/currentCeraPackageChoiceStride {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		item.AvatarChoices = make([]currentCeraPackageChoice, 0, avatarCount)
		for choiceIndex := 0; choiceIndex < avatarCount; choiceIndex++ {
			itemID := binary.LittleEndian.Uint32(body[cursor : cursor+4])
			if itemID == 0 {
				return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
			}
			item.AvatarChoices = append(item.AvatarChoices, currentCeraPackageChoice{ItemID: itemID, Option: body[cursor+4]})
			cursor += currentCeraPackageChoiceStride
		}

		if cursor >= len(body) {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		selectionCount := int(body[cursor])
		cursor++
		const selectionStride = 8
		if selectionCount > currentCeraPackageMaxChoices || selectionCount > (len(body)-cursor)/selectionStride {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		item.PackageSelections = make([]currentCeraShopPackageSelection, 0, selectionCount)
		for selectionIndex := 0; selectionIndex < selectionCount; selectionIndex++ {
			packageItemID := binary.LittleEndian.Uint32(body[cursor : cursor+4])
			if packageItemID == 0 {
				return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
			}
			item.PackageSelections = append(item.PackageSelections, currentCeraShopPackageSelection{
				PackageItemID: packageItemID,
				GroupIndex:    binary.LittleEndian.Uint16(body[cursor+4 : cursor+6]),
				ChoiceIndex:   binary.LittleEndian.Uint16(body[cursor+6 : cursor+8]),
			})
			cursor += selectionStride
		}

		if cursor > len(body)-currentCeraShopRequestItemTail {
			return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
		}
		item.LinkedItemID = binary.LittleEndian.Uint32(body[cursor : cursor+4])
		item.InventorySlot = binary.LittleEndian.Uint16(body[cursor+4 : cursor+6])
		cursor += currentCeraShopRequestItemTail
		request.Items = append(request.Items, item)
	}
	if cursor != len(body) {
		return currentCeraShopPurchaseRequest{}, errCurrentCeraShopRequestMalformed
	}
	if giftPrefix.Gift {
		request.GiftRecipientName = giftPrefix.Name
		request.GiftRecipientRaw = giftPrefix.NameRaw
		request.GiftKind = giftPrefix.Kind
	}
	return request, nil
}

// recordCurrentCeraShopPurchase 记一条商城点券消费记录。
//
// 只在商城一处埋点：go-server 里扣点券只有两条路径——商城结账（本文件）和
// 金库升级（走 cargo owner，通过 alignedcmd.Result.CeraSpent 上报，见 game_aligned.go）。
// 余额取结账事务返回的权威值，不做"扣了多少自己算余额"这种推导。
func (s *Service) recordCurrentCeraShopPurchase(
	session *gameSession,
	request currentCeraShopPurchaseRequest,
	result currentCeraShopCommitResult,
	ok bool,
	reason string,
	bodyLen int,
) {
	if s == nil {
		return
	}
	paymentMode := "点券"
	if request.PaymentMode == currentCeraShopPaymentModeAvatarCoupon {
		paymentMode = "装扮兑换券"
	}
	requested := make([]map[string]any, 0, len(request.Items))
	for _, item := range request.Items {
		row := map[string]any{
			"commodity_id": item.CommodityID,
		}
		if item.AttributeValue != 0 {
			row["attribute_value"] = item.AttributeValue
		}
		if item.LinkedItemID != 0 {
			row["linked_item_id"] = item.LinkedItemID
		}
		if item.InventorySlot != 0 {
			row["inventory_slot"] = item.InventorySlot
		}
		if len(item.AvatarChoices) > 0 {
			row["avatar_choices"] = len(item.AvatarChoices)
		}
		if len(item.PackageSelections) > 0 {
			row["package_selections"] = len(item.PackageSelections)
		}
		requested = append(requested, row)
	}
	fields := map[string]any{
		"result":        "ok",
		"payment_mode":  request.PaymentMode,
		"payment_name":  paymentMode,
		"request_items": requested,
		"item_count":    len(request.Items),
		"body_len":      bodyLen,
	}
	if !ok {
		fields["result"] = "rejected"
		fields["reason"] = reason
		s.recordCeraSpend(session, fields)
		return
	}
	granted := make([]map[string]any, 0, len(result.Products))
	totalCera := int64(0)
	totalGold := int64(0)
	for _, product := range result.Products {
		row := map[string]any{
			"commodity_id": product.CommodityID,
			"item_id":      product.ItemID,
			"count":        product.Count,
			"section":      product.Section,
			"cera_price":   product.CeraPrice,
		}
		// 金币标价才写 gold_price：点券商品的这一列恒 0，写进去只会让 177 行商品
		// 的记录凭空多一个 0 字段（既有记录的字段集不该被改动污染）。
		if product.GoldPrice != 0 {
			row["gold_price"] = product.GoldPrice
		}
		granted = append(granted, row)
		totalCera += int64(product.CeraPrice) * int64(product.Count)
		totalGold += int64(product.GoldPrice) * int64(product.Count)
	}
	slots := make([]string, 0, 8)
	for _, update := range result.Updates {
		for _, entry := range update.Entries {
			if len(entry.data) < 2 {
				continue
			}
			slots = append(slots, fmt.Sprintf("%d:%d", update.ListType, int16(binary.LittleEndian.Uint16(entry.data[0:2]))))
		}
	}
	activations := make([]map[string]any, 0, len(result.PremiumActivations))
	for _, activation := range result.PremiumActivations {
		activations = append(activations, map[string]any{
			"premium_type":      activation.PremiumType,
			"remaining_seconds": activation.RemainingSeconds,
		})
	}
	fields["granted"] = granted
	fields["changed_slots"] = slots
	// 点券维度的"花了多少 / 之前多少"必须按**实际扣掉的点券**算。
	//
	// 支付瀑布上线后 `totalCera`（商品标价之和）不再等于点券扣费：券可能承担了全部或
	// 部分金额。继续用它会让日志声称"点券消费了整笔标价"，并与同一条日志里的新列
	// paid_cera 自相矛盾，按 cera_before - cera_after 核算也对不上。
	fields["cera_spent"] = result.PaidCera
	fields["cera_total_price"] = totalCera
	fields["cera_after"] = result.CeraAfter
	if request.PaymentMode == currentCeraShopPaymentModeCera {
		// 点券付款时余额差就是本次实际扣掉的点券；装扮兑换券付款不扣点券，
		// 别给出会误导的 before。
		fields["cera_before"] = result.CeraAfter + result.PaidCera
	}
	fields["token_cera_after"] = result.TokenCera
	fields["happy_token_cera_after"] = result.HappyTokenCera
	// 支付瀑布里两张券**实际扣了多少 + 扣之前的余额**。
	//
	// ── 为什么必须记（2026-10-05，现场排查"这单是不是白拿"）────────────────
	// 改造前这条记录只有 `cera_spent`（点券实际扣除）和两张券的**之后**余额。
	// 于是一笔"全额用欢乐代币券支付"的正经消费，在记录里长成这样：
	//
	//	实扣点券 = 0
	//	欢乐代币券余额 = 0     ← 这是"之后"的余额，不是"扣了多少"
	//
	// 两个 0 摆在一起，看起来就是一分钱没付。现场为了判断那一单到底有没有付钱，
	// 只能把服务器上加密封包日志（3GB）拉回来解密，再去 packet_log 的
	// game-cera-shop-purchase-committed 里翻 paid_happy_token_cera —— 而那三个字段
	// 本来就只写在 packet-log 事件里，商家侧的记录根本看不到。
	//
	// 注意 `gold_*` 那一路**早就有 before**（见上面 gold_before），券这一路是**漏项**，
	// 不是设计取舍。
	//
	// 只在非 0 时写：与 gold_* 同一个口径 —— 既有记录的字段集不该被无意义地撑大
	// （178 行商品里绝大多数用不到券）。字段缺席 = 该项为 0，语义完备。
	if result.PaidTokenCera != 0 {
		fields["token_cera_spent"] = result.PaidTokenCera
		fields["token_cera_before"] = result.TokenCera + result.PaidTokenCera
	}
	if result.PaidHappyTokenCera != 0 {
		fields["happy_token_cera_spent"] = result.PaidHappyTokenCera
		fields["happy_token_cera_before"] = result.HappyTokenCera + result.PaidHappyTokenCera
	}
	// 金币标价商品：与上面三个点券钱包**并列**记录，不要混进 cera_spent ——
	// 金币是角色货币、点券是账号货币，混在一起会让"点券消费"这条审计线失真。
	if result.PaidGold != 0 {
		fields["gold_spent"] = result.PaidGold
		fields["gold_after"] = result.GoldAfter
		fields["gold_before"] = result.GoldAfter + result.PaidGold
		fields["gold_total_price"] = totalGold
	}
	if len(activations) > 0 {
		fields["premium_activations"] = activations
	}
	if result.MainInventoryExpansion != 0 {
		fields["main_inventory_expansion"] = result.MainInventoryExpansion
	}
	if result.AvatarInventoryExpansion != 0 {
		fields["avatar_inventory_expansion"] = result.AvatarInventoryExpansion
	}
	if result.PersonalCargoSlotCount != 0 {
		fields["personal_cargo_slot_count"] = result.PersonalCargoSlotCount
	}
	if result.AccountCargoSelectionKey != 0 {
		fields["account_cargo_selection_key"] = result.AccountCargoSelectionKey
	}
	if result.CharacterSlotLimit != 0 {
		fields["character_slot_limit"] = result.CharacterSlotLimit
	}
	if result.RenameEntitlementsAdded != 0 {
		fields["rename_entitlements_added"] = result.RenameEntitlementsAdded
	}
	if result.NameTagActivation != nil {
		fields["name_tag_item_id"] = result.NameTagActivation.ItemID
		fields["name_tag_expire_at"] = result.NameTagActivation.ExpireAt
	}
	if result.PetInventoryChanged {
		fields["pet_inventory_changed"] = true
	}
	s.recordCeraSpend(session, fields)
}

// currentCeraShopPurchaseFailureCodeForCommitError maps an ordinary
// (non-gift) shop checkout failure to the current client's op64 error table.
//
// Inventory-full is deliberately distinct from the generic "cannot buy"
// fallback. The client displays code 14 as "物品栏空间不足"; returning 16 here
// makes a full bag look like a product/restriction failure.
func currentCeraShopPurchaseFailureCodeForCommitError(err error) byte {
	switch {
	case errors.Is(err, dnfcerashop.ErrWalletInsufficient),
		errors.Is(err, dnfcerashop.ErrCeraInsufficient):
		return currentCeraShopPurchaseFailureCodeInsufficientCera
	case errors.Is(err, errCurrentCeraShopGoldInsufficient):
		return currentCeraShopPurchaseFailureCodeInsufficientGold
	case errors.Is(err, errDungeonPickupInventoryFull):
		return currentCeraShopPurchaseFailureCodeInventoryFull
	default:
		return currentCeraShopPurchaseFailureCodeCannotBuy
	}
}

func (s *Service) handleCurrentCeraShopPurchase(session *gameSession, body []byte) error {
	request, err := parseCurrentCeraShopPurchaseRequest(body)
	if err != nil {
		// 赠送前缀畸形要和普通购买畸形分开记：现场 2026-09-27 16:14 的那一包
		// 就是被当成普通购买解析、最后报了一个跟真实原因无关的失败码。
		//
		// 形态位是**下标 1**（见文件头那张图），不是下标 0 —— 两种形态的 body[0]
		// 都是 0，拿它判断等于永远判 false（这条分支早先就是那么写的，形同虚设）。
		event := "game-cera-shop-purchase-rejected"
		if errors.Is(err, errCurrentCeraShopGiftRequestMalformed) ||
			len(body) > 1 && body[1] == 1 {
			event = "game-cera-shop-gift-purchase-rejected"
		}
		s.logGameEvent(session, event, "body_len", len(body), "reason", err)
		s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, err.Error(), len(body))
		if errors.Is(err, errCurrentCeraShopGiftRequestMalformed) {
			// 畸形包不是"这件商品不能送"：回 16「出现未知错误」，别让客户端去怪商品。
			return s.sendCurrentCeraShopPurchaseFailureWithCode(session, currentCeraShopPurchaseFailureCodeUnknown)
		}
		return s.sendCurrentCeraShopPurchaseFailure(session)
	}
	if request.GiftRecipientName != "" {
		// 赠送功能总开关（暂时关闭）：**先于一切判定**拒绝，不下单、不扣点券、不发邮件。
		// op136 已经拦过一次，这里是第二道 —— 不信任客户端是否走过那一步。
		if !currentCeraShopGiftEnabled {
			if noticeErr := s.sendCurrentCeraShopGiftClosedNotice(session, "cera_shop_gift_purchase_disabled"); noticeErr != nil {
				s.logGameEvent(session, "game-cera-shop-gift-closed-notice-failed",
					"char_id", session.selectedCharacterID,
					"error", noticeErr)
			}
			s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"item_count", len(request.Items),
				"body_len", len(body),
				"failure_code", currentCeraShopPurchaseFailureCodeGiftForbidden,
				"reason", "gift_feature_disabled")
			s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, "gift feature is disabled", len(body))
			return s.sendCurrentCeraShopGiftFailure(session, currentCeraShopPurchaseFailureCodeGiftForbidden)
		}
		// 赠送形态：先做赠送判定（互为好友满 7 天 / 同大区 / 不能送自己 /
		// 赠送方账号满 7 天 / 双方基础职业相同）。判定用的是 op64 **自己**的包，
		// 不信任客户端是否走过 op136 —— 否则改过的客户端可以直接跳过来下单。
		//
		// 判定失败必须在**扣钱之前**返回：这是"赠送不能被当成买给自己"之外的第二条
		// 经济红线（判定失败却已经扣钱 = 玩家花了钱什么都没拿到）。
		giftSessionSender, senderErr := s.currentCeraShopGiftSender(context.Background(), session)
		if senderErr != nil {
			s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"reason", "sender_unavailable",
				"error", senderErr)
			s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, senderErr.Error(), len(body))
			return s.sendCurrentCeraShopGiftFailure(session, currentCeraShopPurchaseFailureCodeGiftForbidden)
		}
		giftRecipient, giftErr := s.validateCurrentCeraShopGift(context.Background(), session, request)
		if giftErr != nil {
			code := currentCeraShopGiftFailureCode(giftErr)
			s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"item_count", len(request.Items),
				"body_len", len(body),
				"failure_code", code,
				"reason", giftErr)
			s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, giftErr.Error(), len(body))
			return s.sendCurrentCeraShopGiftFailure(session, code)
		}
		// 判定通过：走**和普通购买完全同一套**结账（价钱/限购/多买多送/点券瀑布都不变），
		// 商品由 commit 收集成邮件附件（result.GiftAttachments），结账后投递给收件人。
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		catalog, catalogErr := s.currentCeraShopCatalog()
		if catalogErr != nil {
			s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"reason", catalogErr)
			s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, catalogErr.Error(), len(body))
			return s.sendCurrentCeraShopGiftFailure(session, currentCeraShopPurchaseFailureCodeGiftForbidden)
		}
		result, commitErr := s.commitCurrentCeraShopPurchase(ctx, session, catalog, request)
		if commitErr != nil {
			code := currentCeraShopGiftFailureCode(commitErr)
			event := "game-cera-shop-gift-purchase-rejected"
			if errors.Is(commitErr, errCurrentPurchaseLimitReached) {
				event = "game-cera-shop-purchase-limit-rejected"
			}
			s.logGameEvent(session, event,
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"item_count", len(request.Items),
				"body_len", len(body),
				"failure_code", code,
				"reason", commitErr)
			s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, commitErr.Error(), len(body))
			if errors.Is(commitErr, errCurrentPurchaseLimitReached) {
				return s.sendCurrentCeraShopPurchaseLimitFailure(session)
			}
			return s.sendCurrentCeraShopGiftFailure(session, code)
		}
		// 钱已经扣了，从这里开始必须"要么投递成功、要么退款"。
		mailID, deliverErr := s.deliverCurrentCeraShopGift(ctx, session, request, result, giftSessionSender, giftRecipient)
		if deliverErr != nil {
			// 退款**必须换一个新 context**：上面这个 ctx 是给"判定 + 结账 + 投递"共用的
			// 3 秒预算，投递失败很可能就是因为它已经超时了。拿同一个（已过期的）
			// context 去退款，Checkout 会在第一行 ctx.Err() 上直接返回，
			// 于是"钱扣了、东西没给、退款也失败"，只剩一行日志。
			refundCtx, refundCancel := context.WithTimeout(context.Background(), createWriteTimeout)
			refundErr := s.refundCurrentCeraShopGift(refundCtx, session, result, giftSessionSender)
			refundCancel()
			// 这次结账已经吃掉的限购/多买多送计数与已发的一比一赠品**不会**被回滚
			// （它们和钱在同一次结账里提交）。这里把它们一起记进日志：真出现
			// delivery-failed 时，运营要能一眼看到"这一单还留下了什么"。
			s.logGameEvent(session, "game-cera-shop-gift-delivery-failed",
				"char_id", session.selectedCharacterID,
				"recipient_name", request.GiftRecipientName,
				"recipient_character_id", giftRecipient.characterID,
				"gift", currentCeraShopGiftResultSummary(result),
				"paid_cera", currentCeraShopGiftPaidSummary(result),
				"refund_error", refundErr,
				"buy_restrict_consumed", strings.Join(result.BuyRestrictNotes, ","),
				"buy_count_gift_count", result.BuyCountGiftCount,
				"buy_count_gift_rewards", strings.Join(result.BuyCountGiftNotes, ","),
				"error", deliverErr)
			s.recordCurrentCeraShopPurchase(session, request, result, false, deliverErr.Error(), len(body))
			return s.sendCurrentCeraShopGiftFailure(session, currentCeraShopGiftFailureCode(deliverErr))
		}
		s.recordCurrentCeraShopPurchase(session, request, result, true, "", len(body))
		s.logGameEvent(session, "game-cera-shop-gift-delivered",
			"char_id", session.selectedCharacterID,
			"recipient_name", request.GiftRecipientName,
			"recipient_character_id", giftRecipient.characterID,
			"mail_id", mailID,
			"gift", currentCeraShopGiftResultSummary(result),
			"attachment_count", len(result.GiftAttachments),
			"paid_cera", currentCeraShopGiftPaidSummary(result),
			"cera_after", result.CeraAfter,
			"commodities", currentCeraShopCommoditySummary(result.Products))
		if err := s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID, buildCurrentCeraShopWalletBody(result.CeraAfter, result.TokenCera, result.HappyTokenCera), 0); err != nil {
			return err
		}
		// 赠送形态同样可能买的是金币标价商品（付款方扣金币）。
		if err := s.sendCurrentCeraShopGoldWallet(session, result); err != nil {
			return err
		}
		// 付款方背包里**确实变了**的东西也得推给他：一比一赠品、多买多送跨档奖励都落在
		// 付款方身上（活动奖励归付款方），不推这一包的话玩家要小退/重建容器才看得见。
		for _, update := range result.Updates {
			if len(update.Entries) == 0 {
				continue
			}
			body := buildCurrentItemUpdateBody(update.ListType, update.Entries)
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
				return err
			}
		}
		if result.PetInventoryChanged {
			petBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentPetInventoryListType)
			if !ok {
				s.logGameEvent(session, "game-cera-shop-gift-pet-refresh-failed",
					"char_id", session.selectedCharacterID,
					"reason", "repository_snapshot_unavailable")
			} else if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), petBody); err != nil {
				return err
			}
		}
		// 多买多送次数只增在**付款方**身上（活动奖励归付款方），所以这里要刷新。
		if err := s.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "cera_shop_gift_after_ack"); err != nil {
			s.logGameEvent(session, "game-cera-shop-buycount-gift-send-failed", "source", "cera_shop_gift_after_ack", "err", err)
		}
		// 回包与普通购买**同形**：每件商品一条 op64 成功包，bonus 列表照旧带上一比一赠品
		// （那是付款方的活动奖励，要在付款方的"商城购物奖励"面板里显示）。
		for _, product := range result.Products {
			if err := s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketBuyCerashopItem),
				buildCurrentCeraShopPurchaseSuccessBodyWithCount(product.CommodityID, product.Count, catalog.bonusByCommodity[product.CommodityID]...),
				dnfproto.DefaultChannelClassification,
			); err != nil {
				return err
			}
		}
		return nil
	}
	if request.PaymentMode != currentCeraShopPaymentModeCera && request.PaymentMode != currentCeraShopPaymentModeAvatarCoupon {
		s.logGameEvent(session, "game-cera-shop-purchase-rejected",
			"body_len", len(body), "item_count", len(request.Items), "payment_mode", request.PaymentMode,
			"reason", errCurrentCeraShopPaymentUnsupported)
		s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, errCurrentCeraShopPaymentUnsupported.Error(), len(body))
		return s.sendCurrentCeraShopPurchaseFailure(session)
	}
	catalog, err := s.currentCeraShopCatalog()
	if err != nil {
		s.logGameEvent(session, "game-cera-shop-purchase-rejected", "body_len", len(body), "reason", err)
		s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, err.Error(), len(body))
		return s.sendCurrentCeraShopPurchaseFailure(session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := s.commitCurrentCeraShopPurchase(ctx, session, catalog, request)
	if err != nil {
		event := "game-cera-shop-purchase-rejected"
		if errors.Is(err, errCurrentPurchaseLimitReached) {
			// PVF 限购/商城限购超限单独记一条，便于与“点券不足/商品不可买”区分开。
			event = "game-cera-shop-purchase-limit-rejected"
		}
		s.logGameEvent(session, event,
			"body_len", len(body), "item_count", len(request.Items), "reason", err)
		s.recordCurrentCeraShopPurchase(session, request, currentCeraShopCommitResult{}, false, err.Error(), len(body))
		if errors.Is(err, errCurrentPurchaseLimitReached) {
			return s.sendCurrentCeraShopPurchaseLimitFailure(session)
		}
		return s.sendCurrentCeraShopPurchaseFailureWithCode(
			session,
			currentCeraShopPurchaseFailureCodeForCommitError(err),
		)
	}
	// 结账已经落库，从这里开始无论后续发包是否成功，这条消费都必须留在记录里。
	s.recordCurrentCeraShopPurchase(session, request, result, true, "", len(body))

	if result.NameTagActivation != nil {
		activation := result.NameTagActivation
		s.logGameEvent(session, "game-name-tag-card-applied",
			"char_id", session.selectedCharacterID,
			"action", activation.Action,
			"item_id", activation.ItemID,
			"prev_item_id", activation.PreviousItemID,
			"expire_time", activation.ExpireAt,
			"prev_expire_time", activation.PreviousExpire)
	}

	// 装扮栏扩展券是商城列表里唯一"由客户端现算商品号"的格子：当前客户端的
	// sub_16A03C0(ECX=角色对象) 读取装扮栏扩展阶段后直接返回 0x190F4+阶段 这张券
	// (0x190F4..0x19102 = 1..15 阶段, 0x197DF..0x197E3 = 16..20 阶段)，所以它在
	// 收到 class1/op64 购买成功回包时会立刻重建可见商品列表，并重新挑一次"下一阶段"。
	// 因此新阶段必须先于回包发布：先发回包的话，客户端重建列表时读到的仍是旧阶段，
	// 页面上依旧是刚买过的那张券，用户再点一次就是"买已拥有的阶段"→ 服务器拒绝，
	// 也就是"商城不实时刷新下一阶段"的现场。旧版 86JP 服务端在
	// CeraShopHandler 里对同一现象的处理就是先发 ITEM_LIST 再发购买回包
	// ("publish the new avatar expansion value first so that rebuild selects the
	// next stage")。背包/金库扩展券不依赖这一步，因为它们的券在商城里是多行静态
	// 列出的，不靠阶段推导，所以这里也只提前装扮栏这一张表。
	avatarInventoryRefreshedBeforeAck := false
	if result.AvatarInventoryExpansion != 0 {
		avatarBody, _, _, avatarOK := s.buildCurrentItemListBodyForSession(ctx, session, currentAvatarInventoryListType)
		switch {
		case !avatarOK:
			// 快照失败绝不拦回包：交易已经落库，回包必须发出去，否则客户端一直停在
			// "正在购买"，用户重试就是二次扣点。下面 ACK 之后的同一条刷新会重试。
			s.logGameEvent(session, "game-cera-shop-avatar-inventory-refresh-deferred",
				"char_id", session.selectedCharacterID,
				"reason", "repository_snapshot_unavailable")
		default:
			if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), avatarBody); err != nil {
				return err
			}
			avatarInventoryRefreshedBeforeAck = true
			s.logGameEvent(session, "game-cera-shop-avatar-inventory-refresh-before-ack",
				"char_id", session.selectedCharacterID,
				"list_type", currentAvatarInventoryListType,
				"avatar_inventory_expansion", result.AvatarInventoryExpansion,
				"body_len", len(avatarBody),
				"reason", "client_rebuilds_cerashop_product_list_on_purchase_ack")
		}
	}

	// 账号金库扩容券必须和装扮栏扩展券同一条口径：客户端是在**处理 class1/op64 购买回包
	// 的那一刻**重建仓库/金库面板并按当前容量铺格子的。回包之后再发 list 12 只更新行数据，
	// 不会重建格子——现场 2026-09-18：买了"扩大帐号金库"之后格子还画着锁、实际能放东西，
	// 切一下仓库页（重建面板）才正常。所以这里先把带新等级的 list 12 发出去，再发回包。
	accountCargoRefreshedBeforeAck := false
	if result.AccountCargoSelectionKey != 0 {
		cargoBody, _, _, cargoOK := s.buildCurrentItemListBodyForSession(ctx, session, 12)
		switch {
		case !cargoOK:
			// 和装扮栏一样：快照失败绝不拦回包（交易已落库），ACK 之后那条刷新会重试。
			s.logGameEvent(session, "game-cera-shop-account-cargo-refresh-deferred",
				"char_id", session.selectedCharacterID,
				"reason", "repository_snapshot_unavailable")
		default:
			if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), cargoBody); err != nil {
				return err
			}
			accountCargoRefreshedBeforeAck = true
			s.logGameEvent(session, "game-cera-shop-account-cargo-refresh-before-ack",
				"char_id", session.selectedCharacterID,
				"list_type", 12,
				"account_cargo_level", result.AccountCargoSelectionKey,
				"body_len", len(cargoBody),
				"reason", "client_rebuilds_vault_panel_on_purchase_ack")
		}
	}

	// 买满限购的商品必须在**购买回包之前**告诉客户端：当前客户端的商城列表是在收到
	// class1/op64 回包时重建的，通知晚于回包就会出现"这一格还在、但再买就被拒"的现象。
	if err := s.sendCurrentCeraShopBuyRestrictProjection(ctx, session, "cera_shop_purchase_before_ack"); err != nil {
		s.logGameEvent(session, "game-cera-shop-buy-restrict-send-failed", "err", err)
	}
	for _, product := range result.Products {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketBuyCerashopItem),
			buildCurrentCeraShopPurchaseSuccessBodyWithCount(product.CommodityID, product.Count, catalog.bonusByCommodity[product.CommodityID]...),
			dnfproto.DefaultChannelClassification,
		); err != nil {
			return err
		}
	}
	// 多买多送跨档拿到的赠品：**必须紧跟购买回包**推 `class0/0x02D2` 奖励弹窗。
	//
	// 时序是这条功能的命门：客户端的"购买结果"窗口是在收到 class1/op64 回包时建立的，
	// 0x02D2 是挂到那个窗口上、从它下面**滑下来**的"奖励活动"面板。发晚了（比如排到
	// 道具列表/钱包/次数通知之后）窗口已经收尾，面板就没有宿主可挂 —— 现场
	// 2026-09-19 就是这样：包发出去了、日志全对，客户端什么都不弹。
	//
	// 86JP 参考实现同样是"每条购物车行发出 ACK 之后、紧跟一包 0x02D2"
	// （CeraShopHandler.SendPurchaseAckAndBonusAsync 的 foreach）。
	//
	// 纯展示：失败只记日志，不让一次已经落库的结账变成失败。
	for _, reward := range result.BuyCountGiftRewards {
		if err := s.sendCurrentCeraShopBonusItemPopup(session, reward.ItemID, reward.Count, "cera_shop_buy_count_gift"); err != nil {
			s.logGameEvent(session, "game-cera-shop-bonus-item-send-failed",
				"source", "cera_shop_buy_count_gift", "item_id", reward.ItemID, "err", err)
		}
	}
	// The current client commits the Cera-shop result before it applies the
	// typed actor refresh. Sending mode0 before the class1/op64 success ACK can
	// race the shop UI owner and leave endpoint 30 empty until the next login.
	if result.NameTagActivation != nil {
		if err := s.currentSendNameTagRefresh(ctx, session); err != nil {
			s.logGameEvent(session, "game-name-tag-card-refresh-failed", "err", err)
		}
	}
	notifiedPremiumActivations := 0
	devilPremiumActivated := false
	crystalPremiumActivated := false
	blackDiamondActivated := false
	privateStorePremiumActivated := false
	// Current EXE class0/op66 (sub_1D61460, sub-op 2) reads a one-byte premium
	// type. Ordinary PVF premium types use that route. Devil service slots are
	// internal account types 580..587; sending them here truncates the type to
	// 68..75 and displays unrelated Korean tarot-card effects.
	for _, activation := range result.PremiumActivations {
		if premium.IsDevilSlotType(activation.PremiumType) {
			devilPremiumActivated = true
		}
		if activation.PremiumType == premium.TypeCrystal {
			crystalPremiumActivated = true
		}
		if premium.IsBlackDiamondStorageType(activation.PremiumType) {
			blackDiamondActivated = true
		}
		if activation.PremiumType == premium.TypePrivateStore {
			privateStorePremiumActivated = true
		}
		wireType, canNotify := currentPremiumActivationWireType(activation.PremiumType)
		if !canNotify {
			continue
		}
		if err := s.sendGameUpperRawClass(
			session,
			currentPremiumActivatedMsgID,
			buildCurrentPremiumActivatedBody(wireType, activation.RemainingSeconds),
			0,
		); err != nil {
			return err
		}
		notifiedPremiumActivations++
	}
	if blackDiamondActivated {
		if err := s.refreshCurrentBlackDiamondAfterActivation(ctx, session, "cera_shop_purchase_after_black_diamond_activation"); err != nil {
			return err
		}
	}
	if privateStorePremiumActivated {
		if err := s.currentSendPrivateStoreActorRefresh(ctx, session, "cera_shop_purchase_after_private_store_activation"); err != nil {
			return err
		}
	}
	if crystalPremiumActivated {
		// Current sub_1D61460 records premium type 97, but the crystal panel is
		// rebuilt by the raw class0/op898 handler sub_1E7FAD0. Without this
		// immediate follow-up a purchase made after scene bootstrap remains
		// locally disabled and the client never emits its op535 selection.
		if err := s.sendCurrentCrystalContractState(session, "cera_shop_purchase_after_crystal_contract_activation"); err != nil {
			return err
		}
	}
	for _, update := range result.Updates {
		if len(update.Entries) == 0 {
			continue
		}
		body := buildCurrentItemUpdateBody(update.ListType, update.Entries)
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	if result.PersonalCargoSlotCount != 0 || result.AccountCargoSelectionKey != 0 {
		// 仓库/金库容量变了：先重建仓库底板再发列表。当前 EXE 的面板格子是建面板时按容量
		// 铺的，只更新列表数据不会把新解锁的格子解锁（现场 2026-09-18：买了"扩大帐号金库"
		// 之后格子还画着锁、实际能放东西，切一下仓库页才正常）。
		if err := s.sendCurrentCargoPadResetBeforeItemLists(
			session,
			"cera_shop_purchase_after_ack_before_cargo_expansion_lists",
		); err != nil {
			return err
		}
	}
	if result.MainInventoryExpansion != 0 {
		mainBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentExeInventoryMainListType)
		if !ok {
			return errCurrentCeraShopOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), mainBody); err != nil {
			return err
		}
	}
	if result.AvatarInventoryExpansion != 0 && !avatarInventoryRefreshedBeforeAck {
		avatarBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentAvatarInventoryListType)
		if !ok {
			return errCurrentCeraShopOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), avatarBody); err != nil {
			return err
		}
	}
	if result.PersonalCargoSlotCount != 0 {
		cargoBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentExeInventoryPersonalCargoListType)
		if !ok {
			return errCurrentCeraShopOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), cargoBody); err != nil {
			return err
		}
	}
	if result.AccountCargoSelectionKey != 0 && !accountCargoRefreshedBeforeAck {
		cargoBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, 12)
		if !ok {
			return errCurrentCeraShopOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), cargoBody); err != nil {
			return err
		}
	}
	if result.PetInventoryChanged {
		petBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, currentPetInventoryListType)
		if !ok {
			return errCurrentCeraShopOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), petBody); err != nil {
			return err
		}
	}
	if devilPremiumActivated {
		if err := s.sendCurrentPremiumServiceState(session, "cera_shop_purchase_after_devil_contract_activation", currentPremiumServiceTypeForSession(session)); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID, buildCurrentCeraShopWalletBody(result.CeraAfter, result.TokenCera, result.HappyTokenCera), 0); err != nil {
		return err
	}
	if err := s.sendCurrentCeraShopGoldWallet(session, result); err != nil {
		return err
	}
	// 多买多送已购买次数（类 0 通知 1035）。客户端没有"请求次数"的上行包，纯靠服务端推；
	// 窗口每次打开都会重读这个值。这里重新加载一次角色取权威次数（事务已提交）。
	// 纯展示：失败只记日志，绝不让一次购买因为推送失败而变成失败。
	if err := s.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "cera_shop_purchase_after_ack"); err != nil {
		s.logGameEvent(session, "game-cera-shop-buycount-gift-send-failed", "source", "cera_shop_purchase_after_ack", "err", err)
	}
	if result.RenameEntitlementsAdded != 0 {
		if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketUseRenameCard), nil); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-cera-shop-purchase-committed",
		"item_count", len(result.Products), "container_update_count", len(result.Updates), "main_inventory_expansion", result.MainInventoryExpansion, "avatar_inventory_expansion", result.AvatarInventoryExpansion, "personal_cargo_slot_count", result.PersonalCargoSlotCount, "account_cargo_selection_key", result.AccountCargoSelectionKey, "cera_after", result.CeraAfter, "premium_activation_count", len(result.PremiumActivations),
		"premium_notification_count", notifiedPremiumActivations,
		"crystal_state_refreshed", crystalPremiumActivated,
		"private_store_license_notified", currentCeraShopActivationContains(result.PremiumActivations, premium.TypePrivateStore),
		// 商城限购：本次消费掉的计数（空 = 该商品不限购）。
		"buy_restrict", strings.Join(result.BuyRestrictNotes, ","),
		// 多买多送：本次结账后的累计次数与跨档发奖（空 = 本次没有商品参与该活动）。
		"buy_count_gift_count", result.BuyCountGiftCount,
		"buy_count_gift", strings.Join(result.BuyCountGiftNotes, ","),
		// 本次结账的商品号。**必须记**：多买多送/限购都只对特定商品生效，
		// 没有这一列就只能去翻收包 hex 才能判断"为什么没计数"（现场 2026-09-19 踩过）。
		"commodities", currentCeraShopCommoditySummary(result.Products),
		// 支付瀑布：各钱包实际扣掉多少（线上验收"代币券真的能花"就是看这三个数）。
		"paid_happy_token_cera", result.PaidHappyTokenCera,
		"paid_token_cera", result.PaidTokenCera,
		"paid_cera", result.PaidCera,
		// 金币标价商品实际扣掉的角色金币。**必须记**：没有这一列就无法从日志回答
		// "这单到底扣没扣金币"，而 2026-10-01 的现场正是卡在这个问题上。
		"paid_gold", result.PaidGold,
		"gold_after", result.GoldAfter,
		"ack_body", "current_exe_op64_sub_CD9490", "item_update_body", "current_exe_op14_raw77", "cera_body", "current_exe_op53_u8_u32_u32_u32", "premium_body", "current_exe_class0_op66_u8_types_only_internal_devil_slots_suppressed")
	return nil
}

// currentCeraShopCommoditySummary 把本次结账的商品号拼成 "108722,104412" 这样的串。
//
// 商城的多买多送与限购都**只对特定商品**生效（多买多送更是一张 112 个商品号的
// 白名单）。现场 2026-09-19 排查"为什么买了不计数"时，日志里没有商品号，只能去翻
// 收包 hex 手工解 —— 所以这一列是必须的，不是为了好看。
func currentCeraShopCommoditySummary(products []currentCeraShopProduct) string {
	if len(products) == 0 {
		return ""
	}
	parts := make([]string, 0, len(products))
	for _, product := range products {
		parts = append(parts, strconv.FormatUint(uint64(product.CommodityID), 10))
	}
	return strings.Join(parts, ",")
}

func currentCeraShopActivationContains(activations []currentCeraShopPremiumActivation, premiumType int64) bool {
	for _, activation := range activations {
		if activation.PremiumType == premiumType {
			return true
		}
	}
	return false
}

func (s *Service) commitCurrentCeraShopPurchase(
	ctx context.Context,
	session *gameSession,
	catalog *pvfCeraShopCatalog,
	request currentCeraShopPurchaseRequest,
) (currentCeraShopCommitResult, error) {
	if s == nil {
		return currentCeraShopCommitResult{}, errCurrentCeraShopOwnerUnavailable
	}
	// Settings records are routed by their scope while wallet/inventory rows
	// are routed by character id.  Those keys can live on different database
	// shards, so serialize this checkout owner and compensate the saved header
	// if the character settlement rejects; do not pretend the two shards share
	// one SQL transaction.
	s.ceraShopPurchaseMu.Lock()
	defer s.ceraShopPurchaseMu.Unlock()
	if session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.items == nil || len(request.Items) == 0 {
		return currentCeraShopCommitResult{}, errCurrentCeraShopOwnerUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil || repositories.RentalAssets == nil {
		return currentCeraShopCommitResult{}, errCurrentCeraShopOwnerUnavailable
	}
	// 赠送形态：本该落进付款方背包的**商品**改为收集成邮件附件
	// （见 cera_shop_gift_delivery.go）。判定用 request.GiftRecipientName，
	// 而不是任何客户端传来的"形态位"—— 名字是唯一经过了严格解析（长度/编码/非空）
	// 的赠送凭据。
	giftDelivery := strings.TrimSpace(request.GiftRecipientName) != ""
	giftAttachments := make([]dnfrepo.MailAttachment, 0, 2)
	giftGrantNotes := make([]string, 0, 2)
	purchases := make([]currentCeraShopResolvedPurchase, 0, len(request.Items))
	// totalCost 是点券标价之和（走 Checkout 的 Cost + 支付瀑布）；
	// totalGoldCost 是金币标价之和（只能在事务回调里扣角色金币，见下面 Project）。
	// 一次结账只允许其中一种非 0（见下面的混合车守卫）。
	var totalCost uint64
	var totalGoldCost uint64
	switch request.PaymentMode {
	case currentCeraShopPaymentModeCera:
	case currentCeraShopPaymentModeAvatarCoupon:
		// The current client puts every selected avatar in the same 15-byte
		// cart-row format.  Coupon checkout consumes one matching voucher per
		// row, atomically, so multi-select must use the same path as a Cera
		// cart instead of being rejected as a one-item-only purchase.
	default:
		return currentCeraShopCommitResult{}, errCurrentCeraShopPaymentUnsupported
	}
	// Any purchased item can be a premiumlist contract item (86JP
	// IsContractItem). The premium catalog is loaded lazily; both catalogs
	// come from the same runtime archive in production. The two contract
	// sections are meaningless without it and fail closed, while ordinary
	// sections tolerate a load failure (logged) and keep their normal
	// delivery path.
	var premiumCatalog *currentPremiumCatalog
	premiumCatalogFailed := false
	loadPremiumCatalog := func() error {
		if premiumCatalog != nil {
			return nil
		}
		loaded, err := s.currentPremiumCatalog()
		if err != nil {
			return err
		}
		premiumCatalog = loaded
		return nil
	}
	for _, requestItem := range request.Items {
		product, found := catalog.Product(requestItem.CommodityID)
		if !found {
			return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d", errCurrentCeraShopProductUnavailable, requestItem.CommodityID)
		}
		product, err := catalog.resolvePurchase(product)
		if err != nil {
			return currentCeraShopCommitResult{}, err
		}
		resolved := currentCeraShopResolvedPurchase{Product: product, AttributeValue: requestItem.AttributeValue}
		if request.PaymentMode == currentCeraShopPaymentModeAvatarCoupon {
			couponItemID, err := catalog.resolveAvatarCouponItemID(product)
			if err != nil {
				return currentCeraShopCommitResult{}, err
			}
			resolved.AvatarCouponItemID = couponItemID
		} else {
			// 一行只能有一种标价：金币或 Cera。
			//
			// 改造前这里**只看 CeraPrice**，金币标价行（Cera=0）因此被当成免费商品，
			// 这是现场"买了金币标价道具不扣金币"的直接原因。现在金币列也进来了。
			if product.GoldPrice != 0 && product.CeraPrice != 0 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d gold=%d cera=%d",
					errCurrentCeraShopProductPriceAmbiguous, product.CommodityID, product.GoldPrice, product.CeraPrice)
			}
			if product.GoldPrice != 0 {
				if uint64(product.GoldPrice) > math.MaxUint64-totalGoldCost {
					return currentCeraShopCommitResult{}, errCurrentCeraShopProductUnavailable
				}
				totalGoldCost += uint64(product.GoldPrice)
			} else {
				if uint64(product.CeraPrice) > math.MaxUint64-totalCost {
					return currentCeraShopCommitResult{}, errCurrentCeraShopProductUnavailable
				}
				totalCost += uint64(product.CeraPrice)
			}
		}
		switch {
		case product.Section == "selectable character premium":
			if err := loadPremiumCatalog(); err != nil {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: premium catalog: %v", errCurrentCeraShopProductUnavailable, err)
			}
			slot, ok := premiumCatalog.devilSlots[product.CommodityID]
			if !ok {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d is not a purchasable devil contract slot", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.PremiumType = premium.DevilSlotType(slot.Slot)
			resolved.PremiumDurationSeconds = slot.Days * 86400
			purchases = append(purchases, resolved)
			continue
		case product.Section == "charac premium package":
			if product.PremiumPackageDays == 0 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d has no contract day count", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.PremiumPackage = true
			resolved.PremiumDurationSeconds = int64(product.PremiumPackageDays) * 86400
			purchases = append(purchases, resolved)
			continue
		}
		if err := loadPremiumCatalog(); err != nil {
			if !premiumCatalogFailed {
				premiumCatalogFailed = true
				s.logPacketEvent("game-cera-shop-premium-catalog-unavailable", "error", err)
			}
		} else if contract, ok := premiumCatalog.contractsByItem[int64(product.ItemID)]; ok {
			resolved.PremiumType = contract.PremiumType
			resolved.PremiumDurationSeconds = contract.DurationSeconds
			purchases = append(purchases, resolved)
			continue
		}
		definition, definitionErr := catalog.items.ResolveItem(product.ItemID)
		if definitionErr != nil {
			return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d item=%d: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, definitionErr)
		}
		autoOpenRewards, isAutoOpenPackage, autoOpenErr := resolveCurrentCeraShopAutoOpenRewards(
			catalog,
			definition,
			product.Count,
			requestItem.AvatarChoices,
			requestItem.PackageSelections,
		)
		if autoOpenErr != nil {
			return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d item=%d: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, autoOpenErr)
		}
		if isAutoOpenPackage {
			resolved.AutoOpenRewards = autoOpenRewards
			purchases = append(purchases, resolved)
			continue
		}
		if durationSeconds, isNameTag := currentNameTagCardDuration(catalog.source, definition); isNameTag {
			resolved.NameTagDurationSeconds = durationSeconds
			purchases = append(purchases, resolved)
			continue
		}
		if catalog.isCharacterSlotExpansionTicket(definition) {
			if product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d character slot expansions require count=1", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.CharacterSlotExpansion = true
			purchases = append(purchases, resolved)
			continue
		}
		if autoUse, isRenameCard := catalog.characterRenameCardKind(definition); isRenameCard && autoUse {
			if product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d character rename cards require count=1", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.CharacterRenameEntitlement = true
			purchases = append(purchases, resolved)
			continue
		}
		if stage, isMainInventoryUpgrade := currentCeraShopMainInventoryUpgradeStage(definition); isMainInventoryUpgrade {
			if stage == 0 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d item=%d has an unsupported main inventory upgrade tier", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID)
			}
			if product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d main inventory upgrades require count=1", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.MainInventoryUpgradeStage = stage
		}
		if stage, isAvatarInventoryUpgrade, avatarUpgradeErr := catalog.avatarInventoryUpgradeStage(definition); avatarUpgradeErr != nil {
			return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d item=%d avatar inventory upgrade: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, avatarUpgradeErr)
		} else if isAvatarInventoryUpgrade {
			if stage == 0 || product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d avatar inventory upgrades require one supported stage", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.AvatarInventoryUpgradeStage = stage
		}
		if target, isPersonalCargoUpgrade := catalog.personalCargoUpgradeTarget(definition); isPersonalCargoUpgrade {
			if target == 0 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d item=%d has an unsupported personal cargo upgrade tier", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID)
			}
			if product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d personal cargo upgrades require count=1", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.PersonalCargoUpgradeTarget = target
		}
		if catalog.isAccountCargoUpgradeTool(definition) {
			if product.Count != 1 {
				return currentCeraShopCommitResult{}, fmt.Errorf("%w: commodity=%d account cargo upgrades require count=1", errCurrentCeraShopProductUnavailable, product.CommodityID)
			}
			resolved.AccountCargoUpgrade = true
		}
		purchases = append(purchases, resolved)
	}
	if totalCost > math.MaxInt64 {
		return currentCeraShopCommitResult{}, errCurrentCeraShopProductUnavailable
	}
	if totalGoldCost > math.MaxInt64 {
		return currentCeraShopCommitResult{}, errCurrentCeraShopProductUnavailable
	}
	// 混合车：整笔拒绝（在**扣任何钱之前**）。见 errCurrentCeraShopMixedPaymentUnsupported。
	if totalCost != 0 && totalGoldCost != 0 {
		return currentCeraShopCommitResult{}, fmt.Errorf("%w: gold=%d cera=%d commodities=%s",
			errCurrentCeraShopMixedPaymentUnsupported, totalGoldCost, totalCost,
			currentCeraShopCommodityIDList(request.Items))
	}
	goldCost := int64(totalGoldCost)
	if giftDelivery {
		// 赠送只能送"邮箱能表达成实例行"的道具。任何**结账即消费**的商品
		// （契约时长/改名卡/各种扩容/装扮券兑换）都只作用在付款方自己身上，
		// 放行就等于"送给别人 = 给自己买"。在**扣钱之前**整笔拒绝，
		// 客户端收到的是「该商品无法赠送」。
		for _, purchase := range purchases {
			if currentCeraShopGiftConsumedAtCheckout(purchase) {
				return currentCeraShopCommitResult{}, fmt.Errorf(
					"%w: commodity=%d item=%d is consumed at checkout and cannot be gifted",
					errCurrentCeraShopGiftNotGiftable, purchase.Product.CommodityID, purchase.Product.ItemID)
			}
		}
	}

	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	checkoutOwner, err := dnfcerashop.NewOwner(repositories)
	if err != nil {
		return currentCeraShopCommitResult{}, errCurrentCeraShopOwnerUnavailable
	}
	accountCargoUpgrades := 0
	for _, purchase := range purchases {
		if purchase.AccountCargoUpgrade {
			accountCargoUpgrades++
		}
	}
	if accountCargoUpgrades != 0 {
		if accountCargoUpgrades != 1 || len(purchases) != 1 {
			return currentCeraShopCommitResult{}, fmt.Errorf("%w: account cargo upgrade must be purchased alone", errCurrentCeraShopProductUnavailable)
		}
		return commitCurrentCeraShopAccountCargoUpgrade(ctx, checkoutOwner, accountID, characterID, purchases[0], int64(totalCost), goldCost, s.gameplayNow())
	}
	var result currentCeraShopCommitResult
	checkoutNow := s.gameplayNow().UTC()
	// restrictNotes 收集本次结账消费掉的限购（“维度:键:已购/上限”），只用于日志核对。
	// 事务回调可能被重试，所以在回调开头重置。
	restrictNotes := make([]string, 0, len(purchases))
	// 多买多送：同样的"事务回调可能重试，所以开头重置"约定。
	buyCountGiftNotes := make([]string, 0, 4)
	buyCountGiftRewards := make([]currentCeraShopBuyCountGiftTier, 0, 4)
	buyCountGiftAfter := int64(0)
	buyCountGiftPurchases := make(map[*currentCeraShopBuyCountGiftPlan]int)
	checkoutResult, err := checkoutOwner.Checkout(ctx, dnfcerashop.CheckoutCommand{
		AccountID:     accountID,
		CharacterID:   characterID,
		SettingsScope: dnfrepo.CharacterContainerStateScope(characterID),
		Cost:          int64(totalCost),
		// 支付瀑布：欢乐代币券 → 代币券 → 点券，依次扣到够为止。
		//
		// 客户端只上报"支付方式 0 = 点券"，没有"用代币券买"的选项，所以代币券能
		// 不能花掉完全由服务端决定 —— 不主动从这两个钱包扣，它们就是只能看不能用的
		// 数字。不够时仍然走既有的"点券不足"失败 ACK，客户端可见行为不变。
		PayPolicy: dnfcerashop.PayPolicyWaterfall,
		UpdatedAt: checkoutNow,
		Project: func(assets *dnfcerashop.CheckoutAssets) (dnfcerashop.CheckoutChanges, error) {
			if assets == nil || assets.Account == nil || assets.Character == nil || assets.Inventory == nil ||
				assets.Equipment == nil || assets.Settings == nil {
				return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopOwnerUnavailable
			}
			restrictNotes = restrictNotes[:0]
			buyCountGiftNotes = buyCountGiftNotes[:0]
			buyCountGiftRewards = buyCountGiftRewards[:0]
			buyCountGiftAfter = currentCeraShopBuyCountGiftCountForPlan(
				*assets.Character,
				catalog.BuyCountGiftPlan(),
			)
			clear(buyCountGiftPurchases)
			// 事务回调可能被重试：赠送的收集器也必须在这里清空，否则一次重试会
			// 让同一件商品被邮寄两次。
			giftAttachments = giftAttachments[:0]
			giftGrantNotes = giftGrantNotes[:0]
			if giftDelivery {
				// 每日赠送额度（账号维度、10 万点券/天）：额度记账必须和扣款在**同一次
				// 结账事务**里。超了就整笔回滚 —— 一分钱不扣，客户端收到 11
				//「超出赠送金额上限」。（文案 70107/0xA3 是"每个账号每天最多可以赠送
				// 10万点券的礼物"，在 op136 阶段也会先拦一次。）
				if err := consumeCurrentCeraShopGiftDailyCera(assets.Account, int64(totalCost), checkoutNow); err != nil {
					return dnfcerashop.CheckoutChanges{}, err
				}
			}
			containerStateAfter, mainInventoryExpansion, avatarInventoryExpansion, personalCargoSlotCount, hasContainerUpgrade, err :=
				currentCeraShopPrepareContainerUpgrades(
					assets.Settings,
					assets.SettingsFound,
					characterID,
					purchases,
					checkoutNow,
				)
			if err != nil {
				return dnfcerashop.CheckoutChanges{}, err
			}
			changes := dnfcerashop.CheckoutChanges{Settings: hasContainerUpgrade}
			if hasContainerUpgrade {
				*assets.Settings = containerStateAfter
			}
			placementMainSlotCount := uint16(0)
			if assets.Settings != nil {
				if state, stateErr := dnfrepo.CharacterContainerStateFromSettings(*assets.Settings, characterID); stateErr == nil {
					placementMainSlotCount = state.MainSlotCount
				}
			}
			account := assets.Account
			character := assets.Character
			inventory := assets.Inventory
			equipment := assets.Equipment
			// 金币标价商品：在**同一次结账事务**里扣角色金币。
			//
			// 为什么必须在这里而不是 Checkout 里：Checkout 的 Cost + 支付瀑布只认三个
			// **账号级点券钱包**（account_cera / token_cera / happy_token_cera），
			// 角色金币住在 character.Stats["gold"]（= dnf_characters.gold），
			// 只有 CheckoutAssets.Character 摸得到。同一事务保证"扣了金币没发货"与
			// "发了货没扣金币"都不可能发生（项目回调返回错误 = 整笔回滚）。
			goldAfter, goldErr := chargeCurrentCeraShopGold(character, goldCost)
			if goldErr != nil {
				return dnfcerashop.CheckoutChanges{}, goldErr
			}
			if goldCost != 0 {
				// 少这一句就是"扣在内存里、重启就回来"：金币是角色记录的一个字段，
				// 不显式要求保存 stats，Checkout 根本不会写角色行。
				changes.Character = true
			}
			changedSlots := make(map[string]currentCeraShopChangedSlot)
			// A cart may consume the same coupon stack more than once.  Preserve
			// only its final row image in the outbound list update; sending the
			// intermediate and final counts for the same slot makes this client
			// misread the multi-select response as an inventory-space failure.
			entriesByList := make(map[byte]map[int16]currentItemListEntry)
			recordEntry := func(listType byte, entry currentItemListEntry) {
				if len(entry.data) < 2 {
					return
				}
				entries := entriesByList[listType]
				if entries == nil {
					entries = make(map[int16]currentItemListEntry)
					entriesByList[listType] = entries
				}
				slot := int16(binary.LittleEndian.Uint16(entry.data[0:2]))
				entries[slot] = entry
			}
			premiumActivations := make([]currentCeraShopPremiumActivation, 0, len(purchases))
			petInventoryChanged := false
			inventoryChanged := false
			characterSlotLimit := uint16(0)
			renameEntitlementsAdded := uint16(0)
			var nameTagActivation *currentNameTagCardActivation
			premiumNow := checkoutNow
			for _, purchase := range purchases {
				product := purchase.Product
				// PVF 限购：商场与 NPC 商店共用同一套计数，按“被购买的道具 id”
				// 记账；账号维度落在账号记录 Metadata（本事务无条件保存），角色
				// 维度落在角色 Stats（需要 changes.Character）。
				if product.ItemID != 0 {
					if limitDefinition, resolveErr := catalog.items.ResolveItem(product.ItemID); resolveErr == nil {
						limitPolicy, limitState, limitErr := consumeCurrentPurchaseLimit(character, account, limitDefinition, product.Count, s.currentPurchaseLimitTime())
						if limitErr != nil {
							return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d item=%d scope=%s period=%s used=%d requested=%d limit=%d: %w",
								errCurrentPurchaseLimitReached, product.CommodityID, product.ItemID, limitPolicy.Scope, limitPolicy.Period,
								limitState.Used, product.Count, limitPolicy.Limit, limitErr)
						}
						if limitPolicy.Scope == purchaselimit.ScopeCharacter {
							changes.Character = true
						}
					}
				}
				// 商城自己的限购：`etc/cerashop.etc [buy restrict item]` 按**商品号**
				// 计角色维度次数（客户端工具提示“每个角色只能购买N次”就是这一列）；
				// 没有这一行时按道具文案 / 商品行 Marker==101 兜底成账号维度。
				// 一次结账的一行 = 买一次，所以每次消费 1。
				for _, restrictPolicy := range catalog.BuyRestrictPolicies(product.CommodityID) {
					_, restrictState, restrictErr := consumeCurrentPurchaseLimitPolicy(
						character, account, restrictPolicy, 1, s.currentPurchaseLimitTime())
					if restrictErr != nil {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d source=%s scope=%s used=%d requested=1 limit=%d %s: %w",
							errCurrentPurchaseLimitReached, product.CommodityID, restrictPolicy.Section, restrictPolicy.Scope,
							restrictState.Used, restrictPolicy.Limit, purchaselimit.CountKey(restrictPolicy), restrictErr)
					}
					if restrictPolicy.Scope == purchaselimit.ScopeCharacter {
						changes.Character = true
					}
					restrictNotes = append(restrictNotes, fmt.Sprintf("%s:%s:%d:%d/%d",
						restrictPolicy.Scope, restrictPolicy.Section, product.CommodityID,
						restrictState.Used+1, restrictPolicy.Limit))
				}
				// 多买多送：只有 `event/chn_event/chn_buycountgiftsecond.evt` 的
				// `[bonus event ipg]` 列出的商品才计数（折扣券的 +1000 镜像号也算，
				// 见 BuyCountGiftEligible）。一次结账的一行 = 买一次，
				// 与上面商城限购的记法保持一致。
				for _, plan := range catalog.BuyCountGiftPlans() {
					if catalog.buyCountGiftEligibleForPlan(plan, product.CommodityID) {
						buyCountGiftPurchases[plan]++
					}
				}
				if purchase.AvatarCouponItemID != 0 {
					entry, err := consumeCurrentCeraShopAvatarCoupon(inventory, purchase.AvatarCouponItemID)
					if err != nil {
						return dnfcerashop.CheckoutChanges{}, err
					}
					recordEntry(dnfrepo.MainInventoryListType, entry)
					inventoryChanged = true
				}
				if purchase.CharacterSlotExpansion {
					currentLimit := dnfrepo.AccountCharacterSlotLimit(*account)
					if currentLimit >= dnfrepo.MaxCharacterSlotLimit {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d character_slot_limit=%d", errCurrentCeraShopProductUnavailable, product.CommodityID, currentLimit)
					}
					if err := dnfrepo.SetAccountCharacterSlotLimit(account, currentLimit+1); err != nil {
						return dnfcerashop.CheckoutChanges{}, err
					}
					characterSlotLimit = uint16(currentLimit + 1)
					account.Metadata[currentCharacterSlotExtendEffectPendingMetadataKey] = strconv.Itoa(currentLimit + 1)
					continue
				}
				if purchase.CharacterRenameEntitlement {
					if character.Stats == nil {
						character.Stats = make(map[string]int64, 4)
					}
					if character.Level < currentCharacterRenameMinimumLevel {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d character level=%d requires=%d", errCurrentCeraShopProductUnavailable, product.CommodityID, character.Level, currentCharacterRenameMinimumLevel)
					}
					if character.Stats[currentCharacterRenameEntitlementStat] != 0 {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d character rename entitlement already active", errCurrentCeraShopProductUnavailable, product.CommodityID)
					}
					// Same 90-day window as the bag path: the product must not be
					// charged for a character that cannot rename yet.
					lastRename := character.Stats[currentCharacterRenameLastUnixStat]
					if lastRename < 0 || lastRename > 0 && checkoutNow.Unix() < lastRename+int64(currentCharacterRenameCooldown/time.Second) {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d character rename cooldown active", errCurrentCeraShopProductUnavailable, product.CommodityID)
					}
					character.Stats[currentCharacterRenameEntitlementStat] = 1
					changes.Character = true
					renameEntitlementsAdded++
					continue
				}
				if purchase.MainInventoryUpgradeStage != 0 || purchase.AvatarInventoryUpgradeStage != 0 || purchase.PersonalCargoUpgradeTarget != 0 {
					// Container upgrades are consumed at checkout. They have no
					// inventory row: the real effect is the persisted op13 header
					// state prepared above.
					continue
				}
				if purchase.PremiumPackage {
					// [charac premium package]: consumed at checkout; every 魔王契约
					// devil slot renews for the package day count (86JP
					// ConsumedOnPurchase + UpsertPremiumExpire per slot).
					for devilSlot := int64(0); devilSlot < premium.DevilSlotCount; devilSlot++ {
						premiumType := premium.DevilSlotType(devilSlot)
						premium.Upsert(account, premiumType, purchase.PremiumDurationSeconds, 1, premiumNow)
						premiumActivations = append(premiumActivations, currentCeraShopPremiumActivation{
							PremiumType:      premiumType,
							RemainingSeconds: premium.ExpireAt(*account, premiumType) - premiumNow.Unix(),
						})
					}
					continue
				}
				if purchase.PremiumType > 0 {
					// Devil contract slot or premiumlist contract item: consumed at
					// checkout, account-level premium renews; nothing enters a bag.
					premium.Upsert(account, purchase.PremiumType, purchase.PremiumDurationSeconds, 1, premiumNow)
					for _, activationType := range premium.ActivationTypes(purchase.PremiumType) {
						premiumActivations = append(premiumActivations, currentCeraShopPremiumActivation{
							PremiumType:      activationType,
							RemainingSeconds: premium.ExpireAt(*account, activationType) - premiumNow.Unix(),
						})
					}
					continue
				}
				if purchase.NameTagDurationSeconds > 0 {
					if nameTagActivation == nil {
						activation, err := applyCurrentNameTagCardAssets(
							character,
							equipment,
							product.ItemID,
							purchase.NameTagDurationSeconds,
							premiumNow,
						)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, err
						}
						nameTagActivation = &activation
						changes.Character = true
						changes.Equipment = true
					}
					continue
				}
				if len(purchase.AutoOpenRewards) != 0 {
					if giftDelivery {
						// 赠送：礼包自动展开出来的奖励属于**买到的商品**，要发给收件人。
						// 落格失败（放不下）在这里不该发生——赠送走邮件，不占付款方背包。
						for _, reward := range purchase.AutoOpenRewards {
							definition, err := currentPVFItemDefinitionForGrantAt(reward.Definition, premiumNow)
							if err != nil {
								return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d package=%d reward=%d expiration: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, reward.Definition.ItemID, err)
							}
							rewardProduct := currentCeraShopProduct{
								CommodityID: product.CommodityID,
								ItemID:      definition.ItemID,
								Count:       reward.Count,
								Section:     product.Section,
							}
							if reward.Avatar {
								rewardProduct.Section = "avatar"
							}
							if err := appendCurrentCeraShopGiftGrant(&giftAttachments, &giftGrantNotes, catalog.source, definition, rewardProduct, reward.Option, reward.Count); err != nil {
								return dnfcerashop.CheckoutChanges{}, err
							}
						}
					} else {
						for _, reward := range purchase.AutoOpenRewards {
							definition, err := currentPVFItemDefinitionForGrantAt(reward.Definition, premiumNow)
							if err != nil {
								return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d package=%d reward=%d expiration: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, reward.Definition.ItemID, err)
							}
							rewardProduct := currentCeraShopProduct{
								CommodityID: product.CommodityID,
								ItemID:      definition.ItemID,
								Count:       reward.Count,
								Section:     product.Section,
							}
							switch {
							case reward.Avatar:
								rewardProduct.Section = "avatar"
								for granted := uint32(0); granted < reward.Count; granted++ {
									slot, grantErr := grantCurrentCeraShopAvatar(inventory, catalog.source, definition, rewardProduct, reward.Option)
									if grantErr != nil {
										return dnfcerashop.CheckoutChanges{}, grantErr
									}
									change := currentCeraShopChangedSlot{ListType: 1, Slot: slot, Product: rewardProduct}
									changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
								}
								inventoryChanged = true
							case isCurrentCeraShopPetArtifactItem(definition):
								for granted := uint32(0); granted < reward.Count; granted++ {
									slot, grantErr := grantCurrentCeraShopPetArtifact(inventory, definition)
									if grantErr != nil {
										return dnfcerashop.CheckoutChanges{}, grantErr
									}
									change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: rewardProduct}
									changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
								}
								petInventoryChanged = true
								inventoryChanged = true
							case isCurrentCeraShopCreatureItem(definition):
								for granted := uint32(0); granted < reward.Count; granted++ {
									slot, grantErr := grantCurrentCeraShopPet(inventory, definition)
									if grantErr != nil {
										return dnfcerashop.CheckoutChanges{}, grantErr
									}
									change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: rewardProduct}
									changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
								}
								petInventoryChanged = true
								inventoryChanged = true
							case isCurrentCeraShopPetConsumable(definition):
								slots, grantErr := grantCurrentCeraShopPetConsumable(inventory, definition, reward.Count)
								if grantErr != nil {
									return dnfcerashop.CheckoutChanges{}, grantErr
								}
								for _, slot := range slots {
									change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: rewardProduct}
									changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
								}
								petInventoryChanged = true
								inventoryChanged = true
							default:
								slots, grantErr := grantCurrentCeraShopProduct(inventory, definition, reward.Count, placementMainSlotCount)
								if grantErr != nil {
									return dnfcerashop.CheckoutChanges{}, grantErr
								}
								for _, slot := range slots {
									change := currentCeraShopChangedSlot{ListType: dnfrepo.MainInventoryListType, Slot: int16(slot), Product: rewardProduct}
									changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
								}
								inventoryChanged = true
							}
						}
					}
					// Multibonus is keyed by the purchased commodity, not by the
					// physical source row.  An auto-open package still receives it,
					// while the [cera package] source itself never enters list 0.
					//
					// 赠送形态下这一段**照旧落在付款方**：一比一赠品是"买了才有"的
					// 活动奖励，按用户确认的口径归付款方（A 送道具给 B，奖励归 A）。
					for _, bonus := range catalog.bonusByCommodity[product.CommodityID] {
						bonusDefinition, err := catalog.items.ResolveItem(bonus.ItemID)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d bonus item=%d: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, bonus.ItemID, err)
						}
						bonusDefinition, err = currentPVFItemDefinitionForGrantAt(bonusDefinition, premiumNow)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d bonus item=%d expiration: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, bonus.ItemID, err)
						}
						var bonusMainSlots []uint16
						var bonusPetSlots []int16
						switch {
						case isCurrentCeraShopPetArtifactItem(bonusDefinition):
							for granted := uint32(0); granted < bonus.Count; granted++ {
								slot, grantErr := grantCurrentCeraShopPetArtifact(inventory, bonusDefinition)
								if grantErr != nil {
									return dnfcerashop.CheckoutChanges{}, grantErr
								}
								bonusPetSlots = append(bonusPetSlots, slot)
							}
						case isCurrentCeraShopCreatureItem(bonusDefinition):
							for granted := uint32(0); granted < bonus.Count; granted++ {
								slot, grantErr := grantCurrentCeraShopPet(inventory, bonusDefinition)
								if grantErr != nil {
									return dnfcerashop.CheckoutChanges{}, grantErr
								}
								bonusPetSlots = append(bonusPetSlots, slot)
							}
						case isCurrentCeraShopPetConsumable(bonusDefinition):
							var grantErr error
							bonusPetSlots, grantErr = grantCurrentCeraShopPetConsumable(inventory, bonusDefinition, bonus.Count)
							if grantErr != nil {
								return dnfcerashop.CheckoutChanges{}, grantErr
							}
						default:
							var grantErr error
							bonusMainSlots, grantErr = grantCurrentCeraShopProduct(inventory, bonusDefinition, bonus.Count, placementMainSlotCount)
							if grantErr != nil {
								return dnfcerashop.CheckoutChanges{}, grantErr
							}
						}
						inventoryChanged = true
						bonusProduct := currentCeraShopProduct{CommodityID: product.CommodityID, ItemID: bonus.ItemID, Section: product.Section}
						for _, slot := range bonusMainSlots {
							change := currentCeraShopChangedSlot{ListType: dnfrepo.MainInventoryListType, Slot: int16(slot), Product: bonusProduct, Bonus: true}
							changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						}
						for _, slot := range bonusPetSlots {
							change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: bonusProduct, Bonus: true}
							changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						}
						if len(bonusPetSlots) > 0 {
							petInventoryChanged = true
						}
					}
					continue
				}
				definition, err := catalog.items.ResolveItem(product.ItemID)
				if err != nil {
					return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d item=%d: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, err)
				}
				definition, err = currentCeraShopProductDefinitionForGrantAt(definition, product, premiumNow)
				if err != nil {
					return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d item=%d expiration: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, product.ItemID, err)
				}
				if giftDelivery {
					// 赠送：买到的商品直接变成邮件附件发给收件人，付款方背包一个格子都不动。
					// 这一件能不能邮寄由 appendCurrentCeraShopGiftGrant 判定（判定不通过就
					// 整笔结账回滚，点券不会扣），所以不需要在这里区分时装/宠物/消耗品。
					// 时装会走真正的落格函数，保住买到的时长与属性（见该函数注释）。
					if err := appendCurrentCeraShopGiftGrant(&giftAttachments, &giftGrantNotes, catalog.source, definition, product, purchase.AttributeValue, product.Count); err != nil {
						return dnfcerashop.CheckoutChanges{}, err
					}
				} else {
					if product.Section == "avatar" {
						slot, err := grantCurrentCeraShopAvatar(inventory, catalog.source, definition, product, purchase.AttributeValue)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, err
						}
						change := currentCeraShopChangedSlot{ListType: 1, Slot: slot, Product: product}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						inventoryChanged = true
						continue
					}
					if isCurrentCeraShopPetArtifactItem(definition) {
						slot, err := grantCurrentCeraShopPetArtifact(inventory, definition)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, err
						}
						change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: product}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						petInventoryChanged = true
						inventoryChanged = true
						continue
					}
					if isCurrentCeraShopCreatureItem(definition) {
						slot, err := grantCurrentCeraShopPet(inventory, definition)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, err
						}
						change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: product}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						petInventoryChanged = true
						inventoryChanged = true
						continue
					}
					if isCurrentCeraShopPetConsumable(definition) {
						slots, err := grantCurrentCeraShopPetConsumable(inventory, definition, product.Count)
						if err != nil {
							return dnfcerashop.CheckoutChanges{}, err
						}
						for _, slot := range slots {
							change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: product}
							changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
						}
						petInventoryChanged = true
						inventoryChanged = true
						continue
					}
					slots, err := grantCurrentCeraShopProduct(inventory, definition, product.Count, placementMainSlotCount)
					if err != nil {
						return dnfcerashop.CheckoutChanges{}, err
					}
					inventoryChanged = true
					for _, slot := range slots {
						change := currentCeraShopChangedSlot{ListType: dnfrepo.MainInventoryListType, Slot: int16(slot), Product: product}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
					}
				}
				// One-plus-one IPG gifts: absolute per-purchase counts from the
				// runtime multibonus document, granted into the same container and
				// transaction as the purchased product.
				//
				// 赠送形态下这一段**照旧落在付款方**：活动奖励归付款方（用户口径）。
				for _, bonus := range catalog.bonusByCommodity[product.CommodityID] {
					bonusDefinition, err := catalog.items.ResolveItem(bonus.ItemID)
					if err != nil {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d bonus item=%d: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, bonus.ItemID, err)
					}
					bonusDefinition, err = currentPVFItemDefinitionForGrantAt(bonusDefinition, premiumNow)
					if err != nil {
						return dnfcerashop.CheckoutChanges{}, fmt.Errorf("%w: commodity=%d bonus item=%d expiration: %v", errCurrentCeraShopProductUnavailable, product.CommodityID, bonus.ItemID, err)
					}
					var bonusMainSlots []uint16
					var bonusPetSlots []int16
					switch {
					case isCurrentCeraShopPetArtifactItem(bonusDefinition):
						for granted := uint32(0); granted < bonus.Count; granted++ {
							slot, grantErr := grantCurrentCeraShopPetArtifact(inventory, bonusDefinition)
							if grantErr != nil {
								return dnfcerashop.CheckoutChanges{}, grantErr
							}
							bonusPetSlots = append(bonusPetSlots, slot)
						}
					case isCurrentCeraShopCreatureItem(bonusDefinition):
						for granted := uint32(0); granted < bonus.Count; granted++ {
							slot, grantErr := grantCurrentCeraShopPet(inventory, bonusDefinition)
							if grantErr != nil {
								return dnfcerashop.CheckoutChanges{}, grantErr
							}
							bonusPetSlots = append(bonusPetSlots, slot)
						}
					case isCurrentCeraShopPetConsumable(bonusDefinition):
						var grantErr error
						bonusPetSlots, grantErr = grantCurrentCeraShopPetConsumable(inventory, bonusDefinition, bonus.Count)
						if grantErr != nil {
							return dnfcerashop.CheckoutChanges{}, grantErr
						}
					default:
						var grantErr error
						bonusMainSlots, grantErr = grantCurrentCeraShopProduct(inventory, bonusDefinition, bonus.Count, placementMainSlotCount)
						if grantErr != nil {
							return dnfcerashop.CheckoutChanges{}, grantErr
						}
					}
					inventoryChanged = true
					bonusProduct := currentCeraShopProduct{CommodityID: product.CommodityID, ItemID: bonus.ItemID, Section: product.Section}
					for _, slot := range bonusMainSlots {
						change := currentCeraShopChangedSlot{ListType: dnfrepo.MainInventoryListType, Slot: int16(slot), Product: bonusProduct, Bonus: true}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
					}
					for _, slot := range bonusPetSlots {
						change := currentCeraShopChangedSlot{ListType: currentPetInventoryListType, Slot: slot, Product: bonusProduct, Bonus: true}
						changedSlots[currentCeraShopInventorySlotKey(change.ListType, change.Slot)] = change
					}
					if len(bonusPetSlots) > 0 {
						petInventoryChanged = true
					}
				}
			}

			// 多买多送：把本次结账计入累计次数，并在**同一事务里**发放新跨越的档位奖励。
			//
			//   - 计数键落在角色 Stats（与 purchaselimit 角色维度同一套约定，零 schema 变更），
			//     所以小退、重启服务端之后进度都还在；
			//   - 发奖用的是与主商品、one-plus-one 赠品完全相同的落格函数（含邮件溢出兜底），
			//     失败就整笔结账失败——点券不会被扣，"扣了钱没给奖励"不会发生；
			//   - 奖励 = `(before, after]` 之间新跨越的档位。计数单调递增，所以每个档位
			//     天然只发一次，不需要额外的"已领取"位。
			// Every campaign has its own persisted count. A purchase can match
			// multiple campaigns; each one is then advanced independently in
			// this same transaction. The primary campaign keeps the historical
			// count key and remains the only count projected into the current
			// single client progress window.
			for _, plan := range catalog.BuyCountGiftPlans() {
				counted := buyCountGiftPurchases[plan]
				if !plan.Enabled() || counted <= 0 {
					continue
				}
				if counted > currentCeraShopBuyCountGiftMaxCountedPerCheckout {
					counted = currentCeraShopBuyCountGiftMaxCountedPerCheckout
				}
				before := currentCeraShopBuyCountGiftCountForPlan(*character, plan)
				after := before + int64(counted)
				rewards := plan.RewardsBetween(before, after)
				grants, grantErr := grantCurrentCeraShopBuyCountGiftRewards(
					catalog, inventory, rewards, placementMainSlotCount, premiumNow)
				if grantErr != nil {
					return dnfcerashop.CheckoutChanges{}, grantErr
				}
				for _, reward := range rewards {
					buyCountGiftNotes = append(buyCountGiftNotes,
						fmt.Sprintf("group:%s:tier:%d:item:%d:x%d",
							plan.GroupID, reward.Tier, reward.ItemID, reward.Count))
				}
				buyCountGiftRewards = append(buyCountGiftRewards, rewards...)
				for _, grant := range grants {
					// Section 用固定标记：这条更新的来源是活动奖励而不是买到的商品，
					// 日志/核对时一眼能分辨（ItemID 也换成了奖励道具而不是商品道具）。
					//
					// ⚠ 这里**不要**设 `Bonus: true`：那个字段在既有代码里是"一比一赠品"
					// 的专用标记，会让格子被写成 `last_cera_shop_bonus = "one_plus_one_ipg"`，
					// 把多买多送的奖励标成一比一赠品。识别来源靠 Section 就够了
					// （下面的物化循环会把 `last_cera_shop_section = "buy_count_gift"` 写进去）。
					change := currentCeraShopChangedSlot{
						ListType: grant.ListType,
						Slot:     grant.Slot,
						Product:  currentCeraShopProduct{ItemID: grant.ItemID, Section: "buy_count_gift"},
					}
					key := currentCeraShopInventorySlotKey(change.ListType, change.Slot)
					// 奖励和刚买到的道具可能并叠进**同一个格子**（同道具同格，或奖励落进
					// 本来就有的那堆）。这种情况下不要用奖励的 change 覆盖商品那条：
					// `changedSlots` 是按 列表:格 索引的，覆盖会把该格写成
					// `last_cera_shop_commodity=0`，把"这是买来的"这条线索抹掉。
					// 出站行内容取自最终 inventory，所以跳过登记不影响发给客户端的更新。
					if existing, claimed := changedSlots[key]; claimed && existing.Product.CommodityID != 0 {
						if grant.ListType == currentPetInventoryListType {
							petInventoryChanged = true
						}
						continue
					}
					changedSlots[key] = change
					if grant.ListType == currentPetInventoryListType {
						petInventoryChanged = true
					}
				}
				if len(grants) > 0 {
					inventoryChanged = true
				}
				setCurrentCeraShopBuyCountGiftCountForPlan(character, plan, after)
				// 计数写在角色 Stats 上，必须让事务把角色记录也一起保存。
				changes.Character = true
				if plan.Primary {
					buyCountGiftAfter = after
				}
			}

			for _, change := range changedSlots {
				key := currentCeraShopInventorySlotKey(change.ListType, change.Slot)
				stack, found := inventory.Slots[key]
				if !found || stack.ItemID <= 0 || (change.ListType != 1 && stack.Count <= 0) {
					return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopProductUnavailable
				}
				entry := currentItemListEntryFromStack(change.ListType, change.Slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 5)
				}
				stack.Extra["last_grant_source"] = "cera_shop"
				stack.Extra["last_cera_shop_commodity"] = strconv.FormatUint(uint64(change.Product.CommodityID), 10)
				stack.Extra["last_cera_shop_section"] = change.Product.Section
				if change.Bonus {
					stack.Extra["last_cera_shop_bonus"] = "one_plus_one_ipg"
				}
				if change.Product.DurationDays > 0 {
					stack.Extra["cera_shop_duration_days"] = strconv.FormatUint(uint64(change.Product.DurationDays), 10)
					stack.Extra["expiration_source"] = "runtime_pvf_cera_shop_duration_grant"
				}
				inventory.Slots[key] = stack
				recordEntry(change.ListType, entry)
			}
			updates := make([]currentCeraShopItemUpdate, 0, len(entriesByList))
			for _, listType := range []byte{dnfrepo.MainInventoryListType, 1} {
				entriesBySlot := entriesByList[listType]
				if len(entriesBySlot) == 0 {
					continue
				}
				entries := make([]currentItemListEntry, 0, len(entriesBySlot))
				for _, entry := range entriesBySlot {
					entries = append(entries, entry)
				}
				sort.Slice(entries, func(left, right int) bool {
					return binary.LittleEndian.Uint16(entries[left].data[0:2]) < binary.LittleEndian.Uint16(entries[right].data[0:2])
				})
				updates = append(updates, currentCeraShopItemUpdate{ListType: listType, Entries: entries})
			}
			products := make([]currentCeraShopProduct, 0, len(purchases))
			for _, purchase := range purchases {
				products = append(products, purchase.Product)
			}
			result = currentCeraShopCommitResult{
				Products:                 products,
				Updates:                  updates,
				MainInventoryExpansion:   mainInventoryExpansion,
				AvatarInventoryExpansion: avatarInventoryExpansion,
				PersonalCargoSlotCount:   personalCargoSlotCount,
				CharacterSlotLimit:       characterSlotLimit,
				RenameEntitlementsAdded:  renameEntitlementsAdded,
				PetInventoryChanged:      petInventoryChanged,
				PremiumActivations:       premiumActivations,
				NameTagActivation:        nameTagActivation,
				PaidGold:                 goldCost,
				GoldAfter:                goldAfter,
			}
			changes.Inventory = inventoryChanged
			return changes, nil
		},
	})
	if err != nil {
		return currentCeraShopCommitResult{}, err
	}
	result.CeraAfter = checkoutResult.CeraAfter
	result.TokenCera = checkoutResult.TokenCera
	result.HappyTokenCera = checkoutResult.HappyTokenCera
	// 把本次消费掉的限购抄进结果，供 committed 事件的日志核对（线上验收就是看这里
	// 有没有随购买一起增长）。
	result.BuyRestrictNotes = append(result.BuyRestrictNotes, restrictNotes...)
	result.BuyCountGiftCount = buyCountGiftAfter
	result.BuyCountGiftNotes = append(result.BuyCountGiftNotes, buyCountGiftNotes...)
	result.BuyCountGiftRewards = append(result.BuyCountGiftRewards, buyCountGiftRewards...)
	result.PaidHappyTokenCera = checkoutResult.Paid.FromHappyTokenCera
	result.PaidTokenCera = checkoutResult.Paid.FromTokenCera
	result.PaidCera = checkoutResult.Paid.FromCera
	// 赠送：把事务回调里收集到的商品附件带出结账。**必须复制**——收集器是闭包变量，
	// 普通购买时它是空的（nil 切片），赠送时它才是投递的唯一依据。
	result.GiftAttachments = append(result.GiftAttachments, giftAttachments...)
	result.GiftGrantNotes = append(result.GiftGrantNotes, giftGrantNotes...)
	return result, nil
}

// chargeCurrentCeraShopGold 在结账事务里扣角色金币，返回扣完后的余额。
//
// cost <= 0（点券标价商品、以及三个价格列全 0 的免费行）是**空操作**，直接返回 0 ——
// 这是"不修 A 坏 B"的关键：既有 177 行点券标价商品与那 1 行全 0 的 `[coin]` 行
// 走的都是这条路，行为一字不变。
//
// 调用方负责把 changes.Character 置位（否则扣了不落库）与整笔回滚（返回错误时）。
func chargeCurrentCeraShopGold(character *dnfrepo.CharacterRecord, cost int64) (int64, error) {
	if cost <= 0 {
		return 0, nil
	}
	if character == nil || character.Stats == nil {
		return 0, fmt.Errorf("%w: character stats are unavailable", errCurrentCeraShopGoldInsufficient)
	}
	held := character.Stats["gold"]
	if held < cost {
		// held < 0 是脏数据，同样按不够处理，绝不扣成负数。
		return 0, fmt.Errorf("%w: gold=%d cost=%d", errCurrentCeraShopGoldInsufficient, held, cost)
	}
	character.Stats["gold"] = held - cost
	return character.Stats["gold"], nil
}

// currentCeraShopCommodityIDList 把购物车里的商品号拼成 "104824,104825"。
// 只用于失败日志 —— 混合车/歧义标价被拒时，没有商品号就只能去翻收包 hex。
func currentCeraShopCommodityIDList(items []currentCeraShopPurchaseRequestItem) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strconv.FormatUint(uint64(item.CommodityID), 10))
	}
	return strings.Join(parts, ",")
}

func commitCurrentCeraShopAccountCargoUpgrade(
	ctx context.Context,
	owner *dnfcerashop.Owner,
	accountID string,
	characterID string,
	purchase currentCeraShopResolvedPurchase,
	cost int64,
	goldCost int64,
	now time.Time,
) (currentCeraShopCommitResult, error) {
	if owner == nil || accountID == "" || characterID == "" || cost < 0 || goldCost < 0 {
		return currentCeraShopCommitResult{}, errCurrentCeraShopOwnerUnavailable
	}
	var result currentCeraShopCommitResult
	checkout, err := owner.Checkout(ctx, dnfcerashop.CheckoutCommand{
		AccountID:     accountID,
		CharacterID:   characterID,
		SettingsScope: dnfrepo.CharacterContainerStateScope(characterID),
		Cost:          cost,
		// 与主购买路径同一套瀑布：金库升级也是商城里用点券买的商品，
		// 代币券在两边都能花，不能只让其中一边认。
		PayPolicy: dnfcerashop.PayPolicyWaterfall,
		UpdatedAt: now,
		Project: func(assets *dnfcerashop.CheckoutAssets) (dnfcerashop.CheckoutChanges, error) {
			if assets == nil || assets.Account == nil || assets.Character == nil ||
				assets.Character.Stats == nil {
				return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopOwnerUnavailable
			}
			account := assets.Account
			if account.Metadata == nil {
				account.Metadata = make(map[string]string, 4)
			}
			// 与主购买路径同口径：金币标价的金库升级工具也在同一事务里扣角色金币。
			// 当前 PVF 里 5 个金币标价商品都不是金库升级工具（goldCost 恒为 0），
			// 走的是空操作分支；留着是为了"金币商品一旦被配到这一类也不会白送"。
			goldAfter, goldErr := chargeCurrentCeraShopGold(assets.Character, goldCost)
			if goldErr != nil {
				return dnfcerashop.CheckoutChanges{}, goldErr
			}
			previous, err := strconv.ParseInt(strings.TrimSpace(account.Metadata["account_cargo_level"]), 10, 64)
			if err != nil || previous <= 0 {
				return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopProductUnavailable
			}
			next, ok := dnfcargo.NextAccountCargoTier(previous)
			if !ok || next <= previous || next > math.MaxUint16 {
				return dnfcerashop.CheckoutChanges{}, errCurrentCeraShopProductUnavailable
			}
			account.Metadata["account_cargo_created"] = "true"
			account.Metadata["account_cargo_level"] = strconv.FormatInt(next, 10)
			if strings.TrimSpace(account.Metadata["account_cargo_gold"]) == "" {
				account.Metadata["account_cargo_gold"] = "0"
			}
			result = currentCeraShopCommitResult{
				Products:                 []currentCeraShopProduct{purchase.Product},
				AccountCargoSelectionKey: uint16(next),
				PaidGold:                 goldCost,
				GoldAfter:                goldAfter,
			}
			if goldCost != 0 {
				return dnfcerashop.CheckoutChanges{Character: true}, nil
			}
			return dnfcerashop.CheckoutChanges{}, nil
		},
	})
	if err != nil {
		return currentCeraShopCommitResult{}, err
	}
	result.CeraAfter = checkout.CeraAfter
	result.TokenCera = checkout.TokenCera
	result.HappyTokenCera = checkout.HappyTokenCera
	// 支付瀑布的各钱包扣费量也要回填，否则这条路径的
	// paid_happy_token_cera / paid_token_cera / paid_cera 恒为 0，
	// 而"代币券真的能花"的验收就是看这三个数 —— 现场会误判成"券没被扣"。
	result.PaidHappyTokenCera = checkout.Paid.FromHappyTokenCera
	result.PaidTokenCera = checkout.Paid.FromTokenCera
	result.PaidCera = checkout.Paid.FromCera
	return result, nil
}
