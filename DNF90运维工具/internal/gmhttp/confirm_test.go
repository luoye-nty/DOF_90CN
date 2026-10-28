package gmhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOperationIDMatchesServer 锁住与游戏服 AdminOperationID 的拼法一致。
func TestOperationIDMatchesServer(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"POST", "/local/dnf-gm/mail", "POST /local/dnf-gm/mail"},
		{"post", "/local/dnf-gm/wallet/update", "POST /local/dnf-gm/wallet/update"},
		{"DELETE", "/local/dnf-gm/character/black-diamond", "DELETE /local/dnf-gm/character/black-diamond"},
		{"", "", "GET /"},
	}
	for _, item := range cases {
		if got := OperationID(item.method, item.path); got != item.want {
			t.Fatalf("OperationID(%q,%q) = %q, want %q", item.method, item.path, got, item.want)
		}
	}
}

// TestSetConfirmOnlyOnWrites 读请求不该被加上确认头（服务端不校验，加了反而误导排查）。
func TestSetConfirmOnlyOnWrites(t *testing.T) {
	get := httptest.NewRequest(http.MethodGet, "/local/dnf-gm/characters?account_id=x", nil)
	SetConfirm(get, "ops")
	if got := get.Header.Get(ConfirmHeader); got != "" {
		t.Fatalf("GET 不该带确认头，实际 %q", got)
	}

	post := httptest.NewRequest(http.MethodPost, "/local/dnf-gm/mail", strings.NewReader("{}"))
	SetConfirm(post, "ops")
	if got := post.Header.Get(ConfirmHeader); got != "POST /local/dnf-gm/mail" {
		t.Fatalf("确认头 = %q，want POST /local/dnf-gm/mail", got)
	}
	if got := post.Header.Get(IdempotencyHeader); got == "" {
		t.Fatal("写请求应带幂等键（危险路由要求非空）")
	}
}

// TestSetConfirmIgnoresQueryString 确认头只看路径：带 ?account_id=... 的写请求也算同一条 operation。
func TestSetConfirmIgnoresQueryString(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/local/dnf-gm/character/black-diamond?id=19", nil)
	SetConfirm(req, "ops")
	if got := req.Header.Get(ConfirmHeader); got != "POST /local/dnf-gm/character/black-diamond" {
		t.Fatalf("确认头 = %q（不该带查询串）", got)
	}
}
