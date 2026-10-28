//go:build paid

// 删除全服邮件（批量清空邮箱）：POST /manage/v1/account/mailbox/clear-batch
//
// ⚠⚠ 这是**物理删除、不可恢复**。走的是游戏服务端原生 GM 接口
// /local/dnf-gm/mail/clear：服务端把邮件行删掉，并把新的邮箱列表推给在线客户端。
// 没有回收站、没有"撤销"，除了数据库备份没有任何找回手段。所以这道接口有三重刹车：
//   - confirm 必须显式为 true（缺了/写了 false 直接 400，绝不做"不填就当你同意"）；
//   - scope 必须显式选（默认意图是"删全服邮件"，但绝不把"不填"解释成"删全服"）；
//   - dry_run=true 可以只解析目标、一封都不删，让运营先看清会落到多少角色头上。
//
// 为什么必须走 GM 接口、**绝不能**直删 dnf_mails 表（这个念头很自然，值得写下来挡住下一次）：
//  1. 在线角色的邮箱在**服务端内存**里，数据库只是它的存档。直删表里的行，玩家下线/换线
//     触发存档时内存里的邮箱会被整份写回库 —— 邮件"复活"，运营还会以为删除没生效；
//  2. 直删不会推送刷新：在线玩家界面里邮件还在，点开一封已经不存在的邮件就是新的 bug；
//  3. GM 接口是服务端唯一能把"改内存 + 落库 + 推客户端"三件事一起做完的地方
//     （与单角色清空 handleAccountMailboxClear 是同一条路）。
//
// 批量与单角色的关系：单角色那条（handlers_inventory.go 的 handleAccountMailboxClear）清
// **一个角色**，这里把同一个动作逐角色做一遍 —— 语义完全一致（同一个 GM 接口、confirm 取值
// 相同），只有定位方式不同：批量**只发 character_id、不发 character_name**，
// 理由见 mailboxClearBody 的注释（服务端在"同时给了 ID 和名字"时会做一致性校验，
// 而批量要跑几分钟，期间改过名的角色会被判成查无此人）。
//
// 顺序发放（batchGrantWorkers=1）：见 handlers_batch_mail.go 里 2026-09-25 事故的说明 ——
// 并发压回环口会被游戏服直接回 500。这里复用同一套顺序执行器。
package manage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// clear-batch 的上限是**独立的**，不沿用 batchGrantMaxItems（5000）：
//
//	群发邮件的目标数由"每账号一个角色 / 在线角色"决定，5000 够用；而"删除全服邮件"要覆盖
//	**全服所有角色**，一个正常运营的服轻松就超过 5000 个角色 —— 沿用旧上限会让运营
//	反复执行、每次都以为清完了。所以默认给到 5000（一次点下去能覆盖绝大多数服），
//	硬上限 20000（顺序发放约 13~15 分钟，还在"运营盯着进度条能接受"的范围内）。
//
// 撞上限时任务消息必须明说（见 batchTruncatedSuffix）：不能让运营以为全服清干净了。
const (
	mailboxClearBatchDefaultMax = 5000
	mailboxClearBatchHardMax    = 20000
)

// mailboxClearBatchRequest POST /manage/v1/account/mailbox/clear-batch 入参。
type mailboxClearBatchRequest struct {
	// Scope 三档：all-characters（全服所有角色）/ online-characters（仅在线角色）/
	// all-accounts（每个账号的首角色）。必填。
	Scope string `json:"scope"`
	// Confirm 必须是 true。全服级物理删除不接受省略。
	Confirm bool `json:"confirm"`
	// DryRun 只解析目标、不删任何邮件（把 Total 与将命中的角色列表填进任务消息）。
	DryRun bool `json:"dry_run"`
	// Max 本次最多清空多少个角色（可选）。<=0 用默认值，超过硬上限按硬上限夹住。
	Max int `json:"max"`
}

// mailboxClearBatchLimit 把请求里的 max 夹到 [默认值, 硬上限]。
//
// 单独一个函数只为让"夹取口径"可被单测钉住：上限是安全阀，写错方向（比如把硬上限
// 当成默认值）会让一次点击清掉远超预期的角色数。
func mailboxClearBatchLimit(max int) int {
	if max <= 0 {
		return mailboxClearBatchDefaultMax
	}
	if max > mailboxClearBatchHardMax {
		return mailboxClearBatchHardMax
	}
	return max
}

// handleAccountMailboxClearBatch 启动一次全服邮件清理（立刻返回任务，实际清空走后台）。
func (s *Server) handleAccountMailboxClearBatch(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)

	if s.tasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "task-registry-unavailable")
		return
	}
	var req mailboxClearBatchRequest
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw

	req.Scope = strings.TrimSpace(req.Scope)
	switch req.Scope {
	case batchScopeAllChars, batchScopeOnlineChars, batchScopeAllAccounts:
	default:
		writeErr(w, http.StatusBadRequest,
			"bad-request: scope 必须是 all-characters / online-characters / all-accounts")
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "bad-request: 需要 confirm=true 才允许清空邮件")
		return
	}
	limit := mailboxClearBatchLimit(req.Max)

	// 审计字段按约定记全：这是不可恢复的操作，事后追责/复盘只靠这份 jsonl。
	audit.fields["scope"] = req.Scope
	audit.fields["confirm"] = req.Confirm
	audit.fields["dry_run"] = req.DryRun
	audit.fields["max"] = limit

	// 与群发邮件 / 批量资产邮件共用一把互斥锁（三者都在逐个角色压同一个回环口）。
	ctx, cancel := context.WithCancel(context.Background())
	task, busy := s.tasks.createIfNoRunningKinds(taskKindMailboxClear, req.Scope, cancel, batchPressureKinds)
	if task == nil {
		cancel()
		writeErr(w, http.StatusConflict, batchConflictMessage(busy))
		return
	}

	s.log.Info("batch mailbox clear started", "task", task.id, "scope", req.Scope,
		"dry_run", req.DryRun, "max", limit)

	go s.runBatchMailboxClear(ctx, task, req, limit)

	audit.ok()
	writeOK(w, map[string]any{"task": task.snapshot()})
}

// runBatchMailboxClear 后台执行：先收集目标，再逐个角色清空邮箱，全程更新 task 进度。
func (s *Server) runBatchMailboxClear(ctx context.Context, task *batchTaskState, req mailboxClearBatchRequest, limit int) {
	res, err := s.collectBatchTargets(ctx, req.Scope, task, limit)
	if err != nil {
		task.finish(taskStatusFailed, "收集清空目标失败: "+err.Error())
		s.log.Error("batch mailbox clear collect failed", "task", task.id, "error", err)
		return
	}
	targets := res.Targets
	if len(targets) == 0 {
		// 一个目标都没收到、且收集阶段有账号失败：这是"故障"不是"本来就没角色"。
		// 报 done 会让运营以为邮件清完了，从而漏掉真正的故障。
		if res.CollectFailed > 0 {
			task.finish(taskStatusFailed,
				fmt.Sprintf("没有可清空的角色：%d 个账号在收集阶段失败（原因见 errors）", res.CollectFailed))
			s.log.Error("batch mailbox clear collect empty", "task", task.id, "collect_failed", res.CollectFailed)
			return
		}
		task.finish(taskStatusDone, "没有符合条件的角色（0 个）")
		return
	}
	task.setPhase(taskPhaseSending, len(targets))

	// 按角色 ID 升序清空：顺序固定 → 失败项与日志顺序可复现，便于对着日志逐个处理。
	sortBatchTargetsByCharacterID(targets)

	if req.DryRun {
		// 预演：一封都不删。把 Total 与将命中的角色列出来，运营确认后再正式执行。
		//
		// 这里刻意**不去统计每个角色有多少封邮件**：那要对每个角色发一次邮箱查询回环，
		// 为了消息里多一个数字把回环口压一遍不值得（取不到就不显示）。
		task.finish(taskStatusDone, fmt.Sprintf("预演：将清空 %d 个角色的邮箱（%s）；未删除任何邮件%s",
			len(targets), batchDryRunTargetsPreview(targets),
			batchTruncatedSuffix(res, limit, "全服角色可能没清完")))
		s.log.Info("batch mailbox clear dry-run", "task", task.id, "targets", len(targets))
		return
	}

	s.runBatchTargetsSequential(ctx, task, targets, "batch mailbox clear failed", func(target batchMailTarget) error {
		var result GMTMailboxClearResult
		if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/mail/clear",
			mailboxClearBody(target), &result); err != nil {
			return err
		}
		return nil
	})

	snap := task.snapshot()
	truncated := batchTruncatedSuffix(res, limit, "全服角色可能没清完")
	switch {
	case task.runCanceled():
		task.finish(taskStatusCanceled,
			fmt.Sprintf("已取消：成功 %d，失败 %d，跳过 %d%s", snap.OK, snap.Failed, snap.Skipped, truncated))
	case ctx.Err() != nil:
		task.finish(taskStatusCanceled, "已中断")
	default:
		task.finish(taskStatusDone,
			fmt.Sprintf("清空完成：成功 %d，失败 %d，跳过 %d%s", snap.OK, snap.Failed, snap.Skipped, truncated))
	}
	s.log.Info("batch mailbox clear finished", "task", task.id,
		"ok", snap.OK, "failed", snap.Failed, "skipped", snap.Skipped, "truncated", res.Truncated)
}

// mailboxClearBody 批量清空发给 GM 的请求体：**只带 character_id + confirm**。
//
// ⚠ 这里**故意不带 character_name**（单角色 handler 会带名字，那条链路保持原样不动）。
// 服务端取角色走 gmCharacter（go-server gm_admin_state.go），里面有一条
// "同时给了 ID 和名字时校验两者是否一致，不一致 → ErrGMCharacterNotFound"。
// 批量要跑几千个角色、耗时几分钟，这段时间里任何一个**改过名**的角色都会因为
// "ID 对得上、名字对不上"被判成查无此人而失败。角色 ID 才是权威标识（重名/改名都不影响），
// 与 grant-batch 只发 character_id 是同一个理由。
// 名字仍然留在任务详情、失败原因与运维日志里（那是给人看的），只是不发给服务端做校验。
func mailboxClearBody(target batchMailTarget) map[string]any {
	body := map[string]any{"confirm": true}
	if id := strings.TrimSpace(target.CharacterID); id != "" {
		body["character_id"] = id
	}
	return body
}
