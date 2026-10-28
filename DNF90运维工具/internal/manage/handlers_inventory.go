//go:build paid

// 背包 / 仓库 / 邮箱 管理端点（/manage/v1/account/inventory/*、/manage/v1/account/mailbox/*）。
//
// 为什么单独一个文件、走另一条通道：
//
//	handlers_account.go 里的 AC-* 端点全部转发 /local/dnf-gm/*（旧原生接口）。
//	背包与仓库的查询/删除、邮箱列表与定向删除只存在于 /dnf/gm/*（GM Tool 接口，
//	信封 {success,data,error}），因此这里用 Bridge.AcctGMTCall 转发，信封解析与鉴权头
//	（X-Admin-Confirm / X-Admin-Idempotency-Key）由主程序侧统一处理。
//	审计仍复用 handlers_account.go 的 acctAudit，落同一份 audit/manage_account.jsonl。
//
// 在线约束（必须原样上抛给管理端，不能吞掉）：
//
//	服务端 GMDeleteInventoryItem / GMClearInventory / GMDeleteMailboxMails 第一句就是
//	在线判定，角色在线返回 409。这里映射成机器可读的 character-online 前缀，
//	让管理工具能据此提示"改用清空邮箱 / 让角色下线"，而不是显示一句无头无尾的 502。
//	唯一的例外是邮箱清空：它转发 /local/dnf-gm/mail/clear（支持在线并推送客户端刷新）。
package manage

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// timeNow 便于统一时间来源（审计与日志都用它）。
func timeNow() time.Time { return time.Now() }

// pathEscape 转义 URL 路径段（角色 ID 是十进制数字，转义只为防御性写法）。
func pathEscape(value string) string { return url.PathEscape(value) }

// GM Tool 操作 ID：与 go-server internal/app/dnf90/gm_tool_admin.go 注册时传入 write() 的
// 常量逐字相同（含 {character_id} 字面占位符），否则服务端 dangerous_confirm 打开时会 428。
const (
	gmtOpInventoryGet    = "GET /dnf/gm/characters/{character_id}/inventory"
	gmtOpInventoryDelete = "POST /dnf/gm/characters/{character_id}/inventory/delete"
	gmtOpInventoryClear  = "POST /dnf/gm/characters/{character_id}/inventory/clear"
	gmtOpMailboxGet      = "GET /dnf/gm/characters/{character_id}/mailbox"
	gmtOpMailboxDelete   = "POST /dnf/gm/characters/{character_id}/mailbox/delete"
)

// gmtMaxMailIDs 与服务端 GMDeleteMailboxMails 的硬上限一致。
const gmtMaxMailIDs = 100

// gmtCall 转发 GM Tool 接口（actor 固定 manage）。
func (s *Server) gmtCall(ctx context.Context, method, path, operation string, body, out any) error {
	if s.bridge == nil || s.bridge.AcctGMTCall == nil {
		return errGMNotAvailable
	}
	return s.bridge.AcctGMTCall(ctx, method, path, operation, body, out, "manage")
}

// errGMNotAvailable 运维端未注入 GM Tool 通道能力（旧版运维工具）。
var errGMNotAvailable = &gmtError{"gm-not-available: 运维端未注入 GM Tool 直连能力，请升级运维工具"}

type gmtError struct{ text string }

func (e *gmtError) Error() string { return e.text }

// classifyGMToolError 把 GM Tool 转发错误映射为 HTTP 状态 + 机器可读前缀。
func classifyGMToolError(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}
	text := err.Error()
	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(text, "gm-not-available:"),
		strings.Contains(text, "未配置服务端根目录"),
		strings.Contains(text, "adminListen"),
		strings.Contains(text, "adminToken"),
		strings.Contains(text, "instance.json"),
		strings.Contains(text, "回环"),
		strings.HasPrefix(text, "连接 DNF90Server GM Tool 接口:"),
		strings.HasPrefix(text, "解析 DNF90Server GM Tool 响应:"),
		strings.HasPrefix(text, "解析 DNF90Server GM Tool 数据:"):
		return http.StatusBadGateway, "gm-not-available: " + text
	case strings.Contains(lower, "character is online") || strings.Contains(text, "角色在线"):
		return http.StatusConflict, "character-online: " + text
	case strings.Contains(lower, "character not found") || strings.Contains(text, "角色不存在"),
		strings.Contains(lower, "mailbox is empty"):
		return http.StatusNotFound, "role-not-found: " + text
	case strings.Contains(lower, "read-only") || strings.Contains(lower, "unsupported"),
		strings.Contains(lower, "invalid request"),
		strings.Contains(lower, "must contain 1..100"),
		strings.Contains(text, "不能为空"):
		return http.StatusBadRequest, "bad-request: " + text
	}
	return http.StatusBadGateway, text
}

// gmtWriteError 记录审计并写出带 code 的错误信封。
func (s *Server) gmtWriteError(w http.ResponseWriter, audit *acctAudit, err error) {
	if audit != nil {
		audit.fail(err)
	}
	status, text := classifyGMToolError(err)
	writeErr(w, status, text)
}

// ================= 背包 / 仓库 =================

// POST /manage/v1/account/inventory/query {character_id}
// 返回角色完整库存快照：container ∈ inventory(背包) / warehouse(仓库) / account(账号金库) / equipment(装备栏，只读)。
func (s *Server) handleAccountInventoryQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = req.CharacterID

	var snapshot GMTInventorySnapshot
	if err := s.gmtCall(r.Context(), http.MethodGet,
		"/dnf/gm/characters/"+pathEscape(req.CharacterID)+"/inventory", gmtOpInventoryGet, nil, &snapshot); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, snapshot)
}

// POST /manage/v1/account/inventory/delete {character_id, container, key, count}
// count=0（或 ≥ 持有数）= 整格删除；否则按数量扣减。要求角色离线。
func (s *Server) handleAccountInventoryDelete(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Container   string `json:"container"`
		Key         string `json:"key"`
		Count       int64  `json:"count"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	req.Container = strings.ToLower(strings.TrimSpace(req.Container))
	req.Key = strings.TrimSpace(req.Key)
	if req.CharacterID == "" || req.Container == "" || req.Key == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: character_id / container / key 都不能为空")
		return
	}
	if req.Count < 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: count 不能为负")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["container"] = req.Container
	audit.fields["key"] = req.Key
	audit.fields["count"] = req.Count

	body := map[string]any{"container": req.Container, "key": req.Key, "count": req.Count}
	var snapshot GMTInventorySnapshot
	if err := s.gmtCall(r.Context(), http.MethodPost,
		"/dnf/gm/characters/"+pathEscape(req.CharacterID)+"/inventory/delete", gmtOpInventoryDelete, body, &snapshot); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, snapshot)
}

// POST /manage/v1/account/inventory/clear {character_id, container}
// 清空整个容器，不可撤销。要求角色离线。
func (s *Server) handleAccountInventoryClear(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Container   string `json:"container"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	req.Container = strings.ToLower(strings.TrimSpace(req.Container))
	if req.CharacterID == "" || req.Container == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: character_id / container 都不能为空")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["container"] = req.Container

	body := map[string]any{"container": req.Container}
	var snapshot GMTInventorySnapshot
	if err := s.gmtCall(r.Context(), http.MethodPost,
		"/dnf/gm/characters/"+pathEscape(req.CharacterID)+"/inventory/clear", gmtOpInventoryClear, body, &snapshot); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, snapshot)
}

// ================= 邮箱 =================

// POST /manage/v1/account/mailbox/query {character_id}
func (s *Server) handleAccountMailboxQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = req.CharacterID

	var snapshot GMTMailboxSnapshot
	if err := s.gmtCall(r.Context(), http.MethodGet,
		"/dnf/gm/characters/"+pathEscape(req.CharacterID)+"/mailbox", gmtOpMailboxGet, nil, &snapshot); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, snapshot)
}

// POST /manage/v1/account/mailbox/delete {character_id, mail_ids, force}
// 定向删除最多 100 封邮件；force=false 时服务端会拒绝删除仍带未领取资产的邮件。要求角色离线。
func (s *Server) handleAccountMailboxDelete(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string   `json:"character_id"`
		MailIDs     []string `json:"mail_ids"`
		Force       bool     `json:"force"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	cleaned := make([]string, 0, len(req.MailIDs))
	seen := make(map[string]struct{}, len(req.MailIDs))
	for _, item := range req.MailIDs {
		id := strings.TrimSpace(item)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cleaned = append(cleaned, id)
	}
	if len(cleaned) == 0 || len(cleaned) > gmtMaxMailIDs {
		writeErr(w, http.StatusBadRequest, "bad-request: mail_ids 需要 1~100 条")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["mail_count"] = len(cleaned)
	audit.fields["force"] = req.Force

	body := map[string]any{"mail_ids": cleaned, "force": req.Force}
	var result GMTMailDeleteResult
	if err := s.gmtCall(r.Context(), http.MethodPost,
		"/dnf/gm/characters/"+pathEscape(req.CharacterID)+"/mailbox/delete", gmtOpMailboxDelete, body, &result); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, result)
}

// POST /manage/v1/account/mailbox/clear {character_id, character_name, confirm}
// 清空整个邮箱：转发 /local/dnf-gm/mail/clear（旧原生接口），角色在线也能用，
// 且服务端会向在线客户端推送未读提示 + 完整邮箱列表，玩家立刻看到邮箱空了。
func (s *Server) handleAccountMailboxClear(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID   string `json:"character_id"`
		CharacterName string `json:"character_name"`
		Confirm       bool   `json:"confirm"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	req.CharacterName = strings.TrimSpace(req.CharacterName)
	if req.CharacterID == "" && req.CharacterName == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: character_id 与 character_name 至少填一个")
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "bad-request: 清空邮箱必须带 confirm=true")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["character_name"] = req.CharacterName

	body := map[string]any{"confirm": true}
	if req.CharacterID != "" {
		body["character_id"] = req.CharacterID
	}
	if req.CharacterName != "" {
		body["character_name"] = req.CharacterName
	}
	var result GMTMailboxClearResult
	if _, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/mail/clear", body, &result); err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, result)
}
