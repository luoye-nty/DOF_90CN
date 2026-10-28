package dnfbridge

import (
	"bytes"
	"testing"
)

func TestCurrentSceneSkillInfoBeforeEquipmentOnlyPreloadsSkillManager(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 3}
	projection := currentSceneSkillInfoProjection{
		body:         []byte{0xAA, 0xBB},
		characterID:  "3",
		job:          11,
		learnedCount: 23,
	}
	if err := service.sendCurrentSceneSkillInfoProjectionBeforeEquipment(
		session,
		projection,
		"test_weapon_mastery_preload",
	); err != nil {
		t.Fatal(err)
	}

	skillPacket, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if skillPacket.Header.Classification != 0 ||
		skillPacket.Header.MsgID != currentSkillInfoMsgID ||
		!bytes.Equal(skillPacket.Body, projection.body) || len(trailing) != 0 {
		t.Fatalf("preload header=%+v body=%x trailing=%x", skillPacket.Header, skillPacket.Body, trailing)
	}
}
