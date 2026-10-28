//go:build paid

// 定时公告端点（管理工具「服务器选项 → 定时公告」页）：
//   - GET  /manage/v1/announce-schedule ：列表（含服务端算好的下次计划时刻）
//   - POST /manage/v1/announce-schedule ：整批保存（新增/修改/删除/停用都在这一条里）
//
// 调度本身在运维工具里常驻运行（announce_schedule.go，与在线福利共用一个 30 秒 loop），
// 到点走的就是「立即发送」那条链路：写 launcher_announcements + 广播给在线登录器。
package manage

import (
	"net/http"

	"dnf90ops/internal/config"
)

// announceScheduleApplyRequest POST /manage/v1/announce-schedule 入参。
//
// 整批替换语义（不是增量补丁）：管理工具把表格里当前所有行原样发回来，
// 服务端校验后整体落盘 —— 删除一行就是"这次没带它"。
type announceScheduleApplyRequest struct {
	Items []config.ScheduledAnnouncement `json:"items"`
}

// handleAnnounceScheduleGet 返回定时公告列表。
func (s *Server) handleAnnounceScheduleGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AnnounceScheduleStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "announce-schedule-unavailable")
		return
	}
	writeOK(w, s.bridge.AnnounceScheduleStatus())
}

// handleAnnounceScheduleApply 整批保存定时公告（内容/时间/日期/星期非法则整批拒绝）。
func (s *Server) handleAnnounceScheduleApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AnnounceScheduleApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "announce-schedule-unavailable")
		return
	}
	var req announceScheduleApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	items, err := s.bridge.AnnounceScheduleApply(r.Context(), req.Items)
	if err != nil {
		// 校验失败是运营可自行修正的错误 → 400 并把原因原文回给前端。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := 0
	for _, it := range items {
		if it.Enabled {
			enabled++
		}
	}
	s.log.Info("announce schedule applied", "total", len(items), "enabled", enabled)
	writeOK(w, items)
}
