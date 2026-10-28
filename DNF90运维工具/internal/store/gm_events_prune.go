package store

// GM 事件表的保留策略与清理（2026-09-28 新增）。
//
// ── 为什么必须清理 ────────────────────────────────────────────────────────
// 泡点开着、间隔 10 秒时，**每个在线账号每 10 秒就产生一条 `cera_granted`**：
//
//	单账号：8640 行/天
//	1000 人在线：864 万行/天
//
// 原来这张表**没有任何清理**，只增不减。行数涨上去之后不只是占空间：
// 登录器每次轮询都要按 (account_id, id) 查一次，单账号的历史行越多，
// 查询越慢；最终会把认证服务的数据库连接池（只开了 8 条）占满，
// 连登录、心跳都会被拖垮 —— 也就是"人一多，整个认证服务一起卡"。
//
// ── 为什么要按类型分开保留 ────────────────────────────────────────────────
// 两类事件的性质完全不同，用同一个保留时长必然出事：
//
//	cera_granted（泡点到账提示）：**瞬时**。登录器 10 秒内就会取走；取走之后
//	  这条记录毫无价值。留 30 分钟已经足够容忍网络抖动与短暂重连。
//	kick（封禁/踢下线）：**必须送达**。玩家可能几个小时后才重新打开登录器，
//	  那条"你被封了"的事件必须还在，否则他连游戏进程都不会被关掉。
//	  所以留 7 天。
//
// 如果图省事用同一个时长：留短了封禁通知会丢；留长了表照样爆。

import (
	"context"
	"time"
)

// GMEventTypeCeraGranted 泡点到账提示的事件类型（瞬时，可短保留）。
//
// 与 pointsink.go 的 psEventCeraGranted 必须一致 —— 那里是写入方，这里是清理方，
// 对不上的话清理会失效（表照样无限长）。有单测锁住。
const GMEventTypeCeraGranted = "cera_granted"

// PruneGMEvents 按类型清理过期事件，返回删除总行数。
//
// keepEphemeral 适用于 cera_granted 这类瞬时提示；keepKick 适用于其余所有类型
// （封禁/踢下线等必须送达的事件）。两者都必须为正，否则直接返回 0 不删 ——
// **清理逻辑宁可什么都不做，也不能因为参数为 0 把整张表清空**。
func (s *Store) PruneGMEvents(ctx context.Context, keepEphemeral, keepKick time.Duration) (int64, error) {
	if keepEphemeral <= 0 || keepKick <= 0 {
		return 0, nil
	}

	ephemeralCutoff := time.Now().Add(-keepEphemeral)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM launcher_gm_events WHERE event_type = ? AND created_at < ?`,
		GMEventTypeCeraGranted, ephemeralCutoff)
	if err != nil {
		return 0, err
	}
	removed, _ := res.RowsAffected()

	kickCutoff := time.Now().Add(-keepKick)
	res, err = s.db.ExecContext(ctx,
		`DELETE FROM launcher_gm_events WHERE event_type <> ? AND created_at < ?`,
		GMEventTypeCeraGranted, kickCutoff)
	if err != nil {
		return removed, err
	}
	more, _ := res.RowsAffected()
	return removed + more, nil
}

// GMEventStats 事件表规模快照（诊断/日志用：清理到底有没有在起作用）。
type GMEventStats struct {
	Total     int64
	Ephemeral int64
}

// GMEventStatsSnapshot 读一次事件表规模。失败时返回零值 + 错误，不影响调用方主流程。
func (s *Store) GMEventStatsSnapshot(ctx context.Context) (GMEventStats, error) {
	var stats GMEventStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(event_type = ?), 0) FROM launcher_gm_events`,
		GMEventTypeCeraGranted).Scan(&stats.Total, &stats.Ephemeral)
	if err != nil {
		return GMEventStats{}, err
	}
	return stats, nil
}

// EnsureGMEventPruneIndex 幂等补索引 launcher_gm_events(event_type, created_at)。
//
// 没有它的话，上面两条 DELETE 都只能全表扫描 —— 清理本身就会变成新的卡顿源
// （恰恰是在表最大的时候最慢）。
func (s *Store) EnsureGMEventPruneIndex(ctx context.Context) error {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = 'launcher_gm_events'
		   AND index_name = 'idx_gm_events_type_time'`).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx,
		`ALTER TABLE launcher_gm_events ADD KEY idx_gm_events_type_time (event_type, created_at)`)
	return err
}
