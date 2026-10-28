//go:build paid

// 记录类别白名单的契约测试。
//
// 为什么值得单测：白名单是**硬编码**在运维工具里的，而类别清单的真正定义在
// go-server（activity_record.go 的 recordKinds）。两边一旦不同步，表现是
// "服务端明明写了一整类记录，界面上却选不到" —— 团本记录（raid）就是这么被挡住很久的，
// 而这类问题编译期完全看不出来。
//
// 这里锁三件事：
//  1. 服务端已有的六个类别都必须放行（放行 = 真的转发到游戏服务端，而不是被 400 挡下）；
//  2. 类别大小写不敏感（管理工具将来传 DUNGEON 也不该 400）；
//  3. 不认识的类别仍然要 400，且提示里要把允许值列全（白名单不能形同虚设）。

package manage

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// recordKindsForwardingBridge 造一个只记录"转发出去的 body"的桥接。
func recordKindsForwardingBridge(t *testing.T, forwarded *map[string]any) *Bridge {
	t.Helper()
	return &Bridge{
		AcctGMTCall: func(ctx context.Context, method, path, operation string, body, out any, actor string) error {
			if m, ok := body.(map[string]any); ok {
				*forwarded = m
			}
			page, ok := out.(*GMTRecordPage)
			if !ok {
				t.Fatalf("out 类型 = %T，期望 *GMTRecordPage", out)
			}
			page.Kind = "dungeon"
			return nil
		},
		AccountNamesByID: func(ctx context.Context, accountIDs []string) (map[string]string, error) {
			return map[string]string{}, nil
		},
	}
}

func TestRecordsQueryAllowsEveryServerKind(t *testing.T) {
	// 与 go-server/internal/services/dnfbridge/activity_record.go 的 recordKinds 对齐。
	kinds := []string{
		recordKindCera, recordKindChat, recordKindDungeon,
		recordKindRaid, recordKindGoldConsign, recordKindAuction, recordKindScene,
	}
	for _, kind := range kinds {
		var forwarded map[string]any
		s := newTestServer(t, recordKindsForwardingBridge(t, &forwarded))
		code, raw := doMux(t, s, http.MethodPost, "/manage/v1/records/query",
			`{"kind":"`+kind+`","limit":1}`)
		if code != http.StatusOK {
			t.Fatalf("kind=%s 状态码 = %d，期望 200（响应 %s）", kind, code, raw)
		}
		if forwarded == nil {
			t.Fatalf("kind=%s 没有转发到游戏服务端（可能被白名单挡下了）", kind)
		}
		if got := forwarded["kind"]; got != kind {
			t.Fatalf("转发给服务端的 kind = %v，期望 %s", got, kind)
		}
	}
}

func TestRecordsQueryRejectsUnknownKind(t *testing.T) {
	var forwarded map[string]any
	s := newTestServer(t, recordKindsForwardingBridge(t, &forwarded))
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/records/query", `{"kind":"gold","limit":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知类别状态码 = %d，期望 400（响应 %s）", code, raw)
	}
	if forwarded != nil {
		t.Fatal("未知类别不该转发到游戏服务端")
	}
	// 提示里必须把允许值列全：运营看到 400 时要能从这句话知道该填什么。
	_, errMsg, _ := decodeEnvelope(t, raw)
	for _, want := range []string{"cera", "chat", "dungeon", "raid", "gold_consign", "auction", "scene"} {
		if !strings.Contains(errMsg, want) {
			t.Fatalf("错误提示 %q 里应包含允许值 %q", errMsg, want)
		}
	}
}

func TestRecordsQueryKindIsCaseInsensitive(t *testing.T) {
	var forwarded map[string]any
	s := newTestServer(t, recordKindsForwardingBridge(t, &forwarded))
	code, raw := doMux(t, s, http.MethodPost, "/manage/v1/records/query", `{"kind":"GOLD_CONSIGN","limit":1}`)
	if code != http.StatusOK {
		t.Fatalf("大写类别状态码 = %d，期望 200（响应 %s）", code, raw)
	}
	if got := forwarded["kind"]; got != recordKindGoldConsign {
		t.Fatalf("大写类别应被归一化成小写后转发，实际 %v", got)
	}
}

func TestRecordKindWhitelistCoversEveryServerKind(t *testing.T) {
	for _, kind := range []string{
		recordKindCera, recordKindChat, recordKindDungeon,
		recordKindRaid, recordKindGoldConsign, recordKindAuction, recordKindScene,
	} {
		if !recordKindAllowed(kind) {
			t.Fatalf("类别 %s 未放行", kind)
		}
		if !strings.Contains(recordKindHint, kind) {
			t.Fatalf("白名单提示 %q 里漏了 %q（运营看到 400 时不知道能填什么）", recordKindHint, kind)
		}
	}
	if recordKindAllowed("gold") || recordKindAllowed("") {
		t.Fatal("白名单不该放行未知/空类别")
	}
}
