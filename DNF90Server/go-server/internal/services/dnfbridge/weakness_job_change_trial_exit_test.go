package dnfbridge

import (
	"context"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// 转职体验「领悟时间」(7142) 结束体验回城不能被罚虚弱（现场 2026-09-17：
// packet_log 里 `dungeon_id=7142 exemption=none weakness_level=9
// client_condition=19`，体验 14 秒就顶着 19% 血量上限回城）。
//
// 走的是**真实退出链路**（op132 back-to-village -> dungeon_return_town.go 里那道
// currentWeaknessExemptRoom 闸）。体验副本和修炼场共用「直接进入」按钮形状，所以
// 豁免只能按副本 id 认；用例同时钉住"体验房放行、id 紧邻的普通房照旧罚"，
// 免得白名单哪天被写成范围匹配。
func TestJobChangeTrialRoomExitDoesNotWeaken(t *testing.T) {
	cases := []struct {
		name      string
		dungeonID int64
		path      string
		wantLevel int64
	}{
		{
			name:      "job-change trial 领悟时间",
			dungeonID: int64(currentJobChangeTrialDungeonID),
			path:      "dungeon/Cataclysm/ChangeClass/7142_ChangeClass.dgn",
			wantLevel: 0,
		},
		{
			// 只改 id：这道闸读的就是 run 里那份 dungeon 文档的 id。
			name:      "neighbouring dungeon id is an ordinary run",
			dungeonID: int64(currentJobChangeTrialDungeonID) + 1,
			wantLevel: currentWeaknessForcedReturnLevel,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			runtime.Dungeon.ID = testCase.dungeonID
			if testCase.path != "" {
				runtime.Dungeon.Path = testCase.path
			}
			// 豁免不是从教程标记或 op16 练习模式来的：体验副本两样都不是。
			if isPVFTutorialDungeon(runtime) || currentDungeonRuntimeIsPractice(runtime) {
				t.Fatal("the exit was spared by the tutorial or practice arm, not the room whitelist")
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
				t.Fatalf("weakness after exit=%d want %d", got, testCase.wantLevel)
			}
		})
	}
}
