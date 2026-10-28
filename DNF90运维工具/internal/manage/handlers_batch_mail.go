//go:build paid

// 批量任务的公共基础设施（目标收集 + 顺序执行 + 预演文案）。
//
// 起点是「群发邮件」（管理工具「账号管理 → 物品发放」的两个批量按钮）：
//   - POST /manage/v1/account/items/grant-batch  {scope, item_id, amount, ...}
//   - GET  /manage/v1/tasks /manage/v1/tasks/{id} /POST /manage/v1/tasks/{id}/cancel
//     进度查询与取消（见 tasks.go 的说明：上千账号必须异步）
//
// 发放本身就是单品发放那条路（同一个执行端 PVF 校验 + 同一个 GM 邮件接口），
// 只是把"目标角色"从一个变成了一批。
//
// 后来两个批量接口（handlers_mailbox_batch.go 的批量清空邮箱、handlers_wallet_batch.go
// 的批量资产邮件）完全是同一套骨架 —— 都是"收集一批角色 → 顺序逐个打 GM 回环口"。
// 所以收集器、顺序执行器、预演文案都放在这里共用，**不要各写一份**：
// 三份复制出来的循环迟早会在"取消要检查几次""失败日志写多少条"上跑偏。
package manage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dnf90ops/internal/pvfindex"
	"dnf90ops/internal/store"
)

// 批量发放的并发度与单批上限。
//
// ⚠ 并发度 = 1（**顺序发放**）。这是 2026-09-25 按线上事故改的，证据：
// 现场服务器（203.0.113.13）scope=online-characters、8 个在线角色时，
// 8 个 POST /local/dnf-gm/mail 在同一毫秒内并发发出，游戏服回环管理口有 3 个返回
// **HTTP 500**（运维日志：`batch grant finished task=... ok=5 failed=3`），
// 失败的耗时只有 14~37ms（不是超时，是服务端直接报错）。
// 邮件要落库 + 刷新在线邮箱，并发压上去会互相踩；改成顺序发放后不再出现。
// 代价：每个目标约 40ms，5000 个上限约 3~4 分钟（可接受，且进度条会一直动）。
//
// ⚠ 这个"顺序"对后来新增的两类批量任务（批量资产邮件 / 批量清空邮箱）同样是硬约束：
// 它们压的是同一个回环口，任何一类并发上去都会把事故重演。
const (
	batchGrantWorkers  = 1
	batchGrantMaxItems = 5000
)

// batchGrantLogLimit 逐条失败日志的上限：运维日志里要能直接看到原因，
// 但也不能因为一次事故把日志刷爆（超过上限只计数，不再逐条写）。
const batchGrantLogLimit = 30

// batchAccountPageSize 遍历全服账号时的分页大小（与 store.ListAccountsPage 的上限 2000 无关，
// 取 300 是为了每页一次回环就够快、进度也能动起来）。
const batchAccountPageSize = 300

// batchDryRunPreviewMax 预演消息里最多列几个目标；再多就折叠成"…等 N 个"。
const batchDryRunPreviewMax = 10

// 批量目标范围。
const (
	batchScopeAllAccounts = "all-accounts"      // 每个账号的第一个角色
	batchScopeOnlineChars = "online-characters" // 每个在线角色
	// batchScopeAllChars 全服所有角色（每个账号的**每一个**角色）。
	// 只有批量清空邮箱用它："删除全服邮件"要求的是所有角色都清，不是每账号一个。
	batchScopeAllChars = "all-characters"
	// batchScopeServerFirst 全服唯一最早创建的角色（只读 store 查询，不走游戏服接口）。
	// 批量资产邮件用它：某些活动只给"全服第一个角色"发，运营在点确认前需要先看到它是谁。
	batchScopeServerFirst = "server-first-character"
)

// errAuthStoreUnavailable 认证服务未运行（读不到账号 / 在线数据时的统一原因文案）。
var errAuthStoreUnavailable = errors.New("认证服务未运行")

// batchMailTarget 一个收件目标。
type batchMailTarget struct {
	AccountID     string
	Username      string
	CharacterID   string // 角色 ID：发信只认它（名字可能被改），也用它排序
	CharacterName string
}

// batchCollectResult 目标收集结果。
type batchCollectResult struct {
	Targets []batchMailTarget
	// CollectFailed 收集阶段失败的账号数：这些失败会被 resetCollectCounters 从计数器里清掉，
	// 所以必须单独回传，否则"所有账号都查不到角色"会被报成 done（见 runBatchGrant）。
	CollectFailed int
	// Truncated 是否因上限截断（收集到的角色数 ≥ 上限）：任务消息里必须明说，
	// 否则运营会以为"全服都清完了 / 都发到了"。
	Truncated bool
}

// markTruncatedIfFull 出口处统一判定截断：收集到的角色数 ≥ 上限即算"已达上限"。
//
// 注意这条判定故意取得保守（等于上限就算截断）：上限是运营自己填的，
// 刚好等于上限本身就说明"还有没有下一个"是未知的，报出来比吞掉安全。
func (res *batchCollectResult) markTruncatedIfFull(limit int) {
	if limit > 0 && len(res.Targets) >= limit {
		res.Truncated = true
	}
}

// batchGrantRequest POST /manage/v1/account/items/grant-batch 入参。
type batchGrantRequest struct {
	Scope string `json:"scope"`
	// 与单品发放同字段名：item_id / amount / reinforce_level / amplify_level / amplify_type。
	ItemID         uint32 `json:"item_id"`
	Amount         uint32 `json:"amount"`
	ReinforceLevel int    `json:"reinforce_level"`
	AmplifyLevel   int    `json:"amplify_level"`
	AmplifyType    int    `json:"amplify_type"`
	// Title/Body 可选：留空用默认文案（与单品发放一致）。
	Title string `json:"title"`
	Body  string `json:"body"`
	// Bind 附件是否绑定（默认不绑定，与单品发放一致）。
	Bind bool `json:"bind"`
}

// handleAccountItemsGrantBatch 启动一次群发（立刻返回任务，实际发放走后台）。
func (s *Server) handleAccountItemsGrantBatch(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)

	if s.tasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "task-registry-unavailable")
		return
	}
	var req batchGrantRequest
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw

	req.Scope = strings.TrimSpace(req.Scope)
	switch req.Scope {
	case batchScopeAllAccounts, batchScopeOnlineChars:
	default:
		writeErr(w, http.StatusBadRequest, "bad-request: scope 必须是 all-accounts 或 online-characters")
		return
	}
	if req.ItemID == 0 || req.Amount == 0 || req.Amount > pvfindex.MaxItemAmount {
		writeErr(w, http.StatusBadRequest, "bad-request: item_id/amount 必须为正且数量不超上限")
		return
	}
	if req.ReinforceLevel < 0 || req.AmplifyLevel < 0 || req.AmplifyType < 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: 强化/增幅参数不能为负")
		return
	}
	// 同一批同时在跑的批量任务只允许一个：给上千人发邮件是很重的动作，
	// 连着点两次按钮不该变成"每人收两封"；和批量资产邮件 / 批量清空邮箱也互斥
	// （三类都在逐个角色压同一个回环口，见 batchPressureKinds）。
	//
	// ⚠ 占位必须在**构建 PVF 索引之前**、并且用"查+建"同一把锁的 createIfNoRunning：
	// 原来的写法是"先查有没有在跑的、再去校验 PVF、最后才登记任务"，中间那几秒里
	// 第二次点击同样看到"没有在跑"，于是两个任务并行发放 → 每人两封。
	ctx, cancel := context.WithCancel(context.Background())
	task, running := s.tasks.createIfNoRunning(taskKindGrantMail, req.Scope, cancel)
	if task == nil {
		cancel()
		writeErr(w, http.StatusConflict, batchConflictMessage(running))
		return
	}

	// 执行端 PVF 校验只做一次（不存在/强化非法 → pvf-mismatch），不是每个目标做一次。
	// 校验失败时任务已经占位，必须显式收尾成 failed，否则那道"只允许一个"的守卫会一直占着。
	def, err := s.acctResolveItemForGrant(r.Context(), req.ItemID, req.ReinforceLevel, req.AmplifyLevel, req.AmplifyType)
	if err != nil {
		task.finish(taskStatusFailed, "物品校验失败: "+err.Error())
		cancel()
		s.acctWriteGMError(w, audit, err)
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "GM 物品发放"
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		body = fmt.Sprintf("系统发放：%s（%d）", def.Name, def.ItemID)
	}
	if len([]rune(title)) > 40 {
		task.finish(taskStatusFailed, "邮件标题最多 40 字")
		cancel()
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件标题最多 40 字")
		return
	}
	if len([]rune(body)) > 400 {
		task.finish(taskStatusFailed, "邮件正文最多 400 字")
		cancel()
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件正文最多 400 字")
		return
	}

	audit.fields["scope"] = req.Scope
	audit.fields["item_id"] = def.ItemID
	audit.fields["amount"] = req.Amount

	s.log.Info("batch grant started", "task", task.id, "scope", req.Scope, "item_id", def.ItemID, "amount", req.Amount)

	go s.runBatchGrant(ctx, task, req, def.Name)

	audit.ok()
	writeOK(w, map[string]any{"task": task.snapshot()})
}

// runBatchGrant 后台执行：先收集目标，再逐个发邮件，全程更新 task 进度。
func (s *Server) runBatchGrant(ctx context.Context, task *batchTaskState, req batchGrantRequest, itemName string) {
	// 群发的历史上限仍是 batchGrantMaxItems（5000）：物品发放的目标数由"每个账号一个角色"
	// 或"在线角色"决定，5000 是需求口径；批量清空邮箱那类"全服所有角色"另有独立上限。
	res, err := s.collectBatchTargets(ctx, req.Scope, task, batchGrantMaxItems)
	if err != nil {
		task.finish(taskStatusFailed, "收集发放目标失败: "+err.Error())
		s.log.Error("batch grant collect failed", "task", task.id, "error", err)
		return
	}
	targets := res.Targets
	if len(targets) == 0 {
		// 一个目标都没收到，且收集阶段有账号失败：这是"故障"不是"本来就没角色"。
		// 报 done + failed=0 会让运营以为发完了/没人可发，从而漏掉真正的故障。
		if res.CollectFailed > 0 {
			task.finish(taskStatusFailed,
				fmt.Sprintf("没有可发放的角色：%d 个账号在收集阶段失败（原因见 errors）", res.CollectFailed))
			s.log.Error("batch grant collect empty", "task", task.id, "collect_failed", res.CollectFailed)
			return
		}
		task.finish(taskStatusDone, "没有符合条件的收件角色（0 个）")
		return
	}
	task.setPhase(taskPhaseSending, len(targets))

	// 按角色 ID 升序发放（需求口径）：
	//   ① 顺序发放时"谁先谁后"固定，失败项与日志顺序可复现，便于对着日志逐个处理；
	//   ② 同一账号多角色时也是固定顺序，运营看到的顺序与在线列表一致。
	sortBatchTargetsByCharacterID(targets)

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "GM 物品发放"
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		body = fmt.Sprintf("系统发放：%s（%d）", itemName, req.ItemID)
	}
	payload := gmMailRequest{
		Title: title,
		Body:  body,
		Attachments: []gmMailAttachment{{
			ItemID: req.ItemID, Count: req.Amount, Bind: req.Bind,
			ReinforceLevel: req.ReinforceLevel, AmplifyLevel: req.AmplifyLevel, AmplifyType: req.AmplifyType,
		}},
	}

	// 顺序发放（见 batchGrantWorkers 的事故说明）。
	s.runBatchTargetsSequential(ctx, task, targets, "batch grant mail failed", func(target batchMailTarget) error {
		mail := payload
		// 只用角色 ID 定位收件人：
		//   ① 名字可能在采集之后被改掉（改名卡），而 GM 发信在"同时给了 ID 和名字"时
		//      会校验两者是否一致，不一致直接拒收；
		//   ② ID 是权威标识，重名/改名都不影响投递。
		mail.CharacterID = target.CharacterID
		mail.CharacterName = ""
		var result gmMailResult
		if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/mail", mail, &result); err != nil {
			return err
		}
		return nil
	})

	snap := task.snapshot()
	switch {
	case task.runCanceled():
		task.finish(taskStatusCanceled, fmt.Sprintf("已取消：成功 %d，失败 %d，跳过 %d", snap.OK, snap.Failed, snap.Skipped))
	case ctx.Err() != nil:
		task.finish(taskStatusCanceled, "已中断")
	default:
		task.finish(taskStatusDone,
			fmt.Sprintf("发放完成：成功 %d，失败 %d，跳过 %d", snap.OK, snap.Failed, snap.Skipped))
	}
	s.log.Info("batch grant finished", "task", task.id, "ok", snap.OK, "failed", snap.Failed, "skipped", snap.Skipped)
}

// runBatchTargetsSequential 顺序（batchGrantWorkers=1）把目标逐个交给 fn，并维护任务进度。
//
// 为什么保留这个小并发池的结构而不写成 for 循环：将来真要提速只改 batchGrantWorkers
// 一个常量即可（取消与进度逻辑完全不用动），而顺序语义与 2026-09-25 事故的修复方案一致。
//
// fn 返回 error 非空 = 该目标失败：统一记 task.addResult + 逐条写运维日志
// （上限 batchGrantLogLimit 条）。日志内容必须能看出"哪个角色、什么原因" ——
// 事故复盘时只有一个 failed=N 是查不下去的（线上就是这么查不到原因的）。
func (s *Server) runBatchTargetsSequential(
	ctx context.Context, task *batchTaskState, targets []batchMailTarget,
	logMsg string, fn func(batchMailTarget) error,
) {
	var (
		idxMu sync.Mutex
		next  int
		wg    sync.WaitGroup
	)
	workers := batchGrantWorkers
	if workers > len(targets) {
		workers = len(targets)
	}
	var loggedFailures atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil || task.runCanceled() {
					return
				}
				idxMu.Lock()
				if next >= len(targets) {
					idxMu.Unlock()
					return
				}
				target := targets[next]
				next++
				idxMu.Unlock()

				if err := fn(target); err != nil {
					task.addResult(false, batchTargetErrorText(target, err))
					if loggedFailures.Add(1) <= batchGrantLogLimit {
						s.log.Warn(logMsg, "task", task.id, "kind", task.kind,
							"character_id", target.CharacterID, "character", target.CharacterName,
							"account", target.Username, "account_id", target.AccountID, "error", err)
					}
					continue
				}
				task.addResult(false, "")
			}
		}()
	}
	wg.Wait()
}

// batchTargetErrorText 逐目标失败文案："角色名（账号）：原因"。
// 三段都要有：管理工具把它们直接显示在任务详情里，缺角色或账号都得再翻日志才能定位。
func batchTargetErrorText(target batchMailTarget, err error) string {
	account := strings.TrimSpace(target.Username)
	if account == "" {
		account = strings.TrimSpace(target.AccountID)
	}
	return fmt.Sprintf("%s（%s）：%v", target.CharacterName, account, err)
}

// batchDryRunTargetsPreview 预演消息里的目标摘要："角色A/账号B、角色C/账号D…"，
// 最多列 batchDryRunPreviewMax 个，超过折叠成"…等 N 个"。
//
// 为什么要把目标名列出来：运营点"确认"之前必须能看见这笔操作会落到谁身上 ——
// 尤其是"全服首个角色"这种只有一个目标的范围，那个目标很可能是个测试号/长期不上的号。
func batchDryRunTargetsPreview(targets []batchMailTarget) string {
	shown, suffix := targets, ""
	if len(shown) > batchDryRunPreviewMax {
		suffix = fmt.Sprintf("…等 %d 个", len(targets))
		shown = shown[:batchDryRunPreviewMax]
	}
	parts := make([]string, 0, len(shown))
	for _, t := range shown {
		account := strings.TrimSpace(t.Username)
		if account == "" {
			account = strings.TrimSpace(t.AccountID)
		}
		parts = append(parts, strings.TrimSpace(t.CharacterName)+"/"+account)
	}
	return strings.Join(parts, "、") + suffix
}

// batchTruncatedSuffix 因上限截断时的任务消息后缀（不截断返回空串）。
//
// 为什么必须显式写出来：运营看到"清空完成"就会认为全服清干净了。
// 上限是运营自己填的，撞上限恰恰说明"还有没处理完的角色"，不说清楚就等于撒谎。
func batchTruncatedSuffix(res batchCollectResult, limit int, what string) string {
	if !res.Truncated {
		return ""
	}
	return fmt.Sprintf("；已达上限 %d，%s，请再次执行", limit, what)
}

// collectBatchTargets 收集目标。limit = 本次最多收集多少个角色（由各接口自己的 max 决定）。
//
// 收集阶段也刷新进度（Total 先按 0 显示，收完再补），因为"全服"这一档要逐个账号查角色，
// 本身就要跑一会儿。
func (s *Server) collectBatchTargets(ctx context.Context, scope string, task *batchTaskState, limit int) (batchCollectResult, error) {
	var (
		res batchCollectResult
		err error
	)
	switch scope {
	case batchScopeOnlineChars:
		res, err = s.collectOnlineTargets(ctx, limit)
	case batchScopeAllChars:
		res, err = s.collectAllCharacterTargets(ctx, task, limit)
	default:
		res, err = s.collectFirstCharacterTargets(ctx, task, limit)
	}
	if err != nil {
		return res, err
	}
	res.markTruncatedIfFull(limit)
	return res, nil
}

// ---------- 目标收集 ----------

// listOnlineAccounts 在线账号列表：优先走桥接注入的实现（主程序注入的是同一份 store 查询；
// 测试与将来换数据源也走这条路），未注入时退回认证服务连接池。
//
// 为什么留两条路：桥接少接一个闭包不该让整条批量链路不可用 —— 退回直连是"照旧能用"，
// 而不是"报错让运营去升级工具"。
func (s *Server) listOnlineAccounts(ctx context.Context, limit int) ([]store.OnlineAccount, error) {
	if s.bridge != nil && s.bridge.AcctOnlineAccounts != nil {
		return s.bridge.AcctOnlineAccounts(ctx, limit)
	}
	st := s.bridgeStore()
	if st == nil {
		return nil, errAuthStoreUnavailable
	}
	return st.ListOnlineAccounts(ctx, limit)
}

// listAccountsPage 按 account_id 升序分页取账号（同上：桥接优先，退回连接池）。
func (s *Server) listAccountsPage(ctx context.Context, offset, limit int) ([]store.Account, error) {
	if s.bridge != nil && s.bridge.AcctAccountsPage != nil {
		return s.bridge.AcctAccountsPage(ctx, offset, limit)
	}
	st := s.bridgeStore()
	if st == nil {
		return nil, errAuthStoreUnavailable
	}
	return st.ListAccountsPage(ctx, offset, limit)
}

// accountCharacters 查某个账号的角色列表（统一走 /local/dnf-gm/characters）。
//
// 依赖：游戏服 Character.ListByAccount 按**槽位升序**返回，所以 chars[0] 就是"第一个角色"。
func (s *Server) accountCharacters(ctx context.Context, accountID string) ([]gmCharacterView, error) {
	var chars []gmCharacterView
	path := "/local/dnf-gm/characters?account_id=" + url.QueryEscape(accountID)
	if _, err := s.acctGM(ctx, http.MethodGet, path, nil, &chars); err != nil {
		return nil, err
	}
	return chars, nil
}

// batchTargetUsable 角色能不能当目标：必须有角色 ID（批量只按 ID/名字定位，缺了必然失败），
// 名字空白的目标在任务详情里也认不出是谁，同样不收。
func batchTargetUsable(ch gmCharacterView) bool {
	return strings.TrimSpace(ch.CharacterID) != "" && strings.TrimSpace(ch.Name) != ""
}

// collectOnlineTargets 每个在线角色一个目标（在线判定与「在线角色表」同源）。
func (s *Server) collectOnlineTargets(ctx context.Context, limit int) (batchCollectResult, error) {
	accounts, err := s.listOnlineAccounts(ctx, limit)
	if err != nil {
		return batchCollectResult{}, err
	}
	res := batchCollectResult{Targets: make([]batchMailTarget, 0, len(accounts))}
	for _, acc := range accounts {
		if ctx.Err() != nil || len(res.Targets) >= limit {
			break
		}
		chars, err := s.accountCharacters(ctx, acc.AccountID)
		if err != nil {
			res.CollectFailed++ // 单个账号查不到不影响其他人，但要让调用方知道"有失败"
			continue
		}
		for _, ch := range chars {
			// 收件人必须有角色 ID：批量发放只按 character_id 定位（见 runBatchGrant 的注释），
			// 过滤条件也必须按同一口径，否则 ID 为空的角色会进队列、发出去必然失败。
			if !ch.Online || !batchTargetUsable(ch) {
				continue
			}
			if len(res.Targets) >= limit {
				break
			}
			res.Targets = append(res.Targets, batchMailTarget{
				AccountID: acc.AccountID, Username: acc.Username,
				CharacterID: ch.CharacterID, CharacterName: ch.Name,
			})
		}
	}
	return res, nil
}

// collectFirstCharacterTargets 每个账号的第一个角色（槽位号最小的那个）。
//
// 依赖：/local/dnf-gm/characters 返回的角色列表按**槽位升序**（游戏服 Character.ListByAccount
// 的 ORDER BY slot），所以取第 0 个就是"第一个角色"。列表为空 = 该账号还没建角色 → 跳过。
func (s *Server) collectFirstCharacterTargets(ctx context.Context, task *batchTaskState, limit int) (batchCollectResult, error) {
	res := batchCollectResult{Targets: make([]batchMailTarget, 0, batchAccountPageSize)}
	err := s.walkAccounts(ctx, task, limit, &res, func(acc store.Account, chars []gmCharacterView, res *batchCollectResult) {
		if len(chars) == 0 || !batchTargetUsable(chars[0]) {
			task.addResult(true, "") // 没角色（或没有角色 ID）→ 跳过（不计失败）
			return
		}
		res.Targets = append(res.Targets, batchMailTarget{
			AccountID: acc.AccountID, Username: acc.Username,
			CharacterID: chars[0].CharacterID, CharacterName: chars[0].Name,
		})
	})
	// 收集阶段记的 "失败/跳过" 是"账号层面"的统计，发放阶段会叠加到同一个 task 上，
	// 所以这里把 Done 归零、只保留原因文本，避免进度显示成"已处理 N/总数 N"提前满格。
	// 失败数单独回传给调用方（清零前取出）：全失败时不能报 done。
	task.resetCollectCounters()
	return res, err
}

// collectAllCharacterTargets 全服**所有角色**（每个账号的每一个角色）。
//
// 为什么单独一个收集器：批量清空邮箱的 all-characters 要求"全服邮件一封不剩"，
// 而 all-accounts 只取每账号的首角色，漏掉的正是那些多角色账号的其余角色。
// 目标集合不同就必须分开收集，不能拿首角色凑数。
func (s *Server) collectAllCharacterTargets(ctx context.Context, task *batchTaskState, limit int) (batchCollectResult, error) {
	res := batchCollectResult{Targets: make([]batchMailTarget, 0, batchAccountPageSize)}
	err := s.walkAccounts(ctx, task, limit, &res, func(acc store.Account, chars []gmCharacterView, res *batchCollectResult) {
		usable := 0
		for _, ch := range chars {
			if !batchTargetUsable(ch) {
				continue
			}
			if len(res.Targets) >= limit {
				break
			}
			usable++
			res.Targets = append(res.Targets, batchMailTarget{
				AccountID: acc.AccountID, Username: acc.Username,
				CharacterID: ch.CharacterID, CharacterName: ch.Name,
			})
		}
		if usable == 0 {
			task.addResult(true, "") // 该账号一个可用角色都没有 → 跳过（不计失败）
		}
	})
	task.resetCollectCounters()
	return res, err
}

// walkAccounts 按 account_id 升序分页遍历账号，对每个账号取角色列表后交给 perAccount 决定收谁。
//
// 两个上限都要显式管住，且撞上任何一个都必须把 res.Truncated 立起来：
//   - limit：本次要收集的目标数上限（运营填的 max）；
//   - limit 个账号：账号遍历上限（拿不到角色也照样消耗一次回环，不能无限遍历下去）。
//
// 收集不完整却报"完成"，运营就会以为全服都处理干净了 —— 这正是这个标志存在的理由。
//
// 分页查询本身失败仍然**整体报错**（与既有群发行为一致）：那说明认证服务的库/连接有问题，
// 此时"按已有目标继续发"会让运营以为全发放完了，不如直接失败让人去看库。
func (s *Server) walkAccounts(
	ctx context.Context, task *batchTaskState, limit int, res *batchCollectResult,
	perAccount func(acc store.Account, chars []gmCharacterView, res *batchCollectResult),
) error {
	for offset := 0; ; offset += batchAccountPageSize {
		if ctx.Err() != nil || len(res.Targets) >= limit {
			return nil
		}
		if offset >= limit {
			// 账号还没遍历完就到了遍历上限：剩下的账号里可能还有目标角色。
			res.Truncated = true
			return nil
		}
		accounts, err := s.listAccountsPage(ctx, offset, batchAccountPageSize)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			return nil // 账号取完了：这次收集是完整的
		}
		for _, acc := range accounts {
			if ctx.Err() != nil || len(res.Targets) >= limit {
				return nil
			}
			if strings.TrimSpace(acc.Username) == "" {
				continue
			}
			chars, err := s.accountCharacters(ctx, acc.AccountID)
			if err != nil {
				task.addResult(false, fmt.Sprintf("账号 %s：查角色失败 %v", acc.Username, err))
				res.CollectFailed++
				continue
			}
			perAccount(acc, chars, res)
		}
		if len(accounts) < batchAccountPageSize {
			return nil // 最后一页
		}
	}
}

// ---------- 任务查询 / 取消 ----------

// handleTaskGet GET /manage/v1/tasks / GET /manage/v1/tasks/{id}
func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "task-registry-unavailable")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/manage/v1/tasks")
	id = strings.Trim(strings.TrimSpace(id), "/")
	if id == "" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		writeOK(w, map[string]any{"items": s.tasks.list(limit)})
		return
	}
	task := s.tasks.get(id)
	if task == nil {
		writeErr(w, http.StatusNotFound, "task-not-found")
		return
	}
	writeOK(w, task.snapshot())
}

// handleTaskCancel POST /manage/v1/tasks/{id}/cancel
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if s.tasks == nil {
		writeErr(w, http.StatusServiceUnavailable, "task-registry-unavailable")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/manage/v1/tasks"), "/")
	id = strings.TrimSuffix(id, "/cancel")
	if strings.TrimSpace(id) == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少任务 id")
		return
	}
	if !s.tasks.cancel(id) {
		writeErr(w, http.StatusConflict, "任务不存在或已经结束")
		return
	}
	s.log.Info("batch task canceled", "task", id)
	writeOK(w, map[string]any{"canceled": true, "id": id})
}

// sortBatchTargetsByCharacterID 按角色 ID（数字）升序排列发放目标。
//
// 非数字 ID（理论上不会有）排在最后并按字符串比较：宁可顺序怪一点，也不能丢掉/合并目标。
func sortBatchTargetsByCharacterID(targets []batchMailTarget) {
	sort.SliceStable(targets, func(i, j int) bool {
		left, leftErr := strconv.ParseUint(strings.TrimSpace(targets[i].CharacterID), 10, 64)
		right, rightErr := strconv.ParseUint(strings.TrimSpace(targets[j].CharacterID), 10, 64)
		switch {
		case leftErr == nil && rightErr == nil:
			return left < right
		case leftErr == nil:
			return true
		case rightErr == nil:
			return false
		default:
			return targets[i].CharacterID < targets[j].CharacterID
		}
	})
}

// 说明：原先这里有个 runningTaskOfKind（"先查有没有同类任务在跑，再登记新任务"）。
// 它是两步式的 TOCTOU：两次点击都能在"构建 PVF 索引"那段窗口里看到"没有在跑"，
// 于是同一批发两遍。现在改成 taskRegistry.createIfNoRunning —— 查与建在同一把锁内完成。
