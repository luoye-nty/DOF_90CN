package dnfbridge

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// Party dungeon entry is reached from the dungeon-selection page, and opening
// that page retires town presence by operator instruction. A co-presence test
// against the live roster alone would therefore refuse every party entry with
// "party member N is not present in the leader town area"; the retired record
// has to count while the page is up.
func TestPartyDungeonEntrySurvivesTheSelectorPresenceRetirement(t *testing.T) {
	service, leader, follower := prepareCurrentPartySelectorEntryTest(t)

	// Both members open the dungeon-selection page: this is what the live
	// clients do before the leader picks a dungeon.
	service.leaveTownPresenceForDungeonSelector(leader, "test_selector_open_leader")
	service.leaveTownPresenceForDungeonSelector(follower, "test_selector_open_follower")
	if service.onlinePlayers.PeerInSameArea(99, 100) {
		t.Fatal("the fixture no longer reproduces the retirement it is guarding against")
	}
	if !service.currentPartyEntryMembersShareTownArea(leader, follower) {
		t.Fatal("retired records in the same area must count as co-present")
	}

	prepared, err := currentPartySelectorEntryPreflight(t, service, leader)
	if err != nil {
		t.Fatalf("party entry preflight refused after the selector retirement: %v", err)
	}
	if len(prepared) != 1 || prepared[0].session != follower || prepared[0].runtime == nil {
		t.Fatalf("prepared followers=%d", len(prepared))
	}
}

// The retired record must not become a way around the area check: a member that
// browsed the selector from another town, area or channel is still refused.
func TestPartyDungeonEntryStillRefusesAMemberFromAnotherArea(t *testing.T) {
	for name, move := range map[string]func(*gameSession){
		"another town":    func(session *gameSession) { session.townSelectorRetiredPresence.TownID = 39 },
		"another area":    func(session *gameSession) { session.townSelectorRetiredPresence.AreaID = 2 },
		"another channel": func(session *gameSession) { session.channel.ID = 43 },
	} {
		t.Run(name, func(t *testing.T) {
			service, leader, follower := prepareCurrentPartySelectorEntryTest(t)
			service.leaveTownPresenceForDungeonSelector(leader, "test_selector_open_leader")
			service.leaveTownPresenceForDungeonSelector(follower, "test_selector_open_follower")
			move(follower)

			if service.currentPartyEntryMembersShareTownArea(leader, follower) {
				t.Fatalf("a follower in %s must not count as co-present", name)
			}
			prepared, err := currentPartySelectorEntryPreflight(t, service, leader)
			if err == nil || !strings.Contains(err.Error(), "is not present in the leader town area") {
				t.Fatalf("%s: preflight err=%v prepared=%d, want the co-presence refusal", name, err, len(prepared))
			}
			if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
				t.Fatalf("%s: entry must be refused before any member commits", name)
			}
		})
	}
}

// A member that holds a real town registration again must be judged on that,
// never on a stale retired record.
func TestCurrentPartyEntryCoPresencePrefersTheLiveRoster(t *testing.T) {
	service, leader, follower := prepareCurrentPartySelectorEntryTest(t)
	service.leaveTownPresenceForDungeonSelector(follower, "test_selector_open_follower")
	// The follower walked back into town, in a different area than the leader.
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 100, TownID: 38, AreaID: 2, Session: follower,
	})
	if service.currentPartyEntryMembersShareTownArea(leader, follower) {
		t.Fatal("a live registration in another area must win over the retired record")
	}
}

// currentPartySelectorEntryPreflight runs exactly the leader-side preparation
// the op16 handler runs before any member commits: the leader's own runtime,
// then the party preflight that judges co-presence.
func currentPartySelectorEntryPreflight(
	t *testing.T,
	service *Service,
	leader *gameSession,
) ([]*runtimePartyPreparedDungeonEntry, error) {
	t.Helper()
	selectBody := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(selectBody[:4], 700)
	request, err := dungeoncmd.DecodeSelectDungeonRequest(selectBody)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runtime, scene, err := service.prepareDungeonRuntime(ctx, leader, request)
	if err != nil {
		t.Fatalf("prepare leader runtime: %v", err)
	}
	return service.prepareRuntimePartyDungeonEntry(ctx, leader, runtimePartyStateSnapshot(leader), request, runtime, scene, false, false)
}

func prepareCurrentPartySelectorEntryTest(t *testing.T) (*Service, *gameSession, *gameSession) {
	t.Helper()
	source := bridgeDungeonMovePVF(false)
	source[bridgeDungeonMoveDungeonPath] = strings.Replace(
		source[bridgeDungeonMoveDungeonPath],
		"[limit party count]\n1\n",
		"[limit party count]\n4\n",
		1,
	)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, id := range []string{"99", "100"} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Job:         atSwordmanTutorialJob,
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed := uint32(0)
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { seed++; return 0x2000 + seed, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	leader := &gameSession{conn: &bufferConn{}, channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: &bufferConn{}, channel: channel, residentChannel: channel, selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state := alignedcmd.PartyState{
		PartyID: 99, UserID: 99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, state)
	storeRuntimePartyState(follower, state)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 99, TownID: 38, AreaID: 1, Session: leader,
	})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 100, TownID: 38, AreaID: 1, Session: follower,
	})
	bindDungeonSelectorOriginForTestAt(t, service, leader, 38, 1, 450, 234)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 38, 1, 450, 234)
	return service, leader, follower
}
