package dnfbridge

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// currentBoosterCompactLiveBody 是 2026-09-21 20:39（角色 1111）点开
// 「2018夏日礼包重购惊喜礼盒 4」(item 490702367, `[booster selection num] 2`) 时客户端
// 真实发出的 op160 包体：packet_log.txt 里 `game-legacy type=160 body_len=14` 两条
// 逐字节相同，13 字节包头之后就是这 14 字节。
//
// 72 00            槽位 114
// 00 00            类别 0（PVF `[booster select category] 0 0`）
// f7 85 3f 1d      490702327（类别 0 的 `[etc] 490702327 1`）
// 64 86 3f 1d      490702436（类别 0 的 `[stackable] 490702436 1 ...`）
// 00 00            选项 0 + 尾字节 0
const currentBoosterCompactLiveBody = "72000000f7853f1d64863f1d0000"

func TestParseCurrentBoosterOpenRequestUsesExactLiveCompactSelectionBody(t *testing.T) {
	body, err := hex.DecodeString(currentBoosterCompactLiveBody)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 14 {
		t.Fatalf("live body len=%d want=14", len(body))
	}
	request, err := parseCurrentBoosterOpenRequest(body)
	if err != nil {
		t.Fatalf("parse live compact selection body: %v", err)
	}
	if request.Kind != currentBoosterRequestSelection {
		t.Fatalf("kind=%v want=selection", request.Kind)
	}
	if request.SourceSlot != 114 {
		t.Fatalf("source_slot=%d want=114", request.SourceSlot)
	}
	if request.SelectionContext != 0 {
		t.Fatalf("selection_context=%d want=0", request.SelectionContext)
	}
	want := []uint32{490702327, 490702436}
	if len(request.Selections) != len(want) {
		t.Fatalf("selections=%+v want 2 items %v", request.Selections, want)
	}
	for index, itemID := range want {
		got := request.Selections[index]
		if got.ItemID != itemID || got.Option != 0 {
			t.Fatalf("selections[%d]=%+v want item=%d option=0", index, got, itemID)
		}
	}
	if request.SelectedItemID != want[0] || request.SelectionOption != 0 || request.TrailingUIByte != 0 {
		t.Fatalf("selected=%d option=%d trailing=%d want %d/0/0",
			request.SelectedItemID, request.SelectionOption, request.TrailingUIByte, want[0])
	}
}

// TestParseCurrentBoosterOpenRequestStillAcceptsKnownShapes 是"修 A 不许碰坏 B"的守门测试：
// 新增的紧凑体长度判据不能抢走已经在线跑通的三种写法。
func TestParseCurrentBoosterOpenRequestStillAcceptsKnownShapes(t *testing.T) {
	// 1) 2 字节随机盒（booster_item_test.go 里的真实包体）。
	random, err := parseCurrentBoosterOpenRequest([]byte{0x51, 0x00})
	if err != nil || random.Kind != currentBoosterRequestRandom || random.SourceSlot != 81 {
		t.Fatalf("random=%+v err=%v", random, err)
	}

	// 2) 10 字节单选（真实包体；长度与紧凑体 N=1 等价，必须仍走同一条语义）。
	singleBody := []byte{0x4e, 0x00, 0, 0, 0x43, 0x24, 0x0e, 0x06, 0, 0}
	single, err := parseCurrentBoosterOpenRequest(singleBody)
	if err != nil {
		t.Fatalf("parse 10-byte selection body: %v", err)
	}
	if single.Kind != currentBoosterRequestSelection || single.SourceSlot != 78 ||
		len(single.Selections) != 1 || single.Selections[0].ItemID != 101590083 ||
		single.SelectedItemID != 101590083 || single.SelectionOption != 0 || single.TrailingUIByte != 0 {
		t.Fatalf("single=%+v", single)
	}

	// 3) 6+9n 多选：长度 42（n=4）同时满足紧凑体判据，必须仍按多选解析。
	multi := buildCurrentBoosterMultiBodyForTest(t, 93, 1, []uint32{1001, 1002, 1003, 1004}, []byte{0, 1, 2, 3})
	if len(multi) != currentBoosterMultiBodyBaseSize+4*currentBoosterMultiEntrySize {
		t.Fatalf("multi len=%d want=%d", len(multi), currentBoosterMultiBodyBaseSize+4*currentBoosterMultiEntrySize)
	}
	if _, ok := currentBoosterCompactSelectionCount(len(multi)); !ok {
		t.Fatalf("test fixture len=%d must also satisfy the compact predicate", len(multi))
	}
	parsedMulti, err := parseCurrentBoosterOpenRequest(multi)
	if err != nil {
		t.Fatalf("parse 42-byte multi selection body: %v", err)
	}
	if parsedMulti.Kind != currentBoosterRequestSelection || parsedMulti.SourceSlot != 93 ||
		parsedMulti.SelectionContext != 1 || len(parsedMulti.Selections) != 4 {
		t.Fatalf("multi=%+v", parsedMulti)
	}
	for index, itemID := range []uint32{1001, 1002, 1003, 1004} {
		if parsedMulti.Selections[index].ItemID != itemID || parsedMulti.Selections[index].Option != byte(index) {
			t.Fatalf("multi selections[%d]=%+v", index, parsedMulti.Selections[index])
		}
	}
}

// TestParseCurrentBoosterOpenRequestRejectsUnknownShapes 保证新判据没有把"以前会被拒"的长度
// 悄悄放行，长度对但内容坏（零道具号、重复道具号）也仍然拒。
func TestParseCurrentBoosterOpenRequestRejectsUnknownShapes(t *testing.T) {
	for _, size := range []int{0, 1, 3, 4, 6, 8, 9, 11, 12, 13, 16, 17, 19, 20, 21, 23, 25} {
		if _, err := parseCurrentBoosterOpenRequest(make([]byte, size)); err == nil {
			t.Fatalf("body_len=%d accepted, want malformed", size)
		}
	}
	// 14 字节紧凑体的坏内容：第二件道具号为 0。
	zeroSecond := []byte{0x72, 0x00, 0x00, 0x00, 0xf7, 0x85, 0x3f, 0x1d, 0, 0, 0, 0, 0, 0}
	if _, err := parseCurrentBoosterOpenRequest(zeroSecond); err == nil {
		t.Fatal("compact body with a zero item id accepted")
	}
	// 14 字节紧凑体的坏内容：两件是同一个道具号。
	duplicate := []byte{0x72, 0x00, 0x00, 0x00, 0xf7, 0x85, 0x3f, 0x1d, 0xf7, 0x85, 0x3f, 0x1d, 0, 0}
	if _, err := parseCurrentBoosterOpenRequest(duplicate); err == nil {
		t.Fatal("compact body with duplicate item ids accepted")
	}
}

// TestRealPVFSummerRepurchaseBoxCompactSelectionResolvesAdvertisedRewards 把上面那条真实包体
// 喂进"解析 → PVF 候选校验 → 奖励结算"整条链，断言开出来的正好是礼盒说明里写的那两件
// （1 个热舞一夏自选礼盒 + 1 个 2018热舞一夏劲享礼包 1）。
//
// 默认跳过；设置 DNFBRIDGE_REAL_PVF_SMOKE 指向 Script.pvf，或直接用仓库 runtime 副本时运行：
//
//	$env:DNFBRIDGE_REAL_PVF_SMOKE='D:\test\地下城与勇士\Script.pvf'
func TestRealPVFSummerRepurchaseBoxCompactSelectionResolvesAdvertisedRewards(t *testing.T) {
	pvfPath := currentBoosterRealPVFPathForTest()
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the summer repurchase box compact selection")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatalf("load real item catalog: %v", err)
	}
	body, err := hex.DecodeString(currentBoosterCompactLiveBody)
	if err != nil {
		t.Fatal(err)
	}
	request, err := parseCurrentBoosterOpenRequest(body)
	if err != nil {
		t.Fatalf("parse live compact selection body: %v", err)
	}
	// 真实 PVF：490702367 = 2018夏日礼包重购惊喜礼盒 4，[booster selection num] 2。
	definition, err := resolveCurrentBoosterDefinition(catalog, 490702367, request.Kind)
	if err != nil {
		t.Fatalf("resolve 490702367 as selection box: %v", err)
	}
	if definition.SelectionRequired != 2 {
		t.Fatalf("selection_required=%d want=2", definition.SelectionRequired)
	}
	candidates, err := currentBoosterSelectedCandidates(definition, request)
	if err != nil {
		t.Fatalf("validate live selection against real PVF: %v", err)
	}
	wantCounts := map[uint32]uint32{490702327: 1, 490702436: 1}
	if len(candidates) != len(wantCounts) {
		t.Fatalf("candidates=%+v want %v", candidates, wantCounts)
	}
	for _, candidate := range candidates {
		if wantCounts[candidate.ItemID] != candidate.Count {
			t.Fatalf("candidate=%+v want counts %v", candidate, wantCounts)
		}
	}
	amounts, _, gold, err := resolveCurrentBoosterRewardAmounts(definition, request, nil)
	if err != nil {
		t.Fatalf("resolve rewards: %v", err)
	}
	if gold != 0 {
		t.Fatalf("gold=%d want=0", gold)
	}
	if len(amounts) != len(wantCounts) {
		t.Fatalf("amounts=%v want %v", amounts, wantCounts)
	}
	for itemID, count := range wantCounts {
		if amounts[itemID] != count {
			t.Fatalf("amounts=%v want %v", amounts, wantCounts)
		}
	}
	// 两件奖励都是"非材料类堆叠物"，落点固定为消耗品页 65..120：开这个礼盒要求消耗品页
	// 至少有 2 个空格。现场 1111 那页当时是 56/56（其中一条还是客户端画不出来的幽灵行），
	// 所以解析修好之后，这一页不清出格子依旧会走到"真的放不下"那条分支。
	for _, itemID := range []uint32{490702327, 490702436} {
		rewardDefinition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve reward item=%d: %v", itemID, resolveErr)
		}
		start, end := dungeonDropStackableSlotRange(rewardDefinition.StackableType)
		if start != 65 || end != 120 {
			t.Fatalf("reward item=%d type=%q placement=%d..%d want 65..120",
				itemID, rewardDefinition.StackableType, start, end)
		}
	}

	// 反例 1：件数不对（只选一件）必须被 [booster selection num] 拦住。
	shortRequest := request
	shortRequest.Selections = request.Selections[:1]
	if _, err := currentBoosterSelectedCandidates(definition, shortRequest); err == nil {
		t.Fatal("one-selection request accepted for a selection_num=2 box")
	}
	// 反例 2：把第二件换成**别的类别**的道具（490702472 是类别 1 的候选）必须被拒。
	foreign := request
	foreign.Selections = []currentBoosterSelectionRequest{
		{ItemID: 490702327, Option: 0},
		{ItemID: 490702472, Option: 0},
	}
	if _, err := currentBoosterSelectedCandidates(definition, foreign); err == nil {
		t.Fatal("cross-category selection accepted")
	}
}

// currentBoosterRealPVFPathForTest 沿用本仓库既有的两条约定：优先 DNFBRIDGE_REAL_PVF_SMOKE
// （指向线上 Script.pvf），否则退到工作区 runtime 副本；两者都没有就返回空串让调用方跳过。
func currentBoosterRealPVFPathForTest() string {
	if path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE"); path != "" {
		return path
	}
	fallback := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	if _, err := os.Stat(fallback); err != nil {
		return ""
	}
	return fallback
}

// buildCurrentBoosterMultiBodyForTest 拼一条 6+9n 形态的多选包体：
// u16 槽位 + u16 类别 + N×u32 前缀道具号 + u8 N + N×(u32 道具号 + u8 选项) + u8 尾字节。
func buildCurrentBoosterMultiBodyForTest(
	t *testing.T,
	slot uint16,
	context uint16,
	itemIDs []uint32,
	options []byte,
) []byte {
	t.Helper()
	if len(itemIDs) != len(options) {
		t.Fatalf("fixture itemIDs=%d options=%d", len(itemIDs), len(options))
	}
	body := make([]byte, 0, currentBoosterMultiBodyBaseSize+len(itemIDs)*currentBoosterMultiEntrySize)
	body = binary.LittleEndian.AppendUint16(body, slot)
	body = binary.LittleEndian.AppendUint16(body, context)
	for _, itemID := range itemIDs {
		body = binary.LittleEndian.AppendUint32(body, itemID)
	}
	body = append(body, byte(len(itemIDs)))
	for index, itemID := range itemIDs {
		body = binary.LittleEndian.AppendUint32(body, itemID)
		body = append(body, options[index])
	}
	return append(body, 0)
}
