package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/statbuff"
)

// 城镇增益药剂的"恢复"路径（登录 / 换频道 / 回城 / 重选）必须把两样东西一起补上：
//
//	① class0/op174  效果道具倒计时（{u8 1, u32 itemID, u32 remainingSeconds}）
//	② class0/op487  CHARACTER_ADD_BUFF（buff 栏图标）
//	③ class0/op489  CHARACTER_BUFF_DUNGEON（把它置为激活）
//
// 而**使用**那一发只发 ②③（见 inventory 包的同名用例）：首次使用由客户端按 op44 本地
// 登记 effect item，服务端再发 op174 会让提示变两行。
//
// 服主 2026-10-03 的要求：「重新登录 更换频道 药剂都应该正常生效在使用的角色上
// 直到持续时间耗尽」—— 换频道会新建会话、重跑一次登录/回城路由，所以这条路径必须
// 按角色记录里还没过期的窗口补齐 HUD；过期后则只清残留图标。
func TestSendCurrentStatChangePotionBuffProjectionRestoresCountdownAndIcon(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11",
		AccountID:   "dnf:1",
		Name:        "testx",
		Stats: map[string]int64{
			statbuff.EpicDropRatePercentStatKey: 200,
			statbuff.EpicDropRateExpireStatKey:  now.Add(10 * time.Minute).Unix(),
			statbuff.EpicDropRateItemStatKey:    10008219,
			statbuff.EpicDropRateBuffIDStatKey:  1092,
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "stat-change-potion-restore-test",
		selectedCharacterID: 11,
	}
	if err := service.sendCurrentStatChangePotionBuffProjection(session, "test"); err != nil {
		t.Fatal(err)
	}

	// ① op174：效果道具倒计时
	restore, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) == 0 {
		t.Fatal("the restore path must also send the buff packets after op174")
	}
	if restore.Header.Classification != 0 || restore.Header.MsgID != currentLoadEffectItemInfoMsgID {
		t.Fatalf("restore header=%+v want class0/op%d", restore.Header, currentLoadEffectItemInfoMsgID)
	}
	if len(restore.Body) != 9 || restore.Body[0] != 1 {
		t.Fatalf("restore body=%x want one 9-byte op174 record", restore.Body)
	}
	if got := binary.LittleEndian.Uint32(restore.Body[1:5]); got != 10008219 {
		t.Fatalf("restore item id=%d want 10008219", got)
	}
	if remaining := binary.LittleEndian.Uint32(restore.Body[5:9]); remaining < 598 || remaining > 600 {
		t.Fatalf("restore remaining seconds=%d want 598..600", remaining)
	}

	// ② op487：buff 注册（图标）
	add, rest := splitLongHengGameServerUpperPacket(t, rest)
	if len(rest) == 0 {
		t.Fatal("the add-buff packet must be followed by the activation packet")
	}
	if add.Header.Classification != 0 || add.Header.MsgID != 487 {
		t.Fatalf("add-buff header=%+v want class0/op487", add.Header)
	}
	if len(add.Body) != 17 || add.Body[0] != 1 {
		t.Fatalf("add-buff body=%x want the 17-byte record", add.Body)
	}
	if got := binary.LittleEndian.Uint32(add.Body[1:5]); got != 1092 {
		t.Fatalf("add-buff buff id=%d want 1092 (the rolled tier)", got)
	}

	// ③ op489：激活（列表 = 完整激活集合）
	activate, rest := splitLongHengGameServerUpperPacket(t, rest)
	if len(rest) != 0 {
		t.Fatalf("unexpected trailing bytes: %x", rest)
	}
	if activate.Header.Classification != 0 || activate.Header.MsgID != 489 {
		t.Fatalf("activate header=%+v want class0/op489", activate.Header)
	}
	if len(activate.Body) != 6 || binary.LittleEndian.Uint16(activate.Body[0:2]) != 1 ||
		binary.LittleEndian.Uint32(activate.Body[2:6]) != 1092 {
		t.Fatalf("activate body=%x want u16 count 1 + u32 1092", activate.Body)
	}
	if !session.statChangePotionBuffProjectionSent {
		t.Fatal("restore projection was not marked sent")
	}

	// 每个选中角色只补一次（换频道是新会话，自然会再补一次；同一会话内不重复）。
	written := connection.write.Len()
	if err := service.sendCurrentStatChangePotionBuffProjection(session, "duplicate"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate restore wrote %d additional bytes", connection.write.Len()-written)
	}
}

// 窗口已过期：不补图标，只清残留（清不清得掉取决于 PVF 表，但绝不能报错或补出 buff）。
func TestSendCurrentStatChangePotionBuffProjectionSkipsExpiredWindow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "11",
		AccountID:   "dnf:1",
		Stats: map[string]int64{
			statbuff.EpicDropRatePercentStatKey: 200,
			statbuff.EpicDropRateExpireStatKey:  now.Add(-time.Second).Unix(),
			statbuff.EpicDropRateItemStatKey:    10008219,
			statbuff.EpicDropRateBuffIDStatKey:  1092,
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "stat-change-potion-expired-test",
		selectedCharacterID: 11,
	}
	if err := service.sendCurrentStatChangePotionBuffProjection(session, "test"); err != nil {
		t.Fatal(err)
	}
	for rest := connection.write.Bytes(); len(rest) > 0; {
		packet, next := splitLongHengGameServerUpperPacket(t, rest)
		if packet.Header.MsgID == 487 {
			t.Fatalf("expired window must not re-add a buff: %+v", packet.Header)
		}
		rest = next
	}
}
