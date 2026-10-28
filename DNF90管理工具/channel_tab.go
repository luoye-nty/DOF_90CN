package main

// DNF90 频道管理页 —— 独立标签页（管理端）。
//
// 数据通道：/manage/v1/channels 与 /manage/v1/channels/apply（服务器端运维工具
// 落盘编辑 DNF90Server 的 channel_info.etc）。
//
// 语义与限制（与服务器端实现严格对应，UI 不粉饰）：
//   - 关闭频道 = 从 channel_info.etc 的 [server] 段移除该行；开启 = 按频道号插回原始行。
//   - 改动**只落盘，需要重启 DNF90Server 才对玩家生效**；重启由服务器端运维工具执行，
//     本页不提供远程重启按钮（避免远程误触踢掉全服玩家），只做状态提示。
//   - 服务器端只有两条硬规则：**至少留一个频道**、**至少留一个特殊区域频道（type 22）开启**。
//   - 默认城镇 11 / 引导 19 自 2026-09-30 起可以关闭（服务端只告警），但客户端登录是自动拨
//     它记住的那个频道号、没有手动入口，关掉后会有一批玩家停在选频道界面，所以本页在
//     确认框里单独提示，不再禁用按钮。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// chanModel 频道表模型（频道号|类型|分组|状态|操作权限）。
type chanModel struct {
	walk.TableModelBase
	items []ChannelEntry
}

func (m *chanModel) RowCount() int { return len(m.items) }

func (m *chanModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	entry := m.items[row]
	switch col {
	case 0:
		return entry.ID
	case 1:
		return entry.Type
	case 2:
		// 名键转中文显示（未收录的名键原样显示，见 channel_name.go）。
		return channelDisplayName(entry.Group)
	case 3:
		if entry.Enabled {
			return "已开启"
		}
		return "已关闭"
	case 4:
		if !entry.Enabled {
			switch {
			case entry.SpecialArea:
				return "可开启（特殊区域）"
			case entry.Required:
				return "可开启（默认/引导）"
			}
			return "可开启"
		}
		switch {
		case entry.SpecialArea:
			// 特殊区域频道（type 22）受"至少留一个开启"保护（服务器端 channelstore 校验）。
			return "可关闭（特殊区域）"
		case entry.Required:
			return "可关闭（默认/引导）"
		}
		return "可关闭"
	}
	return nil
}

// chanTabWidgets 频道管理页控件与状态（只在 GUI 线程读写）。
type chanTabWidgets struct {
	pathL      *walk.Label
	summaryL   *walk.Label
	refreshBtn *walk.PushButton
	table      *walk.TableView
	detailL    *walk.Label
	openBtn    *walk.PushButton
	closeBtn   *walk.PushButton
	openAllBtn *walk.PushButton
	hintL      *walk.Label
	logEdit    *walk.TextEdit

	status ChannelStatus
	rows   []ChannelEntry
}

var chanW chanTabWidgets
var chanMdl chanModel

// buildChannelTab 频道管理 TabPage。
func buildChannelTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "频道管理",
		Content: decl.Composite{Layout: decl.VBox{MarginsZero: true, SpacingZero: true}, Children: a.channelTab()},
	}
}

// channelTab 频道管理页控件树。
func (a *App) channelTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.VBox{Margins: decl.Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []decl.Widget{
				decl.Label{AssignTo: &chanW.pathL, Text: "配置文件：-"},
				// 这两行提示必须短：Label 是单行静态控件，长文本会把主窗口撑宽
				// （2026-09-30 运维工具那边踩过：一句两百多字的提示把窗口撑到 1600+）。
				// 细节放进 ToolTipText，并用 MaxSize 兜住宽度。
				decl.Label{
					Text: "说明：关闭频道 = 从 channel_info.etc 的 [server] 段移除该行；开启 = 按频道号插回原始行。" +
						"落盘后自动请求服务端热生效，仅当服务端不支持热重载时才需要重启。",
					TextColor: walk.RGB(120, 90, 0),
					MaxSize:   decl.Size{Width: 860, Height: 0},
				},
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.PushButton{AssignTo: &chanW.refreshBtn, Text: "刷新频道列表", OnClicked: a.onChanRefresh},
						decl.Label{AssignTo: &chanW.summaryL, Text: "-", MinSize: decl.Size{Width: 300, Height: 0}},
					},
				},
				decl.TableView{
					AssignTo:       &chanW.table,
					MultiSelection: true,
					Columns: []decl.TableViewColumn{
						{Title: "频道号", Width: 80},
						{Title: "类型", Width: 60},
						{Title: "分组", Width: 140},
						{Title: "状态", Width: 80},
						{Title: "操作权限", Width: 190},
					},
					Model:                 new(chanModel),
					MinSize:               decl.Size{Width: 860, Height: 260},
					OnCurrentIndexChanged: a.onChanSelected,
				},
				decl.Label{AssignTo: &chanW.detailL, Text: "未选择频道", MinSize: decl.Size{Width: 860, Height: 0}},
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.PushButton{AssignTo: &chanW.closeBtn, Text: "关闭选中频道", OnClicked: a.onChanCloseSelected},
						decl.PushButton{AssignTo: &chanW.openBtn, Text: "开启选中频道", OnClicked: a.onChanOpenSelected},
						decl.PushButton{AssignTo: &chanW.openAllBtn, Text: "全部开启", OnClicked: a.onChanOpenAll},
					},
				},
				decl.Label{
					AssignTo:  &chanW.hintL,
					Text:      "提示：硬规则只有两条——至少留一个频道、至少留一个特殊区域频道（22 类型）。",
					TextColor: walk.RGB(90, 90, 90),
					MaxSize:   decl.Size{Width: 860, Height: 0},
					ToolTipText: "11（默认城镇）与 19（引导频道）现在也可以关闭（服务端自 2026-09-30 起把" +
						"缺 19 起不来 / 缺 11 拒绝热重载都降级为告警）。\n" +
						"注意：登录是客户端自动拨它记住的那个频道号，没有手动入口——关掉某个频道后，" +
						"上次在那个频道下线的玩家会停在选频道界面。\n" +
						"特殊区域频道 = channel_info.etc 里 type 22 的行（线上为 121 远古 / 171 绝望之塔）。",
				},
				decl.Label{Text: "本页日志："},
				decl.TextEdit{
					AssignTo: &chanW.logEdit, ReadOnly: true, VScroll: true, HScroll: true,
					MinSize: decl.Size{Width: 860, Height: 140},
				},
			},
		},
	}
}

// chanLog 本页日志（只允许在 GUI 线程调用）。
func chanLog(format string, args ...any) {
	if chanW.logEdit == nil {
		return
	}
	chanW.logEdit.AppendText(fmt.Sprintf("[%s] %s\r\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
	acctLogScrollBottom(chanW.logEdit)
}

// chanBusyUI 任务期间禁用/恢复本页按钮。
func (a *App) chanBusyUI(busy bool) {
	for _, btn := range []*walk.PushButton{chanW.refreshBtn, chanW.openBtn, chanW.closeBtn, chanW.openAllBtn} {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
}

// chanRun 本页统一后台任务入口。
func (a *App) chanRun(label string, job func() error, done func(err error)) {
	if !a.beginOp() {
		chanLog("已有管理操作进行中，请稍候")
		return
	}
	chanLog("处理中：%s ...", label)
	a.chanBusyUI(true)
	go func() {
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("操作内部异常: %v", r)
				}
			}()
			err = job()
		}()
		a.sync(func() {
			a.chanBusyUI(false)
			a.endOp()
			if err != nil {
				if a.handleAPIError(err) {
					return
				}
				chanLog("失败：%v", err)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// onChanRefresh 读取频道列表。
func (a *App) onChanRefresh() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	var status ChannelStatus
	a.chanRun("读取频道列表", func() error {
		out, err := c.ChannelsGet(context.Background())
		if err != nil {
			return err
		}
		status = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		a.applyChanStatus(status)
	})
}

// applyChanStatus 把状态刷进控件（GUI 线程）。
func (a *App) applyChanStatus(status ChannelStatus) {
	chanW.status = status
	chanW.rows = status.Channels
	chanMdl.items = status.Channels
	if chanW.table != nil {
		chanW.table.SetModel(&chanMdl)
	}
	if chanW.pathL != nil {
		chanW.pathL.SetText("配置文件：" + status.Path)
	}
	if chanW.summaryL != nil {
		chanW.summaryL.SetText(fmt.Sprintf("共 %d 个频道，启用 %d，关闭 %d ｜ 检查于 %s",
			status.Total, status.Enabled, status.Disabled, status.CheckedAt))
	}
	if chanW.detailL != nil {
		chanW.detailL.SetText("未选择频道")
	}
	chanLog("频道列表已刷新：共 %d 个，启用 %d，关闭 %d", status.Total, status.Enabled, status.Disabled)
}

// onChanSelected 行选中。
func (a *App) onChanSelected() {
	if chanW.table == nil {
		return
	}
	index := chanW.table.CurrentIndex()
	if index < 0 || index >= len(chanW.rows) {
		chanW.detailL.SetText("未选择频道")
		return
	}
	entry := chanW.rows[index]
	state := "已开启"
	if !entry.Enabled {
		state = "已关闭"
	}
	note := ""
	if entry.SpecialArea {
		note += "｜特殊区域频道（22 类型）：必须至少留一个开启，关最后一个会被服务器端拒绝"
	}
	if entry.Required {
		note += "｜默认/引导频道：现在可以关闭；关掉后上次在这个频道下线的玩家会停在选频道界面"
	}
	chanW.detailL.SetText(fmt.Sprintf("已选择：频道 %d（类型 %d，分组 %s，%s）%s", entry.ID, entry.Type, channelDisplayNameWithKey(entry.Group), state, note))
}

// chanSelectedEntries 表格中选中的频道行。
func chanSelectedEntries() []ChannelEntry {
	if chanW.table == nil {
		return nil
	}
	indexes := chanW.table.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := chanW.table.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	rows := make([]ChannelEntry, 0, len(indexes))
	seen := make(map[int]struct{}, len(indexes))
	for _, idx := range indexes {
		if idx < 0 || idx >= len(chanW.rows) {
			continue
		}
		entry := chanW.rows[idx]
		if _, dup := seen[entry.ID]; dup {
			continue
		}
		seen[entry.ID] = struct{}{}
		rows = append(rows, entry)
	}
	return rows
}

// chanIDListText 频道号列表转可读文本。
func chanIDListText(ids []int) string {
	parts := make([]string, 0, len(ids))
	for index, id := range ids {
		if index >= 20 {
			parts = append(parts, fmt.Sprintf("…等 %d 个", len(ids)))
			break
		}
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return strings.Join(parts, ",")
}

// onChanCloseSelected 关闭选中频道。
func (a *App) onChanCloseSelected() {
	rows := chanSelectedEntries()
	if len(rows) == 0 {
		chanLog("请先在频道列表里选中要关闭的频道")
		return
	}
	ids := make([]int, 0, len(rows))
	requires := make([]string, 0, len(rows))
	specials := make([]string, 0, len(rows))
	for _, entry := range rows {
		if !entry.Enabled {
			continue
		}
		if entry.Required {
			requires = append(requires, fmt.Sprintf("%d", entry.ID))
		}
		if entry.SpecialArea {
			specials = append(specials, fmt.Sprintf("%d", entry.ID))
		}
		ids = append(ids, entry.ID)
	}
	if len(ids) == 0 {
		chanLog("选中的频道里没有处于「已开启」的项")
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "将关闭频道 %s（从服务器 channel_info.etc 中移除对应行）。\n\n", chanIDListText(ids))
	notes := make([]string, 0, 2)
	if len(specials) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%s 是特殊区域频道（22 类型）：必须至少留一个开启，如果选中的就是最后一个会被服务器端拒绝",
			strings.Join(specials, "、")))
	}
	if len(requires) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%s 是默认/引导频道：关掉它们之后，上次在这些频道或其它已关闭频道下线的玩家会卡在选频道界面（登录是自动选频道，没有手动入口）",
			strings.Join(requires, "、")))
	}
	if len(notes) > 0 {
		fmt.Fprintf(&body, "注意：%s。\n\n", strings.Join(notes, "；"))
	}
	// 文案口径（2026-09-30 修正）：服务器端运维工具在落盘后会立刻请求热重载，
	// **不需要重启、也不踢在线玩家**。
	body.WriteString("落盘后服务器端会立刻请求热生效（重读频道表 + 对账游戏端口），不需要重启 DNF90Server，" +
		"也不会踢在线玩家：\n" +
		"· 已经站在这些频道里的玩家不受影响，直到他们自己离开；\n" +
		"· 新登录、或重开频道列表的客户端立刻看不到这些频道。\n\n" +
		"（只有热生效失败时，才需要在服务器端重启 DNF90Server。）\n\n确认继续？")
	if !a.confirm("关闭频道确认", body.String()) {
		chanLog("已取消关闭频道")
		return
	}
	a.chanApply(ids, false)
}

// onChanOpenSelected 开启选中频道。
func (a *App) onChanOpenSelected() {
	rows := chanSelectedEntries()
	if len(rows) == 0 {
		chanLog("请先在频道列表里选中要开启的频道")
		return
	}
	ids := make([]int, 0, len(rows))
	for _, entry := range rows {
		if entry.Enabled {
			continue
		}
		ids = append(ids, entry.ID)
	}
	if len(ids) == 0 {
		chanLog("选中的频道都已经处于开启状态")
		return
	}
	a.chanApply(ids, true)
}

// onChanOpenAll 开启全部已关闭频道。
func (a *App) onChanOpenAll() {
	ids := make([]int, 0, len(chanW.rows))
	for _, entry := range chanW.rows {
		if !entry.Enabled {
			ids = append(ids, entry.ID)
		}
	}
	if len(ids) == 0 {
		chanLog("当前没有已关闭的频道")
		return
	}
	if !a.confirm("全部开启确认", fmt.Sprintf("将开启全部 %d 个已关闭频道：%s\n\n确认继续？", len(ids), chanIDListText(ids))) {
		chanLog("已取消全部开启")
		return
	}
	a.chanApply(ids, true)
}

// chanApply 执行一次开关并刷新界面。
func (a *App) chanApply(ids []int, enabled bool) {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	label := "开启频道"
	action := "开启"
	if !enabled {
		label = "关闭频道"
		action = "关闭"
	}
	var result ChannelApplyResult
	a.chanRun(label, func() error {
		out, err := c.ChannelsApply(context.Background(), ids, enabled)
		if err != nil {
			return err
		}
		result = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		a.applyChanStatus(result.Status)
		if len(result.Changed) == 0 {
			chanLog("%s：没有实际变化（目标频道已是该状态）", label)
			return
		}
		chanLog("已%s频道 %s。%s", action, chanIDListText(result.Changed), result.Message)
		switch {
		case result.HotApplied:
			chanLog("热生效成功（无需重启）：新登录/重开的玩家立刻看到新频道表。")
			if result.Reload != nil {
				if len(result.Reload.GamePortsOpened) > 0 {
					chanLog("  服务端新开监听端口：%v", result.Reload.GamePortsOpened)
				}
				if len(result.Reload.GamePortsClosed) > 0 {
					chanLog("  服务端已关监听端口：%v", result.Reload.GamePortsClosed)
				}
				for _, warning := range result.Reload.Warnings {
					chanLog("  热生效告警：%s", warning)
				}
			}
			chanLog("  仍在选频道界面上的玩家需要重开频道列表或重新登录（客户端会缓存目录）。")
		default:
			chanLog("热生效未完成：%s", result.HotApplyError)
			chanLog("改动没有丢失（已写入服务器 channel_info.etc），需要服务器端重启 DNF90Server 后才生效。")
		}
	})
}
