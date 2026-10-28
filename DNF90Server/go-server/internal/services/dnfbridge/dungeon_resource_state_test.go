package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfdungeonpermission "longheng.io/server/internal/modules/dnf/dungeonpermission"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestBuildCurrentDungeonResourceStateBody(t *testing.T) {
	want := []byte{1, 0, 0xe9, 0x1b, 0, 0, 1}
	if got := buildCurrentDungeonResourceStateBody(7145); !bytes.Equal(got, want) {
		t.Fatalf("resource state body=%x want=%x", got, want)
	}
}

func TestSendCurrentDungeonResourceStateUsesSingleAntonClientIdentity(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 1001}
	runtime := &runtimeDungeonState{
		Dungeon:         worldmap.Dungeon{ID: 5314},
		clientDungeonID: 210,
	}

	if err := service.sendCurrentDungeonResourceState(session, runtime, "single_anton_test"); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	want := buildCurrentDungeonResourceStateBody(210)
	if packet.Header.MsgID != currentDungeonResourceStateMsgID || packet.Header.Classification != 0 ||
		!bytes.Equal(packet.Body, want) || len(rest) != 0 {
		t.Fatalf("single Anton op5 header=%+v body=%x rest=%x want=%x", packet.Header, packet.Body, rest, want)
	}
}

func TestBuildCurrentDungeonResourceStateEntriesBody(t *testing.T) {
	entries := []currentDungeonResourceStateEntry{
		{DungeonID: 3, State: 1},
		{DungeonID: 7145, State: 4},
	}
	want := []byte{2, 0, 3, 0, 0, 0, 1, 0xe9, 0x1b, 0, 0, 4}
	if got := buildCurrentDungeonResourceStateEntriesBody(entries); !bytes.Equal(got, want) {
		t.Fatalf("resource state entries body=%x want=%x", got, want)
	}
}

func TestCurrentDungeonDifficultyAuthorityIncludesUnclearedRowsWithoutQuest(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source[worldmap.DefaultMapList] += "101 `dungeon/test/second.map`\n"
	source["map/dungeon/test/second.map"] = "[map name]\n`second`\n" +
		"[dungeon]\n701\n" +
		"[type]\n`[start]`\n"
	source[worldmap.DefaultDungeonList] += "701 `second.dgn`\n"
	source["dungeon/test.dgn"] = "[name]\n`First Ordinary Dungeon`\n" +
		"[difficulty level]\n1 1 1 1 0\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	source["dungeon/second.dgn"] = "[name]\n`Second Ordinary Dungeon`\n" +
		"[difficulty level]\n1 1 1 1 0\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 101\n[start map]\n0 0\n[boss map]\n0 0\n"
	source[worldmap.DefaultWorldMapList] = "1 `test.wdm`\n"
	source["worldmap/test.wdm"] = "[name]\n`Synthetic Area`\n[dungeon]\n700 -1 701 -1\n[/dungeon]\n"
	table, _, _ := loadBridgeDungeonStaticData(t, source)

	// Only dungeon 700 has a persisted advanced difficulty. Dungeon 701 has no
	// quest and no clear row, but native op5 must still expose its normal state.
	record := dnfrepo.DungeonPermissionRecord{Entries: []dnfrepo.DungeonPermissionEntry{
		{DungeonID: 700, ClearState: 2},
	}}
	want := []currentDungeonResourceStateEntry{
		{DungeonID: 700, State: 2},
		{DungeonID: 701, State: 0},
	}
	if got := currentDungeonResourceStateEntriesFromPermissionRecord(table, record); !equalCurrentDungeonResourceStateEntries(got, want) {
		t.Fatalf("difficulty authority=%+v want=%+v", got, want)
	}
}

func TestRealPVFDifficultyAuthorityIncludesTreeBreedingWithoutQuest(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run the real dungeon authority test")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	entries := currentDungeonResourceStateEntriesFromPermissionRecord(table, dnfrepo.DungeonPermissionRecord{})
	for _, entry := range entries {
		if entry.DungeonID == 164 {
			if entry.State != 0 {
				t.Fatalf("uncleared tree-breeding difficulty state=%d want=0", entry.State)
			}
			t.Logf("real PVF ordinary difficulty authority rows=%d tree_breeding_state=%d", len(entries), entry.State)
			return
		}
	}
	t.Fatalf("real PVF ordinary difficulty authority omitted tree-breeding dungeon 164; rows=%d", len(entries))
}

func TestRealPVFDifficultyAuthorityIncludesMetroCenterWithoutActiveQuest(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to run the real dungeon authority test")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	table, err := worldmap.LoadSource(context.Background(), archive, worldmap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	entries := currentDungeonResourceStateEntriesFromPermissionRecord(table, dnfrepo.DungeonPermissionRecord{})
	for _, entry := range entries {
		if entry.DungeonID == 310 {
			if entry.State != 0 {
				t.Fatalf("uncleared Time Plaza difficulty state=%d want=0", entry.State)
			}
			return
		}
	}
	t.Fatalf("real PVF ordinary difficulty authority omitted Time Plaza dungeon 310 for a character with no active quest; rows=%d", len(entries))
}

func equalCurrentDungeonResourceStateEntries(left, right []currentDungeonResourceStateEntry) bool {
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

func TestCurrentDungeonNextClearStateUsesPVFMaxDifficulty(t *testing.T) {
	dungeon := worldmap.Dungeon{Metadata: worldmap.DungeonMetadata{
		DifficultyLevels: []int64{1, 2, 3, 4, 5},
	}}
	if got := currentDungeonNextClearState(dungeon, 0); got != 1 {
		t.Fatalf("normal clear state=%d want 1", got)
	}
	if got := currentDungeonNextClearState(dungeon, 4); got != 4 {
		t.Fatalf("highest clear state=%d want capped 4", got)
	}
	dungeon.Metadata.DifficultyLevels = []int64{1}
	if got := currentDungeonNextClearState(dungeon, 0); got != 1 {
		t.Fatalf("single fixed difficulty clear state=%d want 1", got)
	}
	dungeon.Metadata.DifficultyLevels = nil
	if got := currentDungeonNextClearState(dungeon, 0); got != 1 {
		t.Fatalf("implicit fixed difficulty clear state=%d want 1", got)
	}
	dungeon.Metadata.DesignatedDifficulties = []int64{0, 1}
	if got := currentDungeonNextClearState(dungeon, 3); got != 4 {
		t.Fatalf("designated clear state=%d want 4", got)
	}
}

func TestCommitCurrentDungeonPermissionPersistsImplicitFixedDifficultyClear(t *testing.T) {
	repositories := testRepositoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "91", AccountID: defaultAccountPrefix + "1",
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 91}
	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{ID: 5109},
		Request: dungeoncmd.SelectDungeonRequest{Difficulty: 0},
	}

	body, state, updated, err := service.commitCurrentDungeonPermission(context.Background(), session, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !updated || state != 1 {
		t.Fatalf("fixed-difficulty permission updated=%t state=%d want true,1", updated, state)
	}
	wantBody := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{DungeonID: 5109, State: 1}})
	if !bytes.Equal(body, wantBody) {
		t.Fatalf("fixed-difficulty permission body=%x want=%x", body, wantBody)
	}
	accountKey, err := dnfdungeonpermission.AccountOwnerKey(defaultAccountPrefix + "1")
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := repositories.DungeonPermission.Load(context.Background(), accountKey)
	if err != nil || !found || len(record.Entries) != 1 || record.Entries[0].DungeonID != 5109 || record.Entries[0].ClearState != 1 {
		t.Fatalf("persisted account fixed-difficulty permission found=%t record=%+v err=%v", found, record, err)
	}
}

func TestCurrentDungeonPermissionSnapshotSharesLegacyMaximumAcrossAccountCharacters(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Shared Difficulty Dungeon`\n" +
		"[difficulty level]\n1 1 1 1 0\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	repositories := testRepositoryGroup()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "11", AccountID: "account-shared", Slot: 0},
		{CharacterID: "12", AccountID: "account-shared", Slot: 1},
	} {
		if err := repositories.Character.Save(context.Background(), character); err != nil {
			t.Fatal(err)
		}
	}
	if _, changed, err := repositories.DungeonPermission.UpsertMax(context.Background(), "11", 700, 3); err != nil || !changed {
		t.Fatalf("seed legacy character permission changed=%t err=%v", changed, err)
	}
	service := &Service{
		options:            options{accountID: "account-shared"},
		worldMapTable:      table,
		worldMapResolver:   resolver,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	entries, err := service.currentDungeonPermissionSnapshotEntries(context.Background(), &gameSession{
		accountID: "account-shared", selectedCharacterID: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != (currentDungeonResourceStateEntry{DungeonID: 700, State: 3}) {
		t.Fatalf("account-shared permission entries=%+v", entries)
	}
	accountKey, err := dnfdungeonpermission.AccountOwnerKey("account-shared")
	if err != nil {
		t.Fatal(err)
	}
	persisted, found, err := repositories.DungeonPermission.Load(context.Background(), accountKey)
	if err != nil || !found || len(persisted.Entries) != 1 || persisted.Entries[0].ClearState != 3 {
		t.Fatalf("migrated account permission=%+v found=%t err=%v", persisted, found, err)
	}
}

func TestSelectCharacterSendsDungeonPermissionSnapshotImmediatelyAfterAck(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Synthetic Dungeon`\n" +
		"[minimum required level]\n10\n" +
		"[difficulty level]\n0 1 2\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	repositories := testRepositoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "99",
		AccountID:   defaultAccountPrefix + "1",
		Slot:        3,
		Name:        "worldmap-owner",
		Job:         "0",
		Level:       20,
		Stats: map[string]int64{
			"town_id": 38, "area_id": 1,
			"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			currentDungeonTutorialCompletedKey: 1,
		},
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if _, updated, err := repositories.DungeonPermission.UpsertMax(context.Background(), "99", 700, 1); err != nil || !updated {
		t.Fatalf("seed dungeon permission updated=%t err=%v", updated, err)
	}
	service := &Service{
		options:            options{accountPrefix: defaultAccountPrefix},
		worldMapTable:      table,
		worldMapResolver:   resolver,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:            connection,
		channel:         channelcatalog.Channel{ID: 16, Type: 1, Name: "ch.16", Port: 10016},
		residentChannel: channelcatalog.Channel{ID: 16, Type: 1, Name: "ch.16", Port: 10016},
		rosterRequested: true,
	}
	request := make([]byte, 2)
	binary.LittleEndian.PutUint16(request, 99)
	if err := service.sendUpperCSharpSelectInit(session, request); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketSelectCharacter) {
		t.Fatalf("first packet header=%+v body=%x", ack.Header, ack.Body)
	}
	permission, rest := splitGameServerUpperPacket(t, rest)
	wantPermission := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{DungeonID: 700, State: 1}})
	if permission.Header.Classification != 0 || permission.Header.MsgID != currentDungeonResourceStateMsgID || !bytes.Equal(permission.Body, wantPermission) {
		t.Fatalf("post-select ACK permission header=%+v body=%x want=%x", permission.Header, permission.Body, wantPermission)
	}
	digest, _ := splitGameServerUpperPacket(t, rest)
	if digest.Header.MsgID != currentStoryDigestLastLevelMsgID {
		t.Fatalf("packet after early permission header=%+v body=%x", digest.Header, digest.Body)
	}
}

func TestEnterSelectDungeonRefreshesPermissionSnapshotAfterFatigue(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Synthetic Dungeon`\n" +
		"[difficulty level]\n0 1 2\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	service, session, repositories := newTownMoveTest(t)
	service.worldMapTable = table
	service.worldMapResolver = resolver
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if _, updated, err := repositories.DungeonPermission.UpsertMax(context.Background(), "29", 700, 1); err != nil || !updated {
		t.Fatalf("seed dungeon permission updated=%t err=%v", updated, err)
	}
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(884, 248, 6, 100)); err != nil {
		t.Fatal(err)
	}
	connection := session.conn.(*bufferConn)
	connection.write.Reset()
	if err := service.sendEnterSelectDungeonState(session, "test_permission_snapshot_after_fatigue", false, true); err != nil {
		t.Fatal(err)
	}

	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketEnterSelectDungeon) || ack.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("op15 ACK header=%+v", ack.Header)
	}
	fatigue, rest := splitGameServerUpperPacket(t, rest)
	if fatigue.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("packet after op15 ACK header=%+v", fatigue.Header)
	}
	skills, rest := splitGameServerUpperPacket(t, rest)
	if skills.Header.Classification != 0 || skills.Header.MsgID != currentSkillInfoMsgID ||
		!bytes.Equal(skills.Body, townMoveSkillProjectionBody(t, repositories, "29")) {
		t.Fatalf("selector skill refresh header=%+v body=%x rest=%x", skills.Header, skills.Body, rest)
	}
	permission, rest := splitGameServerUpperPacket(t, rest)
	wantPermission := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{DungeonID: 700, State: 1}})
	if permission.Header.Classification != 0 || permission.Header.MsgID != currentDungeonResourceStateMsgID ||
		!bytes.Equal(permission.Body, wantPermission) {
		t.Fatalf("zero-active-quest permission refresh after op19 header=%+v body=%x want=%x", permission.Header, permission.Body, wantPermission)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.MsgID != currentDungeonContextMsgID || len(trailing) != 0 {
		t.Fatalf("selector sequence context=%+v trailing=%x", contextPacket.Header, trailing)
	}
}

func TestEnterSelectDungeonWithEmptyTasksStillPublishesLightLancerDungeonAuthority(t *testing.T) {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Visible Without Tasks`\n" +
		"[difficulty level]\n0 1 2\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	service, session, repositories := newTownMoveTest(t)
	service.worldMapTable = table
	service.worldMapResolver = resolver
	character, found, err := repositories.Character.Load(context.Background(), "29")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Job = "13"
	character.Stats["grow_type"] = 0x23
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(context.Background(), dnfrepo.QuestRecord{CharacterID: "29"}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserArea(session, buildTownMoveRequest(38, 0, 900, 250, 5)); err != nil {
		t.Fatal(err)
	}
	if err := service.handleTownSetUserPosition(session, buildTownPositionRequest(884, 248, 6, 100)); err != nil {
		t.Fatal(err)
	}
	session.initialTownSkillInfoSent = true
	connection := session.conn.(*bufferConn)
	connection.write.Reset()

	if err := service.sendEnterSelectDungeonState(session, "test_empty_task_light_lancer_selector", false, true); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if ack.Header.MsgID != uint16(dnfenum.CmdPacketEnterSelectDungeon) {
		t.Fatalf("op15 ACK header=%+v", ack.Header)
	}
	fatigue, rest := splitGameServerUpperPacket(t, rest)
	if fatigue.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("fatigue header=%+v", fatigue.Header)
	}
	skills, rest := splitGameServerUpperPacket(t, rest)
	if skills.Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("empty-task skill projection header=%+v", skills.Header)
	}
	permission, rest := splitGameServerUpperPacket(t, rest)
	wantPermission := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{DungeonID: 700, State: 0}})
	if permission.Header.MsgID != currentDungeonResourceStateMsgID || !bytes.Equal(permission.Body, wantPermission) {
		t.Fatalf("empty-task permission header=%+v body=%x want=%x", permission.Header, permission.Body, wantPermission)
	}
	contextPacket, trailing := splitGameServerUpperPacket(t, rest)
	if contextPacket.Header.MsgID != currentDungeonContextMsgID || len(trailing) != 0 {
		t.Fatalf("selector context header=%+v trailing=%x", contextPacket.Header, trailing)
	}
}
