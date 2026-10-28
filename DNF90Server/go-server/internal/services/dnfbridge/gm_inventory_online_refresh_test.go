package dnfbridge

// gm_inventory_online_refresh_test.go 钉住"在线实时删除"的核心承诺：
// 仓储改动提交之后，必须向该角色的在线会话补发一条 op14 单格刷新，
// 且被清空的格子以 0xFFFFFFFF 删除行下发。
//
// 为什么这条值得单独测：
//
//	旧实现对在线角色一律返回 ErrGMCharacterOnline（"删个东西先把人踢下线"）。
//	放开这个限制的前提是"仓储是唯一事实源、客户端显示靠增量刷新补"——
//	仓储那半边由现有事务语义保证，客户端那半边就靠这里验证的那一包。
//	如果哪天有人把刷新删掉，在线删除就会静默变成"服务端删了、玩家屏幕上还在"，
//	而这种不一致没有任何报错，只能靠用例拦住。

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	appkit "longheng.io/server/internal/platform/app"
)

func TestGMOnlineInventoryDeletePushesSlotRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11", AccountID: "dnf:live", Name: "Live", Job: "0", Level: 20,
	}); err != nil {
		t.Fatal(err)
	}
	// 两个槽分别覆盖两条刷新路径：
	//   0:4 按数量删 4 个（9→5）→ 刷新行应是"还剩 5 个"的实际物品行；
	//   0:5 整格删 → 刷新行必须变成删除行。
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "11",
		Slots: map[string]dnfrepo.ItemStack{
			"0:4": {ItemID: 1001, Count: 9},
			"0:5": {ItemID: 1002, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	conn := &bufferConn{}
	service.gameSessions[11] = &gameSession{conn: conn, connID: "gm-online-delete", selectedCharacterID: 11}

	// ① 按数量删除：格子还在，刷新行是"剩 5 个"。
	if _, err := service.GMDeleteInventoryItem(ctx, "11", "inventory", "0:4", 4); err != nil {
		t.Fatalf("GMDeleteInventoryItem(partial) = %v", err)
	}
	row := firstUpperItemRow(t, conn, uint16(dnfenum.CmdPacketWalkoutPartyMember))
	if row.slot != 4 || row.itemID != 1001 || row.amount != 5 {
		t.Fatalf("partial delete refresh row = %+v, want slot 4 item 1001 amount 5", row)
	}

	// ② 整格删除：格子空了，刷新行必须是删除行（itemID = 0xFFFFFFFF）。
	conn.write.Reset()
	if _, err := service.GMDeleteInventoryItem(ctx, "11", "inventory", "0:5", 0); err != nil {
		t.Fatalf("GMDeleteInventoryItem(whole) = %v", err)
	}
	row = firstUpperItemRow(t, conn, uint16(dnfenum.CmdPacketWalkoutPartyMember))
	if row.slot != 5 || row.itemID != math.MaxUint32 {
		t.Fatalf("whole-slot delete refresh row = %+v, want slot 5 with delete marker 0xFFFFFFFF", row)
	}

	// 仓储必须真的改了（刷新只是补客户端显示，不改仓储语义）。
	inventory, found, err := repositories.Inventory.Load(ctx, "11")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%v err=%v", found, err)
	}
	if stack := inventory.Slots["0:4"]; stack.ItemID != 1001 || stack.Count != 5 {
		t.Fatalf("slot 0:4 = %+v, want item 1001 count 5", stack)
	}
	if _, still := inventory.Slots["0:5"]; still {
		t.Fatalf("slot 0:5 should be gone, got %+v", inventory.Slots["0:5"])
	}
}

// TestGMOnlineInventoryClearPushesDeleteRowsForEveryOccupiedSlot
// 清空容器要给每一个原本占用的格子发删除行——清空后再去读仓储是读不到这些格子的，
// 所以实现必须在事务内先把占用键取出来（inventoryStackKeys），这条用例正是钉这一点。
func TestGMOnlineInventoryClearPushesDeleteRowsForEveryOccupiedSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "12", AccountID: "dnf:live2", Name: "Live2", Job: "0", Level: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "12",
		Slots: map[string]dnfrepo.ItemStack{
			"0:1": {ItemID: 2001, Count: 2},
			"0:2": {ItemID: 2002, Count: 3},
		},
		Warehouse: map[string]dnfrepo.ItemStack{
			"2:9": {ItemID: 3001, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	conn := &bufferConn{}
	service.gameSessions[12] = &gameSession{conn: conn, connID: "gm-online-clear", selectedCharacterID: 12}

	if _, err := service.GMClearInventory(ctx, "12", "inventory"); err != nil {
		t.Fatalf("GMClearInventory = %v", err)
	}
	rows := allUpperItemRows(t, conn, uint16(dnfenum.CmdPacketWalkoutPartyMember))
	if len(rows) != 2 {
		t.Fatalf("清空后应给 2 个原占用格子各发一行，实际 %d 行：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.itemID != math.MaxUint32 {
			t.Fatalf("清空的格子必须下发删除行，得到 %+v", row)
		}
	}
	if rows[0].slot != 1 || rows[1].slot != 2 {
		t.Fatalf("删除行应按槽位升序覆盖 1/2，得到 %+v", rows)
	}

	// 仓库不受影响：清 inventory 不该动 warehouse。
	inventory, found, err := repositories.Inventory.Load(ctx, "12")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%v err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("inventory 应被清空，得到 %+v", inventory.Slots)
	}
	if stack := inventory.Warehouse["2:9"]; stack.ItemID != 3001 {
		t.Fatalf("warehouse 不该被清空，得到 %+v", inventory.Warehouse)
	}
}

// TestGMOfflineInventoryDeleteSkipsRefresh 离线角色没有在线会话，删除照样成功且不写任何包。
func TestGMOfflineInventoryDeleteSkipsRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "13", AccountID: "dnf:off", Name: "Off", Job: "0", Level: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "13",
		Slots:       map[string]dnfrepo.ItemStack{"0:7": {ItemID: 4001, Count: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	if _, err := service.GMDeleteInventoryItem(ctx, "13", "inventory", "0:7", 0); err != nil {
		t.Fatalf("offline delete should still succeed, got %v", err)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "13")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%v err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("offline delete did not commit: %+v", inventory.Slots)
	}
}

// gmInventoryRow 从 op14 body 里解出的单行（0x77 行布局见 current_item_list_entry.patchCore）。
type gmInventoryRow struct {
	listType byte
	slot     int16
	itemID   uint32
	amount   uint32
}

// firstUpperItemRow 取该连接上发出的第一个包并解出唯一一行；行数不是 1 就失败。
func firstUpperItemRow(t *testing.T, conn *bufferConn, wantMsgID uint16) gmInventoryRow {
	t.Helper()
	rows := allUpperItemRows(t, conn, wantMsgID)
	if len(rows) != 1 {
		t.Fatalf("刷新包应只含 1 行，实际 %d 行：%+v", len(rows), rows)
	}
	return rows[0]
}

// allUpperItemRows 解析连接上发出的第一个 upper 包，按 listType 分组返回全部物品行。
//
// 只解析第一个包：本用例关心的是"补发的那一包"，单个容器清空只会产生一个包。
func allUpperItemRows(t *testing.T, conn *bufferConn, wantMsgID uint16) []gmInventoryRow {
	t.Helper()
	raw := conn.write.Bytes()
	if len(raw) < dnfproto.ChannelHeaderSize {
		t.Fatalf("在线会话没有收到任何包（buffer=%d 字节）——实时刷新没发出去", len(raw))
	}
	header := dnfproto.ChannelHeader{
		Classification: raw[0],
		MsgID:          binary.LittleEndian.Uint16(raw[1:3]),
		Length:         binary.LittleEndian.Uint32(raw[3:7]),
		Checksum:       binary.LittleEndian.Uint32(raw[7:11]),
		Seq:            binary.LittleEndian.Uint16(raw[11:13]),
	}
	if header.MsgID != wantMsgID {
		t.Fatalf("刷新包 msg_id = %d, want %d", header.MsgID, wantMsgID)
	}
	if int(header.Length) != len(raw) {
		t.Fatalf("包长度字段 %d 与实际缓冲 %d 不一致（是否写了多个包？）", header.Length, len(raw))
	}
	body := raw[dnfproto.ChannelHeaderSize:]
	if len(body) < 3 {
		t.Fatalf("op14 body 太短：%d 字节", len(body))
	}
	listType := body[0]
	count := int(binary.LittleEndian.Uint16(body[1:3]))
	if count <= 0 {
		t.Fatalf("op14 body 行数为 %d", count)
	}
	if want := 3 + count*currentItemListEntryWireSize; len(body) != want {
		t.Fatalf("op14 body 长度 %d，按 %d 行应为 %d", len(body), count, want)
	}
	rows := make([]gmInventoryRow, 0, count)
	for index := 0; index < count; index++ {
		row := body[3+index*currentItemListEntryWireSize:]
		rows = append(rows, gmInventoryRow{
			listType: listType,
			slot:     int16(binary.LittleEndian.Uint16(row[0x00:0x02])),
			itemID:   binary.LittleEndian.Uint32(row[0x02:0x06]),
			amount:   binary.LittleEndian.Uint32(row[0x06:0x0A]),
		})
	}
	return rows
}
