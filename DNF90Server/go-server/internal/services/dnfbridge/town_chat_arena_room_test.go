package dnfbridge

import (
	"bytes"
	"testing"
)

// 2026-09-14 22:37 活体（conn game-000147，channel 191，决斗场房间）：
//
//	14:37:07 mode=3 → 只发给说话者自己（scope=private_scene）
//	14:37:14 mode=5 → game-town-chat-rejected reason=unsupported_chat_mode
//
// 聊天窗对这两种情况都表现为"打不出去"。房间是私有地图，永远不进城镇 presence 注册表，
// 所以区域频道也解析不到任何听众；而决斗场频道用的 mode 5 当时根本没被路由。
func TestArenaRoomChatReachesEverySeat(t *testing.T) {
	service := pvpRoomTestService(t)
	ownerConn := &bufferConn{}
	peerConn := &bufferConn{}
	owner := &gameSession{
		conn: ownerConn, connID: "arena-chat-owner",
		selectedCharacterID: 29, channel: service.catalog.Channels()[1],
	}
	peer := &gameSession{
		conn: peerConn, connID: "arena-chat-peer",
		selectedCharacterID: 30, channel: service.catalog.Channels()[1],
	}
	service.bindGameSessionCharacter(owner, 29)
	service.bindGameSessionCharacter(peer, 30)

	seedPVPRoomTestSession(service, owner)
	room := service.pvpRooms[1]
	room.members[1] = &currentPVPMember{session: peer, characterID: 30}
	room.seatState[1] = 1

	for _, test := range []struct {
		name string
		mode byte
	}{
		{name: "arena channel", mode: currentTownChatArenaMode},
		{name: "area channel inside the room", mode: currentTownChatAreaMode},
	} {
		t.Run(test.name, func(t *testing.T) {
			ownerConn.write.Reset()
			peerConn.write.Reset()
			body := buildCurrentTownChatRequestForTest(test.mode, []byte("决斗场房间说话"))
			if err := service.handleCurrentTownChat(owner, body); err != nil {
				t.Fatal(err)
			}
			for _, seat := range []struct {
				label string
				conn  *bufferConn
			}{
				{label: "speaker", conn: ownerConn},
				{label: "other seat", conn: peerConn},
			} {
				packets := seat.conn.write.Bytes()
				if len(packets) == 0 {
					t.Fatalf("%s 没有收到任何聊天包（mode=%d）", seat.label, test.mode)
				}
				packet, rest := splitGameServerUpperPacket(t, packets)
				if packet.Header.MsgID != currentTownChatNotificationMsgID || packet.Header.Classification != 0 ||
					len(rest) != 0 {
					t.Fatalf("%s 收到的不是聊天通知: header=%+v rest=%x", seat.label, packet.Header, rest)
				}
				if !bytes.Contains(packet.Body, []byte("决斗场房间说话")) {
					t.Fatalf("%s 的聊天内容不正确: %x", seat.label, packet.Body)
				}
			}
		})
	}
}

// 未入座时应保持历史行为：只有说话者自己，而不是回答失败码（聊天窗会把它渲染成沉默）。
func TestArenaChannelChatWithoutASeatKeepsTheSpeakerOnly(t *testing.T) {
	service := pvpRoomTestService(t)
	conn := &bufferConn{}
	session := &gameSession{
		conn: conn, connID: "arena-chat-unseated",
		selectedCharacterID: 31, channel: service.catalog.Channels()[1],
	}
	service.bindGameSessionCharacter(session, 31)
	if err := service.handleCurrentTownChat(session, buildCurrentTownChatRequestForTest(
		currentTownChatArenaMode, []byte("没有房间"))); err != nil {
		t.Fatal(err)
	}
	packets := conn.write.Bytes()
	if len(packets) == 0 {
		t.Fatal("未入座的说话者连自己的聊天包都没收到")
	}
	packet, rest := splitGameServerUpperPacket(t, packets)
	if packet.Header.MsgID != currentTownChatNotificationMsgID || len(rest) != 0 {
		t.Fatalf("未入座的说话者收到的不是聊天通知: header=%+v rest=%x", packet.Header, rest)
	}
}
