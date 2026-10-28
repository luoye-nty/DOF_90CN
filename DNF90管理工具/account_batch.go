package main

// 「账号管理」页的两个后台批量任务（服务器端异步执行 + 本工具轮询进度）：
//
//	① 邮件管理 → 删除全服邮件（POST /manage/v1/account/mailbox/clear-batch）
//	② 资产邮件 → 在线角色 / 全服首个角色 批量发放（POST /manage/v1/account/wallet/mail-batch）
//
// 为什么和「群发物品邮件」一样做成后台任务：全服几千个角色要逐个走 GM 接口，
// 一次 HTTP 请求必然超时；服务端立刻返回 task_id，本工具每 1.2 秒查一次进度。
//
// 三个功能共用一套"发起 → 轮询 → 收尾"的骨架（batchJob），
// 差别只在按钮/状态行/日志前缀与收尾回调 —— 别在这里各写一份轮询循环。
//
// ⚠ 三条必须保持的语义（服务端与本文案是一致的，改文案前先看服务器端实现）：
//  1. **清空邮箱是物理删除**：邮件本体、未领取的金币与附件一起从库里消失，不可恢复；
//     所以界面上必须先"预演"（dry_run）看规模，再二次确认。
//  2. **取消/中断 = 半清空**：任务只在服务端内存里，取消或运维工具重启都不会回滚，
//     剩下没处理的角色要再跑一次补齐。
//  3. **点券批量不幂等**：cera 是"先入账再发通知邮件"，通知失败时钱已到账，
//     服务端单独计数且不重试 —— 看到"已入账但通知失败"时绝不能重跑整批。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
)

// acctAssetTargetLabels 资产邮件的发放目标（索引与 onAcctAssetSendBatch 的 switch 一一对应）。
var acctAssetTargetLabels = []string{
	"当前选中角色（单个）",
	"在线角色（每个在线角色各一份）",
	"全服首个角色（创建最早的那一个）",
}

// batchJob 一个后台批量任务在界面上的呈现方式。
type batchJob struct {
	prefix string // 状态行与日志前缀，例如 "删除全服邮件"
	// statusL 进度状态行；cancelBtn 取消按钮；startBtns 进行中要禁用的发起按钮。
	statusL   *walk.Label
	cancelBtn *walk.PushButton
	startBtns []*walk.PushButton
	// taskID/polling 指向该功能自己的状态字段（每个功能一份，避免互相覆盖）。
	taskID  *string
	polling *bool
	// onDone 任务结束后的收尾（可为 nil）。
	onDone func(*BatchTask)
}

// batchStart 记录任务 id、置忙并开始轮询（须在 GUI 线程调用）。
func (a *App) batchStart(job batchJob, task *BatchTask) {
	if task == nil {
		return
	}
	if job.taskID != nil {
		*job.taskID = task.ID
	}
	a.batchSetBusy(job, true)
	a.batchApply(job, task)
	acctLog("%s：已开始，任务 %s", job.prefix, task.ID)
	go a.batchPoll(job, task.ID)
}

// batchPoll 轮询进度直到任务结束（上层是后台 goroutine）。
func (a *App) batchPoll(job batchJob, id string) {
	if job.polling != nil {
		if *job.polling {
			return // 已经在轮询了（防重入）
		}
		*job.polling = true
		defer func() { *job.polling = false }()
	}
	for i := 0; i < 3600; i++ { // 上限约 72 分钟：正常任务远小于它，只为防异常情况下无限轮询
		time.Sleep(1200 * time.Millisecond)
		cl := a.snapshotClient()
		if cl == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		task, err := cl.TaskGet(ctx, id)
		cancel()
		if err != nil {
			// 会话失效由 handleAPIError 那条路处理；这里只把界面恢复成"可再次发起"。
			a.sync(func() {
				a.batchClearRunning(job)
				if job.statusL != nil {
					job.statusL.SetText(job.prefix + "：进度查询失败（任务可能仍在服务端执行）")
				}
			})
			return
		}
		a.sync(func() { a.batchApply(job, task) })
		if task == nil || !task.Running() {
			a.sync(func() {
				a.batchClearRunning(job)
				acctLog("%s结束：%s", job.prefix, batchTaskSummary(task))
				if task != nil && len(task.Errors) > 0 {
					acctLog("%s失败样例：%s", job.prefix, strings.Join(task.Errors, " ｜ "))
				}
				if job.onDone != nil {
					job.onDone(task)
				}
			})
			return
		}
	}
}

// batchClearRunning 清掉"进行中"标记并把按钮恢复成可发起。
func (a *App) batchClearRunning(job batchJob) {
	if job.taskID != nil {
		*job.taskID = ""
	}
	a.batchSetBusy(job, false)
}

// batchSetBusy 置忙/恢复：进行中禁用发起按钮、启用取消按钮。
func (a *App) batchSetBusy(job batchJob, busy bool) {
	for _, btn := range job.startBtns {
		if btn != nil {
			btn.SetEnabled(!busy)
		}
	}
	if job.cancelBtn != nil {
		job.cancelBtn.SetEnabled(busy)
	}
}

// batchApply 把任务快照写进状态行。
func (a *App) batchApply(job batchJob, task *BatchTask) {
	if job.statusL == nil {
		return
	}
	job.statusL.SetText(job.prefix + "：" + batchTaskSummary(task))
	switch {
	case task == nil:
	case task.Running():
		job.statusL.SetTextColor(walk.RGB(0, 100, 180))
	case task.Failed > 0:
		job.statusL.SetTextColor(walk.RGB(180, 60, 0))
	default:
		job.statusL.SetTextColor(walk.RGB(0, 140, 0))
	}
}

// batchCancel 取消正在跑的任务（两个功能共用；已处理的目标不会回滚）。
func (a *App) batchCancel(job batchJob, confirmText string) {
	id := ""
	if job.taskID != nil {
		id = *job.taskID
	}
	if id == "" {
		a.showInfo("没有在跑的批量任务", "当前没有正在执行的"+job.prefix+"任务。")
		return
	}
	if !a.confirm("取消"+job.prefix, confirmText+"\n\n任务 "+id) {
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
			if job.statusL != nil {
				job.statusL.SetText(job.prefix + "：正在取消…")
			}
		})
	}()
}

// ---------- ② 资产邮件（发放给：当前选中角色 / 在线角色 / 全服首个角色） ----------

// acctAssetTargetIndex 当前选择的发放范围下标（控件未建时按 0 = 当前选中角色）。
func acctAssetTargetIndex() int {
	if acctW.assetTargetCb == nil {
		return 0
	}
	if idx := acctW.assetTargetCb.CurrentIndex(); idx > 0 {
		return idx
	}
	return 0
}

// onAcctAssetTargetChanged 切换「发放给」时更新按钮文案与预演勾选框的可用性。
//
// 为什么值得单独写：只有一个动作按钮时，"点下去会发生什么"必须一眼可见 ——
// 文案跟着范围走（单个 / 批量），预演对"当前选中角色"没有意义（它是同步单发），直接置灰。
func (a *App) onAcctAssetTargetChanged() {
	idx := acctAssetTargetIndex()
	if acctW.assetSendBtn != nil {
		switch idx {
		case 1:
			acctW.assetSendBtn.SetText("批量发送（在线角色）")
		case 2:
			acctW.assetSendBtn.SetText("批量发送（全服首个角色）")
		default:
			acctW.assetSendBtn.SetText("发送资产邮件")
		}
	}
	if acctW.assetDryRunChk != nil {
		acctW.assetDryRunChk.SetEnabled(idx != 0)
	}
}

// onAcctAssetSendDispatch 资产邮件唯一的动作入口：按「发放给」分发到单个 / 批量。
func (a *App) onAcctAssetSendDispatch() {
	if acctAssetTargetIndex() == 0 {
		a.onAcctAssetSend()
		return
	}
	a.onAcctAssetSendBatch()
}

// acctAssetBatchJob 资产邮件批量的任务呈现方式。
func (a *App) acctAssetBatchJob() batchJob {
	return batchJob{
		prefix:    "批量资产邮件",
		statusL:   acctW.assetBatchStatusL,
		cancelBtn: acctW.assetBatchCancelBtn,
		// 只有一个发起按钮：批量进行中就把它禁掉（另外「取消批量」由 batchSetBusy 打开）。
		startBtns: []*walk.PushButton{acctW.assetSendBtn},
		taskID:    &acctW.assetBatchTaskID,
		polling:   &acctW.assetBatchPolling,
		onDone: func(task *BatchTask) {
			// 批量发完不一定改了当前选中角色的余额，但玩家端会立刻看到；
			// 这里只在确实选中了角色时刷新一次余额，让运营确认"确实发出去了"。
			if acctW.selRole != nil {
				a.onAcctRefreshBalance()
			}
		},
	}
}

// onAcctAssetSendBatch 按「发放给」的范围批量发资产邮件。
//
// 只由 onAcctAssetSendDispatch 在范围 ≠ "当前选中角色"时调用：单个角色走原来的同步接口，
// 行为、提示、余额摘要与以前完全一致（不要为了少一条代码路径把它也丢进后台任务）。
func (a *App) onAcctAssetSendBatch() {
	idx := acctAssetTargetIndex()
	if idx == 0 {
		// 防御：调用方应该已经分发过了，这里再兜一次，避免以后有人直接调它。
		a.onAcctAssetSend()
		return
	}
	scope := WalletMailScopeOnlineChars
	scopeText := "当前在线的每个角色"
	if idx == 2 {
		scope = WalletMailScopeServerFirst
		scopeText = "全服创建最早的那一个角色（可能是不常上线的号，请先核对下方确认框里的角色名）"
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
	if acctW.assetBatchTaskID != "" {
		a.showInfo("已有批量任务在执行", "上一批还没结束（"+acctW.assetBatchTaskID+"），请等它完成或先点「取消批量」。")
		return
	}

	detail := fmt.Sprintf("将批量发放资产邮件：\n\n方式：%s\n数量：%d\n发放目标：%s",
		acctAssetModeLabel(acctW.assetModeCb), amount, scopeText)
	dryRun := acctW.assetDryRunChk != nil && acctW.assetDryRunChk.Checked()
	if dryRun {
		detail = fmt.Sprintf("**预演：只解析目标，不发放**\n\n方式：%s（实际不会发放）\n数量：%d（实际不会发放）\n发放目标：%s"+
			"\n\n结果会显示在状态行：将要发给哪些角色（最多列 10 个）。确认名单无误后，取消勾选「预演」再执行一次。",
			acctAssetModeLabel(acctW.assetModeCb), amount, scopeText)
		if !a.confirm("预演：只解析不发放", detail) {
			return
		}
	} else {
		if currency == "cera" {
			detail += "\n\n⚠ 点券是直接入账（不是邮件附件）：一旦入账无法撤回。" +
				"\n若某个角色入账成功但通知邮件发送失败，服务端会单独计数且不会重试，看到该计数时不要重跑整批（会重复发券）。"
		}
		detail += "\n\n服务端在后台按角色逐个发放（顺序执行，避免压垮回环管理口），" +
			"进度显示在右侧；过程中可点「取消批量」，已发出/已入账的不回滚。"
		if scope == WalletMailScopeServerFirst {
			detail += "\n\n提示：不确定这条唯一目标是谁时，先勾选「预演」确认角色名。"
		}
		if !a.confirm("确认批量发放资产邮件？", detail) {
			return
		}
	}

	req := WalletMailBatchRequest{
		Scope:    scope,
		Currency: currency,
		Amount:   amount,
		DryRun:   dryRun,
	}
	var task *BatchTask
	a.acctRun("批量资产邮件", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		var err error
		task, err = cl.AcctWalletMailBatch(context.Background(), req)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("批量资产邮件发起失败：%v", err)
			a.showError("批量资产邮件发起失败", err)
			return
		}
		a.batchStart(a.acctAssetBatchJob(), task)
	})
}

// onAcctAssetBatchCancel 取消正在跑的批量资产邮件。
func (a *App) onAcctAssetBatchCancel() {
	a.batchCancel(a.acctAssetBatchJob(),
		"确认取消批量资产邮件？\n\n已经发出/已入账的角色不会回滚，剩余角色不再发放。")
}

// ---------- ① 删除全服邮件（批量清空邮箱） ----------

// mailboxClearBatchMaxAll 全服档显式要的服务端上限（与服务端硬上限 20000 对齐）。
//
// 为什么全服档要显式填满上限：服务端默认只处理 5000 个角色，超过就截断并在任务消息里
// 追加"已达上限 N，全服角色可能没清完，请再次执行"。运营点的是"删除全服邮件"，
// 撞上默认上限会让他在**没清完**的情况下以为清完了 —— 与其指望每个人去读那句后缀，
// 不如一次就把上限开满（顺序发放约 13~15 分钟，有进度条与取消按钮兜着）。
// 服务端硬上限更低时会自行夹取，填大了不会出错。
const mailboxClearBatchMaxAll = 20000

// acctMailboxBatchJob 批量清空邮箱的任务呈现方式。
func (a *App) acctMailboxBatchJob() batchJob {
	return batchJob{
		prefix:    "删除全服邮件",
		statusL:   acctW.mbBatchStatusL,
		cancelBtn: acctW.mbBatchCancelBtn,
		startBtns: []*walk.PushButton{acctW.mbClearAllBtn, acctW.mbClearOnlineBtn},
		taskID:    &acctW.mbBatchTaskID,
		polling:   &acctW.mbBatchPolling,
		onDone: func(task *BatchTask) {
			// 清空的是"别人的邮箱"，当前选中角色的列表可能也受影响：重新读一次最直观。
			if acctW.selRole != nil {
				a.onAcctMailboxLoad()
			}
		},
	}
}

// onAcctMailboxClearBatch 批量清空邮箱。
//
// scope=all-characters 是**全服级物理删除**：邮件本体、未领取的金币与附件一起从库里消失，
// 无法恢复。所以这里的确认是两道：第一道讲清后果，第二道要求确认规模（可先预演）。
func (a *App) onAcctMailboxClearBatch(scope string) {
	if acctW.mbBatchTaskID != "" {
		a.showInfo("已有批量任务在执行", "上一批还没结束（"+acctW.mbBatchTaskID+"），请等它完成或先点「取消批量」。")
		return
	}
	dryRun := acctW.mbDryRunChk != nil && acctW.mbDryRunChk.Checked()

	scopeText := "全服每个角色"
	warn := "将删除**全服所有角色**的邮件"
	if scope == MailboxScopeOnlineCharacters {
		scopeText = "当前在线的每个角色"
		warn = "将删除**当前在线角色**的邮件"
	}

	if dryRun {
		if !a.confirm("预演：只统计不删除", fmt.Sprintf(
			"将**只统计**目标规模，一封邮件都不删：\n\n范围：%s\n\n预演结果会显示在状态行（目标角色数与累计邮件数），确认规模后再取消勾选「预演」真正执行。", scopeText)) {
			return
		}
	} else {
		first := fmt.Sprintf("%s：\n\n范围：%s\n\n⚠ 这是**物理删除**，不可恢复："+
			"\n· 邮件本体、未领取的金币与附件会一起从数据库消失；"+
			"\n· 玩家端会实时刷新（在线角色会立刻看到邮箱空了）；"+
			"\n· 想按批次撤回某次群发在这里做不到（邮件没有批次号），只能整箱删。"+
			"\n\n强烈建议先勾选「预演」量一遍规模，并确认服务端备份已是最新。\n\n继续吗？", warn, scopeText)
		if !a.confirm("删除全服邮件（1/2）", first) {
			return
		}
		if !a.confirm("删除全服邮件（2/2）", fmt.Sprintf(
			"再次确认：范围 %s，执行后**无法撤销**。\n\n确定现在执行吗？", scopeText)) {
			return
		}
	}

	req := MailboxClearBatchRequest{Scope: scope, Confirm: true, DryRun: dryRun}
	if scope == MailboxScopeAllCharacters {
		req.Max = mailboxClearBatchMaxAll
	}
	var task *BatchTask
	a.acctRun("删除全服邮件", func() error {
		cl, cerr := a.acctClient()
		if cerr != nil {
			return cerr
		}
		var err error
		task, err = cl.AcctMailboxClearBatch(context.Background(), req)
		return err
	}, func(err error) {
		if err != nil {
			acctLog("删除全服邮件发起失败：%v", err)
			a.showError("删除全服邮件发起失败", err)
			return
		}
		// 前缀不做"（预演）"这类区分：服务端完成文案自己会写"预演：…未删除任何邮件"，
		// 两边都加会变成"删除全服邮件（预演）：预演：将清空…"这种叠字。
		a.batchStart(a.acctMailboxBatchJob(), task)
	})
}

// onAcctMailboxBatchCancel 取消正在跑的批量清空。
func (a *App) onAcctMailboxBatchCancel() {
	a.batchCancel(a.acctMailboxBatchJob(),
		"确认取消批量删除邮件？\n\n已经清空的邮箱不会恢复，剩余角色不再处理；"+
			"取消后处于**半清空**状态，需要重新跑一次才能覆盖剩下的人。")
}
