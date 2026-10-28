package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

type persistentStore[T any] struct {
	component   *Component
	selectStore func(repository.Group) db.Store[T]
}

func (s *persistentStore[T]) Load(ctx context.Context, key string) (record T, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var loadErr error
		record, found, loadErr = s.selectStore(group).Load(ctx, key)
		return loadErr
	})
	return record, found, err
}

func (s *persistentStore[T]) Save(ctx context.Context, record T) error {
	return s.component.mutateAsync(ctx, func(group repository.Group) error {
		return s.selectStore(group).Save(ctx, record)
	})
}

func (s *persistentStore[T]) Check(ctx context.Context) error { return s.component.check(ctx) }

type accountStore struct {
	*persistentStore[repository.AccountRecord]
}

func (s *accountStore) ListAccounts(ctx context.Context, query string, limit int) (records []repository.AccountRecord, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var listErr error
		records, listErr = repository.ListAccounts(ctx, group.Account, query, limit)
		return listErr
	})
	return records, err
}

func (s *accountStore) FindAccountIDByLoginUsername(ctx context.Context, username string) (string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", false, nil
	}
	if s == nil || s.component == nil || s.component.db == nil {
		return "", false, ErrDatabaseRequired
	}
	var accountID string
	err := s.component.db.QueryRowContext(ctx,
		`SELECT account_id FROM dnf_login_accounts WHERE username = ? AND state = 'active' LIMIT 1`, username,
	).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find SQLite login username: %w", err)
	}
	return strings.TrimSpace(accountID), true, nil
}

func (s *accountStore) SaveMetadataEntry(ctx context.Context, accountID, key, value string, updatedAt time.Time) error {
	return s.component.mutateAsync(ctx, func(group repository.Group) error {
		writer, ok := group.Account.(repository.AccountMetadataEntryWriter)
		if !ok {
			return repository.ErrAccountMetadataUnavailable
		}
		return writer.SaveMetadataEntry(ctx, accountID, key, value, updatedAt)
	})
}

func (s *accountStore) FindAccountIDByRepresentName(ctx context.Context, name string) (accountID string, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		finder, ok := group.Account.(repository.RepresentAccountNameFinder)
		if !ok {
			return fmt.Errorf("account represent-name lookup is unavailable")
		}
		var findErr error
		accountID, found, findErr = finder.FindAccountIDByRepresentName(ctx, name)
		return findErr
	})
	return accountID, found, err
}

type characterStore struct {
	*persistentStore[repository.CharacterRecord]
}

type questStore struct {
	*persistentStore[repository.QuestRecord]
}

type unitedFriendStore struct{ component *Component }

func (s *unitedFriendStore) Check(ctx context.Context) error { return s.component.check(ctx) }
func (s *unitedFriendStore) Load(ctx context.Context, key string) (record repository.UnitedFriendRecord, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var loadErr error
		record, found, loadErr = group.UnitedFriend.Load(ctx, key)
		return loadErr
	})
	return record, found, err
}
func (s *unitedFriendStore) Save(ctx context.Context, record repository.UnitedFriendRecord) error {
	return s.component.mutateSync(ctx, func(group repository.Group) error {
		return group.UnitedFriend.Save(ctx, record)
	})
}
func (s *unitedFriendStore) ListObservers(ctx context.Context, friendName string) (owners []string, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var listErr error
		owners, listErr = group.UnitedFriend.ListObservers(ctx, friendName)
		return listErr
	})
	return owners, err
}

// SaveQuestStateEntry mutates only one in-memory objective under SQLite's
// serialized mutation gate. The resulting snapshot is then flushed by the
// existing asynchronous WAL/checkpoint owner, preserving instant gameplay
// acknowledgement without allowing a stale full record to overwrite another
// objective update.
func (s *questStore) SaveQuestStateEntry(
	ctx context.Context,
	characterID string,
	field repository.QuestField,
	questID int64,
	state repository.QuestState,
	updatedAt time.Time,
) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return db.ErrRecordKeyRequired
	}
	if field != repository.QuestFieldStates && field != repository.QuestFieldProgress {
		return fmt.Errorf("unsupported quest state field %q", field)
	}
	cloned := repository.CloneQuest(repository.QuestRecord{
		States: map[int64]repository.QuestState{questID: state},
	}).States[questID]
	return s.component.mutateAsync(ctx, func(group repository.Group) error {
		record, found, err := group.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			record = repository.QuestRecord{CharacterID: characterID}
		} else {
			record = repository.CloneQuest(record)
		}
		switch field {
		case repository.QuestFieldStates:
			if record.States == nil {
				record.States = make(map[int64]repository.QuestState)
			}
			record.States[questID] = cloned
		case repository.QuestFieldProgress:
			if record.Progress == nil {
				record.Progress = make(map[int64]repository.QuestState)
			}
			record.Progress[questID] = cloned
		}
		record.CharacterID = characterID
		record.UpdatedAt = updatedAt
		return group.Quest.Save(ctx, record)
	})
}

func (s *characterStore) ListByAccount(ctx context.Context, accountID string, limit int) (records []repository.CharacterRecord, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var listErr error
		records, listErr = group.Character.ListByAccount(ctx, accountID, limit)
		return listErr
	})
	return records, err
}

func (s *characterStore) FindIDByName(ctx context.Context, name string) (characterID string, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var findErr error
		characterID, found, findErr = group.Character.FindIDByName(ctx, name)
		return findErr
	})
	return characterID, found, err
}

func (s *characterStore) NextNumericID(ctx context.Context) (next int, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var nextErr error
		next, nextErr = group.Character.NextNumericID(ctx)
		return nextErr
	})
	return next, err
}

func (s *characterStore) CreateCharacter(ctx context.Context, record repository.CharacterRecord) error {
	return s.component.mutateSync(ctx, func(group repository.Group) error {
		creator, ok := group.Character.(repository.CharacterCreator)
		if !ok {
			return repository.ErrCharacterCreateMissing
		}
		return creator.CreateCharacter(ctx, record)
	})
}

func (s *characterStore) SwapCharacterSlots(ctx context.Context, accountID string, slotA, slotB int) error {
	return s.component.mutateSync(ctx, func(group repository.Group) error {
		swapper, ok := group.Character.(repository.CharacterSlotSwapper)
		if !ok {
			return repository.ErrCharacterSlotSwapUnavailable
		}
		return swapper.SwapCharacterSlots(ctx, accountID, slotA, slotB)
	})
}

func (s *characterStore) DeleteCharacterAndCompact(ctx context.Context, accountID string, slot int, name string) error {
	return s.component.mutateSync(ctx, func(group repository.Group) error {
		deleter, ok := group.Character.(repository.CharacterRosterDeleter)
		if !ok {
			return repository.ErrCharacterDeleteUnavailable
		}
		return deleter.DeleteCharacterAndCompact(ctx, accountID, slot, name)
	})
}

func (s *characterStore) AdvanceStoryDigest(ctx context.Context, characterID string, level, migrationVersion uint32) error {
	return s.component.mutateAsync(ctx, func(group repository.Group) error {
		advancer, ok := group.Character.(repository.CharacterStoryDigestAdvancer)
		if !ok {
			return repository.ErrCharacterStoryDigestAdvanceUnavailable
		}
		return advancer.AdvanceStoryDigest(ctx, characterID, level, migrationVersion)
	})
}

type dungeonPermissionStore struct{ component *Component }

func (s *dungeonPermissionStore) Check(ctx context.Context) error { return s.component.check(ctx) }
func (s *dungeonPermissionStore) Load(ctx context.Context, key string) (record repository.DungeonPermissionRecord, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var loadErr error
		record, found, loadErr = group.DungeonPermission.Load(ctx, key)
		return loadErr
	})
	return record, found, err
}
func (s *dungeonPermissionStore) UpsertMax(ctx context.Context, characterID string, dungeonID uint32, clearState byte) (entry repository.DungeonPermissionEntry, changed bool, err error) {
	err = s.component.mutateAsync(ctx, func(group repository.Group) error {
		var upsertErr error
		entry, changed, upsertErr = group.DungeonPermission.UpsertMax(ctx, characterID, dungeonID, clearState)
		return upsertErr
	})
	return entry, changed, err
}

type hellPartyPermissionStore struct{ component *Component }

func (s *hellPartyPermissionStore) Check(ctx context.Context) error { return s.component.check(ctx) }
func (s *hellPartyPermissionStore) Load(ctx context.Context, key string) (record repository.HellPartyPermissionRecord, found bool, err error) {
	err = s.component.read(ctx, func(group repository.Group) error {
		var loadErr error
		record, found, loadErr = group.HellPartyPermission.Load(ctx, key)
		return loadErr
	})
	return record, found, err
}
func (s *hellPartyPermissionStore) Upsert(ctx context.Context, characterID string, areaID, questID int64) (entry repository.HellPartyPermissionEntry, changed bool, err error) {
	err = s.component.mutateAsync(ctx, func(group repository.Group) error {
		var upsertErr error
		entry, changed, upsertErr = group.HellPartyPermission.Upsert(ctx, characterID, areaID, questID)
		return upsertErr
	})
	return entry, changed, err
}
func (s *hellPartyPermissionStore) Delete(ctx context.Context, characterID string, areaID int64) error {
	return s.component.mutateAsync(ctx, func(group repository.Group) error {
		deleter, ok := group.HellPartyPermission.(repository.HellPartyPermissionRestorer)
		if !ok {
			return fmt.Errorf("hell-party permission delete is unavailable")
		}
		return deleter.Delete(ctx, characterID, areaID)
	})
}

func newPersistentStore[T any](component *Component, selector func(repository.Group) db.Store[T]) *persistentStore[T] {
	return &persistentStore[T]{component: component, selectStore: selector}
}

func buildGroup(component *Component) repository.Group {
	account := &accountStore{newPersistentStore(component, func(g repository.Group) db.Store[repository.AccountRecord] { return g.Account })}
	character := &characterStore{newPersistentStore(component, func(g repository.Group) db.Store[repository.CharacterRecord] { return g.Character })}
	quest := &questStore{newPersistentStore(component, func(g repository.Group) db.Store[repository.QuestRecord] { return g.Quest })}
	transactions := &unitOfWork{component: component}
	return repository.Group{
		Account:               account,
		AccountInventory:      newPersistentStore(component, func(g repository.Group) db.Store[repository.AccountInventoryRecord] { return g.AccountInventory }),
		Character:             character,
		Inventory:             newPersistentStore(component, func(g repository.Group) db.Store[repository.InventoryRecord] { return g.Inventory }),
		Equipment:             newPersistentStore(component, func(g repository.Group) db.Store[repository.EquipmentRecord] { return g.Equipment }),
		Pet:                   newPersistentStore(component, func(g repository.Group) db.Store[repository.PetRecord] { return g.Pet }),
		Quest:                 quest,
		Skill:                 newPersistentStore(component, func(g repository.Group) db.Store[repository.SkillRecord] { return g.Skill }),
		DungeonPermission:     &dungeonPermissionStore{component: component},
		HellPartyPermission:   &hellPartyPermissionStore{component: component},
		PacketTemplate:        newPersistentStore(component, func(g repository.Group) db.Store[repository.PacketTemplateRecord] { return g.PacketTemplate }),
		Settings:              newPersistentStore(component, func(g repository.Group) db.Store[repository.SettingsRecord] { return g.Settings }),
		Mailbox:               newPersistentStore(component, func(g repository.Group) db.Store[repository.MailboxRecord] { return g.Mailbox }),
		EpicBook:              newPersistentStore(component, func(g repository.Group) db.Store[repository.EpicBookRecord] { return g.EpicBook }),
		UnitedFriend:          &unitedFriendStore{component: component},
		CharacterCreate:       transactions,
		CharacterItems:        transactions,
		CharacterTrade:        transactions,
		AccountCharacterTrade: transactions,
		AccountPairTrade:      transactions,
		CharacterPets:         transactions,
		AccountItems:          transactions,
		CharacterAssets:       transactions,
		MailboxAssets:         transactions,
		AccountAssets:         transactions,
		BoosterAssets:         transactions,
		RentalAssets:          transactions,
		CeraShopAssets:        transactions,
		CharacterSkills:       transactions,
		CharacterProgression:  transactions,
		CharacterSettlement:   transactions,
		EpicBookCraft:         transactions,
		RaidEntryCost:         transactions,
		RaidRewards:           transactions,
	}
}
