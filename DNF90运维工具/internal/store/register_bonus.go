// Package store 注册赠送点券待发放辅助。
// 注册时只登记 pending，实际入账延迟到玩家首次上线角色后，
// 由泡点循环走游戏服务端 GM 接口 /local/dnf-gm/wallet/update 发放，
// 避免注册时直写 dnf_account_metadata 被游戏建号初始化覆盖。
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RegisterBonusPending 注册赠送待发放行。
type RegisterBonusPending struct {
	AccountID string
	Points    int
	State     string
	CreatedAt time.Time
	GrantedAt sql.NullTime
}

// MarkRegisterBonusPending 登记一笔待发放的注册赠送点券（幂等：已存在则重置为待发并更新点数）。
func (s *Store) MarkRegisterBonusPending(ctx context.Context, accountID string, points int) error {
	if s == nil || s.db == nil {
		return errors.New("store not open")
	}
	if points <= 0 {
		return errors.New("points must be positive")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO launcher_register_bonus_pending (account_id, points, state, created_at)
		 VALUES (?, ?, 'pending', NOW(6)) AS new
		 ON DUPLICATE KEY UPDATE points = new.points, state = 'pending', granted_at = NULL`,
		accountID, points)
	return err
}

// ListRegisterBonusPending 返回所有待发放的注册赠送记录。
func (s *Store) ListRegisterBonusPending(ctx context.Context) ([]RegisterBonusPending, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("store not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, points, state, created_at, granted_at
		 FROM launcher_register_bonus_pending
		 WHERE state = 'pending'
		 ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RegisterBonusPending
	for rows.Next() {
		var p RegisterBonusPending
		if err := rows.Scan(&p.AccountID, &p.Points, &p.State, &p.CreatedAt, &p.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkRegisterBonusGranted 将待发放记录标记为已发放。
func (s *Store) MarkRegisterBonusGranted(ctx context.Context, accountID string) error {
	if s == nil || s.db == nil {
		return errors.New("store not open")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE launcher_register_bonus_pending
		 SET state = 'granted', granted_at = NOW(6)
		 WHERE account_id = ? AND state = 'pending'`,
		accountID)
	return err
}
