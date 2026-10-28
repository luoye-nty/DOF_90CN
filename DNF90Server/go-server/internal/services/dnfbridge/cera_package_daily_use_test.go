package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// `[usable cera package]` 是另一条独立的消费路径（op518），改动之前它和开箱一样
// 完全不看 `[total usable count]`。夹具 500 在原有礼包声明上多加一行
// `[total usable count] 1`（每天只能开 1 个），其余（奖励行、可选项）与既有夹具一致。
func mustCurrentCeraPackageDailyUseCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "500 `cash/test_package.stk`\n600 `material/test_reward.stk`\n",
		"equipment/equipment.lst": "700 `avatar/test_hat.equ`\n",
		"stackable/cash/test_package.stk": "[stackable type]\n`[usable cera package]`\n[stack limit]\n100\n" +
			"[expiration date]\n`2028-08-16 06:00:00`\n[total usable count]\n1\n" +
			"[package data]\n700 1\n600 3\n[/package data]\n",
		"stackable/material/test_reward.stk": "[stackable type]\n`[material]`\n[stack limit]\n999\n[usable period]\n7\n",
		"equipment/avatar/test_hat.equ":      "[equipment type]\n`[hat avatar]`\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// 礼包开箱必须按礼包**自己的** PVF 上限计数：第一次成功并补一包 op541，第二次被拒、
// 礼包不被消耗、计数不再增长。
func TestCurrentCeraPackageChargesThePackagesOwnPVFDailyLimit(t *testing.T) {
	catalog := mustCurrentCeraPackageDailyUseCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Level: 90, Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	sourceStack := dnfrepo.ItemStack{ItemID: 500, Count: 3}
	sourceEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 75, sourceStack)
	sourceStack.RawEntry = append([]byte(nil), sourceEntry.data[:]...)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       map[string]dnfrepo.ItemStack{"0:75": sourceStack},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	service.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	session := &gameSession{conn: connection, connID: "cera-package-daily-use", selectedCharacterID: 19}
	open := func() {
		t.Helper()
		connection.write.Reset()
		if err := service.handleGameCommand(
			session,
			byte(dnfenum.GameCmdCommand),
			uint16(dnfenum.CmdPacketOpenCerapackage),
			currentCeraPackageTestRequestBody(75, currentCeraPackageChoice{ItemID: 700, Option: 9}),
		); err != nil {
			t.Fatal(err)
		}
	}

	// 第一次：开包成功，源礼包 3 -> 2，计数 1，结果包之后补一包 op541（class 0）。
	open()
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketOpenCerapackage) || !bytes.Equal(ack.Body, []byte{1, 75, 0}) {
		t.Fatalf("op518 ACK header=%+v body=%x", ack.Header, ack.Body)
	}
	_, rest = splitGameServerUpperPacket(t, rest)
	_, rest = splitCurrentSceneItemListPacket(t, rest)
	_, rest = splitCurrentSceneItemListPacket(t, rest)
	state, trailing := splitGameServerUpperPacket(t, rest)
	if state.Header.MsgID != currentItemDailyUseStateMsgID ||
		state.Header.Classification != currentItemDailyUseStateClass {
		t.Fatalf("开礼包后没有补发 op541: header=%+v body=%x trailing=%x", state.Header, state.Body, trailing)
	}
	wantState := []byte{1, 0, 0, 0, 0, 1}
	binary.LittleEndian.PutUint32(wantState[1:5], 500)
	if !bytes.Equal(state.Body, wantState) {
		t.Fatalf("op541 body=%x want=%x", state.Body, wantState)
	}
	if len(trailing) != 0 {
		t.Fatalf("op541 之后不该再有包: %x", trailing)
	}
	character, _, _ := repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 500); got != 1 {
		t.Fatalf("开礼包计数=%d want 1 stats=%+v", got, character.Stats)
	}

	// 第二次：到上限，必须拒绝，且不消耗礼包、计数不变。
	open()
	rejected, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if rejected.Header.MsgID != uint16(dnfenum.CmdPacketOpenCerapackage) ||
		!bytes.Equal(rejected.Body, []byte{0, 4}) {
		t.Fatalf("到上限后仍被放行: header=%+v body=%x", rejected.Header, rejected.Body)
	}
	if len(rest) != 0 {
		t.Fatalf("被拒的开包不该产生后续包: %x", rest)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:75"].Count != 2 {
		t.Fatalf("被拒的开包消耗了礼包: %+v", inventory.Slots)
	}
	character, _, _ = repositories.Character.Load(ctx, "19")
	if got := dnfitemdailylimit.Used(character.Stats, 500); got != 1 {
		t.Fatalf("被拒的开包改动了计数: %d stats=%+v", got, character.Stats)
	}
}
