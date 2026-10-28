// 注册限制（关闭玩家自助注册 + 每个 IP 的注册数量上限）：
//   - 两项配置存在 config.json：registration_closed（关总闸）与 registration_max_per_ip（按 IP 计数）；
//     由管理端 /manage/v1/registration-limit 修改并持久化；
//   - 认证服务 handleRegister 每次收到自助注册请求都会查询这两项：关闭时直接拦截，
//     未关闭但该来源 IP 已注册数达到上限时按 IP 拦截，两者都以中文 message 返回给登录器弹窗；
//   - 管理员建号不经过注册入口，且 register_ip 记空，因此既不受开关影响、也不占用 IP 额度。
//
// registration_max_per_ip 的取值语义（对外统一用 -1 表示"不限制"）：
//
//	-1        不限制（配置里没有这个字段时也是这个含义）
//	 0        不允许注册（每个来源 IP 最多 0 个 → 全拒）
//	 1~1000   每个来源 IP 最多这么多个
//
// 注意 0 曾经表示"不限制"，2026-09-14 按用户要求改成"不允许注册"；
// 为了不让老配置（没有该字段 → 解出 0）在升级后把注册全关死，配置层用 *int 区分"没配过"。
package main

import (
	"context"
	"fmt"

	"dnf90ops/internal/server"
)

// regMaxPerIPLimit 每 IP 注册上限的允许区间上限；0 表示"不允许注册"，-1 表示"不限制"。
const regMaxPerIPLimit = 1000

// regUnlimitedPerIP 对外表示"不按 IP 限制"的取值（配置里对应"没有这个字段"）。
const regUnlimitedPerIP = -1

// regStat 注册限制配置快照（与 paid 版 manage.RegistrationClosedStatus 字段一一对应）。
type regStat struct {
	Closed bool `json:"closed"`
	// MaxPerIP 每个来源 IP 允许注册的账号数上限：-1=不限制，0=不允许注册，1~1000=上限。
	MaxPerIP int `json:"max_per_ip"`
	// CodeRequired 注册必须填注册码（邀请码）；独立于 registration_mode，
	// 注册开放时也能要求"必须有码才能注册"。
	CodeRequired bool `json:"code_required"`
}

// regPublicInfo 返回是否关闭玩家自助注册（认证服务启动注入用；线程安全）。
func (a *App) regPublicInfo() bool {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	return a.cfg != nil && a.cfg.RegistrationClosed
}

// regPublicLimit 返回注册闸门快照（closed, maxPerIP, codeRequired），供认证服务注册入口一次性取用。
// 三个值必须在同一把锁下读出，否则可能出现"读到旧 closed + 新 maxPerIP"的混合快照。
func (a *App) regPublicLimit() server.RegistrationPolicy {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil {
		return server.RegistrationPolicy{MaxPerIP: regUnlimitedPerIP}
	}
	return server.RegistrationPolicy{
		Closed:       a.cfg.RegistrationClosed,
		MaxPerIP:     normalizeRegMaxPerIP(a.cfg.RegistrationMaxPerIP),
		CodeRequired: a.cfg.RegistrationCodeRequired,
	}
}

// regStatus 返回归一化后的注册限制配置快照。
func (a *App) regStatus() regStat {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil {
		return regStat{MaxPerIP: regUnlimitedPerIP}
	}
	return regStat{
		Closed:       a.cfg.RegistrationClosed,
		MaxPerIP:     normalizeRegMaxPerIP(a.cfg.RegistrationMaxPerIP),
		CodeRequired: a.cfg.RegistrationCodeRequired,
	}
}

// regApply 应用并持久化注册限制配置（三项一起写，失败整体回滚）。
// maxPerIP：-1=不限制，0=不允许注册，1~1000=每个 IP 上限；codeRequired=注册必须填注册码。
func (a *App) regApply(_ context.Context, closed bool, maxPerIP int, codeRequired bool) (regStat, error) {
	if maxPerIP < regUnlimitedPerIP || maxPerIP > regMaxPerIPLimit {
		return a.regStatus(), fmt.Errorf("每 IP 注册上限必须在 %d~%d 之间（%d=不限制，0=不允许注册）",
			regUnlimitedPerIP, regMaxPerIPLimit, regUnlimitedPerIP)
	}
	if a == nil || a.cfg == nil {
		return regStat{}, fmt.Errorf("配置尚未加载，无法应用注册限制")
	}
	a.psCfgMu.Lock()
	oldClosed := a.cfg.RegistrationClosed
	oldMax := a.cfg.RegistrationMaxPerIP
	oldCode := a.cfg.RegistrationCodeRequired
	a.cfg.RegistrationClosed = closed
	a.cfg.RegistrationCodeRequired = codeRequired
	if maxPerIP == regUnlimitedPerIP {
		// 不限制 = 配置里不放这个字段（下次升级也不会被误读成 0=全拒）。
		a.cfg.RegistrationMaxPerIP = nil
	} else {
		value := maxPerIP
		a.cfg.RegistrationMaxPerIP = &value
	}
	err := a.cfg.Save()
	if err != nil {
		a.cfg.RegistrationClosed = oldClosed
		a.cfg.RegistrationMaxPerIP = oldMax
		a.cfg.RegistrationCodeRequired = oldCode
	}
	a.psCfgMu.Unlock()
	if err != nil {
		return a.regStatus(), fmt.Errorf("配置持久化失败: %w", err)
	}
	state := "开放"
	if closed {
		state = "关闭"
	}
	codeText := "无需注册码"
	if codeRequired {
		codeText = "注册必须填注册码"
	}
	a.uiSafe(func() {
		a.logf("注册限制已更新：自助注册%s，%s，%s；管理员建号不受影响", state, regPerIPText(maxPerIP), codeText)
	})
	return a.regStatus(), nil
}

// regPerIPText 把每 IP 上限翻译成人话（日志与界面共用一套说法）。
func regPerIPText(maxPerIP int) string {
	switch {
	case maxPerIP < 0:
		return "不限每 IP 数量"
	case maxPerIP == 0:
		return "不允许注册（每个 IP 上限 0）"
	default:
		return fmt.Sprintf("每个 IP 最多 %d 个", maxPerIP)
	}
}

// normalizeRegMaxPerIP 把配置里的值收敛成对外语义：nil（没配过）= 不限制。
// 越界值按上限收敛（而不是归零）：归零在新语义下等于"全拒"，手工改坏一个数字就把注册全关死太危险。
func normalizeRegMaxPerIP(value *int) int {
	if value == nil {
		return regUnlimitedPerIP
	}
	if *value < 0 {
		return regUnlimitedPerIP
	}
	if *value > regMaxPerIPLimit {
		return regMaxPerIPLimit
	}
	return *value
}
