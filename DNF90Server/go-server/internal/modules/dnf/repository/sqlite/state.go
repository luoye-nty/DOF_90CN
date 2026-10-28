package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

const (
	kindAccount             = "account"
	kindAccountInventory    = "account_inventory"
	kindCharacter           = "character"
	kindInventory           = "inventory"
	kindEquipment           = "equipment"
	kindPet                 = "pet"
	kindQuest               = "quest"
	kindSkill               = "skill"
	kindDungeonPermission   = "dungeon_permission"
	kindHellPartyPermission = "hell_party_permission"
	kindPacketTemplate      = "packet_template"
	kindSettings            = "settings"
	kindMailbox             = "mailbox"
	kindEpicBook            = "epic_book"
	kindUnitedFriend        = "united_friend"
)

type persistedRecord struct {
	kind     string
	key      string
	payload  []byte
	checksum string
}

func stateRecords(state memory.State) (map[string]persistedRecord, error) {
	out := make(map[string]persistedRecord)
	add := func(kind, key string, value any) error {
		record, err := makePersistedRecord(kind, key, value)
		if err != nil {
			return err
		}
		out[record.kind+"\x00"+record.key] = record
		return nil
	}
	for _, record := range state.Accounts {
		if err := add(kindAccount, record.AccountID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.AccountInventories {
		if err := add(kindAccountInventory, record.AccountID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Characters {
		if err := add(kindCharacter, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Inventories {
		if err := add(kindInventory, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Equipment {
		if err := add(kindEquipment, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Pets {
		if err := add(kindPet, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Quests {
		if err := add(kindQuest, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Skills {
		if err := add(kindSkill, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.DungeonPermissions {
		if err := add(kindDungeonPermission, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.HellPartyPermissions {
		if err := add(kindHellPartyPermission, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.PacketTemplates {
		if err := add(kindPacketTemplate, record.TemplateID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Settings {
		if err := add(kindSettings, record.Scope, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.Mailboxes {
		if err := add(kindMailbox, record.CharacterID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.EpicBooks {
		if err := add(kindEpicBook, record.AccountID, record); err != nil {
			return nil, err
		}
	}
	for _, record := range state.UnitedFriends {
		if err := add(kindUnitedFriend, record.OwnerName, record); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func makePersistedRecord(kind, key string, value any) (persistedRecord, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return persistedRecord{}, fmt.Errorf("sqlite repository %s record key is empty", kind)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return persistedRecord{}, fmt.Errorf("marshal sqlite repository %s %q: %w", kind, key, err)
	}
	sum := sha256.Sum256(payload)
	return persistedRecord{
		kind:     kind,
		key:      key,
		payload:  payload,
		checksum: hex.EncodeToString(sum[:]),
	}, nil
}

func loadState(ctx context.Context, database *sql.DB) (memory.State, error) {
	state := memory.State{Version: memory.StateVersion}
	var versionText string
	err := database.QueryRowContext(ctx, `SELECT meta_value FROM dnf_repository_meta WHERE meta_key = 'state_version'`).Scan(&versionText)
	if err != nil && err != sql.ErrNoRows {
		return memory.State{}, fmt.Errorf("read sqlite repository state version: %w", err)
	}
	if err == nil {
		version, parseErr := strconv.Atoi(versionText)
		if parseErr != nil || version != memory.StateVersion {
			return memory.State{}, fmt.Errorf("unsupported sqlite repository state version %q", versionText)
		}
		state.Version = version
	}
	rows, err := database.QueryContext(ctx, `SELECT record_kind, record_key, payload, checksum FROM dnf_repository_records ORDER BY record_kind, record_key`)
	if err != nil {
		return memory.State{}, fmt.Errorf("query sqlite repository records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key, wantChecksum string
		var payload []byte
		if err := rows.Scan(&kind, &key, &payload, &wantChecksum); err != nil {
			return memory.State{}, fmt.Errorf("scan sqlite repository record: %w", err)
		}
		sum := sha256.Sum256(payload)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantChecksum) {
			return memory.State{}, fmt.Errorf("sqlite repository checksum mismatch for %s %q", kind, key)
		}
		if err := appendStateRecord(&state, kind, key, payload); err != nil {
			return memory.State{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return memory.State{}, fmt.Errorf("iterate sqlite repository records: %w", err)
	}
	return state, nil
}

func appendStateRecord(state *memory.State, kind, key string, payload []byte) error {
	decode := func(target any) error {
		if err := json.Unmarshal(payload, target); err != nil {
			return fmt.Errorf("decode sqlite repository %s %q: %w", kind, key, err)
		}
		return nil
	}
	switch kind {
	case kindAccount:
		var record repository.AccountRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Accounts = append(state.Accounts, record)
	case kindAccountInventory:
		var record repository.AccountInventoryRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.AccountInventories = append(state.AccountInventories, record)
	case kindCharacter:
		var record repository.CharacterRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Characters = append(state.Characters, record)
	case kindInventory:
		var record repository.InventoryRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Inventories = append(state.Inventories, record)
	case kindEquipment:
		var record repository.EquipmentRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Equipment = append(state.Equipment, record)
	case kindPet:
		var record repository.PetRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Pets = append(state.Pets, record)
	case kindQuest:
		var record repository.QuestRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Quests = append(state.Quests, record)
	case kindSkill:
		var record repository.SkillRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Skills = append(state.Skills, record)
	case kindDungeonPermission:
		var record repository.DungeonPermissionRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.DungeonPermissions = append(state.DungeonPermissions, record)
	case kindHellPartyPermission:
		var record repository.HellPartyPermissionRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.HellPartyPermissions = append(state.HellPartyPermissions, record)
	case kindPacketTemplate:
		var record repository.PacketTemplateRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.PacketTemplates = append(state.PacketTemplates, record)
	case kindSettings:
		var record repository.SettingsRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Settings = append(state.Settings, record)
	case kindMailbox:
		var record repository.MailboxRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.Mailboxes = append(state.Mailboxes, record)
	case kindEpicBook:
		var record repository.EpicBookRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.EpicBooks = append(state.EpicBooks, record)
	case kindUnitedFriend:
		var record repository.UnitedFriendRecord
		if err := decode(&record); err != nil {
			return err
		}
		state.UnitedFriends = append(state.UnitedFriends, record)
	default:
		return fmt.Errorf("unsupported sqlite repository record kind %q", kind)
	}
	return nil
}

func persistTransition(ctx context.Context, database *sql.DB, before, after memory.State) error {
	oldRecords, err := stateRecords(before)
	if err != nil {
		return err
	}
	newRecords, err := stateRecords(after)
	if err != nil {
		return err
	}
	return persistRecordTransition(ctx, database, oldRecords, newRecords)
}

// persistCharacterAssetsTransition writes only the durable records covered by
// CharacterAssetUnitOfWork.  The old side intentionally comes from the last
// persisted state rather than the live runtime: an earlier async mutation may
// still be dirty, and this synchronous asset commit must include the current
// character-owned values without flushing unrelated records.
func persistCharacterAssetsTransition(
	ctx context.Context,
	database *sql.DB,
	persisted memory.State,
	after characterAssetsSnapshot,
) error {
	oldRecords, err := characterAssetsStateRecords(persisted, after.character.CharacterID)
	if err != nil {
		return err
	}
	newRecords, err := characterAssetsSnapshotRecords(after)
	if err != nil {
		return err
	}
	return persistRecordTransition(ctx, database, oldRecords, newRecords)
}

func characterAssetsStateRecords(state memory.State, characterID string) (map[string]persistedRecord, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return nil, fmt.Errorf("sqlite character-asset record key is empty")
	}
	out := make(map[string]persistedRecord, 3)
	for _, record := range state.Characters {
		if record.CharacterID != characterID {
			continue
		}
		persisted, err := makePersistedRecord(kindCharacter, record.CharacterID, record)
		if err != nil {
			return nil, err
		}
		out[persisted.kind+"\x00"+persisted.key] = persisted
		break
	}
	for _, record := range state.Inventories {
		if record.CharacterID != characterID {
			continue
		}
		persisted, err := makePersistedRecord(kindInventory, record.CharacterID, record)
		if err != nil {
			return nil, err
		}
		out[persisted.kind+"\x00"+persisted.key] = persisted
		break
	}
	for _, record := range state.Equipment {
		if record.CharacterID != characterID {
			continue
		}
		persisted, err := makePersistedRecord(kindEquipment, record.CharacterID, record)
		if err != nil {
			return nil, err
		}
		out[persisted.kind+"\x00"+persisted.key] = persisted
		break
	}
	return out, nil
}

func characterAssetsSnapshotRecords(snapshot characterAssetsSnapshot) (map[string]persistedRecord, error) {
	out := make(map[string]persistedRecord, 3)
	appendRecord := func(kind string, found bool, key string, value any) error {
		if !found {
			return nil
		}
		persisted, err := makePersistedRecord(kind, key, value)
		if err != nil {
			return err
		}
		out[persisted.kind+"\x00"+persisted.key] = persisted
		return nil
	}
	if err := appendRecord(kindCharacter, snapshot.characterFound, snapshot.character.CharacterID, snapshot.character); err != nil {
		return nil, err
	}
	if err := appendRecord(kindInventory, snapshot.inventoryFound, snapshot.inventory.CharacterID, snapshot.inventory); err != nil {
		return nil, err
	}
	if err := appendRecord(kindEquipment, snapshot.equipmentFound, snapshot.equipment.CharacterID, snapshot.equipment); err != nil {
		return nil, err
	}
	return out, nil
}

func persistRecordTransition(
	ctx context.Context,
	database *sql.DB,
	oldRecords map[string]persistedRecord,
	newRecords map[string]persistedRecord,
) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite repository transaction: %w", err)
	}
	defer tx.Rollback()
	for id, oldRecord := range oldRecords {
		if _, ok := newRecords[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dnf_repository_records WHERE record_kind = ? AND record_key = ?`, oldRecord.kind, oldRecord.key); err != nil {
			return fmt.Errorf("delete sqlite repository %s %q: %w", oldRecord.kind, oldRecord.key, err)
		}
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for id, record := range newRecords {
		if oldRecord, ok := oldRecords[id]; ok && oldRecord.checksum == record.checksum {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO dnf_repository_records (record_kind, record_key, payload, checksum, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(record_kind, record_key) DO UPDATE SET
    payload = excluded.payload,
    checksum = excluded.checksum,
    updated_at = excluded.updated_at`, record.kind, record.key, record.payload, record.checksum, updatedAt); err != nil {
			return fmt.Errorf("upsert sqlite repository %s %q: %w", record.kind, record.key, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO dnf_repository_meta (meta_key, meta_value) VALUES ('state_version', ?)
ON CONFLICT(meta_key) DO UPDATE SET meta_value = excluded.meta_value`, strconv.Itoa(memory.StateVersion)); err != nil {
		return fmt.Errorf("write sqlite repository state version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite repository transaction: %w", err)
	}
	return nil
}
