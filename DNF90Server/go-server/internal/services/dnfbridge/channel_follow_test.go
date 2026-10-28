package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// buildChannelFollowRequestBodyForTest 构造客户端上行体: u8 region + dstr 目标名。
func buildChannelFollowRequestBodyForTest(region byte, name string) []byte {
	var writer packetWriter
	writer.writeByte(region)
	writeClientNameDstr(&writer, name)
	return writer.bytes()
}

func TestDecodeCurrentChannelFollowRequestMatchesLiveWireShape(t *testing.T) {
	// 线上实测: 01 | 04 00 00 00 | c4 cc c2 e8 (= “目标”的 GBK 4 字节)
	request, ok := decodeCurrentChannelFollowRequest(
		[]byte{0x01, 0x04, 0x00, 0x00, 0x00, 0xC4, 0xCC, 0xC2, 0xE8})
	if !ok {
		t.Fatal("live 804 body must decode as u8 region + dstr name")
	}
	if request.region != 1 || request.name == "" {
		t.Fatalf("decoded region=%d name=%q", request.region, request.name)
	}
	for _, rejected := range [][]byte{
		nil,
		{0x01},
		{0x01, 0x04, 0x00, 0x00, 0x00, 0xC4},
	} {
		if _, ok := decodeCurrentChannelFollowRequest(rejected); ok {
			t.Fatalf("body %x must be rejected", rejected)
		}
	}
}

func TestHandleCurrentUserChannelRequestSendsClass1FollowResponse(t *testing.T) {
	sourceConn := &bufferConn{}
	targetConn := &bufferConn{}
	source := &gameSession{conn: sourceConn, accountID: "dnf:1", selectedCharacterID: 1}
	target := &gameSession{conn: targetConn, accountID: "dnf:2", selectedCharacterID: 5}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
			// 频道表里 41/183 这些频道的 ServerID 实测是 11; 响应必须回这个值,
			// 而不是请求里带的 region 字节(公会/好友行硬编码的 1)。
			channelServerID: 11,
		},
		onlinePlayers: newOnlinePlayerManager(),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1, Name: "source", TownID: 1, AreaID: 2, ChannelID: 11, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 5, Name: "target", TownID: 1, AreaID: 2, ChannelID: 93, Session: target})
	service.bindGameSessionCharacter(source, 1)
	service.bindGameSessionCharacter(target, 5)

	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestUserChannel),
		buildChannelFollowRequestBodyForTest(7, "target"),
	); err != nil {
		t.Fatal(err)
	}

	packet, rest := splitLongHengGameServerUpperPacket(t, sourceConn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after follow response: %d", len(rest))
	}
	// 必须是 class-1 同号 804: class-0 的 804 是另一个功能,客户端不会走跟随链。
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestUserChannel) {
		t.Fatalf("follow response header=%+v", packet.Header)
	}
	body := packet.Body
	// 布局(真机验证): mode | flag | dstr 名字 | 大区 | 频道号
	if len(body) < 8 || body[0] != currentChannelFollowDataMode ||
		body[1] != currentChannelFollowFlag {
		t.Fatalf("follow response head=%x want mode=%d flag=%d",
			body, currentChannelFollowDataMode, currentChannelFollowFlag)
	}
	nameLength := int(binary.LittleEndian.Uint32(body[2:6]))
	if nameLength != len("target") || string(body[6:6+nameLength]) != "target" {
		t.Fatalf("follow response name=%x", body[2:])
	}
	tail := body[6+nameLength:]
	// tail[0] = 大区 ID(筛频道表用), tail[1] = 目标频道号(查找目标用)。
	if len(tail) != 2 || tail[0] != 11 || tail[1] != 93 {
		t.Fatalf("follow response server/channel=%x want server=11 channel=93", tail)
	}
}

func TestHandleCurrentUserChannelRequestReportsMissingTargetAsState(t *testing.T) {
	sourceConn := &bufferConn{}
	source := &gameSession{conn: sourceConn, accountID: "dnf:1", selectedCharacterID: 1}
	service := &Service{
		options: options{
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		onlinePlayers: newOnlinePlayerManager(),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 1, Name: "source", TownID: 1, AreaID: 2, ChannelID: 11, Session: source})
	service.bindGameSessionCharacter(source, 1)

	if err := service.handleGameCommand(
		source,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketRequestUserChannel),
		buildChannelFollowRequestBodyForTest(1, "absent"),
	); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitLongHengGameServerUpperPacket(t, sourceConn.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestUserChannel) {
		t.Fatalf("missing-target follow response header=%+v", packet.Header)
	}
	// 只回状态包; 客户端把状态 100 显示成「缺少目标角色。」(字符串 0x8226)。
	if len(packet.Body) != 2 || packet.Body[0] != currentChannelFollowStateMode ||
		packet.Body[1] != currentChannelFollowMissingTargetState {
		t.Fatalf("missing-target follow response body=%x", packet.Body)
	}
}
