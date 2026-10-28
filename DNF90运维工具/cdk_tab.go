// CDK 管理页：生成 CDK（批次）→ 列表/作废 → 兑换流水（补发失败邮件）。
// 数据层复用 internal/store 的 redeem_*（认证服务启动即自动建表 EnsureRedeemSchema）。
// contentRaw 格式与后端方案完全一致：{次数}#{名称}#{itemId:count,cera:数量,black_diamond:天数}
// reward_json 为 JSON 数组 [{item_id,count,bind}|{cera}|{black_diamond_days}]，供
// server.splitRedeemRewards 拆出邮件附件 / 点券入账 / 黑钻激活三路发奖。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dnf90ops/internal/gmhttp"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
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

// cdkRewardJSON 与 server.redeemReward / GM 邮件附件对齐。
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

// cdkGMEnvelope 服务端 /local/dnf-gm/* 统一响应信封（对齐 server.gmEnvelope）。
type cdkGMEnvelope struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

// cdkGMMailAttachment 与 go-server dnfbridge.GMSystemMailAttachment 对齐。
type cdkGMMailAttachment struct {
	ItemID uint32 `json:"item_id"`
	Count  uint32 `json:"count"`
	Bind   bool   `json:"bind"`
}

// cdkGMMailRequest 与 go-server dnfbridge.GMSystemMailRequest 对齐。
type cdkGMMailRequest struct {
	CharacterID string                `json:"character_id,omitempty"`
	Title       string                `json:"title,omitempty"`
	Body        string                `json:"body,omitempty"`
	Attachments []cdkGMMailAttachment `json:"attachments,omitempty"`
}

const (
	cdkMaxRewardRows    = 20
	cdkCodeListPageSize = 500
	cdkGMTimeout        = 8 * time.Second
	// cdkMaxCeraAmount 单张 CDK 的点券上限：与服务端钱包 add 的 2147483647 上限一致。
	cdkMaxCeraAmount = 2147483647
	// cdkMaxBlackDiamondDays 单张 CDK 的黑钻总天数上限：与服务端 GM 黑钻接口的
	// 1..3650 天校验一致（多行黑钻按天数累加后一次性激活）。
	cdkMaxBlackDiamondDays = 3650
)

// cdkTab UI 控件句柄。
type cdkTabWidgets struct {
	// 生成表单
	nameEdit     *walk.LineEdit
	timesEdit    *walk.NumberEdit // 单码可兑换次数，0=无限
	genCountEdit *walk.NumberEdit // 制作数量 1-1000
	remarkEdit   *walk.LineEdit
	rewardTable  *walk.TableView
	itemIDEdit   *walk.LineEdit
	itemCntEdit  *walk.LineEdit
	ceraEdit     *walk.LineEdit
	bdDaysEdit   *walk.LineEdit
	genBtn       *walk.PushButton

	// CDK 列表
	codeTable   *walk.TableView
	codeKeyword *walk.LineEdit

	// 兑换流水
	redeemTable  *walk.TableView
	redeemFilter *walk.LineEdit

	hintL *walk.Label

	rewardRows []cdkRewardRow
}

var cdkWidgets cdkTabWidgets

// cdkValidStore 取账号库连接（认证服务运行中才可用）。
func (a *App) cdkValidStore() *store.Store {
	if !a.svcOn.Load() || a.st == nil {
		walk.MsgBox(a.mw, "CDK 管理", "认证服务未运行，请先在「认证服务」页启动后再操作 CDK", walk.MsgBoxIconWarning)
		return nil
	}
	return a.st
}

func (a *App) cdkTab() []Widget {
	// 奖励列表**默认留空**：以前这里预置了一行空物品，用户一看列表里有个空行很莫名其妙，
	// 而且真的点"生成"还会被校验拦下。现在由用户自己添加物品行/点券行。
	return []Widget{
		GroupBox{
			Title:  "生成 CDK（批次）",
			Layout: VBox{Margins: Margins{10, 6, 10, 2}, Spacing: 4},
			Children: []Widget{
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "CDK 名称:"},
						LineEdit{AssignTo: &cdkWidgets.nameEdit, MinSize: Size{Width: 150, Height: 0}, ToolTipText: "登录器兑换成功时回显的名称；禁止包含 # 与 &"},
						Label{Text: "单码次数:"},
						NumberEdit{AssignTo: &cdkWidgets.timesEdit, Value: 1.0, MinValue: 0, MaxValue: 999999, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 72, Height: 0}, ToolTipText: "0 = 无限次"},
						Label{Text: "制作数量:"},
						NumberEdit{AssignTo: &cdkWidgets.genCountEdit, Value: 1.0, MinValue: 1, MaxValue: 1000, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 72, Height: 0}, ToolTipText: "1-1000"},
						Label{Text: "备注:"},
						LineEdit{AssignTo: &cdkWidgets.remarkEdit, MinSize: Size{Width: 200, Height: 0}},
					},
				},
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "点券:"},
						LineEdit{AssignTo: &cdkWidgets.ceraEdit, MinSize: Size{Width: 100, Height: 0}, ToolTipText: "要发放的点券数量（正整数，1~2147483647）。点券是账号通用余额，兑换时直接入账，不走邮件。"},
						PushButton{Text: "添加点券", OnClicked: a.onCdkAddCeraRow},
						Label{Text: "(点券直接入账号余额；可加多行，兑换时累加)"},
					},
				},
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "黑钻(天):"},
						LineEdit{AssignTo: &cdkWidgets.bdDaysEdit, MinSize: Size{Width: 100, Height: 0}, ToolTipText: "要发放的黑钻天数（正整数，1~3650）。黑钻是账号级权益，兑换时按所选角色直接激活，不走邮件；可加多行，按天数累加后一次激活。"},
						PushButton{Text: "添加黑钻", OnClicked: a.onCdkAddBlackDiamondRow},
						Label{Text: "(黑钻按天算；到期时间在原到期时间上叠加)"},
					},
				},
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "奖励物品(item_id):"},
						LineEdit{AssignTo: &cdkWidgets.itemIDEdit, MinSize: Size{Width: 100, Height: 0}},
						Label{Text: "数量:"},
						LineEdit{AssignTo: &cdkWidgets.itemCntEdit, MinSize: Size{Width: 70, Height: 0}},
						PushButton{Text: "添加物品", OnClicked: a.onCdkAddRewardRow},
						PushButton{Text: "删除选中行", OnClicked: a.onCdkRemoveRewardRow},
						Label{Text: "(最多 20 行；item_id/数量 为正整数，count≥1)"},
					},
				},
				TableView{
					AssignTo: &cdkWidgets.rewardTable,
					Columns: []TableViewColumn{
						{Title: "行号", Width: 50},
						{Title: "类型", Width: 60},
						{Title: "item_id", Width: 140},
						{Title: "数量", Width: 100},
					},
					Model:   new(cdkRewardModel),
					MinSize: Size{Width: 820, Height: 80},
					MaxSize: Size{Width: 0, Height: 110},
				},
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 8},
					Children: []Widget{
						PushButton{AssignTo: &cdkWidgets.genBtn, Text: "生成 CDK", OnClicked: a.onCdkGen},
						Label{AssignTo: &cdkWidgets.hintL, Text: "生成结果将写入账号库 CDK 主表（两表随认证服务启动自动创建）", TextColor: walk.RGB(90, 90, 90)},
					},
				},
			},
		},
		GroupBox{
			Title:  "CDK 列表",
			Layout: VBox{Margins: Margins{10, 4, 10, 2}, Spacing: 4},
			Children: []Widget{
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "关键字(码/名称):"},
						LineEdit{AssignTo: &cdkWidgets.codeKeyword, MinSize: Size{Width: 200, Height: 0}},
						PushButton{Text: "查询", OnClicked: a.onCdkSearch},
						PushButton{Text: "刷新列表", OnClicked: a.onCdkRefresh},
						PushButton{Text: "导出 TXT（当前查询）", OnClicked: a.onCdkExportTxt, ToolTipText: "将当前筛选条件下全部 CDK 导出为 txt，每行格式：兑换码 名称 状态"},
					},
				},
				TableView{
					AssignTo: &cdkWidgets.codeTable,
					Columns: []TableViewColumn{
						{Title: "兑换码", Width: 160},
						{Title: "名称", Width: 90},
						{Title: "可兑换", Width: 60},
						{Title: "已兑换", Width: 60},
						{Title: "状态", Width: 55},
						{Title: "备注", Width: 150},
						{Title: "创建时间", Width: 130},
					},
					Model:   new(cdkCodeModel),
					MinSize: Size{Width: 820, Height: 100},
					MaxSize: Size{Width: 0, Height: 150},
				},
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 8},
					Children: []Widget{
						PushButton{Text: "作废选中 CDK", OnClicked: a.onCdkDisable},
						PushButton{Text: "查看兑换流水", OnClicked: a.onCdkShowRedemptions},
					},
				},
			},
		},
		GroupBox{
			Title:  "兑换流水",
			Layout: VBox{Margins: Margins{10, 4, 10, 4}, Spacing: 4},
			Children: []Widget{
				Composite{
					Layout: HBox{Margins: Margins{0, 0, 0, 0}, Spacing: 6},
					Children: []Widget{
						Label{Text: "筛选码(留空=全部):"},
						LineEdit{AssignTo: &cdkWidgets.redeemFilter, MinSize: Size{Width: 220, Height: 0}},
						PushButton{Text: "刷新流水", OnClicked: a.onCdkRefreshRedemptions},
						PushButton{Text: "补发失败邮件", OnClicked: a.onCdkRetryMails, ToolTipText: "对当前筛选码下 mail_status=failed 的流水逐个重发系统邮件（需服务端已运行）"},
					},
				},
				TableView{
					AssignTo: &cdkWidgets.redeemTable,
					Columns: []TableViewColumn{
						{Title: "兑换码", Width: 155},
						{Title: "账号", Width: 80},
						{Title: "角色", Width: 90},
						{Title: "邮件状态", Width: 70},
						{Title: "错误", Width: 180},
						{Title: "IP", Width: 100},
						{Title: "兑换时间", Width: 130},
					},
					Model:   new(cdkRedemptionModel),
					MinSize: Size{Width: 820, Height: 80},
					MaxSize: Size{Width: 0, Height: 130},
				},
			},
		},
	}
}

// ============ 奖励行编辑器 ============

// cdkRewardModel 生成表单奖励行模型。
type cdkRewardModel struct {
	walk.TableModelBase
}

func (m *cdkRewardModel) RowCount() int { return len(cdkWidgets.rewardRows) }

func (m *cdkRewardModel) Value(row, col int) any {
	if row < 0 || row >= len(cdkWidgets.rewardRows) {
		return nil
	}
	r := cdkWidgets.rewardRows[row]
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

func (a *App) syncRewardModel() {
	if cdkWidgets.rewardTable == nil {
		return
	}
	cdkWidgets.rewardTable.SetModel(new(cdkRewardModel))
}

func (a *App) onCdkAddRewardRow() {
	if len(cdkWidgets.rewardRows) >= cdkMaxRewardRows {
		walk.MsgBox(a.mw, "奖励物品行", fmt.Sprintf("最多支持 %d 行奖励物品", cdkMaxRewardRows), walk.MsgBoxIconWarning)
		return
	}
	id := strings.TrimSpace(cdkWidgets.itemIDEdit.Text())
	cnt := strings.TrimSpace(cdkWidgets.itemCntEdit.Text())
	if id == "" || cnt == "" {
		walk.MsgBox(a.mw, "奖励物品行", "请先填写 item_id 与数量", walk.MsgBoxIconWarning)
		return
	}
	// 添加前做基础数字校验，错误立即定位到当前行。
	if err := validateRewardCell(id, cnt); err != nil {
		walk.MsgBox(a.mw, "奖励物品行", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	cdkWidgets.rewardRows = append(cdkWidgets.rewardRows, cdkRewardRow{Kind: cdkRewardKindItem, ItemID: id, Count: cnt})
	cdkWidgets.itemIDEdit.SetText("")
	cdkWidgets.itemCntEdit.SetText("")
	a.syncRewardModel()
}

// onCdkAddCeraRow 把"点券"输入框的数量作为一行点券奖励加进列表。
func (a *App) onCdkAddCeraRow() {
	if len(cdkWidgets.rewardRows) >= cdkMaxRewardRows {
		walk.MsgBox(a.mw, "奖励点券", fmt.Sprintf("奖励最多 %d 行", cdkMaxRewardRows), walk.MsgBoxIconWarning)
		return
	}
	amount := strings.TrimSpace(cdkWidgets.ceraEdit.Text())
	if amount == "" {
		walk.MsgBox(a.mw, "奖励点券", "请先填写点券数量", walk.MsgBoxIconWarning)
		return
	}
	value, err := strconv.ParseInt(amount, 10, 64)
	if err != nil || value <= 0 || value > cdkMaxCeraAmount {
		walk.MsgBox(a.mw, "奖励点券", fmt.Sprintf("点券数量必须是 1~%d 的整数", cdkMaxCeraAmount), walk.MsgBoxIconWarning)
		return
	}
	cdkWidgets.rewardRows = append(cdkWidgets.rewardRows, cdkRewardRow{Kind: cdkRewardKindCera, Count: strconv.FormatInt(value, 10)})
	cdkWidgets.ceraEdit.SetText("")
	a.syncRewardModel()
}

// onCdkAddBlackDiamondRow 把"黑钻(天)"输入框的天数作为一行黑钻奖励加进列表。
func (a *App) onCdkAddBlackDiamondRow() {
	if len(cdkWidgets.rewardRows) >= cdkMaxRewardRows {
		walk.MsgBox(a.mw, "奖励黑钻", fmt.Sprintf("奖励最多 %d 行", cdkMaxRewardRows), walk.MsgBoxIconWarning)
		return
	}
	text := strings.TrimSpace(cdkWidgets.bdDaysEdit.Text())
	if text == "" {
		walk.MsgBox(a.mw, "奖励黑钻", "请先填写黑钻天数", walk.MsgBoxIconWarning)
		return
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value <= 0 || value > cdkMaxBlackDiamondDays {
		walk.MsgBox(a.mw, "奖励黑钻", fmt.Sprintf("黑钻天数必须是 1~%d 的整数（按天算）", cdkMaxBlackDiamondDays), walk.MsgBoxIconWarning)
		return
	}
	// 多行黑钻会累加后一次性激活，这里先按已加行提示，避免生成时才发现超上限。
	if total := cdkRowsBlackDiamondDays(cdkWidgets.rewardRows) + value; total > cdkMaxBlackDiamondDays {
		walk.MsgBox(a.mw, "奖励黑钻", fmt.Sprintf("黑钻天数合计 %d 天超过上限 %d 天", total, cdkMaxBlackDiamondDays), walk.MsgBoxIconWarning)
		return
	}
	cdkWidgets.rewardRows = append(cdkWidgets.rewardRows, cdkRewardRow{Kind: cdkRewardKindBlackDiamond, Count: strconv.FormatInt(value, 10)})
	cdkWidgets.bdDaysEdit.SetText("")
	a.syncRewardModel()
}

func (a *App) onCdkRemoveRewardRow() {
	if cdkWidgets.rewardTable == nil {
		return
	}
	idx := cdkWidgets.rewardTable.CurrentIndex()
	if idx < 0 || idx >= len(cdkWidgets.rewardRows) {
		walk.MsgBox(a.mw, "删除行", "请先在奖励列表里选中一行", walk.MsgBoxIconWarning)
		return
	}
	// 允许删到一行不剩：奖励列表本来就是"你自己加多少就是多少"，
	// 生成时再校验非空即可（之前强制留 1 行，才会出现那个莫名其妙的空行）。
	cdkWidgets.rewardRows = append(cdkWidgets.rewardRows[:idx], cdkWidgets.rewardRows[idx+1:]...)
	a.syncRewardModel()
}

// validateRewardCell 校验单个物品行的 item_id/count 文本，错误信息含当前行号由调用方补充。
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

// buildRewardTexts 汇总奖励行为 item 串 与 reward_json（不重复解析，格式一致性核心）。
// 返回 contentRaw 奖励段与结构化 JSON 文本；奖励段里物品是 itemId:count、点券是 cera:数量、
// 黑钻是 black_diamond:天数，用逗号连接（例如 "1001:2,cera:5000,black_diamond:30"）。
// content_raw 只作文本留档，权威数据是 reward_json。
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
			return "", "", fmt.Errorf("第 %d 行 item_id 必须是正整数", i+1)
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

// onCdkGen 校验表单 → 组装 contentRaw/reward_json → store.CreateRedeemCodeBatch。
func (a *App) onCdkGen() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	name := strings.TrimSpace(cdkWidgets.nameEdit.Text())
	if name == "" {
		walk.MsgBox(a.mw, "生成 CDK", "请填写 CDK 名称", walk.MsgBoxIconWarning)
		return
	}
	if strings.ContainsAny(name, "#&") {
		walk.MsgBox(a.mw, "生成 CDK", "CDK 名称不能包含 # 与 & 字符", walk.MsgBoxIconWarning)
		return
	}
	times := int(cdkWidgets.timesEdit.Value())
	if times < 0 {
		times = 0
	}
	genCount := int(cdkWidgets.genCountEdit.Value())
	if genCount < 1 || genCount > 1000 {
		walk.MsgBox(a.mw, "生成 CDK", "制作数量必须在 1-1000 之间", walk.MsgBoxIconWarning)
		return
	}
	if len(cdkWidgets.rewardRows) == 0 {
		walk.MsgBox(a.mw, "生成 CDK", "请先添加奖励：至少一行物品、点券或黑钻", walk.MsgBoxIconWarning)
		return
	}
	if len(cdkWidgets.rewardRows) > cdkMaxRewardRows {
		walk.MsgBox(a.mw, "生成 CDK", fmt.Sprintf("奖励最多 %d 行", cdkMaxRewardRows), walk.MsgBoxIconWarning)
		return
	}
	itemStr, rewardJSON, err := buildRewardTexts(cdkWidgets.rewardRows)
	if err != nil {
		walk.MsgBox(a.mw, "生成 CDK", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	remark := strings.TrimSpace(cdkWidgets.remarkEdit.Text())
	// contentRaw 必须与后端约定格式一致：{次数}#{名称}#{奖励段}，
	// 奖励段里物品是 itemId:count、点券是 cera:数量、黑钻是 black_diamond:天数
	// （点券/黑钻为新增，老格式不受影响）。
	contentRaw := fmt.Sprintf("%d#%s#%s", times, name, itemStr)

	var firstCode string
	var exportPath string
	var exportErr error
	kw := strings.TrimSpace(cdkWidgets.codeKeyword.Text())
	a.runAsync("生成 CDK", func() error {
		codes, err := st.CreateRedeemCodeBatch(context.Background(), genCount, name, contentRaw, times, rewardJSON, remark, "ops")
		if err != nil {
			return err
		}
		if len(codes) > 0 {
			firstCode = codes[0]
			a.logf("已生成 %d 张 CDK（名称=%s，单码次数=%d），首码=%s", len(codes), name, times, firstCode)
			// 生成后自动导出为 txt（默认目录 cdk_export/，每行：兑换码 名称）。
			exportPath, exportErr = cdkAutoExportTxt(codes, name)
		}
		return a.loadCdkCodes(kw)
	}, func(err error) {
		if err != nil {
			walk.MsgBox(a.mw, "生成 CDK", "生成失败: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		msg := fmt.Sprintf("已成功生成 %d 张 CDK\n\n首码: %s\n\n完整列表见「CDK 列表」（筛选名称或直接刷新）", genCount, firstCode)
		switch {
		case exportErr != nil:
			msg += "\n\n自动导出 TXT 失败: " + exportErr.Error()
		case exportPath != "":
			msg += "\n\n已自动导出 TXT:\n" + exportPath
		}
		walk.MsgBox(a.mw, "生成 CDK", msg, walk.MsgBoxIconInformation)
	})
}

// ============ CDK 列表 ============

type cdkCodeModel struct {
	walk.TableModelBase
	items []store.RedeemCode
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
		return c.CreatedAt.Format("2006-01-02 15:04")
	}
	return ""
}

// selectedCdkCode 返回 CDK 列表选中行（未选中返回空串）。
func selectedCdkCode() string {
	if cdkWidgets.codeTable == nil {
		return ""
	}
	idx := cdkWidgets.codeTable.CurrentIndex()
	model, ok := cdkWidgets.codeTable.Model().(*cdkCodeModel)
	if !ok || idx < 0 || idx >= len(model.items) {
		return ""
	}
	return model.items[idx].RedeemCode
}

func (a *App) loadCdkCodes(keyword string) error {
	codes, err := a.st.ListRedeemCodes(context.Background(), keyword, 1, cdkCodeListPageSize)
	if err != nil {
		return err
	}
	model := &cdkCodeModel{items: codes}
	a.uiSafe(func() {
		if cdkWidgets.codeTable != nil {
			cdkWidgets.codeTable.SetModel(model)
		}
	})
	return nil
}

func (a *App) onCdkSearch() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	kw := strings.TrimSpace(cdkWidgets.codeKeyword.Text())
	a.runAsync("查询 CDK", func() error {
		return a.loadCdkCodes(kw)
	}, func(err error) {
		if err != nil {
			a.logf("查询 CDK 失败: %v", err)
		}
	})
}

func (a *App) onCdkRefresh() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	kw := strings.TrimSpace(cdkWidgets.codeKeyword.Text())
	a.runAsync("刷新 CDK 列表", func() error {
		return a.loadCdkCodes(kw)
	}, func(err error) {
		if err != nil {
			a.logf("刷新 CDK 列表失败: %v", err)
		} else {
			a.logf("CDK 列表已刷新")
		}
	})
}

// ============ CDK 导出 TXT ============

// cdkExportBaseDir 默认导出目录：exe 所在目录/cdk_export。
// go run 调试时 exe 落在 go-build 临时目录，退回当前工作目录，保证文件落在项目内。
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
	if err := os.MkdirAll(out, 0o755); err != nil {
		return filepath.Join(dir, "cdk_export") // 创建失败仍返回路径，写盘时报错更直观
	}
	return out
}

// cdkExportFileName 生成导出文件名：自动导出带 CDK 名称，时间戳精确到秒避免重名。
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

// cdkStatusText 导出用 CDK 状态文案：已作废 > 已兑完 > 已兑换 > 未兑换。
func cdkStatusText(c store.RedeemCode) string {
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

// buildCdkTxtContent 序列化为 UTF-8（含 BOM，兼容记事本/Excel）文本，每行格式：兑换码 名称 状态。
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

// cdkAutoExportTxt 生成成功后的自动导出（同一批次共用一个名称，新码均为未兑换），返回导出文件路径。
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

// fetchAllRedeemCodes 按当前关键字拉取全量 CDK（分页翻到底），供导出使用。
func (a *App) fetchAllRedeemCodes(ctx context.Context, kw string) ([]store.RedeemCode, error) {
	var all []store.RedeemCode
	for page := 1; ; page++ {
		rows, err := a.st.ListRedeemCodes(ctx, kw, page, cdkCodeListPageSize)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < cdkCodeListPageSize {
			break
		}
	}
	return all, nil
}

// onCdkExportTxt 导出当前筛选条件下全部 CDK：弹保存对话框写 txt，每行：兑换码 名称。
func (a *App) onCdkExportTxt() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	kw := strings.TrimSpace(cdkWidgets.codeKeyword.Text())
	var all []store.RedeemCode
	a.runAsync("导出 CDK", func() error {
		codes, err := a.fetchAllRedeemCodes(context.Background(), kw)
		if err != nil {
			return err
		}
		if len(codes) == 0 {
			return fmt.Errorf("当前筛选条件下没有可导出的 CDK")
		}
		all = codes
		return nil
	}, func(err error) {
		if err != nil {
			walk.MsgBox(a.mw, "导出 CDK", err.Error(), walk.MsgBoxIconWarning)
			return
		}
		dlg := new(walk.FileDialog)
		dlg.Title = "导出 CDK 为 TXT"
		dlg.Filter = "文本文件 (*.txt)|*.txt|所有文件 (*.*)|*.*"
		dlg.FilePath = filepath.Join(cdkExportBaseDir(), cdkExportFileName("", "CDK_导出"))
		ok, err := dlg.ShowSave(a.mw)
		if err != nil {
			walk.MsgBox(a.mw, "导出 CDK", "打开保存对话框失败: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		if !ok {
			a.logf("已取消导出 CDK")
			return
		}
		lines := make([][3]string, 0, len(all))
		for _, c := range all {
			lines = append(lines, [3]string{c.RedeemCode, c.Name, cdkStatusText(c)})
		}
		if err := os.WriteFile(dlg.FilePath, []byte(buildCdkTxtContent(lines)), 0o666); err != nil {
			walk.MsgBox(a.mw, "导出 CDK", "写入文件失败: "+err.Error(), walk.MsgBoxIconError)
			return
		}
		a.logf("已导出 %d 张 CDK 到 %s", len(all), dlg.FilePath)
		walk.MsgBox(a.mw, "导出 CDK", fmt.Sprintf("已导出 %d 张 CDK\n\n%s", len(all), dlg.FilePath), walk.MsgBoxIconInformation)
	})
}

// onCdkDisable 作废选中 CDK（enabled=0，保留流水）。
func (a *App) onCdkDisable() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	code := selectedCdkCode()
	if code == "" {
		walk.MsgBox(a.mw, "作废 CDK", "请先在 CDK 列表中选中一行", walk.MsgBoxIconWarning)
		return
	}
	idx := cdkWidgets.codeTable.CurrentIndex()
	model, _ := cdkWidgets.codeTable.Model().(*cdkCodeModel)
	if !model.items[idx].Enabled {
		walk.MsgBox(a.mw, "作废 CDK", "该 CDK 已是作废状态", walk.MsgBoxIconWarning)
		return
	}
	if walk.MsgBox(a.mw, "作废 CDK", "确定作废 CDK "+code+" ？\n作废后玩家不可再兑换，已兑换流水保留可追溯。", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		return
	}
	kw := strings.TrimSpace(cdkWidgets.codeKeyword.Text())
	a.runAsync("作废 CDK", func() error {
		if err := st.DisableRedeemCode(context.Background(), code); err != nil {
			return err
		}
		a.logf("CDK 已作废: %s", code)
		return a.loadCdkCodes(kw)
	}, func(err error) {
		if err != nil {
			walk.MsgBox(a.mw, "作废 CDK", "作废失败: "+err.Error(), walk.MsgBoxIconError)
			return
		}
	})
}

// ============ 兑换流水 ============

// cdkRedeemStatusText 流水状态的中文显示。
//
// partial 是"物品与点券只成功了一半"：点券入账成功但物品邮件失败（或反过来）。
// 它既不是"已送达"也不是"失败"，必须单独显示，否则运营会以为全部发完了。
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

type cdkRedemptionModel struct {
	walk.TableModelBase
	items []store.RedeemRedemption
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
		return r.Username
	case 2:
		return r.CharacterName
	case 3:
		return cdkRedeemStatusText(r.MailStatus)
	case 4:
		return r.MailError
	case 5:
		return r.RemoteIP
	case 6:
		return r.RedeemedAt.Format("2006-01-02 15:04")
	}
	return ""
}

func (a *App) loadCdkRedemptions(code string) error {
	reds, err := a.st.ListRedeemRedemptions(context.Background(), code, 1000)
	if err != nil {
		return err
	}
	model := &cdkRedemptionModel{items: reds}
	a.uiSafe(func() {
		if cdkWidgets.redeemTable != nil {
			cdkWidgets.redeemTable.SetModel(model)
		}
	})
	return nil
}

func (a *App) onCdkRefreshRedemptions() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	code := strings.TrimSpace(cdkWidgets.redeemFilter.Text())
	a.runAsync("刷新兑换流水", func() error {
		return a.loadCdkRedemptions(code)
	}, func(err error) {
		if err != nil {
			a.logf("刷新兑换流水失败: %v", err)
		}
	})
}

// onCdkShowRedemptions 把 CDK 列表选中码填入流水筛选并加载。
func (a *App) onCdkShowRedemptions() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	code := selectedCdkCode()
	if code == "" {
		walk.MsgBox(a.mw, "查看兑换流水", "请先在 CDK 列表中选中一行", walk.MsgBoxIconWarning)
		return
	}
	cdkWidgets.redeemFilter.SetText(code)
	a.runAsync("查看兑换流水", func() error {
		return a.loadCdkRedemptions(code)
	}, func(err error) {
		if err != nil {
			a.logf("查看兑换流水失败: %v", err)
		}
	})
}

// ============ 补发失败邮件 ============

// onCdkRetryMails 对当前筛选码下 status=failed 的流水逐个重发系统邮件并回写状态。
func (a *App) onCdkRetryMails() {
	st := a.cdkValidStore()
	if st == nil {
		return
	}
	code := strings.TrimSpace(cdkWidgets.redeemFilter.Text())
	if code == "" {
		walk.MsgBox(a.mw, "补发失败邮件", "请先在「CDK 列表」选中目标码并点击「查看兑换流水」，或在上方筛选框填写兑换码", walk.MsgBoxIconWarning)
		return
	}
	a.runAsync("补发失败邮件", func() error {
		ctx := context.Background()

		// 定位该码主表信息（名称 + 奖励 JSON）。
		norm, err := store.NormalizeRedeemCode(code)
		if err != nil {
			return fmt.Errorf("兑换码格式非法: %v", err)
		}
		codes, err := st.ListRedeemCodes(ctx, norm, 1, 50)
		if err != nil {
			return err
		}
		var target *store.RedeemCode
		for i := range codes {
			if codes[i].RedeemCode == norm {
				target = &codes[i]
				break
			}
		}
		if target == nil {
			return fmt.Errorf("未找到兑换码 %s", norm)
		}
		attachments := parseCdkRewardJSON(target.RewardJSON)
		ceraTotal := cdkRewardCeraTotal(target.RewardJSON)
		bdDays := cdkRewardBlackDiamondDays(target.RewardJSON)
		if len(attachments) == 0 {
			switch {
			case ceraTotal > 0 && bdDays > 0:
				return fmt.Errorf("该 CDK 的奖励只有点券（+%d）与黑钻（%d 天），没有可补发的物品邮件；点券入账/黑钻激活都不能自动补发（会重复发放），请到「账号管理」页按角色手动补", ceraTotal, bdDays)
			case bdDays > 0:
				return fmt.Errorf("该 CDK 的奖励只有黑钻（%d 天），没有可补发的物品邮件；黑钻激活不能自动补发（会重复发放），请到「账号管理」页「成长与解锁」按角色手动补", bdDays)
			case ceraTotal > 0:
				return fmt.Errorf("该 CDK 的奖励只有点券（+%d），没有可补发的物品邮件；点券入账不能自动补发（会重复入账），请到「账号管理」页按账号补发点券", ceraTotal)
			}
			return fmt.Errorf("该 CDK 没有可补发的奖励物品")
		}

		reds, err := st.ListRedeemRedemptions(ctx, norm, 1000)
		if err != nil {
			return err
		}
		sent, failed := 0, 0
		for i := range reds {
			r := &reds[i]
			if !redeemRetryTargetsRow(r.MailStatus, r.MailError) {
				continue
			}
			if strings.TrimSpace(r.CharacterID) == "" {
				a.logf("补发跳过(无角色ID) redemption_id=%d code=%s", r.ID, norm)
				continue
			}
			mailErr := a.cdkGmSendMail(ctx, cdkGMMailRequest{
				CharacterID: r.CharacterID,
				Title:       "CDK兑换奖励",
				Body:        "兑换成功：" + target.Name,
				Attachments: attachments,
			})
			if mailErr != nil {
				failed++
				a.logf("补发失败 code=%s username=%s: %v", norm, r.Username, mailErr)
				_ = st.SetRedemptionMailResult(ctx, r.ID, "failed", truncateStr(mailErr.Error()))
				continue
			}
			sent++
			_ = st.SetRedemptionMailResult(ctx, r.ID, "sent", "")
		}
		if sent+failed == 0 {
			a.logf("筛选码 %s 无待补发的失败流水", norm)
		} else {
			a.logf("补发邮件完成：成功 %d 条，失败 %d 条（码=%s）", sent, failed, norm)
		}
		if ceraTotal > 0 {
			a.logf("注意：该 CDK 还带点券 +%d/张。点券入账不经邮件，若那次兑换时点券也没到账，请到「账号管理」页按账号补发（不能在这里自动补，否则会重复入账）", ceraTotal)
		}
		if bdDays > 0 {
			a.logf("注意：该 CDK 还带黑钻 %d 天/张。黑钻激活不经邮件，若那次兑换时黑钻也没到账，请到「账号管理」页「成长与解锁」按角色重新开启黑钻（不能在这里自动补，否则会重复发放）", bdDays)
		}
		return a.loadCdkRedemptions(norm)
	}, func(err error) {
		if err != nil {
			walk.MsgBox(a.mw, "补发失败邮件", err.Error(), walk.MsgBoxIconError)
		}
	})
}

// parseCdkRewardJSON 解析 reward_json 文本 → GM 邮件附件；空/非法返回 nil。
//
// 点券行与黑钻行**都不进附件**：点券是账号余额（wallet/update 入账）、黑钻是账号级权益
// （character/black-diamond 激活），都没有对应的邮件附件；老代码会把这类行当成
// {item_id:0,count:0} 的附件发出去，必须跳过。
func parseCdkRewardJSON(rewardJSON string) []cdkGMMailAttachment {
	rewardJSON = strings.TrimSpace(rewardJSON)
	if rewardJSON == "" || rewardJSON == "null" {
		return nil
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil {
		return nil
	}
	out := make([]cdkGMMailAttachment, 0, len(rewards))
	for _, rw := range rewards {
		if rw.ItemID == 0 || rw.Count == 0 {
			continue
		}
		out = append(out, cdkGMMailAttachment{ItemID: rw.ItemID, Count: rw.Count, Bind: rw.Bind})
	}
	return out
}

// cdkRewardCeraTotal 汇总 reward_json 里的点券总额（补发邮件时用来提示"点券部分不能自动补"）。
func cdkRewardCeraTotal(rewardJSON string) int64 {
	rewardJSON = strings.TrimSpace(rewardJSON)
	if rewardJSON == "" || rewardJSON == "null" {
		return 0
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil {
		return 0
	}
	var total int64
	for _, rw := range rewards {
		if rw.Cera > 0 {
			total += rw.Cera
		}
	}
	return total
}

// cdkRewardBlackDiamondDays 汇总 reward_json 里的黑钻总天数（补发邮件时用来提示"黑钻部分不能自动补"）。
func cdkRewardBlackDiamondDays(rewardJSON string) int64 {
	rewardJSON = strings.TrimSpace(rewardJSON)
	if rewardJSON == "" || rewardJSON == "null" {
		return 0
	}
	var rewards []cdkRewardJSON
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil {
		return 0
	}
	var total int64
	for _, rw := range rewards {
		if rw.BlackDiamondDays > 0 {
			total += rw.BlackDiamondDays
		}
	}
	return total
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

// redeemRetryTargetsRow 判断一条兑换流水是否在"补发失败邮件"的射程内。
//
// 状态语义（见 internal/server/redeem.go）：
//   - failed  ：所有奖励部分**都没发出去** → 重发物品邮件是安全的；
//   - partial ：只成功了一部分。mail_error 里记着失败的是哪几部分：
//     含 "物品邮件" → 可以补发物品；只有 "点券"/"黑钻" → **不能**自动补，
//     点券是余额入账、黑钻是账号权益激活，重发就是重复发放，只能人工核对该角色后手动补；
//   - sent / pending：不补。
func redeemRetryTargetsRow(status, mailError string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return true
	case "partial":
		text := strings.TrimSpace(mailError)
		if text == "" {
			// 老流水（点券/黑钻上线之前）可能没有错误明细，按旧行为仍允许补发物品。
			return true
		}
		// 错误明细由服务端按"物品邮件 / 点券 / 黑钻"顺序拼写，物品部分在首位，
		// 截断（255 字节）也不会把"物品邮件"这四个字切掉。
		return strings.Contains(text, "物品邮件")
	default:
		return false
	}
}

// cdkGmAdminConfig 由服务端目录探测 instance.json，返回 http://adminListen 与 adminToken。
func (a *App) cdkGmAdminConfig() (baseURL, token string, err error) {
	root := strings.TrimSpace(a.rootEdit.Text())
	if root == "" {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	ins := serverdir.Inspect(root)
	if ins == nil || ins.Instance == nil || strings.TrimSpace(ins.Instance.AdminListen) == "" {
		return "", "", fmt.Errorf("instance.json 未配置 adminListen，无法连接服务端管理口")
	}
	token = strings.TrimSpace(ins.Instance.AdminToken)
	if token == "" {
		return "", "", fmt.Errorf("instance.json 未配置 adminToken，请先在服务端初始化")
	}
	return "http://" + ins.Instance.AdminListen, token, nil
}

// cdkGmSendMail 回环 POST /local/dnf-gm/mail（协议与 server 包 GM 调用一致：X-Admin-Token/X-Admin-Actor）。
func (a *App) cdkGmSendMail(ctx context.Context, payload cdkGMMailRequest) error {
	baseURL, token, err := a.cdkGmAdminConfig()
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/local/dnf-gm/mail", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("X-Admin-Actor", "ops")
	gmhttp.SetConfirm(req, "ops")

	client := &http.Client{Timeout: cdkGMTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GM 邮件请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GM 邮件接口 HTTP %d", resp.StatusCode)
	}
	var env cdkGMEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("GM 邮件响应解析失败: %w", err)
	}
	if !env.OK {
		msg := env.Error
		if msg == "" {
			msg = env.Message
		}
		if msg == "" {
			msg = "GM 管理口拒绝请求"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func truncateStr(s string) string {
	if len(s) > 255 {
		return s[:255]
	}
	return s
}
