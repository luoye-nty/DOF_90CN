package main

// DNF90 账号管理功能 —— 阶段3：管理端「账号管理」页 UI（设计文档 §3.3/§4.1/§4.2）。
// 数据通道全部走 APIClient /manage/v1/account/*（AC-1~AC-12），go-server 原生 GM 转发；
// 客户端不做 MySQL 直连/背包直入。物品发放为系统邮件附件（原生无背包直入端点）。
//
// 与运维端页面差异：
//   - 无本机 instance.json 探测 → PVF 路径为「本地搜索用」手动选择（发放校验在执行端二次 resolve）；
//     「执行端PVF状态」按钮（AC-12）查看执行端 instance.json 路径的索引一致性。
//   - 无 SP/TP GET 端点（GM-5 未纳入 AC）→ 成长页 SP/TP 为 LineEdit，留空=不修改，不做现状自动回填。
//   - 远程操作统一经 a.snapshotClient() + a.sync 主线程回调 + a.opMu 全局互斥（与封禁/CDK 同款纪律）。
// 窗口尺寸不动；本页以 ScrollView 垂直滚动承载全部功能区。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"dnf90manager/internal/pvfindex"
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
// 与运维工具 account_tab.go 的同名实现保持一致，两端改动必须成对同步。

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
// 用它去索引标签 slice 就是 index out of range —— 而且这类代码往往写在 acctRun 的 done
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

const (
	acctMailGoldMax  = int64(^uint32(0)) // 系统邮件金币上限（uint32）
	acctMaxWalletAmt = int64(2147483647) // 资产邮件单次数量上限（对齐执行端 accountMaxWalletBalance）
)

// onlineAccountText 在线角色行的账号显示：优先用服务端快照里已经带的登录账号名，
// 缺失时才走账号主键翻译（异常数据下也要显示点东西，绝不留空白）。
func onlineAccountText(item onlineCharacterOut) string {
	if name := strings.TrimSpace(item.Username); name != "" {
		return name
	}
	return acctNameText(item.AccountID)
}

// acctRoleModel 角色查询结果 TableView 模型（角色名|账号|等级|金币|点券|在线）。
type acctRoleModel struct {
	walk.TableModelBase
	items []acctCharacterRow
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
		// 账号列一律显示真实账号名（翻译不到才回退主键，见 account_names.go）
		return acctNameText(item.AccountID)
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

// acctOnlineModel 在线角色表模型（登录账号|角色名|等级|角色ID|金币|点券|IP）。
//
// 勾选框列（walk.ItemChecker）：勾选状态存在 checked 里，与 items 一一对应；
// 刷新/清空列表时要一起重置，否则勾选会"错位"到新数据上。
type acctOnlineModel struct {
	walk.TableModelBase
	items   []onlineCharacterOut
	checked []bool
	// onCheck 用户勾选某一行时的回调（walk 在 GUI 线程调用）。
	//
	// 为什么需要它：勾选框是"踢下线"用的，但运营自然地把它当成"选中这个人"——
	// 服主实测：在角色查询里选过角色后，去在线表勾了一个人再发邮件，邮件发到了**查询结果里那个角色**上
	// （勾选不会改变 CurrentIndex，所以点行选中的逻辑根本不触发）。现在勾选与点行等价，
	// 一律把该行写成"当前操作依据"，谁最后被选中就发给谁。
	onCheck func(index int)
}

func (m *acctOnlineModel) RowCount() int { return len(m.items) }

// reset 换一批数据：勾选状态整体重置。
func (m *acctOnlineModel) reset(items []onlineCharacterOut) {
	m.items = items
	m.checked = make([]bool, len(items))
}

// Checked 实现 walk.ItemChecker（表格勾选框列的取值）。
func (m *acctOnlineModel) Checked(index int) bool {
	if index < 0 || index >= len(m.checked) {
		return false
	}
	return m.checked[index]
}

// SetChecked 实现 walk.ItemChecker（用户点勾选框）。
// 这里只存状态、不发通知：勾选框的视觉更新由 walk 自己完成（官方 tableview 示例同款），
// 在这里再 PublishRowsReset 反而会在通知回调里重入模型。
func (m *acctOnlineModel) SetChecked(index int, checked bool) error {
	if index < 0 || index >= len(m.checked) {
		return nil
	}
	m.checked[index] = checked
	// 勾选 = 选中这个人（见 onCheck 的说明）；取消勾选不改变当前操作依据。
	if checked && m.onCheck != nil {
		m.onCheck(index)
	}
	return nil
}

// checkedCharacters 返回勾选行的角色数据（供「踢下选中」使用）。
func (m *acctOnlineModel) checkedCharacters() []onlineCharacterOut {
	out := make([]onlineCharacterOut, 0, len(m.items))
	for i, item := range m.items {
		if i < len(m.checked) && m.checked[i] {
			out = append(out, item)
		}
	}
	return out
}

func (m *acctOnlineModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return onlineAccountText(item)
	case 1:
		return item.Name
	case 2:
		return item.Level
	case 3:
		return item.CharacterID
	case 4:
		return item.Gold
	case 5:
		return item.Cera
	case 6:
		return item.LastIP
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

// acctTabWidgets 账号管理页 UI 控件句柄（全局变量持有，声明时挂载；与 cdk/ban 先例一致）。
type acctTabWidgets struct {
	// ① 顶部查询条
	pvfEdit       *walk.LineEdit   // 只读，本地搜索/索引用 PVF 路径
	probeBtn      *walk.PushButton // 执行端PVF状态（AC-12）
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
	itemPrevBtn   *walk.PushButton
	itemNextBtn   *walk.PushButton
	itemPageL     *walk.Label
	itemTable     *walk.TableView
	itemDetailL   *walk.Label
	amountEdit    *walk.NumberEdit
	reinforceEdit *walk.NumberEdit
	amplifyEdit   *walk.NumberEdit
	amplifyTypeCb *walk.ComboBox
	previewBtn    *walk.PushButton
	grantBtn      *walk.PushButton

	// ④-1b 群发（后台批量任务：全服/在线）
	batchTitleEdit  *walk.LineEdit
	batchBodyEdit   *walk.LineEdit
	batchAllBtn     *walk.PushButton
	batchOnlineBtn  *walk.PushButton
	batchCancelBtn  *walk.PushButton
	batchStatusL    *walk.Label
	batchTaskID     string // 正在跑的群发任务 id（空=没有在跑）
	batchPolling    bool   // 进度轮询中（防重复起轮询）

	// ④-1c/④-3b/④-5b 另外两个后台批量任务的状态（账号批量的骨架见 account_batch.go）
	mbBatchTaskID     string // 正在跑的"删除全服邮件"任务 id
	mbBatchPolling    bool
	assetBatchTaskID  string // 正在跑的"批量资产邮件"任务 id
	assetBatchPolling bool

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
	// ④-3b 资产邮件批量（发放范围由 assetTargetCb 决定，发起按钮就是 assetSendBtn，见 account_batch.go）
	assetTargetCb       *walk.ComboBox
	assetDryRunChk      *walk.CheckBox
	assetBatchCancelBtn *walk.PushButton
	assetBatchStatusL   *walk.Label

	// ④-4 成长与解锁
	sptpSPEdit   *walk.LineEdit // 留空=不修改
	sptpTPEdit   *walk.LineEdit
	jobLoadBtn   *walk.PushButton
	sptpBtn      *walk.PushButton
	levelEdit    *walk.NumberEdit
	levelBtn     *walk.PushButton
	job1Cb       *walk.ComboBox
	job2Cb       *walk.ComboBox
	job3Cb       *walk.ComboBox
	jobApplyBtn  *walk.PushButton
	// jobCareerBtn 跨大系转职（独立按钮，需角色离线）。理由见 onAcctJobCareerApply。
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
	// ④-5b 批量删除（全服 / 在线，服务端后台任务，见 account_batch.go）
	mbClearAllBtn    *walk.PushButton
	mbClearOnlineBtn *walk.PushButton
	mbDryRunChk      *walk.CheckBox
	mbBatchCancelBtn *walk.PushButton
	mbBatchStatusL   *walk.Label

	// ⑤ 本页日志
	logEdit *walk.TextEdit

	// ⑥ 在线角色表（AC-13）
	onlineRefreshBtn *walk.PushButton
	onlineKickSelBtn *walk.PushButton // 踢下勾选的在线玩家
	onlineKickAllBtn *walk.PushButton // 踢下全部在线玩家
	onlineSummaryL   *walk.Label
	onlineTable      *walk.TableView

	// 状态
	pvfPath       string               // 本地搜索/索引用 PVF 路径（空=未选择）
	selRole       *acctCharacterRow    // 当前选中角色（值副本）
	selItem       *pvfindex.SearchView // 物品发放选中物品（值副本）
	jobGot        bool                 // 已载入转职现状
	// curJob 载入现状时的**职业大类**（0..15）。用来判断"这次转职是不是跨大系"：
	// 服务端的 /character/job 只允许同系换分支/觉醒，跨大系必须走 career 接口。
	curJob int
	itemPage      int                  // 物品搜索当前页（1 起；0=未搜索）
	itemPageCount int                  // 物品搜索总页数
}

var acctW acctTabWidgets
var acctOnlineMdl acctOnlineModel
var acctRoleMdl acctRoleModel
var acctItemMdl acctItemModel

// buildAcctTab 账号管理 TabPage（插在服务器选项之后、封禁之前，见 main.go buildAllPages）。
func buildAcctTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "账号管理",
		Content: decl.Composite{Layout: decl.VBox{MarginsZero: true, SpacingZero: true}, Children: a.accountTab()},
	}
}

// accountTab 账号管理页控件树（ScrollView 垂直滚动承载全部功能区）。
func (a *App) accountTab() []decl.Widget {
	// 页面初值：优先本地持久化的 PVF 路径（config.json acct_pvf_path），免去每次手动选择。
	acctW.pvfPath = a.cfg.AcctPVFPath
	return []decl.Widget{
		decl.ScrollView{
			HorizontalFixed: true,
			Layout:          decl.VBox{Margins: decl.Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []decl.Widget{
				// ① 连接与角色查询条
				decl.Composite{
					Layout: decl.VBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Composite{
							Layout: decl.HBox{MarginsZero: true, Spacing: 6},
							Children: []decl.Widget{
								decl.Label{Text: "本地PVF(搜索用):"},
								decl.LineEdit{AssignTo: &acctW.pvfEdit, ReadOnly: true, Text: acctW.pvfPath, MinSize: decl.Size{Width: 300, Height: 0}, ToolTipText: "当前本地用于物品搜索的 Script.pvf 路径（点「选择PVF...」指定）"},
								decl.PushButton{AssignTo: &acctW.probeBtn, Text: "执行端PVF状态", OnClicked: a.onAcctProbePVF, ToolTipText: "查询执行端 instance.json PVF 索引状态（AC-12），用于与本地点选 PVF 比对一致性"},
								decl.PushButton{AssignTo: &acctW.pickBtn, Text: "选择PVF...", OnClicked: a.onAcctPickPVF, ToolTipText: "手动选择 Script.pvf（仅影响本地物品搜索；发放校验仍以执行端 PVF 为准）"},
							},
						},
						decl.Composite{
							Layout: decl.HBox{MarginsZero: true, Spacing: 6},
							Children: []decl.Widget{
								decl.Label{Text: "查询维度:"},
								decl.ComboBox{AssignTo: &acctW.dimCombo, Model: acctQueryDimLabels, MinSize: decl.Size{Width: 90, Height: 0}},
								decl.LineEdit{AssignTo: &acctW.queryEdit, MinSize: decl.Size{Width: 180, Height: 0}, ToolTipText: "输入账号名或角色名后查询（不支持空=全量）"},
								decl.PushButton{AssignTo: &acctW.queryBtn, Text: "查询角色", OnClicked: a.onAcctQueryRoles},
								decl.PushButton{AssignTo: &acctW.refreshBalBtn, Text: "刷新余额", OnClicked: a.onAcctRefreshBalance, ToolTipText: "查询选中角色当前金币/点券余额"},
							},
						},
					},
				},
				// ② 角色结果区
				decl.Composite{
					Layout: decl.VBox{MarginsZero: true, Spacing: 3},
					Children: []decl.Widget{
						decl.Label{Text: "角色查询结果（点击行选中）:"},
						decl.TableView{
							AssignTo: &acctW.roleTable,
							Columns: []decl.TableViewColumn{
								{Title: "角色名", Width: 150},
								// 「账号」列显示**真实账号名**（账号主键不可逆，统一在 account_names.go 里翻译）
								{Title: "账号", Width: 190},
								{Title: "等级", Width: 60},
								{Title: "金币", Width: 110},
								{Title: "点券", Width: 110},
								{Title: "在线", Width: 60},
							},
							Model:                 new(acctRoleModel),
							MinSize:               decl.Size{Width: 860, Height: 130},
							OnCurrentIndexChanged: a.onAcctRoleSelected,
						},
					},
				},
				// ③ 选中角色摘要
				decl.Label{AssignTo: &acctW.summaryL, Text: "未选择角色", MinSize: decl.Size{Width: 860, Height: 0}, ToolTipText: "选中角色后自动载入转职现状（AC-8）；SP/TP 无 GET 端点，请手动填写"},
				// ④ 在线角色表（AC-13）：点一次「刷新」取一份快照，不做自动轮询。
				// 选中其中一行与在「角色查询结果」里选中完全等价，可直接作为下方物品发放/邮件/成长解锁的操作依据。
				decl.Composite{
					Layout: decl.VBox{MarginsZero: true, Spacing: 3},
					Children: []decl.Widget{
						decl.Composite{
							Layout: decl.HBox{MarginsZero: true, Spacing: 6},
							Children: []decl.Widget{
								decl.Label{Text: "在线角色表:"},
								decl.PushButton{AssignTo: &acctW.onlineRefreshBtn, Text: "刷新", OnClicked: a.onAcctOnlineRefresh, ToolTipText: "点一次取一份快照：先查登录器会话未过期的在线账号，再逐个账号查游戏内真正在线的角色（停在选人界面的不算）"},
								decl.PushButton{
									AssignTo:  &acctW.onlineKickSelBtn,
									Text:      "踢下选中",
									OnClicked: func() { a.onAcctOnlineKickSelected() },
									ToolTipText: "把表格里勾选的玩家踢下线：断开游戏连接 + 通知登录器关闭游戏进程 + 作废其登录票（需重新登录）",
								},
								decl.PushButton{
									AssignTo:  &acctW.onlineKickAllBtn,
									Text:      "全部踢下线",
									OnClicked: func() { a.onAcctOnlineKickAll() },
									ToolTipText: "把当前所有在线玩家踢下线（同样会断开游戏连接、关闭游戏进程、作废登录票）",
								},
								decl.Label{AssignTo: &acctW.onlineSummaryL, Text: "未刷新（点「刷新」获取一份快照）"},
							},
						},
						decl.TableView{
							AssignTo: &acctW.onlineTable,
							Columns: []decl.TableViewColumn{
								{Title: "登录账号", Width: 130},
								{Title: "角色名", Width: 140},
								{Title: "等级", Width: 60},
								{Title: "角色ID", Width: 150},
								// 这里原来还有一列「账号ID」：主键不可逆、运营看不懂，且左边「登录账号」
								// 已经是真实账号名，所以直接去掉（需要主键时用「角色查询」页的账号列）。
								{Title: "金币", Width: 100},
								{Title: "点券", Width: 100},
								{Title: "IP", Width: 120},
							},
							Model:                 new(acctOnlineModel),
							MinSize:               decl.Size{Width: 860, Height: 130},
							OnCurrentIndexChanged: a.onAcctOnlineSelected,
							// 勾选框列：配合下面两个踢下线按钮使用（踢下选中 / 全部踢下）
							CheckBoxes: true,
						},
					},
				},
				// ⑤ 操作子页
				decl.TabWidget{
					ContentMarginsZero: true,
					MinSize:            decl.Size{Width: 0, Height: 330},
					Pages: []decl.TabPage{
						{Title: "物品发放", Content: decl.Composite{Layout: decl.VBox{Margins: decl.Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctItemTab()}},
						{Title: "系统邮件", Content: decl.Composite{Layout: decl.VBox{Margins: decl.Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctMailTab()}},
						{Title: "资产邮件", Content: decl.Composite{Layout: decl.VBox{Margins: decl.Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctAssetTab()}},
						{Title: "邮件管理", Content: decl.Composite{Layout: decl.VBox{Margins: decl.Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctMailboxTab()}},
						{Title: "成长与解锁", Content: decl.Composite{Layout: decl.VBox{Margins: decl.Margins{6, 4, 6, 4}, Spacing: 6}, Children: a.acctGrowthTab()}},
					},
				},
				// ⑥ 本页日志
				decl.Label{Text: "本页操作日志:"},
				decl.TextEdit{AssignTo: &acctW.logEdit, ReadOnly: true, VScroll: true, MinSize: decl.Size{Width: 860, Height: 110}},
			},
		},
	}
}

// acctItemTab 物品发放子页。
func (a *App) acctItemTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "关键词(item_id/名称):"},
				decl.LineEdit{AssignTo: &acctW.itemKeyword, MinSize: decl.Size{Width: 170, Height: 0}},
				decl.Label{Text: "类别:"},
				decl.ComboBox{AssignTo: &acctW.itemCateCb, Model: acctItemCateLabels, MinSize: decl.Size{Width: 80, Height: 0}},
				decl.PushButton{AssignTo: &acctW.itemSearchBtn, Text: "搜索", OnClicked: a.onAcctItemSearch, ToolTipText: "需先在上方选择本地 PVF；首次搜索会构建内存索引（数秒）"},
				decl.Label{Text: " 选中角色后发放物品 = 系统邮件附件。"},
			},
		},
		decl.TableView{
			AssignTo: &acctW.itemTable,
			Columns: []decl.TableViewColumn{
				{Title: "item_id", Width: 90},
				{Title: "名称", Width: 300},
				{Title: "类别", Width: 70},
				{Title: "堆叠上限", Width: 80},
				{Title: "发放位置", Width: 130},
				{Title: "可强化", Width: 60},
			},
			Model:                 new(acctItemModel),
			MinSize:               decl.Size{Width: 860, Height: 110},
			OnCurrentIndexChanged: a.onAcctItemSelected,
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{AssignTo: &acctW.itemPrevBtn, Text: "上一页", Enabled: false, OnClicked: a.onAcctItemPrev},
				decl.PushButton{AssignTo: &acctW.itemNextBtn, Text: "下一页", Enabled: false, OnClicked: a.onAcctItemNext},
				decl.Label{AssignTo: &acctW.itemPageL, Text: "第 0/0 页", MinSize: decl.Size{Width: 120, Height: 0}},
			},
		},
		decl.Label{AssignTo: &acctW.itemDetailL, Text: "未选择物品", MinSize: decl.Size{Width: 860, Height: 0}},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "数量:"},
				decl.NumberEdit{AssignTo: &acctW.amountEdit, MinValue: 1, MaxValue: 999999999, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 90, Height: 0}},
				decl.Label{Text: "强化(0-31):"},
				decl.NumberEdit{AssignTo: &acctW.reinforceEdit, MinValue: 0, MaxValue: 31, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 70, Height: 0}},
				decl.Label{Text: "增幅(0-31):"},
				decl.NumberEdit{AssignTo: &acctW.amplifyEdit, MinValue: 0, MaxValue: 31, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 70, Height: 0}},
				decl.Label{Text: "增幅属性:"},
				decl.ComboBox{
					AssignTo: &acctW.amplifyTypeCb, Model: acctAmplifyLabels, MinSize: decl.Size{Width: 80, Height: 0},
					ToolTipText: "按游戏内叫法选即可：工具会自动换算为服务端线值（力量=3/智力=4/体力=1/精神=2）",
				},
			},
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{AssignTo: &acctW.previewBtn, Text: "预览", OnClicked: a.onAcctItemPreview},
				decl.PushButton{AssignTo: &acctW.grantBtn, Text: "发放(系统邮件附件)", OnClicked: a.onAcctItemGrant, MinSize: decl.Size{Width: 140, Height: 0}},
				decl.Label{Text: " 提示：仅装备类可填强化/增幅/增幅属性；选中道具后这三项自动禁用。"},
			},
		},
		// ── 群发（后台异步任务 + 进度）──────────────────────────────
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "群发邮件标题(可空):"},
				decl.LineEdit{
					AssignTo:    &acctW.batchTitleEdit,
					MaxLength:   40,
					MinSize:     decl.Size{Width: 150, Height: 0},
					ToolTipText: "留空用默认「GM 物品发放」",
				},
				decl.Label{Text: "正文(可空):"},
				decl.LineEdit{
					AssignTo:    &acctW.batchBodyEdit,
					MaxLength:   400,
					MinSize:     decl.Size{Width: 260, Height: 0},
					ToolTipText: "留空用默认「系统发放：<物品名>（item_id）」",
				},
			},
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{
					AssignTo:    &acctW.batchAllBtn,
					Text:        "全服发送(每账号首角色)",
					OnClicked:   func() { a.onAcctItemGrantBatch(batchScopeAllAccounts) },
					MinSize:     decl.Size{Width: 190, Height: 0},
					ToolTipText: "给每个账号的第一个角色（角色槽位号最小的那个）各发一封；没有角色的账号自动跳过。服务端后台执行，下面显示进度。",
				},
				decl.PushButton{
					AssignTo:    &acctW.batchOnlineBtn,
					Text:        "在线玩家发送",
					OnClicked:   func() { a.onAcctItemGrantBatch(batchScopeOnlineCharacters) },
					MinSize:     decl.Size{Width: 140, Height: 0},
					ToolTipText: "给当前在线的每个角色各发一封（同一账号双开两个角色都会收到）",
				},
				decl.PushButton{
					AssignTo:    &acctW.batchCancelBtn,
					Text:        "取消群发",
					Enabled:     false,
					OnClicked:   func() { a.onAcctBatchCancel() },
					ToolTipText: "取消正在执行的群发：已经发出去的不回滚，剩下的不再发",
				},
				decl.Label{AssignTo: &acctW.batchStatusL, Text: "群发：未执行", TextColor: walk.RGB(120, 120, 120)},
			},
		},
	}
}

// acctMailTab 系统邮件子页。
func (a *App) acctMailTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "收件角色:"},
				decl.Label{AssignTo: &acctW.mailRoleL, Text: "（未选中）", MinSize: decl.Size{Width: 180, Height: 0}},
				decl.Label{Text: "标题:"},
				decl.LineEdit{AssignTo: &acctW.mailTitleEdit, Text: "GM 系统邮件", MaxLength: 40, MinSize: decl.Size{Width: 260, Height: 0}},
				decl.Label{Text: "金币:"},
				decl.NumberEdit{AssignTo: &acctW.mailGoldEdit, MinValue: 0, MaxValue: 4294967295, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 110, Height: 0}},
			},
		},
		decl.Label{Text: "正文(≤400字):"},
		decl.TextEdit{AssignTo: &acctW.mailBodyEdit, MaxLength: 400, MinSize: decl.Size{Width: 860, Height: 70}},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "物品附件 item_id(留空=无):"},
				decl.LineEdit{AssignTo: &acctW.mailItemEdit, MinSize: decl.Size{Width: 110, Height: 0}, ToolTipText: "直接填 item_id 即可；也可在「物品发放」页搜索后参考"},
				decl.Label{Text: "数量:"},
				decl.LineEdit{AssignTo: &acctW.mailCntEdit, MinSize: decl.Size{Width: 70, Height: 0}},
				decl.Label{Text: "绑定:"},
				decl.ComboBox{AssignTo: &acctW.mailBindCb, Model: acctMailBindLabels, MinSize: decl.Size{Width: 90, Height: 0}},
				decl.PushButton{AssignTo: &acctW.mailSendBtn, Text: "发送邮件", OnClicked: a.onAcctMailSend},
				decl.Label{Text: " 标题/正文/金币/物品附件至少一项。"},
			},
		},
	}
}

// acctAssetTab 资产邮件子页。
//
// 只有一个动作按钮：**发给谁由「发放给」下拉决定**。
// 以前这里是「发送资产邮件」+「批量发送」两个按钮，服主实测选了"在线角色"却去点了左边那个
// （它只认选中的角色），弹出"请先选择一个角色"，看着像 bug。一个按钮 + 目标下拉，
// 点下去的行为与下拉里写的一致，不给二义性留位置。
func (a *App) acctAssetTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "发放方式:"},
				decl.ComboBox{AssignTo: &acctW.assetModeCb, Model: acctAssetModeLabels, CurrentIndex: 0, MinSize: decl.Size{Width: 260, Height: 0}},
				decl.Label{Text: "数量:"},
				decl.NumberEdit{AssignTo: &acctW.assetAmountEdit, MinValue: 1, MaxValue: 2147483647, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 120, Height: 0}},
				decl.Label{Text: "发放给:"},
				decl.ComboBox{
					AssignTo:              &acctW.assetTargetCb,
					Model:                 acctAssetTargetLabels,
					CurrentIndex:          0,
					MinSize:               decl.Size{Width: 250, Height: 0},
					OnCurrentIndexChanged: a.onAcctAssetTargetChanged,
					ToolTipText: "「当前选中角色」= 只发给他一个；「在线角色」= 每个在线角色各一份；" +
						"「全服首个角色」= 全服创建最早的那一个（唯一），建议先勾「预演」确认是谁",
				},
				decl.PushButton{
					AssignTo:  &acctW.assetSendBtn,
					Text:      "发送资产邮件",
					OnClicked: a.onAcctAssetSendDispatch,
					ToolTipText: "按左边「发放给」的范围发放当前「发放方式 + 数量」；批量范围在服务端后台逐个处理，" +
						"可点「取消批量」（已发出/已入账的不回滚）",
				},
			},
		},
		decl.Label{AssignTo: &acctW.assetBalanceL, Text: "余额摘要：未查询（选中角色后点「刷新余额」）", MinSize: decl.Size{Width: 860, Height: 0}},
		// 批量相关的辅助控件：预演、取消、进度。发起按钮在上面那一行（见本函数顶部说明）。
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.CheckBox{AssignTo: &acctW.assetDryRunChk, Enabled: false, Text: "预演（只解析目标，不发放）", ToolTipText: "只解析会发给哪些角色并在状态行列出（最多 10 个），一个 GM 调用都不发。选「全服首个角色」时建议先预演确认是谁；「当前选中角色」不需要预演（初始置灰，选了批量范围才可用）"},
				decl.PushButton{AssignTo: &acctW.assetBatchCancelBtn, Text: "取消批量", Enabled: false, OnClicked: a.onAcctAssetBatchCancel},
				decl.Label{AssignTo: &acctW.assetBatchStatusL, Text: "批量：未执行", MinSize: decl.Size{Width: 260, Height: 0}},
			},
		},
		decl.Label{Text: " 金币邮件 → 邮件附件，玩家领取后到账；点券到账 → 直接入账 + 通知邮件。只允许增加，防止误扣。"},
		decl.Label{Text: " 批量提示：点券一旦入账无法撤回；「已入账但通知失败」不会重试，别重跑整批（会重复发券）。", TextColor: walk.RGB(140, 60, 0)},
	}
}

// acctGrowthTab 成长与解锁子页。
func (a *App) acctGrowthTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "设置剩余SP:"},
				decl.LineEdit{AssignTo: &acctW.sptpSPEdit, MinSize: decl.Size{Width: 90, Height: 0}, ToolTipText: "留空=不修改；纯数字=设置（执行端无 SP/TP 读取端点，不会自动回填）"},
				decl.Label{Text: "剩余TP:"},
				decl.LineEdit{AssignTo: &acctW.sptpTPEdit, MinSize: decl.Size{Width: 90, Height: 0}, ToolTipText: "留空=不修改；纯数字=设置"},
				decl.PushButton{AssignTo: &acctW.jobLoadBtn, Text: "载入转职现状", OnClicked: a.onAcctGrowthLoad},
				decl.PushButton{AssignTo: &acctW.sptpBtn, Text: "更新SP/TP", OnClicked: a.onAcctSPTPUpdate},
			},
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "新等级:"},
				decl.NumberEdit{AssignTo: &acctW.levelEdit, MinValue: 1, MaxValue: 90, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 90, Height: 0}},
				decl.PushButton{AssignTo: &acctW.levelBtn, Text: "设置等级", OnClicked: a.onAcctLevelSet},
			},
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "职业:"},
				decl.ComboBox{AssignTo: &acctW.job1Cb, Model: acctAllJobNames(), MinSize: decl.Size{Width: 150, Height: 0}, OnCurrentIndexChanged: a.onAcctJob1Changed, ToolTipText: "点「载入转职现状」自动回填；选择职业大类后下方转职/觉醒下拉自动联动"},
				decl.Label{Text: "转职:"},
				decl.ComboBox{AssignTo: &acctW.job2Cb, Model: acctGrowNamesByJob(0), MinSize: decl.Size{Width: 150, Height: 0}, OnCurrentIndexChanged: a.onAcctJob2Changed, ToolTipText: "按职业大类联动显示可选转职"},
				decl.Label{Text: "觉醒:"},
				decl.ComboBox{AssignTo: &acctW.job3Cb, Model: acctAwakeNamesByGrow(0, 0), MinSize: decl.Size{Width: 150, Height: 0}, ToolTipText: "未觉醒 / 一次觉醒 / 二次觉醒（按转职分支提供可选觉醒名）"},
				decl.PushButton{AssignTo: &acctW.jobApplyBtn, Text: "应用转职", OnClicked: a.onAcctJobApply, ToolTipText: "同系换分支/觉醒：按三级下拉一次性提交 job/first_grow/second_grow；需先「载入转职现状」"},
				decl.PushButton{AssignTo: &acctW.jobCareerBtn, Text: "跨大系转职", OnClicked: a.onAcctJobCareerApply, ToolTipText: "换职业大类（如鬼剑士→魔法师）。走服务端 career 接口：会重建技能、重算属性、清转职任务，且【角色必须离线】；装扮/武器可能因职业限制失效"},
			},
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "黑钻时长(天):"},
				decl.NumberEdit{
					AssignTo: &acctW.bdDaysEdit, Value: 30.0, MinValue: 1, MaxValue: 3650,
					Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 80, Height: 0},
					ToolTipText: "黑钻按天算（1~3650）。黑钻是账号级权益：同一账号下所有角色共享，疲劳上限提升到 188。",
				},
				decl.PushButton{AssignTo: &acctW.bdBtn, Text: "开启黑钻", OnClicked: a.onAcctBlackDiamond, ToolTipText: "给选中角色所属账号开启/续期黑钻：到期时间 = max(现在, 当前到期时间) + 天数"},
				decl.PushButton{AssignTo: &acctW.bdQueryBtn, Text: "查询黑钻", OnClicked: a.onAcctBlackDiamondQuery, ToolTipText: "只读查询该角色所属账号的黑钻现状（不改任何数据）；选中角色时会自动查一次"},
				decl.Label{Text: "常用:"},
				decl.PushButton{Text: "30天", OnClicked: func() { a.onAcctBlackDiamondPreset(30) }},
				decl.PushButton{Text: "90天", OnClicked: func() { a.onAcctBlackDiamondPreset(90) }},
				decl.PushButton{Text: "365天", OnClicked: func() { a.onAcctBlackDiamondPreset(365) }},
			},
		},
		decl.Label{
			AssignTo: &acctW.bdStateL, Text: "黑钻状态：选中角色后自动查询",
			MinSize:     decl.Size{Width: 860, Height: 0},
			ToolTipText: "黑钻是账号级权益：同账号角色共享同一到期时间。数据来自执行端（优先服务端只读接口，服务端未更新时自动退回直读游戏库）。",
		},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{AssignTo: &acctW.dungeonBtn, Text: "副本全部解锁", OnClicked: a.onAcctDungeonsUnlock, ToolTipText: "按执行端嵌入 dungeons_max_difficulty.json 的全副本最高难度解锁"},
				decl.PushButton{AssignTo: &acctW.equipSlotBtn, Text: "装备槽全部解锁", OnClicked: a.onAcctEquipSlotsUnlock},
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

// acctLog 在主线程向本页日志追加一行（含时间戳；非主线程调用方必须先经 a.sync 回调）。
var acctLogLimitOnce sync.Once

// acctLogScrollBottom 将日志框滚动到底部（须在主线程调用）。
func acctLogScrollBottom(edit *walk.TextEdit) {
	if edit == nil {
		return
	}
	if h := edit.Handle(); h != 0 {
		win.SendMessage(h, win.WM_VSCROLL, win.SB_BOTTOM, 0)
	}
}

func acctLog(format string, args ...any) {
	if acctW.logEdit == nil {
		return
	}
	// 放开 EDIT 默认 32K 字符上限，避免日志超长后 AppendText 被静默丢弃。
	acctLogLimitOnce.Do(func() {
		if h := acctW.logEdit.Handle(); h != 0 {
			win.SendMessage(h, win.EM_SETLIMITTEXT, 0x7FFFFFFE, 0)
		}
	})
	acctW.logEdit.AppendText(fmt.Sprintf("[%s] %s\r\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
	acctLogScrollBottom(acctW.logEdit)
}

// acctBusyUI 任务期间禁用/恢复本页全部操作按钮（须在 GUI 线程调用）。
func (a *App) acctBusyUI(busy bool) {
	buttons := []*walk.PushButton{
		acctW.probeBtn, acctW.pickBtn, acctW.queryBtn, acctW.refreshBalBtn,
		acctW.itemSearchBtn, acctW.itemPrevBtn, acctW.itemNextBtn, acctW.previewBtn, acctW.grantBtn,
		acctW.batchAllBtn, acctW.batchOnlineBtn,
		acctW.mailSendBtn, acctW.assetSendBtn,
		acctW.jobLoadBtn, acctW.sptpBtn, acctW.levelBtn, acctW.jobApplyBtn, acctW.jobCareerBtn,
		acctW.dungeonBtn, acctW.equipSlotBtn,
		acctW.mbRefreshBtn, acctW.mbDelBtn, acctW.mbClearBtn,
		acctW.mbClearAllBtn, acctW.mbClearOnlineBtn,
	}
	for _, btn := range buttons {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
	// 定向删除邮件还额外受"角色是否在线"约束，忙碌态恢复时按在线状态重判一次。
	if !busy {
		a.acctMailboxApplyOnlineUI()
	}
}

// acctClient 返回当前登录客户端；未登录/已登出时返回可读错误。
//
// 为什么要有这个包装：登出或 token 失效时 requestRelogin 会把 a.client 置空，
// 而 a.acctRun 的 job 闭包是在后台 goroutine 里跑的——如果直接在闭包里写
// `a.snapshotClient().Xxx(...)`，就可能在"某个耗时操作执行期间用户点了登出"时解引用 nil。
// 那个 panic 会被 acctRun 的 recover 转成"操作内部异常: invalid memory address"，
// 它不是 *APIError，handleAPIError 不会触发重新登录，用户就卡在一个已死会话上，
// 还从日志里看不出真实原因。所有 job 一律先取这个包装、失败即 return err。
func (a *App) acctClient() (*APIClient, error) {
	if c := a.snapshotClient(); c != nil {
		return c, nil
	}
	return nil, errors.New("未连接服务器（已登出或令牌失效），请重新登录后再操作")
}

// acctRun 账号管理页统一后台任务入口：opMu 全局互斥 + 后台 goroutine + 主线程回调。
// job 内禁止触碰 UI；回调（done）在 GUI 线程执行。
func (a *App) acctRun(label string, job func() error, done func(err error)) {
	if !a.beginOp() {
		acctLog("已有管理操作进行中，请稍候")
		return
	}
	acctLog("处理中：%s ...", label)
	a.acctBusyUI(true)
	go func() {
		// 兜底 recover：job 内部一旦 panic（例如令牌过期时 client 已被置空后又被取用），
		// 旧实现会直接终止整个进程；这里转成错误交给统一错误处理，工具不崩。
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
			a.acctBusyUI(false)
			a.endOp()
			if err != nil {
				if a.handleAPIError(err) {
					return
				}
				acctLog("失败：%v", err)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// acctSelectedRole 返回当前选中角色（无则日志提示并返回 nil）。
func (a *App) acctSelectedRole() *acctCharacterRow {
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

// acctNum NumberEdit 当前整数值（非法/空按 0）。
func acctNum(edit *walk.NumberEdit) int {
	if edit == nil {
		return 0
	}
	return int(edit.Value())
}

// acctSummarize 更新选中角色摘要标签（在主线程调用）。
func (a *App) acctSummarize() {
	if acctW.selRole == nil {
		acctW.summaryL.SetText("未选择角色")
		return
	}
	r := acctW.selRole
	text := fmt.Sprintf("角色: %s | 账号: %s | 等级: %d | 金币: %d | 点券: %d | 在线: %s",
		r.Name, acctNameText(r.AccountID), r.Level, r.Gold, r.Cera, acctOnlineText(r.Online))
	if acctW.jobGot {
		if job, first, second, ok := acctJobSelections(); ok {
			text += " | 职业: " + acctJobDisplayName(job, first, second)
		}
	}
	acctW.summaryL.SetText(text)
}

func acctHumanSize(n int64) string {
	if n >= 1024*1024*1024 {
		return fmt.Sprintf("%.1f GB", float64(n)/1024/1024/1024)
	}
	if n >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	}
	return fmt.Sprintf("%d B", n)
}

// ---- ① 查询条事件 ----

// onAcctProbePVF 查询执行端 PVF 索引状态（AC-12），用于与本地点选 PVF 比对一致性。
func (a *App) onAcctProbePVF() {
	var st *acctPVFStatusOut
	a.acctRun("查询执行端PVF状态", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		st, err = cl.AcctPVFIndexStatus(context.Background(), "")
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctLog("执行端 PVF 索引：%s（大小 %s，条目 %d）", st.Path, acctHumanSize(st.Size), st.Entries)
		if st.Path != "" && acctW.pvfPath != "" && strings.EqualFold(strings.TrimRight(st.Path, "/\\"), strings.TrimRight(acctW.pvfPath, "/\\")) {
			acctLog("本地所选 PVF 与执行端路径一致（可直接搜索发放）")
		} else {
			acctLog("提示：本地搜索请点「选择PVF...」选取同一份 Script.pvf；发放仍以执行端校验为准")
		}
	})
}

// onAcctPickPVF 手动选择 Script.pvf（仅影响本页搜索/预览；发放校验仍以执行端 PVF 为准）。
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
	a.cfg.AcctPVFPath = acctW.pvfPath
	if err := a.store.Save(a.cfg); err != nil {
		acctLog("本地 PVF 路径持久化失败: %v", err)
		return
	}
	acctLog("已手动指定本地 PVF：%s（已记住，下次启动自动回填；提示：建议与执行端 instance.json game.pvfPath 保持一致）", acctW.pvfPath)
}

// onAcctQueryRoles 按账号名/角色名查询角色（AC-1；不支持空=全量）。
func (a *App) onAcctQueryRoles() {
	keyword := strings.TrimSpace(acctW.queryEdit.Text())
	if keyword == "" {
		acctLog("请输入账号名或角色名")
		return
	}
	dim := acctW.dimCombo.CurrentIndex()
	var username, characterName string
	if dim == 0 {
		username = keyword
	} else {
		characterName = keyword
	}
	var rows []acctCharacterRow
	a.acctRun("查询角色", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		rows, err = cl.AcctCharacters(context.Background(), username, characterName)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctRoleMdl.items = rows
		// 账号列要显示真实账号名：上表前先把这批主键批量翻译掉
		// （翻译失败只会退回显示主键，不会影响本次查询结果）。
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.AccountID)
		}
		a.resolveAccountNames(acctNameList(ids...))
		acctW.roleTable.SetModel(&acctRoleMdl)
		acctW.selRole = nil
		acctW.jobGot = false
		acctW.selItem = nil
		acctW.itemDetailL.SetText("未选择物品")
		acctW.assetBalanceL.SetText("余额摘要：未查询（选中角色后点「刷新余额」）")
		a.acctSummarize()
		acctLog("查询完成：共 %d 名角色。点击行选中后可操作四子页", len(rows))
	})
}

// onAcctOnlineRefresh 拉取一份在线角色快照（AC-13）。
// 点一次刷新取一份，不做自动轮询：逐账号查 GM 角色接口对服务端有开销，按需触发即可。
func (a *App) onAcctOnlineRefresh() {
	var out *onlineCharactersOut
	a.acctRun("刷新在线角色", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		out, err = cl.AcctOnlineCharacters(context.Background())
		return err
	}, func(err error) {
		if err != nil || out == nil {
			return
		}
		// reset 会连勾选状态一起清空：换一批数据后旧勾选不能留在新行上。
		acctOnlineMdl.reset(out.Characters)
		// 快照里服务端已经带了登录账号名，顺手喂给全局缓存（其它页签的主键翻译就不用再问一次）。
		known := make(map[string]string, len(out.Characters))
		for _, row := range out.Characters {
			if row.AccountID != "" && row.Username != "" {
				known[row.AccountID] = row.Username
			}
		}
		acctNames.remember(known)
		// 勾选在线表某一行 = 选中该角色作为操作依据（与点行完全等价）。
		// 每次刷新都设一遍：回调只依赖包级模型，重复赋值没有副作用，也避免"第一次刷新前勾选"的窗口。
		acctOnlineMdl.onCheck = func(index int) {
			if index < 0 || index >= len(acctOnlineMdl.items) {
				return
			}
			a.acctApplyRoleSelection(acctOnlineToCharacterRow(acctOnlineMdl.items[index]), "在线角色表（勾选）", true)
		}
		acctW.onlineTable.SetModel(&acctOnlineMdl)

		summary := fmt.Sprintf("在线角色 %d 个（检查在线账号 %d 个", len(out.Characters), out.Accounts)
		if out.Failures > 0 {
			summary += fmt.Sprintf("，%d 个查询失败", out.Failures)
		}
		summary += "）"
		if t, perr := time.Parse(time.RFC3339, out.CheckedAt); perr == nil {
			summary += " · 快照 " + t.Local().Format("15:04:05")
		}
		acctW.onlineSummaryL.SetText(summary)
		acctLog("在线角色快照：%d 个（检查账号 %d，失败 %d）", len(out.Characters), out.Accounts, out.Failures)
	})
}

// onAcctOnlineKickSelected 踢下勾选的在线玩家。
//
// 按**账号**踢（同一账号双开两个角色都在线时只踢一次）：踢下线在服务端是
// 「断开该账号的在线游戏连接 + 写 GM 踢人事件 + 作废该账号全部登录票」，
// 账号级动作做两遍没有意义。
func (a *App) onAcctOnlineKickSelected() {
	rows := acctOnlineMdl.checkedCharacters()
	if len(rows) == 0 {
		a.showInfo("未勾选", "请先勾选要踢下线的玩家（表格最左侧的勾选框），或直接用「全部踢下线」。")
		return
	}
	names := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.Username)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		a.showInfo("未勾选", "勾选的行里没有有效的登录账号。")
		return
	}
	a.kickOnlineConfirm(fmt.Sprintf("确认踢下勾选的 %d 个账号？", len(names)),
		fmt.Sprintf("账号：%s", strings.Join(names, "、")), names)
}

// onAcctOnlineKickAll 踢下当前全部在线玩家。
func (a *App) onAcctOnlineKickAll() {
	// 优先用表格里已有快照的数量提示；没刷过表就说"全部在线玩家"。
	hint := "当前全部在线玩家"
	if n := len(acctOnlineMdl.items); n > 0 {
		hint = fmt.Sprintf("当前在线角色表中的 %d 个在线角色（全部在线玩家）", n)
	}
	a.kickOnlineConfirm("确认踢下全部在线玩家？",
		fmt.Sprintf("%s\n\n所有在线玩家会被断开游戏连接、登录器会关闭游戏进程，且需重新登录。", hint), nil)
}

// kickOnlineConfirm 二次确认后调用批量踢下线端点（names 为空 = 全部）。
func (a *App) kickOnlineConfirm(title, detail string, names []string) {
	if !a.confirm(title, detail+"\n\n是否继续？") {
		return
	}
	a.acctRun(title, func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		var res *AdminKickResult
		var err error
		if names == nil {
			res, err = cl.AcctKickAll(context.Background())
		} else {
			res, err = cl.AcctKickUsers(context.Background(), names)
		}
		if err != nil {
			return err
		}
		a.lastKickResult = res
		return nil
	}, func(err error) {
		if err != nil {
			acctLog("批量踢下线失败：%v", err)
			a.showError("批量踢下线失败", err)
			return
		}
		res := a.lastKickResult
		if res == nil {
			return
		}
		msg := fmt.Sprintf("请求账号 %d 个\n断开游戏连接 %d 个\n作废登录票 %d 张",
			res.Requested, res.Kicked, res.Sessions)
		if len(res.Failed) > 0 {
			msg += fmt.Sprintf("\n\n%d 个未成功：\n- %s", len(res.Failed), strings.Join(res.Failed, "\n- "))
		}
		a.showInfo("踢下线完成", msg)
		acctLog("批量踢下线：请求 %d，断开 %d，作废票 %d，失败 %d",
			res.Requested, res.Kicked, res.Sessions, len(res.Failed))
		// 踢完刷新一次列表：被踢下线的角色应从在线表里消失
		a.onAcctOnlineRefresh()
	})
}

// ---- ④-1b 群发邮件（全服 / 在线，服务端后台任务） ----

// 群发范围（与服务器端 manage 包的常量对齐）。
const (
	batchScopeAllAccounts      = "all-accounts"      // 每个账号的第一个角色（槽位最小）
	batchScopeOnlineCharacters = "online-characters" // 每个在线角色
)

// onAcctItemGrantBatch 发起一次群发：物品/数量/强化等沿用当前页面的选择，
// 标题正文可留空（留空用服务端默认文案）。
//
// 服务端是**异步**的：这里只负责发起 + 轮询进度；上千账号的循环不会把界面卡住。
func (a *App) onAcctItemGrantBatch(scope string) {
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
	if acctW.batchTaskID != "" {
		a.showInfo("已有群发在执行", "上一批群发还没结束（"+acctW.batchTaskID+"），请等它完成或先点「取消群发」。")
		return
	}

	scopeText := "全服每个账号的第一个角色（槽位号最小）"
	if scope == batchScopeOnlineCharacters {
		scopeText = "当前在线的每个角色"
	}
	detail := fmt.Sprintf("将群发物品邮件：\n\n物品：%s（item_id=%d）\n数量：%d", item.Name, item.ItemID, amount)
	if reinforce > 0 {
		detail += fmt.Sprintf("\n强化：+%d", reinforce)
	}
	if amplify > 0 {
		detail += fmt.Sprintf("\n增幅：+%d（%s）", amplify, acctAmplifyLabel(amplifyType))
	}
	detail += "\n\n收件范围：" + scopeText +
		"\n\n收件人可能有几百上千个，服务端会在后台逐个发送，进度显示在按钮右侧；" +
		"过程中可以点「取消群发」停下（已发出的不回滚）。"
	if !item.SupportsUpgrade && (reinforce > 0 || amplify > 0) {
		detail += "\n\n注意：该物品不支持强化/增幅，发放会被执行端拒绝。"
	}
	if !a.confirm("确认群发物品邮件？", detail) {
		return
	}

	req := BatchGrantRequest{
		Scope:          scope,
		ItemID:         item.ItemID,
		Amount:         uint32(amount),
		ReinforceLevel: reinforce,
		AmplifyLevel:   amplify,
		AmplifyType:    amplifyType,
		Title:          strings.TrimSpace(leText(acctW.batchTitleEdit)),
		Body:           strings.TrimSpace(leText(acctW.batchBodyEdit)),
	}
	var task *BatchTask
	a.acctRun("群发物品邮件", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		var err error
		task, err = cl.AcctItemsGrantBatch(context.Background(), req)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("群发发起失败：%v", err)
			a.showError("群发发起失败", err)
			return
		}
		if task == nil {
			return
		}
		acctW.batchTaskID = task.ID
		acctLog("群发已开始：任务 %s（%s）", task.ID, scopeText)
		a.acctSetBatchBusy(true)
		a.applyBatchTaskToUI(task)
		go a.pollBatchTask(task.ID)
	})
}

// pollBatchTask 轮询任务进度直到结束（每 1.2 秒一次；结束或出错就停）。
func (a *App) pollBatchTask(id string) {
	if acctW.batchPolling {
		return
	}
	acctW.batchPolling = true
	defer func() { acctW.batchPolling = false }()

	for i := 0; i < 3600; i++ { // 上限约 72 分钟，防止异常情况下无限轮询
		time.Sleep(1200 * time.Millisecond)
		cl := a.snapshotClient()
		if cl == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		task, err := cl.TaskGet(ctx, id)
		cancel()
		if err != nil {
			// 会话失效会被 snapshotClient/handleAPIError 那条路处理，这里静默退出。
			a.sync(func() {
				acctW.batchTaskID = ""
				a.acctSetBatchBusy(false)
				if acctW.batchStatusL != nil {
					acctW.batchStatusL.SetText("群发：进度查询失败（任务可能仍在服务端执行）")
				}
			})
			return
		}
		a.sync(func() { a.applyBatchTaskToUI(task) })
		if task == nil || !task.Running() {
			a.sync(func() {
				acctW.batchTaskID = ""
				a.acctSetBatchBusy(false)
				acctLog("群发结束：%s", batchTaskSummary(task))
				if task != nil && len(task.Errors) > 0 {
					acctLog("群发失败样例：%s", strings.Join(task.Errors, " ｜ "))
				}
			})
			return
		}
	}
}

// onAcctBatchCancel 取消正在跑的群发。
func (a *App) onAcctBatchCancel() {
	id := acctW.batchTaskID
	if id == "" {
		a.showInfo("没有在跑的群发", "当前没有正在执行的群发任务。")
		return
	}
	if !a.confirm("取消群发", "确认取消群发任务 "+id+"？\n\n已经发出的邮件不会被撤回，剩余收件人不再发送。") {
		return
	}
	cl := a.snapshotClient()
	if cl == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := cl.TaskCancel(ctx, id); err != nil {
			a.sync(func() { a.showError("取消失败", err) })
			return
		}
		a.sync(func() {
			if acctW.batchStatusL != nil {
				acctW.batchStatusL.SetText("群发：正在取消…")
			}
		})
	}()
}

// acctSetBatchBusy 群发进行中禁用两个发起按钮、启用取消按钮。
func (a *App) acctSetBatchBusy(busy bool) {
	if acctW.batchAllBtn != nil {
		acctW.batchAllBtn.SetEnabled(!busy)
	}
	if acctW.batchOnlineBtn != nil {
		acctW.batchOnlineBtn.SetEnabled(!busy)
	}
	if acctW.batchCancelBtn != nil {
		acctW.batchCancelBtn.SetEnabled(busy)
	}
}

// applyBatchTaskToUI 把任务进度写进状态行。
func (a *App) applyBatchTaskToUI(task *BatchTask) {
	if task == nil || acctW.batchStatusL == nil {
		return
	}
	acctW.batchStatusL.SetText("群发：" + batchTaskSummary(task))
	if task.Running() {
		acctW.batchStatusL.SetTextColor(walk.RGB(0, 100, 180))
		return
	}
	if task.Failed > 0 {
		acctW.batchStatusL.SetTextColor(walk.RGB(180, 60, 0))
	} else {
		acctW.batchStatusL.SetTextColor(walk.RGB(0, 140, 0))
	}
}

// batchTaskSummary 任务一句话摘要（状态行与日志共用）。
func batchTaskSummary(task *BatchTask) string {
	if task == nil {
		return "无任务"
	}
	switch task.Status {
	case "running":
		if task.Phase == "collecting" {
			return "正在收集收件人…（全服范围要逐个账号查角色，稍等）"
		}
		if task.Total > 0 {
			return fmt.Sprintf("进行中 %d/%d（成功 %d，失败 %d，跳过 %d）",
				task.Done, task.Total, task.OK, task.Failed, task.Skipped)
		}
		return fmt.Sprintf("进行中（成功 %d，失败 %d，跳过 %d）", task.OK, task.Failed, task.Skipped)
	case "done":
		// 服务端的完成文案信息量更大，有就用它：里面除了计数还带着
		// 「预演：将清空 N 个角色…未删除任何邮件」「已达上限 N，全服角色可能没清完，请再次执行」
		// 「已入账但通知失败 M 个（点券已到账，请勿重试）」这些**摘要装不下**的关键信息。
		// 本地计数只是服务端没给文案时的兜底 —— 吞掉那句话就等于让运营以为全清完了/全发到了。
		if msg := strings.TrimSpace(task.Message); msg != "" {
			return msg
		}
		return fmt.Sprintf("已完成：成功 %d，失败 %d，跳过 %d", task.OK, task.Failed, task.Skipped)
	case "canceled":
		// 取消同样优先用服务端文案：那里可能还带着"已达上限没清完"这类补充。
		if msg := strings.TrimSpace(task.Message); msg != "" && msg != "已中断" {
			return msg + "（已处理的目标不会回滚）"
		}
		return fmt.Sprintf("已取消：成功 %d，失败 %d，跳过 %d（已处理的目标不会回滚）",
			task.OK, task.Failed, task.Skipped)
	case "failed":
		return "失败：" + task.Message
	default:
		return task.Status
	}
}

// onAcctRefreshBalance 刷新选中角色余额（AC-3），更新资产邮件页余额摘要与摘要行。
func (a *App) onAcctRefreshBalance() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	var wallet *acctWalletBody
	a.acctRun("刷新余额", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		wallet, _, err = cl.AcctWalletQuery(context.Background(), role.Name)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctW.assetBalanceL.SetText(fmt.Sprintf("余额摘要：角色 %s | 金币 %d | 点券 %d（在线 %s）",
			wallet.Character.Name, wallet.Character.Gold, wallet.Character.Cera, acctOnlineText(wallet.Character.Online)))
		if wallet.Character.Online != role.Online || wallet.Character.Gold != role.Gold || wallet.Character.Cera != role.Cera {
			role.Online = wallet.Character.Online
			role.Gold = wallet.Character.Gold
			role.Cera = wallet.Character.Cera
			a.acctSummarize()
		}
		acctLog("余额刷新完成：金币 %d / 点券 %d", wallet.Character.Gold, wallet.Character.Cera)
	})
}

// ---- ② 角色选中 ----

// onAcctRoleSelected 角色查询结果表行选中。
func (a *App) onAcctRoleSelected() {
	if acctW.roleTable == nil {
		return
	}
	index := acctW.roleTable.CurrentIndex()
	if index < 0 || index >= len(acctRoleMdl.items) {
		acctW.selRole = nil
		return
	}
	a.acctApplyRoleSelection(acctRoleMdl.items[index], "角色查询结果", false)
}

// onAcctOnlineSelected 在线角色表行选中：与「角色查询结果」里选中完全等价，
// 选中后即可直接作为物品发放/系统邮件/资产邮件/成长解锁的操作依据。
func (a *App) onAcctOnlineSelected() {
	if acctW.onlineTable == nil {
		return
	}
	index := acctW.onlineTable.CurrentIndex()
	if index < 0 || index >= len(acctOnlineMdl.items) {
		// 刷新后索引会重置为 -1：此时保留原选中角色，摘要行仍显示当前操作依据
		return
	}
	a.acctApplyRoleSelection(acctOnlineToCharacterRow(acctOnlineMdl.items[index]), "在线角色表", true)
}

// acctOnlineToCharacterRow 在线角色行 → 通用角色行（在线表里的行按定义都是在线角色）。
func acctOnlineToCharacterRow(o onlineCharacterOut) acctCharacterRow {
	return acctCharacterRow{
		CharacterID: o.CharacterID,
		AccountID:   o.AccountID,
		Name:        o.Name,
		Level:       o.Level,
		Gold:        o.Gold,
		Cera:        o.Cera,
		Online:      true,
	}
}

// acctApplyRoleSelection 统一处理"选中一个角色"。
// 「角色查询结果」与「在线角色表」共用，保证两处选中后的行为完全一致：
// 写入操作依据 selRole、清掉与上一个角色绑定的显示、刷新摘要与邮件收件人，并后台载入转职现状（AC-8）。
//
// fromOnline 指明这次选中来自哪张表：两张表都会写"当前操作依据"，如果同时高亮两行，
// 运营根本分不清邮件会发给谁（服主反馈："查询了角色以后选择在线列表的玩家发邮件还是到查询的角色上了"）。
// 所以这里把**另一张表**的选中态清掉：界面上高亮的行 = 实际操作对象，永远只有一个。
// 注意清除另一个表的 CurrentIndex 会触发它的 OnCurrentIndexChanged，而那两个回调对 index<0 是直接返回的，
// 不会把刚写好的 selRole 又清掉。
func (a *App) acctApplyRoleSelection(row acctCharacterRow, source string, fromOnline bool) {
	sel := row
	acctW.selRole = &sel
	if fromOnline {
		if acctW.roleTable != nil && acctW.roleTable.CurrentIndex() >= 0 {
			acctW.roleTable.SetCurrentIndex(-1)
		}
	} else if acctW.onlineTable != nil && acctW.onlineTable.CurrentIndex() >= 0 {
		acctW.onlineTable.SetCurrentIndex(-1)
	}
	acctW.jobGot = false
	// 切换角色后必须清掉上一个角色的物品详情与余额，否则会把旧角色的数据当成新角色的显示出来
	acctW.selItem = nil
	acctW.itemDetailL.SetText("未选择物品")
	acctW.assetBalanceL.SetText("余额摘要：未查询（选中角色后点「刷新余额」）")
	if acctW.mailRoleL != nil {
		acctW.mailRoleL.SetText(fmt.Sprintf("%s（账号 %s）", row.Name, acctNameText(row.AccountID)))
	}
	// 邮件管理页与"当前操作依据"强绑定：换角色必须清空列表，避免把上一个角色的 mail_id 删到新角色头上。
	a.acctMailboxResetForRole()
	acctLog("已选中角色（%s）：%s（账号 %s，等级 %d，%s）", source, row.Name, acctNameText(row.AccountID), row.Level, acctOnlineText(row.Online))
	a.acctSummarize()
	// 转职现状载入完成后接着查黑钻现状（两者都走 acctRun，必须串行）。
	a.acctLoadJobThen(row.CharacterID, row.Name, func() {
		a.acctBlackDiamondQueryAsync(&sel)
	})
}

// acctLoadJob 后台载入指定角色的转职现状（AC-8），用于摘要与成长页回填。
func (a *App) acctLoadJob(characterID, name string) {
	a.acctLoadJobThen(characterID, name, nil)
}

// acctLoadJobThen 载入转职现状后接着做一件只读的事（例如查黑钻现状）。
//
// 为什么要 follow-up 参数：acctRun 是全局互斥的（beginOp 失败就直接丢弃任务），
// 在同一个点击流程里连排两个 acctRun，第二个会被"已有管理操作进行中"吃掉。
func (a *App) acctLoadJobThen(characterID, name string, after func()) {
	// 先取客户端引用：登出/令牌过期会把 a.client 置空，此处拿不到就直接跳过，
	// 避免空指针 panic（acctRun 里另有 recover 兜底）。
	cl := a.snapshotClient()
	if cl == nil {
		acctLog("未连接服务器，跳过 %s 的转职现状载入（重新登录后再选中角色即可）", name)
		return
	}
	var job *acctJobOut
	a.acctRun("载入选中角色转职现状", func() error {
		var err error
		job, err = cl.AcctJobGet(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("载入 %s 的转职现状失败：%v（不影响基础操作）", name, err)
		} else {
			acctFillJobCombos(job.Job, job.FirstGrow, job.SecondGrow)
			acctW.jobGot = true
			acctW.curJob = job.Job
			a.acctSummarize()
			acctLog("已载入 %s 的转职现状：%s", name, acctJobDisplayName(job.Job, job.FirstGrow, job.SecondGrow))
		}
		if after != nil {
			after()
		}
	})
}

// ---- ④-1 物品发放 ----

// acctItemSearchPage 本地物品搜索（pvfindex.Ensure 懒构建 + Search 内存分页；category 对齐 GM 语义）。
// page 从 1 起；成功后才推进 itemPage/itemPageCount，失败保持原页不变。
func (a *App) acctItemSearchPage(page int) {
	if page < 1 {
		page = 1
	}
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
		acctLog("请先点击「选择PVF...」确定本地搜索用 PVF")
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
			Page:     page,
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
		acctW.itemPage = result.Page
		acctW.itemPageCount = result.PageCount
		if acctW.itemPageCount < 1 {
			acctW.itemPageCount = 1
		}
		a.acctUpdateItemPageUI()
		acctLog("物品搜索完成：命中 %d 条（第 %d/%d 页，每页 %d 条）", result.Total, result.Page, result.PageCount, result.PageSize)
	})
}

// onAcctItemSearch 搜索按钮：从第 1 页重新搜索。
func (a *App) onAcctItemSearch() {
	a.acctItemSearchPage(1)
}

// onAcctItemPrev 上一页。
func (a *App) onAcctItemPrev() {
	if acctW.itemPage <= 1 {
		return
	}
	a.acctItemSearchPage(acctW.itemPage - 1)
}

// onAcctItemNext 下一页。
func (a *App) onAcctItemNext() {
	if acctW.itemPage >= acctW.itemPageCount {
		return
	}
	a.acctItemSearchPage(acctW.itemPage + 1)
}

// acctUpdateItemPageUI 刷新翻页条（页码显示 + 上一页/下一页可用性）；须在 GUI 线程调用。
func (a *App) acctUpdateItemPageUI() {
	if acctW.itemPageL != nil {
		acctW.itemPageL.SetText(fmt.Sprintf("第 %d/%d 页", acctW.itemPage, acctW.itemPageCount))
	}
	if acctW.itemPrevBtn != nil {
		acctW.itemPrevBtn.SetEnabled(acctW.itemPage > 1)
	}
	if acctW.itemNextBtn != nil {
		acctW.itemNextBtn.SetEnabled(acctW.itemPage < acctW.itemPageCount)
	}
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

// onAcctItemGrant 发放物品（AC-5，系统邮件附件；执行端二次 resolve 校验）。
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
	var out *acctItemGrantOut
	a.acctRun("发放物品", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		out, err = cl.AcctItemGrant(context.Background(), role.Name,
			item.ItemID, uint32(amount), reinforce, amplify, amplifyType)
		return err
	}, func(err error) {
		if err == nil {
			state := "已发送系统邮件；角色上线后可领取"
			if out.Mail != nil && out.Mail.Refreshed {
				state = "已发送系统邮件，在线邮箱已实时刷新"
			}
			acctLog("物品发放成功：%s（%d）x%d → %s（%s）", itemName, item.ItemID, amount, role.Name, state)
		}
	})
}

// ---- ④-2 系统邮件 ----

// onAcctMailSend 发送系统邮件（AC-2；标题/正文/金币/物品附件至少一项；附件 item_id 留空=无）。
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
	var itemID uint32
	var itemCnt uint32
	bind := acctW.mailBindCb.CurrentIndex() == 1
	itemText := strings.TrimSpace(acctW.mailItemEdit.Text())
	cntText := strings.TrimSpace(acctW.mailCntEdit.Text())
	if itemText != "" || cntText != "" {
		if itemText == "" || cntText == "" {
			acctLog("物品附件必须同时填写 item_id 与数量（都不填则不带附件）")
			return
		}
		id64, err1 := strconv.ParseUint(itemText, 10, 32)
		cnt64, err2 := strconv.ParseUint(cntText, 10, 32)
		if err1 != nil || err2 != nil || id64 == 0 || cnt64 == 0 {
			acctLog("物品附件 item_id/数量 必须为正整数")
			return
		}
		itemID = uint32(id64)
		itemCnt = uint32(cnt64)
	}
	if itemID == 0 && gold == 0 && strings.TrimSpace(body) == "" && title == "" {
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
	var out *acctMailOut
	a.acctRun("发送系统邮件", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		out, err = cl.AcctMailSend(context.Background(),
			role.Name, title, body, gold, itemID, itemCnt, bind)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		state := "系统邮件已发送；角色上线后可领取"
		if out.Refreshed {
			state = "系统邮件已发送，在线邮箱已实时刷新"
		} else if out.Online {
			state = "系统邮件已发送；在线刷新未完成，下次打开邮箱时同步"
		}
		acctLog("%s（收件：%s）", state, role.Name)
	})
}

// ---- ④-3 资产邮件 ----

// onAcctAssetSend 资产邮件（AC-4）：金币邮件（可领取附件）/ 点券到账（直接入账+通知邮件）。
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
	if amount < 1 || amount > acctMaxWalletAmt {
		acctLog("数量必须在 1~%d 之间", acctMaxWalletAmt)
		return
	}
	var out *acctWalletMailOut
	a.acctRun("发送资产邮件", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		out, err = cl.AcctWalletMail(context.Background(), role.Name, currency, amount)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		if out.MailError != "" {
			acctLog("严重提示：点券已到账，但通知邮件发送失败（%s），请勿重复操作", out.MailError)
		}
		acctLog("资产邮件处理完成：%s（%s）", currency, acctAssetModeLabel(acctW.assetModeCb))
		if out.Message != "" {
			acctLog("%s", out.Message)
		}
		if out.Character.Name != "" {
			acctW.assetBalanceL.SetText(fmt.Sprintf("余额摘要：角色 %s | 金币 %d | 点券 %d（在线 %s）",
				out.Character.Name, out.Character.Gold, out.Character.Cera, acctOnlineText(out.Character.Online)))
		}
		a.onAcctRefreshBalance()
	})
}

// ---- ④-4 成长与解锁 ----

// onAcctGrowthLoad 手动载入转职现状并回填编辑框（选中角色时也会自动载入一次）。
func (a *App) onAcctGrowthLoad() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var job *acctJobOut
	a.acctRun("载入转职现状", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		job, err = cl.AcctJobGet(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("载入 %s 的转职现状失败：%v", name, err)
			return
		}
		acctFillJobCombos(job.Job, job.FirstGrow, job.SecondGrow)
		acctW.jobGot = true
		acctW.curJob = job.Job
		a.acctSummarize()
		acctLog("已载入 %s 转职现状：%s（可修改后应用）",
			name, acctJobDisplayName(job.Job, job.FirstGrow, job.SecondGrow))
	})
}

// onAcctSPTPUpdate 更新 SP/TP（AC-6）：SP/TP 编辑框各自留空=不修改该字段，纯数字=设置。
func (a *App) onAcctSPTPUpdate() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	spText := strings.TrimSpace(acctW.sptpSPEdit.Text())
	tpText := strings.TrimSpace(acctW.sptpTPEdit.Text())
	if spText == "" && tpText == "" {
		acctLog("至少填写 SP 或 TP 之一（留空=不修改该字段）")
		return
	}
	var sp, tp *int
	if spText != "" {
		v, err := strconv.Atoi(spText)
		if err != nil || v < 0 {
			acctLog("SP 必须为非负整数")
			return
		}
		sp = &v
	}
	if tpText != "" {
		v, err := strconv.Atoi(tpText)
		if err != nil || v < 0 {
			acctLog("TP 必须为非负整数")
			return
		}
		tp = &v
	}
	a.acctRun("更新SP/TP", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		return cl.AcctSPTPSet(context.Background(), role.CharacterID, sp, tp)
	}, func(err error) {
		if err == nil {
			logParts := []string{}
			if sp != nil {
				logParts = append(logParts, fmt.Sprintf("SP 剩余 %d", *sp))
			}
			if tp != nil {
				logParts = append(logParts, fmt.Sprintf("TP 剩余 %d", *tp))
			}
			acctLog("SP/TP 更新成功（角色 %s）：%s", role.Name, strings.Join(logParts, " / "))
		}
	})
}

// onAcctLevelSet 设置等级（AC-7；1~90）。
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
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		return cl.AcctLevelSet(context.Background(), role.CharacterID, level)
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
// 跨大系会被这里主动拦下并指路，而不是把请求发给服务端换回一句拒绝
// （服务端对 `currentJob != request.Job` 直接 400）。跨大系请用 onAcctJobCareerApply。
func (a *App) onAcctJobApply() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	job, firstGrow, secondGrow, ok := acctJobSelections()
	if !ok || (!acctW.jobGot && job == 0 && firstGrow == 0 && secondGrow == 0) {
		acctLog("请先点击「载入转职现状」读取转职现状，再修改后应用（GM 需一次性提交三值）")
		return
	}
	if acctW.jobGot && job != acctW.curJob {
		acctLog("跨大系转职不能用「应用转职」：%s → %s 属于换职业大类。",
			acctJobName(acctW.curJob), acctJobName(job))
		acctLog("请改用右边的「跨大系转职」按钮 —— 它走服务端 career 接口，会重建技能/重算属性/清转职任务，且【角色必须离线】。")
		return
	}
	a.acctRun("应用转职", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		return cl.AcctJobSet(context.Background(), role.CharacterID, job, firstGrow, secondGrow)
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
// 走 /manage/v1/account/character/career → 运维工具转发 → 服务端
// `POST /dnf/gm/characters/{id}/career`。这是服务端**为跨系实现**的那一套：
// 真写 job（不只是 grow_type）、重建技能记录、重算 PVF 属性、清旧系转职/觉醒任务。
//
// 三条硬约束（服务端强制，这里先把话说明白，免得运营白跑）：
//  1. **角色必须离线** —— 在线时服务端返回 ErrGMCharacterOnline（热刷新撑不住换基础职业）；
//  2. **技能会重建、SP/TP 重置**，属性按新职业成长表重算；
//  3. 装扮/武器/租赁装备有 `[usable job]` 限制，**跨系后旧的可能穿不上**；
//     旧系的转职/觉醒任务也可能残留，需要手工处理。
func (a *App) onAcctJobCareerApply() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	if !acctW.jobGot {
		acctLog("请先点击「载入转职现状」读取转职现状：跨大系转职同样需要一次性提交 job/分支/觉醒三值")
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
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		return cl.AcctCareerSet(context.Background(), role.CharacterID, job, firstGrow, secondGrow)
	}, func(err error) {
		if err == nil {
			acctW.curJob = job
			acctW.jobGot = true
			a.acctSummarize()
			acctLog("角色 %s 跨大系转职完成：%s（job=%d first_grow=%d second_grow=%d）。建议让玩家重新登录后再检查技能与装扮。",
				role.Name, acctJobDisplayName(job, firstGrow, secondGrow), job, firstGrow, secondGrow)
			return
		}
		// 最常见的两种拒绝给一句可操作的话，别让运营对着服务端错误码猜。
		text := err.Error()
		switch {
		case strings.Contains(text, "online") || strings.Contains(text, "在线"):
			acctLog("跨大系转职被拒：角色当前在线。请先让该角色下线（退到角色选择界面不算，要退出游戏进程）再重试。")
		case strings.Contains(text, "pvf"):
			acctLog("跨大系转职被拒：目标职业/分支在本服 PVF 里不存在（两端 PVF 可能不一致）。")
		default:
			acctLog("跨大系转职失败：%v", err)
		}
	})
}

// onAcctDungeonsUnlock 副本全部解锁（AC-10；dungeons_max_difficulty.json 由执行端装载）。
func (a *App) onAcctDungeonsUnlock() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	var count int
	a.acctRun("副本全部解锁", func() error {
		var err error
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		count, err = cl.AcctDungeonsUnlockAll(context.Background(), role.CharacterID)
		return err
	}, func(err error) {
		if err == nil {
			acctLog("角色 %s 副本解锁完成：共 %d 个副本已按最高难度解锁", role.Name, count)
		}
	})
}

// onAcctEquipSlotsUnlock 装备槽全部解锁（AC-11）。
func (a *App) onAcctEquipSlotsUnlock() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	a.acctRun("装备槽全部解锁", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		return cl.AcctEquipSlotsUnlockAll(context.Background(), role.CharacterID)
	}, func(err error) {
		if err == nil {
			acctLog("角色 %s 装备槽已全部解锁", role.Name)
		}
	})
}

// acctBlackDiamondMaxDays 黑钻单次开启天数上限（与执行端 GM 接口 1..3650 天一致）。
const acctBlackDiamondMaxDays = 3650

// onAcctBlackDiamondPreset 常用天数快捷按钮：填进输入框（不直接执行，避免误点）。
func (a *App) onAcctBlackDiamondPreset(days int) {
	if acctW.bdDaysEdit == nil {
		return
	}
	_ = acctW.bdDaysEdit.SetValue(float64(days))
}

// acctBlackDiamondStateText 黑钻操作结果文案：到期时间 + 剩余天数 + 疲劳上限。
func acctBlackDiamondStateText(roleName string, days int, result acctBlackDiamondOut) string {
	expire := time.Unix(result.ExpiresAt, 0).Local().Format("2006-01-02 15:04:05")
	text := fmt.Sprintf("黑钻状态：角色 %s 所属账号（%s）已开启/续期 %d 天；到期时间 %s；当前疲劳上限 %d",
		roleName, acctNameText(result.AccountID), days, expire, result.FatigueLimit)
	if result.RemainingSeconds > 0 {
		remainingDays := result.RemainingSeconds / 86400
		if result.RemainingSeconds%86400 != 0 {
			remainingDays++
		}
		text = fmt.Sprintf("%s（剩余约 %d 天）", text, remainingDays)
	}
	return text
}

// acctBlackDiamondSourceText 数据来源文案（执行端实时 / 直读游戏库兜底）。
func acctBlackDiamondSourceText(source string) string {
	if source == "database" {
		return "直读游戏库（执行端服务端版本较旧，建议更新 DNF90Server.exe 后用实时接口）"
	}
	return "执行端实时查询"
}

// acctBlackDiamondStateLine 把黑钻查询结果拼成一行状态文案。
func acctBlackDiamondStateLine(roleName string, out *acctBlackDiamondStatusOut) string {
	head := fmt.Sprintf("黑钻状态：角色 %s（账号 %s）", roleName, acctNameText(out.AccountID))
	if !out.Active || out.ExpiresAt <= 0 {
		if out.ExpiresAt > 0 {
			return fmt.Sprintf("%s 黑钻已过期（原到期 %s）｜疲劳 %d/%d｜来源：%s",
				head, acctFormatUnix(out.ExpiresAt), out.Fatigue, out.FatigueLimit, acctBlackDiamondSourceText(out.Source))
		}
		return fmt.Sprintf("%s 从未开过黑钻｜疲劳 %d/%d｜来源：%s",
			head, out.Fatigue, out.FatigueLimit, acctBlackDiamondSourceText(out.Source))
	}
	days := out.RemainingSeconds / 86400
	if out.RemainingSeconds%86400 != 0 {
		days++
	}
	online := "离线"
	if out.Online {
		online = "在线"
	}
	return fmt.Sprintf("%s 黑钻有效：到期 %s（剩余约 %d 天）｜疲劳 %d/%d｜%s｜数据来源：%s",
		head, acctFormatUnix(out.ExpiresAt), days, out.Fatigue, out.FatigueLimit, online, acctBlackDiamondSourceText(out.Source))
}

// acctFormatUnix unix 秒 → 本地时间文案（0/负数返回占位）。
func acctFormatUnix(seconds int64) string {
	if seconds <= 0 {
		return "（无）"
	}
	return time.Unix(seconds, 0).Local().Format("2006-01-02 15:04:05")
}

// acctBlackDiamondQueryAsync 后台查询黑钻现状并刷新状态标签（选中角色时自动调用）。
func (a *App) acctBlackDiamondQueryAsync(role *acctCharacterRow) {
	if role == nil {
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var out *acctBlackDiamondStatusOut
	a.acctRun("查询黑钻", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		result, err := cl.AcctBlackDiamondQuery(context.Background(), characterID)
		if err != nil {
			return err
		}
		out = result
		return nil
	}, func(err error) {
		if err != nil {
			acctLog("查询 %s 的黑钻现状失败：%v", name, err)
			if acctW.bdStateL != nil {
				_ = acctW.bdStateL.SetText(fmt.Sprintf("黑钻状态：查询失败（%v）", err))
			}
			return
		}
		if out == nil {
			out = &acctBlackDiamondStatusOut{}
		}
		line := acctBlackDiamondStateLine(name, out)
		acctLog("%s", line)
		if acctW.bdStateL != nil {
			_ = acctW.bdStateL.SetText(line)
		}
		if strings.TrimSpace(out.Message) != "" && strings.Contains(out.Message, "直读游戏库") {
			acctLog("提示：%s", out.Message)
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
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		result, err := cl.AcctBlackDiamond(context.Background(), role.CharacterID, days)
		if err != nil {
			return err
		}
		if result == nil {
			result = &acctBlackDiamondOut{}
		}
		acctLog("%s", acctBlackDiamondStateText(role.Name, days, *result))
		if strings.TrimSpace(result.Message) != "" {
			acctLog("执行端返回：%s", result.Message)
		}
		if result.Deferred {
			acctLog("提示：该角色在线且当前不在安全场景，原生黑钻状态包已推迟到回城后下发。")
		} else if result.Online && !result.Refreshed && strings.TrimSpace(result.RefreshError) != "" {
			acctLog("提示：在线刷新未完成（%s），黑钻权益已落库，重新登录后必定生效。", result.RefreshError)
		}
		return nil
	}, func(err error) {
		if err == nil {
			// 开启成功后重新查一次真实状态：不再只显示"我提交了什么"，
			// 而是显示执行端落库后的到期时间与疲劳上限。
			a.acctBlackDiamondQueryAsync(role)
		}
	})
}
