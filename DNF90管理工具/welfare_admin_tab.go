package main

// 服务器选项之外的独立页：「在线福利」。
//
// 三个档期（每日/每周/每月）各自独立配置：启用 + 时间 + 奖励列表 + 邮件标题 + 公告文案。
// 保存时**整批**提交给服务端（服务端校验非法即整批拒绝，不会出现"每日存上了、每周没存上"）。
//
// 奖励行支持三种：道具(item_id+数量) / 点券 / 黑钻天数。
// 发放口径（服务端实现，界面提示里也写明）：
//   - 道具：每个在线角色各发一封（同一账号双开都收到）；
//   - 点券/黑钻：账号级权益，按账号去重只发一次。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// 奖励类型下拉（索引与 welfareKindByIndex 一一对应）。
var welfareKindLabels = []string{"道具", "点券", "黑钻"}

func welfareKindByIndex(idx int) string {
	switch idx {
	case 1:
		return "cera"
	case 2:
		return "black_diamond"
	default:
		return "item"
	}
}

func welfareKindLabel(kind string) string {
	switch kind {
	case "cera":
		return "点券"
	case "black_diamond":
		return "黑钻"
	default:
		return "道具"
	}
}

// welfareSlotWidgets 单个档期的控件集合。
type welfareSlotWidgets struct {
	kind       string
	chk        *walk.CheckBox
	timeHourCb *walk.ComboBox // 时间：小时下拉（0-23）
	timeMinCb  *walk.ComboBox // 时间：分钟下拉（0-59）
	dayLE      *walk.LineEdit // 每周=星期几(1-7)，每月=几号(1-28)
	subjectLE  *walk.LineEdit
	noticeLE   *walk.LineEdit
	table      *walk.TableView
	nextL      *walk.Label
	saveBtn    *walk.PushButton // 保存本档期（只保存这一档）
	tryBtn     *walk.PushButton
	addKindCb  *walk.ComboBox
	// addIDEdit 是**输入框**而不是 NumberEdit：item_id 经常要从别处（PVF 索引、表格、聊天记录）
	// 复制粘贴过来，NumberEdit 连选中文本都做不到（服主实测反馈）。
	// 代价是失去了控件级的数字限制，所以解析时做严格校验（见 welfareParseItemID）。
	addIDEdit  *walk.LineEdit
	addAmtEdit *walk.NumberEdit
	addBtn     *walk.PushButton
	delBtn     *walk.PushButton
	rows       []WelfareReward
}

// welfareSlots 三个档期的控件（索引 by kind）。
var welfareSlots = map[string]*welfareSlotWidgets{}

// welfareOrder 固定顺序（界面与保存都按这个顺序）。
var welfareOrder = []string{"daily", "weekly", "monthly"}

func welfareSlotTitleOf(kind string) string {
	switch kind {
	case "weekly":
		return "每周福利"
	case "monthly":
		return "每月福利"
	default:
		return "每日福利"
	}
}

// welfareRewardRowModel 奖励行表格模型（类型|内容|数量）。
type welfareRewardRowModel struct {
	walk.TableModelBase
	rows []WelfareReward
}

func (m *welfareRewardRowModel) RowCount() int { return len(m.rows) }

func (m *welfareRewardRowModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	r := m.rows[row]
	switch col {
	case 0:
		return welfareKindLabel(r.Kind)
	case 1:
		if r.Kind == "item" {
			if strings.TrimSpace(r.Label) != "" {
				return fmt.Sprintf("%d %s", r.ItemID, r.Label)
			}
			return fmt.Sprintf("item_id=%d", r.ItemID)
		}
		return "—"
	case 2:
		switch r.Kind {
		case "cera":
			return fmt.Sprintf("%d 点券", r.Amount)
		case "black_diamond":
			return fmt.Sprintf("%d 天", r.Amount)
		default:
			return r.Amount
		}
	}
	return nil
}

// 每档期一个模型实例。
var welfareModels = map[string]*welfareRewardRowModel{}

// buildWelfarePage 「在线福利」页。
func buildWelfarePage(a *App) decl.TabPage {
	children := []decl.Widget{
		decl.Label{Text: "在线福利：到点向**在线玩家**发放预制奖励，并按自定义文案发一条全服公告。"},
		decl.Label{Text: "发放口径：道具 → 每个在线角色各一封邮件（双开都收到）；点券/黑钻 → 账号级权益，按账号去重只发一次。"},
		decl.Label{Text: "同一档期只发一次；若到点时服务器/运维工具没在运行，按约定**不补发**（只在运维工具日志里记一笔）。"},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{AssignTo: &a.wfRefreshBtn, Text: "刷新", OnClicked: func() { a.onWelfareRefresh(false) }},
				decl.PushButton{AssignTo: &a.wfSaveBtn, Text: "保存全部档期", OnClicked: func() { a.onWelfareSave() }, MinSize: decl.Size{Width: 120, Height: 0}},
				decl.Label{AssignTo: &a.wfStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
			},
		},
	}
	for _, kind := range welfareOrder {
		w := &welfareSlotWidgets{kind: kind}
		welfareSlots[kind] = w
		welfareModels[kind] = &welfareRewardRowModel{}
		title := welfareSlotTitleOf(kind)
		dayLabel := ""
		dayTip := ""
		if kind == "weekly" {
			dayLabel = "星期几(1=周一…7=周日):"
			dayTip = "1=周一 … 7=周日；到点那一周的这一天 24 小时内只发一次"
		} else if kind == "monthly" {
			dayLabel = "每月几号(1~28):"
			dayTip = "只能填 1~28（避开 29/30/31 这种月份里不存在的日期）"
		}

		rowChildren := []decl.Widget{
			decl.CheckBox{AssignTo: &w.chk, Text: "启用"},
			decl.Label{Text: "时间："},
			// 时分改成两个下拉（小时 0-23 / 分钟 0-59），服务端协议仍是 "HH:MM"。
			decl.ComboBox{
				AssignTo:     &w.timeHourCb,
				Model:        timeHourLabels,
				CurrentIndex: 20,
				MinSize:      decl.Size{Width: 60, Height: 0},
				ToolTipText:  "小时（0-23）",
			},
			decl.Label{Text: "时"},
			decl.ComboBox{
				AssignTo:     &w.timeMinCb,
				Model:        timeMinuteLabels,
				CurrentIndex: 0,
				MinSize:      decl.Size{Width: 60, Height: 0},
				ToolTipText:  "分钟（0-59）",
			},
			decl.Label{Text: "分"},
		}
		if dayLabel != "" {
			rowChildren = append(rowChildren,
				decl.Label{Text: dayLabel},
				decl.LineEdit{AssignTo: &w.dayLE, MaxLength: 2, MinSize: decl.Size{Width: 40, Height: 0}, ToolTipText: dayTip},
			)
		}
		rowChildren = append(rowChildren,
			decl.Label{Text: "邮件标题:"},
			decl.LineEdit{AssignTo: &w.subjectLE, MinSize: decl.Size{Width: 140, Height: 0}, ToolTipText: "留空用「在线福利」"},
			decl.PushButton{
				AssignTo:    &w.saveBtn,
				Text:        "保存本档期",
				OnClicked:   func() { a.onWelfareSaveSlot(kind) },
				ToolTipText: "只保存这一档（其它档期保持不变）：不会因为别的档期配置有问题而挡住这一档",
			},
			decl.PushButton{AssignTo: &w.tryBtn, Text: "立即试发", OnClicked: func() { a.onWelfareTry(kind) }},
			decl.Label{AssignTo: &w.nextL, Text: "下次：未启用"},
		)

		children = append(children, decl.GroupBox{
			Title:  title,
			Layout: decl.VBox{},
			Children: []decl.Widget{
				decl.Composite{Layout: decl.HBox{MarginsZero: true, Spacing: 6}, Children: rowChildren},
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "公告文案(可空=不发公告，≤100字):"},
						decl.LineEdit{AssignTo: &w.noticeLE, MaxLength: 100, MinSize: decl.Size{Width: 420, Height: 0}, ToolTipText: "发放完成后向全服发这条公告，告知玩家在线奖励已发"},
					},
				},
				decl.TableView{
					AssignTo: &w.table,
					Columns: []decl.TableViewColumn{
						{Title: "类型", Width: 70},
						{Title: "内容", Width: 320},
						{Title: "数量", Width: 120},
					},
					Model:   welfareModels[kind],
					MinSize: decl.Size{Width: 700, Height: 90},
				},
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "新增奖励:"},
						decl.ComboBox{AssignTo: &w.addKindCb, Model: welfareKindLabels, CurrentIndex: 0, MinSize: decl.Size{Width: 80, Height: 0}},
						decl.Label{Text: "item_id:"},
						decl.LineEdit{
							AssignTo: &w.addIDEdit,
							MinSize:  decl.Size{Width: 110, Height: 0},
							ToolTipText: "只填数字（可从别处复制粘贴）；道具才需要，点券/黑钻留空。" +
								"填非数字会被拒绝并提示",
						},
						decl.Label{Text: "数量/天数:"},
						decl.NumberEdit{AssignTo: &w.addAmtEdit, MinValue: 0, MaxValue: 100000000, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 110, Height: 0}},
						decl.PushButton{AssignTo: &w.addBtn, Text: "添加", OnClicked: func() { a.onWelfareAddReward(kind) }},
						decl.PushButton{AssignTo: &w.delBtn, Text: "删除选中", OnClicked: func() { a.onWelfareDelReward(kind) }},
						decl.Label{Text: "提示：装备类道具不支持强化/增幅时会被服务端拒绝。"},
					},
				},
			},
		})
	}
	return decl.TabPage{
		Title:   "在线福利",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.ScrollView{Layout: decl.VBox{}, Children: children},
		},
	}
}

// onWelfareRefresh 拉取三档期状态并回填。
func (a *App) onWelfareRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.wfBusy {
		return
	}
	a.wfBusy = true
	a.wfSetBusy(true)
	go func() {
		defer a.sync(func() { a.wfBusy = false; a.wfSetBusy(false) })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.WelfareStatusGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("查询在线福利失败", err)
				}
			}
			return
		}
		a.sync(func() {
			// 只有"真的从服务器读到过"才置位：整组保存要拿它拦住"没加载成功就整组覆盖"。
			a.wfLoaded = true
			a.applyWelfareToUI(st)
		})
	}()
}

// applyWelfareToUI 把服务端状态回填到三个档期的控件。
func (a *App) applyWelfareToUI(st *WelfareStatus) {
	if st == nil {
		return
	}
	apply := func(kind string, slot WelfareSlot) {
		w := welfareSlots[kind]
		if w == nil {
			return
		}
		if w.chk != nil {
			w.chk.SetChecked(slot.Enabled)
		}
		if w.timeHourCb != nil && w.timeMinCb != nil {
			// 时间回填到两个下拉；历史脏值解析不了时回退 20:00（不让界面显示成 00:00）。
			clockComboSet(w.timeHourCb, w.timeMinCb, slot.Time, 20, 0)
		}
		if w.dayLE != nil {
			if kind == "weekly" {
				w.dayLE.SetText(fmt.Sprintf("%d", slot.Weekday))
			} else {
				w.dayLE.SetText(fmt.Sprintf("%d", slot.Day))
			}
		}
		if w.subjectLE != nil {
			w.subjectLE.SetText(slot.Subject)
		}
		if w.noticeLE != nil {
			w.noticeLE.SetText(slot.Notice)
		}
		if m := welfareModels[kind]; m != nil {
			m.rows = append([]WelfareReward(nil), slot.Rewards...)
			if w.table != nil {
				w.table.SetModel(m)
			}
		}
		if w.nextL != nil {
			w.nextL.SetText(welfareNextRunText(slot))
		}
	}
	apply("daily", st.Daily)
	apply("weekly", st.Weekly)
	apply("monthly", st.Monthly)

	if a.wfStateL != nil {
		a.wfStateL.SetText(fmt.Sprintf("状态：每日 %s ｜ 每周 %s ｜ 每月 %s",
			welfareSlotBrief(st.Daily), welfareSlotBrief(st.Weekly), welfareSlotBrief(st.Monthly)))
		a.wfStateL.SetTextColor(walk.RGB(60, 60, 60))
	}
}

// welfareNextRunText 下次计划时刻文案。
func welfareNextRunText(slot WelfareSlot) string {
	if !slot.Enabled {
		return "下次：未启用"
	}
	next := ""
	if t, err := time.Parse(time.RFC3339, slot.NextRun); err == nil {
		next = t.Local().Format("01-02 15:04")
	}
	last := slot.LastRun
	if last == "" {
		last = "无"
	}
	return fmt.Sprintf("下次：%s（上次已发周期：%s）", orDash(next), last)
}

func welfareSlotBrief(slot WelfareSlot) string {
	if !slot.Enabled {
		return "未启用"
	}
	return fmt.Sprintf("%s %d项", slot.Time, len(slot.Rewards))
}

// onWelfareSaveSlot 只保存一个档期（每个档期自己的「保存本档期」按钮）。
//
// 为什么要单独保存：整批保存会把三个档期一起校验，只要别的档期"已启用但没奖励"
// （历史残留配置），运营就永远存不进自己想改的那一档 —— 线上真实反馈。
func (a *App) onWelfareSaveSlot(kind string) {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	w := welfareSlots[kind]
	if w == nil {
		return
	}
	slot, err := a.collectWelfareSlot(kind, w)
	if err != nil {
		a.showInfo("请检查配置", err.Error())
		return
	}
	title := welfareSlotTitleOf(kind)
	if !a.confirm("保存"+title, welfareSlotSaveSummary(slot)+"\n\n只保存这一档，其它档期保持不变。是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.wfSetBusy(true)
	go func() {
		defer a.sync(func() { a.wfSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.WelfareApplySlot(ctx, kind, slot)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存"+title+"失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyWelfareToUI(st)
			a.showInfo("保存成功", title+" 已应用到服务器并持久化（其它档期未改动）。")
		})
	}()
}

// welfareSlotSaveSummary 单档期保存确认框文案。
func welfareSlotSaveSummary(slot WelfareSlot) string {
	text := fmt.Sprintf("档期：%s\n时间：%s\n奖励：%d 项", welfareSlotBriefTime(slot), slot.Time, len(slot.Rewards))
	if !slot.Enabled {
		text += "\n状态：停用（保存后不会自动发放）"
	}
	return text
}

// welfareSlotBriefTime 档期时间描述（每周/每月带上星期与日期）。
func welfareSlotBriefTime(slot WelfareSlot) string {
	switch {
	case slot.Weekday >= 1 && slot.Weekday <= 7:
		return "每周 " + asWeekdayLabel(slot.Weekday)
	case slot.Day >= 1 && slot.Day <= 28:
		return fmt.Sprintf("每月 %d 号", slot.Day)
	default:
		return "每天"
	}
}

// onWelfareSave 组装三档期并整批保存。
func (a *App) onWelfareSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	req, err := a.collectWelfareConfig()
	if err != nil {
		a.showInfo("请检查配置", err.Error())
		return
	}
	if !a.confirm("保存在线福利配置", welfareSaveSummary(req)+"\n\n配置会持久化到服务器 config.json，是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.wfSetBusy(true)
	go func() {
		defer a.sync(func() { a.wfSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := c.WelfareApply(ctx, req)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存在线福利失败", err)
			}
			return
		}
		a.sync(func() {
			a.applyWelfareToUI(st)
			a.showInfo("保存成功", "在线福利配置已应用到服务器并持久化。")
		})
	}()
}

// collectWelfareConfig 从界面收集三档期配置（含本地格式校验，错误信息直接给用户看）。
func (a *App) collectWelfareConfig() (WelfareApplyRequest, error) {
	var req WelfareApplyRequest
	for _, kind := range welfareOrder {
		w := welfareSlots[kind]
		if w == nil {
			continue
		}
		slot := WelfareSlot{
			Enabled: w.chk != nil && w.chk.Checked(),
			// 时分来自两个下拉，只能选出 0-23 / 0-59，不存在格式非法。
			Time:    clockComboText(w.timeHourCb, w.timeMinCb),
			Subject: strings.TrimSpace(leText(w.subjectLE)),
			Notice:  strings.TrimSpace(leText(w.noticeLE)),
		}
		if m := welfareModels[kind]; m != nil {
			slot.Rewards = append([]WelfareReward(nil), m.rows...)
		}
		if slot.Rewards == nil {
			slot.Rewards = []WelfareReward{}
		}
		if kind == "weekly" {
			day, err := parseWelfareDay(leText(w.dayLE), 1, 7)
			if err != nil {
				return req, fmt.Errorf("每周福利：%w", err)
			}
			slot.Weekday = day
		}
		if kind == "monthly" {
			day, err := parseWelfareDay(leText(w.dayLE), 1, 28)
			if err != nil {
				return req, fmt.Errorf("每月福利：%w", err)
			}
			slot.Day = day
		}
		if slot.Enabled {
			if err := validateWelfareClock(slot.Time); err != nil {
				return req, fmt.Errorf("%s：%w", welfareSlotTitleOf(kind), err)
			}
			if len(slot.Rewards) == 0 {
				return req, fmt.Errorf("%s 已启用但奖励列表是空的", welfareSlotTitleOf(kind))
			}
		}
		switch kind {
		case "daily":
			req.Daily = slot
		case "weekly":
			req.Weekly = slot
		case "monthly":
			req.Monthly = slot
		}
	}
	return req, nil
}

// collectWelfareSlot 只收集一个档期（每个档期的「保存本档期」按钮用）。
//
// 校验口径与整批保存一致：启用档期必须有奖励、时间必须合法；停用档期只要求不填坏值。
func (a *App) collectWelfareSlot(kind string, w *welfareSlotWidgets) (WelfareSlot, error) {
	if w == nil {
		return WelfareSlot{}, fmt.Errorf("界面控件未就绪")
	}
	slot := WelfareSlot{
		Enabled: w.chk != nil && w.chk.Checked(),
		Time:    clockComboText(w.timeHourCb, w.timeMinCb),
		Subject: strings.TrimSpace(leText(w.subjectLE)),
		Notice:  strings.TrimSpace(leText(w.noticeLE)),
	}
	if m := welfareModels[kind]; m != nil {
		slot.Rewards = append([]WelfareReward(nil), m.rows...)
	}
	if slot.Rewards == nil {
		slot.Rewards = []WelfareReward{}
	}
	if kind == "weekly" {
		day, err := parseWelfareDay(leText(w.dayLE), 1, 7)
		if err != nil {
			return slot, fmt.Errorf("每周福利：%w", err)
		}
		slot.Weekday = day
	}
	if kind == "monthly" {
		day, err := parseWelfareDay(leText(w.dayLE), 1, 28)
		if err != nil {
			return slot, fmt.Errorf("每月福利：%w", err)
		}
		slot.Day = day
	}
	if slot.Enabled {
		if err := validateWelfareClock(slot.Time); err != nil {
			return slot, fmt.Errorf("%s：%w", welfareSlotTitleOf(kind), err)
		}
		if len(slot.Rewards) == 0 {
			return slot, fmt.Errorf("%s 已启用但奖励列表是空的（先加奖励，或取消勾选「启用」）", welfareSlotTitleOf(kind))
		}
	}
	return slot, nil
}

// parseWelfareDay 解析星期/日期输入（空 = 默认值）。
func parseWelfareDay(text string, min, max int) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return min, nil
	}
	value := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("必须是 %d~%d 的数字", min, max)
		}
		value = value*10 + int(r-'0')
	}
	if value < min || value > max {
		return 0, fmt.Errorf("必须是 %d~%d 的数字（当前 %d）", min, max, value)
	}
	return value, nil
}

// validateWelfareClock 校验 HH:MM。
func validateWelfareClock(text string) error {
	parts := strings.Split(strings.TrimSpace(text), ":")
	if len(parts) != 2 {
		return fmt.Errorf("时间格式必须是 HH:MM（例如 20:00）")
	}
	hour, err1 := parseWelfareDay(parts[0], 0, 23)
	minute, err2 := parseWelfareDay(parts[1], 0, 59)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("时间必须是 00:00 ~ 23:59（例如 20:00）")
	}
	_ = hour
	_ = minute
	return nil
}

// welfareSaveSummary 保存前摘要（确认框）。
func welfareSaveSummary(req WelfareApplyRequest) string {
	var lines []string
	for _, kind := range welfareOrder {
		var slot WelfareSlot
		switch kind {
		case "daily":
			slot = req.Daily
		case "weekly":
			slot = req.Weekly
		default:
			slot = req.Monthly
		}
		if !slot.Enabled {
			lines = append(lines, welfareSlotTitleOf(kind)+"：未启用")
			continue
		}
		when := slot.Time
		if kind == "weekly" {
			when = fmt.Sprintf("每周%s %s", welfareWeekdayText(slot.Weekday), slot.Time)
		}
		if kind == "monthly" {
			when = fmt.Sprintf("每月 %d 号 %s", slot.Day, slot.Time)
		}
		lines = append(lines, fmt.Sprintf("%s：%s，奖励 %d 项，公告：%s",
			welfareSlotTitleOf(kind), when, len(slot.Rewards), orDash(slot.Notice)))
	}
	return strings.Join(lines, "\n")
}

func welfareWeekdayText(weekday int) string {
	names := []string{"一", "二", "三", "四", "五", "六", "日"}
	if weekday < 1 || weekday > 7 {
		return fmt.Sprintf("%d", weekday)
	}
	return "周" + names[weekday-1]
}

// onWelfareAddReward 把"新增奖励"那一行的输入加进表格。
func (a *App) onWelfareAddReward(kind string) {
	w := welfareSlots[kind]
	m := welfareModels[kind]
	if w == nil || m == nil {
		return
	}
	rewardKind := welfareKindByIndex(w.addKindCb.CurrentIndex())
	itemID := uint32(0)
	amount := uint32(0)
	if w.addIDEdit != nil {
		// item_id 是输入框（可复制粘贴），所以这里的校验是唯一防线：
		// 混进字母/空格/全角数字都被挡下，而不是被悄悄截断成一个错误的 ID。
		parsed, err := welfareParseItemID(w.addIDEdit.Text())
		if err != nil {
			a.showInfo("item_id 不正确", err.Error())
			return
		}
		itemID = parsed
	}
	if w.addAmtEdit != nil {
		if v := w.addAmtEdit.Value(); v > 0 {
			amount = uint32(v)
		}
	}
	reward := WelfareReward{Kind: rewardKind, ItemID: itemID, Amount: amount}
	if _, err := welfareValidateRewardLocal(reward); err != nil {
		a.showInfo("奖励参数不正确", err.Error())
		return
	}
	m.rows = append(m.rows, reward)
	sort.SliceStable(m.rows, func(i, j int) bool { return m.rows[i].Kind < m.rows[j].Kind })
	if w.table != nil {
		w.table.SetModel(m)
	}
	if w.addIDEdit != nil {
		w.addIDEdit.SetText("")
	}
	if w.addAmtEdit != nil {
		w.addAmtEdit.SetValue(0)
	}
}

// onWelfareDelReward 删除选中的奖励行。
func (a *App) onWelfareDelReward(kind string) {
	w := welfareSlots[kind]
	m := welfareModels[kind]
	if w == nil || m == nil || w.table == nil {
		return
	}
	idx := w.table.CurrentIndex()
	if idx < 0 || idx >= len(m.rows) {
		a.showInfo("未选择", "请先在奖励表格里选中一行。")
		return
	}
	m.rows = append(m.rows[:idx], m.rows[idx+1:]...)
	w.table.SetModel(m)
}

// welfareParseItemID 解析"新增奖励"里的 item_id 输入框。
//
// 为什么要有它：这个框从 NumberEdit 换成了 LineEdit（为了能复制粘贴 item_id），
// 控件级的数字限制随之消失，所以这里的校验成了唯一防线。三条口径：
//   - 空 = 0（点券/黑钻奖励本来就不填 item_id）；
//   - **全角数字先转半角**（中文输入法下粘贴"１２３"很常见，直接判非法会让人莫名其妙）；
//   - 去掉首尾空格后**必须全是数字**：混进字母/小数点/负号/其它字符一律报错，
//     绝不"尽力解析成某个数字" —— 那会把一个错误的 item_id 发到全服玩家背包里。
func welfareParseItemID(raw string) (uint32, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, nil
	}
	// 全角 ０-９ → 半角
	text = strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' {
			return r - '０' + '0'
		}
		return r
	}, text)
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("item_id 只能填数字（当前填的是 %q）", raw)
		}
	}
	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("item_id 超出范围（%q）", raw)
	}
	if value > welfareMaxItemID {
		return 0, fmt.Errorf("item_id 超出上限 %d（当前 %d）", welfareMaxItemID, value)
	}
	return uint32(value), nil
}

// welfareMaxItemID item_id 上限。
//
// ── 为什么是 MaxUint32 而不是 99999999（2026-10-05 修"在线奖励ID不支持9位ID"）──
// 换控件前这里是 walk 的 `NumberEdit{MaxValue: 99999999}`（8 位），换成长文本框时
// 把那个上限原样搬了过来。但本服道具 ID **有大量 9 位**（实测 PVF 19.5 万个唯一 ID 里
// 9 位占 50.9%，最大 690001556），于是运营填 9 位 ID 会被这里拒掉 ——
// 而调用方 `onWelfareAddReward` 收到错误后只弹一句提示就 return，
// **奖励行根本没进表格**，运营看到的是"填了、保存了、但福利就是不发"。
//
// 更危险的是旧版的静默行为：`NumberEdit` 越界时会把 9 位**截成前 8 位**
// （490701973 → 49070197），而实测有 5446 个 9 位 ID 的"前 8 位"在 PVF 里
// **也是真实道具** —— 也就是说旧版会安静地发出**另一个道具**，比不发更难发现。
//
// 上限交给 `strconv.ParseUint(text, 10, 32)` 兜（见上面 :725），它天然拒绝 >MaxUint32；
// 服务端全链路（config 的 uint32 / pvfindex / DB 的 BIGINT）本来就支持到 4294967295。
const welfareMaxItemID = 4294967295

// welfareValidateRewardLocal 与服务端同口径的本地预校验（避免白跑一趟请求）。
func welfareValidateRewardLocal(r WelfareReward) (WelfareReward, error) {
	switch r.Kind {
	case "item":
		if r.ItemID == 0 {
			return r, fmt.Errorf("道具奖励必须填 item_id")
		}
		if r.Amount == 0 {
			r.Amount = 1
		}
	case "cera":
		if r.Amount == 0 || r.Amount > 100_000_000 {
			return r, fmt.Errorf("点券数量必须在 1~100000000 之间")
		}
	case "black_diamond":
		if r.Amount == 0 || r.Amount > 3650 {
			return r, fmt.Errorf("黑钻天数必须在 1~3650 之间")
		}
	default:
		return r, fmt.Errorf("未知奖励类型 %q", r.Kind)
	}
	return r, nil
}

// onWelfareTry 立即试发（不判定时间、不影响"今天已发"记录）。
func (a *App) onWelfareTry(kind string) {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	title := welfareSlotTitleOf(kind)
	if !a.confirm("立即试发"+title, "将立刻向当前在线玩家发一次"+title+"（不等时间、不影响自动发放记录）。\n\n适合上线前自测：确认能收到邮件、点券/黑钻到账、公告能弹出来。\n\n是否继续？") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再试")
		return
	}
	a.wfSetBusy(true)
	go func() {
		defer a.sync(func() { a.wfSetBusy(false); a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
		defer cancel()
		res, err := c.WelfareTry(ctx, kind)
		if err != nil {
			if !a.handleAPIError(err) {
				// "没有配置奖励"几乎都是"这个档期还没保存成功"（试发读的是服务器上的配置，
				// 不是界面上刚填的内容）——直接把下一步该干什么写清楚。
				msg := err.Error()
				if strings.Contains(msg, "没有配置奖励") {
					msg += "\n\n试发用的是**服务器上已保存**的配置：请先在对应档期点「保存本档期」，再试发。"
				}
				a.showError("试发失败", fmt.Errorf("%s", msg))
			}
			return
		}
		a.sync(func() {
			a.showInfo("试发完成", fmt.Sprintf(
				"在线账号 %d 个，在线角色 %d 个\n道具邮件 %d 封\n点券 %d 笔\n黑钻 %d 笔\n失败 %d ｜ 公告 %s",
				res.Accounts, res.Characters, res.ItemMails, res.CeraGranted, res.BlackGrant,
				res.Failed, yesNo(res.NoticeSent)))
		})
	}()
}

func yesNo(v bool) string {
	if v {
		return "已发"
	}
	return "未发"
}

// wfSetBusy 切换按钮可用状态。
func (a *App) wfSetBusy(b bool) {
	if a.wfSaveBtn != nil {
		a.wfSaveBtn.SetEnabled(!b)
	}
	if a.wfRefreshBtn != nil {
		a.wfRefreshBtn.SetEnabled(!b)
	}
	for _, kind := range welfareOrder {
		if w := welfareSlots[kind]; w != nil {
			if w.tryBtn != nil {
				w.tryBtn.SetEnabled(!b)
			}
			if w.saveBtn != nil {
				w.saveBtn.SetEnabled(!b)
			}
		}
	}
}
