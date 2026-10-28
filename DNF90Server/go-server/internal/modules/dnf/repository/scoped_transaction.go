package repository

import "context"

// WithinCharacterItemsScoped uses a backend's record-scoped durable path when
// available and otherwise preserves the ordinary unit-of-work behavior. This
// is intentionally opt-in so legacy callers that rely on a critical commit
// flushing earlier pending writes keep their existing contract.
func WithinCharacterItemsScoped(
	ctx context.Context,
	owner CharacterItemUnitOfWork,
	characterID string,
	apply func(InventoryRepository, EquipmentRepository) error,
) error {
	if scoped, ok := owner.(CharacterItemScopedUnitOfWork); ok {
		return scoped.WithinCharacterItemsScoped(ctx, characterID, apply)
	}
	return owner.WithinCharacterItems(ctx, characterID, apply)
}

func WithinCharacterPetsScoped(
	ctx context.Context,
	owner CharacterPetUnitOfWork,
	characterID string,
	apply func(InventoryRepository, EquipmentRepository, PetRepository) error,
) error {
	if scoped, ok := owner.(CharacterPetScopedUnitOfWork); ok {
		return scoped.WithinCharacterPetsScoped(ctx, characterID, apply)
	}
	return owner.WithinCharacterPets(ctx, characterID, apply)
}

func WithinCharacterAssetsScoped(
	ctx context.Context,
	owner CharacterAssetUnitOfWork,
	characterID string,
	apply func(CharacterRepository, InventoryRepository, EquipmentRepository) error,
) error {
	if scoped, ok := owner.(CharacterAssetScopedUnitOfWork); ok {
		return scoped.WithinCharacterAssetsScoped(ctx, characterID, apply)
	}
	return owner.WithinCharacterAssets(ctx, characterID, apply)
}

func WithinRentalAssetsScoped(
	ctx context.Context,
	owner RentalAssetUnitOfWork,
	accountID string,
	characterID string,
	apply func(AccountRepository, CharacterRepository, InventoryRepository, EquipmentRepository) error,
) error {
	if scoped, ok := owner.(RentalAssetScopedUnitOfWork); ok {
		return scoped.WithinRentalAssetsScoped(ctx, accountID, characterID, apply)
	}
	return owner.WithinRentalAssets(ctx, accountID, characterID, apply)
}
