package dnfbridge

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestCurrentItemListProjectionSealByteForConvertedRelic 用现场那一行的**真实形状**跑投影。
//
// 现场 2026-10-03（char=10 / jm，item 101030779 圣物武器）：
//   - 品级箱调整后服务端下发的单行 op14 里 raw[0x0D]=1（客户端显示"封装"）；
//   - 但库里这一行 raw_entry[0x0D]=0、raw_entry_hex[0x0D]=00、seal_flag=0，**bind_flag=1**。
//
// 这条用例回答"当前代码对这条真实行到底发什么"，把 bind 兜底与显式 seal_flag=0 的优先级钉住。
func TestCurrentItemListProjectionSealByteForConvertedRelic(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	raw[0x00] = 17 // slot
	raw[0x02] = 0x7B
	raw[0x03] = 0x9B
	raw[0x04] = 0x05
	raw[0x05] = 0x06 // itemID=101030779
	raw[0x0D] = 0    // 落库的封装位：0
	extra := map[string]string{
		"item_kind":         "equipment",
		"pvf_path":          "equipment/character/swordman/weapon/hsword/101030779.equ",
		"equipment_type":    "[hsword]",
		"rarity":            "3",
		"grade":             "94",
		"minimum_level":     "90",
		"quality_seed":      "800834834",
		"amount_or_count":   "800834834",
		"durability":        "45",
		"max_durability":    "45",
		"seal_flag":         "0", // 圣物转换书写入的显式未封装
		"trade_restriction": "true",
		"source":            "equipment_emancipate_op271",
		"grade_adjust_type": "standard_kaleido",
		"raw_entry_hex":     "00000000000000000000000000000000000000",
	}
	stack := dnfrepo.ItemStack{ItemID: 101030779, Count: 1, Bind: true, RawEntry: raw, Extra: extra}

	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 17, stack)
	seal := entry.data[0x0D]
	if seal != 0 {
		t.Fatalf("bind=1 + 显式 seal_flag=0 的装备行，投影出的封装位=%d；"+
			"库里 raw/extra 两份都是 0，这里必须发 0（否则客户端把小退后就恢复的假封装画出来）", seal)
	}

	// 反例：显式未封装必须仍然能压过 bind —— 把 seal_flag 拿掉就该退回 bind 兜底（=1）。
	noSeal := make(map[string]string, len(extra))
	for key, value := range extra {
		noSeal[key] = value
	}
	delete(noSeal, "seal_flag")
	bindStack := stack
	bindStack.Extra = noSeal
	if got := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 17, bindStack).data[0x0D]; got != 1 {
		t.Fatalf("没有 seal_flag 时 bind 兜底应发 1，实际 %d", got)
	}
}
