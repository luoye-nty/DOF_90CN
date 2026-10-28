package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// 修炼场（5000）走的是直接进入按钮：op16 state 1 + token 0xFFFF，
// currentDungeonProtocolModeForWire 刻意把它映回 normal 模式。所以
// currentDungeonRuntimeIsPractice 对这个房间永远是 false —— 白名单必须按
// 地下城 id 认，否则每次进出修炼场都要挨一次 19% 血量上限惩罚。
func TestCurrentWeaknessExemptRoomWhitelistsTrainingRoom(t *testing.T) {
	mode, ok := currentDungeonProtocolModeForWire(1, 0xffff)
	if !ok || mode != currentDungeonProtocolModeNormal {
		t.Fatalf("training room direct entry wire mode = %v ok=%t, want normal", mode, ok)
	}
	runtime := &runtimeDungeonState{
		ProtocolMode: mode,
		Dungeon:      worldmap.Dungeon{ID: int64(currentPungjinTrainingDungeonID)},
	}
	if currentDungeonRuntimeIsPractice(runtime) {
		t.Fatal("the training room must not be mistaken for the op16 practice protocol mode")
	}
	rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime)
	if !exempt || rule != currentWeaknessExemptRuleDungeonWhitelist {
		t.Fatalf("training room exemption rule=%q exempt=%t", rule, exempt)
	}
}

// 转职体验「领悟时间」(7142) 和修炼场一样走直接进入按钮，出口也是普通强退：
// 结束体验回城时 run 以 abandoned 提交，于是体验副本本来会白吃一次 19% 血量上限
// （现场 2026-09-17）。它是白名单的第二项，且带自己的规则名，日志里能和
// 修炼场分开认。
func TestCurrentWeaknessExemptRoomWhitelistsJobChangeTrial(t *testing.T) {
	runtime := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		Dungeon: worldmap.Dungeon{
			ID:   int64(currentJobChangeTrialDungeonID),
			Path: "dungeon/Cataclysm/ChangeClass/7142_ChangeClass.dgn",
		},
	}
	// 它不是教程图，也没有 [tutorial dungeon] 标记：豁免只能按副本 id 认。
	if isPVFTutorialDungeon(runtime) {
		t.Fatal("the job-change trial must not be mistaken for a tutorial dungeon")
	}
	rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime)
	if !exempt || rule != currentWeaknessExemptRuleJobChangeTrialDungeon {
		t.Fatalf("job-change trial exemption rule=%q exempt=%t", rule, exempt)
	}
	// 修炼场仍然报它自己的老规则名，两个房间不互相顶替。
	training := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		Dungeon:      worldmap.Dungeon{ID: int64(currentPungjinTrainingDungeonID)},
	}
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, training); !exempt ||
		rule != currentWeaknessExemptRuleDungeonWhitelist {
		t.Fatalf("training room exemption rule=%q exempt=%t", rule, exempt)
	}
}

func TestCurrentWeaknessExemptRoomKeepsChargingOrdinaryDungeons(t *testing.T) {
	runtime := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		// 和体验副本同一层、同一套资源名的普通房间照旧要罚。
		Dungeon: worldmap.Dungeon{ID: 11000},
	}
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime); exempt {
		t.Fatalf("ordinary dungeon was exempted by rule %q", rule)
	}
	runtime.Dungeon.ID = 7115
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime); exempt {
		t.Fatalf("ordinary dungeon was exempted by rule %q", rule)
	}
	runtime.Dungeon.ID = int64(currentJobChangeTrialDungeonID) + 1
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime); exempt {
		t.Fatalf("a neighbouring dungeon id was exempted by rule %q", rule)
	}
}

func TestCurrentWeaknessExemptRoomKeepsPracticeProtocolMode(t *testing.T) {
	runtime := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModePractice,
		Dungeon:      worldmap.Dungeon{ID: 11000},
	}
	rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime)
	if !exempt || rule != currentWeaknessExemptRulePracticeProtocol {
		t.Fatalf("practice protocol mode rule=%q exempt=%t", rule, exempt)
	}
}

func TestCurrentWeaknessExemptRoomWhitelistsPVPChannel(t *testing.T) {
	runtime := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		Dungeon:      worldmap.Dungeon{ID: 11000},
	}
	session := &gameSession{channel: channelcatalog.Channel{ID: 501, Group: "pvp"}}
	rule, exempt := currentWeaknessExemptRoom(session, runtime)
	if !exempt || rule != currentWeaknessExemptRulePVPChannel {
		t.Fatalf("pvp channel rule=%q exempt=%t", rule, exempt)
	}
	town := &gameSession{channel: channelcatalog.Channel{ID: 42, Group: "normal"}}
	if rule, exempt := currentWeaknessExemptRoom(town, runtime); exempt {
		t.Fatalf("ordinary channel was exempted by rule %q", rule)
	}
}

// 新手教程副本中途退出不能被罚虚弱：判定用 PVF 的 [tutorial dungeon] 标记
// （isPVFTutorialDungeon），所以每个职业自己的教程图都自动算数，不需要维护副本 id 表。
// 现场 2026-09-15：新建角色教程没打完就退出，出城就顶着一个 19% 血量上限。
func TestCurrentWeaknessExemptRoomWhitelistsEveryTutorialDungeon(t *testing.T) {
	for _, dungeon := range []worldmap.Dungeon{
		{ID: 21001, Path: "dungeon/cataclysm/newtutorial/tutorial_swordman.dgn",
			Metadata: worldmap.DungeonMetadata{TutorialDungeon: worldmap.OptionalInt{Set: true, Value: 1}}},
		{ID: 40001, Path: "dungeon/tutorial/tutorial_creator.dgn",
			Metadata: worldmap.DungeonMetadata{TutorialDungeon: worldmap.OptionalInt{Set: true, Value: 1}}},
	} {
		runtime := &runtimeDungeonState{ProtocolMode: currentDungeonProtocolModeNormal, Dungeon: dungeon}
		rule, exempt := currentWeaknessExemptRoom(&gameSession{}, runtime)
		if !exempt || rule != currentWeaknessExemptRuleTutorialDungeon {
			t.Fatalf("tutorial dungeon %d rule=%q exempt=%t", dungeon.ID, rule, exempt)
		}
	}
	// [tutorial dungeon] 0（显式关闭）不是教程，照旧要罚。
	notTutorial := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		Dungeon: worldmap.Dungeon{ID: 21001, Metadata: worldmap.DungeonMetadata{
			TutorialDungeon: worldmap.OptionalInt{Set: true, Value: 0},
		}},
	}
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, notTutorial); exempt {
		t.Fatalf("explicit [tutorial dungeon] 0 was exempted by rule %q", rule)
	}
	// 普通副本（没有这条标记）不受影响。
	ordinary := &runtimeDungeonState{
		ProtocolMode: currentDungeonProtocolModeNormal,
		Dungeon:      worldmap.Dungeon{ID: 21001},
	}
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, ordinary); exempt {
		t.Fatalf("a dungeon without the tutorial marker was exempted by rule %q", rule)
	}
}

// A nil runtime is the PVP-matching shape (决斗场 builds no dungeon runtime),
// and it must not be treated as an exemption on its own.
func TestCurrentWeaknessExemptRoomHandlesAMissingRuntime(t *testing.T) {
	if rule, exempt := currentWeaknessExemptRoom(&gameSession{}, nil); exempt {
		t.Fatalf("a missing runtime was exempted by rule %q", rule)
	}
	if rule, exempt := currentWeaknessExemptRoom(nil, nil); exempt {
		t.Fatalf("a missing session was exempted by rule %q", rule)
	}
}
