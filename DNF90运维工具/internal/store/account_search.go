package store

import (
	"context"
	"database/sql"
	"strings"
)

// AccountRow 是网页后台账号列表使用的轻量账号行。
//
// 不复用 Account 是因为网页只需要展示字段，不应该把密码摘要/救援码摘要
// 这类敏感列带到 JSON 层；查询也刻意不 SELECT 这些列。
type AccountRow struct {
	AccountID   string
	Username    string
	State       string
	LastLoginIP string
	LastLoginAt sql.NullTime
	CreatedAt   sql.NullTime
}

// SearchAccounts 按用户名或账号主键模糊查询，按注册时间倒序返回。
// keyword 为空时列出全部账号；limit<=0 取 300，最大 5000。
func (s *Store) SearchAccounts(ctx context.Context, keyword string, limit int) ([]AccountRow, error) {
	if limit <= 0 {
		limit = 300
	}
	if limit > 5000 {
		limit = 5000
	}
	keyword = strings.TrimSpace(keyword)
	query := `SELECT account_id, username, state, COALESCE(last_login_ip,''), last_login_at, created_at
	          FROM launcher_accounts`
	args := []any{}
	if keyword != "" {
		query += ` WHERE username LIKE ? OR account_id LIKE ?`
		args = append(args, "%"+keyword+"%", "%"+keyword+"%")
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AccountRow, 0, 16)
	for rows.Next() {
		var row AccountRow
		if err := rows.Scan(&row.AccountID, &row.Username, &row.State,
			&row.LastLoginIP, &row.LastLoginAt, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// CountBans 返回历史封禁总数与当前生效封禁数。
func (s *Store) CountBans(ctx context.Context) (total int64, active int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM launcher_bans`).Scan(&total)
	if err != nil {
		return 0, 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM launcher_bans
		WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW(6))`).Scan(&active)
	if err != nil {
		return 0, 0, err
	}
	return total, active, nil
}
