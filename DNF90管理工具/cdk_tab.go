package main

// CDK 管理页：对齐运维工具（生成表单 + 奖励物品表格 + 列表表格 + 兑换流水表格）。
// 数据层走管理 API：GET/POST/DELETE /manage/v1/redeem-codes、
//       POST /manage/v1/redeem-codes/retry-mail、GET ...?redemptions=1&code=（兑换流水）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// cdkRewardKind 奖励行类型：物品（走邮件附件）、点券（直接入账号余额）与黑钻（直接激活账号权益）。
const (
	cdkRewardKindItem         = "item"
	cdkRewardKindCera         = "cera"
	cdkRewardKindBlackDiamond = "black_diamond"
)

// cdkRewardRow 生成表单中的一行奖励（未解析的原始文本，生成时统一校验）。
// Kind 为空按物品处理（兼容老数据）。
type cdkRewardRow struct {
	Kind   string // cdkRewardKindItem / cdkRewardKindCera / cdkRewardKindBlackDiamond
	ItemID string // 物品行的 item_id；点券/黑钻行留空
	Count  string // 物品=数量；点券=点券数量；黑钻=天数
}

// isCera 是否为点券行。
func (r cdkRewardRow) isCera() bool { return r.Kind == cdkRewardKindCera }

// isBlackDiamond 是否为黑钻行。
func (r cdkRewardRow) isBlackDiamond() bool { return r.Kind == cdkRewardKindBlackDiamond }

// cdkRewardJSON 与服务器 reward_json / GM 邮件附件对齐。
// 物品行用 item_id/count/bind；点券行只用 cera；黑钻行只用 black_diamond_days
// （点券是账号通用余额、黑钻是账号级权益，都不走邮件）。
type cdkRewardJSON struct {
	ItemID uint32 `json:"item_id,omitempty"`
	Count  uint32 `json:"count,omitempty"`
	Bind   bool   `json:"bind,omitempty"`
	Cera   int64  `json:"cera,omitempty"`
	// BlackDiamondDays 黑钻天数（1~3650）；多行累加后一次性激活。
	BlackDiamondDays int64 `json:"black_diamond_days,omitempty"`
}

const (
	cdkMaxRewardRows    = 20
	cdkCodeListPageSize = 500
	// cdkMaxCeraAmount 单张 CDK 的点券上限：与服务端钱包 add 的 2147483647 上限一致。
	cdkMaxCeraAmount = 2147483647
	// cdkMaxBlackDiamondDays 单张 CDK 的黑钻总天数上限：与服务端 GM 黑钻接口的
	// 1..3650 天校验一致（多行黑钻按天数累加后一次性激活）。
	cdkMaxBlackDiamondDays = 3650
)

// ============ 表格模型 ============

// cdkRedeemStatusText 流水状态的中文显示。
//
// partial 是"物品与点券只成功了一半"（例如点券入账成功但物品邮件失败），
// 既不能显示成"已送达"也不能显示成"失败"，否则运营会误判。
func cdkRedeemStatusText(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "sent":
		return "已送达"
	case "partial":
		return "部分成功"
	case "failed":
		return "失败"
	default:
		return "发送中"
	}
}

type cdkRewardModel struct {
	walk.TableModelBase
	rows []cdkRewardRow
}

func (m *cdkRewardModel) RowCount() int { return len(m.rows) }

func (m *cdkRewardModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) {
		return nil
	}
	r := m.rows[row]
	switch col {
	case 0:
		return row + 1
	case 1:
		switch {
		case r.isCera():
			return "点券"
		case r.isBlackDiamond():
			return "黑钻"
		}
		return "物品"
	case 2:
		if r.isCera() || r.isBlackDiamond() {
			return "—"
		}
		return r.ItemID
	case 3:
		// 黑钻行在这里显示单位，避免和"物品数量/点券余额"看串。
		if r.isBlackDiamond() {
			return r.Count + " 天"
		}
		return r.Count
	}
	return ""
}

type cdkCodeModel struct {
	walk.TableModelBase
	items []CdkOut
}

func (m *cdkCodeModel) RowCount() int { return len(m.items) }

func (m *cdkCodeModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	c := m.items[row]
	switch col {
	case 0:
		return c.RedeemCode
	case 1:
		return c.Name
	case 2:
		if c.MaxRedeems == 0 {
			return "无限"
		}
		return c.MaxRedeems
	case 3:
		return c.RedeemedCount
	case 4:
		if c.Enabled {
			return "有效"
		}
		return "已作废"
	case 5:
		return c.Remark
	case 6:
		return c.CreatedAt
	}
	return ""
}

type cdkRedemptionModel struct {
	walk.TableModelBase
	items []RedeemRedemption
}

func (m *cdkRedemptionModel) RowCount() int { return len(m.items) }

func (m *cdkRedemptionModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.RedeemCode
	case 1:
		// 兑换流水的账号列：服务端本来就会回 username；老流水可能为空，
		// 那就用账号主键走一遍统一翻译（都拿不到才显示主键，绝不留空白）。
		return textWithFallback(r.Username, acctNameText(r.AccountID))
	case 2:
		return r.CharacterName
	case 3:
		return cdkRedeemStatusText(r.MailStatus)
	case 4:
		return r.MailError
	case 5:
		return r.RemoteIP
	case 6:
		return r.RedeemedAt
	}
	return ""
}

// ============ UI ============

func buildCdkTab(a *App) decl.TabPage {
	// 奖励列表**默认留空**：以前预置一行空物品，列表里凭空多一行很莫名其妙，
	// 而且真去点"生成 CDK"还会被校验拦下。现在由用户自己加物品行/点券行。
	return decl.TabPage{
		Title:   "CDK管理",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.GroupBox{
				Title:  "生成 CDK（批次）",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "CDK 名称:"},
							decl.LineEdit{AssignTo: &a.cdkNameEdit, MinSize: decl.Size{Width: 150, Height: 0}, ToolTipText: "登录器兑换成功时回显的名称；禁止包含 # 与 &"},
							decl.Label{Text: "单码次数:"},
							decl.NumberEdit{AssignTo: &a.cdkTimesEdit, Value: 1.0, MinValue: 0, MaxValue: 999999, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 72, Height: 0}, ToolTipText: "0 = 无限次"},
							decl.Label{Text: "制作数量:"},
							decl.NumberEdit{AssignTo: &a.cdkGenCountEdit, Value: 1.0, MinValue: 1, MaxValue: 1000, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: decl.Size{Width: 72, Height: 0}, ToolTipText: "1-1000"},
							decl.Label{Text: "备注:"},
							decl.LineEdit{AssignTo: &a.cdkRemarkEdit, MinSize: decl.Size{Width: 200, Height: 0}},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "点券:"},
							decl.LineEdit{AssignTo: &a.cdkCeraEdit, MinSize: decl.Size{Width: 100, Height: 0}, ToolTipText: "要发放的点券数量（正整数，1~2147483647）。点券是账号通用余额，兑换时直接入账，不走邮件。"},
							decl.PushButton{Text: "添加点券", OnClicked: func() { a.onCdkAddCeraRow() }},
							decl.Label{Text: "(点券直接入账号余额；可加多行，兑换时累加)"},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "黑钻(天):"},
							decl.LineEdit{AssignTo: &a.cdkBdDaysEdit, MinSize: decl.Size{Width: 100, Height: 0}, ToolTipText: "要发放的黑钻天数（正整数，1~3650）。黑钻是账号级权益，兑换时按所选角色直接激活，不走邮件；可加多行，按天数累加后一次激活。"},
							decl.PushButton{Text: "添加黑钻", OnClicked: func() { a.onCdkAddBlackDiamondRow() }},
							decl.Label{Text: "(黑钻按天算；到期时间在原到期时间上叠加)"},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "奖励物品(item_id):"},
							decl.LineEdit{AssignTo: &a.cdkItemIDEdit, MinSize: decl.Size{Width: 100, Height: 0}},
							decl.Label{Text: "数量:"},
							decl.LineEdit{AssignTo: &a.cdkItemCntEdit, MinSize: decl.Size{Width: 70, Height: 0}},
							decl.PushButton{Text: "添加物品", OnClicked: func() { a.onCdkAddRewardRow() }},
							decl.PushButton{Text: "删除选中行", OnClicked: func() { a.onCdkRemoveRewardRow() }},
							decl.Label{Text: "(最多 20 行，item_id/数量 为整数且 count≥1)"},
						},
					},
					decl.TableView{
						AssignTo: &a.cdkRewardTable,
						Columns: []decl.TableViewColumn{
							{Title: "行号", Width: 50},
							{Title: "类型", Width: 60},
							{Title: "item_id", Width: 140},
							{Title: "数量", Width: 100},
						},
						Model:   &cdkRewardModel{rows: a.cdkRewardRows},
						MinSize: decl.Size{Width: 820, Height: 80},
						MaxSize: decl.Size{Width: 0, Height: 110},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.PushButton{AssignTo: &a.cdkGenBtn, Text: "生成 CDK", OnClicked: func() { a.onCdkGen() }},
							decl.Label{AssignTo: &a.cdkHintL, Text: "生成后将写入账号库，并可在下方列表查询", TextColor: walk.RGB(90, 90, 90)},
						},
					},
				},
			},
			decl.GroupBox{
				Title:  "CDK 列表",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "关键字(码/名称):"},
							decl.LineEdit{AssignTo: &a.cdkKeywordLE, MinSize: decl.Size{Width: 200, Height: 0}},
							decl.PushButton{Text: "查询", OnClicked: func() { a.onCdkSearch() }},
							decl.PushButton{Text: "刷新列表", OnClicked: func() { a.onCdkRefresh() }},
							decl.PushButton{Text: "导出 TXT(当前查询)", OnClicked: func() { a.onCdkExportTxt() }, ToolTipText: "导出当前筛选条件下的全部 CDK 为 txt，每行：兑换码 名称 状态"},
						},
					},
					decl.TableView{
						AssignTo: &a.cdkCodeTable,
						Columns: []decl.TableViewColumn{
							{Title: "兑换码", Width: 160},
							{Title: "名称", Width: 90},
							{Title: "可兑次", Width: 60},
							{Title: "已兑次", Width: 60},
							{Title: "状态", Width: 55},
							{Title: "备注", Width: 150},
							{Title: "创建时间", Width: 130},
						},
						Model:   &cdkCodeModel{},
						MinSize: decl.Size{Width: 820, Height: 100},
						MaxSize: decl.Size{Width: 0, Height: 150},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.PushButton{Text: "作废选中 CDK", OnClicked: func() { a.onCdkDisable() }},
							decl.PushButton{Text: "查看兑换流水", OnClicked: func() { a.onCdkShowRedemptions() }},
						},
					},
				},
			},
			decl.GroupBox{
				Title:  "兑换流水",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "筛选码(空=全部):"},
							decl.LineEdit{AssignTo: &a.cdkRedeemFilter, MinSize: decl.Size{Width: 220, Height: 0}},
							decl.PushButton{Text: "刷新流水", OnClicked: func() { a.onCdkRefreshRedemptions() }},
							decl.PushButton{Text: "补发失败邮件", OnClicked: func() { a.onCdkRetryMails() }, ToolTipText: "对当前筛选码下 mail_status=failed 的流水重发系统邮件"},
						},
					},
					decl.TableView{
						AssignTo: &a.cdkRedeemTable,
						Columns: []decl.TableViewColumn{
							{Title: "兑换码", Width: 155},
							{Title: "账号", Width: 80},
							{Title: "角色", Width: 90},
							{Title: "邮件状态", Width: 70},
							{Title: "失败原因", Width: 180},
							{Title: "IP", Width: 100},
							{Title: "兑换时间", Width: 130},
						},
						Model:   &cdkRedemptionModel{},
						MinSize: decl.Size{Width: 820, Height: 80},
						MaxSize: decl.Size{Width: 0, Height: 130},
					},
				},
			},
		},
	}
}

// ============ 奖励物品表格 ============

func (a *App) syncCdkRewardModel() {
	if a.cdkRewardTable == nil {
		return
	}
	a.cdkRewardTable.SetModel(&cdkRewardModel{rows: a.cdkRewardRows})
}

func (a *App) onCdkAddRewardRow() {
	if len(a.cdkRewardRows) >= cdkMaxRewardRows {
		a.showInfo("奖励", fmt.Sprintf("最多支持 %d 行奖励", cdkMaxRewardRows))
		return
	}
	id := strings.TrimSpace(a.cdkItemIDEdit.Text())
	cnt := strings.TrimSpace(a.cdkItemCntEdit.Text())
	if id == "" || cnt == "" {
		a.showInfo("奖励物品", "请填写 item_id 与数量")
		return
	}
	if err := validateRewardCell(id, cnt); err != nil {
		a.showInfo("奖励物品", err.Error())
		return
	}
	a.cdkRewardRows = append(a.cdkRewardRows, cdkRewardRow{Kind: cdkRewardKindItem, ItemID: id, Count: cnt})
	a.cdkItemIDEdit.SetText("")
	a.cdkItemCntEdit.SetText("")
	a.syncCdkRewardModel()
}

// onCdkAddCeraRow 把"点券"输入框的数量作为一行点券奖励加进列表。
func (a *App) onCdkAddCeraRow() {
	if len(a.cdkRewardRows) >= cdkMaxRewardRows {
		a.showInfo("奖励", fmt.Sprintf("最多支持 %d 行奖励", cdkMaxRewardRows))
		return
	}
	amount := strings.TrimSpace(a.cdkCeraEdit.Text())
	if amount == "" {
		a.showInfo("奖励点券", "请先填写点券数量")
		return
	}
	value, err := strconv.ParseInt(amount, 10, 64)
	if err != nil || value <= 0 || value > cdkMaxCeraAmount {
		a.showInfo("奖励点券", fmt.Sprintf("点券数量必须是 1~%d 的整数", cdkMaxCeraAmount))
		return
	}
	a.cdkRewardRows = append(a.cdkRewardRows, cdkRewardRow{Kind: cdkRewardKindCera, Count: strconv.FormatInt(value, 10)})
	a.cdkCeraEdit.SetText("")
	a.syncCdkRewardModel()
}

// onCdkAddBlackDiamondRow 把"黑钻(天)"输入框的天数作为一行黑钻奖励加进列表。
func (a *App) onCdkAddBlackDiamondRow() {
	if len(a.cdkRewardRows) >= cdkMaxRewardRows {
		a.showInfo("奖励", fmt.Sprintf("最多支持 %d 行奖励", cdkMaxRewardRows))
		return
	}
	text := strings.TrimSpace(a.cdkBdDaysEdit.Text())
	if text == "" {
		a.showInfo("奖励黑钻", "请先填写黑钻天数")
		return
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 || value > cdkMaxBlackDiamondDays {
		a.showInfo("奖励黑钻", fmt.Sprintf("黑钻天数必须是 1~%d 的整数（按天算）", cdkMaxBlackDiamondDays))
		return
	}
	// 多行黑钻会累加后一次性激活，这里先按已加行提示，避免生成时才发现超上限。
	if total := cdkRowsBlackDiamondDays(a.cdkRewardRows) + value; total > cdkMaxBlackDiamondDays {
		a.showInfo("奖励黑钻", fmt.Sprintf("黑钻天数合计 %d 天超过上限 %d 天", total, cdkMaxBlackDiamondDays))
		return
	}
	a.cdkRewardRows = append(a.cdkRewardRows, cdkRewardRow{Kind: cdkRewardKindBlackDiamond, Count: strconv.FormatInt(value, 10)})
	a.cdkBdDaysEdit.SetText("")
	a.syncCdkRewardModel()
}

// cdkRowsBlackDiamondDays 汇总编辑器中已添加的黑钻行天数（添加时先自查上限）。
func cdkRowsBlackDiamondDays(rows []cdkRewardRow) int64 {
	var total int64
	for _, row := range rows {
		if !row.isBlackDiamond() {
			continue
		}
		days, err := strconv.ParseInt(strings.TrimSpace(row.Count), 10, 64)
		if err != nil || days <= 0 {
			continue
		}
		total += days
	}
	return total
}

func (a *App) onCdkRemoveRewardRow() {
	if a.cdkRewardTable == nil {
		return
	}
	idx := a.cdkRewardTable.CurrentIndex()
	if idx < 0 || idx >= len(a.cdkRewardRows) {
		a.showInfo("删除行", "请在奖励列表中选中一行")
		return
	}
	// 允许删到一行不剩（生成时再校验非空）：以前强制留 1 行，才会出现那个空行。
	a.cdkRewardRows = append(a.cdkRewardRows[:idx], a.cdkRewardRows[idx+1:]...)
	a.syncCdkRewardModel()
}

func validateRewardCell(itemID, count string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(itemID), 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("item_id 必须是正整数")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
	if err != nil || n < 1 {
		return fmt.Errorf("count 必须是 ≥1 的整数")
	}
	return nil
}

// buildRewardTexts 汇总奖励为奖励段文本与 reward_json（与服务端 content_raw 约定一致）。
// 物品是 itemId:count、点券是 cera:数量、黑钻是 black_diamond:天数，逗号连接
// （例如 "1001:2,cera:5000,black_diamond:30"）。
func buildRewardTexts(rows []cdkRewardRow) (string, string, error) {
	parts := make([]string, 0, len(rows))
	rewards := make([]cdkRewardJSON, 0, len(rows))
	itemRows, ceraRows, bdRows := 0, 0, 0
	var bdTotal int64
	for i, row := range rows {
		if strings.TrimSpace(row.Count) == "" {
			return "", "", fmt.Errorf("第 %d 行数量为空", i+1)
		}
		if row.isCera() {
			amount, err := strconv.ParseInt(strings.TrimSpace(row.Count), 10, 64)
			if err != nil || amount <= 0 || amount > cdkMaxCeraAmount {
				return "", "", fmt.Errorf("第 %d 行点券数量必须是 1~%d 的整数", i+1, cdkMaxCeraAmount)
			}
			ceraRows++
			parts = append(parts, fmt.Sprintf("cera:%d", amount))
			rewards = append(rewards, cdkRewardJSON{Cera: amount})
			continue
		}
		if row.isBlackDiamond() {
			days, err := strconv.ParseInt(strings.TrimSpace(row.Count), 10, 64)
			if err != nil || days <= 0 || days > cdkMaxBlackDiamondDays {
				return "", "", fmt.Errorf("第 %d 行黑钻天数必须是 1~%d 的整数（按天算）", i+1, cdkMaxBlackDiamondDays)
			}
			if bdTotal+days > cdkMaxBlackDiamondDays {
				return "", "", fmt.Errorf("第 %d 行黑钻天数合计 %d 天超过上限 %d 天（多行黑钻按天数累加后一次性激活）", i+1, bdTotal+days, cdkMaxBlackDiamondDays)
			}
			bdTotal += days
			bdRows++
			parts = append(parts, fmt.Sprintf("black_diamond:%d", days))
			rewards = append(rewards, cdkRewardJSON{BlackDiamondDays: days})
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(row.ItemID), 10, 32)
		if err != nil || id == 0 {
			return "", "", fmt.Errorf("第 %d 行 item_id 不是正整数", i+1)
		}
		cnt, err := strconv.ParseUint(strings.TrimSpace(row.Count), 10, 32)
		if err != nil || cnt == 0 {
			return "", "", fmt.Errorf("第 %d 行 count 必须是 ≥1 的整数", i+1)
		}
		itemRows++
		parts = append(parts, fmt.Sprintf("%d:%d", id, cnt))
		rewards = append(rewards, cdkRewardJSON{ItemID: uint32(id), Count: uint32(cnt), Bind: false})
	}
	if itemRows == 0 && ceraRows == 0 && bdRows == 0 {
		return "", "", fmt.Errorf("至少需要一行奖励（物品、点券或黑钻）")
	}
	raw, err := json.Marshal(rewards)
	if err != nil {
		return "", "", fmt.Errorf("序列化奖励 JSON 失败: %w", err)
	}
	return strings.Join(parts, ","), string(raw), nil
}

// ============ 生成 CDK ============

func (a *App) onCdkGen() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	name := strings.TrimSpace(a.cdkNameEdit.Text())
	if name == "" {
		a.showInfo("生成 CDK", "请填写 CDK 名称")
		return
	}
	if strings.ContainsAny(name, "#&") {
		a.showInfo("生成 CDK", "CDK 名称不能包含 # 与 & 字符")
		return
	}
	times := int(a.cdkTimesEdit.Value())
	if times < 0 {
		times = 0
	}
	genCount := int(a.cdkGenCountEdit.Value())
	if genCount < 1 || genCount > 1000 {
		a.showInfo("生成 CDK", "制作数量必须在 1-1000 之间")
		return
	}
	if len(a.cdkRewardRows) == 0 {
		a.showInfo("生成 CDK", "请先添加奖励：至少一行物品、点券或黑钻")
		return
	}
	itemStr, rewardJSON, err := buildRewardTexts(a.cdkRewardRows)
	if err != nil {
		a.showInfo("生成 CDK", err.Error())
		return
	}
	remark := strings.TrimSpace(a.cdkRemarkEdit.Text())
	// contentRaw 与服务端约定一致：{次数}#{名称}#{奖励段}
	//（物品 itemId:count、点券 cera:数量、黑钻 black_diamond:天数）。
	contentRaw := fmt.Sprintf("%d#%s#%s", times, name, itemStr)

	if !a.confirm("生成 CDK", fmt.Sprintf("确认生成 CDK 批次？\n名称：%s\n数量：%d\n单码次数：%d\n奖励：%s", name, genCount, times, itemStr)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	a.sync(func() { a.cdkGenBtn.SetEnabled(false) })
	defer func() { a.sync(func() { a.cdkGenBtn.SetEnabled(true) }) }()

	kw := strings.TrimSpace(a.cdkKeywordLE.Text())
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		codes, err := c.CdkCreate(ctx, name, genCount, times, contentRaw, rewardJSON, remark)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("生成失败", err)
			}
			return
		}
		exportPath, exportErr := cdkAutoExportTxt(codes, name)
		a.loadCdkCodes(kw)
		a.sync(func() {
			msg := fmt.Sprintf("已成功生成 %d 张 CDK\n\n首码: %s", len(codes), orEmpty(codes))
			switch {
			case exportErr != nil:
				msg += "\n\n自动导出 TXT 失败: " + exportErr.Error()
			case exportPath != "":
				msg += "\n\n已自动导出 TXT:\n" + exportPath
			}
			a.showInfo("生成完成", msg)
		})
	}()
}

func orEmpty(codes []string) string {
	if len(codes) == 0 {
		return ""
	}
	return codes[0]
}

// ============ CDK 列表 ============

func (a *App) loadCdkCodes(keyword string) {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := c.CdkList(ctx, keyword, 1, cdkCodeListPageSize)
	if err != nil {
		if !a.handleAPIError(err) {
			a.showError("CDK 列表拉取失败", err)
		}
		return
	}
	if data.Items == nil {
		data.Items = []CdkOut{}
	}
	model := &cdkCodeModel{items: data.Items}
	a.sync(func() {
		if a.cdkCodeTable != nil {
			a.cdkCodeTable.SetModel(model)
		}
	})
}

func (a *App) onCdkSearch() {
	a.loadCdkCodes(strings.TrimSpace(a.cdkKeywordLE.Text()))
}

func (a *App) onCdkRefresh() {
	a.loadCdkCodes(strings.TrimSpace(a.cdkKeywordLE.Text()))
}

// selectedCdkCode 返回 CDK 列表选中行的兑换码（未选中返回空串）。
func (a *App) selectedCdkCode() string {
	if a.cdkCodeTable == nil {
		return ""
	}
	idx := a.cdkCodeTable.CurrentIndex()
	model, ok := a.cdkCodeTable.Model().(*cdkCodeModel)
	if !ok || idx < 0 || idx >= len(model.items) {
		return ""
	}
	return model.items[idx].RedeemCode
}

// ============ 导出 TXT ============

func cdkExportBaseDir() string {
	dir := ""
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	if dir == "" || strings.Contains(strings.ToLower(filepath.ToSlash(dir)), "go-build") {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if dir == "" {
		dir = "."
	}
	out := filepath.Join(dir, "cdk_export")
	_ = os.MkdirAll(out, 0o755)
	return out
}

func cdkExportFileName(prefix, name string) string {
	safe := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", "#", "_", "&", "_").Replace(strings.TrimSpace(name))
	safe = strings.TrimSpace(strings.Trim(safe, ". "))
	if safe == "" {
		safe = "CDK"
	}
	ts := time.Now().Format("20060102_150405")
	if prefix != "" {
		return fmt.Sprintf("%s_%s_%s.txt", prefix, safe, ts)
	}
	return fmt.Sprintf("%s_%s.txt", safe, ts)
}

func cdkStatusText(c CdkOut) string {
	if !c.Enabled {
		return "已作废"
	}
	if c.MaxRedeems > 0 && c.RedeemedCount >= c.MaxRedeems {
		return "已兑完"
	}
	if c.RedeemedCount > 0 {
		return "已兑换"
	}
	return "未兑换"
}

func buildCdkTxtContent(lines [][3]string) string {
	var b strings.Builder
	b.WriteString("\xef\xbb\xbf")
	for _, l := range lines {
		b.WriteString(l[0])
		b.WriteByte(' ')
		b.WriteString(l[1])
		b.WriteByte(' ')
		b.WriteString(l[2])
		b.WriteString("\r\n")
	}
	return b.String()
}

func cdkAutoExportTxt(codes []string, name string) (string, error) {
	if len(codes) == 0 {
		return "", nil
	}
	lines := make([][3]string, 0, len(codes))
	for _, code := range codes {
		lines = append(lines, [3]string{code, name, "未兑换"})
	}
	path := filepath.Join(cdkExportBaseDir(), cdkExportFileName("CDK", name))
	if err := os.WriteFile(path, []byte(buildCdkTxtContent(lines)), 0o666); err != nil {
		return "", err
	}
	return path, nil
}

func (a *App) fetchAllCdkCodes(ctx context.Context, c *APIClient, kw string) ([]CdkOut, error) {
	var all []CdkOut
	for page := 1; ; page++ {
		data, err := c.CdkList(ctx, kw, page, cdkCodeListPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, data.Items...)
		if len(data.Items) < cdkCodeListPageSize {
			break
		}
	}
	return all, nil
}

func (a *App) onCdkExportTxt() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	kw := strings.TrimSpace(a.cdkKeywordLE.Text())
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		all, err := a.fetchAllCdkCodes(ctx, c, kw)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("导出失败", err)
			}
			return
		}
		if len(all) == 0 {
			a.showInfo("导出 CDK", "当前筛选条件下没有可导出的 CDK")
			return
		}
		a.sync(func() {
			dlg := new(walk.FileDialog)
			dlg.Title = "导出 CDK 为 TXT"
			dlg.Filter = "文本文件 (*.txt)|*.txt|所有文件 (*.*)|*.*"
			dlg.FilePath = filepath.Join(cdkExportBaseDir(), cdkExportFileName("", "CDK_导出"))
			ok, err := dlg.ShowSave(a.mw)
			if err != nil {
				a.showError("导出 CDK", err)
				return
			}
			if !ok {
				return
			}
			lines := make([][3]string, 0, len(all))
			for _, it := range all {
				lines = append(lines, [3]string{it.RedeemCode, it.Name, cdkStatusText(it)})
			}
			if err := os.WriteFile(dlg.FilePath, []byte(buildCdkTxtContent(lines)), 0o666); err != nil {
				a.showError("导出 CDK", err)
				return
			}
			a.showInfo("导出 CDK", fmt.Sprintf("已导出 %d 张 CDK\n\n%s", len(all), dlg.FilePath))
		})
	}()
}

// ============ 作废 ============

func (a *App) onCdkDisable() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	code := a.selectedCdkCode()
	if code == "" {
		a.showInfo("作废 CDK", "请先在 CDK 列表中选中一行")
		return
	}
	idx := a.cdkCodeTable.CurrentIndex()
	model, _ := a.cdkCodeTable.Model().(*cdkCodeModel)
	if !model.items[idx].Enabled {
		a.showInfo("作废 CDK", "该 CDK 已是作废状态")
		return
	}
	if !a.confirm("作废 CDK", "确定作废 CDK "+code+" ？\n作废后玩家不可再兑换，已兑换流水保留可追溯。") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	kw := strings.TrimSpace(a.cdkKeywordLE.Text())
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.CdkDisable(ctx, code); err != nil {
			if !a.handleAPIError(err) {
				a.showError("作废失败", err)
			}
			return
		}
		a.showInfo("作废完成", "该兑换码已作废。")
		a.loadCdkCodes(kw)
	}()
}

// ============ 兑换流水 ============

func (a *App) loadCdkRedemptions(code string) {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reds, err := c.CdkRedemptions(ctx, code)
	if err != nil {
		if !a.handleAPIError(err) {
			a.showError("兑换流水拉取失败", err)
		}
		return
	}
	model := &cdkRedemptionModel{items: reds}
	a.sync(func() {
		if a.cdkRedeemTable != nil {
			a.cdkRedeemTable.SetModel(model)
		}
	})
}

func (a *App) onCdkRefreshRedemptions() {
	a.loadCdkRedemptions(strings.TrimSpace(a.cdkRedeemFilter.Text()))
}

func (a *App) onCdkShowRedemptions() {
	code := a.selectedCdkCode()
	if code == "" {
		a.showInfo("查看兑换流水", "请先在 CDK 列表中选中一行")
		return
	}
	if a.cdkRedeemFilter != nil {
		a.cdkRedeemFilter.SetText(code)
	}
	a.loadCdkRedemptions(code)
}

// ============ 补发失败邮件 ============

func (a *App) onCdkRetryMails() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	code := strings.TrimSpace(a.cdkRedeemFilter.Text())
	if code == "" {
		a.showInfo("补发失败邮件", "请先在「CDK 列表」选中目标码并点击「查看兑换流水」，或在上方筛选框填写兑换码")
		return
	}
	if !a.confirm("补发确认", fmt.Sprintf("确认对兑换码 %s 下的失败邮件执行补发？\n补发由服务器端逐个调用 GM 邮件接口并回写状态，可能耗时较长。", code)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sent, failed, err := c.CdkRetryMail(ctx, code)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("补发失败", err)
			}
			return
		}
		a.showInfo("补发完成", fmt.Sprintf("补发完成：成功 %d 条，仍失败 %d 条。", sent, failed))
		a.loadCdkRedemptions(code)
	}()
}
