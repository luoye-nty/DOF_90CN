// GM 踢人事件与在线会话查询：封禁踢下线 → 事件落库（拉模式下发登录器）、
// 登录器心跳续期（launcher_sessions.expires_at 顺延）后用于推导"当前在线账号"。
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// GMKickEvent GM 踢人事件行（登录器按账号轮询的载荷）。
type GMKickEvent struct {
	ID        int64
	AccountID string
	Username  string
	EventType string
	Reason    string
	Operator  string
	CreatedAt time.Time
}

// OnlineAccount 在线账号行：基于未过期会话（登录器心跳续期）+ 账号冗余信息。
type OnlineAccount struct {
	AccountID string
	Username  string
	State     string
	LastIP    string
	MachineID string
	IssuedAt  sql.NullTime
	ExpiresAt time.Time
}

// CreateGMKickEvent 写入一条账号定向事件（登录器拉取/长轮询后处理）。
//
// 落库成功后会唤醒正在为这个账号长轮询的登录器（见 Store.gmEventNotifier）——
// 这是"到账绿字秒级弹出"的关键：否则登录器要等下一个轮询周期（10 秒）才看得到。
// 通知失败/没人在等都不影响写入结果。
func (s *Store) CreateGMKickEvent(ctx context.Context, accountID, eventType, reason, operator string) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, errors.New("gm-event: empty account_id")
	}
	if strings.TrimSpace(eventType) == "" {
		eventType = "kick"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO launcher_gm_events (account_id, event_type, reason, operator, created_at)
		 VALUES (?, ?, ?, ?, NOW(6))`,
		accountID, eventType, nullString(reason), nullString(operator))
	if err != nil {
		return 0, err
	}
	if s.gmEventNotifier != nil {
		s.gmEventNotifier(accountID)
	}
	return res.LastInsertId()
}

// ListGMKickEventsAfter 返回某账号 id>afterID 且 created_at>=since 的事件（升序，limit<=0 取 20）。
//
// since 为零值时不过滤时间（老调用方的行为不变）。
//
// ── 为什么要加 since（2026-09-28 修真实线上问题）────────────────────────────
// 登录器原来用一个**跨会话持久化**的 id 游标（after_id）。这个游标一旦比服务端
// 现有事件的 id 还大（典型场景：数据库重建/清表/换库，id 从 1 重新开始），
// `id > after_id` 就永远返回空 —— 登录器表现为"轮询正常、没有报错、但永远收不到
// 任何事件"，泡点到账提示再也不弹，而且查不到任何痕迹。实测踩到：游标停在 56232，
// 而库里的新事件 id 只有个位数。
//
// 加上 since 之后，登录器每次会话只要带上"本次启动时刻"，就等于自带自愈：
// 游标被毒化也照样能按时间捞到新事件，不需要任何人工干预。
func (s *Store) ListGMKickEventsAfter(ctx context.Context, accountID string, afterID int64, since time.Time, limit int) ([]GMKickEvent, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("gm-event: empty account_id")
	}
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT e.id, e.account_id, COALESCE(a.username,''), e.event_type,
		        COALESCE(e.reason,''), COALESCE(e.operator,''), e.created_at
		 FROM launcher_gm_events e
		 LEFT JOIN launcher_accounts a ON a.account_id = e.account_id
		 WHERE e.account_id = ? AND e.id > ?`
	args := []any{accountID, afterID}
	if !since.IsZero() {
		query += ` AND e.created_at >= ?`
		args = append(args, since)
	}
	query += ` ORDER BY e.id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GMKickEvent
	for rows.Next() {
		var e GMKickEvent
		if err := rows.Scan(&e.ID, &e.AccountID, &e.Username, &e.EventType,
			&e.Reason, &e.Operator, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// HeartbeatTicket 登录器周期心跳：顺延当前 ticket 的过期时间（会话续期）。
// 返回是否命中（ticket 无效/已过期返回 false，不报错）。
func (s *Store) HeartbeatTicket(ctx context.Context, ticket string, ttl time.Duration) (bool, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return false, nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE launcher_sessions SET expires_at = ? WHERE ticket_hash = ? AND expires_at > NOW(6)`,
		time.Now().Add(ttl), Sha256Hex(ticket))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ResolveAccountIDByTicket 轻量解析 ticket → account_id（仅校验会话存在且未过期，不验封禁；
// 被封禁账号仍需能收到 GM 踢人事件以关闭本地游戏进程）。
func (s *Store) ResolveAccountIDByTicket(ctx context.Context, ticket string) (string, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", ErrTicketNotFound
	}
	var accountID string
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM launcher_sessions WHERE ticket_hash = ? AND expires_at > NOW(6)`,
		Sha256Hex(ticket)).Scan(&accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrTicketNotFound
		}
		return "", err
	}
	return accountID, nil
}

// ListOnlineAccounts 在线账号列表：存在未过期会话（含心跳续期）的账号，
// 按账号聚合取最近会话的 issued_at / machine_id。
func (s *Store) ListOnlineAccounts(ctx context.Context, limit int) ([]OnlineAccount, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts.account_id, COALESCE(a.username,''), COALESCE(a.state,'active'),
		        COALESCE(a.last_login_ip,''),
		        COALESCE((SELECT s2.machine_id FROM launcher_sessions s2
		                  WHERE s2.account_id = ts.account_id
		                  ORDER BY s2.issued_at DESC LIMIT 1), ''),
		        MAX(ts.issued_at), MAX(ts.expires_at)
		 FROM launcher_sessions ts
		 LEFT JOIN launcher_accounts a ON a.account_id = ts.account_id
		 WHERE ts.expires_at > NOW(6)
		 GROUP BY ts.account_id, a.username, a.state, a.last_login_ip
		 ORDER BY MAX(ts.issued_at) DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OnlineAccount
	for rows.Next() {
		var o OnlineAccount
		if err := rows.Scan(&o.AccountID, &o.Username, &o.State, &o.LastIP,
			&o.MachineID, &o.IssuedAt, &o.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
