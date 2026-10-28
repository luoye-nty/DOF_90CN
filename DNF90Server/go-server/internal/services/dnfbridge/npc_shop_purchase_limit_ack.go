// 本文件构造 op715 `SHOP_PURCHASE_COUNT` 的 ACK：把服务端权威的“已购次数”
// 下发给客户端。
//
// 客户端补丁注释（90CN.cpp 的 op715 分支）与原生商店窗口构造函数
// `sub_1D0E4B0`（0x17A0 字节窗口对象，含 4 张计数表：+0x16CC/+0x16E4/
// +0x175C/+0x1770）都把这份 ACK 读成“4 个 u32 计数表”。本服此前恒定回 4 张
// 空表，于是客户端只能用自己的本地计数：买一次加一次、而且加的数是本次入库后
// 的堆叠数（1→2→3…），所以看到 1/3/6/10/15；小退后本地表重建又没有权威值
// 可恢复，于是归零。
//
// 表序（按客户端 UI 的 4 行“账号永久 / 账号周期 / 角色永久 / 角色周期”分组，
// 周期维度把 weekly/daily/agitwarseason 归为“周期”）：
//
//	0 account   + 永久(accumulate)
//	1 account   + 周期(weekly/daily/season)
//	2 character + 永久(accumulate)
//	3 character + 周期(weekly/daily/season)
//
// 键 = 道具 id，值 = 已购次数（与客户端“（已购/上限个）”的显示语义一致）。
//
// 表序与键语义是协议契约，PVF 里查不到，只能真机验证：顺序不对就会显示成
// 另一行的数字，键不对则整行不刷新。改这里即可整体切换。
package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
)

// currentPurchaseLimitListCount 与 npc_shop_protocol.go 的
// currentNPCShopPurchaseCountListCount 是同一个协议常量（4 张表）。
const (
	currentPurchaseLimitListAccountPermanent   = 0
	currentPurchaseLimitListAccountPeriodic    = 1
	currentPurchaseLimitListCharacterPermanent = 2
	currentPurchaseLimitListCharacterPeriodic  = 3
)

func currentPurchaseLimitListIndex(scope purchaselimit.Scope, period purchaselimit.Period) int {
	if scope == purchaselimit.ScopeAccount {
		if period.Periodic() {
			return currentPurchaseLimitListAccountPeriodic
		}
		return currentPurchaseLimitListAccountPermanent
	}
	if period.Periodic() {
		return currentPurchaseLimitListCharacterPeriodic
	}
	return currentPurchaseLimitListCharacterPermanent
}

// currentPurchaseLimitContainers 读取限购计数所在的两个容器：
// 角色 Stats 与账号 Metadata。读失败时返回空容器（回包退化为空表，与旧行为
// 一致，不会因为读库失败把商店打开流程卡住）。
func (s *Service) currentPurchaseLimitContainers(ctx context.Context, session *gameSession) (map[string]int64, map[string]string) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	var stats map[string]int64
	if repositories.Character != nil {
		if character, found, err := repositories.Character.Load(ctx, characterID); err == nil && found {
			stats = character.Stats
		}
	}
	var metadata map[string]string
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID != "" && repositories.Account != nil {
		if account, found, err := repositories.Account.Load(ctx, accountID); err == nil && found {
			metadata = account.Metadata
		}
	}
	return stats, metadata
}

// currentPurchaseLimitBroadcastToAllLists 在“4 张表 ↔ UI 4 行”的映射被真机证实
// 之前保持 true：同一条计数同时写进 4 张表，客户端按哪张表查都能拿到权威值，
// 于是显示值一定正确（代价是无法从显示结果反推映射）。
//
// 证实步骤：把它改成 false 并把 4 张表填成不同值（例如 11/22/33/44），看
// 「账号1周（X/10个）」显示的是哪一个 → 得到 currentPurchaseLimitListIndex 的
// 正确表序，然后把这里的常量改回 false 定稿。
const currentPurchaseLimitBroadcastToAllLists = true

// currentNPCShopPurchaseCountLists 把权威计数按 4 张表的表序投影出来。
//
// 键同时下发“道具 id”和“该道具在商店 [item list] 里的槽位下标（0 基与 1 基各一份）”：
// 客户端到底按哪个索引这张表目前只能真机反推，两种都发就能命中；代价是同一件商品
// 在表里出现 3 条，客户端只查一条，不影响显示。
func (s *Service) currentNPCShopPurchaseCountLists(ctx context.Context, session *gameSession, shopID uint32) [currentNPCShopPurchaseCountListCount]map[uint32]uint32 {
	var lists [currentNPCShopPurchaseCountListCount]map[uint32]uint32
	stats, metadata := s.currentPurchaseLimitContainers(ctx, session)
	entries := purchaselimit.UsedCounts(stats, metadata, s.currentPurchaseLimitTime())
	slotKeys := s.currentPurchaseLimitShopSlotKeys(shopID)
	for _, entry := range entries {
		keys := make([]uint32, 0, 3)
		keys = append(keys, entry.ItemID)
		keys = append(keys, slotKeys[entry.ItemID]...)
		for _, index := range currentPurchaseLimitListIndexes(entry.Scope, entry.Period) {
			if lists[index] == nil {
				lists[index] = make(map[uint32]uint32, 4)
			}
			for _, key := range keys {
				lists[index][key] = entry.Used
			}
		}
	}
	return lists
}

// currentPurchaseLimitListsSummary 把 4 张表渲染成一行紧凑文本，便于日志核对
// “服务端到底下发了什么”（表序号:键=值）。
//
// 分隔符用 `|` 而不是空格：日志是 key=value 空格分隔的，字段里带空格会让
// `maps=(\S+)` 之类的抓取只拿到第一段（2026-09-14 实测踩过）。
func currentPurchaseLimitListsSummary(lists [currentNPCShopPurchaseCountListCount]map[uint32]uint32) string {
	var builder strings.Builder
	for index, entries := range lists {
		if len(entries) == 0 {
			continue
		}
		keys := make([]uint32, 0, len(entries))
		for itemID := range entries {
			keys = append(keys, itemID)
		}
		sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
		fmt.Fprintf(&builder, "%d:{", index)
		for _, itemID := range keys {
			fmt.Fprintf(&builder, "%d=%d,", itemID, entries[itemID])
		}
		builder.WriteString("}|")
	}
	summary := strings.TrimSuffix(builder.String(), "|")
	if len(summary) > 220 {
		summary = summary[:220] + "..."
	}
	if summary == "" {
		return "-"
	}
	return summary
}

// currentPurchaseLimitShopSlotKeys 返回商店里每件道具的槽位下标（0 基与 1 基）。
func (s *Service) currentPurchaseLimitShopSlotKeys(shopID uint32) map[uint32][]uint32 {
	catalog, err := s.currentNPCShopCatalog()
	if err != nil || catalog == nil || len(catalog.shopItems) == 0 {
		return nil
	}
	items := catalog.shopItems[shopID]
	if len(items) == 0 {
		return nil
	}
	keys := make(map[uint32][]uint32, len(items))
	for index, itemID := range items {
		keys[itemID] = append(keys[itemID], uint32(index), uint32(index)+1)
	}
	return keys
}

// currentPurchaseLimitListIndexes 返回一条计数应当写进哪几张表。
func currentPurchaseLimitListIndexes(scope purchaselimit.Scope, period purchaselimit.Period) []int {
	canonical := currentPurchaseLimitListIndex(scope, period)
	if !currentPurchaseLimitBroadcastToAllLists {
		return []int{canonical}
	}
	indexes := make([]int, 0, currentNPCShopPurchaseCountListCount)
	for index := 0; index < currentNPCShopPurchaseCountListCount; index++ {
		indexes = append(indexes, index)
	}
	return indexes
}

// buildCurrentNPCShopPurchaseCountMapsBody 按“前导成功字节 + 4 张 u32 计数表”
// 的布局编码 ACK。
func buildCurrentNPCShopPurchaseCountMapsBody(lists [currentNPCShopPurchaseCountListCount]map[uint32]uint32) []byte {
	var body packetWriter
	body.writeByte(1)
	for _, entries := range lists {
		body.writeUint32(uint32(len(entries)))
		if len(entries) == 0 {
			continue
		}
		keys := make([]uint32, 0, len(entries))
		for itemID := range entries {
			keys = append(keys, itemID)
		}
		sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
		for _, itemID := range keys {
			body.writeUint32(itemID)
			body.writeUint32(entries[itemID])
		}
	}
	return body.bytes()
}
