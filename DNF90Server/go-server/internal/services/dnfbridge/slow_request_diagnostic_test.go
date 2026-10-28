package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// 这两个慢请求诊断事件是现网延迟排查的唯一手段：玩家侧无法复现，只能靠服务器
// 自己把超过门限的请求记下来。因此必须锁死「事件不会被 suppressPacketLogEvent
// 静默丢弃」这一条。
//
// 关键点：本测试调用的是**生产代码同一个字段构造函数**(slowUpperRequestFields /
// slowLegacyRequestFields)，而不是照抄一份字段列表。照抄的版本改坏线上代码也不会
// 失败——这一点已用 mutation test 实测验证过，所以字段定义必须共用。
//
// 曾经踩过的坑：字段名用过 "msg_id"，而 suppressPacketLogEvent 会按 msg_id 命中
// 1276/1516/1518 直接返回 true；那三个恰恰是最可能变慢的高频 opcode，等于诊断在
// 最需要的时候哑火。
func TestSlowRequestDiagnosticEventsAreNotSuppressed(t *testing.T) {
	session := &gameSession{connID: "game-000001", channel: channelcatalog.Channel{ID: 93}}

	// game-slow-upper-request：含最高风险的 1276/1516/1518 与 msg_id 13。
	for _, riskyMsgID := range []uint16{1276, 1516, 1518, 13, 4, 476, 1, 593} {
		packet := dnfproto.ChannelPacket{
			Header: dnfproto.ChannelHeader{MsgID: riskyMsgID, Classification: 1, Seq: 7},
			Body:   []byte{1, 2, 3},
		}
		fields := slowUpperRequestFields(session, packet, "ENUM_CMDPACKET_TEST", true, 3, 250*time.Millisecond)
		if suppressPacketLogEvent("game-slow-upper-request", fields...) {
			t.Fatalf("game-slow-upper-request 被抑制(request_msg_id=%d)；诊断会哑火", riskyMsgID)
		}
	}

	// game-slow-legacy-request：含 type 35(被抑制的 legacy-meta 值)与进游戏主力的 8/197。
	for _, riskyType := range []uint16{35, 8, 197, 1425, 1345, 4} {
		fields := slowLegacyRequestFields(session, byte(1), riskyType, 16, 250*time.Millisecond)
		if suppressPacketLogEvent("game-slow-legacy-request", fields...) {
			t.Fatalf("game-slow-legacy-request 被抑制(request_type=%d)；诊断会哑火", riskyType)
		}
	}
}

// 反向确认：抑制规则本身仍然生效，本测试不是靠「suppressPacketLogEvent 永远
// 返回 false」蒙过去的。
func TestSuppressionRulesStillApplyToKnownChattyEvents(t *testing.T) {
	if !suppressPacketLogEvent("game-upper-send-meta", "msg_id", uint16(1276)) {
		t.Fatal("msg_id 1276 的 game-upper-send-meta 应仍被抑制")
	}
	if !suppressPacketLogEvent("game-upper-send-meta", "msg_id", uint16(13)) {
		t.Fatal("msg_id 13 的 game-upper-send-meta 应仍被抑制")
	}
	if !suppressPacketLogEvent("game-legacy-meta", "type", uint16(35)) {
		t.Fatal("type 35 的 game-legacy-meta 应仍被抑制")
	}
	if !suppressPacketLogEvent("game-town-set-user-position-captured") {
		t.Fatal("game-town-set-user-position-captured 应仍被抑制")
	}
	// 证明抑制是靠 "msg_id"/"type" 这两个字段名触发的：换成别的名字就不该被抑制。
	if suppressPacketLogEvent("game-upper-send-meta", "request_msg_id", uint16(1276)) {
		t.Fatal("字段名不是 msg_id 时不应触发抑制——这正是诊断事件改用 request_msg_id 的原因")
	}
}
