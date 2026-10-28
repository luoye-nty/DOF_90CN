package main

// DNF90 账号管理功能 —— 阶段2：运维端「账号管理」页 UI（设计文档 §3.3/§4.1/§4.2）。
// 无 paid tag：免费版/付费版均编译；严禁引用 paid-only 符号（remote_tab / license_paid 等）。
// 数据通道全部走阶段1 account_gm.go 直连封装（actor=ops，审计落 audit/account_direct.jsonl）。
// 服务端根目录/instance.json 探测复用 cdk_tab.go 同款模式（a.rootEdit / a.cfg.ServerRoot + serverdir）。
// PVF 选择与搜索本阶段接内存索引（pvfindex.Ensure 懒构建），落盘缓存索引（阶段4）留接口（Ensure onProgress 参数）。
// UI 与后续管理端页面功能/布局一致：ScrollView 垂直滚动 + 顶部查询条 + 角色结果 TableView +
// 选中角色摘要 + 内嵌 TabWidget 四子页（物品发放/系统邮件/资产邮件/成长与解锁）+ 本页日志。
// 窗口尺寸不动；后台 goroutine 更新 UI 一律经 a.runAsync 主线程回调 + a.uiSafe 同步机制。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"dnf90ops/internal/pvfindex"
)

var acctQueryDimLabels = []string{"账号名", "角色名"}
var acctItemCateLabels = []string{"全部", "装备", "消耗"}
var acctAmplifyLabels = []string{"无", "力量", "智力", "体力", "精神"}
var acctMailBindLabels = []string{"不绑定", "账号绑定"}
var acctAssetModeLabels = []string{"金币邮件(可领取附件)", "点券到账(直接入账+通知邮件)"}

// ================= 增幅属性映射（修正：界面顺序 ≠ 线值顺序） =================
//
// 界面下拉按玩家习惯排（力量/智力/体力/精神），但服务端 amplify_type 的线值语义不是这个顺序。
// 执行端口径为 1=体力、2=精神、3=力量、4=智力，依据有两条：
//   - go-server/cmd/scratch/grantlevel90loadout/main.go 写死 amplifyStamina=1 / amplifyStrength=3，
//     并带 -repair-strength 子命令专门把发错的 1 改回 3；
//   - etc/amplifyitem.etc 的 [option mapping table] 把 1/2 归为防御侧、3/4 归为攻击侧。
//
// 旧实现把下拉索引直接当 amplify_type 发出去，于是"选力量发出体力、选体力发出力量"，
// 智力/精神同理对调。下面两张表是唯一换算口径：UI 索引进、线值出，回显时再反查。

// acctAmplifyWireByIndex 下拉索引 → 服务端 amplify_type；索引 0=无 1=力量 2=智力 3=体力 4=精神。
var acctAmplifyWireByIndex = [...]int{0, 3, 4, 1, 2}

// acctAmplifyLabelByWire 服务端 amplify_type → 中文属性名；线值 0=无 1=体力 2=精神 3=力量 4=智力。
var acctAmplifyLabelByWire = [...]string{"无", "体力", "精神", "力量", "智力"}

// acctAmplifyWire 下拉索引 → 服务端 amplify_type（越界一律按 0=无，避免发出非法负值/越界值）。
func acctAmplifyWire(index int) int {
	if index < 0 || index >= len(acctAmplifyWireByIndex) {
		return 0
	}
	return acctAmplifyWireByIndex[index]
}

// acctAmplifyLabel 服务端 amplify_type → 中文属性名（越界按"无"，仅用于日志/预览回显）。
func acctAmplifyLabel(wire int) string {
	if wire < 0 || wire >= len(acctAmplifyLabelByWire) {
		return acctAmplifyLabelByWire[0]
	}
	return acctAmplifyLabelByWire[wire]
}

// ================= 下拉取值必须做边界保护 =================
//
// walk 的 ComboBox 只在模型**恰好 1 项**时才自动选中第 0 项（walk/combobox.go resetItems：
// `if !cb.Editable() && model != nil && model.ItemCount() == 1 { cb.SetCurrentIndex(0) }`）。
// 也就是说：一个 N 项（N>1）的下拉如果在声明时没给 CurrentIndex，CurrentIndex() 会返回 **-1**。
// 用它去索引标签 slice 就是 index out of range —— 而且这类代码往往写在 runAsync 的 done
// 回调里，那是在 GUI 线程上跑的，acctRun 的 recover 只包住 job 不包 done，
// 于是 panic 直接把整个进程带走（windowsgui 构建下连报错窗口都没有）。
//
// 所以：所有下拉一律在声明时显式给 CurrentIndex: 0，并在读取处用下面的辅助函数兜底。

// acctAssetModeLabel 资产邮件模式中文名（未选中/越界一律按第 0 项）。
func acctAssetModeLabel(cb *walk.ComboBox) string {
	idx := 0
	if cb != nil {
		idx = cb.CurrentIndex()
	}
	if idx < 0 || idx >= len(acctAssetModeLabels) {
		idx = 0
	}
	return acctAssetModeLabels[idx]
}

// acctRoleModel 角色查询结果 TableView 模型（角色名|账号|等级|金币|点券|在线）。
type acctRoleModel struct {
	walk.TableModelBase
	items []acctCharacterView
}

func (m *acctRoleModel) RowCount() int { return len(m.items) }

func (m *acctRoleModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.Name
	case 1:
		// 账号列一律显示真实账号名（账号主键 dnf:<sha256> 不可逆，见 account_names.go）
		return opsAcctNameText(item.AccountID)
	case 2:
		return item.Level
	case 3:
		return item.Gold
	case 4:
		return item.Cera
	case 5:
		if item.Online {
			return "在线"
		}
		return "离线"
	}
	return nil
}

// acctItemModel 物品搜索结果 TableView 模型（item_id|名称|类别|堆叠上限|发放位置|可强化）。
type acctItemModel struct {
	walk.TableModelBase
	items []pvfindex.SearchView
}

func (m *acctItemModel) RowCount() int { return len(m.items) }

func (m *acctItemModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.ItemID
	case 1:
		return item.Name
	case 2:
		return item.Category
	case 3:
		return item.StackLimit
	case 4:
		return item.Destination
	case 5:
		if item.SupportsUpgrade {
			return "是"
		}
		return "否"
	}
	return nil
}

// acctTabWidgets 账号管理页 UI 控件句柄（构建于 accountTab() 声明时挂载，与 cdk/ban 先例一致）。
type acctTabWidgets struct {
	// ① 顶部查询条
	pvfEdit       *walk.LineEdit // 只读，当前搜索/索引用 PVF 路径
	probeBtn      *walk.PushButton
	pickBtn       *walk.PushButton
	dimCombo      *walk.ComboBox // 账号名/角色名
	queryEdit     *walk.LineEdit
	queryBtn      *walk.PushButton
	refreshBalBtn *walk.PushButton

	// ② 角色结果
	roleTable *walk.TableView

	// ③ 选中角色摘要
	summaryL *walk.Label

	// ④-1 物品发放
	itemKeyword   *walk.LineEdit
	itemCateCb    *walk.ComboBox
	itemSearchBtn *walk.PushButton
	itemTable     *walk.TableView
	itemDetailL   *walk.Label
	amountEdit    *walk.NumberEdit
	reinforceEdit *walk.NumberEdit
	amplifyEdit   *walk.NumberEdit
	amplifyTypeCb *walk.ComboBox
	previewBtn    *walk.PushButton
	grantBtn      *walk.PushButton

	// ④-2 系统邮件
	mailRoleL     *walk.Label
	mailTitleEdit *walk.LineEdit
	mailBodyEdit  *walk.TextEdit
	mailGoldEdit  *walk.NumberEdit
	mailItemEdit  *walk.LineEdit
	mailCntEdit   *walk.LineEdit
	mailBindCb    *walk.ComboBox
	mailSendBtn   *walk.PushButton

	// ④-3 资产邮件
	assetModeCb     *walk.ComboBox
	assetAmountEdit *walk.NumberEdit
	assetBalanceL   *walk.Label
	assetSendBtn    *walk.PushButton

	// ④-4 成长与解锁
	sptpSPEdit   *walk.NumberEdit
	sptpTPEdit   *walk.NumberEdit
	jobLoadBtn   *walk.PushButton
	sptpBtn      *walk.PushButton
	levelEdit    *walk.NumberEdit
	levelBtn     *walk.PushButton
	job1Cb       *walk.ComboBox
	job2Cb       *walk.ComboBox
	job3Cb       *walk.ComboBox
	jobApplyBtn  *walk.PushButton
	// jobCareerBtn 跨大系转职（独立按钮，需角色离线）。
	//
	// 为什么单开一个按钮而不是复用「应用转职」：
	//  · 跨大系在服务端走的是**另一条接口**（/dnf/gm/characters/{id}/career），
	//    它真改 job、重建技能、重算属性、清转职任务，**且要求角色离线**；
	//  · 这条路的后果（技能重置 / 装扮可能穿不上 / 任务残留）是产品级的，
	//    不该由"下拉框里顺手选成了别的职业"触发 —— 必须是一次显式动作；
	//  · 本工具没有确认弹窗机制（危险操作都是"点按钮 + 日志"），
	//    所以"多一个按钮、要多点一下"就是这里最贴合的确认方式。
	jobCareerBtn *walk.PushButton
	dungeonBtn   *walk.PushButton
	equipSlotBtn *walk.PushButton

	// ④-4b 黑钻（账号级权益，按天开启/续期 + 只读实时查询）
	bdDaysEdit *walk.NumberEdit
	bdBtn      *walk.PushButton
	bdQueryBtn *walk.PushButton
	bdStateL   *walk.Label

	// ④-5 邮件管理（删除邮件）
	mbRoleL      *walk.Label
	mbRefreshBtn *walk.PushButton
	mbTable      *walk.TableView
	mbDetailL    *walk.Label
	mbForceChk   *walk.CheckBox
	mbDelBtn     *walk.PushButton
	mbClearBtn   *walk.PushButton

	// ④ 本页日志
	logEdit *walk.TextEdit

	// 状态
	pvfPath    string               // 当前搜索/索引用 PVF 路径（空=未探测）
	selRole    *acctCharacterView   // 当前选中角色（值副本）
	selItem    *pvfindex.SearchView // 物品发放选中物品（值副本）
	sptpGot    bool                 // 已载入 SP/TP 现状
	jobGot     bool                 // 已载入转职现状
	// curJob 载入现状时的**职业大类**（0..15）。用来判断"这次转职是不是跨大系"：
	// 服务端的 /character/job 只允许同系换分支/觉醒（currentJob != request.Job 直接拒绝），
	// 跨大系必须走 /characters/{id}/career。见 onAcctJobApply / onAcctJobCareerApply。
	curJob     int
	curSP      int                  // 当前 SP 剩余（载入后）
	curTP      int                  // 当前 TP 剩余（载入后）
	curTotalSP int                  // 角色最大 SP（载入后）
	curTotalTP int                  // 角色最大 TP（载入后）
}

var acctW acctTabWidgets
var acctRoleMdl acctRoleModel
var acctItemMdl acctItemModel
var acctMboxMdl acctMailboxModel

// accountTab 账号管理页控件树（设计 §3.3；ScrollView 垂直滚动承载全部功能区）。
func (a *App) accountTab() []Widget {
	// 页面初值：优先本地持久化的 PVF 路径（config.json acct_local_pvf），免去每次手动选择/探测。
	acctW.pvfPath = a.cfg.AcctLocalPVF
	acctApp = a // 供本页日志批量刷新回主线程使用
	return []Widget{
		ScrollView{
			HorizontalFixed: true,
			Layout:          VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []Widget{
				// ① 连接与角色查询条
				Composite{
					Layout: VBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 6},
							Children: []Widget{
								Label{Text: "服务器 PVF:"},
								LineEdit{AssignTo: &acctW.pvfEdit, ReadOnly: true, Text: acctW.pvfPath, MinSize: Size{Width: 330, Height: 0}, ToolTipText: "当前用于物品搜索/索引的 Script.pvf 路径（点「探测实例PVF」自动读取 instance.json）"},
								PushButton{AssignTo: &acctW.probeBtn, Text: "探测实例PVF", OnClicked: a.onAcctProbePVF, ToolTipText: "从当前服务端根目录 instance.json 读取 game.pvfPath"},
								PushButton{AssignTo: &acctW.pickBtn, Text: "选择PVF...", OnClicked: a.onAcctPickPVF, ToolTipText: "手动选择 Script.pvf（仅影响搜索；发放仍以执行端 instance.json 的 PVF 为准）"},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 6},
							Children: []Widget{
								Label{Text: "查询维度:"},
								ComboBox{AssignTo: &acctW.dimCombo, Model: acctQueryDimLabels, MinSize: Size{Width: 90, Height: 0}},
								LineEdit{AssignTo: &acctW.queryEdit, MinSize: Size{Width: 180, Height: 0}, ToolTipText: "输入账号名或角色名后查询；可留空=全量角色"},
								PushButton{AssignTo: &acctW.queryBtn, Text: "查询角色", OnClicked: a.onAcctQueryRoles},
								PushButton{AssignTo: &acctW.refreshBalBtn, Text: "刷新余额", OnClicked: a.onAcctRefreshBalance, ToolTipText: "查询选中角色当前金币/点券余额"},
							},
						},
					},
				},
				// ② 角色结果区
				Composite{
					Layout: VBox{MarginsZero: true, Spacing: 3},
					Children: []Widget{
						Label{Text: "角色查询结果（点击行选中）:"},
						TableView{
							AssignTo: &acctW.roleTable,
							Columns: []TableViewColumn{
								{Title: "角色名", Width: 150},
								// 「账号」列显示**真实账号名**（账号主键不可逆，统一在 account_names.go 里翻译）
								{Title: "账号", Width: 190},
								{Title: "等级", Width: 60},
								{Title: "金币", Width: 110},
								{Title: "点券", Width: 110},
								{Title: "在线", Width: 60},
							},
							Model:                 new(acctRoleModel),
							MinSize:               Size{Width: 860, Height: 130},
							OnCurrentIndexChanged: a.onAcctRoleSelected,
						},
					},
				},
				// ③ 选中角色摘要
				Label{AssignTo: &acctW.summaryL, Text: "未选择角色", MinSize: Size{Width: 860, Height: 0}, ToolTipText: "选中角色后自动载入 SP/TP/转职现状（GM-5/GM-8）"},
				// ④ 操作子页
				TabWidget{
					ContentMarginsZero: true,
					MinSize:            Size{Width: 0, Height: 330},
					Pages: []TabPage{
						{Title: "物品发放", Content: Composite{Layout: VBox{Margins: Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctItemTab(a)}},
						{Title: "系统邮件", Content: Composite{Layout: VBox{Margins: Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctMailTab(a)}},
						{Title: "资产邮件", Content: Composite{Layout: VBox{Margins: Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctAssetTab(a)}},
						{Title: "邮件管理", Content: Composite{Layout: VBox{Margins: Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctMailboxTab(a)}},
						{Title: "成长与解锁", Content: Composite{Layout: VBox{Margins: Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctGrowthTab(a)}},
					},
				},
				// ④ 本页日志
				Label{Text: "本页操作日志:"},
				TextEdit{AssignTo: &acctW.logEdit, ReadOnly: true, VScroll: true, MinSize: Size{Width: 860, Height: 110}},
			},
		},
	}
}

// acctItemTab 物品发放子页。
func (a *App) acctItemTab(app *App) []Widget {
	return []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "关键词(item_id/名称):"},
				LineEdit{AssignTo: &acctW.itemKeyword, MinSize: Size{Width: 170, Height: 0}},
				Label{Text: "类别:"},
				ComboBox{AssignTo: &acctW.itemCateCb, Model: acctItemCateLabels, MinSize: Size{Width: 80, Height: 0}},
				PushButton{AssignTo: &acctW.itemSearchBtn, Text: "搜索", OnClicked: a.onAcctItemSearch, ToolTipText: "需先探测/选择 PVF；首次搜索会构建内存索引（数秒）"},
				Label{Text: " 选中角色后发放物品 = 系统邮件附件。"},
			},
		},
		TableView{
			AssignTo: &acctW.itemTable,
			Columns: []TableViewColumn{
				{Title: "item_id", Width: 90},
				{Title: "名称", Width: 300},
				{Title: "类别", Width: 70},
				{Title: "堆叠上限", Width: 80},
				{Title: "发放位置", Width: 130},
				{Title: "可强化", Width: 60},
			},
			Model:                 new(acctItemModel),
			MinSize:               Size{Width: 860, Height: 110},
			OnCurrentIndexChanged: a.onAcctItemSelected,
		},
		Label{AssignTo: &acctW.itemDetailL, Text: "未选择物品", MinSize: Size{Width: 860, Height: 0}},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "数量:"},
				NumberEdit{AssignTo: &acctW.amountEdit, MinValue: 1, MaxValue: 999999999, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}},
				Label{Text: "强化(0-31):"},
				NumberEdit{AssignTo: &acctW.reinforceEdit, MinValue: 0, MaxValue: 31, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 70, Height: 0}},
				Label{Text: "增幅(0-31):"},
				NumberEdit{AssignTo: &acctW.amplifyEdit, MinValue: 0, MaxValue: 31, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 70, Height: 0}},
				Label{Text: "增幅属性:"},
				ComboBox{
					AssignTo: &acctW.amplifyTypeCb, Model: acctAmplifyLabels, MinSize: Size{Width: 80, Height: 0},
					ToolTipText: "按游戏内叫法选即可：工具会自动换算为服务端线值（力量=3/智力=4/体力=1/精神=2）",
				},
			},
		},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				PushButton{AssignTo: &acctW.previewBtn, Text: "预览", OnClicked: a.onAcctItemPreview},
				PushButton{AssignTo: &acctW.grantBtn, Text: "发放(系统邮件附件)", OnClicked: a.onAcctItemGrant, MinSize: Size{Width: 140, Height: 0}},
				Label{Text: " 提示：强化/增幅仅装备类生效，执行端会二次校验。"},
			},
		},
	}
}

// acctMailTab 系统邮件子页。
func (a *App) acctMailTab(app *App) []Widget {
	return []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "收件角色:"},
				Label{AssignTo: &acctW.mailRoleL, Text: "（未选中）", MinSize: Size{Width: 180, Height: 0}},
				Label{Text: "标题:"},
				LineEdit{AssignTo: &acctW.mailTitleEdit, Text: "GM 系统邮件", MaxLength: 40, MinSize: Size{Width: 260, Height: 0}},
				Label{Text: "金币:"},
				NumberEdit{AssignTo: &acctW.mailGoldEdit, MinValue: 0, MaxValue: 4294967295, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 110, Height: 0}},
			},
		},
		Label{Text: "正文(≤400字):"},
		TextEdit{AssignTo: &acctW.mailBodyEdit, MaxLength: 400, MinSize: Size{Width: 860, Height: 70}},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "物品附件 item_id(留空=无):"},
				LineEdit{AssignTo: &acctW.mailItemEdit, MinSize: Size{Width: 110, Height: 0}, ToolTipText: "直接填 item_id 即可；也可在「物品发放」页搜索后参考"},
				Label{Text: "数量:"},
				LineEdit{AssignTo: &acctW.mailCntEdit, MinSize: Size{Width: 70, Height: 0}},
				Label{Text: "绑定:"},
				ComboBox{AssignTo: &acctW.mailBindCb, Model: acctMailBindLabels, MinSize: Size{Width: 90, Height: 0}},
				PushButton{AssignTo: &acctW.mailSendBtn, Text: "发送邮件", OnClicked: a.onAcctMailSend},
				Label{Text: " 标题/正文/金币/物品附件至少一项。"},
			},
		},
	}
}

// acctAssetTab 资产邮件子页。
func (a *App) acctAssetTab(app *App) []Widget {
	return []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "发放方式:"},
				ComboBox{AssignTo: &acctW.assetModeCb, Model: acctAssetModeLabels, CurrentIndex: 0, MinSize: Size{Width: 260, Height: 0}},
				Label{Text: "数量:"},
				NumberEdit{AssignTo: &acctW.assetAmountEdit, MinValue: 1, MaxValue: 2147483647, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 120, Height: 0}},
				PushButton{AssignTo: &acctW.assetSendBtn, Text: "发送资产邮件", OnClicked: a.onAcctAssetSend},
			},
		},
		Label{AssignTo: &acctW.assetBalanceL, Text: "余额摘要：未查询（选中角色后点「刷新余额」）", MinSize: Size{Width: 860, Height: 0}},
		Label{Text: " 金币邮件 → 邮件附件，玩家领取后到账；点券到账 → 直接入账 + 通知邮件。只允许增加，防止误扣。"},
	}
}

// acctGrowthTab 成长与解锁子页。
func (a *App) acctGrowthTab(app *App) []Widget {
	return []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "剩余SP:"},
				NumberEdit{AssignTo: &acctW.sptpSPEdit, MinValue: 0, MaxValue: 2147483647, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}, ToolTipText: "载入现状后自动回填；不能超过该角色最大SP（更新时会校验）"},
				Label{Text: "剩余TP:"},
				NumberEdit{AssignTo: &acctW.sptpTPEdit, MinValue: 0, MaxValue: 2147483647, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}, ToolTipText: "载入现状后自动回填；不能超过该角色最大TP（更新时会校验）"},
				PushButton{AssignTo: &acctW.jobLoadBtn, Text: "载入现状", OnClicked: a.onAcctGrowthLoad},
				PushButton{AssignTo: &acctW.sptpBtn, Text: "更新SP/TP", OnClicked: a.onAcctSPTPUpdate},
			},
		},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "新等级:"},
				NumberEdit{AssignTo: &acctW.levelEdit, MinValue: 1, MaxValue: 90, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}},
				PushButton{AssignTo: &acctW.levelBtn, Text: "设置等级", OnClicked: a.onAcctLevelSet},
			},
		},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "职业:"},
				ComboBox{AssignTo: &acctW.job1Cb, Model: acctAllJobNames(), MinSize: Size{Width: 150, Height: 0}, OnCurrentIndexChanged: a.onAcctJob1Changed, ToolTipText: "点「载入现状」自动回填；选择职业大类后下方转职/觉醒下拉自动联动"},
				Label{Text: "转职:"},
				ComboBox{AssignTo: &acctW.job2Cb, Model: acctGrowNamesByJob(0), MinSize: Size{Width: 150, Height: 0}, OnCurrentIndexChanged: a.onAcctJob2Changed, ToolTipText: "按职业大类联动显示可选转职"},
				Label{Text: "觉醒:"},
				ComboBox{AssignTo: &acctW.job3Cb, Model: acctAwakeNamesByGrow(0, 0), MinSize: Size{Width: 150, Height: 0}, ToolTipText: "未觉醒 / 一次觉醒 / 二次觉醒（按转职分支提供可选觉醒名）"},
				PushButton{AssignTo: &acctW.jobApplyBtn, Text: "应用转职", OnClicked: a.onAcctJobApply, ToolTipText: "按三级下拉选择一次性提交 job/first_grow/second_grow；需先「载入现状」"},
				PushButton{AssignTo: &acctW.jobCareerBtn, Text: "跨大系转职", OnClicked: a.onAcctJobCareerApply, ToolTipText: "换职业大类（如鬼剑士→魔法师）。走服务端 career 接口：会重建技能、重算属性、清转职任务，且【角色必须离线】；装扮/武器可能因职业限制失效"},
			},
		},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "黑钻时长(天):"},
				NumberEdit{
					AssignTo: &acctW.bdDaysEdit, Value: 30.0, MinValue: 1, MaxValue: 3650,
					Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 80, Height: 0},
					ToolTipText: "黑钻按天算（1~3650）。黑钻是账号级权益：同一账号下所有角色共享，疲劳上限提升到 188。",
				},
				PushButton{AssignTo: &acctW.bdBtn, Text: "开启黑钻", OnClicked: a.onAcctBlackDiamond, ToolTipText: "给选中角色所属账号开启/续期黑钻：到期时间 = max(现在, 当前到期时间) + 天数"},
				PushButton{AssignTo: &acctW.bdQueryBtn, Text: "查询黑钻", OnClicked: a.onAcctBlackDiamondQuery, ToolTipText: "只读查询该角色所属账号的黑钻现状（不改任何数据）；选中角色时会自动查一次"},
				Label{Text: "常用:"},
				PushButton{Text: "30天", OnClicked: func() { a.onAcctBlackDiamondPreset(30) }},
				PushButton{Text: "90天", OnClicked: func() { a.onAcctBlackDiamondPreset(90) }},
				PushButton{Text: "365天", OnClicked: func() { a.onAcctBlackDiamondPreset(365) }},
			},
		},
		Label{
			AssignTo: &acctW.bdStateL, Text: "黑钻状态：选中角色后自动查询",
			MinSize:     Size{Width: 860, Height: 0},
			ToolTipText: "黑钻是账号级权益：同账号角色共享同一到期时间。数据优先来自服务端只读接口，服务端未更新时自动退回直读游戏库。",
		},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				PushButton{AssignTo: &acctW.dungeonBtn, Text: "副本全部解锁", OnClicked: a.onAcctDungeonsUnlock, ToolTipText: "按嵌入 dungeons_max_difficulty.json 的全副本最高难度解锁"},
				PushButton{AssignTo: &acctW.equipSlotBtn, Text: "装备槽全部解锁", OnClicked: a.onAcctEquipSlotsUnlock},
			},
		},
	}
}

// ---- ④-4 辅助：职业/转职/觉醒三级下拉联动 ----

// onAcctJob1Changed 职业大类变化后重填转职与觉醒下拉（转职置第 0 项，触发下级级联）。
func (a *App) onAcctJob1Changed() {
	idx := acctW.job1Cb.CurrentIndex()
	if idx < 0 || idx >= len(acctClassTree) {
		return
	}
	job := acctClassTree[idx].JobID
	_ = acctW.job2Cb.SetModel(acctGrowNamesByJob(job))
	_ = acctW.job2Cb.SetCurrentIndex(0)
}

// onAcctJob2Changed 转职分支变化后重填觉醒下拉（置「未觉醒」）。
func (a *App) onAcctJob2Changed() {
	job, firstGrow, ok := acctJobFirstTwo()
	if !ok {
		return
	}
	_ = acctW.job3Cb.SetModel(acctAwakeNamesByGrow(job, firstGrow))
	_ = acctW.job3Cb.SetCurrentIndex(0)
}

// acctFillJobCombos 按角色现状回填三级下拉（载入现状后调用；须在主线程）。
func acctFillJobCombos(job, firstGrow, secondGrow int) {
	if acctW.job1Cb == nil || acctW.job2Cb == nil || acctW.job3Cb == nil {
		return
	}
	ci := acctClassIndexByJob(job)
	if ci < 0 {
		ci = 0
	}
	if err := acctW.job1Cb.SetCurrentIndex(ci); err != nil {
		return
	}
	// onAcctJob1Changed 已把 job2 置第 0 项，这里再切到实际转职值（触发 job3 级联）。
	gi := acctGrowIndexByValue(job, firstGrow)
	if gi < 0 {
		gi = 0
	}
	if err := acctW.job2Cb.SetCurrentIndex(gi); err != nil {
		return
	}
	// onAcctJob2Changed 已把 job3 置「未觉醒」；secondGrow 即觉醒下拉 index（0/1/2）。
	names := acctAwakeNamesByGrow(job, firstGrow)
	ai := 0
	if secondGrow >= 0 && secondGrow < len(names) {
		ai = secondGrow
	}
	_ = acctW.job3Cb.SetCurrentIndex(ai)
}

// acctJobFirstTwo 读取职业大类与转职分支的当前下拉选择值。
func acctJobFirstTwo() (job, firstGrow int, ok bool) {
	jidx := acctW.job1Cb.CurrentIndex()
	if jidx < 0 || jidx >= len(acctClassTree) {
		return 0, 0, false
	}
	cls := &acctClassTree[jidx]
	job = cls.JobID
	gidx := acctW.job2Cb.CurrentIndex()
	if gidx < 0 || gidx >= len(cls.Grows) {
		return job, 0, false
	}
	return job, cls.Grows[gidx].FirstGrow, true
}

// acctJobSelections 读取三级下拉当前值；下拉未就绪时 ok=false。
func acctJobSelections() (job, firstGrow, secondGrow int, ok bool) {
	job, firstGrow, ok = acctJobFirstTwo()
	if !ok {
		return 0, 0, 0, false
	}
	jidx := acctW.job3Cb.CurrentIndex()
	names := acctAwakeNamesByGrow(job, firstGrow)
	if jidx < 0 || jidx >= len(names) {
		return job, firstGrow, 0, false
	}
	return job, firstGrow, jidx, true
}

// ---- 页面日志与忙碌控制 ----
//
// acctLog 同样改为"内存缓冲 + UI 线程批量刷新"（与主日志一致，理由见 main.go 顶部注释）：
// 旧实现每行 AppendText + 滚到底，且把 EDIT 上限放开到 2GB、无行数上限，
// 日志一多每行成本翻数倍，并且从后台协程调用会跨线程写控件把调用方阻塞住。
// 现在任何协程都能安全调用 acctLog。
const acctLogMaxLines = 500

var (
	acctLogLimitOnce sync.Once
	acctLogMu        sync.Mutex
	acctLogBuf       []string
	acctLogDirty     int
)

// flushAcctLog 由主日志刷新循环（main.flushLogs）按同一节奏调用。
func flushAcctLog() {
	acctLogMu.Lock()
	dirty := acctLogDirty
	acctLogDirty = 0
	lines := acctLogBuf
	if len(lines) > acctLogMaxLines {
		lines = lines[len(lines)-acctLogMaxLines:]
	}
	text := ""
	if dirty > 0 && acctW.logEdit != nil {
		text = strings.Join(lines, "\r\n")
	}
	acctLogMu.Unlock()

	if text == "" {
		return
	}

	a := acctApp
	if a == nil {
		return
	}
	a.uiSafe(func() {
		if acctW.logEdit == nil {
			return
		}
		acctLogLimitOnce.Do(func() {
			if h := acctW.logEdit.Handle(); h != 0 {
				win.SendMessage(h, win.EM_SETLIMITTEXT, logEditMaxChars, 0)
			}
		})
		acctW.logEdit.SetText(text + "\r\n")
		acctLogScrollBottom(acctW.logEdit)
	})
}

// acctApp 主 App 引用（buildAccountTab 时注入），供后台刷新回主线程。
var acctApp *App

// acctLogScrollBottom 将日志框滚动到底部（须在主线程调用）。
func acctLogScrollBottom(edit *walk.TextEdit) {
	if edit == nil {
		return
	}
	if h := edit.Handle(); h != 0 {
		win.SendMessage(h, win.WM_VSCROLL, win.SB_BOTTOM, 0)
	}
}

// acctLog 追加一行到本页日志缓冲（线程安全、不阻塞、不碰控件）。
func acctLog(format string, args ...any) {
	if acctW.logEdit == nil {
		return
	}
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	acctLogMu.Lock()
	acctLogBuf = append(acctLogBuf, line)
	if over := len(acctLogBuf) - acctLogMaxLines*2; over > 0 {
		acctLogBuf = append([]string(nil), acctLogBuf[over:]...)
	}
	acctLogDirty++
	acctLogMu.Unlock()
}

// acctBusyUI 任务期间禁用/恢复本页全部操作按钮（由 runAsync 主线程回调驱动）。
func (a *App) acctBusyUI(busy bool) {
	buttons := []*walk.PushButton{
		acctW.probeBtn, acctW.pickBtn, acctW.queryBtn, acctW.refreshBalBtn,
		acctW.itemSearchBtn, acctW.previewBtn, acctW.grantBtn,
		acctW.mailSendBtn, acctW.assetSendBtn,
		acctW.jobLoadBtn, acctW.sptpBtn, acctW.levelBtn, acctW.jobApplyBtn, acctW.jobCareerBtn,
		acctW.dungeonBtn, acctW.equipSlotBtn,
		acctW.mbRefreshBtn, acctW.mbDelBtn, acctW.mbClearBtn,
	}
	for _, btn := range buttons {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
	// 定向删除邮件还要额外受"角色是否在线"约束，忙碌态恢复时重新按在线状态判定一次。
	if !busy {
		a.acctMailboxApplyOnlineUI()
	}
}

// acctRun 账号管理页统一后台任务入口：页面级忙碌提示 + 走 runAsync（全局互斥 + 主线程回调）。
func (a *App) acctRun(label string, job func() error, done func(err error)) {
	if a.busy.Load() {
		acctLog("已有任务进行中，请稍候")
		return
	}
	acctLog("处理中：%s ...", label)
	a.acctBusyUI(true)
	a.runAsync(label, job, func(err error) {
		a.acctBusyUI(false)
		if err != nil {
			acctLog("失败：%v", err)
		}
		if done != nil {
			done(err)
		}
	})
}

// acctSelectedRole 返回当前选中角色（无则日志提示并返回 nil）。
func (a *App) acctSelectedRole() *acctCharacterView {
	if acctW.selRole == nil {
		acctLog("请先在角色结果中选择一个角色")
		return nil
	}
	return acctW.selRole
}

// acctOnlineText 在线状态中文。
func acctOnlineText(online bool) string {
	if online {
		return "在线"
	}
	return "离线"
}

// acctSummarize 更新选中角色摘要标签（在主线程调用）。
func (a *App) acctSummarize() {
	if acctW.selRole == nil {
		acctW.summaryL.SetText("未选择角色")
		return
	}
	r := acctW.selRole
	text := fmt.Sprintf("角色: %s | 账号: %s | 等级: %d | 金币: %d | 点券: %d | 在线: %s",
		r.Name, opsAcctNameText(r.AccountID), r.Level, r.Gold, r.Cera, acctOnlineText(r.Online))
	if acctW.sptpGot {
		text += fmt.Sprintf(" | 剩余SP: %d | 剩余TP: %d", acctW.curSP, acctW.curTP)
	}
	if acctW.jobGot {
		if job, first, second, ok := acctJobSelections(); ok {
			text += " | 职业: " + acctJobDisplayName(job, first, second)
		}
	}
	acctW.summaryL.SetText(text)
}

// acctNum NumberEdit 当前整数值（非法/空按 0）。
func acctNum(edit *walk.NumberEdit) int {
	if edit == nil {
		return 0
	}
	return int(edit.Value())
}

// ---- ① 查询条事件 ----

// onAcctProbePVF 探测 instance.json 中的 game.pvfPath 作为搜索/索引路径。
func (a *App) onAcctProbePVF() {
	a.acctRun("探测实例PVF", func() error {
		path, err := a.acctPVFPathAbs()
		if err != nil {
			return err
		}
		acctW.pvfPath = path
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		acctW.pvfEdit.SetText(acctW.pvfPath)
		a.persistAcctLocalPVF()
		acctLog("已读取实例 PVF 路径：%s", acctW.pvfPath)
	})
}

// persistAcctLocalPVF 将当前 PVF 路径写入 config.json（空值也会覆盖，表示清除记忆）。
func (a *App) persistAcctLocalPVF() {
	a.cfg.AcctLocalPVF = acctW.pvfPath
	if err := a.cfg.Save(); err != nil {
		acctLog("本地 PVF 路径持久化失败: %v", err)
	}
}

// onAcctPickPVF 手动选择 Script.pvf（仅影响本页搜索/预览；发放校验仍以执行端 instance.json 的 PVF 为准）。
func (a *App) onAcctPickPVF() {
	dlg := walk.FileDialog{
		Title:  "选择 Script.pvf",
		Filter: "PVF 文件 (*.pvf)|*.pvf|所有文件 (*.*)|*.*",
	}
	ok, err := dlg.ShowOpen(a.mw)
	if err != nil {
		acctLog("打开文件对话框失败: %v", err)
		return
	}
	if !ok {
		return
	}
	acctW.pvfPath = dlg.FilePath
	acctW.pvfEdit.SetText(acctW.pvfPath)
	a.persistAcctLocalPVF()
	acctLog("已手动指定 PVF：%s（已记住，下次启动自动回填；提示：建议与执行端 instance.json game.pvfPath 保持一致）", acctW.pvfPath)
}

// onAcctQueryRoles 按账号名/角色名查询角色。
func (a *App) onAcctQueryRoles() {
	keyword := strings.TrimSpace(acctW.queryEdit.Text())
	if keyword == "" {
		acctLog("请输入账号名或角色名（支持空=全量，但建议输入以缩小结果）")
		return
	}
	dim := acctW.dimCombo.CurrentIndex()
	var username, characterName string
	if dim == 0 {
		username = keyword
	} else {
		characterName = keyword
	}
	var rows []acctCharacterView
	a.acctRun("查询角色", func() error {
		var err error
		rows, _, err = a.acctCharacters(context.Background(), username, characterName)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctRoleMdl.items = rows
		// 账号列要显示真实账号名：上表前先把这批主键翻译掉
		// （翻译失败只会退回显示主键，不影响本次查询结果）。
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.AccountID)
		}
		a.resolveAcctNames(acctNameList(ids...))
		acctW.roleTable.SetModel(&acctRoleMdl)
		acctW.selRole = nil
		acctW.sptpGot = false
		acctW.jobGot = false
		acctW.selItem = nil
		acctW.itemDetailL.SetText("未选择物品")
		acctW.mailRoleL.SetText("（未选中）")
		acctW.mbRoleL.SetText("（未选中）")
		acctMailboxResetForRole()
		a.acctSummarize()
		acctLog("查询完成：共 %d 名角色。点击行选中后可操作各子页", len(rows))
	})
}

// onAcctRefreshBalance 刷新选中角色余额（GM-3），更新资产邮件页余额摘要与摘要行。
func (a *App) onAcctRefreshBalance() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	var result acctWalletResult
	a.acctRun("刷新余额", func() error {
		var err error
		result, _, err = a.acctWalletQuery(context.Background(), role.Name)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctW.assetBalanceL.SetText(fmt.Sprintf("余额摘要：角色 %s | 金币 %d | 点券 %d（在线 %s）",
			result.Character.Name, result.Cera, result.Gold, acctOnlineText(result.Character.Online)))
		if result.Character.Online != role.Online || result.Character.Gold != role.Gold || result.Character.Cera != role.Cera {
			role.Online = result.Character.Online
			role.Gold = result.Character.Gold
			role.Cera = result.Character.Cera
			a.acctSummarize()
		}
		acctLog("余额刷新完成：金币 %d / 点券 %d", result.Character.Gold, result.Character.Cera)
	})
}

// ---- ② 角色选中 ----

// onAcctRoleSelected 角色行选中：保存值副本，摘要即时更新；后台载入 SP/TP + 转职现状并回填成长页。
func (a *App) onAcctRoleSelected() {
	if acctW.roleTable == nil || acctW.selRole != nil && false {
		return
	}
	index := acctW.roleTable.CurrentIndex()
	if index < 0 || index >= len(acctRoleMdl.items) {
		acctW.selRole = nil
		return
	}
	row := acctRoleMdl.items[index]
	acctW.selRole = &row
	acctW.sptpGot = false
	acctW.jobGot = false
	acctW.mailRoleL.SetText(fmt.Sprintf("%s（账号 %s）", row.Name, opsAcctNameText(row.AccountID)))
	acctW.mbRoleL.SetText(fmt.Sprintf("%s（账号 %s）", row.Name, opsAcctNameText(row.AccountID)))
	acctMailboxResetForRole()
	acctLog("已选中角色：%s（账号 %s，等级 %d，%s）", row.Name, opsAcctNameText(row.AccountID), row.Level, acctOnlineText(row.Online))
	a.acctSummarize()

	// 后台载入 SP/TP + 转职现状（GM-5/GM-8），用于摘要与成长页回填。
	// 黑钻查询排在它后面：acctRun 是本页全局互斥的（busy 时直接拒绝），
	// 两件事不能并发排队，否则后一个会被"已有任务进行中"吃掉。
	characterID := row.CharacterID
	name := row.Name
	var skill acctSkillInfo
	var job acctJobInfo
	a.acctRun("载入选中角色 SP/TP/转职", func() error {
		var err error
		skill, err = a.acctSPTPGet(context.Background(), characterID)
		if err != nil {
			return err
		}
		job, err = a.acctJobGet(context.Background(), characterID)
		return err
	}, func(err error) {
		if err == nil {
			acctW.curSP = skill.RemainingSP
			acctW.curTP = skill.RemainingTP
			acctW.curTotalSP = skill.TotalSP
			acctW.curTotalTP = skill.TotalTP
			acctFillJobCombos(job.Job, job.FirstGrow, job.SecondGrow)
			acctW.sptpSPEdit.SetValue(float64(skill.RemainingSP))
			acctW.sptpTPEdit.SetValue(float64(skill.RemainingTP))
			acctW.sptpGot = true
			acctW.jobGot = true
			acctW.curJob = job.Job
			a.acctSummarize()
			acctLog("已载入 %s 的 SP/TP/转职现状：SP 剩余 %d/%d，TP 剩余 %d/%d，职业 %s",
				name, acctW.curSP, acctW.curTotalSP, acctW.curTP, acctW.curTotalTP, acctJobDisplayName(job.Job, job.FirstGrow, job.SecondGrow))
		} else {
			acctLog("载入 %s 的 SP/TP/转职现状失败：%v（不影响基础操作）", name, err)
		}
		// 黑钻现状是账号级权益，选中角色就顺手查一次（只读；失败不影响其它操作）。
		sel := row
		a.acctBlackDiamondQueryAsync(&sel)
	})
}

// ---- ④-1 物品发放 ----

// onAcctItemSearch 物品搜索（pvfindex.Ensure 懒构建 + Search 内存分页；category 对齐 GM 语义）。
func (a *App) onAcctItemSearch() {
	keyword := strings.TrimSpace(acctW.itemKeyword.Text())
	category := "all"
	switch acctW.itemCateCb.CurrentIndex() {
	case 1:
		category = "equipment"
	case 2:
		category = "item"
	}
	pvf := acctW.pvfPath
	if pvf == "" {
		acctLog("请先点击「探测实例PVF」或「选择PVF...」确定搜索用 PVF")
		return
	}
	var result pvfindex.SearchResult
	a.acctRun("搜索物品", func() error {
		catalog, err := pvfindex.Ensure(context.Background(), pvf, nil)
		if err != nil {
			return fmt.Errorf("构建本地 PVF 索引失败: %w", err)
		}
		result, err = catalog.Search(context.Background(), pvfindex.SearchRequest{
			Keyword:  keyword,
			Category: category,
			Page:     1,
			PageSize: 50,
		})
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctItemMdl.items = result.Items
		acctW.itemTable.SetModel(&acctItemMdl)
		acctW.selItem = nil
		a.acctApplyItemUpgradeUI(nil)
		acctW.itemDetailL.SetText("未选择物品")
		acctLog("物品搜索完成：命中 %d 条（第 %d/%d 页，每页 %d 条）", result.Total, result.Page, result.PageCount, result.PageSize)
	})
}

// onAcctItemSelected 物品搜索结果行选中：展示详情并保存值副本。
func (a *App) onAcctItemSelected() {
	if acctW.itemTable == nil {
		return
	}
	index := acctW.itemTable.CurrentIndex()
	if index < 0 || index >= len(acctItemMdl.items) {
		acctW.selItem = nil
		return
	}
	item := acctItemMdl.items[index]
	acctW.selItem = &item
	a.acctApplyItemUpgradeUI(&item)
	upgrade := "不支持"
	if item.SupportsUpgrade {
		upgrade = "支持强化/增幅"
	}
	acctW.itemDetailL.SetText(fmt.Sprintf("已选择：%s（item_id=%d，类别=%s，发放位置=%s，堆叠上限=%d，%s）",
		item.Name, item.ItemID, item.Category, item.Destination, item.StackLimit, upgrade))
}

// acctApplyItemUpgradeUI 按选中物品类型启停强化/增幅/增幅属性输入：
// 道具（Category != "equipment"，如消耗品/材料）不支持强化增幅，三项禁用并归零；
// 装备（Category == "equipment"）才允许填写。选中行变化或搜索完成（nil）时调用。
func (a *App) acctApplyItemUpgradeUI(item *pvfindex.SearchView) {
	equip := item != nil && item.Category == "equipment"
	acctW.reinforceEdit.SetEnabled(equip)
	acctW.amplifyEdit.SetEnabled(equip)
	acctW.amplifyTypeCb.SetEnabled(equip)
	if !equip {
		if acctW.reinforceEdit.Value() != 0 {
			acctW.reinforceEdit.SetValue(0)
		}
		if acctW.amplifyEdit.Value() != 0 {
			acctW.amplifyEdit.SetValue(0)
		}
		if acctW.amplifyTypeCb.CurrentIndex() != 0 {
			acctW.amplifyTypeCb.SetCurrentIndex(0)
		}
	}
}

// acctItemParams 读取并即时校验发放参数（数量/强化/增幅/增幅属性）。
func (a *App) acctItemParams() (uint32, int, int, int, string) {
	if a.acctSelectedRole() == nil {
		return 0, 0, 0, 0, "未选中角色"
	}
	if acctW.selItem == nil {
		return 0, 0, 0, 0, "请先在物品发放页搜索并选择一个物品"
	}
	amount := int64(acctW.amountEdit.Value())
	if amount < 1 || amount > int64(pvfindex.MaxItemAmount) {
		return 0, 0, 0, 0, fmt.Sprintf("数量必须在 1~%d 之间", pvfindex.MaxItemAmount)
	}

	// 道具（非装备）发放不需要强化/增幅/增幅属性参数，一律按 0 发送；
	// 装备才读取并校验下方三个输入（UI 上道具已禁用，此处属第二道防线）。
	if acctW.selItem.Category != "equipment" {
		return uint32(amount), 0, 0, 0, ""
	}
	reinforce := acctNum(acctW.reinforceEdit)
	amplify := acctNum(acctW.amplifyEdit)
	// 下拉索引必须经 acctAmplifyWire 换算成服务端 amplify_type 再发（见本文件顶部映射表说明）。
	amplifyType := acctAmplifyWire(acctW.amplifyTypeCb.CurrentIndex())
	if amplifyType < 0 {
		amplifyType = 0 // 下拉从未选中时为 -1，按"无属性"处理，避免发给服务端被判负参
	}
	if reinforce > 31 || amplify > 31 {
		return 0, 0, 0, 0, "强化/增幅必须在 0~31 之间"
	}
	if amplify > 0 && amplifyType == 0 {
		return 0, 0, 0, 0, "填写了增幅等级，请选择增幅属性（力量/智力/体力/精神）"
	}
	if amplifyType > 0 && amplify == 0 {
		return 0, 0, 0, 0, "选择了增幅属性，请填写增幅等级（1~31）"
	}
	if reinforce > 0 && amplify > 0 {
		return 0, 0, 0, 0, "强化与增幅不能同时填写（GM 约束）"
	}
	return uint32(amount), reinforce, amplify, amplifyType, ""
}

// onAcctItemPreview 预览发放内容（不真正发送）。
func (a *App) onAcctItemPreview() {
	amount, reinforce, amplify, amplifyType, problem := a.acctItemParams()
	if problem != "" {
		acctLog("%s", problem)
		return
	}
	item := acctW.selItem
	detail := fmt.Sprintf("预览：将向角色 %s 发放物品「%s」(item_id=%d) x%d", acctW.selRole.Name, item.Name, item.ItemID, amount)
	if reinforce > 0 {
		detail += fmt.Sprintf("，强化+%d", reinforce)
	}
	if amplify > 0 {
		detail += fmt.Sprintf("，增幅+%d(%s)", amplify, acctAmplifyLabel(amplifyType))
	}
	if !item.SupportsUpgrade && (reinforce > 0 || amplify > 0) {
		detail += " ｜注意：该物品不支持强化/增幅，发放将被执行端拒绝"
	}
	acctLog("%s", detail)
	acctLog("确认无误后点击「发放(系统邮件附件)」")
}

// onAcctItemGrant 发放物品（系统邮件附件）：本地 PVF 索引 resolve 校验 → GM-2。
func (a *App) onAcctItemGrant() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	amount, reinforce, amplify, amplifyType, problem := a.acctItemParams()
	if problem != "" {
		acctLog("%s", problem)
		return
	}
	item := acctW.selItem
	if item == nil || item.ItemID == 0 {
		acctLog("请先在物品库中搜索并选中要发放的物品")
		return
	}
	itemName := item.Name
	a.acctRun("发放物品", func() error {
		_, _, err := a.acctItemMail(context.Background(), role.Name, item.ItemID, uint32(amount), reinforce, amplify, amplifyType)
		return err
	}, func(err error) {
		if err == nil {
			acctLog("物品发放成功：%s（%d）x%d → %s", itemName, item.ItemID, amount, role.Name)
		}
	})
}

// ---- ④-2 系统邮件 ----

// onAcctMailSend 发送系统邮件（标题/正文/金币/物品附件至少一项；附件 item_id 留空=无）。
func (a *App) onAcctMailSend() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	title := strings.TrimSpace(acctW.mailTitleEdit.Text())
	body := acctW.mailBodyEdit.Text()
	gold := int64(acctW.mailGoldEdit.Value())
	if gold < 0 || gold > acctMailGoldMax {
		acctLog("金币数量必须在 0~%d 之间", acctMailGoldMax)
		return
	}
	var attachments []acctMailAttachment
	itemText := strings.TrimSpace(acctW.mailItemEdit.Text())
	cntText := strings.TrimSpace(acctW.mailCntEdit.Text())
	if itemText != "" || cntText != "" {
		if itemText == "" || cntText == "" {
			acctLog("物品附件必须同时填写 item_id 与数量（都不填则不带附件）")
			return
		}
		itemID, err1 := strconv.ParseUint(itemText, 10, 32)
		cnt, err2 := strconv.ParseUint(cntText, 10, 32)
		if err1 != nil || err2 != nil || itemID == 0 || cnt == 0 {
			acctLog("物品附件 item_id/数量 必须为正整数")
			return
		}
		attachments = append(attachments, acctMailAttachment{
			ItemID: uint32(itemID),
			Count:  uint32(cnt),
			Bind:   acctW.mailBindCb.CurrentIndex() == 1,
		})
	}
	if len(attachments) == 0 && gold == 0 && strings.TrimSpace(body) == "" && title == "" {
		acctLog("标题/正文/金币/物品附件至少需填写一项")
		return
	}
	if len(title) > 40 {
		acctLog("邮件标题不能超过 40 字")
		return
	}
	if len(body) > 400 {
		acctLog("邮件正文不能超过 400 字")
		return
	}
	request := acctMailRequest{
		CharacterName: role.Name,
		Title:         title,
		Body:          body,
		Gold:          gold,
		Attachments:   attachments,
	}
	var result acctMailResult
	a.acctRun("发送系统邮件", func() error {
		var err error
		result, _, err = a.acctMailSend(context.Background(), request)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		state := "系统邮件已发送；角色上线后可领取"
		if result.Refreshed {
			state = "系统邮件已发送，在线邮箱已实时刷新"
		} else if result.Online {
			state = "系统邮件已发送；在线刷新未完成，下次打开邮箱时同步"
		}
		acctLog("%s（收件：%s）", state, role.Name)
	})
}

// ---- ④-3 资产邮件 ----

// onAcctAssetSend 资产邮件：金币邮件（可领取附件）/ 点券到账（直接入账+通知邮件）。
func (a *App) onAcctAssetSend() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	currency := "gold"
	if acctW.assetModeCb.CurrentIndex() == 1 {
		currency = "cera"
	}
	amount := int64(acctW.assetAmountEdit.Value())
	if amount < 1 || amount > acctMaxWalletBalance {
		acctLog("数量必须在 1~%d 之间", acctMaxWalletBalance)
		return
	}
	var data map[string]any
	a.acctRun("发送资产邮件", func() error {
		var err error
		data, _, err = a.acctCurrencyMail(context.Background(), role.Name, currency, amount)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		if mailErr, ok := data["mail_error"].(string); ok && mailErr != "" {
			acctLog("严重提示：点券已到账，但通知邮件发送失败（%s），请勿重复操作", mailErr)
		}
		acctLog("资产邮件处理完成：%s 到账/发放 %d（模式 %s）", currency, amount, acctAssetModeLabel(acctW.assetModeCb))
		a.onAcctRefreshBalance()
	})
}

// ---- ④-4 成长与解锁 ----

// onAcctGrowthLoad 手动载入 SP/TP 与转职现状并回填编辑框（选中角色时也会自动载入一次）。
func (a *App) onAcctGrowthLoad() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var skill acctSkillInfo
	var job acctJobInfo
	a.acctRun("载入 SP/TP/转职现状", func() error {
		var err error
		skill, err = a.acctSPTPGet(context.Background(), characterID)
		if err != nil {
			return err
		}
		job, err = a.acctJobGet(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("载入 %s 的 SP/TP/转职现状失败：%v", name, err)
			return
		}
		acctW.curSP = skill.RemainingSP
		acctW.curTP = skill.RemainingTP
		acctW.curTotalSP = skill.TotalSP
		acctW.curTotalTP = skill.TotalTP
		acctFillJobCombos(job.Job, job.FirstGrow, job.SecondGrow)
		acctW.sptpSPEdit.SetValue(float64(skill.RemainingSP))
		acctW.sptpTPEdit.SetValue(float64(skill.RemainingTP))
		acctW.sptpGot = true
		acctW.jobGot = true
		acctW.curJob = job.Job
		a.acctSummarize()
		acctLog("已载入 %s 现状：SP 剩余 %d/%d，TP 剩余 %d/%d，职业 %s（可修改后应用）",
			name, acctW.curSP, acctW.curTotalSP, acctW.curTP, acctW.curTotalTP, acctJobDisplayName(job.Job, job.FirstGrow, job.SecondGrow))
	})
}

// onAcctSPTPUpdate 更新 SP/TP：需先「载入现状」回填编辑框；双字段整体下发
// （GM-6 支持只带需修改字段，此处始终携带编辑框当前值；保持另一项数值不动即可只改一项）。
func (a *App) onAcctSPTPUpdate() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	if !acctW.sptpGot {
		acctLog("请先点击「载入现状」读取当前 SP/TP，再修改后点击「更新SP/TP」")
		return
	}
	sp := acctNum(acctW.sptpSPEdit)
	tp := acctNum(acctW.sptpTPEdit)
	if acctW.curTotalSP > 0 && sp > acctW.curTotalSP {
		acctLog("SP %d 超过该角色最大 SP %d，已取消更新", sp, acctW.curTotalSP)
		return
	}
	if acctW.curTotalTP > 0 && tp > acctW.curTotalTP {
		acctLog("TP %d 超过该角色最大 TP %d，已取消更新", tp, acctW.curTotalTP)
		return
	}
	a.acctRun("更新SP/TP", func() error {
		return a.acctSPTPSet(context.Background(), role.CharacterID, &sp, &tp)
	}, func(err error) {
		if err == nil {
			acctW.curSP = sp
			acctW.curTP = tp
			a.acctSummarize()
			acctLog("SP/TP 更新成功（角色 %s）：SP 剩余 %d / TP 剩余 %d", role.Name, sp, tp)
		}
	})
}

// onAcctLevelSet 设置等级（1~90）。
func (a *App) onAcctLevelSet() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	level := acctNum(acctW.levelEdit)
	if level < 1 || level > 90 {
		acctLog("等级必须在 1~90 之间")
		return
	}
	a.acctRun("设置等级", func() error {
		return a.acctLevelSet(context.Background(), role.CharacterID, level)
	}, func(err error) {
		if err == nil {
			role.Level = level
			a.acctSummarize()
			acctLog("已将角色 %s 等级设置为 %d", role.Name, level)
		}
	})
}

// onAcctJobApply 应用转职（**限同大系**）：一次性提交 job/first_grow/second_grow（需已载入现状）。
//
// 跨大系会被这里**主动拦下并指路**，而不是把请求发给服务端换回一句拒绝：
// 服务端的 /character/job 明确只支持同系换分支/觉醒（`currentJob != request.Job`
// 直接 400），跨大系要用另一条 career 接口 + 角色离线。见 onAcctJobCareerApply。
func (a *App) onAcctJobApply() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	job, firstGrow, secondGrow, ok := acctJobSelections()
	if !ok || (!acctW.jobGot && job == 0 && firstGrow == 0 && secondGrow == 0) {
		acctLog("请先点击「载入现状」读取转职现状，再修改后应用（GM 需一次性提交三值）")
		return
	}
	if acctW.jobGot && job != acctW.curJob {
		acctLog("跨大系转职不能用「应用转职」：%s → %s 属于换职业大类。",
			acctJobName(acctW.curJob), acctJobName(job))
		acctLog("请改用右边的「跨大系转职」按钮 —— 它走服务端 career 接口，会重建技能/重算属性/清转职任务，且【角色必须离线】。")
		return
	}
	a.acctRun("应用转职", func() error {
		return a.acctJobSet(context.Background(), role.CharacterID, job, firstGrow, secondGrow)
	}, func(err error) {
		if err == nil {
			acctW.jobGot = true
			a.acctSummarize()
			acctLog("角色 %s 转职已更新：%s（job=%d first_grow=%d second_grow=%d）", role.Name, acctJobDisplayName(job, firstGrow, secondGrow), job, firstGrow, secondGrow)
		}
	})
}

// acctJobName 职业大类索引 → 中文名（越界返回原值，避免日志里出现空字符串）。
func acctJobName(job int) string {
	names := acctAllJobNames()
	if job < 0 || job >= len(names) {
		return fmt.Sprintf("job=%d", job)
	}
	return names[job]
}

// onAcctJobCareerApply 跨大系转职（换职业大类，如鬼剑士→魔法师）。
//
// 走服务端 `POST /dnf/gm/characters/{id}/career` —— 这条路由**早就挂好了**
// （internal/app/dnf90/gm_tool_admin.go），是服务端**为跨系实现**的那一套：
// 真写 job（不只是 grow_type）、重建技能记录、重算 PVF 属性、清旧系转职/觉醒任务。
// 之前工具完全没接它，所以跨系只能撞在 /character/job 的同系校验上。
//
// 三条硬约束（都在服务端强制，这里先把话说明白，免得运营白跑）：
//  1. **角色必须离线** —— 服务端返回 ErrGMCharacterOnline，在线时它撑不住换基础职业的热刷新；
//  2. **技能会重建、SP/TP 重置**，属性按新职业成长表重算；
//  3. 装扮/武器/租赁装备有 `[usable job]` 限制，**跨系后旧的可能穿不上**；
//     旧系的转职/觉醒任务也可能残留，需要手工处理。
func (a *App) onAcctJobCareerApply() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	if !acctW.jobGot {
		acctLog("请先点击「载入现状」读取转职现状：跨大系转职同样需要一次性提交 job/分支/觉醒三值")
		return
	}
	job, firstGrow, secondGrow, ok := acctJobSelections()
	if !ok {
		acctLog("三级下拉还没选好（职业/转职/觉醒），无法提交")
		return
	}
	if job == acctW.curJob {
		acctLog("目标职业大类与当前相同（都是 %s）：同系换分支/觉醒请用「应用转职」，不需要走跨大系这条路。",
			acctJobName(job))
		return
	}
	acctLog("跨大系转职：%s → %s。注意：技能会按新职业重建、SP/TP 重置、装扮可能因职业限制失效，且角色必须离线。",
		acctJobName(acctW.curJob), acctJobName(job))
	a.acctRun("跨大系转职", func() error {
		return a.acctCareerSet(context.Background(), role.CharacterID, job, firstGrow, secondGrow)
	}, func(err error) {
		if err == nil {
			acctW.curJob = job
			acctW.jobGot = true
			a.acctSummarize()
			acctLog("角色 %s 跨大系转职完成：%s（job=%d first_grow=%d second_grow=%d）。建议让玩家重新登录后再检查技能与装扮。",
				role.Name, acctJobDisplayName(job, firstGrow, secondGrow), job, firstGrow, secondGrow)
			return
		}
		// 最常见的两种拒绝给一句可操作的话，别让运营对着英文错误码猜。
		text := err.Error()
		switch {
		case strings.Contains(text, "online") || strings.Contains(text, "在线"):
			acctLog("跨大系转职被拒：角色当前在线。请先让该角色下线（退到角色选择界面不算，要退出游戏进程）再重试。")
		case strings.Contains(text, "not present in the active Script.pvf") || strings.Contains(text, "pvf"):
			acctLog("跨大系转职被拒：目标职业/分支在本服 PVF 里不存在（两端 PVF 可能不一致）。")
		default:
			acctLog("跨大系转职失败：%v", err)
		}
	})
}

// onAcctDungeonsUnlock 副本全部解锁（dungeons_max_difficulty.json 装载）。
func (a *App) onAcctDungeonsUnlock() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	var count int
	a.acctRun("副本全部解锁", func() error {
		var err error
		count, err = a.acctDungeonsUnlockAll(context.Background(), role.CharacterID)
		return err
	}, func(err error) {
		if err == nil {
			acctLog("角色 %s 副本解锁完成：共 %d 个副本已按最高难度解锁", role.Name, count)
		}
	})
}

// onAcctEquipSlotsUnlock 装备槽全部解锁。
func (a *App) onAcctEquipSlotsUnlock() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	a.acctRun("装备槽全部解锁", func() error {
		_, err := a.acctEquipSlotsUnlockAll(context.Background(), role.CharacterID)
		return err
	}, func(err error) {
		if err == nil {
			acctLog("角色 %s 装备槽已全部解锁", role.Name)
		}
	})
}

// onAcctBlackDiamondPreset 常用天数快捷按钮：填进输入框（不直接执行，避免误点）。
func (a *App) onAcctBlackDiamondPreset(days int) {
	if acctW.bdDaysEdit == nil {
		return
	}
	_ = acctW.bdDaysEdit.SetValue(float64(days))
}

// acctBlackDiamondSourceText 数据来源文案（服务端实时 / 直读游戏库兜底）。
func acctBlackDiamondSourceText(source string) string {
	if source == acctBDSourceServer {
		return "服务端实时"
	}
	return "直读游戏库（服务端版本较旧，建议更新 DNF90Server.exe 后用实时接口）"
}

// acctBlackDiamondStateLine 把黑钻现状拼成一行状态文案。
func acctBlackDiamondStateLine(roleName string, view acctBlackDiamondView) string {
	head := fmt.Sprintf("黑钻状态：角色 %s（账号 %s）", roleName, view.AccountID)
	if !view.Active || view.ExpiresAt <= 0 {
		if view.ExpiresAt > 0 {
			return fmt.Sprintf("%s 黑钻已过期（原到期 %s）｜疲劳 %d/%d｜来源：%s",
				head, acctFormatUnix(view.ExpiresAt), view.Fatigue, view.FatigueLimit, acctBlackDiamondSourceText(view.Source))
		}
		return fmt.Sprintf("%s 从未开过黑钻｜疲劳 %d/%d｜来源：%s",
			head, view.Fatigue, view.FatigueLimit, acctBlackDiamondSourceText(view.Source))
	}
	days := view.RemainingSeconds / 86400
	if view.RemainingSeconds%86400 != 0 {
		days++
	}
	online := "离线"
	if view.Online {
		online = "在线"
	}
	return fmt.Sprintf("%s 黑钻有效：到期 %s（剩余约 %d 天）｜疲劳 %d/%d｜%s｜数据来源：%s",
		head, acctFormatUnix(view.ExpiresAt), days, view.Fatigue, view.FatigueLimit, online, acctBlackDiamondSourceText(view.Source))
}

// acctFormatUnix unix 秒 → 本地时间文案（0/负数返回占位）。
func acctFormatUnix(seconds int64) string {
	if seconds <= 0 {
		return "（无）"
	}
	return time.Unix(seconds, 0).Local().Format("2006-01-02 15:04:05")
}

// acctBlackDiamondQueryAsync 后台查询黑钻现状并刷新状态标签（选中角色时自动调用）。
func (a *App) acctBlackDiamondQueryAsync(role *acctCharacterView) {
	if role == nil {
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var view acctBlackDiamondView
	a.acctRun("查询黑钻", func() error {
		var err error
		view, err = a.acctBlackDiamondStatus(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("查询 %s 的黑钻现状失败：%v", name, err)
			if acctW.bdStateL != nil {
				_ = acctW.bdStateL.SetText(fmt.Sprintf("黑钻状态：查询失败（%v）", err))
			}
			return
		}
		line := acctBlackDiamondStateLine(name, view)
		acctLog("%s", line)
		if acctW.bdStateL != nil {
			_ = acctW.bdStateL.SetText(line)
		}
	})
}

// onAcctBlackDiamondQuery 「查询黑钻」按钮：只读查询，不改任何数据。
func (a *App) onAcctBlackDiamondQuery() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	a.acctBlackDiamondQueryAsync(role)
}

// onAcctBlackDiamond 开启/续期黑钻（账号级权益，按天算）。
func (a *App) onAcctBlackDiamond() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	days := acctNum(acctW.bdDaysEdit)
	if days < 1 || days > acctBlackDiamondMaxDays {
		acctLog("黑钻天数必须在 1~%d 之间（按天算）", acctBlackDiamondMaxDays)
		return
	}
	a.acctRun("开启黑钻", func() error {
		result, message, err := a.acctBlackDiamondActivate(context.Background(), role.CharacterID, days)
		if err != nil {
			return err
		}
		acctLog("角色 %s 黑钻已生效：%s", role.Name, message)
		acctLog("%s", acctBlackDiamondStateText(role.Name, days, result))
		if result.Deferred {
			acctLog("提示：该角色在线且当前不在安全场景，原生黑钻状态包已推迟到回城后下发（网络包不能在地城内乱发）。")
		}
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		// 开启成功后再查一次真实落库状态，避免只显示"我提交了什么"。
		a.acctBlackDiamondQueryAsync(role)
	})
}

// acctBlackDiamondStateText 黑钻操作结果文案：到期时间 + 剩余天数 + 疲劳上限。
func acctBlackDiamondStateText(roleName string, days int, result acctBlackDiamondResult) string {
	remaining := result.RemainingSeconds
	remainingDays := remaining / 86400
	if remaining%86400 != 0 {
		remainingDays++
	}
	text := fmt.Sprintf("黑钻状态：角色 %s 所属账号（%s）已开启/续期 %d 天；到期时间 %s（剩余约 %d 天）；当前疲劳上限 %d",
		roleName, result.AccountID, days, time.Unix(result.ExpiresAt, 0).Local().Format("2006-01-02 15:04:05"), remainingDays, result.FatigueLimit)
	if result.RemainingSeconds <= 0 && result.ExpiresAt > 0 {
		text = fmt.Sprintf("黑钻状态：角色 %s 所属账号（%s）已开启 %d 天；到期时间 %s；当前疲劳上限 %d",
			roleName, result.AccountID, days, time.Unix(result.ExpiresAt, 0).Local().Format("2006-01-02 15:04:05"), result.FatigueLimit)
	}
	return text
}
