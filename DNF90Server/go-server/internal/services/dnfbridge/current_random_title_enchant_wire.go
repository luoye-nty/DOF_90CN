package dnfbridge

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// 随机称号附魔（etc/randomtitleenchant.etc 的 config/row 对）在 0x77 物品行里的唯一
// 落点：configID 在 +0x61，rowID 在 +0x63。写入侧见
// internal/modules/dnf/inventory/owner.go 的 setRandomTitleEnchantFields，读取侧见下面的
// overlay，另外拍卖行行也把这两段抄进自己的 +0x41/+0x43（auction_runtime.go 的
// buildCurrentAuctionItem83）。偏移只用这一处常量，避免三处各写一遍字面量。
const (
	currentRandomTitleEnchantConfigOffset = 0x61
	currentRandomTitleEnchantRowOffset    = 0x63
	currentRandomTitleEnchantFieldEnd     = currentRandomTitleEnchantRowOffset + 2
)

// overlayCurrentRandomTitleEnchantEntry 把随机称号附魔盖到一条**已经投影好**的 0x77 行上。
//
// 它必须作用在 entry 而不是 raw 上：物品行有两种来源，只有一种是「抄一条存好的整行」
// （currentItemListEntryFromRaw 先看 stack.RawEntry / raw_entry_hex，都没有时逐字段从
// Extra 现造一条全零底座）。旧写法只盖 raw，于是**凡是服务端自己造出来的行都拿不到
// +0x61/+0x63**——2026-09-27 的 [upgrade limit cube] 称号变换正好写的就是这种行
// （owner 换模板时刻意清掉 RawEntry，见 limitedcube/owner.go），附魔会当场消失，和
// 2026-09-25「称号附魔进图就消失」是同一类丢字段。
//
// 覆盖语义与旧写法逐字节一致：把普通宝珠附魔那一段（+0x0E 的卡 id 与 +0x12 的升级数）
// 清零（setRandomTitleEnchantFields 落库时也是这么写的），再写 config/row。Extra 里没有
// 完整 config/row 对时**不动任何字节**——半条附魔是坏行，不能被当成附魔。
func overlayCurrentRandomTitleEnchantEntry(entry *currentItemListEntry, extra map[string]string) {
	if entry == nil || len(extra) == 0 {
		return
	}
	configID, configOK := currentRandomTitleEnchantExtraUint16(extra, "random_title_enchant_config_id")
	rowID, rowOK := currentRandomTitleEnchantExtraUint16(extra, "random_title_enchant_row_id")
	if !configOK || !rowOK || configID == 0 || rowID == 0 {
		return
	}
	for index := 0; index < 4; index++ {
		entry.data[0x0e+index] = 0
	}
	entry.data[0x12] = 0
	binary.LittleEndian.PutUint16(entry.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset], configID)
	binary.LittleEndian.PutUint16(entry.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd], rowID)
}

func currentRandomTitleEnchantExtraUint16(extra map[string]string, key string) (uint16, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(extra[key]), 0, 16)
	return uint16(value), err == nil
}

// currentItemListEntryCarriesRandomTitleEnchant 报告一条已经投影好的 0x77 物品行是否
// 带着随机称号附魔。判定要求 config 与 row 都非零：只写一半的行是坏行，不能当成附魔。
//
// 这个判定存在的理由见 userinfo_current_equipment_rules.go 的
// currentMode1EquipmentRowNeedsDeferredStateRow：mode1 的 create 行不携带 +0x61..+0x65，
// 所以带着附魔的行必须额外再走一遍完整 0x77 行。
func currentItemListEntryCarriesRandomTitleEnchant(entry currentItemListEntry) bool {
	configID := binary.LittleEndian.Uint16(
		entry.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset],
	)
	rowID := binary.LittleEndian.Uint16(
		entry.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd],
	)
	return configID != 0 && rowID != 0
}
