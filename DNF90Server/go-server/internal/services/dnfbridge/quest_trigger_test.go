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
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func questTriggerBridgeCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList:    "4728 `meet.qst`\n3193 `normal.qst`\n3622 `seek_meet.qst`\n2002 `hell_hunt.qst`\n",
		"n_quest/meet.qst":      "[grade]\n`[epic]`\n[type]\n`[meet npc]`\n",
		"n_quest/normal.qst":    "[grade]\n`[epic]`\n[type]\n`[normal clear]`\n",
		"n_quest/seek_meet.qst": "[grade]\n`[epic]`\n[type]\n`[seek n meet npc]`\n[int data]\n3037 500 126\n",
		"n_quest/hell_hunt.qst": "[grade]\n`[side]`\n[type]\n`[hunt enemy]`\n[int data]\n192 1 70034 1 1 193 1 70300 1 1 194 1 70307 1 1\n[reward type]\n`[hell challenge]`\n[reward int data]\n1\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCurrentSetQuestTriggerPersistsBeforeAckAndSuppressesNoopReplay(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: defaultAccountPrefix + "1", Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			4728: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{
		conn: connection, connID: "set-quest-trigger-test",
		channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 19,
	}
	body := []byte{0x21, 0x00, 0x78, 0x12, 0x00, 0x00}
	if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := repositories.Quest.Load(context.Background(), "19")
	if err != nil || !ok {
		t.Fatalf("load persisted quest ok=%t err=%v", ok, err)
	}
	if got := stored.States[4728].ProgressValue; got != 0 {
		t.Fatalf("persisted trigger=%d want=0", got)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		ack.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("ack header=%+v", ack.Header)
	}
	if want := []byte{0x01, 0x78, 0x12, 0x00, 0x00, 0x00, 0x00}; !bytes.Equal(ack.Body, want) {
		t.Fatalf("ack body=%x want=%x", ack.Body, want)
	}
	if len(rest) != 0 {
		t.Fatalf("terminal op33 must not be followed by op574: %x", rest)
	}

	connection.write.Reset()
	if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("same-session replay wrote packets=%x", got)
	}

	// A fresh session may be the first op33 after clear-map persistence already
	// reached active zero. It receives the one transition ACK, but no op574.
	newConnection := &bufferConn{}
	newSession := &gameSession{
		conn: newConnection, connID: "set-quest-trigger-reload-test",
		channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 19,
	}
	if err := service.handleCurrentSetQuestTrigger(newSession, body); err != nil {
		t.Fatal(err)
	}
	replayedACK, replayedTrailing := splitGameServerUpperPacket(t, newConnection.write.Bytes())
	if len(replayedTrailing) != 0 || replayedACK.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		!bytes.Equal(replayedACK.Body, []byte{0x01, 0x78, 0x12, 0x00, 0x00, 0x00, 0x00}) {
		t.Fatalf("persisted active-zero transition ACK=%+v trailing=%x", replayedACK, replayedTrailing)
	}
}

func TestCurrentSetQuestTriggerScalarRemoveClearsPackedObjectiveWithoutCounterLoop(t *testing.T) {
	// Live 2026-09-12T13:32:33Z (packet_log conn game-002646): quest 2900 held
	// the PVF initial trigger 262657 = packTriggerChannels(1, 1, 1) and the
	// scalar six-byte op33 shape used to subtract one from the whole packed u32.
	// Each request therefore changed the durable value and the client fed the
	// changed active-quest snapshot straight back through op33: 3767 identical
	// requests at ~30 Hz across seven logins (262656 -> 258892) while every town
	// entry stayed frozen. The scalar shape must answer the satisfied objective
	// once (terminal zero, no op574) and then stay silent.
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: defaultAccountPrefix + "1", Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "20",
		States: map[int64]dnfrepo.QuestState{
			2002: {Status: "active", ProgressValue: 1 | 1<<9 | 1<<18},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{
		conn: connection, connID: "scalar-packed-objective-trigger",
		channel: channelcatalog.Channel{ID: 20}, selectedCharacterID: 20,
	}
	body := []byte{0x21, 0x00, 0xd2, 0x07, 0x00, 0x00}
	if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(context.Background(), "20")
	if err != nil || !found || stored.States[2002].ProgressValue != 0 {
		t.Fatalf("persisted scalar-remove state found=%t err=%v state=%+v", found, err, stored.States[2002])
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
		binary.LittleEndian.Uint32(ack.Body[3:7]) != 0 || len(rest) != 0 {
		t.Fatalf("scalar-remove terminal ACK=%+v body=%x trailing=%x", ack.Header, ack.Body, rest)
	}

	connection.write.Reset()
	if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Bytes(); len(got) != 0 {
		t.Fatalf("replayed scalar op33 wrote packets=%x", got)
	}
}

func TestCurrentSetQuestTriggerPublishesPackedChannelsWithoutFeedbackPollution(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: defaultAccountPrefix + "1", Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "20",
		States: map[int64]dnfrepo.QuestState{
			3193: {Status: "active", ProgressValue: 513},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{
		conn: connection, connID: "set-packed-quest-trigger-test",
		channel: channelcatalog.Channel{ID: 20}, selectedCharacterID: 20,
	}

	for _, step := range []struct {
		triggerType byte
		want        uint32
	}{
		{triggerType: 0x10, want: 512},
		{triggerType: 0x20, want: 0},
	} {
		before := connection.write.Len()
		body := []byte{0x21, 0x00, 0x79, 0x0c, step.triggerType, 0x00}
		if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
			t.Fatal(err)
		}
		ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes()[before:])
		if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) || binary.LittleEndian.Uint32(ack.Body[3:7]) != step.want {
			t.Fatalf("type=%#x ACK header=%+v body=%x want trigger=%d", step.triggerType, ack.Header, ack.Body, step.want)
		}
		if step.want == 0 {
			if len(rest) != 0 {
				t.Fatalf("type=%#x terminal op33 trailing=%x", step.triggerType, rest)
			}
			continue
		}
		active, trailing := splitGameServerUpperPacket(t, rest)
		if len(trailing) != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID ||
			binary.LittleEndian.Uint32(active.Body[6:10]) != step.want {
			t.Fatalf("type=%#x active header=%+v body=%x trailing=%x want trigger=%d", step.triggerType, active.Header, active.Body, trailing, step.want)
		}
	}
}

func TestCurrentSetQuestTriggerAcceptsNativeMaskedHellPartyProgress(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: defaultAccountPrefix + "1", Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
		CharacterID: "20",
		States: map[int64]dnfrepo.QuestState{
			2002: {Status: "active", ProgressValue: 1 | 1<<9 | 1<<18},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{conn: connection, connID: "native-masked-hell-party-trigger", selectedCharacterID: 20}
	for _, step := range []struct {
		mask byte
		want uint32
	}{
		{mask: 0x10, want: 1<<9 | 1<<18},
		{mask: 0x20, want: 1 << 18},
		{mask: 0x40, want: 0},
	} {
		connection.write.Reset()
		body := []byte{0x21, 0x00, 0xd2, 0x07, step.mask, 0x00, 0x00}
		if err := service.handleCurrentSetQuestTrigger(session, body); err != nil {
			t.Fatal(err)
		}
		ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
		if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) ||
			binary.LittleEndian.Uint32(ack.Body[3:7]) != step.want {
			t.Fatalf("mask=%#x ACK header=%+v body=%x want trigger=%d", step.mask, ack.Header, ack.Body, step.want)
		}
		if step.want == 0 {
			if len(rest) != 0 {
				t.Fatalf("terminal masked op33 trailing=%x", rest)
			}
			continue
		}
		active, trailing := splitGameServerUpperPacket(t, rest)
		if len(trailing) != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID ||
			binary.LittleEndian.Uint32(active.Body[6:10]) != step.want {
			t.Fatalf("mask=%#x active header=%+v body=%x trailing=%x want trigger=%d", step.mask, active.Header, active.Body, trailing, step.want)
		}
	}
	stored, found, err := repositories.Quest.Load(context.Background(), "20")
	if err != nil || !found || stored.States[2002].ProgressValue != 0 {
		t.Fatalf("native masked Hell Party progress found=%t err=%v state=%+v", found, err, stored.States[2002])
	}
}

func TestCurrentSetQuestTriggerSeekAndMeetNPCUsesAccountCrystalCountWithoutFeedbackLoop(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "4", AccountID: "dnf:1", Job: "11", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "4",
		States: map[int64]dnfrepo.QuestState{
			3622: {Status: "active", ProgressValue: 816},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "dnf:1",
		Slots: map[string]dnfrepo.ItemStack{
			"0:358": {ItemID: 3037, Count: 120019},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{
		conn: connection, connID: "set-seek-meet-trigger-test",
		channel: channelcatalog.Channel{ID: 19}, selectedCharacterID: 4,
	}

	itemBody := []byte{0x21, 0x00, 0x26, 0x0e, 0x10, 0x00}
	if err := service.handleCurrentSetQuestTrigger(session, itemBody); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := repositories.Quest.Load(ctx, "4")
	if got := stored.States[3622].ProgressValue; got != 512 {
		t.Fatalf("item-channel progress=%d want=512", got)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) || binary.LittleEndian.Uint32(ack.Body[3:7]) != 512 {
		t.Fatalf("item-channel ACK=%+v body=%x", ack.Header, ack.Body)
	}
	active, trailing := splitGameServerUpperPacket(t, rest)
	if len(trailing) != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID || binary.LittleEndian.Uint32(active.Body[6:10]) != 512 {
		t.Fatalf("item-channel snapshot=%+v body=%x trailing=%x", active.Header, active.Body, trailing)
	}

	connection.write.Reset()
	if err := service.handleCurrentSetQuestTrigger(session, itemBody); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("resolved item-channel replay wrote packets=%x", connection.write.Bytes())
	}

	npcBody := []byte{0x21, 0x00, 0x26, 0x0e, 0x20, 0x00}
	if err := service.handleCurrentSetQuestTrigger(session, npcBody); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = repositories.Quest.Load(ctx, "4")
	if got := stored.States[3622].ProgressValue; got != 0 {
		t.Fatalf("terminal progress=%d want=0", got)
	}
	terminalACK, terminalTrailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if terminalACK.Header.MsgID != uint16(dnfenum.CmdPacketSetQuestTrigger) || binary.LittleEndian.Uint32(terminalACK.Body[3:7]) != 0 || len(terminalTrailing) != 0 {
		t.Fatalf("terminal ACK=%+v body=%x trailing=%x", terminalACK.Header, terminalACK.Body, terminalTrailing)
	}
}

func TestCurrentSetQuestTriggerSeekAndMeetNPCInsufficientMaterialsIsSilent(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "4", AccountID: "dnf:1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{CharacterID: "4", States: map[int64]dnfrepo.QuestState{
		3622: {Status: "active", ProgressValue: 1012},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{AccountID: "dnf:1", Slots: map[string]dnfrepo.ItemStack{
		"0:358": {ItemID: 3037, Count: 499},
	}}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		questCatalog:       questTriggerBridgeCatalog(t),
	}
	session := &gameSession{conn: connection, selectedCharacterID: 4}
	if err := service.handleCurrentSetQuestTrigger(session, []byte{0x21, 0x00, 0x26, 0x0e, 0x10, 0x00}); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := repositories.Quest.Load(ctx, "4")
	if got := stored.States[3622].ProgressValue; got != 1012 || connection.write.Len() != 0 {
		t.Fatalf("insufficient material progress=%d packets=%x", got, connection.write.Bytes())
	}
}

func TestNormalizeLegacySetQuestTriggerStripsOnlyExactCloneTrailer(t *testing.T) {
	semantic := []byte{0x21, 0x00, 0x78, 0x12, 0x00, 0x00}
	wrapped := append(append([]byte(nil), semantic...), 0xaa, 0xbb, 0xcc, 0xdd)
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketSetQuestTrigger), wrapped); !bytes.Equal(got, semantic) {
		t.Fatalf("normalized=%x want=%x", got, semantic)
	}
	opaque := append(append([]byte(nil), semantic...), 1, 2, 3, 4, 5)
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketSetQuestTrigger), opaque); !bytes.Equal(got, opaque) {
		t.Fatalf("non-exact body was changed: %x", got)
	}
}

func TestNormalizeLegacySetQuestTriggerPreservesNativeMaskedSevenByteForm(t *testing.T) {
	// Current-client op33 uses the seventh byte as the masked-form reserved
	// field. Removing it destroys the native 0x10/0x20/0x40 lane layout.
	wire := []byte{0x21, 0x00, 0x7b, 0x0d, 0x10, 0x00, 0x00}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketSetQuestTrigger), wire); !bytes.Equal(got, wire) {
		t.Fatalf("normalized=%x want=%x", got, wire)
	}
	opaque := []byte{0x20, 0x00, 0x7b, 0x0d, 0x10, 0x00, 0x00}
	if got := normalizeLegacyGameBody(uint16(dnfenum.CmdPacketSetQuestTrigger), opaque); !bytes.Equal(got, opaque) {
		t.Fatalf("non-matching wrapped body was changed: %x", got)
	}
}
