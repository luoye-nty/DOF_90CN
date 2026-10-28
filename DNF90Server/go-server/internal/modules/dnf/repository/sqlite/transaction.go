package sqlite

import (
	"context"

	"longheng.io/server/internal/modules/dnf/repository"
)

type unitOfWork struct{ component *Component }

func (u *unitOfWork) WithinCharacterCreation(ctx context.Context, characterID string, _ repository.Group, apply func(repository.Group) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterCreate.WithinCharacterCreation(ctx, characterID, group, apply)
	})
}

func (u *unitOfWork) WithinRaidEntryCost(ctx context.Context, characterIDs []string, apply func(repository.InventoryRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.RaidEntryCost.WithinRaidEntryCost(ctx, characterIDs, apply)
	})
}

func (u *unitOfWork) WithinRaidRewards(ctx context.Context, characterIDs []string, receipt string, apply func(repository.InventoryRepository, repository.SettingsRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.RaidRewards.WithinRaidRewards(ctx, characterIDs, receipt, apply)
	})
}

func (u *unitOfWork) WithinRaidRewardAssets(
	ctx context.Context,
	characterIDs []string,
	accountIDs []string,
	receipt string,
	apply func(repository.AccountRepository, repository.InventoryRepository, repository.SettingsRepository) error,
) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		assets, ok := group.RaidRewards.(repository.RaidRewardAssetUnitOfWork)
		if !ok {
			return repository.ErrRaidRewardTransactionUnavailable
		}
		return assets.WithinRaidRewardAssets(ctx, characterIDs, accountIDs, receipt, apply)
	})
}

func (u *unitOfWork) WithinCharacterItems(ctx context.Context, characterID string, apply func(repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterItems.WithinCharacterItems(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterItemsScoped(ctx context.Context, characterID string, apply func(repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateCharacterItemsScopedSync(ctx, characterID, apply)
}

func (u *unitOfWork) WithinCharacterTrade(ctx context.Context, firstID, secondID string, apply func(repository.CharacterRepository, repository.InventoryRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterTrade.WithinCharacterTrade(ctx, firstID, secondID, apply)
	})
}

func (u *unitOfWork) WithinAccountCharacterTrade(ctx context.Context, accountID, firstID, secondID string, apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EpicBookRepository, repository.PetRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.AccountCharacterTrade.WithinAccountCharacterTrade(ctx, accountID, firstID, secondID, apply)
	})
}

func (u *unitOfWork) WithinAccountPairCharacterTrade(ctx context.Context, firstAccountID, secondAccountID, firstID, secondID string, apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		if group.AccountPairTrade == nil {
			return repository.ErrAccountCharacterTradeTransactionUnavailable
		}
		return group.AccountPairTrade.WithinAccountPairCharacterTrade(ctx, firstAccountID, secondAccountID, firstID, secondID, apply)
	})
}

func (u *unitOfWork) WithinCharacterPets(ctx context.Context, characterID string, apply func(repository.InventoryRepository, repository.EquipmentRepository, repository.PetRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterPets.WithinCharacterPets(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterPetsScoped(ctx context.Context, characterID string, apply func(repository.InventoryRepository, repository.EquipmentRepository, repository.PetRepository) error) error {
	return u.component.mutateCharacterPetsScopedSync(ctx, characterID, apply)
}

func (u *unitOfWork) WithinAccountCharacterItems(ctx context.Context, accountID, characterID string, apply func(repository.AccountInventoryRepository, repository.InventoryRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.AccountItems.WithinAccountCharacterItems(ctx, accountID, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterAssets(ctx context.Context, characterID string, apply func(repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterAssets.WithinCharacterAssets(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterAssetsScoped(ctx context.Context, characterID string, apply func(repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateCharacterAssetsSync(ctx, characterID, apply)
}

func (u *unitOfWork) WithinMailboxAssets(ctx context.Context, characterID, mailboxID string, apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.MailboxRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.MailboxAssets.WithinMailboxAssets(ctx, characterID, mailboxID, apply)
	})
}

func (u *unitOfWork) WithinMailboxClaimAssets(ctx context.Context, characterID, mailboxID string, apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.MailboxRepository, repository.EpicBookRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		claimAssets, ok := group.MailboxAssets.(repository.MailboxClaimAssetUnitOfWork)
		if !ok {
			return repository.ErrMailboxAssetTransactionUnavailable
		}
		return claimAssets.WithinMailboxClaimAssets(ctx, characterID, mailboxID, apply)
	})
}

func (u *unitOfWork) WithinAccountCharacterAssets(ctx context.Context, accountID, characterID string, apply func(repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.AccountAssets.WithinAccountCharacterAssets(ctx, accountID, characterID, apply)
	})
}

func (u *unitOfWork) WithinShopPurchaseAssets(ctx context.Context, accountID, characterID string, apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		shop, ok := group.AccountAssets.(repository.ShopPurchaseAssetUnitOfWork)
		if !ok {
			return repository.ErrShopPurchaseAssetTransactionUnavailable
		}
		return shop.WithinShopPurchaseAssets(ctx, accountID, characterID, apply)
	})
}

func (u *unitOfWork) WithinBoosterAssets(ctx context.Context, accountID, characterID string, apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository, repository.EpicBookRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.BoosterAssets.WithinBoosterAssets(ctx, accountID, characterID, apply)
	})
}

func (u *unitOfWork) WithinRentalAssets(ctx context.Context, accountID, characterID string, apply func(repository.AccountRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.RentalAssets.WithinRentalAssets(ctx, accountID, characterID, apply)
	})
}

func (u *unitOfWork) WithinRentalAssetsScoped(ctx context.Context, accountID, characterID string, apply func(repository.AccountRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateRentalAssetsScopedSync(ctx, accountID, characterID, apply)
}

func (u *unitOfWork) WithinCeraShopAssets(ctx context.Context, accountID, characterID, settingsScope string, apply func(repository.AccountRepository, repository.AccountInventoryRepository, repository.CharacterRepository, repository.InventoryRepository, repository.EquipmentRepository, repository.SettingsRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CeraShopAssets.WithinCeraShopAssets(ctx, accountID, characterID, settingsScope, apply)
	})
}

func (u *unitOfWork) WithinCharacterSkill(ctx context.Context, characterID string, apply func(repository.SkillRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterSkills.WithinCharacterSkill(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterProgression(ctx context.Context, characterID string, apply func(repository.CharacterRepository, repository.SkillRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterProgression.WithinCharacterProgression(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinCharacterSettlement(ctx context.Context, characterID string, apply func(repository.Group) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, apply)
	})
}

func (u *unitOfWork) WithinEpicBookCraft(ctx context.Context, accountID, characterID string, apply func(repository.EpicBookRepository, repository.AccountInventoryRepository, repository.InventoryRepository, repository.EquipmentRepository) error) error {
	return u.component.mutateSync(ctx, func(group repository.Group) error {
		return group.EpicBookCraft.WithinEpicBookCraft(ctx, accountID, characterID, apply)
	})
}
