package memory

import (
	"context"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/repository"
)

// 商城/冒险团商店这几条路会卖出账号共享道具（无色小晶块、灵魂，落 list 0 的
// 354..365 固定格，由**账号记录**持有），所以账号仓必须和扣点在同一个事务里：
//
//	· 事务里交出来的账号仓必须真的能读写；
//	· 提交后落库；
//	· 回调失败时整批（含账号仓）一起回滚 —— 绝不能扣了点却没发货，或反过来。
func TestCeraShopAssetTransactionCarriesAccountInventory(t *testing.T) {
	ctx := context.Background()
	repositories := NewMemoryGroup()
	if err := repositories.Account.Save(ctx, repository.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "1000"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "22",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "22"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, repository.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]repository.ItemStack{"0:365": {ItemID: 10158124, Count: 3}},
	}); err != nil {
		t.Fatal(err)
	}

	committed := 0
	err := repositories.CeraShopAssets.WithinCeraShopAssets(
		ctx, "account-1", "22", repository.CharacterContainerStateScope("22"),
		func(
			accounts repository.AccountRepository,
			accountInventories repository.AccountInventoryRepository,
			_ repository.CharacterRepository,
			_ repository.InventoryRepository,
			_ repository.EquipmentRepository,
			_ repository.SettingsRepository,
		) error {
			account, found, err := accounts.Load(ctx, "account-1")
			if err != nil || !found {
				t.Fatalf("load account found=%t err=%v", found, err)
			}
			account.Metadata["account_cera"] = "900"
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			inventory, found, err := accountInventories.Load(ctx, "account-1")
			if err != nil || !found {
				t.Fatalf("load account inventory found=%t err=%v", found, err)
			}
			inventory.Slots["0:358"] = repository.ItemStack{ItemID: 3037, Count: 43890}
			if err := accountInventories.Save(ctx, inventory); err != nil {
				return err
			}
			committed++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 1 {
		t.Fatalf("callback ran %d times", committed)
	}
	account, _, _ := repositories.Account.Load(ctx, "account-1")
	if account.Metadata["account_cera"] != "900" {
		t.Fatalf("account cera was not committed: %+v", account.Metadata)
	}
	stored, found, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account inventory found=%t err=%v", found, err)
	}
	if crystal, exists := stored.Slots["0:358"]; !exists || crystal.ItemID != 3037 || crystal.Count != 43890 {
		t.Fatalf("account warehouse write was not committed: %+v", stored.Slots)
	}
}

func TestCeraShopAssetTransactionRollsBackAccountInventoryOnFailure(t *testing.T) {
	ctx := context.Background()
	repositories := NewMemoryGroup()
	if err := repositories.Account.Save(ctx, repository.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "1000"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, repository.CharacterRecord{
		CharacterID: "22",
		AccountID:   "account-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: "22"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, repository.AccountInventoryRecord{
		AccountID: "account-1",
		Slots:     map[string]repository.ItemStack{"0:358": {ItemID: 3037, Count: 43890}},
	}); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("checkout failed after the credit")
	err := repositories.CeraShopAssets.WithinCeraShopAssets(
		ctx, "account-1", "22", repository.CharacterContainerStateScope("22"),
		func(
			accounts repository.AccountRepository,
			accountInventories repository.AccountInventoryRepository,
			_ repository.CharacterRepository,
			_ repository.InventoryRepository,
			_ repository.EquipmentRepository,
			_ repository.SettingsRepository,
		) error {
			account, _, err := accounts.Load(ctx, "account-1")
			if err != nil {
				return err
			}
			account.Metadata["account_cera"] = "0"
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			inventory, _, err := accountInventories.Load(ctx, "account-1")
			if err != nil {
				return err
			}
			inventory.Slots["0:358"] = repository.ItemStack{ItemID: 3037, Count: 43910}
			if err := accountInventories.Save(ctx, inventory); err != nil {
				return err
			}
			return sentinel
		},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction error=%v want the callback error", err)
	}
	account, _, _ := repositories.Account.Load(ctx, "account-1")
	if account.Metadata["account_cera"] != "1000" {
		t.Fatalf("failed checkout kept the debit: %+v", account.Metadata)
	}
	stored, _, err := repositories.AccountInventory.Load(ctx, "account-1")
	if err != nil {
		t.Fatal(err)
	}
	if crystal := stored.Slots["0:358"]; crystal.Count != 43890 {
		t.Fatalf("failed checkout kept the account warehouse grant: %+v", crystal)
	}
}
