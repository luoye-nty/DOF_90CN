package main

// DNF90 背包编辑页 —— 独立标签页（管理端）。
//
// 数据通道：/manage/v1/account/inventory/*（服务器端运维工具的 manage 服务转发到
// DNF90Server 的 GM Tool 接口 /dnf/gm/characters/{id}/inventory*）。
// 与「账号管理」页的 AC-* 端点不同通道，但共用同一条登录会话与 token。
//
// 页面结构：
//	① 查询条：维度（账号名/角色名）+ 关键字 + 查询
//	② 角色结果表：角色名 | 账号 | 角色ID | 等级 | 在线
//	③ 容器条：背包 / 仓库 / 账号金库 / 装备栏(只读) + 刷新
//	④ 物品表：格子键 | 列表 | 格子 | item_id | 名称 | 数量 | 绑定
//	⑤ 操作条：删除选中格(整格) / 按数量删除 / 清空当前容器
//	⑥ 本页日志
//
// 在线约束（由执行端强制，本页如实呈现，不静默失败）：
//	服务端删除库存要求角色离线，角色在线时返回 409（错误码前缀 character-online）。
//	因此在线角色只做只读浏览，删除按钮提前禁用并给出原因。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// invContainerLabels / invContainerKeys 容器下拉显示名与执行端 container 取值。
var invContainerLabels = []string{"背包", "仓库", "账号金库", "装备栏(只读)"}
var invContainerKeys = []string{"inventory", "warehouse", "account", "equipment"}

const invCountDeleteMax = 999999999

// invRoleModel 背包页角色结果表模型（角色名|账号|角色ID|等级|在线）。
//
// 刻意不复用「账号管理」页的 acctRoleModel：那边列是 角色名|账号|等级|金币|点券|在线，
// 列数与取值顺序都不同 —— 复用会把等级显示到「角色ID」列、金币显示到「等级」列。
type invRoleModel struct {
	walk.TableModelBase
	items []acctCharacterRow
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
		return acctNameText(item.AccountID)
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
	items []GMTInventoryItem
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
		if !item.ExpireAt.IsZero() {
			notes = append(notes, "到期 "+item.ExpireAt.Local().Format("2006-01-02"))
		}
		return strings.Join(notes, " ")
	}
	return nil
}

// invTabWidgets 背包编辑页控件与状态（全部只在 GUI 线程读写）。
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

	// 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币）：
	// 前四种走 /manage/v1/account/wallet/*（账号级/角色级余额），
	// 复活币是主背包 1 号格的物品（item_id=1），清零走库存删除。
	walletL       *walk.Label
	walletCurCb   *walk.ComboBox
	walletAmtEdit *walk.NumberEdit
	walletRefBtn  *walk.PushButton
	walletZeroBtn *walk.PushButton
	walletSetBtn  *walk.PushButton
	wallet        acctWalletBody
	walletLoaded  bool

	selRole  *acctCharacterRow
	snapshot GMTInventorySnapshot
	rows     []GMTInventoryItem
	loaded   bool
}

var invW invTabWidgets
var invRoleMdl invRoleModel
var invItemMdl invItemModel

// buildInventoryTab 背包编辑 TabPage。
func buildInventoryTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "背包编辑",
		Content: decl.Composite{Layout: decl.VBox{MarginsZero: true, SpacingZero: true}, Children: a.inventoryTab()},
	}
}

// inventoryTab 背包编辑页控件树。
func (a *App) inventoryTab() []decl.Widget {
	return []decl.Widget{
		decl.ScrollView{
			HorizontalFixed: true,
			Layout:          decl.VBox{Margins: decl.Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []decl.Widget{
				// ① 查询条
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "查询维度:"},
						decl.ComboBox{AssignTo: &invW.dimCombo, Model: acctQueryDimLabels, CurrentIndex: 1, MinSize: decl.Size{Width: 90, Height: 0}},
						decl.LineEdit{AssignTo: &invW.queryEdit, MinSize: decl.Size{Width: 200, Height: 0}, ToolTipText: "输入账号名或角色名；背包编辑按角色操作，建议用角色名"},
						decl.PushButton{AssignTo: &invW.queryBtn, Text: "查询角色", OnClicked: a.onInvQueryRoles},
						decl.Label{Text: " 物品删除不可撤销；装备栏为只读容器，请在「账号管理」页处理。", TextColor: walk.RGB(140, 60, 0)},
					},
				},
				// ② 角色结果
				decl.TableView{
					AssignTo: &invW.roleTable,
					Columns: []decl.TableViewColumn{
						{Title: "角色名", Width: 150},
						{Title: "账号", Width: 190},
						{Title: "角色ID", Width: 90},
						{Title: "等级", Width: 60},
						{Title: "在线", Width: 60},
					},
					Model:                 new(invRoleModel),
					MinSize:               decl.Size{Width: 880, Height: 110},
					OnCurrentIndexChanged: a.onInvRoleSelected,
				},
				decl.Label{AssignTo: &invW.roleSumL, Text: "未选择角色", MinSize: decl.Size{Width: 880, Height: 0}},
				// ②.5 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币）
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "钱包:"},
						decl.Label{AssignTo: &invW.walletL, Text: "未读取（点「刷新钱包」）", MinSize: decl.Size{Width: 430, Height: 0}},
						decl.PushButton{AssignTo: &invW.walletRefBtn, Text: "刷新钱包", OnClicked: a.onInvWalletRefresh,
							ToolTipText: "读取点券/代币券/欢乐代币券（账号级）与金币（角色级）；复活币取自背包快照，需先点「刷新背包」"},
						decl.Label{Text: "币种:"},
						decl.ComboBox{AssignTo: &invW.walletCurCb, Model: invWalletCurrencyLabels, CurrentIndex: 0, MinSize: decl.Size{Width: 110, Height: 0}},
						decl.NumberEdit{AssignTo: &invW.walletAmtEdit, MinValue: 0, MaxValue: invCountDeleteMax, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 90, Height: 0}},
						decl.PushButton{AssignTo: &invW.walletZeroBtn, Text: "清零", OnClicked: a.onInvWalletZero,
							ToolTipText: "把所选币种清成 0（不可撤销）。复活币清零走主背包 1 号格删除，要求角色离线。"},
						decl.PushButton{AssignTo: &invW.walletSetBtn, Text: "设为该值", OnClicked: a.onInvWalletSet,
							ToolTipText: "把所选币种设成左侧数量（复活币不支持设值，只能清零）"},
					},
				},
				// ③ 容器条
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.Label{Text: "容器:"},
						decl.ComboBox{
							AssignTo: &invW.containerCb, Model: invContainerLabels, CurrentIndex: 0,
							MinSize:               decl.Size{Width: 130, Height: 0},
							OnCurrentIndexChanged: func() { a.onInvContainerChanged() },
						},
						decl.PushButton{AssignTo: &invW.refreshBtn, Text: "刷新背包", OnClicked: a.onInvRefresh, ToolTipText: "拉取该角色的背包+仓库+账号金库+装备栏完整快照"},
						decl.Label{AssignTo: &invW.onlineHintL, Text: "", TextColor: walk.RGB(90, 90, 90)},
					},
				},
				// ④ 物品表
				decl.TableView{
					AssignTo:       &invW.itemTable,
					MultiSelection: true,
					Columns: []decl.TableViewColumn{
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
					MinSize:               decl.Size{Width: 880, Height: 200},
					OnCurrentIndexChanged: a.onInvItemSelected,
				},
				decl.Label{AssignTo: &invW.itemDetailL, Text: "未选择物品", MinSize: decl.Size{Width: 880, Height: 0}},
				// ⑤ 操作条
				decl.Composite{
					Layout: decl.HBox{MarginsZero: true, Spacing: 6},
					Children: []decl.Widget{
						decl.PushButton{AssignTo: &invW.delBtn, Text: "删除选中格(整格)", OnClicked: a.onInvDeleteWhole, ToolTipText: "把选中的每一行整格删掉（执行端 count=0 语义）"},
						decl.Label{Text: "按数量删:"},
						decl.NumberEdit{AssignTo: &invW.countEdit, MinValue: 1, MaxValue: invCountDeleteMax, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 90, Height: 0}},
						decl.PushButton{AssignTo: &invW.delCountBtn, Text: "删除选中格数量", OnClicked: a.onInvDeleteCount},
						decl.PushButton{AssignTo: &invW.clearBtn, Text: "清空当前容器", OnClicked: a.onInvClearContainer, ToolTipText: "清空当前选中容器里的全部物品，不可撤销"},
					},
				},
				decl.Label{
					Text:      "说明：删除要求角色离线（执行端为避免与在线会话的内存快照互相覆盖，会拒绝在线角色的库存改动）。",
					TextColor: walk.RGB(90, 90, 90),
				},
				// ⑥ 本页日志
				decl.Label{Text: "本页日志："},
				decl.TextEdit{
					AssignTo: &invW.logEdit, ReadOnly: true, VScroll: true, HScroll: true,
					MinSize: decl.Size{Width: 880, Height: 140},
				},
			},
		},
	}
}

// ================= 本页日志（只允许在 GUI 线程调用） =================

// invBoolText 布尔转中文。
func invBoolText(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

func invLog(format string, args ...any) {
	if invW.logEdit == nil {
		return
	}
	invW.logEdit.AppendText(fmt.Sprintf("[%s] %s\r\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...)))
	acctLogScrollBottom(invW.logEdit)
}

// ================= 交互 =================

// invBusyUI 任务期间禁用/恢复本页操作按钮（GUI 线程）。
func (a *App) invBusyUI(busy bool) {
	for _, btn := range []*walk.PushButton{invW.queryBtn, invW.refreshBtn, invW.delBtn, invW.delCountBtn, invW.clearBtn} {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
}

// invRun 本页统一后台任务入口：opMu 互斥 + 后台 goroutine + GUI 线程回调。
func (a *App) invRun(label string, job func() error, done func(err error)) {
	if !a.beginOp() {
		invLog("已有管理操作进行中，请稍候")
		return
	}
	invLog("处理中：%s ...", label)
	a.invBusyUI(true)
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
			a.invBusyUI(false)
			a.endOp()
			if err != nil {
				if a.handleAPIError(err) {
					return
				}
				invLog("失败：%v", err)
			}
			if done != nil {
				done(err)
			}
		})
	}()
}

// invSelectedRole 返回当前选中角色（无则提示）。
func (a *App) invSelectedRole() *acctCharacterRow {
	if invW.selRole == nil {
		invLog("请先在上方角色结果里选中一个角色")
		return nil
	}
	return invW.selRole
}

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

func invContainerTitle(key string) string {
	for i, k := range invContainerKeys {
		if k == key {
			return invContainerLabels[i]
		}
	}
	return key
}

func invContainerReadOnly(key string) bool { return key == "equipment" }

// onInvQueryRoles 查询角色（走 AC-1，同一条查询通道，结果只写入本页表格）。
func (a *App) onInvQueryRoles() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
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
	var rows []acctCharacterRow
	a.invRun("查询角色", func() error {
		var err error
		rows, err = c.AcctCharacters(context.Background(), username, characterName)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		invRoleMdl.items = rows
		// 账号列显示真实账号名：上表前先批量翻译（失败只回退显示主键）
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.AccountID)
		}
		a.resolveAccountNames(acctNameList(ids...))
		invW.roleTable.SetModel(&invRoleMdl)
		a.resetInvRoleView()
		invLog("查询完成：共 %d 名角色。点击行选中后可读取背包", len(rows))
	})
}

// resetInvRoleView 清掉上一个角色的选中态与库存显示。
func (a *App) resetInvRoleView() {
	invW.selRole = nil
	invW.snapshot = GMTInventorySnapshot{}
	invW.rows = nil
	invW.loaded = false
	invW.wallet = acctWalletBody{}
	invW.walletLoaded = false
	invItemMdl.items = nil
	if invW.itemTable != nil {
		invW.itemTable.SetModel(&invItemMdl)
	}
	invW.roleSumL.SetText("未选择角色")
	invW.itemDetailL.SetText("未选择物品")
	invW.onlineHintL.SetText("")
	a.applyInvWalletToUI()
}

// onInvRoleSelected 角色行选中。
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
	invW.snapshot = GMTInventorySnapshot{}
	invW.rows = nil
	invW.loaded = false
	invW.wallet = acctWalletBody{}
	invW.walletLoaded = false
	invItemMdl.items = nil
	invW.itemTable.SetModel(&invItemMdl)
	invW.itemDetailL.SetText("未选择物品")
	a.applyInvWalletToUI()
	invW.roleSumL.SetText(fmt.Sprintf("角色: %s | 账号: %s | 角色ID: %s | 等级: %d | 在线: %s",
		row.Name, acctNameText(row.AccountID), row.CharacterID, row.Level, acctOnlineText(row.Online)))
	a.refreshInvOnlineHint(row.Online)
	invLog("已选中角色：%s（角色ID %s，等级 %d，%s）；点「刷新背包」读取库存",
		row.Name, row.CharacterID, row.Level, acctOnlineText(row.Online))
}

// refreshInvOnlineHint 在线角色给出明确的只读提示。
func (a *App) refreshInvOnlineHint(online bool) {
	if invW.onlineHintL == nil {
		return
	}
	if online {
		invW.onlineHintL.SetText(" 角色状态：在线（执行端拒绝库存改动，只读浏览）")
		invW.onlineHintL.SetTextColor(walk.RGB(170, 40, 40))
		return
	}
	invW.onlineHintL.SetText(" 角色状态：离线，可删除")
	invW.onlineHintL.SetTextColor(walk.RGB(32, 130, 32))
}

// onInvContainerChanged 切换容器：本地过滤，不再打服务端。
func (a *App) onInvContainerChanged() {
	if !invW.loaded {
		return
	}
	a.applyInvFilter()
}

// applyInvFilter 按当前容器过滤快照并刷新表格。
func (a *App) applyInvFilter() {
	container := invCurrentContainer()
	rows := make([]GMTInventoryItem, 0, len(invW.snapshot.Items))
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
	suffix := ""
	if invContainerReadOnly(container) {
		suffix = "——该容器只读，删除会被执行端拒绝"
	}
	invLog("%s：%d 行（合计 %d 件）%s", invContainerTitle(container), len(rows), total, suffix)
}

// onInvRefresh 拉取选中角色的完整库存快照。
func (a *App) onInvRefresh() {
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	characterID := role.CharacterID
	var snapshot GMTInventorySnapshot
	a.invRun("读取背包快照", func() error {
		out, err := c.InventoryQuery(context.Background(), characterID)
		if err != nil {
			return err
		}
		snapshot = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		invW.snapshot = snapshot
		invW.loaded = true
		if invW.selRole != nil {
			// 执行端返回的在线状态是权威值，回写选中行避免提示过时。
			invW.selRole.Online = snapshot.Character.Online
			a.refreshInvOnlineHint(snapshot.Character.Online)
		}
		a.applyInvFilter()
		// 复活币数量就在这份快照里，顺手把钱包摘要刷新一次（点券那几项要另点「刷新钱包」）。
		a.applyInvWalletToUI()
	})
}

// ================= 钱包（点券 / 代币券 / 欢乐代币券 / 金币 / 复活币） =================
//
// 前四种是服务器端钱包余额（点券/代币券/欢乐代币券=账号级，金币=角色级），
// 走 /manage/v1/account/wallet/update（mode=set）就能清零或设值。
// **复活币不在钱包里**：它是主背包 1 号格的物品（item_id=1），清零走库存删除，
// 并且和别的库存改动一样要求角色离线。

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
func (a *App) invWalletReviveCoin() (GMTInventoryItem, bool) {
	for _, item := range invW.snapshot.Items {
		if item.ItemID == invReviveCoinItemID && item.Container == "inventory" {
			return item, true
		}
	}
	return GMTInventoryItem{}, false
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

// onInvWalletRefresh 读取钱包余额（复活币取背包快照）。
func (a *App) onInvWalletRefresh() {
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	name := role.Name
	var wallet acctWalletBody
	a.invRun("读取钱包余额", func() error {
		out, _, err := c.AcctWalletQuery(context.Background(), name)
		if err != nil {
			return err
		}
		wallet = *out
		return nil
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
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	role := a.invSelectedRole()
	if role == nil {
		return
	}
	key := invWalletCurrencyKey(invW.walletCurCb.CurrentIndex())
	label := invWalletCurrencyLabel(key)

	if key == "revive_coin" {
		if amount != 0 {
			invLog("复活币不支持「设为该值」，只能清零（服务器端没有给钱包格加数量的 GM 接口）")
			return
		}
		coin, ok := a.invWalletReviveCoin()
		if !ok {
			invLog("背包快照里没有复活币格：请先点「刷新背包」（复活币在主背包 1 号格，item_id=%d）", invReviveCoinItemID)
			return
		}
		if role.Online {
			invLog("角色 %s 当前在线：服务器端会拒绝库存改动，请让角色下线后再清零复活币", role.Name)
			return
		}
		if !a.confirm("清零复活币确认", fmt.Sprintf(
			"将删除角色 %s 的主背包 %s 格（复活币 item_id=%d，当前 %d 个），操作不可撤销。\n\n确认继续？",
			role.Name, coin.Key, coin.ItemID, coin.Count)) {
			invLog("已取消清零复活币")
			return
		}
		characterID := role.CharacterID
		coinKey := coin.Key
		coinCount := coin.Count
		var snapshot GMTInventorySnapshot
		a.invRun("清零复活币", func() error {
			out, err := c.InventoryDelete(context.Background(), characterID, "inventory", coinKey, 0)
			if err != nil {
				return err
			}
			snapshot = *out
			return nil
		}, func(err error) {
			if err != nil {
				return
			}
			invW.snapshot = snapshot
			a.applyInvFilter()
			a.applyInvWalletToUI()
			invLog("已清零角色 %s 的复活币（原 %d 个，整格删除）", role.Name, coinCount)
		})
		return
	}

	if !a.confirm("钱包变更确认", fmt.Sprintf(
		"将把角色 %s 的%s%s（当前 %d → %d）。\n\n在线角色会立即收到余额刷新；操作不可撤销。\n\n确认继续？",
		role.Name, label, verb, a.invWalletCurrent(key), amount)) {
		invLog("已取消钱包变更")
		return
	}
	name := role.Name
	var wallet acctWalletBody
	a.invRun("变更钱包余额", func() error {
		out, _, err := c.AcctWalletUpdate(context.Background(), name, key, "set", amount)
		if err != nil {
			return err
		}
		wallet = *out
		return nil
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

// invWalletCurrent 该币种当前显示值。
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

// onInvItemSelected 物品行选中。
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
		item.Name, item.ItemID, item.Key, item.Count, invBoolText(item.Bind, "绑定", "不绑定"))
	if !item.ExpireAt.IsZero() {
		detail += "，到期 " + item.ExpireAt.Local().Format("2006-01-02 15:04:05")
	}
	invW.itemDetailL.SetText(detail)
}

// invSelectedRows 表格中选中的物品行（多选；单选控件回退当前行）。
func invSelectedRows() []GMTInventoryItem {
	if invW.itemTable == nil {
		return nil
	}
	indexes := invW.itemTable.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := invW.itemTable.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	rows := make([]GMTInventoryItem, 0, len(indexes))
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

// invGuardMutable 删除前的共同前置校验。
func (a *App) invGuardMutable(action string) (*acctCharacterRow, string) {
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
		invLog("角色 %s 当前在线：执行端会拒绝%s库存改动。请让角色下线后再操作（库存改动与在线会话的内存快照会互相覆盖，执行端刻意拦住了这条路径）。", role.Name, action)
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
	if !a.confirm("删除确认", fmt.Sprintf("将从角色 %s 的「%s」整格删除 %d 格物品，操作不可撤销：%s\n\n确认继续？",
		role.Name, invContainerTitle(container), len(rows), brief.String())) {
		invLog("已取消整格删除")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		return
	}
	characterID, name := role.CharacterID, role.Name
	var snapshot GMTInventorySnapshot
	succeeded := make([]string, 0, len(rows))
	failed := make([]string, 0, len(rows))
	a.invRun("删除物品", func() error {
		for _, item := range rows {
			// count=0：执行端 gmRemoveStack 语义 = 整格删除。
			out, err := c.InventoryDelete(context.Background(), characterID, container, item.Key, 0)
			if err != nil {
				// 不中断：前面已删的格子在执行端是真的删了，直接 return 会让界面与真实状态脱节。
				failed = append(failed, fmt.Sprintf("%s(%s): %v", item.Name, item.Key, err))
				continue
			}
			snapshot = *out
			succeeded = append(succeeded, item.Key)
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d/%d 格删除失败", len(failed), len(rows))
		}
		return nil
	}, func(err error) {
		// 无论成败都用最后一次成功的快照刷新界面：部分成功时界面必须跟上实际结果，
		// 否则操作者会对着已不存在的格子重复删除，还看不出到底删掉了哪些。
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

// onInvDeleteCount 按数量删除选中格（一次只处理一行，避免多格扣减语义含糊）。
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
	if !a.confirm("删除确认", fmt.Sprintf("将从角色 %s 的「%s」格 %s(item_id=%d) 删除 %d 件（持有 %d 件），操作不可撤销。\n\n确认继续？",
		role.Name, invContainerTitle(container), item.Name, item.ItemID, count, item.Count)) {
		invLog("已取消按数量删除")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		return
	}
	characterID, name, key := role.CharacterID, role.Name, item.Key
	var snapshot GMTInventorySnapshot
	a.invRun("按数量删除物品", func() error {
		out, err := c.InventoryDelete(context.Background(), characterID, container, key, count)
		if err != nil {
			return err
		}
		snapshot = *out
		return nil
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

// onInvClearContainer 清空当前容器。
func (a *App) onInvClearContainer() {
	role, container := a.invGuardMutable("清空")
	if role == nil {
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
	if !a.confirm("清空确认", fmt.Sprintf("将清空角色 %s 的「%s」共 %d 格物品，操作不可撤销且无法恢复。\n\n确认继续？",
		role.Name, invContainerTitle(container), held)) {
		invLog("已取消清空")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		return
	}
	characterID, name := role.CharacterID, role.Name
	var snapshot GMTInventorySnapshot
	a.invRun("清空容器", func() error {
		out, err := c.InventoryClear(context.Background(), characterID, container)
		if err != nil {
			return err
		}
		snapshot = *out
		return nil
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
