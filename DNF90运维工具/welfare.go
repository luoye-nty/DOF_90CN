package main

// 在线福利：预制奖励（道具 / 点券 / 黑钻）+ 每日 / 每周 / 每月定时向**在线玩家**发放，
// 同时按自定义文案发一条全服公告告知玩家。
//
// 三条关键约束（都来自需求确认）：
//  1. 同一档期**只发一次**：周期键（daily=2026-09-24 / weekly=2026-W39 / monthly=2026-09）
//     记在 config.json 的 last_runs 里 —— 运维工具重启后不能把当天已经发过的福利再发一遍；
//  2. **错过不补发**：工具当时没开/服务器在维护，超过容忍窗口（默认 5 分钟）就只记一次日志并
//     把该周期标记为已处理，绝不事后突然补一堆邮件；
//  3. 发放粒度：道具按**在线角色**各发一封（同一账号双开两个角色都收到）；
//     点券/黑钻是账号级权益，按**账号去重**只发一次（否则双开会翻倍）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/pvfindex"
)

// 奖励类型。
const (
	welfareKindItem         = "item"
	welfareKindCera         = "cera"
	welfareKindBlackDiamond = "black_diamond"
)

// 档期类型与周期键前缀。
const (
	welfareDaily   = "daily"
	welfareWeekly  = "weekly"
	welfareMonthly = "monthly"
)

const (
	// welfareTickEvery 调度扫描间隔：30 秒足够准（容忍窗口 5 分钟）。
	welfareTickEvery = 30 * time.Second
	// welfareTolerance 到点后多久之内算"该发"；超过就当错过了（不补发）。
	welfareTolerance = 5 * time.Minute
	// welfareMaxTargets 单次发放最多处理多少个在线账号（防止异常数据把循环拖死）。
	welfareMaxTargets = 2000
)

// 配置类型在 internal/config 里（config 包不能反向依赖主程序）；这里起别名方便本文件使用。
type (
	WelfareReward = config.WelfareReward
	WelfareSlot   = config.WelfareSlot
	WelfareConfig = config.WelfareConfig
)
// welfareDecision 一次调度判定结果（通用引擎 scheduleDecision 的别名，便于单测引用）。
type welfareDecision = scheduleDecision

// welfareSpec 把档期配置映射成通用周期描述（时间判定的实现在 schedule.go）。
func welfareSpec(kind string, slot WelfareSlot) scheduleSpec {
	mode := scheduleModeDaily
	switch kind {
	case welfareWeekly:
		mode = scheduleModeWeekly
	case welfareMonthly:
		mode = scheduleModeMonthly
	}
	return scheduleSpec{Mode: mode, Time: slot.Time, Weekday: slot.Weekday, Day: slot.Day}
}

// welfareParseClock 解析 "HH:MM"（保留给单测与界面复用，实现见 schedule.go）。
func welfareParseClock(raw string) (hour, minute int, ok bool) { return parseClock(raw) }

// welfarePeriodKey 周期键（保留给单测；实现见 schedule.go）。
func welfarePeriodKey(kind string, now time.Time) string {
	return schedulePeriodKey(welfareSpec(kind, WelfareSlot{}), now)
}

// welfareDueAt 该档期在 now 所属周期内的计划时刻（实现见 schedule.go）。
func welfareDueAt(kind string, slot WelfareSlot, now time.Time) (time.Time, bool) {
	return scheduleDueAt(welfareSpec(kind, slot), now)
}

// welfareDecide 判断此刻该不该发放。
//
// 这里只做"福利特有"的前置判断（是否启用、有没有配奖励），时间与"错过不补发"那套规则
// 一律交给 schedule.go 的通用引擎 —— 定时公告与在线福利共用一份实现，
// 免得两处各写一遍时间逻辑、日后出现"福利不补发、公告却补发"的不一致。
func welfareDecide(kind string, slot WelfareSlot, now time.Time, lastRun string, tolerance time.Duration) welfareDecision {
	spec := welfareSpec(kind, slot)
	if !slot.Enabled {
		return welfareDecision{Period: schedulePeriodKey(spec, now), Reason: "档期未启用"}
	}
	if len(slot.Rewards) == 0 {
		return welfareDecision{Period: schedulePeriodKey(spec, now), Reason: "没有配置奖励"}
	}
	return scheduleDecide(spec, now, lastRun, tolerance)
}

// ---------------- App 侧：配置读写 + 常驻循环 ----------------

// welfareSlotTitle 档期中文名。
func welfareSlotTitle(kind string) string {
	switch kind {
	case welfareWeekly:
		return "每周福利"
	case welfareMonthly:
		return "每月福利"
	default:
		return "每日福利"
	}
}

// welfareConf 读取配置副本（不在锁内做任何耗时动作）。
func (a *App) welfareConf() WelfareConfig {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil || a.cfg.Welfare == nil {
		return WelfareConfig{}
	}
	cp := *a.cfg.Welfare
	cp.LastRuns = map[string]string{}
	for k, v := range a.cfg.Welfare.LastRuns {
		cp.LastRuns[k] = v
	}
	return cp
}

// welfareApplySlot 只应用**一个档期**（管理工具的「保存本档期」）。
//
// 为什么要单独提供：整批保存会把三个档期一起校验，只要另一个档期"已启用但没奖励"
// （例如上一轮残留的配置），运营就永远存不进他想改的那一档 —— 线上真实反馈。
// 这里只校验并写入目标档期，其余档期原样保留。
func (a *App) welfareApplySlot(_ context.Context, kind string, slot WelfareSlot) (WelfareConfig, error) {
	if a == nil || a.cfg == nil {
		return WelfareConfig{}, fmt.Errorf("配置尚未加载，无法应用在线福利")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case welfareDaily, welfareWeekly, welfareMonthly:
	default:
		return a.welfareConf(), fmt.Errorf("档期只能是 daily / weekly / monthly")
	}
	if err := welfareValidateSlot(kind, slot); err != nil {
		return a.welfareConf(), err
	}

	a.psCfgMu.Lock()
	old := a.cfg.Welfare
	base := WelfareConfig{}
	if old != nil {
		base = *old
	}
	switch kind {
	case welfareWeekly:
		base.Weekly = slot
	case welfareMonthly:
		base.Monthly = slot
	default:
		base.Daily = slot
	}
	// last_runs 保持（保存配置不该让"今天已经发过"的记录消失，否则会重发一遍）。
	if base.LastRuns == nil {
		base.LastRuns = map[string]string{}
	}
	// 整组替换 + 落盘放进配置写锁（cfg.Mutate）：既避免与 Save 的 marshal 抢同一份
	// Config（map/slice 并发访问会崩进程），又保留"落盘失败就回滚内存"的语义。
	err := a.cfg.Mutate(func() { a.cfg.Welfare = &base }, func() { a.cfg.Welfare = old })
	a.psCfgMu.Unlock()
	if err != nil {
		return a.welfareConf(), fmt.Errorf("配置持久化失败: %w", err)
	}
	a.logf("在线福利·%s 配置已更新：%s", welfareSlotTitle(kind), welfareSlotSummary(slot))
	return a.welfareConf(), nil
}

// welfareValidateSlot 校验单个档期（整批保存与分档保存共用同一套规则）。
func welfareValidateSlot(kind string, slot WelfareSlot) error {
	if !slot.Enabled {
		// 停用的档期只做"奖励能解析"的体检：留着坏奖励会在下次启用时炸。
		for _, r := range slot.Rewards {
			if _, err := welfareValidateReward(r); err != nil {
				return fmt.Errorf("%s：%w", welfareSlotTitle(kind), err)
			}
		}
		return nil
	}
	if _, _, ok := welfareParseClock(slot.Time); !ok {
		return fmt.Errorf("%s 的时间格式必须是 HH:MM（当前 %q）", welfareSlotTitle(kind), slot.Time)
	}
	if kind == welfareWeekly && (slot.Weekday < 1 || slot.Weekday > 7) {
		return fmt.Errorf("每周福利必须指定星期几（1=周一 … 7=周日）")
	}
	if kind == welfareMonthly && (slot.Day < 1 || slot.Day > 28) {
		return fmt.Errorf("每月福利的日期必须在 1~28 之间（避免月末不存在的日期）")
	}
	if len(slot.Rewards) == 0 {
		return fmt.Errorf("%s 已启用但没有任何奖励", welfareSlotTitle(kind))
	}
	for _, r := range slot.Rewards {
		if _, err := welfareValidateReward(r); err != nil {
			return fmt.Errorf("%s：%w", welfareSlotTitle(kind), err)
		}
	}
	return nil
}

// welfareApply 应用并持久化在线福利配置（失败整体回滚）。
func (a *App) welfareApply(_ context.Context, in WelfareConfig) (WelfareConfig, error) {
	if a == nil || a.cfg == nil {
		return WelfareConfig{}, fmt.Errorf("配置尚未加载，无法应用在线福利")
	}
	// 校验：每个启用档期的时间/星期/日 必须合法，奖励必须能解析。
	for _, kind := range []string{welfareDaily, welfareWeekly, welfareMonthly} {
		if err := welfareValidateSlot(kind, welfareSlotOf(in, kind)); err != nil {
			return a.welfareConf(), err
		}
	}

	a.psCfgMu.Lock()
	old := a.cfg.Welfare
	// 保留已有的 last_runs（配置下发不应把"已发过"的记录清掉）。
	next := in
	next.LastRuns = map[string]string{}
	if old != nil {
		for k, v := range old.LastRuns {
			next.LastRuns[k] = v
		}
	}
	// 同上：替换 Welfare 指针 + 落盘在同一把配置写锁内完成（失败回滚内存）。
	err := a.cfg.Mutate(func() { a.cfg.Welfare = &next }, func() { a.cfg.Welfare = old })
	a.psCfgMu.Unlock()
	if err != nil {
		return a.welfareConf(), fmt.Errorf("配置持久化失败: %w", err)
	}
	a.uiSafe(func() {
		a.logf("在线福利配置已更新：每日%s，每周%s，每月%s",
			welfareSlotSummary(in.Daily), welfareSlotSummary(in.Weekly), welfareSlotSummary(in.Monthly))
	})
	return a.welfareConf(), nil
}

// welfareSlotOf 取某个档期的配置。
func welfareSlotOf(cfg WelfareConfig, kind string) WelfareSlot {
	switch kind {
	case welfareWeekly:
		return cfg.Weekly
	case welfareMonthly:
		return cfg.Monthly
	default:
		return cfg.Daily
	}
}

// welfareSlotSummary 一句话摘要（日志用）。
func welfareSlotSummary(slot WelfareSlot) string {
	if !slot.Enabled {
		return "未启用"
	}
	return fmt.Sprintf("%s×%d项", slot.Time, len(slot.Rewards))
}

// welfareValidateReward 校验单条奖励。
func welfareValidateReward(r WelfareReward) (WelfareReward, error) {
	r.Kind = strings.TrimSpace(r.Kind)
	switch r.Kind {
	case welfareKindItem:
		if r.ItemID == 0 {
			return r, fmt.Errorf("道具奖励必须填 item_id")
		}
		if r.Amount == 0 {
			r.Amount = 1
		}
		if r.ReinforceLevel < 0 || r.AmplifyLevel < 0 || r.AmplifyType < 0 {
			return r, fmt.Errorf("道具强化/增幅参数不能为负")
		}
	case welfareKindCera:
		if r.Amount == 0 || r.Amount > 100_000_000 {
			return r, fmt.Errorf("点券数量必须在 1~100000000 之间")
		}
	case welfareKindBlackDiamond:
		if r.Amount == 0 || r.Amount > 3650 {
			return r, fmt.Errorf("黑钻天数必须在 1~3650 之间")
		}
	default:
		return r, fmt.Errorf("未知奖励类型 %q（只能是 item / cera / black_diamond）", r.Kind)
	}
	return r, nil
}

// welfareMarkRun 把某档期的周期标记为已处理（落盘）。
//
// 改 map + 落盘必须整体放进 cfg.Mutate（配置写锁）：Save 会 json.Marshal 整份配置，
// 与"往 Welfare.LastRuns 里塞键"并发时 Go 会直接 fatal error: concurrent map read and map write。
func (a *App) welfareMarkRun(kind, period string) {
	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil {
		return
	}
	_ = a.cfg.Mutate(func() {
		if a.cfg.Welfare == nil {
			a.cfg.Welfare = &WelfareConfig{}
		}
		if a.cfg.Welfare.LastRuns == nil {
			a.cfg.Welfare.LastRuns = map[string]string{}
		}
		a.cfg.Welfare.LastRuns[kind] = period
	}, nil)
}

// startScheduler 启动常驻调度循环（幂等）：在线福利 + 定时公告共用同一个 30 秒 ticker。
// 与泡点一样：即使当前没启用也把循环挂在后台（30 秒一次，代价可忽略），
// 用户一开启就立刻生效，不需要重启工具。
func (a *App) startScheduler() {
	a.wfOnce.Do(func() { go a.schedulerLoop() })
}

func (a *App) schedulerLoop() {
	ticker := time.NewTicker(welfareTickEvery)
	defer ticker.Stop()
	for range ticker.C {
		a.welfareTick()
		// 定时公告（E）：到点自动发全服公告，与在线福利同一套周期判定规则。
		a.announceScheduleTick()
	}
}

// welfareTick 扫描三个档期，该发的发。
func (a *App) welfareTick() {
	cfg := a.welfareConf()
	now := time.Now()
	for _, kind := range []string{welfareDaily, welfareWeekly, welfareMonthly} {
		slot := welfareSlotOf(cfg, kind)
		if !slot.Enabled {
			continue
		}
		dec := welfareDecide(kind, slot, now, cfg.LastRuns[kind], welfareTolerance)
		if !dec.Run {
			if dec.Mark {
				// 已经处理过 / 错过窗口：记一次日志并标记，避免每分钟重复判定。
				if cfg.LastRuns[kind] != dec.Period {
					a.welfareMarkRun(kind, dec.Period)
					a.uiSafe(func() { a.logf("%s：%s", welfareSlotTitle(kind), dec.Reason) })
				}
			}
			continue
		}
		if !a.welfareSkipRetryDue(kind) {
			// 上一次前置失败还在冷却里：这次既不重试也不打日志（否则 30 秒刷一行）。
			continue
		}
		res := a.welfareRunOnce(context.Background(), kind, slot, "定时发放")
		// ── 只有"真的跑过一遍"才标记本期已发（2026-10-05 修）─────────────────────
		// 改造前这里是无条件 `a.welfareMarkRun(kind, dec.Period)`，于是**任何一次前置失败
		// 都会被记成"今天已经发过了"**：认证服务没起来、在线列表查询超时、
		// 甚至运营把一个 9 位道具 ID 填错（这个 bug 见管理工具 welfare_admin_tab.go）
		// —— 结果就是那一档福利当天再也不发，而且只有一行日志、不再重试。
		//
		// 现在：没真正执行（res.Skipped）就不标记，改为限流重试（见 welfareSkipRetryDue）。
		if res.Skipped {
			a.welfareNoteSkipped(kind, dec.Period)
			continue
		}
		a.welfareMarkRun(kind, dec.Period)
	}
}

// welfareRunResult 一次发放的结果摘要（日志与"立即试发"共用）。
type welfareRunResult struct {
	Accounts    int
	Characters  int
	ItemMails   int
	CeraGranted int
	BlackGrant  int
	Failed      int
	NoticeSent  bool
	// Skipped 本次**没有真正执行**（前置条件不满足：认证服务没起来 / 配置非法 /
	// 附件解析失败 / 在线列表查询失败 / 当前没有在线玩家）。
	//
	// 调用方据此决定"要不要把本期标记成已发"：Skipped 时**不能**标记 ——
	// 否则一次瞬时故障就吃掉当天整档福利，而且不会重试。见 welfareTick。
	Skipped bool
}

// welfareSkipRetryEvery 前置失败后的重试间隔。
//
// 为什么要限流：调度循环是 30 秒一跳，如果失败就重试而不加限制，
// 一行"认证服务未运行"会每 30 秒刷一次日志（而它可能持续几小时）。
// 5 分钟既保证"服务恢复后几分钟内就补发"，又不会刷屏。
const welfareSkipRetryEvery = 5 * time.Minute

// welfareNoteSkipped 记录一次"前置失败、本期未发"，并按限流决定下次最早何时重试。
//
// 与旧的"直接标记已发"相比，语义差别很大：
//   - 旧：这一天再也不发了（哪怕故障 1 分钟后就恢复）；
//   - 新：最多等 5 分钟就重试，直到真正发出去或错过窗口。
func (a *App) welfareNoteSkipped(kind, period string) {
	a.wfSkipMu.Lock()
	if a.wfSkipUntil == nil {
		a.wfSkipUntil = make(map[string]time.Time)
	}
	a.wfSkipUntil[kind] = time.Now().Add(welfareSkipRetryEvery)
	a.wfSkipMu.Unlock()
}

// welfareSkipRetryDue 该档期是否到了可以再试的时刻（从未失败过 = 可以试）。
func (a *App) welfareSkipRetryDue(kind string) bool {
	a.wfSkipMu.Lock()
	defer a.wfSkipMu.Unlock()
	until, ok := a.wfSkipUntil[kind]
	return !ok || !time.Now().Before(until)
}

// welfareRunOnce 执行一次发放：在线角色发道具邮件、按账号发点券/黑钻、发公告。
//
// 发放粒度（需求确认过的口径）：
//   - 道具：每个**在线角色**一封（附件是所有道具奖励）；
//   - 点券/黑钻：**按账号去重**，用该账号第一个在线角色作为载体（账号级权益，双开不该翻倍）。
func (a *App) welfareRunOnce(ctx context.Context, kind string, slot WelfareSlot, trigger string) welfareRunResult {
	var res welfareRunResult
	st := a.pointsinkStore()
	if st == nil {
		a.uiSafe(func() { a.logf("%s：认证服务未运行，跳过本次发放", welfareSlotTitle(kind)) })
		res.Skipped = true
		return res
	}
	title := welfareSlotTitle(kind)

	// 奖励分类。
	var itemRewards, ceraRewards, diamondRewards []WelfareReward
	for _, r := range slot.Rewards {
		valid, err := welfareValidateReward(r)
		if err != nil {
			a.uiSafe(func() { a.logf("%s：奖励配置非法，跳过：%v", title, err) })
			res.Skipped = true
			return res
		}
		switch valid.Kind {
		case welfareKindItem:
			itemRewards = append(itemRewards, valid)
		case welfareKindCera:
			ceraRewards = append(ceraRewards, valid)
		case welfareKindBlackDiamond:
			diamondRewards = append(diamondRewards, valid)
		}
	}

	// 道具附件只解析一次（本地 PVF 索引，校验 item_id 是否存在）。
	attachments := make([]acctMailAttachment, 0, len(itemRewards))
	itemNames := make([]string, 0, len(itemRewards))
	if len(itemRewards) > 0 {
		atts, names, err := a.welfareItemAttachments(ctx, itemRewards)
		if err != nil {
			a.uiSafe(func() { a.logf("%s：道具附件解析失败，本次不发：%v", title, err) })
			res.Skipped = true
			return res
		}
		attachments, itemNames = atts, names
	}

	online, err := st.ListOnlineAccounts(ctx, welfareMaxTargets)
	if err != nil {
		a.uiSafe(func() { a.logf("%s：在线账号列表查询失败，本次不发：%v", title, err) })
		res.Skipped = true
		return res
	}
	res.Accounts = len(online)
	if len(online) == 0 {
		a.uiSafe(func() { a.logf("%s：当前没有在线玩家，本次不发（%s）", title, trigger) })
		res.Skipped = true
		return res
	}

	mailTitle := strings.TrimSpace(slot.Subject)
	if mailTitle == "" {
		mailTitle = "在线福利"
	}
	body := welfareMailBody(title, trigger, itemNames, ceraRewards, diamondRewards)

	// 每个在线账号：查该账号的在线角色。
	for _, acc := range online {
		if ctx.Err() != nil {
			break
		}
		chars, _, err := a.acctCharacters(ctx, acc.Username, "")
		if err != nil {
			res.Failed++
			continue
		}
		var onlineChars []acctCharacterView
		for _, ch := range chars {
			if ch.Online && strings.TrimSpace(ch.Name) != "" {
				onlineChars = append(onlineChars, ch)
			}
		}
		if len(onlineChars) == 0 {
			continue
		}
		res.Characters += len(onlineChars)

		// ① 道具：每个在线角色一封（同一账号双开都收到）。
		for _, ch := range onlineChars {
			if len(attachments) == 0 {
				break
			}
			req := acctMailRequest{
				CharacterName: ch.Name,
				Title:         mailTitle,
				Body:          body,
				Attachments:   attachments,
			}
			if _, _, err := a.acctMailSend(ctx, req); err != nil {
				res.Failed++
				a.uiSafe(func() { a.logf("%s：给 %s 发道具邮件失败：%v", title, ch.Name, err) })
				continue
			}
			res.ItemMails++
		}

		// ② 点券 / 黑钻：账号级，取第一个在线角色做载体，只发一次。
		carrier := onlineChars[0]
		for _, r := range ceraRewards {
			if _, _, err := a.acctWalletUpdate(ctx, carrier.Name, "cera", "add", int64(r.Amount)); err != nil {
				res.Failed++
				a.uiSafe(func() { a.logf("%s：给账号 %s 加点券失败：%v", title, acc.Username, err) })
				continue
			}
			res.CeraGranted++
		}
		for _, r := range diamondRewards {
			if _, _, err := a.acctBlackDiamondActivate(ctx, carrier.CharacterID, int(r.Amount)); err != nil {
				res.Failed++
				a.uiSafe(func() { a.logf("%s：给账号 %s 开黑钻失败：%v", title, acc.Username, err) })
				continue
			}
			res.BlackGrant++
		}
	}

	// ③ 公告（自定义文案；留空则不发）。
	if notice := strings.TrimSpace(slot.Notice); notice != "" {
		if a.welfarePublishNotice(ctx, notice) {
			res.NoticeSent = true
		}
	}

	a.uiSafe(func() {
		a.logf("%s（%s）：在线账号 %d，在线角色 %d，道具邮件 %d，点券 %d 笔，黑钻 %d 笔，失败 %d，公告 %v",
			title, trigger, res.Accounts, res.Characters, res.ItemMails, res.CeraGranted, res.BlackGrant, res.Failed, res.NoticeSent)
	})
	return res
}

// welfareItemAttachments 把道具奖励解析成邮件附件（本地 PVF 校验 + 名称收集）。
func (a *App) welfareItemAttachments(ctx context.Context, rewards []WelfareReward) ([]acctMailAttachment, []string, error) {
	pvfPath, err := a.acctPVFPathAbs()
	if err != nil {
		return nil, nil, err
	}
	catalog, err := pvfindex.Ensure(ctx, pvfPath, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("构建本地 PVF 索引失败: %w", err)
	}
	atts := make([]acctMailAttachment, 0, len(rewards))
	names := make([]string, 0, len(rewards))
	for _, r := range rewards {
		def, ok := catalog.Resolve(r.ItemID)
		if !ok {
			return nil, nil, fmt.Errorf("本地 PVF 无 item_id=%d，请核对两端 PVF 目录一致性", r.ItemID)
		}
		def, err := pvfindex.WithUpgrade(def, r.ReinforceLevel, r.AmplifyLevel, r.AmplifyType)
		if err != nil {
			return nil, nil, err
		}
		if err := pvfindex.Validate(def); err != nil {
			return nil, nil, fmt.Errorf("PVF 物品定义不完整: %w", err)
		}
		if (r.ReinforceLevel > 0 || r.AmplifyLevel > 0 || r.AmplifyType > 0) && !def.SupportsUpgrade {
			return nil, nil, fmt.Errorf("物品 %s（%d）不支持强化/增幅", def.Name, def.ItemID)
		}
		atts = append(atts, acctMailAttachment{
			ItemID: def.ItemID, Count: r.Amount,
			ReinforceLevel: r.ReinforceLevel, AmplifyLevel: r.AmplifyLevel, AmplifyType: r.AmplifyType,
		})
		names = append(names, def.Name)
	}
	return atts, names, nil
}

// welfareMailBody 邮件正文（把这次发的东西写清楚）。
func welfareMailBody(title, trigger string, itemNames []string, cera, diamond []WelfareReward) string {
	var b strings.Builder
	b.WriteString(title)
	if strings.TrimSpace(trigger) != "" {
		b.WriteString("（" + trigger + "）")
	}
	b.WriteString("\n")
	for i, name := range itemNames {
		fmt.Fprintf(&b, "· %s\n", name)
		_ = i
	}
	for _, r := range cera {
		fmt.Fprintf(&b, "· 点券 %d\n", r.Amount)
	}
	for _, r := range diamond {
		fmt.Fprintf(&b, "· 黑钻 %d 天\n", r.Amount)
	}
	b.WriteString("\n请查收，祝游戏愉快。")
	return b.String()
}

// welfarePublishNotice 发一条全服公告（写库 + 广播给在线登录器），成功返回 true。
// 与「立即发送」「定时公告」共用 publishAnnouncement 一条路径。
func (a *App) welfarePublishNotice(ctx context.Context, content string) bool {
	return a.publishAnnouncement(ctx, content, announceDefaultColor, "welfare")
}

// welfareTryNow 管理工具「立即试发」：不判定时间、只发一次，也不改 last_runs。
func (a *App) welfareTryNow(ctx context.Context, kind string) (welfareRunResult, error) {
	cfg := a.welfareConf()
	slot := welfareSlotOf(cfg, kind)
	if len(slot.Rewards) == 0 {
		return welfareRunResult{}, fmt.Errorf("%s 没有配置奖励，无法试发", welfareSlotTitle(kind))
	}
	return a.welfareRunOnce(ctx, kind, slot, "管理员试发"), nil
}

// welfareStatus 给管理工具的状态快照（含各档期的下次计划时刻）。
func (a *App) welfareStatus() welfareStatusView {
	cfg := a.welfareConf()
	now := time.Now()
	view := welfareStatusView{LastRuns: map[string]string{}}
	for k, v := range cfg.LastRuns {
		view.LastRuns[k] = v
	}
	view.Daily = welfareSlotViewOf(cfg.Daily, welfareDaily, now, cfg.LastRuns[welfareDaily])
	view.Weekly = welfareSlotViewOf(cfg.Weekly, welfareWeekly, now, cfg.LastRuns[welfareWeekly])
	view.Monthly = welfareSlotViewOf(cfg.Monthly, welfareMonthly, now, cfg.LastRuns[welfareMonthly])
	return view
}

// welfareStatusView 管理工具用的状态视图。
type welfareStatusView struct {
	Daily   welfareSlotView   `json:"daily"`
	Weekly  welfareSlotView   `json:"weekly"`
	Monthly welfareSlotView   `json:"monthly"`
	LastRuns map[string]string `json:"last_runs"`
}

// welfareSlotView 单档期视图：配置 + 下次计划时刻（管理工具直接展示，不用自己算）。
type welfareSlotView struct {
	Enabled  bool            `json:"enabled"`
	Time     string          `json:"time"`
	Weekday  int             `json:"weekday,omitempty"`
	Day      int             `json:"day,omitempty"`
	Subject  string          `json:"subject,omitempty"`
	Notice   string          `json:"notice,omitempty"`
	Rewards  []WelfareReward `json:"rewards"`
	NextRun  string          `json:"next_run,omitempty"` // 下一个计划时刻（RFC3339）
	LastRun  string          `json:"last_run,omitempty"` // 最近一次已处理的周期键
}

func welfareSlotViewOf(slot WelfareSlot, kind string, now time.Time, lastRun string) welfareSlotView {
	view := welfareSlotView{
		Enabled: slot.Enabled, Time: slot.Time, Weekday: slot.Weekday, Day: slot.Day,
		Subject: slot.Subject, Notice: slot.Notice, Rewards: slot.Rewards, LastRun: lastRun,
	}
	if view.Rewards == nil {
		view.Rewards = []WelfareReward{}
	}
	if !slot.Enabled {
		return view
	}
	// 下次计划时刻交给通用引擎算（schedule.go），界面不重复实现日历逻辑。
	if next, ok := scheduleNextAfter(welfareSpec(kind, slot), now); ok {
		view.NextRun = next.Format(time.RFC3339)
	}
	return view
}
