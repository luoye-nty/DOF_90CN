package server

// 黑钻发奖的连接层测试：用 httptest 假装 DNF90Server 的原生 GM 口，
// 锁住"工具到底发了什么"——URL、X-Admin-Token/X-Admin-Actor 头、请求体字段名与天数。
//
// 这一层是 CDK 黑钻发奖与「账号管理 → 开启黑钻」共用的最后一跳：
// 字段名或路径写错不会被编译器发现，只会表现为"兑换成功但黑钻没到账"。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dnf90ops/internal/config"
)

// newTestServiceWithInstance 造一个临时服务端根目录（runtime\config\instance.json），
// 让 gmAdminConfig 探测到 httptest 的地址与 token。
func newTestServiceWithInstance(t *testing.T, adminListen, adminToken string) *Service {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "runtime", "config")
	if err := os.MkdirAll(configDir, 0o777); err != nil {
		t.Fatalf("创建临时服务端目录失败: %v", err)
	}
	instance := map[string]any{
		"installationId": "test",
		"database":       map[string]any{"mode": "portable"},
		"server": map[string]any{
			"adminListen":   adminListen,
			"adminToken":    adminToken,
			"channelListen": "127.0.0.1:18112",
		},
	}
	raw, err := json.Marshal(instance)
	if err != nil {
		t.Fatalf("序列化 instance.json 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "instance.json"), raw, 0o666); err != nil {
		t.Fatalf("写 instance.json 失败: %v", err)
	}
	return New(&config.Config{ServerRoot: root}, nil, nil, nil, nil, nil, nil)
}

func TestGMActivateBlackDiamondSendsExpectedRequest(t *testing.T) {
	var (
		gotPath  string
		gotToken string
		gotActor string
		gotBody  map[string]any
	)
	gm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Admin-Token")
		gotActor = r.Header.Get("X-Admin-Actor")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"message":"Black Diamond entitlement committed","data":{`+
			`"character_id":"77","account_id":"acc-1","days":30,"expires_at":1800000000,`+
			`"remaining_seconds":2592000,"fatigue":12,"fatigue_limit":188,"online":false,"refreshed":false}}`)
	}))
	defer gm.Close()

	service := newTestServiceWithInstance(t, strings.TrimPrefix(gm.URL, "http://"), "test-token")
	var out gmBlackDiamondResult
	if err := service.gmActivateBlackDiamond(context.Background(), " 77 ", 30, &out); err != nil {
		t.Fatalf("gmActivateBlackDiamond 失败: %v", err)
	}

	if gotPath != "/local/dnf-gm/character/black-diamond" {
		t.Fatalf("请求路径 = %q", gotPath)
	}
	if gotToken != "test-token" || gotActor != "ops" {
		t.Fatalf("请求头不对: token=%q actor=%q", gotToken, gotActor)
	}
	// 服务端 GMBlackDiamondRequest 用 DisallowUnknownFields，字段名必须完全一致。
	if got, want := gotBody["character_id"], "77"; got != want {
		t.Fatalf("character_id = %v, want %v（工具端会先去空白）", got, want)
	}
	if got, want := gotBody["days"], float64(30); got != want {
		t.Fatalf("days = %v, want %v", got, want)
	}
	if len(gotBody) != 2 {
		t.Fatalf("请求体字段数 = %d, want 2（多余的字段会被服务端拒绝）: %+v", len(gotBody), gotBody)
	}
	if out.ExpiresAt != 1800000000 || out.FatigueLimit != 188 || out.Fatigue != 12 || out.Online {
		t.Fatalf("回显解析不对: %+v", out)
	}
}

func TestGMActivateBlackDiamondSurfacesGMFailure(t *testing.T) {
	gm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"message":"","error":"character not found"}`)
	}))
	defer gm.Close()

	service := newTestServiceWithInstance(t, strings.TrimPrefix(gm.URL, "http://"), "test-token")
	err := service.gmActivateBlackDiamond(context.Background(), "999999", 30, nil)
	if err == nil {
		t.Fatal("服务端拒绝时应当返回错误")
	}
	if !strings.Contains(err.Error(), "character not found") {
		t.Fatalf("错误文案应当透传服务端 error: %v", err)
	}
}

// TestGMActivateBlackDiamondRequiresInstance：没探测到 instance.json 时报错，
// 而不是静默成功（静默成功会让运营以为黑钻发了，实际谁也没收到）。
func TestGMActivateBlackDiamondRequiresInstance(t *testing.T) {
	service := New(&config.Config{ServerRoot: t.TempDir()}, nil, nil, nil, nil, nil, nil)
	err := service.gmActivateBlackDiamond(context.Background(), "77", 30, nil)
	if err == nil || !strings.Contains(err.Error(), "instance.json") {
		t.Fatalf("缺少 instance.json 时应当报错并提到 instance.json: %v", err)
	}
}
