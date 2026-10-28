package dnfbridge

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 入场道具不足只回客户端自己的 op16 失败回执（reason 17），不再附加自定义红字。
func TestSendCurrentDungeonEntryItemMissingFailureSendsOfficialFailureAckOnly(t *testing.T) {
	service, session, connection := newNoticeRoutingTest()
	cause := fmt.Errorf("consume party member 17 dungeon admission: %w", currentDungeonEntryItemShortage{
		CharacterID: session.selectedCharacterID,
		ItemID:      3330,
		Required:    28,
		Available:   3,
	})
	if !errors.Is(cause, errDungeonEntryItemMissing) {
		t.Fatal("shortage must still satisfy errors.Is(errDungeonEntryItemMissing)")
	}
	if err := service.sendCurrentDungeonEntryItemMissingFailure(session, nil, cause); err != nil {
		t.Fatal(err)
	}

	failure, remaining := splitGameServerUpperPacket(t, connection.write.Bytes())
	if failure.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) ||
		failure.Header.Classification != 1 ||
		!bytes.Equal(failure.Body, []byte{0, currentSelectDungeonEntryItemMissingReason, 0}) {
		t.Fatalf("select failure header=%+v body=%x", failure.Header, failure.Body)
	}
	if len(remaining) != 0 {
		t.Fatalf("op16 失败回执后有多余报文: %x", remaining)
	}
}

func TestCurrentDungeonEntryItemShortageNoticeTextNamesPartyMemberAndFallsBack(t *testing.T) {
	service, session, _ := newNoticeRoutingTest()
	member := service.currentDungeonEntryItemShortageNoticeText(currentDungeonEntryItemShortage{
		CharacterID: session.selectedCharacterID + 1, ItemID: 3330, Required: 26, Available: 0,
	}, session)
	if member != "队友入场道具不足：需要26个，当前0个。请先兑换该道具或准备对应通行证。" {
		t.Fatalf("party member text=%q", member)
	}
	generic := service.currentDungeonEntryItemShortageNoticeText(errDungeonEntryItemMissing, session)
	if generic != "入场道具不足。请先准备该地下城要求的邀请函或通行证。" {
		t.Fatalf("generic text=%q", generic)
	}
	// 这段文本只进诊断日志，不再生成客户端提示包。
}
