package dnfbridge

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentImageCommunicationEquipmentUseSendsActivePVFNPCWithoutMutation(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	wantCharacter := dnfrepo.CharacterRecord{CharacterID: "19", Stats: map[string]int64{"gold": 12345}}
	if err := repositories.Character.Save(context.Background(), wantCharacter); err != nil {
		t.Fatal(err)
	}
	wantQuest := dnfrepo.QuestRecord{
		CharacterID: "19",
		States: map[int64]dnfrepo.QuestState{
			3734: {Status: "active", ProgressValue: 1},
		},
	}
	if err := repositories.Quest.Save(context.Background(), wantQuest); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildImageCommunicationTestCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "image-communication-test",
		channel:             channelcatalog.Channel{ID: 38},
		selectedCharacterID: 19,
	}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		currentImageCommunicationEquipmentMsgID,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	success, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if success.Header.Classification != 1 || success.Header.MsgID != currentImageCommunicationEquipmentMsgID {
		t.Fatalf("success header=%+v trailing=%x", success.Header, trailing)
	}
	if want := []byte{1, 19, 0, 0, 0, 0xd0, 0x07, 0, 0}; !bytes.Equal(success.Body, want) {
		t.Fatalf("success body=%x want=%x", success.Body, want)
	}
	notify, trailing := splitGameServerUpperPacket(t, trailing)
	if len(trailing) != 0 || notify.Header.Classification != 0 || notify.Header.MsgID != currentImageCommunicationEquipmentMsgID {
		t.Fatalf("notify header=%+v trailing=%x", notify.Header, trailing)
	}
	if want := []byte{19, 0, 0, 0}; !bytes.Equal(notify.Body, want) {
		t.Fatalf("notify body=%x want=%x", notify.Body, want)
	}
	gotCharacter, foundCharacter, characterErr := repositories.Character.Load(context.Background(), "19")
	if characterErr != nil || !foundCharacter || !reflect.DeepEqual(gotCharacter.Stats, wantCharacter.Stats) {
		t.Fatalf("character assets mutated: %+v err=%v", gotCharacter, characterErr)
	}
	gotQuest, found, err := repositories.Quest.Load(context.Background(), "19")
	if err != nil || !found || !reflect.DeepEqual(gotQuest.States, wantQuest.States) || !reflect.DeepEqual(gotQuest.Progress, wantQuest.Progress) {
		t.Fatalf("quest mutated found=%t err=%v got=%+v want=%+v", found, err, gotQuest, wantQuest)
	}
}

func TestCurrentImageCommunicationEquipmentUseRejectsMalformedOrInactiveRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   []byte
		status string
	}{
		{name: "malformed body", body: []byte{1}, status: "active"},
		{name: "completed quest", status: "completed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{
				CharacterID: "19",
				States:      map[int64]dnfrepo.QuestState{3734: {Status: test.status}},
			}); err != nil {
				t.Fatal(err)
			}
			connection := &bufferConn{}
			service := &Service{
				questCatalog:       buildImageCommunicationTestCatalog(t),
				repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
			}
			session := &gameSession{conn: connection, selectedCharacterID: 19}
			if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), currentImageCommunicationEquipmentMsgID, test.body); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != 0 {
				t.Fatalf("blocked request emitted packet=%x", connection.write.Bytes())
			}
		})
	}
}

func buildImageCommunicationTestCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList:              "3734 `image_communication.qst`\n",
		"n_quest/image_communication.qst": "[name]\n`神秘的魔刹石`\n[condition message]\n`利用通信装备与梅尔文联系。 (远程通话装置只能在相关任务存在的情况下才能使用)`\n[grade]\n`[epic]`\n[type]\n`[meet npc]`\n[int data]\n2000\n[level]\n82 90\n[job]\n`[all]`\n",
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
