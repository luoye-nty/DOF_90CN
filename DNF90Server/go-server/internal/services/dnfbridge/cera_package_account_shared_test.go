package dnfbridge

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 礼包（赛丽亚商店开箱）的奖励里同样会有无色小晶块 / 灵魂，必须落账号共享仓。
//
// 这条路的事务回调一直拿得到账号仓记录，却用 `_ dnfrepo.AccountInventoryRepository`
// 丢掉了 —— 与翻牌那次同一形态。统一入口见 account_shared_grant.go。

// cubePackageTestCatalog 造一个"开箱即得 20 个无色小晶块"的礼包。
//
// ⚠️ 无色小晶块的 .stk **故意不声明 `[stack limit]`**，与线上 Script.pvf 一致
// （2026-09-21 用 pvfcat 核对：12 个道具全部没有 [stack limit]）。这不是细节：
// 若给个 999，而账号格已经有 43890，唯一入库原语会按"容器内同 id 已超上限"正确拒绝，
// 整份奖励退回角色背包 —— 那测的就不是本路径了。
func cubePackageTestCatalog(t *testing.T) *pvfDungeonDropCatalog {
	t.Helper()
	source := dungeonDropCatalogTestSource{
		"monster/monster.lst":                    "",
		"stackable/stackable.lst":                "500 `cash/cube_package.stk`\n3037 `material/cubepiece_clear.stk`\n",
		"equipment/equipment.lst":                "",
		"stackable/cash/cube_package.stk":        "[stackable type]\n`[usable cera package]`\n[stack limit]\n100\n[expiration date]\n`2028-08-16 06:00:00`\n[package data]\n3037 20\n[/package data]\n",
		"stackable/material/cubepiece_clear.stk": "[stackable type]\n`[material]` 2\n",
	}
	catalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func openCubePackageForTest(t *testing.T, catalog *pvfDungeonDropCatalog) dnfrepo.Group {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	// 现场形态：账号晶体仓里已经堆着 43890 个无色。
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatal(err)
	}
	sourceStack := dnfrepo.ItemStack{ItemID: 500, Count: 2}
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
	session := &gameSession{conn: connection, connID: "cera-package-cube-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketOpenCerapackage),
		currentCeraPackageTestRequestBody(75),
	); err != nil {
		t.Fatal(err)
	}
	return repositories
}

// 开箱得到无色小晶块 → 并进账号晶体仓 0:358，背包里一个都不许有；礼包本身照旧被消耗。
//
// ⚠️ 注意这个用例必须断言**溯源标记**，不能只断言数量：
// 开箱成功后 handleCurrentCeraPackageOpen 会重建 list-0，而重建里跑着 P4 的收敛兜底
// （inventory.MigrateAccountSharedSlots）—— 就算这里没接统一入口，兜底也会把这 20 个
// 从背包搬进账号格，数量对得上、背包也是空的。两种情况唯一的区别是**账号行由谁写**：
//
//	· 接了统一入口 → 礼包路径自己写账号行，Extra 带 source=cera_package
//	· 只靠兜底     → 账号行是原有的那一行被合并，Extra 保持原样
//
// 所以下面同时断言 Extra，这个用例才真的钉住了本路径的修复。
func TestCurrentCeraPackageRoutesCubeRewardIntoAccountCell(t *testing.T) {
	ctx := context.Background()
	repositories := openCubePackageForTest(t, cubePackageTestCatalog(t))

	account, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	stack, exists := account.Slots["0:358"]
	if !exists || stack.ItemID != 3037 || stack.Count != 43910 {
		t.Fatalf("account crystal cell=%+v want 3037x43910 (43890 + the 20 from the package)", stack)
	}
	if stack.Extra["source"] != "cera_package" || stack.Extra["last_grant_source"] != "cera_package" {
		t.Fatalf("account row provenance=%+v want source=cera_package: the package path must write the row itself, not leave it to the login sweep", stack.Extra)
	}

	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == 3037 {
			t.Fatalf("the cube reward must not sit in the backpack: %s=%+v", key, stack)
		}
	}
	if source := inventory.Slots["0:75"]; source.ItemID != 500 || source.Count != 1 {
		t.Fatalf("package source after=%+v want 500x1 (one of the two was consumed)", source)
	}
}

// 行为保持：普通奖励（600）照旧落角色背包，账号仓不因这次开箱新增行。
func TestCurrentCeraPackageLeavesOrdinaryRewardsInTheBag(t *testing.T) {
	ctx := context.Background()
	catalog := mustCurrentCeraPackageTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "account-1", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	sourceStack := dnfrepo.ItemStack{ItemID: 500, Count: 1}
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
	session := &gameSession{conn: connection, connID: "cera-package-ordinary-test", selectedCharacterID: 19}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketOpenCerapackage),
		currentCeraPackageTestRequestBody(75, currentCeraPackageChoice{ItemID: 700, Option: 9}),
	); err != nil {
		t.Fatal(err)
	}

	inventory, _, err := repositories.Inventory.Load(ctx, "19")
	if err != nil {
		t.Fatal(err)
	}
	if got := inventory.Slots["0:121"]; got.ItemID != 600 || got.Count != 3 {
		t.Fatalf("ordinary reward must stay in the backpack: %+v", got)
	}
	account, _, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(account.Slots) != 0 {
		t.Fatalf("an ordinary package must not add account rows: %+v", account.Slots)
	}
}
