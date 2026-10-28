package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

func newNoticeRoutingTest() (*Service, *gameSession, *bufferConn) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	return service, &gameSession{conn: connection, selectedCharacterID: 17}, connection
}

func TestCurrentRandomOptionGoldInsufficientNoticeText(t *testing.T) {
	tests := []struct {
		operation string
		want      string
	}{
		{operation: "unseal_random_option", want: "金币不足，无法解除魔法封印。"},
		{operation: "change_random_option", want: "金币不足，无法变更魔法封印属性。"},
	}
	for _, test := range tests {
		got := currentRandomOptionGoldInsufficientNoticeText(test.operation)
		if got != test.want {
			t.Fatalf("operation %q text = %q, want %q", test.operation, got, test.want)
		}
		if _, err := buildCurrentNativeNoticeBody(currentRandomOptionGoldInsufficientNoticeSurface.Type, 0, 0, got); err != nil {
			t.Fatalf("operation %q text does not fit the client bound: %v", test.operation, err)
		}
	}
}

// 【op455 成功提示的顺序契约】
//
// 客户端 op455 处理器没有成功分支（0x2137820 只在 result==0 时按错误码弹提示），
// 所以成功提示只能由服务端补。顺序必须是：先 op14 道具行（装备 + 调整箱），再一次
// class-1 {0x01} 解开 UI 等待，最后才是系统页提示 —— 否则提示会早于词条刷新出现。
func TestRandomOptionResetSuccessAppendsVisibleNotice(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	result := alignedcmd.Result{
		Operation: "reset_random_option",
		UpperResponses: []alignedcmd.UpperResponse{
			// class0/op14 增量道具行，与 op401/op437 成功回包同一条通道。
			{MsgID: 0x000E, Classification: 0, Body: []byte{0, 0, 0}},
			{MsgID: uint16(dnfenum.CmdPacketResetRandomOption), Classification: 1, Body: []byte{1}},
		},
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionNotifyRandomOptionResetSuccess},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}

	update, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if update.Header.MsgID != 0x000E || update.Header.Classification != 0 {
		t.Fatalf("first packet header=%+v, want the op14 item rows", update.Header)
	}
	ack, remaining := splitGameServerUpperPacket(t, remaining)
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketResetRandomOption) || ack.Header.Classification != 1 || !bytes.Equal(ack.Body, []byte{1}) {
		t.Fatalf("ACK header=%+v body=%x", ack.Header, ack.Body)
	}

	text := currentRandomOptionResetSuccessNoticeText()
	if text != "已成功重调该物品的封印属性品级。" {
		t.Fatalf("notice text = %q", text)
	}
	want, err := buildCurrentNativeNoticeBody(currentRandomOptionResetSuccessNoticeSurface.Type, 0, 0, text)
	if err != nil {
		t.Fatalf("notice text does not fit the client bound: %v", err)
	}
	notice, trailing := splitGameServerUpperPacketWithHeader(t, remaining, dnfproto.GameServerUpperHeaderSize16)
	if notice.Header.MsgID != currentNativeNoticeMsgID || notice.Header.Classification != 0 || !bytes.Equal(notice.Body, want) || len(trailing) != 0 {
		t.Fatalf("notice header=%+v body=%x want=%x trailing=%x", notice.Header, notice.Body, want, trailing)
	}
	if currentRandomOptionResetSuccessNoticeSurface != noticeSurfaceSystemTab {
		t.Fatalf("success notice surface = %+v, want the system tab like the op81 message", currentRandomOptionResetSuccessNoticeSurface)
	}
}

func TestRandomOptionFailureACKPrecedesVisibleNotice(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	result := alignedcmd.Result{
		Operation: "unseal_random_option",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID:          uint16(dnfenum.CmdPacketUnsealRandomOption),
			Classification: 1,
			Body:           []byte{0},
		}},
		PostActions: []alignedcmd.PostAction{alignedcmd.PostActionNotifyRandomOptionGoldInsufficient},
	}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}

	ack, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketUnsealRandomOption) || ack.Header.Classification != 1 || !bytes.Equal(ack.Body, []byte{0}) {
		t.Fatalf("ACK header=%+v body=%x", ack.Header, ack.Body)
	}

	text := currentRandomOptionGoldInsufficientNoticeText(result.Operation)
	notice, trailing := splitGameServerUpperPacketWithHeader(t, remaining, dnfproto.GameServerUpperHeaderSize16)
	want, err := buildCurrentNativeNoticeBody(currentRandomOptionGoldInsufficientNoticeSurface.Type, 0, 0, text)
	if err != nil {
		t.Fatal(err)
	}
	if notice.Header.MsgID != currentNativeNoticeMsgID || notice.Header.Classification != 0 || !bytes.Equal(notice.Body, want) || len(trailing) != 0 {
		t.Fatalf("notice header=%+v body=%x want=%x trailing=%x", notice.Header, notice.Body, want, trailing)
	}
}
