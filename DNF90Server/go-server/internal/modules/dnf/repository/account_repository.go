// 本文件定义 DNF 账号仓储接口和记录。
package repository

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/platform/db"
)

// AccountRepository 保存账号级可变状态。
type AccountRepository interface {
	db.Store[AccountRecord]
}

// AccountMetadataEntryWriter persists one normalized account-metadata row
// without replacing unrelated keys.
type AccountMetadataEntryWriter interface {
	SaveMetadataEntry(context.Context, string, string, string, time.Time) error
}

// PVPTotalTeamNameFinder checks the account-wide native duel team name.
type PVPTotalTeamNameFinder interface {
	FindAccountIDByPVPTotalTeamName(context.Context, string) (string, bool, error)
}

// LoginAccountUsernameFinder resolves the public local-launcher username to
// the opaque account ID used by DNF repositories. MySQL and SQLite own the
// same dnf_login_accounts mapping behind this database-neutral contract.
type LoginAccountUsernameFinder interface {
	FindAccountIDByLoginUsername(context.Context, string) (string, bool, error)
}

// RepresentAccountNameFinder exposes the account-wide nickname uniqueness
// lookup required by the current client's adventure-group registration flow.
// It is kept separate from AccountRepository so transaction-scoped stores do
// not need to support a cross-account query.
type RepresentAccountNameFinder interface {
	FindAccountIDByRepresentName(context.Context, string) (string, bool, error)
}

// AccountLister exposes the bounded cross-account lookup used by the
// authenticated loopback GM surface. Transaction-scoped repositories remain
// deliberately exempt from this capability.
type AccountLister interface {
	ListAccounts(context.Context, string, int) ([]AccountRecord, error)
}

var ErrRepresentAccountNameExists = errors.New("dnf represent account name already exists")

var (
	ErrAccountMetadataKeyRequired = errors.New("dnf account metadata key is required")
	ErrAccountMetadataUnavailable = errors.New("dnf account metadata repository is unavailable")
	ErrAccountListUnavailable     = errors.New("dnf account list repository is unavailable")
	ErrCharacterSlotLimitInvalid  = errors.New("dnf character slot limit is invalid")
)

const CharacterSlotLimitMetadataKey = "character_slot_limit"

// AccountRecord 是 DNF 账号仓储记录。
type AccountRecord struct {
	AccountID            string            `json:"account_id"`
	State                string            `json:"state,omitempty"`
	HonorExp             uint64            `json:"honor_exp,omitempty"`
	RepresentAccountName string            `json:"represent_account_name,omitempty"`
	Metadata             map[string]string `json:"metadata,omitempty"`
	CreatedAt            time.Time         `json:"created_at,omitempty"`
	UpdatedAt            time.Time         `json:"updated_at,omitempty"`
}

// CloneAccount 拷贝账号记录，避免调用方和底层存储共享 Metadata map。
func CloneAccount(record AccountRecord) AccountRecord {
	record.Metadata = cloneStringMap(record.Metadata)
	return record
}

func AccountCharacterSlotLimit(record AccountRecord) int {
	raw := strings.TrimSpace(record.Metadata[CharacterSlotLimitMetadataKey])
	if raw == "" {
		return DefaultCharacterSlotLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < DefaultCharacterSlotLimit || limit > MaxCharacterSlotLimit {
		return DefaultCharacterSlotLimit
	}
	return limit
}

func SetAccountCharacterSlotLimit(record *AccountRecord, limit int) error {
	if record == nil || limit < DefaultCharacterSlotLimit || limit > MaxCharacterSlotLimit {
		return ErrCharacterSlotLimitInvalid
	}
	if record.Metadata == nil {
		record.Metadata = make(map[string]string)
	}
	record.Metadata[CharacterSlotLimitMetadataKey] = strconv.Itoa(limit)
	return nil
}

// SaveAccountMetadataEntry updates one metadata key. MySQL and memory stores
// implement a native key-level write; other repositories fall back to saving
// the supplied authoritative account snapshot.
func SaveAccountMetadataEntry(
	ctx context.Context,
	repo AccountRepository,
	account AccountRecord,
	key string,
	value string,
	updatedAt time.Time,
) error {
	if repo == nil {
		return ErrAccountMetadataUnavailable
	}
	accountID := strings.TrimSpace(account.AccountID)
	if accountID == "" {
		return db.ErrRecordKeyRequired
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return ErrAccountMetadataKeyRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	updatedAt = updatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	if writer, ok := repo.(AccountMetadataEntryWriter); ok {
		return writer.SaveMetadataEntry(ctx, accountID, key, value, updatedAt)
	}
	account = CloneAccount(account)
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	account.Metadata[key] = value
	account.UpdatedAt = updatedAt
	return repo.Save(ctx, account)
}

func AccountKey(record AccountRecord) string {
	return strings.TrimSpace(record.AccountID)
}

func ListAccounts(ctx context.Context, repo AccountRepository, query string, limit int) ([]AccountRecord, error) {
	if repo == nil {
		return nil, ErrAccountListUnavailable
	}
	lister, ok := repo.(AccountLister)
	if !ok {
		return nil, ErrAccountListUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return lister.ListAccounts(ctx, strings.TrimSpace(query), limit)
}
