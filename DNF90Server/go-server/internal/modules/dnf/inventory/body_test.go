package inventory

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestBuildCommonItemListRefreshBodyAddsAvatarOptionalLengthsPerRow(t *testing.T) {
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypeAvatar, 3): {ItemID: 1001, Count: 1},
		slotKey(listTypeAvatar, 4): {ItemID: 1002, Count: 1},
	}
	body := buildCommonItemListRefreshBodyWithState(listTypeAvatar, slots, dnfrepo.CharacterContainerState{})
	const rowSize = currentItemListEntrySize + 8
	if len(body) != 5+2*rowSize {
		t.Fatalf("avatar refresh len=%d want=%d", len(body), 5+2*rowSize)
	}
	for index := range 2 {
		row := body[5+index*rowSize : 5+(index+1)*rowSize]
		if !bytes.Equal(row[currentItemListEntrySize:], make([]byte, 8)) {
			t.Fatalf("row %d optional lengths=%x want zero", index, row[currentItemListEntrySize:])
		}
	}
}

func TestBuildCommonItemListRefreshBodyKeepsMainAndCargoRowsExact(t *testing.T) {
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 3):          {ItemID: 1001, Count: 1},
		slotKey(listTypePersonalCargo, 4): {ItemID: 1002, Count: 1},
	}
	state := dnfrepo.CharacterContainerState{MainSlotCount: 24, PersonalCargoSlotCount: 8}

	main := buildCommonItemListRefreshBodyWithState(listTypeMain, slots, state)
	if len(main) != 5+currentItemListEntrySize {
		t.Fatalf("main refresh len=%d want=%d", len(main), 5+currentItemListEntrySize)
	}
	cargo := buildCommonItemListRefreshBodyWithState(listTypePersonalCargo, slots, state)
	if len(cargo) != 6+currentItemListEntrySize {
		t.Fatalf("cargo refresh len=%d want=%d", len(cargo), 6+currentItemListEntrySize)
	}
	if cargo[len(cargo)-1] != 0 {
		t.Fatalf("cargo group count=%d want=0", cargo[len(cargo)-1])
	}
}

func TestBuildCommonItemListRefreshBodyWritesExpireTimeOffset(t *testing.T) {
	expireAt := time.Unix(1849989600, 0).UTC()
	raw := make([]byte, currentItemListEntrySize)
	binary.LittleEndian.PutUint32(raw[legacyWrongItemExpireTimeOffset:legacyWrongItemExpireTimeOffset+4], uint32(expireAt.Unix()))
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypeMain, 3): {
			ItemID:   1001,
			Count:    1,
			ExpireAt: expireAt,
			RawEntry: raw,
			Extra:    map[string]string{"value_b": "777"},
		},
	}
	body := buildCommonItemListRefreshBodyWithState(listTypeMain, slots, dnfrepo.CharacterContainerState{MainSlotCount: 24})
	if len(body) != 5+currentItemListEntrySize {
		t.Fatalf("main refresh len=%d want=%d", len(body), 5+currentItemListEntrySize)
	}
	row := body[5:]
	if got := binary.LittleEndian.Uint32(row[currentItemListExpireTimeOffset : currentItemListExpireTimeOffset+4]); got != uint32(expireAt.Unix()) {
		t.Fatalf("expire offset value=%d want=%d row=%x", got, expireAt.Unix(), row)
	}
	if got := binary.LittleEndian.Uint32(row[legacyWrongItemExpireTimeOffset : legacyWrongItemExpireTimeOffset+4]); got != 0 {
		t.Fatalf("legacy wrong expire offset value=%d want=0 row=%x", got, row)
	}
}

func TestBuildCommonItemListUpdateSuppliesStableOp44Identity(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID: 31,
		Count:  63,
		Extra: map[string]string{
			"item_kind": "stackable",
			"pvf_path":  "stackable/cash/contract_monarch3.stk",
		},
	}
	body := buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{
		slot:  69,
		stack: stack,
	}})
	if len(body) != 3+currentItemListEntrySize {
		t.Fatalf("item update len=%d want=%d", len(body), 3+currentItemListEntrySize)
	}
	row := body[3:]
	if got := binary.LittleEndian.Uint32(row[0x0E:0x12]); got != uint32(stack.ItemID) {
		t.Fatalf("stackable op14 identity=%d want item id %d", got, stack.ItemID)
	}

	stack.Extra["value_a"] = "12345"
	body = buildCommonItemListUpdateBody(listTypeMain, []commonItemListEntry{{
		slot:  69,
		stack: stack,
	}})
	if got := binary.LittleEndian.Uint32(body[3+0x0E : 3+0x12]); got != 12345 {
		t.Fatalf("explicit op14 identity=%d want 12345", got)
	}
}

func TestBuildPetItemListUpdateCarriesEnchantFieldsWithoutReplacingSerial(t *testing.T) {
	stack := dnfrepo.ItemStack{
		ItemID: 400990168,
		Count:  1,
		Extra: map[string]string{
			"creature_serial_or_handle": "37",
			"value_a":                   "10008663",
			"byte_12":                   "2",
		},
	}
	body := buildPetItemListUpdateBody([]commonItemListEntry{{slot: 24, stack: stack}})
	if len(body) != 3+currentItemListEntrySize || body[0] != listTypePet {
		t.Fatalf("pet update len=%d body=%x", len(body), body)
	}
	row := body[3:]
	if got := binary.LittleEndian.Uint32(row[0x06:0x0A]); got != 37 {
		t.Fatalf("pet serial=%d want=37", got)
	}
	if got := binary.LittleEndian.Uint32(row[0x0E:0x12]); got != 10008663 {
		t.Fatalf("pet enchant card=%d want=10008663", got)
	}
	if got := row[0x12]; got != 2 {
		t.Fatalf("pet enchant upgrade=%d want=2", got)
	}
	if got := binary.LittleEndian.Uint32(row[currentPetRemainSecondsOffset : currentPetRemainSecondsOffset+4]); got != 0 {
		t.Fatalf("permanent pet remaining seconds=%d want=0", got)
	}
}

func TestBuildPetItemListUpdateWritesRealRemainingSeconds(t *testing.T) {
	now := time.Now().UTC()
	expireAt := now.Add(time.Hour)
	stack := dnfrepo.ItemStack{
		ItemID:   400990168,
		Count:    1,
		ExpireAt: expireAt,
		Extra:    map[string]string{"creature_serial_or_handle": "41"},
	}
	body := buildPetItemListUpdateBody([]commonItemListEntry{{slot: 39, stack: stack}})
	row := body[3:]
	remaining := binary.LittleEndian.Uint32(row[currentPetRemainSecondsOffset : currentPetRemainSecondsOffset+4])
	if remaining < 3_598 || remaining > 3_600 {
		t.Fatalf("timed pet remaining seconds=%d want about 3600", remaining)
	}
	if got := binary.LittleEndian.Uint32(row[currentItemListExpireTimeOffset : currentItemListExpireTimeOffset+4]); got != uint32(expireAt.Unix()) {
		t.Fatalf("timed pet expiration=%d want=%d", got, expireAt.Unix())
	}
}

// 把装备放进个人仓库时，owner 会把 list 2 的 refresh 交给这个编码器（见
// account_shared.go 的 withAccountSharedCountRefresh 与 owner.go 的 RefreshListTypes
// 白名单）。+0x06 写成 Count=1 的话客户端立刻按种子 1 重画品级，不必等重登——这就是
// "装备一放进仓库品级就变了"的那条链路。
//
// 这条测试与 dnfbridge 的
// TestCurrentItemListEquipmentRowKeepsQualitySeedInEveryEquipmentContainer 是同一份契约
// 的两半：两层编码器的容器集合必须一致，历史上它们一起漏掉 list 2 才出的问题。
func TestBuildCommonItemListRefreshBodyKeepsCargoEquipmentQualitySeed(t *testing.T) {
	const seed = uint32(123456)
	slots := map[string]dnfrepo.ItemStack{
		slotKey(listTypePersonalCargo, 4): {
			ItemID: 1002,
			Count:  1,
			Extra: map[string]string{
				"item_kind":    "equipment",
				"quality_seed": "123456",
			},
		},
		slotKey(listTypePersonalCargo, 5): {
			ItemID: 3330,
			Count:  7,
			Extra:  map[string]string{"item_kind": "stackable"},
		},
		// 模块层目前不会自己编 list 12 的包体（account_cargo.go 明确跳过
		// listAccountCargo），但两层的容器集合必须一致，否则下一次谁补上 list-12
		// 编码就会再踩一次同样的坑。这里顺手钉住它能被 list-2 查询正确排除。
		slotKey(listTypeAccountCargo, 6): {
			ItemID: 1003,
			Count:  1,
			Extra: map[string]string{
				"item_kind":    "equipment",
				"quality_seed": "123456",
			},
		},
	}

	body := buildCommonItemListRefreshBodyWithState(
		listTypePersonalCargo, slots, dnfrepo.CharacterContainerState{PersonalCargoSlotCount: 8})

	if body[0] != listTypePersonalCargo {
		t.Fatalf("list byte=%d want=%d", body[0], listTypePersonalCargo)
	}
	// 5 字节表头 + 2 行（list 2 只收自己的键）+ 1 字节分组终止符。
	if len(body) != 5+2*currentItemListEntrySize+1 {
		t.Fatalf("cargo refresh len=%d want=%d", len(body), 5+2*currentItemListEntrySize+1)
	}

	equipment := body[5 : 5+currentItemListEntrySize]
	if got := binary.LittleEndian.Uint32(equipment[0x06:0x0A]); got != seed {
		t.Fatalf("仓库装备行 +0x06 = %d, want 品级种子 %d", got, seed)
	}
	stackable := body[5+currentItemListEntrySize : 5+2*currentItemListEntrySize]
	if got := binary.LittleEndian.Uint32(stackable[0x06:0x0A]); got != 7 {
		t.Fatalf("仓库消耗品行 +0x06 = %d, want 数量 7", got)
	}
	if body[len(body)-1] != 0 {
		t.Fatalf("cargo group count=%d want=0", body[len(body)-1])
	}
}

// 层间一致性：模块层编码器与 dnfbridge 投影对同一件装备必须给出同一个 +0x06。
// 两处各自维护一份容器表，漂移一次就是这次的现场，所以这条不变量单独钉一遍。
// dnfbridge 侧的同名断言在
// TestCurrentItemListEquipmentRowKeepsQualitySeedInEveryEquipmentContainer。
func TestCommonItemListRowAmountMatchesBridgeContainerSet(t *testing.T) {
	equipment := dnfrepo.ItemStack{
		ItemID: 1001,
		Count:  1,
		Extra: map[string]string{
			"item_kind":    "equipment",
			"quality_seed": "123456",
		},
	}
	for _, listType := range []byte{listTypeMain, listTypePersonalCargo, listTypeEquipment, listTypeAccountCargo} {
		if got := commonItemListRowAmount(listType, equipment); got != 123456 {
			t.Fatalf("list %d equipment amount=%d want quality seed 123456", listType, got)
		}
	}

	// 反面：时装 1 与公会勋章 38 的行 +0x06 本来就是 1，即使带着合法种子也不能改。
	for _, listType := range []byte{listTypeAvatar, listTypeGuildMedal} {
		if got := commonItemListRowAmount(listType, equipment); got != 1 {
			t.Fatalf("list %d amount=%d want the historical 1", listType, got)
		}
	}
}
