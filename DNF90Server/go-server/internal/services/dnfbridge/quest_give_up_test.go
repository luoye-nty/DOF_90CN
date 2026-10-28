package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentGiveUpQuestLiveLegacyFramePersistsRemovalBeforeNativeAck(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "1",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "1",
		States: map[int64]dnfrepo.QuestState{
			5760: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := questListTestSource{
		dnfquest.DefaultList: "5760 `giveup.qst`\n",
		"n_quest/giveup.qst": "[grade]\n`[epic]`\n[type]\n`[clear map]`\n[int data]\n76126\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "give-up-quest-live-legacy-test",
		channel:             channelcatalog.Channel{ID: 121},
		selectedCharacterID: 1,
	}
	// Exact 17-byte frame observed from the reported current client. Its body
	// is u16(op32) + u16(quest 5760), not a class-1 channel packet.
	liveFrame := []byte{
		0x01, 0x20, 0x00, 0x11, 0x00, 0x00, 0x00, 0xe9, 0x43, 0x9f, 0x92, 0xd5, 0x04,
		0x20, 0x00, 0x80, 0x16,
	}
	if err := service.handleLegacyGamePacket(session, liveFrame); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := repositories.Quest.Load(context.Background(), "1")
	if err != nil || !ok {
		t.Fatalf("load persisted quest ok=%t err=%v", ok, err)
	}
	if _, exists := stored.States[5760]; exists {
		t.Fatalf("legacy op32 left quest 5760 active: %+v", stored.States[5760])
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketGiveupQuest) {
		t.Fatalf("give-up ACK header=%+v trailing=%x", packet.Header, rest)
	}
	if want := []byte{0x01, 0x80, 0x16}; !bytes.Equal(packet.Body, want) {
		t.Fatalf("give-up ACK body=%x want=%x", packet.Body, want)
	}
}

func TestCurrentGiveUpQuestPersistsRemovalBeforeExactCurrentEXEAck(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			3144: {Status: "completed", ProgressValue: 1},
			3145: {Status: "active", ProgressValue: 7},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "give-up-quest-test",
		channel:             channelcatalog.Channel{ID: 38},
		selectedCharacterID: 17,
	}
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketGiveupQuest),
		[]byte{0x20, 0x00, 0x49, 0x0c},
		9,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatal(err)
	}

	stored, ok, err := repositories.Quest.Load(context.Background(), "17")
	if err != nil || !ok {
		t.Fatalf("load persisted quest ok=%t err=%v", ok, err)
	}
	if _, exists := stored.States[3145]; exists {
		t.Fatalf("abandoned quest remains active: %+v", stored.States[3145])
	}
	if stored.States[3144].Status != "completed" {
		t.Fatalf("unrelated completed quest changed: %+v", stored.States[3144])
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketGiveupQuest) {
		t.Fatalf("give-up ACK header=%+v trailing=%x", packet.Header, rest)
	}
	if want := []byte{0x01, 0x49, 0x0c}; !bytes.Equal(packet.Body, want) {
		t.Fatalf("give-up ACK body=%x want=%x", packet.Body, want)
	}

	connection.write.Reset()
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("same-session give-up replay emitted packet=%x", connection.write.Bytes())
	}
}

func TestCurrentGiveUpQuestMalformedRequestDoesNotMutateOrAck(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "18", AccountID: defaultAccountPrefix + "1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "18",
		States:      map[int64]dnfrepo.QuestState{3145: {Status: "active"}},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAcceptQuestTestCatalog(t, false),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 18}
	if err := service.handleCurrentGiveUpQuest(session, []byte{0x20, 0x00, 0x49, 0x0c, 0x00}); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "18")
	if err != nil || !ok || stored.States[3145].Status != "active" {
		t.Fatalf("malformed give-up mutated state ok=%t err=%v state=%+v", ok, err, stored.States[3145])
	}
	if connection.write.Len() != 0 {
		t.Fatalf("malformed give-up emitted packet=%x", connection.write.Bytes())
	}
}
