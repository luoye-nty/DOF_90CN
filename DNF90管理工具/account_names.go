package main

// 账号显示的统一口径：管理工具里**任何**要显示 account_id 的地方，都必须先过这里
// 换成真实账号名，绝不允许把 dnf:<sha256(用户名)> 这种主键直接摆到运营面前。
//
// 为什么不能在本地算：账号主键是 `dnf:<sha256(用户名)>`，哈希不可逆；"主键 → 用户名"
// 的映射表（launcher_accounts）只在服务器端运维工具的 MySQL 里。管理工具只能问服务端：
//   POST /manage/v1/account/names  {"account_ids":[...]}  → {"account_names":{...}}
// （服务端实现见运维工具 internal/manage/handlers_account_names.go）
//
// 三条口径，改动时不要破：
//  1. **先查缓存**：同一批数据反复刷新不会重复打服务端（缓存只活在内存，进程重启即清空）；
//  2. **翻译失败不报错、不弹窗**：退回显示原始主键。服务器端运维工具还没升级到带该路由的
//     版本时（老版本会回 404 not-found），管理工具除"显示真名"以外的功能必须照常可用，
//     所以这里只记一条日志，绝不把失败弹给运营；
//  3. **缓存只增不失效**：用户名在登录器侧基本不变（换用户名等于换账号），
//     真变了重启管理工具即可 —— 不做 TTL 是不想为了极小概率让每次刷新都多发一次请求。
//
// 已知限制：一个账号若从未在登录器注册过、或记录来自外部工具（wpf-… 这类主键），
// 服务端也翻不出来，此时界面按口径 2 显示原始主键。

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// acctNameTimeout 账号名批量翻译的超时（只影响显示，不该拖住任何页面）。
const acctNameTimeout = 15 * time.Second

// accountNameResolver 账号主键 → 真实账号名 的批量翻译缓存（并发安全）。
type accountNameResolver struct {
	mu    sync.Mutex
	names map[string]string
	// failed 记录"上次请求是否失败"，用于只把**第一次**失败写进日志：
	// 老服务端会一直 404，每次刷新都打一行日志只会把有用信息淹掉。
	failed bool
}

// newAccountNameResolver 造一个空缓存（测试用；生产用包级 acctNames）。
func newAccountNameResolver() *accountNameResolver {
	return &accountNameResolver{names: make(map[string]string)}
}

// acctNames 各页签共用的全局缓存。
var acctNames = newAccountNameResolver()

// text 账号显示文本：翻译到就显示真实账号名，翻译不到原样显示主键（绝不显示空白）。
//
// 表格模型（walk.TableModel.Value）是同步取值，所以显示一律走这个读缓存的方法：
// 名字翻译回来后再 PublishRowsReset 重画一次，列里就自动变成真名了。
func (r *accountNameResolver) text(accountID string) string {
	return textWithFallback(r.cached(accountID), accountID)
}

// cached 取缓存里的账号名（没有则空串）。
func (r *accountNameResolver) cached(accountID string) string {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.names[id]
}

// textWithFallback 缓存值 → 显示文本的兜底：没有名字就原样回显主键。
func textWithFallback(name, accountID string) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(accountID)
}

// resolve 把 ids 里"还没翻译过"的主键批量问一次服务端，返回这批 ids 能翻出来的映射。
//
// 返回的 map 只包含翻译成功的键：调用方显示时用 text() 兜底，别拿 map 的零值当账号名。
// c 为 nil（未登录）或请求失败时返回已缓存的部分，绝不返回错误 —— 翻译是显示增强，
// 不是业务流程的一环。
func (r *accountNameResolver) resolve(ctx context.Context, c *APIClient, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	missing := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	r.mu.Lock()
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if name := r.names[id]; name != "" {
			out[id] = name
			continue
		}
		missing = append(missing, id)
	}
	r.mu.Unlock()

	// 未登录（c 为 nil）时只回缓存里已有的部分：没有会话就不该发请求，
	// 但也不能把已经翻译好的名字丢掉。
	if c == nil || len(missing) == 0 {
		return out
	}
	names, err := c.AcctNames(ctx, missing)
	if err != nil {
		r.noteFailure(err)
		return out
	}
	r.mu.Lock()
	r.failed = false
	for id, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		r.names[id] = name
		out[id] = name
	}
	r.mu.Unlock()
	return out
}

// noteFailure 只在第一次（或失败状态被成功后重置后的第一次）失败时记日志。
func (r *accountNameResolver) noteFailure(err error) {
	r.mu.Lock()
	first := !r.failed
	r.failed = true
	r.mu.Unlock()
	if first {
		// 老版本运维工具没有 /manage/v1/account/names 路由，会回 404；这是预期内的降级路径。
		log.Printf("账号名批量翻译失败，界面退回显示账号主键（服务器端运维工具可能未升级）：%v", err)
	}
}

// remember 直接塞入已知映射（供测试与"接口本来就带用户名"的场景复用缓存）。
func (r *accountNameResolver) remember(names map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, name := range names {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		r.names[strings.TrimSpace(id)] = name
	}
}

// resolveAccountNames 便捷入口：用当前登录客户端翻译一批账号主键。
// 未登录 / 请求失败时返回能拿到的部分（可能是空 map），调用方不必判错。
func (a *App) resolveAccountNames(ids []string) map[string]string {
	c := a.snapshotClient()
	if c == nil {
		return map[string]string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), acctNameTimeout)
	defer cancel()
	return acctNames.resolve(ctx, c, ids)
}

// acctNameText 包级显示口径（表格模型与状态标签直接用）。
func acctNameText(accountID string) string { return acctNames.text(accountID) }

// acctNameList 收集一批账号主键（去重、忽略空值），供批量翻译用。
func acctNameList(ids ...string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
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
