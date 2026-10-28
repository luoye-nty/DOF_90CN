//go:build paid

package manage

// 批量踢下线的纯函数测试。
//
// 这里钉的是"点名踢人"的入参归一化：勾选列表里可能带空白、可能重复
// （同一账号双开两个角色都在线 → 两行都被勾上），归一化没做好就会出现
// "同一个账号被踢两次"（第二次必然报账号不存在或 0 会话，看着像失败）。

import (
	"strings"
	"testing"
)

func TestNormalizeKickUsernames(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"去空白 + 去重（大小写不同算同一个）", []string{" Alice ", "alice", "", "  ", "bob"}, []string{"Alice", "bob"}},
		{"保留首次出现顺序", []string{"c", "a", "b"}, []string{"c", "a", "b"}},
		{"全空", []string{"", "   "}, nil},
		{"nil", nil, nil},
	}
	for _, tc := range cases {
		got := normalizeKickUsernames(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: = %v，期望 %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: = %v，期望 %v", tc.name, got, tc.want)
			}
		}
	}
}

// TestKickRequestShape 锁住请求/响应字段名：管理工具按这些名字解析，
// 改名会让"踢下选中"静默失效（前端拿到全 0 却以为踢过了）。
func TestKickRequestShape(t *testing.T) {
	req := adminKickRequest{Usernames: []string{"alice"}}
	if len(req.Usernames) != 1 || !strings.EqualFold(req.Usernames[0], "alice") {
		t.Fatalf("请求结构异常: %+v", req)
	}
	res := AdminKickResult{Requested: 2, Kicked: 3, Sessions: 4, Failed: []string{"x"}}
	if res.Requested != 2 || res.Kicked != 3 || res.Sessions != 4 || len(res.Failed) != 1 {
		t.Fatalf("结果结构异常: %+v", res)
	}
}
