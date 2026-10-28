package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func assertCurrentDungeonFatiguePromptStream(
	t *testing.T,
	stream []byte,
	wantReason byte,
) {
	t.Helper()
	if currentSelectDungeonFatigueExhaustedReason != 255 {
		t.Fatalf("fatigue reason = %d, want text-free selector marker 255", currentSelectDungeonFatigueExhaustedReason)
	}
	if currentSelectDungeonPartyFatigueExhaustedReason != 254 {
		t.Fatalf("party fatigue reason = %d, want text-free selector marker 254", currentSelectDungeonPartyFatigueExhaustedReason)
	}
	failure, rest := splitGameServerUpperPacket(t, stream)
	if failure.Header.Classification != dnfproto.DefaultChannelClassification ||
		failure.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) ||
		!bytes.Equal(failure.Body, buildSelectDungeonFailureBody(wantReason)) ||
		len(rest) != 0 {
		t.Fatalf("fatigue failure packet=%+v body=%x want_reason=%d", failure.Header, failure.Body, wantReason)
	}
}

func TestDungeonEntryFatigueUsesNativeSelectorFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		party      bool
		wantReason byte
	}{
		{name: "solo", wantReason: currentSelectDungeonFatigueExhaustedReason},
		{name: "party leader", party: true, wantReason: currentSelectDungeonPartyFatigueExhaustedReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := bridgeDungeonPVF(false)
			if test.party {
				source["dungeon/test.dgn"] = strings.Replace(
					source["dungeon/test.dgn"],
					"[limit party count]\n1\n",
					"[limit party count]\n4\n",
					1,
				)
			}
			table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
			repositories := dnfrepomemory.NewMemoryGroup()
			if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
				CharacterID: "99",
				AccountID:   "account-1",
				Level:       20,
				Stats: map[string]int64{
					"fatigue": 0, "town_id": 38, "area_id": 1,
					"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
				},
			}); err != nil {
				t.Fatal(err)
			}
			service := &Service{
				options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
				worldMapTable:       table,
				worldMapResolver:    resolver,
				dungeonMonsterTable: monsters,
				repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 99}
			if test.party {
				storeRuntimePartyState(session, alignedcmd.PartyState{
					PartyID: 99,
					UserID:  99,
					Members: []alignedcmd.PartyMemberState{
						{UserID: 99, UserState: 1},
						{UserID: 100, UserState: 1},
					},
				})
			}
			body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
			binary.LittleEndian.PutUint32(body[:4], 700)
			if err := service.handleDungeonSelectUpper(session, body); err != nil {
				t.Fatal(err)
			}
			assertCurrentDungeonFatiguePromptStream(t, connection.write.Bytes(), test.wantReason)
			if session.dungeon.runtime != nil {
				t.Fatalf("fatigue rejection committed runtime=%+v", session.dungeon.runtime)
			}
		})
	}
}

func TestPartyDungeonEntryFollowerFatiguePromptsOnlyLeader(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(
		source["dungeon/test.dgn"],
		"[limit party count]\n1\n",
		"[limit party count]\n4\n",
		1,
	)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	for id, fatigue := range map[string]int64{"99": 100, "100": 0} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Level:       20,
			Stats: map[string]int64{
				"fatigue": fatigue, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id,
			Slots:       map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	leaderConnection, followerConnection := &bufferConn{}, &bufferConn{}
	leader := &gameSession{conn: leaderConnection, channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: followerConnection, channel: channel, residentChannel: channel, selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	state := alignedcmd.PartyState{
		PartyID: 99,
		UserID:  99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(leader, state)
	storeRuntimePartyState(follower, state)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 99, TownID: 38, AreaID: 1, Session: leader})
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 100, TownID: 38, AreaID: 1, Session: follower})
	bindDungeonSelectorOriginForTestAt(t, service, leader, 38, 1, 450, 234)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 38, 1, 450, 234)

	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[:4], 700)
	if err := service.handleDungeonSelectUpper(leader, body); err != nil {
		t.Fatal(err)
	}
	assertCurrentDungeonFatiguePromptStream(
		t,
		leaderConnection.write.Bytes(),
		currentSelectDungeonPartyFatigueExhaustedReason,
	)
	if followerConnection.write.Len() != 0 {
		t.Fatalf("follower fatigue rejection wrote unsolicited packets=%x", followerConnection.write.Bytes())
	}
	if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
		t.Fatalf("follower fatigue rejection committed runtime leader=%p follower=%p", leader.dungeon.runtime, follower.dungeon.runtime)
	}
}
