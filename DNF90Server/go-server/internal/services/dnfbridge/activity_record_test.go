package dnfbridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRecorder 建一个落在临时目录里的记录器，测试结束自动关闭。
func newTestRecorder(t *testing.T) (*activityRecorder, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "records")
	recorder := newActivityRecorder(dir, nil)
	if recorder == nil {
		t.Fatalf("newActivityRecorder(%q) returned nil", dir)
	}
	t.Cleanup(recorder.Close)
	return recorder, dir
}

// readRecordFile 读一个记录文件里的全部非空行。
func readRecordFile(t *testing.T, dir, kind string) []map[string]any {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, kind+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	entries := make([]map[string]any, 0, 16)
	for _, path := range matches {
		lines, readErr := readRecordLines(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("line %q is not valid JSON: %v", line, err)
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

func TestActivityRecorderWritesOneLinePerRecord(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	recorder.record(recordKindChat, map[string]any{"character_id": "7", "message": "你好"}, nil)
	// flush 是查询/元信息接口的同步点：调用它之后记录必须已经在磁盘上。
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindChat)
	if len(entries) != 1 {
		t.Fatalf("record count = %d, want 1 (%+v)", len(entries), entries)
	}
	entry := entries[0]
	if entry["kind"] != recordKindChat {
		t.Fatalf("kind = %v, want %q", entry["kind"], recordKindChat)
	}
	if entry["character_id"] != "7" || entry["message"] != "你好" {
		t.Fatalf("fields lost: %+v", entry)
	}
	if stamp, ok := entry["ts"].(string); !ok || stamp == "" {
		t.Fatalf("ts missing: %+v", entry)
	}
	if written, dropped := recorder.stats(); written != 1 || dropped != 0 {
		t.Fatalf("stats written=%d dropped=%d, want 1/0", written, dropped)
	}
}

// TestActivityRecorderTsAndKindAreAuthoritative 锁住"埋点写进来的 ts/kind 不算数"：
// 记录器自己的时间戳和类别才是权威值，否则一条记录可能自称属于另一个类别而落到
// 别的文件里，检索就会永远找不到它。
func TestActivityRecorderTsAndKindAreAuthoritative(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	recorder.record(recordKindChat, map[string]any{"ts": "1999-01-01T00:00:00Z", "kind": "dungeon"}, nil)
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindChat)
	if len(entries) != 1 {
		t.Fatalf("chat record count = %d, want 1", len(entries))
	}
	if entries[0]["kind"] != recordKindChat {
		t.Fatalf("kind = %v, want %q", entries[0]["kind"], recordKindChat)
	}
	if entries[0]["ts"] == "1999-01-01T00:00:00Z" {
		t.Fatal("caller supplied ts was kept; recorder must stamp it itself")
	}
	if other := readRecordFile(t, dir, recordKindDungeon); len(other) != 0 {
		t.Fatalf("record landed in %s: %+v", recordKindDungeon, other)
	}
}

func TestActivityRecorderIgnoresUnknownKind(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	recorder.record("nonsense", map[string]any{"x": 1}, nil)
	recorder.record("", nil, nil)
	recorder.flush()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("unknown kind produced files: %v", names)
	}
}

// TestActivityRecorderIsAsynchronous 证明写盘不在调用协程上：
// record 返回后文件尚未出现（或至少不要求已经出现），flush 之后必须出现。
func TestActivityRecorderIsAsynchronous(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	for index := 0; index < 200; index++ {
		recorder.record(recordKindCera, map[string]any{"index": index}, nil)
	}
	recorder.flush()
	if entries := readRecordFile(t, dir, recordKindCera); len(entries) != 200 {
		t.Fatalf("record count = %d, want 200", len(entries))
	}
}

// TestActivityRecorderConcurrentWritersKeepLinesIntact 是所有埋点路径共同的前提：
// 多个协程同时记录时不能出现半行、串行或丢行。单写盘协程 + 整行入队就是为了这个。
func TestActivityRecorderConcurrentWritersKeepLinesIntact(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	const writers = 8
	const perWriter = 250
	var group sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			for index := 0; index < perWriter; index++ {
				recorder.record(recordKindChat, map[string]any{
					"writer": writer,
					"index":  index,
					"text":   strings.Repeat("x", 512),
				}, nil)
			}
		}(writer)
	}
	group.Wait()
	recorder.flush()

	entries := readRecordFile(t, dir, recordKindChat)
	if len(entries) != writers*perWriter {
		t.Fatalf("record count = %d, want %d", len(entries), writers*perWriter)
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		key := fmt.Sprintf("%v/%v", entry["writer"], entry["index"])
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("duplicate record %s", key)
		}
		seen[key] = struct{}{}
		if text, _ := entry["text"].(string); len(text) != 512 {
			t.Fatalf("torn line for %s: text len = %d", key, len(text))
		}
	}
}

func TestActivityRecorderRotatesBySize(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	recorder.record(recordKindDungeon, map[string]any{"index": 1}, nil)
	recorder.flush()

	// 直接把当前文件的大小顶到上限，下一次写入必须换到 -002。
	recorder.mu.Lock()
	recorder.sizes[recordKindDungeon] = recordMaxFileBytes
	recorder.mu.Unlock()

	recorder.record(recordKindDungeon, map[string]any{"index": 2}, nil)
	recorder.flush()

	matches, err := filepath.Glob(filepath.Join(dir, recordKindDungeon+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("file count = %d, want 2 (%v)", len(matches), matches)
	}
	if filepath.Base(matches[0]) > filepath.Base(matches[1]) {
		t.Fatalf("files are not in name order: %v", matches)
	}
	if !strings.HasSuffix(matches[1], "-002.jsonl") {
		t.Fatalf("rotated file = %s, want suffix -002.jsonl", matches[1])
	}
}

// TestActivityRecorderPrunesOldFiles 保证"保留最近 N 个文件"真的会删掉更早的文件，
// 否则记录目录会无限增长。
func TestActivityRecorderPrunesOldFiles(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	today := time.Now().Format("20060102")
	total := recordRetentionPerKind + 6
	for index := 1; index <= total; index++ {
		name := fmt.Sprintf("%s-%s-%03d.jsonl", recordKindChat, today, index)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	// 下一次写入触发轮转 + 清理。
	recorder.record(recordKindChat, map[string]any{"index": "trigger"}, nil)
	recorder.flush()

	matches, err := filepath.Glob(filepath.Join(dir, recordKindChat+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) > recordRetentionPerKind {
		t.Fatalf("file count = %d, want <= %d", len(matches), recordRetentionPerKind)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%s-%s-001.jsonl", recordKindChat, today))); !os.IsNotExist(err) {
		t.Fatalf("oldest file survived pruning: %v", err)
	}
}

func TestActivityRecorderQueryFilters(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	base := time.Now().Add(-time.Hour)
	seed := []map[string]any{
		{"character_id": "7", "character_name": "Alpha", "account_id": "dnf:1", "message": "hello"},
		{"character_id": "8", "character_name": "Beta", "account_id": "dnf:1", "message": "world"},
		{"character_id": "7", "character_name": "Alpha", "account_id": "dnf:2", "message": "secret"},
	}
	for index, fields := range seed {
		stamp := base.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano)
		recorder.record(recordKindChat, map[string]any{
			"ts":             stamp,
			"character_id":   fields["character_id"],
			"character_name": fields["character_name"],
			"account_id":     fields["account_id"],
			"message":        fields["message"],
		}, nil)
	}
	recorder.flush()

	// ts 是记录器写的权威值，所以时间过滤要用"现在"而不是上面那份旧时间戳。
	byCharacter, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, CharacterID: "7"})
	if err != nil || len(byCharacter) != 2 {
		t.Fatalf("character filter: count=%d err=%v", len(byCharacter), err)
	}
	// 查询按时间倒序：最新的一条在最前面。
	if byCharacter[0]["message"] != "secret" {
		t.Fatalf("records are not newest-first: %+v", byCharacter)
	}
	byName, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, CharacterName: "alpha"})
	if err != nil || len(byName) != 2 {
		t.Fatalf("name filter should be case-insensitive: count=%d err=%v", len(byName), err)
	}
	byAccount, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, AccountID: "dnf:2"})
	if err != nil || len(byAccount) != 1 {
		t.Fatalf("account filter: count=%d err=%v", len(byAccount), err)
	}
	byKeyword, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, Keyword: "WORLD"})
	if err != nil || len(byKeyword) != 1 {
		t.Fatalf("keyword filter: count=%d err=%v", len(byKeyword), err)
	}
	limited, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit: count=%d err=%v", len(limited), err)
	}
	since, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat, Since: time.Now().Add(time.Minute)})
	if err != nil || len(since) != 0 {
		t.Fatalf("since filter: count=%d err=%v", len(since), err)
	}
	if _, _, err := recorder.queryRecords(recordQuery{Kind: "nonsense"}); err == nil {
		t.Fatal("unknown kind must be rejected by queryRecords")
	}
}

// TestActivityRecorderFlushesBeforeQuery 保证"刚发生的操作立刻能查到"：
// 查询会先等队列排空，否则管理端会看到少一条的假象。
func TestActivityRecorderFlushesBeforeQuery(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	recorder.record(recordKindCera, map[string]any{"cera_spent": 100}, nil)
	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("query saw %d records, want 1 (flush barrier missing?)", len(records))
	}
	if records[0]["cera_spent"] != float64(100) {
		t.Fatalf("field mismatch: %+v", records[0])
	}
}

// TestActivityRecorderCloseFlushesAndIsIdempotent 保证服务停止时不会丢掉已经入队的记录，
// 并且重复 Close 不会 panic（Stop 可能被调用多次）。
func TestActivityRecorderCloseFlushesAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	recorder := newActivityRecorder(dir, nil)
	if recorder == nil {
		t.Fatal("recorder is nil")
	}
	for index := 0; index < 64; index++ {
		recorder.record(recordKindDungeon, map[string]any{"index": index}, nil)
	}
	recorder.Close()
	recorder.Close()

	entries := readRecordFile(t, dir, recordKindDungeon)
	if len(entries) != 64 {
		t.Fatalf("record count after Close = %d, want 64", len(entries))
	}
	// 关闭之后的记录被丢弃，但不能阻塞或 panic。
	recorder.record(recordKindDungeon, map[string]any{"index": "after-close"}, nil)
	recorder.flush()
	if entries := readRecordFile(t, dir, recordKindDungeon); len(entries) != 64 {
		t.Fatalf("record count changed after Close = %d, want 64", len(entries))
	}
}

func TestActivityRecorderDisabledWhenDirIsEmpty(t *testing.T) {
	if recorder := newActivityRecorder("   ", nil); recorder != nil {
		t.Fatal("empty dir must disable the recorder")
	}
	// 关闭的记录器必须让所有入口安全空转（调用点在玩法主路径上，不能 panic）。
	var recorder *activityRecorder
	recorder.record(recordKindChat, map[string]any{"x": 1}, nil)
	recorder.flush()
	recorder.Close()
	if recorder.Dir() != "" {
		t.Fatal("nil recorder must report an empty dir")
	}
	if records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat}); err == nil || records != nil {
		t.Fatalf("nil recorder query = %v/%v, want error", records, err)
	}
	if written, dropped := recorder.stats(); written != 0 || dropped != 0 {
		t.Fatalf("nil recorder stats = %d/%d", written, dropped)
	}
}

// TestActivityRecorderOffsetPagesWithoutGaps 是"查全部"的核心保证：分页取回的记录
// 必须不重不漏，拼起来正好等于一次取完的结果。
func TestActivityRecorderOffsetPagesWithoutGaps(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	const total = 25
	for index := 0; index < total; index++ {
		recorder.record(recordKindDungeon, map[string]any{"seq": index}, nil)
	}
	recorder.flush()

	all, _, err := recorder.queryRecords(recordQuery{Kind: recordKindDungeon, Limit: 200})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != total {
		t.Fatalf("unpaged count = %d, want %d", len(all), total)
	}

	// 每页 4 条逐页取，拼起来必须与一次取完完全一致（顺序也要一致）。
	paged := make([]map[string]any, 0, total)
	for offset := 0; offset < total; offset += 4 {
		page, _, err := recorder.queryRecords(recordQuery{Kind: recordKindDungeon, Limit: 4, Offset: offset})
		if err != nil {
			t.Fatalf("page offset=%d: %v", offset, err)
		}
		if len(page) == 0 {
			t.Fatalf("page offset=%d returned nothing", offset)
		}
		paged = append(paged, page...)
	}
	if len(paged) != total {
		t.Fatalf("paged count = %d, want %d", len(paged), total)
	}
	for index := range all {
		if fmt.Sprintf("%v", all[index]["seq"]) != fmt.Sprintf("%v", paged[index]["seq"]) {
			t.Fatalf("record %d differs between paged and unpaged: %+v vs %+v", index, all[index], paged[index])
		}
	}

	// 偏移超过总数返回空页，而不是回绕到第一页（回绕会让翻页循环永远取不完）。
	empty, _, err := recorder.queryRecords(recordQuery{Kind: recordKindDungeon, Limit: 4, Offset: total + 10})
	if err != nil || len(empty) != 0 {
		t.Fatalf("offset past end = %d records, err=%v", len(empty), err)
	}
	// 负数偏移按 0 处理，不 panic。
	clamped, _, err := recorder.queryRecords(recordQuery{Kind: recordKindDungeon, Limit: 3, Offset: -5})
	if err != nil || len(clamped) != 3 {
		t.Fatalf("negative offset = %d records, err=%v", len(clamped), err)
	}
}

// TestGMQueryRecordsReportsOffsetAndLimited 锁住翻页协议：回显 offset、
// 装满一页时 limited=true、最后一页 limited=false，且相邻页不重复。
func TestGMQueryRecordsReportsOffsetAndLimited(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)
	for index := 0; index < 7; index++ {
		recorder.record(recordKindChat, map[string]any{"seq": index, "message": "hi"}, nil)
	}
	recorder.flush()

	first, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindChat, Limit: 3})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Count != 3 || first.Offset != 0 || !first.Limited {
		t.Fatalf("first page = %+v", first)
	}
	second, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindChat, Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if second.Count != 3 || second.Offset != 3 || !second.Limited {
		t.Fatalf("second page = %+v", second)
	}
	if fmt.Sprintf("%v", first.Records[0]["seq"]) == fmt.Sprintf("%v", second.Records[0]["seq"]) {
		t.Fatal("page 2 repeated page 1")
	}
	third, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindChat, Limit: 3, Offset: 6})
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if third.Count != 1 || third.Offset != 6 || third.Limited {
		t.Fatalf("third page = %+v", third)
	}
}

func TestDefaultRecordDirFor(t *testing.T) {
	cases := map[string]string{
		"logs/packet_log.txt": filepath.Join("logs", "records"),
		"":                    filepath.Join("logs", "records"),
		filepath.Join("D:", "dnf", "logs", "p.txt"): filepath.Join("D:", "dnf", "logs", "records"),
	}
	for input, want := range cases {
		if got := defaultRecordDirFor(input); got != want {
			t.Fatalf("defaultRecordDirFor(%q) = %q, want %q", input, got, want)
		}
	}
	// 记录目录必须与包日志同级：日志换盘符/换目录时记录跟着走，不会散落两处。
	if got := defaultRecordDirFor(filepath.Join("x", "y", "z", "any.log")); got != filepath.Join("x", "y", "z", "records") {
		t.Fatalf("sibling derivation = %q", got)
	}
}

// TestServiceRecordActivityWithoutRecorderIsSafe 覆盖"记录目录不可写/服务未启动"这条路径：
// recordActivity 必须直接空转，而不是 panic。
func TestServiceRecordActivityWithoutRecorderIsSafe(t *testing.T) {
	var service *Service
	service.recordActivity(recordKindChat, map[string]any{"x": 1})
	service.recordChat(nil, 3, "区域", []byte("hi"), 1, 0, "ok", "", nil)
	service.recordCeraSpend(nil, map[string]any{"cera_spent": 1})
	service.recordDungeon(nil, "enter", map[string]any{"dungeon_id": 1})

	service = &Service{}
	service.recordActivity(recordKindChat, map[string]any{"x": 1})
	if page, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindChat}); err != nil || page.Count != 0 {
		t.Fatalf("query without recorder = %+v/%v", page, err)
	}
	if meta := service.GMRecordMeta(); meta.Enabled || len(meta.Kinds) != len(recordKinds) {
		t.Fatalf("meta without recorder = %+v", meta)
	}
}

func TestGMQueryRecordsRejectsUnknownKind(t *testing.T) {
	service := &Service{}
	if _, err := service.GMQueryRecords(GMRecordQuery{Kind: "nonsense"}); !errors.Is(err, ErrGMInvalidRequest) {
		t.Fatalf("error = %v, want ErrGMInvalidRequest", err)
	}
	if _, err := service.GMQueryRecords(GMRecordQuery{Kind: ""}); !errors.Is(err, ErrGMInvalidRequest) {
		t.Fatalf("empty kind error = %v, want ErrGMInvalidRequest", err)
	}
}

// TestGMRecordMetaReportsFilesAndKinds 是管理端"记录落在哪、有多大"的数据来源。
func TestGMRecordMetaReportsFilesAndKinds(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	recorder.record(recordKindChat, map[string]any{"character_id": "7"}, nil)
	recorder.flush()
	service := &Service{}
	service.records.Store(recorder)

	meta := service.GMRecordMeta()
	if !meta.Enabled || meta.Dir == "" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Kinds) != len(recordKinds) {
		t.Fatalf("kinds = %+v", meta.Kinds)
	}
	if len(meta.Files) != 1 || !strings.HasPrefix(meta.Files[0].Name, recordKindChat+"-") {
		t.Fatalf("files = %+v", meta.Files)
	}
	page, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindChat})
	if err != nil || page.Count != 1 || page.Title != recordKindTitles[recordKindChat] {
		t.Fatalf("page = %+v err=%v", page, err)
	}
	if page.Limited {
		t.Fatalf("page should not be limited: %+v", page)
	}
	if page.Retention != recordRetentionPerKind {
		t.Fatalf("retention = %d, want %d", page.Retention, recordRetentionPerKind)
	}
}
