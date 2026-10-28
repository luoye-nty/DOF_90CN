package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestParseCurrentSequentialDungeonCatalog(t *testing.T) {
	document, err := dnfpvf.Parse(currentSequentialDungeonPVFPath, `
[sequential dungeon]
28
[dungeon index check]
243 244 245 246 247
[entrance except dungeon]
247
[sequential dungeon]
42
[entrance except dungeon]
5106
[dungeon index check]
5101 5102 5103 5104 5105 5106
[rewardable dungeon index]
5106
`)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCurrentSequentialDungeonCatalog(document)
	if err != nil {
		t.Fatal(err)
	}
	luke, found := catalog.Find(42)
	if !found || luke.ID != 42 || len(luke.DungeonIDs) != 6 || luke.DungeonIDs[0] != 5101 || luke.DungeonIDs[5] != 5106 {
		t.Fatalf("Luke sequential definition found=%t definition=%+v", found, luke)
	}
}

func TestBuildCurrentSequentialDungeonProgressBodyUsesPVFOrderAndBooleanClear(t *testing.T) {
	definition := currentSequentialDungeonDefinition{ID: 42, DungeonIDs: []uint32{5101, 5102, 5103}}
	permission := dnfrepo.DungeonPermissionRecord{Entries: []dnfrepo.DungeonPermissionEntry{
		{DungeonID: 5103, ClearState: 3},
		{DungeonID: 5101, ClearState: 1},
	}}
	want := []byte{1, 42, 0, 0, 0, 3, 0, 1, 1, 0, 2, 1}
	if got := buildCurrentSequentialDungeonProgressBody(definition, permission); !bytes.Equal(got, want) {
		t.Fatalf("sequential progress body=%x want=%x", got, want)
	}
}

func TestHandleCurrentSequentialDungeonInfoSendsClassZeroProgressThenAckWithoutCreateCharacterCollision(t *testing.T) {
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "91", AccountID: defaultAccountPrefix + "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := repositories.DungeonPermission.UpsertMax(context.Background(), "91", 5101, 1); err != nil || !changed {
		t.Fatalf("seed 5101 changed=%t err=%v", changed, err)
	}
	if _, changed, err := repositories.DungeonPermission.UpsertMax(context.Background(), "91", 5103, 3); err != nil || !changed {
		t.Fatalf("seed 5103 changed=%t err=%v", changed, err)
	}
	definition := currentSequentialDungeonDefinition{ID: 42, DungeonIDs: []uint32{5101, 5102, 5103, 5104, 5105, 5106}}
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		sequentialDungeonCatalog: &currentSequentialDungeonCatalog{definitions: map[uint32]currentSequentialDungeonDefinition{
			42: definition,
		}},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 253, Type: 1, Name: "Luke", Port: 10253},
		selectedCharacterID: 91,
	}
	request := make([]byte, currentSequentialDungeonRequestSize)
	binary.LittleEndian.PutUint32(request, 42)
	if err := service.handleCurrentSequentialDungeonInfo(session, request); err != nil {
		t.Fatal(err)
	}

	progressPacket, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	wantProgress := buildCurrentSequentialDungeonProgressBody(definition, dnfrepo.DungeonPermissionRecord{Entries: []dnfrepo.DungeonPermissionEntry{
		{DungeonID: 5101, ClearState: 1},
		{DungeonID: 5103, ClearState: 3},
	}})
	if progressPacket.Header.MsgID != currentSequentialDungeonProgressMsgID || progressPacket.Header.Classification != 0 || !bytes.Equal(progressPacket.Body, wantProgress) {
		t.Fatalf("progress packet header=%+v body=%x want=%x", progressPacket.Header, progressPacket.Body, wantProgress)
	}
	ackPacket, rest := splitGameServerUpperPacket(t, rest)
	if ackPacket.Header.MsgID != uint16(dnfenum.CmdPacketSequentialDungeonInfo) || ackPacket.Header.Classification != dnfproto.DefaultChannelClassification || !bytes.Equal(ackPacket.Body, []byte{1, 0}) || len(rest) != 0 {
		t.Fatalf("ack packet header=%+v body=%x rest=%x", ackPacket.Header, ackPacket.Body, rest)
	}
}

func TestHandleCurrentSequentialDungeonInfoRejectsUnknownPVFSequence(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		sequentialDungeonCatalog: &currentSequentialDungeonCatalog{definitions: map[uint32]currentSequentialDungeonDefinition{
			42: {ID: 42, DungeonIDs: []uint32{5101}},
		}},
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 91}
	request := make([]byte, currentSequentialDungeonRequestSize)
	binary.LittleEndian.PutUint32(request, 99)
	if err := service.handleCurrentSequentialDungeonInfo(session, request); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketSequentialDungeonInfo) || !bytes.Equal(packet.Body, []byte{0, 0}) || len(rest) != 0 {
		t.Fatalf("unknown sequence ack header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
}
