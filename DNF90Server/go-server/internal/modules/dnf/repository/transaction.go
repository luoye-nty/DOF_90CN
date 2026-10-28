// 本文件定义 DNF 仓储事务边界接口。
package repository

import (
	"context"
	"errors"
	"strings"

	"longheng.io/server/internal/platform/db"
)

var (
	ErrCharacterCreationTransactionUnavailable     = errors.New("character creation transaction is unavailable")
	ErrCharacterItemTransactionUnavailable         = errors.New("character item transaction is unavailable")
	ErrCharacterTradeTransactionUnavailable        = errors.New("character trade transaction is unavailable")
	ErrAccountCharacterTradeTransactionUnavailable = errors.New("account and character trade transaction is unavailable")
	ErrCharacterPetTransactionUnavailable          = errors.New("character pet transaction is unavailable")
	ErrAccountCharacterItemTransactionUnavailable  = errors.New("account and character item transaction is unavailable")
	ErrCharacterAssetTransactionUnavailable        = errors.New("character asset transaction is unavailable")
	ErrAccountCharacterAssetTransactionUnavailable = errors.New("account inventory and character asset transaction is unavailable")
	ErrShopPurchaseAssetTransactionUnavailable     = errors.New("shop purchase asset transaction is unavailable")
	ErrBoosterAssetTransactionUnavailable          = errors.New("booster asset transaction is unavailable")
	ErrRentalAssetTransactionUnavailable           = errors.New("rental asset transaction is unavailable")
	ErrCeraShopAssetTransactionUnavailable         = errors.New("cera shop asset transaction is unavailable")
	ErrCharacterSkillTransactionUnavailable        = errors.New("character skill transaction is unavailable")
	ErrCharacterProgressionTransactionUnavailable  = errors.New("character progression transaction is unavailable")
	ErrCharacterSettlementTransactionUnavailable   = errors.New("character settlement transaction is unavailable")
	ErrMailboxAssetTransactionUnavailable          = errors.New("mailbox asset transaction is unavailable")
	ErrEpicBookCraftTransactionUnavailable         = errors.New("epic book craft transaction is unavailable")
	ErrRaidEntryCostTransactionUnavailable         = errors.New("raid entry-cost transaction is unavailable")
	ErrRaidRewardTransactionUnavailable            = errors.New("raid reward transaction is unavailable")
)

// CharacterCreationUnitOfWork atomically persists a character aggregate. It
// is used by creation and by missing-only initialization repair. The base
// group is supplied explicitly so test/local repository overrides participate
// in the same transaction boundary.
type CharacterCreationUnitOfWork interface {
	WithinCharacterCreation(context.Context, string, Group, func(Group) error) error
}

// RaidEntryCostUnitOfWork supplies one transaction-scoped inventory owner for
// the complete frozen raid roster. Implementations must reject access outside
// the provided character IDs and commit every member debit atomically.
type RaidEntryCostUnitOfWork interface {
	WithinRaidEntryCost(context.Context, []string, func(InventoryRepository) error) error
}

// RaidRewardUnitOfWork atomically grants every frozen member inventory reward
// and writes the idempotency receipt that makes retries read-only.
type RaidRewardUnitOfWork interface {
	WithinRaidRewards(context.Context, []string, string, func(InventoryRepository, SettingsRepository) error) error
}

// RaidRewardAssetUnitOfWork is the account-aware raid settlement boundary.
// Token-Cera vouchers are account wallet credits rather than inventory rows,
// so they must commit in the same transaction as ordinary item rewards and the
// idempotency receipt. Implementations scope the account repository to the
// supplied account IDs.
type RaidRewardAssetUnitOfWork interface {
	WithinRaidRewardAssets(
		context.Context,
		[]string,
		[]string,
		string,
		func(AccountRepository, InventoryRepository, SettingsRepository) error,
	) error
}

// CharacterItemUnitOfWork supplies transaction-scoped inventory and equipment
// repositories for one character. The callback owns domain decisions; the
// repository only owns atomic commit and rollback.
type CharacterItemUnitOfWork interface {
	WithinCharacterItems(context.Context, string, func(InventoryRepository, EquipmentRepository) error) error
}

// CharacterItemScopedUnitOfWork is an optional latency-sensitive persistence
// capability. Backends that implement it durably commit only the records
// owned by CharacterItemUnitOfWork; callers must fall back to the ordinary
// interface when it is unavailable.
type CharacterItemScopedUnitOfWork interface {
	WithinCharacterItemsScoped(context.Context, string, func(InventoryRepository, EquipmentRepository) error) error
}

// CharacterTradeUnitOfWork atomically moves wallet and inventory assets
// between two distinct characters. Both transaction-scoped repositories
// accept only the two supplied character IDs; callers must load them in stable
// ID order before saving so concurrent trades acquire row locks consistently.
type CharacterTradeUnitOfWork interface {
	WithinCharacterTrade(context.Context, string, string, func(CharacterRepository, InventoryRepository) error) error
}

// AccountCharacterTradeUnitOfWork extends a two-character trade with every
// account-owned asset used by same-account transfers. Expert-job stores use
// the account/character/inventory subset; equipment and creature transport
// additionally use Epic Book fragments and durable pet state. Every callback
// receives repositories scoped to the supplied account and two character IDs.
type AccountCharacterTradeUnitOfWork interface {
	WithinAccountCharacterTrade(context.Context, string, string, string, func(AccountInventoryRepository, CharacterRepository, InventoryRepository, EpicBookRepository, PetRepository) error) error
}

// AccountPairCharacterTradeUnitOfWork is the boundary for a transfer that
// moves an account-owned item between two characters on *different* accounts.
// The crystal/soul warehouse is account storage on both sides, so the debit and
// the credit touch two account records at once; AccountCharacterTradeUnitOfWork
// scopes a single account and cannot express that. The callback receives
// repositories scoped to the two accounts and the two characters.
type AccountPairCharacterTradeUnitOfWork interface {
	WithinAccountPairCharacterTrade(context.Context, string, string, string, string, func(AccountInventoryRepository, CharacterRepository, InventoryRepository) error) error
}

// CharacterPetUnitOfWork supplies one character-scoped transaction for the
// inventory pet container, worn creature slots, and durable creature state.
// Pet owners must use this boundary whenever one action can mutate more than
// one of those aggregates.
type CharacterPetUnitOfWork interface {
	WithinCharacterPets(context.Context, string, func(InventoryRepository, EquipmentRepository, PetRepository) error) error
}

type CharacterPetScopedUnitOfWork interface {
	WithinCharacterPetsScoped(context.Context, string, func(InventoryRepository, EquipmentRepository, PetRepository) error) error
}

// AccountCharacterItemUnitOfWork atomically persists the account-owned
// crystal/soul slots and the selected character's ordinary inventory. The two
// repositories are key-scoped for the supplied account and character.
type AccountCharacterItemUnitOfWork interface {
	WithinAccountCharacterItems(context.Context, string, string, func(AccountInventoryRepository, InventoryRepository) error) error
}

// CharacterAssetUnitOfWork supplies one transaction for the real character
// wallet fields and both item aggregates. Reward handlers must use this
// boundary when a single grant can change gold and inventory/equipment.
type CharacterAssetUnitOfWork interface {
	WithinCharacterAssets(context.Context, string, func(CharacterRepository, InventoryRepository, EquipmentRepository) error) error
}

type CharacterAssetScopedUnitOfWork interface {
	WithinCharacterAssetsScoped(context.Context, string, func(CharacterRepository, InventoryRepository, EquipmentRepository) error) error
}

// MailboxAssetUnitOfWork atomically moves account/character wallet, the
// account-owned crystal/soul warehouse, ordinary inventory assets and one
// mailbox aggregate. The two owner IDs may differ for send and are equal for
// attachment extraction.
type MailboxAssetUnitOfWork interface {
	WithinMailboxAssets(
		context.Context,
		string,
		string,
		func(AccountRepository, AccountInventoryRepository, CharacterRepository, InventoryRepository, MailboxRepository) error,
	) error
}

// MailboxClaimAssetUnitOfWork is the optional extraction-only extension of
// MailboxAssetUnitOfWork. Epic-fragment attachments are account-owned Epic
// Book balances rather than material-bag stacks, so the attachment removal,
// every ordinary asset in the same letter, and the Epic Book credit must
// commit or roll back together. Ordinary mail send/state/delete flows retain
// the narrower MailboxAssetUnitOfWork boundary.
type MailboxClaimAssetUnitOfWork interface {
	WithinMailboxClaimAssets(
		context.Context,
		string,
		string,
		func(AccountRepository, AccountInventoryRepository, CharacterRepository, InventoryRepository, MailboxRepository, EpicBookRepository) error,
	) error
}

// AccountCharacterAssetUnitOfWork supplies one transaction for the
// account-scoped crystal/soul inventory, the selected character wallet, and
// both item aggregates. Upgrade and shop flows must use this boundary when
// one action consumes account-owned materials while also mutating character
// gold or equipment.
type AccountCharacterAssetUnitOfWork interface {
	WithinAccountCharacterAssets(context.Context, string, string, func(AccountInventoryRepository, CharacterRepository, InventoryRepository, EquipmentRepository) error) error
}

// ShopPurchaseAssetUnitOfWork 是 AccountCharacterAssetUnitOfWork 的限购扩展：
// 同一事务里额外交出账号记录本身。PVF `[purchase limit]` 的作用域可以是
// account，账号维度的已购次数落在账号记录的 Metadata 上，必须与钱包、账号
// 仓库、角色背包的变更一起提交；否则一次提交失败就会既扣了次数又没发货，或者
// 发了货却没计次数（相当于白送一件限购商品）。
//
// 实现与 AccountCharacterAssetUnitOfWork 是同一个具体类型，调用方按接口断言
// 取得，因此不需要新的 Group 装配字段。
type ShopPurchaseAssetUnitOfWork interface {
	WithinShopPurchaseAssets(
		context.Context,
		string,
		string,
		func(AccountRepository, AccountInventoryRepository, CharacterRepository, InventoryRepository) error,
	) error
}

// BoosterAssetUnitOfWork extends the ordinary account/character item boundary
// with account premium state and the account-owned Epic Book. Premium-contract
// boxes and selectable fragment boxes must consume their source and commit the
// matching account effect in one transaction; otherwise a failed save can
// duplicate the box or lose its reward.
type BoosterAssetUnitOfWork interface {
	WithinBoosterAssets(context.Context, string, string, func(AccountRepository, AccountInventoryRepository, CharacterRepository, InventoryRepository, EquipmentRepository, EpicBookRepository) error) error
}

// RentalAssetUnitOfWork supplies one transaction for the account-scoped
// rental-point wallet, the selected character's gold wallet, and both item
// aggregates. Implementations must scope every repository to the two supplied
// owner keys so a forged request cannot mutate another account or character.
type RentalAssetUnitOfWork interface {
	WithinRentalAssets(context.Context, string, string, func(AccountRepository, CharacterRepository, InventoryRepository, EquipmentRepository) error) error
}

type RentalAssetScopedUnitOfWork interface {
	WithinRentalAssetsScoped(context.Context, string, string, func(AccountRepository, CharacterRepository, InventoryRepository, EquipmentRepository) error) error
}

// CeraShopAssetUnitOfWork supplies one transaction for the account Cera
// wallet, selected character, inventory/equipment projections, and the exact
// character container-settings scope. Checkout owners use this boundary so a
// debit cannot commit without every purchased effect.
//
// AccountInventoryRepository is part of the same boundary because the shop can
// hand out account-shared materials (cubes/souls, list 0 slots 354..365): those
// live in the account record, so crediting the purchase and landing the item
// must share one transaction.
type CeraShopAssetUnitOfWork interface {
	WithinCeraShopAssets(
		context.Context,
		string,
		string,
		string,
		func(AccountRepository, AccountInventoryRepository, CharacterRepository, InventoryRepository, EquipmentRepository, SettingsRepository) error,
	) error
}

// CharacterSkillUnitOfWork serializes and atomically commits one character's
// learned levels and SP/TP ledger.
type CharacterSkillUnitOfWork interface {
	WithinCharacterSkill(context.Context, string, func(SkillRepository) error) error
}

// CharacterProgressionUnitOfWork supplies one transaction-scoped character
// repository and skill repository for the same character. Experience/level
// and the SP/TP ledger must use this boundary so neither aggregate can commit
// without the other.
type CharacterProgressionUnitOfWork interface {
	WithinCharacterProgression(context.Context, string, func(CharacterRepository, SkillRepository) error) error
}

// CharacterSettlementUnitOfWork supplies one character-scoped transaction
// spanning every aggregate that a quest or dungeon settlement may mutate.
// The callback owns reward and progression rules; the repository owns atomic
// commit, rollback, and rejection of cross-character access.
type CharacterSettlementUnitOfWork interface {
	WithinCharacterSettlement(context.Context, string, func(Group) error) error
}

// EpicBookCraftUnitOfWork atomically updates account epic-book balances,
// account-owned materials, and the selected character's inventory/equipment.
type EpicBookCraftUnitOfWork interface {
	WithinEpicBookCraft(context.Context, string, string, func(EpicBookRepository, AccountInventoryRepository, InventoryRepository, EquipmentRepository) error) error
}

func TransactionKeyError(ctx context.Context, expected string, actual string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(actual) != expected {
		return db.ErrRecordKeyRequired
	}
	return nil
}
