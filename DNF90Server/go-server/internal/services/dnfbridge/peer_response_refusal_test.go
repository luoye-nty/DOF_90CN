package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 现场 2026-09-26：申请人 B 点 A 申请组队，A 点「否」，B 那边什么都不弹。
//
// 客户端的拒绝包是 9 字节（body[7..9] = 0x0085，三次实测 07:24:24 / 08:32:39 /
// 08:32:48），而 party.DecodeResponsePeerRequest 只收正好 7 字节，唯一的 9 字节兜底
// runtimeRaidPeerResponseDeclined 又把 mode 限制成 10。于是组队拒绝整包解不出来：
// 服务端只写下一句 game-party-response-peer-body-short 就返回，待处理的邀请从未被消费，
// 申请人收不到任何东西，客户端也就永远不弹「对方不在线，或拒绝了您的组队请求」。
func TestDecodeCurrentPeerResponseAnswerAcceptsBothWireForms(t *testing.T) {
	accept, err := decodeCurrentPeerResponseAnswer([]byte{0xe9, 0x03, 0, 0, 0, 0, 0})
	if err != nil || accept.Refused || accept.Request.TargetID != 1001 || accept.Request.Mode != 0 {
		t.Fatalf("accept form = %+v err=%v", accept, err)
	}
	refusal, err := decodeCurrentPeerResponseAnswer([]byte{0xe9, 0x03, 0, 0, 0, 0, 0, 0x85, 0})
	if err != nil || !refusal.Refused || refusal.Reason != 0x0085 || refusal.Request.TargetID != 1001 {
		t.Fatalf("refusal form = %+v err=%v", refusal, err)
	}
	if _, err := decodeCurrentPeerResponseAnswer([]byte{0xe9, 0x03, 0}); err == nil {
		t.Fatal("a short body must stay an error")
	}
}

// 端到端：拒绝要走完「消费邀请 + 通知申请人」两步，而且不能给申请人建队。
//
// 接受那条路有 TestHandleResponsePeerAcceptsOnlinePartyInvite 钉着（它同时断言
// 邀请人**不会**收到 class0/op8）—— 本测试是它拒绝方向的对照，不能互相污染：
// 只有拒绝才发 op8。
func TestHandleResponsePeerForwardsRefusalAndConsumesTheInvite(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	inviterConn := &bufferConn{}
	acceptorConn := &bufferConn{}
	channel := channelcatalog.Channel{ID: 38, Type: 1, Name: "ch.38", Port: 10038}
	inviter := &gameSession{conn: inviterConn, channel: channel, selectedCharacterID: 1001}
	acceptor := &gameSession{conn: acceptorConn, channel: channel, selectedCharacterID: 1002}
	service.bindGameSessionCharacter(inviter, 1001)
	service.bindGameSessionCharacter(acceptor, 1002)
	inviterIdentity, inviterBound := service.boundGameSessionCharacterSnapshot(inviter)
	acceptorIdentity, acceptorBound := service.boundGameSessionCharacterSnapshot(acceptor)
	if !inviterBound || !acceptorBound || !service.runtimePartyManagerForService().RecordInvite(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 0, 0,
	) {
		t.Fatal("could not install a current-session party invite")
	}

	handled, err := service.handleAlignedGameCommand(
		acceptor,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketResponsePeer),
		[]byte{0xe9, 0x03, 0, 0, 0, 0, 0, 0x85, 0},
	)
	if err != nil {
		t.Fatalf("handle refusal: %v", err)
	}
	if !handled {
		t.Fatal("ResponsePeer should be handled by online party bridge")
	}
	if _, pending := service.runtimePartyManagerForService().ConsumePeerRequest(
		acceptorIdentity.character, acceptorIdentity.generation,
		inviterIdentity.character, inviterIdentity.generation, 0,
	); pending {
		t.Fatal("refused invite was left pending")
	}
	if acceptorConn.write.Len() == 0 {
		t.Fatal("refusing client received no acknowledgement")
	}
	// 申请人**什么都不会收到** —— 这是有意的，也是实测结论：
	//   op8 会让客户端显示"已接受组队请求"（正好说反，客户端没有"拒绝"的孪生文案）；
	//   class-1 op10 失败包客户端收到了但直接丢弃（一次请求只认一条回应，成功 ACK 已经用掉）。
	// 那条"对方不在线，或拒绝了您的组队请求"在本客户端里是死条目（70054/70055 无任何引用）。
	if inviterConn.write.Len() != 0 {
		t.Fatalf("refusal must not send the requester anything, got %d bytes", inviterConn.write.Len())
	}
}
