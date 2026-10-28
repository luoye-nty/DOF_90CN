// 全服公告（喇叭广播）：管理端 POST /manage/v1/announce 写入，
// 登录器长轮询 GET /launcher/v1/announcements/wait 拉取。
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Announcement 全服公告行。
type Announcement struct {
	ID        int64
	Content   string
	Color     string
	Operator  string
	CreatedAt time.Time
}

// CreateAnnouncement 写入一条全服公告，返回自增 id。
func (s *Store) CreateAnnouncement(ctx context.Context, content, color, operator string) (int64, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, errors.New("announcement: empty content")
	}
	color = strings.TrimSpace(color)
	if color == "" {
		color = "0xFF0000FF"
	}
	operator = strings.TrimSpace(operator)
	if operator == "" {
		operator = "admin"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO launcher_announcements (content, color, operator, created_at)
		 VALUES (?, ?, ?, NOW(6))`,
		content, color, operator)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAnnouncementsAfter 返回 id>afterID 的公告（升序；limit<=0 取 1）。
func (s *Store) ListAnnouncementsAfter(ctx context.Context, afterID int64, limit int) ([]Announcement, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, color, COALESCE(operator,''), created_at
		 FROM launcher_announcements
		 WHERE id > ?
		 ORDER BY id ASC
		 LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Content, &a.Color, &a.Operator, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
