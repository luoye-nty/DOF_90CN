// 注册赠送点券（服务器端常驻能力）：
//   - 新账号注册成功后立即向账号点券 dnf_account_metadata(account_cera) 入账（best-effort，失败不阻断注册）；
//   - 配置存 config.json register_bonus 段，由管理端 /manage/v1/register-bonus 修改持久化；
//   - 入账成功写 launcher_gm_events(event_type=cera_granted)，登录器轮询到后走 system_notify 弹绿字。
package main

import (
	"context"
	"fmt"
	"strings"

	"dnf90ops/internal/server"
)

const (
	rbMinPoints = psMinPoints // 与泡点对齐
	rbMaxPoints = psMaxPoints // 与泡点对齐
)

// rbStat 注册赠送配置快照（与 paid 版 manage.RegisterBonusStatus 字段一一对应）。
type rbStat struct {
	Enabled  bool   `json:"enabled"`
	Points   int    `json:"points"`
	Currency string `json:"currency"`
}

// 钱包可选币种（口径与服务端 /local/dnf-gm/wallet/update 一致）。
//
// 注册赠送只放行前两种（2026-09-30 服主要求：点券或代币券）—— 那个限制在
// rbApply 与 config.Load 各自的校验里，**不在这里**，所以扩展本函数的识别集合
// 不会让注册赠送多出第三种币种。
// 泡点（2026-10-05）三种都放行，因为服务端钱包本来就支持三种。
const (
	rbCurrencyCera           = "cera"
	rbCurrencyTokenCera      = "token_cera"
	rbCurrencyHappyTokenCera = "happy_token_cera"
)

// rbNormalizeCurrency 收敛币种：识别点券 / 代币券 / 欢乐代币券，其余（含空值）一律按点券。
// 这样老配置（没有 currency 字段）不需要迁移，行为与改动前完全一致。
func rbNormalizeCurrency(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case rbCurrencyTokenCera, "token", "代币券":
		return rbCurrencyTokenCera
	case rbCurrencyHappyTokenCera, "happy_token", "happy", "欢乐代币券":
		return rbCurrencyHappyTokenCera
	default:
		return rbCurrencyCera
	}
}

// rbCurrencyLabel 币种中文名（日志 / 到账提示文案共用）。
func rbCurrencyLabel(currency string) string {
	switch rbNormalizeCurrency(currency) {
	case rbCurrencyTokenCera:
		return "代币券"
	case rbCurrencyHappyTokenCera:
		return "欢乐代币券"
	default:
		return "点券"
	}
}

// rbPublicInfo 返回注册赠送公开配置（认证服务启动注入用；线程安全）。
func (a *App) rbPublicInfo() server.RegisterBonusPublic {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	var out server.RegisterBonusPublic
	if a.cfg != nil && a.cfg.RegisterBonus != nil {
		out.Enabled = a.cfg.RegisterBonus.Enabled
		out.Points = a.cfg.RegisterBonus.Points
	}
	return out
}

// rbStatus 返回归一化后的注册赠送配置快照（非法值就近收敛）。
func (a *App) rbStatus() rbStat {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	out := rbStat{Currency: rbCurrencyCera}
	if a.cfg == nil || a.cfg.RegisterBonus == nil {
		return out
	}
	out.Enabled = a.cfg.RegisterBonus.Enabled
	out.Points = a.cfg.RegisterBonus.Points
	out.Currency = rbNormalizeCurrency(a.cfg.RegisterBonus.Currency)
	if out.Points < rbMinPoints {
		out.Points = rbMinPoints
	}
	if out.Points > rbMaxPoints {
		out.Points = rbMaxPoints
	}
	return out
}

// rbApply 应用并持久化注册赠送配置（points 硬约束与泡点对齐 1~1000000；
// currency 只认 cera / token_cera，非法值直接报错而不是静默改写）。
func (a *App) rbApply(_ context.Context, enabled bool, points int, currency string) (rbStat, error) {
	if points < rbMinPoints || points > rbMaxPoints {
		return a.rbStatus(), fmt.Errorf("points 需在 %d~%d", rbMinPoints, rbMaxPoints)
	}
	normalized := rbNormalizeCurrency(currency)
	if raw := strings.ToLower(strings.TrimSpace(currency)); raw != "" &&
		raw != rbCurrencyCera && raw != rbCurrencyTokenCera &&
		raw != "token" && raw != "代币券" {
		return a.rbStatus(), fmt.Errorf("currency 只能是 %s（点券）或 %s（代币券）", rbCurrencyCera, rbCurrencyTokenCera)
	}
	a.psCfgMu.Lock()
	rc := a.cfg.RegisterBonus
	if rc == nil {
		a.psCfgMu.Unlock()
		return a.rbStatus(), fmt.Errorf("注册赠送配置缺失")
	}
	oldEnabled, oldPoints, oldCurrency := rc.Enabled, rc.Points, rc.Currency
	rc.Enabled = enabled
	rc.Points = points
	rc.Currency = normalized
	err := a.cfg.Save()
	if err != nil {
		rc.Enabled, rc.Points, rc.Currency = oldEnabled, oldPoints, oldCurrency
	}
	a.psCfgMu.Unlock()
	if err != nil {
		return a.rbStatus(), fmt.Errorf("配置持久化失败: %w", err)
	}
	state := "关闭"
	if enabled {
		state = "开启"
	}
	a.uiSafe(func() {
		a.logf("注册赠送配置已更新: %s，新注册账号送 %d %s", state, points, rbCurrencyLabel(normalized))
	})
	return a.rbStatus(), nil
}
