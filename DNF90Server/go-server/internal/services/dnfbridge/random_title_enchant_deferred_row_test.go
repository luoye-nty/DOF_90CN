package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 本文件钉住 2026-09-25「称号随机附魔一进图就消失」的修法：
// mode1 的 create 行不携带 0x77 行的 +0x61..+0x65，所以带着附魔的行除了照旧进 create
// 列表，还必须额外进一次延时更新行（sub_2007B00，逐字节携带完整 0x77 行）。
// 相邻的守护者宝石 socket 字（+0x65..+0x6C）落在同一个缺口里，走的是另一条已验证通路
// （mode1 之后补 list-3 op14）。
const (
	randomTitleEnchantTestCharacterID = "19"
	randomTitleEnchantTestActorSlot   = 0x0d
	randomTitleEnchantTestItemID      = 400330111
	randomTitleEnchantTestConfigID    = 10001
	randomTitleEnchantTestRowID       = 72
	randomTitleEnchantTestSeed        = 898202557
)

// randomTitleEnchantTestEquipmentEntry 造一条与现场同形的 0x77 称号行：槽位 13、
// item 400330111、品质种子 898202557，附魔（config/row）在 +0x61..+0x65。
// withEnchant=false 时只少这一段，其余完全相同，用来做"没有附魔的行不该被改"的对照。
func randomTitleEnchantTestEquipmentEntry(itemID int64, withEnchant bool) dnfrepo.EquipmentEntry {
	var entry currentItemListEntry
	entry.patchCore(13, uint32(itemID), randomTitleEnchantTestSeed)
	if withEnchant {
		binary.LittleEndian.PutUint16(
			entry.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset],
			randomTitleEnchantTestConfigID,
		)
		binary.LittleEndian.PutUint16(
			entry.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd],
			randomTitleEnchantTestRowID,
		)
	}
	extra := map[string]string{"current_exe_equipment_type": "13"}
	if withEnchant {
		extra["random_title_enchant_config_id"] = "10001"
		extra["random_title_enchant_row_id"] = "72"
	}
	return dnfrepo.EquipmentEntry{
		SlotIndex: 13,
		ItemID:    itemID,
		RawEntry:  append([]byte(nil), entry.data[:]...),
		Extra:     extra,
	}
}

func newRandomTitleEnchantRepositories(t *testing.T, entries map[string]dnfrepo.EquipmentEntry) dnfrepo.Group {
	t.Helper()
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: randomTitleEnchantTestCharacterID,
		AccountID:   "account-1",
		Slot:        0,
		Name:        "EnchantedTitle",
		Job:         "0",
		Level:       90,
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: randomTitleEnchantTestCharacterID,
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: randomTitleEnchantTestCharacterID,
		Entries:     entries,
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	return repos
}

func newRandomTitleEnchantTestFixture(
	t *testing.T,
	entries map[string]dnfrepo.EquipmentEntry,
) (*Service, *csharpLegacyUserInfoReader, dnfrepo.CharacterRecord) {
	t.Helper()
	ctx := context.Background()
	repos := newRandomTitleEnchantRepositories(t, entries)
	service, _, _ := newGameplayDomainBridgeFixture(repos)
	character, found, err := repos.Character.Load(ctx, randomTitleEnchantTestCharacterID)
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	reader := &csharpLegacyUserInfoReader{
		ctx:         ctx,
		characterID: randomTitleEnchantTestCharacterID,
		service:     service,
	}
	return service, reader, character
}

func TestCurrentItemListEntryCarriesRandomTitleEnchant(t *testing.T) {
	cases := []struct {
		name         string
		configID     uint16
		rowID        uint16
		wantCarrying bool
	}{
		{name: "config and row", configID: randomTitleEnchantTestConfigID, rowID: randomTitleEnchantTestRowID, wantCarrying: true},
		{name: "config only", configID: randomTitleEnchantTestConfigID, wantCarrying: false},
		{name: "row only", rowID: randomTitleEnchantTestRowID, wantCarrying: false},
		{name: "neither", wantCarrying: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var entry currentItemListEntry
			binary.LittleEndian.PutUint16(
				entry.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset],
				testCase.configID,
			)
			binary.LittleEndian.PutUint16(
				entry.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd],
				testCase.rowID,
			)
			if got := currentItemListEntryCarriesRandomTitleEnchant(entry); got != testCase.wantCarrying {
				t.Fatalf("carries enchant = %t, want %t", got, testCase.wantCarrying)
			}
		})
	}
}

// 核心钉子：带附魔的称号行必须**同时**出现在 create 列表和延时更新列表里。
// 只进延时列表是不允许的——create 行是角色身上装备对象唯一的建立来源，去掉它等于把称号
// 本身弄丢（那正是本次改动要避免的"修了 A 坏了 B"）。
func TestCurrentMode1EquipmentRowsKeepEnchantedTitleInCreateAndAddDeferredRow(t *testing.T) {
	_, reader, _ := newRandomTitleEnchantTestFixture(t, map[string]dnfrepo.EquipmentEntry{
		"13": randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, true),
	})
	rows := reader.currentMode1EquipmentObjectRows()
	if len(rows) != 1 {
		t.Fatalf("projected equipment rows = %d, want 1", len(rows))
	}
	if !currentMode1EquipmentCreateRowHasVerifiedState(rows[0]) {
		t.Fatalf("enchanted title row lost its verified create state: %+v", rows[0])
	}
	if got := len(currentMode1EquipmentCreateRows(rows)); got != 1 {
		t.Fatalf("create rows = %d, want 1 (the title object must still be created)", got)
	}
	updateRows := currentMode1EquipmentDeferredUpdateRows(rows)
	if len(updateRows) != 1 {
		t.Fatalf("deferred update rows = %d, want 1", len(updateRows))
	}
	if !currentItemListEntryCarriesRandomTitleEnchant(updateRows[0].update) {
		t.Fatalf("deferred row does not carry the enchant: %x", updateRows[0].update.data[:])
	}
}

// 反向钉子：同一个称号行，只是没有附魔，行为必须和改动前完全一致——只进 create 列表，
// 延时更新列表为空。
func TestCurrentMode1EquipmentRowsLeaveUnenchantedTitleAsCreateOnly(t *testing.T) {
	_, reader, _ := newRandomTitleEnchantTestFixture(t, map[string]dnfrepo.EquipmentEntry{
		"13": randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, false),
	})
	rows := reader.currentMode1EquipmentObjectRows()
	if len(rows) != 1 || !currentMode1EquipmentCreateRowHasVerifiedState(rows[0]) {
		t.Fatalf("unenchanted title rows = %+v", rows)
	}
	if got := len(currentMode1EquipmentCreateRows(rows)); got != 1 {
		t.Fatalf("create rows = %d, want 1", got)
	}
	if got := len(currentMode1EquipmentDeferredUpdateRows(rows)); got != 0 {
		t.Fatalf("deferred update rows = %d, want 0 for a row without the enchant", got)
	}
}

// 回归钉子：普通装备（武器）必须一行都不多、一行都不少。
func TestCurrentMode1EquipmentRowsLeaveOrdinaryEquipmentAlone(t *testing.T) {
	title := randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, true)
	var weapon currentItemListEntry
	weapon.patchCore(12, 101040485, 12345)
	titleOnly := map[string]dnfrepo.EquipmentEntry{"13": title}
	titleAndWeapon := map[string]dnfrepo.EquipmentEntry{
		"13": title,
		"12": {SlotIndex: 12, ItemID: 101040485, RawEntry: append([]byte(nil), weapon.data[:]...),
			Extra: map[string]string{"current_exe_equipment_type": "12"}},
	}

	_, onlyTitleReader, _ := newRandomTitleEnchantTestFixture(t, titleOnly)
	onlyTitleRows := onlyTitleReader.currentMode1EquipmentObjectRows()
	_, bothReader, _ := newRandomTitleEnchantTestFixture(t, titleAndWeapon)
	bothRows := bothReader.currentMode1EquipmentObjectRows()

	if len(onlyTitleRows) != 1 || len(bothRows) != 2 {
		t.Fatalf("projected rows title=%d title+weapon=%d, want 1 and 2", len(onlyTitleRows), len(bothRows))
	}
	if got := len(currentMode1EquipmentCreateRows(bothRows)); got != 2 {
		t.Fatalf("create rows = %d, want 2 (ordinary equipment unchanged)", got)
	}
	updateRows := currentMode1EquipmentDeferredUpdateRows(bothRows)
	if len(updateRows) != 1 || updateRows[0].slot != 13 {
		t.Fatalf("deferred update rows = %+v, want only the enchanted title slot 13", updateRows)
	}
}

// 端到端钉子：真正构出来的 mode1 报体里，称号的 create 行仍在**原位**，附魔的完整 0x77 行
// 出现在它后面的延时更新区里。
func TestBuildCurrentSelectedUserInfoMode1BodyCarriesTitleEnchantInDeferredRow(t *testing.T) {
	for _, withEnchant := range []bool{true, false} {
		name := "without enchant"
		if withEnchant {
			name = "with enchant"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			service, reader, character := newRandomTitleEnchantTestFixture(t, map[string]dnfrepo.EquipmentEntry{
				"13": randomTitleEnchantTestEquipmentEntry(randomTitleEnchantTestItemID, withEnchant),
			})
			rows := reader.currentMode1EquipmentObjectRows()
			if len(rows) != 1 {
				t.Fatalf("projected rows = %d, want 1", len(rows))
			}
			createRowSize := currentMode1EquipmentCreateRowWireSizeFor(rows[0])
			mode1 := service.buildCurrentSelectedUserInfoMode1Body(
				ctx,
				&gameSession{selectedCharacterID: 19},
				nil,
				character,
				true,
				19,
			)
			if len(mode1) < currentMode1CreateRowsOffset+createRowSize+7 {
				t.Fatalf("mode1 body too short: %d", len(mode1))
			}
			if got := int(mode1[currentMode1CreateCountOffset]); got != 1 {
				t.Fatalf("mode1 create count = %d, want 1", got)
			}
			createRow := mode1[currentMode1CreateRowsOffset : currentMode1CreateRowsOffset+createRowSize]
			if createRow[0] != randomTitleEnchantTestActorSlot {
				t.Fatalf("create row actor slot = 0x%02x, want 0x%02x", createRow[0], randomTitleEnchantTestActorSlot)
			}
			if got := binary.LittleEndian.Uint32(createRow[1:5]); got != randomTitleEnchantTestItemID {
				t.Fatalf("create row item id = %d, want %d", got, randomTitleEnchantTestItemID)
			}

			pos := currentMode1CreateRowsOffset + createRowSize
			pos += 4 // sub_1D77560 clone-title dword
			pos += 2 // sub_2007B00 key/count pair key
			deferredCount := int(mode1[pos])
			pos++
			if !withEnchant {
				if deferredCount != 0 {
					t.Fatalf("mode1 deferred row count = %d, want 0 without the enchant", deferredCount)
				}
				return
			}
			if deferredCount != 1 {
				t.Fatalf("mode1 deferred row count = %d, want 1", deferredCount)
			}
			if mode1[pos] != 0 || mode1[pos+1] != 3 {
				t.Fatalf("deferred row kind/type = 0x%02x/0x%02x, want 0x00/0x03", mode1[pos], mode1[pos+1])
			}
			if got := binary.LittleEndian.Uint16(mode1[pos+2 : pos+4]); got != 13 {
				t.Fatalf("deferred row slot = %d, want 13", got)
			}
			rowStart := pos + 4
			if rowStart+currentItemListEntryWireSize > len(mode1) {
				t.Fatalf("mode1 body truncated inside the deferred row: %d", len(mode1))
			}
			var row currentItemListEntry
			copy(row.data[:], mode1[rowStart:rowStart+currentItemListEntryWireSize])
			if !currentItemListEntryCarriesRandomTitleEnchant(row) {
				t.Fatalf("deferred 0x77 row lost the enchant: %x", row.data[:])
			}
			if got := binary.LittleEndian.Uint16(
				row.data[currentRandomTitleEnchantConfigOffset:currentRandomTitleEnchantRowOffset],
			); got != randomTitleEnchantTestConfigID {
				t.Fatalf("deferred row config = %d, want %d", got, randomTitleEnchantTestConfigID)
			}
			if got := binary.LittleEndian.Uint16(
				row.data[currentRandomTitleEnchantRowOffset:currentRandomTitleEnchantFieldEnd],
			); got != randomTitleEnchantTestRowID {
				t.Fatalf("deferred row row id = %d, want %d", got, randomTitleEnchantTestRowID)
			}
		})
	}
}
