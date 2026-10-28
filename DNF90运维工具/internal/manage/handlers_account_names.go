//go:build paid

// 账号名批量翻译端点（POST /manage/v1/account/names）。
//
// 为什么需要独立端点：游戏侧到处存的都是账号主键 account_id = dnf:<sha256(用户名)>，
// 这个哈希**不可逆**，而"主键 ↔ 用户名"的映射表（launcher_accounts）只在运维工具侧的
// MySQL 里。管理工具的角色查询 / 在线角色 / 背包 / 兑换流水 / 封禁等界面拿到的都是主键，
// 直接显示出来运营没法用（截图里的 dnf:aa2e171fb68f22664692103a…）。
//
// 于是统一成一个只读翻译口：管理工具把**界面上所有要显示的主键**一次性 POST 过来，
// 拿回 {主键: 用户名}。这样以后管理工具再加新页签也不用再动服务端。
//
// 两个刻意的取舍：
//   - 查不到的主键**不进 map**，而是单独放进 missing：管理工具据此原样显示主键
//     （翻译失败不能把账号显示成空白，更不能把整页数据吞掉）；
//   - 一次最多 accountNamesMaxIDs 个：与 store.accountNameLookupMax 同口径，
//     底层按占位符拼 IN 查询，条数必须有上限。
package manage

import (
	"net/http"
	"strings"
)

// accountNamesMaxIDs 单次批量翻译的主键上限（与 store.accountNameLookupMax 一致）。
const accountNamesMaxIDs = 2000

// accountNamesRequest POST /manage/v1/account/names 入参。
type accountNamesRequest struct {
	AccountIDs []string `json:"account_ids"`
}

// accountNamesResult POST /manage/v1/account/names 响应。
type accountNamesResult struct {
	// AccountNames 只包含翻译成功的条目：account_id → 登录用户名。
	AccountNames map[string]string `json:"account_names"`
	// Missing 没查到用户名的主键（原样回显给管理工具，便于它显示主键而不是空白）。
	Missing []string `json:"missing,omitempty"`
}

// handleAccountNames 批量把账号主键翻成登录账号名。
func (s *Server) handleAccountNames(w http.ResponseWriter, r *http.Request) {
	var req accountNamesRequest
	if _, ok := s.acctDecode(w, r, &req); !ok {
		return
	}
	ids := normalizeAccountIDs(req.AccountIDs)
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: account_ids 不能为空")
		return
	}
	if len(ids) > accountNamesMaxIDs {
		writeErr(w, http.StatusBadRequest, "bad-request: account_ids 一次最多 2000 个")
		return
	}
	if s.bridge == nil || s.bridge.AccountNamesByID == nil {
		writeErr(w, http.StatusServiceUnavailable, "account-names-unavailable: 运维端未注入账号名翻译能力")
		return
	}
	names, err := s.bridge.AccountNamesByID(r.Context(), ids)
	if err != nil {
		s.log.Error("account names resolve failed", "count", len(ids), "error", err)
		writeErr(w, http.StatusBadGateway, "db-error: 账号名翻译失败: "+err.Error())
		return
	}
	if names == nil {
		names = map[string]string{}
	}
	missing := make([]string, 0, 4)
	for _, id := range ids {
		if strings.TrimSpace(names[id]) == "" {
			missing = append(missing, id)
		}
	}
	writeOK(w, accountNamesResult{AccountNames: names, Missing: missing})
}

// normalizeAccountIDs 去空、去重、保持原顺序（顺序只影响请求体可读性）。
func normalizeAccountIDs(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		id := strings.TrimSpace(v)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
