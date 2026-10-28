// 泡点点券 Runner（服务器端常驻循环）：
//   - 配置存 config.json pointsink 段，可由管理端 /manage/v1/pointsink 修改持久化；
//   - 每秒探测一次在线账号（store.launcher_sessions 未过期会话）；
//   - 仅对"游戏角色真正在线"（go-server /local/dnf-gm/characters 的 online=true）的账号
//     到期发放点券（/local/dnf-gm/wallet/update, currency=cera mode=add）；
//   - 发放成功才重置该账号计时；失败不重置，下个周期自动补发；
//   - 发放成功写 launcher_gm_events(event_type=cera_granted)，登录器轮询到后
//     用现有 system_notify 通道只给对应账号弹绿字（reason 即消息文案）；
//   - 按账号去重：同账号多角色在线时只给第一个在线角色发一次（点券账号级）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dnf90ops/internal/gmhttp"
	"dnf90ops/internal/server"
	"dnf90ops/internal/store"
)

const (
	psTickEvery      = time.Second
	psGMTimeout      = 8 * time.Second
	psMinIntervalSec = 10
	psMaxIntervalSec = 86400
	psMinPoints      = 1
	psMaxPoints      = 1000000
	// psGrantWorkers 一轮发放的最大并发账号数。
	//
	// 为什么是 6：每个账号至少 3 次 HTTP（查角色/查余额/加点券），而对面是游戏服务端的
	// 本地 GM 管理口（单机、SQLite/MySQL 混合读写）。并发太小等于没并发（原来就是 1），
	// 太大又会把管理口和数据库压住 —— 100 个账号一波打进去，反而个个超时。
	// 6 路并发把"后面的账号等前面的"从 O(N) 降到 O(N/6)，同时对端压力可控。
	psGrantWorkers = 6
	// psMaxDailyLimit 账号级每日限额上限（0 = 不限）。
	psMaxDailyLimit    = 100000000
	psEventCeraGranted = "cera_granted"
)

// psConf 循环使用的配置快照。
type psConf struct {
	Enabled  bool
	Interval time.Duration
	Points   int
	// DailyLimit 账号级每日限额（当前币种的数量）；0 = 不限额。
	DailyLimit int
	// Currency 发放币种（2026-10-05 起可选）："cera"=点券（默认）/ "token_cera"=代币券。
	//
	// 币种口径与服务端 /local/dnf-gm/wallet/update 一致；老配置没有这个键，
	// 归一化后一律按点券（见 rbNormalizeCurrency），行为与改动前完全一致。
	Currency string
}

// psStat 泡点状态快照（与 paid 版 manage.PointsinkStatus 字段一一对应）。
type psStat struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int    `json:"interval_seconds"`
	Points          int    `json:"points"`
	DailyLimit      int    `json:"daily_limit"`
	Currency        string `json:"currency"`
	CurrencyLabel   string `json:"currency_label"`
	Running         bool   `json:"running"`
	Awarded         int64  `json:"awarded"`
	GrantedPoints   int64  `json:"granted_points"`
	ErrorCount      int64  `json:"error_count"`
	LastGrantAt     int64  `json:"last_grant_at"`
	NextGrantAt     int64  `json:"next_grant_at"`
}

// psState 泡点运行时统计（psMu 保护）。
type psState struct {
	lastAward map[string]time.Time // account_id -> 最近一次成功发放时间
	// sessionGranted 本次登录（= 本次**连续在线**）期间累计到账点数：account_id -> 累计。
	//
	// 生命周期与 lastAward 完全一致：账号上线时建、**掉线时一并删**。
	// 所以它天然就是"本次登录共计"，重登即归零，不需要额外的时间戳判断。
	sessionGranted map[string]int64
	awarded        int64 // 成功发放账号次数
	granted        int64 // 累计发放点券
	errors         int64 // 发放失败次数（网络/服务端拒绝）
	lastGrant      time.Time
	nextDue        time.Time // 下一次到期发放时刻（仅在线账号中最早者）
	lastPrune      time.Time // 最近一次清理限额台账的时刻（每天只清一次）
}

// psGMChar go-server /characters 返回元素（仅取用字段）。
type psGMChar struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Online      bool   `json:"online"`
}

// psWalletEnvelope /wallet/update 信封。
type psWalletEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Data  struct {
		After     int64 `json:"after"`
		Online    bool  `json:"online"`
		Refreshed bool  `json:"refreshed"`
	} `json:"data"`
}

// psGMAdminClient 复用 CDK 的服务端管理口连接配置（instance.json adminListen/adminToken）。
type psGMAdminClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// startPointsinkRunner 启动泡点常驻循环（幂等）；付费版 maybeStartManage 时调用。
// 即使泡点当前未启用也启动循环，由配置开关决定是否工作。
func (a *App) startPointsinkRunner() {
	a.psOnce.Do(func() {
		a.psMu.Lock()
		a.ps.lastAward = map[string]time.Time{}
		a.ps.sessionGranted = map[string]int64{}
		a.psMu.Unlock()
		go a.pointsinkLoop()
		// GM 事件表清理：与泡点开关**无关**地常驻跑。
		// 事件表是登录器轮询的取数源，它涨爆了会拖垮整个认证服务（连接池只有 8 条），
		// 所以不能挂在"泡点开着才跑"的循环里。
		go a.gmEventPruneLoop()
	})
}

// ── GM 事件表清理（见 internal/store/gm_events_prune.go）────────────────────

const (
	// gmEventPruneEvery 清理间隔。取 10 分钟：清理是廉价的（走索引的范围删除），
	// 频率高一点可以让表规模一直贴着"保留窗口内的量"，不会出现锯齿式暴涨。
	gmEventPruneEvery = 10 * time.Minute

	// gmEventKeepEphemeral 到账提示（cera_granted）保留时长。
	//
	// 30 分钟的依据：登录器正常 10 秒内就取走；短暂断网/重连也在分钟级。
	// 留着纯粹是为了容忍抖动 —— 一条"获得 100 点券"的历史提示再久也没有价值。
	gmEventKeepEphemeral = 30 * time.Minute

	// gmEventKeepKick 封禁/踢下线等**必须送达**事件的保留时长。
	//
	// 7 天：玩家可能几天后才重新开登录器，那条"你被封了"的事件必须还在，
	// 否则他的游戏进程不会被关掉（封禁在登录时另有硬拦，但这条例事不能丢）。
	gmEventKeepKick = 7 * 24 * time.Hour
)

// gmEventPruneLoop 周期性清理 GM 事件表。
func (a *App) gmEventPruneLoop() {
	ticker := time.NewTicker(gmEventPruneEvery)
	defer ticker.Stop()
	for range ticker.C {
		a.gmEventPruneOnce()
	}
}

// gmEventPruneOnce 执行一次清理并记一行日志（删了 0 条就完全不吭声，避免刷屏）。
func (a *App) gmEventPruneOnce() {
	st := a.pointsinkStore()
	if st == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	removed, err := st.PruneGMEvents(ctx, gmEventKeepEphemeral, gmEventKeepKick)
	if err != nil {
		// 清理失败不影响任何业务（最坏就是表继续长），记一行即可，不弹窗。
		a.uiSafe(func() { a.logf("GM 事件表清理失败（不影响发放与推送）: %v", err) })
		return
	}
	if removed <= 0 {
		return
	}
	stats, statErr := st.GMEventStatsSnapshot(ctx)
	if statErr != nil {
		a.uiSafe(func() { a.logf("GM 事件表已清理 %d 条过期记录", removed) })
		return
	}
	a.uiSafe(func() {
		a.logf("GM 事件表已清理 %d 条过期记录（到账提示保留 %v、封禁类保留 %v；当前共 %d 行，其中到账提示 %d 行）",
			removed, gmEventKeepEphemeral, gmEventKeepKick, stats.Total, stats.Ephemeral)
	})
}

func (a *App) pointsinkLoop() {
	ticker := time.NewTicker(psTickEvery)
	defer ticker.Stop()
	for range ticker.C {
		a.pointsinkTick()
	}
}

func (a *App) pointsinkTick() {
	pc := a.psConf()
	rb := a.rbStatus()
	if (pc == nil || !pc.Enabled) && !rb.Enabled {
		a.psClearTimers()
		return
	}
	st := a.pointsinkStore()
	if st == nil {
		// 认证服务未运行：没有可靠在线数据源，不做任何发放。
		a.psClearTimers()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	online, err := st.ListOnlineAccounts(ctx, 100000)
	if err != nil {
		if pc != nil && pc.Enabled {
			a.uiSafe(func() { a.logf("泡点: 在线账号列表查询失败: %v", err) })
		} else {
			a.uiSafe(func() { a.logf("注册赠送: 在线账号列表查询失败: %v", err) })
		}
		return
	}
	// 注册赠送：独立于泡点开关，玩家首次上线角色后通过游戏服务端 GM 接口发放。
	if rb.Enabled {
		a.rbGrantPending(ctx, online, rb.Currency)
	}
	now := time.Now()
	onlineSet := make(map[string]bool, len(online))
	for _, acc := range online {
		onlineSet[acc.AccountID] = true
	}
	if pc != nil && pc.Enabled {
		due := map[string]time.Time{}
		a.psMu.Lock()
		// 离线账号移除计时（掉线重连自动清零重计时）。
		// 同时清掉"本次登录累计"——它与计时同生命周期，重登就该从 0 开始。
		for id := range a.ps.lastAward {
			if !onlineSet[id] {
				delete(a.ps.lastAward, id)
				delete(a.ps.sessionGranted, id)
			}
		}
		// 新在线账号建立计时起点；到期账号收集。
		for _, acc := range online {
			t, ok := a.ps.lastAward[acc.AccountID]
			if !ok {
				a.ps.lastAward[acc.AccountID] = now
				continue
			}
			if now.Sub(t) >= pc.Interval {
				due[acc.AccountID] = now
			}
		}
		a.psMu.Unlock()
		// 限额台账每天清一次过期行（保留 14 天），失败不影响发放。
		a.psPruneDaily(ctx, now.Format("2006-01-02"))
		if len(due) > 0 {
			awarded := a.pointsinkGrantBatch(ctx, due)
			if awarded > 0 {
				a.uiSafe(func() { a.logf("泡点: 本轮向 %d 个在线账号发放%s", awarded, a.psGrantCurrencyLabel()) })
			}
		}
		a.psRefreshNextDue(now, onlineSet)
	} else {
		// 泡点关闭：清空计时，避免恢复开启后沿用旧计时。
		a.psClearTimers()
	}
}

// rbGrantPending 发放注册赠送：对已有待发放记录且角色真正在线的账号，
// 复用泡点的游戏服务端 GM 接口 /local/dnf-gm/wallet/update 入账
// （币种由配置决定：点券 cera 或代币券 token_cera），
// 成功后标记已发并写 launcher_gm_events(event_type=cera_granted)。
func (a *App) rbGrantPending(ctx context.Context, online []store.OnlineAccount, currency string) {
	st := a.pointsinkStore()
	if st == nil {
		return
	}
	grantCurrency := rbNormalizeCurrency(currency)
	grantLabel := rbCurrencyLabel(grantCurrency)
	pending, err := st.ListRegisterBonusPending(ctx)
	if err != nil {
		a.uiSafe(func() { a.logf("注册赠送: 待发放列表查询失败: %v", err) })
		return
	}
	if len(pending) == 0 {
		return
	}
	onlineSet := make(map[string]bool, len(online))
	for _, acc := range online {
		onlineSet[acc.AccountID] = true
	}
	client, err := a.psAdminClient()
	if err != nil {
		a.uiSafe(func() { a.logf("注册赠送: %v", err) })
		return
	}
	for _, p := range pending {
		if !onlineSet[p.AccountID] {
			continue
		}
		char, onlineOK, err := a.psFindOnlineCharacter(ctx, client, p.AccountID)
		if err != nil {
			a.uiSafe(func() { a.logf("注册赠送: 账号 %s 角色查询失败: %v", p.AccountID, err) })
			continue
		}
		if !onlineOK || char == nil {
			continue
		}
		if _, err := a.psAddWalletCurrency(ctx, client, char.CharacterID, grantCurrency, p.Points); err != nil {
			a.uiSafe(func() { a.logf("注册赠送: 账号 %s %s发放失败: %v", p.AccountID, grantLabel, err) })
			continue
		}
		if err := st.MarkRegisterBonusGranted(ctx, p.AccountID); err != nil {
			a.uiSafe(func() {
				a.logf("注册赠送: 账号 %s 待发记录标记失败(%s已发): %v", p.AccountID, grantLabel, err)
			})
			continue
		}
		msg := fmt.Sprintf("注册赠送%d%s", p.Points, grantLabel)
		if _, err := st.CreateGMKickEvent(ctx, p.AccountID, "cera_granted", msg, "register_bonus"); err != nil {
			a.uiSafe(func() {
				a.logf("注册赠送: 账号 %s 到账事件写库失败(%s已发): %v", p.AccountID, grantLabel, err)
			})
		}
		a.uiSafe(func() { a.logf("注册赠送: 账号 %s 已发放 %d %s", p.AccountID, p.Points, grantLabel) })
	}
}

func (a *App) psClearTimers() {
	a.psMu.Lock()
	a.ps.lastAward = map[string]time.Time{}
	a.ps.nextDue = time.Time{}
	a.psMu.Unlock()
}

// pointsinkGrantBatch 对到期账号逐个真实发放；返回成功账号数。
//
// ── 为什么改成并发（2026-09-28 修"泡点不实时"）──────────────────────────────
// 原来是纯串行：每个账号至少要 3 次 HTTP 往返（查在线角色 → 查余额 → 加点券），
// 每次都带 8 秒超时。到期账号一多，"排在后面的账号"就要依次等前面的全部走完，
// 玩家看到的就是"同一个泡点周期，有人秒到、有人要等十几秒"。
//
// 这里用固定大小的 worker 池并发跑：**打满并发上限**（而不是把所有账号一次性放出去），
// 免得一次性对游戏服务端管理口打出几十个并发请求把它压住。
// 顺序按账号名排序后再发，日志顺序稳定，便于比对"谁先谁后"。
func (a *App) pointsinkGrantBatch(ctx context.Context, due map[string]time.Time) int {
	pc := a.psConf()
	if pc == nil || pc.Points <= 0 {
		return 0
	}
	client, err := a.psAdminClient()
	if err != nil {
		a.uiSafe(func() { a.logf("泡点: %v", err) })
		a.psCountError()
		return 0
	}

	accountIDs := make([]string, 0, len(due))
	for accountID := range due {
		accountIDs = append(accountIDs, accountID)
	}
	sort.Strings(accountIDs)

	today := time.Now().Format("2006-01-02")
	var success atomic.Int64
	workers := psGrantWorkers
	if workers > len(accountIDs) {
		workers = len(accountIDs)
	}
	if workers <= 0 {
		return 0
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, accountID := range accountIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(accountID string) {
			defer wg.Done()
			defer func() { <-sem }()
			if a.psGrantOneAccount(ctx, client, pc, accountID, today) {
				success.Add(1)
			}
		}(accountID)
	}
	wg.Wait()
	return int(success.Load())
}

// psGrantOneAccount 给**一个**到期账号发放一次；返回是否发放成功。
//
// 抽成独立函数是为了能被 pointsinkGrantBatch 并发调用 —— 它只碰两类共享状态：
// a.psMu 保护的计时/统计（仍按原样加锁），以及线程安全的 HTTP 客户端与 UI 日志通道。
func (a *App) psGrantOneAccount(
	ctx context.Context, client *psGMAdminClient, pc *psConf, accountID, today string,
) bool {
	char, online, err := a.psFindOnlineCharacter(ctx, client, accountID)
	if err != nil {
		a.psCountError()
		a.uiSafe(func() { a.logf("泡点: 账号 %s 角色查询失败: %v", accountID, err) })
		return false
	}
	if !online || char == nil {
		return false // 账号有会话但无真正在线角色：本轮不发。
	}
	// 账号级每日限额：先看今天已经发出去多少，再决定这一轮发多少。
	// 超额不是错误（运营设的口径），所以放过 lastAward（下一轮按期再审），只记一条日志。
	amount := pc.Points
	currency := rbNormalizeCurrency(pc.Currency)
	currencyLabel := rbCurrencyLabel(currency)
	if pc.DailyLimit > 0 {
		used, err := a.psDailyUsed(ctx, accountID, today)
		if err != nil {
			a.psCountError()
			a.uiSafe(func() {
				a.logf("泡点: 账号 %s 每日限额查询失败: %v（本轮跳过，避免超发）", accountID, err)
			})
			return false
		}
		amount = psGrantableAmount(pc.Points, pc.DailyLimit, used)
		if amount <= 0 {
			a.psMu.Lock()
			a.ps.lastAward[accountID] = time.Now() // 今天已满：按间隔再审，不再空转重试
			a.psMu.Unlock()
			a.uiSafe(func() {
				a.logf("泡点: 账号 %s 已达每日限额（今日已发 %d / 上限 %d %s），今天不再发放",
					accountID, used, pc.DailyLimit, currencyLabel)
			})
			return false
		}
	}
	granted, err := a.psGrantOnce(ctx, client, char.CharacterID, currency, amount)
	if err != nil {
		a.psCountError()
		a.uiSafe(func() { a.logf("泡点: 账号 %s %s发放失败: %v", accountID, currencyLabel, err) })
		return false
	}
	// 记账（每日限额台账）：失败只记日志，不影响"已经发出去"这个事实。
	if pc.DailyLimit > 0 {
		if err := a.psAddDailyUsed(ctx, accountID, today, int64(amount)); err != nil {
			a.uiSafe(func() {
				a.logf("泡点: 账号 %s 每日限额记账失败(%s已发，可能被多算额度): %v", accountID, currencyLabel, err)
			})
		}
	}
	// 先累计"本次登录共计"，再拼文案 —— 文案里的 a 要**包含本轮**。
	now := time.Now()
	a.psMu.Lock()
	a.ps.sessionGranted[accountID] += int64(amount)
	sessionTotal := a.ps.sessionGranted[accountID]
	a.ps.lastAward[accountID] = now
	a.ps.awarded++
	a.ps.granted += int64(amount)
	a.ps.lastGrant = now
	a.psMu.Unlock()

	// 发放成功：写账号定向 GM 事件（登录器长轮询/轮询后走 system_notify 只通知本账号）。
	// 写库成功后 store 会立刻唤醒该账号的长轮询 —— 这就是"秒级弹绿字"的来源。
	msg := psGrantMessage(psIntervalSeconds(pc.Interval), int64(pc.Points), int64(amount), sessionTotal, currency)
	if err := a.psWriteGrantEvent(ctx, accountID, msg); err != nil {
		a.uiSafe(func() { a.logf("泡点: 账号 %s 到账事件写库失败(%s已发): %v", accountID, currencyLabel, err) })
	}
	// 成功也要留一行日志（含余额变化）：线上"提示有、点券不加"这类反馈，
	// 只能靠"发放前余额 → 发放后余额"这一行才能判定到底是没发还是客户端没刷新。
	a.uiSafe(func() {
		a.logf("泡点: 账号 %s 已发放 %d %s（角色 %s，余额 %s，本次登录累计 %d）",
			accountID, amount, currencyLabel, char.CharacterID, granted.describe(), sessionTotal)
	})
	return true
}

// psIntervalSeconds 泡点间隔的秒数（给提示文案用）。异常值兜到最小 1 秒，
// 避免文案里出现"每0秒"这种明显不对劲的说法。
func psIntervalSeconds(interval time.Duration) int64 {
	seconds := int64(interval / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// psGrantMessage 组装到账提示文案（格式由需求方指定，2026-09-28）：
//
//	每x秒获得y点券，当前获得z点券，本次登录共计获得a点券。
//
//	x = 泡点间隔秒数（规则）
//	y = 每轮配置点数（规则）
//	z = 本轮**实际**到账点数 —— 账号级每日限额触发时最后一轮会被削减，
//	    所以它不一定等于 y，必须用实际发放量而不是配置值
//	a = 本次登录（= 本次连续在线）期间累计到账点数，掉线重连归零
func psGrantMessage(intervalSeconds, perRound, thisRound, sessionTotal int64, currency string) string {
	label := rbCurrencyLabel(currency)
	return fmt.Sprintf("每%d秒获得%d%s，当前获得%d%s，本次登录共计获得%d%s。",
		intervalSeconds, perRound, label, thisRound, label, sessionTotal, label)
}

// psGrantOutcome 一次发放的结果摘要（用于日志）。余额未知时留空，不猜数字。
type psGrantOutcome struct {
	before     int64
	after      int64
	hasBalance bool
	// confirmedAfterTimeout：客户端 8s 超时，但用只读查询核对确认"其实已经到账"。
	confirmedAfterTimeout bool
}

func (o psGrantOutcome) describe() string {
	if !o.hasBalance {
		return "（服务端未回显）"
	}
	text := fmt.Sprintf("%d → %d", o.before, o.after)
	if o.confirmedAfterTimeout {
		text += "，超时后核对确认已到账"
	}
	return text
}

// psGrantOnce 给一个角色加一次指定币种，并返回"发放前 → 发放后"的余额。
//
// 为什么超时要单独核对：wallet/update 是"先提交事务、再刷新在线客户端的钱包"，
// 刷新要排队进游戏会话的事件循环——会话在跑副本/网络卡顿时这个队列可能几秒到十几秒，
// 客户端 8 秒超时后就会看到"失败"，但**钱其实已经到账**（服务器最后回的是 200）。
// 如果直接按失败处理，就会 (a) 台账少记一笔（限额被绕过）、(b) 下一轮立刻重发。
// 所以这里在超时后用只读的 wallet/query 核对一次余额，能确认就按成功算。
//
// currency 必须与发放时用的一致：核对读的是**同一个币种**的余额，
// 拿点券余额去比代币券的增量会永远判定失败（台账漏记 + 下一轮重发）。
func (a *App) psGrantOnce(
	ctx context.Context, client *psGMAdminClient, characterID, currency string, amount int,
) (psGrantOutcome, error) {
	if amount <= 0 {
		return psGrantOutcome{}, fmt.Errorf("发放数量必须为正（当前 %d）", amount)
	}
	currency = rbNormalizeCurrency(currency)
	var outcome psGrantOutcome
	before, okBefore := a.psWalletBalance(ctx, client, characterID, currency)
	if okBefore {
		outcome.before, outcome.hasBalance = before, true
	}
	after, err := a.psAddWalletCurrency(ctx, client, characterID, currency, amount)
	if err != nil {
		if !psIsTimeout(err) || !okBefore {
			return outcome, err
		}
		// 超时：只读核对一次（wallet/query 不碰会话事件循环，不会被同样的卡顿拖住）。
		confirmed, okNow := a.psWalletBalance(ctx, client, characterID, currency)
		if !okNow || confirmed < before+int64(amount) {
			return outcome, err
		}
		outcome.after, outcome.hasBalance = confirmed, true
		outcome.confirmedAfterTimeout = true
		return outcome, nil
	}
	if !outcome.hasBalance {
		outcome.after = after
		outcome.hasBalance = true
	} else {
		outcome.after = after
	}
	return outcome, nil
}

// psGrantableAmount 计算本轮实际发放的点券数：
//
//	points    每轮配置数量（<=0 视为不发）
//	dailyLimit 账号级每日限额；<=0 表示不限
//	usedToday 今天该账号已经发出去的数量
//
// 返回 0 表示"今天已满，本轮不发"；否则返回 min(points, 剩余额度)——
// 最后一轮按剩余额度发，保证"每天最多不超过限额"这条口径成立（限额 1000、每轮 300、
// 已发 900 → 本轮发 100）。
func psGrantableAmount(points, dailyLimit int, usedToday int64) int {
	if points <= 0 {
		return 0
	}
	if dailyLimit <= 0 {
		return points
	}
	remaining := dailyLimit - int(usedToday)
	if remaining <= 0 {
		return 0
	}
	if remaining < points {
		return remaining
	}
	return points
}

// psIsTimeout 判断是否是"客户端等超时"类错误（这类错误不能直接当失败）。
func psIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "context deadline exceeded") ||
		strings.Contains(text, "Client.Timeout exceeded") ||
		strings.Contains(text, "i/o timeout")
}

func (a *App) psCountError() {
	a.psMu.Lock()
	a.ps.errors++
	a.psMu.Unlock()
}

// psRefreshNextDue 计算下一到期时刻（供状态展示）。
func (a *App) psRefreshNextDue(now time.Time, onlineSet map[string]bool) {
	pc := a.psConf()
	if pc == nil || !pc.Enabled {
		return
	}
	a.psMu.Lock()
	defer a.psMu.Unlock()
	var earliest time.Time
	for id, t := range a.ps.lastAward {
		if !onlineSet[id] {
			continue
		}
		due := t.Add(pc.Interval)
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	a.ps.nextDue = earliest
}

// psStatus 组装状态快照。
func (a *App) psStatus() psStat {
	pc := a.psConf()
	if pc == nil {
		return psStat{}
	}
	out := psStat{
		Enabled:         pc.Enabled,
		IntervalSeconds: int(pc.Interval / time.Second),
		Points:          pc.Points,
		DailyLimit:      pc.DailyLimit,
		Currency:        pc.Currency,
		CurrencyLabel:   rbCurrencyLabel(pc.Currency),
		Running:         a.pointsinkStore() != nil,
	}
	a.psMu.Lock()
	out.Awarded = a.ps.awarded
	out.GrantedPoints = a.ps.granted
	out.ErrorCount = a.ps.errors
	if !a.ps.lastGrant.IsZero() {
		out.LastGrantAt = a.ps.lastGrant.Unix()
	}
	if !a.ps.nextDue.IsZero() {
		out.NextGrantAt = a.ps.nextDue.Unix()
	}
	a.psMu.Unlock()
	return out
}

// psApply 应用并持久化泡点配置（interval/points/dailyLimit 硬约束由调用方与管理端共用校验）。
// dailyLimit = 0 表示不限额；>0 时是账号级"每天最多获得多少**当前币种**"。
//
// currency 为空按点券（兼容老管理端）；只放行点券 / 代币券 / 欢乐代币券三种，
// 与服务端 /local/dnf-gm/wallet/update 的口径一致。
func (a *App) psApply(
	_ context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string,
) (psStat, error) {
	if intervalSeconds < psMinIntervalSec || intervalSeconds > psMaxIntervalSec {
		return a.psStatus(), fmt.Errorf("interval 需在 %d~%d 秒", psMinIntervalSec, psMaxIntervalSec)
	}
	if points < psMinPoints || points > psMaxPoints {
		return a.psStatus(), fmt.Errorf("points 需在 %d~%d", psMinPoints, psMaxPoints)
	}
	if dailyLimit < 0 || dailyLimit > psMaxDailyLimit {
		return a.psStatus(), fmt.Errorf("每日限额需在 0（不限）~%d 之间", psMaxDailyLimit)
	}
	if !psCurrencyKnown(currency) {
		return a.psStatus(), fmt.Errorf("currency 只能是 %s（点券）/ %s（代币券）/ %s（欢乐代币券）",
			rbCurrencyCera, rbCurrencyTokenCera, rbCurrencyHappyTokenCera)
	}
	normalized := rbNormalizeCurrency(currency)
	a.psCfgMu.Lock()
	pc := a.cfg.Pointsink
	if pc == nil {
		a.psCfgMu.Unlock()
		return a.psStatus(), fmt.Errorf("泡点配置缺失")
	}
	oldEnabled := pc.Enabled
	oldInterval := pc.IntervalSeconds
	oldPoints := pc.Points
	oldLimit := pc.DailyLimit
	oldCurrency := pc.Currency
	pc.Enabled = enabled
	pc.IntervalSeconds = intervalSeconds
	pc.Points = points
	pc.DailyLimit = dailyLimit
	pc.Currency = normalized
	err := a.cfg.Save()
	if err != nil {
		pc.Enabled = oldEnabled
		pc.IntervalSeconds = oldInterval
		pc.Points = oldPoints
		pc.DailyLimit = oldLimit
		pc.Currency = oldCurrency
	}
	a.psCfgMu.Unlock()
	if err != nil {
		return a.psStatus(), fmt.Errorf("配置持久化失败: %w", err)
	}
	// 配置变更后清空既有计时，按新配置重新起算，行为可预期。
	a.psClearTimers()
	state := "关闭"
	if enabled {
		state = "开启"
	}
	label := rbCurrencyLabel(normalized)
	a.uiSafe(func() {
		if dailyLimit > 0 {
			a.logf("泡点配置已更新: %s，每 %d 秒 %d %s，账号每日上限 %d %s",
				state, intervalSeconds, points, label, dailyLimit, label)
			return
		}
		a.logf("泡点配置已更新: %s，每 %d 秒 %d %s，账号每日不限量", state, intervalSeconds, points, label)
	})
	return a.psStatus(), nil
}

// psCurrencyKnown 放行三种钱包币种（含中文别名，便于手工填配置）。
// 空值也放行 —— 语义是"没指定"，由 rbNormalizeCurrency 落到点券，兼容老管理端。
func psCurrencyKnown(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", rbCurrencyCera, "点券",
		rbCurrencyTokenCera, "token", "代币券",
		rbCurrencyHappyTokenCera, "happy_token", "happy", "欢乐代币券":
		return true
	default:
		return false
	}
}

// ---------- 小工具 ----------

// psPublicInfo 返回泡点公开信息（登录器启动自检用；线程安全）。
func (a *App) psPublicInfo() server.PointsinkPublic {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	var out server.PointsinkPublic
	if a.cfg != nil && a.cfg.Pointsink != nil {
		out.Enabled = a.cfg.Pointsink.Enabled
		out.IntervalSeconds = a.cfg.Pointsink.IntervalSeconds
		out.Points = a.cfg.Pointsink.Points
	}
	return out
}

// psGrantCurrencyLabel 当前配置的发放币种中文名。
//
// 给"本轮向 N 个账号发放 X"这类**与具体账号无关**的汇总日志用：
// 那种地方手里没有 psConf 快照，又不值得为此多持一把配置锁。
func (a *App) psGrantCurrencyLabel() string {
	if pc := a.psConf(); pc != nil {
		return rbCurrencyLabel(pc.Currency)
	}
	return rbCurrencyLabel(rbCurrencyCera)
}

// psConf 返回归一化后的配置快照（非法值就近收敛）。
func (a *App) psConf() *psConf {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil || a.cfg.Pointsink == nil {
		return nil
	}
	c := a.cfg.Pointsink
	iv := c.IntervalSeconds
	if iv < psMinIntervalSec {
		iv = psMinIntervalSec
	}
	if iv > psMaxIntervalSec {
		iv = psMaxIntervalSec
	}
	pts := c.Points
	if pts < psMinPoints {
		pts = psMinPoints
	}
	if pts > psMaxPoints {
		pts = psMaxPoints
	}
	limit := c.DailyLimit
	if limit < 0 {
		limit = 0
	}
	if limit > psMaxDailyLimit {
		limit = psMaxDailyLimit
	}
	return &psConf{
		Enabled:    c.Enabled,
		Interval:   time.Duration(iv) * time.Second,
		Points:     pts,
		DailyLimit: limit,
		Currency:   rbNormalizeCurrency(c.Currency),
	}
}

// pointsinkStore 泡点依赖的在线数据源（认证服务连接池）。
func (a *App) pointsinkStore() *store.Store {
	if !a.svcOn.Load() || a.st == nil {
		return nil
	}
	return a.st
}

// psAdminClient 由 instance.json 管理口构造 GM 客户端。
func (a *App) psAdminClient() (*psGMAdminClient, error) {
	baseURL, token, err := a.cdkGmAdminConfig()
	if err != nil {
		return nil, err
	}
	return &psGMAdminClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: psGMTimeout},
	}, nil
}

// psFindOnlineCharacter 查询账号下的在线角色；无在线角色返回 (nil,false,nil)。
func (a *App) psFindOnlineCharacter(ctx context.Context, c *psGMAdminClient, accountID string) (*psGMChar, bool, error) {
	url := c.baseURL + "/local/dnf-gm/characters?account_id=" + accountID
	body, err := a.psGet(ctx, c, url)
	if err != nil {
		return nil, false, err
	}
	var env struct {
		OK    bool       `json:"ok"`
		Error string     `json:"error"`
		Data  []psGMChar `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false, err
	}
	if !env.OK {
		return nil, false, fmt.Errorf("服务端拒绝角色查询: %s", strings.TrimSpace(env.Error))
	}
	for i := range env.Data {
		if env.Data[i].Online {
			ch := env.Data[i]
			return &ch, true, nil
		}
	}
	return nil, false, nil
}

// psWalletEnvelopeQuery /wallet/query 信封（只读查询用：三种钱包币种的余额）。
//
// 三个字段都要解析：发放币种可配置（点券 / 代币券），核对时必须读**对应币种**的余额，
// 否则"超时后核对"这段在发代币券时会永远判定失败 → 台账漏记 + 下一轮重发。
type psWalletEnvelopeQuery struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Data  struct {
		Cera           int64 `json:"cera"`
		TokenCera      int64 `json:"token_cera"`
		HappyTokenCera int64 `json:"happy_token_cera"`
		Gold           int64 `json:"gold"`
		After          int64 `json:"after"`
	} `json:"data"`
}

// psWalletBalance 只读查询角色所属账号的**指定币种**当前余额。
//
// 用途：① 发放前取"基准余额"；② 客户端超时后用它与基准对比，判断那一笔到底有没有到账。
// 之所以可以用它做判定：/local/dnf-gm/wallet/query 是纯仓储读，不经过游戏会话的事件循环，
// 因此不会被"会话卡顿"拖到超时。
func (a *App) psWalletBalance(ctx context.Context, c *psGMAdminClient, characterID, currency string) (int64, bool) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return 0, false
	}
	currency = rbNormalizeCurrency(currency)
	payload, err := json.Marshal(map[string]any{"character_id": characterID, "currency": currency})
	if err != nil {
		return 0, false
	}
	respBody, err := a.psPost(ctx, c, c.baseURL+"/local/dnf-gm/wallet/query", payload)
	if err != nil {
		return 0, false
	}
	var env psWalletEnvelopeQuery
	if err := json.Unmarshal(respBody, &env); err != nil || !env.OK {
		return 0, false
	}
	switch currency {
	case rbCurrencyTokenCera:
		return env.Data.TokenCera, true
	case rbCurrencyHappyTokenCera:
		return env.Data.HappyTokenCera, true
	default:
		return env.Data.Cera, true
	}
}

// psDailyUsed 读账号今天已发出的泡点点券（无记录 = 0；数据源不可用时返回错误让调用方跳过）。
func (a *App) psDailyUsed(ctx context.Context, accountID, day string) (int64, error) {
	st := a.pointsinkStore()
	if st == nil {
		return 0, errors.New("认证服务未运行")
	}
	return st.PointsinkDailyGranted(ctx, accountID, day)
}

// psAddDailyUsed 给账号当天累加已发出的泡点点券（限额台账）。
func (a *App) psAddDailyUsed(ctx context.Context, accountID, day string, delta int64) error {
	st := a.pointsinkStore()
	if st == nil {
		return errors.New("认证服务未运行")
	}
	return st.AddPointsinkDailyGranted(ctx, accountID, day, delta)
}

// psPruneDaily 清理过期的限额台账（每天最多做一次，失败只记日志）。
func (a *App) psPruneDaily(ctx context.Context, today string) {
	a.psMu.Lock()
	last := a.ps.lastPrune
	if !last.IsZero() && last.Format("2006-01-02") == today {
		a.psMu.Unlock()
		return
	}
	a.ps.lastPrune = time.Now()
	a.psMu.Unlock()
	st := a.pointsinkStore()
	if st == nil {
		return
	}
	if removed, err := st.PrunePointsinkDaily(ctx, today, 0); err != nil {
		a.uiSafe(func() { a.logf("泡点: 清理过期限额台账失败（不影响发放）: %v", err) })
	} else if removed > 0 {
		a.uiSafe(func() { a.logf("泡点: 已清理 %d 条过期限额台账", removed) })
	}
}

// psAddCera 回环给角色加 Cera（账号级点券），返回变动后余额。
func (a *App) psAddCera(ctx context.Context, c *psGMAdminClient, characterID string, amount int) (int64, error) {
	return a.psAddWalletCurrency(ctx, c, characterID, rbCurrencyCera, amount)
}

// psAddWalletCurrency 回环给角色加任意钱包币种（点券 / 代币券），返回变动后余额。
//
// 币种口径与服务端 /local/dnf-gm/wallet/update 一致：cera=点券、token_cera=代币券
// （account_token_cera）。注册赠送可选币种就是靠这个参数区分的。
func (a *App) psAddWalletCurrency(
	ctx context.Context, c *psGMAdminClient, characterID, currency string, amount int,
) (int64, error) {
	payload := map[string]any{
		"character_id": characterID,
		"currency":     rbNormalizeCurrency(currency),
		"mode":         "add",
		"amount":       amount,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	respBody, err := a.psPost(ctx, c, c.baseURL+"/local/dnf-gm/wallet/update", raw)
	if err != nil {
		return 0, err
	}
	var env psWalletEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return 0, err
	}
	if !env.OK {
		return 0, fmt.Errorf("服务端拒绝发放: %s", strings.TrimSpace(env.Error))
	}
	return env.Data.After, nil
}

// psWriteGrantEvent 写账号定向到账事件。
func (a *App) psWriteGrantEvent(ctx context.Context, accountID, msg string) error {
	st := a.pointsinkStore()
	if st == nil {
		return fmt.Errorf("认证服务未运行")
	}
	_, err := st.CreateGMKickEvent(ctx, accountID, psEventCeraGranted, msg, "pointsink")
	return err
}

// psGet / psPost 简单回环请求，返回响应体（2xx）。
func (a *App) psGet(ctx context.Context, c *psGMAdminClient, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Admin-Token", c.token)
	req.Header.Set("X-Admin-Actor", "ops")
	gmhttp.SetConfirm(req, "ops")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GM 接口 HTTP %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), nil
}

func (a *App) psPost(ctx context.Context, c *psGMAdminClient, url string, raw []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", c.token)
	req.Header.Set("X-Admin-Actor", "ops")
	gmhttp.SetConfirm(req, "ops")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GM 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GM 接口 HTTP %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), nil
}
