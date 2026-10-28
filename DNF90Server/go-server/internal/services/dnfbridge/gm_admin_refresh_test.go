package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestSendSelectedCharacterNativeProgressionStateRefreshesTownActorBeforeCommittedOp37(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "dnf:1",
		Name:        "LevelTwo",
		Job:         "10",
		Level:       2,
		Stats:       map[string]int64{"exp": 1000},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "19",
		Skills:      map[int64]dnfrepo.SkillState{91: {Level: 1, Enabled: true}},
		Points: dnfrepo.SkillPointState{
			TotalSP: 30, RemainingSP: 30, SyncedLevel: 2,
		},
		// Reproduce the live failure: the selected alternate tree still has
		// its pre-sync level-one point state when the post-commit chain starts.
		Trees: map[int]dnfrepo.SkillTreeState{
			1: {
				Skills: map[int64]dnfrepo.SkillState{91: {Level: 1, Enabled: true}},
				Points: dnfrepo.SkillPointState{SyncedLevel: 1},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "native-level-progression",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
	}

	if err := service.sendSelectedCharacterNativeProgressionState(session, "event_server_level_up"); err != nil {
		t.Fatalf("sendSelectedCharacterNativeProgressionState error = %v", err)
	}

	mode0, rest := splitGameServerUpperPacketWithHeader(t, connection.write.Bytes(), service.gameUpperHeaderSize())
	if mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode0.Header.Classification != 0 || len(mode0.Body) == 0 || mode0.Body[0] != 0 {
		t.Fatalf("town actor level refresh header=%+v body=%x", mode0.Header, mode0.Body)
	}
	if level, _, ok := currentSceneObjectLevelForLog(mode0.Body); !ok || level != 2 {
		t.Fatalf("town actor mode0 level=%d ok=%t body=%x", level, ok, mode0.Body)
	}

	op37, trailing := splitGameServerUpperPacketWithHeader(t, rest, service.gameUpperHeaderSize())
	if op37.Header.MsgID != currentDungeonCharacterStateMsgID ||
		op37.Header.Classification != 0 || len(op37.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("op37 header=%+v body=%x", op37.Header, op37.Body)
	}
	if op37.Body[0] != 2 || binary.LittleEndian.Uint32(op37.Body[1:5]) != 1000 ||
		binary.LittleEndian.Uint16(op37.Body[9:11]) != 30 ||
		binary.LittleEndian.Uint16(op37.Body[11:13]) != 30 {
		t.Fatalf("op37 committed level/exp/SP body=%x", op37.Body)
	}
	if len(trailing) != 0 {
		t.Fatalf("unexpected trailing progression packets=%x", trailing)
	}
}
