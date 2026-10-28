//go:build paid

// 「服务端指定 DLL」端点（管理工具「服务器选项 → 可加载 DLL」页）：
//   - GET  /manage/v1/dlls ：当前清单（含停用条目）
//   - POST /manage/v1/dlls ：整表保存（新增 / 改哈希 / 停用 / 删除都在这一条请求里）
//
// 只存文件名 + SHA256：dll 本体由服主随客户端分发，登录器在客户端根目录校验哈希后注入。
package manage

import (
	"net/http"

	"dnf90ops/internal/store"
)

// dllsApplyRequest POST /manage/v1/dlls 入参。
type dllsApplyRequest struct {
	Items []store.LauncherDLL `json:"items"`
}

// handleDLLsGet 返回全部条目（含停用，供管理工具维护）。
func (s *Server) handleDLLsGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.DLLList == nil {
		writeErr(w, http.StatusServiceUnavailable, "dlls-unavailable")
		return
	}
	items, err := s.bridge.DLLList(r.Context())
	if err != nil {
		s.log.Error("dlls list failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "dlls-list-failed: "+err.Error())
		return
	}
	writeOK(w, items)
}

// handleDLLsApply 整表保存（文件名/SHA256/策略非法 → 400，且整批不落盘）。
func (s *Server) handleDLLsApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.DLLReplace == nil {
		writeErr(w, http.StatusServiceUnavailable, "dlls-unavailable")
		return
	}
	var req dllsApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	items, err := s.bridge.DLLReplace(r.Context(), req.Items)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled, blocking := 0, 0
	for _, item := range items {
		if item.Enabled {
			enabled++
		}
		if item.OnMismatch == store.DLLMismatchBlock {
			blocking++
		}
	}
	s.log.Info("launcher dlls replaced", "total", len(items), "enabled", enabled, "blocking", blocking)
	writeOK(w, items)
}
