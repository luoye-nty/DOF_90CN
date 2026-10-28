// 本文件提供 DNF 仓储的内存实现，用于本地开发、单元测试和 SQLite
// 单进程持久化后端的领域事务复用。
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/platform/db"
)

const StateVersion = 1

// State is a deterministic, transport-neutral snapshot of every repository
// owned by the memory aggregate. SQLite persists this value atomically; it is
// deliberately made only from domain records and contains no store internals.
type State struct {
	Version              int                                    `json:"version"`
	Accounts             []repository.AccountRecord             `json:"accounts,omitempty"`
	AccountInventories   []repository.AccountInventoryRecord    `json:"account_inventories,omitempty"`
	Characters           []repository.CharacterRecord           `json:"characters,omitempty"`
	Inventories          []repository.InventoryRecord           `json:"inventories,omitempty"`
	Equipment            []repository.EquipmentRecord           `json:"equipment,omitempty"`
	Pets                 []repository.PetRecord                 `json:"pets,omitempty"`
	Quests               []repository.QuestRecord               `json:"quests,omitempty"`
	Skills               []repository.SkillRecord               `json:"skills,omitempty"`
	DungeonPermissions   []repository.DungeonPermissionRecord   `json:"dungeon_permissions,omitempty"`
	HellPartyPermissions []repository.HellPartyPermissionRecord `json:"hell_party_permissions,omitempty"`
	PacketTemplates      []repository.PacketTemplateRecord      `json:"packet_templates,omitempty"`
	Settings             []repository.SettingsRecord            `json:"settings,omitempty"`
	Mailboxes            []repository.MailboxRecord             `json:"mailboxes,omitempty"`
	EpicBooks            []repository.EpicBookRecord            `json:"epic_books,omitempty"`
	UnitedFriends        []repository.UnitedFriendRecord        `json:"united_friends,omitempty"`
}

// Runtime owns the concrete memory stores so callers can atomically snapshot
// or rebuild the complete aggregate without exposing mutable map internals.
type Runtime struct {
	group                repository.Group
	accounts             *memoryAccountStore
	accountInventory     *db.MemoryStore[repository.AccountInventoryRecord]
	characters           *memoryCharStore
	inventory            *db.MemoryStore[repository.InventoryRecord]
	equipment            *db.MemoryStore[repository.EquipmentRecord]
	pets                 *db.MemoryStore[repository.PetRecord]
	quests               *db.MemoryStore[repository.QuestRecord]
	skills               *db.MemoryStore[repository.SkillRecord]
	dungeonPermissions   *memoryDungeonPermissionStore
	hellPartyPermissions *memoryHellPartyPermissionStore
	packetTemplates      *db.MemoryStore[repository.PacketTemplateRecord]
	settings             *db.MemoryStore[repository.SettingsRecord]
	mailboxes            *db.MemoryStore[repository.MailboxRecord]
	epicBooks            *db.MemoryStore[repository.EpicBookRecord]
	unitedFriends        *memoryUnitedFriendStore
}

// NewMemoryGroup creates the complete volatile repository aggregate.
func NewMemoryGroup() repository.Group {
	return NewRuntime().Group()
}

// NewRuntime creates an empty memory aggregate with all repository and unit-
// of-work owners wired to the same stores.
func NewRuntime() *Runtime {
	accounts := newMemoryAccountStore()
	accountInventory := db.NewMemoryStore(repository.AccountInventoryKey, repository.CloneAccountInventory)
	characters := newMemoryCharStore()
	inventory := db.NewMemoryStore(repository.InventoryKey, repository.CloneInventory)
	equipment := db.NewMemoryStore(repository.EquipmentKey, repository.CloneEquipment)
	pets := db.NewMemoryStore(repository.PetKey, repository.ClonePet)
	quests := db.NewMemoryStore(repository.QuestKey, repository.CloneQuest)
	skills := db.NewMemoryStore(repository.SkillKey, repository.CloneSkill)
	dungeonPermissions := newMemoryDungeonPermissionStore()
	hellPartyPermissions := newMemoryHellPartyPermissionStore()
	packetTemplates := db.NewMemoryStore(repository.PacketTemplateKey, repository.ClonePacketTemplate)
	settings := db.NewMemoryStore(repository.SettingsKey, repository.CloneSettings)
	mailboxes := db.NewMemoryStore(repository.MailboxKey, repository.CloneMailbox)
	epicBooks := db.NewMemoryStore(repository.EpicBookKey, repository.CloneEpicBook)
	unitedFriends := newMemoryUnitedFriendStore()
	aggregateTxMu := &sync.Mutex{}
	runtime := &Runtime{
		accounts:             accounts,
		accountInventory:     accountInventory,
		characters:           characters,
		inventory:            inventory,
		equipment:            equipment,
		pets:                 pets,
		quests:               quests,
		skills:               skills,
		dungeonPermissions:   dungeonPermissions,
		hellPartyPermissions: hellPartyPermissions,
		packetTemplates:      packetTemplates,
		settings:             settings,
		mailboxes:            mailboxes,
		epicBooks:            epicBooks,
		unitedFriends:        unitedFriends,
	}
	runtime.group = repository.Group{
		Account:               accounts,
		AccountInventory:      accountInventory,
		Character:             characters,
		Inventory:             inventory,
		Equipment:             equipment,
		Pet:                   pets,
		Quest:                 quests,
		Skill:                 skills,
		DungeonPermission:     dungeonPermissions,
		HellPartyPermission:   hellPartyPermissions,
		PacketTemplate:        packetTemplates,
		Settings:              settings,
		Mailbox:               mailboxes,
		EpicBook:              epicBooks,
		UnitedFriend:          unitedFriends,
		CharacterCreate:       &memoryCharacterCreationUnitOfWork{},
		CharacterItems:        &memoryCharacterItemUnitOfWork{sharedMu: aggregateTxMu, inventory: inventory, equipment: equipment},
		CharacterTrade:        &memoryCharacterTradeUnitOfWork{sharedMu: aggregateTxMu, character: characters, inventory: inventory},
		AccountCharacterTrade: &memoryAccountCharacterTradeUnitOfWork{sharedMu: aggregateTxMu, accountInventory: accountInventory, character: characters, inventory: inventory, epicBook: epicBooks, pets: pets},
		AccountPairTrade:      &memoryAccountPairCharacterTradeUnitOfWork{sharedMu: aggregateTxMu, accountInventory: accountInventory, character: characters, inventory: inventory},
		CharacterPets:         &memoryCharacterPetUnitOfWork{sharedMu: aggregateTxMu, inventory: inventory, equipment: equipment, pets: pets},
		AccountItems:          &memoryAccountCharacterItemUnitOfWork{sharedMu: aggregateTxMu, accountInventory: accountInventory, inventory: inventory},
		CharacterAssets:       &memoryCharacterAssetUnitOfWork{sharedMu: aggregateTxMu, character: characters, inventory: inventory, equipment: equipment},
		MailboxAssets:         &memoryMailboxAssetUnitOfWork{sharedMu: aggregateTxMu, account: accounts, accountInventory: accountInventory, character: characters, inventory: inventory, mailbox: mailboxes, epicBook: epicBooks},
		AccountAssets:         &memoryAccountCharacterAssetUnitOfWork{sharedMu: aggregateTxMu, account: accounts, accountInventory: accountInventory, character: characters, inventory: inventory, equipment: equipment},
		BoosterAssets:         &memoryBoosterAssetUnitOfWork{sharedMu: aggregateTxMu, account: accounts, accountInventory: accountInventory, character: characters, inventory: inventory, equipment: equipment, epicBook: epicBooks},
		RentalAssets:          &memoryRentalAssetUnitOfWork{sharedMu: aggregateTxMu, account: accounts, character: characters, inventory: inventory, equipment: equipment},
		CeraShopAssets:        &memoryCeraShopAssetUnitOfWork{sharedMu: aggregateTxMu, account: accounts, accountInventory: accountInventory, character: characters, inventory: inventory, equipment: equipment, settings: settings},
		CharacterSkills:       &memoryCharacterSkillUnitOfWork{sharedMu: aggregateTxMu, skills: skills},
		CharacterProgression:  &memoryCharacterProgressionUnitOfWork{sharedMu: aggregateTxMu, character: characters, skills: skills},
		CharacterSettlement:   &memoryCharacterSettlementUnitOfWork{sharedMu: aggregateTxMu, account: accounts, accountInventory: accountInventory, character: characters, quests: quests, inventory: inventory, equipment: equipment, skills: skills, settings: settings, hellPartyPermissions: hellPartyPermissions},
		EpicBookCraft:         &memoryEpicBookCraftUnitOfWork{sharedMu: aggregateTxMu, epicBook: epicBooks, accountInventory: accountInventory, inventory: inventory, equipment: equipment},
		RaidEntryCost:         &memoryRaidEntryCostUnitOfWork{sharedMu: aggregateTxMu, inventory: inventory},
		RaidRewards:           &memoryRaidRewardUnitOfWork{sharedMu: aggregateTxMu, account: accounts, inventory: inventory, settings: settings},
	}
	return runtime
}

// NewRuntimeFromState validates and restores one complete snapshot.
func NewRuntimeFromState(ctx context.Context, state State) (*Runtime, error) {
	if state.Version != 0 && state.Version != StateVersion {
		return nil, fmt.Errorf("unsupported memory repository state version=%d", state.Version)
	}
	runtime := NewRuntime()
	group := runtime.Group()
	for _, record := range state.Accounts {
		if err := group.Account.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore account %q: %w", record.AccountID, err)
		}
	}
	for _, record := range state.AccountInventories {
		if err := group.AccountInventory.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore account inventory %q: %w", record.AccountID, err)
		}
	}
	for _, record := range state.Characters {
		if err := group.Character.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore character %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.Inventories {
		if err := group.Inventory.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore inventory %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.Equipment {
		if err := group.Equipment.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore equipment %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.Pets {
		if err := group.Pet.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore pet %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.Quests {
		if err := group.Quest.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore quest %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.Skills {
		if err := group.Skill.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore skill %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.PacketTemplates {
		if err := group.PacketTemplate.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore packet template %q: %w", record.TemplateID, err)
		}
	}
	for _, record := range state.Settings {
		if err := group.Settings.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore settings %q: %w", record.Scope, err)
		}
	}
	for _, record := range state.Mailboxes {
		if err := group.Mailbox.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore mailbox %q: %w", record.CharacterID, err)
		}
	}
	for _, record := range state.EpicBooks {
		if err := group.EpicBook.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore epic book %q: %w", record.AccountID, err)
		}
	}
	for _, record := range state.UnitedFriends {
		if err := group.UnitedFriend.Save(ctx, record); err != nil {
			return nil, fmt.Errorf("restore united friend %q: %w", record.OwnerName, err)
		}
	}
	runtime.dungeonPermissions.replace(state.DungeonPermissions)
	runtime.hellPartyPermissions.replace(state.HellPartyPermissions)
	return runtime, nil
}

func (r *Runtime) Group() repository.Group {
	if r == nil {
		return repository.Group{}
	}
	return r.group
}

// Snapshot returns stable key-ordered records suitable for deterministic JSON.
func (r *Runtime) Snapshot() State {
	if r == nil {
		return State{Version: StateVersion}
	}
	state := State{
		Version:              StateVersion,
		Accounts:             r.accounts.Snapshot(nil),
		AccountInventories:   r.accountInventory.Snapshot(nil),
		Characters:           r.characters.Snapshot(nil),
		Inventories:          r.inventory.Snapshot(nil),
		Equipment:            r.equipment.Snapshot(nil),
		Pets:                 r.pets.Snapshot(nil),
		Quests:               r.quests.Snapshot(nil),
		Skills:               r.skills.Snapshot(nil),
		DungeonPermissions:   r.dungeonPermissions.snapshot(),
		HellPartyPermissions: r.hellPartyPermissions.snapshot(),
		PacketTemplates:      r.packetTemplates.Snapshot(nil),
		Settings:             r.settings.Snapshot(nil),
		Mailboxes:            r.mailboxes.Snapshot(nil),
		EpicBooks:            r.epicBooks.Snapshot(nil),
		UnitedFriends:        r.unitedFriends.Snapshot(nil),
	}
	sort.Slice(state.Accounts, func(i, j int) bool { return state.Accounts[i].AccountID < state.Accounts[j].AccountID })
	sort.Slice(state.AccountInventories, func(i, j int) bool {
		return state.AccountInventories[i].AccountID < state.AccountInventories[j].AccountID
	})
	sort.Slice(state.Characters, func(i, j int) bool { return state.Characters[i].CharacterID < state.Characters[j].CharacterID })
	sort.Slice(state.Inventories, func(i, j int) bool { return state.Inventories[i].CharacterID < state.Inventories[j].CharacterID })
	sort.Slice(state.Equipment, func(i, j int) bool { return state.Equipment[i].CharacterID < state.Equipment[j].CharacterID })
	sort.Slice(state.Pets, func(i, j int) bool { return state.Pets[i].CharacterID < state.Pets[j].CharacterID })
	sort.Slice(state.Quests, func(i, j int) bool { return state.Quests[i].CharacterID < state.Quests[j].CharacterID })
	sort.Slice(state.Skills, func(i, j int) bool { return state.Skills[i].CharacterID < state.Skills[j].CharacterID })
	sort.Slice(state.PacketTemplates, func(i, j int) bool { return state.PacketTemplates[i].TemplateID < state.PacketTemplates[j].TemplateID })
	sort.Slice(state.Settings, func(i, j int) bool { return state.Settings[i].Scope < state.Settings[j].Scope })
	sort.Slice(state.Mailboxes, func(i, j int) bool { return state.Mailboxes[i].CharacterID < state.Mailboxes[j].CharacterID })
	sort.Slice(state.EpicBooks, func(i, j int) bool { return state.EpicBooks[i].AccountID < state.EpicBooks[j].AccountID })
	sort.Slice(state.UnitedFriends, func(i, j int) bool { return state.UnitedFriends[i].OwnerName < state.UnitedFriends[j].OwnerName })
	return state
}
