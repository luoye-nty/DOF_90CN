//go:build paid

// 泡点点券管理端点：GET /manage/v1/pointsink 查询状态；
// POST /manage/v1/pointsink 修改并持久化配置（仅做协议编排，实现在主程序 Bridge）。
package manage

import (
	"errors"
	"net/http"
	"strings"
)

// pointsinkApplyRequest POST /manage/v1/pointsink 入参。
type pointsinkApplyRequest struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	Points          int  `json:"points"`
	// DailyLimit 账号级每日限额（当前币种的数量）：0 = 不限，上限 100000000。
	DailyLimit int `json:"daily_limit"`
	// Currency 发放币种（2026-10-05 起可选）：cera=点券（缺省）/ token_cera=代币券 /
	// happy_token_cera=欢乐代币券。
	//
	// **空值必须放行并按点券处理**：管理端可能还是老版本，不带这个字段；
	// 拒绝空值会让"老管理端改不了泡点配置"。
	Currency string `json:"currency"`
}

// pointsinkCurrencyKnown 放行三种钱包币种；空值也放行（= 未指定 → 点券）。
// 中文别名一并接受，便于手工构造请求。
func pointsinkCurrencyKnown(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "cera", "点券",
		"token_cera", "token", "代币券",
		"happy_token_cera", "happy_token", "happy", "欢乐代币券":
		return true
	default:
		return false
	}
}

// pointsinkApplyError 配置非法（Bridge 校验层错误统一描述）。
var pointsinkApplyError = errors.New("invalid-pointsink-config")

// handlePointsinkGet 返回泡点运行状态快照。
func (s *Server) handlePointsinkGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.PointsinkStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "pointsink-status-unavailable")
		return
	}
	writeOK(w, s.bridge.PointsinkStatus())
}

// handlePointsinkApply 校验并应用泡点配置。
func (s *Server) handlePointsinkApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.PointsinkApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "pointsink-apply-unavailable")
		return
	}
	var req pointsinkApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	// 与主程序共用的硬性合法区间：interval 10~86400，points 1~1000000。
	if req.IntervalSeconds < 10 || req.IntervalSeconds > 86400 {
		writeErr(w, http.StatusBadRequest, pointsinkApplyError.Error())
		return
	}
	if req.Points < 1 || req.Points > 1000000 {
		writeErr(w, http.StatusBadRequest, pointsinkApplyError.Error())
		return
	}
	// 每日限额：0 = 不限；上限与主程序 psMaxDailyLimit 一致。
	if req.DailyLimit < 0 || req.DailyLimit > 100000000 {
		writeErr(w, http.StatusBadRequest, pointsinkApplyError.Error())
		return
	}
	// 币种：空值放行（老管理端），非法值拒绝。
	if !pointsinkCurrencyKnown(req.Currency) {
		writeErr(w, http.StatusBadRequest, pointsinkApplyError.Error())
		return
	}
	status, err := s.bridge.PointsinkApply(r.Context(), req.Enabled, req.IntervalSeconds, req.Points, req.DailyLimit, req.Currency)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "apply-failed")
		return
	}
	writeOK(w, status)
}
