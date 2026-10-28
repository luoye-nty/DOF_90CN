package dnfbridge

// stress_dungeon_entry_test.go stress-tests simultaneous party dungeon entry.
//
// TestPartyLeaderDungeonSelectionEntersEveryOnlineMemberWithStablePartyIndex is
// the behavioural model for one 2-member party. The tests here run the SAME
// production flow (op16 -> handleDungeonSelectUpper -> shared
// runtimePartyDungeonOwner -> op37 loading barrier -> follower-first/leader-last
// release fan-out in game_select_scene_request_handlers.go) for larger member
// counts, and measure what that fan-out actually costs.
//
// What is deliberately NOT faked: every packet asserted on is produced by the
// production handlers, every release goes through completeRuntimePartyDungeonLoading
// and fanOutToGameSessions, and no production symbol is stubbed. The only test
// doubles are the connection (bufferConn + the shared ordered write log + an
// optional per-write latency that models socket cost) and the in-memory
// repository group, exactly like the 2-member model test.
//
// TestStressPartyDungeonEntryPartySizeParameterization measures the production
// ceiling on ONE party roster. TestStressPartyDungeonEntryConcurrentSquads runs
// the aggregate shapes the game actually uses for 40 and 1000 players
// (runtimeRaidMaxGroups squads of runtimeRaidGroupSize members).

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	stressDungeonID = 700

	stressTownID byte = 38
	stressAreaID byte = 1

	// stressBaseChannelID keeps every stress squad inside a normal channel
	// catalogue range. Squads that must share a town lobby reuse one channel.
	stressBaseChannelID = 42

	// stressPartyLimit is the synthetic dungeon's [limit party count]. One static
	// table backs every N below, so it is raised once to the game's legal maximum
	// (runtimeRaidMaxGroups * runtimeRaidGroupSize = 40) and built once.
	stressPartyLimit = 40

	// stressReleaseWriteLatency stands in for the per-write socket cost that made
	// the live party entry slow (5.95s for a single member, 2026-09-25 00:27).
	// It is applied ONLY around the loading-release phase, so entry stays cheap,
	// and it turns "these writes overlapped" into a measurable claim: a serial
	// implementation has to pay latency x writes, a concurrent one pays about one
	// wave of it.
	stressReleaseWriteLatency = 250 * time.Microsecond

	// stressReleaseWaveCeiling bounds a party release by whole member-equivalents.
	// The production release is three sequential waves (the trigger releases
	// itself, then every follower at once, then the leader alone), so a party's
	// wall time is a small constant multiple of ONE member's release -- never the
	// sum over its members. 4 leaves room for the fan-out's own overhead.
	stressReleaseWaveCeiling = 5
)

// stressDungeonPVFSource is bridgeDungeonPVF with a 40-member party limit and a
// dungeon admission item every member must own and spend.
func stressDungeonPVFSource() bridgePVFSource {
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = strings.Replace(
		source["dungeon/test.dgn"],
		"[limit party count]\n1\n",
		fmt.Sprintf("[limit party count]\n%d\n", stressPartyLimit),
		1,
	)
	source["dungeon/test.dgn"] += "[required item]\n4726 1 0\n[/required item]\n"
	return source
}

// stressWriteLog is the existing orderedPartyWriteLog plus a timestamp per write.
//
// The 2-member model test reuses orderedPartyConn/orderedPartyWriteLog for
// cross-member ORDER. Measuring the fan-out needs the same thing plus time, and
// the timestamps must stay index-aligned with log.writes, so they are appended
// inside the log's own critical section.
type stressWriteLog struct {
	*orderedPartyWriteLog
	times []time.Time
}

// stressWireConn is orderedPartyConn plus (a) a mutex around the byte buffer and
// (b) a test-controlled per-write latency.
//
// The buffer mutex matters here and not in the 2-member model test: a town-area
// departure broadcast writes into PEER connections from the departing member's
// goroutine, so one bufferConn can be written by two goroutines at once. The log
// mutex is released before the (possibly slow) buffer write so the fan-out is
// not accidentally serialized by the instrumentation itself.
type stressWireConn struct {
	*bufferConn
	name  string
	log   *stressWriteLog
	mu    sync.Mutex
	delay atomic.Int64 // nanoseconds
}

func (connection *stressWireConn) Write(data []byte) (int, error) {
	connection.log.mu.Lock()
	connection.log.writes = append(connection.log.writes, orderedPartyWrite{
		connection: connection.name,
		data:       append([]byte(nil), data...),
	})
	connection.log.times = append(connection.log.times, time.Now())
	connection.log.mu.Unlock()
	if delay := time.Duration(connection.delay.Load()); delay > 0 {
		time.Sleep(delay)
	}
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.bufferConn.Write(data)
}

// stream returns a stable copy of everything written so far.
func (connection *stressWireConn) stream() []byte {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return append([]byte(nil), connection.bufferConn.write.Bytes()...)
}

type stressPartyMember struct {
	slot        int
	characterID uint16
	accountID   string
	id          string
	name        string
	session     *gameSession
	conn        *stressWireConn

	// entered is set once the production flow proved the member holds a shared
	// party dungeon runtime.
	entered bool
	// finishLoadingCall is how long this member's own op37 call took. The trigger
	// is the one whose call carries the whole fan-out.
	finishLoadingCall time.Duration
}

// stressPartyFixture is one party (one runtimePartyDungeonOwner roster) built the
// same way the 2-member model test builds its two members.
type stressPartyFixture struct {
	t       *testing.T
	service *Service
	repos   dnfrepo.Group
	channel channelcatalog.Channel
	log     *stressWriteLog
	partyID int
	members []stressPartyMember
}

func stressNewService(
	t *testing.T,
	repositories dnfrepo.Group,
	table *worldmap.Table,
	resolver *worldmap.Resolver,
	monsters *pvfDungeonMonsterCatalog,
) *Service {
	t.Helper()
	return &Service{
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
}

// buildStressPartyFixture performs the full per-member setup of the model test
// for `memberCount` sessions of one party. Member 0 is the leader and the party
// owner's leader slot; every member stands in the same town area of `channel`.
func buildStressPartyFixture(
	t *testing.T,
	service *Service,
	repositories dnfrepo.Group,
	partyID int,
	baseCharacterID uint16,
	memberCount int,
	channel channelcatalog.Channel,
) *stressPartyFixture {
	t.Helper()
	fixture := &stressPartyFixture{
		t:       t,
		service: service,
		repos:   repositories,
		channel: channel,
		log:     &stressWriteLog{orderedPartyWriteLog: &orderedPartyWriteLog{}},
		partyID: partyID,
		members: make([]stressPartyMember, 0, memberCount),
	}
	states := make([]alignedcmd.PartyMemberState, 0, memberCount)
	for index := 0; index < memberCount; index++ {
		characterID := baseCharacterID + uint16(index)
		id := strconv.Itoa(int(characterID))
		accountID := "stress-account-" + id
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: id,
			AccountID:   accountID,
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": int64(stressTownID), "area_id": int64(stressAreaID),
				"pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		// One worn equipment row (the actor projection reads it) plus the dungeon
		// admission item the synthetic dungeon requires.
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

		name := fmt.Sprintf("p%d-m%d", partyID, index)
		conn := &bufferConn{}
		wire := &stressWireConn{bufferConn: conn, name: name, log: fixture.log}
		session := &gameSession{
			conn:                wire,
			connID:              name,
			accountID:           accountID,
			channel:             channel,
			residentChannel:     channel,
			selectedCharacterID: characterID,
		}
		service.bindGameSessionCharacter(session, characterID)
		seedPartyOwnerActorProjectionForTest(t, service, session, characterID, byte(channel.ID))
		registerPartyPeerEndpointForTest(session, byte(index+1), uint16(50000+index))
		enableTownMoveSkillProjection(t, service, repositories, id)

		fixture.members = append(fixture.members, stressPartyMember{
			slot:        index,
			characterID: characterID,
			accountID:   accountID,
			id:          id,
			name:        name,
			session:     session,
			conn:        wire,
		})
		states = append(states, alignedcmd.PartyMemberState{
			UserID:    characterID,
			Slot:      byte(index),
			UserState: 1,
			HPPercent: 100,
			MPPercent: 100,
		})
	}
	state := alignedcmd.PartyState{
		PartyID: partyID,
		UserID:  baseCharacterID, // the leader owns the party
		Members: states,
	}
	for index := range fixture.members {
		member := &fixture.members[index]
		storeRuntimePartyState(member.session, state)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: member.characterID,
			AccountID:   member.accountID,
			Level:       20,
			ChannelID:   uint16(channel.ID),
			TownID:      stressTownID,
			AreaID:      stressAreaID,
			PositionX:   450,
			PositionY:   234,
			Direction:   5,
			AreaState:   3,
			Session:     member.session,
		})
	}
	for index := range fixture.members {
		member := &fixture.members[index]
		bindDungeonSelectorOriginForTestAt(t, service, member.session, stressTownID, stressAreaID, 450, 234)
	}
	return fixture
}

func (fixture *stressPartyFixture) leader() *stressPartyMember {
	return &fixture.members[0]
}

// enter runs the one op16 request that carries the whole party, exactly like the
// model test: only the leader asks, every other member is driven by the server.
func (fixture *stressPartyFixture) enter() error {
	body := make([]byte, dungeoncmd.SelectDungeonRequestSize)
	binary.LittleEndian.PutUint32(body[:4], stressDungeonID)
	return fixture.service.handleDungeonSelectUpper(fixture.leader().session, body)
}

// markEntered resolves which members actually hold a shared party dungeon
// runtime, which is the production evidence that they were admitted.
func (fixture *stressPartyFixture) markEntered() []int {
	entered := make([]int, 0, len(fixture.members))
	for index := range fixture.members {
		session := fixture.members[index].session
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		admitted := runtime != nil && runtime.partyDungeonOwner != nil
		session.dungeon.mu.Unlock()
		fixture.members[index].entered = admitted
		if admitted {
			entered = append(entered, index)
		}
	}
	return entered
}

func (fixture *stressPartyFixture) setWriteLatency(latency time.Duration) {
	for index := range fixture.members {
		fixture.members[index].conn.delay.Store(int64(latency))
	}
}

type stressLogSnapshot struct {
	writes []orderedPartyWrite
	times  []time.Time
}

// snapshotLog is goroutine-safe and must be used instead of releaseChains when
// the caller is not the test goroutine: releaseChains parses packets through the
// shared test helpers, which fail the test, and only the test goroutine may do
// that.
func (fixture *stressPartyFixture) snapshotLog() stressLogSnapshot {
	fixture.log.mu.Lock()
	defer fixture.log.mu.Unlock()
	return stressLogSnapshot{
		writes: append([]orderedPartyWrite(nil), fixture.log.writes...),
		times:  append([]time.Time(nil), fixture.log.times...),
	}
}

func (fixture *stressPartyFixture) resetWriteLog() {
	fixture.log.mu.Lock()
	fixture.log.writes = nil
	fixture.log.times = nil
	fixture.log.mu.Unlock()
}

// release drives the shared op37 loading barrier. Every admitted member asks to
// finish loading, the last registration releases the party, and the production
// handler releases followers first and the leader last. The leader asks first so
// that a follower is the trigger, which is the shape the model test asserts.
func (fixture *stressPartyFixture) release() (time.Duration, error) {
	request := make([]byte, currentFinishLoadingLegacyRequestBodySize)
	started := time.Now()
	for index := range fixture.members {
		member := &fixture.members[index]
		if !member.entered {
			continue
		}
		callStarted := time.Now()
		err := fixture.service.sendFinishLoadingStatus(member.session, request)
		member.finishLoadingCall = time.Since(callStarted)
		if err != nil {
			return time.Since(started), fmt.Errorf("party %d member %s finish loading: %w",
				fixture.partyID, member.name, err)
		}
	}
	return time.Since(started), nil
}

// fanoutWallTime is the measured wall time of the release fan-out itself: the
// trigger member's own op37 call is the one that carries fanOutToGameSessions for
// every follower and then for the leader, so its duration is the fan-out's cost.
// It is also the value partyFanoutBudget actually bounds -- a breach inside the
// fan-out surfaces as a context deadline error out of Release().
func (fixture *stressPartyFixture) fanoutWallTime() time.Duration {
	worst := time.Duration(0)
	for index := range fixture.members {
		member := &fixture.members[index]
		if member.entered && member.finishLoadingCall > worst {
			worst = member.finishLoadingCall
		}
	}
	return worst
}

// stressReleaseChain locates one member's loading-release write chain inside the
// shared ordered log. The chain starts at the first op120 placement row for
// roster slot 0 -- completeRuntimePartyDungeonLoading writes one placement row
// per roster slot before anything else -- and ends at the bodyless class0/op42
// consumable-use gate reset, which is the last write of a release.
type stressReleaseChain struct {
	member     *stressPartyMember
	startIndex int
	endIndex   int
	startedAt  time.Time
	finishedAt time.Time
	writes     int // this member's own writes inside the chain
	ok         bool
}

func (chain *stressReleaseChain) window() time.Duration {
	if chain.startedAt.IsZero() || chain.finishedAt.IsZero() || chain.finishedAt.Before(chain.startedAt) {
		return 0
	}
	return chain.finishedAt.Sub(chain.startedAt)
}

func (fixture *stressPartyFixture) releaseChains() ([]stressReleaseChain, int) {
	fixture.t.Helper()
	return fixture.releaseChainsFrom(fixture.snapshotLog())
}

func (fixture *stressPartyFixture) releaseChainsFrom(snapshot stressLogSnapshot) ([]stressReleaseChain, int) {
	chains := make([]stressReleaseChain, len(fixture.members))
	byName := make(map[string]int, len(fixture.members))
	for index := range fixture.members {
		chains[index] = stressReleaseChain{member: &fixture.members[index], startIndex: -1, endIndex: -1}
		byName[fixture.members[index].name] = index
	}
	newestEnd := -1
	for writeIndex, write := range snapshot.writes {
		memberIndex, known := byName[write.connection]
		if !known {
			continue
		}
		packet, trailing := splitGameServerUpperPacket(fixture.t, write.data)
		if len(trailing) != 0 || packet.Header.Classification != 0 {
			continue
		}
		chain := &chains[memberIndex]
		switch {
		case packet.Header.MsgID == uint16(dnfenum.CmdPacketRequestBlacklist) &&
			bytes.Equal(packet.Body, buildRuntimePartyDungeonPlacementBody(0)):
			if chain.startIndex < 0 {
				chain.startIndex = writeIndex
				if writeIndex < len(snapshot.times) {
					chain.startedAt = snapshot.times[writeIndex]
				}
			}
		case packet.Header.MsgID == currentDungeonConsumableUseGateResetWireOpcode:
			if chain.startIndex >= 0 {
				chain.endIndex = writeIndex
				if writeIndex < len(snapshot.times) {
					chain.finishedAt = snapshot.times[writeIndex]
				}
			}
		}
	}
	for index := range chains {
		chain := &chains[index]
		chain.ok = chain.startIndex >= 0 && chain.endIndex >= chain.startIndex
		if !chain.ok {
			continue
		}
		for writeIndex := chain.startIndex; writeIndex <= chain.endIndex; writeIndex++ {
			if snapshot.writes[writeIndex].connection == chain.member.name {
				chain.writes++
			}
		}
		if chain.endIndex > newestEnd {
			newestEnd = chain.endIndex
		}
	}
	return chains, newestEnd
}

// stressAssertReleaseWaveShaped is the concurrency evidence for one party: the
// measured release wall time must be a small constant number of member-equivalents
// rather than the serial sum, and the followers released together must actually
// overlap in time. Returns the serial sum and the slowest member window so the
// caller can report them.
func stressAssertReleaseWaveShaped(
	t *testing.T,
	label string,
	elapsed time.Duration,
	chains []stressReleaseChain,
	leaderIndex int,
) (time.Duration, time.Duration) {
	t.Helper()
	serialSum := time.Duration(0)
	worst := time.Duration(0)
	measured := 0
	trigger := -1
	for index := range chains {
		chain := &chains[index]
		if !chain.ok {
			continue
		}
		window := chain.window()
		serialSum += window
		if window > worst {
			worst = window
		}
		measured++
		if trigger < 0 || chain.startedAt.Before(chains[trigger].startedAt) {
			trigger = index
		}
	}
	if measured == 0 || worst == 0 {
		return serialSum, worst
	}
	if elapsed > time.Duration(stressReleaseWaveCeiling)*worst {
		t.Errorf("%s: release took %s, more than %d member-equivalents of the slowest member window %s "+
			"(serial sum %s): the fan-out is not releasing concurrently",
			label, elapsed, stressReleaseWaveCeiling, worst, serialSum)
	}
	// Followers released by the same fan-out must overlap: they share an instant.
	overlapStart, overlapFinish := time.Time{}, time.Time{}
	followerCount := 0
	for index := range chains {
		chain := &chains[index]
		if !chain.ok || index == leaderIndex || index == trigger {
			continue
		}
		followerCount++
		if overlapStart.IsZero() || chain.startedAt.After(overlapStart) {
			overlapStart = chain.startedAt
		}
		if overlapFinish.IsZero() || chain.finishedAt.Before(overlapFinish) {
			overlapFinish = chain.finishedAt
		}
	}
	if followerCount >= 2 && (overlapFinish.IsZero() || overlapFinish.Before(overlapStart)) {
		t.Errorf("%s: the %d followers released by the same fan-out never overlapped in time", label, followerCount)
	}
	return serialSum, worst
}

// stressGoroutineBaseline waits for the process to settle and returns the
// current goroutine count.
func stressGoroutineBaseline() int {
	previous := runtime.NumGoroutine()
	for attempt := 0; attempt < 40; attempt++ {
		time.Sleep(25 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == previous {
			return current
		}
		previous = current
	}
	return previous
}

// stressAssertGoroutinesSettled waits for the goroutine count to come back to the
// baseline and fails when it does not.
func stressAssertGoroutinesSettled(t *testing.T, label string, baseline int) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	current := runtime.NumGoroutine()
	for {
		current = runtime.NumGoroutine()
		if current <= baseline+2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if current > baseline+8 {
		t.Errorf("%s: goroutines did not return to the baseline: before=%d after=%d", label, baseline, current)
	}
	t.Logf("%s: goroutines before=%d after=%d", label, baseline, current)
	return current
}

// stressAssertAdmissionItemSpent checks the per-member side of the shared entry:
// each admitted member consumed its OWN dungeon admission item and got its own
// op14 deletion row.
func stressAssertAdmissionItemSpent(t *testing.T, label string, fixture *stressPartyFixture) {
	t.Helper()
	for index := range fixture.members {
		member := &fixture.members[index]
		if !member.entered {
			continue
		}
		inventory, found, err := fixture.repos.Inventory.Load(context.Background(), member.id)
		if err != nil || !found {
			t.Fatalf("%s: load member %s admission inventory found=%t err=%v", label, member.name, found, err)
		}
		if _, remains := inventory.Slots["0:180"]; remains {
			t.Errorf("%s: member %s retained its consumed dungeon admission item", label, member.name)
		}
		if !containsAdmissionDeleteUpdateForSlot(t, member.conn.stream(), 180) {
			t.Errorf("%s: member %s did not receive its own final-stack op14 deletion", label, member.name)
		}
	}
}

// stressAssertDungeonInfoForEveryMember proves every admitted member received the
// shared dungeon prelude (op28 currentDungeonInfoNotification), and that a member
// the production roster never carried received none of it.
func stressAssertDungeonInfoForEveryMember(t *testing.T, label string, fixture *stressPartyFixture) {
	t.Helper()
	for index := range fixture.members {
		member := &fixture.members[index]
		seen := false
		leftTown := 0
		for _, packet := range splitAllCurrentGameServerUpperPackets(t, member.conn.stream()) {
			if packet.Header.Classification != 0 {
				continue
			}
			switch packet.Header.MsgID {
			case currentDungeonInfoNotification:
				seen = true
			case currentTownUserLeaveMsgID:
				leftTown++
			}
		}
		if member.entered && !seen {
			t.Errorf("%s: admitted member %s (slot %d) never received op28", label, member.name, member.slot)
		}
		if member.entered {
			continue
		}
		if seen {
			t.Errorf("%s: non-admitted member %s received op28", label, member.name)
		}
		// A member the runtime party table dropped is still standing in the town
		// area: it must have seen its party leave instead of entering.
		if leftTown == 0 {
			t.Errorf("%s: non-admitted member %s saw no town-leave row for its departing party", label, member.name)
		}
	}
}

// TestStressPartyDungeonEntryPartySizeParameterization is the table-driven
// version of the 2-member model test. For each N it builds N sessions, puts them
// in ONE party roster (member 0 is the leader), puts them in the same channel and
// town area, and asks the leader to enter one dungeon for all of them.
//
// Measured production ceiling (asserted below and reported by t.Logf): one party
// roster cannot exceed 4 members anywhere on this path.
//
//   - runtimePartyMembers (party_online.go) caps a party at MaxMembers, and
//     MaxMembers is itself clamped to 4 (currentRuntimePartyMaxMembers).
//   - validateDungeonEntry counts that clamped roster.
//   - newRuntimePartyDungeonOwner requires "an active 2..4 member party".
//   - buildRuntimePartyDungeonActorProjection rejects rosters outside 2..4.
//   - the ready/release barrier in runtimePartyDungeonOwner is a uint8 slot mask,
//     so it cannot even address slot 8.
//
// Above 4 the extra members are dropped from the runtime party table: they stay
// in town, receive no op28 and no release. A live process cannot build such a
// state -- the central party manager caps every party at 4 -- so this is an
// unreachable configuration rather than a live defect, which is why the
// 40-member fan-out measurement lives in
// TestStressPartyDungeonEntryConcurrentSquads, in the shape the game actually
// uses for 40 players (runtimeRaidMaxGroups squads x runtimeRaidGroupSize).
func TestStressPartyDungeonEntryPartySizeParameterization(t *testing.T) {
	table, resolver, monsters := loadBridgeDungeonStaticData(t, stressDungeonPVFSource())
	goroutinesBefore := stressGoroutineBaseline()

	for _, test := range []struct {
		name             string
		memberCount      int
		wantAdmitted     int
		wantPartyCeiling int
	}{
		{name: "two_members", memberCount: 2, wantAdmitted: 2, wantPartyCeiling: 2},
		{name: "eight_members", memberCount: 8, wantAdmitted: 4, wantPartyCeiling: 4},
		{name: "forty_members", memberCount: 40, wantAdmitted: 4, wantPartyCeiling: 4},
	} {
		test := test
		t.Run(fmt.Sprintf("N=%d", test.memberCount), func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			service := stressNewService(t, repositories, table, resolver, monsters)
			channel := channelcatalog.Channel{
				ServerID: 1, ID: stressBaseChannelID, Type: 11,
				Name: fmt.Sprintf("ch.%d", stressBaseChannelID), Port: 10000 + stressBaseChannelID,
			}
			fixture := buildStressPartyFixture(t, service, repositories, 99, 99, test.memberCount, channel)

			// The runtime party table is what the entry actually carries.
			partyTable := runtimePartyMembers(runtimePartyStateSnapshot(fixture.leader().session))
			if len(partyTable) != test.wantPartyCeiling {
				t.Fatalf("N=%d runtime party table=%d members, want the production ceiling %d",
					test.memberCount, len(partyTable), test.wantPartyCeiling)
			}

			enterStarted := time.Now()
			if err := fixture.enter(); err != nil {
				t.Fatalf("N=%d leader op16 party entry: %v", test.memberCount, err)
			}
			enterElapsed := time.Since(enterStarted)
			entered := fixture.markEntered()
			if len(entered) != test.wantAdmitted {
				t.Fatalf("N=%d admitted members=%d want=%d (roster=%d)",
					test.memberCount, len(entered), test.wantAdmitted, test.memberCount)
			}
			t.Logf("N=%d requested_roster=%d runtime_party_table=%d admitted=%d entry_elapsed=%s",
				test.memberCount, test.memberCount, len(partyTable), len(entered), enterElapsed)
			if test.memberCount > test.wantAdmitted {
				t.Logf("N=%d production party ceiling is %d; the other %d members were never in the party table, "+
					"stayed in town and received no dungeon state",
					test.memberCount, test.wantPartyCeiling, test.memberCount-test.wantAdmitted)
			}

			stressAssertDungeonInfoForEveryMember(t, fmt.Sprintf("N=%d entry", test.memberCount), fixture)
			stressAssertAdmissionItemSpent(t, fmt.Sprintf("N=%d entry", test.memberCount), fixture)

			owner := fixture.leader().session.dungeon.runtime.partyDungeonOwner
			if owner == nil || len(owner.roster()) != test.wantAdmitted {
				t.Fatalf("N=%d shared owner roster=%v want=%d members",
					test.memberCount, owner.roster(), test.wantAdmitted)
			}

			fixture.resetWriteLog()
			fixture.setWriteLatency(stressReleaseWriteLatency)
			releaseElapsed, releaseErr := fixture.release()
			fixture.setWriteLatency(0)
			if releaseErr != nil {
				t.Fatalf("N=%d loading release: %v", test.memberCount, releaseErr)
			}

			chains, newestEnd := fixture.releaseChains()
			released := 0
			for index := range chains {
				if chains[index].ok {
					released++
				}
			}
			if released != test.wantAdmitted {
				t.Fatalf("N=%d released members=%d want=%d (newest release write=%d)",
					test.memberCount, released, test.wantAdmitted, newestEnd)
			}
			leaderChain := chains[0]
			if !leaderChain.ok {
				t.Fatalf("N=%d the leader never received its release", test.memberCount)
			}
			// Leader-last contract: every follower's release is completely on the
			// wire before the leader's release starts.
			for index := range fixture.members {
				chain := chains[index]
				if index == 0 || !chain.ok {
					continue
				}
				if chain.endIndex >= leaderChain.startIndex {
					t.Fatalf("N=%d leader release started at write %d before member %s finished at write %d",
						test.memberCount, leaderChain.startIndex, fixture.members[index].name, chain.endIndex)
				}
			}
			if leaderChain.endIndex != newestEnd {
				t.Fatalf("N=%d the last release write (%d) is not the leader's (%d)",
					test.memberCount, newestEnd, leaderChain.endIndex)
			}
			if releaseElapsed >= partyFanoutBudget {
				t.Fatalf("N=%d loading release took %s, over partyFanoutBudget=%s",
					test.memberCount, releaseElapsed, partyFanoutBudget)
			}
			serialSum, worstMember := stressAssertReleaseWaveShaped(
				t, fmt.Sprintf("N=%d", test.memberCount), releaseElapsed, chains, 0)
			fanoutWall := fixture.fanoutWallTime()
			if fanoutWall >= partyFanoutBudget {
				t.Fatalf("N=%d the release fan-out itself took %s, over partyFanoutBudget=%s",
					test.memberCount, fanoutWall, partyFanoutBudget)
			}
			t.Logf("N=%d admitted=%d release_elapsed=%s fan_out_wall_time=%s budget=%s serial_member_sum=%s "+
				"worst_member=%s release_elapsed/worst_member=%.2fx release_elapsed/serial_sum=%.2fx",
				test.memberCount, len(entered), releaseElapsed, fanoutWall, partyFanoutBudget, serialSum, worstMember,
				float64(releaseElapsed)/float64(worstMember),
				float64(releaseElapsed)/float64(serialSum))
			for index := range fixture.members {
				member := &fixture.members[index]
				if !member.entered {
					continue
				}
				chain := chains[index]
				t.Logf("N=%d member=%s slot=%d admitted=true release_writes=%d release_window=%s "+
					"own_finish_loading_call=%s",
					test.memberCount, member.name, member.slot, chain.writes, chain.window(),
					member.finishLoadingCall)
			}
		})
	}
	stressAssertGoroutinesSettled(t, "party-size parameterization", goroutinesBefore)
}

// stressSquadOutcome is one squad's measured contribution to the aggregate run.
type stressSquadOutcome struct {
	index          int
	partyID        int
	admitted       int
	released       int
	enterErr       error
	releaseErr     error
	enterElapsed   time.Duration
	releaseElapsed time.Duration
	fanoutWall     time.Duration
	log            stressLogSnapshot
}

// TestStressPartyDungeonEntryConcurrentSquads is the aggregate scale test.
//
// A 40-player raid is runtimeRaidMaxGroups squads x runtimeRaidGroupSize members,
// and each squad owns its own party dungeon: 10 squads for the raid maximum, and
// 250 squads (1000 sessions) for a 1000-player booking. Every squad's leader
// triggers handleDungeonSelectUpper concurrently, then every squad runs the real
// op37 loading barrier, whose release fan-out is the code under test.
//
// Fidelity note: the 250-squad row places each squad in its OWN channel instead
// of putting 250 parties in one town area. The shared entry broadcasts a
// town-leave row to every non-party peer in the area, so 1000 sessions in one
// area measures the quadratic town broadcast rather than the dungeon fan-out.
// The 40-member row keeps the single shared channel/area the task asks for.
func TestStressPartyDungeonEntryConcurrentSquads(t *testing.T) {
	table, resolver, monsters := loadBridgeDungeonStaticData(t, stressDungeonPVFSource())
	goroutinesBefore := stressGoroutineBaseline()

	for _, test := range []struct {
		name          string
		squads        int
		shareOneLobby bool
	}{
		{name: "forty_members_ten_squads_same_lobby", squads: runtimeRaidMaxGroups, shareOneLobby: true},
		{name: "thousand_members_two_hundred_fifty_squads", squads: 250, shareOneLobby: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			repositories := dnfrepomemory.NewMemoryGroup()
			service := stressNewService(t, repositories, table, resolver, monsters)
			// Warm the lazily created central party manager on this goroutine so the
			// concurrent phase measures entry + fan-out, not first-use init.
			_ = service.runtimePartyManagerForService()

			setupStarted := time.Now()
			squads := make([]*stressPartyFixture, 0, test.squads)
			for squadIndex := 0; squadIndex < test.squads; squadIndex++ {
				channelID := stressBaseChannelID
				if !test.shareOneLobby {
					channelID = stressBaseChannelID + squadIndex
				}
				channel := channelcatalog.Channel{
					ServerID: 1, ID: channelID, Type: 11,
					Name: fmt.Sprintf("ch.%d", channelID), Port: 10000 + channelID%5000,
				}
				squads = append(squads, buildStressPartyFixture(
					t,
					service,
					repositories,
					squadIndex+1,
					uint16(squadIndex*runtimeRaidGroupSize+1),
					runtimeRaidGroupSize,
					channel,
				))
			}
			memberCount := test.squads * runtimeRaidGroupSize
			setupElapsed := time.Since(setupStarted)

			for _, squad := range squads {
				squad.resetWriteLog()
			}
			// Phase 1: every squad's leader triggers handleDungeonSelectUpper at the
			// same instant. No injected latency here, so this measures the real
			// entry work (including the shared town-area departure broadcast).
			outcomes := make([]stressSquadOutcome, len(squads))
			entryStart := make(chan struct{})
			var entryWait sync.WaitGroup
			concurrentEntryStarted := time.Now()
			for squadIndex := range squads {
				entryWait.Add(1)
				go func(index int) {
					defer entryWait.Done()
					squad := squads[index]
					outcome := stressSquadOutcome{index: index, partyID: squad.partyID}
					defer func() { outcomes[index] = outcome }()
					<-entryStart
					enterStarted := time.Now()
					if err := squad.enter(); err != nil {
						outcome.enterErr = err
						return
					}
					outcome.enterElapsed = time.Since(enterStarted)
					outcome.admitted = len(squad.markEntered())
				}(squadIndex)
			}
			close(entryStart)
			entryWait.Wait()
			concurrentEntryElapsed := time.Since(concurrentEntryStarted)

			// Phase 2: every squad runs the real op37 loading barrier at the same
			// instant. This is the fan-out under test, so the per-write latency is
			// enabled only here and the logs are re-based to it.
			for _, squad := range squads {
				squad.resetWriteLog()
				squad.setWriteLatency(stressReleaseWriteLatency)
			}
			releaseStart := make(chan struct{})
			var releaseWait sync.WaitGroup
			concurrentReleaseStarted := time.Now()
			for squadIndex := range squads {
				releaseWait.Add(1)
				go func(index int) {
					defer releaseWait.Done()
					squad := squads[index]
					<-releaseStart
					elapsed, err := squad.release()
					// Packet parsing stays on the test goroutine.
					outcomes[index].releaseElapsed = elapsed
					outcomes[index].releaseErr = err
					outcomes[index].fanoutWall = squad.fanoutWallTime()
					outcomes[index].log = squad.snapshotLog()
				}(squadIndex)
			}
			close(releaseStart)
			releaseWait.Wait()
			concurrentReleaseElapsed := time.Since(concurrentReleaseStarted)
			for _, squad := range squads {
				squad.setWriteLatency(0)
			}

			totalReleased := 0
			totalEntered := 0
			memberSum := time.Duration(0)
			worstMember := time.Duration(0)
			slowestSquad := time.Duration(0)
			serialSquadSum := time.Duration(0)
			slowestFanoutWall := time.Duration(0)
			for index := range outcomes {
				outcome := &outcomes[index]
				if outcome.enterErr != nil {
					t.Fatalf("squad %d op16 entry: %v", outcome.partyID, outcome.enterErr)
				}
				if outcome.releaseErr != nil {
					t.Fatalf("squad %d loading release: %v", outcome.partyID, outcome.releaseErr)
				}
				if outcome.admitted != runtimeRaidGroupSize {
					t.Fatalf("squad %d admitted=%d want=%d", outcome.partyID, outcome.admitted, runtimeRaidGroupSize)
				}
				if outcome.releaseElapsed >= partyFanoutBudget {
					t.Fatalf("squad %d loading release took %s, over partyFanoutBudget=%s",
						outcome.partyID, outcome.releaseElapsed, partyFanoutBudget)
				}
				if outcome.fanoutWall >= partyFanoutBudget {
					t.Fatalf("squad %d release fan-out itself took %s, over partyFanoutBudget=%s",
						outcome.partyID, outcome.fanoutWall, partyFanoutBudget)
				}
				if outcome.fanoutWall > slowestFanoutWall {
					slowestFanoutWall = outcome.fanoutWall
				}
				squad := squads[index]
				chains, newestEnd := squad.releaseChainsFrom(outcome.log)
				squadReleased := 0
				for chainIndex := range chains {
					if chains[chainIndex].ok {
						squadReleased++
					}
				}
				if squadReleased != runtimeRaidGroupSize {
					t.Fatalf("squad %d released=%d want=%d", outcome.partyID, squadReleased, runtimeRaidGroupSize)
				}
				if !chains[0].ok || chains[0].endIndex != newestEnd {
					t.Fatalf("squad %d did not release its leader last (leader_end=%d newest=%d)",
						outcome.partyID, chains[0].endIndex, newestEnd)
				}
				for chainIndex := range chains {
					chain := &chains[chainIndex]
					if chainIndex == 0 || !chain.ok {
						continue
					}
					if chain.endIndex >= chains[0].startIndex {
						t.Fatalf("squad %d leader release started at write %d before member %s finished at %d",
							outcome.partyID, chains[0].startIndex, chain.member.name, chain.endIndex)
					}
				}
				squadSum, squadWorst := stressAssertReleaseWaveShaped(
					t, fmt.Sprintf("squad %d", outcome.partyID), outcome.releaseElapsed, chains, 0)
				outcome.released = squadReleased
				totalReleased += squadReleased
				totalEntered += outcome.admitted
				memberSum += squadSum
				serialSquadSum += outcome.releaseElapsed
				if squadWorst > worstMember {
					worstMember = squadWorst
				}
				if outcome.releaseElapsed > slowestSquad {
					slowestSquad = outcome.releaseElapsed
				}
				for memberIndex := range squad.members {
					if !squad.members[memberIndex].entered {
						t.Fatalf("squad %d member %s was never admitted", outcome.partyID, squad.members[memberIndex].name)
					}
				}
				stressAssertDungeonInfoForEveryMember(t, fmt.Sprintf("squad %d entry", outcome.partyID), squad)
				stressAssertAdmissionItemSpent(t, fmt.Sprintf("squad %d entry", outcome.partyID), squad)
			}
			for _, squad := range squads {
				squad.setWriteLatency(0)
			}
			if totalReleased != memberCount || totalEntered != memberCount {
				t.Fatalf("entered=%d released=%d want=%d", totalEntered, totalReleased, memberCount)
			}
			t.Logf("squads=%d members=%d entered=%d released=%d setup_elapsed=%s",
				test.squads, memberCount, totalEntered, totalReleased, setupElapsed)
			t.Logf("squads=%d concurrent_entry_elapsed=%s concurrent_release_elapsed=%s slowest_squad_release=%s",
				test.squads, concurrentEntryElapsed, concurrentReleaseElapsed, slowestSquad)
			t.Logf("squads=%d serial_squad_release_sum=%s member_release_sum=%s worst_member_release=%s",
				test.squads, serialSquadSum, memberSum, worstMember)
			t.Logf("squads=%d budget=%s gain_vs_serial_member_sum=%.1fx release_elapsed/worst_member=%.1fx "+
				"slowest_fan_out_wall_time=%s",
				test.squads, partyFanoutBudget,
				float64(memberSum)/float64(concurrentReleaseElapsed),
				float64(concurrentReleaseElapsed)/float64(worstMember),
				slowestFanoutWall)
			// The whole point of the fan-out: N squads cost far less than the sum of
			// their members' release times, because the squads -- and the members
			// inside each squad -- release concurrently.
			if test.squads > 1 && concurrentReleaseElapsed*2 >= memberSum {
				t.Errorf("concurrent release of %d squads took %s, not better than half the serial member sum %s",
					test.squads, concurrentReleaseElapsed, memberSum)
			}
			if concurrentReleaseElapsed >= partyFanoutBudget {
				t.Fatalf("concurrent release took %s, over partyFanoutBudget=%s",
					concurrentReleaseElapsed, partyFanoutBudget)
			}
		})
	}
	stressAssertGoroutinesSettled(t, "concurrent squads", goroutinesBefore)
}
