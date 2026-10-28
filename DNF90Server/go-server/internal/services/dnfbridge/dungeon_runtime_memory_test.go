package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestDungeonRuntimeMemorySnapshotDeduplicatesPartyRuntime(t *testing.T) {
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{ID: 1234},
		Rooms: map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit{
			{X: 1}: {},
		},
		LayerChains: make(map[worldmap.RoomCoordinate]*runtimeDungeonLayerChain),
	}
	first := &gameSession{selectedCharacterID: 19}
	second := &gameSession{selectedCharacterID: 20}
	first.dungeon.runtime = runtime
	second.dungeon.runtime = runtime
	service := &Service{gameSessions: map[uint16]*gameSession{19: first, 20: second}}

	snapshot := service.dungeonRuntimeMemorySnapshot()
	if snapshot.OnlineSessions != 2 || snapshot.ActiveRuntimes != 1 ||
		snapshot.TotalRooms != 1 || snapshot.TotalLayerChains != 0 ||
		snapshot.PayloadRuntimes != 1 {
		t.Fatalf("unexpected runtime snapshot: %+v", snapshot)
	}
}

func TestReleaseDungeonRuntimePayloadDropsLargeRunState(t *testing.T) {
	beforeReleases := dungeonRuntimePayloadReleaseCount.Load()
	runtime := &runtimeDungeonState{
		Rooms:                                  make(map[runtimeDungeonRoomKey]*runtimeDungeonRoomVisit),
		Room:                                   &runtimeDungeonRoom{},
		LayerChains:                            make(map[worldmap.RoomCoordinate]*runtimeDungeonLayerChain),
		StoryStages:                            []worldmap.DungeonStoryStage{{}},
		DropOwner:                              &runtimeDungeonDropOwner{},
		HellParty:                              &currentDungeonHellPartyOwner{},
		partyDungeonOwner:                      &runtimePartyDungeonOwner{},
		settlementPlayResultBody:               []byte{1},
		settlementStatisticBody:                []byte{2},
		partyCardSelectionPending:              [][]byte{{3}},
		settlementMonsterExperienceByObjectKey: map[uint32]uint32{4: 5},
		clearMapCompletionQuestIDs:             []int64{6},
	}
	session := &worldmap.DungeonSession{}
	runtime.Session = session

	releaseDungeonRuntimePayload(runtime)

	if runtime.Rooms != nil || runtime.Room != nil || runtime.LayerChains != nil || runtime.StoryStages != nil ||
		runtime.DropOwner != nil || runtime.HellParty != nil || runtime.partyDungeonOwner != nil ||
		runtime.settlementPlayResultBody != nil || runtime.settlementStatisticBody != nil ||
		runtime.partyCardSelectionPending != nil || runtime.settlementMonsterExperienceByObjectKey != nil ||
		runtime.clearMapCompletionQuestIDs != nil {
		t.Fatalf("large runtime payload retained: %+v", runtime)
	}
	if runtime.Session != session {
		t.Fatal("post-detach session pointer was cleared")
	}
	releaseDungeonRuntimePayload(runtime)
	if got := dungeonRuntimePayloadReleaseCount.Load(); got != beforeReleases+1 {
		t.Fatalf("release was not counted exactly once: before=%d after=%d", beforeReleases, got)
	}
}
