package dnfbridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func newAccountMaterialCompletionTestRuntime(t *testing.T) (*Service, *gameSession, dnfrepo.Group, *bufferConn) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	account := dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  make(map[string]string),
	}
	premium.Upsert(&account, premium.TypeCrystal, int64(time.Hour/time.Second), 1, now)
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   account.AccountID,
		Stats:       make(map[string]int64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       make(map[string]dnfrepo.ItemStack),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "19",
		Entries:     make(map[string]dnfrepo.EquipmentEntry),
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          account.AccountID,
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	return service, &gameSession{
		conn:                connection,
		connID:              "account-material-completion-test",
		selectedCharacterID: 19,
	}, repositories, connection
}

func TestPostActionDeleteItemAccountMaterialCompletionNeverSendsClass0Op18(t *testing.T) {
	service, session, _, connection := newAccountMaterialCompletionTestRuntime(t)
	action := alignedcmd.PostActionSendDeleteItemAccountMaterialCompletion

	// 城镇内：post action no-op，不发包。
	if err := service.sendAlignedPostAction(session, "delete_item", action); err != nil {
		t.Fatalf("town post action error = %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("town sent bytes: %x", connection.write.Bytes())
	}

	// 活跃副本内：同样不发。
	//
	// class-0/op18 在本客户端的处理器是 sub_1D61130 —— 交易完成处理器。
	// 空体 {00 00} 会被读成「0 条结果的交易完成」，于是释放消耗无色小晶块这类
	// 账号共享材料的技能时会弹出「物品交易已完成」的误提示。
	// 删除完成由 class-1/op18 的 DELETE_ITEM RETURN 加权威 op14 数量行负责。
	connection.write.Reset()
	session.dungeon.runtime = &runtimeDungeonState{
		Character: dnfrepo.CharacterRecord{CharacterID: "19"},
	}
	if err := service.sendAlignedPostAction(session, "delete_item", action); err != nil {
		t.Fatalf("dungeon post action error = %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("dungeon sent bytes: %x (class-0/op18 would be read as a trade finish)",
			connection.write.Bytes())
	}
}

func TestPostActionDeleteItemCompletionBodyIsLEU16Zero(t *testing.T) {
	if got := buildDeleteItemCompletionBody(); !bytes.Equal(got, []byte{0x00, 0x00}) {
		t.Fatalf("buildDeleteItemCompletionBody() = %x, want {00 00}", got)
	}
}
