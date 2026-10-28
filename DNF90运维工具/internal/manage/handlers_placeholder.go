//go:build paid

// P0 占位端点：POST /manage/v1/announce、POST /manage/v1/pvf/info。
// 仅提供 501 响应壳子，不实装（后续版本扩展公告与 PVF 信息接口时替换 handler）。
package manage

import "net/http"

func (s *Server) handleNotImplemented(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "not-implemented")
}
