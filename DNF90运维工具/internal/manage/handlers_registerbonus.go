//go:build paid

// 注册赠送点券管理端点：GET /manage/v1/register-bonus 查询配置；
// POST /manage/v1/register-bonus 修改并持久化配置（仅做协议编排，实现在主程序 Bridge）。
package manage

import (
	"errors"
	"net/http"
	"strings"
)

// registerBonusApplyRequest POST /manage/v1/register-bonus 入参。
type registerBonusApplyRequest struct {
	Enabled bool `json:"enabled"`
	Points  int  `json:"points"`
	// Currency 赠送币种：cera=点券（缺省）/ token_cera=代币券。
	Currency string `json:"currency"`
}

// registerBonusCurrencyKnown 只放行两种币种（空值按点券，兼容老管理端）。
func registerBonusCurrencyKnown(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "cera", "token_cera":
		return true
	default:
		return false
	}
}

// registerBonusApplyError 配置非法（Bridge 校验层错误统一描述）。
var registerBonusApplyError = errors.New("invalid-register-bonus-config")

// handleRegisterBonusGet 返回注册赠送配置快照。
func (s *Server) handleRegisterBonusGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegisterBonusStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "register-bonus-status-unavailable")
		return
	}
	writeOK(w, s.bridge.RegisterBonusStatus())
}

// handleRegisterBonusApply 校验并应用注册赠送配置（points 与泡点同区间 1~1000000；
// currency 只允许 cera / token_cera）。
func (s *Server) handleRegisterBonusApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RegisterBonusApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "register-bonus-apply-unavailable")
		return
	}
	var req registerBonusApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	if req.Points < 1 || req.Points > 1000000 {
		writeErr(w, http.StatusBadRequest, registerBonusApplyError.Error())
		return
	}
	if !registerBonusCurrencyKnown(req.Currency) {
		writeErr(w, http.StatusBadRequest, registerBonusApplyError.Error())
		return
	}
	status, err := s.bridge.RegisterBonusApply(r.Context(), req.Enabled, req.Points, req.Currency)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "apply-failed")
		return
	}
	writeOK(w, status)
}
