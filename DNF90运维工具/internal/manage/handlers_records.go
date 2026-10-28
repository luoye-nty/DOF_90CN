//go:build paid

// 记录查询端点（/manage/v1/records/*）：点券消费记录 / 对话记录 / 副本记录。
//
// 记录本体落在 DNF90Server 进程侧（JSONL 文件，见 go-server 的 activity_record.go），
// 这里只做转发：管理工具 →（HTTPS + X-Manage-Token）→ 本端点 →（回环 /dnf/gm/records/*）
// → DNF90Server。信封与鉴权头由 Bridge.AcctGMTCall 统一处理。
//
// 两个端点都是只读的：查记录不改任何运行态，所以审计里记的是"谁在什么时候查了
// 哪一类记录、用什么条件"，而不是"改了什么"。
package manage

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// GMT Tool 操作 ID：与 go-server gm_tool_admin.go 注册时的字面量一致。
const (
	gmtOpRecordsQuery = "POST /dnf/gm/records/query"
	gmtOpRecordsMeta  = "GET /dnf/gm/records/meta"
)

// 与服务端一致的记录类别与查询上限。
//
// ⚠️ 这份清单是**硬编码白名单**，必须与 go-server 的 activity_record.go(recordKinds) 同步。
// 团本记录（raid）当初就是服务端已经写了一整类、这里没跟，导致界面上选都选不到 ——
// 所以白名单改成按 recordKindAllowed 集中判断，加类别时只改这一处。
const (
	recordKindCera    = "cera"
	recordKindChat    = "chat"
	recordKindDungeon = "dungeon"
	// 团本记录（raid_log.go 写入）。
	recordKindRaid = "raid"
	// 金币寄售 / 拍卖行（2026-09 新增；此前只有点券消费里的一条 source=gold_consign）。
	recordKindGoldConsign = "gold_consign"
	recordKindAuction     = "auction"
	// 场景加载诊断（选人选场景时"引导包被推迟"这类高频事件）。
	// 2026-09 从 raid 类别里拆出来：它们把真正的团本事件淹没了，还会按文件轮转把团本记录挤出去。
	recordKindScene = "scene"
	// recordsMaxLimit 与服务端 activity_record.go 的 queryRecords 上限一致。
	recordsMaxLimit = 2000
	// recordsMaxOffset 与服务端 gm_tool_admin.go 的 gmRecordMaxOffset 一致。
	recordsMaxOffset = 100000
)

// recordKindAllowed 判断类别是否放行（白名单，大小写不敏感）。
func recordKindAllowed(kind string) bool {
	switch kind {
	case recordKindCera, recordKindChat, recordKindDungeon,
		recordKindRaid, recordKindGoldConsign, recordKindAuction, recordKindScene:
		return true
	}
	return false
}

// recordKindHint 白名单校验失败时的提示（把允许值写全，免得运营对着界面猜）。
const recordKindHint = "bad-request: kind 只能是 cera / chat / dungeon / raid / gold_consign / auction / scene"

// POST /manage/v1/records/query
//
// 入参：{kind, limit, offset, keyword, character_id, character_name, account_id, since, until}
//   - kind：cera(点券消费) / chat(对话) / dungeon(副本) / raid(团本) /
//     gold_consign(金币寄售) / auction(拍卖行) / scene(场景加载诊断)，必填；
//   - since/until：RFC3339 或 "2006-01-02 15:04:05" / "2006-01-02"，留空表示不限；
//   - limit：1..2000（单次返回上限，超过按 2000 处理），默认 200；
//   - offset：跳过最新的多少条命中，用于翻页取"全部记录"。
//
// 返回服务端原样透传的 GMTRecordPage（records 已按时间倒序，带 offset 与 limited）。
func (s *Server) handleRecordsQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		Kind          string `json:"kind"`
		Limit         int    `json:"limit"`
		Offset        int    `json:"offset"`
		Keyword       string `json:"keyword"`
		CharacterID   string `json:"character_id"`
		CharacterName string `json:"character_name"`
		AccountID     string `json:"account_id"`
		Since         string `json:"since"`
		Until         string `json:"until"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Keyword = strings.TrimSpace(req.Keyword)
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	req.CharacterName = strings.TrimSpace(req.CharacterName)
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Since = strings.TrimSpace(req.Since)
	req.Until = strings.TrimSpace(req.Until)
	if !recordKindAllowed(req.Kind) {
		writeErr(w, http.StatusBadRequest, recordKindHint)
		return
	}
	if req.Limit < 0 || req.Limit > recordsMaxLimit {
		writeErr(w, http.StatusBadRequest, "bad-request: limit 需要在 1~2000 之间")
		return
	}
	if req.Offset < 0 || req.Offset > recordsMaxOffset {
		writeErr(w, http.StatusBadRequest, "bad-request: offset 需要在 0~100000 之间")
		return
	}
	if len(req.Keyword) > 128 || len(req.CharacterName) > 64 || len(req.AccountID) > 128 || len(req.CharacterID) > 32 {
		writeErr(w, http.StatusBadRequest, "bad-request: 查询条件过长")
		return
	}
	audit.fields["kind"] = req.Kind
	audit.fields["limit"] = req.Limit
	if req.Offset > 0 {
		audit.fields["offset"] = req.Offset
	}
	if req.CharacterID != "" {
		audit.fields["character_id"] = req.CharacterID
	}
	if req.CharacterName != "" {
		audit.fields["character_name"] = req.CharacterName
	}
	if req.AccountID != "" {
		audit.fields["account_id"] = req.AccountID
	}
	if req.Keyword != "" {
		audit.fields["keyword"] = req.Keyword
	}
	if req.Since != "" {
		audit.fields["since"] = req.Since
	}
	if req.Until != "" {
		audit.fields["until"] = req.Until
	}

	// 账号条件允许直接填用户名：管理工具那边框里写的是「账号ID 或 用户名」。
	// 解析只在"库真出错"时报错；查不到会原样返回（0 条结果本身就是反馈）。
	if req.AccountID != "" && s.bridge != nil && s.bridge.ResolveAccountInput != nil {
		resolved, err := s.bridge.ResolveAccountInput(r.Context(), req.AccountID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "账号解析失败: "+err.Error())
			return
		}
		if resolved != "" {
			req.AccountID = resolved
			audit.fields["account_id_resolved"] = resolved
		}
	}

	body := map[string]any{"kind": req.Kind}
	if req.Limit > 0 {
		body["limit"] = req.Limit
	}
	if req.Offset > 0 {
		body["offset"] = req.Offset
	}
	if req.Keyword != "" {
		body["keyword"] = req.Keyword
	}
	if req.CharacterID != "" {
		body["character_id"] = req.CharacterID
	}
	if req.CharacterName != "" {
		body["character_name"] = req.CharacterName
	}
	if req.AccountID != "" {
		body["account_id"] = req.AccountID
	}
	if req.Since != "" {
		body["since"] = req.Since
	}
	if req.Until != "" {
		body["until"] = req.Until
	}

	var page GMTRecordPage
	if err := s.gmtCall(r.Context(), http.MethodPost, "/dnf/gm/records/query", gmtOpRecordsQuery, body, &page); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.fields["count"] = page.Count
	audit.ok()

	// 把这一页里出现过的 account_id 批量翻成真实账号名，一起回给管理工具。
	// 翻译失败不影响查询本身：管理工具会退回显示原始主键（不能让"翻译不了"变成"看不见记录"）。
	result := RecordsQueryResult{GMTRecordPage: page}
	if s.bridge != nil && s.bridge.AccountNamesByID != nil {
		if ids := recordAccountIDs(page.Records); len(ids) > 0 {
			names, err := s.bridge.AccountNamesByID(r.Context(), ids)
			if err != nil {
				s.log.Warn("records account names failed", "account_ids", len(ids), "error", err)
			} else {
				result.AccountNames = names
			}
		}
	}
	writeOK(w, result)
}

// recordsAccountLookupMax 单次批量翻译的账号数上限（一页最多 2000 条记录）。
const recordsAccountLookupMax = 2000

// recordAccountIDs 收集这一页记录里出现过的账号主键（去重、忽略空值）。
func recordAccountIDs(records []map[string]any) []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, entry := range records {
		raw, ok := entry["account_id"]
		if !ok {
			continue
		}
		id, _ := raw.(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= recordsAccountLookupMax {
			break
		}
	}
	return out
}

// POST /manage/v1/records/meta
//
// 返回记录系统自身的状态：目录、三类记录的可用性、当前文件清单与写入计数。
// 管理工具用它回答"记录到底有没有在写、写到哪去了"。
func (s *Server) handleRecordsMeta(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: timeNow(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	// 这个端点没有入参，允许空请求体（只做"读一下"的调用方不必特意构造 {}）。
	// 有请求体时仍按 JSON 校验，避免把明显写错的东西当成合法调用记进审计。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 读取请求体失败")
		return
	}
	audit.body = raw
	if len(bytes.TrimSpace(raw)) > 0 {
		var body struct{}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad-request: JSON 解析失败: "+err.Error())
			return
		}
	}
	var meta GMTRecordMeta
	if err := s.gmtCall(r.Context(), http.MethodGet, "/dnf/gm/records/meta", gmtOpRecordsMeta, nil, &meta); err != nil {
		s.gmtWriteError(w, audit, err)
		return
	}
	audit.fields["enabled"] = meta.Enabled
	audit.fields["file_count"] = len(meta.Files)
	audit.ok()
	writeOK(w, meta)
}
