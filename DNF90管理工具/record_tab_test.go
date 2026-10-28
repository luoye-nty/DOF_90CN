package main

// 记录查询页的纯逻辑测试（不建窗口、不发网络请求）。
//
// 重点是分页循环：它是"一次查出全部记录"的关键路径，而且这里踩过一次真 bug
// （walk 的 NumberEdit 默认值被 SetRange 夹成 1，导致查询只回 1 条）。
// 所以用假客户端把"多页拼接、翻页能力探测、上限保护、部分失败"逐条锁住。

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fakeRecQuerier 一个按 offset/limit 切片的假记录服务端。
type fakeRecQuerier struct {
	total int
	// ignoreOffset 模拟"老服务端不认识 offset"：永远从第 0 条开始返回。
	ignoreOffset bool
	// failAtPage 从第几页（1 起）开始返回错误；0 表示不失败。
	failAtPage int
	// alwaysLimited 模拟"服务端永远说还有更多"（用于验证翻页次数上限）。
	alwaysLimited bool
	// withAccountNames 模拟运维工具"按页翻译账号名"：每页只回本页出现过的映射。
	withAccountNames bool
	calls            []RecordQueryParams
}

func (f *fakeRecQuerier) RecordsQuery(ctx context.Context, params RecordQueryParams) (*RecordPage, error) {
	page := len(f.calls) + 1
	f.calls = append(f.calls, params)
	if f.failAtPage > 0 && page >= f.failAtPage {
		return nil, errors.New("fake failure")
	}
	limit := params.Limit
	if limit <= 0 {
		limit = 200
	}
	offset := params.Offset
	// echoOffset 是老服务端的回显：它的响应结构里根本没有 offset 字段，
	// 解出来必然是 0；这正是客户端用来判断"对端支不支持翻页"的依据。
	echoOffset := params.Offset
	if f.ignoreOffset {
		offset = 0
		echoOffset = 0
	}
	records := make([]map[string]any, 0, limit)
	for index := offset; index < f.total && len(records) < limit; index++ {
		entry := map[string]any{
			"kind":         RecordKindDungeon,
			"ts":           fmt.Sprintf("2026-09-14T10:00:%02dZ", index%60),
			"seq":          float64(index),
			"dungeon_id":   float64(11000 + index),
			"event_name":   "进入副本",
			"character_id": "19",
		}
		if f.withAccountNames {
			entry["account_id"] = fmt.Sprintf("dnf:%d", index)
		}
		records = append(records, entry)
	}
	names := map[string]string(nil)
	if f.withAccountNames {
		// 运维工具是按页翻译的：每页只回"本页出现过的 account_id"。
		names = make(map[string]string, len(records))
		for index := offset; index < offset+len(records); index++ {
			names[fmt.Sprintf("dnf:%d", index)] = fmt.Sprintf("玩家%d", index)
		}
	}
	limited := offset+len(records) < f.total
	if f.alwaysLimited && len(records) > 0 {
		limited = true
	}
	return &RecordPage{
		Kind:         RecordKindDungeon,
		Title:        "副本记录",
		Dir:          "/fake/records",
		Records:      records,
		Count:        len(records),
		Scanned:      len(records),
		Offset:       echoOffset,
		Limited:      limited,
		AccountNames: names,
	}, nil
}

// TestFetchRecRecordsSinglePage：12 条记录（就是现场那种规模）必须一次全拿到。
// 这条用例正面锁住"只查到 1 条"的回归：默认目标 1000 条时，任何页都不该被截成 1 条。
func TestFetchRecRecordsSinglePage(t *testing.T) {
	client := &fakeRecQuerier{total: 12}
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, recDefaultLoad, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got.rows) != 12 || got.pages != 1 || got.more {
		t.Fatalf("rows=%d pages=%d more=%t, want 12/1/false", len(got.rows), got.pages, got.more)
	}
	if client.calls[0].Limit != recDefaultLoad {
		t.Fatalf("first request limit = %d, want %d（这条断言就是那个 limit=1 的坑）", client.calls[0].Limit, recDefaultLoad)
	}
	if client.calls[0].Offset != 0 {
		t.Fatalf("first request offset = %d, want 0（第一页必须对老服务端兼容）", client.calls[0].Offset)
	}
}

// TestFetchRecRecordsPagesUntilTarget：目标超过一页时自动翻页，拼满为止。
func TestFetchRecRecordsPagesUntilTarget(t *testing.T) {
	client := &fakeRecQuerier{total: 5000}
	progress := make([]int, 0, 4)
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, 2500,
		func(loaded, pages int) { progress = append(progress, loaded) })
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got.rows) != 2500 {
		t.Fatalf("rows = %d, want 2500", len(got.rows))
	}
	if got.pages != 3 {
		t.Fatalf("pages = %d, want 3", got.pages)
	}
	if !got.more {
		t.Fatal("more should be true: 服务端还有 2500 条没取")
	}
	if len(progress) != 3 || progress[0] != 1000 || progress[2] != 2500 {
		t.Fatalf("progress = %v", progress)
	}
	// 每页的 offset 必须严格递进，且最后一页只多要 500 条。
	for index, call := range client.calls {
		wantOffset := index * recPageSize
		if call.Offset != wantOffset {
			t.Fatalf("call %d offset = %d, want %d", index, call.Offset, wantOffset)
		}
	}
	if client.calls[2].Limit != 500 {
		t.Fatalf("third page limit = %d, want 500", client.calls[2].Limit)
	}
	// 拼接结果不能有重复：seq 必须各不相同。
	seen := make(map[string]struct{}, len(got.rows))
	for _, row := range got.rows {
		key := recString(row.record, "seq")
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate record seq=%s in paged result", key)
		}
		seen[key] = struct{}{}
	}
}

// TestFetchRecRecordsMergesAccountNamesAcrossPages：账号名映射必须跨页累积。
//
// 运维工具是"按页翻译"的（每页只回本页出现过的 account_id）。翻页修好之后如果不合并，
// 前面几页的账号列会退化成 dnf:<sha256> 原始主键 —— 运营看到的就是一串哈希。
func TestFetchRecRecordsMergesAccountNamesAcrossPages(t *testing.T) {
	client := &fakeRecQuerier{total: 5000, withAccountNames: true}
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, 2500, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.pages != 3 {
		t.Fatalf("pages = %d, want 3", got.pages)
	}
	if got.pagingUnsupported {
		t.Fatal("回显 offset 正确时不该判定对端不支持翻页")
	}
	if got.page == nil {
		t.Fatal("fetched.page 不应为 nil（详情面板依赖它）")
	}
	// 三页各自的账号名都要在最终映射里（第 0 页最容易丢，因为它不在"最后一页"里）。
	for _, index := range []int{0, 1000, 2000} {
		key := fmt.Sprintf("dnf:%d", index)
		if name := got.page.AccountNames[key]; name != fmt.Sprintf("玩家%d", index) {
			t.Fatalf("跨页账号名映射 %s = %q，want 玩家%d（映射被最后一页覆盖了）", key, name, index)
		}
	}
}

// TestFetchRecRecordsStopsWhenServerLacksPaging：对端不认识 offset 时必须停，
// 并且明确标记 pagingUnsupported —— 否则会无限重取第一页。
func TestFetchRecRecordsStopsWhenServerLacksPaging(t *testing.T) {
	client := &fakeRecQuerier{total: 5000, ignoreOffset: true}
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, 20000, nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !got.pagingUnsupported {
		t.Fatal("pagingUnsupported should be true")
	}
	if got.pages != 2 {
		t.Fatalf("pages = %d, want 2（第 2 页发现对端忽略 offset 就该停）", got.pages)
	}
	if len(got.rows) != recPageSize {
		t.Fatalf("rows = %d, want %d", len(got.rows), recPageSize)
	}
}

// TestFetchRecRecordsKeepsPartialResultsOnError：翻页中途失败时，
// 已经取到的记录要照常返回（否则用户会因为第 2 页失败而看不到第 1 页）。
func TestFetchRecRecordsKeepsPartialResultsOnError(t *testing.T) {
	client := &fakeRecQuerier{total: 9000, failAtPage: 2}
	got, err := fetchRecRecords(context.Background(), client, RecordQueryParams{Kind: RecordKindDungeon}, 9000, nil)
	if err == nil {
		t.Fatal("expected the page-2 error to surface")
	}
	if len(got.rows) != recPageSize || got.pages != 1 {
		t.Fatalf("rows=%d pages=%d, want %d/1", len(got.rows), got.pages, recPageSize)
	}
}

// TestFetchRecRecordsEmptyAndGuards：空结果、目标非法值、以及"服务端一直说还有更多"
// 三种情况都不能死循环。
func TestFetchRecRecordsEmptyAndGuards(t *testing.T) {
	empty := &fakeRecQuerier{total: 0}
	got, err := fetchRecRecords(context.Background(), empty, RecordQueryParams{Kind: RecordKindDungeon}, recDefaultLoad, nil)
	if err != nil || len(got.rows) != 0 || got.pages != 1 || got.more {
		t.Fatalf("empty result = rows=%d pages=%d more=%t err=%v", len(got.rows), got.pages, got.more, err)
	}

	// 目标值非法时回落到默认值，绝不发出 limit=0。
	bad := &fakeRecQuerier{total: 5}
	if _, err := fetchRecRecords(context.Background(), bad, RecordQueryParams{Kind: RecordKindDungeon}, 0, nil); err != nil {
		t.Fatalf("zero target: %v", err)
	}
	if bad.calls[0].Limit != recDefaultLoad {
		t.Fatalf("zero target produced limit=%d, want %d", bad.calls[0].Limit, recDefaultLoad)
	}

	// 服务端谎报 limited 时，靠 recMaxPages 收口。
	endless := &fakeRecQuerier{total: 1000000, alwaysLimited: true}
	gotEndless, err := fetchRecRecords(context.Background(), endless, RecordQueryParams{Kind: RecordKindDungeon}, recMaxLoad, nil)
	if err != nil {
		t.Fatalf("endless: %v", err)
	}
	if gotEndless.pages > recMaxPages {
		t.Fatalf("pages = %d, want <= %d", gotEndless.pages, recMaxPages)
	}
	if len(gotEndless.rows) > recMaxLoad {
		t.Fatalf("rows = %d, want <= %d", len(gotEndless.rows), recMaxLoad)
	}
}

// TestRecLoadTargetFallsBackToDefault 锁住控件默认值这件事本身：
// 无论控件读到什么（0 / 负数 / 超上限），发出去的条数都必须合法。
func TestRecLoadTargetFallsBackToDefault(t *testing.T) {
	if recDefaultLoad < 1 || recDefaultLoad > recMaxLoad {
		t.Fatalf("recDefaultLoad=%d 不在 1..%d 之内", recDefaultLoad, recMaxLoad)
	}
	// 没有控件（未创建窗口）时也必须给出默认值，而不是 0。
	if got := recLoadTarget(); got != recDefaultLoad {
		t.Fatalf("recLoadTarget() without widget = %d, want %d", got, recDefaultLoad)
	}
}
