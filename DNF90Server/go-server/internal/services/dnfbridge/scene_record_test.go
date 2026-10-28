package dnfbridge

// 「场景加载诊断」（records/scene-*.jsonl）的回归测试。
//
// 这个类别是 2026-09-30 从 raid 类别里搬出来的。服主实测反馈：「记录查询 → 团本记录」
// 里几乎全是同一个角色的「场景尾巴包被推迟」（每秒好几条、result 在 sent/deferred
// 之间跳），真正的团本事件被淹得翻不到。这些 scene_tail_deferred 一族是**选人选场景/
// 进图**时的引导包诊断，与团本毫无关系；更严重的是量极大，会把 raid-*.jsonl 迅速
// 顶到记录器的轮转上限，把真正的团本记录挤出"每类保留最近 14 个文件"的保留窗口。
//
// 这里锁住三件事（也正是这次修正的全部内容）：
//
//  1. **类别登记齐全**：scene 在 recordKinds 里（且在最后，顺序 = UI 下拉顺序），
//     recordKindTitles 有中文名，isRecordKind("scene") 为真；
//  2. **落盘落对文件**：走真实的推迟路径产生的记录只出现在 scene-*.jsonl，
//     raid-*.jsonl 里一条都没有、文件根本不生成 —— 这是服主看到的问题本身；
//  3. **身份字段是字符串**：运维工具只认字符串型 account_id / character_id，
//     数字型会被静默忽略、账号名翻译随之失效（与 gold_consign/auction 同一口径）。
//
// 注意本测试**不**断言 scene_tail_deferred 不再写 packet_log：logGameEvent 那一条
// 必须原样保留，因为 packet_log 在高负载下实测会丢约 25% 的行，这批诊断当初就是
// 为了排查"卡在加载"补的，搬家的只是记录类别，不是删诊断。

import (
	"path/filepath"
	"strings"
	"testing"
)

// sceneRecordKindIsLast 断言 scene 是 recordKinds 的最后一项。
//
// 之所以单独提出来断言位置：recordKinds 的顺序就是管理端「记录查询」下拉的顺序，
// 父 agent 那边的下拉会同步这个顺序；插到中间会让运营已经找惯的团本/交易类别挪位。
func sceneRecordKindPosition(t *testing.T) int {
	t.Helper()
	for index, kind := range recordKinds {
		if kind == recordKindScene {
			return index
		}
	}
	t.Fatalf("recordKinds 里没有 %q: %v", recordKindScene, recordKinds)
	return -1
}

// TestRecordKindSceneIsRegistered 覆盖"类别本身登记齐全"这一层。
func TestRecordKindSceneIsRegistered(t *testing.T) {
	if recordKindScene != "scene" {
		t.Fatalf("recordKindScene = %q, want %q", recordKindScene, "scene")
	}
	index := sceneRecordKindPosition(t)
	if index != len(recordKinds)-1 {
		t.Fatalf("scene 在 recordKinds 的位置 = %d（共 %d 项），want 最后一项 %d: %v",
			index, len(recordKinds), len(recordKinds)-1, recordKinds)
	}
	if !isRecordKind(recordKindScene) {
		t.Fatalf("isRecordKind(%q) = false，记录会因未知类别被静默丢弃", recordKindScene)
	}
	if title := recordKindTitles[recordKindScene]; title != "场景加载诊断" {
		t.Fatalf("recordKindTitles[%q] = %q, want %q", recordKindScene, title, "场景加载诊断")
	}
	// 团本类别的标题与登记不能被这次搬家碰到。
	if title := recordKindTitles[recordKindRaid]; title != "团本记录" {
		t.Fatalf("recordKindTitles[%q] = %q, want %q", recordKindRaid, title, "团本记录")
	}
}

// TestGMRecordMetaExposesSceneKind 是管理端类别下拉的数据来源：
// GMRecordMeta 必须把 scene 连同中文名一起暴露出去，否则前端根本选不到这个类别。
func TestGMRecordMetaExposesSceneKind(t *testing.T) {
	service := &Service{}
	meta := service.GMRecordMeta()
	found := false
	for _, info := range meta.Kinds {
		if info.Kind != recordKindScene {
			continue
		}
		found = true
		if info.Title != "场景加载诊断" {
			t.Fatalf("scene 类别标题 = %q, want %q", info.Title, "场景加载诊断")
		}
	}
	if !found {
		t.Fatalf("GMRecordMeta 没有暴露 scene 类别: %+v", meta.Kinds)
	}
	if len(meta.Kinds) != len(recordKinds) {
		t.Fatalf("暴露的类别数 = %d, want %d", len(meta.Kinds), len(recordKinds))
	}

	// 新类别必须能直接查询，而不是被 GMQueryRecords 当成未知类别拒掉。
	recorder, _ := newTestRecorder(t)
	service.records.Store(recorder)
	page, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindScene})
	if err != nil {
		t.Fatalf("GMQueryRecords(scene) = %v", err)
	}
	if page.Title != "场景加载诊断" {
		t.Fatalf("查询回显标题 = %q, want %q", page.Title, "场景加载诊断")
	}
}

// sceneRecordIdentityString 取一个必须是字符串的字段（口径同 gold_consign 测试）。
func sceneRecordIdentityString(t *testing.T, entry map[string]any, key string) string {
	t.Helper()
	value, ok := entry[key].(string)
	if !ok {
		t.Fatalf("字段 %s 落盘类型 = %T (%v), want string（数字型会被运维工具静默忽略）",
			key, entry[key], entry[key])
	}
	if value == "" {
		t.Fatalf("字段 %s 是空串（记录里必须有人）: %+v", key, entry)
	}
	return value
}

// TestSceneTailDeferredRecordsLandInSceneNotRaid 是本任务的核心断言，
// 复现的正是服主截图里的那两行（result=deferred 与 result=sent）。
//
// 用真实的推迟路径（logDeferredSceneTail / sendDeferredSelectSceneTail），
// 而不是直接调 recordScene：要证明的是"调用点搬对了"，直接调新函数证明不了这一点。
func TestSceneTailDeferredRecordsLandInSceneNotRaid(t *testing.T) {
	service, session := newDeferredSceneRecordFixture(t)
	recorder := service.records.Load()
	if recorder == nil {
		t.Fatal("测试夹具没有装上记录器")
	}
	dir := recorder.Dir()

	// ① 推迟那一条（对应截图里的 deferred 行）。
	service.logDeferredSceneTail(session, "test_scene_tail_deferred", "wait_for_selected_actor_and_scene_transition_owner")
	// ② 最终发出那一条（对应截图里的 sent 行）。这条走 sendDeferredSelectSceneTail，
	// 是另一个调用点（recordScene(session, "scene_tail_deferred", {...result:"sent"})）。
	if err := service.sendDeferredSelectSceneTail(session, "test_scene_tail_sent"); err != nil {
		t.Fatalf("sendDeferredSelectSceneTail: %v", err)
	}
	recorder.flush()

	sceneFiles, err := filepath.Glob(filepath.Join(dir, recordKindScene+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob scene: %v", err)
	}
	if len(sceneFiles) == 0 {
		t.Fatalf("没有生成 %s-*.jsonl（目录 %s）", recordKindScene, dir)
	}
	entries := readRecordFile(t, dir, recordKindScene)
	if len(entries) != 2 {
		t.Fatalf("scene 记录条数 = %d, want 2（deferred 与 sent 各一条）: %+v", len(entries), entries)
	}

	results := map[string]map[string]any{}
	for _, entry := range entries {
		if entry["kind"] != recordKindScene {
			t.Fatalf("kind = %v, want %q", entry["kind"], recordKindScene)
		}
		if entry["event"] != "scene_tail_deferred" {
			t.Fatalf("event = %v, want scene_tail_deferred: %+v", entry["event"], entry)
		}
		// 概要列显示的中文名与搬家前保持一致（同一个事件只是换了文件）。
		if entry["event_name"] != "场景尾巴包被推迟" {
			t.Fatalf("event_name = %v, want 场景尾巴包被推迟: %+v", entry["event_name"], entry)
		}
		result, _ := entry["result"].(string)
		results[result] = entry
	}
	if _, ok := results["deferred"]; !ok {
		t.Fatalf("缺少 result=deferred 的记录: %+v", entries)
	}
	if _, ok := results["sent"]; !ok {
		t.Fatalf("缺少 result=sent 的记录: %+v", entries)
	}

	// ③ 最关键的一条：raid-*.jsonl 里一条都不能有，文件都不该生成。
	raidFiles, err := filepath.Glob(filepath.Join(dir, recordKindRaid+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob raid: %v", err)
	}
	if len(raidFiles) != 0 {
		t.Fatalf("产生了 %s 文件（引导包诊断不该再占用团本的轮转窗口）: %v",
			recordKindRaid, raidFiles)
	}
	if raid := readRecordFile(t, dir, recordKindRaid); len(raid) != 0 {
		t.Fatalf("场景尾巴包记录落进了团本类别: %+v", raid)
	}

	// ④ 身份字段必须是字符串，且连接侧诊断字段一个都不能丢。
	for result, entry := range results {
		if account := sceneRecordIdentityString(t, entry, "account_id"); account != "dnf:1" {
			t.Fatalf("result=%s 的 account_id = %q, want dnf:1", result, account)
		}
		if character := sceneRecordIdentityString(t, entry, "character_id"); character != "29" {
			t.Fatalf("result=%s 的 character_id = %q, want 29", result, character)
		}
		// conn_id / character_generation / session_online 是搬家前就在记录里的
		// 排障字段（尤其 session_online 是判断"玩家还在不在线"的第一手依据），
		// 新包装函数必须原样带上。
		for _, key := range []string{"conn_id", "character_generation", "session_online"} {
			if _, present := entry[key]; !present {
				t.Fatalf("result=%s 的记录丢了字段 %s（搬家不该丢信息）: %+v", result, key, entry)
			}
		}
	}

	// ⑤ "推迟"那一条还要带上完整的等待状态：packet_log 会丢行，这些字段是
	// "到底卡在哪一个门"的唯一证据，搬家不能把它们弄丢。
	//
	// 只断言 deferred 这一条：sent 那一条的字段集合是调用点显式写死的
	// （packet_count / resume_packet_index / ...），本来就没有 tail_deferred，
	// 这里不替它发明字段。
	deferredEntry := results["deferred"]
	if _, present := deferredEntry["tail_deferred"]; !present {
		t.Fatalf("deferred 记录缺少 tail_deferred 等待状态: %+v", deferredEntry)
	}
	if _, present := deferredEntry["route_stage"]; !present {
		t.Fatalf("deferred 记录缺少 route_stage 等待状态: %+v", deferredEntry)
	}
	if deferredEntry["source"] != "test_scene_tail_deferred" {
		t.Fatalf("deferred 记录 source = %v, want test_scene_tail_deferred", deferredEntry["source"])
	}
	if sentEntry := results["sent"]; sentEntry["packet_count"] != float64(1) {
		t.Fatalf("sent 记录 packet_count = %v, want 1（本事件自己的字段不能丢）: %+v",
			sentEntry["packet_count"], sentEntry)
	}
}

// TestRecordSceneIsRejectedByRaidQuery 从检索侧再确认一次搬家生效：
// 同一条诊断既不在团本记录里，也能在场景诊断类别里查到。
func TestRecordSceneIsRejectedByRaidQuery(t *testing.T) {
	service, session := newDeferredSceneRecordFixture(t)
	recorder := service.records.Load()
	service.logDeferredSceneTail(session, "test_scene_query", "wait_for_current_exe_legacy_type1345_u32_2_scene_ready")
	recorder.flush()

	scenePage, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindScene})
	if err != nil {
		t.Fatalf("查询 scene: %v", err)
	}
	if scenePage.Count != 1 {
		t.Fatalf("scene 查询命中 = %d, want 1: %+v", scenePage.Count, scenePage.Records)
	}
	raidPage, err := service.GMQueryRecords(GMRecordQuery{Kind: recordKindRaid})
	if err != nil {
		t.Fatalf("查询 raid: %v", err)
	}
	if raidPage.Count != 0 {
		t.Fatalf("团本记录里出现引导包诊断 = %d 条: %+v", raidPage.Count, raidPage.Records)
	}
}

// TestRecordSceneCarriesSessionFieldsWithoutService 覆盖"记录器未启用"这条路径：
// recordScene 必须直接空转，而不是 panic（埋点都在玩法主路径上）。
func TestRecordSceneCarriesSessionFieldsWithoutService(t *testing.T) {
	var service *Service
	service.recordScene(nil, "scene_tail_deferred", nil)

	service = &Service{}
	service.recordScene(nil, "scene_tail_deferred", map[string]any{"result": "deferred"})
	service.recordScene(&gameSession{conn: &bufferConn{}}, "scene_tail_deferred", nil)
}

// newDeferredSceneRecordFixture 造一个"能真的走通引导包推迟/发出两条路"的会话，
// 并装上临时记录器。
//
// 门禁状态直接复用既有的 newDeferredTailResumeFixture
// （game_select_scene_deferred_tail_retry_test.go）：只有那一整套 flag 都在位，
// sendDeferredSelectSceneTail 才会走到"真正发出去"的那条记录点。这里刻意不再抄一份
// 夹具——夹具一旦漂移，本测试就会变成"路径根本没走到，却因为记录条数是 0 而误报"。
func newDeferredSceneRecordFixture(t *testing.T) (*Service, *gameSession) {
	t.Helper()
	service, session := newDeferredTailResumeFixture(t)
	recorder, _ := newTestRecorder(t)
	service.records.Store(recorder)

	// 记录里必须有人：accountID 由登录阶段绑定，测试夹具不经过登录，这里显式补上。
	session.accountID = "dnf:1"
	return service, session
}

// sceneRecordFileNamesStartingWith 是给"文件真的按类别分开"用的名字断言助手。
func sceneRecordFileNamesStartingWith(t *testing.T, dir, prefix string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.jsonl"))
	if err != nil {
		t.Fatalf("glob %s: %v", prefix, err)
	}
	names := make([]string, 0, len(matches))
	for _, path := range matches {
		names = append(names, filepath.Base(path))
	}
	return names
}

// TestSceneRecordFileIsNamedScene 直接锁住文件名前缀：类别名一旦写错，
// 管理端的文件名过滤（kind 取 '-' 之前那一段）就会把这个类别的文件归错档。
func TestSceneRecordFileIsNamedScene(t *testing.T) {
	recorder, dir := newTestRecorder(t)
	recorder.record(recordKindScene, map[string]any{"event": "scene_tail_deferred"}, nil)
	recorder.flush()

	names := sceneRecordFileNamesStartingWith(t, dir, recordKindScene)
	if len(names) != 1 {
		t.Fatalf("scene 文件 = %v, want 恰好一个", names)
	}
	if !strings.HasPrefix(names[0], "scene-") || !strings.HasSuffix(names[0], ".jsonl") {
		t.Fatalf("scene 文件名 = %q, want scene-YYYYMMDD-NNN.jsonl", names[0])
	}
	if leftovers := sceneRecordFileNamesStartingWith(t, dir, recordKindRaid); len(leftovers) != 0 {
		t.Fatalf("写 scene 却生成了 raid 文件: %v", leftovers)
	}
}
