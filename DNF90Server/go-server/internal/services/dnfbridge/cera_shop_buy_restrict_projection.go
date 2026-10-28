// 商城"限购已买满"的商品列表通知（类 0 通知 860）。
//
// 现场要求（2026-09-18 运营）：账号限购的商品买完以后应当**从商品列表里消失**，
// 而且小退/大退/重启服务端之后依然是消失的——就像背包扩展券买满 3 次之后永久消失。
// 客户端的限购记忆只在会话内存里，所以这份状态必须由服务端在每个会话重新下发。
//
// 逆向依据（真机 90CN DNF.exe，2026-09-18）：
//   - 客户端字符串：`ENUM_NOTIPACKET_BUY_RESTRICT_ITEM_LIST`（带 `-restrictedEndTime : [%d]`），
//     "BUY_RESTRICT_ITEM_LIST" 这个名字就是"限购商品列表"。
//   - NOTI 注册表（push handler / push opcode / call sub_2262980）实测：
//     处理器 0x01D557D0 注册在 **opcode 0x35C = 860**。
//   - 处理器 0x01D557D0 的读包顺序：先读 u32 条数，再每条读 3 个 u32：
//     A（条目号）B（到期时间）C（标志）
//     然后按 (B, C) 分派到商店管理器（[0x51B2764]）：
//     C != 0 → 只记标志，不动限制；
//     B == 0x7FFF53F0（哨兵）→ 解除限制（Remove(A)）；
//     其它 B → 记限制到 B 为止（Add(A, B)）。
//     即"永久限购"用哨兵表示"No"，我们要表达"一直限制"就送一个足够远的未来时间。
//
// 因此本文件负责：把**持久化计数已经买满**的商品算出来，用 860 通知下发给客户端。
// 下发时机两处：选角进城镇后（重上/重启后仍然消失）与购买回包**之前**（当前客户端的
// 商城列表是在收到 op64 回包时重建的，先发通知才能让刚买满的那一格立刻消失——与
// 装扮栏扩展券"先发 ITEM_LIST 再发购买回包"是同一条经验）。
package dnfbridge

import (
	"context"
	"encoding/binary"
	"sort"
	"strconv"
	"time"

	"longheng.io/server/internal/modules/dnf/purchaselimit"
)

// currentCeraShopBuyRestrictMsgID 是"限购商品列表"类 0 通知的 opcode（真机实测）。
const currentCeraShopBuyRestrictMsgID uint16 = 860

// currentCeraShopBuyRestrictUnlimitedSentinel 是处理器里的哨兵值：B 等于它就**解除**
// 限制（不是"永久限制"）。永久限制要用一个足够远的未来时间。
const currentCeraShopBuyRestrictUnlimitedSentinel uint32 = 0x7FFF53F0

// currentCeraShopBuyRestrictNotificationEnabled 允许一键关掉**下发**（保留服务端拦截）。
// 860 是本轮新接的包：如果真机上客户端表现异常，把这里改成 false 即可只保留"买不了"，
// 不再尝试让商品从列表里消失。
const currentCeraShopBuyRestrictNotificationEnabled = true

// currentCeraShopBuyRestrictPermanentEndTime 用来表达"永久限购"的到期时间：
// 2033-05-18，远大于任何人在线时间，同时刻意避开哨兵值本身。
const currentCeraShopBuyRestrictPermanentEndTime uint32 = 2_000_000_000

// currentCeraShopBuyRestrictEntry 是通知里的一行。
type currentCeraShopBuyRestrictEntry struct {
	CommodityID uint32
	EndTime     uint32
	Flag        uint32
}

// buildCurrentCeraShopBuyRestrictBody 编码 `[u32 条数]` + 每行 `[u32 商品号][u32 到期][u32 标志]`。
// 条数为 0 时返回 nil：调用方不发包（空包会把客户端刚建立的限制表清空）。
func buildCurrentCeraShopBuyRestrictBody(entries []currentCeraShopBuyRestrictEntry) []byte {
	if len(entries) == 0 {
		return nil
	}
	body := make([]byte, 0, 4+12*len(entries))
	body = binary.LittleEndian.AppendUint32(body, uint32(len(entries)))
	for _, entry := range entries {
		body = binary.LittleEndian.AppendUint32(body, entry.CommodityID)
		body = binary.LittleEndian.AppendUint32(body, entry.EndTime)
		body = binary.LittleEndian.AppendUint32(body, entry.Flag)
	}
	return body
}

// currentCeraShopSoldOutEntries 把"已经买满、必须从商城列表里消失"的商品算出来。
//
// 判定完全基于持久化计数（角色 Stats / 账号 Metadata），所以小退、大退、重启服务端
// 之后算出来的结果都一样。
//
// 每个买满的商品会送**两个**条目号：`[buy restrict item]` 的序号（第 1 列）与商品号。
// 根据客户端逆向，商品行状态是这样判定的（sub_16C57F0 构建商品行）：
//
//	① 用**商品号**查角色对象 +0x2484C 的 map → 取到该商品对应的"限购条目号" X
//	② X == -1 → 该行没有限购
//	③ 否则查 +0x2488C 的 map（本通知写入的就是这一张）里有没有 X → 有则这一行置为
//	   受限状态（[row+0x94]）
//
// 即：客户端的限购条目号是**表里的序号**（`[buy restrict item]` 第 1 列，1..111），
// 不是商品号。只发商品号时 ② 取到的 X 与集合里的键对不上，所以商品照旧显示——这正是
// 现场「大退以后 104830 仍在列表里」的原因。两种号一起发是安全的：序号只到 111，
// 与 6 位商品号不会撞键，命中的那一个生效。
func (s *Service) currentCeraShopSoldOutEntries(
	catalog *pvfCeraShopCatalog,
	characterStats map[string]int64,
	accountMetadata map[string]string,
	now time.Time,
) []currentCeraShopBuyRestrictEntry {
	if s == nil || catalog == nil {
		return nil
	}
	entries := make([]currentCeraShopBuyRestrictEntry, 0, 16)
	for _, commodityID := range sortedCeraShopCommodityIDs(catalog) {
		policies := catalog.BuyRestrictPolicies(commodityID)
		if len(policies) == 0 {
			continue
		}
		exhausted := false
		for _, policy := range policies {
			state := purchaselimit.Read(characterStats, accountMetadata, policy, now)
			if state.Limit > 0 && state.Used >= state.Limit {
				exhausted = true
				break
			}
		}
		if !exhausted {
			continue
		}
		// ① 表里的序号（客户端真正用来判定商品行状态的那个号）。
		if row, found := catalog.buyRestrict[commodityID]; found && row.Index != 0 {
			entries = append(entries, currentCeraShopBuyRestrictEntry{
				CommodityID: row.Index,
				EndTime:     currentCeraShopBuyRestrictPermanentEndTime,
				Flag:        0,
			})
		}
		// ② 商品号兜底（Marker=101 但不在表里的商品只有商品号可用）。
		entries = append(entries, currentCeraShopBuyRestrictEntry{
			CommodityID: commodityID,
			EndTime:     currentCeraShopBuyRestrictPermanentEndTime,
			Flag:        0,
		})
	}
	return entries
}

// sendCurrentCeraShopBuyRestrictProjection 把"已买满"的商城商品下发给客户端。
// 没有买满的商品时**不发包**（见 buildCurrentCeraShopBuyRestrictBody 的说明）。
//
// 这是**纯展示**的一步：拿不到商城目录、读不到角色记录都只记日志返回 nil，绝不把
// 回城/选角流程带崩（服务端拦截在购买事务里，与这里无关）。
func (s *Service) sendCurrentCeraShopBuyRestrictProjection(
	ctx context.Context,
	session *gameSession,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !currentCeraShopBuyRestrictNotificationEnabled {
		return nil
	}
	catalog, err := s.currentCeraShopCatalog()
	if err != nil || catalog == nil {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return nil
	}
	var accountMetadata map[string]string
	if repositories.Account != nil {
		accountID := s.accountIDForSession(session)
		if account, accountFound, accountErr := repositories.Account.Load(ctx, accountID); accountErr == nil && accountFound {
			accountMetadata = account.Metadata
		}
	}
	entries := s.currentCeraShopSoldOutEntries(catalog, character.Stats, accountMetadata, s.currentPurchaseLimitTime())
	body := buildCurrentCeraShopBuyRestrictBody(entries)
	if len(body) == 0 {
		return nil
	}
	if err := s.sendGameUpperRawClass(session, currentCeraShopBuyRestrictMsgID, body, 0); err != nil {
		return err
	}
	s.logPacketEvent("game-cera-shop-buy-restrict-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source", source,
		"entry_count", len(entries),
		"commodities", currentCeraShopBuyRestrictSummary(entries),
		"body_len", len(body),
		"sequence", "class0/op860-buy-restrict-item-list")
	return nil
}

// sortedCeraShopCommodityIDs 让"买满清单"的遍历顺序稳定可比（map 顺序随机会让
// 日志与通知体每次都变，不好核对）。
func sortedCeraShopCommodityIDs(catalog *pvfCeraShopCatalog) []uint32 {
	if catalog == nil {
		return nil
	}
	ids := make([]uint32, 0, len(catalog.products))
	for commodityID := range catalog.products {
		ids = append(ids, commodityID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func currentCeraShopBuyRestrictSummary(entries []currentCeraShopBuyRestrictEntry) string {
	if len(entries) == 0 {
		return "-"
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, int(entry.CommodityID))
	}
	sort.Ints(ids)
	out := ""
	for index, id := range ids {
		if index > 0 {
			out += ","
		}
		out += strconv.Itoa(id)
	}
	return out
}
