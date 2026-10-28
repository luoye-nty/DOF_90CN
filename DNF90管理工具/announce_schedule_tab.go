package main

// 服务器选项页「定时公告」分组逻辑：排队多条"一次性 / 每天 / 每周"公告，
// 由服务器端（运维工具）到点自动按「发送公告」同一条链路发出。
//
// 交互约定（与服务端整批保存语义一致）：
//   - 表格里的增删改勾选**只在本页生效**，点「保存全部到服务器」才写入 config.json；
//   - 保存是"整批替换"：表格里没有的行等于删除；
//   - 勾选框 = 是否启用（停用后配置保留但不发送）。

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lxn/walk"
)

// 方式下拉：显示文案与协议值一一对应（顺序不能改，索引映射依赖它）。
var (
	asModeLabels = []string{"每天", "一次性", "每周"}
	asModeValues = []string{"daily", "once", "weekly"}
	// asWeekdayLabels 星期下拉：索引 0..6 对应服务端 weekday 1..7。
	asWeekdayLabels = []string{"周一", "周二", "周三", "周四", "周五", "周六", "周日"}
)

// asTableModel 定时公告表模型（启用勾选框列 + 内容|方式|时间|日期/星期|下次发送|最近已处理）。
//
// 勾选框列（walk.ItemChecker）与 acctOnlineModel 同款做法：
// 勾选状态存 checked，勾选时同步写回 items[row].Enabled（保存时直接用 items）。
type asTableModel struct {
	walk.TableModelBase
	items   []ScheduledAnnouncement
	checked []bool
}

func (m *asTableModel) RowCount() int { return len(m.items) }

// reset 换一批数据：勾选状态按服务端的 enabled 重建，避免"错位"到新数据上。
func (m *asTableModel) reset(items []ScheduledAnnouncement) {
	m.items = items
	m.checked = make([]bool, len(items))
	for i, it := range items {
		m.checked[i] = it.Enabled
	}
}

// Checked 实现 walk.ItemChecker（表格勾选框列的取值）。
func (m *asTableModel) Checked(index int) bool {
	if index < 0 || index >= len(m.checked) {
		return false
	}
	return m.checked[index]
}

// SetChecked 实现 walk.ItemChecker（用户点勾选框）。
// 这里只存状态、不发通知：勾选框的视觉更新由 walk 自己完成，
// 在回调里再 PublishRowsReset 会重入模型（与在线列表同款处理）。
func (m *asTableModel) SetChecked(index int, checked bool) error {
	if index < 0 || index >= len(m.checked) {
		return nil
	}
	m.checked[index] = checked
	m.items[index].Enabled = checked
	return nil
}

func (m *asTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	it := m.items[row]
	switch col {
	case 0:
		// 勾选框列：文字留给勾选框本身，不再重复写"启用/停用"。
		return ""
	case 1:
		return it.Content
	case 2:
		return asModeLabel(it.Mode)
	case 3:
		return it.Time
	case 4:
		switch it.Mode {
		case "once":
			return it.Date
		case "weekly":
			return asWeekdayLabel(it.Weekday)
		}
		return "每天"
	case 5:
		if !it.Enabled {
			return "未启用"
		}
		if it.NextRun == "" {
			return "不再发送"
		}
		return asFormatNextRun(it.NextRun)
	case 6:
		return it.LastRun
	}
	return nil
}

// asModeLabel 协议值 → 下拉文案。
func asModeLabel(mode string) string {
	for i, v := range asModeValues {
		if v == mode {
			return asModeLabels[i]
		}
	}
	return mode
}

// asWeekdayLabel 服务端 weekday（1..7）→ 文案（越界原样显示）。
func asWeekdayLabel(wd int) string {
	if wd >= 1 && wd <= len(asWeekdayLabels) {
		return asWeekdayLabels[wd-1]
	}
	return fmt.Sprintf("%d", wd)
}

// asFormatNextRun 把服务端给的 RFC3339 时刻换成"今天 20:00 / 09-26 20:00"这种好读的样子。
func asFormatNextRun(s string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return s
	}
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return "今天 " + t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// asSyncModeFields 按"方式"显示/隐藏只对某些方式有意义的输入框。
// 每天：日期与星期都不需要；一次性：只需要日期；每周：只需要星期。
func (a *App) asSyncModeFields() {
	mode := a.asCurrentMode()
	showDate := mode == "once"
	showWeekday := mode == "weekly"
	if a.asDateLabel != nil {
		a.asDateLabel.SetVisible(showDate)
	}
	if a.asDateLE != nil {
		a.asDateLE.SetVisible(showDate)
	}
	if a.asWeekdayLabel != nil {
		a.asWeekdayLabel.SetVisible(showWeekday)
	}
	if a.asWeekdayCb != nil {
		a.asWeekdayCb.SetVisible(showWeekday)
	}
}

// asCurrentMode 当前下拉选中的方式（协议值；控件缺失/越界时按"每天"处理）。
func (a *App) asCurrentMode() string {
	if a.asModeCb == nil {
		return "daily"
	}
	idx := a.asModeCb.CurrentIndex()
	if idx < 0 || idx >= len(asModeValues) {
		return "daily"
	}
	return asModeValues[idx]
}

// asSetMode 把方式回填到下拉（用于双击行回填）。
func (a *App) asSetMode(mode string) {
	if a.asModeCb == nil {
		return
	}
	for i, v := range asModeValues {
		if v == mode {
			a.asModeCb.SetCurrentIndex(i)
			break
		}
	}
	a.asSyncModeFields()
}

// asColorIndex 当前选中的颜色下标（越界按蓝色=1）。
func (a *App) asColorIndex() int {
	if a.asColorCb == nil {
		return 1
	}
	idx := a.asColorCb.CurrentIndex()
	if idx < 0 || idx >= len(announceColorValues) {
		return 1
	}
	return idx
}

// asColorValue 当前选中的颜色协议值。
func (a *App) asColorValue() string { return announceColorValues[a.asColorIndex()] }

// asSetColor 按颜色协议值回填下拉（未知颜色按下标 1）。
func (a *App) asSetColor(color string) {
	if a.asColorCb == nil {
		return
	}
	for i, v := range announceColorValues {
		if strings.EqualFold(v, strings.TrimSpace(color)) {
			a.asColorCb.SetCurrentIndex(i)
			return
		}
	}
	a.asColorCb.SetCurrentIndex(1)
}

// asMsg 状态行文案（灰色常规 / 红色错误）。
func (a *App) asMsg(text string, isErr bool) {
	if a.asMsgL == nil {
		return
	}
	a.asMsgL.SetText(text)
	if isErr {
		a.asMsgL.SetTextColor(walk.RGB(190, 30, 30))
	} else {
		a.asMsgL.SetTextColor(walk.RGB(120, 120, 120))
	}
}

// asApplyTable 把内存列表刷进表格（保持当前选中行不至于跳走）。
func (a *App) asApplyTable() {
	if a.asTable == nil {
		return
	}
	model, ok := a.asTable.Model().(*asTableModel)
	if !ok || model == nil {
		model = new(asTableModel)
	}
	model.reset(a.asItems)
	a.asTable.SetModel(model)
}

// asValidateDate 本地校验 YYYY-MM-DD（同时接受没补零的写法，与服务端同口径）。
func asValidateDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006-1-2"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("日期格式必须是 YYYY-MM-DD（例如 2026-10-01）")
}

// asReadInputs 读取"新增/编辑"输入区，返回一条待保存的公告（id/last_run 由调用方保留）。
func (a *App) asReadInputs() (ScheduledAnnouncement, error) {
	item := ScheduledAnnouncement{Color: a.asColorValue(), Enabled: true}
	if a.asContentLE != nil {
		item.Content = strings.TrimSpace(a.asContentLE.Text())
	}
	if item.Content == "" {
		return item, fmt.Errorf("公告内容不能为空")
	}
	if utf8.RuneCountInString(item.Content) > 100 {
		return item, fmt.Errorf("公告内容超过 100 字")
	}
	item.Mode = a.asCurrentMode()
	if a.asTimeHourCb == nil || a.asTimeMinCb == nil {
		return item, fmt.Errorf("时间控件未就绪")
	}
	// 下拉框只能选到 0-23 / 0-59，不存在格式非法，直接拼 "HH:MM"。
	item.Time = clockComboText(a.asTimeHourCb, a.asTimeMinCb)
	switch item.Mode {
	case "once":
		if a.asDateLE == nil {
			return item, fmt.Errorf("一次性公告必须填日期")
		}
		date, err := asValidateDate(a.asDateLE.Text())
		if err != nil {
			return item, err
		}
		item.Date = date
	case "weekly":
		if a.asWeekdayCb == nil {
			return item, fmt.Errorf("每周公告必须选星期几")
		}
		idx := a.asWeekdayCb.CurrentIndex()
		if idx < 0 || idx >= len(asWeekdayLabels) {
			return item, fmt.Errorf("每周公告必须选星期几")
		}
		item.Weekday = idx + 1
	}
	return item, nil
}

// onAnnounceScheduleAdd 把输入区的内容追加成一条待保存的公告（只改本页列表）。
func (a *App) onAnnounceScheduleAdd() {
	item, err := a.asReadInputs()
	if err != nil {
		a.asMsg("无法添加："+err.Error(), true)
		return
	}
	// 新建项的 id 由服务端在保存时生成（这里留空，保存后会用服务端返回的列表刷新）。
	item.CreatedAt = time.Now().Format(time.RFC3339)
	a.asItems = append(a.asItems, item)
	a.asApplyTable()
	if a.asTable != nil {
		a.asTable.SetCurrentIndex(len(a.asItems) - 1)
	}
	if a.asContentLE != nil {
		a.asContentLE.SetText("")
	}
	a.asMsg(fmt.Sprintf("已加入列表（共 %d 条），别忘了点「保存全部到服务器」", len(a.asItems)), false)
}

// onAnnounceScheduleUpdate 用输入区覆盖当前选中的那一行（id / 最近已处理 保持不变）。
func (a *App) onAnnounceScheduleUpdate() {
	if a.asTable == nil {
		return
	}
	idx := a.asTable.CurrentIndex()
	if idx < 0 || idx >= len(a.asItems) {
		a.asMsg("请先在表格里选中一行，再点「更新选中行」", true)
		return
	}
	item, err := a.asReadInputs()
	if err != nil {
		a.asMsg("无法更新："+err.Error(), true)
		return
	}
	old := a.asItems[idx]
	item.ID = old.ID
	item.LastRun = old.LastRun
	item.CreatedAt = old.CreatedAt
	item.Enabled = old.Enabled
	a.asItems[idx] = item
	a.asApplyTable()
	if a.asTable != nil {
		a.asTable.SetCurrentIndex(idx)
	}
	a.asMsg(fmt.Sprintf("已更新第 %d 行，别忘了点「保存全部到服务器」", idx+1), false)
}

// onAnnounceScheduleDelete 删除当前选中的那一行。
func (a *App) onAnnounceScheduleDelete() {
	if a.asTable == nil {
		return
	}
	idx := a.asTable.CurrentIndex()
	if idx < 0 || idx >= len(a.asItems) {
		a.asMsg("请先在表格里选中一行，再点「删除选中行」", true)
		return
	}
	a.asItems = append(a.asItems[:idx], a.asItems[idx+1:]...)
	a.asApplyTable()
	a.asMsg(fmt.Sprintf("已删除，剩余 %d 条；点「保存全部到服务器」后正式生效", len(a.asItems)), false)
}

// onAnnounceSchedulePick 双击表格某行：回填输入区（便于改完再「更新选中行」）。
func (a *App) onAnnounceSchedulePick() {
	if a.asTable == nil {
		return
	}
	idx := a.asTable.CurrentIndex()
	if idx < 0 || idx >= len(a.asItems) {
		return
	}
	it := a.asItems[idx]
	if a.asContentLE != nil {
		a.asContentLE.SetText(it.Content)
	}
	a.asSetColor(it.Color)
	a.asSetMode(it.Mode)
	// 时间回填到两个下拉；服务端存的历史脏值解析不了时回退到 20:00。
	clockComboSet(a.asTimeHourCb, a.asTimeMinCb, it.Time, 20, 0)
	if a.asDateLE != nil && it.Date != "" {
		a.asDateLE.SetText(it.Date)
	}
	if a.asWeekdayCb != nil && it.Weekday >= 1 && it.Weekday <= len(asWeekdayLabels) {
		a.asWeekdayCb.SetCurrentIndex(it.Weekday - 1)
	}
	a.asMsg(fmt.Sprintf("已回填第 %d 行到输入区（改动后点「更新选中行」）", idx+1), false)
}

// onAnnounceScheduleRefresh 拉取服务端列表并覆盖本页（未保存的改动会丢失，故先问一句）。
func (a *App) onAnnounceScheduleRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.asBusy {
		return
	}
	a.asBusy = true
	go func() {
		defer a.sync(func() { a.asBusy = false })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := c.AnnounceScheduleGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("读取定时公告失败", err)
				}
				a.sync(func() { a.asMsg("读取失败："+err.Error(), true) })
			}
			return
		}
		a.sync(func() {
			a.asItems = items
			a.asLoaded = true
			a.asApplyTable()
			a.asMsg(asSummary(len(items)), false)
		})
	}()
}

// onAnnounceScheduleSave 整批保存到服务器（校验失败会被服务端整批拒绝，本页列表不变）。
func (a *App) onAnnounceScheduleSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	if a.asBusy {
		a.showInfo("操作进行中", "正在与服务器通信，请稍候")
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再保存")
		return
	}
	// 发一份拷贝出去：保存期间用户还可能继续点表格，不能边发边改。
	items := append([]ScheduledAnnouncement(nil), a.asItems...)
	enabled := 0
	for _, it := range items {
		if it.Enabled {
			enabled++
		}
	}
	tip := fmt.Sprintf("将把当前 %d 条定时公告（启用 %d 条）整批写入服务器。\n\n表格里没有的公告等于删除。是否继续？", len(items), enabled)
	if len(items) == 0 {
		// 空表 = 整表清空：单独再确认一次（正常保存流程不受影响）。
		tip += "\n\n⚠ 当前列表是**空的**：确认后服务器上的定时公告会被整表清空。"
	}
	if !a.confirm("保存定时公告", tip) {
		a.endOp()
		return
	}
	if len(items) == 0 && !a.confirm("清空定时公告？",
		"再确认一次：表格里一条都没有，保存会把服务器上的定时公告全部删除。\n\n确定要清空吗？") {
		a.endOp()
		return
	}
	a.asBusy = true
	a.asMsg("正在保存…", false)
	go func() {
		defer a.sync(func() { a.asBusy = false; a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		saved, err := c.AnnounceScheduleApply(ctx, items)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存定时公告失败", err)
			}
			a.sync(func() { a.asMsg("保存失败："+err.Error(), true) })
			return
		}
		a.sync(func() {
			// 用服务端返回的列表刷新：新建项拿到了 id、下次计划时刻也是服务端算好的。
			a.asItems = saved
			a.asLoaded = true
			a.asApplyTable()
			a.asMsg("已保存："+asSummary(len(saved)), false)
		})
	}()
}

// asSummary 列表摘要文案。
func asSummary(n int) string {
	if n == 0 {
		return "服务器上暂无定时公告（填好内容与时间后点「添加到列表」）"
	}
	return fmt.Sprintf("已加载 %d 条定时公告", n)
}
