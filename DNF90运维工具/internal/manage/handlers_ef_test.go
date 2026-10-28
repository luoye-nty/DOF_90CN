//go:build paid

// 定时公告（E）与服务器公告栏（F）的 manage 端点契约测试。
//
// 为什么值得单测（这两条链路的"接口形状"改了以后没人会立刻发现）：
//   - 管理工具是按 {"items":[...]} / data 数组的形状解析的，字段名一变就整页空白；
//   - 服务端校验失败必须回 400 + 原因原文（管理工具会把这段文字直接弹给运营看），
//     如果哪天被改成 500 或吞掉原因，运营只会看到"保存失败"却不知道哪一行写错了；
//   - 路由注册一旦漏掉，管理工具会拿到 404，而这在编译期完全看不出来。
//
// 测试直接打 mux（绕过 X-Manage-Token 校验），聚焦"路由 + 入参解析 + 出参形状 + 状态码"。

package manage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

// newTestServer 造一个只带桥接的最小管理服务（不启动监听、不校验证书）。
func newTestServer(t *testing.T, bridge *Bridge) *Server {
	t.Helper()
	cfg := &config.Config{Manage: &config.ManageConfig{}}
	s, err := NewServer(cfg, bridge, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewServer 失败: %v", err)
	}
	return s
}

// doMux 直接向 mux 发请求（跳过 token 校验），返回状态码与响应体。
func doMux(t *testing.T, s *Server, method, path, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// decodeEnvelope 解析 {ok, error, data} 信封。
func decodeEnvelope(t *testing.T, raw string) (bool, string, json.RawMessage) {
	t.Helper()
	var env struct {
		OK    bool            `json:"ok"`
		Error string          `json:"error"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（原文 %s）", err, raw)
	}
	return env.OK, env.Error, env.Data
}

func TestAnnounceScheduleEndpoints(t *testing.T) {
	var applied []config.ScheduledAnnouncement
	bridge := &Bridge{
		AnnounceScheduleStatus: func() []ScheduledAnnouncementStatus {
			return []ScheduledAnnouncementStatus{{
				ScheduledAnnouncement: config.ScheduledAnnouncement{
					ID: "ann-1", Content: "今晚 8 点双倍经验", Color: "0xFF0000FF",
					Enabled: true, Mode: "daily", Time: "20:00", LastRun: "2026-09-24",
				},
				NextRun: "2026-09-25T20:00:00+08:00",
			}}
		},
		AnnounceScheduleApply: func(ctx context.Context, items []config.ScheduledAnnouncement) ([]ScheduledAnnouncementStatus, error) {
			applied = items
			out := make([]ScheduledAnnouncementStatus, 0, len(items))
			for _, it := range items {
				out = append(out, ScheduledAnnouncementStatus{ScheduledAnnouncement: it, NextRun: "2026-09-25T20:00:00+08:00"})
			}
			return out, nil
		},
	}
	s := newTestServer(t, bridge)

	// GET：列表 + 服务端算好的 next_run。
	code, raw := doMux(t, s, http.MethodGet, "/manage/v1/announce-schedule", "")
	if code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok || errMsg != "" {
		t.Fatalf("GET 应 ok=true，实际 ok=%v error=%q", ok, errMsg)
	}
	var got []ScheduledAnnouncementStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("GET data 不是数组: %v（原文 %s）", err, string(data))
	}
	if len(got) != 1 || got[0].ID != "ann-1" || got[0].NextRun == "" {
		t.Fatalf("GET 返回内容不对: %+v", got)
	}

	// POST：整批保存，{"items":[...]} 必须被正确解析（字段名写错就会丢内容）。
	body := `{"items":[{"content":"维护公告","mode":"once","time":"09:00","date":"2026-10-01","enabled":true},
	                    {"id":"ann-1","content":"每天公告","mode":"daily","time":"20:00","enabled":false}]}`
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/announce-schedule", body)
	if code != http.StatusOK {
		t.Fatalf("POST 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if len(applied) != 2 {
		t.Fatalf("桥接收到的条目数 = %d，期望 2", len(applied))
	}
	if applied[0].Content != "维护公告" || applied[0].Date != "2026-10-01" || applied[0].Mode != "once" {
		t.Fatalf("第 1 条解析错误: %+v", applied[0])
	}
	if applied[1].ID != "ann-1" || applied[1].Enabled {
		t.Fatalf("第 2 条解析错误: %+v", applied[1])
	}

	// POST 校验失败：必须 400 + 原因原文（管理工具直接展示这段文字）。
	bridge.AnnounceScheduleApply = func(ctx context.Context, items []config.ScheduledAnnouncement) ([]ScheduledAnnouncementStatus, error) {
		return nil, errors.New("第 1 条：公告内容不能为空")
	}
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/announce-schedule", `{"items":[{"content":"","mode":"daily","time":"20:00"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("校验失败应回 400，实际 %d（响应 %s）", code, raw)
	}
	ok, errMsg, _ = decodeEnvelope(t, raw)
	if ok || !strings.Contains(errMsg, "公告内容不能为空") {
		t.Fatalf("校验失败应回 ok=false + 原因原文，实际 ok=%v error=%q", ok, errMsg)
	}

	// 非法 JSON 请求体：400，且不能 panic。
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/announce-schedule", `{items:bad}`)
	if code != http.StatusBadRequest {
		t.Fatalf("坏请求体应回 400，实际 %d（响应 %s）", code, raw)
	}
}

func TestNoticesEndpoints(t *testing.T) {
	saved := []store.LauncherNotice{
		{ID: 1, Title: "9 月 24 日例行维护", NoticeDate: "2026-09-24", Enabled: true},
	}
	var replaced []store.LauncherNotice
	bridge := &Bridge{
		NoticesList: func(ctx context.Context) ([]store.LauncherNotice, error) {
			return saved, nil
		},
		NoticesReplace: func(ctx context.Context, items []store.LauncherNotice) ([]store.LauncherNotice, error) {
			replaced = items
			return items, nil
		},
	}
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodGet, "/manage/v1/notices", "")
	if code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("GET 应 ok=true，实际 error=%q", errMsg)
	}
	var list []store.LauncherNotice
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("GET data 不是数组: %v", err)
	}
	if len(list) != 1 || list[0].Title != "9 月 24 日例行维护" || !list[0].Enabled {
		t.Fatalf("GET 返回内容不对: %+v", list)
	}

	body := `{"items":[{"title":"国庆活动开启","notice_date":"2026-10-01","enabled":true},
	                    {"title":"已下架的公告","enabled":false}]}`
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/notices", body)
	if code != http.StatusOK {
		t.Fatalf("POST 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if len(replaced) != 2 || replaced[0].Title != "国庆活动开启" || replaced[0].NoticeDate != "2026-10-01" || replaced[1].Enabled {
		t.Fatalf("桥接收到的条目解析错误: %+v", replaced)
	}

	// 服务端拒绝（标题为空 / 日期非法）：400 + 原因。
	bridge.NoticesReplace = func(ctx context.Context, items []store.LauncherNotice) ([]store.LauncherNotice, error) {
		return nil, errors.New("notice: title required")
	}
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/notices", `{"items":[{"title":""}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("校验失败应回 400，实际 %d（响应 %s）", code, raw)
	}
	ok, errMsg, _ = decodeEnvelope(t, raw)
	if ok || !strings.Contains(errMsg, "title required") {
		t.Fatalf("校验失败应回 ok=false + 原因，实际 ok=%v error=%q", ok, errMsg)
	}
}

// TestNewRoutesRegistered 钉住本轮新增的 4 条路由确实注册在 mux 上
// （漏注册只会在管理工具侧表现成 404，编译期发现不了）。
func TestNewRoutesRegistered(t *testing.T) {
	bridge := &Bridge{
		AnnounceScheduleStatus: func() []ScheduledAnnouncementStatus { return nil },
		AnnounceScheduleApply: func(ctx context.Context, items []config.ScheduledAnnouncement) ([]ScheduledAnnouncementStatus, error) {
			return nil, nil
		},
		NoticesList: func(ctx context.Context) ([]store.LauncherNotice, error) { return nil, nil },
		NoticesReplace: func(ctx context.Context, items []store.LauncherNotice) ([]store.LauncherNotice, error) {
			return nil, nil
		},
	}
	s := newTestServer(t, bridge)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/manage/v1/announce-schedule"},
		{http.MethodPost, "/manage/v1/announce-schedule"},
		{http.MethodGet, "/manage/v1/notices"},
		{http.MethodPost, "/manage/v1/notices"},
	}
	for _, c := range cases {
		code, raw := doMux(t, s, c.method, c.path, "")
		if code == http.StatusNotFound {
			t.Fatalf("%s %s 未注册（404）", c.method, c.path)
		}
		if code == http.StatusServiceUnavailable {
			t.Fatalf("%s %s 桥接未接上: %s", c.method, c.path, raw)
		}
	}
}
