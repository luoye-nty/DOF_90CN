package npcshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrOwnerUnavailable  = errors.New("NPC shop owner is unavailable")
	ErrAccountRequired   = errors.New("NPC shop account id is required")
	ErrAccountNotFound   = errors.New("NPC shop account is not found")
	ErrCharacterRequired = errors.New("NPC shop character id is required")
	ErrCharacterNotFound = errors.New("NPC shop character is not found")
	ErrAccountMismatch   = errors.New("NPC shop character account mismatch")
	ErrInventoryNotFound = errors.New("NPC shop inventory is not found")
	ErrProjectorRequired = errors.New("NPC shop mutation projector is required")
	// ErrShopPurchaseTransactionUnavailable 表示仓储没有实现限购所需的
	// ShopPurchaseAssetUnitOfWork 边界；此时不能安全地落账号维度计数。
	ErrShopPurchaseTransactionUnavailable = errors.New("NPC shop purchase transaction is unavailable")
)

type Assets struct {
	// Account 是账号记录本身，限购计数放在它的 Metadata 上（PVF
	// `[purchase limit] `account`` 的已购次数是账号维度状态）。
	Account          *dnfrepo.AccountRecord
	AccountInventory *dnfrepo.AccountInventoryRecord
	Character        *dnfrepo.CharacterRecord
	Inventory        *dnfrepo.InventoryRecord
}

type Changes struct {
	Account          bool
	AccountInventory bool
	Character        bool
	Inventory        bool
}

type Projector func(*Assets) (Changes, error)

type Command struct {
	AccountID   string
	CharacterID string
	UpdatedAt   time.Time
	Project     Projector
}

// Owner owns NPC-shop authorization and the atomic account warehouse,
// character wallet, and character inventory commit. Current-client packet
// rows and PVF projections remain in the bridge.
type Owner struct {
	assets dnfrepo.AccountCharacterAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.AccountAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.AccountAssets}, nil
}

func (o *Owner) Mutate(ctx context.Context, command Command) error {
	if o == nil || o.assets == nil {
		return ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(command.AccountID)
	characterID := strings.TrimSpace(command.CharacterID)
	if accountID == "" {
		return ErrAccountRequired
	}
	if characterID == "" {
		return ErrCharacterRequired
	}
	if command.Project == nil {
		return ErrProjectorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := command.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// 限购计数分两个作用域：角色维度落在角色记录 Stats，账号维度落在账号记录
	// Metadata。两者都必须与钱包/背包变更同事务提交，所以这里要求仓储实现
	// ShopPurchaseAssetUnitOfWork（AccountCharacterAssetUnitOfWork 的扩展）。
	shopAssets, ok := o.assets.(dnfrepo.ShopPurchaseAssetUnitOfWork)
	if !ok {
		return ErrShopPurchaseTransactionUnavailable
	}
	return shopAssets.WithinShopPurchaseAssets(ctx, accountID, characterID, func(
		account dnfrepo.AccountRepository,
		accountInventories dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
	) error {
		if account == nil || accountInventories == nil || characters == nil || inventories == nil {
			return ErrOwnerUnavailable
		}
		// 先校验角色归属（账号不匹配要在读账号记录之前就拒绝），再读账号记录。
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if strings.TrimSpace(character.AccountID) != accountID {
			return fmt.Errorf("%w: account=%s character=%s", ErrAccountMismatch, accountID, characterID)
		}
		accountRecord, found, err := account.Load(ctx, accountID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: account=%s", ErrAccountNotFound, accountID)
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrInventoryNotFound, characterID)
		}
		accountInventory, _, err := accountInventories.Load(ctx, accountID)
		if err != nil {
			return err
		}

		accountRecord = dnfrepo.CloneAccount(accountRecord)
		accountRecord.AccountID = accountID
		if accountRecord.Metadata == nil {
			accountRecord.Metadata = make(map[string]string)
		}
		character = dnfrepo.CloneCharacter(character)
		inventory = dnfrepo.CloneInventory(inventory)
		accountInventory = dnfrepo.CloneAccountInventory(accountInventory)
		accountInventory.AccountID = accountID
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if accountInventory.Slots == nil {
			accountInventory.Slots = make(map[string]dnfrepo.ItemStack)
		}

		changes, err := command.Project(&Assets{
			Account:          &accountRecord,
			AccountInventory: &accountInventory,
			Character:        &character,
			Inventory:        &inventory,
		})
		if err != nil {
			return err
		}
		if changes.Account {
			accountRecord.UpdatedAt = now
			if err := account.Save(ctx, accountRecord); err != nil {
				return err
			}
		}
		if changes.Character {
			character.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		if changes.Inventory {
			inventory.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		if changes.AccountInventory {
			accountInventory.UpdatedAt = now
			if err := accountInventories.Save(ctx, accountInventory); err != nil {
				return err
			}
		}
		return nil
	})
}
