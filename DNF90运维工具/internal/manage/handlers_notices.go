//go:build paid

// 登录器「服务器公告」栏端点（管理工具「服务器公告」分组）：
//   - GET  /manage/v1/notices ：当前公告栏全部条目（含停用，供管理工具维护）
//   - POST /manage/v1/notices ：整表保存（新增 / 改标题日期 / 停用 / 删除都在这一条里）
//
// 与「全服公告」（launcher_announcements，逐条喇叭推送）完全是两回事：
// 这里是登录页左侧那块常驻公告板，玩家每次打开登录器拉一份当前列表即可。
package manage

import (
	"net/http"

	"dnf90ops/internal/store"
)

// noticesApplyRequest POST /manage/v1/notices 入参。
type noticesApplyRequest struct {
	Items []store.LauncherNotice `json:"items"`
}

// handleNoticesGet 返回公告栏全部条目（升序）。
func (s *Server) handleNoticesGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.NoticesList == nil {
		writeErr(w, http.StatusServiceUnavailable, "notices-unavailable")
		return
	}
	items, err := s.bridge.NoticesList(r.Context())
	if err != nil {
		s.log.Error("notices list failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "notices-list-failed: "+err.Error())
		return
	}
	writeOK(w, items)
}

// handleNoticesApply 整表保存公告栏（标题为空 / 日期格式非法 → 400 且整批不落盘）。
func (s *Server) handleNoticesApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.NoticesReplace == nil {
		writeErr(w, http.StatusServiceUnavailable, "notices-unavailable")
		return
	}
	var req noticesApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	items, err := s.bridge.NoticesReplace(r.Context(), req.Items)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := 0
	for _, it := range items {
		if it.Enabled {
			enabled++
		}
	}
	s.log.Info("launcher notices replaced", "total", len(items), "enabled", enabled)
	writeOK(w, items)
}
