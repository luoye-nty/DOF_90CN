package main

// 更新维护（维护期间禁止玩家登录与进游戏；注册照常）：
//   - 两项配置在 config.json：maintenance_mode（总开关）与 maintenance_allow_users（白名单账号名）；
//   - 由管理端 /manage/v1/maintenance 修改并持久化；**落盘保存**，运维工具重启后状态保持
//     （避免"重启一次维护就悄悄关了"）；
//   - 认证服务侧见 internal/server/maintenance.go：登录与进游戏验票两处硬拦（白名单放行）；
//   - 配置变更后调用 svc.MaintenanceChanged() 唤醒所有登录器长轮询 —— 这是"取消维护后
//     所有已打开的登录器按钮立刻亮起"的实现方式。

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dnf90ops/internal/server"
	"dnf90ops/internal/store"
)

// maintStat 维护配置快照（与 paid 版 manage.MaintenanceStatus 字段一一对应）。
type maintStat struct {
	Enabled    bool     `json:"enabled"`
	AllowUsers []string `json:"allow_users"`
}

// maintPublicState 返回维护状态实时快照（认证服务注入用；线程安全）。
func (a *App) maintPublicState() server.MaintenanceState {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil {
		return server.MaintenanceState{}
	}
	return server.MaintenanceState{
		Enabled:    a.cfg.MaintenanceMode,
		AllowUsers: append([]string(nil), a.cfg.MaintenanceAllowUsers...),
	}
}

// maintStatus 返回归一化后的维护配置快照（管理端展示用）。
func (a *App) maintStatus() maintStat {
	state := a.maintPublicState()
	return maintStat{Enabled: state.Enabled, AllowUsers: maintAllowUsersText(state.AllowUsers)}
}

// maintApply 应用并持久化维护配置（两项一起写，失败整体回滚）。
// 成功后唤醒所有登录器长轮询：开始维护 → 按钮熄灭 + 结束游戏进程；取消维护 → 按钮亮起。
func (a *App) maintApply(_ context.Context, enabled bool, allowUsers []string) (maintStat, error) {
	if a == nil || a.cfg == nil {
		return maintStat{}, fmt.Errorf("配置尚未加载，无法应用更新维护")
	}
	cleaned := maintNormalizeAllowUsers(allowUsers)

	a.psCfgMu.Lock()
	oldEnabled := a.cfg.MaintenanceMode
	oldAllow := a.cfg.MaintenanceAllowUsers
	a.cfg.MaintenanceMode = enabled
	a.cfg.MaintenanceAllowUsers = cleaned
	err := a.cfg.Save()
	if err != nil {
		a.cfg.MaintenanceMode = oldEnabled
		a.cfg.MaintenanceAllowUsers = oldAllow
	}
	a.psCfgMu.Unlock()
	if err != nil {
		return a.maintStatus(), fmt.Errorf("配置持久化失败: %w", err)
	}

	// 广播给所有等待中的登录器（长轮询 /launcher/v1/maintenance/wait）。
	waiting := 0
	if a.svc != nil {
		a.svc.MaintenanceChanged()
		waiting = a.svc.MaintenanceWaiting()
	}
	state := "已结束维护"
	if enabled {
		state = "已开始维护"
	}
	a.uiSafe(func() {
		a.logf("更新维护%s：玩家%s，白名单 %d 个账号（已唤醒等待中的登录器 %d 个）",
			state, maintPlayerStateText(enabled), len(cleaned), waiting)
	})
	return a.maintStatus(), nil
}

// maintPlayerStateText 维护开关对应的玩家侧状态文案。
func maintPlayerStateText(enabled bool) string {
	if enabled {
		return "登录与进游戏一律拦截"
	}
	return "可正常登录与进游戏"
}

// maintNormalizeAllowUsers 归一化白名单：去空白、去掉重复、丢弃空项，排序保证顺序稳定。
// 账号名按 store.NormalizeUsername 归一（与注册/登录写入一致），因此这里统一小写。
func maintNormalizeAllowUsers(users []string) []string {
	seen := make(map[string]struct{}, len(users))
	out := make([]string, 0, len(users))
	for _, raw := range users {
		name := store.NormalizeUsername(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// maintAllowUsersText 把白名单拼成给管理工具显示的文本（空 = 无白名单）。
func maintAllowUsersText(users []string) []string {
	if len(users) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
