package cerashop

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	accountCeraMetadataKey           = "account_cera"
	accountTokenCeraMetadataKey      = "account_token_cera"
	accountHappyTokenCeraMetadataKey = "account_happy_token_cera"
)

var (
	ErrOwnerUnavailable  = errors.New("cera shop owner is unavailable")
	ErrAccountNotFound   = errors.New("cera shop account is not found")
	ErrCharacterNotFound = errors.New("cera shop character is not found")
	ErrInventoryNotFound = errors.New("cera shop inventory is not found")
	ErrAccountMismatch   = errors.New("cera shop character account mismatch")
	ErrCeraInsufficient  = errors.New("cera shop cera balance is insufficient")
	ErrProjectorRequired = errors.New("cera shop checkout projector is required")
)

type CheckoutChanges struct {
	Character bool
	Inventory bool
	Equipment bool
	Settings  bool
}

type CheckoutAssets struct {
	Account        *dnfrepo.AccountRecord
	Character      *dnfrepo.CharacterRecord
	Inventory      *dnfrepo.InventoryRecord
	Equipment      *dnfrepo.EquipmentRecord
	Settings       *dnfrepo.SettingsRecord
	EquipmentFound bool
	SettingsFound  bool
}

type CheckoutProjector func(*CheckoutAssets) (CheckoutChanges, error)

type CheckoutCommand struct {
	AccountID     string
	CharacterID   string
	SettingsScope string
	Cost          int64
	UpdatedAt     time.Time
	// PayPolicy 决定这笔钱从哪些钱包扣。
	//
	// **零值 = PayPolicyCeraOnly = 只扣点券 = 改动前的行为**，所以既有调用方一行都
	// 不用改，也不会因为漏填这个字段而悄悄换一种扣费方式。
	PayPolicy PayPolicy
	Project   CheckoutProjector
}

type CheckoutResult struct {
	CeraBefore     int64
	CeraAfter      int64
	TokenCera      int64
	HappyTokenCera int64
	// Paid 是本次结账各钱包实际扣掉的数量。
	// 用于日志核对，也是"代币券真的能花掉"这条验收的凭据。
	Paid WalletPayment
}

// Wallet is the complete account-owned Cera projection consumed by the
// current client's class0/op53 update packet.
type Wallet struct {
	Cera           int64
	TokenCera      int64
	HappyTokenCera int64
}

type Owner struct {
	assets dnfrepo.CeraShopAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CeraShopAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.CeraShopAssets}, nil
}

func (o *Owner) Checkout(ctx context.Context, command CheckoutCommand) (CheckoutResult, error) {
	accountID := strings.TrimSpace(command.AccountID)
	characterID := strings.TrimSpace(command.CharacterID)
	settingsScope := strings.TrimSpace(command.SettingsScope)
	if o == nil || o.assets == nil || accountID == "" || characterID == "" || settingsScope == "" || command.Cost < 0 {
		return CheckoutResult{}, ErrOwnerUnavailable
	}
	if command.Project == nil {
		return CheckoutResult{}, ErrProjectorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CheckoutResult{}, err
	}
	now := command.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var result CheckoutResult
	err := o.assets.WithinCeraShopAssets(
		ctx,
		accountID,
		characterID,
		settingsScope,
		func(
			accounts dnfrepo.AccountRepository,
			_ dnfrepo.AccountInventoryRepository,
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			equipment dnfrepo.EquipmentRepository,
			settings dnfrepo.SettingsRepository,
		) error {
			if accounts == nil || characters == nil || inventories == nil || equipment == nil || settings == nil {
				return ErrOwnerUnavailable
			}
			account, found, err := accounts.Load(ctx, accountID)
			if err != nil {
				return err
			}
			if !found {
				return ErrAccountNotFound
			}
			character, found, err := characters.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found {
				return ErrCharacterNotFound
			}
			if strings.TrimSpace(character.AccountID) != accountID {
				return ErrAccountMismatch
			}
			inventory, found, err := inventories.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
				return ErrInventoryNotFound
			}
			equipmentRecord, equipmentFound, err := equipment.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if !equipmentFound {
				equipmentRecord = dnfrepo.EquipmentRecord{CharacterID: characterID}
			}
			settingsRecord, settingsFound, err := settings.Load(ctx, settingsScope)
			if err != nil {
				return err
			}
			if !settingsFound {
				settingsRecord = dnfrepo.SettingsRecord{Scope: settingsScope}
			}

			account = dnfrepo.CloneAccount(account)
			character = dnfrepo.CloneCharacter(character)
			inventory = dnfrepo.CloneInventory(inventory)
			equipmentRecord = dnfrepo.CloneEquipment(equipmentRecord)
			settingsRecord = dnfrepo.CloneSettings(settingsRecord)

			ceraBefore := Balance(account)
			// 按策略算扣费方案：默认（零值）就是"只扣点券"，与改动前完全一致。
			// 瀑布策略下代币券 / 欢乐代币券会先被花掉，点券只补差额。
			payment, planErr := PlanWalletPayment(WalletBalance(account), command.Cost, command.PayPolicy)
			if planErr != nil {
				if errors.Is(planErr, ErrWalletInsufficient) && command.PayPolicy == PayPolicyCeraOnly {
					// 保持既有错误口径：只用点券时仍然报 ErrCeraInsufficient，
					// 免得改了错误类型却没人更新调用方的判定。
					return fmt.Errorf("%w: balance=%d cost=%d", ErrCeraInsufficient, ceraBefore, command.Cost)
				}
				return planErr
			}
			ceraAfter := ceraBefore - payment.FromCera
			ApplyWalletPayment(&account, payment)

			assets := &CheckoutAssets{
				Account:        &account,
				Character:      &character,
				Inventory:      &inventory,
				Equipment:      &equipmentRecord,
				Settings:       &settingsRecord,
				EquipmentFound: equipmentFound,
				SettingsFound:  settingsFound,
			}
			changes, err := command.Project(assets)
			if err != nil {
				return err
			}

			account.UpdatedAt = now
			if err := accounts.Save(ctx, account); err != nil {
				return err
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
			if changes.Equipment {
				equipmentRecord.UpdatedAt = now
				if err := dnfrepo.SaveEquipmentFields(ctx, equipment, equipmentRecord, dnfrepo.EquipmentFieldEntries); err != nil {
					return err
				}
			}
			if changes.Settings {
				settingsRecord.UpdatedAt = now
				if err := dnfrepo.SaveSettingsFields(ctx, settings, settingsRecord, dnfrepo.SettingsFieldValues); err != nil {
					return err
				}
			}
			wallet := WalletBalance(account)
			result = CheckoutResult{
				CeraBefore:     ceraBefore,
				CeraAfter:      ceraAfter,
				TokenCera:      wallet.TokenCera,
				HappyTokenCera: wallet.HappyTokenCera,
				Paid:           payment,
			}
			return nil
		},
	)
	if err != nil {
		return CheckoutResult{}, err
	}
	return result, nil
}

func Balance(account dnfrepo.AccountRecord) int64 {
	return metadataBalance(account, accountCeraMetadataKey)
}

func TokenCeraBalance(account dnfrepo.AccountRecord) int64 {
	return metadataBalance(account, accountTokenCeraMetadataKey)
}

func HappyTokenCeraBalance(account dnfrepo.AccountRecord) int64 {
	return metadataBalance(account, accountHappyTokenCeraMetadataKey)
}

func WalletBalance(account dnfrepo.AccountRecord) Wallet {
	return Wallet{
		Cera:           Balance(account),
		TokenCera:      TokenCeraBalance(account),
		HappyTokenCera: HappyTokenCeraBalance(account),
	}
}

func metadataBalance(account dnfrepo.AccountRecord, key string) int64 {
	raw := strings.TrimSpace(account.Metadata[key])
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func SetBalance(account *dnfrepo.AccountRecord, balance int64) {
	setMetadataBalance(account, accountCeraMetadataKey, balance)
}

func SetTokenCeraBalance(account *dnfrepo.AccountRecord, balance int64) {
	setMetadataBalance(account, accountTokenCeraMetadataKey, balance)
}

func SetHappyTokenCeraBalance(account *dnfrepo.AccountRecord, balance int64) {
	setMetadataBalance(account, accountHappyTokenCeraMetadataKey, balance)
}

func setMetadataBalance(account *dnfrepo.AccountRecord, key string, balance int64) {
	if account == nil {
		return
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string, 4)
	}
	if balance < 0 {
		balance = 0
	}
	account.Metadata[key] = strconv.FormatInt(balance, 10)
}
