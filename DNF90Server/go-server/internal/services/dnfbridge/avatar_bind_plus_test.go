package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// observedDreamCubeRequestHex 是现场实测的 op1119 请求体（2026-10-04 21:06:45，
// CubeProbe.log 逐字节抄下来）：+0x10 = 101560739（玩家点名的稀有装扮头发），
// +0x16 = 8 件材料，+0x18 起 8 个 (槽, 道具)。
const observedDreamCubeRequestHex = "2003000058020000EDA6852B385D0000A3B10D0604000800" +
	"230018B20D06240023B20D061C006D8A0D061D006D8A0D061600B6A300001A00638A0D061B006D8A0D06210010B20D06" +
	"A7040000580000000C99F81A60F81A00B97B32023C3E199204000000320000000018144360F81A0000143A4364F81A00" +
	"E000AA01A8B0716560F81A0001000000E4F81A007A325301"

func observedDreamCubeRequest(t *testing.T) []byte {
	t.Helper()
	body, err := hex.DecodeString(observedDreamCubeRequestHex)
	if err != nil {
		t.Fatalf("fixture hex: %v", err)
	}
	if len(body) != 144 {
		t.Fatalf("fixture body=%d want=144", len(body))
	}
	return body
}

func observedDreamCubeMaterials() []currentAvatarBindPlusMaterial {
	return []currentAvatarBindPlusMaterial{
		{Slot: 0x23, ItemID: 101560856},
		{Slot: 0x24, ItemID: 101560867},
		{Slot: 0x1C, ItemID: 101550701},
		{Slot: 0x1D, ItemID: 101550701},
		{Slot: 0x16, ItemID: 41910},
		{Slot: 0x1A, ItemID: 101550691},
		{Slot: 0x1B, ItemID: 101550701},
		{Slot: 0x21, ItemID: 101560848},
	}
}

func TestCurrentAvatarBindPlusParsesObservedDreamCubeRequest(t *testing.T) {
	request, err := parseCurrentAvatarBindPlusRequest(observedDreamCubeRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestedItemID != 101560739 || request.Count != 8 || request.AbilityNo != 4 || request.BodyLength != 144 {
		t.Fatalf("request=%+v", request)
	}
	want := observedDreamCubeMaterials()
	if len(request.Materials) != len(want) {
		t.Fatalf("materials=%d want=%d", len(request.Materials), len(want))
	}
	for index := range want {
		if request.Materials[index] != want[index] {
			t.Fatalf("material[%d]=%+v want=%+v", index, request.Materials[index], want[index])
		}
	}
}

func TestCurrentAvatarBindPlusRejectsMalformedRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(body []byte) []byte
	}{
		{"too short", func([]byte) []byte { return make([]byte, 8) }},
		{"zero requested item", func(body []byte) []byte {
			binary.LittleEndian.PutUint32(body[0x10:0x14], 0)
			return body
		}},
		{"zero material count", func(body []byte) []byte {
			binary.LittleEndian.PutUint16(body[0x16:0x18], 0)
			return body
		}},
		{"count over eight", func(body []byte) []byte {
			binary.LittleEndian.PutUint16(body[0x16:0x18], 9)
			return body
		}},
		{"duplicate material slot", func(body []byte) []byte {
			copy(body[0x1E:0x24], body[0x18:0x1E])
			return body
		}},
		{"truncated materials", func(body []byte) []byte { return body[:0x20] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCurrentAvatarBindPlusRequest(test.mutate(observedDreamCubeRequest(t))); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// TestCurrentAvatarBindPlusSuccessBodyMatchesDecodedHandler 把回包逐字节钉死。
// 形状读自客户端处理器 0x1135C10（ReadBytes 0x36）与工作函数 0x1135680 /
// 0x1F2B470（容器下标）/ 0x1F2B850（按槽扣数量）。
func TestCurrentAvatarBindPlusSuccessBodyMatchesDecodedHandler(t *testing.T) {
	outcome := currentAvatarBindPlusOutcome{
		OutputItemID:  101560739,
		OutputSlot:    35,
		AbilityNo:     4,
		ConsumedSlots: []int16{35, 36, 28, 29, 22, 26, 27, 33},
	}
	body := buildCurrentAvatarBindPlusSuccessBody(outcome)
	if len(body) != currentAvatarBindPlusReplySize {
		t.Fatalf("reply=%d want=%d", len(body), currentAvatarBindPlusReplySize)
	}
	if body[0] != 1 {
		t.Fatalf("status=%d", body[0])
	}
	structured := body[1:]
	if len(structured) != currentAvatarBindPlusResultSize {
		t.Fatalf("struct=%d", len(structured))
	}
	if got := binary.LittleEndian.Uint32(structured[0x00:0x04]); got != 0 {
		t.Fatalf("+0x00=%d", got)
	}
	// +0x04 是容器下标（结果对象写进 container[槽]）：0x1F2B55D `mov [eax+ecx*4], esi`。
	if got := binary.LittleEndian.Uint16(structured[0x04:0x06]); got != 35 {
		t.Fatalf("+0x04=%d want=35（新道具落在哪个槽）", got)
	}
	if got := binary.LittleEndian.Uint32(structured[0x06:0x0A]); got != 101560739 {
		t.Fatalf("+0x06=%d want=101560739", got)
	}
	// +0x0A 是玩家选的属性（结果描述符构造器的参数）。现场曾经把槽号写在这里，
	// 客户端就把属性显示成了那个数字。
	if got := binary.LittleEndian.Uint16(structured[0x0A:0x0C]); got != 4 {
		t.Fatalf("+0x0A=%d want=4（玩家选的属性）", got)
	}
	if got := binary.LittleEndian.Uint16(structured[0x0C:0x0E]); got != 1 {
		t.Fatalf("+0x0C=%d want=1", got)
	}
	for index, want := range []int16{35, 36, 28, 29, 22, 26, 27, 33} {
		offset := 0x0E + index*2
		if got := int16(binary.LittleEndian.Uint16(structured[offset : offset+2])); got != want {
			t.Fatalf("slot[%d]=%d want=%d", index, got, want)
		}
	}
	for offset := 0x1E; offset < currentAvatarBindPlusResultSize; offset++ {
		if structured[offset] != 0 {
			t.Fatalf("tail byte %#x=%d want=0", offset, structured[offset])
		}
	}
	if got := hex.EncodeToString(currentAvatarBindPlusFailureBody()); got != "001600" {
		t.Fatalf("failure=%s", got)
	}
}

// TestCurrentAvatarBindPlusPadsEmptyMaterialGrid 短装（count<8）时后补 0：
// 客户端只为它自己填过的格子取用这些值，空格的 0 不会被读。
func TestCurrentAvatarBindPlusPadsEmptyMaterialGrid(t *testing.T) {
	body := buildCurrentAvatarBindPlusSuccessBody(currentAvatarBindPlusOutcome{
		OutputItemID:  2004,
		OutputSlot:    21,
		ConsumedSlots: []int16{21, 22},
	})
	structured := body[1:]
	if got := int16(binary.LittleEndian.Uint16(structured[0x0E:0x10])); got != 21 {
		t.Fatalf("slot0=%d", got)
	}
	if got := int16(binary.LittleEndian.Uint16(structured[0x10:0x12])); got != 22 {
		t.Fatalf("slot1=%d", got)
	}
	for index := 2; index < currentAvatarBindPlusMaterialCount; index++ {
		offset := 0x0E + index*2
		if got := binary.LittleEndian.Uint16(structured[offset : offset+2]); got != 0 {
			t.Fatalf("slot[%d]=%d want=0", index, got)
		}
	}
}

// TestRealPVFAvatarBindPlusResolvesDreamCubeTable 读真实 Script.pvf：
// 套装 0 的剑士头发必须解析成现场请求里那个 101560739。
func TestRealPVFAvatarBindPlusResolvesDreamCubeTable(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		pvfPath = realLukeRewardPVFPath()
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	config, err := loadCurrentAvatarBindPlusConfig(archive)
	if err != nil {
		t.Fatalf("load %s: %v", currentAvatarBindPlusPVFPath, err)
	}
	jobKey, err := currentAvatarBindPlusJobKey(archive, 0) // job 0 = 鬼剑士
	if err != nil {
		t.Fatalf("job key: %v", err)
	}
	if jobKey != "swordman" {
		t.Fatalf("jobKey=%q", jobKey)
	}
	output, err := currentAvatarBindPlusResolveOutput(config, jobKey, 101560739)
	if err != nil {
		t.Fatalf("resolve observed output: %v", err)
	}
	if output.SetIndex != 0 || output.Part != "hair" {
		t.Fatalf("output=%+v", output)
	}
	// 上一轮现场（20:21）点名的是 101546903：两张 PVF 里都没有这个道具，
	// 说明它不是这套表的输出，必须被拒。
	if _, err := currentAvatarBindPlusResolveOutput(config, jobKey, 101546903); err == nil {
		t.Fatal("101546903 must not resolve through the absolute-bind table")
	}
	// 别的职业的输出不能被鬼剑士点名。
	if _, err := currentAvatarBindPlusResolveOutput(config, jobKey, 102560721); err == nil {
		t.Fatal("fighter hair must not be requestable by a swordman")
	}
	// 同一批道具挂在 [swordman]/[demonic swordman]/[knight] 三个名字下，三个都要能解析。
	for _, key := range []string{"swordman", "demonicswordman", "knight"} {
		if _, err := currentAvatarBindPlusResolveOutput(config, key, 101560739); err != nil {
			t.Fatalf("job=%s: %v", key, err)
		}
	}
	if len(config.Values["hair"]) != 3 || config.Goals["hair"] != 2970 {
		t.Fatalf("hair values=%v goal=%d", config.Values["hair"], config.Goals["hair"])
	}
}

func newAvatarBindPlusPVFTestSource() avatarCompoundPVFTestSource {
	cube := "[absolute bind cube]\n0\n[absolute bind plus rare]\n" +
		"`[swordman]` `hair` 2004 `hat` 2005 `face` 2006 `neck` 2007 `coat` 2008 `pants` 2009 `belt` 2010 `shoes` 2011 " +
		"`[fighter]` `hair` 3004 `hat` 3005 `face` 3006 `neck` 3007 `coat` 3008 `pants` 3009 `belt` 3010 `shoes` 3011 \n" +
		"[/absolute bind plus rare]\n[/absolute bind cube]\n" +
		"[absolute bind cube]\n1\n[absolute bind plus rare]\n" +
		"`[swordman]` `hair` 4004 `hat` 4005 `face` 4006 `neck` 4007 `coat` 4008 `pants` 4009 `belt` 4010 `shoes` 4011 \n" +
		"[/absolute bind plus rare]\n[/absolute bind cube]\n" +
		"[absolute bind plus value]\n`hair` 198 264 792 \n[/absolute bind plus value]\n" +
		"[output avatar goal value]\n`hair` 2970 \n[/output avatar goal value]\n"
	return avatarCompoundPVFTestSource{
		"etc/compoundavatar.etc":         "1 `CompoundAvatar_swordman.etc` 2 `CompoundAvatar_fighter.etc`\n",
		"etc/chn_absolute_bind_cube.etc": cube,
	}
}

// newAvatarBindPlusCatalog 造一个最小目录：777 = 套装 0 的绝对绑定合成器，
// 778 = 套装 1 的，200x = 套装 0 的输出，301x = 材料装扮。
func newAvatarBindPlusCatalog(source avatarCompoundPVFTestSource, materials []currentAvatarBindPlusMaterial) *pvfDungeonDropCatalog {
	source["equipment/output.equ"] = "[equipment type] `[hair avatar]`\n[grade] 3\n"
	source["equipment/material.equ"] = "[equipment type] `[hair avatar]`\n[grade] 1\n"
	refs := map[uint32]dungeonDropItemReference{
		777:  {kind: dungeonDropItemStackable, path: "cube.stk"},
		778:  {kind: dungeonDropItemStackable, path: "cube.stk"},
		2004: {kind: dungeonDropItemEquipment, path: "output.equ"},
	}
	definitions := map[uint32]dungeonDropItemDefinition{
		777:  {ItemID: 777, Kind: dungeonDropItemStackable, PVFPath: "stackable/cube.stk", ActionType: "[absolute bind cube]", ActionValues: &dungeonDropActionValues{Values: []int64{0, 8, 2, 0, 0}}},
		778:  {ItemID: 778, Kind: dungeonDropItemStackable, PVFPath: "stackable/cube.stk", ActionType: "[absolute bind cube]", ActionValues: &dungeonDropActionValues{Values: []int64{1, 8, 2, 0, 0}}},
		2004: {ItemID: 2004, Kind: dungeonDropItemEquipment, PVFPath: "equipment/output.equ", EquipmentType: "[hair avatar]", Grade: 3},
	}
	for index, material := range materials {
		refs[material.ItemID] = dungeonDropItemReference{kind: dungeonDropItemEquipment, path: "material.equ"}
		definitions[material.ItemID] = dungeonDropItemDefinition{
			ItemID: material.ItemID, Kind: dungeonDropItemEquipment,
			PVFPath: "equipment/material.equ", EquipmentType: "[hair avatar]", Grade: int64(index + 1),
		}
	}
	return &pvfDungeonDropCatalog{source: source, itemRefs: refs, itemCache: definitions}
}

func syntheticBindPlusMaterials() []currentAvatarBindPlusMaterial {
	return []currentAvatarBindPlusMaterial{
		{Slot: 21, ItemID: 3001}, {Slot: 22, ItemID: 3002}, {Slot: 23, ItemID: 3003}, {Slot: 24, ItemID: 3004},
		{Slot: 25, ItemID: 3005}, {Slot: 26, ItemID: 3006}, {Slot: 27, ItemID: 3007}, {Slot: 28, ItemID: 3008},
	}
}

func bindPlusTestService(t *testing.T, ctx context.Context, slots map[string]dnfrepo.ItemStack) (*Service, dnfrepo.Group) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: slots}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	return service, repositories
}

// parseBindPlusItemRow 拆一条 0x14 增量行：u8 列表 / u16 行数 / 每行 0x77 字节，
// 行首就是 (slot, itemID, amount)。
func parseBindPlusItemRow(t *testing.T, body []byte) (byte, int16, uint32, uint32) {
	t.Helper()
	if len(body) < 3+currentItemListEntryWireSize {
		t.Fatalf("item row too short: %d", len(body))
	}
	listType := body[0]
	if count := binary.LittleEndian.Uint16(body[1:3]); count != 1 {
		t.Fatalf("item row count=%d want=1 body=%x", count, body)
	}
	entry := body[3:]
	return listType,
		int16(binary.LittleEndian.Uint16(entry[0x00:0x02])),
		binary.LittleEndian.Uint32(entry[0x02:0x06]),
		binary.LittleEndian.Uint32(entry[0x06:0x0A])
}

// TestCurrentAvatarBindPlusSendsCubeRowThenReplyThenOutputRow 钉住三包顺序：
//
//  1. 合成器那一格的增量行 —— 现场"合成器不及时消失、要整理背包才消失"的修复：
//     客户端 op1119 的处理器只认回包 +0x0E 的 8 个材料格，合成器那一格必须单独下发；
//  2. class-1 op1119 成功回包；
//  3. 结果装扮那一格的权威行 —— 把客户端自建对象的属性校正成服务端口径。
func TestCurrentAvatarBindPlusSendsCubeRowThenReplyThenOutputRow(t *testing.T) {
	ctx := context.Background()
	materials := syntheticBindPlusMaterials()
	// 提交之后的真实仓库状态：合成器由 2 变 1，第一个材料槽变成输出装扮。
	committed := map[string]dnfrepo.ItemStack{"0:93": {ItemID: 777, Count: 1}}
	for _, material := range materials[1:] {
		committed["1:"+strconv.Itoa(int(material.Slot))] = dnfrepo.ItemStack{ItemID: int64(material.ItemID), Count: 1}
	}
	committed["1:21"] = dnfrepo.ItemStack{
		ItemID: 2004, Count: 1,
		Extra: map[string]string{"avatar_ability_no": "7", "item_kind": "avatar"},
	}
	service, _ := bindPlusTestService(t, ctx, committed)
	connection := &bufferConn{}
	session := &gameSession{conn: connection, accountID: "dnf:1", selectedCharacterID: 19}
	outcome := currentAvatarBindPlusOutcome{
		OutputItemID: 2004, OutputSlot: 21, AbilityNo: 7,
		ConsumedSlots: []int16{21, 22, 23, 24, 25, 26, 27, 28},
		SetIndex:      0, Part: "hair", CubeItemID: 777,
		CubeListType: dnfrepo.MainInventoryListType, CubeSlot: 93, CubeRemain: 1,
	}
	if err := service.sendCurrentAvatarBindPlusResult(session, outcome); err != nil {
		t.Fatal(err)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	second, rest := splitGameServerUpperPacket(t, rest)
	third, rest := splitGameServerUpperPacket(t, rest)
	if len(rest) != 0 {
		t.Fatalf("多出来的字节: %x", rest)
	}
	// 1) 合成器行：主背包(list 0) 槽 93，整堆终值 = 1（不是删除行）。
	if first.Header.Classification != 0 || first.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("第一包必须先是增量行: %+v", first.Header)
	}
	listType, slot, itemID, amount := parseBindPlusItemRow(t, first.Body)
	if listType != dnfrepo.MainInventoryListType || slot != 93 || itemID != 777 || amount != 1 {
		t.Fatalf("合成器行 = list%d slot=%d item=%d amount=%d，want list0 slot93 item777 amount1",
			listType, slot, itemID, amount)
	}
	// 2) 回包：class-1 op1119，body 逐字节等于构造器输出。
	if second.Header.Classification != 1 || second.Header.MsgID != uint16(dnfenum.CmdPacketBindPlus) {
		t.Fatalf("第二包必须是 class-1 op1119: %+v", second.Header)
	}
	if want := buildCurrentAvatarBindPlusSuccessBody(outcome); !bytes.Equal(second.Body, want) {
		t.Fatalf("回包=%x\nwant=%x", second.Body, want)
	}
	// 3) 结果装扮行：装扮栏(list 1) 槽 21，道具 2004。
	if third.Header.Classification != 0 || third.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("第三包必须是增量行: %+v", third.Header)
	}
	listType, slot, itemID, amount = parseBindPlusItemRow(t, third.Body)
	if listType != currentAvatarInventoryListType || slot != 21 || itemID != 2004 || amount != 1 {
		t.Fatalf("结果行 = list%d slot=%d item=%d amount=%d，want list1 slot21 item2004 amount1",
			listType, slot, itemID, amount)
	}
}

// TestCurrentAvatarBindPlusCubeRowIsDeletionWhenStackEmpties 最后一个合成器用完时，
// 那一行必须是删除行（itemID 0xFFFFFFFF），否则客户端会留着一个数量 0 的空图标。
func TestCurrentAvatarBindPlusCubeRowIsDeletionWhenStackEmpties(t *testing.T) {
	ctx := context.Background()
	service, _ := bindPlusTestService(t, ctx, map[string]dnfrepo.ItemStack{
		"1:21": {ItemID: 2004, Count: 1, Extra: map[string]string{"avatar_ability_no": "4"}},
	})
	connection := &bufferConn{}
	session := &gameSession{conn: connection, accountID: "dnf:1", selectedCharacterID: 19}
	outcome := currentAvatarBindPlusOutcome{
		OutputItemID: 2004, OutputSlot: 21, AbilityNo: 4, ConsumedSlots: []int16{21},
		CubeItemID: 777, CubeListType: dnfrepo.MainInventoryListType, CubeSlot: 93, CubeRemain: 0,
	}
	if err := service.sendCurrentAvatarBindPlusResult(session, outcome); err != nil {
		t.Fatal(err)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	_, rest = splitGameServerUpperPacket(t, rest) // class-1 op1119 回包
	_, rest = splitGameServerUpperPacket(t, rest) // 结果装扮那一行
	if len(rest) != 0 {
		t.Fatalf("多出来的字节: %x", rest)
	}
	_, slot, itemID, amount := parseBindPlusItemRow(t, first.Body)
	if slot != 93 || itemID != math.MaxUint32 || amount != 0 {
		t.Fatalf("空合成器行 = slot=%d item=%d amount=%d，want slot93 item=0xFFFFFFFF amount=0",
			slot, itemID, amount)
	}
}

func TestCurrentAvatarBindPlusCommitsEightMaterialsAndOneCube(t *testing.T) {
	ctx := context.Background()
	materials := syntheticBindPlusMaterials()
	catalog := newAvatarBindPlusCatalog(newAvatarBindPlusPVFTestSource(), materials)
	slots := map[string]dnfrepo.ItemStack{"0:93": {ItemID: 777, Count: 2}}
	for _, material := range materials {
		slots["1:"+strconv.Itoa(int(material.Slot))] = dnfrepo.ItemStack{ItemID: int64(material.ItemID), Count: 1}
	}
	service, repositories := bindPlusTestService(t, ctx, slots)
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	outcome, err := service.commitCurrentAvatarBindPlus(ctx, session, catalog, currentAvatarBindPlusRequest{
		RequestedItemID: 2004, AbilityNo: 7, Count: 8, Materials: materials, BodyLength: 144,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.OutputItemID != 2004 || outcome.OutputSlot != 21 || outcome.SetIndex != 0 ||
		outcome.Part != "hair" || outcome.CubeItemID != 777 || outcome.CubeRemain != 1 {
		t.Fatalf("outcome=%+v", outcome)
	}
	if len(outcome.ConsumedSlots) != 8 || outcome.ConsumedSlots[7] != 28 {
		t.Fatalf("consumed=%v", outcome.ConsumedSlots)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if inventory.Slots["0:93"].Count != 1 {
		t.Fatalf("cube=%+v", inventory.Slots["0:93"])
	}
	for _, material := range materials[1:] {
		if _, remains := inventory.Slots["1:"+strconv.Itoa(int(material.Slot))]; remains {
			t.Fatalf("material slot %d remains", material.Slot)
		}
	}
	output := inventory.Slots["1:21"]
	if output.ItemID != 2004 || output.Count != 1 || output.Extra["avatar_ability_no"] != "7" ||
		len(output.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("output=%+v", output)
	}
}

func TestCurrentAvatarBindPlusLastCubeRemovesTheStack(t *testing.T) {
	ctx := context.Background()
	materials := syntheticBindPlusMaterials()
	catalog := newAvatarBindPlusCatalog(newAvatarBindPlusPVFTestSource(), materials)
	slots := map[string]dnfrepo.ItemStack{"0:93": {ItemID: 777, Count: 1}}
	for _, material := range materials {
		slots["1:"+strconv.Itoa(int(material.Slot))] = dnfrepo.ItemStack{ItemID: int64(material.ItemID), Count: 1}
	}
	service, repositories := bindPlusTestService(t, ctx, slots)
	outcome, err := service.commitCurrentAvatarBindPlus(ctx, &gameSession{accountID: "dnf:1", selectedCharacterID: 19},
		catalog, currentAvatarBindPlusRequest{RequestedItemID: 2004, AbilityNo: 4, Count: 8, Materials: materials, BodyLength: 144})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.CubeRemain != 0 {
		t.Fatalf("outcome=%+v", outcome)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if _, remains := inventory.Slots["0:93"]; remains {
		t.Fatalf("empty cube stack remains: %+v", inventory.Slots["0:93"])
	}
}

func TestCurrentAvatarBindPlusRejectsAndRollsBack(t *testing.T) {
	for _, test := range []struct {
		name          string
		mutate        func(slots map[string]dnfrepo.ItemStack)
		requestedItem uint32
		wantErr       error
	}{
		{"no cube in bag", func(slots map[string]dnfrepo.ItemStack) { delete(slots, "0:93") }, 2004, errCurrentAvatarBindPlusCube},
		{"cube of another set only", func(slots map[string]dnfrepo.ItemStack) { slots["0:93"] = dnfrepo.ItemStack{ItemID: 778, Count: 2} }, 2004, errCurrentAvatarBindPlusCube},
		{"material item mismatch", func(slots map[string]dnfrepo.ItemStack) {
			slots["1:22"] = dnfrepo.ItemStack{ItemID: 9999, Count: 1}
		}, 2004, errCurrentAvatarBindPlusMaterial},
		{"material slot empty", func(slots map[string]dnfrepo.ItemStack) { delete(slots, "1:25") }, 2004, errCurrentAvatarBindPlusMaterial},
		{"material stacked twice", func(slots map[string]dnfrepo.ItemStack) {
			slots["1:25"] = dnfrepo.ItemStack{ItemID: 3005, Count: 2}
		}, 2004, errCurrentAvatarBindPlusMaterial},
		{"unknown output", func(map[string]dnfrepo.ItemStack) {}, 9999, errCurrentAvatarBindPlusOutputInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			materials := syntheticBindPlusMaterials()
			catalog := newAvatarBindPlusCatalog(newAvatarBindPlusPVFTestSource(), materials)
			slots := map[string]dnfrepo.ItemStack{"0:93": {ItemID: 777, Count: 2}}
			for _, material := range materials {
				slots["1:"+strconv.Itoa(int(material.Slot))] = dnfrepo.ItemStack{ItemID: int64(material.ItemID), Count: 1}
			}
			test.mutate(slots)
			// 记下"动过之后"的现场：回滚必须把它原样还回来。
			wantCube, wantCubeExists := slots["0:93"]
			wantMaterial := slots["1:21"]
			service, repositories := bindPlusTestService(t, ctx, slots)
			_, err := service.commitCurrentAvatarBindPlus(ctx, &gameSession{accountID: "dnf:1", selectedCharacterID: 19},
				catalog, currentAvatarBindPlusRequest{
					RequestedItemID: test.requestedItem, AbilityNo: 4, Count: 8, Materials: materials, BodyLength: 144,
				})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
			inventory, _, _ := repositories.Inventory.Load(ctx, "19")
			if got, exists := inventory.Slots["0:93"]; exists != wantCubeExists || (exists && got.Count != wantCube.Count) {
				t.Fatalf("cube not rolled back: exists=%t got=%+v wantExists=%t want=%+v", exists, got, wantCubeExists, wantCube)
			}
			if got, exists := inventory.Slots["1:21"]; !exists || got.ItemID != wantMaterial.ItemID {
				t.Fatalf("material 21 not rolled back: %+v", got)
			}
		})
	}
}
