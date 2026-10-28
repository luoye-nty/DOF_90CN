package main

// DNF90 频道管理页 —— 独立标签页（免费版/赞助版共用，无 build tag）。
//
// 做什么：把服务端 channel_info.etc 的 [server] 段列出成一张表，逐行开启/关闭。
// 关闭 = 把该频道行从文件里删掉；开启 = 按频道号升序插回原行文本（原始缩进逐字还原）。
// 因此"关闭"的玩家侧效果是：重启服务端后重登，客户端频道列表里不再出现该频道。
//
// 三条硬保护（由 internal/channelstore 强制，UI 也提前禁用按钮，避免点了才报错）：
//  1. 频道 19（引导频道）不能关——缺了它服务端启动时构建客户端频道表会直接失败；
//  2. 频道 11（默认城镇频道）不能关——登录选频道的回包依赖它；
//  3. 不能把普通城镇频道（type 11）全部关掉，至少要留一个。
//
// 生效方式：只落盘，不热更。页面固定显示"需要重启 DNF90Server"，
// 并提供「重启服务端生效」按钮（复用一键停止/一键启动链路）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"dnf90ops/internal/channelstore"
)

// chanModel 频道表模型（频道号|类型|分组|状态|可否关闭）。
type chanModel struct {
	walk.TableModelBase
	items []channelstore.Entry
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
			// 特殊区域频道（type 22）受"至少留一个开启"保护，见 channelstore.checkDisableTargets。
			return "可关闭（特殊区域）"
		case entry.Required:
			return "可关闭（默认/引导）"
		}
		return "可关闭"
	}
	return nil
}

// chanTabWidgets 频道管理页控件与状态。
type chanTabWidgets struct {
	pathL      *walk.Label
	summaryL   *walk.Label
	refreshBtn *walk.PushButton
	table      *walk.TableView
	detailL    *walk.Label
	openBtn    *walk.PushButton
	closeBtn   *walk.PushButton
	openAllBtn *walk.PushButton
	restartBtn *walk.PushButton
	hintL      *walk.Label

	status channelstore.Status
	rows   []channelstore.Entry
}

var chanW chanTabWidgets
var chanMdl chanModel
var chanApp *App

// channelTab 频道管理页控件树。
func (a *App) channelTab() []Widget {
	chanApp = a
	return []Widget{
		Composite{
			Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []Widget{
				Label{AssignTo: &chanW.pathL, Text: "配置文件：-"},
				Label{
					Text:      "说明：关闭频道 = 从 channel_info.etc 的 [server] 段移除该行；开启 = 按频道号插回原始行。改动需要重启 DNF90Server 才对玩家生效。",
					TextColor: walk.RGB(120, 90, 0),
				},
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						PushButton{AssignTo: &chanW.refreshBtn, Text: "刷新频道列表", OnClicked: a.onChanRefresh},
						Label{AssignTo: &chanW.summaryL, Text: "-", MinSize: Size{Width: 260, Height: 0}},
					},
				},
				TableView{
					AssignTo:       &chanW.table,
					MultiSelection: true,
					Columns: []TableViewColumn{
						{Title: "频道号", Width: 80},
						{Title: "类型", Width: 60},
						{Title: "分组", Width: 140},
						{Title: "状态", Width: 80},
						{Title: "操作权限", Width: 190},
					},
					Model:                 new(chanModel),
					MinSize:               Size{Width: 860, Height: 260},
					OnCurrentIndexChanged: a.onChanSelected,
				},
				Label{AssignTo: &chanW.detailL, Text: "未选择频道", MinSize: Size{Width: 860, Height: 0}},
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						PushButton{AssignTo: &chanW.closeBtn, Text: "关闭选中频道", OnClicked: a.onChanCloseSelected},
						PushButton{AssignTo: &chanW.openBtn, Text: "开启选中频道", OnClicked: a.onChanOpenSelected},
						PushButton{AssignTo: &chanW.openAllBtn, Text: "全部开启", OnClicked: a.onChanOpenAll},
						PushButton{AssignTo: &chanW.restartBtn, Text: "重启服务端生效", OnClicked: a.onChanRestart},
					},
				},
				// 这一行必须短：Label 是单行静态控件，长文本会把整个主窗口撑宽
				// （2026-09-30 踩过：一句两百多字的提示把窗口从 960 撑到 1600+）。
				// 细节放进 ToolTipText，并用 MaxSize 兜住宽度。
				Label{
					AssignTo:  &chanW.hintL,
					Text:      "提示：硬规则只有两条——至少留一个频道、至少留一个特殊区域频道（22 类型）。",
					TextColor: walk.RGB(90, 90, 90),
					MaxSize:   Size{Width: 860, Height: 0},
					ToolTipText: "11（默认城镇）与 19（引导频道）现在也可以关闭（服务端自 2026-09-30 起把" +
						"缺 19 起不来 / 缺 11 拒绝热重载都降级为告警）。\n" +
						"注意：登录是客户端自动拨它记住的那个频道号，没有手动入口——关掉某个频道后，" +
						"上次在那个频道下线的玩家会停在选频道界面（客户端补丁见 " +
						"登录自动选频道_客户端逆向与hook点_20260930.md）。\n" +
						"特殊区域频道 = channel_info.etc 里 type 22 的行（线上为 121 远古 / 171 绝望之塔）。",
				},
			},
		},
	}
}

// chanBusyUI 任务期间禁用/恢复本页按钮。
func (a *App) chanBusyUI(busy bool) {
	for _, btn := range []*walk.PushButton{chanW.refreshBtn, chanW.openBtn, chanW.closeBtn, chanW.openAllBtn, chanW.restartBtn} {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
}

// chanRun 本页统一后台任务入口。
func (a *App) chanRun(label string, job func() error, done func(err error)) {
	if a.busy.Load() {
		a.logf("已有任务进行中，请稍候")
		return
	}
	a.logf("处理中：%s ...", label)
	a.chanBusyUI(true)
	a.runAsync(label, job, func(err error) {
		a.chanBusyUI(false)
		if err != nil {
			a.logf("失败：%v", err)
		}
		if done != nil {
			done(err)
		}
	})
}

// onChanRefresh 读取并展示频道列表。
func (a *App) onChanRefresh() {
	var status channelstore.Status
	a.chanRun("读取频道列表", func() error {
		var err error
		status, err = a.channelsStatus()
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		a.applyChanStatus(status)
	})
}

// applyChanStatus 把状态刷进控件（UI 线程）。
func (a *App) applyChanStatus(status channelstore.Status) {
	chanW.status = status
	chanW.rows = status.Channels
	chanMdl.items = status.Channels
	if chanW.table != nil {
		chanW.table.SetModel(&chanMdl)
	}
	if chanW.pathL != nil {
		chanW.pathL.SetText(fmt.Sprintf("配置文件：%s", status.Path))
	}
	if chanW.summaryL != nil {
		chanW.summaryL.SetText(fmt.Sprintf("共 %d 个频道，启用 %d，关闭 %d ｜ 检查于 %s",
			status.Total, status.Enabled, status.Disabled, status.CheckedAt))
	}
	if chanW.detailL != nil {
		chanW.detailL.SetText("未选择频道")
	}
	if chanW.hintL != nil {
		chanW.hintL.SetText(a.channelRestartHint())
	}
	a.logf("频道列表已刷新：共 %d 个，启用 %d，关闭 %d（%s）", status.Total, status.Enabled, status.Disabled, status.Path)
}

// onChanSelected 行选中：显示明细。
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
		note += "｜特殊区域频道（22 类型）：必须至少留一个开启，关最后一个会被拒绝"
	}
	if entry.Required {
		note += "｜默认/引导频道：现在可以关闭；关掉后上次在这个频道下线的玩家会停在选频道界面"
	}
	chanW.detailL.SetText(fmt.Sprintf("已选择：频道 %d（类型 %d，分组 %s，%s）%s", entry.ID, entry.Type, channelDisplayNameWithKey(entry.Group), state, note))
}

// chanSelectedEntries 返回表格里选中的频道行（多选；单选控件回退到当前行）。
func chanSelectedEntries() []channelstore.Entry {
	if chanW.table == nil {
		return nil
	}
	indexes := chanW.table.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := chanW.table.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	rows := make([]channelstore.Entry, 0, len(indexes))
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

// onChanCloseSelected 关闭选中频道。
func (a *App) onChanCloseSelected() {
	rows := chanSelectedEntries()
	if len(rows) == 0 {
		a.logf("请先在频道列表里选中要关闭的频道")
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
			// 允许关闭，但先记一笔，确认对话框里单独点出来。
			requires = append(requires, fmt.Sprintf("%d", entry.ID))
		}
		if entry.SpecialArea {
			specials = append(specials, fmt.Sprintf("%d", entry.ID))
		}
		ids = append(ids, entry.ID)
	}
	if len(ids) == 0 {
		a.logf("选中的频道里没有处于「已开启」的项")
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "将关闭频道 %s（从 channel_info.etc 中移除对应行）。\n\n", channelIDListText(ids))
	notes := make([]string, 0, 2)
	if len(specials) > 0 {
		notes = append(notes, fmt.Sprintf(
			"%s 是特殊区域频道（22 类型）：必须至少留一个开启，如果选中的就是最后一个会被服务端拒绝",
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
	// 文案口径（2026-09-30 修正）：落盘后工具会立刻请求服务端热重载，
	// **不需要重启、也不踢在线玩家**。原来写"需要重启 DNF90Server 才对玩家生效，
	// 重启会踢下所有在线玩家"是误导 —— 只有服务端没起来或热生效失败时才需要重启。
	body.WriteString("落盘后会立刻请求服务端热生效（重读频道表 + 对账游戏端口），不需要重启 DNF90Server，" +
		"也不会踢在线玩家：\n" +
		"· 已经站在这些频道里的玩家不受影响，直到他们自己离开；\n" +
		"· 新登录、或重开频道列表的客户端立刻看不到这些频道。\n\n" +
		"（只有服务端未运行、或热生效失败时，才需要重启 DNF90Server。）\n\n确认继续？")
	if walk.MsgBox(a.mw, "关闭频道确认", body.String(),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		a.logf("已取消关闭频道")
		return
	}
	a.chanApply(ids, false)
}

// onChanOpenSelected 开启选中频道。
func (a *App) onChanOpenSelected() {
	rows := chanSelectedEntries()
	if len(rows) == 0 {
		a.logf("请先在频道列表里选中要开启的频道")
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
		a.logf("选中的频道都已经处于开启状态")
		return
	}
	a.chanApply(ids, true)
}

// onChanOpenAll 把所有已关闭的频道一次性开启。
func (a *App) onChanOpenAll() {
	ids := make([]int, 0, len(chanW.rows))
	for _, entry := range chanW.rows {
		if !entry.Enabled {
			ids = append(ids, entry.ID)
		}
	}
	if len(ids) == 0 {
		a.logf("当前没有已关闭的频道")
		return
	}
	if walk.MsgBox(a.mw, "全部开启确认",
		fmt.Sprintf("将开启全部 %d 个已关闭频道：%s\n\n确认继续？", len(ids), channelIDListText(ids)),
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		a.logf("已取消全部开启")
		return
	}
	a.chanApply(ids, true)
}

// chanApply 执行一次开关：先落盘，再请求服务端热生效，最后刷新界面。
//
// 顺序是有意的：文件先改（单一事实源，改坏了也能靠备份回滚），
// 然后请求服务端重读。热生效失败不算整次操作失败——改动已经在文件里，
// 服务端下次启动照样生效，只是需要走「重启服务端生效」。
func (a *App) chanApply(ids []int, enabled bool) {
	label := "开启频道"
	if !enabled {
		label = "关闭频道"
	}
	var status channelstore.Status
	var changed []int
	var hotResult acctGMTChannelReload
	var hotErr error
	a.chanRun(label, func() error {
		var err error
		changed, status, err = a.channelsApply(ids, enabled)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			return nil
		}
		// 只有真的改了文件才值得让服务端重读一遍。
		hotResult, hotErr = a.chanHotReload(context.Background())
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		a.applyChanStatus(status)
		if len(changed) == 0 {
			a.logf("%s：没有实际变化（目标频道已是该状态）", label)
			return
		}
		action := "关闭"
		if enabled {
			action = "开启"
		}
		a.logf("已%s频道 %s（channel_info.etc 已落盘）", action, channelIDListText(changed))
		switch {
		case hotErr == nil:
			a.logf("热生效成功：%s", chanHotReloadSummary(hotResult))
			for _, warning := range hotResult.Warnings {
				a.logf("热生效告警：%s", warning)
			}
		default:
			a.logf("热生效失败：%v", hotErr)
			a.logf("改动没有丢失（文件已落盘），可点「重启服务端生效」让它立刻生效，或等下次服务端启动自动生效。")
		}
	})
}

// onChanRestart 重启服务端让频道改动生效。
func (a *App) onChanRestart() {
	if walk.MsgBox(a.mw, "重启服务端",
		"将执行一次「停止服务 → 重新启动服务」来让频道改动生效。\n\n重启期间所有在线玩家会掉线，玩家需要重登才能在频道列表里看到最新频道。\n\n确认继续？",
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		a.logf("已取消重启服务端")
		return
	}
	a.chanRun("重启服务端使频道生效", a.restartServerForChannels, func(err error) {
		if err != nil {
			return
		}
		a.logf("服务端已重启，频道改动已生效；玩家重登后即可看到最新频道列表")
		a.onChanRefresh()
	})
}
