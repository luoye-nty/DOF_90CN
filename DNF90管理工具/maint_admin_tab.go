package main

// 服务器选项页：更新维护（维护期间玩家不能登录/进游戏；白名单放行；注册照常）。
// 与 api_client.go 的 MaintenanceStatusGet / MaintenanceApply 一一对应。
//
// 关键行为都在服务端：保存成功后服务端立刻广播给所有登录器长轮询 ——
//   开启维护 → 已打开的登录器按钮变「正在维护」且不可点，正在游戏的玩家被结束游戏进程；
//   取消维护 → 按钮立刻恢复可用。
// 所以管理工具这边只需要"保存 + 回显状态"，不需要额外做任何推送。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// maintMaxAllowUsers 与服务端 manage.maintenanceMaxAllowUsers 保持一致（此处只做前端预校验）。
const maintMaxAllowUsers = 50

// onMaintenanceRefresh 拉取维护配置并回填 UI。
//
// 这里刻意**不占用 opMu**：那是给管理写操作（保存/启停等）用的互斥锁，而这只是一个只读 GET。
// 如果占锁，主窗口打开时的三次静默拉取（注册限制/注册码/维护）会互相挤掉 ——
// 先触发的那次锁住 opMu，后面的 beginOp 失败只能静默跳过，结果维护状态要到用户手点刷新才显示。
func (a *App) onMaintenanceRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.maintBusy {
		return
	}
	a.maintSetBusy(true)
	go func() {
		defer a.sync(func() { a.maintSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.MaintenanceStatusGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询维护配置失败", err)
				}
			}
			return
		}
		a.sync(func() { a.applyMaintenanceToUI(st) })
	}()
}

// onMaintenanceSave 校验输入并应用维护配置。
// 确认框里把"玩家侧会立刻发生什么"讲清楚：这是一个影响全服的开关。
func (a *App) onMaintenanceSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	enabled := a.maintChk != nil && a.maintChk.Checked()
	allowUsers, ok := maintParseAllowUsers(a.maintAllowLE)
	if !ok {
		a.showInfo("参数错误", fmt.Sprintf("白名单账号最多 %d 个，多个用逗号或空格分隔", maintMaxAllowUsers))
		return
	}
	effect := "保存后：所有已打开的登录器立刻恢复可登录，玩家可以进游戏。"
	if enabled {
		effect = "保存后：服务端立刻禁止玩家登录与进游戏；所有已打开的登录器登录按钮变「正在维护」，" +
			"正在游戏中的玩家会被直接结束游戏进程。"
		if len(allowUsers) > 0 {
			effect += fmt.Sprintf("\n白名单（维护期间仍可进）：%s", strings.Join(allowUsers, "、"))
		} else {
			effect += "\n白名单为空：维护期间连管理员也进不去（需要自测请先填白名单或先取消维护）。"
		}
	}
	title := "开启更新维护"
	if !enabled {
		title = "结束更新维护"
	}
	if !a.confirm(title, effect+"\n\n配置会持久化到服务器 config.json，是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.maintSetBusy(true)
	go func() {
		defer a.sync(func() { a.maintSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.MaintenanceApply(ctx, enabled, allowUsers)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存维护配置失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyMaintenanceToUI(st)
			if st != nil && st.Enabled {
				a.showInfo("已开启维护", "维护已开启：玩家无法登录与进游戏，已打开的登录器按钮会立刻变为「正在维护」。")
			} else {
				a.showInfo("维护已结束", "维护已结束：所有已打开的登录器按钮会立刻恢复可用。")
			}
		})
	}()
}

// maintParseAllowUsers 解析白名单输入框：逗号/分号/空格/换行分隔，去空去重（不区分大小写）。
func maintParseAllowUsers(edit *walk.LineEdit) ([]string, bool) {
	if edit == nil {
		return nil, true
	}
	raw := strings.FieldsFunc(edit.Text(), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r' || r == '\n' || r == '，' || r == '；'
	})
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
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
	if len(out) > maintMaxAllowUsers {
		return nil, false
	}
	return out, true
}

// maintSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) maintSetBusy(b bool) {
	a.maintBusy = b
	if a.maintSaveBtn != nil {
		a.maintSaveBtn.SetEnabled(!b)
	}
	if a.maintRefresh != nil {
		a.maintRefresh.SetEnabled(!b)
	}
}

// applyMaintenanceToUI 将远端配置回填控件与状态标签。
func (a *App) applyMaintenanceToUI(st *MaintenanceStatus) {
	if st == nil {
		return
	}
	if a.maintChk != nil {
		a.maintChk.SetChecked(st.Enabled)
	}
	if a.maintAllowLE != nil {
		a.maintAllowLE.SetText(strings.Join(st.AllowUsers, ", "))
	}
	if a.maintStateL == nil {
		return
	}
	if st.Enabled {
		text := "状态：★ 更新维护中 —— 玩家无法登录、无法进入游戏；已打开的登录器登录按钮为「正在维护」"
		if len(st.AllowUsers) > 0 {
			text += "；白名单可进：" + strings.Join(st.AllowUsers, "、")
		} else {
			text += "；白名单为空（管理员也进不去）"
		}
		a.maintStateL.SetText(text)
		a.maintStateL.SetTextColor(walk.RGB(180, 60, 0))
		return
	}
	a.maintStateL.SetText("状态：正常运营中（玩家可正常登录与进游戏）")
	a.maintStateL.SetTextColor(walk.RGB(0, 140, 0))
}
