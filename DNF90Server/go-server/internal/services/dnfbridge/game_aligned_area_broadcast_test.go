package dnfbridge

import (
	"bytes"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
)

func TestSendAlignedUpperResponsesBroadcastsOnlyToSameAreaPeers(t *testing.T) {
	sourceConn := &bufferConn{}
	peerConn := &bufferConn{}
	otherAreaConn := &bufferConn{}
	source := &gameSession{conn: sourceConn, selectedCharacterID: 77}
	peer := &gameSession{conn: peerConn, selectedCharacterID: 78}
	otherArea := &gameSession{conn: otherAreaConn, selectedCharacterID: 79}
	service := &Service{onlinePlayers: newOnlinePlayerManager()}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 77, TownID: 38, AreaID: 1, Session: source})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 78, TownID: 38, AreaID: 1, Session: peer})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 79, TownID: 38, AreaID: 2, Session: otherArea})
	noticeBody := []byte{14, 1, 77, 0, 0xBC, 0x02, 0, 0, 13, 6}

	err := service.sendAlignedUpperResponses(source, alignedcmd.Result{
		Operation: "upgrade_item_separate",
		AreaBroadcastResponses: []alignedcmd.UpperResponse{{
			MsgID:          0x56,
			Body:           noticeBody,
			Classification: 0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sourceConn.write.Len() != 0 {
		t.Fatalf("source received duplicate area notice: %x", sourceConn.write.Bytes())
	}
	if otherAreaConn.write.Len() != 0 {
		t.Fatalf("other area received notice: %x", otherAreaConn.write.Bytes())
	}
	packets := splitAllCurrentGameServerUpperPackets(t, peerConn.write.Bytes())
	if len(packets) != 1 || packets[0].Header.MsgID != 0x56 || packets[0].Header.Classification != 0 || !bytes.Equal(packets[0].Body, noticeBody) {
		t.Fatalf("same-area notice packets=%+v stream=%x", packets, peerConn.write.Bytes())
	}
}
