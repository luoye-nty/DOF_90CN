package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func seedPartyOwnerActorProjectionForTest(
	t *testing.T,
	service *Service,
	session *gameSession,
	characterID uint16,
	ownerChannel byte,
) {
	t.Helper()
	characterIDLoaded, characterName, character, found := service.characterForEnter(
		context.Background(),
		session,
		characterID,
	)
	if !found || characterIDLoaded != characterID {
		t.Fatalf("load owner projection character %d", characterID)
	}
	session.connectionTownActorOwnerChannel = ownerChannel
	session.townActorOwnerChannel = ownerChannel
	mode0, err := service.buildCurrentSceneObjectListBodyForSessionInContextStrict(
		context.Background(),
		session,
		characterID,
		characterName,
		character,
		true,
		ownerChannel,
	)
	if err != nil {
		t.Fatal(err)
	}
	adventure := service.currentAccountAdventureGroupSummaryForPacket(
		context.Background(),
		session,
		character,
		true,
	)
	mode1 := service.buildCurrentActorBindingMode1BodyForSelectedWithEquipmentInContext(
		context.Background(),
		session,
		character,
		true,
		characterID,
		uint32(adventure.ManageLevel),
		ownerChannel,
		true,
	)
	if err := captureCurrentPartyActorProjection(session, characterID, mode0, mode1); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePartyMemberIndexUsesStableNativeSlot(t *testing.T) {
	state := alignedcmd.PartyState{
		PartyID: 2,
		UserID:  10,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 10, Slot: 0, UserState: 100},
			{UserID: 30, Slot: 2, UserState: 100},
		},
	}
	if slot, ok := runtimePartyMemberIndex(30, state); !ok || slot != 2 {
		t.Fatalf("stable party slot=%d ok=%t, want slot 2", slot, ok)
	}
}

func TestPartyLeaderDungeonSelectionEntersEveryOnlineMemberWithStablePartyIndex(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(source["dungeon/test.dgn"], "[limit party count]\n1\n", "[limit party count]\n4\n", 1)
	source["dungeon/test.dgn"] += "[required item]\n4726 1 0\n[/required item]\n"
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	accounts := map[string]string{"99": "account-leader", "100": "account-follower"}
	for index, id := range []string{"99", "100"} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   accounts[id],
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		itemID := int64(116010135 + index)
		serial := 20 + index
		raw := make([]byte, currentItemListEntryWireSize)
		binary.LittleEndian.PutUint16(raw[0:2], 17)
		binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
		raw[0x27] = 1
		binary.LittleEndian.PutUint32(raw[0x28:0x2c], currentChronicleCanonicalAuraBase+2)
		raw[0x30] = 15
		raw[0x32] = 2
		raw[0x34] = 12
		raw[0x36] = 11
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: id,
			Slots: map[string]dnfrepo.ItemStack{
				"0:17": {
					ItemID:   itemID,
					Count:    1,
					RawEntry: raw,
					Extra:    map[string]string{"item_serial": strconv.Itoa(serial)},
				},
				"0:180": {ItemID: 4726, Count: 1},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := saveSkillSwitchInventoryState(context.Background(), repositories.Settings, id, skillSwitchInventoryState{
			SkillID: 46,
			Items: []skillSwitchBinding{{
				EquipmentPart: 12,
				ItemSerial:    uint32(serial),
				ItemID:        uint32(itemID),
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:             options{accountID: "fallback-account", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:      newFakeCurrentDungeonDeathTimerQueue(),
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	enableTownMoveSkillProjection(t, service, repositories, "100")
	// Exercise the actor-construction passive route on both receivers. Fear
	// Aura remains a paid learned skill: it is present only in each durable
	// active tree and is deliberately absent from initialSkillsByJob.
	fearAuraCatalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":         "0 `swordman.lst`\n",
		"skill/swordman.lst":          "46 `swordman/active.skl`\n89 `swordman/darkaura.skl`\n",
		"skill/swordman/active.skl":   "[type]\n`active`\n",
		"skill/swordman/darkaura.skl": "[type]\n`passive`\n[skill class]\n3\n[skill fitness second growtype]\n1 1\n[purchase cost]\n45\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.skillCatalog = fearAuraCatalog
	for _, characterID := range []string{"99", "100"} {
		character, found, err := repositories.Character.Load(context.Background(), characterID)
		if err != nil || !found {
			t.Fatalf("load party Soul Bender %s: found=%t err=%v", characterID, found, err)
		}
		character.Job = "0"
		character.Stats["grow_type"] = 0x12
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
		record, found, err := repositories.Skill.Load(context.Background(), characterID)
		if err != nil || !found {
			t.Fatalf("load party Soul Bender skill %s: found=%t err=%v", characterID, found, err)
		}
		record.Skills[currentSoulBenderFearAuraSkillID] = dnfrepo.SkillState{Level: 1, Enabled: true}
		activeTree, found := dnfrepo.SkillTree(record, 1)
		if !found {
			t.Fatalf("party Soul Bender %s has no active skill tree", characterID)
		}
		activeTree.Skills[currentSoulBenderFearAuraSkillID] = dnfrepo.SkillState{Level: 1, Enabled: true}
		dnfrepo.SetSkillTree(&record, 1, activeTree)
		if err := repositories.Skill.Save(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	writeLog := &orderedPartyWriteLog{}
	leaderConn, followerConn, observerConn := &bufferConn{}, &bufferConn{}, &bufferConn{}
	leaderWireConn := &orderedPartyConn{bufferConn: leaderConn, name: "leader", log: writeLog}
	followerWireConn := &orderedPartyConn{bufferConn: followerConn, name: "follower", log: writeLog}
	leader := &gameSession{conn: leaderWireConn, accountID: accounts["99"], channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: followerWireConn, accountID: accounts["100"], channel: channel, residentChannel: channel, selectedCharacterID: 100}
	observer := &gameSession{conn: observerConn, channel: channel, residentChannel: channel, selectedCharacterID: 101}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	seedPartyOwnerActorProjectionForTest(t, service, leader, 99, 42)
	seedPartyOwnerActorProjectionForTest(t, service, follower, 100, 42)
	registerPartyPeerEndpointForTest(leader, 3, 5063)
	registerPartyPeerEndpointForTest(follower, 3, 60359)
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
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: 101, TownID: 38, AreaID: 1, Session: observer})
	bindDungeonSelectorOriginForTestAt(t, service, leader, 38, 1, 450, 234)
	bindDungeonSelectorOriginForTestAt(t, service, follower, 38, 1, 450, 234)
	if got := runtimePartyStateSnapshot(leader); got.PartyID != 99 || len(got.Members) != 2 {
		t.Fatalf("leader party before dungeon entry=%+v", got)
	}
	// 这个 fixture 表示的是「队长从 op15 选图页选图进来」那条路 ——
	// bindDungeonSelectorOriginForTestAt 做的是 bindCurrentTownSelectorOrigin，
	// 也就是选图页打开时做的那次原点绑定。真实的选图页还会**发 op27 并把
	// enterSelectDungeonContextSent 置真**（sendCurrentTownEnterSelectContext，
	// town_enter_select.go:318/332）；helper 没做那一半，所以这里补上。
	//
	// 不补的话 fixture 表示的其实是另一条路：「服务端驱动的进图」——
	// 超时空遭遇战就是那种（op36 走进区域触发，玩家从来没有打开过选图页，
	// latch 一直是假）。两条路对 op27 的期望正好相反，见下面 wantPrelude 之后那段。
	leader.enterSelectDungeonContextSent = true
	if index, ok := runtimePartyMemberIndexForSession(leader); !ok || index != 0 {
		t.Fatalf("leader party index before dungeon entry=%d ok=%t", index, ok)
	}

	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[:4], 700)
	if err := service.handleDungeonSelectUpper(leader, body); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		characterID   string
		connection    *bufferConn
		admissionSlot uint16
	}{
		{name: "leader", characterID: "99", connection: leaderConn, admissionSlot: 180},
		{name: "follower", characterID: "100", connection: followerConn, admissionSlot: 180},
	} {
		inventory, found, err := repositories.Inventory.Load(context.Background(), test.characterID)
		if err != nil || !found {
			t.Fatalf("%s admission inventory found=%t err=%v", test.name, found, err)
		}
		if _, remains := inventory.Slots["0:180"]; remains {
			t.Fatalf("%s retained its consumed dungeon admission item", test.name)
		}
		if !containsAdmissionDeleteUpdateForSlot(t, test.connection.write.Bytes(), test.admissionSlot) {
			t.Fatalf("%s did not receive its own final-stack op14 deletion", test.name)
		}
	}
	writeLog.mu.Lock()
	orderedWrites := append([]orderedPartyWrite(nil), writeLog.writes...)
	writeLog.mu.Unlock()
	preparedBeforeRoom := make(map[string]bool, 2)
	roomPhaseSeen := false
	for _, write := range orderedWrites {
		packet, trailing := splitGameServerUpperPacket(t, write.data)
		if len(trailing) != 0 || packet.Header.Classification != 0 {
			continue
		}
		switch packet.Header.MsgID {
		case currentDungeonInfoNotification:
			if roomPhaseSeen {
				t.Fatalf("%s op28 arrived after initial room release", write.connection)
			}
			preparedBeforeRoom[write.connection] = true
		case currentTownActorSceneSnapshotMsgID, currentDungeonStartNotification:
			roomPhaseSeen = true
			if !preparedBeforeRoom["leader"] || !preparedBeforeRoom["follower"] {
				t.Fatalf("first op%d arrived before both members completed op28: %+v",
					packet.Header.MsgID, preparedBeforeRoom)
			}
		}
	}
	if !roomPhaseSeen {
		t.Fatal("party dungeon entry never released op800/op29")
	}
	actorProjectionBodies := func(stream []byte) [][]byte {
		packets := splitAllCurrentGameServerUpperPackets(t, stream)
		bodies := make([][]byte, 0, 2)
		for _, packet := range packets {
			if packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
				len(packet.Body) > 0 && (packet.Body[0] == 0 || packet.Body[0] == 1) {
				bodies = append(bodies, append([]byte(nil), packet.Body...))
			}
		}
		return bodies
	}
	leaderActorProjection := actorProjectionBodies(leaderConn.write.Bytes())
	followerActorProjection := actorProjectionBodies(followerConn.write.Bytes())
	if len(leaderActorProjection) != 2 || len(followerActorProjection) != 2 ||
		!bytes.Equal(leaderActorProjection[0], followerActorProjection[0]) ||
		!bytes.Equal(leaderActorProjection[1], followerActorProjection[1]) {
		t.Fatalf("party actor projection differs by receiver: leader=%d follower=%d", len(leaderActorProjection), len(followerActorProjection))
	}
	leaderOwnerProjection, leaderProjectionFound := currentPartyActorProjectionSnapshot(
		leader,
		99,
		leader.characterGeneration,
	)
	followerOwnerProjection, followerProjectionFound := currentPartyActorProjectionSnapshot(
		follower,
		100,
		follower.characterGeneration,
	)
	if !leaderProjectionFound || !followerProjectionFound {
		t.Fatal("party entry lost an owner-produced actor projection")
	}
	wantMode0 := append([]byte{0, 2, 0}, leaderOwnerProjection.mode0[3:]...)
	wantMode0 = append(wantMode0, followerOwnerProjection.mode0[3:]...)
	wantMode1 := append([]byte{1, 2, 0}, leaderOwnerProjection.mode1[3:]...)
	wantMode1 = append(wantMode1, followerOwnerProjection.mode1[3:]...)
	if !bytes.Equal(leaderActorProjection[0], wantMode0) ||
		!bytes.Equal(leaderActorProjection[1], wantMode1) {
		t.Fatal("party entry rebuilt owner-produced actor bytes instead of composing the frozen rows")
	}
	entryOwner := leader.dungeon.runtime.partyDungeonOwner
	if entryOwner == nil {
		t.Fatal("party dungeon entry has no shared owner")
	}
	entryOwner.mu.Lock()
	retainedEntryContinuations := len(entryOwner.entryContinuations)
	entryOwner.mu.Unlock()
	if retainedEntryContinuations != 0 {
		t.Fatalf("released party entry barrier retained %d old session/runtime continuations", retainedEntryContinuations)
	}
	firstProjection, err := service.runtimePartyDungeonActorProjectionForOwner(context.Background(), leader, entryOwner)
	if err != nil {
		t.Fatal(err)
	}
	firstProjection.mode0[0] ^= 0xff
	secondProjection, err := service.runtimePartyDungeonActorProjectionForOwner(context.Background(), follower, entryOwner)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(secondProjection.mode0, leaderActorProjection[0]) ||
		!bytes.Equal(secondProjection.mode1, leaderActorProjection[1]) {
		t.Fatal("party actor projection cache leaked a receiver mutation")
	}
	for _, test := range []struct {
		name       string
		session    *gameSession
		connection *bufferConn
		index      byte
	}{
		{name: "leader", session: leader, connection: leaderConn, index: 0},
		{name: "follower", session: follower, connection: followerConn, index: 1},
	} {
		if test.session.dungeon.runtime == nil || !test.session.dungeon.runtime.partyMemberIndexed ||
			test.session.dungeon.runtime.partyMemberIndex != test.index {
			t.Fatalf("%s runtime party index=%d indexed=%t", test.name,
				currentDungeonRuntimePartyMemberIndex(test.session.dungeon.runtime),
				test.session.dungeon.runtime != nil && test.session.dungeon.runtime.partyMemberIndexed)
		}
		if test.session.dungeon.runtime.Seed != 0x12345678 {
			t.Fatalf("%s seed=%08x", test.name, test.session.dungeon.runtime.Seed)
		}
		assertDungeonStartMapPartyIndex(t, test.connection.write.Bytes(), 0xff)
		packets := splitAllCurrentGameServerUpperPackets(t, test.connection.write.Bytes())
		op9Count := 0
		op9AfterOp29 := false
		op800BeforeOp29 := false
		seenOp800 := false
		seenOp29 := false
		var roomSlotTable []byte
		messages := make([]uint16, 0, len(packets))
		for _, packet := range packets {
			if packet.Header.Classification == 0 && packet.Header.MsgID == currentTownUserLeaveMsgID {
				t.Fatalf("%s received party peer town op6 during shared dungeon admission body=%x", test.name, packet.Body)
			}
			messages = append(messages, packet.Header.MsgID)
			switch packet.Header.MsgID {
			case uint16(dnfenum.CmdPacketRecoverStamina):
				op9Count++
				op9AfterOp29 = seenOp29
				roomSlotTable = packet.Body
			case currentTownActorSceneSnapshotMsgID:
				seenOp800 = true
			case currentDungeonStartNotification:
				op800BeforeOp29 = seenOp800
				seenOp29 = true
			}
		}
		// The prelude itself carries no op9 (the captured party flow has none
		// there); the single op9 is the room slot table, sent after op29 once
		// every party actor exists in the receiver's room. It is what arms the
		// client's host arbitration (sceneActorManager+0x48/+0x68).
		if op9Count != 1 || !op9AfterOp29 || !op800BeforeOp29 {
			t.Fatalf("%s party entry op9=%d op9_after_op29=%t op800_before_op29=%t", test.name, op9Count, op9AfterOp29, op800BeforeOp29)
		}
		assertPartyDungeonRoomSlotTable(t, test.name, roomSlotTable, currentSceneActorObjectKey(test.session.selectedCharacterID),
			map[byte]uint16{0: 99, 1: 100}, 0)
		wantPrelude := []uint16{
			currentTownUserAreaNotificationMsgID,
			uint16(dnfenum.CmdPacketSetUDPIPPort),
			currentSkillInfoMsgID,
			uint16(dnfenum.CmdPacketSetUDPIPPort),
			11,
			uint16(dnfenum.CmdPacketNotifyUserState),
			26,
			currentDungeonInfoNotification,
			currentTownActorSceneSnapshotMsgID,
			currentDungeonStartNotification,
			currentFatigueMsgID,
		}
		if !containsUint16Sequence(messages, wantPrelude) {
			t.Fatalf("%s party entry sequence=%v missing=%v", test.name, messages, wantPrelude)
		}
		// Pin the other half of the settlement-retry gate: a party entry by a member
		// that ALREADY has the dungeon context must not carry op27 again inside the
		// shared-dungeon prelude. A follower's own follower-selector context
		// (op36 + op27, before op5) is legitimate and is not what this guards; the
		// packet this guards is the one emitRuntimePartyDungeonStart inserts after
		// the op26 leader-slot notification. MsgID 26 is that notification (sent as
		// a literal 26).
		//
		// The condition is the LATCH, not "ordinary vs 再次挑战":
		//   * member already has the context (this fixture: the leader came through
		//     the op15 selector page, latch true) -> nothing to insert;
		//   * 再次挑战 (retryFromSettlement) -> insert every time, latch or not;
		//   * member has NEVER had the context (latch false) -> insert. That is the
		//     server-driven entry: Fiend War's encounter is triggered by op36 walking
		//     into an area, so the driver never opens a selector page. Live
		//     2026-10-01 23:07, without it the driver's client never switched dungeon
		//     context and never sent op37, i.e. it hung on the loading screen while
		//     the follower loaded fine. Covered by
		//     TestRealPVFFiendWarDifferentSquadsSharingOneCellEnterTogether.
		seenLeaderSlotNotification := false
		for _, message := range messages {
			if message == 26 {
				seenLeaderSlotNotification = true
				continue
			}
			if seenLeaderSlotNotification && message == currentDungeonContextMsgID {
				t.Fatalf("%s ordinary party entry emitted op27 inside the dungeon-start prelude sequence=%v",
					test.name, messages)
			}
		}
		prebindIndex, mode1Index := -1, -1
		for packetIndex, packet := range packets {
			if packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID {
				prebindIndex = packetIndex
				slots := currentSkillInfoFirstTreeSlots(t, packet.Body)
				foundFearAura := false
				for _, skillID := range slots {
					foundFearAura = foundFearAura || int64(skillID) == currentSoulBenderFearAuraSkillID
				}
				if !foundFearAura {
					t.Fatalf("%s party Fear Aura prebind slots=%v", test.name, slots)
				}
			}
			if packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) &&
				len(packet.Body) > 0 && packet.Body[0] == 1 {
				mode1Index = packetIndex
			}
		}
		if prebindIndex < 0 || mode1Index < 0 || prebindIndex >= mode1Index {
			t.Fatalf("%s party Fear Aura prebind=%d mode1=%d", test.name, prebindIndex, mode1Index)
		}
		if test.name == "follower" {
			assertDungeonPartyFollowerHasNoUnsolicitedSelectAck(t, test.connection.write.Bytes())
			assertDungeonPartyFollowerPrerequisitePrecedesRuntime(t, test.connection.write.Bytes())
		}
		if resident := service.onlinePlayers.SessionForCharacter(test.session.selectedCharacterID); resident != nil {
			t.Fatalf("%s retained town presence after dungeon entry: %p", test.name, resident)
		}
	}
	seenRetirements := map[uint16]bool{}
	observerStream := observerConn.write.Bytes()
	observerPackets := 0
	for len(observerStream) > 0 {
		packet, rest := splitLongHengGameServerUpperPacket(t, observerStream)
		observerPackets++
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentTownUserLeaveMsgID || len(packet.Body) < 2 {
			t.Fatalf("non-party observer transition packet=%+v body=%x", packet.Header, packet.Body)
		}
		seenRetirements[binary.LittleEndian.Uint16(packet.Body[:2])] = true
		observerStream = rest
	}
	if observerPackets != 2 {
		t.Fatalf("non-party observer dungeon-transition packets=%d want=2 stream=%x", observerPackets, observerConn.write.Bytes())
	}
	if !seenRetirements[99] || !seenRetirements[100] {
		t.Fatalf("non-party observer retirements=%v want leader and follower", seenRetirements)
	}

	leaderConn.write.Reset()
	followerConn.write.Reset()
	writeLog.mu.Lock()
	writeLog.writes = nil
	writeLog.mu.Unlock()
	if err := service.sendFinishLoadingStatus(leader, make([]byte, currentFinishLoadingLegacyRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	if got := splitAllCurrentGameServerUpperPackets(t, leaderConn.write.Bytes()); len(got) != 1 ||
		got[0].Header.Classification != dnfproto.DefaultChannelClassification ||
		got[0].Header.MsgID != uint16(dnfenum.CmdPacketFinishLoading) {
		t.Fatalf("first ready must receive ACK only: %+v", got)
	}
	if followerConn.write.Len() != 0 {
		t.Fatalf("unready follower received premature release=%x", followerConn.write.Bytes())
	}
	if err := service.sendFinishLoadingStatus(follower, make([]byte, currentFinishLoadingLegacyRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		connection *bufferConn
	}{
		{name: "leader", connection: leaderConn},
		{name: "follower", connection: followerConn},
	} {
		packets := splitAllCurrentGameServerUpperPackets(t, test.connection.write.Bytes())
		messages := make([]uint16, 0, len(packets))
		for _, packet := range packets {
			messages = append(messages, packet.Header.MsgID)
		}
		want := []uint16{
			uint16(dnfenum.CmdPacketFinishLoading),
			uint16(dnfenum.CmdPacketRequestBlacklist),
			uint16(dnfenum.CmdPacketRequestBlacklist),
			uint16(dnfenum.CmdPacketNotifyUserState),
			uint16(dnfenum.CmdPacketFinishLoading),
			currentIncreaseStatusResultMsgID,
			currentSkillInfoMsgID,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			skillSwitchInventoryAllDataWire,
			currentDungeonConsumableUseGateResetWireOpcode,
		}
		if !equalUint16Values(messages, want) {
			t.Fatalf("%s shared loading sequence=%v want=%v", test.name, messages, want)
		}
		finalSkills := packets[6]
		if finalSkills.Header.MsgID != currentSkillInfoMsgID {
			t.Fatalf("%s first-room final skill packet=op%d", test.name, finalSkills.Header.MsgID)
		}
		foundFearAura := false
		for _, skillID := range currentSkillInfoFirstTreeSlots(t, finalSkills.Body) {
			foundFearAura = foundFearAura || int64(skillID) == currentSoulBenderFearAuraSkillID
		}
		if !foundFearAura {
			t.Fatalf("%s first-room final op19 omitted Fear Aura", test.name)
		}
		if !bytes.Equal(packets[1].Body, []byte{0, 0}) || !bytes.Equal(packets[2].Body, []byte{1, 0}) {
			t.Fatalf("%s stable placements=%x/%x", test.name, packets[1].Body, packets[2].Body)
		}
		chronicleRefresh := packets[len(packets)-3]
		if len(chronicleRefresh.Body) != 3+currentItemListEntryWireSize ||
			chronicleRefresh.Body[0] != 0 ||
			binary.LittleEndian.Uint16(chronicleRefresh.Body[1:3]) != 1 ||
			chronicleRefresh.Body[3+0x27] != 1 ||
			binary.LittleEndian.Uint32(chronicleRefresh.Body[3+0x28:3+0x2c]) != currentChronicleCanonicalAuraBase+2 {
			t.Fatalf("%s registered Chronicle refresh body=%x", test.name, chronicleRefresh.Body)
		}
		if wantBody := []byte{46, 0, 1, 1, 12, 0, 17, 0}; !bytes.Equal(packets[len(packets)-2].Body, wantBody) {
			t.Fatalf("%s party BUFF registration body=%x want=%x", test.name, packets[len(packets)-2].Body, wantBody)
		}
		if reset := packets[len(packets)-1]; reset.Header.MsgID != currentDungeonConsumableUseGateResetWireOpcode ||
			reset.Header.Classification != 0 || len(reset.Body) != 0 {
			t.Fatalf("%s final consumable gate reset=%+v body=%x", test.name, reset.Header, reset.Body)
		}
	}
	writeLog.mu.Lock()
	loadingWrites := append([]orderedPartyWrite(nil), writeLog.writes...)
	writeLog.mu.Unlock()
	followerFinalIndex, leaderReleaseIndex := -1, -1
	for index, write := range loadingWrites {
		packet, trailing := splitGameServerUpperPacket(t, write.data)
		if len(trailing) != 0 || packet.Header.Classification != 0 {
			continue
		}
		if write.connection == "follower" && packet.Header.MsgID == currentDungeonConsumableUseGateResetWireOpcode {
			followerFinalIndex = index
		}
		if write.connection == "leader" && packet.Header.MsgID == uint16(dnfenum.CmdPacketRequestBlacklist) && leaderReleaseIndex < 0 {
			leaderReleaseIndex = index
		}
	}
	if followerFinalIndex < 0 || leaderReleaseIndex < 0 || followerFinalIndex >= leaderReleaseIndex {
		t.Fatalf("party loading activated leader before follower completed: follower_final=%d leader_release=%d writes=%d",
			followerFinalIndex, leaderReleaseIndex, len(loadingWrites))
	}
	// A room transition must not replay op19. The current client rebuilds its
	// complete skill manager when op19 arrives, which clears live P2P skill
	// effects (for example a teammate's Courage Blessing). The second and later
	// party loading cycles preserve that manager just like ordinary solo room
	// moves, while still publishing every room-ready state packet.
	leaderConn.write.Reset()
	followerConn.write.Reset()
	resetDungeonEntrySceneGates(leader)
	resetDungeonEntrySceneGates(follower)
	if cycle := entryOwner.beginRoomLoading(); cycle != 2 {
		t.Fatalf("second party loading cycle=%d want=2", cycle)
	}
	if err := service.sendFinishLoadingStatus(leader, make([]byte, currentFinishLoadingLegacyRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	if err := service.sendFinishLoadingStatus(follower, make([]byte, currentFinishLoadingLegacyRequestBodySize)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		session    *gameSession
		connection *bufferConn
	}{
		{name: "leader", session: leader, connection: leaderConn},
		{name: "follower", session: follower, connection: followerConn},
	} {
		packets := splitAllCurrentGameServerUpperPackets(t, test.connection.write.Bytes())
		messages := make([]uint16, 0, len(packets))
		for _, packet := range packets {
			messages = append(messages, packet.Header.MsgID)
			if packet.Header.Classification == 0 && (packet.Header.MsgID == currentSkillInfoMsgID ||
				packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
				packet.Header.MsgID == skillSwitchInventoryAllDataWire) {
				t.Fatalf("%s room-transition loading rebuilt skill/Buff manager with op%d body=%x",
					test.name, packet.Header.MsgID, packet.Body)
			}
		}
		want := []uint16{
			uint16(dnfenum.CmdPacketFinishLoading),
			uint16(dnfenum.CmdPacketRequestBlacklist),
			uint16(dnfenum.CmdPacketRequestBlacklist),
			uint16(dnfenum.CmdPacketNotifyUserState),
			uint16(dnfenum.CmdPacketFinishLoading),
			currentIncreaseStatusResultMsgID,
		}
		if !equalUint16Values(messages, want) {
			t.Fatalf("%s room-transition loading sequence=%v want=%v", test.name, messages, want)
		}
		if !test.session.dungeonSceneSkillInfoSent || !test.session.postFinishLoadingPlayerStateSent {
			t.Fatalf("%s room-transition lifecycle flags skill=%t post=%t",
				test.name, test.session.dungeonSceneSkillInfoSent, test.session.postFinishLoadingPlayerStateSent)
		}
	}
	// Member death is one shared room fact. A single death must not start the
	// party wipe transition, self-revive clears the shared slot, and only the
	// final dead slot aligns every member to the reference server's 10s edge.
	leaderConn.write.Reset()
	followerConn.write.Reset()
	if err := service.handleDungeonDieCharacter(leader, make([]byte, currentDungeonDieCharacterBodySize)); err != nil {
		t.Fatal(err)
	}
	owner := leader.dungeon.runtime.partyDungeonOwner
	if owner == nil || owner.allMembersDead() || owner.deadSlots != 1 {
		t.Fatalf("single party death owner=%p dead_slots=%08b all_dead=%t", owner, owner.deadSlots, owner.allMembersDead())
	}
	for _, test := range []struct {
		name       string
		connection *bufferConn
	}{
		{name: "leader", connection: leaderConn},
		{name: "follower", connection: followerConn},
	} {
		packets := splitAllCurrentGameServerUpperPackets(t, test.connection.write.Bytes())
		if len(packets) != 1 || packets[0].Header.MsgID != currentDungeonDeathStateMsgID ||
			!bytes.Equal(packets[0].Body, buildCurrentDungeonDeathStateBody(99, 0, 0)) {
			t.Fatalf("%s shared death projection=%x", test.name, test.connection.write.Bytes())
		}
	}
	revive := make([]byte, currentDungeonUseCoinBodySize)
	binary.LittleEndian.PutUint16(revive, 99)
	leaderConn.write.Reset()
	followerConn.write.Reset()
	if err := service.handleDungeonUseCoin(leader, revive); err != nil {
		t.Fatal(err)
	}
	if owner.deadSlots != 0 || owner.allMembersDead() {
		t.Fatalf("party revive dead_slots=%08b all_dead=%t", owner.deadSlots, owner.allMembersDead())
	}
	followerPackets := splitAllCurrentGameServerUpperPackets(t, followerConn.write.Bytes())
	if len(followerPackets) != 1 || followerPackets[0].Header.MsgID != currentDungeonDeathStateMsgID ||
		!bytes.Equal(followerPackets[0].Body, buildCurrentDungeonDeathStateBody(99, 1, 0)) {
		t.Fatalf("follower revive projection=%x", followerConn.write.Bytes())
	}

	leaderConn.write.Reset()
	followerConn.write.Reset()
	if err := service.handleDungeonDieCharacter(leader, make([]byte, currentDungeonDieCharacterBodySize)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonDieCharacter(follower, make([]byte, currentDungeonDieCharacterBodySize)); err != nil {
		t.Fatal(err)
	}
	if !owner.allMembersDead() || owner.deadSlots != 3 {
		t.Fatalf("party wipe dead_slots=%08b all_dead=%t", owner.deadSlots, owner.allMembersDead())
	}
	queue := service.gameplayTimers.(*fakeCurrentDungeonDeathTimerQueue)
	queue.mu.Lock()
	activeDelays := make([]time.Duration, 0, len(queue.active))
	for _, task := range queue.active {
		activeDelays = append(activeDelays, task.delay)
	}
	queue.mu.Unlock()
	if len(activeDelays) != 2 {
		t.Fatalf("party wipe active timers=%v", activeDelays)
	}
	for _, delay := range activeDelays {
		if delay != currentPartyDungeonAllDeadReturnDelay {
			t.Fatalf("party wipe timer delay=%s want=%s", delay, currentPartyDungeonAllDeadReturnDelay)
		}
	}
	for _, test := range []struct {
		session *gameSession
		target  uint16
	}{
		{session: leader, target: 99},
		{session: follower, target: 100},
	} {
		body := make([]byte, currentDungeonUseCoinBodySize)
		binary.LittleEndian.PutUint16(body, test.target)
		if err := service.handleDungeonUseCoin(test.session, body); err != nil {
			t.Fatal(err)
		}
	}
	if owner.deadSlots != 0 {
		t.Fatalf("party wipe revive dead_slots=%08b", owner.deadSlots)
	}

	leaderConn.write.Reset()
	followerConn.write.Reset()
	roomMonsters := leader.dungeon.runtime.Room.Snapshot().Monsters
	if len(roomMonsters) != 0 {
		if err := service.handleDungeonMonsterDeath(
			leader,
			ordinaryTutorialDeathBody(roomMonsters[0].ObjectKey, leader.selectedCharacterID),
		); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name       string
			session    *gameSession
			connection *bufferConn
		}{
			{name: "leader", session: leader, connection: leaderConn},
			{name: "follower", session: follower, connection: followerConn},
		} {
			if got := test.session.dungeon.runtime.Room.Snapshot().Monsters[0].State; got != runtimeDungeonMonsterDefeated {
				t.Fatalf("%s shared monster state=%s", test.name, got)
			}
			packets := splitAllCurrentGameServerUpperPackets(t, test.connection.write.Bytes())
			foundDeath := false
			for _, packet := range packets {
				if packet.Header.MsgID == uint16(dnfenum.CmdPacketNotifyDieMonster) {
					foundDeath = true
					break
				}
			}
			if !foundDeath {
				t.Fatalf("%s received no shared op38 death projection: %x", test.name, test.connection.write.Bytes())
			}
		}
		if err := service.handleDungeonBossDieCheck(
			leader,
			bossDieCheckRequestBody(leader.selectedCharacterID, uint16(roomMonsters[0].ObjectKey)),
		); err != nil {
			t.Fatal(err)
		}
		for _, runtime := range []*runtimeDungeonState{leader.dungeon.runtime, follower.dungeon.runtime} {
			runtime.settlementPlayResultReceived = true
			runtime.settlementStatisticReceived = true
			markCurrentDungeonCardRewardCommittedForTest(runtime)
		}
		leaderConn.write.Reset()
		followerConn.write.Reset()
		if err := service.handleDungeonEplpCommand(leader, []byte{1, 1}); err != nil {
			t.Fatal(err)
		}
		if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
			t.Fatalf("party select-other retained runtimes leader=%p follower=%p", leader.dungeon.runtime, follower.dungeon.runtime)
		}
		leaderPackets := splitAllCurrentGameServerUpperPackets(t, leaderConn.write.Bytes())
		followerPackets := splitAllCurrentGameServerUpperPackets(t, followerConn.write.Bytes())
		if len(leaderPackets) == 0 || leaderPackets[0].Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) {
			t.Fatalf("leader party settlement selector stream=%x", leaderConn.write.Bytes())
		}
		if len(followerPackets) == 0 || followerPackets[0].Header.MsgID != uint16(dnfenum.CmdPacketEplpCommand) {
			t.Fatalf("follower party settlement selector stream=%x", followerConn.write.Bytes())
		}
		followerSwitchedToSelector := false
		for _, packet := range followerPackets {
			if packet.Header.MsgID == currentFatigueMsgID {
				followerSwitchedToSelector = true
			}
		}
		if !followerSwitchedToSelector {
			// 选择其他地下城 takes the whole party to the selector page. Retiring the
			// member's run without that follower sequence left it holding the
			// settlement page inside the instance -- live 2026-09-19 in an ordinary
			// party and again in an Anton gate, where its own op15 answered
			// raid_member_not_in_private_waiting_instance, 返回城镇 answered "Anton
			// selector origin is not bound", and only a client restart got it out.
			t.Fatalf("follower was never moved to the selector page: %x", followerConn.write.Bytes())
		}
		for _, packet := range followerPackets {
			if packet.Header.MsgID == uint16(dnfenum.CmdPacketEnterSelectDungeon) {
				t.Fatalf("follower received fabricated op15 ACK: %x", followerConn.write.Bytes())
			}
		}
	}
}

// assertPartyDungeonRoomSlotTable parses a dungeon-room op9 kind-0 record the
// way buildCurrentSceneOp9DungeonRoomSlotTableBodyInContext writes it - fixed
// prefix, the receiver's name DSTR, the remaining header, then the rows - and
// pins the fields the client's host arbitration keys off: the record is keyed
// by the receiver's own actor in owner context 0, every member row is the
// extended form at its real slot with the present marker set, and the tail
// carries the shared leader slot.
func assertPartyDungeonRoomSlotTable(
	t *testing.T,
	name string,
	body []byte,
	receiverKey uint16,
	wantRows map[byte]uint16,
	leaderSlot byte,
) {
	t.Helper()
	reader := body
	need := func(n int) []byte {
		if len(reader) < n {
			t.Fatalf("%s room slot table truncated at offset %d: %x", name, len(body)-len(reader), body)
		}
		chunk := reader[:n]
		reader = reader[n:]
		return chunk
	}
	u8 := func() byte { return need(1)[0] }
	u16 := func() uint16 { return binary.LittleEndian.Uint16(need(2)) }
	u32 := func() uint32 { return binary.LittleEndian.Uint32(need(4)) }
	dstr := func() {
		length := int(int32(u32()))
		if length < 0 || length > 64 {
			t.Fatalf("%s room slot table DSTR length=%d body=%x", name, length, body)
		}
		need(length)
	}
	if count := u16(); count != 1 {
		t.Fatalf("%s room slot table record count=%d", name, count)
	}
	if scene := u16(); scene != currentSceneOp9StableSceneValue {
		t.Fatalf("%s room slot table scene=%d", name, scene)
	}
	if key := u16(); key != receiverKey {
		t.Fatalf("%s room slot table record key=%d want receiver actor %d", name, key, receiverKey)
	}
	u8()
	if owner := u8(); owner != currentSceneObjectContext {
		t.Fatalf("%s room slot table owner context=%d want dungeon context %d", name, owner, currentSceneObjectContext)
	}
	u8()
	if kind := u8(); kind != currentSceneOp9ActorDisplayKind {
		t.Fatalf("%s room slot table record kind=%d", name, kind)
	}
	u8() // route
	if context := u8(); context != currentSceneObjectContext {
		t.Fatalf("%s room slot table inline identity discriminator=%d", name, context)
	}
	dstr() // receiver name
	u8()   // job
	u8()   // member cap
	u32()
	u8() // grow
	u16()
	u8()
	u8() // party type
	u16()
	rowCount := int(u8())
	if rowCount != len(wantRows) {
		t.Fatalf("%s room slot table rows=%d want=%d body=%x", name, rowCount, len(wantRows), body)
	}
	seen := map[byte]uint16{}
	for index := 0; index < rowCount; index++ {
		slot := u8()
		uid := u16()
		u8() // job
		if level := u8(); level == 0 {
			t.Fatalf("%s room slot table row slot=%d carries level 0", name, slot)
		}
		u8() // grow
		u8() // flag
		dstr()
		u8()
		if state := u8(); state != 100 {
			t.Fatalf("%s room slot table row slot=%d state=%d want 100", name, slot, state)
		}
		if present := u8(); present != currentSceneOp9PartyMemberSlotPresent {
			t.Fatalf("%s room slot table row slot=%d present marker=%d", name, slot, present)
		}
		if _, duplicate := seen[slot]; duplicate {
			t.Fatalf("%s room slot table repeats slot %d", name, slot)
		}
		seen[slot] = uid
	}
	for slot, uid := range wantRows {
		if seen[slot] != uid {
			t.Fatalf("%s room slot table slot %d uid=%d want=%d rows=%v", name, slot, seen[slot], uid, seen)
		}
	}
	u8()
	if got := u8(); got != leaderSlot {
		t.Fatalf("%s room slot table host slot=%d want=%d", name, got, leaderSlot)
	}
	u8()
	u8()
	if len(reader) != 0 {
		t.Fatalf("%s room slot table has %d trailing bytes: %x", name, len(reader), reader)
	}
}

func equalUint16Values(left, right []uint16) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsUint16Sequence(values, sequence []uint16) bool {
	if len(sequence) == 0 {
		return true
	}
	for start := 0; start+len(sequence) <= len(values); start++ {
		if equalUint16Values(values[start:start+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestPartyLeaderDungeonRoomMoveSynchronizesEveryMemberAndLeaderSeed(t *testing.T) {
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
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
	}
	seedCalls := uint32(0)
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed: func() (uint32, error) {
			seedCalls++
			return 0x1000 + seedCalls, nil
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	enableTownMoveSkillProjection(t, service, repositories, "99")
	enableTownMoveSkillProjection(t, service, repositories, "100")
	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	leaderConn, followerConn := &bufferConn{}, &bufferConn{}
	leader := &gameSession{conn: leaderConn, channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: followerConn, channel: channel, residentChannel: channel, selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	seedPartyOwnerActorProjectionForTest(t, service, leader, 99, 42)
	seedPartyOwnerActorProjectionForTest(t, service, follower, 100, 42)
	registerPartyPeerEndpointForTest(leader, 3, 5063)
	registerPartyPeerEndpointForTest(follower, 3, 60359)
	state := alignedcmd.PartyState{
		PartyID: 99, UserID: 99,
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

	selectBody := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(selectBody[:4], 700)
	if err := service.handleDungeonSelectUpper(leader, selectBody); err != nil {
		t.Fatal(err)
	}
	if seedCalls != 1 {
		t.Fatalf("party entry independently rolled follower seed calls=%d", seedCalls)
	}
	leaderConn.write.Reset()
	followerConn.write.Reset()
	moveBody := make([]byte, dungeoncmd.MoveMapRequestSize)
	moveBody[0] = 1
	if err := service.handleDungeonMoveMap(leader, moveBody); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		session    *gameSession
		connection *bufferConn
	}{
		{name: "leader", session: leader, connection: leaderConn},
		{name: "follower", session: follower, connection: followerConn},
	} {
		snapshot := test.session.dungeon.runtime.Session.Snapshot()
		if snapshot.Scene.Coordinate != (worldmap.RoomCoordinate{X: 1, Y: 0}) ||
			snapshot.Scene.Map.Map.ID != 101 || test.session.dungeon.runtime.Seed != 0x1002 {
			t.Fatalf("%s move snapshot=%+v seed=%x", test.name, snapshot, test.session.dungeon.runtime.Seed)
		}
		packet, trailing := splitGameServerUpperPacket(t, test.connection.write.Bytes())
		trailing = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, trailing)
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentDungeonStartNotification || len(trailing) != 0 {
			t.Fatalf("%s move packet=%+v trailing=%x", test.name, packet.Header, trailing)
		}
	}
	if seedCalls != 3 {
		t.Fatalf("room move seed chooser calls=%d want leader+discarded follower=3", seedCalls)
	}
}

func TestPartyDungeonEntryRejectsEveryMemberBeforeCommitWhenFollowerLevelIsTooLow(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(source["dungeon/test.dgn"], "[limit party count]\n1\n", "[limit party count]\n4\n", 1)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	for id, level := range map[string]int{"99": 20, "100": 5} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   "account-1",
			Level:       level,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
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
	leaderConn, followerConn := &bufferConn{}, &bufferConn{}
	leader := &gameSession{conn: leaderConn, channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: followerConn, channel: channel, residentChannel: channel, selectedCharacterID: 100}
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
	if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
		t.Fatalf("party entry partially committed leader_runtime=%p follower_runtime=%p", leader.dungeon.runtime, follower.dungeon.runtime)
	}
	packet, trailing := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) ||
		!bytes.Equal(packet.Body, buildSelectDungeonFailureBody(currentSelectDungeonPreparationRejectedReason)) ||
		len(trailing) != 0 {
		t.Fatalf("blocked party entry rejection packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	if followerConn.write.Len() != 0 {
		t.Fatalf("blocked party entry wrote unsolicited follower packets=%x", followerConn.write.Bytes())
	}
	if service.onlinePlayers.SessionForCharacter(99) != leader || service.onlinePlayers.SessionForCharacter(100) != follower {
		t.Fatal("blocked party entry removed a town presence")
	}
}

func TestPartyDungeonEntryRejectsBeforeCommitWhenNativePeerEndpointIsIncomplete(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(source["dungeon/test.dgn"], "[limit party count]\n1\n", "[limit party count]\n4\n", 1)
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	accounts := map[string]string{"99": "account-leader", "100": "account-follower"}
	for _, id := range []string{"99", "100"} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   accounts[id],
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 38, "area_id": 1,
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:             options{accountID: "fallback-account", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:       newOnlinePlayerManager(),
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 42, Type: 11, Name: "ch.42", Port: 10042}
	leaderConn, followerConn := &bufferConn{}, &bufferConn{}
	leader := &gameSession{conn: leaderConn, accountID: accounts["99"], channel: channel, residentChannel: channel, selectedCharacterID: 99}
	follower := &gameSession{conn: followerConn, accountID: accounts["100"], channel: channel, residentChannel: channel, selectedCharacterID: 100}
	service.bindGameSessionCharacter(leader, 99)
	service.bindGameSessionCharacter(follower, 100)
	seedPartyOwnerActorProjectionForTest(t, service, leader, 99, 42)
	seedPartyOwnerActorProjectionForTest(t, service, follower, 100, 42)
	registerPartyPeerEndpointForTest(leader, 3, 5063)
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
	if leader.dungeon.runtime != nil || follower.dungeon.runtime != nil {
		t.Fatalf("endpoint rejection partially committed leader_runtime=%p follower_runtime=%p", leader.dungeon.runtime, follower.dungeon.runtime)
	}
	packet, trailing := splitGameServerUpperPacket(t, leaderConn.write.Bytes())
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) ||
		!bytes.Equal(packet.Body, buildSelectDungeonFailureBody(currentSelectDungeonPreparationRejectedReason)) ||
		len(trailing) != 0 {
		t.Fatalf("endpoint rejection packet=%+v body=%x trailing=%x", packet.Header, packet.Body, trailing)
	}
	for stream := followerConn.write.Bytes(); len(stream) != 0; {
		followerPacket, rest := splitGameServerUpperPacket(t, stream)
		if followerPacket.Header.MsgID == uint16(dnfenum.CmdPacketSelectDungeon) ||
			followerPacket.Header.MsgID == currentDungeonInfoNotification ||
			followerPacket.Header.MsgID == currentDungeonStartNotification {
			t.Fatalf("endpoint rejection advanced follower into dungeon packet=%+v body=%x", followerPacket.Header, followerPacket.Body)
		}
		stream = rest
	}
}

func assertDungeonPartyFollowerPrerequisitePrecedesRuntime(t *testing.T, stream []byte) {
	t.Helper()
	fatigueIndex, contextIndex, infoIndex, startIndex := -1, -1, -1, -1
	for index := 0; len(stream) > 0; index++ {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == 0 {
			switch packet.Header.MsgID {
			case currentFatigueMsgID:
				if fatigueIndex < 0 {
					fatigueIndex = index
				}
			case currentDungeonContextMsgID:
				if contextIndex < 0 {
					contextIndex = index
				}
			case currentDungeonInfoNotification:
				if infoIndex < 0 {
					infoIndex = index
				}
			case currentDungeonStartNotification:
				if startIndex < 0 {
					startIndex = index
				}
			}
		}
		stream = rest
	}
	if fatigueIndex < 0 || contextIndex < 0 || infoIndex < 0 || startIndex < 0 ||
		!(fatigueIndex < contextIndex && contextIndex < infoIndex && infoIndex < startIndex) {
		t.Fatalf("party follower sequence fatigue=%d context=%d dungeon_info=%d start=%d", fatigueIndex, contextIndex, infoIndex, startIndex)
	}
}

func assertDungeonPartyFollowerHasNoUnsolicitedSelectAck(t *testing.T, stream []byte) {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == dnfproto.DefaultChannelClassification &&
			packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectDungeon) {
			t.Fatalf("party follower received unsolicited class1 op16 ack body=%x", packet.Body)
		}
		stream = rest
	}
}

func containsAdmissionDeleteUpdateForSlot(t *testing.T, stream []byte, slot uint16) bool {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		stream = rest
		if packet.Header.Classification != 0 ||
			packet.Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
			len(packet.Body) < 3+currentItemListEntryWireSize ||
			packet.Body[0] != dnfrepo.MainInventoryListType {
			continue
		}
		rowCount := int(binary.LittleEndian.Uint16(packet.Body[1:3]))
		for index := 0; index < rowCount; index++ {
			offset := 3 + index*currentItemListEntryWireSize
			if offset+currentItemListEntryWireSize > len(packet.Body) {
				break
			}
			row := packet.Body[offset : offset+currentItemListEntryWireSize]
			if binary.LittleEndian.Uint16(row[0:2]) == slot &&
				binary.LittleEndian.Uint32(row[2:6]) == uint32(^uint32(0)) &&
				binary.LittleEndian.Uint32(row[6:10]) == 0 {
				return true
			}
		}
	}
	return false
}

func assertDungeonPartyRealtimeSnapshot(t *testing.T, stream []byte, wantMembers byte) {
	t.Helper()
	foundRealtime := false
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == 0x0099 && len(packet.Body) > 0 && packet.Body[0] == wantMembers {
			if len(packet.Body) != 1+int(wantMembers)*5 {
				t.Fatalf("dungeon party realtime body_len=%d want=%d body=%x", len(packet.Body), 1+int(wantMembers)*5, packet.Body)
			}
			for slot := byte(0); slot < wantMembers; slot++ {
				offset := 1 + int(slot)*5
				if packet.Body[offset+2] == 0 || packet.Body[offset+4] != slot {
					t.Fatalf("dungeon party realtime slot=%d row=%x", slot, packet.Body[offset:offset+5])
				}
			}
			foundRealtime = true
		}
		stream = rest
	}
	if !foundRealtime {
		t.Fatalf("dungeon party snapshot realtime=%t want_members=%d", foundRealtime, wantMembers)
	}
}

func assertDungeonPartyFrameCount(t *testing.T, stream []byte, characterID uint16, wantCount int) {
	t.Helper()
	wantObjectKey := currentSceneActorObjectKey(characterID)
	count := 0
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina) &&
			len(packet.Body) >= 10 && packet.Body[9] == currentSceneOp9ActorDisplayKind &&
			binary.LittleEndian.Uint16(packet.Body[4:6]) == wantObjectKey {
			count++
		}
		stream = rest
	}
	if count != wantCount {
		t.Fatalf("dungeon local party frame count=%d want=%d object_key=%d", count, wantCount, wantObjectKey)
	}
}

func assertDungeonStartMapPartyIndex(t *testing.T, stream []byte, want byte) {
	t.Helper()
	for len(stream) > 0 {
		packet, rest := splitGameServerUpperPacket(t, stream)
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentDungeonStartNotification {
			if len(packet.Body) == 0 || packet.Body[len(packet.Body)-1] != want {
				t.Fatalf("op29 party index body_tail=%x want=%d", packet.Body, want)
			}
			return
		}
		stream = rest
	}
	t.Fatal("op29 start-map packet not found")
}

func TestBuildCurrentDungeonUserStateBodyUsesDynamicObjectKey(t *testing.T) {
	body, err := buildCurrentDungeonUserStateBody(19)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte{1, 0x13, 0x00, currentDungeonPlayerUserState}) {
		t.Fatalf("user-state body=%x", body)
	}
	if _, err := buildCurrentDungeonUserStateBody(0); err == nil {
		t.Fatal("zero object key accepted")
	}
}

func TestBuildCurrentDungeonEntryPacketsUsesRuntimeAndPVFOwners(t *testing.T) {
	coordinate := worldmap.RoomCoordinate{X: 2, Y: 3}
	spawn := worldmap.MonsterSpawn{MonsterID: 3001, AutoLevel: 20, Rank: "[normal]"}
	room := &runtimeDungeonRoom{
		coordinate: coordinate,
		mapID:      70000,
		monsters: []runtimeDungeonMonster{{
			ObjectKey: 402,
			Reference: worldmap.HostileReference{Kind: worldmap.HostileMonster, Index: 0},
			Spawn:     spawn,
			State:     runtimeDungeonMonsterPlanned,
		}},
		extendedActors: []runtimeDungeonExtendedActor{{
			Kind:      runtimeDungeonActorAICharacter,
			ObjectKey: 403,
			Packet: currentDungeonStartMapActor{
				ObjectKey: 403, Code: 4001, Level: 21, Type: 5, Blocking: 0,
			},
			State: runtimeDungeonMonsterPlanned,
		}},
		aiCharacterCount: 1,
	}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 900, Difficulty: 2},
		Dungeon:        worldmap.Dungeon{ID: 900, Mazes: []worldmap.Maze{{}}},
		MazeIndex:      0,
		Session:        &worldmap.DungeonSession{},
		Room:           room,
		BossCoordinate: worldmap.RoomCoordinate{X: 8, Y: 9},
		BossSet:        true,
		Seed:           0x12345678,
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate:   coordinate,
		Map:          worldmap.ResolvedMap{Map: worldmap.Map{ID: 70000}},
		Monsters:     []worldmap.MonsterSpawn{spawn},
		AICharacters: []worldmap.AICharacter{{Code: 4001, Faction: "[character]", AIType: "[normal]"}},
	}
	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets.DungeonInfo) != 36 {
		t.Fatalf("dungeon-info len=%d body=%x", len(packets.DungeonInfo), packets.DungeonInfo)
	}
	if got := binary.LittleEndian.Uint32(packets.DungeonInfo[0:4]); got != 900 {
		t.Fatalf("dungeon id=%d", got)
	}
	if packets.DungeonInfo[4] != 2 || packets.DungeonInfo[7] != 0 || packets.DungeonInfo[8] != 8 || packets.DungeonInfo[9] != 9 {
		t.Fatalf("dungeon-info runtime fields=%x", packets.DungeonInfo[:10])
	}
	if packets.DungeonInfo[10] != 0xff || packets.DungeonInfo[11] != 0xff || binary.LittleEndian.Uint32(packets.DungeonInfo[20:24]) != ^uint32(0) {
		t.Fatalf("dungeon-info ordinary sentinels=%x", packets.DungeonInfo)
	}
	if len(packets.StartMap) != 65 {
		t.Fatalf("start-map len=%d body=%x", len(packets.StartMap), packets.StartMap)
	}
	if packets.StartMap[0] != 2 || packets.StartMap[1] != 3 || binary.LittleEndian.Uint32(packets.StartMap[3:7]) != 0x12345678 {
		t.Fatalf("start-map room/seed=%x", packets.StartMap[:7])
	}
	if got := binary.LittleEndian.Uint32(packets.StartMap[14:18]); got != 70000 || packets.StartMap[18] != 2 {
		t.Fatalf("start-map map/count map=%d count=%d", got, packets.StartMap[18])
	}
	if binary.LittleEndian.Uint16(packets.StartMap[25:27]) != 402 || binary.LittleEndian.Uint32(packets.StartMap[27:31]) != 3001 || packets.StartMap[39] != 0 {
		t.Fatalf("normal actor row=%x", packets.StartMap[19:40])
	}
	if binary.LittleEndian.Uint16(packets.StartMap[46:48]) != 403 || binary.LittleEndian.Uint32(packets.StartMap[48:52]) != 4001 || packets.StartMap[53] != 5 || packets.StartMap[60] != 0 {
		t.Fatalf("APC actor row=%x", packets.StartMap[40:61])
	}
}

func TestBuildCurrentDungeonEntryPacketsKeepsSingleAntonPVFIdentityInsideDungeon(t *testing.T) {
	coordinate := worldmap.RoomCoordinate{X: 0, Y: 0}
	const pvfMapID = int64(292101788)
	runtime := &runtimeDungeonState{
		Request:         dungeoncmd.SelectDungeonRequest{DungeonID: 5314},
		Dungeon:         worldmap.Dungeon{ID: 5314, Mazes: []worldmap.Maze{{}}},
		clientDungeonID: 210,
		MazeIndex:       0,
		Session:         &worldmap.DungeonSession{},
		Room:            &runtimeDungeonRoom{coordinate: coordinate, mapID: pvfMapID},
		BossCoordinate:  coordinate,
		BossSet:         true,
		Seed:            0x12345678,
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate: coordinate,
		Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: pvfMapID}},
	}

	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint32(packets.DungeonInfo[:4]); got != 5314 {
		t.Fatalf("single Anton op28 dungeon id=%d want dedicated PVF dungeon 5314", got)
	}
	if got := binary.LittleEndian.Uint32(packets.StartMap[14:18]); got != uint32(pvfMapID) {
		t.Fatalf("single Anton op29 map id=%d want PVF room %d", got, pvfMapID)
	}
	if runtime.Request.DungeonID != 5314 || runtime.Dungeon.ID != 5314 {
		t.Fatalf("single Anton server identity request=%d dungeon=%d", runtime.Request.DungeonID, runtime.Dungeon.ID)
	}
}

func TestBuildCurrentDungeonEntryPacketsRejectsUnownedState(t *testing.T) {
	base := func() (*runtimeDungeonState, worldmap.DungeonRoomScene) {
		coordinate := worldmap.RoomCoordinate{X: 0, Y: 0}
		room := &runtimeDungeonRoom{coordinate: coordinate, mapID: 100}
		return &runtimeDungeonState{
				Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 700},
				Dungeon:        worldmap.Dungeon{ID: 700, Mazes: []worldmap.Maze{{}}},
				Session:        &worldmap.DungeonSession{},
				Room:           room,
				BossCoordinate: coordinate,
				BossSet:        true,
			}, worldmap.DungeonRoomScene{
				Coordinate: coordinate,
				Map:        worldmap.ResolvedMap{Map: worldmap.Map{ID: 100}},
			}
	}
	tests := []struct {
		name   string
		mutate func(*runtimeDungeonState, *worldmap.DungeonRoomScene)
		want   error
	}{
		{name: "request mode", want: errDungeonEntryModeUnsupported, mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene) { runtime.Request.SpecialMode = 1 }},
		{name: "boss missing", want: errDungeonBossCoordinateRequired, mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene) { runtime.BossSet = false }},
		{name: "boss range", want: errDungeonBossCoordinateRange, mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene) { runtime.BossCoordinate.X = 256 }},
		{name: "randomized object", want: errDungeonRandomizedObjectOwner, mutate: func(runtime *runtimeDungeonState, _ *worldmap.DungeonRoomScene) {
			runtime.Dungeon.Mazes[0].RandomizedObjects = []worldmap.RandomizedObjectScript{{}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, scene := base()
			test.mutate(runtime, &scene)
			_, err := buildCurrentDungeonEntryPackets(runtime, scene)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

// The raw pair mapping alone stays conservative: state=1 + token=UINT16_MAX is
// the 练习模式 switch for an ordinary room but the guided rooms' own button shape
// (修炼场 5000, 转职体验 7142), so only the room-aware
// currentDungeonProtocolModeForRoom may promote it to practice.
func TestCurrentDungeonProtocolModeForWireMapsEveryProvenPair(t *testing.T) {
	tests := []struct {
		name         string
		runtimeState byte
		runtimeToken uint16
		wantMode     currentDungeonProtocolMode
		wantOK       bool
	}{
		{name: "normal zero token", runtimeState: 0, runtimeToken: 0, wantMode: currentDungeonProtocolModeNormal, wantOK: true},
		{name: "normal current client token", runtimeState: 0, runtimeToken: 0xffff, wantMode: currentDungeonProtocolModeNormal, wantOK: true},
		{name: "practice or guided shape without the room", runtimeState: 1, runtimeToken: 0xffff, wantMode: currentDungeonProtocolModeNormal, wantOK: true},
		{name: "explicit practice", runtimeState: 1, runtimeToken: 0, wantMode: currentDungeonProtocolModePractice, wantOK: true},
		{name: "unproven token", runtimeState: 1, runtimeToken: 1, wantMode: currentDungeonProtocolModeNormal, wantOK: false},
		{name: "unproven state", runtimeState: 2, runtimeToken: 0xffff, wantMode: currentDungeonProtocolModeNormal, wantOK: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotMode, gotOK := currentDungeonProtocolModeForWire(test.runtimeState, test.runtimeToken)
			if gotMode != test.wantMode || gotOK != test.wantOK {
				t.Fatalf("currentDungeonProtocolModeForWire(%d,%d)=(%s,%t), want=(%s,%t)", test.runtimeState, test.runtimeToken, gotMode, gotOK, test.wantMode, test.wantOK)
			}
		})
	}
}

func TestValidateCurrentDungeonEntryRequestAcceptsDefaultAndExplicitPracticeModes(t *testing.T) {
	request := dungeoncmd.SelectDungeonRequest{DungeonID: 4999, RuntimeState: 1, RuntimeToken: 0xffff}
	if err := validateCurrentDungeonEntryRequest(request); err != nil {
		t.Fatalf("ordinary current-client mode rejected: %v", err)
	}
	request.RuntimeToken = 0
	if err := validateCurrentDungeonEntryRequest(request); err != nil {
		t.Fatalf("explicit practice mode rejected: %v", err)
	}
	request.RuntimeToken = 1
	if err := validateCurrentDungeonEntryRequest(request); !errors.Is(err, errDungeonEntryModeUnsupported) {
		t.Fatalf("ordinary dungeon accepted unproven runtime token error=%v", err)
	}
}

func TestBuildCurrentDungeonEntryPacketsKeepsSpecialSpawnMetadataOutOfActorAndDropTables(t *testing.T) {
	coordinate := worldmap.RoomCoordinate{X: 0, Y: 0}
	specialObject := worldmap.SpecialPassiveObject{
		PassiveObject: worldmap.PassiveObject{ObjectID: 109006908},
		Spawns: []worldmap.SpecialObjectSpawn{
			{Kind: "[item]", Code: 1001},
			{Kind: "[trap]", Code: 1002},
		},
	}
	room := &runtimeDungeonRoom{
		coordinate: coordinate,
		mapID:      76191,
		extendedActors: []runtimeDungeonExtendedActor{{
			Kind:      runtimeDungeonActorSpecialObject,
			ObjectKey: 402,
			Packet: currentDungeonStartMapActor{
				PacketIndex: 0,
				ObjectKey:   402,
				Code:        uint32(specialObject.ObjectID),
				Type:        9,
			},
			State: runtimeDungeonMonsterPlanned,
		}},
		retainedSpecialSpawns: []runtimeDungeonRetainedSpecialSpawn{
			{ObjectIndex: 0, SpawnIndex: 0, Spawn: specialObject.Spawns[0]},
			{ObjectIndex: 0, SpawnIndex: 1, Spawn: specialObject.Spawns[1]},
		},
		specialObjectCount: 1,
	}
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 1000},
		Dungeon:        worldmap.Dungeon{ID: 1000, Mazes: []worldmap.Maze{{}}},
		MazeIndex:      0,
		Session:        &worldmap.DungeonSession{},
		Room:           room,
		BossCoordinate: coordinate,
		BossSet:        true,
		Seed:           1,
	}
	scene := worldmap.DungeonRoomScene{
		Coordinate:            coordinate,
		Map:                   worldmap.ResolvedMap{Map: worldmap.Map{ID: 76191}},
		SpecialPassiveObjects: []worldmap.SpecialPassiveObject{specialObject},
	}

	packets, err := buildCurrentDungeonEntryPackets(runtime, scene)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets.DungeonInfo) != 36 {
		t.Fatalf("dungeon-info len=%d body=%x", len(packets.DungeonInfo), packets.DungeonInfo)
	}
	if len(packets.StartMap) != 44 {
		t.Fatalf("start-map len=%d body=%x", len(packets.StartMap), packets.StartMap)
	}
	if packets.StartMap[18] != 1 {
		t.Fatalf("actor count=%d body=%x", packets.StartMap[18], packets.StartMap)
	}
	if got := binary.LittleEndian.Uint32(packets.StartMap[27:31]); got != uint32(specialObject.ObjectID) || packets.StartMap[32] != 9 {
		t.Fatalf("special object code=%d type=%d body=%x", got, packets.StartMap[32], packets.StartMap)
	}
	if packets.StartMap[40] != 0 {
		t.Fatalf("metadata child rows were invented as materialized drops: count=%d body=%x", packets.StartMap[40], packets.StartMap)
	}
}

func TestRuntimeDungeonRoomAnnounceAllActorsCommitsAfterPreflight(t *testing.T) {
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, bridgeDungeonPVF(false), nil)
	count, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("announced count=%d", count)
	}
	snapshot := runtime.Room.Snapshot()
	if snapshot.Monsters[0].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("monster state=%s", snapshot.Monsters[0].State)
	}
	scene, _ := runtime.Session.Scene()
	if got := scene.RuntimeObjects[402]; got != (worldmap.HostileReference{Kind: worldmap.HostileMonster, Index: 0}) {
		t.Fatalf("runtime binding=%+v", got)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); !errors.Is(err, errDungeonActorAlreadyAnnounced) {
		t.Fatalf("duplicate announce error=%v", err)
	}
}

func TestRuntimeDungeonRoomAnnounceAllActorsIncludesHostileAPC(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] += "[ai character]\n4001 10 20 0 `[monster]` `[boss]` 0 0\n"
	source[defaultDungeonAICharacterList] = "4001 `Test/Enemy.aic`\n"
	source["AICharacter/Test/Enemy.aic"] = "[minimum info]\n`Enemy APC` 1 2 3 4 25\n"
	aiCatalog, err := newPVFDungeonAICharacterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, source, aiCatalog)
	if runtime.NextObjectKey != 404 {
		t.Fatalf("next object key=%d", runtime.NextObjectKey)
	}
	count, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("announced count=%d", count)
	}
	snapshot := runtime.Room.Snapshot()
	if len(snapshot.ExtendedActors) != 1 || snapshot.ExtendedActors[0].State != runtimeDungeonMonsterAnnounced || snapshot.ExtendedActors[0].ObjectKey != 403 {
		t.Fatalf("extended actors=%+v", snapshot.ExtendedActors)
	}
	scene, _ := runtime.Session.Scene()
	if got := scene.RuntimeObjects[403]; got != (worldmap.HostileReference{Kind: worldmap.HostileAICharacter, Index: 0}) {
		t.Fatalf("APC runtime binding=%+v", got)
	}
}

func TestHandleDungeonMonsterDeathSupportsHostileAPCAndClearsLastHostile(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] += "[ai character]\n4001 10 20 0 `[monster]` `[boss]` 0 0\n4002 30 20 0 `[character]` `[normal]` 0 0\n"
	source[defaultDungeonAICharacterList] = "4001 `Test/Enemy.aic`\n4002 `Test/Friend.aic`\n"
	source["AICharacter/Test/Enemy.aic"] = "[minimum info]\n`Enemy APC` 1 2 3 4 25\n"
	source["AICharacter/Test/Friend.aic"] = "[minimum info]\n`Friend APC` 1 2 3 4 25\n"
	aiCatalog, err := newPVFDungeonAICharacterCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, source, aiCatalog)
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); err != nil {
		t.Fatal(err)
	}

	conn := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: conn, connID: "dungeon-death-ai-test", dungeon: dungeonSessionState{runtime: runtime}}

	aiObjectKey := uint32(403)
	aiDeathBody, err := hex.DecodeString("9301000094010000000000000000910d0000130000000211019401bf0b00000d001101130094010000060064024d01000000001e3c0001000000430d00000000000064024d0164024d0112002f0003000000300280d6")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleDungeonMonsterDeath(session, aiDeathBody); err != nil {
		t.Fatal(err)
	}
	aiPacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	wantAI, err := buildCurrentDungeonDeathNotificationBody(
		aiObjectKey,
		currentDungeonDeathResponseAICharacter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if aiPacket.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		aiPacket.Header.Classification != 0 ||
		!bytes.Equal(aiPacket.Body, wantAI) {
		t.Fatalf("AI death notification = header=%+v body=%x rest=%x", aiPacket.Header, aiPacket.Body, rest)
	}
	forcedPacket, rest := splitGameServerUpperPacket(t, rest)
	wantForced, err := buildCurrentDungeonDeathNotificationBody(
		402,
		currentDungeonDeathResponseMonster,
	)
	if err != nil {
		t.Fatal(err)
	}
	if forcedPacket.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		forcedPacket.Header.Classification != 0 ||
		!bytes.Equal(forcedPacket.Body, wantForced) ||
		len(rest) != 0 {
		t.Fatalf("AI boss forced-clear packet = header=%+v body=%x rest=%x", forcedPacket.Header, forcedPacket.Body, rest)
	}
	snapshot := runtime.Room.Snapshot()
	scene, _ := runtime.Session.Scene()
	if snapshot.ExtendedActors[0].State != runtimeDungeonMonsterDefeated ||
		snapshot.Monsters[0].State != runtimeDungeonMonsterDefeated ||
		!scene.Cleared {
		t.Fatalf("APC death state = room=%+v scene=%+v", snapshot, scene)
	}

	conn.write.Reset()
	fixedSentinelBody := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(fixedSentinelBody[0:4], 404)
	binary.LittleEndian.PutUint16(fixedSentinelBody[4:6], ^uint16(0))
	if err := service.handleDungeonMonsterDeath(session, fixedSentinelBody); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || runtime.Room.Snapshot().ExtendedActors[1].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("fixed62 owner=ffff was not rejected: writes=%d room=%+v", conn.write.Len(), runtime.Room.Snapshot())
	}

	variableCombatSentinelBody := make([]byte, dungeoncmd.DieMonsterVariableBaseSize+dungeoncmd.DieMonsterVariableCombatEntrySize)
	binary.LittleEndian.PutUint32(variableCombatSentinelBody[0:4], 404)
	binary.LittleEndian.PutUint16(variableCombatSentinelBody[4:6], ^uint16(0))
	variableCombatSentinelBody[22] = 1
	if err := service.handleDungeonMonsterDeath(session, variableCombatSentinelBody); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 || runtime.Room.Snapshot().ExtendedActors[1].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("variable count=1 owner=ffff was not rejected: writes=%d room=%+v", conn.write.Len(), runtime.Room.Snapshot())
	}

	conn.write.Reset()
	nonHostileDeathBody := make([]byte, dungeoncmd.DieMonsterVariableBaseSize)
	binary.LittleEndian.PutUint32(nonHostileDeathBody[0:4], 404)
	binary.LittleEndian.PutUint16(nonHostileDeathBody[4:6], ^uint16(0))
	if err := service.handleDungeonMonsterDeath(session, nonHostileDeathBody); err != nil {
		t.Fatal(err)
	}
	nonHostilePacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	wantNonHostile, err := buildCurrentDungeonDeathNotificationBody(
		404,
		currentDungeonDeathResponseAICharacter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if nonHostilePacket.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		nonHostilePacket.Header.Classification != 0 ||
		!bytes.Equal(nonHostilePacket.Body, wantNonHostile) ||
		len(rest) != 0 {
		t.Fatalf("non-hostile AI retirement notification = header=%+v body=%x rest=%x", nonHostilePacket.Header, nonHostilePacket.Body, rest)
	}
	snapshot = runtime.Room.Snapshot()
	scene, _ = runtime.Session.Scene()
	if snapshot.ExtendedActors[1].State != runtimeDungeonMonsterDefeated || !scene.Cleared {
		t.Fatalf("non-hostile AI retirement changed completed clear state = room=%+v scene=%+v", snapshot, scene)
	}

	conn.write.Reset()
	if err := service.handleDungeonMonsterDeath(session, nonHostileDeathBody); err != nil {
		t.Fatal(err)
	}
	duplicateNonHostilePacket, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if duplicateNonHostilePacket.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		duplicateNonHostilePacket.Header.Classification != 0 ||
		!bytes.Equal(duplicateNonHostilePacket.Body, wantNonHostile) ||
		len(rest) != 0 {
		t.Fatalf("duplicate non-hostile AI retirement notification = header=%+v body=%x rest=%x", duplicateNonHostilePacket.Header, duplicateNonHostilePacket.Body, rest)
	}
	snapshot = runtime.Room.Snapshot()
	scene, _ = runtime.Session.Scene()
	if snapshot.ExtendedActors[1].State != runtimeDungeonMonsterDefeated || !scene.Cleared {
		t.Fatalf("duplicate non-hostile AI retirement changed completed clear state = room=%+v scene=%+v", snapshot, scene)
	}

	conn.write.Reset()
	monsterObjectKey := uint32(firstDungeonMonsterObjectKey)
	monsterDeathBody := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(monsterDeathBody[0:4], monsterObjectKey)
	binary.LittleEndian.PutUint16(monsterDeathBody[4:6], currentSceneBootstrapObjectKey)
	if err := service.handleDungeonMonsterDeath(session, monsterDeathBody); err != nil {
		t.Fatal(err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("forced-cleared monster death replay wrote=%x", conn.write.Bytes())
	}
	snapshot = runtime.Room.Snapshot()
	scene, _ = runtime.Session.Scene()
	if snapshot.Monsters[0].State != runtimeDungeonMonsterDefeated || !scene.Cleared {
		t.Fatalf("last hostile death state = room=%+v scene=%+v", snapshot, scene)
	}
}

func TestHandleDungeonMonsterDeathRetiresSpecialMonsterVisualActor(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["map/dungeon/test/start.map"] += "[special passive object]\n7001 30 20 0 1 `[monster]` 3001 20 0 0 0\n"
	// 特殊被动对象要经 passiveobject.lst 反查安装路径才认得出是怪物视觉体
	// （dungeon_monster_catalog.go 的 ensurePassiveObjectIndexLocked）。共享桩里没有
	// 这张表，缺了它解析直接报错，而不是走到本测试要验的"退场"分支。只列 7001，
	// 不动 bridgeDungeonPVF，避免影响其他用同一个桩的测试。
	source[currentDungeonPassiveObjectList] = "7001 `test.obj`\n"
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, source, nil)
	count, err := runtime.Room.AnnounceAllActors(runtime.Session)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("announced count=%d", count)
	}
	snapshot := runtime.Room.Snapshot()
	if len(snapshot.ExtendedActors) != 2 ||
		snapshot.ExtendedActors[0].Kind != runtimeDungeonActorSpecialObject ||
		snapshot.ExtendedActors[0].ObjectKey != 403 ||
		snapshot.ExtendedActors[1].Kind != runtimeDungeonActorSpecialMonster ||
		snapshot.ExtendedActors[1].ObjectKey != 404 ||
		snapshot.ExtendedActors[1].HostileReference != nil ||
		snapshot.ExtendedActors[1].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("special actors=%+v", snapshot.ExtendedActors)
	}
	scene, _ := runtime.Session.Scene()
	if got := scene.RuntimeObjects[402]; got != (worldmap.HostileReference{Kind: worldmap.HostileMonster, Index: 0}) {
		t.Fatalf("ordinary monster binding=%+v", got)
	}
	if _, bound := scene.RuntimeObjects[404]; bound {
		t.Fatalf("special monster was bound as a room owner: %+v", scene.RuntimeObjects)
	}

	conn := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: conn, connID: "dungeon-special-monster-test", dungeon: dungeonSessionState{runtime: runtime}}

	specialDeathBody := make([]byte, dungeoncmd.DieMonsterVariableBaseSize+dungeoncmd.DieMonsterVariableCombatEntrySize)
	binary.LittleEndian.PutUint32(specialDeathBody[0:4], 404)
	binary.LittleEndian.PutUint16(specialDeathBody[4:6], currentSceneBootstrapObjectKey)
	specialDeathBody[22] = 1
	if err := service.handleDungeonMonsterDeath(session, specialDeathBody); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	want, err := buildCurrentDungeonDeathNotificationBody(
		404,
		currentDungeonDeathResponseMonster,
	)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		packet.Header.Classification != 0 ||
		!bytes.Equal(packet.Body, want) ||
		len(rest) != 0 {
		t.Fatalf("special monster retirement packet = header=%+v body=%x rest=%x", packet.Header, packet.Body, rest)
	}
	snapshot = runtime.Room.Snapshot()
	scene, _ = runtime.Session.Scene()
	if snapshot.ExtendedActors[1].State != runtimeDungeonMonsterDefeated ||
		snapshot.Monsters[0].State != runtimeDungeonMonsterAnnounced ||
		scene.Cleared {
		t.Fatalf("special monster retirement changed owner state = room=%+v scene=%+v", snapshot, scene)
	}

	conn.write.Reset()
	if err := service.handleDungeonMonsterDeath(session, specialDeathBody); err != nil {
		t.Fatal(err)
	}
	duplicate, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if duplicate.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		duplicate.Header.Classification != 0 ||
		!bytes.Equal(duplicate.Body, want) ||
		len(rest) != 0 {
		t.Fatalf("duplicate special monster retirement packet = header=%+v body=%x rest=%x", duplicate.Header, duplicate.Body, rest)
	}
	snapshot = runtime.Room.Snapshot()
	scene, _ = runtime.Session.Scene()
	if snapshot.ExtendedActors[1].State != runtimeDungeonMonsterDefeated ||
		snapshot.Monsters[0].State != runtimeDungeonMonsterAnnounced ||
		scene.Cleared {
		t.Fatalf("duplicate special monster retirement changed owner state = room=%+v scene=%+v", snapshot, scene)
	}
}

func TestRuntimeDungeonRoomAnnounceAllActorsRejectsConflictWithoutStateAdvance(t *testing.T) {
	runtime := prepareSyntheticDungeonRuntimeForEntryTest(t, bridgeDungeonPVF(false), nil)
	reference := worldmap.HostileReference{Kind: worldmap.HostileMonster, Index: 0}
	if err := runtime.Session.BindHostileObject(reference, 999); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Room.AnnounceAllActors(runtime.Session); !errors.Is(err, errDungeonActorOwnerConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	if got := runtime.Room.Snapshot().Monsters[0].State; got != runtimeDungeonMonsterPlanned {
		t.Fatalf("monster advanced to %s", got)
	}
}

func TestHandleDungeonSelectUpperDefersEntryPacketValidationUntilTypedScenePhases(t *testing.T) {
	table, resolver, monsters := loadBridgeDungeonStaticData(t, bridgeDungeonPVF(false))
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "99", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	// An equipped weapon must survive the selector-page rebind: playguide_5.co
	// raises the no-weapon alarm on its `dungeon entrance` event whenever the
	// mode1 equipment model is empty.
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"11": {
				SlotIndex: 11,
				ItemID:    900001,
				RawEntry:  buildInitialEquipmentRawEntry(11, 900001, 27),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 1, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	channel := channelcatalog.Channel{ServerID: 1, ID: 253, Type: 40, Name: "ch.253", Port: 10253}
	session := &gameSession{
		conn:                            connection,
		channel:                         channel,
		residentChannel:                 channel,
		selectedCharacterID:             99,
		connectionTownActorOwnerChannel: byte(channel.ID),
		townActorOwnerChannel:           byte(channel.ID),
	}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body[0:4], 700)
	binary.LittleEndian.PutUint16(body[9:11], 0xffff)
	binary.LittleEndian.PutUint32(body[16:20], 3145)
	if err := service.handleDungeonSelectUpper(session, body); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketSelectDungeon) {
		t.Fatalf("select ACK packet=%+v rest=%x", packet.Header, rest)
	}
	resourcePacket, rest := splitGameServerUpperPacket(t, rest)
	if resourcePacket.Header.Classification != 0 || resourcePacket.Header.MsgID != currentDungeonResourceStateMsgID || !bytes.Equal(resourcePacket.Body, []byte{1, 0, 0xbc, 2, 0, 0, currentDungeonResourceSelectedState}) {
		t.Fatalf("op5 packet=%+v body=%x rest=%x", resourcePacket.Header, resourcePacket.Body, rest)
	}
	rest = assertCurrentPreDungeonContextPlayerStateWithEquipment(t, session, rest, 1)
	contextPacket, rest := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.Classification != 0 || contextPacket.Header.MsgID != currentDungeonContextMsgID || len(contextPacket.Body) != 37 {
		t.Fatalf("op27 packet=%+v body_len=%d rest=%x", contextPacket.Header, len(contextPacket.Body), rest)
	}
	infoPacket, rest := splitGameServerUpperPacket(t, rest)
	if infoPacket.Header.Classification != 0 || infoPacket.Header.MsgID != currentDungeonInfoNotification || len(infoPacket.Body) != 36 {
		t.Fatalf("op28 packet=%+v body_len=%d rest=%x", infoPacket.Header, len(infoPacket.Body), rest)
	}
	startPacket, rest := splitGameServerUpperPacket(t, rest)
	if startPacket.Header.Classification != 0 || startPacket.Header.MsgID != currentDungeonStartNotification || len(startPacket.Body) < 23 {
		t.Fatalf("op29 packet=%+v body_len=%d rest=%x", startPacket.Header, len(startPacket.Body), rest)
	}
	rest = assertCurrentPostStartMapPlayerState(t, session, rest, true, true)
	if len(rest) != 0 {
		t.Fatalf("post-op29 trailing bytes=%x", rest)
	}
	if !session.preDungeonContextPlayerStateSent || !session.postStartMapPlayerStateSent || !session.sceneBootstrapTailSent || session.sceneBootstrapTailDeferred {
		t.Fatalf("scene flags pre=%v placed=%v tail=%v deferred=%v", session.preDungeonContextPlayerStateSent, session.postStartMapPlayerStateSent, session.sceneBootstrapTailSent, session.sceneBootstrapTailDeferred)
	}
	if session.dungeon.runtime == nil || session.dungeon.runtime.Request.RuntimeToken != 0xffff || session.dungeon.runtime.Request.LeaderObjectKey != 3145 {
		t.Fatalf("runtime request=%+v", session.dungeon.runtime)
	}
}

func TestHandleDungeonSelectUpperCommitsStartMapBeforePostPlayerState(t *testing.T) {
	table, resolver, monsters := loadBridgeDungeonStaticData(t, bridgeDungeonPVF(false))
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "99", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 1, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &failNthDungeonWriteConn{failAt: 23, err: errors.New("unexpected twenty-third write")}
	session := &gameSession{conn: connection, selectedCharacterID: 99}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body[0:4], 700)
	if err := service.handleDungeonSelectUpper(session, body); err == nil || !strings.Contains(err.Error(), "unexpected twenty-third write") {
		t.Fatalf("handler error=%v, want post-op29 placement failure", err)
	}
	if connection.writes != 23 {
		t.Fatalf("write calls=%d", connection.writes)
	}
	if session.dungeon.runtime == nil {
		t.Fatal("runtime reservation missing after select-dungeon ACK")
	}
	snapshot := session.dungeon.runtime.Room.Snapshot()
	if snapshot.Monsters[0].State != runtimeDungeonMonsterAnnounced {
		t.Fatalf("monster state after start-map notification: %s", snapshot.Monsters[0].State)
	}
	scene, _ := session.dungeon.runtime.Session.Scene()
	if len(scene.RuntimeObjects) != 1 {
		t.Fatalf("runtime objects after start-map notification: %+v", scene.RuntimeObjects)
	}
	if !session.preDungeonContextPlayerStateSent || session.postStartMapPlayerStateSent || !session.selectedUserInfoRefreshSent || session.selectedUserInfoMode3Sent {
		t.Fatal("post-start-map placement state committed incorrectly after its final write failed")
	}
}

func TestHandleDungeonSelectUpperStopsBeforeOp27WhenLifecycleMode1WriteFails(t *testing.T) {
	table, resolver, monsters := loadBridgeDungeonStaticData(t, bridgeDungeonPVF(false))
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats: map[string]int64{
			"fatigue": 100, "town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: "99", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:             options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:       table,
		worldMapResolver:    resolver,
		dungeonMonsterTable: monsters,
		dungeonChoice:       func(int) (int, error) { return 0, nil },
		dungeonSeed:         func() (uint32, error) { return 1, nil },
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &failNthDungeonWriteConn{failAt: 4, err: errors.New("actor-binding mode1 write failed")}
	session := &gameSession{conn: connection, selectedCharacterID: 99}
	bindDungeonSelectorOriginForTestAt(t, service, session, 38, 1, 450, 234)
	body := make([]byte, 21)
	binary.LittleEndian.PutUint32(body[0:4], 700)
	if err := service.handleDungeonSelectUpper(session, body); err == nil || !strings.Contains(err.Error(), "actor-binding mode1 write failed") {
		t.Fatalf("handler error=%v", err)
	}
	_, rest := splitGameServerUpperPacket(t, connection.write.Bytes()) // op16 ACK
	_, rest = splitGameServerUpperPacket(t, rest)                      // op5
	mode0, rest := splitGameServerUpperPacket(t, rest)
	if mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(mode0.Body) == 0 || mode0.Body[0] != 0 || len(rest) != 0 {
		t.Fatalf("writes before actor-binding mode1 failure mode0=%+v body=%x rest=%x", mode0.Header, mode0.Body, rest)
	}
	if session.preDungeonContextPlayerStateSent || session.postStartMapPlayerStateSent {
		t.Fatalf("failed actor-binding stage committed flags pre=%v post=%v", session.preDungeonContextPlayerStateSent, session.postStartMapPlayerStateSent)
	}
}

func TestSendCurrentPreDungeonContextPlayerStateIsOneShot(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "77",
		AccountID:   "account-1",
		Name:        "hero",
		Job:         "11",
		Level:       1,
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	channel := channelcatalog.Channel{ServerID: 1, ID: 16, Type: 1, Name: "ch.16", Port: 10016}
	session := &gameSession{
		conn:                connection,
		channel:             channel,
		residentChannel:     channel,
		selectedCharacterID: 77,
	}
	if err := service.sendCurrentPreDungeonContextPlayerState(session, "test"); err != nil {
		t.Fatal(err)
	}
	rest := assertCurrentPreDungeonContextPlayerState(t, session, connection.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("pre-op27 actor-binding trailing bytes=%x", rest)
	}
	written := connection.write.Len()
	if err := service.sendCurrentPreDungeonContextPlayerState(session, "test_duplicate"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != written {
		t.Fatalf("duplicate pre-op27 actor-binding wrote %d extra bytes", connection.write.Len()-written)
	}
}

func assertCurrentPreDungeonContextPlayerState(t *testing.T, session *gameSession, rest []byte) []byte {
	t.Helper()
	return assertCurrentPreDungeonContextPlayerStateWithEquipment(t, session, rest, 0)
}

// assertCurrentPreDungeonContextPlayerStateWithEquipment pins the selector-page
// rebind. The mode1 must carry the character's equipment create rows: the play
// guide playguide_5.co checks "equipment slot weapon empty" on its `dungeon
// entrance` event, so a binding-only mode1 here raised the no-weapon alarm for
// every armed character entering the dungeon selector.
func assertCurrentPreDungeonContextPlayerStateWithEquipment(t *testing.T, session *gameSession, rest []byte, wantCreateRows byte) []byte {
	t.Helper()
	localObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	ownerChannel := byte(currentSceneObjectContext)
	mode0, rest := splitGameServerUpperPacket(t, rest)
	if mode0.Header.Classification != 0 || mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(mode0.Body) < 0x4e || mode0.Body[0] != 0 ||
		mode0.Body[3] != currentSceneObjectRoute ||
		mode0.Body[4] != ownerChannel ||
		binary.LittleEndian.Uint16(mode0.Body[0x4c:0x4e]) != localObjectKey {
		t.Fatalf("pre-op27 mode0 packet=%+v body=%x rest=%x", mode0.Header, mode0.Body, rest)
	}
	mode1, rest := splitGameServerUpperPacket(t, rest)
	if mode1.Header.Classification != 0 || mode1.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		mode1.Body[0] != 1 || binary.LittleEndian.Uint16(mode1.Body[0x15:0x17]) != localObjectKey ||
		mode1.Body[3] != currentSceneObjectRoute || mode1.Body[4] != ownerChannel ||
		mode1.Body[currentMode1CreateCountOffset] != wantCreateRows {
		t.Fatalf("pre-op27 actor-binding mode1 packet=%+v body=%x rest=%x", mode1.Header, mode1.Body, rest)
	}
	if wantCreateRows == 0 && len(mode1.Body) != currentMode1BaseWireSize {
		t.Fatalf("pre-op27 mode1 with no equipment must stay at the base size: len=%d body=%x", len(mode1.Body), mode1.Body)
	}
	if wantCreateRows > 0 && len(mode1.Body) <= currentMode1BaseWireSize {
		t.Fatalf("pre-op27 mode1 with equipment must carry the rows: len=%d body=%x", len(mode1.Body), mode1.Body)
	}
	if !session.preDungeonContextPlayerStateSent {
		t.Fatal("pre-op27 actor-binding stage was not committed")
	}
	return rest
}

func assertCurrentPostStartMapPlayerState(t *testing.T, session *gameSession, rest []byte, expectUserState bool, expectPreLifecycle bool) []byte {
	t.Helper()
	rest = consumeOptionalCurrentDungeonFatigueRefreshForTest(t, rest)
	ownerChannel := byte(currentSceneObjectContext)
	for page := 0; page < currentSceneOverseerPageCount; page++ {
		packet, next := splitGameServerUpperPacket(t, rest)
		if packet.Header.Classification != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketRequestOverseer) {
			t.Fatalf("post-op29 page[%d] = class %d msg %d body=%x", page, packet.Header.Classification, packet.Header.MsgID, packet.Body)
		}
		rest = next
	}
	actionTable, rest := splitGameServerUpperPacket(t, rest)
	if actionTable.Header.Classification != 0 || actionTable.Header.MsgID != uint16(dnfenum.CmdPacketPVPMissionHpPercent) {
		t.Fatalf("post-op29 action table = class %d msg %d body=%x", actionTable.Header.Classification, actionTable.Header.MsgID, actionTable.Body)
	}
	if !expectPreLifecycle {
		mode0, next := splitGameServerUpperPacket(t, rest)
		if mode0.Header.Classification != 0 ||
			mode0.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
			len(mode0.Body) < 0x4e || mode0.Body[0] != 0 ||
			mode0.Body[3] != currentSceneObjectRoute ||
			mode0.Body[4] != ownerChannel {
			t.Fatalf("post-op29 fallback mode0 packet=%+v body=%x", mode0.Header, mode0.Body)
		}
		rest = next
	}
	for _, listType := range currentSelectInventoryBootstrapListTypes {
		itemPacket, next := splitCurrentSceneItemListPacket(t, rest)
		if itemPacket.Header.Classification != 0 || itemPacket.Header.MsgID != uint16(dnfenum.CmdPacketLeaveParty) || len(itemPacket.Body) == 0 || itemPacket.Body[0] != listType {
			t.Fatalf("actor-bound pre-mode1 op13 type=%d packet=%+v body=%x", listType, itemPacket.Header, itemPacket.Body)
		}
		if listType == 2 && (len(itemPacket.Body) != 6 || itemPacket.Body[5] != 0) {
			t.Fatalf("pre-mode0 op13 type2 body=%x want zero group terminator", itemPacket.Body)
		}
		rest = next
	}
	lockSnapshot, next := splitCurrentGameServerUpperPacketAuto(t, rest)
	if lockSnapshot.Header.Classification != 0 ||
		lockSnapshot.Header.MsgID != dnfitemlock.LockListMessageID ||
		!bytes.Equal(lockSnapshot.Body, []byte{0, 0}) {
		t.Fatalf("actor-bound pre-mode1 item-lock snapshot=%+v body=%x", lockSnapshot.Header, lockSnapshot.Body)
	}
	rest = next
	want := make([]struct {
		msgID uint16
		mode  byte
	}, 0, 8)
	want = append(want,
		struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketSetUDPIPPort), 1},
		struct {
			msgID uint16
			mode  byte
		}{currentCreatureStateTableMsgID, 0xff},
		struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketInsertOverseer), 0xff},
	)
	// The pre-op27 mode0 descriptor plus post-op29 mode1 local actor binding
	// establish the wrapper. Mode3 is deliberately absent: a fresh live trace
	// proved it opens the personal-information panel and still emits no op43.
	want = append(want,
		struct {
			msgID uint16
			mode  byte
		}{currentClearQuestListMsgID, 0xff},
		struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketReportClientSpec), 0xff},
		struct {
			msgID uint16
			mode  byte
		}{skillSwitchInventoryAllDataWire, 0xff},
		struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketRecoverStamina), 0xff},
		struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketRequestBlacklist), 0xff},
	)
	// Ordinary dungeon activation is deliberately last: the working client
	// produces op43 pickup only when op120 commits the room placement before
	// op3 marks the local wrapper active. Tutorials defer op3 to finish-loading.
	if expectUserState {
		want = append(want, struct {
			msgID uint16
			mode  byte
		}{uint16(dnfenum.CmdPacketNotifyUserState), 0xff})
	}
	for idx, expected := range want {
		var packet dnfproto.ChannelPacket
		var next []byte
		if expected.msgID == currentClearQuestListMsgID {
			packet, next = splitLongHengGameServerUpperPacket(t, rest)
		} else {
			packet, next = splitGameServerUpperPacket(t, rest)
		}
		if packet.Header.Classification != 0 || packet.Header.MsgID != expected.msgID {
			t.Fatalf("post-op29 packet[%d] = class %d msg %d, want msg %d body=%x", idx, packet.Header.Classification, packet.Header.MsgID, expected.msgID, packet.Body)
		}
		if expected.mode != 0xff && (len(packet.Body) == 0 || packet.Body[0] != expected.mode) {
			t.Fatalf("post-op29 msg2 mode%d body=%x", expected.mode, packet.Body)
		}
		localObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
		if expected.mode == 0 && (len(packet.Body) < 0x4e ||
			packet.Body[3] != currentSceneObjectRoute ||
			packet.Body[4] != ownerChannel ||
			binary.LittleEndian.Uint16(packet.Body[0x4c:0x4e]) != localObjectKey) {
			t.Fatalf("post-op29 msg2 mode0 scene key body=%x", packet.Body)
		}
		if expected.msgID == uint16(dnfenum.CmdPacketNotifyUserState) {
			wantBody := []byte{1, byte(localObjectKey), byte(localObjectKey >> 8), currentDungeonPlayerUserState}
			if !bytes.Equal(packet.Body, wantBody) {
				t.Fatalf("post-op29 op3 user-state body=%x want=%x", packet.Body, wantBody)
			}
		}
		if expected.mode == 1 && (len(packet.Body) <= currentMode1CreateCountOffset ||
			packet.Body[3] != currentSceneObjectRoute ||
			packet.Body[4] != ownerChannel ||
			binary.LittleEndian.Uint16(packet.Body[0x15:0x17]) != localObjectKey) {
			t.Fatalf("post-op29 msg2 mode1 local owner/equipment body=%x", packet.Body)
		}
		if expected.mode == 3 && (len(packet.Body) < 15 ||
			packet.Body[3] != currentSceneObjectRoute ||
			packet.Body[4] != ownerChannel ||
			binary.LittleEndian.Uint16(packet.Body[13:15]) != localObjectKey) {
			t.Fatalf("post-op29 msg2 mode3 local owner/runtime finalizer body=%x", packet.Body)
		}
		if expected.msgID == currentClearQuestListMsgID {
			plain, err := zlibDecompress(packet.Body)
			if err != nil || len(plain) != 30004 || binary.LittleEndian.Uint32(plain[:4]) != 30000 {
				t.Fatalf("post-op29 op356 body len=%d plain_len=%d err=%v", len(packet.Body), len(plain), err)
			}
			if plain[4+int(localObjectKey)] != 0 {
				t.Fatalf("post-op29 op356 wrote actor key %#x into clear-quest list", localObjectKey)
			}
		}
		if expected.msgID == uint16(dnfenum.CmdPacketRecoverStamina) && (len(packet.Body) < 12 ||
			binary.LittleEndian.Uint16(packet.Body[0:2]) != 1 ||
			binary.LittleEndian.Uint16(packet.Body[4:6]) != localObjectKey ||
			packet.Body[9] != currentSceneOp9ActorDisplayKind ||
			packet.Body[10] != currentSceneObjectRoute ||
			packet.Body[11] != ownerChannel) {
			t.Fatalf("post-op29 op9 actor display body=%x", packet.Body)
		}
		if expected.msgID == uint16(dnfenum.CmdPacketRequestBlacklist) && !bytes.Equal(packet.Body, []byte{0, 0}) {
			t.Fatalf("post-op29 placement body=%x", packet.Body)
		}
		rest = next
	}
	if session.preDungeonContextPlayerStateSent != expectPreLifecycle || !session.selectedUserInfoRefreshSent || session.selectedUserInfoMode3Sent || !session.selectedItemListRefreshSent {
		t.Fatalf("post-op29 flags pre=%v placed=%v mode1=%v mode3=%v item_lists=%v", session.preDungeonContextPlayerStateSent, session.postStartMapPlayerStateSent, session.selectedUserInfoRefreshSent, session.selectedUserInfoMode3Sent, session.selectedItemListRefreshSent)
	}
	return rest
}

func TestCurrentPostStartMapSuppressesTutorialUserState(t *testing.T) {
	for _, fixture := range []struct {
		name             string
		options          tutorialScopeFixtureOptions
		expectUserState  bool
		expectDeferred   bool
		mutateSceneOwner bool
	}{
		{name: "female Slayer tutorial", expectDeferred: true},
		{
			name: "Knight tutorial",
			options: tutorialScopeFixtureOptions{
				job:              "12",
				dungeonReference: tutorialScopeKnightFDungeonReference,
				mapDirectory:     tutorialScopeKnightFMapDirectory,
			},
			expectDeferred: true,
		},
		{
			name: "PVF tutorial stale scene owner",
			options: tutorialScopeFixtureOptions{
				job:              "12",
				dungeonReference: tutorialScopeKnightFDungeonReference,
				mapDirectory:     tutorialScopeKnightFMapDirectory,
			},
			mutateSceneOwner: true,
		},
		{name: "ordinary non-tutorial dungeon", options: tutorialScopeFixtureOptions{disableTutorial: true}, expectUserState: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			service, runtime := prepareTutorialScopeRuntime(t, fixture.options)
			service.characterStats = testCharacterStatTable(t)
			connection := &bufferConn{}
			channel := channelcatalog.Channel{ServerID: 1, ID: 16, Type: 1, Name: "ch.16", Port: 10016}
			session := &gameSession{
				conn:                connection,
				channel:             channel,
				residentChannel:     channel,
				selectedCharacterID: 99,
			}
			scene := runtime.Session.Snapshot().Scene
			if fixture.mutateSceneOwner {
				scene.Map.Map.ID++
			}
			if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, scene, "test_owned_dungeon_user_state"); err != nil {
				t.Fatal(err)
			}
			rest := assertCurrentPostStartMapPlayerState(t, session, connection.write.Bytes(), fixture.expectUserState, false)
			if len(rest) != 0 || !session.postStartMapPlayerStateSent {
				t.Fatalf("post-op29 state incomplete: rest=%x placed=%v", rest, session.postStartMapPlayerStateSent)
			}
			wantDeferredObjectKey := uint16(0)
			if fixture.expectDeferred {
				wantDeferredObjectKey = currentSceneActorObjectKey(session.selectedCharacterID)
			}
			if session.deferredDungeonUserStateObjectKey != wantDeferredObjectKey {
				t.Fatalf(
					"deferred dungeon user-state object key=%d want=%d",
					session.deferredDungeonUserStateObjectKey,
					wantDeferredObjectKey,
				)
			}
			written := connection.write.Len()
			if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, scene, "test_ordinary_room_change_no_replay"); err != nil {
				t.Fatal(err)
			}
			if connection.write.Len() != written {
				t.Fatalf("second room placement replayed %d bytes", connection.write.Len()-written)
			}
		})
	}
}

func TestCurrentPostStartMapRehydratesGuardianGemAfterFullMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		t.Fatal("dungeon entry fixture equipment repository unavailable")
	}
	medalRaw := currentGuardianGemTestRaw(32, 100380017, 1)
	binary.LittleEndian.PutUint16(medalRaw[currentGuardianGemRawSocketOffset:currentGuardianGemRawSocketOffset+2], 3)
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"32": {SlotIndex: 32, ItemID: 100380017, RawEntry: medalRaw},
		},
	}); err != nil {
		t.Fatalf("save dungeon guardian-gem medal: %v", err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	scene := runtime.Session.Snapshot().Scene
	if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, scene, "test_guardian_gem_after_mode1"); err != nil {
		t.Fatal(err)
	}

	sawMode1 := false
	sawGuardianGem := false
	stream := connection.write.Bytes()
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			sawMode1 = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == currentSocketListEquipment &&
			binary.LittleEndian.Uint16(packet.Body[3:5]) == uint16(currentGuildMedalActorSlot) {
			if !sawMode1 {
				t.Fatal("guardian-gem op14 preceded dungeon full mode1")
			}
			if !currentGuardianGemRawSocketOccupied(packet.Body[3:]) {
				t.Fatalf("dungeon guardian-gem op14 omitted raw socket state: %x", packet.Body)
			}
			sawGuardianGem = true
		}
		stream = rest
	}
	if !sawMode1 || !sawGuardianGem {
		t.Fatalf("dungeon mode1/guardian refresh mode1=%t guardian=%t", sawMode1, sawGuardianGem)
	}
}

func TestCurrentPostStartMapProjectsCollectboxAfterFullMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Account == nil {
		t.Fatal("dungeon entry fixture account repository unavailable")
	}
	state := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"5": {"0": 9005}}}
	raw, err := dnfcollectbox.EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{dnfcollectbox.MetadataKey: raw},
	}); err != nil {
		t.Fatal(err)
	}
	service.collectboxCatalog = &currentCollectboxCatalog{
		byIndex: map[byte]dnfcollectbox.Definition{
			1: {Index: 1, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9001, Count: 1}}},
			5: {Index: 5, BaseExpirationDate: "2037-01-05 06:00:00", Slots: []dnfcollectbox.Slot{{ItemID: 9005, Count: 1}}},
		},
		indexes: []byte{1, 5},
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		accountID:           "account-1",
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, runtime.Session.Snapshot().Scene, "test_collectbox_after_mode1"); err != nil {
		t.Fatal(err)
	}

	mode1Index, cacheIndex, selectionIndex, selectedIndex := -1, -1, -1, -1
	stream := connection.write.Bytes()
	packetIndex := 0
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		switch {
		case packet.Header.Classification == 0 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1:
			mode1Index = packetIndex
		case packet.Header.Classification == 0 && packet.Header.MsgID == collectboxNotificationMsgID && len(packet.Body) >= 8 && packet.Body[1] == 0:
			cacheIndex = packetIndex
		case packet.Header.Classification == 1 && packet.Header.MsgID == uint16(dnfenum.CmdPacketSelectCollectbox):
			selectionIndex = packetIndex
		case packet.Header.Classification == 0 && packet.Header.MsgID == collectboxNotificationMsgID && len(packet.Body) >= 12 && packet.Body[0] == defaultCollectboxIndex && packet.Body[1] == 1:
			if binary.LittleEndian.Uint32(packet.Body[8:12]) != 9005 {
				t.Fatalf("selected collection-box item body=%x", packet.Body)
			}
			selectedIndex = packetIndex
		}
		packetIndex++
		stream = rest
	}
	if !(mode1Index >= 0 && mode1Index < cacheIndex && cacheIndex < selectionIndex && selectionIndex < selectedIndex) {
		t.Fatalf("dungeon collection-box order mode1=%d cache=%d select=%d selected=%d", mode1Index, cacheIndex, selectionIndex, selectedIndex)
	}
	if !session.collectboxSceneProjectionSent {
		t.Fatal("dungeon actor generation did not commit collection-box projection")
	}
}

func TestCurrentPostStartMapRehydratesPetEnchantAndBuffRegistrationAfterFullMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("dungeon entry fixture repositories unavailable")
	}
	const (
		petItemID       uint32 = 400990167
		petSerial       uint32 = 37
		cardID          uint32 = 10008713
		chronicleItemID uint32 = 116010135
		chronicleSerial uint32 = 20
	)
	chronicleRaw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(chronicleRaw[0:2], 17)
	binary.LittleEndian.PutUint32(chronicleRaw[2:6], chronicleItemID)
	chronicleRaw[0x27] = 1
	binary.LittleEndian.PutUint32(chronicleRaw[0x28:0x2c], currentChronicleCanonicalAuraBase+2)
	chronicleRaw[0x30] = 15
	chronicleRaw[0x32] = 2
	chronicleRaw[0x34] = 12
	chronicleRaw[0x36] = 11
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:17": {
				ItemID:   int64(chronicleItemID),
				Count:    1,
				RawEntry: chronicleRaw,
				Extra:    map[string]string{"item_serial": "20"},
			},
		},
	}); err != nil {
		t.Fatalf("save registered Chronicle inventory: %v", err)
	}
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"26": {
				SlotIndex: 26,
				ItemID:    int64(petItemID),
				RawEntry:  testCurrentEquippedCreatureEnchantRaw(26, petItemID, petSerial, cardID, 2),
				Extra: map[string]string{
					"pet_enchant_card_item_id":  "10008713",
					"enchant_card_id":           "10008713",
					"pet_enchant_upgrade_count": "2",
				},
			},
		},
	}); err != nil {
		t.Fatalf("save dungeon pet equipment: %v", err)
	}
	if err := repositories.Pet.Save(context.Background(), dnfrepo.PetRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.PetEntry{
			"37": {
				PetKey:          "37",
				CreatureKey:     petSerial,
				ItemID:          int64(petItemID),
				Name:            "PetB",
				NameRaw:         []byte("PetB"),
				Satiety:         72,
				Level:           7,
				Exp:             0x12345678,
				SourceListType:  3,
				SourceSlotIndex: 26,
				Extra: map[string]string{
					"pet_enchant_card_item_id":  "10008713",
					"pet_enchant_upgrade_count": "2",
				},
			},
		},
		EquippedKey: "37",
	}); err != nil {
		t.Fatalf("save dungeon pet state: %v", err)
	}
	if err := saveSkillSwitchInventoryState(context.Background(), repositories.Settings, "99", skillSwitchInventoryState{
		SkillID: 40,
		Items:   []skillSwitchBinding{{EquipmentPart: 12, ItemSerial: chronicleSerial, ItemID: chronicleItemID}},
	}); err != nil {
		t.Fatalf("save dungeon BUFF registration: %v", err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, runtime.Session.Snapshot().Scene, "test_pet_and_buff_after_mode1"); err != nil {
		t.Fatal(err)
	}

	mode1Index, petIndex, stateIndex, growthIndex, buffIndex, insertIndex, layoutIndex, chronicleIndex := -1, -1, -1, -1, -1, -1, -1, -1
	stream := connection.write.Bytes()
	packetIndex := 0
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			mode1Index = packetIndex
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == currentSocketListEquipment &&
			binary.LittleEndian.Uint16(packet.Body[3:5]) == 26 {
			assertEquippedCreatureItemRefreshPacket(t, packet, petItemID, petSerial, cardID, 2)
			petIndex = packetIndex
		}
		if packet.Header.MsgID == currentCreatureStateTableMsgID {
			stateIndex = packetIndex
		}
		if packet.Header.MsgID == currentCreatureGrowthMsgID {
			growthIndex = packetIndex
		}
		if packet.Header.MsgID == skillSwitchInventoryAllDataWire {
			buffIndex = packetIndex
			if want := []byte{40, 0, 1, 1, 12, 0, 17, 0}; !bytes.Equal(packet.Body, want) {
				t.Fatalf("dungeon BUFF refresh body=%x want=%x", packet.Body, want)
			}
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketInsertOverseer) {
			insertIndex = packetIndex
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketReportClientSpec) {
			layoutIndex = packetIndex
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketWalkoutPartyMember) &&
			len(packet.Body) == 3+currentItemListEntryWireSize &&
			packet.Body[0] == 0 &&
			binary.LittleEndian.Uint16(packet.Body[3:5]) == 17 {
			if packet.Body[3+0x27] != 1 ||
				binary.LittleEndian.Uint32(packet.Body[3+0x28:3+0x2c]) != currentChronicleCanonicalAuraBase+2 {
				t.Fatalf("dungeon Chronicle BUFF item refresh=%x", packet.Body)
			}
			chronicleIndex = packetIndex
		}
		packetIndex++
		stream = rest
	}
	if mode1Index < 0 || petIndex < 0 || stateIndex < 0 || growthIndex < 0 || buffIndex < 0 || insertIndex < 0 || layoutIndex < 0 || chronicleIndex < 0 {
		t.Fatalf("dungeon refresh packets mode1=%d pet=%d state=%d growth=%d insert=%d layout=%d chronicle=%d buff=%d", mode1Index, petIndex, stateIndex, growthIndex, insertIndex, layoutIndex, chronicleIndex, buffIndex)
	}
	if !(mode1Index < petIndex && petIndex < stateIndex && stateIndex < growthIndex && growthIndex < insertIndex && insertIndex < layoutIndex && layoutIndex < chronicleIndex && chronicleIndex < buffIndex) {
		t.Fatalf("dungeon refresh order mode1=%d pet=%d state=%d growth=%d insert=%d layout=%d chronicle=%d buff=%d", mode1Index, petIndex, stateIndex, growthIndex, insertIndex, layoutIndex, chronicleIndex, buffIndex)
	}
}

func TestCurrentPostStartMapInitializesSkillsAfterDungeonMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Skill == nil {
		t.Fatal("dungeon entry fixture skill repository unavailable")
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":    "11 `job11.lst`\n",
		"skill/job11.lst":        "1 `job11/active.skl`\n",
		"skill/job11/active.skl": "[skill type]\n`active`\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.skillCatalog = catalog
	service.initialSkillsByJob = map[byte][]initialSkillEntry{
		11: {{SkillID: 1, Level: 1}},
	}
	service.initialSPTable = map[int]int{20: 20}
	service.initialTPTable = map[int]int{20: 0}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{
		CharacterID: "99",
		Points:      dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
		Skills:      map[int64]dnfrepo.SkillState{1: {Level: 1, Enabled: true}},
	}); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	if err := service.sendCurrentPostStartMapPlayerPlacement(session, runtime, runtime.Session.Snapshot().Scene, "test_dungeon_skill_initialization"); err != nil {
		t.Fatal(err)
	}

	mode1Index, buffIndex, skillIndex, insertIndex, layoutIndex := -1, -1, -1, -1, -1
	stream := connection.write.Bytes()
	packetIndex := 0
	for len(stream) > 0 {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			mode1Index = packetIndex
		}
		if packet.Header.MsgID == skillSwitchInventoryAllDataWire {
			buffIndex = packetIndex
		}
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID {
			skillIndex = packetIndex
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketInsertOverseer) {
			insertIndex = packetIndex
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketReportClientSpec) {
			layoutIndex = packetIndex
		}
		packetIndex++
		stream = rest
	}
	if mode1Index < 0 || buffIndex < 0 || skillIndex < 0 || insertIndex < 0 || layoutIndex < 0 {
		t.Fatalf("dungeon skill packets mode1=%d skill=%d insert=%d layout=%d buff=%d", mode1Index, skillIndex, insertIndex, layoutIndex, buffIndex)
	}
	if !(mode1Index < skillIndex && skillIndex < insertIndex && insertIndex < layoutIndex && layoutIndex < buffIndex) {
		t.Fatalf("dungeon skill packet order mode1=%d skill=%d insert=%d layout=%d buff=%d", mode1Index, skillIndex, insertIndex, layoutIndex, buffIndex)
	}
	if !session.dungeonSceneSkillInfoSent {
		t.Fatal("dungeonSceneSkillInfoSent = false after op19")
	}
}

func TestCurrentPostStartMapPreinstallsSwordDemonSnakeSwordBeforeDungeonMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Skill == nil || repositories.Character == nil {
		t.Fatal("Sword Demon dungeon entry repositories unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load Sword Demon character: found=%t err=%v", found, err)
	}
	character.Stats["grow_type"] = int64(currentSwordDemonFirstGrowType)
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":             "11 `job11.lst`\n",
		"skill/job11.lst":                 "31 `atswordman/calldaimus.skl`\n",
		"skill/atswordman/calldaimus.skl": "[skill type]\n`passive`\n[skill class]\n3\n[skill fitness growtype]\n3\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.skillCatalog = catalog
	service.professionMu.Lock()
	service.professionProfiles = nil
	service.professionProfilesErr = nil
	service.professionMu.Unlock()
	service.initialSkillsByJob = map[byte][]initialSkillEntry{
		currentATSwordmanJob: {{SkillID: currentSwordDemonSnakeSwordSkillID, Level: 1}},
	}
	service.initialSPTable = map[int]int{20: 20}
	service.initialTPTable = map[int]int{20: 0}
	skillRecord := dnfrepo.SkillRecord{
		CharacterID: "99",
		Points:      dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
		Skills: map[int64]dnfrepo.SkillState{
			currentSwordDemonSnakeSwordSkillID: {Level: 1, Enabled: true},
		},
	}
	dnfrepo.SetSkillTree(&skillRecord, 1, dnfrepo.SkillTreeState{
		Skills: map[int64]dnfrepo.SkillState{
			currentSwordDemonSnakeSwordSkillID: {Level: 1, Enabled: true},
		},
		Points: skillRecord.Points,
	})
	if err := repositories.Skill.Save(context.Background(), skillRecord); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	if err := service.sendCurrentPostStartMapPlayerPlacement(
		session,
		runtime,
		runtime.Session.Snapshot().Scene,
		"test_sword_demon_snake_sword_prebind",
	); err != nil {
		t.Fatal(err)
	}

	mode1Index := -1
	skillIndices := make([]int, 0, 2)
	skillBodies := make([][]byte, 0, 2)
	stream := connection.write.Bytes()
	for packetIndex := 0; len(stream) > 0; packetIndex++ {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			mode1Index = packetIndex
		}
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID {
			skillIndices = append(skillIndices, packetIndex)
			skillBodies = append(skillBodies, append([]byte(nil), packet.Body...))
		}
		stream = rest
	}
	if mode1Index < 0 || len(skillIndices) != 2 || !(skillIndices[0] < mode1Index && mode1Index < skillIndices[1]) {
		t.Fatalf("Snake Sword dungeon order skills=%v mode1=%d", skillIndices, mode1Index)
	}
	for index, body := range skillBodies {
		slots := currentSkillInfoFirstTreeSlots(t, body)
		foundSnakeSword := false
		for _, skillID := range slots {
			foundSnakeSword = foundSnakeSword || int64(skillID) == currentSwordDemonSnakeSwordSkillID
		}
		if !foundSnakeSword {
			t.Fatalf("Snake Sword op19[%d] slots=%v", index, slots)
		}
	}
	if !session.dungeonSceneSkillInfoSent {
		t.Fatal("dungeonSceneSkillInfoSent = false after final Sword Demon op19")
	}
}

func TestCurrentDungeonActorPassivePrebindSelectsOnlyPVFProvenGrowths(t *testing.T) {
	tests := []struct {
		name      string
		job       string
		growType  int64
		wantSkill int64
		want      bool
	}{
		{name: "Soul Bender first awakening", job: "0", growType: 0x12, wantSkill: currentSoulBenderFearAuraSkillID, want: true},
		{name: "Soul Bender second awakening", job: "0", growType: 0x22, wantSkill: currentSoulBenderFearAuraSkillID, want: true},
		{name: "Soul Bender before awakening", job: "0", growType: 0x02},
		{name: "Swordman other awakened branch", job: "0", growType: 0x11},
		{name: "Kunoichi Leave Trace TP", job: "6", growType: 0x03, wantSkill: currentKunoichiLeaveTraceEXSkillID, want: true},
		{name: "Thief other branch", job: "6", growType: 0x02},
		{name: "Male Brawler second awakening", job: "7", growType: 0x23, wantSkill: currentMaleBrawlerRuleBreakSkillID, want: true},
		{name: "Male Brawler first awakening", job: "7", growType: 0x13},
		{name: "Dimension Walker second awakening", job: "8", growType: 0x25, wantSkill: currentDimensionWalkerChaosStoneSkillID, want: true},
		{name: "Male Mage other second awakening", job: "8", growType: 0x24},
		{name: "Sword Demon", job: "11", growType: 0x03, wantSkill: currentSwordDemonSnakeSwordSkillID, want: true},
		{name: "Paladin first awakening", job: "12", growType: 0x13, wantSkill: currentPaladinGloryOfLightSkillID, want: true},
		{name: "Paladin second awakening", job: "12", growType: 0x23, wantSkill: currentPaladinCommandmentSkillID, want: true},
		{name: "Dragon Knight second awakening", job: "12", growType: 0x24, wantSkill: currentDragonKnightHeirSkillID, want: true},
		{name: "Dragon Knight first awakening", job: "12", growType: 0x14},
		{name: "out of range grow type", job: "0", growType: 0x112},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			character := dnfrepo.CharacterRecord{
				Job:   test.job,
				Stats: map[string]int64{"grow_type": test.growType},
			}
			rule, found := currentDungeonActorPassivePrebindForCharacter(character)
			if found != test.want {
				t.Fatalf("prebind found=%t, want %t rule=%+v", found, test.want, rule)
			}
			if found && !currentSkillIDsContain(rule.skillIDs, test.wantSkill) {
				t.Fatalf("prebind skills=%v, want %d", rule.skillIDs, test.wantSkill)
			}
		})
	}
}

func TestCurrentDungeonActorPassivePrebindDoesNotSendUnlearnedPaidFearAura(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":         "0 `swordman.lst`\n",
		"skill/swordman.lst":          "46 `swordman/active.skl`\n89 `swordman/darkaura.skl`\n",
		"skill/swordman/active.skl":   "[type]\n`active`\n",
		"skill/swordman/darkaura.skl": "[type]\n`passive`\n[skill class]\n3\n[skill fitness second growtype]\n1 1\n[purchase cost]\n45\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.SkillRecord{
		CharacterID: "99",
		Skills: map[int64]dnfrepo.SkillState{
			46: {Level: 1, Enabled: true},
		},
		Points: dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
	}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points})
	if err := repositories.Skill.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		skillCatalog:       catalog,
		initialSkillsByJob: map[byte][]initialSkillEntry{
			currentSwordmanJob: {{SkillID: 46, Level: 1}},
		},
		initialSPTable: map[int]int{20: 20},
		initialTPTable: map[int]int{20: 0},
	}
	connection := &bufferConn{}
	projection, ready, err := service.buildAndSendDungeonActorPassivePrebind(
		&gameSession{conn: connection},
		context.Background(),
		dnfrepo.CharacterRecord{
			CharacterID: "99",
			Job:         "0",
			Level:       20,
			Stats:       map[string]int64{"grow_type": 0x12},
		},
		"test_unlearned_paid_fear_aura",
	)
	if err != nil || !ready {
		t.Fatalf("unlearned Fear Aura projection ready=%t err=%v", ready, err)
	}
	if currentProjectionHasEnabledSkill(projection, currentSoulBenderFearAuraSkillID) {
		t.Fatalf("unlearned paid Fear Aura entered projection: %+v", projection.learnedSkillIDs)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unlearned paid Fear Aura sent pre-mode1 op19: %x", connection.write.Bytes())
	}
}

func TestCurrentDungeonActorPassivePrebindSendsPaladinSkillsInOneProjection(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":              "12 `knight.lst`\n",
		"skill/knight.lst":                 "120 `knight/gloryofthelight.skl`\n149 `knight/commandment.skl`\n",
		"skill/knight/gloryofthelight.skl": "[type]\n`passive`\n[skill class]\n3\n[skill fitness second growtype]\n1\n[purchase cost]\n45\n",
		"skill/knight/commandment.skl":     "[type]\n`passive`\n[skill class]\n3\n[skill fitness second growtype]\n2\n[purchase cost]\n80\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	record := dnfrepo.SkillRecord{
		CharacterID: "99",
		Skills: map[int64]dnfrepo.SkillState{
			currentPaladinGloryOfLightSkillID: {Level: 1, Enabled: true},
			currentPaladinCommandmentSkillID:  {Level: 1, Enabled: true},
		},
		Points: dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 0, SyncedLevel: 90},
	}
	dnfrepo.SetSkillTree(&record, 1, dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points})
	if err := repositories.Skill.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		skillCatalog:       catalog,
		initialSkillsByJob: map[byte][]initialSkillEntry{currentActorPassiveKnightJob: {}},
		initialSPTable:     map[int]int{90: 100},
		initialTPTable:     map[int]int{90: 0},
	}
	connection := &bufferConn{}
	projection, ready, err := service.buildAndSendDungeonActorPassivePrebind(
		&gameSession{conn: connection},
		context.Background(),
		dnfrepo.CharacterRecord{
			CharacterID: "99",
			Job:         "12",
			Level:       90,
			Stats:       map[string]int64{"grow_type": 0x23},
		},
		"test_paladin_multiple_entry_passives",
	)
	if err != nil || !ready {
		t.Fatalf("Paladin prebind ready=%t err=%v", ready, err)
	}
	for _, skillID := range []int64{currentPaladinGloryOfLightSkillID, currentPaladinCommandmentSkillID} {
		if !currentProjectionHasEnabledSkill(projection, skillID) {
			t.Fatalf("Paladin projection omitted skill %d: %v", skillID, projection.learnedSkillIDs)
		}
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("Paladin prebind packet=%+v trailing=%x", packet.Header, trailing)
	}
	slots := currentSkillInfoFirstTreeSlots(t, packet.Body)
	for _, want := range []int64{currentPaladinGloryOfLightSkillID, currentPaladinCommandmentSkillID} {
		found := false
		for _, skillID := range slots {
			found = found || int64(skillID) == want
		}
		if !found {
			t.Fatalf("Paladin one-shot prebind omitted skill %d from slots=%v", want, slots)
		}
	}
}

func TestCurrentPostStartMapPreinstallsSoulBenderFearAuraBeforeDungeonMode1(t *testing.T) {
	service, runtime := prepareTutorialScopeRuntime(t, tutorialScopeFixtureOptions{disableTutorial: true})
	repositories, ok := service.repositoryGroup()
	if !ok || repositories.Skill == nil || repositories.Character == nil {
		t.Fatal("Soul Bender dungeon entry repositories unavailable")
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load Soul Bender character: found=%t err=%v", found, err)
	}
	character.Job = "0"
	character.Stats["grow_type"] = 0x12
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	catalog, err := buildSkillCatalogFromSource(context.Background(), initialEquipmentMemSource{
		"skill/skilllist.lst":         "0 `swordman.lst`\n",
		"skill/swordman.lst":          "89 `swordman/darkaura.skl`\n",
		"skill/swordman/darkaura.skl": "[type]\n`passive`\n[skill class]\n3\n[skill fitness second growtype]\n1 1\n[purchase cost]\n45\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.skillCatalog = catalog
	service.professionMu.Lock()
	service.professionProfiles = nil
	service.professionProfilesErr = nil
	service.professionMu.Unlock()
	service.initialSkillsByJob = map[byte][]initialSkillEntry{currentSwordmanJob: {}}
	service.initialSPTable = map[int]int{20: 20}
	service.initialTPTable = map[int]int{20: 0}
	skillRecord := dnfrepo.SkillRecord{
		CharacterID: "99",
		Points:      dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
		Skills: map[int64]dnfrepo.SkillState{
			currentSoulBenderFearAuraSkillID: {Level: 1, Enabled: true},
		},
	}
	dnfrepo.SetSkillTree(&skillRecord, 1, dnfrepo.SkillTreeState{
		Skills: map[int64]dnfrepo.SkillState{
			currentSoulBenderFearAuraSkillID: {Level: 1, Enabled: true},
		},
		Points: skillRecord.Points,
	})
	if err := repositories.Skill.Save(context.Background(), skillRecord); err != nil {
		t.Fatal(err)
	}

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 16},
		residentChannel:     channelcatalog.Channel{ID: 16},
		selectedCharacterID: 99,
	}
	if err := service.sendCurrentPostStartMapPlayerPlacement(
		session,
		runtime,
		runtime.Session.Snapshot().Scene,
		"test_soul_bender_fear_aura_prebind",
	); err != nil {
		t.Fatal(err)
	}

	mode1Index := -1
	skillIndices := make([]int, 0, 2)
	skillBodies := make([][]byte, 0, 2)
	stream := connection.write.Bytes()
	for packetIndex := 0; len(stream) > 0; packetIndex++ {
		packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1 {
			mode1Index = packetIndex
		}
		if packet.Header.Classification == 0 && packet.Header.MsgID == currentSkillInfoMsgID {
			skillIndices = append(skillIndices, packetIndex)
			skillBodies = append(skillBodies, append([]byte(nil), packet.Body...))
		}
		stream = rest
	}
	if mode1Index < 0 || len(skillIndices) != 2 || !(skillIndices[0] < mode1Index && mode1Index < skillIndices[1]) {
		t.Fatalf("Fear Aura dungeon order skills=%v mode1=%d", skillIndices, mode1Index)
	}
	for index, body := range skillBodies {
		slots := currentSkillInfoFirstTreeSlots(t, body)
		foundFearAura := false
		for _, skillID := range slots {
			foundFearAura = foundFearAura || int64(skillID) == currentSoulBenderFearAuraSkillID
		}
		if !foundFearAura {
			t.Fatalf("Fear Aura op19[%d] slots=%v", index, slots)
		}
	}
	if !session.dungeonSceneSkillInfoSent {
		t.Fatal("dungeonSceneSkillInfoSent = false after final Soul Bender op19")
	}
}

type failNthDungeonWriteConn struct {
	bufferConn
	writes int
	failAt int
	err    error
}

func (connection *failNthDungeonWriteConn) Write(data []byte) (int, error) {
	connection.writes++
	if connection.writes == connection.failAt {
		return 0, connection.err
	}
	return connection.bufferConn.Write(data)
}

func prepareSyntheticDungeonRuntimeForEntryTest(
	t *testing.T,
	source bridgePVFSource,
	aiCatalog *pvfDungeonAICharacterCatalog,
) *runtimeDungeonState {
	t.Helper()
	table, resolver, monsters := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   "account-1",
		Level:       20,
		Stats:       map[string]int64{"fatigue": 100},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:                 options{accountID: "account-1"},
		worldMapTable:           table,
		worldMapResolver:        resolver,
		dungeonMonsterTable:     monsters,
		dungeonAICharacterTable: aiCatalog,
		dungeonChoice:           func(int) (int, error) { return 0, nil },
		dungeonSeed:             func() (uint32, error) { return 0x12345678, nil },
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 99}
	runtime, _, err := service.prepareDungeonRuntime(
		context.Background(),
		session,
		dungeoncmd.SelectDungeonRequest{DungeonID: 700, Difficulty: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}
