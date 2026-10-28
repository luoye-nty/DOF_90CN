//go:build paid

// 在线福利端点（管理工具「在线福利」页）：
//   - GET  /manage/v1/welfare        ：三档期配置 + 下次计划时刻（服务端算好）
//   - POST /manage/v1/welfare        ：应用并持久化三档期配置
//   - POST /manage/v1/welfare/try    ：立即试发某个档期（不判定时间、不改 last_runs）
//
// 调度本身在运维工具里常驻运行（welfare.go），管理工具只负责读写配置与试发。
package manage

import (
	"context"
	"net/http"
	"strings"
	"time"

	"dnf90ops/internal/config"
)

// welfareApplyRequest POST /manage/v1/welfare 入参：直接就是配置结构。
type welfareApplyRequest = config.WelfareConfig

// welfareTryRequest POST /manage/v1/welfare/try 入参。
type welfareTryRequest struct {
	// Kind：daily / weekly / monthly。
	Kind string `json:"kind"`
}

// handleWelfareGet 返回在线福利状态快照。
func (s *Server) handleWelfareGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.WelfareStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "welfare-status-unavailable")
		return
	}
	writeOK(w, s.bridge.WelfareStatus())
}

// handleWelfareApply 应用三档期配置（服务端校验时间/星期/奖励合法性，非法整批拒绝）。
func (s *Server) handleWelfareApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.WelfareApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "welfare-apply-unavailable")
		return
	}
	var req welfareApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	status, err := s.bridge.WelfareApply(r.Context(), req)
	if err != nil {
		// 配置非法（时间格式/星期/奖励类型）是用户可修正的错误 → 400 并把原因原文回给前端。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("welfare config applied",
		"daily", status.Daily.Enabled, "weekly", status.Weekly.Enabled, "monthly", status.Monthly.Enabled)
	writeOK(w, status)
}

// welfareSlotApplyRequest POST /manage/v1/welfare/slot 入参。
type welfareSlotApplyRequest struct {
	// Kind：daily / weekly / monthly。
	Kind string `json:"kind"`
	// Slot 该档期的完整配置（只写这一档，其余档期保持不变）。
	//
	// 用**指针**是有意的：缺字段（老客户端、键名拼错）时反序列化出来是全零值，
	// 而"停用 + 无奖励"正好也是全零 —— 直接落盘就把这一档福利悄悄抹掉了，还返回 200。
	// 指针能区分"没传"（nil → 400，一个字节都不改）与"显式全零"（合法的清空操作）。
	Slot *config.WelfareSlot `json:"slot"`
}

// handleWelfareApplySlot 只保存一个档期（管理工具每个档期自己的「保存本档期」）。
//
// 起因（线上反馈）：整批保存会一起校验三个档期，只要别的档期"已启用但没奖励"，
// 运营就永远存不进自己想改的那一档；这里只校验并写入目标档期。
func (s *Server) handleWelfareApplySlot(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.WelfareApplySlot == nil {
		writeErr(w, http.StatusServiceUnavailable, "welfare-apply-slot-unavailable")
		return
	}
	var req welfareSlotApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case "daily", "weekly", "monthly":
	default:
		writeErr(w, http.StatusBadRequest, "kind 必须是 daily / weekly / monthly")
		return
	}
	// 少了 slot 对象就拒（见上面 Slot 字段的注释）：这是"防止把整档福利静默抹掉"的最后一道闸。
	if req.Slot == nil {
		writeErr(w, http.StatusBadRequest,
			"bad-request: 缺少 slot 对象（字段名应为 slot；本次未做任何修改，请升级管理工具后重试）")
		return
	}
	status, err := s.bridge.WelfareApplySlot(r.Context(), kind, *req.Slot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.log.Info("welfare slot applied", "kind", kind, "enabled", req.Slot.Enabled, "rewards", len(req.Slot.Rewards))
	writeOK(w, status)
}

// handleWelfareTry 立即试发（给运营验证配置用，不动 last_runs、不判定时间）。
func (s *Server) handleWelfareTry(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.WelfareTry == nil {
		writeErr(w, http.StatusServiceUnavailable, "welfare-try-unavailable")
		return
	}
	var req welfareTryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case "daily", "weekly", "monthly":
	default:
		writeErr(w, http.StatusBadRequest, "kind 必须是 daily / weekly / monthly")
		return
	}
	// 试发要现查在线玩家并逐个发邮件，给足时间。
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	res, err := s.bridge.WelfareTry(ctx, kind)
	if err != nil {
		s.log.Error("welfare try failed", "kind", kind, "error", err)
		writeErr(w, http.StatusInternalServerError, "试发失败: "+err.Error())
		return
	}
	s.log.Info("welfare try done", "kind", kind,
		"accounts", res.Accounts, "mails", res.ItemMails, "cera", res.CeraGranted, "failed", res.Failed)
	writeOK(w, res)
}
