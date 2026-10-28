package main

// 封禁页：封禁列表、执行封禁（account/ip/machine/triple）、解封、踢下线。
// 端点：GET/POST/DELETE /manage/v1/bans、POST /manage/v1/bans/kick（契约见服务器 handlers_business.go）。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	decl "github.com/lxn/walk/declarative"
)

const (
	banTypeAccount = "account"
	banTypeIP      = "ip"
	banTypeMachine = "machine"
	banTypeTriple  = "triple"
)

var banTypeOptions = []string{
	"account — 账号（直封精确账号ID；或填用户名自动解析）",
	"ip — IP",
	"machine — 机器码",
	"triple — 一键三封（账号+最近IP+机器码，需填用户名）",
}

// banValueIsAccountID 判断某类封禁的"封禁值"是不是账号主键。
// account / triple 两类的 BanValue 存的就是 account_id（dnf:<sha256>），显示要给运营换成真名；
// ip / machine 维度的值是地址与机器码，原样显示。
func banValueIsAccountID(banType string) bool {
	t := strings.ToLower(strings.TrimSpace(banType))
	return t == banTypeAccount || t == banTypeTriple
}

func banTypeValueFromIndex(idx int) string {
	switch idx {
	case 1:
		return banTypeIP
	case 2:
		return banTypeMachine
	case 3:
		return banTypeTriple
	default:
		return banTypeAccount
	}
}

func banTypeShortLabel(t string) string {
	switch t {
	case banTypeIP:
		return "IP"
	case banTypeMachine:
		return "机器码"
	case banTypeTriple:
		return "三封"
	default:
		return "账号"
	}
}

func buildBanTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "账号封禁",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.GroupBox{
				Title:  "封禁列表（最新 200 条）",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.PushButton{
								AssignTo: &a.banRefreshBtn,
								Text:     "刷新列表",
								OnClicked: func() {
									go a.refreshBanList()
								},
							},
							decl.Label{Text: "列表仅展示有效封禁（含永久/到期前），解封请复制 ban_value 后到下方「解封」区操作。"},
						},
					},
					decl.TextEdit{
						AssignTo: &a.banListText,
						ReadOnly: true,
						MinSize:  decl.Size{Width: 0, Height: 160},
						Font:     decl.Font{Family: "Consolas", PointSize: 9},
					},
				},
			},
			decl.GroupBox{
				Title:  "执行封禁",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "封禁类型："},
							decl.ComboBox{
								AssignTo:     &a.banTypeCb,
								Model:        banTypeOptions,
								CurrentIndex: 0,
							},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "封禁值："},
							decl.LineEdit{AssignTo: &a.banValueLE},
							decl.Label{Text: "用户名："},
							decl.LineEdit{AssignTo: &a.banUserLE, MinSize: decl.Size{Width: 180}},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "原因："},
							decl.LineEdit{AssignTo: &a.banReasonLE},
							decl.Label{Text: "时长(天,0=永久)："},
							decl.LineEdit{AssignTo: &a.banDaysLE, Text: "0", MaxSize: decl.Size{Width: 90}},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.PushButton{
								AssignTo: &a.banDoBtn,
								Text:     "执行封禁",
								OnClicked: func() {
									a.onBanCreate()
								},
							},
							decl.Label{Text: "封禁值用于精确维度直封；account/triple 填用户名时由服务器解析账号ID/最近IP/机器码。"},
						},
					},
				},
			},
			decl.GroupBox{
				Title:  "解封（DELETE /manage/v1/bans，按 类型+值）",
				Layout: decl.HBox{},
				Children: []decl.Widget{
					decl.Label{Text: "类型："},
					decl.ComboBox{
						AssignTo:     &a.ubTypeCb,
						Model:        []string{banTypeAccount, banTypeIP, banTypeMachine},
						CurrentIndex: 0,
					},
					decl.Label{Text: "值："},
					decl.LineEdit{AssignTo: &a.ubValueLE},
					decl.PushButton{
						AssignTo: &a.ubBtn,
						Text:     "解封",
						OnClicked: func() {
							a.onBanUnban()
						},
					},
				},
			},
			decl.GroupBox{
				Title:  "踢下线（POST /manage/v1/bans/kick）",
				Layout: decl.HBox{},
				Children: []decl.Widget{
					decl.Label{Text: "账号："},
					decl.LineEdit{AssignTo: &a.kickLE},
					decl.PushButton{
						AssignTo: &a.kickBtn,
						Text:     "踢下线",
						OnClicked: func() {
							a.onBanKick()
						},
					},
					decl.Label{Text: "按 account 维度踢出该账号全部在线会话。"},
				},
			},
			decl.GroupBox{
				Title:  "收益制裁（占位，待四端联动实装）",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Label{Text: "被制裁玩家无法获得金币/经验/道具，并收到红色提示公告；制裁到期自动解除。当前仅为 UI 占位，执行/解除尚未接服务端。"},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "账号/用户名："},
							decl.LineEdit{AssignTo: &a.sanctionAcctLE},
							decl.Label{Text: "时长(分钟)："},
							decl.LineEdit{AssignTo: &a.sanctionMinLE, Text: "60", MaxSize: decl.Size{Width: 90}},
							decl.PushButton{
								AssignTo:  &a.sanctionBtn,
								Text:      "执行制裁",
								OnClicked: func() { a.onSanctionApply() },
							},
							decl.PushButton{
								AssignTo:  &a.sanctionLiftBtn,
								Text:      "解除制裁",
								OnClicked: func() { a.onSanctionLift() },
							},
						},
					},
				},
			},
		},
	}
}

// refreshBanList 拉取封禁列表并渲染。
func (a *App) refreshBanList() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	items, err := c.BanList(ctx)
	if err != nil {
		if !a.handleAPIError(err) {
			a.showError("封禁列表拉取失败", err)
		}
		return
	}
	if items == nil {
		items = []BanOut{}
	}
	// account/triple 型封禁的"值"就是账号主键（dnf:<sha256>，不可逆）：先批量翻成真实账号名
	// 再渲染（翻译失败只是回退显示主键，不影响封禁列表本身）。
	ids := make([]string, 0, len(items))
	for _, it := range items {
		if banValueIsAccountID(it.BanType) {
			ids = append(ids, it.BanValue)
		}
	}
	a.resolveAccountNames(acctNameList(ids...))

	var b strings.Builder
	if len(items) == 0 {
		b.WriteString("（无有效封禁记录）")
	} else {
		for _, it := range items {
			banValue := it.BanValue
			if banValueIsAccountID(it.BanType) {
				banValue = acctNameText(it.BanValue)
			}
			fmt.Fprintf(&b, "[%d] %s | 值: %s | 用户: %s | 原因: %s | 操作人: %s\n",
				it.ID, banTypeShortLabel(it.BanType), banValue, it.Username, it.Reason, it.Operator)
			fmt.Fprintf(&b, "        至 %s | 状态: %s | 封禁时间: %s\n", orDash(it.ExpiresAt), it.State, orDash(it.BannedAt))
		}
	}
	a.sync(func() {
		setEditText(a.banListText, b.String())
	})
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func (a *App) onBanCreate() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	bt := banTypeValueFromIndex(a.banTypeCb.CurrentIndex())
	value := strings.TrimSpace(leText(a.banValueLE))
	username := strings.TrimSpace(leText(a.banUserLE))
	reason := strings.TrimSpace(leText(a.banReasonLE))
	days := 0
	if s := strings.TrimSpace(leText(a.banDaysLE)); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			days = v
		} else {
			a.showInfo("输入错误", "时长(天)必须是整数")
			return
		}
	}
	if value == "" && username == "" {
		a.showInfo("输入错误", "请填写封禁值或用户名（account/triple 可两者填一，其余需封禁值）")
		return
	}

	msg := fmt.Sprintf("确认执行以下封禁？\n类型：%s\n值：%s\n用户名：%s\n原因：%s\n时长：%d 天（0=永久）",
		banTypeShortLabel(bt), orDash(value), orDash(username), orDash(reason), days)
	if !a.confirm("封禁确认", msg) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	a.sync(func() { a.banDoBtn.SetEnabled(false) })
	defer func() { a.sync(func() { a.banDoBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := c.BanCreate(ctx, bt, value, username, reason, days)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("封禁失败", err)
			}
			return
		}
		summary := fmt.Sprintf("封禁成功：类型 %s，目标 %s，踢下线会话 %d 个", res.Type, res.Value, res.Kicked)
		if len(res.Warnings) > 0 {
			summary += "\n提示：\n- " + strings.Join(res.Warnings, "\n- ")
		}
		a.showInfo("封禁完成", summary)
		a.refreshBanList()
	}()
}

func (a *App) onBanUnban() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	var bt string
	switch a.ubTypeCb.CurrentIndex() {
	case 1:
		bt = banTypeIP
	case 2:
		bt = banTypeMachine
	default:
		bt = banTypeAccount
	}
	value := strings.TrimSpace(leText(a.ubValueLE))
	if value == "" {
		a.showInfo("输入错误", "请填写要解封的值（可从列表复制）")
		return
	}
	if !a.confirm("解封确认", fmt.Sprintf("确认解除以下封禁？\n类型：%s\n值：%s", banTypeShortLabel(bt), value)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	a.sync(func() { a.ubBtn.SetEnabled(false) })
	defer func() { a.sync(func() { a.ubBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := c.BanDelete(ctx, bt, value)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("解封失败", err)
			}
			return
		}
		a.showInfo("解封完成", "该封禁记录已解除。")
		a.refreshBanList()
	}()
}

func (a *App) onBanKick() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	acct := strings.TrimSpace(leText(a.kickLE))
	if acct == "" {
		a.showInfo("输入错误", "请填写要踢下线的账号")
		return
	}
	if !a.confirm("踢下线确认", fmt.Sprintf("确认将账号「%s」的所有在线会话踢下线？", acct)) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()

	a.sync(func() { a.kickBtn.SetEnabled(false) })
	defer func() { a.sync(func() { a.kickBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		kicked, err := c.BanKick(ctx, banTypeAccount, acct)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("踢下线失败", err)
			}
			return
		}
		a.showInfo("踢下线完成", fmt.Sprintf("已踢出在线会话 %d 个。", kicked))
	}()
}

// onSanctionApply 收益制裁执行（占位：需服务端/登录器/运维工具/管理工具四端联动后实装）。
func (a *App) onSanctionApply() {
	a.showInfo("尚未实装", "收益制裁需要服务端、登录器、运维工具、管理工具四端联动，当前仅为 UI 占位。")
}

// onSanctionLift 收益制裁解除（占位）。
func (a *App) onSanctionLift() {
	a.showInfo("尚未实装", "收益制裁解除同样依赖四端联动，当前仅为 UI 占位。")
}
