package dnfbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"reflect"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func currentCharacterOptionTestBody() []byte {
	body := bytes.Repeat([]byte{0xff}, currentSaveCharacterOptionBodySize)
	binary.LittleEndian.PutUint32(body[:currentCharacterOptionLengthBytes], currentCharacterOptionPayloadBytes)
	binary.LittleEndian.PutUint16(body[currentCharacterOptionLengthBytes+2*2:], 47)
	binary.LittleEndian.PutUint16(body[currentCharacterOptionLengthBytes+3*2:], 52)
	body[currentCharacterOptionLengthBytes+300] = 0x5a
	return body
}

func newCharacterOptionTestService(t *testing.T, characterID uint16, characterAccount string) (*Service, *gameSession, dnfrepo.Group) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: strconv.FormatUint(uint64(characterID), 10),
		AccountID:   characterAccount,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{
			accountPrefix:      defaultAccountPrefix,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                &bufferConn{},
		accountID:           defaultAccountPrefix + "1",
		selectedCharacterID: characterID,
	}
	return service, session, repositories
}

func TestValidCurrentSaveCharacterOptionBodyRequiresCurrentEXEBlobShape(t *testing.T) {
	body := currentCharacterOptionTestBody()
	if !validCurrentSaveCharacterOptionBody(body) {
		t.Fatal("current op447 body was rejected")
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "nil", body: nil},
		{name: "short", body: body[:currentSaveCharacterOptionBodySize-1]},
		{name: "long", body: append(append([]byte(nil), body...), 0)},
		{name: "wrong length prefix", body: func() []byte {
			copyBody := append([]byte(nil), body...)
			binary.LittleEndian.PutUint32(copyBody[:4], currentCharacterOptionPayloadBytes-1)
			return copyBody
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validCurrentSaveCharacterOptionBody(test.body) {
				t.Fatal("invalid current op447 body was accepted")
			}
		})
	}
}

func TestCurrentSaveCharacterOptionLegacyRoutePersistsWithoutSyntheticAck(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"1")
	body := currentCharacterOptionTestBody()
	handled, err := currentGameplayModules.DispatchLegacy(service, session, uint16(dnfenum.CmdPacketSaveCharacterOption), body)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("op447 synthesized an unsafe ACK: %x", session.conn.(*bufferConn).write.Bytes())
	}
	stored, found, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19")
	if err != nil || !found || !bytes.Equal(stored, body[currentCharacterOptionLengthBytes:]) {
		t.Fatalf("stored found=%t err=%v payload=%x", found, err, stored)
	}
	if got := currentCharacterOptionCooldownSkills(stored); !reflect.DeepEqual(got, []uint16{47, 52}) {
		t.Fatalf("cooldown skills=%v", got)
	}
}

func TestCurrentSaveCharacterOptionUpperRoutePersistsWithoutSyntheticAck(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"1")
	body := currentCharacterOptionTestBody()
	handled, err := currentGameplayModules.DispatchUpper(service, session, uint16(dnfenum.CmdPacketSaveCharacterOption), protocol.DefaultChannelClassification, body)
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("upper op447 synthesized an unsafe ACK: %x", session.conn.(*bufferConn).write.Bytes())
	}
	stored, found, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19")
	if err != nil || !found || !bytes.Equal(stored, body[currentCharacterOptionLengthBytes:]) {
		t.Fatalf("stored found=%t err=%v payload=%x", found, err, stored)
	}
}

func TestCurrentSaveCharacterOptionUpperRouteRejectsNonDefaultClass(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"1")
	handled, err := currentGameplayModules.DispatchUpper(service, session, uint16(dnfenum.CmdPacketSaveCharacterOption), 0, currentCharacterOptionTestBody())
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("non-default op447 class wrote=%x", session.conn.(*bufferConn).write.Bytes())
	}
	if payload, found, loadErr := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19"); loadErr != nil || found || payload != nil {
		t.Fatalf("wrong-class op447 persisted found=%t payload=%x err=%v", found, payload, loadErr)
	}
}

func TestCurrentSaveCharacterOptionRejectsCrossAccountSelectedCharacter(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"2")
	handled, err := currentGameplayModules.DispatchUpper(service, session, uint16(dnfenum.CmdPacketSaveCharacterOption), protocol.DefaultChannelClassification, currentCharacterOptionTestBody())
	if err != nil || !handled {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if payload, found, loadErr := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19"); loadErr != nil || found || payload != nil {
		t.Fatalf("cross-account op447 persisted found=%t payload=%x err=%v", found, payload, loadErr)
	}
}

func TestCurrentCharacterOptionRestoreUsesClassZeroOp390AndExactOpaquePayload(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"1")
	if err := service.sendCurrentCharacterOptionSnapshot(session, "test_absent"); err != nil {
		t.Fatal(err)
	}
	if session.conn.(*bufferConn).write.Len() != 0 {
		t.Fatalf("absent snapshot emitted packet=%x", session.conn.(*bufferConn).write.Bytes())
	}
	body := currentCharacterOptionTestBody()
	if err := dnfrepo.SaveCharacterOptionSnapshot(context.Background(), repositories.Settings, "19", body[currentCharacterOptionLengthBytes:], service.gameplayNow().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := service.sendCurrentCharacterOptionSnapshot(session, "test_restore"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCharacterOptionRestoreMsgID || !bytes.Equal(packet.Body, body) {
		t.Fatalf("restore header=%+v body=%x want=%x", packet.Header, packet.Body, body)
	}
	if len(rest) != 0 {
		t.Fatalf("restore emitted trailing packets=%x", rest)
	}
}

func TestCurrentCharacterOptionRestoreMigratesPriorReleaseSnapshot(t *testing.T) {
	service, session, repositories := newCharacterOptionTestService(t, 19, defaultAccountPrefix+"1")
	body := currentCharacterOptionTestBody()
	if err := repositories.Settings.Save(context.Background(), dnfrepo.SettingsRecord{
		Scope: legacyCharacterOptionScope("19"),
		Values: map[string]string{
			legacyCharacterOptionValueKey: base64.StdEncoding.EncodeToString(body),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.sendCurrentCharacterOptionSnapshot(session, "test_legacy_restore"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCharacterOptionRestoreMsgID || !bytes.Equal(packet.Body, body) || len(rest) != 0 {
		t.Fatalf("legacy restore header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	payload, found, err := dnfrepo.LoadCharacterOptionSnapshot(context.Background(), repositories.Settings, "19")
	if err != nil || !found || !bytes.Equal(payload, body[currentCharacterOptionLengthBytes:]) {
		t.Fatalf("legacy migration found=%t err=%v payload=%x", found, err, payload)
	}
}
