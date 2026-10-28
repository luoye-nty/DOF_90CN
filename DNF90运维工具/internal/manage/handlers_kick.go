//go:build paid

// 批量踢下线端点（管理工具「账号管理 → 在线角色表」的两个按钮）：
//   - POST /manage/v1/account/kick-all  ：先把在线账号列出来，再逐个踢；
//   - POST /manage/v1/account/kick      ：踢请求里点名的账号。
//
// 两者最终都走同一个 bridge 钩子 AdminKickUsers —— 踢下线不是一个动作而是三件事：
// 断开在线游戏连接、写 GM 踢人事件（登录器据此结束本地游戏进程）、作废登录 ticket。
// 循环放在服务端：管理工具只发一次请求，不受客户端超时限制。
package manage

import (
	"context"
	"net/http"
	"strings"
	"time"

	"dnf90ops/internal/store"
)

// adminKickMaxUsers 单次批量踢人上限：一次点几百个已经远超正常运维场景，
// 上限只是防止误操作（比如把整张表塞进请求体）。
const adminKickMaxUsers = 1000

// adminKickRequest POST /manage/v1/account/kick 入参。
type adminKickRequest struct {
	Usernames []string `json:"usernames"`
}

// handleAccountKickUsers 踢掉请求里点名的账号（在线列表勾选后调用）。
func (s *Server) handleAccountKickUsers(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AdminKickUsers == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-kick-unavailable")
		return
	}
	var req adminKickRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}
	names := normalizeKickUsernames(req.Usernames)
	if len(names) == 0 {
		writeErr(w, http.StatusBadRequest, "请先勾选要踢下线的在线玩家")
		return
	}
	if len(names) > adminKickMaxUsers {
		writeErr(w, http.StatusBadRequest, "一次最多踢 1000 个账号")
		return
	}
	// 整体超时：账号多时逐个 GM 调用会拉长，给足 60 秒。
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.bridge.AdminKickUsers(ctx, names)
	if err != nil {
		s.log.Error("batch kick failed", "count", len(names), "error", err)
		writeErr(w, http.StatusInternalServerError, "批量踢下线失败: "+err.Error())
		return
	}
	s.log.Info("batch kick done", "requested", res.Requested, "kicked", res.Kicked, "sessions", res.Sessions, "failed", len(res.Failed))
	writeOK(w, res)
}

// handleAccountKickAll 踢掉全部在线账号（先列在线，再逐个踢）。
func (s *Server) handleAccountKickAll(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.AdminKickUsers == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-kick-unavailable")
		return
	}
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// 与「在线角色表」同源：先取登录器会话未过期的在线账号。
	accounts, err := st.ListOnlineAccounts(ctx, adminKickMaxUsers)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "db-error: 读取在线会话失败: "+err.Error())
		return
	}
	names := make([]string, 0, len(accounts))
	for _, acc := range accounts {
		if name := store.NormalizeUsername(acc.Username); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		// 没有在线账号不是错误：回一个零结果，管理工具照常显示"当前没有在线玩家"。
		writeOK(w, AdminKickResult{Failed: []string{}})
		return
	}
	res, err := s.bridge.AdminKickUsers(ctx, names)
	if err != nil {
		s.log.Error("kick all failed", "count", len(names), "error", err)
		writeErr(w, http.StatusInternalServerError, "全部踢下线失败: "+err.Error())
		return
	}
	s.log.Info("kick all done", "requested", res.Requested, "kicked", res.Kicked, "sessions", res.Sessions, "failed", len(res.Failed))
	writeOK(w, res)
}

// normalizeKickUsernames 账号名去空白、去重（保持首次出现顺序）。
func normalizeKickUsernames(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	return out
}
