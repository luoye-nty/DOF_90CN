//go:build paid

// 资产邮件批量发放：POST /manage/v1/account/wallet/mail-batch
//
// scope 三档：
//   - online-characters      每个在线角色各发一份
//   - all-accounts           每个账号的首角色（槽位号最小的那个）
//   - server-first-character 全服唯一最早创建的角色（只读查库，**不走游戏服接口**）
//
// currency 两档：gold / cera，与单角色接口（POST /manage/v1/account/wallet/mail）同口径 ——
// 业务实现就是同一个 walletMailToTarget（handlers_account.go），这里只负责"把目标变一批"。
//
// 与群发物品邮件（grant-batch）的区别只在"发什么"：那条是邮件附件带物品，
// 这条是金币邮件 / 点券入账 + 到账通知邮件。骨架（异步任务、顺序发放、进度、取消、
// 跨 kind 互斥）完全共用，见 handlers_batch_mail.go 与 tasks.go。
//
// ⚠ cera 分支**不幂等**：先入账（GM wallet/update add），再发到账通知邮件。
// 通知邮件失败时**钱已经在玩家账上**了，所以这种目标只单独计数、绝不重试 ——
// 把"发放"整体重试一遍等于再入账一次，那是重复发券（见 walletMailOutcome.NotifyFailed）。
package manage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"dnf90ops/internal/store"
)

// wallet-mail-batch 的 max：缺省 2000，超过 batchGrantMaxItems（5000）按上限夹住。
//
// 与 clear-batch 的上限是两套（那边默认 5000 / 硬上限 20000）：清邮箱要覆盖"全服所有角色"，
// 资产邮件发的是钱，一次发太多在事故时更难收拾，所以沿用群发邮件那一档保守上限。
const (
	walletMailBatchDefaultMax = 2000
	walletMailBatchHardMax    = batchGrantMaxItems
)

// walletMailBatchRequest POST /manage/v1/account/wallet/mail-batch 入参。
type walletMailBatchRequest struct {
	Scope    string `json:"scope"`
	Currency string `json:"currency"` // gold / cera
	Amount   int64  `json:"amount"`
	// Title / Body 可选：留空按币种用默认文案（与单角色接口一致）。
	Title string `json:"title"`
	Body  string `json:"body"`
	// Max 本次最多发放多少个角色（可选）。<=0 用默认值，超过硬上限按硬上限夹住。
	Max int `json:"max"`
	// DryRun 只解析目标、不做任何发放（把 Total 与目标角色列进任务消息）。
	//
	// 为什么需要它：scope=server-first-character 只有一个目标，而运营在点确认前
	// **根本不知道那是谁**（很可能是个测试号/长期不上的号）。先解析、不发放，
	// 看一眼再决定 —— 这是"发钱"类操作最低限度的可核对性。
	DryRun bool `json:"dry_run"`
}

// walletMailBatchAmountMax 批量资产邮件的单次数量上限：**两个币种共用**，与单角色接口同口径。
//
// ⚠ 别按币种分开：历史上（含单角色接口 handleAccountWalletMail 与 AC-2 的校验）就是
// gold / cera 共用 accountMaxWalletBalance 这一个上限，管理工具界面也按它校验。
// 就算邮件金币字段本身是 uint32（比 int32 宽），批量偷偷多接受 2147483648..4294967295
// 也只会变成"批量能过、单个过不了"的诡异不一致，运营没法解释。宁可保守，也不制造第二种口径。
const walletMailBatchAmountMax = accountMaxWalletBalance

// walletMailBatchLimit 把请求里的 max 夹到 [默认值, 硬上限]（口径同 clear-batch，见那里的注释）。
func walletMailBatchLimit(max int) int {
	if max <= 0 {
		return walletMailBatchDefaultMax
	}
	if max > walletMailBatchHardMax {
		return walletMailBatchHardMax
	}
	return max
}

// handleAccountWalletMailBatch 启动一次批量资产邮件发放（立刻返回任务，实际发放走后台）。
func (s *Server) handleAccountWalletMailBatch(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)

	if s.tasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "task-registry-unavailable")
		return
	}
	var req walletMailBatchRequest
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw

	req.Scope = strings.TrimSpace(req.Scope)
	switch req.Scope {
	case batchScopeOnlineChars, batchScopeAllAccounts, batchScopeServerFirst:
	default:
		writeErr(w, http.StatusBadRequest,
			"bad-request: scope 必须是 online-characters / all-accounts / server-first-character")
		return
	}
	req.Currency = strings.ToLower(strings.TrimSpace(req.Currency))
	if req.Currency != "gold" && req.Currency != "cera" {
		writeErr(w, http.StatusBadRequest, "bad-request: currency 只能是 gold 或 cera")
		return
	}
	if req.Amount <= 0 || req.Amount > walletMailBatchAmountMax {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("bad-request: amount 必须为正且不超过上限 %d（与单角色资产邮件同口径）", walletMailBatchAmountMax))
		return
	}
	// 标题/正文长度与群发物品邮件同口径（服务端邮件字段有限长，太长会被截断或整封拒收）。
	if len([]rune(strings.TrimSpace(req.Title))) > 40 {
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件标题最多 40 字")
		return
	}
	if len([]rune(strings.TrimSpace(req.Body))) > 400 {
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件正文最多 400 字")
		return
	}
	limit := walletMailBatchLimit(req.Max)

	audit.fields["scope"] = req.Scope
	audit.fields["currency"] = req.Currency
	audit.fields["amount"] = req.Amount
	audit.fields["dry_run"] = req.DryRun
	audit.fields["max"] = limit

	// 与群发邮件 / 批量清空邮箱共用一把互斥锁（三者都在逐个角色压同一个回环口）。
	ctx, cancel := context.WithCancel(context.Background())
	task, busy := s.tasks.createIfNoRunningKinds(taskKindWalletMail, req.Scope, cancel, batchPressureKinds)
	if task == nil {
		cancel()
		writeErr(w, http.StatusConflict, batchConflictMessage(busy))
		return
	}

	s.log.Info("batch wallet mail started", "task", task.id, "scope", req.Scope,
		"currency", req.Currency, "amount", req.Amount, "dry_run", req.DryRun, "max", limit)

	go s.runBatchWalletMail(ctx, task, req, limit)

	audit.ok()
	writeOK(w, map[string]any{"task": task.snapshot()})
}

// runBatchWalletMail 后台执行：先收集目标，再逐个角色发放，全程更新 task 进度。
func (s *Server) runBatchWalletMail(ctx context.Context, task *batchTaskState, req walletMailBatchRequest, limit int) {
	res, err := s.collectWalletTargets(ctx, task, req.Scope, limit)
	if err != nil {
		task.finish(taskStatusFailed, "收集发放目标失败: "+err.Error())
		s.log.Error("batch wallet mail collect failed", "task", task.id, "scope", req.Scope, "error", err)
		return
	}
	targets := res.Targets
	if len(targets) == 0 {
		if res.CollectFailed > 0 {
			task.finish(taskStatusFailed,
				fmt.Sprintf("没有可发放的角色：%d 个账号在收集阶段失败（原因见 errors）", res.CollectFailed))
			s.log.Error("batch wallet mail collect empty", "task", task.id, "collect_failed", res.CollectFailed)
			return
		}
		task.finish(taskStatusDone, "没有符合条件的收件角色（0 个）")
		return
	}
	task.setPhase(taskPhaseSending, len(targets))
	sortBatchTargetsByCharacterID(targets)

	if req.DryRun {
		// 预演：一个 GM 调用都不发（server-first-character 这一档连收集都只读库，
		// 所以整条链路是零回环；另外两档在收集阶段本就必需查角色列表）。
		task.finish(taskStatusDone, fmt.Sprintf("预演：将向 %d 个角色发放（%s）；未执行%s",
			len(targets), batchDryRunTargetsPreview(targets),
			batchTruncatedSuffix(res, limit, "可能有角色没发到")))
		s.log.Info("batch wallet mail dry-run", "task", task.id, "targets", len(targets))
		return
	}

	// 已入账但通知失败的目标数（仅 cera 会出现）。
	// 单独计数、绝不重试：钱已经到账，重试等于重复发券。
	var notifyFailed atomic.Int64
	s.runBatchTargetsSequential(ctx, task, targets, "batch wallet mail failed", func(target batchMailTarget) error {
		outcome, err := s.walletMailToTarget(ctx, target.CharacterName, walletMailRequest{
			Currency: req.Currency,
			Amount:   req.Amount,
			Title:    req.Title,
			Body:     req.Body,
		})
		if err != nil {
			return err
		}
		if outcome.NotifyFailed {
			if n := notifyFailed.Add(1); n <= batchGrantLogLimit {
				s.log.Warn("batch wallet mail notify failed", "task", task.id,
					"character_id", target.CharacterID, "character", target.CharacterName,
					"account", target.Username, "amount", req.Amount, "error", outcome.NotifyError)
			}
		}
		return nil
	})

	snap := task.snapshot()
	// "已入账但通知失败"既不算失败（钱到了，不能重试）也不能只藏在计数里：
	// 必须在消息里点名，运营才会去人工补一封通知，而不是以为一切正常。
	suffix := ""
	if n := notifyFailed.Load(); n > 0 {
		suffix = fmt.Sprintf("；已入账但通知失败 %d 个（点券已到账，请勿重试，可人工补发通知）", n)
	}
	suffix += batchTruncatedSuffix(res, limit, "可能有角色没发到")

	switch {
	case task.runCanceled():
		task.finish(taskStatusCanceled,
			fmt.Sprintf("已取消：成功 %d，失败 %d，跳过 %d%s", snap.OK, snap.Failed, snap.Skipped, suffix))
	case ctx.Err() != nil:
		task.finish(taskStatusCanceled, "已中断")
	default:
		task.finish(taskStatusDone,
			fmt.Sprintf("发放完成：成功 %d，失败 %d，跳过 %d%s", snap.OK, snap.Failed, snap.Skipped, suffix))
	}
	s.log.Info("batch wallet mail finished", "task", task.id, "ok", snap.OK, "failed", snap.Failed,
		"skipped", snap.Skipped, "notify_failed", notifyFailed.Load(), "truncated", res.Truncated)
}

// collectWalletTargets 收集资产邮件发放目标。
//
// server-first-character 不走通用的账号遍历收集器：它的目标不在游戏服的角色列表里，
// 而是"全服唯一最早创建的角色"，直接只读查库（internal/store/game_character.go）。
func (s *Server) collectWalletTargets(ctx context.Context, task *batchTaskState, scope string, limit int) (batchCollectResult, error) {
	if scope != batchScopeServerFirst {
		return s.collectBatchTargets(ctx, scope, task, limit)
	}
	ch, found, err := s.serverFirstCharacter(ctx)
	if err != nil {
		return batchCollectResult{}, err
	}
	if !found {
		// "全服还没角色"是业务状态，不是故障；但目标数为 0 的任务**必须报错**，
		// 否则运营会以为"已经发给那个唯一的目标了"。
		return batchCollectResult{}, errNoServerFirstCharacter
	}
	return batchCollectResult{Targets: []batchMailTarget{{
		AccountID:     ch.AccountID,
		Username:      s.resolveAccountUsername(ctx, ch.AccountID),
		CharacterID:   ch.CharacterID,
		CharacterName: ch.Name,
	}}}, nil
}

// errNoServerFirstCharacter 全服一个角色都没有（dnf_characters 里没有 delete_flag=0 的行）。
var errNoServerFirstCharacter = errors.New(
	"全服还没有任何角色（dnf_characters 里没有 delete_flag=0 的记录），无法确定发放目标")

// serverFirstCharacter 全服最早创建的角色：优先桥接注入的实现，未注入时退回连接池
// （与 handlers_batch_mail.go 里两个列表查询同样的两条路，理由见那里的注释）。
func (s *Server) serverFirstCharacter(ctx context.Context) (store.ServerFirstCharacter, bool, error) {
	if s.bridge != nil && s.bridge.AcctServerFirstCharacter != nil {
		return s.bridge.AcctServerFirstCharacter(ctx)
	}
	st := s.bridgeStore()
	if st == nil {
		return store.ServerFirstCharacter{}, false, errAuthStoreUnavailable
	}
	return st.ServerFirstCharacter(ctx)
}

// resolveAccountUsername 尽力把账号主键翻成登录用户名（拿不到就返回空串）。
//
// 只服务于"预演消息里那句提示"：发放本身只认角色名，翻译失败不影响任何业务，
// 所以这里绝不上抛错误 —— 一句更好看的提示不值得让整批任务失败。
// 翻不出来时调用方会退化成显示账号主键（dnf:<sha256>），总比显示空白强。
func (s *Server) resolveAccountUsername(ctx context.Context, accountID string) string {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || s.bridge == nil || s.bridge.AccountNamesByID == nil {
		return ""
	}
	names, err := s.bridge.AccountNamesByID(ctx, []string{accountID})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(names[accountID])
}
