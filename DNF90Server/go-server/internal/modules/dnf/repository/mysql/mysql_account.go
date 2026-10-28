// 本文件实现 DNF 账号仓储的 MySQL 读写。
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"longheng.io/server/internal/modules/dnf/repository"
	"sort"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
	"longheng.io/server/internal/platform/db"
)

const (
	mysqlAccountTable                  = "accounts"
	mysqlLoginAccountTable             = "login_accounts"
	mysqlAccountHonorColumn            = "honor_exp"
	mysqlAccountHonorDDL               = "BIGINT UNSIGNED NOT NULL DEFAULT 0"
	mysqlAccountRepresentNameColumn    = "represent_account_name"
	mysqlAccountRepresentNameDDL       = "VARCHAR(64) NULL"
	mysqlAccountRepresentNameUniqueKey = "uk_dnf_accounts_represent_account_name"
)

func (s *mysqlAccountStore) FindAccountIDByLoginUsername(ctx context.Context, username string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "", false, nil
	}
	for _, database := range s.router.readDBs {
		table := mysqlTable(database, s.router.tablePrefix, mysqlLoginAccountTable)
		query := "SELECT account_id FROM " + table + " WHERE username = ? AND state = 'active' LIMIT 1"
		var accountID string
		err := s.router.db.QueryRowContext(ctx, query, username).Scan(&accountID)
		if err == nil {
			return strings.TrimSpace(accountID), true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return "", false, err
	}
	return "", false, nil
}

type mysqlAccountStore struct {
	mysqlStoreBase
}

func (s *mysqlAccountStore) ListAccounts(ctx context.Context, search string, limit int) ([]repository.AccountRecord, error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	search = strings.TrimSpace(search)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	pattern := "%" + search + "%"
	records := make([]repository.AccountRecord, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, database := range s.router.readDBs {
		table := mysqlTable(database, s.router.tablePrefix, mysqlAccountTable)
		query := "SELECT account_id, state, honor_exp, represent_account_name, created_at, updated_at FROM " + table
		args := []any{}
		if search != "" {
			query += " WHERE account_id LIKE ? OR represent_account_name LIKE ?"
			args = append(args, pattern, pattern)
		}
		query += " ORDER BY updated_at DESC, account_id ASC LIMIT ?"
		args = append(args, limit)
		rows, err := s.router.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var record repository.AccountRecord
			var representName sql.NullString
			var createdAt, updatedAt sql.NullTime
			if err := rows.Scan(&record.AccountID, &record.State, &record.HonorExp, &representName, &createdAt, &updatedAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, duplicate := seen[record.AccountID]; duplicate {
				continue
			}
			seen[record.AccountID] = struct{}{}
			if representName.Valid {
				record.RepresentAccountName = strings.TrimSpace(representName.String)
			}
			record.CreatedAt = scanTime(createdAt)
			record.UpdatedAt = scanTime(updatedAt)
			records = append(records, record)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].UpdatedAt.After(records[j].UpdatedAt)
		}
		return records[i].AccountID < records[j].AccountID
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

// Load 按账号 ID 从 MySQL 读取 DNF 账号记录。
func (s *mysqlAccountStore) Load(ctx context.Context, accountID string) (repository.AccountRecord, bool, error) {
	table, err := s.router.readTable(mysqlAccountTable, accountID)
	if err != nil {
		return repository.AccountRecord{}, false, err
	}
	metadataTable, err := s.router.readTable(mysqlAccountMetadataTable, accountID)
	if err != nil {
		return repository.AccountRecord{}, false, err
	}
	query := s.router.selectQuery("SELECT account_id, state, honor_exp, represent_account_name, created_at, updated_at FROM " + table + " WHERE account_id = ?")
	var record repository.AccountRecord
	var representName sql.NullString
	var createdAt, updatedAt sql.NullTime
	err = s.router.db.QueryRowContext(ctx, query, accountID).Scan(
		&record.AccountID,
		&record.State,
		&record.HonorExp,
		&representName,
		&createdAt,
		&updatedAt,
	)
	if err != nil {
		ok, scanErr := scanErr(err)
		return repository.AccountRecord{}, ok, scanErr
	}
	if representName.Valid {
		record.RepresentAccountName = strings.TrimSpace(representName.String)
	}
	record.Metadata, err = loadStringMap(
		ctx,
		s.router.db,
		s.router.selectQuery("SELECT entry_key, entry_value FROM "+metadataTable+" WHERE account_id = ? ORDER BY entry_key"),
		accountID,
	)
	if err != nil {
		return repository.AccountRecord{}, false, err
	}
	record.CreatedAt = scanTime(createdAt)
	record.UpdatedAt = scanTime(updatedAt)
	return repository.CloneAccount(record), true, nil
}

// Save 保存完整 DNF 账号记录到 MySQL。
func (s *mysqlAccountStore) Save(ctx context.Context, record repository.AccountRecord) error {
	accountID, err := requireRecordKey(repository.AccountKey, record, "account")
	if err != nil {
		return err
	}
	table, err := s.router.writeTable(mysqlAccountTable, accountID)
	if err != nil {
		return err
	}
	metadataTable, err := s.router.writeTable(mysqlAccountMetadataTable, accountID)
	if err != nil {
		return err
	}
	updatedAt := timeOrNow(record.UpdatedAt, s.router.now)
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = updatedAt
	}
	representName := strings.TrimSpace(record.RepresentAccountName)
	var representNameArg any
	if representName != "" {
		representNameArg = representName
	}
	columns := []string{"account_id", "state", mysqlAccountHonorColumn, mysqlAccountRepresentNameColumn, "created_at", "updated_at"}
	args := []any{accountID, record.State, record.HonorExp, representNameArg, sqlTime(createdAt), updatedAt}
	updates := []string{
		updateValue("state"),
		updateValue(mysqlAccountHonorColumn),
		updateValue(mysqlAccountRepresentNameColumn),
		keepCreatedAt("created_at"),
		updateValue("updated_at"),
	}
	err = withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		if _, execErr := database.ExecContext(ctx, buildUpsert(table, columns, updates), args...); execErr != nil {
			return execErr
		}
		return replaceStringMap(ctx, database, metadataTable, "account_id", accountID, record.Metadata)
	})
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		return repository.ErrRepresentAccountNameExists
	}
	return err
}

func (s *mysqlAccountStore) SaveMetadataEntry(
	ctx context.Context,
	accountID string,
	key string,
	value string,
	updatedAt time.Time,
) error {
	accountID = strings.TrimSpace(accountID)
	key = strings.TrimSpace(key)
	if accountID == "" {
		return db.ErrRecordKeyRequired
	}
	if key == "" {
		return repository.ErrAccountMetadataKeyRequired
	}
	table, err := s.router.writeTable(mysqlAccountTable, accountID)
	if err != nil {
		return err
	}
	metadataTable, err := s.router.writeTable(mysqlAccountMetadataTable, accountID)
	if err != nil {
		return err
	}
	updatedAt = timeOrNow(updatedAt, s.router.now)
	return withMySQLWriteExecutor(ctx, s.router.db, func(database SQLDB) error {
		updateQuery := "UPDATE " + table + " SET updated_at = ? WHERE account_id = ?"
		if _, err := database.ExecContext(ctx, updateQuery, updatedAt, accountID); err != nil {
			return err
		}
		columns := []string{"account_id", "entry_key", "entry_value"}
		updates := []string{updateValue("entry_value")}
		_, err := database.ExecContext(
			ctx,
			buildUpsert(metadataTable, columns, updates),
			accountID,
			key,
			value,
		)
		return err
	})
}

func (s *mysqlAccountStore) FindAccountIDByPVPTotalTeamName(ctx context.Context, name string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	for _, database := range s.router.readDBs {
		table := mysqlTable(database, s.router.tablePrefix, mysqlAccountMetadataTable)
		query := "SELECT account_id FROM " + table + " WHERE entry_key = ? AND LOWER(JSON_UNQUOTE(JSON_EXTRACT(entry_value, '$.name'))) = LOWER(?) LIMIT 1"
		var accountID string
		err := s.router.db.QueryRowContext(ctx, query, "pvp_total_match_team_v1", name).Scan(&accountID)
		if err == nil {
			return accountID, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, err
		}
	}
	return "", false, nil
}

func (s *mysqlAccountStore) FindAccountIDByRepresentName(ctx context.Context, name string) (string, bool, error) {
	if err := ctxErr(ctx); err != nil {
		return "", false, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false, nil
	}
	for _, database := range s.router.readDBs {
		table := mysqlTable(database, s.router.tablePrefix, mysqlAccountTable)
		query := "SELECT account_id FROM " + table + " WHERE represent_account_name = ? LIMIT 1"
		var accountID string
		err := s.router.db.QueryRowContext(ctx, query, name).Scan(&accountID)
		if err == nil {
			return accountID, true, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return "", false, err
	}
	return "", false, nil
}
