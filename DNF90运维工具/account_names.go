package main

// 账号显示的统一口径（运维工具本机版）：运维工具里**任何**要显示 account_id 的地方，
// 都必须先过这里换成真实账号名，不把 dnf:<sha256(用户名)> 这种不可逆主键摆给运营看。
//
// 与管理工具那份（DNF90管理工具\account_names.go）的区别：
//   - 管理工具是远程客户端，只能走 POST /manage/v1/account/names 问服务端；
//   - 本工具**就是账号库的主人**（launcher_accounts 在同一个 MySQL），所以直接查库翻译，
//     不绕 HTTP，也不需要"服务端版本过旧"那套降级话术。
//
// 两条口径与管理工具保持一致，别改：
//  1. 先查内存缓存，只把没翻译过的主键发去查库（一次 IN 查询，条数上限由 store 侧兜住）；
//  2. 查库失败**不报错、不弹窗**：退回显示原始主键 —— 翻译只是显示增强，
//     认证库没起时不该连"看一眼角色列表"都做不了。
//
// 为什么缓存与"查库"分成两半（包级缓存 + App 方法）：
// walk 的 TableModel.Value 是同步取值，拿不到 *App，所以显示只能走包级函数；
// 而查库需要 App（它持有 store 与"认证服务是否在跑"的判断），所以 I/O 留在 App 上。
//
// 已知限制：从未在登录器注册过、或外部工具造的 wpf-… 主键翻不出来，
// 此时按口径 2 显示原始主键。

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/store"
)

// acctNameLookupTimeout 账号名批量翻译的超时（只影响显示，不该拖住任何页面）。
const acctNameLookupTimeout = 15 * time.Second

// opsAccountNames 账号主键 → 真实账号名 的缓存（并发安全；GUI 线程与后台协程都会读）。
type opsAccountNames struct {
	mu    sync.Mutex
	names map[string]string
	// failed 只用于"失败日志只打一次"：认证库没起来时每次刷新都打一行会把有用信息淹掉。
	failed bool
}

// opsAcctNames 全局实例（各页签共用一份缓存）。
var opsAcctNames = &opsAccountNames{names: make(map[string]string)}

// text 账号显示文本：翻译到就显示真实账号名，翻译不到原样显示主键（绝不显示空白）。
func (r *opsAccountNames) text(accountID string) string {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return ""
	}
	r.mu.Lock()
	name := r.names[id]
	r.mu.Unlock()
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return id
}

// remember 把已知映射塞进缓存（接口本来就带用户名时直接用，省一次查库）。
func (r *opsAccountNames) remember(names map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, name := range names {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		r.names[strings.TrimSpace(id)] = name
	}
}

// missing 返回 ids 里"还没翻译过"的那部分（去重、忽略空值、保持原顺序）。
func (r *opsAccountNames) missing(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if r.names[id] == "" {
			out = append(out, id)
		}
	}
	return out
}

// store 把翻译结果并入缓存。
func (r *opsAccountNames) store(names map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = false
	for id, name := range names {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		r.names[id] = name
	}
}

// noteFailure 只在第一次（或失败状态被成功后重置后的第一次）失败时记日志。
func (r *opsAccountNames) noteFailure(err error) {
	r.mu.Lock()
	first := !r.failed
	r.failed = true
	r.mu.Unlock()
	if first {
		slog.Warn("账号名翻译失败，界面退回显示账号主键", "error", err)
	}
}

// opsAcctNameText 包级显示口径（表格模型与状态标签直接用）。
func opsAcctNameText(accountID string) string { return opsAcctNames.text(accountID) }

// acctNameStore 取账号库；认证服务没在跑时返回 nil。
//
// ⚠ 为什么不直接调 `a.manageStore()`：那个 helper 定义在 `remote_tab.go`，而那个文件带
// `//go:build paid` 标签 —— 免费版编译时根本不存在它，这里一调**免费版就编不过**（实测踩到）。
// 判断逻辑与它保持一致（判的就是 `a.svcOn && a.st`，两个字段都声明在 main.go，两个版本都有）；
// 免费版没有认证库时按口径 2 降级：显示原始账号主键。
func (a *App) acctNameStore() *store.Store {
	if a == nil || !a.svcOn.Load() || a.st == nil {
		return nil
	}
	return a.st
}

// resolveAcctNames 把一批账号主键翻译成真名（查本机账号库；失败静默降级）。
//
// 调用时机：拿到数据、**上表/拼文案之前**。缓存命中就不查库，所以同一批数据反复刷新不会打库。
func (a *App) resolveAcctNames(ids []string) {
	pending := opsAcctNames.missing(ids)
	if len(pending) == 0 {
		return
	}
	st := a.acctNameStore()
	if st == nil {
		return // 认证服务没在跑：退回显示主键，不打扰用户
	}
	ctx, cancel := context.WithTimeout(context.Background(), acctNameLookupTimeout)
	defer cancel()
	names, err := st.AccountNamesByID(ctx, pending)
	if err != nil {
		opsAcctNames.noteFailure(err)
		return
	}
	opsAcctNames.store(names)
}

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
