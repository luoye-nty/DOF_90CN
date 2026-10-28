// PVF 加密 key 下发表（pvf_keys）数据访问：三端联动的服务端存储层。
// file_sha256 = 玩家侧 Script.pvf 的 SHA-256（登录器本地计算后作为查钥条件）；
// 管理工具每次加密换 key 后按 file_sha256 upsert 覆盖当前版本记录。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PVF key 业务错误哨兵（server/manage 层转 JSON code）。
var (
	ErrPvfNotFound = errors.New("pvf-not-found")
)

// pvfDDL 幂等建表语句（追加进 EnsureSchema 流程自动建表）。
var pvfDDL = []string{
	`CREATE TABLE IF NOT EXISTS pvf_keys (
  id            BIGINT       NOT NULL AUTO_INCREMENT,
  file_sha256   CHAR(64)     NOT NULL,
  pvf_version   VARCHAR(64)  NOT NULL,
  key_set       VARCHAR(96)  NOT NULL,
  active        TINYINT(1)   NOT NULL DEFAULT 1,
  download_url  VARCHAR(1024) NOT NULL DEFAULT '',
  pvf_enabled   TINYINT(1)   NOT NULL DEFAULT 1,
  client_download_url VARCHAR(1024) NOT NULL DEFAULT '',
  created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY uk_pvf_keys_sha (file_sha256)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// pvfUpgradeDDL 老库（无 download_url / pvf_enabled / client_download_url 列）幂等补列语句，
// 每条带目标列名，已存在则跳过（按列名探测避免重复 ALTER）。
var pvfUpgradeDDL = []struct {
	Column string
	SQL    string
}{
	{"download_url", `ALTER TABLE pvf_keys ADD COLUMN download_url VARCHAR(1024) NOT NULL DEFAULT '' AFTER key_set`},
	{"pvf_enabled", `ALTER TABLE pvf_keys ADD COLUMN pvf_enabled TINYINT(1) NOT NULL DEFAULT 1 AFTER download_url`},
	{"client_download_url", `ALTER TABLE pvf_keys ADD COLUMN client_download_url VARCHAR(1024) NOT NULL DEFAULT '' AFTER pvf_enabled`},
}

// PvfKeyInfo pvf_keys 行。
type PvfKeyInfo struct {
	ID                int64
	FileSHA256        string
	PVFVersion        string
	KeySet            string
	Active            bool
	DownloadURL       string
	PvfEnabled        bool
	ClientDownloadURL string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// EnsurePvfSchema 幂等创建 pvf_keys 表（认证服务 / GUI 启动 EnsureSchema 流程统一调用）。
func (s *Store) EnsurePvfSchema(ctx context.Context) error {
	for _, ddl := range pvfDDL {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("ensure pvf schema: %w", err)
		}
	}
	// 老库兼容：仅补缺失列
	for _, up := range pvfUpgradeDDL {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'pvf_keys' AND COLUMN_NAME = ?`,
			up.Column).Scan(&n); err != nil {
			return fmt.Errorf("check pvf column %s: %w", up.Column, err)
		}
		if n > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, up.SQL); err != nil {
			return fmt.Errorf("ensure pvf column %s: %w", up.Column, err)
		}
	}
	return nil
}

// NormalizeSHA256Hex 校验并规范化 64 位小写 hex（SHA-256）。
func NormalizeSHA256Hex(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != 64 {
		return "", errors.New("sha256 must be 64 hex chars")
	}
	for _, ch := range v {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return "", errors.New("sha256 must be hex")
		}
	}
	return v, nil
}

// NormalizeKeySetHex 校验并规范化 96 位小写 hex（6 段 × 8B key）。
func NormalizeKeySetHex(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if len(v) != 96 {
		return "", errors.New("key_set must be 96 hex chars (6 groups x 8 bytes)")
	}
	for _, ch := range v {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return "", errors.New("key_set must be hex")
		}
	}
	return v, nil
}

// UpsertPvfKey 发布（幂等写入/更新）一条 PVF key 记录（按 file_sha256 覆盖并置 active=1）。
// 发布动作会使旧版本全部失效：同一事务内先将既有 active 全部置 0，再落当前版本，
// 保证“只认最新受信哈希”（登录器旧客户端 sha 查不到即 404 阻断要求更新）。
// downloadURL 为加密后客户端更新包直链（zip/7z/rar，登录器按此下载更新）；
// clientDownloadURL 为整包客户端直链（zip/7z/rar，登录器本机无客户端时按此下载整包）；
// enabled=false 表示本次发布不启用 PVF 加密校验。
// 返回记录 id（已存在时返回原 id）。
func (s *Store) UpsertPvfKey(ctx context.Context, pvfVersion, fileSHA256, keySet, downloadURL, clientDownloadURL string, enabled bool) (int64, error) {
	sha, err := NormalizeSHA256Hex(fileSHA256)
	if err != nil {
		return 0, err
	}
	ks, err := NormalizeKeySetHex(keySet)
	if err != nil {
		return 0, err
	}
	version := strings.TrimSpace(pvfVersion)
	if version == "" {
		return 0, errors.New("pvf_version is required")
	}
	url := strings.TrimSpace(downloadURL)
	clientURL := strings.TrimSpace(clientDownloadURL)
	en := 0
	if enabled {
		en = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("upsert pvf key begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE pvf_keys SET active = 0 WHERE active = 1`); err != nil {
		return 0, fmt.Errorf("deactivate old pvf keys: %w", err)
	}

	const q = `INSERT INTO pvf_keys (file_sha256, pvf_version, key_set, active, download_url, pvf_enabled, client_download_url)
VALUES (?, ?, ?, 1, ?, ?, ?)
ON DUPLICATE KEY UPDATE pvf_version = VALUES(pvf_version), key_set = VALUES(key_set),
  active = 1, download_url = VALUES(download_url), pvf_enabled = VALUES(pvf_enabled),
  client_download_url = VALUES(client_download_url)`
	res, err := tx.ExecContext(ctx, q, sha, version, ks, url, en, clientURL)
	if err != nil {
		return 0, fmt.Errorf("upsert pvf key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		// 命中唯一键走 UPDATE 时 LastInsertId 可能为 0，回查原 id。
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM pvf_keys WHERE file_sha256 = ?`, sha).Scan(&id); err != nil {
			return 0, fmt.Errorf("resolve pvf key id: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("upsert pvf key commit: %w", err)
	}
	return id, nil
}

// pvfRowColumns 与 ListActivePvfKeys / GetPvfKeyBySHA / GetLatestActivePvfKey 共用列清单。
const pvfRowColumns = `id, file_sha256, pvf_version, key_set, active, download_url, pvf_enabled, client_download_url, created_at, updated_at`

func scanPvfKey(row interface{ Scan(...any) error }) (*PvfKeyInfo, error) {
	var it PvfKeyInfo
	if err := row.Scan(&it.ID, &it.FileSHA256, &it.PVFVersion, &it.KeySet, &it.Active,
		&it.DownloadURL, &it.PvfEnabled, &it.ClientDownloadURL, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return nil, err
	}
	return &it, nil
}

// GetPvfKeyBySHA 按玩家侧 Script.pvf 的 SHA-256 查当前有效 key_set。
// 未命中返回 ErrPvfNotFound。
func (s *Store) GetPvfKeyBySHA(ctx context.Context, fileSHA256 string) (*PvfKeyInfo, error) {
	sha, err := NormalizeSHA256Hex(fileSHA256)
	if err != nil {
		return nil, err
	}
	it, err := scanPvfKey(s.db.QueryRowContext(ctx,
		`SELECT `+pvfRowColumns+` FROM pvf_keys WHERE file_sha256 = ? AND active = 1`, sha))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPvfNotFound
		}
		return nil, fmt.Errorf("get pvf key: %w", err)
	}
	return it, nil
}

// GetLatestActivePvfKey 返回最新发布（active=1 中 id 最大）的一条记录。
// 无任何发布返回 ErrPvfNotFound。
func (s *Store) GetLatestActivePvfKey(ctx context.Context) (*PvfKeyInfo, error) {
	it, err := scanPvfKey(s.db.QueryRowContext(ctx,
		`SELECT `+pvfRowColumns+` FROM pvf_keys WHERE active = 1 ORDER BY id DESC LIMIT 1`))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPvfNotFound
		}
		return nil, fmt.Errorf("get latest pvf key: %w", err)
	}
	return it, nil
}

// HasActivePvfKey 是否存在至少一条 active=1 的 key 记录。
// 用于区分“PVF 加密从未启用”（表空，服务端应放行、登录器不注入 key_set）
// 与“已启用但该客户端 sha 未发布”（应 404 阻断，要求更新客户端）。
func (s *Store) HasActivePvfKey(ctx context.Context) (bool, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pvf_keys WHERE active = 1`).Scan(&n); err != nil {
		return false, fmt.Errorf("count active pvf keys: %w", err)
	}
	return n > 0, nil
}

// ListActivePvfKeys 返回全部 active=1 的 key 记录（管理工具已发布列表）。
func (s *Store) ListActivePvfKeys(ctx context.Context) ([]PvfKeyInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pvfRowColumns+` FROM pvf_keys WHERE active = 1 ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list active pvf keys: %w", err)
	}
	defer rows.Close()
	var list []PvfKeyInfo
	for rows.Next() {
		it, err := scanPvfKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pvf key: %w", err)
		}
		list = append(list, *it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pvf keys rows: %w", err)
	}
	return list, nil
}
