package repository

import (
	"context"
	"errors"

	"longheng.io/server/internal/platform/db"
)

var ErrRepoMissing = errors.New("dnf repository is missing")

type Group struct {
	Account               AccountRepository
	AccountInventory      AccountInventoryRepository
	Character             CharacterRepository
	Inventory             InventoryRepository
	Equipment             EquipmentRepository
	Pet                   PetRepository
	Quest                 QuestRepository
	Skill                 SkillRepository
	DungeonPermission     DungeonPermissionRepository
	HellPartyPermission   HellPartyPermissionRepository
	PacketTemplate        PacketTemplateRepository
	Settings              SettingsRepository
	Mailbox               MailboxRepository
	UnitedFriend          UnitedFriendRepository
	EpicBook              EpicBookRepository
	LegacyUserInfo        LegacyUserInfoRepository
	LegacyInventory       LegacyInventoryRepository
	CharacterCreate       CharacterCreationUnitOfWork
	CharacterItems        CharacterItemUnitOfWork
	CharacterTrade        CharacterTradeUnitOfWork
	AccountCharacterTrade AccountCharacterTradeUnitOfWork
	AccountPairTrade      AccountPairCharacterTradeUnitOfWork
	CharacterPets         CharacterPetUnitOfWork
	AccountItems          AccountCharacterItemUnitOfWork
	CharacterAssets       CharacterAssetUnitOfWork
	MailboxAssets         MailboxAssetUnitOfWork
	AccountAssets         AccountCharacterAssetUnitOfWork
	BoosterAssets         BoosterAssetUnitOfWork
	RentalAssets          RentalAssetUnitOfWork
	CeraShopAssets        CeraShopAssetUnitOfWork
	CharacterSkills       CharacterSkillUnitOfWork
	CharacterProgression  CharacterProgressionUnitOfWork
	CharacterSettlement   CharacterSettlementUnitOfWork
	EpicBookCraft         EpicBookCraftUnitOfWork
	RaidEntryCost         RaidEntryCostUnitOfWork
	RaidRewards           RaidRewardUnitOfWork
}

type RepositoryGroup = Group

func (g Group) Check(ctx context.Context) error {
	repos := []any{
		g.Account,
		g.AccountInventory,
		g.Character,
		g.Inventory,
		g.Equipment,
		g.Pet,
		g.Quest,
		g.Skill,
		g.DungeonPermission,
		g.HellPartyPermission,
		g.PacketTemplate,
		g.Settings,
		g.Mailbox,
		g.UnitedFriend,
	}
	for _, repo := range repos {
		if repo == nil {
			return ErrRepoMissing
		}
		if err := db.Check(ctx, repo); err != nil {
			return err
		}
	}
	if g.LegacyUserInfo != nil {
		if err := db.Check(ctx, g.LegacyUserInfo); err != nil {
			return err
		}
	}
	return nil
}
