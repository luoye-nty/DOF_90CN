package dnfbridge

import (
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

func TestRuntimeRaidSingleSelectorMappingKeepsDedicatedPVFMaps(t *testing.T) {
	tests := []struct {
		typ      byte
		raidMap  int
		selector int
	}{
		{typ: 0, raidMap: 5314, selector: 210},
		{typ: 0, raidMap: 5317, selector: 213},
		{typ: 0, raidMap: 5322, selector: 218},
		{typ: 0, raidMap: 5319, selector: 219},
		{typ: 0, raidMap: 5320, selector: 220},
		{typ: 3, raidMap: 5315, selector: 212},
		{typ: 3, raidMap: 5318, selector: 218},
		{typ: 6, raidMap: 5316, selector: 216},
		{typ: 6, raidMap: 5321, selector: 218},
	}
	for _, test := range tests {
		selector, ok := runtimeRaidSingleSelectorMapID(test.typ, test.raidMap)
		if !ok || selector != test.selector {
			t.Fatalf("type=%d raid_map=%d selector=(%d,%t), want=%d", test.typ, test.raidMap, selector, ok, test.selector)
		}
		raidMap, ok := runtimeRaidSinglePVFMapID(test.typ, test.selector)
		if !ok || raidMap != test.raidMap {
			t.Fatalf("type=%d selector=%d raid_map=(%d,%t), want=%d", test.typ, test.selector, raidMap, ok, test.raidMap)
		}
	}
	for _, test := range []struct {
		typ     byte
		raidMap int
	}{
		{typ: 0, raidMap: 5315},
		{typ: 0, raidMap: 5316},
		{typ: 3, raidMap: 5317},
		{typ: 6, raidMap: 5317},
	} {
		if selector, ok := runtimeRaidSingleSelectorMapID(test.typ, test.raidMap); ok {
			t.Fatalf("inactive branch type=%d raid_map=%d mapped to selector=%d", test.typ, test.raidMap, selector)
		}
	}
}

func TestResolveAntonRaidDungeonSelectionTranslatesOnlySingleRaidChannel(t *testing.T) {
	const raidKey = uint32(0x00170001)
	service := &Service{raids: map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey:        raidKey,
			LeaderID:       1001,
			TailFlag:       1,
			SingleRaidType: 6,
			Members:        []runtimeRaidMemberState{{CharID: 1001}},
		},
	}}
	session := &gameSession{channel: channelcatalog.Channel{ID: 23, Type: 23}}
	service.bindGameSessionCharacter(session, 1001)

	if got, translated := service.resolveAntonRaidDungeonSelection(session, 210); got != 5314 || !translated {
		t.Fatalf("single raid black-mist selection=(%d,%t), want=(5314,true)", got, translated)
	}
	if got, translated := service.resolveAntonRaidDungeonSelection(session, 216); got != 5316 || !translated {
		t.Fatalf("single raid fleet selection=(%d,%t), want=(5316,true)", got, translated)
	}
	if got, translated := service.resolveAntonRaidDungeonSelection(session, 5314); got != 5314 || translated {
		t.Fatalf("dedicated raid map selection=(%d,%t), want=(5314,false)", got, translated)
	}
	if got, projected := service.projectSingleAntonRaidClientDungeonID(session, 5314); got != 210 || !projected {
		t.Fatalf("single raid client dungeon projection=(%d,%t), want=(210,true)", got, projected)
	}

	session.channel.Type = 1
	if got, translated := service.resolveAntonRaidDungeonSelection(session, 210); got != 210 || translated {
		t.Fatalf("ordinary channel selection=(%d,%t), want=(210,false)", got, translated)
	}
	if got, projected := service.projectSingleAntonRaidClientDungeonID(session, 5314); got != 5314 || projected {
		t.Fatalf("ordinary channel client dungeon projection=(%d,%t), want=(5314,false)", got, projected)
	}

	session.channel.Type = 23
	service.raids[raidKey].TailFlag = 0
	if got, translated := service.resolveAntonRaidDungeonSelection(session, 210); got != 210 || translated {
		t.Fatalf("normal raid selection=(%d,%t), want=(210,false)", got, translated)
	}
	if got, projected := service.projectSingleAntonRaidClientDungeonID(session, 5314); got != 5314 || projected {
		t.Fatalf("normal raid client dungeon projection=(%d,%t), want=(5314,false)", got, projected)
	}
}

func TestRuntimeRaidLukeSingleSelectorUsesNativeMapsAndSymbols(t *testing.T) {
	luke := runtimeRaidBridgeTestLukeConfig()
	lifecycle, err := newRuntimeRaidLifecycleStateFromModeAndType(
		luke.Raid,
		runtimeRaidOwnerLuke,
		time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
		true,
		runtimeRaidLukeLightRoute,
	)
	if err != nil {
		t.Fatal(err)
	}
	state := runtimeRaidState{
		RaidKey:        0x00200001,
		LeaderID:       1001,
		RaidMode:       runtimeRaidOwnerLuke,
		TailFlag:       1,
		SingleRaidType: runtimeRaidLukeLightRoute,
		Started:        true,
		EntryCostPaid:  true,
		Members:        []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
		FrozenMembers:  []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
		Lifecycle:      lifecycle,
	}
	for mapID := 5323; mapID <= 5334; mapID++ {
		got, mapped := runtimeRaidSingleSelectorMapIDForState(state, mapID)
		if mapID <= 5328 {
			if !mapped || got != mapID {
				t.Fatalf("active Luke map %d selector=(%d,%t), want native identity", mapID, got, mapped)
			}
		} else if mapped {
			t.Fatalf("inactive dark-route map %d unexpectedly mapped to %d", mapID, got)
		}
		wantKey := uint32(73 + mapID - 5323)
		if key, ok := runtimeRaidObjectiveSymbolKey(mapID); !ok || key != wantKey {
			t.Fatalf("Luke map %d symbol=(%d,%t), want=%d", mapID, key, ok, wantKey)
		}
	}

	symbolBody, dungeonBody, err := runtimeRaidSingleSelectorSnapshotBodies(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(symbolBody) != 1+12*8 || symbolBody[0] != 12 {
		t.Fatalf("Luke symbol snapshot len=%d body=%x", len(symbolBody), symbolBody)
	}
	if len(dungeonBody) != 2+12*5+4 || dungeonBody[0] != 0 || dungeonBody[1] != 12 {
		t.Fatalf("Luke dungeon snapshot len=%d body=%x", len(dungeonBody), dungeonBody)
	}
	for index, mapID := range []uint32{5323, 5324, 5325, 5326, 5327, 5328, 5329, 5330, 5331, 5332, 5333, 5334} {
		offset := 2 + index*5
		if got := binary.LittleEndian.Uint32(dungeonBody[offset : offset+4]); got != mapID {
			t.Fatalf("Luke dungeon row %d id=%d want=%d", index, got, mapID)
		}
		wantState := runtimeRaidObjectiveWireState(raid.AntonDungeonHide)
		switch mapID {
		case 5323:
			wantState = runtimeRaidObjectiveWireState(raid.AntonDungeonOpen)
		case 5324:
			wantState = runtimeRaidObjectiveWireState(raid.AntonDungeonClosed)
		}
		if got := uint32(dungeonBody[offset+4]); got != wantState {
			t.Fatalf("Luke dungeon row %d state=%d want=%d", mapID, got, wantState)
		}
	}

	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	service := &Service{raids: map[uint32]*runtimeRaidState{state.RaidKey: &state}}
	session := &gameSession{channel: channelcatalog.Channel{ID: 241, Type: 32}}
	service.bindGameSessionCharacter(session, 1001)
	if got, translated := service.resolveAntonRaidDungeonSelection(session, 5323); got != 5323 || translated {
		t.Fatalf("Luke native selection=(%d,%t), want=(5323,false)", got, translated)
	}
	if got, projected := service.projectSingleAntonRaidClientDungeonID(session, 5323); got != 5323 || !projected {
		t.Fatalf("Luke native projection=(%d,%t), want=(5323,true)", got, projected)
	}
	if got, projected := service.projectSingleAntonRaidClientDungeonID(session, 5329); got != 5329 || projected {
		t.Fatalf("inactive Luke route projection=(%d,%t), want=(5329,false)", got, projected)
	}
	if err := service.validateAntonRaidDungeonEntry(session, 5323); err != nil {
		t.Fatalf("open Luke objective rejected: %v", err)
	}
	if err := service.validateAntonRaidDungeonEntry(session, 5324); err == nil {
		t.Fatal("closed Luke objective was admitted")
	}
}
