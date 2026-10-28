package dnfbridge

import (
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// TestDungeonCreatureMovePublishesActorRowAndActivationTail pins the shipped
// behaviour for an in-dungeon pet move end to end:
//
//  1. the op2/mode0 actor row in the scene context - the only packet measured to
//     rebind the client's pet layer (op342+op357 left the pet stale, op9+op120+op357
//     left the pet stale);
//  2. the room-entry activation tail in its native order op9 -> op120 -> op357 -> op3,
//     because the client treats that row as a fresh scene binding and "op3 marks the
//     selected local wrapper active". Sending the row without the tail is exactly what
//     stopped the client from ever issuing CHANGE MAP (op45) after a pet move - the
//     socket-level probe saw no send() attempt at the door at all.
func TestDungeonCreatureMovePublishesActorRowAndActivationTail(t *testing.T) {
	service, session, connection := newDungeonCreatureAppearanceTest(t, true)

	if err := service.sendAlignedUpperResponses(session, dungeonCreatureMoveResult()); err != nil {
		t.Fatalf("dungeon creature move: %v", err)
	}
	stream := connection.write.Bytes()

	mode0, ownerContext, found := firstMode0WithOwnerContext(t, stream)
	if !found {
		t.Fatalf("dungeon creature move published no op2 actor row: %x", stream)
	}
	if mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || mode0.Header.Classification != 0 {
		t.Fatalf("dungeon actor row header=%+v", mode0.Header)
	}
	if ownerContext != currentSceneObjectContext {
		t.Fatalf("dungeon actor row owner context=%d, want %d body_head=%x",
			ownerContext, currentSceneObjectContext, mode0.Body[:min(16, len(mode0.Body))])
	}

	// 激活尾巴：op9 角色显示 -> op120 落位 -> op357 重绘 -> op3 激活本地 wrapper。
	if _, found := firstPacketWithMsgID(t, stream, uint16(dnfenum.CmdPacketRecoverStamina)); !found {
		t.Fatalf("activation tail is missing op9 actor display: %x", stream)
	}
	if _, found := firstPacketWithMsgID(t, stream, uint16(dnfenum.CmdPacketRequestBlacklist)); !found {
		t.Fatalf("activation tail is missing op120 placement: %x", stream)
	}
	if _, found := firstPacketWithMsgID(t, stream, currentActorUserStateMsgID); !found {
		t.Fatalf("activation tail is missing op357 redraw: %x", stream)
	}
	if _, found := firstPacketWithMsgID(t, stream, uint16(dnfenum.CmdPacketNotifyUserState)); !found {
		t.Fatalf("activation tail is missing op3 wrapper activation: %x", stream)
	}

	// 宠物状态投影照旧，不受发布影响。
	if _, found := firstPacketWithMsgID(t, stream, currentCreatureStateTableMsgID); !found {
		t.Fatalf("dungeon creature move dropped the creature state table: %x", stream)
	}
}

// TestDungeonActorRowIsFollowedByTheActivationTailOrder pins the ORDER, not just the
// presence: the client needs op120 (room object manager commit) before op3 (wrapper
// activation), and the display row before both.
func TestDungeonActorRowIsFollowedByTheActivationTailOrder(t *testing.T) {
	service, session, connection := newDungeonCreatureAppearanceTest(t, true)

	if err := service.sendAlignedUpperResponses(session, dungeonCreatureMoveResult()); err != nil {
		t.Fatalf("dungeon creature move: %v", err)
	}
	stream := connection.write.Bytes()

	order := []uint16{
		uint16(dnfenum.CmdPacketSetUDPIPPort),     // op2 actor row (mode0)
		uint16(dnfenum.CmdPacketRecoverStamina),   // op9  actor display
		uint16(dnfenum.CmdPacketRequestBlacklist), // op120 placement
		currentActorUserStateMsgID,                // op357 redraw
		uint16(dnfenum.CmdPacketNotifyUserState),  // op3  wrapper activation
	}
	position := 0
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if position < len(order) && packet.Header.MsgID == order[position] {
			position++
		}
	}
	if position != len(order) {
		t.Fatalf("activation tail order broken: matched %d/%d of %v", position, len(order), order)
	}
}

// firstPacketWithMsgID returns the first packet carrying msgID in a server-upper
// stream written by sendAlignedUpperResponses, regardless of classification.
func firstPacketWithMsgID(t *testing.T, data []byte, msgID uint16) (dnfproto.ChannelPacket, bool) {
	t.Helper()
	for len(data) > 0 {
		packet, rest := splitGameServerUpperPacket(t, data)
		data = rest
		if packet.Header.MsgID == msgID {
			return packet, true
		}
	}
	return dnfproto.ChannelPacket{}, false
}
