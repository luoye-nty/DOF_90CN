//go:build paid

// 账号名批量翻译端点（POST /manage/v1/account/names）的契约测试。
//
// 为什么值得单测：管理工具**所有**显示账号的地方（角色查询/在线角色/背包/兑换流水/
// 封禁/记录）都靠这个接口把 dnf:<sha256> 主键换成真实用户名。它一旦形状变了或路由漏注册：
//   - 管理工具侧表现成 404 或整片空白，运营又只能看到不可逆的哈希；
//   - 而这两件事在编译期都发现不了。
//
// 另外两条契约是"安全阀"，同样容易在后续重构里被删掉：
//   - 传进来的主键必须**先去空去重**再打到库上（管理工具一页可能几百行重复账号）；
//   - 查不到的主键必须进 missing 而不是被塞进 map（否则管理工具会把主键显示成用户名）。

package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAccountNamesEndpointResolvesAndReportsMissing(t *testing.T) {
	var gotIDs []string
	bridge := &Bridge{
		AccountNamesByID: func(ctx context.Context, accountIDs []string) (map[string]string, error) {
			gotIDs = append([]string(nil), accountIDs...)
			return map[string]string{"dnf:abc": "1000000001"}, nil
		},
	}
	s := newTestServer(t, bridge)

	// 混入空串与重复主键：只应把去重后的两个非空主键传给桥接。
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/names",
		`{"account_ids":["dnf:abc","","dnf:abc","  dnf:missing  "]}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if len(gotIDs) != 2 || gotIDs[0] != "dnf:abc" || gotIDs[1] != "dnf:missing" {
		t.Fatalf("传给桥接的主键 = %v，期望 [dnf:abc dnf:missing]（去空去重、trim）", gotIDs)
	}

	ok, errMsg, data := decodeEnvelope(t, raw)
	if !ok {
		t.Fatalf("应 ok=true，实际 error=%q", errMsg)
	}
	var result accountNamesResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("响应解析失败: %v（原文 %s）", err, string(data))
	}
	if name := result.AccountNames["dnf:abc"]; name != "1000000001" {
		t.Fatalf("account_names[dnf:abc] = %q，期望 1000000001", name)
	}
	if _, exists := result.AccountNames["dnf:missing"]; exists {
		t.Fatal("查不到用户名的主键不该出现在 account_names 里（应进 missing）")
	}
	if len(result.Missing) != 1 || result.Missing[0] != "dnf:missing" {
		t.Fatalf("missing = %v，期望 [dnf:missing]", result.Missing)
	}
}

func TestAccountNamesEndpointRejectsEmptyAndOverLimit(t *testing.T) {
	bridge := &Bridge{
		AccountNamesByID: func(ctx context.Context, accountIDs []string) (map[string]string, error) {
			t.Fatal("入参非法时不该打到桥接（库查询）")
			return nil, nil
		},
	}
	s := newTestServer(t, bridge)

	if code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/names", `{"account_ids":["","  "]}`); code != http.StatusBadRequest {
		t.Fatalf("空列表状态码 = %d，期望 400（响应 %s）", code, raw)
	}

	// 上限 + 1 个**互不相同**的主键：去重后仍超限，才会命中上限校验。
	ids := make([]string, 0, accountNamesMaxIDs+1)
	for i := 0; i <= accountNamesMaxIDs; i++ {
		ids = append(ids, fmt.Sprintf("dnf:id%05d", i))
	}
	body, err := json.Marshal(accountNamesRequest{AccountIDs: ids})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/names", string(body)); code != http.StatusBadRequest {
		t.Fatalf("超限状态码 = %d，期望 400（响应 %s）", code, raw)
	}
}

func TestAccountNamesEndpointWithoutBridgeFails(t *testing.T) {
	s := newTestServer(t, &Bridge{})
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/account/names", `{"account_ids":["dnf:abc"]}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（响应 %s）", code, raw)
	}
	if !strings.Contains(raw, "account-names-unavailable") {
		t.Fatalf("响应应带 account-names-unavailable 前缀，实际 %s", raw)
	}
}
