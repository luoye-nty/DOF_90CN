package dnfbridge

import (
	"bytes"
	"testing"
)

// 城镇增益药剂（史诗Buff药剂一族）的 buff 注册包 —— 图标就是靠它出现的。
//
// 协议号在**本客户端**里定标（2026-10-03 第四轮，服主提醒"A21 与我们客户端不同，
// 不能照抄"）：客户端启动时把 294 个 ENUM_NOTIPACKET_* 名字按枚举顺序注册进
// 0x52bc170 + 4*i（tools/_gb_noti_table_re.py），id = base + slot，而 base = 485 —— 用
// 本仓库现场验证过的号反查得到：
//
//	slot 98..102 = RAID_SET_SYMBOL..RAID_STATE ↔ raid/body.go 0x247..0x24b (583..587)
//	slot 95      = SEND_DECK_INFO             ↔ guardian shield deck 通知 580
//
// ⇒ ADD_BUFF = 487、DEL_BUFF = 488、BUFF_DUNGEON = 489（旧代码用的 488/489/490 是
// A21 的 486 基址，在本客户端分别落在 DEL_BUFF / BUFF_DUNGEON / 疲劳值加速状态上，
// 等于 ADD_BUFF 从未发出去过）。
//
// 现场 2026-10-03：只发 class0/op174（效果道具倒计时列表）时，客户端只弹
// "[史诗Buff药剂]的效果时间还剩10分钟"，buff 栏仍为空 —— 两套东西，必须都发。
func TestCharacterAddBuffBodyMatchesTheReferenceLayout(t *testing.T) {
	add := buildCurrentCharacterAddBuffBody(1136)
	want := []byte{1, 0x70, 0x04, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(add, want) {
		t.Fatalf("ADD_BUFF body=%x want=%x", add, want)
	}
	if len(add) != 17 {
		t.Fatalf("ADD_BUFF body_len=%d want 17 (u8 count + i32 buffId + 3*i32)", len(add))
	}

	remove := buildCurrentCharacterRemoveBuffBody([]int32{1240, 0, 1239})
	wantRemove := []byte{2, 0xD8, 0x04, 0x00, 0x00, 0xD7, 0x04, 0x00, 0x00}
	if !bytes.Equal(remove, wantRemove) {
		t.Fatalf("REMOVE_BUFF body=%x want=%x（0 与非正 id 必须被跳过，count 只数有效行）", remove, wantRemove)
	}
	if body := buildCurrentCharacterRemoveBuffBody(nil); body != nil {
		t.Fatalf("empty remove body=%x want nil", body)
	}
}

// 大瓶/小瓶/赛丽亚三档的 buff id（PVF 表查出来的真实值）也必须编码正确。
func TestCharacterAddBuffBodyCoversEveryFamilyBuffID(t *testing.T) {
	for _, tc := range []struct {
		buffID int32
		want   []byte
	}{
		{buffID: 1136, want: []byte{0x70, 0x04, 0x00, 0x00}},
		{buffID: 1239, want: []byte{0xD7, 0x04, 0x00, 0x00}},
		{buffID: 1240, want: []byte{0xD8, 0x04, 0x00, 0x00}},
		{buffID: 1090, want: []byte{0x42, 0x04, 0x00, 0x00}},
		{buffID: 1092, want: []byte{0x44, 0x04, 0x00, 0x00}},
	} {
		body := buildCurrentCharacterAddBuffBody(tc.buffID)
		if body[0] != 1 || !bytes.Equal(body[1:5], tc.want) {
			t.Fatalf("buff %d body=%x want buff id bytes %x", tc.buffID, body, tc.want)
		}
		for index := 5; index < len(body); index++ {
			if body[index] != 0 {
				t.Fatalf("buff %d body=%x: 三个状态字段必须为 0", tc.buffID, body)
			}
		}
	}
}

// CHARACTER_BUFF_DUNGEON（class0/op489）是"激活"那一发：u16 count + u32 buffId 列表。
// 参考实现注释：ADD_BUFF 只创建/更新条目，BUFF_DUNGEON 才 toggles existing entries active，
// 且列表必须是完整激活集合（客户端先清空本地标志再按列表置位）。
func TestCharacterBuffDungeonBodyLayout(t *testing.T) {
	active := buildCurrentCharacterBuffDungeonBody([]int32{1136})
	want := []byte{1, 0x00, 0x70, 0x04, 0x00, 0x00}
	if !bytes.Equal(active, want) {
		t.Fatalf("activate body=%x want=%x", active, want)
	}
	empty := buildCurrentCharacterBuffDungeonBody(nil)
	if !bytes.Equal(empty, []byte{0, 0}) {
		t.Fatalf("empty activate body=%x want 0000（清空激活标志）", empty)
	}
	multi := buildCurrentCharacterBuffDungeonBody([]int32{1090, 0, 1092})
	if !bytes.Equal(multi, []byte{2, 0x00, 0x42, 0x04, 0x00, 0x00, 0x44, 0x04, 0x00, 0x00}) {
		t.Fatalf("multi activate body=%x（非正 id 必须跳过，count 只数有效行）", multi)
	}
}

// TestCharacterBuffNotificationMsgIDsArePinnedOnThisClient 用**字面量**钉住三个通知号。
//
// 这里刻意不写成 `if currentCharacterAddBuffMsgID != someConst`：常量对常量是恒真的
// 空断言。本客户端（90CN，DNF.exe）的 NOTIPACKET 基址是 485，不是参考服务端 A21 的
// 486 —— 照抄 A21 会把 ADD_BUFF 发成 DEL_BUFF（数值 488），图标永远不会出现。
//
// 定标证据（全部是本仓库现场验证过的号，见 stat_change_potion_projection.go 顶部注释）：
//
//	slot  98 RAID_SET_SYMBOL   ↔ 583    slot 102 RAID_STATE           ↔ 587
//	slot  95 SEND_DECK_INFO    ↔ 580（守护者盾牌牌组通知）
//	⇒ base = 485 ⇒ slot 2 CHARACTER_ADD_BUFF = 487
func TestCharacterBuffNotificationMsgIDsArePinnedOnThisClient(t *testing.T) {
	if currentCharacterAddBuffMsgID != 487 {
		t.Fatalf("CHARACTER_ADD_BUFF msg_id=%d want 487", currentCharacterAddBuffMsgID)
	}
	if currentCharacterRemoveBuffMsgID != 488 {
		t.Fatalf("CHARACTER_DEL_BUFF msg_id=%d want 488", currentCharacterRemoveBuffMsgID)
	}
	if currentCharacterBuffDungeonMsgID != 489 {
		t.Fatalf("CHARACTER_BUFF_DUNGEON msg_id=%d want 489", currentCharacterBuffDungeonMsgID)
	}
}
