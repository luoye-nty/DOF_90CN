package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 商城"多买多送"（客户端窗口 BuyCountGiftItemWindow 第二代）。
//
// 事件表：`event/chn_event/chn_buycountgiftsecond.evt`
//
//	[bonus event ipg]   ← 参与计数的**商城商品号**清单
//	[bonus item]        ← 每 3 个数字一组：档位 道具号 数量
//	[buy count gift type] ← 0/1/2（客户端解析器只接受这三个值）
//
// 客户端协议（全部由反编译证实，详见
// `多买多送与代币券_客户端逆向规格_20260919.md` 附录 E）：
//
//   - `class0/0x040B`，包体 = **1 个 u32**（绝对次数）。handler `FUN_01d56450` 会把它
//     写进商城管理器 `+0x274C0`；次数不为 0 时顺带把门禁字节 `+0x274C4` 置 1，
//     多买多送窗口才会出现。窗口刷新 `FUN_01ac7fa0` 每次开窗都重读 `+0x274C0`。
//   - 客户端**没有**"领取"上行包（全字符串表没有任何 `ENUM_CMDPACKET_*BUY_COUNT*`），
//     所以奖励必须由服务端在购买事务里发。
//
// 设计要点：
//   - 计数落在**角色** `Stats`（与 purchaselimit 角色维度同一套约定，零 schema 变更）。
//   - 计数与发奖都在 `Checkout` 的 Project 回调里，**与扣费同一事务**：
//     要么玩家扣了点券也拿到了奖励，要么两者都没发生。
//   - 奖励 = 本次跨越的档位（`(before, after]`）。计数单调递增，所以每个档位天然只发一次，
//     不需要额外的"已领取"状态。
//   - 只实现真机 PVF 里的 `[buy count gift type] 0`；别的取值一律不启用并记账原因，
//     宁可什么都不做，也不按猜出来的语义发奖。
const (
	// currentCeraShopBuyCountGiftSecondPVFPath 是第二代多买多送事件表。
	currentCeraShopBuyCountGiftSecondPVFPath = "event/chn_event/chn_buycountgiftsecond.evt"
	// Additional independent campaigns use the same three-section grammar and
	// live beside the primary file:
	//
	//	event/chn_event/chn_buycountgiftsecond_<group>.evt
	//
	// The <group> suffix is the stable persisted identity. Renaming an extra
	// file intentionally starts a new progress counter for that campaign.
	currentCeraShopBuyCountGiftExtraPVFPathPrefix = "event/chn_event/chn_buycountgiftsecond_"
	// currentCeraShopBuyCountGiftSecondMsgID 是"已购买次数"类 0 通知的 opcode。
	// 同一个数字 1035 在类 1 表里是婚礼命令回包，所以这里必须发类 0（见 currentCeraShopClass0）。
	currentCeraShopBuyCountGiftSecondMsgID uint16 = 0x040B
	// currentCeraShopBuyCountGiftSecondCountKey 是角色 Stats 里的计数键。
	// 与 purchaselimit 的角色维度键同处一个 map，但不共用前缀，避免与限购键互相污染。
	currentCeraShopBuyCountGiftSecondCountKey = "cerashop_buycountgiftsecond_count"
	// currentCeraShopBuyCountGiftTypePerPurchase 是本服实现的计数模式（真机 PVF 的值）。
	currentCeraShopBuyCountGiftTypePerPurchase uint32 = 0
	// currentCeraShopBuyCountGiftTypeMax 是客户端解析器接受的类型上限：
	// 反编译里 `if (local_35 == 0 || 2 < uVar8) break;` —— 越界直接中断解析。
	currentCeraShopBuyCountGiftTypeMax uint32 = 2
	// currentCeraShopBuyCountGiftMaxCountedPerCheckout 是一次结账最多累计的次数。
	// 只是防御性上限（购物车行数本来就有上限），避免异常输入把计数推到离谱的量级。
	currentCeraShopBuyCountGiftMaxCountedPerCheckout = 4096
	// currentCeraShopBuyCountGiftDiscountProductIDOffset 是"折扣券镜像商品号"的偏移。
	//
	// 玩家选用商城折扣券时，客户端上报的是 `基础商品号 + 1000`（例 104278 -> 105278）。
	// 这个数字来自 86JP 参考实现 `CeraShopPurchaseBonusCatalog.DiscountProductIdOffset`；
	// 本服 `etc/cerashop.etc [discount list]` 目前是空的，所以镜像号暂时不会出现，
	// 但认它没有坏处，能避免将来配上折扣券后出现"用券买同一件礼包却不计数"的静默 bug。
	// 判定逻辑见 pvfCeraShopCatalog.BuyCountGiftEligible（还会要求两行发同一件道具）。
	currentCeraShopBuyCountGiftDiscountProductIDOffset uint32 = 1000
)

// currentCeraShopBuyCountGiftNotificationEnabled 允许一键关掉**下发**。
//
// 0x040B 是本轮新接的包：真机上如果客户端表现异常，把这里改成 false 即回到
// "服务端照常计数发奖、只是不告诉客户端"的状态，不需要回滚其它改动。
const currentCeraShopBuyCountGiftNotificationEnabled = true

// currentCeraShopBuyCountGiftTier 是 `[bonus item]` 的一行：累计购买到 Tier 次时
// 发放 ItemID × Count。
type currentCeraShopBuyCountGiftTier struct {
	Tier   int64
	ItemID uint32
	Count  uint32
}

// currentCeraShopBuyCountGiftPlan 是多买多送事件表的解析结果。
type currentCeraShopBuyCountGiftPlan struct {
	GroupID     string
	PVFPath     string
	CountKey    string
	Primary     bool
	giftType    uint32
	commodities map[uint32]struct{}
	tiers       []currentCeraShopBuyCountGiftTier
	// disabledReason 非空表示"解析到了但本服不启用"，附带可直接写进日志的原因。
	// 空字符串 + 非空 commodities/tiers = 启用。
	disabledReason string
}

// Enabled 表示这份计划是否参与计数与发奖。
func (p *currentCeraShopBuyCountGiftPlan) Enabled() bool {
	return p != nil && p.disabledReason == "" && len(p.commodities) > 0 && len(p.tiers) > 0
}

// CountsTowardGift 判断某个商品号是否在 `[bonus event ipg]` 清单里。
func (p *currentCeraShopBuyCountGiftPlan) CountsTowardGift(commodityID uint32) bool {
	if !p.Enabled() || commodityID == 0 {
		return false
	}
	_, found := p.commodities[commodityID]
	return found
}

// MaxTier 是最高档位（没有档位时 0）。
func (p *currentCeraShopBuyCountGiftPlan) MaxTier() int64 {
	if p == nil || len(p.tiers) == 0 {
		return 0
	}
	max := int64(0)
	for _, tier := range p.tiers {
		if tier.Tier > max {
			max = tier.Tier
		}
	}
	return max
}

// Tiers 返回档位表（升序副本，供日志与测试使用）。
func (p *currentCeraShopBuyCountGiftPlan) Tiers() []currentCeraShopBuyCountGiftTier {
	if p == nil {
		return nil
	}
	out := make([]currentCeraShopBuyCountGiftTier, len(p.tiers))
	copy(out, p.tiers)
	return out
}

// RewardsBetween 返回累计次数从 before 涨到 after 时**新跨越**的档位。
//
// 计数单调递增，所以每个档位只会在这里出现一次，不需要额外的"已领取"状态。
// 一档有多个道具行时（同一 Tier 出现多行）会全部返回。
func (p *currentCeraShopBuyCountGiftPlan) RewardsBetween(before, after int64) []currentCeraShopBuyCountGiftTier {
	if !p.Enabled() || after <= before {
		return nil
	}
	var out []currentCeraShopBuyCountGiftTier
	for _, tier := range p.tiers {
		if tier.Tier > before && tier.Tier <= after {
			out = append(out, tier)
		}
	}
	return out
}

// parseCurrentCeraShopBuyCountGiftSecond 解析第二代多买多送事件表。
//
// **永远不返回错误**：这张表是可选的活动配置，缺文件、缺段落、类型不认识都应该让
// 功能安静地不启用，而不是让整个商城目录构造失败（商城里还有一堆无关商品）。
// 不启用的原因记在 disabledReason 里，由调用方打一次日志。
func parseCurrentCeraShopBuyCountGiftSecond(source dnfpvf.Source) *currentCeraShopBuyCountGiftPlan {
	return parseCurrentCeraShopBuyCountGiftAtPath(
		source,
		currentCeraShopBuyCountGiftSecondPVFPath,
		"primary",
		true,
	)
}

func parseCurrentCeraShopBuyCountGiftAtPath(
	source dnfpvf.Source,
	pvfPath string,
	groupID string,
	primary bool,
) *currentCeraShopBuyCountGiftPlan {
	countKey := currentCeraShopBuyCountGiftSecondCountKey
	if !primary {
		countKey += "." + groupID
	}
	plan := &currentCeraShopBuyCountGiftPlan{
		GroupID:  groupID,
		PVFPath:  pvfPath,
		CountKey: countKey,
		Primary:  primary,
	}
	if source == nil {
		plan.disabledReason = "no pvf source"
		return plan
	}
	document, err := parseDungeonCardPVFDocument(source, pvfPath)
	if err != nil || document == nil {
		plan.disabledReason = fmt.Sprintf("read %s: %v", pvfPath, err)
		return plan
	}

	ipgTokens, found := document.Section("bonus event ipg")
	if !found {
		plan.disabledReason = "no [bonus event ipg] section"
		return plan
	}
	tierTokens, found := document.Section("bonus item")
	if !found {
		plan.disabledReason = "no [bonus item] section"
		return plan
	}

	commodities := make(map[uint32]struct{})
	for _, token := range ipgTokens {
		// 段落里可能有换行分隔的多个数字，也可能夹着非数字 token（注释等），
		// 一律只认整数，别的跳过而不是报错。
		if token.Kind != dnfpvf.TokenInt || token.Int <= 0 {
			continue
		}
		commodities[uint32(token.Int)] = struct{}{}
	}
	if len(commodities) == 0 {
		plan.disabledReason = "[bonus event ipg] has no commodity ids"
		return plan
	}

	ints := make([]int64, 0, len(tierTokens))
	for _, token := range tierTokens {
		if token.Kind != dnfpvf.TokenInt {
			continue
		}
		ints = append(ints, token.Int)
	}
	if len(ints) == 0 || len(ints)%3 != 0 {
		// 3 个一组（档位 道具号 数量），列数不对说明表坏了：不启用比猜一行安全。
		plan.disabledReason = fmt.Sprintf("[bonus item] token count %d is not a multiple of 3", len(ints))
		return plan
	}
	tiers := make([]currentCeraShopBuyCountGiftTier, 0, len(ints)/3)
	for index := 0; index+2 < len(ints); index += 3 {
		tier := currentCeraShopBuyCountGiftTier{Tier: ints[index], ItemID: uint32(ints[index+1]), Count: uint32(ints[index+2])}
		if tier.Tier <= 0 || tier.ItemID == 0 || tier.Count == 0 {
			plan.disabledReason = fmt.Sprintf("[bonus item] invalid row tier=%d item=%d count=%d", tier.Tier, tier.ItemID, tier.Count)
			return plan
		}
		tiers = append(tiers, tier)
	}
	if len(tiers) == 0 {
		plan.disabledReason = "[bonus item] has no rows"
		return plan
	}

	giftType := currentCeraShopBuyCountGiftTypePerPurchase
	if typeTokens, typeFound := document.Section("buy count gift type"); typeFound {
		for _, token := range typeTokens {
			if token.Kind == dnfpvf.TokenInt {
				giftType = uint32(token.Int)
				break
			}
		}
	}
	if giftType > currentCeraShopBuyCountGiftTypeMax {
		// 客户端自己也会因此中断解析，说明表本身就不对。
		plan.disabledReason = fmt.Sprintf("[buy count gift type] %d exceeds client parser bound %d", giftType, currentCeraShopBuyCountGiftTypeMax)
		return plan
	}
	if giftType != currentCeraShopBuyCountGiftTypePerPurchase {
		// 只实现真机在用的那一种。别的取值语义未经证实，宁可不动。
		plan.disabledReason = fmt.Sprintf("[buy count gift type] %d is not implemented (only %d)", giftType, currentCeraShopBuyCountGiftTypePerPurchase)
		return plan
	}

	plan.giftType = giftType
	plan.commodities = commodities
	plan.tiers = tiers
	return plan
}

// parseCurrentCeraShopBuyCountGiftPlans parses the primary campaign first,
// then discovers every additional file by the fixed prefix. The primary plan
// remains the first entry and keeps its historical count key. Extra files are
// isolated: a malformed or missing extra file disables only that campaign and
// never changes the primary campaign.
func parseCurrentCeraShopBuyCountGiftPlans(source dnfpvf.Source) []*currentCeraShopBuyCountGiftPlan {
	primary := parseCurrentCeraShopBuyCountGiftSecond(source)
	plans := []*currentCeraShopBuyCountGiftPlan{primary}
	ranger, ok := source.(interface {
		RangePaths(func(string) bool)
	})
	if !ok {
		return plans
	}
	groups := make(map[string]string)
	ranger.RangePaths(func(archivePath string) bool {
		normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(archivePath, "\\", "/")))
		groupID, valid := currentCeraShopBuyCountGiftExtraGroupID(normalized)
		if valid {
			groups[groupID] = normalized
		}
		return true
	})
	groupIDs := make([]string, 0, len(groups))
	for groupID := range groups {
		groupIDs = append(groupIDs, groupID)
	}
	sort.Strings(groupIDs)
	for _, groupID := range groupIDs {
		plans = append(plans, parseCurrentCeraShopBuyCountGiftAtPath(
			source,
			groups[groupID],
			groupID,
			false,
		))
	}
	return plans
}

func currentCeraShopBuyCountGiftExtraGroupID(archivePath string) (string, bool) {
	if !strings.HasPrefix(archivePath, currentCeraShopBuyCountGiftExtraPVFPathPrefix) ||
		!strings.HasSuffix(archivePath, ".evt") {
		return "", false
	}
	groupID := strings.TrimSuffix(
		strings.TrimPrefix(archivePath, currentCeraShopBuyCountGiftExtraPVFPathPrefix),
		".evt",
	)
	if groupID == "" {
		return "", false
	}
	for _, char := range groupID {
		if (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '_' || char == '-' {
			continue
		}
		return "", false
	}
	if len(currentCeraShopBuyCountGiftSecondCountKey)+1+len(groupID) > 128 {
		return "", false
	}
	return groupID, true
}

// currentCeraShopBuyCountGiftCount 读角色当前的累计次数。缺键 / 负值按 0 处理。
func currentCeraShopBuyCountGiftCount(character dnfrepo.CharacterRecord) int64 {
	return currentCeraShopBuyCountGiftCountForKey(character, currentCeraShopBuyCountGiftSecondCountKey)
}

func currentCeraShopBuyCountGiftCountForPlan(
	character dnfrepo.CharacterRecord,
	plan *currentCeraShopBuyCountGiftPlan,
) int64 {
	if plan == nil {
		return 0
	}
	return currentCeraShopBuyCountGiftCountForKey(character, plan.CountKey)
}

func currentCeraShopBuyCountGiftCountForKey(character dnfrepo.CharacterRecord, key string) int64 {
	if character.Stats == nil {
		return 0
	}
	value := character.Stats[key]
	if value < 0 {
		return 0
	}
	return value
}

// setCurrentCeraShopBuyCountGiftCount 写回累计次数（负数夹到 0）。
func setCurrentCeraShopBuyCountGiftCount(character *dnfrepo.CharacterRecord, count int64) {
	setCurrentCeraShopBuyCountGiftCountForKey(character, currentCeraShopBuyCountGiftSecondCountKey, count)
}

func setCurrentCeraShopBuyCountGiftCountForPlan(
	character *dnfrepo.CharacterRecord,
	plan *currentCeraShopBuyCountGiftPlan,
	count int64,
) {
	if plan == nil {
		return
	}
	setCurrentCeraShopBuyCountGiftCountForKey(character, plan.CountKey, count)
}

func setCurrentCeraShopBuyCountGiftCountForKey(
	character *dnfrepo.CharacterRecord,
	key string,
	count int64,
) {
	if character == nil {
		return
	}
	if count < 0 {
		count = 0
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 4)
	}
	character.Stats[key] = count
}

// buildCurrentCeraShopBuyCountGiftBody 编码 `class0/0x040B` 的包体：一个 u32 绝对次数。
//
// 客户端 handler `FUN_01d56450` 整包只读这一个 u32，多余的字节会被静默忽略，
// 所以这里严格按 4 字节发，不多不少。
func buildCurrentCeraShopBuyCountGiftBody(count int64) []byte {
	if count < 0 {
		count = 0
	}
	body := make([]byte, 4)
	binary.LittleEndian.PutUint32(body, uint32(count))
	return body
}

// sendCurrentCeraShopBuyCountGiftProjection 把角色的累计购买次数下发给客户端。
//
// 与 860 限购投影同款：纯展示。取不到目录/角色就静默返回 nil，绝不把选角进城流程带崩。
// 次数为 0 时**照发**：客户端 handler 会无条件把 0 写进 `+0x274C0`（写次数在可见性
// 判断之前），正好清掉上一次会话残留的旧次数。
func (s *Service) sendCurrentCeraShopBuyCountGiftProjection(ctx context.Context, session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !currentCeraShopBuyCountGiftNotificationEnabled {
		return nil
	}
	catalog, err := s.currentCeraShopCatalog()
	if err != nil || catalog == nil || !catalog.buyCountGift.Enabled() {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return nil
	}
	count := currentCeraShopBuyCountGiftCount(character)
	body := buildCurrentCeraShopBuyCountGiftBody(count)
	if err := s.sendGameUpperRawClass(session, currentCeraShopBuyCountGiftSecondMsgID, body, currentCeraShopClass0); err != nil {
		return err
	}
	s.logPacketEvent("game-cera-shop-buycount-gift-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"count", count,
		"max_tier", catalog.buyCountGift.MaxTier(),
		// 参与计数的商品号数量：一眼就能看出活动到底有没有被加载
		// （112 = 真机 PVF 的值；0 或缺失 = 事件表没读到，功能等于关闭）。
		"participating_commodities", len(catalog.buyCountGift.commodities),
		"body_len", len(body),
		"wire", "class0/op1035-u32")
	return nil
}

// currentCeraShopBuyCountGiftGrant 是一次多买多送发奖落到的具体格子。
// 调用方拿它去登记出站道具更新（与 one-plus-one 赠品同一套机制）。
type currentCeraShopBuyCountGiftGrant struct {
	Tier     int64
	ItemID   uint32
	ListType byte
	Slot     int16
}

// grantCurrentCeraShopBuyCountGiftRewards 把跨越档位的奖励发进背包。
//
// 走的是与购买主商品、one-plus-one 赠品**完全相同**的落格函数（含溢出邮件兜底），
// 所以不会出现"商城买东西能进邮件、活动奖励却卡住"这种不一致。
// 主背包 / 宠物装备 / 宠物 / 宠物消耗品四类都覆盖，判据用与购买路径同一套谓词。
func grantCurrentCeraShopBuyCountGiftRewards(
	catalog *pvfCeraShopCatalog,
	inventory *dnfrepo.InventoryRecord,
	rewards []currentCeraShopBuyCountGiftTier,
	placementMainSlotCount uint16,
	grantedAt time.Time,
) ([]currentCeraShopBuyCountGiftGrant, error) {
	if catalog == nil || catalog.items == nil || inventory == nil || len(rewards) == 0 {
		return nil, nil
	}
	grants := make([]currentCeraShopBuyCountGiftGrant, 0, len(rewards))
	for _, reward := range rewards {
		definition, err := catalog.items.ResolveItem(reward.ItemID)
		if err != nil {
			return nil, fmt.Errorf("%w: buy count gift tier=%d item=%d: %v", errCurrentCeraShopProductUnavailable, reward.Tier, reward.ItemID, err)
		}
		definition, err = currentPVFItemDefinitionForGrantAt(definition, grantedAt)
		if err != nil {
			return nil, fmt.Errorf("%w: buy count gift tier=%d item=%d expiration: %v", errCurrentCeraShopProductUnavailable, reward.Tier, reward.ItemID, err)
		}
		switch {
		case isCurrentCeraShopPetArtifactItem(definition):
			for granted := uint32(0); granted < reward.Count; granted++ {
				slot, grantErr := grantCurrentCeraShopPetArtifact(inventory, definition)
				if grantErr != nil {
					return nil, grantErr
				}
				grants = append(grants, currentCeraShopBuyCountGiftGrant{
					Tier: reward.Tier, ItemID: reward.ItemID, ListType: currentPetInventoryListType, Slot: slot,
				})
			}
		case isCurrentCeraShopCreatureItem(definition):
			for granted := uint32(0); granted < reward.Count; granted++ {
				slot, grantErr := grantCurrentCeraShopPet(inventory, definition)
				if grantErr != nil {
					return nil, grantErr
				}
				grants = append(grants, currentCeraShopBuyCountGiftGrant{
					Tier: reward.Tier, ItemID: reward.ItemID, ListType: currentPetInventoryListType, Slot: slot,
				})
			}
		case isCurrentCeraShopPetConsumable(definition):
			slots, grantErr := grantCurrentCeraShopPetConsumable(inventory, definition, reward.Count)
			if grantErr != nil {
				return nil, grantErr
			}
			for _, slot := range slots {
				grants = append(grants, currentCeraShopBuyCountGiftGrant{
					Tier: reward.Tier, ItemID: reward.ItemID, ListType: currentPetInventoryListType, Slot: slot,
				})
			}
		default:
			slots, grantErr := grantCurrentCeraShopProduct(inventory, definition, reward.Count, placementMainSlotCount)
			if grantErr != nil {
				return nil, grantErr
			}
			for _, slot := range slots {
				grants = append(grants, currentCeraShopBuyCountGiftGrant{
					Tier: reward.Tier, ItemID: reward.ItemID, ListType: dnfrepo.MainInventoryListType, Slot: int16(slot),
				})
			}
		}
	}
	return grants, nil
}
