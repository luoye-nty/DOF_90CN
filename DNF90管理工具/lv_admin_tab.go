package main

// 服务器选项页：登录器版本校验（登录器自检/点登录时上报版本号，与服务端内置版本号
// 完全一致才放行；不一致则强制更新，更新完成前禁止进入登录界面）。
// 与 api_client.go 的 LauncherVersionGet / LauncherVersionApply 一一对应。
//
// ── 三端分工（改动时务必一起看）────────────────────────────────────────────
//   · 运维工具：版本号的**唯一事实源**（launcher_version.go 的 builtinLauncherVersion），
//     以及对登录器的公开端点 /launcher/v1/version-check、对本工具的管理端点
//     /manage/v1/launcher-version。
//   · 管理工具（本文件）：只负责"勾开关 + 填直链"，并展示服务端回读的版本号供核对。
//     **本工具没有版本号的本地副本**，所以不需要跟着发布改。
//   · 登录器：上报自己内置的版本号（LauncherVersion.cs），不一致就走强制更新。
//
// ── 为什么不需要像"更新维护"那样做广播 ────────────────────────────────────
// 维护要实时推给已打开的登录器（按钮熄灭）；版本校验不需要 —— 登录器在
// **自检阶段**和**点登录时**各主动查一次，服务端只负责如实回答。
//
// ── 危险配置要标红 ───────────────────────────────────────────────────────
// "勾选校验但没填直链"是一个**合法但危险**的状态：服务端会照常开启校验，
// 于是版本不符的玩家只会看到一句「请联系管理员」，谁也进不去。
// 服务端允许保存（这是需求确认过的分支），但本页必须把它标红说清楚。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// lvURLMaxLen 与服务器端 manage.launcherVersionMaxURLLen 同口径（这里只做前端预校验）。
const lvURLMaxLen = 2048

// onLauncherVersionRefresh 拉取登录器版本校验配置并回填 UI。
//
// 与 onMaintenanceRefresh 一样刻意**不占用 opMu**：这是只读 GET，
// 占锁会让主窗口打开时的多次静默拉取互相挤掉。
func (a *App) onLauncherVersionRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.lvBusy {
		return
	}
	a.lvSetBusy(true)
	go func() {
		defer a.sync(func() { a.lvSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.LauncherVersionGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询登录器版本校验配置失败", err)
				}
			}
			return
		}
		a.sync(func() { a.applyLauncherVersionToUI(st) })
	}()
}

// onLauncherVersionSave 校验输入并应用登录器版本校验配置。
//
// 确认框里把"玩家侧会立刻发生什么"讲清楚：这是一个影响全服的开关。
func (a *App) onLauncherVersionSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	enabled := a.lvChk != nil && a.lvChk.Checked()
	downloadURL := ""
	if a.lvUrlLE != nil {
		downloadURL = strings.TrimSpace(a.lvUrlLE.Text())
	}
	if msg, ok := lvValidateURL(downloadURL); !ok {
		a.showInfo("参数错误", msg)
		return
	}

	ver := a.lvCurrentVersionText()
	title := "开启登录器版本校验"
	var effect string
	switch {
	case !enabled:
		title = "关闭登录器版本校验"
		effect = "保存后：登录器不再校验版本，任何版本的登录器都能正常登录与进游戏。"
		if downloadURL != "" {
			effect += "\n（下载直链会保留在服务器上，下次再开启时不用重填。）"
		}
	case downloadURL == "":
		// 合法但需要说清后果的配置：新登录器没法自动更新，只能来找你。
		effect = "⚠ 你勾选了「登录器版本校验」但没有填「登录器下载直链」：\n\n" +
			"保存后，凡是不是 " + ver + " 的新登录器都会弹窗提示「请联系管理员」并**禁止进入登录界面**，" +
			"玩家自己无法更新，只能来找你。\n\n" +
			"旧版登录器（不认识版本校验的老包）同样会被服务端直接拒绝，拿不到任何接口。\n\n" +
			"如果你并不想全服停摆，请先取消勾选，或先把直链填上。"
	default:
		effect = "保存后，登录器在**启动自检阶段**与**点登录时**各上报一次自己的版本号：\n\n" +
			"· 版本 = " + ver + " → 正常进入登录界面；\n" +
			"· 版本 ≠ " + ver + " → 立刻停在更新窗口，从下面的直链下载新登录器，" +
			"删掉原登录器并自动打开新版本，**更新完成前无法进入登录界面**。\n\n" +
			"⚠ 另外还有一道服务端硬拦：**旧版登录器**（没有版本校验代码的老包）服务端认不出来，" +
			"它们拿不到任何接口 —— 不能注册、不能登录、探测不到在线、拉不到公告与泡点，" +
			"只会看到固定的一句「登录器版本过旧，请联系管理员下载新版登录器」。\n" +
			"也就是说这个开关一开就是硬切换 —— 还没换新登录器的玩家立刻什么都做不了。\n" +
			"建议先把新包发出去、通知到位，等大部分玩家换完再开。\n\n" +
			"下载直链（只给新登录器自动更新用）：" + downloadURL
	}
	if !a.confirm(title, effect+"\n\n配置会持久化到服务器 config.json，是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.lvSetBusy(true)
	go func() {
		defer a.sync(func() { a.lvSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.LauncherVersionApply(ctx, enabled, downloadURL)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存登录器版本校验配置失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyLauncherVersionToUI(st)
			switch {
			case st != nil && st.Enabled && st.DownloadURL == "":
				a.showInfo("已保存（注意）",
					"登录器版本校验已开启，但服务器上没有下载直链。\n\n"+
						"版本不符的登录器会提示「请联系管理员」并禁止进入登录界面 —— 请尽快把直链填上。")
			case st != nil && st.Enabled:
				a.showInfo("已开启版本校验",
					fmt.Sprintf("已开启：登录器版本必须为 %s。\n\n"+
						"版本不符的登录器会在自检阶段自动下载更新，更新完成前无法进入登录界面。",
						lvVersionText(st.RequiredVersion)))
			default:
				a.showInfo("已关闭版本校验", "登录器不再校验版本，任何版本都能正常登录。")
			}
		})
	}()
}

// lvValidateURL 前端预校验下载直链；返回 (错误说明, 是否通过)。
//
// 口径与服务器端完全一致（能在这里拦下就不要等服务器 400）：
// 空串合法；非空必须是 http/https、有主机名、只含 ASCII 可见字符、不超长。
func lvValidateURL(raw string) (string, bool) {
	url := strings.TrimSpace(raw)
	if url == "" {
		return "", true
	}
	if len(url) > lvURLMaxLen {
		return fmt.Sprintf("下载直链过长：最多 %d 个字符", lvURLMaxLen), false
	}
	for _, r := range url {
		if r <= 0x20 || r >= 0x7F {
			return "下载直链只能包含英文/数字/符号，不能有中文、空格或换行（多半是复制粘贴带进来的）", false
		}
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "下载直链必须以 http:// 或 https:// 开头", false
	}
	host := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if idx := strings.IndexAny(host, "/?#"); idx >= 0 {
		host = host[:idx]
	}
	if strings.TrimSpace(host) == "" {
		return "下载直链缺少主机名（例如 https://example.com/launcher.zip）", false
	}
	return "", true
}

// lvSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) lvSetBusy(b bool) {
	a.lvBusy = b
	if a.lvSaveBtn != nil {
		a.lvSaveBtn.SetEnabled(!b)
	}
	if a.lvRefresh != nil {
		a.lvRefresh.SetEnabled(!b)
	}
}

// applyLauncherVersionToUI 将远端配置回填控件与状态标签。
func (a *App) applyLauncherVersionToUI(st *LauncherVersionStatus) {
	if st == nil {
		return
	}
	if a.lvChk != nil {
		a.lvChk.SetChecked(st.Enabled)
	}
	if a.lvUrlLE != nil {
		a.lvUrlLE.SetText(st.DownloadURL)
	}
	// 缓存服务端回读的版本号：保存确认框要用真实版本号说话，不能只说"内置版本号"。
	a.lvVersionCache = strings.TrimSpace(st.RequiredVersion)
	if a.lvStateL == nil {
		return
	}

	ver := lvVersionText(st.RequiredVersion)
	switch {
	case !st.Enabled:
		a.lvStateL.SetText(fmt.Sprintf(
			"状态：未开启版本校验（登录器版本不作要求；服务器内置的当前登录器版本号为 %s）", ver))
		a.lvStateL.SetTextColor(walk.RGB(0, 140, 0))
	case st.DownloadURL == "":
		// 危险状态：校验开着但没有直链 → 版本不符的玩家只会看到「请联系管理员」。
		a.lvStateL.SetText(fmt.Sprintf(
			"状态：⚠ 已开启，但**未填下载直链** —— 凡不是 %s 的登录器都会弹「请联系管理员」并禁止进入登录界面，玩家无法自行更新！",
			ver))
		a.lvStateL.SetTextColor(walk.RGB(190, 0, 0))
	default:
		a.lvStateL.SetText(fmt.Sprintf(
			"状态：已开启 —— 要求登录器版本 %s；版本不符的登录器会在自检阶段自动下载更新（更新完成前无法进入登录界面）",
			ver))
		a.lvStateL.SetTextColor(walk.RGB(0, 140, 0))
	}
}

// lvVersionText 版本号显示文案：空值给出可读提示而不是空白。
func lvVersionText(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "（未配置）"
	}
	return "V" + v
}

// lvCurrentVersionText 保存确认框里用的"当前版本号"文案。
//
// 优先用最近一次刷新缓存到的真实版本号（applyLauncherVersionToUI 写进去的）；
// 还没成功刷新过时退回一句说明 —— 版本号来自服务端编译期常量，
// 界面拿不到也不该让保存失败（服务端自己会用常量兜底）。
func (a *App) lvCurrentVersionText() string {
	if v := strings.TrimSpace(a.lvVersionCache); v != "" {
		return "V" + v
	}
	return "服务器内置的登录器版本号"
}
