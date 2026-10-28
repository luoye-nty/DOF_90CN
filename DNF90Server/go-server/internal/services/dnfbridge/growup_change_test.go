package dnfbridge

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestDecodeGrowupChangeTargetGrowType(t *testing.T) {
	// 90CN 客户端 legacy 785 请求 body 为 14 字节，目标职业分支位于最后一个字节。
	body90 := make([]byte, 14)
	body90[13] = 2
	target, err := decodeGrowupChangeTargetGrowType(body90)
	if err != nil {
		t.Fatal(err)
	}
	if target != 2 {
		t.Fatalf("90CN body target = %d want 2", target)
	}
	// 兼容 86 客户端 15 字节布局（GrowupChangeRequestParser offset 14）。
	body86 := make([]byte, 15)
	body86[14] = 3
	target, err = decodeGrowupChangeTargetGrowType(body86)
	if err != nil {
		t.Fatal(err)
	}
	if target != 3 {
		t.Fatalf("86 body target = %d want 3", target)
	}
	// 其它长度直接拒绝。
	if _, err := decodeGrowupChangeTargetGrowType(make([]byte, 13)); !errors.Is(err, errGrowupChangeBodyTooShort) {
		t.Fatalf("13-byte body err = %v want errGrowupChangeBodyTooShort", err)
	}
	if _, err := decodeGrowupChangeTargetGrowType(nil); !errors.Is(err, errGrowupChangeBodyTooShort) {
		t.Fatalf("nil body err = %v want errGrowupChangeBodyTooShort", err)
	}
}

func TestBuildGrowupChangeAckBody(t *testing.T) {
	// 与 86 GrowupChangeAckBuilder 一致：i32 result code + u8 changeCount。
	body := buildGrowupChangeAckBody(19, 3)
	if len(body) != 5 {
		t.Fatalf("ack body length = %d want 5", len(body))
	}
	if code := int(binary.LittleEndian.Uint32(body[0:4])); code != 19 {
		t.Fatalf("ack result code = %d want 19", code)
	}
	if body[4] != 3 {
		t.Fatalf("ack change count = %d want 3", body[4])
	}
}

func TestCurrentGrowupTransitionNeedsDungeonOnlySkill(t *testing.T) {
	// 无会话 / 无 runtime（城镇）不需要副本内技能专用刷新，应走完整刷新链路。
	if currentGrowupTransitionNeedsDungeonOnlySkill(nil) {
		t.Fatal("nil session must not require dungeon-only skill refresh")
	}
	if currentGrowupTransitionNeedsDungeonOnlySkill(&gameSession{}) {
		t.Fatal("session without dungeon runtime must not require dungeon-only skill refresh")
	}

	// 真正副本内（runtime 存在且尚未进入回城过渡）：只重发 op19 技能信息。
	active := &gameSession{}
	active.dungeon.runtime = &runtimeDungeonState{}
	if !currentGrowupTransitionNeedsDungeonOnlySkill(active) {
		t.Fatal("active dungeon runtime must require dungeon-only skill refresh")
	}

	// 回城过渡中（op42 GIVEUP_GAME 已写出回城包但 runtime 仍保留等待城镇确认）：
	// 转职请求必须走完整 mode0/mode1 + op19 + op357 刷新，否则客户端职业信息
	// 停留在旧职业。
	pending := &gameSession{}
	pending.dungeon.runtime = &runtimeDungeonState{townReturnPending: true}
	if currentGrowupTransitionNeedsDungeonOnlySkill(pending) {
		t.Fatal("town-return pending runtime must use the full town refresh chain")
	}
	op24Sent := &gameSession{}
	op24Sent.dungeon.runtime = &runtimeDungeonState{townReturnOp24Sent: true}
	if currentGrowupTransitionNeedsDungeonOnlySkill(op24Sent) {
		t.Fatal("town-return op24-sent runtime must use the full town refresh chain")
	}
}
