package main

// 服务器选项页「服务器公告栏」分组逻辑：维护登录器登录页左侧那块公告板。
//
// 条目模型：标题 + 日期（无正文）+ 是否启用；顺序就是表格顺序（保存时按行序落库）。
// 保存同样是"整表替换"：表格里没有的行等于删除。

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lxn/walk"
)

// noticeMaxTitleRunes 标题长度上限（与服务端 store.NoticeMaxTitleRunes 同口径）。
const noticeMaxTitleRunes = 60

// ntTableModel 公告栏表模型（启用勾选框列 + 标题 + 日期）。
type ntTableModel struct {
	walk.TableModelBase
	items   []LauncherNotice
	checked []bool
}

func (m *ntTableModel) RowCount() int { return len(m.items) }

func (m *ntTableModel) reset(items []LauncherNotice) {
	m.items = items
	m.checked = make([]bool, len(items))
	for i, it := range items {
		m.checked[i] = it.Enabled
	}
}

// Checked 实现 walk.ItemChecker（表格勾选框列的取值）。
func (m *ntTableModel) Checked(index int) bool {
	if index < 0 || index >= len(m.checked) {
		return false
	}
	return m.checked[index]
}

// SetChecked 实现 walk.ItemChecker：只存状态 + 回写 items（保存时直接用它），不发通知。
func (m *ntTableModel) SetChecked(index int, checked bool) error {
	if index < 0 || index >= len(m.checked) {
		return nil
	}
	m.checked[index] = checked
	m.items[index].Enabled = checked
	return nil
}

func (m *ntTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	it := m.items[row]
	switch col {
	case 0:
		return "" // 勾选框列：文字留给勾选框本身
	case 1:
		return it.Title
	case 2:
		return it.NoticeDate
	}
	return nil
}

// ntMsg 状态行文案（灰色常规 / 红色错误）。
func (a *App) ntMsg(text string, isErr bool) {
	if a.ntMsgL == nil {
		return
	}
	a.ntMsgL.SetText(text)
	if isErr {
		a.ntMsgL.SetTextColor(walk.RGB(190, 30, 30))
	} else {
		a.ntMsgL.SetTextColor(walk.RGB(120, 120, 120))
	}
}

// ntApplyTable 把内存列表刷进表格。
func (a *App) ntApplyTable() {
	if a.ntTable == nil {
		return
	}
	model, ok := a.ntTable.Model().(*ntTableModel)
	if !ok || model == nil {
		model = new(ntTableModel)
	}
	model.reset(a.ntItems)
	a.ntTable.SetModel(model)
}

// ntReadInputs 读取标题/日期输入（本地预校验，避免白跑一趟请求）。
func (a *App) ntReadInputs() (LauncherNotice, error) {
	item := LauncherNotice{Enabled: true}
	if a.ntTitleLE != nil {
		item.Title = strings.TrimSpace(a.ntTitleLE.Text())
	}
	if item.Title == "" {
		return item, fmt.Errorf("公告标题不能为空")
	}
	if utf8.RuneCountInString(item.Title) > noticeMaxTitleRunes {
		return item, fmt.Errorf("公告标题超过 %d 字", noticeMaxTitleRunes)
	}
	if a.ntDateLE != nil {
		raw := strings.TrimSpace(a.ntDateLE.Text())
		if raw != "" {
			var parsed time.Time
			var err error
			for _, layout := range []string{"2006-01-02", "2006-1-2"} {
				if parsed, err = time.ParseInLocation(layout, raw, time.Local); err == nil {
					break
				}
			}
			if err != nil {
				return item, fmt.Errorf("日期格式必须是 YYYY-MM-DD（留空则不显示日期）")
			}
			item.NoticeDate = parsed.Format("2006-01-02")
		}
	}
	return item, nil
}

// onNoticesAdd 把输入区内容追加成一条待保存公告（只改本页列表）。
func (a *App) onNoticesAdd() {
	item, err := a.ntReadInputs()
	if err != nil {
		a.ntMsg("无法添加："+err.Error(), true)
		return
	}
	a.ntItems = append(a.ntItems, item)
	a.ntApplyTable()
	if a.ntTable != nil {
		a.ntTable.SetCurrentIndex(len(a.ntItems) - 1)
	}
	if a.ntTitleLE != nil {
		a.ntTitleLE.SetText("")
	}
	a.ntMsg(fmt.Sprintf("已加入列表（共 %d 条），别忘了点「保存全部到服务器」", len(a.ntItems)), false)
}

// onNoticesUpdate 用输入区覆盖当前选中行（启用状态保持不变）。
func (a *App) onNoticesUpdate() {
	if a.ntTable == nil {
		return
	}
	idx := a.ntTable.CurrentIndex()
	if idx < 0 || idx >= len(a.ntItems) {
		a.ntMsg("请先在表格里选中一行，再点「更新选中行」", true)
		return
	}
	item, err := a.ntReadInputs()
	if err != nil {
		a.ntMsg("无法更新："+err.Error(), true)
		return
	}
	item.ID = a.ntItems[idx].ID
	item.Enabled = a.ntItems[idx].Enabled
	a.ntItems[idx] = item
	a.ntApplyTable()
	if a.ntTable != nil {
		a.ntTable.SetCurrentIndex(idx)
	}
	a.ntMsg(fmt.Sprintf("已更新第 %d 行，别忘了点「保存全部到服务器」", idx+1), false)
}

// onNoticesDelete 删除当前选中行。
func (a *App) onNoticesDelete() {
	if a.ntTable == nil {
		return
	}
	idx := a.ntTable.CurrentIndex()
	if idx < 0 || idx >= len(a.ntItems) {
		a.ntMsg("请先在表格里选中一行，再点「删除选中行」", true)
		return
	}
	a.ntItems = append(a.ntItems[:idx], a.ntItems[idx+1:]...)
	a.ntApplyTable()
	a.ntMsg(fmt.Sprintf("已删除，剩余 %d 条；点「保存全部到服务器」后正式生效", len(a.ntItems)), false)
}

// onNoticesPick 双击表格某行：回填输入区。
func (a *App) onNoticesPick() {
	if a.ntTable == nil {
		return
	}
	idx := a.ntTable.CurrentIndex()
	if idx < 0 || idx >= len(a.ntItems) {
		return
	}
	it := a.ntItems[idx]
	if a.ntTitleLE != nil {
		a.ntTitleLE.SetText(it.Title)
	}
	if a.ntDateLE != nil {
		a.ntDateLE.SetText(it.NoticeDate)
	}
	a.ntMsg(fmt.Sprintf("已回填第 %d 行到输入区（改动后点「更新选中行」）", idx+1), false)
}

// onNoticesRefresh 拉取服务端公告栏并覆盖本页。
func (a *App) onNoticesRefresh(silent bool) {
	c := a.snapshotClient()
	if c == nil {
		if !silent {
			a.showInfo("未连接", "请先登录")
		}
		return
	}
	if a.ntBusy {
		return
	}
	a.ntBusy = true
	go func() {
		defer a.sync(func() { a.ntBusy = false })
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		items, err := c.NoticesGet(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				if !silent {
					a.showError("读取服务器公告栏失败", err)
				}
				a.sync(func() { a.ntMsg("读取失败："+err.Error(), true) })
			}
			return
		}
		a.sync(func() {
			a.ntItems = items
			a.ntLoaded = true
			a.ntApplyTable()
			a.ntMsg(ntSummary(len(items)), false)
		})
	}()
}

// onNoticesSave 整表保存到服务器。
func (a *App) onNoticesSave() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	if a.ntBusy {
		a.showInfo("操作进行中", "正在与服务器通信，请稍候")
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作正在执行，请等它结束后再保存")
		return
	}
	items := append([]LauncherNotice(nil), a.ntItems...)
	enabled := 0
	for _, it := range items {
		if it.Enabled {
			enabled++
		}
	}
	tip := fmt.Sprintf("将把当前 %d 条公告（启用 %d 条）整批写入服务器。\n\n表格里没有的公告等于删除；玩家下次打开登录器即可看到。是否继续？", len(items), enabled)
	if len(items) == 0 {
		// 空表 = 整表清空：单独再确认一次（正常保存流程不受影响）。
		tip += "\n\n⚠ 当前列表是**空的**：确认后服务器上的公告栏会被整表清空，登录器登录页的公告板会整块消失。"
	}
	if !a.confirm("保存服务器公告栏", tip) {
		a.endOp()
		return
	}
	if len(items) == 0 && !a.confirm("清空服务器公告栏？",
		"再确认一次：表格里一条都没有，保存会把服务器上的公告全部删除（登录器登录页的公告板会消失）。\n\n确定要清空吗？") {
		a.endOp()
		return
	}
	a.ntBusy = true
	a.ntMsg("正在保存…", false)
	go func() {
		defer a.sync(func() { a.ntBusy = false; a.endOp() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		saved, err := c.NoticesApply(ctx, items)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存服务器公告栏失败", err)
			}
			a.sync(func() { a.ntMsg("保存失败："+err.Error(), true) })
			return
		}
		a.sync(func() {
			a.ntItems = saved
			a.ntApplyTable()
			a.ntMsg("已保存："+ntSummary(len(saved)), false)
		})
	}()
}

// ntSummary 列表摘要文案。
func ntSummary(n int) string {
	if n == 0 {
		return "服务器上暂无公告（填好标题后点「添加到列表」）"
	}
	return fmt.Sprintf("已加载 %d 条公告", n)
}
