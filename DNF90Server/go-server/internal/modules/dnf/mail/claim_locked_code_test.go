package mail

import (
	"errors"
	"fmt"
	"testing"
)

// 这组用例钉住"锁定"这条拒绝在 op95 上到底回什么码，以及它**不能**被算成别的语义。
//
// 现场证据（2026-09-15，线上包 packet log，玩家在游戏里点领取那一刻）：
//
//	SEND kind=game-upper msg_id=95 body=0004
//
// 服务端确实回了 ackType 0x005F 的码 4，而客户端**没有画任何东西**。所以：
//   - 码 4 在 op95 表里是可达的（老注释若说它渲染"背包已满"就与本机客户端不符），
//   - 无论回哪个码，都不能再假设"客户端一定会自己画提示"。
//
// 这个用例只保证服务端这一侧稳定：锁定必须走锁定那条错误，且码值集中在一个 switch 里，
// 将来客户端码表反查清楚后只改一处。
func TestMailboxClaimErrorCodeKeepsLockedDistinct(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want byte
		why  string
	}{
		{"nil", nil, 0, "成功不回失败码"},
		{"locked", ErrMailboxItemLocked, 4, "锁定走'放不进去'这一格（当前客户端不渲染，安全性不受影响）"},
		{"locked wrapped", fmt.Errorf("%w: list=0 slot=65 item=2001", ErrMailboxItemLocked), 4, "带上文包装后仍要认出锁定"},
		{"inventory full", ErrMailboxInventoryFull, 4, "背包放不下与锁定共用同一格"},
		{"mail gone", ErrMailboxMailNotFound, 21, "信件不存在"},
		{"already claimed", ErrMailboxAlreadyClaimed, 21, "已领取"},
		{"expired", ErrMailboxExpired, 21, "已过期"},
		{"gold", ErrMailboxGoldInsufficient, 10, "金币携带上限"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailboxClaimErrorCode(tc.err); got != tc.want {
				t.Fatalf("code=%d want=%d (%s)", got, tc.want, tc.why)
			}
		})
	}
	// 锁定永远不能被折叠成"成功"或"0"：静默成功会把锁定附件真的发出去。
	body := buildErrorAck(mailboxClaimErrorCode(fmt.Errorf("%w: x", ErrMailboxItemLocked)))
	if len(body) != 2 || body[0] != 0x00 || body[1] == 0 {
		t.Fatalf("锁定拒绝帧必须是非零失败码，实际 % X", body)
	}
	// 而且 must 不等于"没有错误"这条路径。
	if errors.Is(ErrMailboxItemLocked, ErrMailboxInventoryFull) {
		t.Fatal("锁定与背包已满是两个不同原因，不能互为别名")
	}
}
