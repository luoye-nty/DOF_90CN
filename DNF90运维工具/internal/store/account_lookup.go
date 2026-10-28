// 账号主键 ↔ 用户名的解析（记录查询显示、以及"账号输入框允许直接填用户名"）。
//
// 背景：记录里存的 account_id 是账号主键，形如 `dnf:<sha256(用户名)>`（不可逆），
// 也见过历史/外部工具造的 `wpf-…`。运营要的是"真实账号名"，所以：
//   - AccountNamesByID：批量把主键翻成用户名（一次 SQL，供记录查询回显）；
//   - ResolveAccountInput：把"主键 或 用户名"统一成主键（供各类查询条件用）。
package store

import (
	"context"
	"strings"
)

// accountNameLookupMax 单次批量翻译的上限（记录一页最多 2000 条，够了）。
const accountNameLookupMax = 2000

// AccountNamesByID 批量查账号主键 → 用户名。
//
// 返回的 map 只包含能匹配到的账号：查不到的主键（例如玩家从未在登录器注册过、
// 或记录来自外部工具）由调用方原样显示，不能因为翻译失败就把记录吞掉。
func (s *Store) AccountNamesByID(ctx context.Context, accountIDs []string) (map[string]string, error) {
	ids := make([]string, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, raw := range accountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		if len(ids) >= accountNameLookupMax {
			break
		}
	}
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// 占位符按实际条数拼（MySQL 驱动不支持数组参数；条数已被上限约束）。
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, username FROM launcher_accounts WHERE account_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// ResolveAccountInput 把用户填的"账号主键 或 用户名"统一成账号主键。
//
// 语义（刻意宽松）：
//   - 空串 → 空串；
//   - 已经是主键形态（`dnf:<hex>`，或历史/外部的 `xxx-…` 串）→ 原样返回，不多查库；
//   - 否则按用户名查（归一化后不分大小写）；
//   - **查不到也原样返回**：这是查询条件，不是写操作 —— 写错一个字就该显示 0 条结果，
//     而不是弹一个"账号不存在"把运营挡在查询外面。
func (s *Store) ResolveAccountInput(ctx context.Context, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if LooksLikeAccountID(value) {
		return value, nil
	}
	// 先按主键直接查一遍（覆盖 wpf-… 这类非 dnf: 前缀的历史主键）。
	if acc, err := s.GetAccountByID(ctx, value); err == nil && acc != nil {
		return acc.AccountID, nil
	} else if err != nil && !isUserNotFound(err) {
		return "", err
	}
	if acc, err := s.GetAccountByUsername(ctx, value); err == nil && acc != nil {
		return acc.AccountID, nil
	} else if err != nil && !isUserNotFound(err) {
		return "", err
	}
	return value, nil
}

// isUserNotFound 判断错误是否为"账号不存在"（只有它才允许降级为原样返回）。
func isUserNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrUserNotFound.Error())
}
