package dnfbridge

// stress_raid_projection_test.go is a MEASUREMENT HARNESS for the raid
// scene-boundary mate-actor projection. It measures; it does not fix anything.
//
// The production code under measurement is, unmodified:
//
//	Service.syncRuntimeRaidMateActorsAllAtSceneBoundary   (online_player_manager.go:592)
//	  -> Service.sendRuntimeRaidPeerActor                  (online_player_manager.go:462)
//	       -> Service.sendTownRemoteActorProjection        (online_player_manager.go:1002)
//	            -> Service.characterForEnter               (game_select_resolve.go:171)
//	            -> Service.attachEquipmentSummary          (initial_equipment_roster_projection.go:119)
//
// It is a serial double loop over state.FrozenMembers, so it performs exactly
// len(FrozenMembers) * (len(FrozenMembers)-1) synchronous peer-actor installs,
// each of which re-reads the peer's character aggregate from the repository with
// nothing shared between two sends of the same peer.
//
// The dungeon lifecycle boundaries no longer use that function: they call
// syncRuntimeRaidMateActorsMovedAtSceneBoundary, which installs only the moved
// member's 2*(N-1) pairs. The tests below that drive runSceneBoundary measure the
// every-pair function directly (phase activation's caller); the one test that
// drives the real lifecycle trigger measures the moved-scope function.
//
// What this file deliberately does NOT fake:
//   - the two loops and the whole call chain above are the real production
//     functions; nothing is stubbed, shadowed or reimplemented;
//   - the raid state is a real runtimeRaidState registered in Service.raids and
//     the sessions are real *gameSession values bound through
//     Service.bindGameSessionCharacter, so Service.onlineGameSession and
//     Service.runtimeRaidForMember resolve them exactly as in production;
//   - the packets are built and written by the production wire path
//     (sendGameUpperRawClass -> ... -> session.conn.Write).
//
// What IS a test double, stated plainly:
//   - the socket: bufferConn plus a per-session write counter;
//   - the repository: the ordinary in-memory group, optionally wrapped in
//     counting decorators so every repository read is counted. Only *_test.go
//     files are involved; no production file was modified for instrumentation.
//   - the reported numbers are NOT built with -race: this box's cc1 cannot
//     compile the 64-bit race runtime. The measured projection is fully serial
//     on one goroutine, so dropping -race costs no fidelity for these numbers.
//
// This is the RAID path, not the ordinary party path: the rosters below go up to
// runtimeRaidMaxGroups*runtimeRaidGroupSize = 40 members, which the ordinary
// party table cannot express at all (runtimePartyMembers clamps to four). Every
// fixture asserts the roster size and that no ordinary party carries the
// sessions before it measures anything.

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformdb "longheng.io/server/internal/platform/db"
)

// ---------------------------------------------------------------------------
// Repository read counting
// ---------------------------------------------------------------------------

// stressRaidRepoReads counts every repository call the measured chain performs,
// split by repository and method, keeping reads and writes apart. The mutex is
// only so the harness stays correct if it is ever driven concurrently; the
// measured projection itself is serial.
type stressRaidRepoReads struct {
	mu       sync.Mutex
	reads    int64
	writes   int64
	byMethod map[string]int64
}

func newStressRaidRepoReads() *stressRaidRepoReads {
	return &stressRaidRepoReads{byMethod: make(map[string]int64)}
}

func (counts *stressRaidRepoReads) noteRead(repo string, method string) {
	counts.mu.Lock()
	counts.reads++
	counts.byMethod[repo+"."+method]++
	counts.mu.Unlock()
}

func (counts *stressRaidRepoReads) noteWrite(repo string, method string) {
	counts.mu.Lock()
	counts.writes++
	counts.byMethod[repo+"."+method]++
	counts.mu.Unlock()
}

type stressRaidRepoReadSnapshot struct {
	reads    int64
	writes   int64
	byMethod map[string]int64
}

func (counts *stressRaidRepoReads) snapshot() stressRaidRepoReadSnapshot {
	counts.mu.Lock()
	defer counts.mu.Unlock()
	out := stressRaidRepoReadSnapshot{
		reads:    counts.reads,
		writes:   counts.writes,
		byMethod: make(map[string]int64, len(counts.byMethod)),
	}
	for method, count := range counts.byMethod {
		out.byMethod[method] = count
	}
	return out
}

// deltaFrom returns the calls performed between the receiver's snapshot and an
// earlier one. The receiver is the LATER snapshot.
func (after stressRaidRepoReadSnapshot) deltaFrom(before stressRaidRepoReadSnapshot) stressRaidRepoReadSnapshot {
	out := stressRaidRepoReadSnapshot{
		reads:    after.reads - before.reads,
		writes:   after.writes - before.writes,
		byMethod: make(map[string]int64),
	}
	for method, count := range after.byMethod {
		if change := count - before.byMethod[method]; change != 0 {
			out.byMethod[method] = change
		}
	}
	return out
}

// stressRaidCountingStore wraps the repositories whose interface is just
// db.Store[T]. Embedding the interface keeps every method reachable, so only the
// counting changes; Load is overridden, Save is promoted.
type stressRaidCountingStore[T any] struct {
	platformdb.Store[T]
	counts *stressRaidRepoReads
	repo   string
}

func (store stressRaidCountingStore[T]) Load(ctx context.Context, key string) (T, bool, error) {
	store.counts.noteRead(store.repo, "Load")
	return store.Store.Load(ctx, key)
}

// stressRaidCountingCharacterRepository counts the whole CharacterRepository
// surface the measured chain could reach: Load is the important one, but
// ListByAccount / FindIDByName / NextNumericID are reads too, and leaving them
// uncounted would let the total understate itself.
type stressRaidCountingCharacterRepository struct {
	dnfrepo.CharacterRepository
	counts *stressRaidRepoReads
}

func (repository stressRaidCountingCharacterRepository) Load(ctx context.Context, key string) (dnfrepo.CharacterRecord, bool, error) {
	repository.counts.noteRead("Character", "Load")
	return repository.CharacterRepository.Load(ctx, key)
}

func (repository stressRaidCountingCharacterRepository) ListByAccount(ctx context.Context, accountID string, limit int) ([]dnfrepo.CharacterRecord, error) {
	repository.counts.noteRead("Character", "ListByAccount")
	return repository.CharacterRepository.ListByAccount(ctx, accountID, limit)
}

func (repository stressRaidCountingCharacterRepository) FindIDByName(ctx context.Context, name string) (string, bool, error) {
	repository.counts.noteRead("Character", "FindIDByName")
	return repository.CharacterRepository.FindIDByName(ctx, name)
}

func (repository stressRaidCountingCharacterRepository) NextNumericID(ctx context.Context) (int, error) {
	repository.counts.noteRead("Character", "NextNumericID")
	return repository.CharacterRepository.NextNumericID(ctx)
}

type stressRaidCountingDungeonPermissionRepository struct {
	dnfrepo.DungeonPermissionRepository
	counts *stressRaidRepoReads
}

func (repository stressRaidCountingDungeonPermissionRepository) Load(ctx context.Context, key string) (dnfrepo.DungeonPermissionRecord, bool, error) {
	repository.counts.noteRead("DungeonPermission", "Load")
	return repository.DungeonPermissionRepository.Load(ctx, key)
}

func (repository stressRaidCountingDungeonPermissionRepository) UpsertMax(ctx context.Context, key string, dungeonID uint32, count byte) (dnfrepo.DungeonPermissionEntry, bool, error) {
	repository.counts.noteWrite("DungeonPermission", "UpsertMax")
	return repository.DungeonPermissionRepository.UpsertMax(ctx, key, dungeonID, count)
}

type stressRaidCountingHellPartyPermissionRepository struct {
	dnfrepo.HellPartyPermissionRepository
	counts *stressRaidRepoReads
}

func (repository stressRaidCountingHellPartyPermissionRepository) Load(ctx context.Context, key string) (dnfrepo.HellPartyPermissionRecord, bool, error) {
	repository.counts.noteRead("HellPartyPermission", "Load")
	return repository.HellPartyPermissionRepository.Load(ctx, key)
}

func (repository stressRaidCountingHellPartyPermissionRepository) Upsert(ctx context.Context, key string, index int64, value int64) (dnfrepo.HellPartyPermissionEntry, bool, error) {
	repository.counts.noteWrite("HellPartyPermission", "Upsert")
	return repository.HellPartyPermissionRepository.Upsert(ctx, key, index, value)
}

type stressRaidCountingUnitedFriendRepository struct {
	dnfrepo.UnitedFriendRepository
	counts *stressRaidRepoReads
}

func (repository stressRaidCountingUnitedFriendRepository) Load(ctx context.Context, key string) (dnfrepo.UnitedFriendRecord, bool, error) {
	repository.counts.noteRead("UnitedFriend", "Load")
	return repository.UnitedFriendRepository.Load(ctx, key)
}

func (repository stressRaidCountingUnitedFriendRepository) ListObservers(ctx context.Context, name string) ([]string, error) {
	repository.counts.noteRead("UnitedFriend", "ListObservers")
	return repository.UnitedFriendRepository.ListObservers(ctx, name)
}

// stressRaidCountingRepositories wraps every read-capable field the in-memory
// group populates. LegacyUserInfo and LegacyInventory are deliberately left
// alone: the in-memory group leaves them nil, and wrapping a nil interface would
// turn a production `!= nil` availability check into a live nil dereference.
func stressRaidCountingRepositories(inner dnfrepo.Group, counts *stressRaidRepoReads) dnfrepo.Group {
	out := inner
	out.Account = stressRaidCountingStore[dnfrepo.AccountRecord]{Store: inner.Account, counts: counts, repo: "Account"}
	out.AccountInventory = stressRaidCountingStore[dnfrepo.AccountInventoryRecord]{Store: inner.AccountInventory, counts: counts, repo: "AccountInventory"}
	out.Character = stressRaidCountingCharacterRepository{CharacterRepository: inner.Character, counts: counts}
	out.Inventory = stressRaidCountingStore[dnfrepo.InventoryRecord]{Store: inner.Inventory, counts: counts, repo: "Inventory"}
	out.Equipment = stressRaidCountingStore[dnfrepo.EquipmentRecord]{Store: inner.Equipment, counts: counts, repo: "Equipment"}
	out.Pet = stressRaidCountingStore[dnfrepo.PetRecord]{Store: inner.Pet, counts: counts, repo: "Pet"}
	out.Quest = stressRaidCountingStore[dnfrepo.QuestRecord]{Store: inner.Quest, counts: counts, repo: "Quest"}
	out.Skill = stressRaidCountingStore[dnfrepo.SkillRecord]{Store: inner.Skill, counts: counts, repo: "Skill"}
	out.DungeonPermission = stressRaidCountingDungeonPermissionRepository{DungeonPermissionRepository: inner.DungeonPermission, counts: counts}
	out.HellPartyPermission = stressRaidCountingHellPartyPermissionRepository{HellPartyPermissionRepository: inner.HellPartyPermission, counts: counts}
	out.PacketTemplate = stressRaidCountingStore[dnfrepo.PacketTemplateRecord]{Store: inner.PacketTemplate, counts: counts, repo: "PacketTemplate"}
	out.Settings = stressRaidCountingStore[dnfrepo.SettingsRecord]{Store: inner.Settings, counts: counts, repo: "Settings"}
	out.Mailbox = stressRaidCountingStore[dnfrepo.MailboxRecord]{Store: inner.Mailbox, counts: counts, repo: "Mailbox"}
	out.UnitedFriend = stressRaidCountingUnitedFriendRepository{UnitedFriendRepository: inner.UnitedFriend, counts: counts}
	out.EpicBook = stressRaidCountingStore[dnfrepo.EpicBookRecord]{Store: inner.EpicBook, counts: counts, repo: "EpicBook"}
	return out
}

// ---------------------------------------------------------------------------
// Wire counting
// ---------------------------------------------------------------------------

// stressRaidConn is the existing bufferConn plus a per-session write counter.
// One conn.Write call is one packet: writeGameConn (game_dproto.go:175) hands
// the whole built frame to conn.Write in a single call.
type stressRaidConn struct {
	*bufferConn
	name   string
	writes int64
	bytes  int64
}

func (connection *stressRaidConn) Write(data []byte) (int, error) {
	connection.writes++
	connection.bytes += int64(len(data))
	return connection.bufferConn.Write(data)
}

func (connection *stressRaidConn) reset() {
	connection.writes = 0
	connection.bytes = 0
	connection.bufferConn.write.Reset()
}

func (connection *stressRaidConn) stream() []byte {
	return append([]byte(nil), connection.bufferConn.write.Bytes()...)
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	stressRaidChannelID = 200
	stressRaidBaseID    = uint16(1001)
)

type stressRaidProjectionMember struct {
	index   int
	charID  uint16
	id      string
	name    string
	session *gameSession
	conn    *stressRaidConn
}

type stressRaidProjectionFixture struct {
	t       *testing.T
	service *Service
	repos   dnfrepo.Group
	counts  *stressRaidRepoReads
	raidKey uint32
	channel channelcatalog.Channel
	members []stressRaidProjectionMember
}

func newStressRaidProjectionFixture(t *testing.T, memberCount int) *stressRaidProjectionFixture {
	t.Helper()
	return stressRaidBuildFixture(t, memberCount, true)
}

// newStressRaidProjectionFixtureRaw uses the raw in-memory group with no
// decorators at all. It exists only for the transparency check that proves the
// counting wrappers change nothing about the packets.
func newStressRaidProjectionFixtureRaw(t *testing.T, memberCount int) *stressRaidProjectionFixture {
	t.Helper()
	return stressRaidBuildFixture(t, memberCount, false)
}

// stressRaidBuildFixture builds memberCount raid members, each with a real bound
// game session and a real registered raid roster.
//
// Every member is seeded with a complete structured snapshot (inventory,
// equipment, skill and the three initialization settings scopes) so the measured
// path never enters the character-initialization backfill and every character
// costs the same number of repository reads. That is what makes the per-pair
// cost stable enough to compare against N*(N-1).
func stressRaidBuildFixture(t *testing.T, memberCount int, counted bool) *stressRaidProjectionFixture {
	t.Helper()
	if memberCount < 2 {
		t.Fatalf("raid roster of %d cannot exercise the pair loop", memberCount)
	}
	counts := newStressRaidRepoReads()
	inner := dnfrepomemory.NewMemoryGroup()
	repos := inner
	if counted {
		repos = stressRaidCountingRepositories(inner, counts)
	}

	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		raids:              make(map[uint32]*runtimeRaidState),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	channel := channelcatalog.Channel{ID: stressRaidChannelID, Type: 23, Name: "anton", Port: 10238}
	fixture := &stressRaidProjectionFixture{
		t:       t,
		service: service,
		repos:   repos,
		counts:  counts,
		raidKey: uint32(0x00c8a100) + uint32(memberCount),
		channel: channel,
		members: make([]stressRaidProjectionMember, 0, memberCount),
	}

	roster := make([]runtimeRaidMemberState, 0, memberCount)
	for index := 0; index < memberCount; index++ {
		charID := stressRaidBaseID + uint16(index)
		id := strconv.Itoa(int(charID))
		stressRaidSeedCharacter(t, repos, charID, index)
		conn := &stressRaidConn{bufferConn: &bufferConn{}, name: "raid-" + id}
		session := &gameSession{
			conn:      conn,
			connID:    conn.name,
			accountID: "stress-raid-account-" + id,
			channel:   channel,
			// A raid member standing in the raid waiting area owns a channel
			// scene. That is the ordinary production state at a gate boundary,
			// and it is what makes each install the full mode0-record +
			// mode1-binding form rather than the record-only one.
			townActorOwnerChannel:           byte(channel.ID),
			connectionTownActorOwnerChannel: byte(channel.ID),
			selectedCharacterID:             charID,
			residentChannel:                 channel,
		}
		service.bindGameSessionCharacter(session, charID)
		// runtimeRaidMaxGroups squads x runtimeRaidGroupSize members: the same
		// group/slot shape a real 40-member raid roster carries.
		roster = append(roster, runtimeRaidMemberState{
			CharID:     charID,
			Name:       "raid" + id,
			Level:      90,
			GroupIndex: byte(index/runtimeRaidGroupSize) + 1,
			SlotOrder:  byte(index % runtimeRaidGroupSize),
			UserState:  1,
			HPPercent:  100,
			MPPercent:  100,
		})
		fixture.members = append(fixture.members, stressRaidProjectionMember{
			index:   index,
			charID:  charID,
			id:      id,
			name:    conn.name,
			session: session,
			conn:    conn,
		})
	}

	service.raids[fixture.raidKey] = &runtimeRaidState{
		RaidKey:        fixture.raidKey,
		LeaderID:       roster[0].CharID,
		NameBytes:      []byte("anton"),
		TailFlag:       5,
		Members:        append([]runtimeRaidMemberState(nil), roster...),
		FrozenMembers:  append([]runtimeRaidMemberState(nil), roster...),
		Started:        true,
		EntryCostPaid:  true,
		StartAttempt:   1,
		ChargedMembers: runtimeRaidMemberIDs(runtimeRaidState{Members: roster}),
		Lifecycle:      newRuntimeRaidLifecycleState(time.Now().UTC()),
	}
	fixture.assertRoster(t, memberCount)
	return fixture
}

// stressRaidSeedCharacter writes the durable character state a real, already
// initialized character has: base record plus inventory/equipment/pet/skill and
// the three settings scopes characterInitializationPresenceForRecord checks.
// character_initialization.go:31 then returns immediately (presence.complete()),
// so the measured read count is the steady-state production one and no PVF
// archive or backfill is involved.
//
// The account and creature records are seeded too. Several readers on this path
// call Account.Load / Pet.Load unconditionally and only branch on `found`, so
// seeding them removes any doubt that a missing record could steer the chain
// down a different branch than a live server would take.
func stressRaidSeedCharacter(t *testing.T, repos dnfrepo.Group, characterID uint16, index int) {
	t.Helper()
	ctx := context.Background()
	id := strconv.Itoa(int(characterID))
	accountID := "stress-raid-account-" + id
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: accountID}); err != nil {
		t.Fatalf("seed account %s: %v", accountID, err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: id,
		AccountID:   accountID,
		Slot:        index,
		Name:        "raid" + id,
		Job:         "0",
		Level:       90,
		Stats: map[string]int64{
			"town_id": 38, "area_id": 1, "pos_x": 450, "pos_y": 234, "direction": 5, "area_state": 3,
		},
	}); err != nil {
		t.Fatalf("seed character %s: %v", id, err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: id}); err != nil {
		t.Fatalf("seed inventory %s: %v", id, err)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: id}); err != nil {
		t.Fatalf("seed equipment %s: %v", id, err)
	}
	if err := repos.Pet.Save(ctx, dnfrepo.PetRecord{CharacterID: id}); err != nil {
		t.Fatalf("seed pet %s: %v", id, err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: id}); err != nil {
		t.Fatalf("seed skill %s: %v", id, err)
	}
	for _, scope := range []string{
		newCharacterContainerStateSettingsScope(id),
		newCharacterInitBodiesSettingsScope(id),
		newCharacterHotkeySettingsScope(id),
	} {
		if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{Scope: scope, Values: map[string]string{}}); err != nil {
			t.Fatalf("seed settings %s/%s: %v", id, scope, err)
		}
	}
}

// assertRoster is the fidelity gate: prove the roster really carries N members
// on the RAID path before anything is measured, and that the ordinary party
// table -- which cannot hold more than four -- is not what is being measured.
func (fixture *stressRaidProjectionFixture) assertRoster(t *testing.T, want int) {
	t.Helper()
	for _, member := range fixture.members {
		session, online := fixture.service.onlineGameSession(member.charID)
		if !online || session != member.session {
			t.Fatalf("char %d is not reachable through onlineGameSession (online=%t)", member.charID, online)
		}
		if session.selectedCharacterID != member.charID || session.conn == nil {
			t.Fatalf("char %d session is not bound to a usable conn", member.charID)
		}
		state, ok := fixture.service.runtimeRaidForMember(member.charID)
		if !ok {
			t.Fatalf("char %d does not resolve a raid through runtimeRaidForMember", member.charID)
		}
		if len(state.FrozenMembers) != want {
			t.Fatalf("char %d resolves a raid with %d frozen members, want %d", member.charID, len(state.FrozenMembers), want)
		}
		party := runtimePartyStateSnapshot(session)
		if party.PartyID != 0 || len(runtimePartyMembers(party)) != 0 {
			t.Fatalf("char %d is carried by an ordinary party (party_id=%d members=%d): this is not the raid path",
				member.charID, party.PartyID, len(runtimePartyMembers(party)))
		}
	}
	t.Logf("roster=%d members asserted present on the RAID path (runtimePartyStateSnapshot.PartyID=0 for every member; the ordinary party table clamps at 4)", want)
}

// ---------------------------------------------------------------------------
// Measurement
// ---------------------------------------------------------------------------

type stressRaidSendCost struct {
	reads  int64
	writes int64
}

// calibrateSend runs the REAL production sendRuntimeRaidPeerActor exactly once
// and reports what that one ordered pair costs. Every packet the whole
// projection produces comes from this same function, so the calibration is the
// unit the totals are checked against.
func (fixture *stressRaidProjectionFixture) calibrateSend(target *gameSession, peerCharID uint16) stressRaidSendCost {
	fixture.t.Helper()
	state, ok := fixture.service.runtimeRaidForMember(target.selectedCharacterID)
	if !ok {
		fixture.t.Fatalf("calibration target %d has no raid", target.selectedCharacterID)
	}
	writesBefore := fixture.totalWrites()
	before := fixture.counts.snapshot()
	fixture.service.sendRuntimeRaidPeerActor(state, target, peerCharID, "stress_raid_calibration")
	after := fixture.counts.snapshot()
	return stressRaidSendCost{
		reads:  after.deltaFrom(before).reads,
		writes: fixture.totalWrites() - writesBefore,
	}
}

func (fixture *stressRaidProjectionFixture) totalWrites() int64 {
	total := int64(0)
	for index := range fixture.members {
		total += fixture.members[index].conn.writes
	}
	return total
}

func (fixture *stressRaidProjectionFixture) totalBytes() int64 {
	total := int64(0)
	for index := range fixture.members {
		total += fixture.members[index].conn.bytes
	}
	return total
}

// resetWire clears the packet counters and buffers of every member conn.
func (fixture *stressRaidProjectionFixture) resetWire() {
	for index := range fixture.members {
		fixture.members[index].conn.reset()
	}
}

type stressRaidProjectionRun struct {
	wall          time.Duration
	reads         int64
	writes        int64
	bytes         int64
	readsByMethod map[string]int64
}

// runSceneBoundary invokes the real production
// syncRuntimeRaidMateActorsAllAtSceneBoundary with a state snapshot obtained the
// same way the production caller obtains it (Service.runtimeRaidForMember) and
// measures one call.
func (fixture *stressRaidProjectionFixture) runSceneBoundary(source string, deferWhileBetweenScenes bool) stressRaidProjectionRun {
	fixture.t.Helper()
	state, ok := fixture.service.runtimeRaidForMember(fixture.members[0].charID)
	if !ok {
		fixture.t.Fatalf("leader does not resolve a raid")
	}
	fixture.resetWire()
	before := fixture.counts.snapshot()
	started := time.Now()
	fixture.service.syncRuntimeRaidMateActorsAllAtSceneBoundary(state, source, deferWhileBetweenScenes)
	wall := time.Since(started)
	after := fixture.counts.snapshot()
	delta := after.deltaFrom(before)
	return stressRaidProjectionRun{
		wall:          wall,
		reads:         delta.reads,
		writes:        fixture.totalWrites(),
		bytes:         fixture.totalBytes(),
		readsByMethod: delta.byMethod,
	}
}

// timeSceneBoundaryBatches runs the projection `batch` times back to back inside
// ONE timed window and returns the mean per-call wall time plus the batch's
// repository-read and packet-write totals.
//
// The batching exists because this host's clock cannot resolve a sub-millisecond
// projection: a single N=4 call was observed to measure as exactly 0s, which
// would make the reported curve an artefact of the timer rather than of the
// code. The state snapshot is resolved once for the whole batch, so what is timed
// is the projection itself and not the trigger's roster clone.
func (fixture *stressRaidProjectionFixture) timeSceneBoundaryBatches(source string, batch int) (time.Duration, int64, int64) {
	fixture.t.Helper()
	if batch < 1 {
		batch = 1
	}
	state, ok := fixture.service.runtimeRaidForMember(fixture.members[0].charID)
	if !ok {
		fixture.t.Fatalf("leader does not resolve a raid")
	}
	fixture.resetWire()
	before := fixture.counts.snapshot()
	started := time.Now()
	for repeat := 0; repeat < batch; repeat++ {
		fixture.service.syncRuntimeRaidMateActorsAllAtSceneBoundary(state, source, false)
	}
	total := time.Since(started)
	delta := fixture.counts.snapshot().deltaFrom(before)
	return total / time.Duration(batch), delta.reads, fixture.totalWrites()
}

// stressRaidClockGranularity reports the smallest non-zero interval this host's
// monotonic clock can express, by sampling time.Now() in a tight loop. It is
// logged with the results so the timing numbers can be read against the timer
// they were taken with.
func stressRaidClockGranularity() time.Duration {
	best := time.Duration(1) << 62
	previous := time.Now()
	for sample := 0; sample < 500000; sample++ {
		now := time.Now()
		if delta := now.Sub(previous); delta > 0 && delta < best {
			best = delta
		}
		previous = now
	}
	if best == time.Duration(1)<<62 {
		return 0
	}
	return best
}

func stressRaidMedian(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func stressRaidMin(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	best := values[0]
	for _, value := range values[1:] {
		if value < best {
			best = value
		}
	}
	return best
}

func stressRaidMax(values []time.Duration) time.Duration {
	worst := time.Duration(0)
	for _, value := range values {
		if value > worst {
			worst = value
		}
	}
	return worst
}

// ---------------------------------------------------------------------------
// Main scaling measurement: N = 4 / 8 / 16 / 40
// ---------------------------------------------------------------------------

// stressRaidProjectionRepeats is how many timed windows each size is measured
// in after a discarded warm-up call, so the reported time is not dominated by
// first touch of the packet builder's lazy state.
const stressRaidProjectionRepeats = 5

// stressRaidTimingWindow is the minimum wall time one timed window should cover,
// and stressRaidTimingMaxBatch caps how many calls that costs. Both exist only
// to keep the host's clock granularity (see stressRaidClockGranularity) out of
// the reported per-call number.
const (
	stressRaidTimingWindow   = 20 * time.Millisecond
	stressRaidTimingMaxBatch = 200
)

type stressRaidScalingRow struct {
	members        int
	pairs          int64 // N*(N-1): the sends the double loop performs
	reads          int64
	characterLoads int64
	writes         int64
	bytes          int64
	wallMedian     time.Duration
	wallMin        time.Duration
	wallMax        time.Duration
	timingBatch    int
	perPairReads   float64
	perPairWrites  float64
	perPairMicros  float64
	perMemberReads float64
}

func TestStressRaidSceneBoundaryProjectionScaling(t *testing.T) {
	// Report the timer the timing numbers were taken with: a coarse clock is a
	// first-class caveat for a sub-millisecond measurement, not a footnote.
	t.Logf("host monotonic clock granularity = %s", stressRaidClockGranularity())
	sizes := []int{4, 8, 16, 40}
	rows := make([]stressRaidScalingRow, 0, len(sizes))
	for _, size := range sizes {
		size := size
		t.Run(fmt.Sprintf("N=%d", size), func(t *testing.T) {
			rows = append(rows, stressRaidMeasureSize(t, size))
		})
	}
	if t.Failed() || len(rows) != len(sizes) {
		t.Fatalf("scaling measurement incomplete: rows=%d want=%d", len(rows), len(sizes))
	}
	stressRaidReportScaling(t, rows)
}

func stressRaidMeasureSize(t *testing.T, memberCount int) stressRaidScalingRow {
	t.Helper()
	fixture := newStressRaidProjectionFixture(t, memberCount)
	pairs := int64(memberCount) * int64(memberCount-1)

	// Warm-up: one full projection, discarded. It also proves the chain runs.
	warmup := fixture.runSceneBoundary("stress_raid_projection_warmup", false)
	if warmup.reads == 0 || warmup.writes == 0 {
		t.Fatalf("N=%d warm-up projection produced reads=%d writes=%d: the real chain did not run",
			memberCount, warmup.reads, warmup.writes)
	}

	// Calibrate the per-pair cost with the real production single-pair send.
	// This happens AFTER the warm-up so nothing lazy is charged to the unit.
	single := fixture.calibrateSend(fixture.members[0].session, fixture.members[1].charID)
	if single.reads == 0 || single.writes == 0 {
		t.Fatalf("N=%d calibration send produced reads=%d writes=%d", memberCount, single.reads, single.writes)
	}

	// The PACKET count is the direct evidence that the whole chain really ran for
	// every ordered pair: N*(N-1) sends, each producing exactly the packets one
	// calibration send produced -- no silent early return, no skipped install. It
	// is asserted exactly, and the batch-scoped read cache does not move it.
	wantWrites := pairs * single.writes
	if warmup.writes != wantWrites {
		t.Fatalf("N=%d one projection wrote %d packets; %d ordered pairs x %d writes/send = %d",
			memberCount, warmup.writes, pairs, single.writes, wantWrites)
	}

	// Repository reads used to be asserted as the exact equality
	// pairs*single.reads with the comment "no ... memoization". That equality only
	// held while nothing was cached, and it pinned the quadratic N*(N-1) read
	// count in place. The projection now memoizes the per-actor loads for the
	// duration of one fan-out (projection_read_cache.go), so the reads are
	// asserted as the shape that fix promises instead:
	//
	//   * a LINEAR upper bound: resolving every actor may cost at most one
	//     single-pair send's worth of reads per member, whatever the roster size;
	//   * strictly below the unmemoized total, which is what proves the cache is
	//     actually in effect on this route rather than silently disabled.
	//
	// A return to per-pair recomputation fails both checks at every N > 2.
	maxReads := int64(memberCount) * single.reads
	unmemoizedReads := pairs * single.reads
	if warmup.reads > maxReads {
		t.Fatalf("N=%d one projection read the repository %d times; one batch-scoped resolution per member allows at most %d (N x %d reads/pair)",
			memberCount, warmup.reads, maxReads, single.reads)
	}
	if warmup.reads >= unmemoizedReads {
		t.Fatalf("N=%d one projection read the repository %d times, not below the %d an unmemoized pair loop costs: the batch read cache is not in effect",
			memberCount, warmup.reads, unmemoizedReads)
	}
	t.Logf("N=%d repo_reads=%d (linear bound %d, unmemoized %d, reduction %.1fx)",
		memberCount, warmup.reads, maxReads, unmemoizedReads,
		float64(unmemoizedReads)/float64(warmup.reads))

	// Wall time. It is taken on a SECOND fixture whose repository group carries no
	// counting decorators at all, so the reported number is the projection's own
	// cost and not the instrumentation's: the decorators cost one mutex + one map
	// write per call, which measured ~10% at N=40.
	//
	// Each window is batched so it is worth at least stressRaidTimingWindow
	// regardless of how fast the projection is; a single sub-millisecond call
	// would otherwise be quantised to zero by this host's ~508us clock, which is
	// exactly what the first run of this harness produced.
	raw := newStressRaidProjectionFixtureRaw(t, memberCount)
	rawWarmup := raw.runSceneBoundary("stress_raid_projection_timing_warmup", false)
	if rawWarmup.reads != 0 {
		t.Fatalf("N=%d the raw timing fixture is unexpectedly instrumented (reads=%d)", memberCount, rawWarmup.reads)
	}
	batch := stressRaidBatchFor(rawWarmup.wall)
	times := make([]time.Duration, 0, stressRaidProjectionRepeats)
	var last stressRaidProjectionRun
	for repeat := 0; repeat < stressRaidProjectionRepeats; repeat++ {
		perCall, _, batchWrites := raw.timeSceneBoundaryBatches("stress_raid_projection_timing", batch)
		if batchWrites != wantWrites*int64(batch) {
			t.Fatalf("N=%d timing batch of %d wrote %d packets, want %d",
				memberCount, batch, batchWrites, wantWrites*int64(batch))
		}
		times = append(times, perCall)

		// The counted fixture keeps being re-verified on every repeat, so the
		// read/write totals in the table are the numbers actually asserted.
		last = fixture.runSceneBoundary("stress_raid_projection_scaling", false)
		if last.reads > maxReads || last.writes != wantWrites {
			t.Fatalf("N=%d single call drifted: reads=%d max=%d writes=%d want=%d",
				memberCount, last.reads, maxReads, last.writes, wantWrites)
		}
	}

	row := stressRaidScalingRow{
		members:        memberCount,
		pairs:          pairs,
		reads:          last.reads,
		characterLoads: last.readsByMethod["Character.Load"],
		writes:         last.writes,
		bytes:          last.bytes,
		wallMedian:     stressRaidMedian(times),
		wallMin:        stressRaidMin(times),
		wallMax:        stressRaidMax(times),
		timingBatch:    batch,
	}
	row.perPairReads = float64(row.reads) / float64(pairs)
	row.perPairWrites = float64(row.writes) / float64(pairs)
	row.perPairMicros = float64(row.wallMedian.Microseconds()) / float64(pairs)
	row.perMemberReads = float64(row.reads) / float64(memberCount)

	t.Logf("N=%d ordered_pairs=%d reads=%d (Character.Load=%d) writes=%d bytes=%d",
		memberCount, pairs, row.reads, row.characterLoads, row.writes, row.bytes)
	t.Logf("N=%d wall per call (median of %d windows of %d calls each)=%s min=%s max=%s per_pair_time=%s",
		memberCount, stressRaidProjectionRepeats, batch, row.wallMedian, row.wallMin, row.wallMax,
		row.wallMedian/time.Duration(pairs))
	t.Logf("N=%d per_pair_reads=%.2f per_pair_writes=%.2f per_member_reads=%.1f",
		memberCount, row.perPairReads, row.perPairWrites, row.perMemberReads)
	t.Logf("N=%d single_pair_unit(reads=%d writes=%d) reads_by_repository=%s",
		memberCount, single.reads, single.writes, stressRaidFormatCounts(last.readsByMethod))
	return row
}

func stressRaidFormatCounts(byMethod map[string]int64) string {
	keys := make([]string, 0, len(byMethod))
	for key := range byMethod {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, byMethod[key]))
	}
	return strings.Join(parts, ",")
}

// stressRaidReportScaling prints the curve and fits it against the two candidate
// shapes: quadratic N*(N-1) and linear N.
func stressRaidReportScaling(t *testing.T, rows []stressRaidScalingRow) {
	t.Helper()
	t.Logf("SCALING TABLE -- one syncRuntimeRaidMateActorsAllAtSceneBoundary call per row:")
	t.Logf("%6s %8s %11s %16s %10s %13s %13s %13s %8s",
		"N", "pairs", "repo_reads", "character_loads", "packets", "wall_median", "wall/pair", "reads/member", "batch")
	for _, row := range rows {
		t.Logf("%6d %8d %11d %16d %10d %13s %13s %13.1f %8d",
			row.members, row.pairs, row.reads, row.characterLoads, row.writes,
			row.wallMedian.Round(time.Microsecond),
			(row.wallMedian / time.Duration(row.pairs)).Round(time.Nanosecond),
			row.perMemberReads, row.timingBatch)
	}

	// Fit each candidate shape by least squares through the origin and report
	// the worst relative residual. The shape with the small residual is the one
	// the cost actually tracks.
	quadCoefficient := 0.0
	linearCoefficient := 0.0
	var quadDenominator, linearDenominator float64
	for _, row := range rows {
		pairs := float64(row.pairs)
		quadCoefficient += pairs * float64(row.reads)
		quadDenominator += pairs * pairs
		linearCoefficient += float64(row.members) * float64(row.reads)
		linearDenominator += float64(row.members) * float64(row.members)
	}
	quadCoefficient /= quadDenominator
	linearCoefficient /= linearDenominator

	worstQuadratic, worstLinear := 0.0, 0.0
	t.Logf("MODEL FIT -- repository reads:")
	t.Logf("%6s %8s %11s %15s %13s %11s %13s", "N", "pairs", "observed", "fit=N*(N-1)", "fit=N", "err_quad", "err_linear")
	for _, row := range rows {
		quadratic := quadCoefficient * float64(row.pairs)
		linear := linearCoefficient * float64(row.members)
		observed := float64(row.reads)
		quadraticError := stressRaidRelativeError(observed, quadratic)
		linearError := stressRaidRelativeError(observed, linear)
		if quadraticError > worstQuadratic {
			worstQuadratic = quadraticError
		}
		if linearError > worstLinear {
			worstLinear = linearError
		}
		t.Logf("%6d %8d %11.0f %15.1f %13.1f %10.3f%% %12.3f%%",
			row.members, row.pairs, observed, quadratic, linear, quadraticError*100, linearError*100)
	}
	t.Logf("fit: reads = %.3f * N*(N-1) (worst residual %.3f%%)   vs   reads = %.3f * N (worst residual %.3f%%)",
		quadCoefficient, worstQuadratic*100, linearCoefficient, worstLinear*100)

	first, last := rows[0], rows[len(rows)-1]
	growth := float64(last.reads) / float64(first.reads)
	pairGrowth := float64(last.pairs) / float64(first.pairs)
	memberGrowth := float64(last.members) / float64(first.members)
	t.Logf("growth N=%d -> N=%d: observed_reads=%.2fx  N*(N-1) predicts=%.2fx  N predicts=%.2fx",
		first.members, last.members, growth, pairGrowth, memberGrowth)
	t.Logf("per-member reads stay bounded at %.1f..%.1f for every N (the linear signature of the batch read cache); "+
		"per-pair reads shrink %.1f -> %.1f as the roster grows",
		rows[0].perMemberReads, rows[len(rows)-1].perMemberReads, rows[0].perPairReads, rows[len(rows)-1].perPairReads)
	t.Logf("per-pair wall time: %s at N=%d, %s at N=%d (the N*(N-1) sends themselves are still serial)",
		(rows[0].wallMedian / time.Duration(rows[0].pairs)).Round(time.Nanosecond), rows[0].members,
		(last.wallMedian / time.Duration(last.pairs)).Round(time.Nanosecond), last.members)

	// Hard assertions on the deterministic quantities.
	//
	// These were flipped when the per-actor projection became cached for the
	// duration of one fan-out. The pre-cache contract asserted the reads TRACK
	// N*(N-1); the contract now is that they track N, and that the quadratic
	// shape is gone. The linear tolerance is loose because the count is a small
	// constant per member plus one global settings read, which a least-squares
	// line through the origin cannot hit exactly at N=4.
	if worstLinear > 0.05 {
		t.Errorf("repository reads no longer track N: worst residual %.3f%%", worstLinear*100)
	}
	if worstQuadratic <= 0.05 {
		t.Errorf("repository reads still track N*(N-1) (worst residual %.3f%%): the batch-scoped read cache is not in effect",
			worstQuadratic*100)
	}
	if growth > 1.25*memberGrowth {
		t.Errorf("reads grew %.2fx for N=%d..%d; N predicts %.2fx -- the per-member read cost is no longer bounded",
			growth, first.members, last.members, memberGrowth)
	}
	if growth >= 0.9*pairGrowth {
		t.Errorf("reads grew %.2fx, indistinguishable from the quadratic prediction %.2fx -- the per-pair recomputation is back",
			growth, pairGrowth)
	}
	// Timing is reported, not asserted tightly; only the coarse shape is checked,
	// and only between the two extremes where the signal is far above the noise.
	firstPerMember := float64(first.wallMedian.Nanoseconds()) / float64(first.members)
	lastPerMember := float64(last.wallMedian.Nanoseconds()) / float64(last.members)
	ratio := 0.0
	if firstPerMember > 0 {
		ratio = lastPerMember / firstPerMember
	}
	t.Logf("per-member wall time: N=%d %.1fus  N=%d %.1fus  ratio=%.2fx (a linear cost would hold it at ~1.0x)",
		first.members, firstPerMember/1000, last.members, lastPerMember/1000, ratio)
	// 时间只报数，不判红。
	//
	// 这条曾经断言 ratio >= 2（配对循环是串行的，每人成本应随人数增长）。但它量的是墙钟比值：
	// 本机同时跑着游戏服与 MySQL，全包并行时 N=4 的中位数容易被抬高、N=40 被摊薄，比值掉到
	// 2 以下就红 —— 2026-09-27 两次全包就是这么红的（隔离跑 3 次全绿；通过时 ratio=4.19x）。
	// 真正防回归的是上面那些结构性断言：仓储读次数必须跟 N 走、不能跟 N(N-1) 走，包写次数按
	// 公式精确核对，字节流在计数装饰器下必须一致 —— 批量读缓存一旦失效，那几条立刻红，
	// 且与机器负载无关。所以这里保留数值、去掉判红。
	if ratio < 2 {
		t.Logf("NOTE: per-member wall time grew only %.2fx from N=%d to N=%d (an idle machine shows >= 2x): "+
			"timing is informational here, the repository-read assertions above are the regression guard",
			ratio, first.members, last.members)
	}
}

func stressRaidRelativeError(observed float64, predicted float64) float64 {
	if predicted == 0 {
		return 1
	}
	error := (observed - predicted) / predicted
	if error < 0 {
		return -error
	}
	return error
}

// ---------------------------------------------------------------------------
// Corroboration: the real production trigger
// ---------------------------------------------------------------------------

// TestStressRaidSceneBoundaryProjectionThroughRealLifecycleTrigger drives the
// projection through its real production caller instead of calling it directly:
//
//	Service.freezeRuntimeRaidDungeonSelection  (raid_dungeon_lifecycle.go:84)
//	  -> notifyRuntimeRaidDungeonLifecycleLocked                        (:277)
//	       -> applyRuntimeRaidDungeonLifecycleEvent
//	       -> publishRuntimeRaidDungeonParticipation                    (:487)
//	       -> syncRuntimeRaidMateActorsMovedAtSceneBoundary              (:418-423)
//
// The `selected` boundary is the exact transition raid_dungeon_lifecycle.go:418
// names, and publishRuntimeRaidDungeonParticipation returns early for it
// (raid_dungeon_lifecycle.go:521-529), so this route isolates the projection: it
// performs no other repository work and writes no other packet. The `loaded`
// boundary is then driven off the same binding, which is the
// deferWhileBetweenScenes=false shape the abandoned/completed boundaries use.
func TestStressRaidSceneBoundaryProjectionThroughRealLifecycleTrigger(t *testing.T) {
	for _, memberCount := range []int{8, 40} {
		memberCount := memberCount
		t.Run(fmt.Sprintf("N=%d", memberCount), func(t *testing.T) {
			stressRaidMeasureRealTrigger(t, memberCount)
		})
	}
}

func stressRaidMeasureRealTrigger(t *testing.T, memberCount int) {
	t.Helper()
	fixture := newStressRaidProjectionFixture(t, memberCount)
	pairs := int64(memberCount) * int64(memberCount-1)

	// Warm the chain first, so nothing lazy is charged to the trigger.
	warmup := fixture.runSceneBoundary("stress_raid_trigger_warmup", false)
	if warmup.reads == 0 {
		t.Fatalf("N=%d warm-up produced no repository reads", memberCount)
	}

	mover := fixture.members[0]
	other := fixture.members[1]
	runtime := &runtimeDungeonState{
		Request:        dungeoncmd.SelectDungeonRequest{DungeonID: 210},
		Character:      dnfrepo.CharacterRecord{CharacterID: mover.id},
		lifecycleToken: 41,
	}
	mover.session.dungeon = dungeonSessionState{runtime: runtime}

	// The real `selected` trigger. With deferWhileBetweenScenes=true this
	// boundary skips a receiver whose channel scene is gone; every member here
	// owns a channel scene, so no receiver is skipped and both directions of the
	// moved member's pairs are installed.
	fixture.resetWire()
	before := fixture.counts.snapshot()
	selectedStarted := time.Now()
	if err := fixture.service.freezeRuntimeRaidDungeonSelection(mover.session, runtime); err != nil {
		t.Fatalf("N=%d freeze raid dungeon selection (the real selected trigger): %v", memberCount, err)
	}
	selectedWall := time.Since(selectedStarted)
	selectedReads := fixture.counts.snapshot().deltaFrom(before)
	selectedWrites := fixture.totalWrites()

	// The trigger leaves the mover inside an open gate run, which changes the
	// install mode for the two directions this boundary installs: an install INTO
	// the mover is the record-only form (mode1 is withheld from a client rendering
	// an instance) while the mover's record INTO each member that stayed is the
	// bound form. Both units are measured through the real
	// sendRuntimeRaidPeerActor and their PACKET totals are checked against the
	// trigger separately, because the two forms write a different number of
	// packets -- that exact packet count is the evidence that the moved member's
	// own scene change reached both directions of every one of its pairs.
	//
	// The boundary no longer sweeps every ordered pair, and it no longer installs
	// the moved member alone either. A pair that excludes the mover has neither
	// side changing scene, so the old N*(N-1) sweep re-sent it verbatim: measured
	// on 203.0.113.11 (2026-09-27, 12-member Anton raid), 88.2% of this boundary's
	// installs and 57.5% of the `selected` boundary's were an identical (target,
	// actor, mode1 length) triple already sent to the same target for the same
	// actor within 10 s. Cutting it to the moved member's own pairs then produced
	// the operator's "看不到是哪队进图 就显示个单 队长头像也是黑的", because the
	// 战况表 resolves a row's cell through the row's FIRST member, who is typically
	// NOT the member that entered. The scope is therefore the moved member's board
	// row -- its squad, runtimeRaidGroupSize members -- so a mate that stayed gets
	// the whole row minus itself, and the mover still gets every mate:
	//
	//	wantBoundSends   = (rowSize-1)*(rowSize-1) + (N-rowSize)*rowSize
	//	wantRecordSends  = N-1
	//
	// which at N=8 is 3x3 + 4x4 = 25 and at N=40 is 3x3 + 36x4 = 153, against 7
	// and 39 for the moved-only scope and 56 / 1560 for the every-pair sweep.
	// The read total is no longer expected to be N*(N-1) x (reads per send): the
	// projection memoizes its per-actor loads for the duration of one fan-out
	// (projection_read_cache.go), so the reads are bounded linearly instead. The
	// read count does not depend on the install mode either way, which is why one
	// per-pair figure bounds both forms. Narrowing the pair scope also narrows the
	// gap the two read assertions below separate: a cached run resolves one body
	// per member (N*R) while an unmemoized one pays per installed direction, so the
	// guard still holds for every N this test drives, with a 2x margin at N=40
	// instead of the N-fold margin the pair sweep used to give.
	recordCost := fixture.calibrateSend(mover.session, other.charID)
	boundCost := fixture.calibrateSend(other.session, mover.charID)

	rowSize := int64(runtimeRaidGroupSize)
	if rowSize > int64(memberCount) {
		rowSize = int64(memberCount)
	}
	wantRecordSends := int64(memberCount - 1) // every mate's record INTO the mover
	wantBoundSends := (rowSize-1)*(rowSize-1) + (int64(memberCount)-rowSize)*rowSize
	perPairReads := recordCost.reads
	if boundCost.reads > perPairReads {
		perPairReads = boundCost.reads
	}
	maxReads := int64(memberCount) * perPairReads
	unmemoizedReads := wantRecordSends*recordCost.reads + wantBoundSends*boundCost.reads
	wantWrites := wantRecordSends*recordCost.writes + wantBoundSends*boundCost.writes
	if selectedReads.reads > maxReads {
		t.Fatalf("N=%d real selected trigger read the repository %d times; one batch-scoped resolution per member allows at most %d (N x %d reads/pair)",
			memberCount, selectedReads.reads, maxReads, perPairReads)
	}
	if selectedReads.reads >= unmemoizedReads {
		t.Fatalf("N=%d real selected trigger read the repository %d times, not below the %d an unmemoized pair loop costs: the batch read cache is not in effect",
			memberCount, selectedReads.reads, unmemoizedReads)
	}
	if selectedWrites != wantWrites {
		t.Fatalf("N=%d real selected trigger wrote %d packets; %d x %d + %d x %d = %d",
			memberCount, selectedWrites, wantRecordSends, recordCost.writes, wantBoundSends, boundCost.writes, wantWrites)
	}
	t.Logf("N=%d REAL selected trigger: ordered_pairs=%d record_sends=%d bound_sends=%d repo_reads=%d (max=%d, unmemoized=%d) packets=%d wall=%s",
		memberCount, pairs, wantRecordSends, wantBoundSends, selectedReads.reads, maxReads, unmemoizedReads, selectedWrites, selectedWall)
	t.Logf("N=%d single-pair unit via the real sendRuntimeRaidPeerActor: record_only=%d reads/%d writes  bound=%d reads/%d writes  reads_per_pair=%d",
		memberCount, recordCost.reads, recordCost.writes, boundCost.reads, boundCost.writes, recordCost.reads)

	// The real `loaded` boundary repeats the same install off the same binding,
	// and additionally publishes the squad's op584 participation rows.
	fixture.resetWire()
	before = fixture.counts.snapshot()
	loadedStarted := time.Now()
	if err := fixture.service.notifyRuntimeRaidDungeonLoaded(mover.session); err != nil {
		t.Fatalf("N=%d notify raid dungeon loaded (the real loaded trigger): %v", memberCount, err)
	}
	loadedWall := time.Since(loadedStarted)
	loadedReads := fixture.counts.snapshot().deltaFrom(before)
	loadedWrites := fixture.totalWrites()
	// This route's read floor used to be asserted as ">= the unmemoized pair
	// loop cost". It is now the opposite: the cached route must stay strictly
	// below it (participation reads included), which is the cache-is-live guard
	// for the `loaded` boundary.
	if loadedReads.reads >= unmemoizedReads {
		t.Fatalf("N=%d loaded trigger read the repository %d times, not below the %d an unmemoized pair loop costs: the batch read cache is not in effect",
			memberCount, loadedReads.reads, unmemoizedReads)
	}
	t.Logf("N=%d REAL loaded trigger: repo_reads=%d (unmemoized pair loop=%d, saved=%d) packets=%d (extra=%d) wall=%s",
		memberCount, loadedReads.reads, unmemoizedReads, unmemoizedReads-loadedReads.reads,
		loadedWrites, loadedWrites-wantWrites, loadedWall)
}

// ---------------------------------------------------------------------------
// Fidelity check: the counting decorators must not change behaviour
// ---------------------------------------------------------------------------

// TestStressRaidProjectionCountingDecoratorIsTransparent is the guard on the
// instrumentation itself. The counting wrappers embed interfaces, so an optional
// interface the concrete repository also implements is no longer reachable
// through a type assertion on the wrapped value, and such an assertion could in
// principle take a different branch in production code. This test runs the same
// projection over the raw in-memory group and over the counting group and
// requires byte-identical per-connection packet streams.
func TestStressRaidProjectionCountingDecoratorIsTransparent(t *testing.T) {
	const memberCount = 8

	counted := newStressRaidProjectionFixture(t, memberCount)
	countedRun := counted.runSceneBoundary("stress_raid_decorator_fidelity", false)

	raw := newStressRaidProjectionFixtureRaw(t, memberCount)
	rawRun := raw.runSceneBoundary("stress_raid_decorator_fidelity", false)

	if countedRun.writes != rawRun.writes {
		t.Errorf("counting decorators changed the packet count: counted=%d raw=%d", countedRun.writes, rawRun.writes)
	}
	if countedRun.bytes != rawRun.bytes {
		t.Errorf("counting decorators changed the byte count: counted=%d raw=%d", countedRun.bytes, rawRun.bytes)
	}
	for index := range counted.members {
		countedStream := counted.members[index].conn.stream()
		rawStream := raw.members[index].conn.stream()
		if string(countedStream) != string(rawStream) {
			t.Fatalf("member %s received a different packet stream under the counting decorators (%d vs %d bytes)",
				counted.members[index].name, len(countedStream), len(rawStream))
		}
	}
	t.Logf("counting decorators transparent: N=%d packets=%d bytes=%d per-conn streams byte-identical to the raw group",
		memberCount, countedRun.writes, countedRun.bytes)
	t.Logf("counted repo_reads=%d (the raw-group fixture is not instrumented and reports %d)", countedRun.reads, rawRun.reads)

	// The decorators still cost something at runtime: one mutex lock plus one map
	// write per counted call. Measure it, so the reported wall times can be read
	// with or without that overhead made explicit.
	stressRaidReportDecoratorOverhead(t, 8)
	stressRaidReportDecoratorOverhead(t, 40)
}

// stressRaidReportDecoratorOverhead times the same projection on the counted and
// the raw fixture and logs the difference. Both sides use the same batch sizing
// as the scaling measurement, so a sub-millisecond N=8 projection is not
// quantised by the ~508us clock into a meaningless ratio.
func stressRaidReportDecoratorOverhead(t *testing.T, memberCount int) {
	t.Helper()
	counted := newStressRaidProjectionFixture(t, memberCount)
	countedProbe := counted.runSceneBoundary("stress_raid_decorator_overhead_warmup", false)
	countedTimes := make([]time.Duration, 0, stressRaidProjectionRepeats)
	for repeat := 0; repeat < stressRaidProjectionRepeats; repeat++ {
		wall, _, _ := counted.timeSceneBoundaryBatches("stress_raid_decorator_overhead", stressRaidBatchFor(countedProbe.wall))
		countedTimes = append(countedTimes, wall)
	}

	raw := newStressRaidProjectionFixtureRaw(t, memberCount)
	rawProbe := raw.runSceneBoundary("stress_raid_decorator_overhead_warmup", false)
	rawTimes := make([]time.Duration, 0, stressRaidProjectionRepeats)
	for repeat := 0; repeat < stressRaidProjectionRepeats; repeat++ {
		wall, _, _ := raw.timeSceneBoundaryBatches("stress_raid_decorator_overhead", stressRaidBatchFor(rawProbe.wall))
		rawTimes = append(rawTimes, wall)
	}

	countedMedian := stressRaidMedian(countedTimes)
	rawMedian := stressRaidMedian(rawTimes)
	overhead := 0.0
	if rawMedian > 0 {
		overhead = float64(countedMedian-rawMedian) / float64(rawMedian)
	}
	batch := stressRaidBatchFor(rawProbe.wall)
	t.Logf("N=%d counting-decorator timing overhead (windows of %d calls): counted=%s raw=%s delta=%s (%.1f%% of the raw projection)",
		memberCount, batch, countedMedian, rawMedian, countedMedian-rawMedian, overhead*100)
}

// stressRaidBatchFor sizes a timed window from a single observed call so the
// window is worth at least stressRaidTimingWindow.
func stressRaidBatchFor(observed time.Duration) int {
	batch := 1
	if observed > 0 {
		if window := int64(stressRaidTimingWindow / observed); window > 0 {
			batch = int(window) + 1
		}
	}
	if batch > stressRaidTimingMaxBatch {
		batch = stressRaidTimingMaxBatch
	}
	return batch
}

// ---------------------------------------------------------------------------
// Independent install count from the production packet log
// ---------------------------------------------------------------------------

// TestStressRaidSceneBoundaryProjectionInstallCount reads the number of
// successful installs straight out of the production packet log: every
// successful sendTownRemoteActorProjection emits exactly one
// game-town-copresence-actor-state-send EVENT before it writes, so the event
// count is an independent check on the arithmetic the tests above assert.
//
// The logger has a bounded queue (packetLogQueueSize) and drops lines when the
// writer falls behind. Any drop is reported, and the count is then a lower bound
// rather than an equality.
func TestStressRaidSceneBoundaryProjectionInstallCount(t *testing.T) {
	packetLogTestKeyEnv(t)
	for _, memberCount := range []int{8, 40} {
		memberCount := memberCount
		t.Run(fmt.Sprintf("N=%d", memberCount), func(t *testing.T) {
			fixture := newStressRaidProjectionFixture(t, memberCount)
			// Warm up BEFORE the logger exists, so the file contains exactly the
			// lines of the one measured projection below and no flush-timing
			// guesswork is needed to separate the two runs.
			fixture.runSceneBoundary("stress_raid_log_warmup", false)

			logPath := filepath.Join(t.TempDir(), "raid-projection-packets.log")
			logger, err := openPacketLogger(logPath)
			if err != nil {
				t.Fatal(err)
			}
			fixture.service.packetLog.Store(logger)
			run := fixture.runSceneBoundary("stress_raid_install_count", false)
			// Close drains the queue and waits for the writer, so the file is
			// complete when it is read back.
			if err := logger.Close(); err != nil {
				t.Fatal(err)
			}
			dropped := logger.Dropped()

			data := stressRaidReadLog(t, logPath)
			installs := strings.Count(data, "kind=game-town-copresence-actor-state-send")
			failures := strings.Count(data, "kind=game-raid-peer-actor-failed") +
				strings.Count(data, "kind=game-raid-peer-actor-unavailable")
			deferred := strings.Count(data, "kind=game-raid-peer-actor-deferred")
			pairs := memberCount * (memberCount - 1)

			t.Logf("N=%d production packet log for one projection: installs=%d ordered_pairs=%d failed_or_unavailable=%d deferred=%d dropped_log_lines=%d packets=%d",
				memberCount, installs, pairs, failures, deferred, dropped, run.writes)
			if dropped != 0 {
				t.Logf("N=%d the packet log dropped %d lines: the install count above is a LOWER BOUND, not an equality", memberCount, dropped)
			}
			if failures != 0 {
				t.Errorf("N=%d the production chain reported %d failed/unavailable peer-actor installs", memberCount, failures)
			}
			if deferred != 0 {
				t.Errorf("N=%d %d receivers were deferred although every member owns a channel scene", memberCount, deferred)
			}
			if dropped == 0 && int64(installs) != int64(pairs) {
				t.Errorf("N=%d packet log recorded %d installs, want one per ordered pair = %d", memberCount, installs, pairs)
			}
		})
	}
}

func stressRaidReadLog(t *testing.T, path string) string {
	t.Helper()
	// 包日志自 2026-09-30 起落盘即密文；这里走与运维工具相同的解密路径。
	return packetLogPlaintext(t, path)
}
