//go:build paid

// 第二批功能（服务端指定 DLL / 记录查询账号名）的端点契约测试。
//
// 这两条链路的"接口形状"错了很难在编译期发现：
//   - 管理工具按 {"items":[...]} 解析、按 data 数组渲染；字段名一变整页空白；
//   - 记录查询的 account_names 是管理工具显示"真实账号"的唯一来源，
//     少了它运营只能看到 dnf:<sha256> 这种主键（而它不可逆）；
//   - 路由漏注册只会在管理工具侧表现成 404。

package manage

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"dnf90ops/internal/store"
)

func dllTestItem(name, sum string) store.LauncherDLL {
	return store.LauncherDLL{FileName: name, SHA256: sum, OnMismatch: store.DLLMismatchSkip, Enabled: true}
}

func TestDLLsEndpoints(t *testing.T) {
	sum := strings.Repeat("ab", 32) // 64 位 hex
	var saved []store.LauncherDLL
	bridge := &Bridge{
		DLLList: func(ctx context.Context) ([]store.LauncherDLL, error) {
			return []store.LauncherDLL{dllTestItem("90CN.dll", sum)}, nil
		},
		DLLReplace: func(ctx context.Context, items []store.LauncherDLL) ([]store.LauncherDLL, error) {
			saved = items
			return items, nil
		},
	}
	s := newTestServer(t, bridge)

	// GET：清单
	code, raw := doMux(t, s, http.MethodGet, "/manage/v1/dlls", "")
	if code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("GET 应 ok=true，实际 error=%q", errMsg)
	}
	var list []store.LauncherDLL
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("GET data 不是数组: %v（原文 %s）", err, string(data))
	}
	if len(list) != 1 || list[0].FileName != "90CN.dll" {
		t.Fatalf("GET 返回内容不对: %+v", list)
	}

	// POST：整表保存（含"拒绝启动"策略），字段必须逐字解析成功
	body := `{"items":[{"file_name":"AntiCheat.dll","sha256":"` + sum + `","on_mismatch":"block","enabled":true},
	                    {"file_name":"Optional.dll","sha256":"` + sum + `","on_mismatch":"skip","enabled":false}]}`
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/dlls", body)
	if code != http.StatusOK {
		t.Fatalf("POST 状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if len(saved) != 2 {
		t.Fatalf("桥接收到的条目数 = %d，期望 2", len(saved))
	}
	if saved[0].FileName != "AntiCheat.dll" || saved[0].OnMismatch != store.DLLMismatchBlock || !saved[0].Enabled {
		t.Fatalf("第 1 条解析错误: %+v", saved[0])
	}
	if saved[1].Enabled {
		t.Fatalf("第 2 条应为停用: %+v", saved[1])
	}

	// 服务端校验失败（文件名带路径 / 哈希不合法）→ 400 + 原因原文
	bridge.DLLReplace = func(ctx context.Context, items []store.LauncherDLL) ([]store.LauncherDLL, error) {
		// 直接用真实校验器产出的错误，保证测试文案与线上一致。
		_, err := store.NormalizeDLLFileName("a/b.dll")
		return nil, err
	}
	code, raw = doMux(t, s, http.MethodPost, "/manage/v1/dlls", `{"items":[{"file_name":"a/b.dll","sha256":"`+sum+`"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("校验失败应回 400，实际 %d（响应 %s）", code, raw)
	}
	if ok, errMsg, _ := decodeEnvelope(t, raw); ok || !strings.Contains(errMsg, "文件名") {
		t.Fatalf("校验失败应回 ok=false + 原因，实际 ok=%v error=%q", ok, errMsg)
	}
}

func TestRecordsQueryResolvesUsernameAndReturnsAccountNames(t *testing.T) {
	var forwarded map[string]any
	bridge := &Bridge{
		AcctGMTCall: func(ctx context.Context, method, path, operation string, body, out any, actor string) error {
			if m, ok := body.(map[string]any); ok {
				forwarded = m
			}
			// 模拟游戏服务端返回一页记录（两条同一个账号）。
			page, ok := out.(*GMTRecordPage)
			if !ok {
				t.Fatalf("out 类型 = %T，期望 *GMTRecordPage", out)
			}
			page.Kind = "dungeon"
			page.Count = 2
			page.Records = []map[string]any{
				{"account_id": "dnf:abc", "character_name": "2222", "elapsed_seconds": float64(192)},
				{"account_id": "dnf:abc", "character_name": "2222", "elapsed_seconds": float64(30)},
				{"account_id": "", "character_name": "匿名"},
			}
			return nil
		},
		ResolveAccountInput: func(ctx context.Context, value string) (string, error) {
			if value == "1000000001" {
				return "dnf:abc", nil
			}
			return value, nil
		},
		AccountNamesByID: func(ctx context.Context, accountIDs []string) (map[string]string, error) {
			// 断言：只把非空且去重后的主键传进来。
			if len(accountIDs) != 1 || accountIDs[0] != "dnf:abc" {
				t.Fatalf("待翻译主键 = %v，期望 [dnf:abc]", accountIDs)
			}
			return map[string]string{"dnf:abc": "1000000001"}, nil
		},
	}
	s := newTestServer(t, bridge)

	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/records/query",
		`{"kind":"dungeon","account_id":"1000000001","limit":10,"since":"2026-09-24 00:00:00","until":"2026-09-24 23:59:59"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}

	// 用户名被解析成账号主键后才转发给游戏服务端
	if forwarded == nil {
		t.Fatal("没有转发到游戏服务端")
	}
	if got := forwarded["account_id"]; got != "dnf:abc" {
		t.Fatalf("转发给服务端的 account_id = %v，期望 dnf:abc（用户名应被解析）", got)
	}
	if got := forwarded["since"]; got != "2026-09-24 00:00:00" {
		t.Fatalf("转发给服务端的 since = %v（时间范围应原样透传）", got)
	}

	// 响应里带上 account_id → 真实账号名 的映射，且记录原样透传
	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("应 ok=true，实际 error=%q", errMsg)
	}
	var result RecordsQueryResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("响应解析失败: %v（原文 %s）", err, string(data))
	}
	if result.Count != 2 || len(result.Records) != 3 {
		t.Fatalf("记录页透传不对: count=%d records=%d", result.Count, len(result.Records))
	}
	if name := result.AccountNames["dnf:abc"]; name != "1000000001" {
		t.Fatalf("account_names[dnf:abc] = %q，期望 1000000001", name)
	}
	if _, exists := result.AccountNames[""]; exists {
		t.Fatal("空账号不该进映射表")
	}
}

func TestRecordAccountIDsDedupesAndSkipsEmpty(t *testing.T) {
	records := []map[string]any{
		{"account_id": "dnf:a"},
		{"account_id": "dnf:a"},
		{"account_id": ""},
		{"account_id": 12345}, // 非字符串：忽略
		{"character_name": "没有账号字段"},
		{"account_id": "  dnf:b  "},
	}
	got := recordAccountIDs(records)
	if len(got) != 2 {
		t.Fatalf("去重后主键数 = %d（%v），期望 2", len(got), got)
	}
	if got[0] != "dnf:a" || got[1] != "dnf:b" {
		t.Fatalf("主键顺序/取值不对: %v", got)
	}
}

// TestRound2RoutesRegistered 钉住本轮新增的 4 条路由（漏注册只表现为 404）。
func TestRound2RoutesRegistered(t *testing.T) {
	bridge := &Bridge{
		DLLList:    func(ctx context.Context) ([]store.LauncherDLL, error) { return nil, nil },
		DLLReplace: func(ctx context.Context, items []store.LauncherDLL) ([]store.LauncherDLL, error) { return nil, nil },
		AcctGMTCall: func(ctx context.Context, method, path, operation string, body, out any, actor string) error {
			return nil
		},
	}
	s := newTestServer(t, bridge)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/manage/v1/dlls", ""},
		{http.MethodPost, "/manage/v1/dlls", `{"items":[]}`},
		{http.MethodPost, "/manage/v1/records/query", `{"kind":"dungeon","limit":1}`},
		{http.MethodPost, "/manage/v1/records/meta", `{}`},
	} {
		code, raw := doMux(t, s, c.method, c.path, c.body)
		if code == http.StatusNotFound {
			t.Fatalf("%s %s 未注册（404）", c.method, c.path)
		}
		if code == http.StatusServiceUnavailable {
			t.Fatalf("%s %s 桥接未接上: %s", c.method, c.path, raw)
		}
	}
}
