package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 【op416 副职业提取器批量提取 2026-09-25】回归闸门。
//
// 这一组用例回答三个问题：
//  1. 6 字节单件请求永远不被当成批量信封（老客户端 / 单件提取行为不变）；
//  2. 批量信封的字段与校验边界（哨兵、长度、重复槽、提取器槽混入、职业类型、上限）；
//  3. 端到端：一个信封在真机 Script.pvf 上跑完，只回**一个** ACK、只重建**一次**容器，
//     而且与"逐件发 N 次单件请求"得到相同的仓库状态与材料总量。

// ---------------------------------------------------------------------------
// 1) 解析层
// ---------------------------------------------------------------------------

// 单件 6 字节请求必须原样落到原生路径。这里连"targetSlot 恰好是 0xFFFF"这种
// 最容易与哨兵混淆的形状也钉住：长度不足 11 就永远不是信封。
func TestParseCurrentExpertJobExtractionBatchKeepsSingleRequestsNative(t *testing.T) {
	single := []byte{2, 9, 0, 0, 10, 0}
	batch, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(single)
	if err != nil || isBatch || batch.Slots != nil {
		t.Fatalf("six-byte single request misread as batch: batch=%+v is_batch=%t err=%v", batch, isBatch, err)
	}
	parsed, err := parseCurrentExpertJobExtractionRequest(single)
	if err != nil || parsed.ExtractorType != 2 || parsed.ExtractorSlot != 9 || parsed.TargetList != 0 || parsed.TargetSlot != 10 {
		t.Fatalf("single request no longer parses: %+v err=%v", parsed, err)
	}
	// 单件请求的 targetSlot 位置写 -1：单件解析器本来就拒绝（TargetSlot<0），
	// 而批量解析器因为长度不足同样不认——两边都不会误判。
	negative := []byte{2, 9, 0, 0, 0xFF, 0xFF}
	if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(negative); err != nil || isBatch {
		t.Fatalf("negative target slot misread as batch: is_batch=%t err=%v", isBatch, err)
	}
	if _, err := parseCurrentExpertJobExtractionRequest(negative); err == nil {
		t.Fatal("negative single target slot accepted")
	}
}

func TestParseCurrentExpertJobExtractionBatchAcceptsHookEnvelope(t *testing.T) {
	body := currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 66, 67})
	batch, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(body)
	if err != nil || !isBatch {
		t.Fatalf("envelope rejected: batch=%+v is_batch=%t err=%v", batch, isBatch, err)
	}
	if batch.ExtractorType != 2 || batch.ExtractorSlot != 240 || batch.TargetList != 0 ||
		len(batch.Slots) != 3 || batch.Slots[0] != 65 || batch.Slots[1] != 66 || batch.Slots[2] != 67 {
		t.Fatalf("envelope fields=%+v", batch)
	}
	if len(body) != currentExpertJobExtractionBatchHeaderSize+6 {
		t.Fatalf("envelope length=%d", len(body))
	}
	// 三个职业（附魔师 1 / 炼金师 2 / 控偶师 4）必须被同一个解析器接受：
	// 它们共用 op416 与同一条客户端循环，差别只有这一个字节。
	for _, extractorType := range []byte{1, 2, 4} {
		if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(
			currentExpertJobExtractionBatchTestBody(extractorType, 240, 0, []int16{65, 66})); err != nil || !isBatch {
			t.Fatalf("extractor type %d rejected: is_batch=%t err=%v", extractorType, isBatch, err)
		}
	}
}

func TestParseCurrentExpertJobExtractionBatchRejectsBrokenEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{name: "count_zero", body: currentExpertJobExtractionBatchTestBody(2, 240, 0, nil)},
		{name: "unsupported_profession_type", body: currentExpertJobExtractionBatchTestBody(3, 240, 0, []int16{65, 66})},
		{name: "target_list_not_main", body: currentExpertJobExtractionBatchTestBody(2, 240, 2, []int16{65, 66})},
		{name: "extractor_slot_inside_selection", body: currentExpertJobExtractionBatchTestBody(2, 65, 0, []int16{65, 66})},
		{name: "duplicate_slot", body: currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 65})},
		{name: "negative_slot", body: currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{-1, 66})},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(test.body); err == nil || !isBatch {
				t.Fatalf("is_batch=%t err=%v body=%x", isBatch, err, test.body)
			}
		})
	}

	// 长度与 count 不匹配、count 超过上限：仍然是"批量但坏了"（is_batch=true），
	// 调用方据此回失败码，而不是掉进单件路径去解析一个半截信封。
	truncated := currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 66})
	if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(truncated[:len(truncated)-2]); err == nil || !isBatch {
		t.Fatalf("truncated envelope is_batch=%t err=%v", isBatch, err)
	}
	over := currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 66})
	over[10] = currentExpertJobExtractionBatchMaxItems + 1
	if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(over); err == nil || !isBatch {
		t.Fatalf("over-limit envelope is_batch=%t err=%v", isBatch, err)
	}
	// 标记写错 = 不是信封（避免把随机 6 字节以上的旧形状误判成批量）。
	wrongMarker := currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 66})
	wrongMarker[7] = 'Z'
	if _, isBatch, err := parseCurrentExpertJobExtractionBatchRequest(wrongMarker); err != nil || isBatch {
		t.Fatalf("wrong marker is_batch=%t err=%v", isBatch, err)
	}
}

// ---------------------------------------------------------------------------
// 2) 纯函数：行合并 + 合并回包
// ---------------------------------------------------------------------------

// 同一格的多行必须合成一行：客户端是按行把回包应用到容器行上的，同一槽出现多行时
// 它的语义取决于底层是置值还是累加（未取证）。op26 批量的合并口径与此一致。
func TestCurrentExpertJobMergeExtractionBatchRowsCollapsesSameSlotRows(t *testing.T) {
	merged := currentExpertJobMergeExtractionBatchRows([]currentExpertJobExtractionMaterial{
		{Slot: 65, ItemID: 2610024, Count: 1},
		{Slot: 65, ItemID: 2610024, Count: 2},
		{Slot: 66, ItemID: 2610024, Count: 1},
		{Slot: 358, ItemID: 3037, Count: 5},
		{Slot: 65, ItemID: 3037, Count: 4},
	})
	want := []currentExpertJobExtractionMaterial{
		{Slot: 65, ItemID: 2610024, Count: 3},
		{Slot: 66, ItemID: 2610024, Count: 1},
		{Slot: 358, ItemID: 3037, Count: 5},
		{Slot: 65, ItemID: 3037, Count: 4},
	}
	if len(merged) != len(want) {
		t.Fatalf("merged=%+v", merged)
	}
	for index := range want {
		if merged[index] != want[index] {
			t.Fatalf("merged[%d]=%+v want %+v (all=%+v)", index, merged[index], want[index], merged)
		}
	}
	if got := currentExpertJobMergeExtractionBatchRows(nil); len(got) != 0 {
		t.Fatalf("nil rows merged into %+v", got)
	}
	if rows := []currentExpertJobExtractionMaterial{{Slot: 65, ItemID: 2610024, Count: 1}}; len(currentExpertJobMergeExtractionBatchRows(rows)) != 1 {
		t.Fatal("single row changed by merge")
	}
}

func TestBuildCurrentExpertJobExtractionSuccessBodyCarriesMergedRows(t *testing.T) {
	body := buildCurrentExpertJobExtractionSuccessBody(currentExpertJobExtractionResult{
		JobType:    2,
		TargetList: 0,
		TargetSlot: 65,
		Materials: []currentExpertJobExtractionMaterial{
			{Slot: 65, ItemID: 2610024, Count: 2},
			{Slot: 358, ItemID: 3037, Count: 7},
		},
	})
	// u8 flag=1 | u8 targetList | u16 targetSlot | u8 rowCount | rows{u16 slot,u32 itemID,u32 count}
	want := []byte{1, 0, 65, 0, 2}
	want = binary.LittleEndian.AppendUint16(want, 65)
	want = binary.LittleEndian.AppendUint32(want, 2610024)
	want = binary.LittleEndian.AppendUint32(want, 2)
	want = binary.LittleEndian.AppendUint16(want, 358)
	want = binary.LittleEndian.AppendUint32(want, 3037)
	want = binary.LittleEndian.AppendUint32(want, 7)
	if len(body) != len(want) {
		t.Fatalf("body=%x want=%x", body, want)
	}
	for index := range want {
		if body[index] != want[index] {
			t.Fatalf("body[%d]=%02x want %02x (body=%x)", index, body[index], want[index], body)
		}
	}
}

// ---------------------------------------------------------------------------
// 3) 端到端（真机 Script.pvf）
// ---------------------------------------------------------------------------

// 一个批量信封必须只回一个 ACK、只重建一次容器，并且与"逐件发 N 次单件请求"
// 得到相同的仓库状态与材料总量。
//
// 为了让"相同"是可判定的：测试在真机数据里挑一件**产物与随机数无关**的装备
// （炼金提取器 + 稀有度 0/1 装备：产物恒为基础材料行），只有经验增益在
// [min,max] 之间随机，因此对经验只断言区间，对产物与槽位断言精确相等。
func TestRealScriptPVFExtractionBatchSendsOneAckAndMatchesPerItemRequests(t *testing.T) {
	items, expert, archive := currentExtractionBatchRealPVFAssets(t)
	extractorID, equipmentID := currentExtractionBatchRealPVFProbe(t, expert, archive)

	batchFixture := newCurrentExtractionBatchFixture(t, items, expert, extractorID, equipmentID)
	batchPackets := batchFixture.run(t, currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 66}))
	batchSnapshot := batchFixture.snapshot(t)

	singleFixture := newCurrentExtractionBatchFixture(t, items, expert, extractorID, equipmentID)
	singlePackets := singleFixture.run(t, currentExpertJobExtractionTestBody(2, 240, 0, 65))
	singlePackets = append(singlePackets, singleFixture.run(t, currentExpertJobExtractionTestBody(2, 240, 0, 66))...)
	singleSnapshot := singleFixture.snapshot(t)

	// ---- 包形状：批量 = 1 个 ACK + 1 次容器重建；逐件 = 2 个 ACK + 2 次重建 ----
	if got := countCurrentExtractionBatchAcks(batchPackets); got != 1 {
		t.Fatalf("batch ACKs=%d want 1 packets=%s", got, describeCurrentExtractionBatchPackets(batchPackets))
	}
	if got := countCurrentExtractionBatchAcks(singlePackets); got != 2 {
		t.Fatalf("single ACKs=%d want 2 packets=%s", got, describeCurrentExtractionBatchPackets(singlePackets))
	}
	batchRefreshes := countCurrentExtractionBatchRefreshes(batchPackets)
	if batchRefreshes == 0 {
		t.Fatalf("batch sent no container rebuild: %s", describeCurrentExtractionBatchPackets(batchPackets))
	}
	if got := countCurrentExtractionBatchRefreshes(singlePackets); got != 2*batchRefreshes {
		t.Fatalf("refreshes batch=%d single=%d want %d", batchRefreshes, got, 2*batchRefreshes)
	}
	if len(batchPackets) >= len(singlePackets) {
		t.Fatalf("batch packets=%d single packets=%d (batch must be strictly cheaper)",
			len(batchPackets), len(singlePackets))
	}

	// ---- ACK 本体：只命名一个 targetSlot（第一个被消耗的槽）----
	batchAck := firstCurrentExtractionBatchAck(t, batchPackets)
	if batchAck.body[0] != 1 || batchAck.body[1] != 0 || binary.LittleEndian.Uint16(batchAck.body[2:4]) != 65 {
		t.Fatalf("batch ACK head=%x", batchAck.body)
	}
	rowCount := int(batchAck.body[4])
	if rowCount < 1 || len(batchAck.body) != 5+rowCount*10 {
		t.Fatalf("batch ACK rows=%d body=%x", rowCount, batchAck.body)
	}
	ackMaterialTotal := int64(0)
	for index := 0; index < rowCount; index++ {
		offset := 5 + index*10
		if itemID := binary.LittleEndian.Uint32(batchAck.body[offset+2 : offset+6]); itemID != batchSnapshot.materialItemID {
			t.Fatalf("batch ACK row %d item=%d want %d body=%x", index, itemID, batchSnapshot.materialItemID, batchAck.body)
		}
		ackMaterialTotal += int64(binary.LittleEndian.Uint32(batchAck.body[offset+6 : offset+10]))
	}

	// ---- 仓库状态：两件都被消耗、提取器留着、材料总量一致且与 ACK 对得上 ----
	for name, snapshot := range map[string]currentExtractionBatchSnapshot{"batch": batchSnapshot, "single": singleSnapshot} {
		if !snapshot.extractorPresent {
			t.Fatalf("%s: extractor was consumed: %+v", name, snapshot)
		}
		if snapshot.source65Present || snapshot.source66Present {
			t.Fatalf("%s: consumed source survived: %+v", name, snapshot)
		}
		if snapshot.materialTotal != 2 {
			t.Fatalf("%s: material total=%d want 2 snapshot=%+v", name, snapshot.materialTotal, snapshot)
		}
		if snapshot.materialRowCount != 1 {
			t.Fatalf("%s: material rows=%d want 1 (same-slot rows must merge) snapshot=%+v", name, snapshot.materialRowCount, snapshot)
		}
		if snapshot.experienceGain < 0 || snapshot.experienceGain > 2 {
			t.Fatalf("%s: experience gain=%d want 0..2", name, snapshot.experienceGain)
		}
	}
	if ackMaterialTotal != batchSnapshot.materialTotal {
		t.Fatalf("ACK material total=%d repository=%d", ackMaterialTotal, batchSnapshot.materialTotal)
	}
	if batchSnapshot.materialItemID != singleSnapshot.materialItemID ||
		batchSnapshot.materialTotal != singleSnapshot.materialTotal ||
		batchSnapshot.materialRowCount != singleSnapshot.materialRowCount {
		t.Fatalf("batch snapshot=%+v single snapshot=%+v", batchSnapshot, singleSnapshot)
	}
}

// 整批原子：选中集合里混进一个已经空掉的槽时，整批拒绝，已选中的那件也不能被消耗。
func TestRealScriptPVFExtractionBatchRollsBackWhenOneTargetIsStale(t *testing.T) {
	items, expert, archive := currentExtractionBatchRealPVFAssets(t)
	extractorID, equipmentID := currentExtractionBatchRealPVFProbe(t, expert, archive)
	fixture := newCurrentExtractionBatchFixture(t, items, expert, extractorID, equipmentID)

	packets := fixture.run(t, currentExpertJobExtractionBatchTestBody(2, 240, 0, []int16{65, 99}))
	if len(packets) != 1 {
		t.Fatalf("rejected batch packets=%s", describeCurrentExtractionBatchPackets(packets))
	}
	packet := packets[0]
	if packet.msgID != uint16(dnfenum.CmdPacketExpertExtraction) || len(packet.body) != 2 ||
		packet.body[0] != 0 || packet.body[1] != 13 {
		t.Fatalf("failure ACK class=%d msg=%d body=%x", packet.classification, packet.msgID, packet.body)
	}
	snapshot := fixture.snapshot(t)
	if !snapshot.source65Present {
		t.Fatalf("rejected batch consumed the valid source: %+v", snapshot)
	}
	if snapshot.materialTotal != 0 || snapshot.experienceGain != 0 {
		t.Fatalf("rejected batch mutated state: %+v", snapshot)
	}
}

// ---------------------------------------------------------------------------
// 端到端夹具
// ---------------------------------------------------------------------------

type currentExtractionBatchSnapshot struct {
	extractorPresent bool
	source65Present  bool
	source66Present  bool
	materialTotal    int64
	materialRowCount int
	materialItemID   uint32
	experienceGain   int64
}

type currentExtractionBatchFixture struct {
	service      *Service
	repositories dnfrepo.Group
	session      *gameSession
	connection   *bufferConn
	baseExp      int64
	excluded     map[int64]bool
}

type currentExtractionBatchPacket struct {
	classification byte
	msgID          uint16
	body           []byte
}

func currentExtractionBatchRealPVFAssets(t *testing.T) (*pvfDungeonDropCatalog, *dnfexpertjob.Catalog, *platformpvf.Archive) {
	t.Helper()
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	items, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	expert, err := dnfexpertjob.Load(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	return items, expert, archive
}

// currentExtractionBatchRealPVFProbe 在真机数据里挑"1 级炼金师就能用"的提取器，
// 以及一件产物与随机数无关的可分解装备（稀有度 0/1、产物形态恒定）。
func currentExtractionBatchRealPVFProbe(t *testing.T, expert *dnfexpertjob.Catalog, archive *platformpvf.Archive) (int64, int64) {
	t.Helper()
	config, ok := expert.Config(dnfexpertjob.AlchemistType)
	if !ok {
		t.Fatal("active PVF has no alchemist config")
	}
	level := config.Level(0)
	extractorID := int64(0)
	for id, extractor := range config.Extractors {
		if extractor.RequiredLevel > level {
			continue
		}
		if extractorID == 0 || id < extractorID {
			extractorID = id
		}
	}
	if extractorID == 0 {
		t.Fatalf("active PVF has no alchemist extractor usable at level %d", level)
	}
	text, err := archive.ReadText("equipment/equipment.lst")
	if err != nil {
		t.Fatalf("read equipment list: %v", err)
	}
	inspected := 0
	for _, field := range strings.Fields(text) {
		id, convErr := strconv.ParseInt(field, 10, 64)
		if convErr != nil || id <= 0 || inspected >= 8000 {
			continue
		}
		metadata, metadataErr := expert.Equipment(id)
		if metadataErr != nil || metadata.Rarity > 1 || metadata.DisjointForbidden ||
			metadata.AttachType == "trade delete" || metadata.SellGold <= 0 {
			continue
		}
		inspected++
		if !currentExtractionBatchPlanIsStable(expert, extractorID, metadata) {
			continue
		}
		return extractorID, id
	}
	t.Skipf("no rng-stable extraction target found (extractor=%d inspected=%d)", extractorID, inspected)
	return 0, 0
}

// 用同一件装备反复求解，产物形态必须恒定；只有经验增益允许在 [min,max] 之间抖动。
func currentExtractionBatchPlanIsStable(expert *dnfexpertjob.Catalog, extractorID int64, metadata dnfexpertjob.EquipmentMetadata) bool {
	first := ""
	for index := 0; index < 33; index++ {
		probe := index
		rng := func(n int) int {
			if n <= 1 {
				return 0
			}
			return probe % n
		}
		plan, err := expert.PlanExtraction(dnfexpertjob.AlchemistType, 0, extractorID, metadata, dnfexpertjob.RandomFunc(rng))
		if err != nil {
			return false
		}
		desc := fmt.Sprintf("%v", plan.Materials)
		if first == "" {
			first = desc
			continue
		}
		if desc != first {
			return false
		}
	}
	return first != ""
}

func newCurrentExtractionBatchFixture(t *testing.T, items *pvfDungeonDropCatalog, expert *dnfexpertjob.Catalog, extractorID, equipmentID int64) *currentExtractionBatchFixture {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	const baseExp = int64(0)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats: map[string]int64{
			"expert_job_type": int64(dnfexpertjob.AlchemistType),
			"expert_job_exp":  baseExp,
		},
	}); err != nil {
		t.Fatal(err)
	}
	slots := map[string]dnfrepo.ItemStack{
		currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 240): {ItemID: extractorID, Count: 1},
		currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 65):  {ItemID: equipmentID, Count: 1},
		currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 66):  {ItemID: equipmentID, Count: 1},
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		pvfItemCatalog:   items,
		expertJobCatalog: expert,
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	return &currentExtractionBatchFixture{
		service:      service,
		repositories: repositories,
		session:      &gameSession{conn: connection, selectedCharacterID: 19, accountID: "account-1"},
		connection:   connection,
		baseExp:      baseExp,
		excluded:     map[int64]bool{extractorID: true, equipmentID: true},
	}
}

// run 走一次请求并把这一趟发出的所有包解析出来（缓冲区随之清空）。
func (f *currentExtractionBatchFixture) run(t *testing.T, body []byte) []currentExtractionBatchPacket {
	t.Helper()
	if err := f.service.handleCurrentExpertJobExtraction(f.session, body); err != nil {
		t.Fatal(err)
	}
	packets := currentExtractionBatchSplitAll(t, f.connection.write.Bytes())
	f.connection.write.Reset()
	return packets
}

func (f *currentExtractionBatchFixture) snapshot(t *testing.T) currentExtractionBatchSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := currentExtractionBatchSnapshot{}
	inventory, found, err := f.repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	_, snapshot.source65Present = inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 65)]
	_, snapshot.source66Present = inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 66)]
	_, snapshot.extractorPresent = inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 240)]
	account, _, err := f.repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	// 产物 = 除提取器与两件装备以外的所有行；材料 id 取唯一那个。
	totals := map[uint32]int64{}
	rows := map[uint32]int{}
	for _, source := range []map[string]dnfrepo.ItemStack{inventory.Slots, account.Slots} {
		for _, stack := range source {
			if stack.ItemID == 0 || stack.Count <= 0 || f.excluded[stack.ItemID] {
				continue
			}
			totals[uint32(stack.ItemID)] += stack.Count
			rows[uint32(stack.ItemID)]++
		}
	}
	for itemID, total := range totals {
		snapshot.materialItemID = itemID
		snapshot.materialTotal += total
		snapshot.materialRowCount += rows[itemID]
	}
	character, found, err := f.repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("character found=%t err=%v", found, err)
	}
	snapshot.experienceGain = character.Stats["expert_job_exp"] - f.baseExp
	return snapshot
}

// ---------------------------------------------------------------------------
// 信封与包解析小工具
// ---------------------------------------------------------------------------

// 原生 6 字节单件请求：u8 职业类型 | i16 提取器槽 | u8 目标容器 | i16 目标槽。
func currentExpertJobExtractionTestBody(extractorType byte, extractorSlot int16, listType byte, targetSlot int16) []byte {
	body := make([]byte, currentExpertJobExtractionRequestSize)
	body[0] = extractorType
	binary.LittleEndian.PutUint16(body[1:3], uint16(extractorSlot))
	body[3] = listType
	binary.LittleEndian.PutUint16(body[4:6], uint16(targetSlot))
	return body
}

func currentExpertJobExtractionBatchTestBody(extractorType byte, extractorSlot int16, listType byte, slots []int16) []byte {
	body := make([]byte, currentExpertJobExtractionBatchHeaderSize+len(slots)*2)
	body[0] = extractorType
	binary.LittleEndian.PutUint16(body[1:3], uint16(extractorSlot))
	body[3] = listType
	binary.LittleEndian.PutUint16(body[4:6], uint16(currentExpertJobExtractionBatchSentinel))
	copy(body[6:10], currentExpertJobExtractionBatchMarker[:])
	body[10] = byte(len(slots))
	for index, slot := range slots {
		offset := currentExpertJobExtractionBatchHeaderSize + index*2
		binary.LittleEndian.PutUint16(body[offset:offset+2], uint16(slot))
	}
	return body
}

// 一个连接缓冲区里混着两种包头：op416/op205 走 13 字节普通包头，容器重建走
// 16 字节场景包头。这里按 opcode 选包头长度逐个切出来（长度字段两者一致）。
func currentExtractionBatchSplitAll(t *testing.T, data []byte) []currentExtractionBatchPacket {
	t.Helper()
	packets := []currentExtractionBatchPacket{}
	for len(data) > 0 {
		if len(data) < dnfproto.GameServerUpperHeaderSize {
			t.Fatalf("trailing game upper bytes=%d data=%x", len(data), data)
		}
		total := int(binary.LittleEndian.Uint32(data[3:7]))
		if total < dnfproto.GameServerUpperHeaderSize || total > len(data) {
			t.Fatalf("game upper length=%d data=%d", total, len(data))
		}
		headerSize := dnfproto.GameServerUpperHeaderSize
		msgID := binary.LittleEndian.Uint16(data[1:3])
		if msgID == uint16(dnfenum.CmdPacketLeaveParty) {
			headerSize = dnfproto.GameServerUpperHeaderSize16
		}
		if total < headerSize {
			t.Fatalf("game upper packet too short for header: total=%d header=%d", total, headerSize)
		}
		packets = append(packets, currentExtractionBatchPacket{
			classification: data[0],
			msgID:          msgID,
			body:           append([]byte(nil), data[headerSize:total]...),
		})
		data = data[total:]
	}
	return packets
}

func countCurrentExtractionBatchAcks(packets []currentExtractionBatchPacket) int {
	count := 0
	for _, packet := range packets {
		if packet.msgID == uint16(dnfenum.CmdPacketExpertExtraction) {
			count++
		}
	}
	return count
}

func countCurrentExtractionBatchRefreshes(packets []currentExtractionBatchPacket) int {
	count := 0
	for _, packet := range packets {
		if packet.msgID == uint16(dnfenum.CmdPacketLeaveParty) {
			count++
		}
	}
	return count
}

func firstCurrentExtractionBatchAck(t *testing.T, packets []currentExtractionBatchPacket) currentExtractionBatchPacket {
	t.Helper()
	for _, packet := range packets {
		if packet.msgID == uint16(dnfenum.CmdPacketExpertExtraction) {
			return packet
		}
	}
	t.Fatalf("no extraction ACK in %s", describeCurrentExtractionBatchPackets(packets))
	return currentExtractionBatchPacket{}
}

func describeCurrentExtractionBatchPackets(packets []currentExtractionBatchPacket) string {
	parts := make([]string, 0, len(packets))
	for _, packet := range packets {
		parts = append(parts, fmt.Sprintf("class=%d msg=%d body=%d", packet.classification, packet.msgID, len(packet.body)))
	}
	return "[" + strings.Join(parts, " ") + "]"
}
