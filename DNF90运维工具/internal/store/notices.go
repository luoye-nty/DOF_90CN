// 登录器「服务器公告」栏的数据访问。
//
// 与 launcher_announcements（游戏内喇叭公告，逐条 append + 长轮询推送）不同：
// 这里是**公告板**，管理工具整表维护（标题 + 日期，无正文），
// 登录器每次进登录页拉一份当前列表即可 —— 没有推送、也没有"已读游标"。
package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// NoticeMaxTitleRunes 标题长度上限（与前端输入框、DB 列宽一起收紧）。
const NoticeMaxTitleRunes = 60

// NoticeMaxItems 公告板最多保留多少条（够用即可，防止误粘贴灌爆登录页）。
const NoticeMaxItems = 100

// LauncherNotice 公告板的一行。
type LauncherNotice struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	NoticeDate string    `json:"notice_date"` // YYYY-MM-DD；空=不在日期列显示
	Enabled    bool      `json:"enabled"`
	SortOrder  int       `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
}

// normalizeNoticeDate 校验并规范化日期（空串允许；非空必须是 YYYY-MM-DD）。
// 同时接受没补零的写法（2026-9-4），统一收敛成 2026-09-04 落库 ——
// 运营手打日期漏个 0 不该让整批保存失败，但落库格式必须统一（否则登录页日期列会参差）。
func normalizeNoticeDate(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	for _, layout := range []string{"2006-01-02", "2006-1-2"} {
		if d, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return d.Format("2006-01-02"), nil
		}
	}
	return "", errors.New("notice: notice_date must be YYYY-MM-DD")
}

// NormalizeNoticeTitle 去空白 + 长度校验（导出给上层做同口径预校验）。
func NormalizeNoticeTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", errors.New("notice: title required")
	}
	if utf8.RuneCountInString(title) > NoticeMaxTitleRunes {
		return "", errors.New("notice: title too long")
	}
	return title, nil
}

// ListNotices 返回全部公告（升序：sort_order, id）。
// onlyEnabled=true 时只返回启用的（登录器侧用）。
func (s *Store) ListNotices(ctx context.Context, onlyEnabled bool) ([]LauncherNotice, error) {
	q := `SELECT id, title, COALESCE(notice_date,''), enabled, sort_order, created_at
	      FROM launcher_notices`
	if onlyEnabled {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY sort_order ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LauncherNotice, 0, 16)
	for rows.Next() {
		var n LauncherNotice
		var enabled int
		if err := rows.Scan(&n.ID, &n.Title, &n.NoticeDate, &enabled, &n.SortOrder, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Enabled = enabled != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReplaceNotices 整表替换公告板（管理工具「保存全部」）。
//
// 语义：入参就是最终列表 —— 没带的行等于删除；带的行按传入顺序重排 sort_order。
// 全程一个事务：中途失败不留半份数据（否则登录器可能读到"删了一半"的公告板）。
func (s *Store) ReplaceNotices(ctx context.Context, items []LauncherNotice) ([]LauncherNotice, error) {
	if len(items) > NoticeMaxItems {
		return nil, errors.New("notice: too many items")
	}
	type row struct {
		title      string
		noticeDate string
		enabled    bool
	}
	prepared := make([]row, 0, len(items))
	for _, it := range items {
		title, err := NormalizeNoticeTitle(it.Title)
		if err != nil {
			return nil, err
		}
		date, err := normalizeNoticeDate(it.NoticeDate)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, row{title: title, noticeDate: date, enabled: it.Enabled})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM launcher_notices`); err != nil {
		return nil, err
	}
	for i, r := range prepared {
		enabled := 0
		if r.enabled {
			enabled = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO launcher_notices (title, notice_date, enabled, sort_order, created_at, updated_at)
			 VALUES (?, ?, ?, ?, NOW(6), NOW(6))`,
			r.title, r.noticeDate, enabled, i); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// 回读一次：让调用方拿到真实 id 与 created_at（管理工具要把它们带回下次保存）。
	return s.ListNotices(ctx, false)
}
