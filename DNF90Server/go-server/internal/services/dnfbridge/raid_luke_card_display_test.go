package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 抓包统一用同包既有的 bufferConn（service_test.go）：它的 write 缓冲就是线上的
// 全部字节，所以任何“包真的发出去了”的断言都直接看它。
// lukeCardDisplayTestArcana is the `[CARD]` monster-id list of
// etc/raid/luke/LukeCardSet.etc, verbatim (major arcana 0..21).
var lukeCardDisplayTestArcana = []uint32{
	59171, 59098, 59873, 59096, 56065, 59874, 56699, 57155, 59278, 56166, 56673,
	59875, 56066, 56674, 56067, 56068, 56063, 56700, 57156, 59872, 59099, 59095,
}

// lukeCardDisplayTestPatternKeys is the (dungeon id, pattern index) list of the
// same file's eight `[PATTERN]` blocks: 5054 x2, 5055 x4, 5056 x2.
var lukeCardDisplayTestPatternKeys = [][2]int{
	{5054, 0}, {5054, 1},
	{5055, 0}, {5055, 1}, {5055, 2}, {5055, 3},
	{5056, 0}, {5056, 1},
}

func lukeCardDisplayTestConfig() *raid.LukeCardSetConfig {
	config := &raid.LukeCardSetConfig{}
	for _, key := range lukeCardDisplayTestPatternKeys {
		positions := make([]raid.LukeCardPosition, lukeCardSlotsPerPattern)
		for index := range positions {
			positions[index] = raid.LukeCardPosition{X: float64(index % 4), Y: float64(index / 4)}
		}
		config.Patterns = append(config.Patterns, raid.LukeCardPattern{
			DungeonID:     uint32(key[0]),
			Index:         key[1],
			ImagePath:     `Interface2\raid\luke\luke_card_system\card_buff_icon.img`,
			ImageIndices:  []int{key[1]},
			CardPositions: positions,
			Tooltip:       "test",
		})
	}
	for index, monsterID := range lukeCardDisplayTestArcana {
		config.Cards = append(config.Cards, raid.LukeCard{
			MonsterID:  monsterID,
			ImagePath:  `Interface2\raid\luke\luke_card_system\tarotcard.img`,
			ImageIndex: index,
			Tooltip:    "test",
		})
	}
	return config
}

// lukeCardDisplayTestDungeon builds one [DUNGEON] block of the monster-list
// fixture: maze index i spawns monsters[i] in source order.
func lukeCardDisplayTestDungeon(dungeonID uint32, groups [][]uint32) raid.LukeRaidMonsterListDungeon {
	dungeon := raid.LukeRaidMonsterListDungeon{DungeonID: dungeonID}
	for index, monsters := range groups {
		dungeon.Mazes = append(dungeon.Mazes, raid.LukeRaidMonsterListMaze{
			Index:      index,
			MonsterIDs: append([]uint32(nil), monsters...),
		})
	}
	return dungeon
}

// lukeCardDisplayTestMonsterList mirrors the shipped [RAID MONSTER LIST] shape:
// fifteen mazes per sanctum, five groups of three, the first maze of every group
// carrying only a boss and the other two a named monster plus a boss, which is
// exactly the 1+2+2 = five arcanum the shipped table yields per group.
//
// Each sanctum draws only from its own third of lukeCardDisplayTestArcana, so no
// two sanctums can ever produce the same face whichever group the day picks: that
// keeps "each sanctum reads its own mazes" observable.
func lukeCardDisplayTestMonsterList() *raid.LukeRaidMonsterListConfig {
	blocks := map[uint32][]uint32{
		lukeBirthSanctumMapID:      lukeCardDisplayTestArcana[0:7],
		lukeExtinctionSanctumMapID: lukeCardDisplayTestArcana[7:15],
		lukeSpreadSanctumMapID:     lukeCardDisplayTestArcana[15:22],
	}
	config := &raid.LukeRaidMonsterListConfig{}
	for _, dungeonID := range runtimeRaidLukeTarotSanctums {
		block := blocks[dungeonID]
		groups := make([][]uint32, 0, 15)
		for maze := 0; maze < 15; maze++ {
			group := maze / lukeSanctumClearsPerGroup
			pick := func(offset int) uint32 { return block[(group+offset)%len(block)] }
			if maze%lukeSanctumClearsPerGroup == 0 {
				groups = append(groups, []uint32{pick(0)})
				continue
			}
			groups = append(groups, []uint32{pick(1 + (maze%lukeSanctumClearsPerGroup-1)*2), pick(2 + (maze%lukeSanctumClearsPerGroup-1)*2)})
		}
		config.Dungeons = append(config.Dungeons, lukeCardDisplayTestDungeon(dungeonID, groups))
	}
	return config
}

func lukeCardDisplayTestState() runtimeRaidState {
	return runtimeRaidState{
		RaidKey:  9001,
		RaidMode: runtimeRaidOwnerLuke,
		Started:  true,
		LukeConfig: &raid.LukeConfig{
			CardSetMapInfo: append([]int(nil), lukeCardSetMapInfo...),
		},
	}
}

// TestLukeTarotCardSetIndexIsOneSetPerDayInRange pins the field-1 byte's source:
// the client validates it against the [CARD SET MAP INFO] record count (5) and
// drops the whole packet outside it, and its value is a function of the tarot
// day alone because the official notice says every channel shows the same cards
// on the same day.
func TestLukeTarotCardSetIndexIsOneSetPerDayInRange(t *testing.T) {
	index, records := runtimeRaidLukeTarotCardSetIndex(20260929, lukeCardSetMapInfo)
	if records != 5 {
		t.Fatalf("records=%d want 5 ([CARD SET MAP INFO] 15 values / 3)", records)
	}
	if index < 0 || index >= records {
		t.Fatalf("card set index=%d outside [0,%d)", index, records)
	}
	if again, _ := runtimeRaidLukeTarotCardSetIndex(20260929, lukeCardSetMapInfo); again != index {
		t.Fatalf("one day drew %d then %d", index, again)
	}
	seen := map[int]bool{}
	for day := 1; day <= 30; day++ {
		value, _ := runtimeRaidLukeTarotCardSetIndex(20260900+day, lukeCardSetMapInfo)
		if value < 0 || value >= records {
			t.Fatalf("day %d index=%d outside [0,%d)", day, value, records)
		}
		seen[value] = true
	}
	if len(seen) < 3 {
		t.Fatalf("a month of tarot days drew only card sets %v", seen)
	}
	// A missing table is not "set 0": the caller must refuse to send at all.
	if _, records := runtimeRaidLukeTarotCardSetIndex(20260929, nil); records != 0 {
		t.Fatalf("records for an empty [CARD SET MAP INFO]=%d want 0", records)
	}
	if index, records := runtimeRaidLukeTarotCardSetIndex(20260929, []int{0, 0, 0}); index != 0 || records != 1 {
		t.Fatalf("single record index=%d records=%d want 0/1", index, records)
	}
}

// TestLukeCardSetIndexEqualsTheMazeRecordTheSanctumsWalk is the cross-check the
// 18:26 production log raised: the byte the client is sent as field 1
// (`card_set_index`) and the [CARD SET MAP INFO] record the three sanctums
// actually walk must be the SAME number, and the record count behind it must be
// the table's 5 records -- never the packet's N (the 3 sanctum records).
func TestLukeCardSetIndexEqualsTheMazeRecordTheSanctumsWalk(t *testing.T) {
	if got := runtimeRaidLukeTarotCardSetRecords(lukeCardSetMapInfo); got != 5 {
		t.Fatalf("[CARD SET MAP INFO] records=%d want 5", got)
	}
	if got := runtimeRaidLukeCardSetRecordCount(lukeCardSetMapInfo); got != 5 {
		t.Fatalf("runtimeRaidLukeCardSetRecordCount=%d want 5", got)
	}
	for day := 1; day <= 30; day++ {
		raidDay := 20260900 + day
		records := runtimeRaidLukeTarotCardSetRecords(lukeCardSetMapInfo)
		record := runtimeRaidLukeTarotCardSetRecord(raidDay, records)
		index, counted := runtimeRaidLukeTarotCardSetIndex(raidDay, lukeCardSetMapInfo)
		if counted != records || index != record {
			t.Fatalf("day %d: packet field 1=%d (of %d) but the day's record=%d (of %d)",
				raidDay, index, counted, record, records)
		}
		// The maze side: every sanctum reports that same record.
		draw := func(limit int) (int, error) { return runtimeRaidLukeTarotCardSetRecord(raidDay, limit), nil }
		state := lukeSanctumMazeTestState()
		for _, sanctum := range []int{lukeBirthSanctumMapID, lukeExtinctionSanctumMapID, lukeSpreadSanctumMapID} {
			maze, ok, err := runtimeRaidLukeSanctumMazeLocked(state, sanctum, 15, draw)
			if err != nil || !ok {
				t.Fatalf("day %d sanctum %d ok=%t err=%v", raidDay, sanctum, ok, err)
			}
			if maze.Record != index {
				t.Fatalf("day %d sanctum %d walked record %d but the packet says %d",
					raidDay, sanctum, maze.Record, index)
			}
		}
	}
	// 2026-09-29 is the day the 18:26 production log was taken on: field 1 was 2.
	if got := runtimeRaidLukeTarotCardSetRecord(20260929, 5); got != 2 {
		t.Fatalf("20260929 record=%d want 2 (the value the live packet carried)", got)
	}
}

// TestLukeTarotCardsAreReproducibleDistinctAndSizedToThePattern is the pin for
// this implementation's own card-picking convention (the official rule is not
// public): seven DISTINCT major arcana out of the 22, identical for the same
// (day, sanctum, pattern) so a raid can reproduce them, and different once the
// day turns over.
func TestLukeTarotCardsAreReproducibleDistinctAndSizedToThePattern(t *testing.T) {
	for _, sanctum := range runtimeRaidLukeTarotSanctums {
		for pattern := 0; pattern < 4; pattern++ {
			cards := runtimeRaidLukeTarotCards(20260929, sanctum, pattern, lukeCardDisplayTestArcana, lukeCardSlotsPerPattern)
			if len(cards) != lukeCardSlotsPerPattern {
				t.Fatalf("sanctum %d pattern %d cards=%v want %d", sanctum, pattern, cards, lukeCardSlotsPerPattern)
			}
			seen := map[uint32]bool{}
			for _, cardID := range cards {
				if seen[cardID] {
					t.Fatalf("sanctum %d pattern %d repeated card %d", sanctum, pattern, cardID)
				}
				seen[cardID] = true
				if !containsRaidCard(lukeCardDisplayTestArcana, cardID) {
					t.Fatalf("sanctum %d pattern %d card %d is not a [CARD] key", sanctum, pattern, cardID)
				}
			}
			again := runtimeRaidLukeTarotCards(20260929, sanctum, pattern, lukeCardDisplayTestArcana, lukeCardSlotsPerPattern)
			if !reflect.DeepEqual(cards, again) {
				t.Fatalf("sanctum %d pattern %d drew %v then %v", sanctum, pattern, cards, again)
			}
		}
	}
	// The three sanctums of one day do not all show the same seven cards.
	first := runtimeRaidLukeTarotCards(20260929, 5054, 0, lukeCardDisplayTestArcana, lukeCardSlotsPerPattern)
	second := runtimeRaidLukeTarotCards(20260929, 5055, 0, lukeCardDisplayTestArcana, lukeCardSlotsPerPattern)
	if reflect.DeepEqual(first, second) {
		t.Fatalf("both sanctums drew %v", first)
	}
	// Across a month the draw changes.
	changed := false
	for day := 1; day <= 30; day++ {
		cards := runtimeRaidLukeTarotCards(20260900+day, 5055, 0, lukeCardDisplayTestArcana, lukeCardSlotsPerPattern)
		if !reflect.DeepEqual(cards, first) {
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("a month of tarot days drew the same seven cards")
	}
	// A short table is refused rather than padded or repeated.
	if cards := runtimeRaidLukeTarotCards(20260929, 5054, 0, lukeCardDisplayTestArcana[:6], lukeCardSlotsPerPattern); cards != nil {
		t.Fatalf("cards from a six-entry table=%v want nil", cards)
	}
	if cards := runtimeRaidLukeTarotCards(20260929, 5054, 0, lukeCardDisplayTestArcana, 0); cards != nil {
		t.Fatalf("cards for want=0 = %v want nil", cards)
	}
}

func containsRaidCard(cards []uint32, want uint32) bool {
	for _, cardID := range cards {
		if cardID == want {
			return true
		}
	}
	return false
}

// TestLukeTarotPatternIndexStaysInsideEachSanctumsOwnPatterns pins field B: the
// pattern index must be one of that sanctum's own `[PATTERN]` blocks (5054 has
// two, 5055 four, 5056 two), because the client looks the pair up in the pattern
// map and silently draws nothing when it misses.
func TestLukeTarotPatternIndexStaysInsideEachSanctumsOwnPatterns(t *testing.T) {
	config := lukeCardDisplayTestConfig()
	for _, sanctum := range runtimeRaidLukeTarotSanctums {
		patterns := config.PatternIndices(sanctum)
		if len(patterns) == 0 {
			t.Fatalf("sanctum %d has no patterns", sanctum)
		}
		picked, ok := runtimeRaidLukeTarotPatternIndex(20260929, sanctum, patterns)
		if !ok {
			t.Fatalf("sanctum %d gave no pattern", sanctum)
		}
		if !containsRaidPattern(patterns, picked) {
			t.Fatalf("sanctum %d picked pattern %d not in %v", sanctum, picked, patterns)
		}
		if again, _ := runtimeRaidLukeTarotPatternIndex(20260929, sanctum, patterns); again != picked {
			t.Fatalf("sanctum %d picked %d then %d", sanctum, picked, again)
		}
	}
	if _, ok := runtimeRaidLukeTarotPatternIndex(20260929, 5054, nil); ok {
		t.Fatal("an empty pattern list produced a pattern")
	}
}

func containsRaidPattern(patterns []int, want int) bool {
	for _, pattern := range patterns {
		if pattern == want {
			return true
		}
	}
	return false
}

// lukeCardDisplayTestService builds the smallest service that can actually send:
// one Luke raid in s.raids, the card set preset so no archive is read, and a
// session whose connection records the packet.
func lukeCardDisplayTestService(t *testing.T) (*Service, *gameSession, *bufferConn, uint32) {
	t.Helper()
	const (
		raidKey     = uint32(9001)
		characterID = uint16(1001)
	)
	service := &Service{raids: map[uint32]*runtimeRaidState{}}
	// luke.etc and the two tarot tables are **global** configuration now: the 1181
	// payload no longer comes from the raid, so the fixture presets all three
	// service-level caches (raid_luke_card_display.go).
	service.raidLuke = &raid.LukeConfig{CardSetMapInfo: append([]int(nil), lukeCardSetMapInfo...)}
	service.raidLukeCardSet = lukeCardDisplayTestConfig()
	service.raidLukeMonsterList = lukeCardDisplayTestMonsterList()
	state := lukeCardDisplayTestState()
	state.RaidKey = raidKey
	state.Members = []runtimeRaidMemberState{{CharID: characterID, GroupIndex: 1, SlotOrder: 0}}
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	service.raids[raidKey] = &state

	conn := &bufferConn{}
	session := &gameSession{conn: conn, selectedCharacterID: characterID}
	return service, session, conn, raidKey
}

// TestPublishRuntimeRaidLukeCardDisplaySendsOpcode1181Once 只验证**发布函数本身**
// （直接调用它），不作为“客户端真会走到那里”的证据 —— 真实路径的证据在
// TestLukeChannelTownEntryWithoutAnyRaidSendsTheCardDisplay 那一组。
//
// 它钉的是记账语义：账本是**每个登录会话、每个 tarot day 一次**（官方
// 「每次登陆后，第一次进入卢克 RAID 频道…展示一次」）。同一 session 的第一包
// openWindowFlag=1，之后每一包仍然发出去但 flag=0 —— 换团也一样是 0，因为这个包
// 与团无关；换一个 session（重新登录）才重新弹一次。
func TestPublishRuntimeRaidLukeCardDisplaySendsOpcode1181Once(t *testing.T) {
	service, session, conn, raidKey := lukeCardDisplayTestService(t)

	service.publishRuntimeRaidLukeCardDisplay(session, raidKey, "unit_test")
	packet := conn.write.Bytes()
	if len(packet) == 0 {
		t.Fatal("no packet was sent")
	}
	parsed, err := dnfproto.ParseChannelPacketUnchecked(packet)
	if err != nil {
		t.Fatalf("sent packet does not parse: %v", err)
	}
	if parsed.Header.MsgID != raid.LukeCardDisplayMsgID {
		t.Fatalf("msg_id=%d want %d", parsed.Header.MsgID, raid.LukeCardDisplayMsgID)
	}
	if parsed.Header.Classification != 0 {
		t.Fatalf("classification=%d want 0", parsed.Header.Classification)
	}
	want, ok, err := runtimeRaidLukeCardDisplayBody(
		lukeCardSetMapInfo, service.raidLukeCardSet, service.raidLukeMonsterList,
		runtimeRaidLukeTarotDay(service.gameplayNow()), true)
	if err != nil || !ok {
		t.Fatalf("expected body ok=%t err=%v", ok, err)
	}
	if !bytes.Equal(parsed.Body, want) {
		t.Fatalf("sent body=%x\nwant=%x", parsed.Body, want)
	}
	if parsed.Body[1] != 3 || parsed.Body[len(parsed.Body)-1] != 1 {
		t.Fatalf("N=%d open_window=%d want 3/1", parsed.Body[1], parsed.Body[len(parsed.Body)-1])
	}

	// 同一个团第二次：数据照发，但末字节必须是 0（不再弹窗）。
	conn.write.Reset()
	service.publishRuntimeRaidLukeCardDisplay(session, raidKey, "unit_test")
	second, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("second packet does not parse: %v", err)
	}
	if second.Header.MsgID != raid.LukeCardDisplayMsgID {
		t.Fatalf("second msg_id=%d want %d", second.Header.MsgID, raid.LukeCardDisplayMsgID)
	}
	if len(second.Body) != len(want) {
		t.Fatalf("second body_len=%d want %d", len(second.Body), len(want))
	}
	if second.Body[len(second.Body)-1] != 0 {
		t.Fatalf("second open_window=%d want 0", second.Body[len(second.Body)-1])
	}
	if !bytes.Equal(second.Body[:len(second.Body)-1], want[:len(want)-1]) {
		t.Fatal("the second packet must carry the same cards, only the flag differs")
	}

	// 新连接 = 新的一次团队频道入场 ⇒ 重新弹一次（账本随 session 一起是新的）。
	reconnectConn := &bufferConn{}
	reconnected := &gameSession{conn: reconnectConn, selectedCharacterID: session.selectedCharacterID}
	service.publishRuntimeRaidLukeCardDisplay(reconnected, raidKey, "unit_test_reconnect")
	reparsed, err := dnfproto.ParseChannelPacketUnchecked(reconnectConn.write.Bytes())
	if err != nil {
		t.Fatalf("reconnect packet does not parse: %v", err)
	}
	if reparsed.Body[len(reparsed.Body)-1] != 1 {
		t.Fatalf("reconnect open_window=%d want 1", reparsed.Body[len(reparsed.Body)-1])
	}

	// 换成另一个团（另一个 raidKey）⇒ **仍然是 0**：账本按天记，不按团记。
	// 一个角色只可能在一个团里，所以旧的团要先撤掉（否则 runtimeRaidForMember
	// 会在两个团之间按 map 顺序任选，测试就不再确定）。
	otherKey := raidKey + 1
	other := lukeCardDisplayTestState()
	other.RaidKey = otherKey
	other.Members = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	other.FrozenMembers = append([]runtimeRaidMemberState(nil), service.raids[raidKey].Members...)
	service.raids = map[uint32]*runtimeRaidState{otherKey: &other}
	conn.write.Reset()
	service.publishRuntimeRaidLukeCardDisplay(session, otherKey, "unit_test_other_raid")
	third, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("third packet does not parse: %v", err)
	}
	if third.Body[len(third.Body)-1] != 0 {
		t.Fatalf("a new raid in the same login session open_window=%d want 0 "+
			"(the ledger is per login session and tarot day, not per raid)",
			third.Body[len(third.Body)-1])
	}

	// 换一天 ⇒ 新的一天还没弹过，重新弹一次（tarot day 是账本的键）。
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service.gameplayTimers = queue
	queue.now = queue.now.Add(24 * time.Hour)
	conn.write.Reset()
	service.publishRuntimeRaidLukeCardDisplay(session, otherKey, "unit_test_next_day")
	nextDay, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("next-day packet does not parse: %v", err)
	}
	if nextDay.Body[len(nextDay.Body)-1] != 1 {
		t.Fatalf("a new tarot day open_window=%d want 1", nextDay.Body[len(nextDay.Body)-1])
	}
}

// TestPublishRuntimeRaidLukeCardDisplayStaysOffEveryOtherOwnerAndRaid covers the
// guards that still have to hold after the payload became raid-independent: an
// Anton raid, an Anton channel entry, a stale raid key, a raid key for a raid the
// character is not in, and an unloadable global config all send nothing.
//
// It deliberately does NOT list "no raid yet" or "single mode" any more: those are
// exactly what the official behaviour says must now send (see
// TestSingleModeLukeChannelTownEntrySendsCardDisplayWithoutAnyRaid).
func TestPublishRuntimeRaidLukeCardDisplayStaysOffEveryOtherOwnerAndRaid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(service *Service, session *gameSession, raidKey uint32) uint32
	}{
		{"anton raid", func(service *Service, _ *gameSession, raidKey uint32) uint32 {
			service.raids[raidKey].RaidMode = runtimeRaidOwnerAnton
			return raidKey
		}},
		{"stale raid key", func(_ *Service, _ *gameSession, raidKey uint32) uint32 { return raidKey + 1 }},
		{"member of no raid", func(service *Service, _ *gameSession, raidKey uint32) uint32 {
			service.raids = map[uint32]*runtimeRaidState{}
			return raidKey
		}},
		// raidKey = 0 is the raid-independent form: it only needs the Luke channel.
		{"channel entry outside a luke raid channel", func(_ *Service, session *gameSession, _ uint32) uint32 {
			session.channel.Type = dnfenum.RaidChannelType
			return 0
		}},
	}
	for _, testCase := range cases {
		service, session, conn, raidKey := lukeCardDisplayTestService(t)
		raidKey = testCase.mutate(service, session, raidKey)
		service.publishRuntimeRaidLukeCardDisplay(session, raidKey, "unit_test")
		if conn.write.Len() != 0 {
			t.Fatalf("%s: wrote %d bytes", testCase.name, conn.write.Len())
		}
	}

	// Each global table that could not be loaded is a skip, not a partial packet:
	// the fixture's preset caches are dropped so the loader's cached failure is
	// what the publisher sees. Fail closed on all three.
	for _, testCase := range []struct {
		name    string
		breakIt func(service *Service)
	}{
		{"luke.etc", func(service *Service) {
			service.raidLuke = nil
			service.raidLukeErr = raid.ErrLukePVFDocumentInvalid
		}},
		{"LukeCardSet.etc", func(service *Service) {
			service.raidLukeCardSet = nil
			service.raidLukeCardSetErr = raid.ErrLukeCardSetDocumentInvalid
		}},
		{"luke.etc.buff", func(service *Service) {
			service.raidLukeMonsterList = nil
			service.raidLukeMonsterListErr = raid.ErrLukeRaidMonsterListDocumentInvalid
		}},
	} {
		service, session, conn, raidKey := lukeCardDisplayTestService(t)
		testCase.breakIt(service)
		service.publishRuntimeRaidLukeCardDisplay(session, raidKey, "unit_test")
		if conn.write.Len() != 0 {
			t.Fatalf("an unloadable %s wrote %d bytes", testCase.name, conn.write.Len())
		}
		// The same broken table must not let the raid-independent entry through
		// either: fail closed means a Luke-channel entry with no raid sends nothing.
		independentConn := &bufferConn{}
		independent := &gameSession{
			conn:                independentConn,
			selectedCharacterID: session.selectedCharacterID,
			channel:             channelcatalog.Channel{ID: 28, Type: dnfenum.HiddenRaidType, Name: "luke", Port: 10028},
		}
		service.publishRuntimeRaidLukeCardDisplay(independent, 0, "unit_test_no_raid")
		if independentConn.write.Len() != 0 {
			t.Fatalf("an unloadable %s let the no-raid entry write %d bytes",
				testCase.name, independentConn.write.Len())
		}
	}
}

// lukeCardDisplayRealPathFixture builds a service whose town catalog comes from
// the shipped Script.pvf, a Luke team channel session, and (withRaid) one live
// Luke raid -- i.e. everything currentRaidTownTransition itself needs.
//
// withRaid = false is the official shape from 2026-09-29: the character is not in
// a raid at all, only standing in the Luke raid channel.
func lukeCardDisplayRealPathFixture(
	t *testing.T,
	withRaid bool,
) (*Service, *gameSession, *bufferConn, uint32, dnfrepo.CharacterRecord) {
	t.Helper()
	const (
		raidKey     = uint32(0x00070001)
		characterID = uint16(1001)
	)
	archive, err := platformpvf.Open(realLukeRewardPVFPath())
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	lukeConfig, err := raid.LoadLukeConfig(archive)
	if err != nil {
		t.Fatalf("load Luke raid config: %v", err)
	}
	channel := channelcatalog.Channel{ID: 28, Type: dnfenum.HiddenRaidType, Name: "luke", Port: 10028}
	service := &Service{
		options:                 options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		initialEquipmentArchive: archive,
		raids:                   map[uint32]*runtimeRaidState{},
		onlinePlayers:           newOnlinePlayerManager(),
	}
	if err := service.preloadTownCatalog(context.Background()); err != nil {
		t.Fatalf("load the real town catalog: %v", err)
	}
	if withRaid {
		state := runtimeRaidState{
			RaidKey:       raidKey,
			LeaderID:      characterID,
			RaidMode:      runtimeRaidOwnerLuke,
			Started:       true,
			EntryCostPaid: true,
			Members:       []runtimeRaidMemberState{{CharID: characterID, GroupIndex: 1, SlotOrder: 0}},
			FrozenMembers: []runtimeRaidMemberState{{CharID: characterID, GroupIndex: 1, SlotOrder: 0}},
			LukeConfig:    lukeConfig,
			Lifecycle: runtimeRaidLifecycleState{
				Status: runtimeRaidLifecyclePreparing,
				Phase:  runtimeRaidDungeonPhaseOne,
			},
		}
		service.raids[raidKey] = &state
	}
	conn := &bufferConn{}
	session := &gameSession{
		conn:                conn,
		connID:              "luke-card-display-real-path",
		channel:             channel,
		residentChannel:     channel,
		selectedCharacterID: characterID,
	}
	character := dnfrepo.CharacterRecord{
		CharacterID: "1001",
		AccountID:   "account-1",
		Name:        "luke-card-display",
		Level:       90,
		Stats:       map[string]int64{"town_id": 32, "area_id": 0, "pos_x": 474, "pos_y": 234, "direction": 5, "area_state": 0},
	}
	return service, session, conn, raidKey, character
}

// TestLukeRaidChannelTownEntrySendsCardDisplayOnTheRealPath drives the
// **production relogin/reconnect entry function** — currentRaidTownTransition,
// the sole emitter of game-raid-town-location-used, i.e. the line the live log
// shows at 2026-09-29T10:19:44Z / 10:19:47Z for the two characters that saw
// nothing — with a real town catalog from the shipped Script.pvf, and asserts a
// 1181 actually reaches the wire, byte-for-byte the builder's body.
func TestLukeRaidChannelTownEntrySendsCardDisplayOnTheRealPath(t *testing.T) {
	service, session, conn, _, character := lukeCardDisplayRealPathFixture(t, true)
	spec, ok := currentRaidTownSpecForSession(session)
	if !ok || spec.townID != currentLukeRaidTownID {
		t.Fatalf("the fixture session is not a Luke raid channel: %+v ok=%t", spec, ok)
	}
	updated, townID, areaID, _, mapPath, err := service.currentRaidTownTransition(
		session, session.selectedCharacterID, character, "unit_test_luke_channel_login", spec)
	if err != nil {
		t.Fatalf("currentRaidTownTransition error = %v", err)
	}
	if townID != byte(currentLukeRaidTownID) || int64(areaID) != currentLukeRaidAreaID {
		t.Fatalf("landed town=%d area=%d want %d/%d", townID, areaID, currentLukeRaidTownID, currentLukeRaidAreaID)
	}
	if mapPath == "" || updated.CharacterID != character.CharacterID {
		t.Fatalf("transition map=%q character=%q", mapPath, updated.CharacterID)
	}

	packet := conn.write.Bytes()
	if len(packet) == 0 {
		t.Fatal("the real relogin path sent no packet at all")
	}
	parsed, err := dnfproto.ParseChannelPacketUnchecked(packet)
	if err != nil {
		t.Fatalf("sent packet does not parse: %v", err)
	}
	if parsed.Header.MsgID != raid.LukeCardDisplayMsgID {
		t.Fatalf("msg_id=%d want %d", parsed.Header.MsgID, raid.LukeCardDisplayMsgID)
	}
	if len(parsed.Body) != 120 {
		t.Fatalf("body_len=%d want 120", len(parsed.Body))
	}
	cardSet, err := service.runtimeRaidLukeCardSetConfig()
	if err != nil {
		t.Fatalf("load the real card set: %v", err)
	}
	state, member := service.runtimeRaidForMember(session.selectedCharacterID)
	if !member || state.LukeConfig == nil {
		t.Fatal("the fixture member is not in a Luke raid")
	}
	raidDay := runtimeRaidLukeTarotDay(service.gameplayNow())
	monsters, err := service.runtimeRaidLukeRaidMonsterListConfig()
	if err != nil {
		t.Fatalf("load the real raid monster list: %v", err)
	}
	want, ok, err := runtimeRaidLukeCardDisplayBody(state.LukeConfig.CardSetMapInfo, cardSet, monsters, raidDay, true)
	if err != nil || !ok {
		t.Fatalf("expected body ok=%t err=%v", ok, err)
	}
	if !bytes.Equal(parsed.Body, want) {
		t.Fatalf("sent body=%x\nwant=%x", parsed.Body, want)
	}
	if int(parsed.Body[0]) != runtimeRaidLukeTarotCardSetRecord(raidDay, 5) {
		t.Fatalf("field 1=%d but the day's [CARD SET MAP INFO] record is %d",
			parsed.Body[0], runtimeRaidLukeTarotCardSetRecord(raidDay, 5))
	}
	if parsed.Body[1] != 3 || parsed.Body[len(parsed.Body)-1] != 1 {
		t.Fatalf("N=%d open_window=%d want 3/1", parsed.Body[1], parsed.Body[len(parsed.Body)-1])
	}
	t.Logf("relogin channel entry -> msg_id=%d body_len=%d card_set_index=%d packet_records=%d open_window=%d",
		parsed.Header.MsgID, len(parsed.Body), parsed.Body[0], parsed.Body[1], parsed.Body[len(parsed.Body)-1])

	// A second landing in the same session for the same raid refreshes only.
	conn.write.Reset()
	if _, _, _, _, _, err := service.currentRaidTownTransition(
		session, session.selectedCharacterID, character, "unit_test_luke_channel_reenter", spec); err != nil {
		t.Fatalf("second currentRaidTownTransition error = %v", err)
	}
	second, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("second packet does not parse: %v", err)
	}
	if second.Header.MsgID != raid.LukeCardDisplayMsgID || second.Body[len(second.Body)-1] != 0 {
		t.Fatalf("second landing msg_id=%d open_window=%d want %d/0",
			second.Header.MsgID, second.Body[len(second.Body)-1], raid.LukeCardDisplayMsgID)
	}
}

// TestLukeChannelReloginPopsOnlyOnceAcrossTheOp36PathToo is the cross-entry pin:
// the relogin path and the op36 waiting-room path share ONE ledger, so a member
// who relogs into an existing raid and then walks into the waiting room is not
// popped twice for the same raid.
func TestLukeChannelReloginPopsOnlyOnceAcrossTheOp36PathToo(t *testing.T) {
	service, session, conn, raidKey, character := lukeCardDisplayRealPathFixture(t, true)
	spec, _ := currentRaidTownSpecForSession(session)
	if _, _, _, _, _, err := service.currentRaidTownTransition(
		session, session.selectedCharacterID, character, "unit_test_luke_channel_login", spec); err != nil {
		t.Fatalf("relogin transition error = %v", err)
	}
	first, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("relogin packet does not parse: %v", err)
	}
	if first.Body[len(first.Body)-1] != 1 {
		t.Fatalf("relogin open_window=%d want 1", first.Body[len(first.Body)-1])
	}

	// The op36 waiting-room entry runs the same publish function with the key the
	// relogin already spent, so it must NOT pop again.
	conn.write.Reset()
	service.publishRuntimeRaidLukeCardDisplay(session, raidKey, "town_set_user_area_raid_waiting_room")
	waiting, err := dnfproto.ParseChannelPacketUnchecked(conn.write.Bytes())
	if err != nil {
		t.Fatalf("waiting-room packet does not parse: %v", err)
	}
	if waiting.Header.MsgID != raid.LukeCardDisplayMsgID {
		t.Fatalf("waiting-room msg_id=%d want %d", waiting.Header.MsgID, raid.LukeCardDisplayMsgID)
	}
	if waiting.Body[len(waiting.Body)-1] != 0 {
		t.Fatalf("waiting-room open_window=%d want 0 after the relogin already popped",
			waiting.Body[len(waiting.Body)-1])
	}
	if !bytes.Equal(waiting.Body[:len(waiting.Body)-1], first.Body[:len(first.Body)-1]) {
		t.Fatal("both entries must describe the same cards")
	}
}

// lukeChannelTownLanding drives the **production** entry -- currentRaidTownTransition,
// the sole emitter of game-raid-town-location-used -- for a Luke-channel session
// and returns the bytes it put on the wire (nil for "nothing went out").
func lukeChannelTownLanding(
	t *testing.T,
	service *Service,
	session *gameSession,
	character dnfrepo.CharacterRecord,
	source string,
) []byte {
	t.Helper()
	spec, ok := currentRaidTownSpecForSession(session)
	if !ok || spec.townID != currentLukeRaidTownID {
		t.Fatalf("the fixture session is not a Luke raid channel: %+v ok=%t", spec, ok)
	}
	if _, _, _, _, _, err := service.currentRaidTownTransition(
		session, session.selectedCharacterID, character, source, spec); err != nil {
		t.Fatalf("currentRaidTownTransition error = %v", err)
	}
	return session.conn.(*bufferConn).write.Bytes()
}

// TestLukeChannelTownEntryWithoutAnyRaidSendsTheCardDisplay is the official
// behaviour with the character in **no raid at all**:
//
//	「玩家在每次登陆后，第一次进入卢克 RAID 频道时，离开赛利亚旅馆会自动展示
//	  一次塔罗牌。」
//
// The payload is global PVF configuration, so the raid is irrelevant: the packet
// must go out, be 120 bytes, pop the window once, and be byte-for-byte the
// builder's body for the day's global config.
func TestLukeChannelTownEntryWithoutAnyRaidSendsTheCardDisplay(t *testing.T) {
	service, session, _, _, character := lukeCardDisplayRealPathFixture(t, false)
	if state, member := service.runtimeRaidForMember(session.selectedCharacterID); member {
		t.Fatalf("this test needs a character in no raid at all: %+v", state)
	}

	data := lukeChannelTownLanding(t, service, session, character, "unit_test_luke_channel_login_no_raid")
	if len(data) == 0 {
		t.Fatal("a raid-less Luke channel entry sent nothing")
	}
	parsed, err := dnfproto.ParseChannelPacketUnchecked(data)
	if err != nil {
		t.Fatalf("sent packet does not parse: %v", err)
	}
	if parsed.Header.MsgID != raid.LukeCardDisplayMsgID {
		t.Fatalf("msg_id=%d want %d", parsed.Header.MsgID, raid.LukeCardDisplayMsgID)
	}
	if len(parsed.Body) != 120 {
		t.Fatalf("body_len=%d want 120", len(parsed.Body))
	}
	if parsed.Body[len(parsed.Body)-1] != 1 {
		t.Fatalf("first landing open_window=%d want 1", parsed.Body[len(parsed.Body)-1])
	}

	// Byte-for-byte the global configuration's body: luke.etc + LukeCardSet.etc +
	// luke.etc.buff, none of which came from a raid.
	lukeConfig, err := service.runtimeRaidLukeConfig()
	if err != nil {
		t.Fatalf("load the global luke.etc: %v", err)
	}
	cardSet, err := service.runtimeRaidLukeCardSetConfig()
	if err != nil {
		t.Fatalf("load the real card set: %v", err)
	}
	monsters, err := service.runtimeRaidLukeRaidMonsterListConfig()
	if err != nil {
		t.Fatalf("load the real raid monster list: %v", err)
	}
	raidDay := runtimeRaidLukeTarotDay(service.gameplayNow())
	want, ok, err := runtimeRaidLukeCardDisplayBody(lukeConfig.CardSetMapInfo, cardSet, monsters, raidDay, true)
	if err != nil || !ok {
		t.Fatalf("expected body ok=%t err=%v", ok, err)
	}
	if !bytes.Equal(parsed.Body, want) {
		t.Fatalf("sent body=%x\nwant=%x", parsed.Body, want)
	}
	if int(parsed.Body[0]) != runtimeRaidLukeTarotCardSetRecord(raidDay, 5) {
		t.Fatalf("field 1=%d but the day's [CARD SET MAP INFO] record is %d",
			parsed.Body[0], runtimeRaidLukeTarotCardSetRecord(raidDay, 5))
	}
	t.Logf("no-raid landing -> msg_id=%d body_len=%d card_set_index=%d packet_records=%d open_window=%d",
		parsed.Header.MsgID, len(parsed.Body), parsed.Body[0], parsed.Body[1], parsed.Body[len(parsed.Body)-1])
}

// TestLukeChannelTownEntrySecondLandingRefreshesWithoutTheWindow pins the "每个
// 登录会话只弹一次" half: the same session landing in the Luke channel town again
// still gets the fresh cards, but with open_window=0.
func TestLukeChannelTownEntrySecondLandingRefreshesWithoutTheWindow(t *testing.T) {
	service, session, conn, _, character := lukeCardDisplayRealPathFixture(t, false)

	first := lukeChannelTownLanding(t, service, session, character, "unit_test_luke_channel_login_no_raid")
	if len(first) == 0 || first[len(first)-1] != 1 {
		t.Fatalf("the first landing did not pop: % X", first)
	}

	conn.write.Reset()
	second := lukeChannelTownLanding(t, service, session, character, "unit_test_luke_channel_reenter")
	if len(second) == 0 {
		t.Fatal("the second landing sent nothing (the cards must stay fresh)")
	}
	if len(second) != len(first) {
		t.Fatalf("second body_len=%d want %d", len(second), len(first))
	}
	if second[len(second)-1] != 0 {
		t.Fatalf("second landing open_window=%d want 0", second[len(second)-1])
	}
	if !bytes.Equal(second[:len(second)-1], first[:len(first)-1]) {
		t.Fatal("both landings must describe the same day's cards")
	}
}

// TestLukeChannelTownEntryAfterReloginPopsAgain pins "每次登陆后": a new session is
// a new login, so its ledger is empty and the window pops again.
func TestLukeChannelTownEntryAfterReloginPopsAgain(t *testing.T) {
	service, session, _, _, character := lukeCardDisplayRealPathFixture(t, false)

	first := lukeChannelTownLanding(t, service, session, character, "unit_test_luke_channel_login_no_raid")
	if len(first) == 0 || first[len(first)-1] != 1 {
		t.Fatalf("the first login did not pop: % X", first)
	}
	// Same login, second landing: no window.
	if again := lukeChannelTownLanding(t, service, session, character, "unit_test_luke_channel_reenter"); again[len(again)-1] != 0 {
		t.Fatalf("the same session popped twice: open_window=%d", again[len(again)-1])
	}

	// 重新登录 = 新的 gameSession（新连接、新账本）。
	reloginConn := &bufferConn{}
	relogin := &gameSession{
		conn:                reloginConn,
		connID:              "luke-card-display-real-path-relogin",
		channel:             session.channel,
		residentChannel:     session.residentChannel,
		selectedCharacterID: session.selectedCharacterID,
	}
	after := lukeChannelTownLanding(t, service, relogin, character, "unit_test_luke_channel_relogin")
	if len(after) == 0 {
		t.Fatal("the relogin landing sent nothing")
	}
	if after[len(after)-1] != 1 {
		t.Fatalf("a new login session open_window=%d want 1", after[len(after)-1])
	}
	if !bytes.Equal(after[:len(after)-1], first[:len(first)-1]) {
		t.Fatal("both logins must describe the same day's cards")
	}
}

// TestAntonRaidChannelTownEntrySendsNoCardDisplay is the 安徒恩零改动 half of the
// same real path: the identical function, an identical transition, but town 19.
// The character is in no raid at all, so nothing but the town id can be holding
// the packet back.
func TestAntonRaidChannelTownEntrySendsNoCardDisplay(t *testing.T) {
	service, session, conn, _, character := lukeCardDisplayRealPathFixture(t, false)
	session.channel = channelcatalog.Channel{ID: 23, Type: dnfenum.RaidChannelType, Name: "anton", Port: 10023}
	session.residentChannel = session.channel
	spec := runtimeRaidTownSpec{
		townID: currentAntonRaidTownID, areaID: currentAntonRaidAreaID,
		waitingArea: currentAntonRaidWaitingAreaID, ownerName: "anton",
	}
	if _, _, _, _, _, err := service.currentRaidTownTransition(
		session, session.selectedCharacterID, character, "unit_test_anton_channel_login", spec); err != nil {
		t.Fatalf("currentRaidTownTransition error = %v", err)
	}
	if conn.write.Len() != 0 {
		t.Fatalf("an Anton channel entry wrote %d bytes", conn.write.Len())
	}
}

// TestLukeCardDisplayBodyDescribesThreeSanctumsWithSevenSlots checks the shape
// the runtime sends: N=3, K=7 per record, field B inside that sanctum's own
// patterns, field P one of the 22 arcana, and the trailing open-window byte set.
func TestLukeCardDisplayBodyDescribesThreeSanctumsWithSevenSlots(t *testing.T) {
	state := lukeCardDisplayTestState()
	config := lukeCardDisplayTestConfig()
	body, ok, err := runtimeRaidLukeCardDisplayBody(state.LukeConfig.CardSetMapInfo, config, lukeCardDisplayTestMonsterList(), 20260929, true)
	if err != nil || !ok {
		t.Fatalf("body ok=%t err=%v", ok, err)
	}
	wantSize := 1 + 1 + len(runtimeRaidLukeTarotSanctums)*(2+1+1+lukeCardSlotsPerPattern*5) + 1
	if len(body) != wantSize {
		t.Fatalf("body_len=%d want=%d", len(body), wantSize)
	}
	if int(body[0]) >= runtimeRaidLukeCardSetRecordCount(state.LukeConfig.CardSetMapInfo) {
		t.Fatalf("cardSetIndex=%d above the record count", body[0])
	}
	if int(body[1]) != 3 {
		t.Fatalf("N=%d want 3", body[1])
	}
	offset := 2
	for index, sanctum := range runtimeRaidLukeTarotSanctums {
		if got := binary.LittleEndian.Uint16(body[offset : offset+2]); uint32(got) != sanctum {
			t.Fatalf("record %d A=%d want %d", index, got, sanctum)
		}
		offset += 2
		pattern := int(body[offset])
		if !containsRaidPattern(config.PatternIndices(sanctum), pattern) {
			t.Fatalf("record %d B=%d is not one of sanctum %d's patterns %v",
				index, pattern, sanctum, config.PatternIndices(sanctum))
		}
		offset++
		if int(body[offset]) != lukeCardSlotsPerPattern {
			t.Fatalf("record %d K=%d want %d", index, body[offset], lukeCardSlotsPerPattern)
		}
		offset++
		for slot := 0; slot < lukeCardSlotsPerPattern; slot++ {
			cardID := binary.LittleEndian.Uint32(body[offset : offset+4])
			// A slot the day's monsters cannot fill is the empty sentinel, which
			// the client draws nothing for; every other slot is a real arcana.
			if cardID != lukeCardEmptySlot && !containsRaidCard(lukeCardDisplayTestArcana, cardID) {
				t.Fatalf("record %d slot %d P=%d is neither empty nor a [CARD] key", index, slot, cardID)
			}
			offset += 4
			if body[offset] != 0 {
				t.Fatalf("record %d slot %d Q=%d want 0", index, slot, body[offset])
			}
			offset++
		}
	}
	if offset != len(body)-1 {
		t.Fatalf("open window offset=%d want=%d", offset, len(body)-1)
	}
	if body[len(body)-1] != 1 {
		t.Fatalf("openWindowFlag=%d want 1", body[len(body)-1])
	}

	// The whole body is a pure function of (day, config, [CARD SET MAP INFO]) and
	// the open-window flag.
	again, ok, err := runtimeRaidLukeCardDisplayBody(state.LukeConfig.CardSetMapInfo, config, lukeCardDisplayTestMonsterList(), 20260929, true)
	if err != nil || !ok || !bytes.Equal(body, again) {
		t.Fatalf("the same day produced a different body (ok=%t err=%v)", ok, err)
	}
	other, ok, err := runtimeRaidLukeCardDisplayBody(state.LukeConfig.CardSetMapInfo, config, lukeCardDisplayTestMonsterList(), 20261031, true)
	if err != nil || !ok {
		t.Fatalf("another day ok=%t err=%v", ok, err)
	}
	if bytes.Equal(body, other) {
		t.Fatal("two different tarot days produced byte-identical bodies")
	}
	quiet, ok, err := runtimeRaidLukeCardDisplayBody(state.LukeConfig.CardSetMapInfo, config, lukeCardDisplayTestMonsterList(), 20260929, false)
	if err != nil || !ok {
		t.Fatalf("openWindow=false ok=%t err=%v", ok, err)
	}
	if quiet[len(quiet)-1] != 0 || !bytes.Equal(quiet[:len(quiet)-1], body[:len(body)-1]) {
		t.Fatal("openWindow=false must change only the trailing byte")
	}
}

// TestLukeCardDisplayBodyFailsClosed keeps the "never send a half-described
// packet" rule: a missing [CARD SET MAP INFO], a shorter arcana table, or a
// sanctum with no `[PATTERN]` block all produce ok=false and no body at all.
func TestLukeCardDisplayBodyFailsClosed(t *testing.T) {
	config := lukeCardDisplayTestConfig()

	if body, ok, _ := runtimeRaidLukeCardDisplayBody(nil, config, lukeCardDisplayTestMonsterList(), 20260929, true); ok || body != nil {
		t.Fatalf("empty [CARD SET MAP INFO] produced ok=%t body_len=%d", ok, len(body))
	}
	if body, ok, _ := runtimeRaidLukeCardDisplayBody([]int{}, config, lukeCardDisplayTestMonsterList(), 20260929, true); ok || body != nil {
		t.Fatalf("a zero-value [CARD SET MAP INFO] produced ok=%t body_len=%d", ok, len(body))
	}
	if body, ok, _ := runtimeRaidLukeCardDisplayBody(lukeCardSetMapInfo, nil, lukeCardDisplayTestMonsterList(), 20260929, true); ok || body != nil {
		t.Fatalf("no card set config produced ok=%t body_len=%d", ok, len(body))
	}

	shortCards := &raid.LukeCardSetConfig{
		Patterns: config.Patterns,
		Cards:    config.Cards[:6],
	}
	if body, ok, _ := runtimeRaidLukeCardDisplayBody(lukeCardSetMapInfo, shortCards, lukeCardDisplayTestMonsterList(), 20260929, true); ok || body != nil {
		t.Fatalf("a six-card table produced ok=%t body_len=%d", ok, len(body))
	}

	missingSanctum := &raid.LukeCardSetConfig{Cards: config.Cards}
	for _, pattern := range config.Patterns {
		if pattern.DungeonID == lukeSpreadSanctumMapID {
			continue
		}
		missingSanctum.Patterns = append(missingSanctum.Patterns, pattern)
	}
	if body, ok, _ := runtimeRaidLukeCardDisplayBody(lukeCardSetMapInfo, missingSanctum, lukeCardDisplayTestMonsterList(), 20260929, true); ok || body != nil {
		t.Fatalf("a table without 5056 produced ok=%t body_len=%d", ok, len(body))
	}
}

// TestLukeCardDisplayBodyBuildsFromTheRealPVF is the end-to-end pin: the
// shipped Script.pvf's luke.etc, LukeCardSet.etc and luke.etc.buff must together
// describe today's cards with N=3, K=7, every named card a real `[CARD]` key,
// and every card one of the monsters that sanctum's three mazes really spawn.
func TestLukeCardDisplayBodyBuildsFromTheRealPVF(t *testing.T) {
	pvfPath := realLukeRewardPVFPath()
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	lukeConfig, err := raid.LoadLukeConfig(archive)
	if err != nil {
		t.Fatalf("load Luke raid config: %v", err)
	}
	cardSet, err := raid.LoadLukeCardSet(archive)
	if err != nil {
		t.Fatalf("load Luke card set: %v", err)
	}
	monsters, err := raid.LoadLukeRaidMonsterList(archive)
	if err != nil {
		t.Fatalf("load Luke raid monster list: %v", err)
	}
	raidDay := runtimeRaidLukeTarotDay(time.Now())
	body, ok, err := runtimeRaidLukeCardDisplayBody(lukeConfig.CardSetMapInfo, cardSet, monsters, raidDay, true)
	if err != nil || !ok {
		t.Fatalf("real PVF body ok=%t err=%v", ok, err)
	}
	if len(body) != 1+1+3*(2+1+1+7*5)+1 {
		t.Fatalf("real body_len=%d want 120", len(body))
	}
	t.Logf("pvf=%s tarot_day=%d card_set_index=%d card_set_records=%d packet_records=%d body_len=%d",
		pvfPath, raidDay, body[0], runtimeRaidLukeCardSetRecordCount(lukeConfig.CardSetMapInfo), body[1], len(body))
	offset := 2
	seenCards := make([]map[uint32]bool, 0, 3)
	for record := 0; record < 3; record++ {
		sanctum := binary.LittleEndian.Uint16(body[offset : offset+2])
		patternIndex := int(body[offset+2])
		slots := int(body[offset+3])
		offset += 4
		cards := make([]uint32, 0, slots)
		named := make([]uint32, 0, slots)
		for slot := 0; slot < slots; slot++ {
			cardID := binary.LittleEndian.Uint32(body[offset : offset+4])
			offset += 5
			// A slot the day cannot fill must be the empty sentinel, which the
			// client silently draws nothing for; every other slot must be a real
			// `[CARD]` key, never a made-up one.
			if cardID == lukeCardEmptySlot {
				cards = append(cards, cardID)
				continue
			}
			if !cardSet.HasCard(cardID) {
				t.Fatalf("record %d slot %d P=%d is neither empty nor a [CARD] key", record, slot, cardID)
			}
			cards = append(cards, cardID)
			named = append(named, cardID)
		}
		// The five named cards must be exactly this sanctum's own three mazes on
		// the day's group, in maze order and in-maze order, capped at seven. The
		// shipped table yields five per sanctum except where a group's monster has
		// no `[CARD]` row (5055 ships one such monster), which is why the count is
		// compared rather than assumed.
		mazeCount, ok := monsters.MazeCount(uint32(sanctum))
		if !ok {
			t.Fatalf("record %d sanctum %d is not in [RAID MONSTER LIST]", record, sanctum)
		}
		groupStart, ok := runtimeRaidLukeTarotGroupStart(
			lukeConfig.CardSetMapInfo, int(body[0]), int(sanctum), mazeCount, raidDay)
		if !ok {
			t.Fatalf("record %d sanctum %d has no group for day %d", record, sanctum, raidDay)
		}
		want := make([]uint32, 0, 7)
		for maze := groupStart; maze < groupStart+lukeSanctumClearsPerGroup; maze++ {
			spawned, ok := monsters.MazeMonsters(uint32(sanctum), maze)
			if !ok {
				t.Fatalf("record %d sanctum %d maze %d missing", record, sanctum, maze)
			}
			for _, monsterID := range spawned {
				if cardSet.HasCard(monsterID) {
					want = append(want, monsterID)
				}
			}
		}
		if !reflect.DeepEqual(named, want) {
			t.Fatalf("record %d sanctum %d cards=%v want its own mazes %d..%d = %v",
				record, sanctum, named, groupStart, groupStart+lukeSanctumClearsPerGroup-1, want)
		}
		if len(named) < 4 || len(named) > 5 {
			t.Fatalf("record %d sanctum %d filled %d slots from mazes %d..%d, want 4..5 of the day's monsters",
				record, sanctum, len(named), groupStart, groupStart+lukeSanctumClearsPerGroup-1)
		}
		for slot := len(named); slot < lukeCardSlotsPerPattern; slot++ {
			if cards[slot] != lukeCardEmptySlot {
				t.Fatalf("record %d slot %d=%d want the empty sentinel", record, slot, cards[slot])
			}
		}
		seen := make(map[uint32]bool, len(named))
		for _, cardID := range named {
			seen[cardID] = true
		}
		seenCards = append(seenCards, seen)
		pattern, found := cardSet.Pattern(uint32(sanctum), patternIndex)
		if !found {
			t.Fatalf("record %d (%d,%d) is not a [PATTERN] key", record, sanctum, patternIndex)
		}
		if len(pattern.CardPositions) != slots {
			t.Fatalf("record %d (%d,%d) slots=%d [CARD POS]=%d",
				record, sanctum, patternIndex, slots, len(pattern.CardPositions))
		}
		t.Logf("record %d sanctum=%d pattern=%d slots=%d tooltip=%q cards=%v",
			record, sanctum, patternIndex, slots, pattern.Tooltip, cards)
	}
	if offset != len(body)-1 || body[len(body)-1] != 1 {
		t.Fatalf("tail offset=%d len=%d flag=%d", offset, len(body), body[len(body)-1])
	}
	// Three sanctums, three different faces: each one reads its own mazes.
	for left := 0; left < len(seenCards); left++ {
		for right := left + 1; right < len(seenCards); right++ {
			shared := false
			for cardID := range seenCards[left] {
				shared = shared || seenCards[right][cardID]
			}
			if shared {
				t.Fatalf("sanctum records %d and %d share an arcanum: %v / %v",
					left, right, seenCards[left], seenCards[right])
			}
		}
	}
}

// TestLukeTarotCardsComeFromEachSanctumsOwnMazes pins which mazes feed the seven
// slots: the day's group of the sanctum's own dungeon, in maze order and then in
// the document's in-maze order, with the slots the day cannot fill left empty. The
// expectation is walked straight off the fixture (maze order -> spawn order ->
// `[CARD]` membership, first seven), not off the derivation.
func TestLukeTarotCardsComeFromEachSanctumsOwnMazes(t *testing.T) {
	cardSet := lukeCardDisplayTestConfig()
	monsters := lukeCardDisplayTestMonsterList()
	const raidDay = 20260929
	record := runtimeRaidLukeTarotCardSetRecord(raidDay, runtimeRaidLukeCardSetRecordCount(lukeCardSetMapInfo))
	for _, sanctum := range runtimeRaidLukeTarotSanctums {
		mazeCount, ok := monsters.MazeCount(sanctum)
		if !ok {
			t.Fatalf("sanctum %d is not in the monster list", sanctum)
		}
		groupStart, ok := runtimeRaidLukeTarotGroupStart(lukeCardSetMapInfo, record, int(sanctum), mazeCount, raidDay)
		if !ok {
			t.Fatalf("sanctum %d has no group on day %d", sanctum, raidDay)
		}
		want := make([]uint32, 0, lukeCardSlotsPerPattern)
		for maze := groupStart; maze < groupStart+lukeSanctumClearsPerGroup; maze++ {
			spawned, found := monsters.MazeMonsters(sanctum, maze)
			if !found {
				t.Fatalf("sanctum %d maze %d missing", sanctum, maze)
			}
			for _, monsterID := range spawned {
				if len(want) >= lukeCardSlotsPerPattern {
					break
				}
				if !cardSet.HasCard(monsterID) {
					continue
				}
				duplicate := false
				for _, cardID := range want {
					duplicate = duplicate || cardID == monsterID
				}
				if !duplicate {
					want = append(want, monsterID)
				}
			}
		}
		wantEmpty := lukeCardSlotsPerPattern - len(want)
		for len(want) < lukeCardSlotsPerPattern {
			want = append(want, lukeCardEmptySlot)
		}
		cards := runtimeRaidLukeTarotCardsForSanctum(
			sanctum, lukeCardSetMapInfo, record, raidDay, monsters, cardSet, lukeCardSlotsPerPattern)
		if !reflect.DeepEqual(cards, want) {
			t.Fatalf("sanctum %d cards=%v want its own mazes %d..%d = %v",
				sanctum, cards, groupStart, groupStart+lukeSanctumClearsPerGroup-1, want)
		}
		// The shipped shape leaves exactly two slots empty: 1 + 2 + 2 monsters per
		// group, and only one of the three mazes can differ from the others.
		if wantEmpty != 2 {
			t.Fatalf("sanctum %d filled %d slots, want 5 named + 2 empty",
				sanctum, lukeCardSlotsPerPattern-wantEmpty)
		}
	}

	// Three sanctums, three different faces: each one reads its own mazes.
	cards := make([][]uint32, 0, len(runtimeRaidLukeTarotSanctums))
	for _, sanctum := range runtimeRaidLukeTarotSanctums {
		cards = append(cards, runtimeRaidLukeTarotCardsForSanctum(
			sanctum, lukeCardSetMapInfo, record, raidDay, monsters, cardSet, lukeCardSlotsPerPattern))
	}
	for left := 0; left < len(cards); left++ {
		for right := left + 1; right < len(cards); right++ {
			for _, cardID := range cards[left] {
				if cardID == lukeCardEmptySlot {
					continue
				}
				for _, peer := range cards[right] {
					if peer == cardID {
						t.Fatalf("sanctums %d and %d share arcanum %d: %v / %v",
							runtimeRaidLukeTarotSanctums[left], runtimeRaidLukeTarotSanctums[right],
							cardID, cards[left], cards[right])
					}
				}
			}
		}
	}

	// A different group of the same dungeon produces different faces: the group
	// start really is what selects the monsters.
	groupStart, ok := runtimeRaidLukeTarotGroupStart(lukeCardSetMapInfo, record, int(lukeBirthSanctumMapID), 15, raidDay)
	if !ok {
		t.Fatal("no group for the birth sanctum")
	}
	moved := runtimeRaidLukeTarotCardsFromMazeMonsters(
		lukeBirthSanctumMapID, groupStart+lukeSanctumClearsPerGroup, monsters, cardSet, lukeCardSlotsPerPattern)
	if moved == nil || reflect.DeepEqual(cards[0], moved) {
		t.Fatalf("group %d and group %d produced the same faces: %v",
			groupStart, groupStart+lukeSanctumClearsPerGroup, moved)
	}
}

// TestLukeTarotCardsFailClosedWithoutAnyArcanum keeps the "never send a
// half-described packet" rule for the derivation: a day whose group holds no
// `[CARD]` monster at all describes nothing, and the caller must not send.
func TestLukeTarotCardsFailClosedWithoutAnyArcanum(t *testing.T) {
	cardSet := lukeCardDisplayTestConfig()
	config := lukeCardDisplayTestMonsterList()
	// Replace the whole of 5054's groups with monsters that are not `[CARD]` keys.
	for index := range config.Dungeons {
		if config.Dungeons[index].DungeonID != lukeBirthSanctumMapID {
			continue
		}
		for maze := range config.Dungeons[index].Mazes {
			config.Dungeons[index].Mazes[maze].MonsterIDs = []uint32{123456, 654321}
		}
	}
	for _, sanctum := range runtimeRaidLukeTarotSanctums {
		cards := runtimeRaidLukeTarotCardsForSanctum(
			sanctum, lukeCardSetMapInfo, 2, 20260929, config, cardSet, lukeCardSlotsPerPattern)
		if sanctum == lukeBirthSanctumMapID {
			if cards != nil {
				t.Fatalf("sanctum %d derived %v from monsters that hold no arcanum", sanctum, cards)
			}
			continue
		}
		if len(cards) != lukeCardSlotsPerPattern {
			t.Fatalf("sanctum %d cards=%v want %d slots", sanctum, cards, lukeCardSlotsPerPattern)
		}
	}
	if body, ok, err := runtimeRaidLukeCardDisplayBody(
		lukeCardSetMapInfo, cardSet, config, 20260929, true); ok || body != nil || err != nil {
		t.Fatalf("a day with no arcanum produced ok=%t body_len=%d err=%v", ok, len(body), err)
	}
	// No monster table at all is the same answer, not an arbitrary face.
	if body, ok, _ := runtimeRaidLukeCardDisplayBody(
		lukeCardSetMapInfo, cardSet, nil, 20260929, true); ok || body != nil {
		t.Fatalf("a missing [RAID MONSTER LIST] produced ok=%t body_len=%d", ok, len(body))
	}
}
