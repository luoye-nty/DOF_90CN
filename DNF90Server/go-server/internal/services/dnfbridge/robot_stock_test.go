package dnfbridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

func TestGMStockRobotCharacterCommitsTradeableRowsForPrivateStore(t *testing.T) {
	service, repositories := newGMRobotStockTestService(t)

	result, err := service.GMStockRobotCharacter(context.Background(), GMRobotStockRequest{
		AccountID:   "robot-account",
		CharacterID: 7,
		Items:       []GMRobotStockItem{{ItemID: 7001, Count: 19}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CharacterID != 7 || len(result.Slots) != 1 || len(result.Items) != 1 {
		t.Fatalf("result = %+v", result)
	}

	rows, err := service.RobotPrivateStoreRows(context.Background(), 7, 14, 2500)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Slot != result.Slots[0] || rows[0].WireValue != 19 || rows[0].Price != 2500 {
		t.Fatalf("rows = %+v", rows)
	}

	inventory, found, err := repositories.Inventory.Load(context.Background(), "7")
	if err != nil || !found {
		t.Fatalf("load inventory: found=%v err=%v", found, err)
	}
	stack, found := inventory.Slots["0:"+int16String(result.Slots[0])]
	if !found || stack.ItemID != 7001 || stack.Count != 19 || stack.Extra["source"] != "robot_gm_stock" || len(stack.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("stock stack = %+v found=%v", stack, found)
	}
}

func TestGMStockRobotCharacterRollsBackWholeBatchWhenOneItemIsNotTradeable(t *testing.T) {
	service, repositories := newGMRobotStockTestService(t)

	_, err := service.GMStockRobotCharacter(context.Background(), GMRobotStockRequest{
		AccountID:   "robot-account",
		CharacterID: 7,
		Items: []GMRobotStockItem{
			{ItemID: 7001, Count: 19},
			{ItemID: 7002, Count: 3},
		},
	})
	if err == nil {
		t.Fatal("expected non-tradeable item rejection")
	}
	inventory, found, loadErr := repositories.Inventory.Load(context.Background(), "7")
	if loadErr != nil || !found {
		t.Fatalf("load inventory: found=%v err=%v", found, loadErr)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("partial robot stock escaped rollback: %+v", inventory.Slots)
	}
}

func TestEnsureRobotPrivateStoreStockFillsMissingRowsOnce(t *testing.T) {
	service, repositories := newGMRobotStockTestService(t)
	if err := service.EnsureRobotPrivateStoreStock(context.Background(), "robot-account", 7, 1); err != nil {
		t.Fatal(err)
	}
	first, found, err := repositories.Inventory.Load(context.Background(), "7")
	if err != nil || !found || len(first.Slots) != 1 {
		t.Fatalf("first automatic stock: found=%v err=%v inventory=%+v", found, err, first)
	}
	if err := service.EnsureRobotPrivateStoreStock(context.Background(), "robot-account", 7, 1); err != nil {
		t.Fatal(err)
	}
	second, found, err := repositories.Inventory.Load(context.Background(), "7")
	if err != nil || !found || len(second.Slots) != 1 {
		t.Fatalf("second automatic stock duplicated rows: found=%v err=%v inventory=%+v", found, err, second)
	}
	rows, err := service.RobotPrivateStoreRows(context.Background(), 7, 1, 999)
	if err != nil || len(rows) != 1 || rows[0].Price != 999 || rows[0].WireValue == 0 {
		t.Fatalf("automatic private-store rows = %+v err=%v", rows, err)
	}
}

func TestConfiguredRobotPrivateStoreUsesExactQuantityAndPerRowPrice(t *testing.T) {
	service, repositories := newGMRobotStockTestService(t)
	items := []dnfrobotdomain.PrivateStoreItem{{ItemID: 7001, Count: 50, Price: 1234}}
	rows, err := service.PrepareRobotPrivateStoreRows(context.Background(), "robot-account", 7, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Price != 1234 || rows[0].WireValue != 50 {
		t.Fatalf("configured rows = %+v", rows)
	}
	rows, err = service.PrepareRobotPrivateStoreRows(context.Background(), "robot-account", 7, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Price != 1234 || rows[0].WireValue != 50 {
		t.Fatalf("replenished configured rows = %+v", rows)
	}
	inventory, found, err := repositories.Inventory.Load(context.Background(), "7")
	if err != nil || !found || len(inventory.Slots) != 1 {
		t.Fatalf("configured stock inventory found=%t err=%v rows=%+v", found, err, inventory.Slots)
	}
	account, found, err := repositories.Account.Load(context.Background(), "robot-account")
	if err != nil || !found || !premium.Active(account, premium.TypePrivateStore, time.Now().UTC()) {
		t.Fatalf("private-store license active=%t found=%t err=%v", premium.Active(account, premium.TypePrivateStore, time.Now().UTC()), found, err)
	}
}

func TestConfiguredRobotPrivateStoreAllowsItemWithoutDeclaredStackLimit(t *testing.T) {
	service, _ := newGMRobotStockTestService(t)
	rows, err := service.PrepareRobotPrivateStoreRows(context.Background(), "robot-account", 7, []dnfrobotdomain.PrivateStoreItem{
		{ItemID: 3037, Count: 10_000, Price: 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].WireValue != 10_000 || rows[0].Price != 500 {
		t.Fatalf("undeclared-stack-limit rows = %+v", rows)
	}
}

func newGMRobotStockTestService(t *testing.T) (*Service, dnfrepo.Group) {
	t.Helper()
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":     "",
		"stackable/stackable.lst": "7001 `material/trade.stk`\n7002 `material/account.stk`\n3037 `material/cube.stk`\n",
		"equipment/equipment.lst": "",
		// 交易白名单是 {free, sealing}，夹具里「可交易」用 `[free]` 表达；
		// account.stk 仍是 `[account]`，继续覆盖「不可交易不可补货」那一支。
		"stackable/material/trade.stk":   "[name] `Trade Material`\n[stackable type] `[material]`\n[stack limit] 999\n[attach type] `[free]`\n",
		"stackable/material/account.stk": "[name] `Account Material`\n[stackable type] `[material]`\n[stack limit] 999\n[attach type] `[account]`\n",
		"stackable/material/cube.stk":    "[name] `Undeclared Stack Limit`\n[stackable type] `[material]`\n[attach type] `[free]`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "7", AccountID: "robot-account", Name: "Robot"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "robot-account", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "7", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	return &Service{
		pvfItemCatalog:     catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}, repositories
}

func int16String(value int16) string {
	return fmt.Sprintf("%d", value)
}
