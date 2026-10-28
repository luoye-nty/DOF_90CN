package dnfbridge

import (
	"context"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 新手教程中途退出不能被罚虚弱（现场 2026-09-15）。
//
// 判定走 PVF 自己的 [tutorial dungeon] 标记（isPVFTutorialDungeon），而不是副本 id
// 或职业：每个职业的教程图都不一样，但这条 PVF 契约是共用的。这里走的是**真实退出链路**
// （op132 back-to-village -> dungeon_return_town.go 里那道 currentWeaknessExemptRoom 闸），
// 所以它同时钉住了"闸门读到的是 run 里那份 dungeon 文档"。
func TestTutorialDungeonExitDoesNotWeaken(t *testing.T) {
	cases := []struct {
		name          string
		tutorial      worldmap.OptionalInt
		wantLevel     int64
		wantExemption string
	}{
		{
			name:      "tutorial dungeon mid-run exit",
			tutorial:  worldmap.OptionalInt{Set: true, Value: 1},
			wantLevel: 0,
		},
		{
			name:      "[tutorial dungeon] 0 is an ordinary run",
			tutorial:  worldmap.OptionalInt{Set: true, Value: 0},
			wantLevel: currentWeaknessForcedReturnLevel,
		},
		{
			name:      "no marker at all is an ordinary run",
			tutorial:  worldmap.OptionalInt{},
			wantLevel: currentWeaknessForcedReturnLevel,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			runtime.Dungeon.ID = 21001
			runtime.Dungeon.Path = "dungeon/cataclysm/newtutorial/tutorial_swordman.dgn"
			runtime.Dungeon.Metadata.TutorialDungeon = testCase.tutorial
			if got := isPVFTutorialDungeon(runtime); got != (testCase.wantLevel == 0) {
				t.Fatalf("isPVFTutorialDungeon=%t for %+v", got, testCase.tutorial)
			}
			request, err := dnfproto.BuildChannelPacket(
				uint16(dnfenum.CmdPacketBack2Village), nil, 0, dnfproto.DefaultChannelClassification,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleGameUpper(session, request); err != nil {
				t.Fatalf("handle op132: %v", err)
			}
			repositories, ok := service.repositoryGroup()
			if !ok {
				t.Fatal("no repositories")
			}
			character, _, err := repositories.Character.Load(
				context.Background(), strconv.Itoa(int(session.selectedCharacterID)))
			if err != nil {
				t.Fatal(err)
			}
			if got := currentWeaknessLevel(character); got != testCase.wantLevel {
				t.Fatalf("weakness after tutorial exit=%d want %d", got, testCase.wantLevel)
			}
		})
	}
}
