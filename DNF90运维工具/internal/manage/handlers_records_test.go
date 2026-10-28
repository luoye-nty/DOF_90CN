//go:build paid

package manage

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRecordsPageOffsetSurvivesForwarding 锁住一个线上事故（2026-09-25 审计发现）：
//
// 游戏服的记录页响应里有 `offset`（本次返回的起始位置），管理工具的"翻页能力探测"
// 就是拿"请求 offset"与"响应 offset"比对。运维工具这一跳原来是
// "反序列化成 GMTRecordPage → 再序列化转发"，而 GMTRecordPage 当时**没声明 Offset**，
// 于是第二页的回显恒为 0 → 管理工具判定"对端不支持翻页"，只显示第一页，
// 还提示运营"请重新部署服务端与运维工具"（重装也没用，字段就是漏了）。
//
// 这条用例模拟"上游响应 → 解码 → 转发编码"这整条链路，offset 必须原样活下来。
func TestRecordsPageOffsetSurvivesForwarding(t *testing.T) {
	upstream := []byte(`{"kind":"dungeon","title":"副本记录","dir":"logs/records",` +
		`"records":[{"ts":"2026-09-25T13:00:00Z"}],"count":1,"offset":1000,"scanned":1001,` +
		`"limited":true,"retention_files":3}`)

	var page GMTRecordPage
	if err := json.Unmarshal(upstream, &page); err != nil {
		t.Fatalf("解码上游记录页: %v", err)
	}
	if page.Offset != 1000 {
		t.Fatalf("解码后 Offset = %d，期望 1000（GMTRecordPage 少了 offset 字段）", page.Offset)
	}

	out, err := json.Marshal(RecordsQueryResult{GMTRecordPage: page})
	if err != nil {
		t.Fatalf("转发编码: %v", err)
	}
	if !strings.Contains(string(out), `"offset":1000`) {
		t.Fatalf("转发给管理工具的响应里丢了 offset: %s", out)
	}
}
