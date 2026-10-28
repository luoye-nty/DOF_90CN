package main

// DNF90 账号管理 —— 「邮件管理」子页（免费版/赞助版共用，无 build tag）。
//
// 需求来源：在「账号管理」原有能力上补一个删除邮件的入口。
//
// 两条删除通道（服务端行为不同，UI 必须让操作者看得见区别）：
//
//	① 定向删除选中邮件 —— /dnf/gm/characters/{id}/mailbox/delete（GMDeleteMailboxMails）
//	   优点：可以只删某几封；缺点：服务端要求角色离线（在线返回 409），
//	   且 force=false 时只要邮件里还有未领取的金币/附件就会被拒绝，必须显式勾选"强制"。
//
//	② 清空整个邮箱 —— /local/dnf-gm/mail/clear（GMClearMailbox）
//	   优点：角色在线也能用，并且会向在线客户端推送"未读提示 + 完整邮箱列表"，
//	         玩家那边立刻看到邮箱空了；缺点：全清，不能挑。
//
// 所以本页把两个按钮并排给出，并在角色在线时把"定向删除"的按钮禁用、写明原因，
// 让操作者直接走清空通道，而不是点了才报错。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

var acctMailboxBindLabels = []string{"全部", "绑定", "不绑定"} // 预留给后续筛选，当前仅展示

// acctMailboxModel 邮箱列表模型。
// 列：标题 | 发件人 | 金币 | 附件 | 已读 | 已领取 | 创建时间 | 到期时间 | mail_id
type acctMailboxModel struct {
	walk.TableModelBase
	items []acctGMTMail
}

func (m *acctMailboxModel) RowCount() int { return len(m.items) }

func (m *acctMailboxModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	mail := m.items[row]
	switch col {
	case 0:
		title := mail.Title
		if title == "" {
			title = "(无标题)"
		}
		return title
	case 1:
		if mail.SenderName == "" {
			return "(系统)"
		}
		return mail.SenderName
	case 2:
		return mail.Gold
	case 3:
		return acctMailAttachmentBrief(mail)
	case 4:
		return acctBoolText(mail.Read, "已读", "未读")
	case 5:
		return acctBoolText(mail.Claimed, "已领取", "未领取")
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
func acctMailAttachmentBrief(mail acctGMTMail) string {
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

// acctBoolText 布尔转中文。
func acctBoolText(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

// acctMailTimeText 时间列显示：零值显示 "-"，否则本地时间到分钟。
func acctMailTimeText(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04")
}

// acctMailboxTab 邮件管理子页控件树。
func (a *App) acctMailboxTab(app *App) []Widget {
	return []Widget{
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				Label{Text: "收件角色:"},
				Label{AssignTo: &acctW.mbRoleL, Text: "（未选中）", MinSize: Size{Width: 200, Height: 0}},
				PushButton{AssignTo: &acctW.mbRefreshBtn, Text: "读取邮箱", OnClicked: a.onAcctMailboxLoad, ToolTipText: "拉取选中角色的完整邮箱（含正文与附件摘要）"},
				CheckBox{AssignTo: &acctW.mbForceChk, Text: "强制（连未领取的金币/附件一起删）", ToolTipText: "不勾选时，只要邮件里还有未领取的资产，服务端会拒绝删除该邮件"},
			},
		},
		TableView{
			AssignTo:       &acctW.mbTable,
			MultiSelection: true,
			Columns: []TableViewColumn{
				{Title: "标题", Width: 190},
				{Title: "发件人", Width: 90},
				{Title: "金币", Width: 80},
				{Title: "附件", Width: 170},
				{Title: "状态", Width: 70},
				{Title: "领取", Width: 70},
				{Title: "创建时间", Width: 120},
				{Title: "到期时间", Width: 120},
				{Title: "mail_id", Width: 120},
			},
			Model:                 new(acctMailboxModel),
			MinSize:               Size{Width: 860, Height: 150},
			OnCurrentIndexChanged: a.onAcctMailboxSelected,
		},
		Label{AssignTo: &acctW.mbDetailL, Text: "未选择邮件", MinSize: Size{Width: 860, Height: 0}},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 6},
			Children: []Widget{
				PushButton{AssignTo: &acctW.mbDelBtn, Text: "删除选中邮件", OnClicked: a.onAcctMailboxDelete, ToolTipText: "定向删除勾选/选中的邮件（单次最多 100 封）"},
				PushButton{AssignTo: &acctW.mbClearBtn, Text: "清空整个邮箱", OnClicked: a.onAcctMailboxClear, ToolTipText: "全清，不可撤销；角色在线也能用，且会实时刷新玩家客户端邮箱"},
			},
		},
		Label{
			Text: " 说明：定向删除要求角色离线（在线会话会缓存邮箱，服务端刻意拦住直改）；\r\n" +
				"       清空整个邮箱走 /local/dnf-gm/mail/clear，角色在线时会推送客户端刷新，玩家立刻看到邮箱空了。",
			TextColor: walk.RGB(90, 90, 90),
		},
	}
}

// acctMailboxResetForRole 切换/清空选中角色时重置邮箱列表与按钮态，
// 避免上一个角色的邮件行被误当成新角色的邮件删除。
func acctMailboxResetForRole() {
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
	if acctApp != nil {
		acctApp.acctMailboxApplyOnlineUI()
	}
}

// onAcctMailboxLoad 读取选中角色邮箱。
func (a *App) onAcctMailboxLoad() {
	role := a.acctSelectedRole()
	if role == nil {
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var snapshot acctGMTMailboxSnapshot
	a.acctRun("读取邮箱", func() error {
		var err error
		snapshot, err = a.acctGMTMailbox(context.Background(), characterID)
		return err
	}, func(err error) {
		if err != nil {
			return
		}
		acctMboxMdl.items = snapshot.Mails
		acctW.mbTable.SetModel(&acctMboxMdl)
		acctW.mbDetailL.SetText("未选择邮件")
		// 服务端返回的在线状态是权威值，回写选中行（邮箱删除按钮的可用性依赖它）。
		if acctW.selRole != nil {
			acctW.selRole.Online = snapshot.Character.Online
			a.acctSummarize()
		}
		a.acctMailboxApplyOnlineUI()
		acctLog("角色 %s 邮箱共 %d 封（%s）", name, len(snapshot.Mails), acctOnlineText(snapshot.Character.Online))
	})
}

// acctMailboxApplyOnlineUI 按角色在线状态启用/禁用定向删除。
// 清空按钮始终可用：GMClearMailbox 对在线角色支持并会推送刷新。
func (a *App) acctMailboxApplyOnlineUI() {
	if acctW.mbDelBtn == nil {
		return
	}
	online := acctW.selRole != nil && acctW.selRole.Online
	acctW.mbDelBtn.SetEnabled(!online && !a.busy.Load())
	if online {
		acctW.mbDelBtn.SetToolTipText("角色在线：服务端拒绝定向删除，请用「清空整个邮箱」（支持在线并实时刷新客户端）或让角色下线")
	} else {
		acctW.mbDelBtn.SetToolTipText("定向删除勾选/选中的邮件（单次最多 100 封）")
	}
}

// onAcctMailboxSelected 邮件行选中：更新明细行。
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
		acctBoolText(mail.Read, "已读", "未读"), acctBoolText(mail.Claimed, "已领取", "未领取"))
	if body := strings.TrimSpace(mail.Body); body != "" {
		if len([]rune(body)) > 60 {
			body = string([]rune(body)[:60]) + "…"
		}
		detail += " ｜正文：" + body
	}
	acctW.mbDetailL.SetText(detail)
}

// acctMailboxSelectedMails 返回表格中选中的邮件（多选；单选控件回退到当前行）。
func acctMailboxSelectedMails() []acctGMTMail {
	if acctW.mbTable == nil {
		return nil
	}
	indexes := acctW.mbTable.SelectedIndexes()
	if len(indexes) == 0 {
		if cur := acctW.mbTable.CurrentIndex(); cur >= 0 {
			indexes = []int{cur}
		}
	}
	mails := make([]acctGMTMail, 0, len(indexes))
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
		acctLog("角色 %s 当前在线：服务端拒绝定向删除邮件。请改用「清空整个邮箱」（支持在线并实时刷新客户端），或让角色下线后重试。", role.Name)
		return
	}
	force := acctW.mbForceChk.Checked()
	ids := make([]string, 0, len(mails))
	for _, mail := range mails {
		ids = append(ids, mail.MailID)
	}
	if len(ids) > acctGMTMaxMailIDs {
		acctLog("单次最多删除 %d 封（当前选中 %d 封），请分批操作", acctGMTMaxMailIDs, len(ids))
		return
	}
	warning := ""
	if force {
		warning = "\n\n注意：已勾选「强制」，未领取的金币与附件会一并销毁且无法找回。"
	}
	if walk.MsgBox(a.mw, "删除邮件确认",
		fmt.Sprintf("将从角色 %s 的邮箱删除 %d 封邮件，操作不可撤销。%s\n\n确认继续？", role.Name, len(ids), warning),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		acctLog("已取消删除邮件")
		return
	}

	characterID := role.CharacterID
	name := role.Name
	var result acctGMTMailDeleteResult
	a.acctRun("删除邮件", func() error {
		var err error
		result, err = a.acctGMTMailboxDelete(context.Background(), characterID, ids, force)
		return err
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
	if walk.MsgBox(a.mw, "清空邮箱确认",
		fmt.Sprintf("将清空角色 %s 的整个邮箱（含未领取的金币与附件），操作不可撤销且无法恢复。\n\n确认继续？", role.Name),
		walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		acctLog("已取消清空邮箱")
		return
	}
	characterID := role.CharacterID
	name := role.Name
	var result acctMailboxClearResult
	a.acctRun("清空邮箱", func() error {
		var err error
		result, _, err = a.acctMailboxClearAll(context.Background(), characterID, name)
		return err
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
		if result.RefreshError != "" {
			acctLog("在线刷新返回的告警：%s", result.RefreshError)
		}
	})
}
