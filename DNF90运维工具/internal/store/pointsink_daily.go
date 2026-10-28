// 泡点每日限额台账：账号级、按自然日累计"通过泡点已发出去的点券"。
//
// 为什么要落库而不是放内存：泡点循环可能因为工具重启、配置改动、服务器重启而重新起算，
// 内存计数一重启就归零，限额就形同虚设（玩家重启一次工具就又能刷满一整天）。
//
// 口径：
//   - key = (account_id, day)，day 取**工具本机自然日**（YYYY-MM-DD，与运营看时钟一致）；
//   - granted 只累加"确认发放成功"的点券数（含超时后核对确认到账的）；
//   - 过期行会被顺手清理（保留 psDailyKeepDays 天），表不会无限长。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// psDailyKeepDays 每日台账保留天数（比"今天"早于这个天数的行会被清理）。
const psDailyKeepDays = 14

// AddPointsinkDailyGranted 给某账号当天累加已发放点券数（原子 UPSERT）。
//
// delta <= 0 直接忽略：调用方只应在"确认发放成功"后调用。
func (s *Store) AddPointsinkDailyGranted(ctx context.Context, accountID, day string, delta int64) error {
	accountID = strings.TrimSpace(accountID)
	day = strings.TrimSpace(day)
	if accountID == "" || day == "" {
		return errors.New("pointsink-daily: empty account_id or day")
	}
	if delta <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO launcher_pointsink_daily (account_id, day, granted, updated_at)
		 VALUES (?, ?, ?, NOW(6)) AS new
		 ON DUPLICATE KEY UPDATE granted = launcher_pointsink_daily.granted + new.granted, updated_at = NOW(6)`,
		accountID, day, delta)
	return err
}

// PointsinkDailyGranted 返回某账号当天已发放的点券数（没有记录 = 0）。
func (s *Store) PointsinkDailyGranted(ctx context.Context, accountID, day string) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	day = strings.TrimSpace(day)
	if accountID == "" || day == "" {
		return 0, errors.New("pointsink-daily: empty account_id or day")
	}
	var granted int64
	err := s.db.QueryRowContext(ctx,
		`SELECT granted FROM launcher_pointsink_daily WHERE account_id = ? AND day = ?`,
		accountID, day).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return granted, nil
}

// PrunePointsinkDaily 清理过期台账（保留最近 keepDays 天；keepDays<=0 用默认值）。
// 失败只返回错误，调用方按"尽力而为"处理（清理失败不影响发放）。
func (s *Store) PrunePointsinkDaily(ctx context.Context, today string, keepDays int) (int64, error) {
	today = strings.TrimSpace(today)
	if today == "" {
		return 0, errors.New("pointsink-daily: empty day")
	}
	if keepDays <= 0 {
		keepDays = psDailyKeepDays
	}
	cutoff := today
	if parsed, err := time.Parse("2006-01-02", today); err == nil {
		cutoff = parsed.AddDate(0, 0, -keepDays).Format("2006-01-02")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM launcher_pointsink_daily WHERE day < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}
