//go:build paid

// 双开（多开）开关端点（管理工具「服务器选项 → 双开开关」区）：
//   - GET  /manage/v1/multi-open：查询当前开关（玩家侧生效值 + 是否显式设置过）；
//   - POST /manage/v1/multi-open：应用并持久化（落盘 config.json，运维工具重启后保持）。
//
// 语义：关闭后，**新**登录器在自检阶段拿到 enabled:false，把「双开」勾选框与副账号
// 登录按钮置灰、文案改成「双开已关闭」，并在按钮回调里硬拦一道。
//
// ⚠ 关闭**不会**踢掉已经在运行的副账号（与「更新维护」故意不同：维护会 KillAll）。
//
//	管理工具 UI 必须把这一点写清楚，否则运营会以为"关了就能把双开的人赶下线"。
//
// ⚠ 老登录器（不认识这个开关）照旧能双开：本功能**不是**服务端硬拦。
//
//	服务端不参与双开许可判定（不同账号双开在服务端本来就是合法的），
//	所以这个开关的能力边界就是"新登录器自觉遵守"。
//
// 详见主程序 multi_open.go（三态语义与降级口径）。
package manage

import (
	"net/http"
)

// multiOpenApplyRequest POST /manage/v1/multi-open 入参。
//
// 用 *bool 而不是 bool：`{}`（漏带字段）解出来会是 false，等于"悄悄把全服双开关掉"。
// 缺字段必须报错让调用方修，而不是按零值生效。
type multiOpenApplyRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleMultiOpenGet 返回当前双开开关配置快照。
func (s *Server) handleMultiOpenGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.MultiOpenStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "multi-open-status-unavailable")
		return
	}
	writeOK(w, s.bridge.MultiOpenStatus())
}

// handleMultiOpenApply 应用并持久化双开开关。
//
// 这里**不做**任何"业务合法性"判断（bool 没有非法值）：校验只有一条 ——
// 请求体必须明确给出 enabled，否则整批拒绝、不落盘（见 multiOpenApplyRequest 的说明）。
func (s *Server) handleMultiOpenApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.MultiOpenApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "multi-open-apply-unavailable")
		return
	}
	var req multiOpenApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	if req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "缺少 enabled 字段（必须明确给出 true/false）")
		return
	}

	status, err := s.bridge.MultiOpenApply(r.Context(), *req.Enabled)
	if err != nil {
		s.log.Error("multi-open apply failed", "enabled", *req.Enabled, "error", err)
		writeErr(w, http.StatusInternalServerError, "保存双开开关失败: "+err.Error())
		return
	}
	s.log.Info("multi-open config applied", "enabled", status.Enabled, "configured", status.Configured)
	writeOK(w, status)
}
