package main

// DNF90 账号管理 —— 「邮件管理」子页（管理端）。
//
// 需求来源：在「账号管理」原有能力上补一个删除邮件的入口。
//
// 两条删除通道（执行端行为不同，UI 必须让操作者看清区别）：
//
//	① 定向删除选中邮件 —— /manage/v1/account/mailbox/delete
//	   → 服务端 GMDeleteMailboxMails。可只删某几封；但要求角色离线（在线返回 409），
//	   且未勾选「强制」时，只要邮件里还有未领取的金币/附件就会被拒绝。
//
//	② 清空整个邮箱 —— /manage/v1/account/mailbox/clear
//	   → 服务端 GMClearMailbox。全清不可挑；但角色在线也能用，且会向在线客户端推送
//	   "未读提示 + 完整邮箱列表"，玩家那边立刻看到邮箱空了。
//
// 因此在线角色下"定向删除"按钮提前禁用并给出原因，引导操作者走清空通道。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// gmtMaxMailIDs 与执行端 GMDeleteMailboxMails 的硬上限一致。
const gmtMaxMailIDs = 100

// acctMailboxModel 邮箱列表模型。
// 列：标题 | 发件人 | 金币 | 附件 | 状态 | 领取 | 创建时间 | 到期时间 | mail_id
type acctMailboxModel struct {
	walk.TableModelBase
	items []GMTMail
}

func (m *acctMailboxModel) RowCount() int { return len(m.items) }

func (m *acctMailboxModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	mail := m.items[row]
	switch col {
	case 0:
		if strings.TrimSpace(mail.Title) == "" {
			return "(无标题)"
		}
		return mail.Title
	case 1:
		if strings.TrimSpace(mail.SenderName) == "" {
			return "(系统)"
		}
		return mail.SenderName
	case 2:
		return mail.Gold
	case 3:
		return acctMailAttachmentBrief(mail)
	case 4:
		return invBoolText(mail.Read, "已读", "未读")
	case 5:
		return invBoolText(mail.Claimed, "已领取", "未领取")
	case 6:
		return acctMailTimeText(mail.CreatedAt)
	case 7:
		return acctMailTimeText(mail.ExpireAt)
	case 8:
		return mail.MailID
	}
	return nil
}

// acctMailAttachmentBrief 附件摘要：无附件返回 "-"，有则列前两件并标总数。
func acctMailAttachmentBrief(mail GMTMail) string {
	if len(mail.Attachments) == 0 {
		return "-"
	}
	parts := make([]string, 0, 2)
	for i, att := range mail.Attachments {
		if i >= 2 {
			break
		}
		parts = append(parts, fmt.Sprintf("%d x%d", att.ItemID, att.Count))
	}
	brief := strings.Join(parts, ", ")
	if len(mail.Attachments) > 2 {
		brief += fmt.Sprintf(" 等 %d 项", len(mail.Attachments))
	}
	return brief
}

// acctMailTimeText 时间列显示：零值显示 "-"。
func acctMailTimeText(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04")
}

// acctMailboxTab 邮件管理子页控件树。
func (a *App) acctMailboxTab() []decl.Widget {
	return []decl.Widget{
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "收件角色:"},
				decl.Label{AssignTo: &acctW.mbRoleL, Text: "（未选中）", MinSize: decl.Size{Width: 200, Height: 0}},
				decl.PushButton{AssignTo: &acctW.mbRefreshBtn, Text: "读取邮箱", OnClicked: a.onAcctMailboxLoad, ToolTipText: "拉取选中角色的完整邮箱（含正文与附件摘要）"},
				decl.CheckBox{AssignTo: &acctW.mbForceChk, Text: "强制（连未领取的金币/附件一起删）", ToolTipText: "不勾选时，只要邮件里还有未领取的资产，执行端会拒绝删除该邮件"},
			},
		},
		decl.TableView{
			AssignTo:       &acctW.mbTable,
			MultiSelection: true,
			Columns: []decl.TableViewColumn{
				{Title: "标题", Width: 180},
				{Title: "发件人", Width: 90},
				{Title: "金币", Width: 80},
				{Title: "附件", Width: 160},
				{Title: "状态", Width: 70},
				{Title: "领取", Width: 70},
				{Title: "创建时间", Width: 120},
				{Title: "到期时间", Width: 120},
				{Title: "mail_id", Width: 120},
			},
			Model:                 new(acctMailboxModel),
			MinSize:               decl.Size{Width: 860, Height: 150},
			OnCurrentIndexChanged: a.onAcctMailboxSelected,
		},
		decl.Label{AssignTo: &acctW.mbDetailL, Text: "未选择邮件", MinSize: decl.Size{Width: 860, Height: 0}},
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.PushButton{AssignTo: &acctW.mbDelBtn, Text: "删除选中邮件", OnClicked: a.onAcctMailboxDelete, ToolTipText: "定向删除选中的邮件（单次最多 100 封）"},
				decl.PushButton{AssignTo: &acctW.mbClearBtn, Text: "清空整个邮箱", OnClicked: a.onAcctMailboxClear, ToolTipText: "全清，不可撤销；角色在线也能用，且会实时刷新玩家客户端邮箱"},
			},
		},
		// 批量删除（全服 / 在线）：服务端后台任务，逐角色走 GM 清空接口。
		// ⚠ 这是**物理删除**，未领取的金币与附件一起消失，所以要「预演 → 两道确认」。
		decl.Composite{
			Layout: decl.HBox{MarginsZero: true, Spacing: 6},
			Children: []decl.Widget{
				decl.Label{Text: "批量删除:"},
				decl.CheckBox{AssignTo: &acctW.mbDryRunChk, Text: "预演（只统计，不删除）", ToolTipText: "先量一遍规模：只统计目标角色数与累计邮件数，一封都不删。建议每次正式执行前先预演"},
				decl.PushButton{AssignTo: &acctW.mbClearAllBtn, Text: "删除全服邮件", OnClicked: func() { a.onAcctMailboxClearBatch(MailboxScopeAllCharacters) }, ToolTipText: "删除全服所有角色的邮件（物理删除、不可恢复、未领取资产一并消失）"},
				decl.PushButton{AssignTo: &acctW.mbClearOnlineBtn, Text: "删除在线角色邮件", OnClicked: func() { a.onAcctMailboxClearBatch(MailboxScopeOnlineCharacters) }, ToolTipText: "只清当前在线角色的邮箱（范围小、可控，适合日常清理）"},
				decl.PushButton{AssignTo: &acctW.mbBatchCancelBtn, Text: "取消批量", Enabled: false, OnClicked: a.onAcctMailboxBatchCancel},
				decl.Label{AssignTo: &acctW.mbBatchStatusL, Text: "批量：未执行", MinSize: decl.Size{Width: 220, Height: 0}},
			},
		},
		decl.Label{
			// 这一段必须短：本页控件行数变多（新增批量删除那一行）会顶高子页高度，
			// 而子页所在 TabWidget 的高度是有限的 —— 说明文字长了会把整个主窗口撑高（踩过）。
			// 细节都放在按钮的 ToolTipText 里。
			Text: " 说明：定向删除要求角色离线；清空（单个 / 批量）都走 /local/dnf-gm/mail/clear，在线会推送刷新，玩家立刻看到邮箱空了。\r\n" +
				"       批量是**物理删除**且不可恢复；批量任务只在服务端内存，取消/重启不会回滚 —— 中断即半清空，需重跑补齐。",
			TextColor: walk.RGB(90, 90, 90),
		},
	}
}

// acctMailboxResetForRole 切换/清空选中角色时重置邮箱列表与按钮态。
// 必须带 a：重置后要按新角色的在线状态重判"定向删除"按钮的可用性，
// 否则选中一个在线角色后按钮仍停留在一个离线角色的可用态（点了才被服务端拒）。
func (a *App) acctMailboxResetForRole() {
	acctMboxMdl.items = nil
	if acctW.mbTable != nil {
		acctW.mbTable.SetModel(&acctMboxMdl)
	}
	if acctW.mbDetailL != nil {
		acctW.mbDetailL.SetText("未选择邮件")
	}
	if acctW.mbForceChk != nil {
		acctW.mbForceChk.SetChecked(false)
	}
	if acctW.mbRoleL != nil {
		if acctW.selRole == nil {
			acctW.mbRoleL.SetText("（未选中）")
		} else {
			acctW.mbRoleL.SetText(fmt.Sprintf("%s（账号 %s）", acctW.selRole.Name, acctNameText(acctW.selRole.AccountID)))
		}
	}
	a.acctMailboxApplyOnlineUI()
}

// acctMailboxApplyOnlineUI 按角色在线状态启用/禁用定向删除（清空按钮始终可用）。
func (a *App) acctMailboxApplyOnlineUI() {
	if acctW.mbDelBtn == nil {
		return
	}
	online := acctW.selRole != nil && acctW.selRole.Online
	acctW.mbDelBtn.SetEnabled(!online)
	if online {
		acctW.mbDelBtn.SetToolTipText("角色在线：执行端拒绝定向删除，请用「清空整个邮箱」（支持在线并实时刷新客户端）或让角色下线")
	} else {
		acctW.mbDelBtn.SetToolTipText("定向删除选中的邮件（单次最多 100 封）")
	}
}

// onAcctMailboxLoad 读取选中角色邮箱。
func (a *App) onAcctMailboxLoad() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	c := a.snapshotClient()
	if c == nil {
		a.showInfo("未连接", "请先登录")
		return
	}
	characterID, name := role.CharacterID, role.Name
	var snapshot GMTMailboxSnapshot
	a.acctRun("读取邮箱", func() error {
		out, err := c.MailboxQuery(context.Background(), characterID)
		if err != nil {
			return err
		}
		snapshot = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		acctMboxMdl.items = snapshot.Mails
		acctW.mbTable.SetModel(&acctMboxMdl)
		acctW.mbDetailL.SetText("未选择邮件")
		if acctW.selRole != nil {
			acctW.selRole.Online = snapshot.Character.Online
			a.acctSummarize()
		}
		a.acctMailboxApplyOnlineUI()
		acctLog("角色 %s 邮箱共 %d 封（%s）", name, len(snapshot.Mails), acctOnlineText(snapshot.Character.Online))
	})
}

// onAcctMailboxSelected 邮件行选中。
func (a *App) onAcctMailboxSelected() {
	if acctW.mbTable == nil {
		return
	}
	index := acctW.mbTable.CurrentIndex()
	if index < 0 || index >= len(acctMboxMdl.items) {
		acctW.mbDetailL.SetText("未选择邮件")
		return
	}
	mail := acctMboxMdl.items[index]
	detail := fmt.Sprintf("已选择：%s（mail_id=%s，发件人 %s，金币 %d，附件 %d 件，%s，%s）",
		mail.Title, mail.MailID, mail.SenderName, mail.Gold, len(mail.Attachments),
		invBoolText(mail.Read, "已读", "未读"), invBoolText(mail.Claimed, "已领取", "未领取"))
	if body := strings.TrimSpace(mail.Body); body != "" {
		runes := []rune(body)
		if len(runes) > 60 {
			body = string(runes[:60]) + "…"
		}
		detail += " ｜正文：" + body
	}
	acctW.mbDetailL.SetText(detail)
}

// acctMailboxSelectedMails 表格中选中的邮件（多选；单选控件回退当前行）。
func acctMailboxSelectedMails() []GMTMail {
	if acctW.mbTable == nil {
		return nil
	}
	indexes := acctW.mbTable.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := acctW.mbTable.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	mails := make([]GMTMail, 0, len(indexes))
	seen := make(map[string]struct{}, len(indexes))
	for _, idx := range indexes {
		if idx < 0 || idx >= len(acctMboxMdl.items) {
			continue
		}
		mail := acctMboxMdl.items[idx]
		if _, dup := seen[mail.MailID]; dup {
			continue
		}
		seen[mail.MailID] = struct{}{}
		mails = append(mails, mail)
	}
	return mails
}

// onAcctMailboxDelete 定向删除选中邮件（要求角色离线）。
func (a *App) onAcctMailboxDelete() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	mails := acctMailboxSelectedMails()
	if len(mails) == 0 {
		acctLog("请先在邮箱列表里选中要删除的邮件")
		return
	}
	if role.Online {
		acctLog("角色 %s 当前在线：执行端拒绝定向删除邮件。请改用「清空整个邮箱」（支持在线并实时刷新客户端），或让角色下线后重试。", role.Name)
		return
	}
	force := acctW.mbForceChk.Checked()
	ids := make([]string, 0, len(mails))
	for _, mail := range mails {
		ids = append(ids, mail.MailID)
	}
	if len(ids) > gmtMaxMailIDs {
		acctLog("单次最多删除 %d 封（当前选中 %d 封），请分批操作", gmtMaxMailIDs, len(ids))
		return
	}
	warning := ""
	if force {
		warning = "\n\n注意：已勾选「强制」，未领取的金币与附件会一并销毁且无法找回。"
	}
	if !a.confirm("删除邮件确认", fmt.Sprintf("将从角色 %s 的邮箱删除 %d 封邮件，操作不可撤销。%s\n\n确认继续？", role.Name, len(ids), warning)) {
		acctLog("已取消删除邮件")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		return
	}
	characterID, name := role.CharacterID, role.Name
	var result GMTMailDeleteResult
	a.acctRun("删除邮件", func() error {
		out, err := c.MailboxDelete(context.Background(), characterID, ids, force)
		if err != nil {
			return err
		}
		result = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		acctMboxMdl.items = result.Mailbox.Mails
		acctW.mbTable.SetModel(&acctMboxMdl)
		acctW.mbDetailL.SetText("未选择邮件")
		acctLog("已删除角色 %s 的 %d 封邮件，剩余 %d 封", name, result.Deleted, len(result.Mailbox.Mails))
	})
}

// onAcctMailboxClear 清空整个邮箱（在线可用，且会推送客户端刷新）。
func (a *App) onAcctMailboxClear() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	if !a.confirm("清空邮箱确认", fmt.Sprintf(
		"将清空角色 %s 的整个邮箱（含未领取的金币与附件），操作不可撤销且无法恢复。\n\n确认继续？", role.Name)) {
		acctLog("已取消清空邮箱")
		return
	}
	c := a.snapshotClient()
	if c == nil {
		return
	}
	characterID, name := role.CharacterID, role.Name
	var result GMTMailboxClearResult
	a.acctRun("清空邮箱", func() error {
		out, err := c.MailboxClear(context.Background(), characterID, name)
		if err != nil {
			return err
		}
		result = *out
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		acctMboxMdl.items = nil
		acctW.mbTable.SetModel(&acctMboxMdl)
		acctW.mbDetailL.SetText("未选择邮件")
		state := "角色离线，上线后即为空邮箱"
		if result.Online {
			if result.Refreshed {
				state = "角色在线，客户端邮箱已实时刷新"
			} else {
				state = "角色在线，但实时刷新未完成，下次打开邮箱时同步"
			}
		}
		acctLog("已清空角色 %s 的邮箱：删除 %d 封（未读 %d，可领取 %d，金币 %d，附件 %d 件）；%s",
			result.CharacterName, result.RemovedMails, result.RemovedUnread, result.RemovedClaimable,
			result.RemovedGold, result.RemovedAttachments, state)
		if result.RemovedMails == 0 {
			acctLog("提示：执行端报告删除 0 封，可能是该角色当前根本没有邮件。")
		}
		if result.RefreshError != "" {
			acctLog("在线刷新返回的告警：%s", result.RefreshError)
		}
	})
}

// acctMboxMdl 邮箱列表模型实例。
var acctMboxMdl acctMailboxModel
