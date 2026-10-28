package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfmonster "longheng.io/server/internal/modules/dnf/monster"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestHarterFourthConditionMonsterOp39PublishesGateImmediatelyAfterOp38Once(t *testing.T) {
	service, runtime := prepareHarterGatePacketOrderTest(t)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "harter-fourth-tower-gate-order",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}

	monsters := runtime.Room.Snapshot().Monsters
	if len(monsters) != 4 {
		t.Fatalf("condition monster count=%d want=4", len(monsters))
	}
	for index, monster := range monsters {
		connection.write.Reset()
		request := make([]byte, dungeoncmd.DieMonsterRequestSize)
		binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
		binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(session.selectedCharacterID))

		if err := service.handleDungeonMonsterDeath(session, request); err != nil {
			t.Fatalf("condition death %d: %v", index+1, err)
		}
		op38, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
		wantOp38Body, err := buildCurrentDungeonDeathNotificationBody(
			monster.ObjectKey,
			currentDungeonDeathResponseMonster,
		)
		if err != nil {
			t.Fatal(err)
		}
		if op38.Header.Classification != 0 ||
			op38.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
			!bytes.Equal(op38.Body, wantOp38Body) {
			t.Fatalf("condition death %d first packet class=%d msg=%d body=%x want op38=%x",
				index+1, op38.Header.Classification, op38.Header.MsgID, op38.Body, wantOp38Body)
		}

		if index < 3 {
			if len(rest) != 0 {
				t.Fatalf("condition death %d published before fourth target: trailing=%x", index+1, rest)
			}
			if runtime.bossEntranceConditionComplete || runtime.bossEntranceGateNotificationSent {
				t.Fatalf("condition death %d complete=%t sent=%t",
					index+1, runtime.bossEntranceConditionComplete, runtime.bossEntranceGateNotificationSent)
			}
			continue
		}

		op312, trailing := splitGameServerUpperPacket(t, rest)
		if op312.Header.Classification != 0 ||
			op312.Header.MsgID != currentDungeonCompleteConditionPassGateMsgID ||
			!bytes.Equal(op312.Body, []byte{0, 0, 0, 0, 0}) {
			t.Fatalf("fourth condition death second packet class=%d msg=%d body=%x trailing=%x",
				op312.Header.Classification, op312.Header.MsgID, op312.Body, trailing)
		}
		for len(trailing) != 0 {
			packet, next := splitGameServerUpperPacket(t, trailing)
			if packet.Header.MsgID == currentDungeonCompleteConditionPassGateMsgID {
				t.Fatalf("fourth condition death duplicated op312 in tail: body=%x", packet.Body)
			}
			trailing = next
		}
		if !runtime.bossEntranceConditionComplete || !runtime.bossEntranceGateNotificationSent {
			t.Fatalf("fourth condition death complete=%t sent=%t",
				runtime.bossEntranceConditionComplete, runtime.bossEntranceGateNotificationSent)
		}

		connection.write.Reset()
		if err := service.handleDungeonMonsterDeath(session, request); err != nil {
			t.Fatalf("duplicate fourth condition death: %v", err)
		}
		if connection.write.Len() != 0 {
			t.Fatalf("duplicate fourth condition death emitted packets=%x", connection.write.Bytes())
		}
	}
}

func prepareHarterGatePacketOrderTest(t *testing.T) (*Service, *runtimeDungeonState) {
	t.Helper()
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] = "[map name]\n`start`\n" +
		"[dungeon]\n700\n" +
		"[type]\n`[start]`\n" +
		"[monster]\n" +
		"69201 10 0 100 200 0 0 0 `[fixed]` `[normal]`\n" +
		"69201 10 0 200 200 0 0 0 `[fixed]` `[normal]`\n" +
		"69210 10 0 300 200 0 0 0 `[fixed]` `[normal]`\n" +
		"69210 10 0 400 200 0 0 0 `[fixed]` `[normal]`\n"
	source[dnfmonster.DefaultList] = "69201 `tower_a.gob`\n69210 `tower_b.gob`\n"
	source["monster/tower_a.gob"] = "[name]\n`Harter Tower A`\n[level]\n70\n[hp]\n500\n[exp]\n0\n"
	source["monster/tower_b.gob"] = "[name]\n`Harter Tower B`\n[level]\n70\n[hp]\n500\n[exp]\n0\n"

	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       70,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots:       map[string]dnfrepo.ItemStack{},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	owner := &gameSession{selectedCharacterID: 99}
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		owner,
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ProtocolMode = currentDungeonProtocolModePractice
	runtime.Dungeon.ID = 2005
	runtime.Dungeon.Mazes[0].BossRoomEntranceCondition = &worldmap.BossRoomEntranceCondition{
		Action: "clear action",
		HuntMonsters: []worldmap.BossRoomEntranceHuntMonster{
			{TargetID: 69201, Count: 2},
			{TargetID: 69210, Count: 2},
		},
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}
	if err := runtime.cacheCurrentDungeonRoom(); err != nil {
		t.Fatal(err)
	}
	return service, runtime
}
