// 服务端指定 DLL 的数据访问（管理工具维护，登录器外网版校验 + 注入）。
//
// 只存「文件名 + SHA256 + 校验失败策略」，**不存 dll 本体**：
// 服主自行把 dll 随客户端分发，登录器只在客户端根目录（DNF.exe 同目录）校验同名文件的哈希。
package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// DLL 校验失败时的处理策略。
const (
	// DLLMismatchBlock 缺文件/哈希不符 → 登录器弹窗并拒绝启动游戏。
	DLLMismatchBlock = "block"
	// DLLMismatchSkip 缺文件/哈希不符 → 只跳过注入，照常进游戏（记日志）。
	DLLMismatchSkip = "skip"
)

// LauncherDLL 一条"服务端指定 DLL"。
type LauncherDLL struct {
	ID         int64  `json:"id"`
	FileName   string `json:"file_name"`   // 只允许文件名（不含路径），放在客户端根目录
	SHA256     string `json:"sha256"`      // 小写 hex，64 位
	OnMismatch string `json:"on_mismatch"` // block / skip
	Enabled    bool   `json:"enabled"`
	SortOrder  int    `json:"sort_order"`
}

// dllMaxItems 列表上限：防止误粘贴把配置写成一本账。
const dllMaxItems = 50

// NormalizeDLLFileName 校验并规范化 dll 文件名（只允许文件名，杜绝路径穿越）。
func NormalizeDLLFileName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("dll: 文件名为空")
	}
	if len([]rune(name)) > 120 {
		return "", errors.New("dll: 文件名过长")
	}
	// 必须是纯文件名：带目录/盘符/上跳的一律拒绝（登录器只在客户端根目录找它）。
	if strings.ContainsAny(name, `\/:`) || name == "." || name == ".." {
		return "", errors.New("dll: 只接受文件名（不能带路径），例如 90CN.dll")
	}
	if filepath.Base(name) != name {
		return "", errors.New("dll: 只接受文件名（不能带路径）")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".dll") {
		return "", errors.New("dll: 必须是 .dll 文件")
	}
	return name, nil
}

// NormalizeDLLSHA256 校验并规范化哈希（小写 64 位 hex）。
func NormalizeDLLSHA256(raw string) (string, error) {
	sum := strings.ToLower(strings.TrimSpace(raw))
	if len(sum) != 64 {
		return "", errors.New("dll: SHA256 必须是 64 位十六进制")
	}
	for i := 0; i < len(sum); i++ {
		c := sum[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return "", errors.New("dll: SHA256 含非十六进制字符")
		}
	}
	return sum, nil
}

// normalizeDLLMismatch 策略归一（未知值一律按最保守的"跳过注入"处理）。
func normalizeDLLMismatch(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), DLLMismatchBlock) {
		return DLLMismatchBlock
	}
	return DLLMismatchSkip
}

// ListDLLs 返回全部"服务端指定 DLL"（升序）。onlyEnabled=true 时只返回启用的（登录器侧用）。
func (s *Store) ListDLLs(ctx context.Context, onlyEnabled bool) ([]LauncherDLL, error) {
	q := `SELECT id, file_name, sha256, on_mismatch, enabled, sort_order FROM launcher_dlls`
	if onlyEnabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY sort_order ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LauncherDLL, 0, 8)
	for rows.Next() {
		var item LauncherDLL
		var enabled int
		if err := rows.Scan(&item.ID, &item.FileName, &item.SHA256, &item.OnMismatch, &enabled, &item.SortOrder); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		item.OnMismatch = normalizeDLLMismatch(item.OnMismatch)
		out = append(out, item)
	}
	return out, rows.Err()
}

// ReplaceDLLs 整表替换（管理工具「保存全部」）：没带的行等于删除，顺序按传入顺序落 sort_order。
// 全程一个事务：中途失败不留半份数据（否则登录器可能读到"删了一半"的清单）。
func (s *Store) ReplaceDLLs(ctx context.Context, items []LauncherDLL) ([]LauncherDLL, error) {
	if len(items) > dllMaxItems {
		return nil, errors.New("dll: 条数超过上限")
	}
	type row struct {
		fileName   string
		sha256     string
		onMismatch string
		enabled    bool
	}
	prepared := make([]row, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		fileName, err := NormalizeDLLFileName(item.FileName)
		if err != nil {
			return nil, err
		}
		sum, err := NormalizeDLLSHA256(item.SHA256)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(fileName)
		if _, dup := seen[key]; dup {
			return nil, errors.New("dll: 重复的文件名 " + fileName)
		}
		seen[key] = struct{}{}
		prepared = append(prepared, row{
			fileName:   fileName,
			sha256:     sum,
			onMismatch: normalizeDLLMismatch(item.OnMismatch),
			enabled:    item.Enabled,
		})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM launcher_dlls`); err != nil {
		return nil, err
	}
	for i, r := range prepared {
		enabled := 0
		if r.enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO launcher_dlls (file_name, sha256, on_mismatch, enabled, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, NOW(6), NOW(6))`,
			r.fileName, r.sha256, r.onMismatch, enabled, i); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.ListDLLs(ctx, false)
}
