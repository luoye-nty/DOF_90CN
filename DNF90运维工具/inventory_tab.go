package main

// DNF90 背包编辑页 —— 独立标签页（免费版/赞助版共用，无 build tag）。
//
// 定位：运维侧"查角色 → 选容器 → 删物品"的直连工具页，数据通道是 account_gmt.go 的
// /dnf/gm/*（GM Tool 信封 {success,data,error}），与「账号管理」页的 /local/dnf-gm/*
// 是两条并行的原生通道，共用一个回环 adminListen + adminToken。
//
// 页面结构（自上而下）：
//	① 查询条：维度（账号名/角色名）+ 关键字 + 查询按钮
//	② 角色结果表：名称 | 账号 | 角色ID | 等级 | 在线
//	③ 容器条：容器下拉（背包/仓库/账号金库/装备栏）+ 刷新 + 在线状态提示
//	④ 物品表：格子键 | 列表 | 格子 | item_id | 名称 | 数量 | 绑定
//	⑤ 操作条：删除选中格 | 按数量删除 | 清空当前容器
//	⑥ 本页日志
//
// 在线约束与实时性（务必保留这段说明，行为不是"没做"）：
//	go-server 的 GMDeleteInventoryItem / GMClearInventory 第一句就是会话在线判定，
//	角色在线时返回 409（ErrGMCharacterOnline），所以本页对在线角色只做只读浏览，
//	删除按钮给出明确中文原因，不静默失败、也不假装成功。
//	"在线也能删 + 客户端格子实时消失"需要服务端放开判定并补一格刷新包
//	（sendCurrentSocketInventorySlotUpdate 已有同类能力），属于 go-server 侧改造，
//	工具侧不需要再改：判定一放开，同一批请求立刻生效。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// invContainerLabels / invContainerKeys 容器下拉的显示名与 /dnf/gm/* 的 container 取值。
// equipment 容器服务端标记 read_only=true，只用于查看与"一键卸下/删除装备"入口的对照，
// 本页不对它做删除（服务端也会以 invalid request 拒绝）。
var invContainerLabels = []string{"背包", "仓库", "账号金库", "装备栏(只读)"}
var invContainerKeys = []string{"inventory", "warehouse", "account", "equipment"}

const (
	invLogMaxLines = 400
	// invCountDeleteMax 单格"按数量删除"上限，仅用于输入框钳位；
	// 真正生效值以服务端 gmRemoveStack 语义为准：count==0 或 count>=持有数 = 整格删除。
	invCountDeleteMax = 999999999
)

// invRoleModel 背包页角色结果表模型（角色名|账号|角色ID|等级|在线）。
//
// 刻意不复用「账号管理」页的 acctRoleModel：那边列是 角色名|账号|等级|金币|点券|在线，
// 列数与取值顺序都不同 —— 复用会把等级显示到「角色ID」列、金币显示到「等级」列。
type invRoleModel struct {
	walk.TableModelBase
	items []acctCharacterView
}

func (m *invRoleModel) RowCount() int { return len(m.items) }

func (m *invRoleModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.Name
	case 1:
		// 账号列显示真实账号名（统一口径见 account_names.go）
		return opsAcctNameText(item.AccountID)
	case 2:
		return item.CharacterID
	case 3:
		return item.Level
	case 4:
		return acctOnlineText(item.Online)
	}
	return nil
}

// invItemModel 物品表模型（格子键|列表|格子|item_id|名称|数量|绑定|备注）。
type invItemModel struct {
	walk.TableModelBase
	items []acctGMTInventoryItem
}

func (m *invItemModel) RowCount() int { return len(m.items) }

func (m *invItemModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	item := m.items[row]
	switch col {
	case 0:
		return item.Key
	case 1:
		return item.ListType
	case 2:
		return item.Slot
	case 3:
		return item.ItemID
	case 4:
		return item.Name
	case 5:
		return item.Count
	case 6:
		if item.Bind {
			return "绑定"
		}
		return "不绑定"
	case 7:
		notes := make([]string, 0, 2)
		if item.ReadOnly {
			notes = append(notes, "只读容器")
		}
		if expiring := invExpireNote(item); expiring != "" {
			notes = append(notes, expiring)
		}
		return strings.Join(notes, " ")
	}
	return nil
}

// invBindingText 绑定状态转中文。
func invBindingText(bind bool) string {
	if bind {
		return "绑定"
	}
	return "不绑定"
}

// invExpireNote 把带期限的装备/道具标出来：删之前能一眼看到"这件是限时的"。
func invExpireNote(item acctGMTInventoryItem) string {
	if item.ExpireAt.IsZero() {
		return ""
	}
	return "到期 " + item.ExpireAt.Local().Format("2006-01-02")
}

// invTabWidgets 背包编辑页控件与状态。
type invTabWidgets struct {
	dimCombo    *walk.ComboBox
	queryEdit   *walk.LineEdit
	queryBtn    *walk.PushButton
	roleTable   *walk.TableView
	roleSumL    *walk.Label
	containerCb *walk.ComboBox
	refreshBtn  *walk.PushButton
	onlineHintL *walk.Label
	itemTable   *walk.TableView
	itemDetailL *walk.Label
	countEdit   *walk.NumberEdit
	delBtn      *walk.PushButton
	delCountBtn *walk.PushButton
	clearBtn    *walk.PushButton
	logEdit     *walk.TextEdit

	// 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币）——
	// 前四种是账号级 / 角色级余额（走 /local/dnf-gm/wallet/*），
	// 复活币是主背包 1 号格的物品（item_id=1），所以它的清零走库存删除。
	walletL       *walk.Label
	walletCurCb   *walk.ComboBox
	walletAmtEdit *walk.NumberEdit
	walletRefBtn  *walk.PushButton
	walletZeroBtn *walk.PushButton
	walletSetBtn  *walk.PushButton
	wallet        acctWalletResult
	walletLoaded  bool

	// 状态（只在 UI 线程读写；后台回调统一经 a.uiSafe / acctRun 主线程回调）
	selRole  *acctCharacterView
	snapshot acctGMTInventorySnapshot
	rows     []acctGMTInventoryItem // 当前容器过滤后的行，索引与表格行一一对应
	loaded   bool

	logMu    sync.Mutex
	logBuf   []string
	logDirty int
}

var invW invTabWidgets
var invRoleMdl invRoleModel
var invItemMdl invItemModel

// inventoryTab 背包编辑页控件树。
func (a *App) inventoryTab() []Widget {
	invApp = a // 供本页日志批量刷新回主线程使用
	return []Widget{
		ScrollView{
			HorizontalFixed: true,
			Layout:          VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []Widget{
				// ① 查询条
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						Label{Text: "查询维度:"},
						ComboBox{AssignTo: &invW.dimCombo, Model: acctQueryDimLabels, CurrentIndex: 1, MinSize: Size{Width: 90, Height: 0}},
						LineEdit{AssignTo: &invW.queryEdit, MinSize: Size{Width: 200, Height: 0}, ToolTipText: "输入账号名或角色名；背包编辑按角色操作，建议用角色名"},
						PushButton{AssignTo: &invW.queryBtn, Text: "查询角色", OnClicked: a.onInvQueryRoles},
						Label{
							Text:      "提示：物品删除不可撤销，本页只处理背包/仓库/账号金库，装备栏请在「账号管理」页处理。",
							TextColor: walk.RGB(140, 60, 0),
						},
					},
				},
				// ② 角色结果
				TableView{
					AssignTo: &invW.roleTable,
					Columns: []TableViewColumn{
						{Title: "角色名", Width: 140},
						{Title: "账号", Width: 220},
						{Title: "角色ID", Width: 90},
						{Title: "等级", Width: 60},
						{Title: "在线", Width: 60},
					},
					Model:                 new(invRoleModel),
					MinSize:               Size{Width: 880, Height: 110},
					OnCurrentIndexChanged: a.onInvRoleSelected,
				},
				Label{AssignTo: &invW.roleSumL, Text: "未选择角色", MinSize: Size{Width: 880, Height: 0}},
				// ②.5 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币）
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						Label{Text: "钱包:"},
						Label{AssignTo: &invW.walletL, Text: "未读取（点「刷新钱包」）", MinSize: Size{Width: 430, Height: 0}},
						PushButton{AssignTo: &invW.walletRefBtn, Text: "刷新钱包", OnClicked: a.onInvWalletRefresh,
							ToolTipText: "读取点券/代币券/欢乐代币券（账号级）与金币（角色级）；复活币取自背包快照，需先点「刷新背包」"},
						Label{Text: "币种:"},
						ComboBox{AssignTo: &invW.walletCurCb, Model: invWalletCurrencyLabels, CurrentIndex: 0, MinSize: Size{Width: 110, Height: 0}},
						NumberEdit{AssignTo: &invW.walletAmtEdit, MinValue: 0, MaxValue: invCountDeleteMax, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}},
						PushButton{AssignTo: &invW.walletZeroBtn, Text: "清零", OnClicked: a.onInvWalletZero,
							ToolTipText: "把所选币种清成 0（不可撤销）。复活币清零走主背包 1 号格删除，要求角色离线。"},
						PushButton{AssignTo: &invW.walletSetBtn, Text: "设为该值", OnClicked: a.onInvWalletSet,
							ToolTipText: "把所选币种设成左侧数量（复活币不支持设值，只能清零）"},
					},
				},
				// ③ 容器条
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						Label{Text: "容器:"},
						ComboBox{
							AssignTo: &invW.containerCb, Model: invContainerLabels, CurrentIndex: 0,
							MinSize:               Size{Width: 130, Height: 0},
							OnCurrentIndexChanged: func() { a.onInvContainerChanged() },
						},
						PushButton{AssignTo: &invW.refreshBtn, Text: "刷新背包", OnClicked: a.onInvRefresh, ToolTipText: "重新拉取该角色的背包+仓库+账号金库+装备栏完整快照"},
						Label{AssignTo: &invW.onlineHintL, Text: "", TextColor: walk.RGB(90, 90, 90)},
					},
				},
				// ④ 物品表
				TableView{
					AssignTo:       &invW.itemTable,
					MultiSelection: true,
					Columns: []TableViewColumn{
						{Title: "格子键", Width: 110},
						{Title: "列表", Width: 50},
						{Title: "格子", Width: 50},
						{Title: "item_id", Width: 80},
						{Title: "名称", Width: 250},
						{Title: "数量", Width: 60},
						{Title: "绑定", Width: 60},
						{Title: "备注", Width: 140},
					},
					Model:                 new(invItemModel),
					MinSize:               Size{Width: 880, Height: 200},
					OnCurrentIndexChanged: a.onInvItemSelected,
				},
				Label{AssignTo: &invW.itemDetailL, Text: "未选择物品", MinSize: Size{Width: 880, Height: 0}},
				// ⑤ 操作条
				Composite{
					Layout: HBox{MarginsZero: true, Spacing: 6},
					Children: []Widget{
						PushButton{AssignTo: &invW.delBtn, Text: "删除选中格(整格)", OnClicked: a.onInvDeleteWhole, ToolTipText: "把选中的每一行整格删掉（服务端 count=0 语义）"},
						Label{Text: "按数量删:"},
						NumberEdit{AssignTo: &invW.countEdit, MinValue: 1, MaxValue: invCountDeleteMax, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}},
						PushButton{AssignTo: &invW.delCountBtn, Text: "删除选中格数量", OnClicked: a.onInvDeleteCount},
						PushButton{AssignTo: &invW.clearBtn, Text: "清空当前容器", OnClicked: a.onInvClearContainer, ToolTipText: "清空当前选中容器里的全部物品，不可撤销"},
					},
				},
				Label{
					Text:      "说明：删除要求角色离线（服务端 GM 库存改动会拒绝在线角色，避免与在线会话的内存快照互相覆盖）。邮箱清空不受此限制，见「账号管理」页。",
					TextColor: walk.RGB(90, 90, 90),
				},
				// ⑥ 本页日志
				Label{Text: "本页日志："},
				TextEdit{
					AssignTo: &invW.logEdit, ReadOnly: true, VScroll: true, HScroll: true,
					MinSize: Size{Width: 880, Height: 130},
					Font:    Font{Family: "Consolas", PointSize: 9},
				},
			},
		},
	}
}

// ================= 本页日志 =================

// invLog 追加一行到本页日志缓冲（线程安全、不阻塞、不碰控件）。
func invLog(format string, args ...any) {
	if invW.logEdit == nil {
		return
	}
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	invW.logMu.Lock()
	invW.logBuf = append(invW.logBuf, line)
	if over := len(invW.logBuf) - invLogMaxLines*2; over > 0 {
		invW.logBuf = append([]string(nil), invW.logBuf[over:]...)
	}
	invW.logDirty++
	invW.logMu.Unlock()
}

// flushInvLog 由主日志刷新循环（main.flushLogs）按同一节奏调用。
func flushInvLog() {
	invW.logMu.Lock()
	dirty := invW.logDirty
	invW.logDirty = 0
	lines := invW.logBuf
	if len(lines) > invLogMaxLines {
		lines = lines[len(lines)-invLogMaxLines:]
	}
	text := ""
	if dirty > 0 {
		text = strings.Join(lines, "\r\n")
	}
	invW.logMu.Unlock()
	if text == "" || invW.logEdit == nil {
		return
	}
	// 与账号管理页同款：主窗口 Synchronize 到 UI 线程再写控件。
	if invApp != nil {
		invApp.uiSafe(func() {
			if invW.logEdit == nil {
				return
			}
			invW.logEdit.SetText(text + "\r\n")
			acctLogScrollBottom(invW.logEdit)
		})
	}
}

// invApp 主 App 引用（构建页面时注入），供后台刷新回主线程。
var invApp *App

// ================= 交互 =================

// invBusyUI 任务期间禁用/恢复本页操作按钮。
func (a *App) invBusyUI(busy bool) {
	for _, btn := range []*walk.PushButton{invW.queryBtn, invW.refreshBtn, invW.delBtn, invW.delCountBtn, invW.clearBtn} {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
}

// invRun 本页统一后台任务入口（全局互斥 + 主线程回调 + 页面忙碌态）。
func (a *App) invRun(label string, job func() error, done func(err error)) {
	if a.busy.Load() {
		invLog("已有任务进行中，请稍候")
		return
	}
	invLog("处理中：%s ...", label)
	a.invBusyUI(true)
	a.runAsync(label, job, func(err error) {
		a.invBusyUI(false)
		if err != nil {
			invLog("失败：%v", err)
		}
		if done != nil {
			done(err)
		}
	})
}

// invSelectedRole 返回当前选中角色（无则日志提示并返回 nil）。
func (a *App) invSelectedRole() *acctCharacterView {
	if invW.selRole == nil {
		invLog("请先在上方角色结果里选中一个角色")
		return nil
	}
	return invW.selRole
}

// invCurrentContainer /dnf/gm/* 的 container 取值（未选中时按背包）。
func invCurrentContainer() string {
	idx := 0
	if invW.containerCb != nil {
		idx = invW.containerCb.CurrentIndex()
	}
	if idx < 0 || idx >= len(invContainerKeys) {
		return invContainerKeys[0]
	}
	return invContainerKeys[idx]
}

// invContainerTitle 容器中文名。
func invContainerTitle(key string) string {
	for i, k := range invContainerKeys {
		if k == key {
			return invContainerLabels[i]
		}
	}
	return key
}

// invContainerReadOnly 装备栏容器只读（服务端 GMDeleteInventoryItem 拒绝该 container）。
func invContainerReadOnly(key string) bool { return key == "equipment" }

// onInvQueryRoles 查询角色（与账号管理页同一条 GM-1 通道，结果只写入本页表格）。
func (a *App) onInvQueryRoles() {
	keyword := strings.TrimSpace(invW.queryEdit.Text())
	if keyword == "" {
		invLog("请输入账号名或角色名")
		return
	}
	dim := invW.dimCombo.CurrentIndex()
	var username, characterName string
	if dim == 0 {
		username = keyword
	} else {
		characterName = keyword
	}
	var rows []acctCharacterView
	a.invRun("查询角色", func() error {
		var err error
		rows, _, err = a.acctCharacters(context.Background(), username, characterName)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invRoleMdl.items = rows
		// 账号列要显示真实账号名：上表前先把这批主键翻译掉（失败只回退显示主键）
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.AccountID)
		}
		a.resolveAcctNames(acctNameList(ids...))
		invW.roleTable.SetModel(&invRoleMdl)
		invW.selRole = nil
		invW.snapshot = acctGMTInventorySnapshot{}
		invW.rows = nil
		invW.loaded = false
		invW.wallet = acctWalletResult{}
		invW.walletLoaded = false
		invItemMdl.items = nil
		invW.itemTable.SetModel(&invItemMdl)
		invW.roleSumL.SetText("未选择角色")
		invW.itemDetailL.SetText("未选择物品")
		invW.onlineHintL.SetText("")
		a.applyInvWalletToUI()
		invLog("查询完成：共 %d 名角色。点击行选中后可读取背包", len(rows))
	})
}

// onInvRoleSelected 角色行选中：清掉上一个角色的背包显示，不自动拉快照（避免误触大查询）。
func (a *App) onInvRoleSelected() {
	if invW.roleTable == nil {
		return
	}
	index := invW.roleTable.CurrentIndex()
	if index < 0 || index >= len(invRoleMdl.items) {
		invW.selRole = nil
		return
	}
	row := invRoleMdl.items[index]
	invW.selRole = &row
	invW.snapshot = acctGMTInventorySnapshot{}
	invW.rows = nil
	invW.loaded = false
	invW.wallet = acctWalletResult{}
	invW.walletLoaded = false
	invItemMdl.items = nil
	invW.itemTable.SetModel(&invItemMdl)
	invW.itemDetailL.SetText("未选择物品")
	a.applyInvWalletToUI()
	invW.roleSumL.SetText(fmt.Sprintf("角色: %s | 账号: %s | 角色ID: %s | 等级: %d | 在线: %s",
		row.Name, opsAcctNameText(row.AccountID), row.CharacterID, row.Level, acctOnlineText(row.Online)))
	a.refreshOnlineHint(row.Online)
	invLog("已选中角色：%s（角色ID %s，等级 %d，%s）；点「刷新背包」读取库存",
		row.Name, row.CharacterID, row.Level, acctOnlineText(row.Online))
}

// refreshOnlineHint 在线角色给出明确的只读提示（删除会被服务端 409 拒绝）。
func (a *App) refreshOnlineHint(online bool) {
	if invW.onlineHintL == nil {
		return
	}
	status := "离线，可删除"
	color := walk.RGB(32, 130, 32)
	if online {
		status = "在线：服务端拒绝库存改动（只读浏览）"
		color = walk.RGB(170, 40, 40)
	}
	invW.onlineHintL.SetText(" 角色状态：" + status)
	invW.onlineHintL.SetTextColor(color)
}

// onInvContainerChanged 切换容器：只做本地过滤，不再打服务端（快照是一次全量拉取的）。
func (a *App) onInvContainerChanged() {
	if !invW.loaded {
		return
	}
	a.applyInvFilter()
}

// applyInvFilter 按当前容器过滤快照并刷新表格。
func (a *App) applyInvFilter() {
	container := invCurrentContainer()
	rows := make([]acctGMTInventoryItem, 0, len(invW.snapshot.Items))
	for _, item := range invW.snapshot.Items {
		if item.Container == container {
			rows = append(rows, item)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ListType != rows[j].ListType {
			return rows[i].ListType < rows[j].ListType
		}
		if rows[i].Slot != rows[j].Slot {
			return rows[i].Slot < rows[j].Slot
		}
		return rows[i].Key < rows[j].Key
	})
	invW.rows = rows
	invItemMdl.items = rows
	invW.itemTable.SetModel(&invItemMdl)
	invW.itemDetailL.SetText("未选择物品")
	total := 0
	for _, item := range rows {
		total += int(item.Count)
	}
	if invContainerReadOnly(container) {
		invLog("%s：%d 行（合计 %d 件）——该容器只读，删除会被服务端拒绝", invContainerTitle(container), len(rows), total)
	} else {
		invLog("%s：%d 行（合计 %d 件）", invContainerTitle(container), len(rows), total)
	}
}

// onInvRefresh 拉取选中角色的完整库存快照。
func (a *App) onInvRefresh() {
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	characterID := role.CharacterID
	var snapshot acctGMTInventorySnapshot
	a.invRun("读取背包快照", func() error {
		var err error
		snapshot, err = a.acctGMTInventory(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invW.snapshot = snapshot
		invW.loaded = true
		// 服务端返回的 character 是权威在线状态，回写选中行，避免角色刚上线/下线后提示过时。
		if invW.selRole != nil {
			invW.selRole.Online = snapshot.Character.Online
			a.refreshOnlineHint(snapshot.Character.Online)
		}
		a.applyInvFilter()
		// 复活币数量就在这份快照里，顺手把钱包摘要刷新一次（点券那几项要另点「刷新钱包」）。
		a.applyInvWalletToUI()
	})
}

// ================= 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币） =================
//
// 前四种是服务端 /local/dnf-gm/wallet/* 的余额（点券/代币券/欢乐代币券=账号级，
// 金币=角色级），mode=set 就能直接清成 0 或设成指定值。
// **复活币不在钱包里**：它是主背包 1 号格的物品（item_id=1），所以它的"清零"
// 走库存删除（整格删除语义），并且和别的库存改动一样要求角色离线。

// invReviveCoinItemID 复活币的 item_id（服务端 dnfbridge.csharpReviveCoinItemID）。
const invReviveCoinItemID = 1

var (
	invWalletCurrencyKeys   = []string{"cera", "token_cera", "happy_token_cera", "gold", "revive_coin"}
	invWalletCurrencyLabels = []string{"点券", "代币券", "欢乐代币券", "金币", "复活币"}
)

// invWalletCurrencyKey 下拉索引 → 币种取值（越界回落到点券）。
func invWalletCurrencyKey(idx int) string {
	if idx < 0 || idx >= len(invWalletCurrencyKeys) {
		return invWalletCurrencyKeys[0]
	}
	return invWalletCurrencyKeys[idx]
}

// invWalletCurrencyLabel 币种取值 → 中文。
func invWalletCurrencyLabel(key string) string {
	for i, k := range invWalletCurrencyKeys {
		if k == key {
			return invWalletCurrencyLabels[i]
		}
	}
	return key
}

// invWalletReviveCoin 从背包快照里取复活币那一格（未加载/没有则返回 false）。
func (a *App) invWalletReviveCoin() (acctGMTInventoryItem, bool) {
	for _, item := range invW.snapshot.Items {
		if item.ItemID == invReviveCoinItemID && item.Container == "inventory" {
			return item, true
		}
	}
	return acctGMTInventoryItem{}, false
}

// applyInvWalletToUI 组装钱包摘要（GUI 线程调用）。
func (a *App) applyInvWalletToUI() {
	if invW.walletL == nil {
		return
	}
	if !invW.walletLoaded {
		invW.walletL.SetText("未读取（点「刷新钱包」）")
		return
	}
	w := invW.wallet
	reviveText := "复活币 —（点「刷新背包」读取）"
	if coin, ok := a.invWalletReviveCoin(); ok {
		reviveText = fmt.Sprintf("复活币 %d（主背包 %s 格）", coin.Count, coin.Key)
	}
	invW.walletL.SetText(fmt.Sprintf("点券 %d ｜代币券 %d ｜欢乐代币券 %d ｜金币 %d ｜%s",
		w.Cera, w.TokenCera, w.HappyTokenCera, w.Gold, reviveText))
}

// onInvWalletRefresh 读取钱包余额（点券/代币券/欢乐代币券/金币），复活币取背包快照。
func (a *App) onInvWalletRefresh() {
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	name := role.Name
	var wallet acctWalletResult
	a.invRun("读取钱包余额", func() error {
		var err error
		wallet, _, err = a.acctWalletQuery(context.Background(), name)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invW.wallet = wallet
		invW.walletLoaded = true
		a.applyInvWalletToUI()
		invLog("钱包余额：点券 %d，代币券 %d，欢乐代币券 %d，金币 %d",
			wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera, wallet.Gold)
		if _, ok := a.invWalletReviveCoin(); !ok {
			invLog("复活币数量要读背包快照：请点「刷新背包」后再看本行")
		}
	})
}

// invWalletSet 钱包改值的公共实现（amount=0 即清零）。
func (a *App) invWalletSet(amount int64, verb string) {
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	key := invWalletCurrencyKey(invW.walletCurCb.CurrentIndex())
	label := invWalletCurrencyLabel(key)

	// 复活币不是钱包余额：清零 = 删掉主背包那一格（要求离线）。
	if key == "revive_coin" {
		if amount != 0 {
			invLog("复活币不支持「设为该值」，只能清零（服务端没有给钱包格加数量的 GM 接口）")
			return
		}
		coin, ok := a.invWalletReviveCoin()
		if !ok {
			invLog("背包快照里没有复活币格：请先点「刷新背包」（复活币在主背包 1 号格，item_id=%d）", invReviveCoinItemID)
			return
		}
		if role.Online {
			invLog("角色 %s 当前在线：服务端会拒绝库存改动，请让角色下线后再清零复活币", role.Name)
			return
		}
		if walk.MsgBox(a.mw, "清零复活币确认", fmt.Sprintf(
			"将删除角色 %s 的主背包 %s 格（复活币 item_id=%d，当前 %d 个），操作不可撤销。\n\n确认继续？",
			role.Name, coin.Key, coin.ItemID, coin.Count),
			walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
			invLog("已取消清零复活币")
			return
		}
		characterID := role.CharacterID
		var snapshot acctGMTInventorySnapshot
		a.invRun("清零复活币", func() error {
			var err error
			snapshot, err = a.acctGMTInventoryDelete(context.Background(), characterID, "inventory", coin.Key, 0)
			return err
		}, func(err error) {
			if err != nil {
				return
			}
			invW.snapshot = snapshot
			a.applyInvFilter()
			a.applyInvWalletToUI()
			invLog("已清零角色 %s 的复活币（原 %d 个，整格删除）", role.Name, coin.Count)
		})
		return
	}

	// 其余四种走钱包接口（在线也生效，服务端会顺带刷新在线客户端的钱包）。
	if walk.MsgBox(a.mw, "钱包变更确认", fmt.Sprintf(
		"将把角色 %s 的%s%s（当前 %d → %d）。\n\n在线角色会立即收到余额刷新；操作不可撤销。\n\n确认继续？",
		role.Name, label, verb, a.invWalletCurrent(key), amount),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		invLog("已取消钱包变更")
		return
	}
	name := role.Name
	var wallet acctWalletResult
	a.invRun("变更钱包余额", func() error {
		var err error
		wallet, _, err = a.acctWalletUpdate(context.Background(), name, key, "set", amount)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invW.wallet = wallet
		invW.walletLoaded = true
		a.applyInvWalletToUI()
		invLog("已把角色 %s 的%s%s：%d → %d（点券 %d，代币券 %d，欢乐代币券 %d，金币 %d）",
			role.Name, label, verb, wallet.Before, wallet.After,
			wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera, wallet.Gold)
	})
}

// invWalletCurrent 该币种当前显示值（金币取钱包结果的 Gold，其余取对应余额）。
func (a *App) invWalletCurrent(key string) int64 {
	switch key {
	case "cera":
		return invW.wallet.Cera
	case "token_cera":
		return invW.wallet.TokenCera
	case "happy_token_cera":
		return invW.wallet.HappyTokenCera
	case "gold":
		return invW.wallet.Gold
	}
	return 0
}

// onInvWalletZero 清零所选币种。
func (a *App) onInvWalletZero() { a.invWalletSet(0, "清零") }

// onInvWalletSet 把所选币种设成左侧数量。
func (a *App) onInvWalletSet() {
	if invW.walletAmtEdit == nil {
		return
	}
	a.invWalletSet(int64(invW.walletAmtEdit.Value()), "设为")
}

// onInvItemSelected 物品行选中：更新明细行。
func (a *App) onInvItemSelected() {
	if invW.itemTable == nil {
		return
	}
	index := invW.itemTable.CurrentIndex()
	if index < 0 || index >= len(invW.rows) {
		invW.itemDetailL.SetText("未选择物品")
		return
	}
	item := invW.rows[index]
	detail := fmt.Sprintf("已选择：%s（item_id=%d，格子键=%s，数量=%d，%s）",
		item.Name, item.ItemID, item.Key, item.Count, invBindingText(item.Bind))
	if !item.ExpireAt.IsZero() {
		detail += "，到期 " + item.ExpireAt.Local().Format("2006-01-02 15:04:05")
	}
	invW.itemDetailL.SetText(detail)
}

// invSelectedRows 返回表格里选中的行（多选；单选控件回退到当前行）。
func invSelectedRows() []acctGMTInventoryItem {
	if invW.itemTable == nil {
		return nil
	}
	indexes := invW.itemTable.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := invW.itemTable.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	rows := make([]acctGMTInventoryItem, 0, len(indexes))
	seen := make(map[string]struct{}, len(indexes))
	for _, idx := range indexes {
		if idx < 0 || idx >= len(invW.rows) {
			continue
		}
		item := invW.rows[idx]
		if _, dup := seen[item.Key]; dup {
			continue
		}
		seen[item.Key] = struct{}{}
		rows = append(rows, item)
	}
	return rows
}

// invGuardMutable 删除前的共同前置校验：选中角色 + 容器可写 + 角色离线。
func (a *App) invGuardMutable(action string) (*acctCharacterView, string) {
	role := a.invSelectedRole()
	if role == nil {
		return nil, ""
	}
	container := invCurrentContainer()
	if invContainerReadOnly(container) {
		invLog("%s 是只读容器，无法%s（请在「账号管理」页用装备槽入口处理）", invContainerTitle(container), action)
		return nil, ""
	}
	if role.Online {
		invLog("角色 %s 当前在线：服务端会拒绝%s库存改动。请让角色下线后再操作（背包改动与在线会话的内存快照会互相覆盖，服务端刻意拦住了这条路径）。", role.Name, action)
		return nil, ""
	}
	return role, container
}

// onInvDeleteWhole 整格删除选中行。
func (a *App) onInvDeleteWhole() {
	rows := invSelectedRows()
	if len(rows) == 0 {
		invLog("请先在物品表里选中要删除的格子")
		return
	}
	role, container := a.invGuardMutable("删除")
	if role == nil {
		return
	}
	var brief strings.Builder
	for i, item := range rows {
		if i >= 5 {
			fmt.Fprintf(&brief, "\n... 以及另外 %d 格", len(rows)-5)
			break
		}
		fmt.Fprintf(&brief, "\n  %s  %s(item_id=%d) x%d", item.Key, item.Name, item.ItemID, item.Count)
	}
	if walk.MsgBox(a.mw, "删除确认",
		fmt.Sprintf("将从角色 %s 的「%s」整格删除 %d 格物品，操作不可撤销：%s\n\n确认继续？",
			role.Name, invContainerTitle(container), len(rows), brief.String()),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		invLog("已取消整格删除")
		return
	}

	characterID := role.CharacterID
	name := role.Name
	var snapshot acctGMTInventorySnapshot
	succeeded := make([]string, 0, len(rows))
	failed := make([]string, 0, len(rows))
	a.invRun("删除物品", func() error {
		for _, item := range rows {
			// count=0：服务端 gmRemoveStack 语义 = 整格删除。
			out, err := a.acctGMTInventoryDelete(context.Background(), characterID, container, item.Key, 0)
			if err != nil {
				// 不中断：前面已经删掉的格子服务端是真的删了，
				// 直接 return 会让界面与真实状态脱节（见下面的 done 回调）。
				failed = append(failed, fmt.Sprintf("%s(%s): %v", item.Name, item.Key, err))
				continue
			}
			snapshot = out
			succeeded = append(succeeded, item.Key)
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d/%d 格删除失败", len(failed), len(rows))
		}
		return nil
	}, func(err error) {
		// 无论成败都必须用最后一次成功的快照刷新界面：
		// 部分成功时，已删掉的格子在服务端已经没了，界面还列着的话操作者会重复删、
		// 然后收到"inventory slot was not found"，却无从判断到底删掉了哪些。
		if len(succeeded) > 0 {
			invW.snapshot = snapshot
			invW.loaded = true
			a.applyInvFilter()
			invLog("已从角色 %s 的「%s」删除 %d 格物品", name, invContainerTitle(container), len(succeeded))
		}
		if len(failed) > 0 {
			invLog("以下 %d 格删除失败（其余已生效，界面已按实际结果刷新）：", len(failed))
			for _, msg := range failed {
				invLog("  %s", msg)
			}
		}
		if len(succeeded) == 0 && len(failed) == 0 {
			invLog("没有执行任何删除（选中的格子已不在快照里）")
		}
	})
}

// onInvDeleteCount 按数量删除选中格（只作用于第一个选中行，避免多格减数量语义含糊）。
func (a *App) onInvDeleteCount() {
	rows := invSelectedRows()
	if len(rows) == 0 {
		invLog("请先在物品表里选中要扣减的格子")
		return
	}
	if len(rows) > 1 {
		invLog("「按数量删除」一次只处理一行，请只选中一行（当前选中 %d 行）；整格删除请用左边的按钮", len(rows))
		return
	}
	role, container := a.invGuardMutable("删除")
	if role == nil {
		return
	}
	item := rows[0]
	count := int64(invW.countEdit.Value())
	if count < 1 {
		invLog("请填写要删除的数量（1~%d）", invCountDeleteMax)
		return
	}
	if count > item.Count {
		invLog("要删除的数量 %d 超过该格持有量 %d；如需全部删除请直接按整格删除", count, item.Count)
		return
	}
	if walk.MsgBox(a.mw, "删除确认",
		fmt.Sprintf("将从角色 %s 的「%s」格 %s(item_id=%d) 删除 %d 件（持有 %d 件），操作不可撤销。\n\n确认继续？",
			role.Name, invContainerTitle(container), item.Name, item.ItemID, count, item.Count),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		invLog("已取消按数量删除")
		return
	}

	characterID := role.CharacterID
	name := role.Name
	key := item.Key
	var snapshot acctGMTInventorySnapshot
	a.invRun("按数量删除物品", func() error {
		var err error
		snapshot, err = a.acctGMTInventoryDelete(context.Background(), characterID, container, key, count)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invW.snapshot = snapshot
		invW.loaded = true
		a.applyInvFilter()
		invLog("已从角色 %s 的「%s」格 %s 删除 %d 件", name, invContainerTitle(container), key, count)
	})
}

// onInvClearContainer 清空当前容器（危险操作，二次确认里要求输入容器名）。
func (a *App) onInvClearContainer() {
	role, container := a.invGuardMutable("清空")
	if role == nil {
		return
	}
	if invW.itemTable == nil {
		return
	}
	if !invW.loaded {
		invLog("请先点「刷新背包」读取一次快照，确认容器里确实是要清的内容")
		return
	}
	held := 0
	for _, item := range invW.snapshot.Items {
		if item.Container == container {
			held++
		}
	}
	if held == 0 {
		invLog("角色 %s 的「%s」当前没有物品，无需清空", role.Name, invContainerTitle(container))
		return
	}
	if walk.MsgBox(a.mw, "清空确认",
		fmt.Sprintf("将清空角色 %s 的「%s」共 %d 格物品，操作不可撤销且无法恢复。\n\n确认继续？",
			role.Name, invContainerTitle(container), held),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		invLog("已取消清空")
		return
	}

	characterID := role.CharacterID
	name := role.Name
	var snapshot acctGMTInventorySnapshot
	a.invRun("清空容器", func() error {
		var err error
		snapshot, err = a.acctGMTInventoryClear(context.Background(), characterID, container)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invW.snapshot = snapshot
		invW.loaded = true
		a.applyInvFilter()
		invLog("已清空角色 %s 的「%s」（原 %d 格）", name, invContainerTitle(container), held)
	})
}
