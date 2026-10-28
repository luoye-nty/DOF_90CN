// Package store 封装 DNF90 账号体系 MySQL 访问：
// launcher_accounts / launcher_invites / launcher_sessions 三张表（结构见设计文档 2.2）。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"dnf90ops/internal/config"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

// 业务错误哨兵（API 层转 JSON reason）。
var (
	ErrUsernameTaken    = errors.New("username-taken")
	ErrUserNotFound     = errors.New("user-not-found")
	ErrInvalidPassword  = errors.New("invalid-password")
	ErrBanned           = errors.New("account-banned")
	ErrMachineBanned    = errors.New("machine-banned")
	ErrIPBanned         = errors.New("ip-banned")
	ErrInvalidInvite    = errors.New("invalid-or-used-invite")
	ErrTicketNotFound   = errors.New("ticket-not-found")
	ErrTicketExpired    = errors.New("ticket-expired")
	ErrBanTargetInvalid = errors.New("ban-target-invalid")
	// 救援代码相关：账号未设置救援代码、代码格式非法或校验不匹配统一返回本哨兵。
	ErrInvalidRescueCode = errors.New("invalid-rescue-code")
	// 每 IP 注册数量超限（register_ip 计数达到配置上限）。
	ErrRegisterIPLimit = errors.New("register-ip-limit")
)

// schemaDDL 幂等建表语句，与设计文档 2.2 一致。
var schemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS launcher_accounts (
  account_id      VARCHAR(128) NOT NULL,
  username        VARCHAR(64)  NOT NULL,
  email           VARCHAR(128) NULL,
  password_hash   VARCHAR(100) NOT NULL,
  rescue_code_hash VARCHAR(100) NULL,
  state           VARCHAR(16)  NOT NULL DEFAULT 'active',
  last_login_ip   VARCHAR(64)  NULL,
  register_ip     VARCHAR(64)  NULL,
  last_login_at   DATETIME(6)  NULL,
  created_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (account_id),
  UNIQUE KEY uk_launcher_accounts_username (username),
  KEY idx_launcher_accounts_register_ip (register_ip)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS launcher_invites (
  invite_code   VARCHAR(64) NOT NULL,
  state         VARCHAR(16) NOT NULL DEFAULT 'unused',
  used_by       VARCHAR(128) NULL,
  used_at       DATETIME(6) NULL,
  created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (invite_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS launcher_sessions (
  ticket_hash   CHAR(64)    NOT NULL,
  account_id    VARCHAR(128) NOT NULL,
  machine_id    VARCHAR(128) NULL,
  machine_name  VARCHAR(64)  NULL,
  issued_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at    DATETIME(6) NOT NULL,
  consumed_at   DATETIME(6) NULL,
  PRIMARY KEY (ticket_hash),
  KEY idx_sessions_account (account_id, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 封禁记录：account=按账号(account_id)，ip=按来源IP，machine=按机器码。
	// 同一目标只保留一行（UNIQUE），到期/手动解封后再次封禁走 UPDATE 重置。
	`CREATE TABLE IF NOT EXISTS launcher_bans (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  ban_type   VARCHAR(16)  NOT NULL,
  ban_value  VARCHAR(255) NOT NULL,
  username   VARCHAR(64)  NULL,
  reason     VARCHAR(255) NULL,
  operator   VARCHAR(64)  NULL,
  banned_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at DATETIME(6)  NULL,
  revoked_at DATETIME(6)  NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_launcher_bans_target (ban_type, ban_value),
  KEY idx_launcher_bans_active (ban_type, revoked_at, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// GM 踢人事件：运维工具封禁/踢下线时写入，登录器按账号轮询拉取后关闭本地游戏进程。
	// 设计为拉模式（玩家 NAT 后不可 push）：登录器携带 ticket 查询本账号 after_id 之后的事件。
	`CREATE TABLE IF NOT EXISTS launcher_gm_events (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  account_id VARCHAR(128) NOT NULL,
  event_type VARCHAR(16)  NOT NULL DEFAULT 'kick',
  reason     VARCHAR(255) NULL,
  operator   VARCHAR(64)  NULL,
  created_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY idx_gm_events_account (account_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS launcher_announcements (
  id         BIGINT       NOT NULL AUTO_INCREMENT,
  content    VARCHAR(500) NOT NULL,
  color      VARCHAR(16)  NOT NULL DEFAULT '0xFF0000FF',
  operator   VARCHAR(64)  NOT NULL DEFAULT 'admin',
  created_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 注册赠送点券待发放表：注册时记账，玩家首次上线角色后由泡点循环走 GM 接口发放。
	`CREATE TABLE IF NOT EXISTS launcher_register_bonus_pending (
  account_id   VARCHAR(128) NOT NULL,
  points       INT          NOT NULL,
  state        VARCHAR(16)  NOT NULL DEFAULT 'pending',
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  granted_at   DATETIME(6)  NULL,
  PRIMARY KEY (account_id),
  KEY idx_rb_pending_state (state, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 泡点每日限额台账：账号级、按自然日累计"已发出去的点券"，用于实现
	// 「每个账号每天最多通过泡点获得 N 点券」。必须落库：内存计数一重启就归零，
	// 限额会形同虚设（见 pointsink_daily.go）。
	`CREATE TABLE IF NOT EXISTS launcher_pointsink_daily (
  account_id VARCHAR(128) NOT NULL,
  day        CHAR(10)     NOT NULL,
  granted    BIGINT       NOT NULL DEFAULT 0,
  updated_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (account_id, day),
  KEY idx_ps_daily_day (day)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 登录器「服务器公告」栏：管理工具维护的公告标题 + 日期（无正文），
	// 登录器（外网版）登录页读取后展示在登录框左侧。整表替换式维护（见 ReplaceNotices）。
	`CREATE TABLE IF NOT EXISTS launcher_notices (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  title       VARCHAR(120) NOT NULL,
  notice_date VARCHAR(10)  NOT NULL DEFAULT '',
  enabled     TINYINT(1)   NOT NULL DEFAULT 1,
  sort_order  INT          NOT NULL DEFAULT 0,
  created_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY idx_launcher_notices_enabled (enabled, sort_order, notice_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	// 服务端指定 DLL：管理工具只上传「文件名 + SHA256」（dll 本体由服主随客户端分发），
	// 登录器（外网版）自检与登录时校验客户端根目录同名文件的哈希，通过则启动游戏时注入。
	`CREATE TABLE IF NOT EXISTS launcher_dlls (
  id          BIGINT       NOT NULL AUTO_INCREMENT,
  file_name   VARCHAR(128) NOT NULL,
  sha256      CHAR(64)     NOT NULL,
  on_mismatch VARCHAR(16)  NOT NULL DEFAULT 'skip',
  enabled     TINYINT(1)   NOT NULL DEFAULT 1,
  sort_order  INT          NOT NULL DEFAULT 0,
  created_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  KEY idx_launcher_dlls_enabled (enabled, sort_order)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// Store MySQL 数据访问对象。
type Store struct {
	db *sql.DB

	// gmEventNotifier 写入 GM 事件成功后的回调（可空）。
	//
	// 为什么挂在 Store 上而不是让每个调用点自己通知：写事件的地方有十来处
	//（封禁踢人 / 批量踢下线 / 泡点到账 / 注册赠送到账 / 福利到账 …），
	// 逐个手动补通知一定会有漏的，将来新增一处也会忘。
	// 挂在这里就是"只要落了库就一定有人被唤醒"，写事件的人不需要知道推送的存在。
	//
	// 契约：**必须非阻塞**（实现是 hashmap 查表 + 非阻塞 channel 投递）。
	// 它是旁路，绝不能让一次事件写入因为"没人在等"而变慢或失败。
	gmEventNotifier func(accountID string)
}

// SetGMEventNotifier 注册 GM 事件写入回调（主程序把它接到认证服务的事件 hub 上）。
func (s *Store) SetGMEventNotifier(fn func(accountID string)) { s.gmEventNotifier = fn }

// Open 建立连接池并 Ping（2s 超时）。
func Open(cfg config.MySQLConfig) (*Store, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.DBName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(3 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.db.Close() }

// Ping 轻量探活。
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// EnsureSchema 幂等建表。
func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, ddl := range schemaDDL {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("ensure schema: %w", err)
		}
	}
	// CDK 兑换功能两张表（认证服务 / GUI 启动走 EnsureSchema 即自动建表）。
	if err := s.EnsureRedeemSchema(ctx); err != nil {
		return err
	}
	// PVF 加密 key 下发表（三端联动）。
	if err := s.EnsurePvfSchema(ctx); err != nil {
		return err
	}
	// 救援代码列：老库（此前版本建的 launcher_accounts）无此列，需幂等补列。
	if err := s.EnsureRescueCodeColumn(ctx); err != nil {
		return err
	}
	// 注册来源 IP 列：用于「每个 IP 最多注册 N 个账号」的限制，老库同样需要幂等补列 + 回填。
	if err := s.EnsureRegisterIPColumn(ctx); err != nil {
		return err
	}
	// 会话计算机名列：机器码换成硬件指纹后，旧「按计算机名」的机器封禁用它继续生效。
	if err := s.EnsureSessionMachineNameColumn(ctx); err != nil {
		return err
	}
	// GM 事件表的清理索引：老库没有它，清理会退化成全表扫描（见 gm_events_prune.go）。
	if err := s.EnsureGMEventPruneIndex(ctx); err != nil {
		return err
	}
	return nil
}

// EnsureSessionMachineNameColumn 幂等补列 launcher_sessions.machine_name。
//
// 背景（2026-09-24）：登录器上报的 machine_id 从「计算机名」换成硬件指纹（hw:<32hex>），
// 而库里历史 machine 维度封禁的 ban_value 是按计算机名写的。登录器现在会同时上报
// computer_name，落到这一列，checkBans 用「硬件码优先 + 计算机名兜底」双匹配，
// 避免"换了机器码，旧机器封禁全部失效"。
func (s *Store) EnsureSessionMachineNameColumn(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'launcher_sessions' AND column_name = 'machine_name'`,
	).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`ALTER TABLE launcher_sessions ADD COLUMN machine_name VARCHAR(64) NULL AFTER machine_id`)
	return err
}

// EnsureRescueCodeColumn 幂等补列：information_schema 确认不存在时执行 ALTER。
func (s *Store) EnsureRescueCodeColumn(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'launcher_accounts' AND column_name = 'rescue_code_hash'`,
	).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`ALTER TABLE launcher_accounts ADD COLUMN rescue_code_hash VARCHAR(100) NULL AFTER password_hash`)
	return err
}

// EnsureRegisterIPColumn 幂等补列 launcher_accounts.register_ip 并建索引。
//
// register_ip 与 last_login_ip 是两回事：后者每次登录都会被覆盖，拿它做"注册来源"统计
// 会把老账号算到最近一次登录的 IP 上。所以注册时单独记一份来源 IP，并且只增不改。
//
// 补列后做一次回填：把 register_ip 为空的既有账号按 last_login_ip 填上。
// 这是近似值（历史账号没有留下真实注册 IP），但能让"每 IP 上限"对老库立刻生效。
//
// 回填必须严格限定在"刚刚 ALTER 出这一列"的分支里。EnsureSchema 每次认证服务启动都会
// 调用本函数，回填若写在分支外就会被反复执行，后果是：管理员建号（register_ip 按设计留空、
// 不占额度）→ 该玩家登录一次（Login 会覆盖 last_login_ip）→ 认证服务重启 → 回填把
// last_login_ip 抄成 register_ip → 这个从未自助注册过的账号白白吃掉一份 IP 额度，
// 同 IP 的合法自助注册反而被 403 拒绝。
func (s *Store) EnsureRegisterIPColumn(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'launcher_accounts' AND column_name = 'register_ip'`,
	).Scan(&n)
	if err != nil {
		return err
	}
	created := n == 0
	if created {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE launcher_accounts ADD COLUMN register_ip VARCHAR(64) NULL AFTER last_login_ip`); err != nil {
			return err
		}
	}
	var idx int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = 'launcher_accounts' AND index_name = 'idx_launcher_accounts_register_ip'`,
	).Scan(&idx)
	if err != nil {
		return err
	}
	if idx == 0 {
		if _, err := s.db.ExecContext(ctx,
			`ALTER TABLE launcher_accounts ADD KEY idx_launcher_accounts_register_ip (register_ip)`); err != nil {
			return err
		}
	}
	if !created {
		// 列本来就存在：回填早已做过（或库是新装的、建表时就带这一列），不再重跑。
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE launcher_accounts SET register_ip = last_login_ip
		 WHERE register_ip IS NULL AND last_login_ip IS NOT NULL AND last_login_ip <> ''`)
	return err
}

// ValidRescueCode 校验救援代码：必须为 6 位纯数字（设计约束）。
func ValidRescueCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// NormalizeUsername 规范化登录名：trim + 小写（ASCII 部分），与设计 2.1 一致。
func NormalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// AccountIDFor 计算账号主键 dnf:<sha256hex(normalized username)>。
func AccountIDFor(username string) string {
	sum := sha256.Sum256([]byte(NormalizeUsername(username)))
	return "dnf:" + hex.EncodeToString(sum[:])
}

// LooksLikeAccountID 判断字符串是否已经是账号主键（dnf:<hex>）形式。
// 兼容两种历史长度：老库迁移过来的 32 位 md5、以及现在新注册的 64 位 sha256。
// 用途：封禁/踢人等接口既接受用户名也接受精确账号 ID，靠它区分该查库还是直封。
func LooksLikeAccountID(raw string) bool {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "dnf:") {
		return false
	}
	digits := s[len("dnf:"):]
	if len(digits) != 32 && len(digits) != 64 {
		return false
	}
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Sha256Hex 返回 sha256 hex。
func Sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// Account 账号行。
type Account struct {
	AccountID      string
	Username       string
	Email          string
	PasswordHash   string
	RescueCodeHash string
	State          string
	LastLoginIP    string
	LastLoginAt    sql.NullTime
	CreatedAt      sql.NullTime
	MachineID      string
}

// HasRescueCode 账号是否已设置救援代码。
func (a *Account) HasRescueCode() bool {
	return a != nil && strings.TrimSpace(a.RescueCodeHash) != ""
}

// hashRescueCode 生成救援代码 bcrypt 哈希。
func hashRescueCode(code string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// consumeInviteTx 在事务内消费一枚注册码：命中未使用的码就标记 used 并返回 true。
//
// 为什么要试两种形态：库里既有 2026-09-24 之前的长码（DNF90-xxxxxxxxxxxx-xxxxxxxx），
// 也有之后生成的短码（ABCD-EFGH）。玩家手输时还可能漏横线、大小写不一致，所以：
//  1. 先按原样精确匹配（走 invite_code 主键索引，绝大多数情况一次命中）；
//  2. 未命中再按"两边都去掉分隔符并转大写"匹配一次（launcher_invites 只有几百行，
//     这次全表扫描的代价可以忽略）。
//
// 两次用的是同一个 UPDATE 条件（state='unused'），所以不存在把同一枚码用两次的可能：
// 谁先命中谁就把它置成 used。
func consumeInviteTx(ctx context.Context, tx *sql.Tx, accountID, raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	mark := func(whereClause string, arg any) (bool, error) {
		res, err := tx.ExecContext(ctx,
			`UPDATE launcher_invites SET state='used', used_by=?, used_at=NOW(6)
			 WHERE `+whereClause+` AND state='unused'`, accountID, arg)
		if err != nil {
			return false, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		return affected > 0, nil
	}
	if ok, err := mark(`invite_code = ?`, raw); err != nil || ok {
		return ok, err
	}
	normalized := NormalizeInviteCode(raw)
	if normalized == "" || normalized == raw {
		return false, nil
	}
	return mark(`UPPER(REPLACE(invite_code, '-', '')) = ?`, normalized)
}

// Register 注册新账号。mode=invite 时校验邀请码并事务内标记 used。
// rescueCode 为 6 位数字救援代码（找回密码用，bcrypt 存储），格式非法返回 ErrInvalidRescueCode。
//
// maxPerIP 为「同一个注册来源 IP 最多允许注册几个账号」：**0 = 不允许注册**，1~N = 最多 N 个。
// 计数只看 register_ip（注册时写入的来源 IP）：管理员建号与命令行工具传空 remoteIP，
// 因此 register_ip 为 NULL，天然不计入上限、也不占用额度（不看 maxPerIP 的值）。
//
// 并发说明：计数在同一事务内做 SELECT COUNT(*)，没有额外加行锁，同一 IP 的极端并发
// 注册理论上可以多进几个（超出上限 1~2 个）。对这个规模的自助注册入口（登录器串行
// 提交 + 人工输入）属于可接受取舍；如需严格串行，要引入独立 IP 计数表并对该行加锁，
// 代价是每次注册都争同一热点行。
func (s *Store) Register(ctx context.Context, username, password, email, inviteCode, mode, remoteIP, rescueCode string, maxPerIP int) (string, error) {
	username = NormalizeUsername(username)
	if len(password) < 8 {
		return "", errors.New("password-too-short")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	var rescueHash sql.NullString
	if strings.TrimSpace(rescueCode) != "" {
		if !ValidRescueCode(rescueCode) {
			return "", ErrInvalidRescueCode
		}
		rh, err := hashRescueCode(rescueCode)
		if err != nil {
			return "", err
		}
		rescueHash = sql.NullString{String: rh, Valid: true}
	}
	accountID := AccountIDFor(username)
	remoteIP = strings.TrimSpace(remoteIP)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	// 每 IP 注册数量限制：只统计带来源 IP 的账号（管理员建号 register_ip 为空，天然不计入）。
	//
	// maxPerIP 语义（与配置/管理端一致）：**0 = 不允许注册**（每个来源 IP 最多 0 个账号），
	// 1~N = 每个来源 IP 最多 N 个。所以这里不能再用 `maxPerIP > 0` 当开关 —— 那样 0 会变成"不限制"。
	// 真正决定"要不要按 IP 计数"的是"有没有来源 IP"：管理员/自检工具建号不传 IP，直接跳过。
	if remoteIP != "" && maxPerIP >= 0 {
		var existing int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM launcher_accounts WHERE register_ip = ?`, remoteIP).Scan(&existing); err != nil {
			return "", err
		}
		if existing >= maxPerIP {
			return "", ErrRegisterIPLimit
		}
	}

	if mode == "invite" {
		raw := strings.TrimSpace(inviteCode)
		if raw == "" {
			return "", ErrInvalidInvite
		}
		ok, err := consumeInviteTx(ctx, tx, accountID, raw)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrInvalidInvite
		}
	}

	emailVal := sql.NullString{String: strings.TrimSpace(email), Valid: strings.TrimSpace(email) != ""}
	ipVal := sql.NullString{String: remoteIP, Valid: remoteIP != ""}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO launcher_accounts (account_id, username, email, password_hash, rescue_code_hash, state, last_login_ip, register_ip)
		 VALUES (?, ?, ?, ?, ?, 'active', ?, ?)`,
		accountID, username, emailVal, string(hash), rescueHash, ipVal, ipVal)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return "", ErrUsernameTaken
		}
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return accountID, nil
}

// CountAccountsByRegisterIP 统计某来源 IP 已注册的账号数（管理端展示/排障用）。
func (s *Store) CountAccountsByRegisterIP(ctx context.Context, remoteIP string) (int, error) {
	remoteIP = strings.TrimSpace(remoteIP)
	if remoteIP == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM launcher_accounts WHERE register_ip = ?`, remoteIP).Scan(&n)
	return n, err
}

// ChangePassword 修改账号密码（管理员改密）：新密码至少 8 位；
// 成功后作废该账号全部已签发会话（launcher_sessions），防止旧密码会话续用。
func (s *Store) ChangePassword(ctx context.Context, username, newPassword string) error {
	username = NormalizeUsername(username)
	if len(newPassword) < 8 {
		return errors.New("password-too-short")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE launcher_accounts SET password_hash=? WHERE username=?`, string(hash), username)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrUserNotFound
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM launcher_sessions WHERE account_id IN (SELECT account_id FROM launcher_accounts WHERE username=?)`, username)
	return err
}

// SetRescueCode 设置/重置救援代码：仅校验 6 位数字与账号存在性（是否已设置由 handler 判定），
// 账号已设置时允许覆盖（用户在设置弹窗主动重新录入）。
func (s *Store) SetRescueCode(ctx context.Context, username, rescueCode string) error {
	username = NormalizeUsername(username)
	if !ValidRescueCode(rescueCode) {
		return ErrInvalidRescueCode
	}
	rh, err := hashRescueCode(rescueCode)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE launcher_accounts SET rescue_code_hash=? WHERE username=?`, rh, username)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ResetPasswordWithRescueCode 救援代码找回密码：校验账号存在且救援代码匹配（bcrypt），
// 通过后改密并作废该账号全部会话（防旧密码会话续用）。
// 账号未设置救援代码或代码不匹配统一返回 ErrInvalidRescueCode（不向调用方区分，防枚举）。
func (s *Store) ResetPasswordWithRescueCode(ctx context.Context, username, rescueCode, newPassword string) error {
	username = NormalizeUsername(username)
	if len(newPassword) < 8 {
		return errors.New("password-too-short")
	}
	if !ValidRescueCode(rescueCode) {
		return ErrInvalidRescueCode
	}
	account, err := s.GetAccountByUsername(ctx, username)
	if err != nil {
		return err
	}
	if !account.HasRescueCode() {
		return ErrInvalidRescueCode
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.RescueCodeHash), []byte(rescueCode)); err != nil {
		return ErrInvalidRescueCode
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE launcher_accounts SET password_hash=? WHERE username=?`, string(hash), username)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrUserNotFound
	}
	// 改密成功踢在线：作废该账号全部已签发会话。
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM launcher_sessions WHERE account_id IN (SELECT account_id FROM launcher_accounts WHERE username=?)`, username)
	return err
}

// ForceLogoutByUsername 强制下线：作废指定账号全部会话，返回被踢出的会话数。
func (s *Store) ForceLogoutByUsername(ctx context.Context, username string) (int64, error) {
	username = NormalizeUsername(username)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM launcher_sessions WHERE account_id IN (SELECT account_id FROM launcher_accounts WHERE username=?)`, username)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAccountByUsername 按登录名取账号。
func (s *Store) GetAccountByUsername(ctx context.Context, username string) (*Account, error) {
	username = NormalizeUsername(username)
	row := s.db.QueryRowContext(ctx,
		`SELECT account_id, username, COALESCE(email,''), password_hash, COALESCE(rescue_code_hash,''), state
		 FROM launcher_accounts WHERE username = ?`, username)
	var a Account
	var email sql.NullString
	if err := row.Scan(&a.AccountID, &a.Username, &email, &a.PasswordHash, &a.RescueCodeHash, &a.State); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	a.Email = email.String
	return &a, nil
}

// GetAccountByID 按 account_id 取账号（内部验证/管理用）。
func (s *Store) GetAccountByID(ctx context.Context, accountID string) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT account_id, username, COALESCE(email,''), password_hash, COALESCE(rescue_code_hash,''), state
		 FROM launcher_accounts WHERE account_id = ?`, accountID)
	var a Account
	var email sql.NullString
	if err := row.Scan(&a.AccountID, &a.Username, &email, &a.PasswordHash, &a.RescueCodeHash, &a.State); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	a.Email = email.String
	return &a, nil
}

// BanAccountTarget 账号型封禁/解封所需的目标信息（账号ID + 最近登录 IP + 最近机器码）。
//
// 为什么不直接用 GetAccountByUsername 的返回值：那个查询不 SELECT last_login_ip /
// machine_id，拿到的 Account 里这两个字段恒为空串——"一键三封"曾经因此静默地
// 只封了账号一条（IP/机器码被 `if bv == "" { continue }` 跳过）。
// 这里与 ListAccounts 用同一套取值（最近登录 IP + launcher_sessions 最新票据的 machine_id），
// 保证"封"和"解封"认的是同一个目标。
type BanAccountTarget struct {
	AccountID string
	Username  string
	LastIP    string
	MachineID string
}

// AccountBanTargets 按「用户名或 account_id」取封禁/解封目标；查不到返回 ErrUserNotFound。
// 用户名按 NormalizeUsername 归一（与注册/登录写入时一致），account_id 原样比对；
// 万一某个用户名本身就长得像 account_id，ORDER BY 保证按 account_id 命中优先
// ——与封禁侧 chooseAccountLookup（ban_resolve.go）的"ID 形式优先当 ID"语义一致，
// 结果确定可复现。
func (s *Store) AccountBanTargets(ctx context.Context, identifier string) (BanAccountTarget, error) {
	raw := strings.TrimSpace(identifier)
	if raw == "" {
		return BanAccountTarget{}, ErrUserNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT a.account_id, a.username, COALESCE(a.last_login_ip,''),
		        COALESCE((SELECT s.machine_id FROM launcher_sessions s
		                  WHERE s.account_id = a.account_id ORDER BY s.issued_at DESC LIMIT 1), '')
		 FROM launcher_accounts a
		 WHERE a.username = ? OR a.account_id = ?
		 ORDER BY (a.account_id = ?) DESC
		 LIMIT 1`, NormalizeUsername(raw), raw, raw)
	var t BanAccountTarget
	if err := row.Scan(&t.AccountID, &t.Username, &t.LastIP, &t.MachineID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BanAccountTarget{}, ErrUserNotFound
		}
		return BanAccountTarget{}, err
	}
	return t, nil
}

// Login 校验密码后签发 ticket：ticket 为 32B 随机 hex，库中只存 sha256 摘要。
// 返回 hasRescueCode 供登录器判断该账号是否已设置救援代码（无码老号引导设置）。
func (s *Store) Login(ctx context.Context, username, password string, machine MachineIdentity, remoteIP string, ttl time.Duration) (accountID, ticket string, hasRescueCode bool, err error) {
	account, err := s.GetAccountByUsername(ctx, username)
	if err != nil {
		return "", "", false, err
	}
	if account.State != "active" {
		return "", "", false, ErrBanned
	}
	if err := s.checkBans(ctx, account.AccountID, machine, remoteIP); err != nil {
		return "", "", false, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)); err != nil {
		return "", "", false, ErrInvalidPassword
	}
	hasRescueCode = account.HasRescueCode()
	ticketRaw := make([]byte, 32)
	if _, err := rand.Read(ticketRaw); err != nil {
		return "", "", hasRescueCode, err
	}
	ticket = hex.EncodeToString(ticketRaw)
	ticketHash := Sha256Hex(ticket)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", hasRescueCode, err
	}
	defer func() { _ = tx.Rollback() }()
	machineVal := nullTrimmed(machine.MachineID)
	nameVal := nullTrimmed(machine.Name)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO launcher_sessions (ticket_hash, account_id, machine_id, machine_name, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, NOW(6), ?)`,
		ticketHash, account.AccountID, machineVal, nameVal, time.Now().Add(ttl))
	if err != nil {
		return "", "", hasRescueCode, err
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE launcher_accounts SET last_login_ip=?, last_login_at=NOW(6) WHERE account_id=?`,
		remoteIP, account.AccountID)
	if err != nil {
		return "", "", hasRescueCode, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", hasRescueCode, err
	}
	return account.AccountID, ticket, hasRescueCode, nil
}

// Logout 按明文 ticket 删除会话（sha256 定位）。
func (s *Store) Logout(ctx context.Context, ticket string) error {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return ErrTicketNotFound
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM launcher_sessions WHERE ticket_hash = ?`, Sha256Hex(ticket))
	return err
}

// VerifyTicket 内部验证：sha256(ticket) 查会话、校验过期与账号状态。
// 成功返回 account_id；失败返回对应哨兵错误。
func (s *Store) VerifyTicket(ctx context.Context, ticket string, machine MachineIdentity, remoteIP string) (string, error) {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return "", ErrTicketNotFound
	}
	ticketHash := Sha256Hex(ticket)

	var accountID, state, sessionMachineName string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx,
		`SELECT ts.account_id, ac.state, ts.expires_at, COALESCE(ts.machine_name, '')
		 FROM launcher_sessions ts
		 JOIN launcher_accounts ac ON ac.account_id = ts.account_id
		 WHERE ts.ticket_hash = ?`, ticketHash).
		Scan(&accountID, &state, &expiresAt, &sessionMachineName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrTicketNotFound
		}
		return "", err
	}
	if time.Now().After(expiresAt) {
		return "", ErrTicketExpired
	}
	if state != "active" {
		return "", ErrBanned
	}
	// 游戏服验票帧里只有 machine_id（90CN.dll 填的），没有计算机名；
	// 但会话行记着发出这张票时登录器上报的计算机名 —— 拿它兜底，
	// 旧「按计算机名」的机器封禁在这条路径上同样生效。
	if strings.TrimSpace(machine.Name) == "" {
		machine.Name = sessionMachineName
	}
	if err := s.checkBans(ctx, accountID, machine, remoteIP); err != nil {
		return "", err
	}

	// 回填首次出现的 machine_id / machine_name 与最近登录 IP（信息性，不阻塞主流程）。
	_, _ = s.db.ExecContext(ctx,
		`UPDATE launcher_sessions SET machine_id = COALESCE(machine_id, ?), machine_name = COALESCE(machine_name, ?)
		 WHERE ticket_hash = ?`,
		nullTrimmed(machine.MachineID), nullTrimmed(machine.Name), ticketHash)
	_, _ = s.db.ExecContext(ctx,
		`UPDATE launcher_accounts SET last_login_ip=?, last_login_at=NOW(6) WHERE account_id=?`,
		remoteIP, accountID)
	return accountID, nil
}

// nullTrimmed 空串按 NULL 落库（机器码/计算机名允许缺失）。
func nullTrimmed(v string) sql.NullString {
	v = strings.TrimSpace(v)
	return sql.NullString{String: v, Valid: v != ""}
}

// inviteCharset 注册码字符集：去掉易混的 0/O/1/I（与运维工具配对码同一思路），
// 玩家手抄/手输时不会把 O 打成 0。
const inviteCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// inviteCodeLength 注册码有效字符数（展示时中间插一个 '-'，真正比对时不带横线）
// —— 32^8 ≈ 1.1e12 种组合，一次一码不可能猜中。
const inviteCodeLength = 8

// GenerateInviteCode 生成一枚注册码，形如 ABCD-EFGH（纯函数，便于单测）。
// 校验/存储都按**去掉横线并转大写**后的 8 位来比对（见 NormalizeInviteCode），
// 这样玩家填 "abcd-efgh"、"ABCDEFGH"、"abcd efgh" 都能用。
func GenerateInviteCode() (string, error) {
	buf := make([]byte, inviteCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, inviteCodeLength)
	for i := range out {
		out[i] = inviteCharset[int(buf[i])%len(inviteCharset)]
	}
	return string(out[:4]) + "-" + string(out[4:]), nil
}

// NormalizeInviteCode 把玩家/管理端填的注册码归一：去掉所有分隔（'-'、空格）并转大写。
// 老格式的码（DNF90-xxxx-xxxx）归一后仍然等于自身去掉分隔的形式，因此历史码继续可用。
func NormalizeInviteCode(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(raw)) {
		if r == '-' || r == ' ' || r == '\t' || r == '_' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// InviteRecord 注册码（邀请码）行。
type InviteRecord struct {
	Code      string       // 库里原样存的码（含横线）
	State     string       // unused / used / revoked
	UsedBy    string       // 使用者账号ID（used 时）
	UsedAt    sql.NullTime // 使用时间
	CreatedAt time.Time    // 生成时间
}

// Used 便于 UI 判断。
func (r InviteRecord) Used() bool { return strings.EqualFold(r.State, "used") }

// CreateInvites 批量生成注册码并入库，返回生成成功的码（顺序与生成一致）。
//
// 唯一冲突（PRIMARY KEY）用 INSERT IGNORE 探到后重试，重试上限 = 每个码 5 次：
// 32^8 的空间下同一批里撞码的概率可以忽略，重试只是把"理论上可能"堵死。
// n 上限 200：一次生成太多既不好发给玩家，也容易变成"码池泄漏"。
func (s *Store) CreateInvites(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, errors.New("invite-count-invalid")
	}
	if n > 200 {
		n = 200
	}
	codes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		inserted := false
		for attempt := 0; attempt < 5 && !inserted; attempt++ {
			code, err := GenerateInviteCode()
			if err != nil {
				return codes, err
			}
			res, err := s.db.ExecContext(ctx,
				`INSERT IGNORE INTO launcher_invites (invite_code, state) VALUES (?, 'unused')`, code)
			if err != nil {
				return codes, err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return codes, err
			}
			if affected > 0 {
				codes = append(codes, code)
				inserted = true
			}
		}
		if !inserted {
			return codes, errors.New("invite-code-collision")
		}
	}
	return codes, nil
}

// ListInvites 列出注册码，state 为空表示全部；limit<=0 时取 200 条。
// 排序：未用的在前（方便运营直接复制发人），同组内新生成的在前。
func (s *Store) ListInvites(ctx context.Context, state string, limit int) ([]InviteRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	state = strings.ToLower(strings.TrimSpace(state))
	query := `SELECT invite_code, state, COALESCE(used_by,''), used_at, created_at FROM launcher_invites`
	args := []any{}
	if state != "" && state != "all" {
		query += ` WHERE LOWER(state) = ?`
		args = append(args, state)
	}
	query += ` ORDER BY (LOWER(state) = 'unused') DESC, created_at DESC, invite_code ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InviteRecord
	for rows.Next() {
		var r InviteRecord
		if err := rows.Scan(&r.Code, &r.State, &r.UsedBy, &r.UsedAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeInvites 作废**未使用**的注册码（置 state='revoked'），返回实际作废条数。
// 已经用掉的码不动：那枚码对应的账号是真的注册出来了，作废它只会让审计记录对不上。
func (s *Store) RevokeInvites(ctx context.Context, codes []string) (int, error) {
	cleaned := make([]string, 0, len(codes))
	for _, c := range codes {
		if c = strings.TrimSpace(c); c != "" {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) == 0 {
		return 0, nil
	}
	total := 0
	for _, code := range cleaned {
		res, err := s.db.ExecContext(ctx,
			`UPDATE launcher_invites SET state='revoked' WHERE invite_code=? AND LOWER(state)='unused'`, code)
		if err != nil {
			return total, err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(affected)
	}
	return total, nil
}

// CreateInvite 生成单个注册码（GUI/启动引导用）。
// 2026-09-24 起统一用短码格式（见 GenerateInviteCode）——历史长格式的码照旧可用，
// 校验只按归一化后的字符串查库，与格式无关。
func (s *Store) CreateInvite(ctx context.Context) (string, error) {
	codes, err := s.CreateInvites(ctx, 1)
	if err != nil {
		return "", err
	}
	if len(codes) == 0 {
		return "", errors.New("invite-code-collision")
	}
	return codes[0], nil
}

// EnsureBootstrapInvite 首次以 invite 模式启动时生成一个引导邀请码并返回，已存在则跳过。
func (s *Store) EnsureBootstrapInvite(ctx context.Context) (string, bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM launcher_invites WHERE state='unused'`).Scan(&count); err != nil {
		return "", false, err
	}
	if count > 0 {
		return "", false, nil
	}
	code, err := s.CreateInvite(ctx)
	if err != nil {
		return "", false, err
	}
	return code, true, nil
}
