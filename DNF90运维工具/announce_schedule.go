package main

// 定时公告（E）：管理工具里排队多条「一次性 / 每天 / 每周」公告，到点自动发全服公告。
//
// 发送走的就是"立即发送"那条链路（写 launcher_announcements + 广播给在线登录器长轮询），
// 所以玩家侧表现完全一致：游戏内喇叭 + 登录器通知。
//
// 时间判定复用 schedule.go 的通用引擎，与在线福利同一套规则：
// 到点发、同一周期只发一次、错过窗口（工具没开）不补发只记日志。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/config"
)

const (
	// announceScheduleTickEvery 定时公告扫描间隔。
	announceScheduleTickEvery = 30 * time.Second
	// announceScheduleTolerance 到点后多久内算"该发"，超过当错过（不补发）。
	announceScheduleTolerance = 5 * time.Minute
	// announceMaxContentRunes 与立即发送的上限保持一致（服务端与游戏内喇叭都按 100 字走）。
	announceMaxContentRunes = 100
	// announceScheduleMaxItems 列表上限：够排队用，也不至于把配置写成一本账。
	announceScheduleMaxItems = 100
	// announceDefaultColor 默认颜色（与立即发送缺省一致：蓝色）。
	announceDefaultColor = "0xFF0000FF"
)

// announceScheduleMu 保护配置读写（与福利共用一把？不：福利用的是 psCfgMu，
// 这里另起一把锁，避免两种配置互相阻塞）。
var announceScheduleMu sync.Mutex

// ScheduledAnnouncement 是 config.ScheduledAnnouncement 的本包别名
// （与 welfare.go 对福利配置的做法一致：配置结构必须住在 internal/config 里，
// 否则 manage 包没法引用；本包内用别名少写 config. 前缀）。
type ScheduledAnnouncement = config.ScheduledAnnouncement

// announceScheduleItem 管理工具视图：配置 + 服务端算好的下次计划时刻。
type announceScheduleItem struct {
	config.ScheduledAnnouncement
	NextRun string `json:"next_run,omitempty"`
}

// announceScheduleList 读取配置副本。
func (a *App) announceScheduleList() []config.ScheduledAnnouncement {
	announceScheduleMu.Lock()
	defer announceScheduleMu.Unlock()
	if a.cfg == nil {
		return nil
	}
	out := make([]config.ScheduledAnnouncement, len(a.cfg.AnnounceSchedule))
	copy(out, a.cfg.AnnounceSchedule)
	return out
}

// announceScheduleView 列表 + 下次计划时刻（管理工具直接展示）。
func (a *App) announceScheduleView() []announceScheduleItem {
	items := a.announceScheduleList()
	now := time.Now()
	out := make([]announceScheduleItem, 0, len(items))
	for _, it := range items {
		view := announceScheduleItem{ScheduledAnnouncement: it}
		if it.Enabled {
			// 已到点、正在等下一次 30 秒扫描执行的条目：直接把"计划时刻"显示出来，
			// 而不是显示成明天的同一时刻（那会让运营以为今天不会发）。
			if dec := scheduleDecide(announceSpecOf(it), now, it.LastRun, announceScheduleTolerance); dec.Run {
				view.NextRun = dec.DueAt.Format(time.RFC3339)
			} else if next, ok := scheduleNextAfter(announceSpecOf(it), now); ok {
				view.NextRun = next.Format(time.RFC3339)
			}
		}
		out = append(out, view)
	}
	return out
}

// announceSpecOf 把配置项映射成通用周期描述。
func announceSpecOf(it config.ScheduledAnnouncement) scheduleSpec {
	mode := strings.ToLower(strings.TrimSpace(it.Mode))
	switch mode {
	case scheduleModeOnce, scheduleModeDaily, scheduleModeWeekly:
	default:
		mode = scheduleModeDaily
	}
	return scheduleSpec{Mode: mode, Time: it.Time, Date: it.Date, Weekday: it.Weekday}
}

// validateAnnounceItem 校验一条定时公告配置（错误信息直接给运营看）。
func validateAnnounceItem(it config.ScheduledAnnouncement) error {
	content := strings.TrimSpace(it.Content)
	if content == "" {
		return fmt.Errorf("公告内容不能为空")
	}
	if len([]rune(content)) > announceMaxContentRunes {
		return fmt.Errorf("公告内容超过 %d 字", announceMaxContentRunes)
	}
	spec := announceSpecOf(it)
	if _, _, ok := parseClock(spec.Time); !ok {
		return fmt.Errorf("时间格式必须是 HH:MM（例如 20:00）")
	}
	switch spec.Mode {
	case scheduleModeOnce:
		if _, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(spec.Date), time.Local); err != nil {
			return fmt.Errorf("一次性公告必须填日期，格式 YYYY-MM-DD（例如 2026-10-01）")
		}
	case scheduleModeWeekly:
		if spec.Weekday < 1 || spec.Weekday > 7 {
			return fmt.Errorf("每周公告必须填星期几（1=周一 … 7=周日）")
		}
	}
	return nil
}

// announceScheduleApply 整批保存定时公告配置（服务端校验失败即整批拒绝）。
//
// 已存在条目的 LastRun 会按 id 保留：管理工具改内容/时间不应该让"今天已经发过"
// 的记录消失（否则保存一次就会重发一遍）。
func (a *App) announceScheduleApply(_ context.Context, items []config.ScheduledAnnouncement) ([]announceScheduleItem, error) {
	if a == nil || a.cfg == nil {
		return nil, fmt.Errorf("配置尚未加载，无法保存定时公告")
	}
	if len(items) > announceScheduleMaxItems {
		return nil, fmt.Errorf("定时公告最多 %d 条", announceScheduleMaxItems)
	}
	oldLastRun := map[string]string{}
	for _, it := range a.announceScheduleList() {
		if it.ID != "" {
			oldLastRun[it.ID] = it.LastRun
		}
	}
	now := time.Now()
	seen := map[string]struct{}{}
	out := make([]config.ScheduledAnnouncement, 0, len(items))
	for i, raw := range items {
		raw.Content = strings.TrimSpace(raw.Content)
		raw.Color = strings.TrimSpace(raw.Color)
		if raw.Color == "" {
			raw.Color = announceDefaultColor
		}
		raw.ID = strings.TrimSpace(raw.ID)
		if raw.ID == "" {
			// 用时间戳+序号生成稳定 id（管理工具下次保存会把它带回来）。
			raw.ID = fmt.Sprintf("ann-%d-%d", now.Unix(), i+1)
		}
		if _, dup := seen[raw.ID]; dup {
			return nil, fmt.Errorf("存在重复的公告 id（%s）", raw.ID)
		}
		seen[raw.ID] = struct{}{}
		if err := validateAnnounceItem(raw); err != nil {
			return nil, fmt.Errorf("第 %d 条：%w", i+1, err)
		}
		if raw.Mode == "" {
			raw.Mode = scheduleModeDaily
		}
		raw.CreatedAt = strings.TrimSpace(raw.CreatedAt)
		raw.LastRun = oldLastRun[raw.ID]
		out = append(out, raw)
	}

	announceScheduleMu.Lock()
	var old []ScheduledAnnouncement
	// 整表替换 + 落盘放进配置写锁（cfg.Mutate）：与 Save 的 json.Marshal 互斥，
	// 避免 slice 头部被撕裂读；落盘失败回滚内存，保持"内存 == 磁盘"。
	err := a.cfg.Mutate(func() {
		old = a.cfg.AnnounceSchedule
		a.cfg.AnnounceSchedule = out
	}, func() {
		a.cfg.AnnounceSchedule = old
	})
	announceScheduleMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("配置持久化失败: %w", err)
	}
	a.logf("定时公告配置已更新：共 %d 条", len(out))
	return a.announceScheduleView(), nil
}

// announceScheduleMarkRun 把某条公告的周期标记为已处理（落盘）。
//
// 改 slice 元素 + 落盘必须整体放进 cfg.Mutate（配置写锁）：Save 会 json.Marshal 整份配置，
// 与"改 AnnounceSchedule[i].LastRun"并发时 slice 头部/元素可能被撕裂读。
func (a *App) announceScheduleMarkRun(id, period string) {
	announceScheduleMu.Lock()
	defer announceScheduleMu.Unlock()
	if a.cfg == nil {
		return
	}
	_ = a.cfg.Mutate(func() {
		for i := range a.cfg.AnnounceSchedule {
			if a.cfg.AnnounceSchedule[i].ID == id {
				a.cfg.AnnounceSchedule[i].LastRun = period
				return
			}
		}
	}, nil)
}

// announceScheduleTick 扫描定时公告（由 schedulerLoop 每 30 秒调用）。
func (a *App) announceScheduleTick() {
	items := a.announceScheduleList()
	if len(items) == 0 {
		return
	}
	now := time.Now()
	for _, it := range items {
		if !it.Enabled {
			continue
		}
		dec := scheduleDecide(announceSpecOf(it), now, it.LastRun, announceScheduleTolerance)
		if !dec.Run {
			if dec.Mark && strings.TrimSpace(it.LastRun) != dec.Period {
				a.announceScheduleMarkRun(it.ID, dec.Period)
				a.logf("定时公告「%s」：%s", announceBrief(it.Content), dec.Reason)
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok := a.publishAnnouncement(ctx, it.Content, it.Color, "schedule")
		cancel()
		// 不论发成功没发成功都标记本周期已处理：失败重试容易变成"补发一堆"，
		// 运营在日志里能看到失败原因，需要的话可以手动"立即发送"补一条。
		a.announceScheduleMarkRun(it.ID, dec.Period)
		if ok {
			a.logf("定时公告已发送：%s（%s %s）", announceBrief(it.Content), it.Mode, it.Time)
		} else {
			a.logf("定时公告发送失败：%s（本周期标记为已处理，可手动补发）", announceBrief(it.Content))
		}
	}
}

// announceBrief 日志里的短文案（避免把整条公告刷进日志）。
func announceBrief(content string) string {
	runes := []rune(strings.TrimSpace(content))
	if len(runes) <= 16 {
		return string(runes)
	}
	return string(runes[:16]) + "…"
}

// publishAnnouncementID 发一条全服公告（写库 + 广播给等待中的登录器），返回公告 id。
//
// 「立即发送」「定时公告」「在线福利公告」三条路径全部走它，保证表现完全一致
// （游戏内喇叭与登录器通知都依赖写库 + 广播这两步）。
func (a *App) publishAnnouncementID(ctx context.Context, content, color, operator string) (int64, error) {
	st := a.pointsinkStore()
	if st == nil {
		return 0, errors.New("认证服务未运行，无法发布公告")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, errors.New("公告内容不能为空")
	}
	if len([]rune(content)) > announceMaxContentRunes {
		content = string([]rune(content)[:announceMaxContentRunes])
	}
	color = strings.TrimSpace(color)
	if color == "" {
		color = announceDefaultColor
	}
	operator = strings.TrimSpace(operator)
	if operator == "" {
		operator = "ops"
	}
	id, err := st.CreateAnnouncement(ctx, content, color, operator)
	if err != nil {
		return 0, err
	}
	if a.svc != nil {
		payload, _ := json.Marshal(map[string]any{"id": id, "content": content, "color": color})
		a.svc.Announce().Broadcast(payload)
	}
	return id, nil
}

// publishAnnouncement 同 publishAnnouncementID，只是把结果压成 bool（定时/福利路径用），
// 失败时负责写一条日志。
func (a *App) publishAnnouncement(ctx context.Context, content, color, operator string) bool {
	if _, err := a.publishAnnouncementID(ctx, content, color, operator); err != nil {
		a.logf("公告发送失败（%s）：%v", operator, err)
		return false
	}
	return true
}
