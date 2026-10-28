// 「服务端指定 DLL」对登录器的公开端点（免 ticket）。
//
// 登录器（外网版）在启动自检与玩家点"登录"时各拉一次：拿到清单后在客户端根目录
// （DNF.exe 同目录）校验同名文件的 SHA256，通过才在启动游戏时注入。
//
// 只返回**启用**的条目，且不返回任何服务器本地路径 —— 登录器只需要文件名与哈希。
package server

import (
	"net/http"
)

// dllRequirementItem 一条 dll 要求。
type dllRequirementItem struct {
	// FileName 只含文件名（客户端根目录下的相对文件名）。
	FileName string `json:"file_name"`
	// SHA256 小写 64 位 hex；登录器算完本地文件哈希后逐字符比对。
	SHA256 string `json:"sha256"`
	// OnMismatch 缺文件/哈希不符时的处理：block=拒绝启动游戏（登录器弹窗），skip=跳过注入。
	OnMismatch string `json:"on_mismatch"`
}

// dllsResponse GET /launcher/v1/dlls 的 data 项。
type dllsResponse struct {
	DLLs []dllRequirementItem `json:"dlls"`
}

// handleDLLList 公开端点（无需 ticket）：登录器读取"服务端指定 DLL"清单。
func (s *Service) handleDLLList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	rows, err := s.store.ListDLLs(r.Context(), true)
	if err != nil {
		s.log.Error("dlls list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "dlls list failed")
		return
	}
	out := dllsResponse{DLLs: make([]dllRequirementItem, 0, len(rows))}
	for _, item := range rows {
		out.DLLs = append(out.DLLs, dllRequirementItem{
			FileName:   item.FileName,
			SHA256:     item.SHA256,
			OnMismatch: item.OnMismatch,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": out})
}
