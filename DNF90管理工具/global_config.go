package main

// 「已连接」旁边的全局配置按钮：一键保存 / 一键刷新。
//
// 为什么需要：服务器选项页现在有 8 组配置（泡点 / 注册赠送 / 注册限制 / 更新维护 /
// 登录器版本校验 / 定时公告 / 服务器公告栏 / 可加载 DLL），加上在线福利页的三档期，挨个点保存很费事，
// 而且最容易出的事故就是"界面上改了、忘了点保存"。这两个按钮把它们串起来。
//
// 口径（已与需求确认）：
//   - 保存 = 依次把各组**当前界面上的值**提交给服务器，最后汇总每一组的结果；
//   - 刷新 = 依次触发各组自己的静默刷新（结果各自回填到对应分组）；
//   - 各组独立生效：某一组失败**不影响**其它组（配置本来就是互相独立的），
//     失败的会把服务端返回的原因原样列在汇总里，方便直接改。
//   - 一律先问一次确认，避免误点把一堆没填完的配置推上去。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	decl "github.com/lxn/walk/declarative"
)

// globalSaveOutcome 一组配置的保存结果。
type globalSaveOutcome struct {
	name string
	err  error
}

// onGlobalSave 一键保存全局配置（服务器选项页各组 + 在线福利三档期）。
func (a *App) onGlobalSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	const tip = "将把服务器选项页各组**当前界面上显示的值**依次提交给服务器：\n\n" +
		"· 泡点规则\n· 注册赠送点券\n· 注册限制（含「注册需要注册码」）\n· 更新维护（含白名单）\n" +
		"· 登录器版本校验（含下载直链）\n" +
		"· 定时公告列表\n· 服务器公告栏\n· 可加载 DLL 清单\n· 在线福利三档期\n\n" +
		"⚠ 定时公告 / 公告栏 / DLL 清单 / 在线福利是**整表替换**：\n" +
		"   提交的就是界面上这张表（表格为空 = 清空服务器上的那张表）。\n" +
		"   还没成功加载过（没点过刷新、或刷新失败）的组会被自动跳过并如实报错。\n\n" +
		"各组独立生效：某一组失败不影响其它组（失败原因会列在结果里）。\n是否继续？"
	if !a.confirm("保存全局配置", tip) {
		a.endOp()
		return
	}

	// 整表替换组的"空表闸门"（必须在 UI 线程算好，goroutine 里不再读界面状态）。
	//
	// 定时公告 / 公告栏 / DLL 清单 / 在线福利在服务端都是**整表替换**语义：提交一份空表
	// 就是把服务器上那张表删光（DLL 清单清空后登录器的校验与注入会全部失效）。
	// 而本页四组的"内存表"要等启动时的静默刷新回来才有内容——刷新没回来或刷新失败时
	// 它就是空的，这时点「保存全局配置」会把服务器配置清空，界面上却看不出异常。
	// 所以：没成功加载过的组，本次直接跳过并如实报错，让运营先点「刷新」。
	skips := map[string]string{}
	if !a.asLoaded {
		skips["定时公告"] = "尚未成功加载过列表（先点该组的『刷新』），本次跳过以免把服务器上的公告清空"
	}
	if !a.ntLoaded {
		skips["服务器公告栏"] = "尚未成功加载过列表（先点该组的『刷新』），本次跳过以免把服务器上的公告栏清空"
	}
	if !a.dlLoaded {
		skips["可加载 DLL"] = "尚未成功加载过清单（先点该组的『刷新』），本次跳过以免把服务器上的 DLL 清单清空"
	}
	if !a.wfLoaded {
		skips["在线福利"] = "尚未成功加载过配置（先点『刷新』），本次跳过以免把服务器上已配好的三档福利改回全停用"
	}

	go func() {
		defer a.sync(func() { a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		outcomes := make([]globalSaveOutcome, 0, 8)
		record := func(name string, err error) {
			outcomes = append(outcomes, globalSaveOutcome{name: name, err: err})
		}
		// recordSkipped：跳过也要如实记成"失败"，否则汇总会报"N 组全部保存成功"，
		// 运营以为福利/公告已经按界面存进服务器了。
		recordSkipped := func(name string) bool {
			if reason, skip := skips[name]; skip {
				record(name, fmt.Errorf("已跳过：%s", reason))
				return true
			}
			return false
		}

		if !recordSkipped("定时公告") {
			record("定时公告", a.globalSaveAnnounceSchedule(ctx, c))
		}
		if !recordSkipped("服务器公告栏") {
			record("服务器公告栏", a.globalSaveNotices(ctx, c))
		}
		if !recordSkipped("可加载 DLL") {
			record("可加载 DLL", a.globalSaveDLLs(ctx, c))
		}
		if !recordSkipped("在线福利") {
			record("在线福利", a.globalSaveWelfare(ctx, c))
		}
		record("泡点规则", a.globalSavePointsink(ctx, c))
		record("注册赠送点券", a.globalSaveRegisterBonus(ctx, c))
		record("注册限制", a.globalSaveRegistrationLimit(ctx, c))
		record("更新维护", a.globalSaveMaintenance(ctx, c))
		record("登录器版本校验", a.globalSaveLauncherVersion(ctx, c))
		record("双开开关", a.globalSaveMultiOpen(ctx, c))

		a.sync(func() { a.showGlobalSaveResult(outcomes) })
	}()
}

// onGlobalRefresh 一键刷新全局配置。
//
// 这里直接触发各组自己的静默刷新（内部各自更新 UI + 各自的忙碌标记），
// 而不是重复实现一遍读取逻辑：刷新是只读操作，快慢不一致也不影响正确性。
func (a *App) onGlobalRefresh() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	a.onPointsinkRefresh(true)
	a.onRegisterBonusRefresh(true)
	a.onRegistrationLimitRefresh(true)
	a.onRegCodeRefresh(true)
	a.onMaintenanceRefresh(true)
	a.onLauncherVersionRefresh(true)
	a.onMultiOpenRefresh(true)
	a.onAnnounceScheduleRefresh(true)
	a.onNoticesRefresh(true)
	a.onDllRefresh(true)
	a.onWelfareRefresh(true)
	a.connInfoL.SetText(fmt.Sprintf("已连接 %s:%d（已请求刷新全部配置）", a.cfg.ServerIP, a.cfg.Port))
}

// showGlobalSaveResult 汇总弹窗：成功几组、失败几组 + 每组的失败原因。
func (a *App) showGlobalSaveResult(outcomes []globalSaveOutcome) {
	okCount := 0
	var lines []string
	for _, item := range outcomes {
		if item.err == nil {
			okCount++
			lines = append(lines, "· "+item.name+"：已保存")
			continue
		}
		lines = append(lines, "· "+item.name+"：失败 —— "+item.err.Error())
	}
	title := "保存全局配置完成"
	if okCount == len(outcomes) {
		a.showInfo(title, fmt.Sprintf("%d 组配置全部保存成功：\n\n%s", okCount, strings.Join(lines, "\n")))
		return
	}
	a.showError(title, fmt.Errorf("成功 %d 组，失败 %d 组（各组独立生效，失败的那组改完可以单独再存）：\n\n%s",
		okCount, len(outcomes)-okCount, strings.Join(lines, "\n")))
}

// ---- 每一组的"静默保存"实现：不弹确认、不弹单个结果，只返回错误 ----

func (a *App) globalSavePointsink(ctx context.Context, c *APIClient) error {
	if a.psIntLE == nil || a.psPtsLE == nil || a.psChk == nil {
		return nil // 控件未就绪（页面没建起来）→ 跳过，不算失败
	}
	interval, err := strconv.Atoi(strings.TrimSpace(a.psIntLE.Text()))
	if err != nil || interval < 10 || interval > 86400 {
		return fmt.Errorf("发放间隔需为 10~86400 的整数秒（当前 %q）", strings.TrimSpace(a.psIntLE.Text()))
	}
	points, err := strconv.Atoi(strings.TrimSpace(a.psPtsLE.Text()))
	if err != nil || points < 1 || points > 1000000 {
		return fmt.Errorf("每轮点券需为 1~1000000 的整数（当前 %q）", strings.TrimSpace(a.psPtsLE.Text()))
	}
	// 每日限额必须由这里一并带上：服务器端是"以本次入参为准"的整组写入，
	// 少传就等于把限额清成 0（不限），运营配好的限额会被全局保存悄悄抹掉。
	limit := 0
	if a.psLimitLE != nil {
		text := strings.TrimSpace(a.psLimitLE.Text())
		if text != "" {
			limit, err = strconv.Atoi(text)
			if err != nil || limit < 0 || limit > psMaxDailyLimit {
				return fmt.Errorf("账号每日限额需为 0（不限）~%d 的整数（当前 %q）", psMaxDailyLimit, text)
			}
		}
	}
	// 发放币种同理，**必须一并带上**：服务器端整组写入，少传就等于把币种重置成点券
	// （和上面 daily_limit 是同一个坑）。控件未就绪时传空串，服务端会按点券归一化。
	currency := ""
	if a.psCurCb != nil {
		currency = psCurrencyValue(a.psCurCb.CurrentIndex())
	}
	st, err := c.PointsinkApply(ctx, a.psChk.Checked(), interval, points, limit, currency)
	if err != nil {
		return err
	}
	a.sync(func() {
		a.applyPointsinkInputs(st)
		a.applyPointsinkStatusText(st)
	})
	return nil
}

func (a *App) globalSaveRegisterBonus(ctx context.Context, c *APIClient) error {
	if a.rbChk == nil || a.rbPtsLE == nil {
		return nil
	}
	points, err := strconv.Atoi(strings.TrimSpace(a.rbPtsLE.Text()))
	if err != nil || points < 1 || points > 1000000 {
		return fmt.Errorf("注册赠送数量需为 1~1000000 的整数（当前 %q）", strings.TrimSpace(a.rbPtsLE.Text()))
	}
	currencyIdx := 0
	if a.rbCurCb != nil {
		currencyIdx = a.rbCurCb.CurrentIndex()
	}
	st, err := c.RegisterBonusApply(ctx, a.rbChk.Checked(), points, rbCurrencyValue(currencyIdx))
	if err != nil {
		return err
	}
	a.sync(func() { a.applyRegisterBonusToUI(st) })
	return nil
}

func (a *App) globalSaveRegistrationLimit(ctx context.Context, c *APIClient) error {
	if a.regChk == nil || a.regMaxIPLE == nil {
		return nil
	}
	maxPerIP, ok := regParseMaxPerIP(a.regMaxIPLE)
	if !ok {
		return fmt.Errorf("每 IP 最多注册数不合法（应为 %d~%d）", regUnlimitedPerIP, regMaxPerIPLimit)
	}
	codeRequired := a.regCodeChk != nil && a.regCodeChk.Checked()
	st, err := c.RegistrationLimitApply(ctx, a.regChk.Checked(), maxPerIP, codeRequired)
	if err != nil {
		return err
	}
	a.sync(func() { a.applyRegistrationLimitToUI(st) })
	return nil
}

func (a *App) globalSaveMaintenance(ctx context.Context, c *APIClient) error {
	if a.maintChk == nil {
		return nil
	}
	users, ok := maintParseAllowUsers(a.maintAllowLE)
	if !ok {
		return fmt.Errorf("白名单账号过多（最多 %d 个）", maintMaxAllowUsers)
	}
	st, err := c.MaintenanceApply(ctx, a.maintChk.Checked(), users)
	if err != nil {
		return err
	}
	a.sync(func() { a.applyMaintenanceToUI(st) })
	return nil
}

// globalSaveLauncherVersion 登录器版本校验组的一键保存。
//
// 注意这里**不**复用 onLauncherVersionSave：一键保存是"不弹单个确认"的批量路径，
// 由最外层那一句确认统一负责。但危险组合（开着校验却没直链）仍然要拦下来报错 ——
// 一键保存最容易出的事故就是"忘了填直链还顺手点了保存"，那会让全服玩家全部卡在
// 「请联系管理员」。
func (a *App) globalSaveLauncherVersion(ctx context.Context, c *APIClient) error {
	if a.lvChk == nil {
		return nil // 控件未就绪（页面没建起来）→ 跳过，不算失败
	}
	enabled := a.lvChk.Checked()
	downloadURL := ""
	if a.lvUrlLE != nil {
		downloadURL = strings.TrimSpace(a.lvUrlLE.Text())
	}
	if msg, ok := lvValidateURL(downloadURL); !ok {
		return fmt.Errorf("%s", msg)
	}
	if enabled && downloadURL == "" {
		return fmt.Errorf("「登录器版本校验」已勾选但下载直链为空：这样保存后全服版本不符的登录器" +
			"都会弹「请联系管理员」并禁止进入，玩家无法自行更新。请填上直链，或取消勾选")
	}
	st, err := c.LauncherVersionApply(ctx, enabled, downloadURL)
	if err != nil {
		return err
	}
	a.sync(func() { a.applyLauncherVersionToUI(st) })
	return nil
}

// globalSaveMultiOpen 双开开关组的一键保存。
//
// 服务器端是"以本次入参为准"的整组写入，但本组只有一个 bool，所以直接提交勾选框的
// 当前状态即可。
//
// ⚠ 唯一的坑：**没成功加载过就不要提交**。页面刚建起来时勾选框是未勾选的（Go 的零值），
// 如果这时被"保存全局配置"顺手提交，就会把服务器上的"允许双开"改成"关闭双开"，
// 而运营在界面上完全看不出发生了什么（与公告/福利那些整组替换的组同一个坑）。
// 所以：没成功刷新过就如实报错，让运营先点「刷新」。
func (a *App) globalSaveMultiOpen(ctx context.Context, c *APIClient) error {
	if a.moChk == nil {
		return nil // 控件未就绪（页面没建起来）→ 跳过，不算失败
	}
	if !a.moLoaded {
		return fmt.Errorf("尚未成功加载过（先点该组的『刷新状态』或顶部『刷新全部配置』），" +
			"本次跳过以免把服务器上的双开开关改成界面上的默认勾选")
	}
	st, err := c.MultiOpenApply(ctx, a.moChk.Checked())
	if err != nil {
		return err
	}
	a.sync(func() { a.applyMultiOpenToUI(st) })
	return nil
}

func (a *App) globalSaveAnnounceSchedule(ctx context.Context, c *APIClient) error {
	// 定时公告/公告栏/DLL 都是"整表替换"：直接提交表格里当前的内容。
	items := append([]ScheduledAnnouncement(nil), a.asItems...)
	saved, err := c.AnnounceScheduleApply(ctx, items)
	if err != nil {
		return err
	}
	a.sync(func() {
		a.asItems = saved
		a.asApplyTable()
	})
	return nil
}

func (a *App) globalSaveNotices(ctx context.Context, c *APIClient) error {
	items := append([]LauncherNotice(nil), a.ntItems...)
	saved, err := c.NoticesApply(ctx, items)
	if err != nil {
		return err
	}
	a.sync(func() {
		a.ntItems = saved
		a.ntApplyTable()
	})
	return nil
}

func (a *App) globalSaveDLLs(ctx context.Context, c *APIClient) error {
	items := append([]LauncherDLL(nil), a.dlItems...)
	saved, err := c.DLLsApply(ctx, items)
	if err != nil {
		return err
	}
	a.sync(func() {
		a.dlItems = saved
		a.dlApplyTable()
	})
	return nil
}

func (a *App) globalSaveWelfare(ctx context.Context, c *APIClient) error {
	req, err := a.collectWelfareConfig()
	if err != nil {
		return err
	}
	st, err := c.WelfareApply(ctx, req)
	if err != nil {
		return err
	}
	a.sync(func() { a.applyWelfareToUI(st) })
	return nil
}

// globalSaveButtons 状态栏两个按钮的构造（供 main.go 复用，避免两处写样式）。
func (a *App) globalSaveButtons() []decl.Widget {
	return []decl.Widget{
		decl.PushButton{
			AssignTo:    &a.globalSaveBtn,
			Text:        "保存全局配置",
			MaxSize:     decl.Size{Width: 120, Height: 0},
			OnClicked:   func() { a.onGlobalSave() },
			ToolTipText: "把服务器选项页各组当前界面上的值依次保存到服务器（在线福利三档期也在内）；各组独立生效，失败的会列出原因",
		},
		decl.PushButton{
			AssignTo:    &a.globalRefreshBtn,
			Text:        "刷新全局配置",
			MaxSize:     decl.Size{Width: 120, Height: 0},
			OnClicked:   func() { a.onGlobalRefresh() },
			ToolTipText: "重新从服务器拉取全部配置并回填到对应分组",
		},
	}
}
