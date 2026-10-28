// 登录器「服务器公告」栏（登录页左侧公告板，免 ticket）。
//
// 与管理工具写入的 launcher_notices 表对应：只返回**启用**的条目（按排序 + id 升序）。
// 没有推送、没有游标：登录器每次进登录页/定时拉一份当前列表即可 —— 公告板是"静态内容"，
// 不像喇叭公告那样要求实时。
package server

import (
	"net/http"
)

// noticeItem 公告栏的一条（无正文：标题 + 日期）。
type noticeItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Date  string `json:"notice_date"`
}

// noticesResponse GET /launcher/v1/notices 的 data 项。
type noticesResponse struct {
	Notices []noticeItem `json:"notices"`
}

// handleNoticeList 公开端点（无需 ticket）：登录器登录页读取公告板。
func (s *Service) handleNoticeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	rows, err := s.store.ListNotices(r.Context(), true)
	if err != nil {
		s.log.Error("notices list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "notices list failed")
		return
	}
	out := noticesResponse{Notices: make([]noticeItem, 0, len(rows))}
	for _, n := range rows {
		out.Notices = append(out.Notices, noticeItem{ID: n.ID, Title: n.Title, Date: n.NoticeDate})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": out})
}
