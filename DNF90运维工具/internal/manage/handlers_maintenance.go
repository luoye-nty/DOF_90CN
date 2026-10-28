//go:build paid

// 更新维护端点（管理工具「更新维护」区）：
//   - GET  /manage/v1/maintenance：查询维护开关 + 白名单；
//   - POST /manage/v1/maintenance：应用并持久化（落盘 config.json，运维工具重启后保持）。
//
// 语义：维护开启时禁止玩家**登录**与**进游戏**（注册照常）；白名单账号照常放行。
// 服务端在保存成功后立刻广播给所有登录器长轮询 —— 开始维护按钮熄灭并结束游戏进程，
// 取消维护按钮立刻亮起，不需要玩家重开登录器。
package manage

import (
	"net/http"
	"strings"

	"dnf90ops/internal/store"
)

// 白名单容量与单项长度上限：账号名本身是 3~32 位，这里放宽到 64 只是为了给出更友好的
// 400 提示（真正的归一化与去重由主程序 maintNormalizeAllowUsers 负责）。
const (
	maintenanceMaxAllowUsers = 50
	maintenanceMaxNameLen    = 64
)

// maintenanceApplyRequest POST /manage/v1/maintenance 入参。
type maintenanceApplyRequest struct {
	Enabled bool `json:"enabled"`
	// AllowUsers 账号名数组（也容忍前端传 "a,b,c" 这种单串，见 splitAllowUsers）。
	AllowUsers []string `json:"allow_users"`
}

// handleMaintenanceGet 返回当前维护配置快照。
func (s *Server) handleMaintenanceGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.MaintenanceStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "maintenance-status-unavailable")
		return
	}
	status := s.bridge.MaintenanceStatus()
	if status.AllowUsers == nil {
		status.AllowUsers = []string{}
	}
	writeOK(w, status)
}

// handleMaintenanceApply 应用并持久化维护配置；成功后服务端广播给所有登录器。
func (s *Server) handleMaintenanceApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.MaintenanceApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "maintenance-apply-unavailable")
		return
	}
	var req maintenanceApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	raw := splitAllowUsers(req.AllowUsers)
	if len(raw) > maintenanceMaxAllowUsers {
		writeErr(w, http.StatusBadRequest, "白名单最多 50 个账号")
		return
	}
	users := make([]string, 0, len(raw))
	for _, name := range raw {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if len([]rune(trimmed)) > maintenanceMaxNameLen {
			writeErr(w, http.StatusBadRequest, "白名单账号名过长：最多 64 个字符")
			return
		}
		users = append(users, store.NormalizeUsername(trimmed))
	}
	status, err := s.bridge.MaintenanceApply(r.Context(), req.Enabled, users)
	if err != nil {
		s.log.Error("maintenance apply failed", "enabled", req.Enabled, "error", err)
		writeErr(w, http.StatusInternalServerError, "保存维护配置失败: "+err.Error())
		return
	}
	if status.AllowUsers == nil {
		status.AllowUsers = []string{}
	}
	s.log.Info("maintenance applied", "enabled", status.Enabled, "allow_users", len(status.AllowUsers))
	writeOK(w, status)
}

// splitAllowUsers 容忍两种入参形态：字符串数组，或一条 "a,b c" 的串（前端少写一层数组时不报错）。
func splitAllowUsers(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		if !strings.ContainsAny(item, ",; \t") {
			out = append(out, item)
			continue
		}
		for _, part := range strings.FieldsFunc(item, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t'
		}) {
			out = append(out, part)
		}
	}
	return out
}
