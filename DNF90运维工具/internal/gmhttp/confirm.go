// Package gmhttp 收敛"运维工具 → 游戏服回环管理口（/local/dnf-gm/*）"的公共请求约定。
//
// 目前只有一件事：危险操作确认头（X-Admin-Confirm）。
//
// 背景（2026-09-25 审计）：游戏服的 admin 中间件在 policy.RequireConfirmation=true 时，
// 对**所有**写方法（POST/PUT/PATCH/DELETE）要求 X-Admin-Confirm 必须等于该路由的
// operationID；对没有显式注册 operation 的路由，operationID 就是 "<METHOD> <PATH>"
// （见 go-server internal/platform/httpx/admin.go:91-94 与 142-152）。
//
// /local/dnf-gm/* 全部是 env.AdminAuth("dnf-gm", ...) 注册的、没有显式 operation，
// 所以这里按同一规则拼就行。现在线上 dnfbridge.toml 没开 dangerous_confirm，
// 这个头会被直接忽略；一旦有人打开它，缺这个头会让发邮件/发点券/改等级等**全部**变成
// 428（dangerous admin operation requires confirmation），而界面上只会看到失败。
package gmhttp

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// ConfirmHeader 危险操作确认头（值与 operationID 逐字相等）。
	ConfirmHeader = "X-Admin-Confirm"
	// IdempotencyHeader 幂等键头：dangerous 路由要求非空。
	// /local/dnf-gm/* 目前不是 dangerous 路由，带上它没有副作用（服务端只在
	// dangerous && 写方法 时才校验）。
	IdempotencyHeader = "X-Admin-Idempotency-Key"
)

var idempotencySeq atomic.Uint64

// Mutates 是否是写方法（与 go-server httpx.adminMethodMutates 同一口径）。
func Mutates(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// OperationID 复刻 go-server httpx.AdminOperationID 的拼法（"<METHOD> <PATH>"）。
func OperationID(method, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	return method + " " + path
}

// SetConfirm 给写请求补 X-Admin-Confirm（+ X-Admin-Idempotency-Key）。
//
// 只对写方法生效（读方法服务端不校验，发了也没意义）。
// 幂等键按"一次调用一个键"生成：这些路由本身不做命令去重（不是 dangerous 路由），
// 每次调用都是独立动作，键只用于满足"非空"约束。
func SetConfirm(req *http.Request, actor string) {
	if req == nil || !Mutates(req.Method) {
		return
	}
	// 注意用 URL.Path 而不是 RequestURI：operationID 用的是服务端看到的路径（不含查询串）。
	req.Header.Set(ConfirmHeader, OperationID(req.Method, req.URL.Path))
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "ops"
	}
	req.Header.Set(IdempotencyHeader,
		actor+"-"+time.Now().UTC().Format("20060102T150405.000000000")+"-"+strings.TrimSpace(itoa(idempotencySeq.Add(1))))
}

// itoa 只用于拼幂等键，避免为一个数字引入 strconv 之外的东西。
func itoa(value uint64) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[pos:])
}
