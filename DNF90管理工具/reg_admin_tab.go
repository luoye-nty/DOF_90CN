package main

// 服务器选项页：注册限制（关闭玩家自助注册）+ 管理员建号/改密/强制下线。
// 与 api_client.go 中 RegistrationLimit*/Admin* 端点封装一一对应。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// onRegistrationLimitRefresh 拉取服务器注册限制状态并刷新 UI。
func (a *App) onRegistrationLimitRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if !a.beginOp() {
		// 静默调用（打开主窗口时的首次拉取）不弹窗：那时用户没做任何操作，
		// 弹一个"操作进行中"只会莫名其妙；下一次刷新按钮仍能正常触发。
		if !silent {
			a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		}
		return
	}
	a.regSetBusy(true)
	go func() {
		defer a.sync(func() { a.regSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.RegistrationLimitStatus(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询注册限制配置失败", err)
				}
			}
			return
		}
		a.sync(func() { a.applyRegistrationLimitToUI(st) })
	}()
}

// onRegistrationLimitSave 校验输入并调用服务器应用注册限制配置（关闭开关 + 每 IP 上限 + 注册码开关）。
func (a *App) onRegistrationLimitSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	closed := a.regChk.Checked()
	codeRequired := a.regCodeChk != nil && a.regCodeChk.Checked()
	maxPerIP, ok := regParseMaxPerIP(a.regMaxIPLE)
	if !ok {
		a.showInfo("参数错误", fmt.Sprintf("每 IP 最多注册必须是 %d~%d 的整数（%d = 不限制，0 = 不允许注册）", regUnlimitedPerIP, regMaxPerIPLimit, regUnlimitedPerIP))
		return
	}
	action := "开放玩家自助注册"
	if closed {
		action = "关闭玩家自助注册（新玩家只能由管理员建号）"
	}
	ipRule := regPerIPRuleText(maxPerIP)
	codeRule := regCodeRuleText(codeRequired)
	if !a.confirm("保存注册限制配置", fmt.Sprintf("将向服务器应用以下配置：\n\n%s\n%s\n%s\n\n配置会持久化到服务器 config.json，是否继续？", action, ipRule, codeRule)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.regSetBusy(true)
	go func() {
		defer a.sync(func() { a.regSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.RegistrationLimitApply(ctx, closed, maxPerIP, codeRequired)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存注册限制配置失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyRegistrationLimitToUI(st)
			a.showInfo("保存成功", "注册限制配置已应用到服务器并持久化。")
		})
	}()
}

// regCodeRuleText 把"注册码开关"翻译成人话（确认框/状态行共用）。
func regCodeRuleText(codeRequired bool) string {
	if codeRequired {
		return "注册需要注册码（登录器注册界面会多出注册码输入框，必须填对一枚未使用的码）"
	}
	return "注册不需要注册码"
}

// regMaxPerIPLimit 与服务端 regMaxPerIPLimit 保持一致（internal/config 之外的 UI 侧校验）。
const regMaxPerIPLimit = 1000

// regUnlimitedPerIP 表示"不按 IP 限制"的取值（与服务端一致）。
// 注意 0 **不是**不限制，而是"不允许注册"。
const regUnlimitedPerIP = -1

// regPerIPRuleText 把每 IP 上限翻译成人话（确认框/状态行共用）。
func regPerIPRuleText(maxPerIP int) string {
	switch {
	case maxPerIP < 0:
		return "注册数量不按 IP 限制"
	case maxPerIP == 0:
		return "不允许注册（每个 IP 上限 0，等于关闭自助注册）"
	default:
		return fmt.Sprintf("同一个 IP 最多注册 %d 个账号", maxPerIP)
	}
}

// regParseMaxPerIP 解析"每 IP 最多注册"输入框：留空 = 不限制（-1），非法值返回 false。
// 取值范围 -1~1000：-1=不限制，0=不允许注册，1~1000=每个 IP 上限。
func regParseMaxPerIP(edit *walk.LineEdit) (int, bool) {
	if edit == nil {
		return regUnlimitedPerIP, true
	}
	text := strings.TrimSpace(edit.Text())
	if text == "" {
		return regUnlimitedPerIP, true
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < regUnlimitedPerIP || value > regMaxPerIPLimit {
		return regUnlimitedPerIP, false
	}
	return value, true
}

// regSetBusy 切换按钮可用状态（防并发保存/刷新）。
func (a *App) regSetBusy(b bool) {
	a.regBusy = b
	if a.regSaveBtn != nil {
		a.regSaveBtn.SetEnabled(!b)
	}
	if a.regRefresh != nil {
		a.regRefresh.SetEnabled(!b)
	}
	if a.regCodeGenBtn != nil {
		a.regCodeGenBtn.SetEnabled(!b)
	}
	if a.regCodeCopyBtn != nil {
		a.regCodeCopyBtn.SetEnabled(!b)
	}
	if a.regCodeRevokeBtn != nil {
		a.regCodeRevokeBtn.SetEnabled(!b)
	}
	if a.regCodeReloadBtn != nil {
		a.regCodeReloadBtn.SetEnabled(!b)
	}
}

// applyRegistrationLimitToUI 将远端配置回填控件与状态标签。
func (a *App) applyRegistrationLimitToUI(st *RegistrationLimitStatus) {
	if st == nil {
		return
	}
	if a.regChk != nil {
		a.regChk.SetChecked(st.Closed)
	}
	if a.regCodeChk != nil {
		a.regCodeChk.SetChecked(st.CodeRequired)
	}
	if a.regMaxIPLE != nil {
		a.regMaxIPLE.SetText(strconv.Itoa(st.MaxPerIP))
	}
	if a.regStateL == nil {
		return
	}
	ipPart := regPerIPRuleText(st.MaxPerIP)
	codePart := "注册不需注册码"
	if st.CodeRequired {
		codePart = "注册需要注册码"
	}
	if st.Closed {
		a.regStateL.SetText("状态：已关闭玩家自助注册（登录器注册将被拦截，仅管理员可建号）｜" + ipPart + "｜" + codePart)
		a.regStateL.SetTextColor(walk.RGB(180, 60, 0))
	} else {
		a.regStateL.SetText("状态：自助注册开放中（玩家可直接在登录器注册）｜" + ipPart + "｜" + codePart)
		a.regStateL.SetTextColor(walk.RGB(0, 140, 0))
	}
}

// ---------------- 注册码（邀请码）生成 / 列表 / 复制 / 作废 ----------------

// regCodeFilterOptions 列表过滤下拉（索引与 regCodeFilterState 一一对应）。
var regCodeFilterOptions = []string{
	"未使用",
	"全部",
	"已使用",
	"已作废",
}

// regCodeFilterState 把下拉索引翻成服务端 state 参数（空=全部）。
func regCodeFilterState(idx int) string {
	switch idx {
	case 0:
		return "unused"
	case 2:
		return "used"
	case 3:
		return "revoked"
	default:
		return ""
	}
}

// regCodeStateText 状态中文（列表展示用）。
func regCodeStateText(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "unused":
		return "未使用"
	case "used":
		return "已使用"
	case "revoked":
		return "已作废"
	default:
		return state
	}
}

// onRegCodeFilterChanged 注册码列表的过滤下拉变化 → 重新拉列表。
//
// 注意：walk 的 ComboBox.SetCurrentIndex 会发布 CurrentIndexChanged，
// 而建窗口时 declarative 就会设一次 CurrentIndex —— 也就是说这个回调会在**还没登录、
// 窗口还没显示**的时候先被调一次。那时如果走正常流程就会弹一个"请先登录"，非常莫名。
// 所以这里先判客户端，没有就直接返回。
func (a *App) onRegCodeFilterChanged() {
	if a.snapshotClient() == nil {
		return
	}
	a.onRegCodeRefresh(false)
}

// regCodeStuckAfter 超过这个时长仍没有结果，就认为上一次查询卡死了：
// 强制复位忙碌标记，让「刷新列表」重新可用 —— 否则那行字会永远停在"正在查询中"，
// 只能重启程序（2026-09-28 线上就是这个现象）。
const regCodeStuckAfter = 25 * time.Second

// onRegCodeRefresh 拉取注册码列表（silent=true 时不弹错误框，用于打开页面时的静默刷新）。
func (a *App) onRegCodeRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.regCodeBusy {
		stuckFor := time.Since(a.regCodeStartedAt)
		if !a.regCodeStartedAt.IsZero() && stuckFor > regCodeStuckAfter {
			// 上一次查询卡死：留日志 + 强制复位后继续本次刷新（自愈）。
			clientLogf("注册码刷新：上一次查询已卡住 %s，强制复位后重试", stuckFor.Round(time.Second))
			a.regCodeBusy = false
			a.sync(func() {
				a.regSetBusy(false)
				if a.regCodeStateL != nil {
					a.regCodeStateL.SetText("注册码：上次查询超时，已复位，正在重试…")
				}
			})
		} else {
			// 正常在查：给一句可见反馈，别让「刷新列表」看起来"点了没反应"。
			a.sync(func() {
				if a.regCodeStateL != nil {
					a.regCodeStateL.SetText("注册码：正在查询中，请稍候…")
				}
			})
			return
		}
	}
	a.regCodeBusy = true
	a.regCodeStartedAt = time.Now()
	a.regSetBusy(true)
	// UI 更新一律走 a.sync：本函数既会被 GUI 线程的按钮调用，也会被 main 的
	// "打开窗口就静默拉一次"调用（那时还没进消息循环）。
	a.sync(func() {
		if a.regCodeStateL != nil {
			a.regCodeStateL.SetText("注册码：加载中…")
		}
	})
	state := ""
	if a.regCodeFilter != nil {
		state = regCodeFilterState(a.regCodeFilter.CurrentIndex())
	}
	clientLogf("注册码刷新：开始（state=%q silent=%v）", state, silent)
	go func() {
		started := time.Now()
		// 复位直接在 goroutine 里做，不再依赖 GUI 线程执行回调：
		// 回调若因为窗口状态没跑到，忙碌标记就会永久卡在 true（线上的现象）。
		defer func() {
			a.regCodeBusy = false
			clientLogf("注册码刷新：结束（耗时 %s）", time.Since(started).Round(time.Millisecond))
			a.sync(func() { a.regSetBusy(false) })
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		data, err := c.RegistrationCodeList(ctx, state, 200)
		if err != nil {
			clientLogf("注册码刷新：请求失败（%s）：%v", time.Since(started).Round(time.Millisecond), err)
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询注册码失败", err)
				}
				brief := err.Error()
				if len([]rune(brief)) > 60 {
					brief = string([]rune(brief)[:60]) + "…"
				}
				a.sync(func() {
					if a.regCodeStateL != nil {
						a.regCodeStateL.SetText("注册码：查询失败 —— " + brief)
					}
				})
			}
			return
		}
		clientLogf("注册码刷新：成功，共 %d 枚（未使用 %d）", data.Total, data.Unused)
		// 已使用的注册码会显示"使用者"（存的是账号主键）：先批量翻成真实账号名再回填。
		ids := make([]string, 0, len(data.Items))
		for _, it := range data.Items {
			if strings.EqualFold(it.State, "used") {
				ids = append(ids, it.UsedBy)
			}
		}
		a.resolveAccountNames(acctNameList(ids...))
		a.sync(func() { a.applyRegCodeListToUI(data) })
	}()
}

// applyRegCodeListToUI 回填列表与状态行（仅主线程调用）。
func (a *App) applyRegCodeListToUI(data *RegistrationCodeListData) {
	if data == nil {
		return
	}
	a.regCodeItems = data.Items
	displays := make([]string, 0, len(data.Items))
	for _, it := range data.Items {
		used := ""
		if strings.EqualFold(it.State, "used") {
			// 使用者存的是账号主键（dnf:<sha256>）：显示真实账号名，翻不到才回退主键。
			used = "｜使用者 " + orDash(acctNameText(it.UsedBy)) + "｜" + orDash(it.UsedAt)
		}
		displays = append(displays, fmt.Sprintf("%s  [%s]%s", it.Code, regCodeStateText(it.State), used))
	}
	if a.regCodeList != nil {
		if len(displays) == 0 {
			displays = []string{"（没有符合条件的注册码）"}
		}
		a.regCodeList.SetModel(displays)
		a.regCodeList.SetCurrentIndex(-1)
	}
	if a.regCodeStateL != nil {
		a.regCodeStateL.SetText(fmt.Sprintf("注册码：共 %d 枚，其中未使用 %d 枚（一次性：玩家注册用过即失效）", data.Total, data.Unused))
	}
}

// onRegCodeGenerate 生成注册码：数量 1~200，成功后刷新列表并把新码显示在状态行便于复制。
func (a *App) onRegCodeGenerate() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	count, ok := regParseCodeCount(a.regCodeCount)
	if !ok {
		a.showInfo("参数错误", "生成数量必须是 1~200 的整数")
		return
	}
	if !a.confirm("生成注册码", fmt.Sprintf("将在服务器生成 %d 枚一次性注册码（8 位短码，形如 ABCD-EFGH）。\n\n注册码用过即失效；玩家在登录器注册界面填写。是否继续？", count)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.regSetBusy(true)
	go func() {
		defer a.sync(func() { a.regSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		codes, err := c.RegistrationCodeCreate(ctx, count)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("生成注册码失败", err)
			}
			return
		}
		a.sync(func() {
			if len(codes) > 0 {
				clip := walk.Clipboard()
				if err := clip.SetText(strings.Join(codes, "\r\n")); err != nil {
					a.showInfo("生成成功", fmt.Sprintf("已生成 %d 枚注册码：\n%s\n\n（写入剪贴板失败：%v，请从列表里复制）", len(codes), strings.Join(codes, "\n"), err))
				} else {
					a.showInfo("生成成功", fmt.Sprintf("已生成 %d 枚注册码（已复制到剪贴板）：\n%s", len(codes), strings.Join(codes, "\n")))
				}
			}
			a.onRegCodeRefresh(true)
		})
	}()
}

// regParseCodeCount 解析"生成数量"输入框：默认 1，非法返回 false。
func regParseCodeCount(edit *walk.LineEdit) (int, bool) {
	if edit == nil {
		return 1, true
	}
	text := strings.TrimSpace(edit.Text())
	if text == "" {
		return 1, true
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 1 || value > 200 {
		return 0, false
	}
	return value, true
}

// onRegCodeCopy 复制选中的注册码（未选中时提示）。
func (a *App) onRegCodeCopy() {
	idx := -1
	if a.regCodeList != nil {
		idx = a.regCodeList.CurrentIndex()
	}
	if idx < 0 || idx >= len(a.regCodeItems) {
		a.showInfo("未选择", "请先在列表里选中一枚注册码")
		return
	}
	code := a.regCodeItems[idx].Code
	if err := walk.Clipboard().SetText(code); err != nil {
		a.showError("复制失败", err)
		return
	}
	a.showInfo("已复制", "注册码已复制到剪贴板："+code)
}

// onRegCodeRevoke 作废选中的注册码（仅未使用的能作废；已使用的码服务端不会动）。
func (a *App) onRegCodeRevoke() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	idx := -1
	if a.regCodeList != nil {
		idx = a.regCodeList.CurrentIndex()
	}
	if idx < 0 || idx >= len(a.regCodeItems) {
		a.showInfo("未选择", "请先在列表里选中一枚注册码")
		return
	}
	item := a.regCodeItems[idx]
	if !strings.EqualFold(item.State, "unused") {
		a.showInfo("无需作废", fmt.Sprintf("这枚注册码状态是「%s」，只有未使用的码可以作废。", regCodeStateText(item.State)))
		return
	}
	if !a.confirm("作废注册码", fmt.Sprintf("确认作废这枚注册码？\n\n%s\n\n作废后无法再用于注册。", item.Code)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.regSetBusy(true)
	go func() {
		defer a.sync(func() { a.regSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		revoked, err := c.RegistrationCodeRevoke(ctx, []string{item.Code})
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("作废注册码失败", err)
			}
			return
		}
		a.sync(func() {
			if revoked == 0 {
				a.showInfo("未作废", "服务器未作废任何注册码：它可能刚刚被玩家用掉，请刷新列表查看最新状态。")
			} else {
				a.showInfo("已作废", "注册码已作废。")
			}
			a.onRegCodeRefresh(true)
		})
	}()
}

// adSetBusy 切换管理员操作按钮可用状态。
func (a *App) adSetBusy(b bool) {
	a.adBusy = b
	if a.adRegBtn != nil {
		a.adRegBtn.SetEnabled(!b)
	}
	if a.adPwBtn != nil {
		a.adPwBtn.SetEnabled(!b)
	}
	if a.adKoBtn != nil {
		a.adKoBtn.SetEnabled(!b)
	}
}

// adSetMsg 状态栏提示（仅主线程调用）。
func (a *App) adSetMsg(text string, color walk.Color) {
	if a.adMsgL == nil {
		return
	}
	a.adMsgL.SetText(text)
	a.adMsgL.SetTextColor(color)
}

// onAdminRegister 管理员建号（绕过注册限制，自动套用注册赠送规则）。
func (a *App) onAdminRegister() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	username := strings.TrimSpace(a.adUserLE.Text())
	password := a.adPassLE.Text()
	rescueCode := strings.TrimSpace(a.adRescueLE.Text())
	if len(username) < 3 || len(username) > 32 {
		a.showInfo("参数错误", "账号名需 3~32 位")
		return
	}
	if len(password) < 8 {
		a.showInfo("参数错误", "密码至少 8 位")
		return
	}
	// 救援代码留空 = 不设置；填了就必须是 6 位数字（与服务端 store.ValidRescueCode 一致）。
	if rescueCode != "" && !admValidRescueCode(rescueCode) {
		a.showInfo("参数错误", "救援代码必须是 6 位数字（例如 123456）；留空表示不设置。")
		return
	}
	rescueText := "（不设置）"
	if rescueCode != "" {
		rescueText = rescueCode
	}
	if !a.confirm("管理员建号", fmt.Sprintf("将在服务器创建账号：\n\n账号：%s\n救援代码：%s\n\n创建后自动按「注册赠送」规则登记点券，是否继续？", username, rescueText)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.adSetBusy(true)
	a.adSetMsg("正在创建账号...", walk.RGB(120, 120, 120))
	go func() {
		defer a.sync(func() { a.adSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := c.AdminRegister(ctx, username, password, rescueCode)
		if err != nil {
			a.sync(func() { a.adSetMsg("建号失败："+err.Error(), walk.RGB(200, 0, 0)) })
			if !a.handleAPIError(err) {
				a.showError("管理员建号失败", err)
			}
			return
		}
		a.sync(func() {
			a.adSetMsg(fmt.Sprintf("账号 %s 创建成功（已按注册赠送规则登记点券）", username), walk.RGB(0, 140, 0))
			a.adPassLE.SetText("")
			a.adRescueLE.SetText("")
			a.showInfo("建号成功", fmt.Sprintf("账号 %s 已创建，密码已生效，可直接用于登录器登录。", username))
		})
	}()
}

// admValidRescueCode 救援代码必须为 6 位纯数字（与服务端 store.ValidRescueCode 同一规则，
// 这里先拦一道，避免白跑一次网络请求才报错）。
func admValidRescueCode(code string) bool {
	if len(code) != 6 {
		return false
	}
	for _, ch := range code {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// onAdminChangePassword 管理员改密（改密后该账号旧会话全部下线）。
func (a *App) onAdminChangePassword() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	username := strings.TrimSpace(a.adPwUserLE.Text())
	newPassword := a.adPwNewLE.Text()
	if username == "" {
		a.showInfo("参数错误", "请填写要改密的账号")
		return
	}
	if len(newPassword) < 8 {
		a.showInfo("参数错误", "新密码至少 8 位")
		return
	}
	if !a.confirm("管理员改密", fmt.Sprintf("将重置账号 %s 的密码，且该账号当前所有登录会话会被强制下线，是否继续？", username)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.adSetBusy(true)
	a.adSetMsg("正在修改密码...", walk.RGB(120, 120, 120))
	go func() {
		defer a.sync(func() { a.adSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := c.AdminChangePassword(ctx, username, newPassword)
		if err != nil {
			a.sync(func() { a.adSetMsg("改密失败："+err.Error(), walk.RGB(200, 0, 0)) })
			if !a.handleAPIError(err) {
				a.showError("管理员改密失败", err)
			}
			return
		}
		a.sync(func() {
			a.adSetMsg(fmt.Sprintf("账号 %s 密码已修改，旧会话已全部下线", username), walk.RGB(0, 140, 0))
			a.adPwNewLE.SetText("")
			a.showInfo("改密成功", fmt.Sprintf("账号 %s 的密码已修改，旧会话已全部下线。", username))
		})
	}()
}

// onAdminForceLogout 强制指定账号下线。
func (a *App) onAdminForceLogout() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	username := strings.TrimSpace(a.adKoUserLE.Text())
	if username == "" {
		a.showInfo("参数错误", "请填写要强制下线的账号")
		return
	}
	if !a.confirm("强制下线", fmt.Sprintf("将强制账号 %s 的所有登录会话下线（登录器将提示重新登录），是否继续？", username)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.adSetBusy(true)
	a.adSetMsg("正在强制下线...", walk.RGB(120, 120, 120))
	go func() {
		defer a.sync(func() { a.adSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err := c.AdminForceLogout(ctx, username)
		if err != nil {
			a.sync(func() { a.adSetMsg("强制下线失败："+err.Error(), walk.RGB(200, 0, 0)) })
			if !a.handleAPIError(err) {
				a.showError("强制下线失败", err)
			}
			return
		}
		a.sync(func() {
			a.adSetMsg(fmt.Sprintf("账号 %s 已强制下线", username), walk.RGB(0, 140, 0))
			a.showInfo("操作成功", fmt.Sprintf("账号 %s 的所有会话已强制下线。", username))
		})
	}()
}
