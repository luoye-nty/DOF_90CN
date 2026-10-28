package main

// 副本记录「三合一」的纯逻辑测试（不建窗口、不发请求）。
//
// 这块的规则很容易写错，而写错的后果是**误导排查**：
//   - 合错了 → 两局不相干的副本被并成一条，用时/击杀全乱；
//   - 合漏了 → 玩家崩溃/掉线的局（没有"离开"记录）整条消失；
//   - 时间列取错 → 列表按"离开时间"排序，看不出是什么时候进的图。

import (
	"context"
	"strings"
	"testing"
)

func dungeonEvent(ts, event, eventName, startedAt, token string, extra map[string]any) map[string]any {
	record := map[string]any{
		"kind":            "dungeon",
		"ts":              ts,
		"event":           event,
		"event_name":      eventName,
		"started_at":      startedAt,
		"account_id":      "dnf:abc",
		"character_id":    "2",
		"character_name":  "2222",
		"dungeon_id":      1101,
		"elapsed_seconds": float64(0),
	}
	if token != "" {
		record["lifecycle_token"] = token
	}
	for key, value := range extra {
		record[key] = value
	}
	return record
}

// TestMergeDungeonRunThreeEvents 正常一局：进入 + 通关 + 离开 → 一行，且取到"离开"那条的完整字段。
func TestMergeDungeonRunThreeEvents(t *testing.T) {
	const startedAt = "2026-09-24T12:00:00Z"
	records := []map[string]any{
		// 服务端按时间倒序返回：离开 → 通关 → 进入
		dungeonEvent("2026-09-24T12:03:12Z", "exit", "离开副本", startedAt, "tok-1",
			map[string]any{"elapsed_seconds": float64(192), "colorless_spent": float64(24), "exit_reason": "town-return", "cleared": true}),
		dungeonEvent("2026-09-24T12:03:10Z", "clear", "通关", startedAt, "tok-1",
			map[string]any{"run_status": "cleared", "defeated_actor_count": float64(37)}),
		dungeonEvent("2026-09-24T12:00:00Z", "enter", "进入副本", startedAt, "tok-1",
			map[string]any{"difficulty": float64(2)}),
	}

	merged := mergeDungeonRecords(records)
	if len(merged) != 1 {
		t.Fatalf("三合一后行数 = %d，期望 1", len(merged))
	}
	row := merged[0]
	if got := recString(row, "event"); got != "exit" {
		t.Fatalf("代表事件应为 exit（字段最全），实际 %q", got)
	}
	if got := recString(row, "elapsed_seconds"); got != "192" {
		t.Fatalf("通关用时应保留，实际 %q", got)
	}
	if got := recString(row, "colorless_spent"); got != "24" {
		t.Fatalf("无色消耗应保留，实际 %q", got)
	}
	// 时间列取"进副本时刻"（列表按进图时间看）
	if got := recString(row, "ts"); got != "2026-09-24T12:00:00Z" {
		t.Fatalf("合并后 ts 应取最早（进副本）那条，实际 %q", got)
	}
	// 代表记录没有的字段要从其它两条补上
	if got := recString(row, "run_status"); got != "cleared" {
		t.Fatalf("run_status 应从 clear 记录补进来，实际 %q", got)
	}
	if got := recString(row, "difficulty"); got != "2" {
		t.Fatalf("difficulty 应从 enter 记录补进来，实际 %q", got)
	}
	if note := recString(row, recMergedEventsField); !strings.Contains(note, "进入副本") || !strings.Contains(note, "共 3 条") {
		t.Fatalf("应标注由哪几条原始记录合并且注明条数，实际 %q", note)
	}
}

// TestMergeDungeonRunWithoutExit 崩溃/掉线局：只有进入 + 通关 → 仍是一行（证据不能丢）。
func TestMergeDungeonRunWithoutExit(t *testing.T) {
	const startedAt = "2026-09-24T13:00:00Z"
	records := []map[string]any{
		dungeonEvent("2026-09-24T13:02:00Z", "clear", "通关", startedAt, "tok-2", map[string]any{"run_status": "cleared"}),
		dungeonEvent("2026-09-24T13:00:00Z", "enter", "进入副本", startedAt, "tok-2", nil),
	}
	merged := mergeDungeonRecords(records)
	if len(merged) != 1 {
		t.Fatalf("掉线局应合并成 1 行，实际 %d", len(merged))
	}
	if got := recString(merged[0], "event"); got != "clear" {
		t.Fatalf("没有 exit 时代表记录应为 clear，实际 %q", got)
	}
}

// TestMergeDungeonRunEnterOnly 崩在副本里（只有进入记录）→ 保留这一行。
func TestMergeDungeonRunEnterOnly(t *testing.T) {
	records := []map[string]any{
		dungeonEvent("2026-09-24T14:00:00Z", "enter", "进入副本", "2026-09-24T14:00:00Z", "tok-3", nil),
	}
	merged := mergeDungeonRecords(records)
	if len(merged) != 1 || recString(merged[0], "event") != "enter" {
		t.Fatalf("只有进入记录时应原样保留一行，实际 %v", merged)
	}
}

// TestMergeDungeonKeepsDifferentRunsApart 不同两局（不同进本时刻）不能被并到一行。
func TestMergeDungeonKeepsDifferentRunsApart(t *testing.T) {
	records := []map[string]any{
		dungeonEvent("2026-09-24T15:10:00Z", "exit", "离开副本", "2026-09-24T15:05:00Z", "tok-b", nil),
		dungeonEvent("2026-09-24T15:00:00Z", "exit", "离开副本", "2026-09-24T14:55:00Z", "tok-a", nil),
	}
	merged := mergeDungeonRecords(records)
	if len(merged) != 2 {
		t.Fatalf("两局不同副本应保留 2 行，实际 %d", len(merged))
	}
}

// TestMergeDungeonSameSecondDifferentToken 同一秒连刷两个本：靠 lifecycle_token 区分。
func TestMergeDungeonSameSecondDifferentToken(t *testing.T) {
	const startedAt = "2026-09-24T16:00:00Z"
	records := []map[string]any{
		dungeonEvent("2026-09-24T16:01:00Z", "exit", "离开副本", startedAt, "tok-y", nil),
		dungeonEvent("2026-09-24T16:01:00Z", "exit", "离开副本", startedAt, "tok-x", nil),
	}
	merged := mergeDungeonRecords(records)
	if len(merged) != 2 {
		t.Fatalf("同秒不同 token 应保留 2 行，实际 %d", len(merged))
	}
}

// TestMergeDungeonWithoutKeyStaysSeparate 既没有 started_at 也没有 token → 不合并（宁可多显示）。
func TestMergeDungeonWithoutKeyStaysSeparate(t *testing.T) {
	a := dungeonEvent("2026-09-24T17:00:00Z", "enter", "进入副本", "", "", nil)
	b := dungeonEvent("2026-09-24T17:01:00Z", "exit", "离开副本", "", "", nil)
	merged := mergeDungeonRecords([]map[string]any{a, b})
	if len(merged) != 2 {
		t.Fatalf("无法可靠分组时应各自成行，实际 %d", len(merged))
	}
}

// TestBuildRecRowsOnlyMergesDungeon 其它类别（点券/对话）不受影响。
func TestBuildRecRowsOnlyMergesDungeon(t *testing.T) {
	page := &RecordPage{
		Records: []map[string]any{
			{"kind": "cera", "ts": "2026-09-24T18:00:00Z", "account_id": "dnf:abc", "cera_spent": float64(100)},
			{"kind": "cera", "ts": "2026-09-24T18:01:00Z", "account_id": "dnf:abc", "cera_spent": float64(200)},
		},
	}
	rows := buildRecRows(page, "cera")
	if len(rows) != 2 {
		t.Fatalf("点券记录不应被合并，实际 %d 行", len(rows))
	}
}

// pageSplitQuerier 模拟"一局副本的三条记录正好被切在两页之间"。
type pageSplitQuerier struct {
	// 页 1（offset=0）：通关；页 2（offset=1）：进入 + 离开。
	clearRecord map[string]any
	enterRecord map[string]any
	exitRecord  map[string]any
	calls       []int
}

func (q *pageSplitQuerier) RecordsQuery(_ context.Context, params RecordQueryParams) (*RecordPage, error) {
	q.calls = append(q.calls, params.Offset)
	if params.Offset == 0 {
		return &RecordPage{
			Kind: "dungeon", Count: 1, Offset: 0, Limited: true,
			Records: []map[string]any{q.clearRecord},
		}, nil
	}
	return &RecordPage{
		Kind: "dungeon", Count: 2, Offset: params.Offset, Limited: false,
		Records: []map[string]any{q.enterRecord, q.exitRecord},
	}, nil
}

// TestFetchRecRecordsMergesAcrossPages 跨页也要合并成一行（否则一局副本会显示成两行）。
func TestFetchRecRecordsMergesAcrossPages(t *testing.T) {
	const startedAt = "2026-09-24T19:00:00Z"
	client := &pageSplitQuerier{
		clearRecord: dungeonEvent("2026-09-24T19:02:00Z", "clear", "通关", startedAt, "tok-p", map[string]any{"run_status": "cleared"}),
		enterRecord: dungeonEvent("2026-09-24T19:00:00Z", "enter", "进入副本", startedAt, "tok-p", nil),
		exitRecord:  dungeonEvent("2026-09-24T19:02:30Z", "exit", "离开副本", startedAt, "tok-p", map[string]any{"elapsed_seconds": float64(150), "cleared": true}),
	}
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, 10, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got.rows) != 1 {
		t.Fatalf("跨页的一局副本应合并成 1 行，实际 %d 行（查询了 %d 页）", len(got.rows), len(client.calls))
	}
	if event := recString(got.rows[0].record, "event"); event != "exit" {
		t.Fatalf("代表事件应为 exit，实际 %q", event)
	}
	if len(client.calls) < 2 {
		t.Fatalf("应至少查询 2 页，实际 %v", client.calls)
	}
}
