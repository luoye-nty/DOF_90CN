package main

// 服务器选项页：双开（多开）开关 —— 控制登录器「双开」勾选框能不能用。
// 与 api_client.go 的 MultiOpenGet / MultiOpenApply 一一对应。
//
// ── 三端分工（改动时务必一起看）────────────────────────────────────────────
//   · 运维工具：配置字段是 `*bool`（config.MultiOpenEnabled），
//     对登录器的公开端点 /launcher/v1/multi-open、对本工具的管理端点 /manage/v1/multi-open。
//   · 管理工具（本文件）：只负责"勾/不勾 + 保存"，并如实展示服务端回读的生效值。
//   · 登录器：自检阶段读一次，把「双开」勾选框与「副账号登录」按钮置灰，并在
//     按钮回调里再拦一道（LoginWindow.xaml.cs 的 ApplyMultiOpenGate / 四道闸门）。
//
// ── ★ 三态：只有明确收到 enabled:false 才关闭 ★ ────────────────────────────
// 服务端的响应可能是**三种**：
//   {enabled:true}  → 运营明确允许双开；
//   {enabled:false} → 运营明确关闭 —— 只有这一种情况登录器才关闸；
//   {}（不带 enabled）→ 后台**从未设置过**（老 config.json）→ 登录器保持"允许双开"，
//                       也就是改造前的现有行为（升级运维工具不影响玩家）。
// 所以本页的状态行会把第三种如实写成「尚未设置过」，而不是含糊地说"已开启"。
//
// ── 关闭不会踢人（这是最容易被误解的一点）─────────────────────────────────
// 关双开只是"以后不许再开"：**已经在运行的副账号不会被踢下线**。
// 与「更新维护」故意不同 —— 维护会 GameProcessManager.KillAll() 立刻结束游戏进程。
// 所以确认框里必须把这一点写清楚，否则运营会以为关了就能把双开的人赶下线。
//
// ── 为什么不需要像"更新维护"那样做广播 ────────────────────────────────────
// 维护要实时推给已打开的登录器（按钮熄灭 + 结束游戏进程）；双开开关不需要 ——
// 登录器在**自检阶段**主动查一次即可，而且关掉也不应该去动已经在跑的游戏进程。

import (
	"context"
	"time"

	"github.com/lxn/walk"
)

// onMultiOpenRefresh 拉取双开开关并回填 UI。
//
// 与 onLauncherVersionRefresh / onMaintenanceRefresh 一样刻意**不占用 opMu**：
// 这是只读 GET，占锁会让主窗口打开时的多次静默拉取互相挤掉。
func (a *App) onMultiOpenRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.moBusy {
		return
	}
	a.moSetBusy(true)
	go func() {
		defer a.sync(func() { a.moSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.MultiOpenGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询双开开关失败", err)
				}
			}
			return
		}
		a.sync(func() { a.applyMultiOpenToUI(st) })
	}()
}

// onMultiOpenSave 应用双开开关（先弹确认，把"玩家侧会立刻发生什么"讲清楚）。
func (a *App) onMultiOpenSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	enabled := a.moChk != nil && a.moChk.Checked()

	title := "允许玩家双开"
	var effect string
	if enabled {
		effect = "保存后：玩家登录器上的「双开」勾选框可用，点「副账号登录」可以再拉一个游戏窗口。\n\n" +
			"这是改造前的默认行为，只是把开关明确记到服务器配置里（重启运维工具后保持）。"
	} else {
		title = "关闭双开"
		effect = "保存后：玩家下次打开登录器时，「双开」勾选框与「副账号登录」按钮变灰不可点，" +
			"按钮文案变成「双开已关闭」；玩家点下去会被拦下并提示「服务器已关闭双开，如需开启请联系管理员」。\n\n" +
			"⚠ 已经在跑的副账号**不会被踢下线** —— 关双开只是「以后不许再开」，不碰任何游戏进程。" +
			"要马上把双开的人赶下线，请用「更新维护」或在「在线列表」里踢人。\n\n" +
			"⚠ 只对**认识这个开关的新登录器**生效：老登录器照旧能双开（服务端不参与双开许可判定）。"
	}
	if !a.confirm(title, effect+"\n\n配置会持久化到服务器 config.json，是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.moSetBusy(true)
	go func() {
		defer a.sync(func() { a.moSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.MultiOpenApply(ctx, enabled)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存双开开关失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyMultiOpenToUI(st)
			if enabled {
				a.showInfo("已允许双开", "登录器「双开」勾选框已恢复可用（玩家下次打开登录器生效）。")
				return
			}
			a.showInfo("已关闭双开",
				"新登录器的「双开」勾选框与「副账号登录」按钮已置灰。\n\n"+
					"注意：**已经在运行的副账号不会被踢下线** —— 这次改动只影响之后还能不能再开。")
		})
	}()
}

// moSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) moSetBusy(b bool) {
	a.moBusy = b
	if a.moSaveBtn != nil {
		a.moSaveBtn.SetEnabled(!b)
	}
	if a.moRefresh != nil {
		a.moRefresh.SetEnabled(!b)
	}
}

// applyMultiOpenToUI 将远端配置回填控件与状态标签。
func (a *App) applyMultiOpenToUI(st *MultiOpenStatus) {
	if st == nil {
		return
	}
	// Configured=false 表示服务器上**从未设置过**：这时 Enabled 报的是玩家侧生效值
	// （= 允许双开），界面上也照实勾上 —— 勾选框必须与玩家实际能不能双开一致。
	if a.moChk != nil {
		a.moChk.SetChecked(st.Enabled)
	}
	a.moLoaded = true
	if a.moStateL == nil {
		return
	}

	switch {
	case !st.Configured && st.Enabled:
		a.moStateL.SetText("状态：尚未在后台设置过 —— 当前按默认「允许双开」执行（升级运维工具不会影响玩家，保存一次后本条会变成明确设置）")
		a.moStateL.SetTextColor(walk.RGB(120, 120, 120))
	case st.Enabled:
		a.moStateL.SetText("状态：已允许双开 —— 登录器「双开」勾选框与「副账号登录」按钮可用")
		a.moStateL.SetTextColor(walk.RGB(0, 140, 0))
	default:
		a.moStateL.SetText("状态：已关闭双开 —— 新登录器的「双开」勾选框与「副账号登录」按钮置灰（**已在运行的副账号不会被踢下线**；老登录器不受影响）")
		a.moStateL.SetTextColor(walk.RGB(190, 0, 0))
	}
}
