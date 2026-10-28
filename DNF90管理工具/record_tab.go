package main

// DNF90 记录查询页 —— 独立标签页（管理工具专属）。
//
// 数据通道：/manage/v1/records/*（服务器端运维工具的 manage 服务转发到 DNF90Server
// 的 GM Tool 接口 /dnf/gm/records/*）。记录本体是服务端进程写的 JSONL 文件，
// 见 go-server internal/services/dnfbridge/activity_record.go。
//
// 六类记录（与服务端 go-server/activity_record.go 的 recordKinds 一一对应）：
//	点券消费记录（cera）        ｜ 商城购买（含被拒绝的尝试）与金库升级扣点券
//	对话记录（chat）            ｜ 普通/区域/队伍/公会/攻坚队/私聊/喇叭，含被拒绝的发言
//	副本记录（dungeon）         ｜ 进入 / 通关 / 离开（含退出原因与击杀统计）
//	团本记录（raid）            ｜ 攻坚创建/开始/阶段推进/加载屏障/奖励发放等（raid_log.go）
//	金币寄售记录（gold_consign）｜ 上架 / 下架 / 买入 / 成交扣手续费 / 过期退回
//	拍卖行记录（auction）       ｜ 上架(含押金) / 下架 / 出价 / 被超越 / 一口价 / 成交 / 领取 / 过期
//
// 类别清单是**硬编码在四处的**（服务端 recordKinds/recordKindTitles、运维工具白名单、
// 本文件的 recKindLabels/recKindValues、api_client.go 的 RecordKind* 常量），
// 加新类别四处都要改 —— 团本记录当初就是漏改了运维工具白名单，整整一类记录在界面上选不到。
//
// 页面结构：
//	① 条件条：类别 + 条数 + 角色ID/角色名/账号ID + 关键字 + 起止时间
//	② 操作条：查询 / 记录状态 / 导出CSV
//	③ 记录表：时间 | 角色 | 角色ID | 账号 | 频道 | 概要 | 结果
//	④ 详情区：选中行的完整字段（JSON 展开，逐字段一行）
//	⑤ 本页日志
//
// 为什么表格只放几列、详情放全部：三类记录的字段集合不一样（而且服务端以后还会加），
// 表格列固定下来必然要丢字段；"信息尽可能详细"这条要求由详情区与 CSV 导出满足。

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// recKindLabels / recKindValues 类别下拉的显示名与执行端取值。
//
// ⚠️ 两个数组必须**一一对应、只能往后追加**：下拉的 CurrentIndex 直接当数组下标用
// （见 recCurrentKind），中间插一项会把"对话记录"这类默认选中项错位到别的类别上。
var recKindLabels = []string{"点券消费记录", "对话记录", "副本记录", "团本记录", "金币寄售记录", "拍卖行记录", "场景加载诊断"}
var recKindValues = []string{
	RecordKindCera, RecordKindChat, RecordKindDungeon,
	RecordKindRaid, RecordKindGoldConsign, RecordKindAuction, RecordKindScene,
}

// 说明：时间范围已改成"日期 + 时/分下拉"，不再需要文本格式白名单；
// 发出去的仍然是服务端认识的 "2006-01-02 15:04:05"。

const (
	// recPageSize 每次请求向服务端要多少条。服务端单次上限 2000，这里取 1000：
	// 单页响应稳定在几百 KB，远低于各层读上限（转发端 8MB、本工具 16MB）。
	recPageSize = 1000
	// recDefaultLoad "最多加载"输入框的默认值。
	//
	// 这个值**不能**写进 declarative 字面量的 Value 字段：declarative.NumberEdit.Value
	// 是 Property，最终由 walk 的 "Value" 属性 setter 处理成
	// SetValue(assertFloat64Or(v, 0.0))，而 assertFloat64Or 只认 float64 ——
	// 传 int 会退化成 0.0，SetValue(0) 又落在 [MinValue, MaxValue] 之外，于是
	// Create() 直接报 "value out of range"；windowsgui 构建里那句 log.Fatalf
	// 没有控制台可打，表现为"登录后主界面不出现"（实测踩到）。
	//
	// 也不能只靠 SetRange 兜底：NumberEdit 建出来是 0，SetRange(min,max) 会把越界值
	// 夹成 min，于是 MinValue=1 ⇒ 控件值 1 ⇒ 查询只回 1 条（同样实测踩到）。
	//
	// 所以默认值由 applyRecordTabDefaults() 在建窗之后显式 SetValue 设置，
	// 并由 mainwindow_test.go 断言住。
	recDefaultLoad = 1000
	// recMaxLoad 单次查询最多加载多少条（内存与表格可控）。
	recMaxLoad = 20000
	// recMaxPages 翻页次数上限（防"对端忽略 offset"时无限循环）。
	recMaxPages = 64
)

// recFetchResult 一次"查到底"的结果。
type recFetchResult struct {
	rows  []recRow
	pages int
	// page 是最后一页（用于把 dir / retention / title 填到汇总行）。
	page *RecordPage
	// more 表示服务端还有更早的记录没取完（达到本地条数上限或翻页次数到顶）。
	more bool
	// pagingUnsupported 表示对端不支持翻页（服务端或转发端版本较旧）。
	pagingUnsupported bool
}

// recRow 表格里的一行（把 map 记录投影成固定列）。
type recRow struct {
	ts          string
	character   string
	charID      string
	accountID   string // 记录里的账号主键（dnf:<sha256(用户名)>）
	accountName string // 运维工具翻译出的真实账号名；空=翻译不到，显示时退回主键
	channel     string
	summary     string
	// clearTime 通关用时（只有副本记录有；已格式化成"3 分 12 秒（192 秒）"）。
	clearTime   string
	result      string
	record      map[string]any
	rawJSONLine string
}

// recTableModel 记录表模型。
type recTableModel struct {
	walk.TableModelBase
	rows []recRow
}

func (m *recTableModel) RowCount() int { return len(m.rows) }

func (m *recTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	item := m.rows[row]
	switch col {
	case 0:
		return item.ts
	case 1:
		return item.character
	case 2:
		return item.charID
	case 3:
		// 账号列显示真实账号名；翻译不到时退回原始主键（不让它变成空白）。
		if item.accountName != "" {
			return item.accountName
		}
		return item.accountID
	case 4:
		return item.channel
	case 5:
		return item.summary
	case 6:
		// 通关时间：只有副本记录有（其它类别返回空串，列里就是空白，不显示"—"之外的东西）。
		return item.clearTime
	case 7:
		return item.result
	}
	return nil
}

// recTabWidgets 记录查询页控件与状态（全部只在 GUI 线程读写）。
type recTabWidgets struct {
	kindCb    *walk.ComboBox
	limitEdit *walk.NumberEdit
	charIDEd  *walk.LineEdit
	charNameE *walk.LineEdit
	accountEd *walk.LineEdit
	keywordEd *walk.LineEdit

	// 时间范围：勾选"按时间过滤"后才生效；起止 = 日期 + 时/分下拉（0-23 / 0-59）。
	timeChk     *walk.CheckBox
	sinceDate   *walk.DateEdit
	sinceHourCb *walk.ComboBox
	sinceMinCb  *walk.ComboBox
	untilDate   *walk.DateEdit
	untilHourCb *walk.ComboBox
	untilMinCb  *walk.ComboBox

	queryBtn *walk.PushButton
	metaBtn  *walk.PushButton
	exportBt *walk.PushButton

	table    *walk.TableView
	sumL     *walk.Label
	detailEd *walk.TextEdit
	logEdit  *walk.TextEdit

	page    *RecordPage
	rows    []recRow
	lastUse time.Time
}

var recW recTabWidgets
var recMdl recTableModel

// buildRecordTab 记录查询 TabPage。
func buildRecordTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "记录查询",
		Content: decl.Composite{Layout: decl.VBox{MarginsZero: true, SpacingZero: true}, Children: a.recordTab()},
	}
}

// recordTab 记录查询页控件树。
func (a *App) recordTab() []decl.Widget {
	return []decl.Widget{
		decl.ScrollView{
			HorizontalFixed: true,
			Layout:          decl.VBox{Margins: decl.Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []decl.Widget{
				// ① 条件条
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "类别:"},
						decl.ComboBox{AssignTo: &recW.kindCb, Model: recKindLabels, CurrentIndex: 1, MinSize: decl.Size{Width: 120, Height: 0}},
						decl.Label{Text: "最多加载:"},
						decl.NumberEdit{
							AssignTo: &recW.limitEdit,
							// 这里**故意不写 Value**：declarative 的 Value 走
							// assertFloat64Or（只认 float64），传 int 会退化成 0.0 并让
							// SetValue 越界 → Create() 失败、主窗口起不来。
							// 默认值由 applyRecordTabDefaults() 建窗后显式设置。
							MinValue: 1, MaxValue: recMaxLoad,
							Decimals: 0, Increment: 100, SpinButtonsVisible: true, MinSize: decl.Size{Width: 90, Height: 0},
							ToolTipText: fmt.Sprintf("本次查询最多加载多少条（1~%d，默认 %d）。超过 1000 条会自动分页向服务端多次取，取满为止。", recMaxLoad, recDefaultLoad),
						},
						decl.Label{Text: "角色名:"},
						decl.LineEdit{AssignTo: &recW.charNameE, MinSize: decl.Size{Width: 110, Height: 0}},
						decl.Label{Text: "角色ID:"},
						decl.LineEdit{AssignTo: &recW.charIDEd, MinSize: decl.Size{Width: 70, Height: 0}},
						decl.Label{Text: "账号ID/用户名:"},
						decl.LineEdit{AssignTo: &recW.accountEd, MinSize: decl.Size{Width: 110, Height: 0}, ToolTipText: "填账号ID（dnf:…）或直接填登录用户名都可以：服务端会把用户名解析成账号主键"},
					},
				},
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "关键字:"},
						decl.LineEdit{AssignTo: &recW.keywordEd, MinSize: decl.Size{Width: 180, Height: 0}, ToolTipText: "在整条记录的 JSON 里做包含匹配（不区分大小写）"},
						decl.PushButton{AssignTo: &recW.queryBtn, Text: "查询", OnClicked: a.onRecQuery},
						decl.PushButton{AssignTo: &recW.metaBtn, Text: "记录状态", OnClicked: a.onRecMeta, ToolTipText: "查看记录目录、文件清单与类别可用性"},
						decl.PushButton{AssignTo: &recW.exportBt, Text: "导出CSV", OnClicked: a.onRecExportCSV, ToolTipText: "把当前表格里已加载的记录逐字段导出为 CSV（UTF-8 带 BOM，Excel 可直接打开）"},
					},
				},
				// 时间范围：日期 + 时/分下拉；不勾选=不限时间（与改动前行为一致）。
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.CheckBox{
							AssignTo:         &recW.timeChk,
							Text:             "按时间过滤",
							OnCheckedChanged: func() { recSyncTimeEnabled() },
							ToolTipText:      "不勾选=查全部时间（默认）。勾选后按下面的起止时间过滤。",
						},
						decl.Label{Text: "起:"},
						decl.DateEdit{AssignTo: &recW.sinceDate, Format: "2006-01-02", MinSize: decl.Size{Width: 110, Height: 0}},
						decl.ComboBox{AssignTo: &recW.sinceHourCb, Model: timeHourLabels, MinSize: decl.Size{Width: 55, Height: 0}},
						decl.Label{Text: "时"},
						decl.ComboBox{AssignTo: &recW.sinceMinCb, Model: timeMinuteLabels, MinSize: decl.Size{Width: 55, Height: 0}},
						decl.Label{Text: "分"},
						decl.Label{Text: "止:"},
						decl.DateEdit{AssignTo: &recW.untilDate, Format: "2006-01-02", MinSize: decl.Size{Width: 110, Height: 0}},
						decl.ComboBox{AssignTo: &recW.untilHourCb, Model: timeHourLabels, MinSize: decl.Size{Width: 55, Height: 0}},
						decl.Label{Text: "时"},
						decl.ComboBox{AssignTo: &recW.untilMinCb, Model: timeMinuteLabels, MinSize: decl.Size{Width: 55, Height: 0}},
						decl.Label{Text: "分"},
						decl.Label{Text: "（止时间含该分钟）"},
					},
				},
				decl.Label{AssignTo: &recW.sumL, Text: "未查询", MinSize: decl.Size{Width: 880, Height: 0}},
				// ③ 记录表
				decl.TableView{
					AssignTo: &recW.table,
					Columns: []decl.TableViewColumn{
						{Title: "时间", Width: 160},
						{Title: "角色", Width: 120},
						{Title: "角色ID", Width: 70},
						{Title: "账号", Width: 150},
						{Title: "频道", Width: 60},
						{Title: "概要", Width: 300},
						// 「通关时间」：只有副本记录有值（服务端按进本→Boss 结算的秒数）。
						// 独立成列是为了能一眼横向比较（概要列里那句话被截断时看不到用时）。
						{Title: "通关时间", Width: 100},
						{Title: "结果", Width: 120},
					},
					Model:                 new(recTableModel),
					MinSize:               decl.Size{Width: 880, Height: 220},
					OnCurrentIndexChanged: a.onRecRowSelected,
				},
				// ④ 详情区
				decl.Label{Text: "选中记录的完整字段（服务端原始记录，含类别专属字段）："},
				decl.TextEdit{
					AssignTo: &recW.detailEd, ReadOnly: true, VScroll: true, HScroll: true,
					MinSize: decl.Size{Width: 880, Height: 180},
				},
				decl.Label{
					Text:      "说明：记录由 DNF90Server 进程写成本地 JSONL 文件；查询只读，不改动任何运行态。被拒绝的操作（发言被拦、点券不足、购买超限）同样会留下记录，result 字段区分 ok / rejected。",
					TextColor: walk.RGB(90, 90, 90),
				},
				// ⑤ 本页日志
				decl.Label{Text: "本页日志："},
				decl.TextEdit{
					AssignTo: &recW.logEdit, ReadOnly: true, VScroll: true, HScroll: true,
					MinSize: decl.Size{Width: 880, Height: 120},
				},
			},
		},
	}
}

// ================= 本页日志 =================

func recLog(format string, args ...any) {
	if recW.logEdit == nil {
		return
	}
	recW.logEdit.AppendText(fmt.Sprintf("[%s] %s\r\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
	acctLogScrollBottom(recW.logEdit)
}

// ================= 字段投影 =================

// recString 取记录里的字符串字段；数字/布尔也转成字符串显示。
func recString(entry map[string]any, key string) string {
	value, ok := entry[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// recKindOf 判断一行记录属于哪一类：优先用服务端的 kind 字段，取不到时用查询类别兜底。
func recKindOf(entry map[string]any, fallback string) string {
	if kind := recString(entry, "kind"); kind != "" {
		return kind
	}
	return fallback
}

// recSummary 一行记录的"概要"列：按类别挑最有用的一两个字段。
func recSummary(kind string, entry map[string]any) string {
	switch kind {
	case RecordKindChat:
		mode := recString(entry, "mode_name")
		message := recString(entry, "message")
		scope := recString(entry, "scope")
		parts := make([]string, 0, 3)
		if mode != "" {
			parts = append(parts, "["+mode+"]")
		}
		if target := recString(entry, "target_name"); target != "" {
			parts = append(parts, "→"+target)
		}
		if message != "" {
			parts = append(parts, message)
		}
		if scope != "" {
			parts = append(parts, "("+scope+")")
		}
		return strings.Join(parts, " ")
	case RecordKindCera:
		parts := make([]string, 0, 5)
		if name := recString(entry, "payment_name"); name != "" {
			parts = append(parts, name)
		}
		// 实扣才是"真正花掉多少"；标价合计（一组价×数量）不同时并列显示，避免被误当成消费额。
		if spent := recString(entry, "cera_spent"); spent != "" {
			parts = append(parts, "实扣 "+spent+" 点券")
			if total := recString(entry, "cera_total_price"); total != "" && total != spent {
				parts = append(parts, "（商品标价合计 "+total+"）")
			}
		}
		if after := recString(entry, "cera_after"); after != "" {
			parts = append(parts, "余额 "+after)
		}
		if note := recString(entry, "cera_spent_note"); note != "" {
			parts = append(parts, note)
		}
		if granted, ok := entry["granted"].([]any); ok && len(granted) > 0 {
			parts = append(parts, fmt.Sprintf("发货 %d 项", len(granted)))
		}
		return strings.Join(parts, " ")
	case RecordKindDungeon:
		parts := make([]string, 0, 5)
		// 合并行（一局副本一行）不再显示"离开副本"这类单一事件名 —— 它是三条记录合成的，
		// 打头的事件名会让人以为这行只是"离开"那一条。
		if recString(entry, recMergedEventsField) == "" {
			if name := recString(entry, "event_name"); name != "" {
				parts = append(parts, name)
			}
		} else {
			parts = append(parts, "一次副本")
		}
		if dungeon := recString(entry, "dungeon_name"); dungeon != "" {
			parts = append(parts, dungeon)
		} else if id := recString(entry, "dungeon_id"); id != "" {
			parts = append(parts, "副本 "+id)
		}
		if difficulty := recString(entry, "difficulty"); difficulty != "" {
			parts = append(parts, "难度 "+difficulty)
		}
		if elapsed := recString(entry, "elapsed_seconds"); elapsed != "" {
			// 通关用时：从进副本（started_at）到这条 clear 记录落盘（Boss 结算）的秒数。
			if seconds, ok := parseRecSeconds(elapsed); ok {
				parts = append(parts, "通关用时 "+recDurationText(seconds))
			} else {
				parts = append(parts, "通关用时 "+elapsed+" 秒")
			}
		}
		if reason := recString(entry, "exit_reason"); reason != "" {
			parts = append(parts, "("+reason+")")
		}
		return strings.Join(parts, " ")
	case RecordKindRaid:
		// 团本记录：event_name 已经是最有信息量的一列（服务端 raid_log.go 把"谁在等谁、
		// 名单/就绪人数、驱动者、失败原因"都编进了事件名），这里只补几个一眼能用的数字。
		parts := make([]string, 0, 4)
		if name := recString(entry, "event_name"); name != "" {
			parts = append(parts, name)
		}
		if online := recString(entry, "online_members"); online != "" {
			if roster := recString(entry, "roster_members"); roster != "" {
				parts = append(parts, fmt.Sprintf("在线 %s/%s", online, roster))
			} else {
				parts = append(parts, "在线 "+online)
			}
		}
		if wall := recString(entry, "wall_ms"); wall != "" {
			if ms, err := strconv.ParseFloat(wall, 64); err == nil {
				parts = append(parts, "耗时 "+recDurationText(ms/1000))
			}
		}
		if reason := recString(entry, "reason"); reason != "" {
			parts = append(parts, "("+reason+")")
		}
		return strings.Join(parts, " ")
	case RecordKindGoldConsign:
		// 金币寄售：运营最关心"卖了多少金币、扣/得多少代币券、手续费抽了多少"。
		parts := make([]string, 0, 6)
		if name := recString(entry, "event_name"); name != "" {
			parts = append(parts, name)
		}
		if gold := recString(entry, "gold_amount"); gold != "" {
			parts = append(parts, gold+" 金币")
		}
		if price := recString(entry, "price_cera"); price != "" {
			parts = append(parts, "售价 "+price+" 代币券")
		} else if spent := recString(entry, "cera_spent"); spent != "" {
			parts = append(parts, "实扣 "+spent+" 代币券")
		}
		if fee := recString(entry, "fee_cera"); fee != "" {
			if percent := recString(entry, "fee_percent"); percent != "" {
				parts = append(parts, fmt.Sprintf("手续费 %s（%s%%）", fee, percent))
			} else {
				parts = append(parts, "手续费 "+fee)
			}
		}
		if proceeds := recString(entry, "proceeds_cera"); proceeds != "" {
			parts = append(parts, "卖家实得 "+proceeds)
		}
		if listing := recString(entry, "listing_id"); listing != "" {
			parts = append(parts, "单号 "+listing)
		}
		return strings.Join(parts, " ")
	case RecordKindAuction:
		// 拍卖行：物品 + 金额（成交价 > 出价 > 一口价 > 起拍价，只显示最先命中的那个）+ 手续费。
		parts := make([]string, 0, 6)
		if name := recString(entry, "event_name"); name != "" {
			parts = append(parts, name)
		}
		if item := recString(entry, "item_name"); item != "" {
			if count := recString(entry, "item_count"); count != "" {
				parts = append(parts, item+" ×"+count)
			} else {
				parts = append(parts, item)
			}
		} else if id := recString(entry, "item_id"); id != "" {
			parts = append(parts, "道具 "+id)
		}
		if price := recString(entry, "sale_price"); price != "" {
			parts = append(parts, "成交 "+price+" 金币")
		} else if bid := recString(entry, "bid_total"); bid != "" {
			parts = append(parts, "出价 "+bid+" 金币")
		} else if buyout := recString(entry, "buyout_price"); buyout != "" {
			parts = append(parts, "一口价 "+buyout+" 金币")
		} else if start := recString(entry, "start_price"); start != "" {
			parts = append(parts, "起拍 "+start+" 金币")
		}
		if fee := recString(entry, "fee_gold"); fee != "" {
			if percent := recString(entry, "fee_percent"); percent != "" {
				parts = append(parts, fmt.Sprintf("手续费 %s（%s%%）", fee, percent))
			} else {
				parts = append(parts, "手续费 "+fee)
			}
		}
		if proceeds := recString(entry, "seller_proceeds"); proceeds != "" {
			parts = append(parts, "卖家实得 "+proceeds)
		}
		if listing := recString(entry, "listing_id"); listing != "" {
			parts = append(parts, "单号 "+listing)
		}
		return strings.Join(parts, " ")
	case RecordKindScene:
		// 场景加载诊断：一行说清"在哪一步、为什么被推迟 / 还是已经发出去了"。
		// defer_reason 是给排查用的英文标识（很长的状态机判断名），原样显示不去翻译 ——
		// 翻译成中文反而对不上代码里的分支名。
		parts := make([]string, 0, 4)
		if name := recString(entry, "event_name"); name != "" {
			parts = append(parts, name)
		}
		if reason := recString(entry, "defer_reason"); reason != "" {
			parts = append(parts, "原因 "+reason)
		}
		if src := recString(entry, "source"); src != "" {
			parts = append(parts, "来自 "+src)
		}
		if count := recString(entry, "packet_count"); count != "" {
			parts = append(parts, "共 "+count+" 包")
		}
		return strings.Join(parts, " ")
	default:
		return recString(entry, "event_name")
	}
}

// recResultText "结果"列：ok/rejected（被拒绝时补原因）；副本记录按通关与否显示中文。
func recResultText(entry map[string]any) string {
	// 副本记录：cleared 布尔最直观（合并行同样适用）。
	if cleared, ok := entry["cleared"].(bool); ok {
		text := "未通关"
		if cleared {
			text = "通关"
		}
		if reason := recString(entry, "exit_reason"); reason != "" {
			// exit_reason 是服务端的机器可读值（town-return 之类），原样附在后面便于核对。
			return text + "（" + reason + "）"
		}
		return text
	}

	result := recString(entry, "result")
	if result == "" {
		return ""
	}
	if result == "ok" {
		return "成功"
	}
	if reason := recString(entry, "reason"); reason != "" {
		return recResultLabel(result) + "：" + recReasonShort(reason)
	}
	return recResultLabel(result)
}

// recResultLabel 结果值的中文（未收录的原样返回）。
func recResultLabel(result string) string {
	switch result {
	case "rejected":
		return "被拒绝"
	case "ok":
		return "成功"
	}
	return result
}

// recReasonShort 原因串可能很长（例如带 PVF 路径），表格里只显示前 60 个字符，
// 完整原因在详情区与 CSV 里。
func recReasonShort(reason string) string {
	runes := []rune(reason)
	if len(runes) <= 60 {
		return reason
	}
	return string(runes[:60]) + "…"
}

// recLocalTime 记录里的 ts 是 RFC3339Nano（UTC 或本地带时区），表格里统一显示本地时间。
func recLocalTime(stamp string) string {
	if stamp == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}
	return parsed.Local().Format("2006-01-02 15:04:05.000")
}

// buildRecRows 把服务端记录投影成表格行。
//
// 副本类先做「三合一」：服务端一局副本会写进入/通关/离开三条，
// 这里按 started_at(+lifecycle_token) 合并成一行（见 record_merge.go），
// 账号列同时给出运维工具翻译好的真实账号名。
func buildRecRows(page *RecordPage, fallbackKind string) []recRow {
	if page == nil {
		return nil
	}
	return buildRecRowsFrom(page.Records, page.AccountNames, fallbackKind)
}

// buildRecRowsFrom 把一批记录投影成表格行（副本先三合一）。
func buildRecRowsFrom(records []map[string]any, accountNames map[string]string, fallbackKind string) []recRow {
	entries := records
	if recordKindIsDungeon(fallbackKind) {
		entries = mergeDungeonRecords(entries)
	}
	rows := make([]recRow, 0, len(entries))
	for _, entry := range entries {
		kind := recKindOf(entry, fallbackKind)
		channel := recString(entry, "channel_id")
		accountID := recString(entry, "account_id")
		rows = append(rows, recRow{
			ts:          recLocalTime(recString(entry, "ts")),
			character:   recString(entry, "character_name"),
			charID:      recString(entry, "character_id"),
			accountID:   accountID,
			accountName: recAccountName(accountID, accountNames),
			channel:     channel,
			summary:     recSummary(kind, entry),
			clearTime:   recClearTimeText(entry),
			result:      recResultText(entry),
			record:      entry,
		})
	}
	return rows
}

// recClearTimeText 表格「通关时间」列的取值：副本记录的通关用时（进本 → Boss 结算）。
//
// 只有副本记录有这个字段（`elapsed_seconds`，由服务端在通关那条记录上写），
// 其它类别返回空串 —— 列里就是空白，不去编一个"—"出来占地方。
func recClearTimeText(entry map[string]any) string {
	raw := recString(entry, "elapsed_seconds")
	if raw == "" {
		return ""
	}
	if seconds, ok := parseRecSeconds(raw); ok {
		return recDurationText(seconds)
	}
	return raw + " 秒"
}

// recAccountName 取账号主键对应的真实账号名（翻译不到返回空串，由调用方退回显示主键）。
func recAccountName(accountID string, names map[string]string) string {
	if accountID == "" || len(names) == 0 {
		return ""
	}
	return strings.TrimSpace(names[accountID])
}

// recDetailText 详情区文本：逐字段排序展开，字段名走汉化字典，
// 账号主键一律翻成真实账号名（翻不到才回退主键并标注），秒数换成"X 分 Y 秒"。
func recDetailText(entry map[string]any, accountNames map[string]string) string {
	if len(entry) == 0 {
		return "（未选择记录）"
	}
	keys := make([]string, 0, len(entry))
	for key := range entry {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		value := entry[key]
		label := recFieldLabel(key)
		// 多行文本（例如喇叭/私聊正文）原样展开更可读；结构化字段（数组/对象）走 JSON。
		if text, ok := value.(string); ok {
			fmt.Fprintf(&builder, "%s = %s\r\n", label, recFieldValueText(key, text, accountNames))
			continue
		}
		if isPlainScalar(value) {
			fmt.Fprintf(&builder, "%s = %s\r\n", label, recFieldValueText(key, value, accountNames))
			continue
		}
		fmt.Fprintf(&builder, "%s = %s\r\n", label, recJSONValueText(value, accountNames))
	}
	return builder.String()
}

// isPlainScalar 判断是不是"直接能读"的标量（数字/布尔），数组与对象要走 JSON 展开。
func isPlainScalar(value any) bool {
	switch value.(type) {
	case bool, float64, int, int64, nil:
		return true
	}
	return false
}

// recJSONValueText 数组/对象：JSON 展开，并把里面的账号主键与字段名一并汉化
// （granted[]、request_items[] 这些子结构里也有 account_id/价格字段）。
func recJSONValueText(value any, accountNames map[string]string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	text := string(encoded)
	// 子结构里的账号主键：先整体替换成"账号名(主键)"，避免在 JSON 里看到一串 sha256。
	for id, name := range accountNames {
		if strings.TrimSpace(name) == "" {
			continue
		}
		text = strings.ReplaceAll(text, `"`+id+`"`, fmt.Sprintf(`"%s"`, name))
	}
	return text
}

// ================= 交互 =================

// recBusyUI 任务期间禁用/恢复本页按钮（GUI 线程）。
func (a *App) recBusyUI(busy bool) {
	for _, btn := range []*walk.PushButton{recW.queryBtn, recW.metaBtn, recW.exportBt} {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
}

// recRun 本页统一后台任务入口：opMu 互斥 + 后台 goroutine + GUI 线程回调。
func (a *App) recRun(label string, job func() error, done func(err error)) {
	if !a.beginOp() {
		recLog("已有管理操作进行中，请稍候")
		return
	}
	recLog("处理中：%s ...", label)
	a.recBusyUI(true)
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
			a.recBusyUI(false)
			a.endOp()
			if err != nil {
				if a.handleAPIError(err) {
					return
				}
				recLog("失败：%v", err)
				if recIsMissingEndpoint(err) {
					recLog("提示：当前连接的运维工具没有记录查询端点（/manage/v1/records/*）。远程管理服务只在赞助版运维工具里提供，请改连赞助版。")
				}
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// recIsMissingEndpoint 判断错误是否为"对端没有这个端点"（404）。
//
// 记录查询只在赞助版运维工具的远程管理服务里注册；免费版没有 manage 服务，
// 管理工具连上去会拿到 404。这种情况下给一句明确的提示，而不是让用户对着
// "not-found (HTTP 404)" 猜。
func recIsMissingEndpoint(err error) bool {
	var apie *APIError
	if ok := asAPIError(err, &apie); ok && apie != nil {
		return apie.Status == 404
	}
	return false
}

// recCurrentKind 当前选中的类别（越界时回落到对话记录）。
func recCurrentKind() string {
	idx := 0
	if recW.kindCb != nil {
		idx = recW.kindCb.CurrentIndex()
	}
	if idx < 0 || idx >= len(recKindValues) {
		return RecordKindChat
	}
	return recKindValues[idx]
}

// recClient 取已登录客户端（未登录时提示并返回 nil）。
func (a *App) recClient() *APIClient {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return nil
	}
	return c
}

// recTimeRange 读时间范围（勾了"按时间过滤"才生效）。
//
// 起 = 该分钟的 0 秒，止 = 该分钟的 59 秒：止时间含整分钟，符合"查到 HH:MM 为止"的直觉，
// 不会出现"选了 23:59 却查不到 23:59:30 那条"的坑。服务端接受 "2006-01-02 15:04:05"。
func recTimeRange() (since, until string) {
	if recW.timeChk == nil || !recW.timeChk.Checked() {
		return "", ""
	}
	compose := func(date *walk.DateEdit, hourCb, minuteCb *walk.ComboBox, endOfMinute bool) string {
		if date == nil {
			return ""
		}
		clock := clockComboText(hourCb, minuteCb) // 已保证是合法 "HH:MM"
		second := ":00"
		if endOfMinute {
			second = ":59"
		}
		return date.Date().Format("2006-01-02") + " " + clock + second
	}
	return compose(recW.sinceDate, recW.sinceHourCb, recW.sinceMinCb, false),
		compose(recW.untilDate, recW.untilHourCb, recW.untilMinCb, true)
}

// recSyncTimeEnabled 勾选框联动：不勾选时把起止控件置灰，避免让人以为改了会生效。
func recSyncTimeEnabled() {
	enabled := recW.timeChk != nil && recW.timeChk.Checked()
	for _, widget := range []walk.Widget{
		recW.sinceDate, recW.sinceHourCb, recW.sinceMinCb,
		recW.untilDate, recW.untilHourCb, recW.untilMinCb,
	} {
		if widget != nil {
			widget.SetEnabled(enabled)
		}
	}
}

// applyRecordTabDefaults 建窗之后把记录页的默认值补上（必须由 GUI 线程调用）。
//
// 为什么必须显式设置、而不是写进 declarative 字面量：见 recDefaultLoad 的注释 ——
// declarative 的 Value 传 int 会退化成 0.0，让 SetValue 越界、Create() 失败；
// 而"不写 Value"又会让控件停在 MinValue=1，查询只回 1 条。两头都是坑，
// 所以默认值只能在窗口建好之后设一次。
func applyRecordTabDefaults() error {
	// 时间范围默认值：今天 00:00 ~ 23:59（勾选框默认不勾=不限时间，与改动前一致）。
	now := time.Now()
	if recW.sinceDate != nil {
		if err := recW.sinceDate.SetDate(now); err != nil {
			return err
		}
	}
	if recW.untilDate != nil {
		if err := recW.untilDate.SetDate(now); err != nil {
			return err
		}
	}
	if recW.sinceHourCb != nil {
		recW.sinceHourCb.SetCurrentIndex(0)
	}
	if recW.sinceMinCb != nil {
		recW.sinceMinCb.SetCurrentIndex(0)
	}
	if recW.untilHourCb != nil {
		recW.untilHourCb.SetCurrentIndex(23)
	}
	if recW.untilMinCb != nil {
		recW.untilMinCb.SetCurrentIndex(59)
	}
	recSyncTimeEnabled()
	if recW.limitEdit == nil {
		return nil
	}
	return recW.limitEdit.SetValue(recDefaultLoad)
}

// recLoadTarget 读"最多加载"输入框。控件值由 applyRecordTabDefaults 初始化成
// recDefaultLoad；这里仍然显式兜底（0/负数/超上限），绝不把 limit=0 之类的值发出去。
func recLoadTarget() int {
	target := recDefaultLoad
	if recW.limitEdit != nil {
		target = int(recW.limitEdit.Value())
	}
	if target < 1 {
		target = recDefaultLoad
	}
	if target > recMaxLoad {
		target = recMaxLoad
	}
	return target
}

// recQuerier 只依赖"查一页"这一个能力。
//
// 抽成接口是为了让分页循环能被测试覆盖：这段循环是"查全部"的关键（而且踩过
// limit=1 的坑），但工具本身没有 GUI 测试脚手架，所以把网络依赖收敛到一个方法上。
type recQuerier interface {
	RecordsQuery(ctx context.Context, params RecordQueryParams) (*RecordPage, error)
}

// fetchRecRecords 把一个查询"取到底"：按 recPageSize 分页向服务端要，直到取满 target
// 或服务端表示没有更多。
//
// 为什么要分页而不是一次要 20000 条：服务端单次上限 2000，且一次几十 MB 的响应会把
// 各层读上限和本工具内存都拖垮。分页后每次请求都是几百 KB。
//
// 兼容性：offset 只在第二页起才发（第一页对老服务端完全兼容）。老服务端/老转发端
// 不认识 offset 时，第二页的回显 offset 会与请求值不一致 —— 这时立刻停下并如实报告，
// 而不是无限重取第一页。
func fetchRecRecords(ctx context.Context, c recQuerier, params RecordQueryParams, target int,
	onProgress func(loaded, pages int)) (recFetchResult, error) {
	if target < 1 {
		target = recDefaultLoad
	}
	result := recFetchResult{rows: make([]recRow, 0, min(target, recPageSize))}
	offset := 0
	// 副本类要跨页合并（一局副本的三条记录可能正好被切在两页之间），
	// 所以这里累积原始记录、每页重新投影一次（合并是 O(n)，页数不多，代价可忽略）。
	allRecords := make([]map[string]any, 0, min(target, recPageSize))
	// 账号名映射同样跨页累积：运维工具是"按页翻译"的（每页只回本页出现过的 account_id），
	// 只留最后一页的映射会让前面几页的账号列退化成 dnf:<sha256> 原始主键
	// （record_fields.go 里查不到就显示主键），翻页功能一修好就会暴露出来。
	allNames := make(map[string]string)
	for {
		if len(result.rows) >= target || result.pages >= recMaxPages {
			result.more = true
			break
		}
		pageSize := recPageSize
		if remaining := target - len(allRecords); remaining < pageSize {
			pageSize = remaining
		}
		if pageSize < 1 {
			// 记录已取够（合并后行数还没到目标，但原始记录已够）：收尾。
			result.more = true
			break
		}
		pageParams := params
		pageParams.Limit = pageSize
		pageParams.Offset = offset
		page, err := c.RecordsQuery(ctx, pageParams)
		if err != nil {
			// 已经取到的部分照样交给调用方展示：部分结果 + 错误原因比"什么都没有"有用。
			return result, err
		}
		result.pages++
		result.page = page
		if page == nil {
			break
		}
		if offset > 0 && page.Offset != offset {
			result.pagingUnsupported = true
			result.more = true
			break
		}
		if page.Count == 0 {
			break
		}
		for id, name := range page.AccountNames {
			if _, exists := allNames[id]; !exists {
				allNames[id] = name
			}
		}
		if len(allNames) > 0 {
			// 把合并后的映射挂到本页上：调用方（recW.page）与详情面板都只认这一份。
			page.AccountNames = allNames
		}
		allRecords = append(allRecords, page.Records...)
		result.rows = buildRecRowsFrom(allRecords, page.AccountNames, params.Kind)
		if onProgress != nil {
			onProgress(len(result.rows), result.pages)
		}
		if !page.Limited {
			break
		}
		offset += page.Count
	}
	return result, nil
}

// min 取两数较小值（Go 1.21 起标准库有 min，这里显式写一份避免依赖版本）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// onRecQuery 查询当前类别的记录（按"最多加载"条数自动分页取到底）。
func (a *App) onRecQuery() {
	c := a.recClient()
	if c == nil {
		return
	}
	kind := recCurrentKind()
	// 时间范围来自"按时间过滤"+ 日期/时分下拉，控件值天然合法，不再需要文本校验。
	since, until := recTimeRange()
	target := recLoadTarget()
	params := RecordQueryParams{
		Kind:          kind,
		CharacterID:   strings.TrimSpace(recW.charIDEd.Text()),
		CharacterName: strings.TrimSpace(recW.charNameE.Text()),
		AccountID:     strings.TrimSpace(recW.accountEd.Text()),
		Keyword:       strings.TrimSpace(recW.keywordEd.Text()),
		Since:         since,
		Until:         until,
	}
	var fetched recFetchResult
	a.recRun("查询"+recKindTitle(kind), func() error {
		var err error
		fetched, err = fetchRecRecords(context.Background(), c, params, target, func(loaded, pages int) {
			a.sync(func() { recLog("已加载 %d 条（第 %d 页）…", loaded, pages) })
		})
		return err
	}, func(err error) {
		if err != nil && len(fetched.rows) == 0 {
			// 一条都没取到：recRun 已经记过失败原因，这里不再重复。
			return
		}
		a.applyRecResult(fetched, kind, target)
		if err != nil {
			// 翻页中途失败：已经取到的照样展示，并给出更具体的排查方向。
			recLog("注意：翻页在第 %d 页中断（%v），上表只有已取到的 %d 条。若服务端版本较旧（不认识 offset），请重新部署 DNF90Server.exe 与赞助版运维工具；否则请缩小时间范围重试。",
				fetched.pages+1, err, len(fetched.rows))
		}
	})
}

// recKindTitle 类别中文名（本地兜底，服务端返回的 title 优先）。
func recKindTitle(kind string) string {
	for index, value := range recKindValues {
		if value == kind {
			return recKindLabels[index]
		}
	}
	return kind
}

// applyRecResult 把"取到底"的结果灌进表格与详情区（GUI 线程）。
func (a *App) applyRecResult(fetched recFetchResult, kind string, target int) {
	recW.page = fetched.page
	recW.rows = fetched.rows
	recW.lastUse = time.Now()
	recMdl.rows = recW.rows
	if recW.table != nil {
		recW.table.SetModel(&recMdl)
	}
	if recW.detailEd != nil {
		recW.detailEd.SetText("（未选择记录）")
	}
	title := recKindTitle(kind)
	dir := ""
	retention := 0
	scanned := 0
	if fetched.page != nil {
		if fetched.page.Title != "" {
			title = fetched.page.Title
		}
		dir = fetched.page.Dir
		retention = fetched.page.Retention
		scanned = fetched.page.Scanned
	}
	if recW.sumL != nil {
		text := fmt.Sprintf("%s：已加载 %d 条（目标 %d，服务端本次扫描 %d 条，共 %d 页）｜记录目录 %s｜每类保留最近 %d 个文件",
			title, len(fetched.rows), target, scanned, fetched.pages, dir, retention)
		switch {
		case fetched.pagingUnsupported:
			text += "｜对端不支持翻页，只取到第一页（需重新部署服务端与运维工具）"
		case fetched.more:
			text += "｜还有更早的记录未取：把「最多加载」调大再查，或用起止时间缩小范围"
		}
		recW.sumL.SetText(text)
	}
	recLog("查询完成：%s 已加载 %d 条（%d 页）", title, len(fetched.rows), fetched.pages)
	if fetched.pagingUnsupported {
		recLog("提示：服务端或运维工具版本较旧，不认识分页参数 offset，因此只能取到第一页（%d 条）。重新部署最新的 DNF90Server.exe 与赞助版运维工具后即可翻页取全部。", len(fetched.rows))
	} else if fetched.more {
		recLog("提示：本次达到「最多加载」上限（%d 条），更早的记录未取。需要的话把上限调大（最大 %d）再查。", target, recMaxLoad)
	}
}

// onRecRowSelected 选中一行时把完整字段铺到详情区。
func (a *App) onRecRowSelected() {
	if recW.table == nil || recW.detailEd == nil {
		return
	}
	index := recW.table.CurrentIndex()
	if index < 0 || index >= len(recW.rows) {
		return
	}
	var accountNames map[string]string
	if recW.page != nil {
		accountNames = recW.page.AccountNames
	}
	recW.detailEd.SetText(recDetailText(recW.rows[index].record, accountNames))
}

// onRecMeta 查看记录系统状态：目录、类别与文件清单。
func (a *App) onRecMeta() {
	c := a.recClient()
	if c == nil {
		return
	}
	var meta *RecordMeta
	a.recRun("读取记录状态", func() error {
		var err error
		meta, err = c.RecordsMeta(context.Background())
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		if meta == nil {
			recLog("记录状态为空")
			return
		}
		if !meta.Enabled {
			recLog("记录未启用（服务端记录目录不可写或服务未启动）：dir=%s", meta.Dir)
			return
		}
		var total int64
		for _, file := range meta.Files {
			total += file.Size
		}
		kinds := make([]string, 0, len(meta.Kinds))
		for _, item := range meta.Kinds {
			kinds = append(kinds, item.Kind+"/"+item.Title)
		}
		recLog("记录已启用：dir=%s｜类别 %s｜文件 %d 个，合计 %s", meta.Dir, strings.Join(kinds, "、"), len(meta.Files), humanBytes(total))
		for index, file := range meta.Files {
			if index >= 20 {
				recLog("…… 其余 %d 个文件略（详情可在服务端记录目录查看）", len(meta.Files)-index)
				break
			}
			recLog("  %s  %s  %s", file.Name, humanBytes(file.Size), file.ModTime)
		}
	})
}

// humanBytes 人类可读的字节数。
func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, name := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f %s", value, name)
		}
	}
	return fmt.Sprintf("%.2f PB", value/unit)
}

// onRecExportCSV 把当前表格里的记录导出为 CSV。
//
// 列 = 记录里出现过的全部字段并集（每行按自己的字段填空），这样"信息尽可能详细"
// 在导出时也成立，而不是只导出表格里那 7 列。
func (a *App) onRecExportCSV() {
	if len(recW.rows) == 0 {
		recLog("当前没有可导出的记录，请先查询")
		return
	}
	kind := recCurrentKind()
	if recW.page != nil && recW.page.Kind != "" {
		kind = recW.page.Kind
	}
	dlg := new(walk.FileDialog)
	dlg.Title = "导出记录为 CSV"
	dlg.Filter = "CSV 文件 (*.csv)|*.csv|所有文件 (*.*)|*.*"
	dlg.FilePath = fmt.Sprintf("%s-%s.csv", kind, time.Now().Format("20060102-150405"))
	ok, err := dlg.ShowSave(a.mw)
	if err != nil {
		recLog("打开保存对话框失败：%v", err)
		return
	}
	if !ok || strings.TrimSpace(dlg.FilePath) == "" {
		return
	}
	path := dlg.FilePath
	rows := recW.rows
	a.recRun("导出CSV", func() error {
		return recWriteCSV(path, rows)
	}, func(err error) {
		if err != nil {
			return
		}
		recLog("已导出 %d 条记录到 %s", len(rows), path)
	})
}

// recWriteCSV 写 CSV：UTF-8 带 BOM（Excel 直接双击不乱码），字段并集做表头。
func recWriteCSV(path string, rows []recRow) error {
	columns := make([]string, 0, 16)
	seen := map[string]bool{}
	for _, row := range rows {
		for key := range row.record {
			if !seen[key] {
				seen[key] = true
				columns = append(columns, key)
			}
		}
	}
	sort.Strings(columns)
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString("\xEF\xBB\xBF"); err != nil {
		return fmt.Errorf("写入 BOM 失败: %w", err)
	}
	writer := csv.NewWriter(file)
	if err := writer.Write(columns); err != nil {
		return fmt.Errorf("写入表头失败: %w", err)
	}
	for _, row := range rows {
		line := make([]string, 0, len(columns))
		for _, column := range columns {
			// 导出的 CSV 是给运营拿去核对、转发、做表的：账号列同样要给真实账号名，
			// 不能把不可逆的 dnf:<sha256> 主键带出去（翻不到才回退原主键）。
			if column == "account_id" && strings.TrimSpace(row.accountName) != "" {
				line = append(line, row.accountName)
				continue
			}
			line = append(line, recCSVCell(row.record[column]))
		}
		if err := writer.Write(line); err != nil {
			return fmt.Errorf("写入数据失败: %w", err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("刷新 CSV 失败: %w", err)
	}
	return file.Sync()
}

// recCSVCell 把一个字段值转成 CSV 单元格文本。
func recCSVCell(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
