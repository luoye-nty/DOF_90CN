// 封禁（launcher_bans）数据访问：支持账号 / IP / 机器码三种维度，
// 可自定义时长（days>0 到期自动失效，days<=0 永久），支持手动解封（置 revoked_at）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 封禁维度。
const (
	BanAccount = "account"
	BanIP      = "ip"
	BanMachine = "machine"
)

func validBanType(t string) bool {
	return t == BanAccount || t == BanIP || t == BanMachine
}

// BanRecord 封禁行。
type BanRecord struct {
	ID        int64
	BanType   string
	BanValue  string
	Username  string // 冗余展示：account 类型记 username，其他类型记命中账号（若有）
	Reason    string
	Operator  string
	BannedAt  time.Time
	ExpiresAt sql.NullTime
	RevokedAt sql.NullTime
}

// Active 判断当前是否仍处于生效封禁（未手动解封且未到期）。
func (b BanRecord) Active(now time.Time) bool {
	if b.RevokedAt.Valid {
		return false
	}
	if b.ExpiresAt.Valid && !b.ExpiresAt.Time.After(now) {
		return false
	}
	return true
}

// StateLabel 返回面向 UI 的状态文案。
func (b BanRecord) StateLabel(now time.Time) string {
	if b.RevokedAt.Valid {
		return "已解封"
	}
	if b.ExpiresAt.Valid && !b.ExpiresAt.Time.After(now) {
		return "已到期"
	}
	return "生效中"
}

// MachineIdentity 登录器上报的机器标识。
//
//	MachineID —— 新硬件码（hw:<32hex>，登录器 Services/HardwareId.cs 算的）；
//	             老登录器上仍然是计算机名，所以两个字段都可能相同。
//	Name      —— 计算机名（即 2026-09-24 之前的旧机器码）。
//
// 为什么两个都要：库里历史 machine 维度封禁的 ban_value 是按**计算机名**写的，
// 换成硬件码后那些行会失配。匹配时硬件码优先、旧计算机名兜底，旧封禁继续生效。
type MachineIdentity struct {
	MachineID string
	Name      string
}

// empty 两个字段都为空视为"没有机器信息"（游戏服验票帧里可能不带）。
func (m MachineIdentity) empty() bool {
	return len(machineBanCandidates(m)) == 0
}

// machineBanCandidates 机器维度封禁要比对的候选值，按优先级排列：上报值（新硬件码）→ 计算机名。
// 去重（老登录器两个字段相同）并跳过空值；纯函数，便于单测。
func machineBanCandidates(m MachineIdentity) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		for _, seen := range out {
			if strings.EqualFold(seen, v) {
				return
			}
		}
		out = append(out, v)
	}
	add(m.MachineID)
	add(m.Name)
	return out
}

// HasActiveBanForMachine 机器维度封禁匹配：先按上报值查，未命中再用计算机名查一次。
// 每一跳都是精确匹配（ban_value 是唯一键），只是候选值可能有两个。
func (s *Store) HasActiveBanForMachine(ctx context.Context, machine MachineIdentity) (bool, error) {
	for _, value := range machineBanCandidates(machine) {
		hit, err := s.HasActiveBan(ctx, BanMachine, value)
		if err != nil {
			return false, err
		}
		if hit {
			return true, nil
		}
	}
	return false, nil
}

// checkBans 按优先级检查三层封禁：账号 > 机器码 > IP。命中返回对应哨兵，未命中返回 nil。
func (s *Store) checkBans(ctx context.Context, accountID string, machine MachineIdentity, remoteIP string) error {
	accountID = strings.TrimSpace(accountID)
	remoteIP = strings.TrimSpace(remoteIP)

	if accountID != "" {
		hit, err := s.HasActiveBan(ctx, BanAccount, accountID)
		if err != nil {
			return err
		}
		if hit {
			return ErrBanned
		}
	}
	if !machine.empty() {
		hit, err := s.HasActiveBanForMachine(ctx, machine)
		if err != nil {
			return err
		}
		if hit {
			return ErrMachineBanned
		}
	}
	if remoteIP != "" {
		hit, err := s.HasActiveBan(ctx, BanIP, remoteIP)
		if err != nil {
			return err
		}
		if hit {
			return ErrIPBanned
		}
	}
	return nil
}

// HasActiveBan 查询某维度某值是否存在生效中封禁。
func (s *Store) HasActiveBan(ctx context.Context, banType, banValue string) (bool, error) {
	banValue = strings.TrimSpace(banValue)
	if !validBanType(banType) || banValue == "" {
		return false, ErrBanTargetInvalid
	}
	var exists int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(
		   SELECT 1 FROM launcher_bans
		   WHERE ban_type=? AND ban_value=?
		     AND revoked_at IS NULL
		     AND (expires_at IS NULL OR expires_at > NOW(6))
		 )`,
		banType, banValue).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists == 1, nil
}

// ListAccountsPage 按 account_id 升序分页取账号（群发邮件/在线福利这类"全服遍历"用）。
//
// 为什么不复用 ListAccounts：那个是给运营选号用的（按注册时间倒序、上限 300、支持模糊），
// 分页遍历要的是稳定顺序 + 不丢不漏，所以单独给一个按主键升序的分页查询。
func (s *Store) ListAccountsPage(ctx context.Context, offset, limit int) ([]Account, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 300
	}
	if limit > 2000 {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, username, state, COALESCE(last_login_ip,''), last_login_at, created_at,
		        COALESCE((SELECT s.machine_id FROM launcher_sessions s
		                  WHERE s.account_id=a.account_id ORDER BY s.issued_at DESC LIMIT 1), '')
		 FROM launcher_accounts a
		 ORDER BY account_id ASC
		 LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.AccountID, &a.Username, &a.State, &a.LastLoginIP,
			&a.LastLoginAt, &a.CreatedAt, &a.MachineID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Ban 封禁目标：days<=0 表示永久；同目标已存在记录（含已解封/已到期）时重置复用。
func (s *Store) Ban(ctx context.Context, banType, banValue, username, reason, operator string, days int) (*BanRecord, error) {
	banType = strings.TrimSpace(banType)
	banValue = strings.TrimSpace(banValue)
	username = strings.TrimSpace(username)
	reason = strings.TrimSpace(reason)
	operator = strings.TrimSpace(operator)
	if !validBanType(banType) || banValue == "" {
		return nil, ErrBanTargetInvalid
	}

	var expires any
	if days > 0 {
		expires = time.Now().AddDate(0, 0, days)
	} else {
		expires = nil
	}
	// 复用唯一键：对已存在目标（含历史已失效行）整体重置。
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO launcher_bans (ban_type, ban_value, username, reason, operator, banned_at, expires_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?, NOW(6), ?, NULL)
		 ON DUPLICATE KEY UPDATE
		   username=VALUES(username),
		   reason=VALUES(reason),
		   operator=VALUES(operator),
		   banned_at=NOW(6),
		   expires_at=VALUES(expires_at),
		   revoked_at=NULL`,
		banType, banValue, nullString(username), nullString(reason), nullString(operator), expires)
	if err != nil {
		return nil, err
	}
	return s.getBan(ctx, banType, banValue)
}

// Unban 手动解封：置 revoked_at=NOW(6)。返回是否确有生效记录被解封。
func (s *Store) Unban(ctx context.Context, banType, banValue string) (bool, error) {
	banType = strings.TrimSpace(banType)
	banValue = strings.TrimSpace(banValue)
	if !validBanType(banType) || banValue == "" {
		return false, ErrBanTargetInvalid
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE launcher_bans SET revoked_at=NOW(6)
		 WHERE ban_type=? AND ban_value=? AND revoked_at IS NULL`, banType, banValue)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// BanTarget 解封目标：一条具体记录（类型 + 值）。
type BanTarget struct {
	BanType  string
	BanValue string
}

// UnbanTargets 批量解封：逐条按「类型+值」置 revoked_at，返回本次真正从
// "生效中"变为"已解封"的记录。
//
// 为什么不写成一条 UPDATE ... WHERE (ban_type,ban_value) IN (...)：
//  1) 账号型解封会连带解除该账号的 IP/机器码封禁，运营必须看到"到底解除了哪几条"，
//     而一条 UPDATE 的 RowsAffected 给不出这个清单；
//  2) 复用已验证的单条 Unban()，不再新增第二套写库路径（避免"修了 A 坏了 B"）。
//
// 重复目标自动去重，非法/空目标跳过；任一条写库失败立即返回（已解封的不回滚，
// 与单条 Unban 一致：解封本身是幂等的安全方向）。
func (s *Store) UnbanTargets(ctx context.Context, targets []BanTarget) ([]BanRecord, error) {
	seen := make(map[BanTarget]struct{}, len(targets))
	lifted := make([]BanRecord, 0, len(targets))
	for _, t := range targets {
		bt := strings.TrimSpace(t.BanType)
		bv := strings.TrimSpace(t.BanValue)
		if !validBanType(bt) || bv == "" {
			continue
		}
		key := BanTarget{BanType: bt, BanValue: bv}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		ok, err := s.Unban(ctx, bt, bv)
		if err != nil {
			return lifted, err
		}
		if !ok {
			continue // 没有生效中的记录（不存在 / 已解封）
		}
		rec, err := s.getBan(ctx, bt, bv)
		if err != nil {
			return lifted, err
		}
		lifted = append(lifted, *rec)
	}
	return lifted, nil
}

// ListActiveIPMachineBansByUsername 返回某账号名下仍生效的 IP / 机器码封禁。
//
// 为什么按 username 反查：封禁写入时每一条都带上了 username 冗余列
// （见 ban_tab.go / remote_tab.go 的三封分支），username 是"这行属于哪个账号"
// 的唯一可靠线索；账号型解封要连带解除的就是这些行。
func (s *Store) ListActiveIPMachineBansByUsername(ctx context.Context, username string) ([]BanRecord, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ban_type, ban_value, COALESCE(username,''), COALESCE(reason,''), COALESCE(operator,''),
		        banned_at, expires_at, revoked_at
		 FROM launcher_bans
		 WHERE username = ? AND ban_type IN (?, ?)
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > NOW(6))
		 ORDER BY banned_at DESC`,
		username, BanIP, BanMachine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BanRecord
	for rows.Next() {
		var b BanRecord
		if err := rows.Scan(&b.ID, &b.BanType, &b.BanValue, &b.Username, &b.Reason, &b.Operator,
			&b.BannedAt, &b.ExpiresAt, &b.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BanLookup 按 类型+值 查一行（含已解封的历史行），用于给"解封失败"一个准确原因：
// 「记录根本不存在」和「已经解封过」以前都只报 no-active-ban-found(404)，运营看不出区别。
func (s *Store) BanLookup(ctx context.Context, banType, banValue string) (exists bool, revoked bool, err error) {
	banType = strings.TrimSpace(banType)
	banValue = strings.TrimSpace(banValue)
	if !validBanType(banType) || banValue == "" {
		return false, false, ErrBanTargetInvalid
	}
	var revokedAt sql.NullTime
	row := s.db.QueryRowContext(ctx,
		`SELECT revoked_at FROM launcher_bans WHERE ban_type=? AND ban_value=?`, banType, banValue)
	if scanErr := row.Scan(&revokedAt); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, scanErr
	}
	return true, revokedAt.Valid, nil
}

// ListBans 按封禁时间倒序返回封禁记录（含已解封/已到期历史），limit<=0 时取 300 条。
func (s *Store) ListBans(ctx context.Context, limit int) ([]BanRecord, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ban_type, ban_value, COALESCE(username,''), COALESCE(reason,''), COALESCE(operator,''),
		        banned_at, expires_at, revoked_at
		 FROM launcher_bans
		 ORDER BY banned_at DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BanRecord
	for rows.Next() {
		var b BanRecord
		if err := rows.Scan(&b.ID, &b.BanType, &b.BanValue, &b.Username, &b.Reason, &b.Operator,
			&b.BannedAt, &b.ExpiresAt, &b.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListAccounts 列出账号（按用户名模糊或空=全部，取最近 300 条），供封禁管理选号。
// 附带最近一次登录的 machine_id（来自 launcher_sessions 最新票据）。
func (s *Store) ListAccounts(ctx context.Context, keyword string) ([]Account, error) {
	keyword = strings.TrimSpace(keyword)
	query := `SELECT a.account_id, a.username, a.state, COALESCE(a.last_login_ip,''), a.last_login_at, a.created_at,
	                 COALESCE((SELECT s.machine_id FROM launcher_sessions s
	                           WHERE s.account_id=a.account_id ORDER BY s.issued_at DESC LIMIT 1), '')
	          FROM launcher_accounts a`
	args := []any{}
	if keyword != "" {
		query += ` WHERE a.username LIKE ?`
		args = append(args, "%"+keyword+"%")
	}
	query += ` ORDER BY a.created_at DESC LIMIT 300`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.AccountID, &a.Username, &a.State, &a.LastLoginIP,
			&a.LastLoginAt, &a.CreatedAt, &a.MachineID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) getBan(ctx context.Context, banType, banValue string) (*BanRecord, error) {
	var b BanRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ban_type, ban_value, COALESCE(username,''), COALESCE(reason,''), COALESCE(operator,''),
		        banned_at, expires_at, revoked_at
		 FROM launcher_bans WHERE ban_type=? AND ban_value=?`,
		banType, banValue).
		Scan(&b.ID, &b.BanType, &b.BanValue, &b.Username, &b.Reason, &b.Operator,
			&b.BannedAt, &b.ExpiresAt, &b.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("load ban: %w", err)
	}
	return &b, nil
}

func nullString(v string) any {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return v
}

// EnsureBansSchema 单独保证 launcher_bans 建表（存量库首次升级用）。
func (s *Store) EnsureBansSchema(ctx context.Context) error {
	if len(schemaDDL) == 0 {
		return nil
	}
	ddl := schemaDDL[len(schemaDDL)-1]
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ensure bans schema: %w", err)
	}
	return nil
}
