package dnfbridge

// repro_20raid_test.go — 20-client raid harness.
//
// WHY THIS FILE EXISTS
//
// The existing stress fixtures (stress_raid_projection_test.go) build sessions
// WITHOUT an event loop. postGameSessionCharacterEvent then takes its
// `session.events == nil` shortcut and runs the closure inline, so those tests
// measure the projection alone and can never observe the delivery coupling.
//
// This harness builds a raid whose members each own a REAL eventloop.Loop and a
// REAL writer goroutine (startGameSessionEvents), backed by a socket whose Write
// can be throttled or stalled. That is what makes it possible to ask the two
// questions the live incident left open:
//
//	A) how much does one scene-boundary fan-out cost at 20 members, and
//	B) does one stalled client make OTHER members stop working?
//
// Nothing in this file is production code.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// ---------------------------------------------------------------------------
// A socket whose Write can be throttled or stalled.
// ---------------------------------------------------------------------------

type reproConn struct {
	*bufferConn
	name string

	// remote, when set, is the authenticated IPv4 TCP tuple this simulated
	// client dialled from. Party formation keys every relay and publish decision
	// on it (currentPartySessionRemoteAddress), so a simulation of the party
	// paths needs a real *net.TCPAddr instead of the fixture's synthetic name.
	remote *net.TCPAddr

	mu      sync.Mutex
	writes  int64
	bytes   int64
	stalled bool
	// stallUntil: while now < stallUntil every Write parks. This models the
	// client that stopped reading, which is what makes the writer goroutine
	// (and therefore that session's loop) slow.
	stallUntil time.Time
}

func (c *reproConn) RemoteAddr() net.Addr {
	if c.remote != nil {
		return c.remote
	}
	return c.bufferConn.RemoteAddr()
}

func (c *reproConn) LocalAddr() net.Addr {
	if c.remote != nil {
		return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10238}
	}
	return c.bufferConn.LocalAddr()
}

func (c *reproConn) Write(data []byte) (int, error) {
	for {
		c.mu.Lock()
		until := c.stallUntil
		c.mu.Unlock()
		if until.IsZero() || !time.Now().Before(until) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	c.mu.Lock()
	c.writes++
	c.bytes += int64(len(data))
	c.mu.Unlock()
	return c.bufferConn.Write(data)
}

func (c *reproConn) stallFor(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = true
	c.stallUntil = time.Now().Add(d)
}

func (c *reproConn) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = false
	c.stallUntil = time.Time{}
}

func (c *reproConn) counters() (writes int64, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes, c.bytes
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const reproChannelID = 200

type reproMember struct {
	index   int
	charID  uint16
	session *gameSession
	conn    *reproConn
}

type reproRaid struct {
	t       *testing.T
	service *Service
	repos   dnfrepo.Group
	ctx     context.Context
	cancel  context.CancelFunc
	raidKey uint32
	members []reproMember
}

// newReproRaid builds memberCount members, each with a real session loop, real
// writer goroutine and a stallable socket, and registers them as one started
// raid roster so the raid-wide fan-out paths run.
func newReproRaid(t *testing.T, memberCount int) *reproRaid {
	t.Helper()
	if memberCount < 2 {
		t.Fatalf("a raid of %d members cannot exercise the fan-out", memberCount)
	}
	ctx, cancel := context.WithCancel(context.Background())
	repos := dnfrepomemory.NewMemoryGroup()
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers:      newOnlinePlayerManager(),
		raids:              make(map[uint32]*runtimeRaidState),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	channel := channelcatalog.Channel{ID: reproChannelID, Type: 23, Name: "anton", Port: 10238}
	raid := &reproRaid{
		t:       t,
		service: service,
		repos:   repos,
		ctx:     ctx,
		cancel:  cancel,
		raidKey: uint32(0x00c8b000) + uint32(memberCount),
		members: make([]reproMember, 0, memberCount),
	}

	roster := make([]runtimeRaidMemberState, 0, memberCount)
	for index := 0; index < memberCount; index++ {
		charID := uint16(3001 + index)
		id := strconv.Itoa(int(charID))
		// Reuse the existing seeder: a complete structured snapshot so the
		// measured path never enters the character-initialization backfill.
		stressRaidSeedCharacter(t, repos, charID, index)
		conn := &reproConn{
			bufferConn: &bufferConn{},
			name:       "repro-" + id,
			// A distinct IPv4 TCP tuple per simulated client: party formation keys
			// the relay legs and the direct endpoint table on it.
			remote: &net.TCPAddr{
				IP:   net.IPv4(127, 0, 0, byte(1+index%200)),
				Port: 41000 + index,
			},
		}
		session := &gameSession{
			conn:                            conn,
			connID:                          conn.name,
			accountID:                       "repro-account-" + id,
			channel:                         channel,
			townActorOwnerChannel:           byte(channel.ID),
			connectionTownActorOwnerChannel: byte(channel.ID),
			selectedCharacterID:             charID,
			residentChannel:                 channel,
			ctx:                             ctx,
		}
		service.bindGameSessionCharacter(session, charID)
		if err := service.startGameSessionEvents(ctx, session); err != nil {
			t.Fatalf("start session events for char %d: %v", charID, err)
		}
		roster = append(roster, runtimeRaidMemberState{
			CharID:     charID,
			Name:       "repro" + id,
			Level:      90,
			GroupIndex: byte(index/runtimeRaidGroupSize) + 1,
			SlotOrder:  byte(index % runtimeRaidGroupSize),
			UserState:  1,
			HPPercent:  100,
			MPPercent:  100,
		})
		raid.members = append(raid.members, reproMember{
			index:   index,
			charID:  charID,
			session: session,
			conn:    conn,
		})
	}

	service.raids[raid.raidKey] = &runtimeRaidState{
		RaidKey:        raid.raidKey,
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
	return raid
}

func (r *reproRaid) close() {
	if r == nil {
		return
	}
	for _, member := range r.members {
		if member.conn != nil {
			member.conn.release()
		}
		if member.session != nil && member.session.events != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = r.service.stopGameSessionEvents(shutdownCtx, member.session)
			r.service.stopGameSessionWriter(shutdownCtx, member.session)
			cancel()
		}
	}
	r.cancel()
}

func (r *reproRaid) state() runtimeRaidState {
	return cloneRuntimeRaidState(*r.service.raids[r.raidKey])
}

func (r *reproRaid) totalWrites() int64 {
	var total int64
	for _, member := range r.members {
		writes, _ := member.conn.counters()
		total += writes
	}
	return total
}

// queueDepths reports every member's event-loop queue depth, so a fan-out that
// cannot be drained shows up as a growing queue rather than as a slower clock.
func (r *reproRaid) queueDepths() []int {
	out := make([]int, 0, len(r.members))
	for _, member := range r.members {
		if member.session.events == nil || member.session.events.loop == nil {
			out = append(out, -1)
			continue
		}
		lengths := member.session.events.loop.QueueLengths()
		out = append(out, lengths.Medium+lengths.High+lengths.Low)
	}
	return out
}

// runBoundaryFrom drives one scene-boundary fan-out FROM a member's own event
// loop, which is what production does (the lifecycle boundary runs on the
// session that reported it). Returns how long the call itself took.
func (r *reproRaid) runBoundaryFrom(caller *reproMember, source string) time.Duration {
	state := r.state()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := r.service.callGameSession(ctx, caller.session, source, func() error {
		r.service.syncRuntimeRaidMateActorsAllAtSceneBoundary(state, source, false)
		return nil
	})
	if err != nil {
		r.t.Logf("boundary via %s returned %v after %s", caller.conn.name, err, time.Since(start))
	}
	return time.Since(start)
}

// canStillWork reports whether another member's loop is still able to run a
// trivial task within the budget. This is the "is everyone frozen" probe.
func (r *reproRaid) canStillWork(member *reproMember, budget time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.service.callGameSession(ctx, member.session, "repro-liveness", func() error { return nil })
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(budget + 200*time.Millisecond):
		return false
	}
}

// ---------------------------------------------------------------------------
// (A) What does one 20-member boundary cost when every client is healthy?
// ---------------------------------------------------------------------------

func TestRepro20RaidBoundaryCost(t *testing.T) {
	for _, memberCount := range []int{8, 20} {
		raid := newReproRaid(t, memberCount)
		// Warm the caches the way a second boundary in the same raid would.
		raid.runBoundaryFrom(&raid.members[0], "repro_warm_raid_mate_actors")
		time.Sleep(300 * time.Millisecond)

		samples := make([]time.Duration, 0, 3)
		for i := 0; i < 3; i++ {
			samples = append(samples, raid.runBoundaryFrom(&raid.members[0],
				fmt.Sprintf("repro_dungeon_loaded_raid_mate_actors_%d", i)))
		}
		var worst time.Duration
		for _, sample := range samples {
			if sample > worst {
				worst = sample
			}
		}
		time.Sleep(500 * time.Millisecond)
		t.Logf("N=%d healthy: boundary wall (caller's loop) worst=%s samples=%v queue_depths=%v writes=%d",
			memberCount, worst, samples, raid.queueDepths(), raid.totalWrites())
		raid.close()
	}
}

// ---------------------------------------------------------------------------
// (B) Does ONE stalled client make the raid stop?
// ---------------------------------------------------------------------------

// TestRepro20RaidOneStalledClient is the falsification test for the delivery
// coupling. It stalls exactly one member's socket, then drives a boundary from
// a different member's loop and asks two things:
//
//  1. does the caller's own loop come back (or does it park on the stalled peer)?
//  2. can an unrelated third member still do work while that is happening?
func TestRepro20RaidOneStalledClient(t *testing.T) {
	raid := newReproRaid(t, 20)
	raid.runBoundaryFrom(&raid.members[0], "repro_warm_raid_mate_actors")
	time.Sleep(500 * time.Millisecond)

	victim := &raid.members[7]
	caller := &raid.members[0]
	third := &raid.members[13]

	victim.conn.stallFor(5 * time.Second)
	t.Logf("stalled %s for 5s; driving a boundary from %s", victim.conn.name, caller.conn.name)

	start := time.Now()
	wall := raid.runBoundaryFrom(caller, "repro_stalled_boundary_raid_mate_actors")
	alive := raid.canStillWork(third, 2*time.Second)
	t.Logf("boundary returned after %s; third member (%s) still workable=%t; queue_depths=%v",
		wall, third.conn.name, alive, raid.queueDepths())

	// While the victim is still stalled, does a SECOND boundary from a healthy
	// caller also pay the penalty? That is the "one client freezes the raid"
	// prediction, and it must not reproduce once the coupling is removed.
	start2 := time.Now()
	wall2 := raid.runBoundaryFrom(third, "repro_stalled_boundary2_raid_mate_actors")
	t.Logf("second boundary (caller %s) returned after %s (elapsed since stall start %s)",
		third.conn.name, wall2, time.Since(start))

	victim.conn.release()
	time.Sleep(1 * time.Second)
	t.Logf("after release: canStillWork(victim)=%t queue_depths=%v",
		raid.canStillWork(victim, 2*time.Second), raid.queueDepths())
	_ = start2
	raid.close()
}

// ---------------------------------------------------------------------------
// (D) The SYNCHRONOUS fan-out: does the initiator's own loop stop?
// ---------------------------------------------------------------------------

// TestRepro20RaidSyncFanoutParksInitiator measures the property the 2026-09-24
// incident was blamed on: fanOutToGameSessions runs on the INITIATOR's event
// loop and wg.Wait()s for every member, so one slow member parks that loop --
// and with it every other request of that one character.
//
// memberCount targets, one of them slow. The caller is member[0]'s loop.
func TestRepro20RaidSyncFanoutParksInitiator(t *testing.T) {
	raid := newReproRaid(t, 20)
	time.Sleep(200 * time.Millisecond)

	const slowMember = 7
	const slowWork = 3 * time.Second

	targets := make([]sessionFanoutTarget, 0, len(raid.members))
	for i := range raid.members {
		targets = append(targets, sessionFanoutTarget{
			label:       raid.members[i].conn.name,
			characterID: raid.members[i].charID,
			session:     raid.members[i].session,
		})
	}

	work := func(target sessionFanoutTarget) error {
		if target.characterID == raid.members[slowMember].charID {
			time.Sleep(slowWork)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	caller := &raid.members[0]
	other := &raid.members[13]

	var fanoutWall time.Duration
	callerLoopBusy := make(chan bool, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = raid.service.callGameSession(ctx, caller.session, "repro-sync-fanout", func() error {
			// Probe this loop's availability from another goroutine while the
			// fan-out is running: if the fan-out parks the loop, the probe
			// cannot be served until it finishes.
			go func() {
				time.Sleep(300 * time.Millisecond)
				callerLoopBusy <- !raid.canStillWork(caller, 500*time.Millisecond)
			}()
			return raid.service.fanOutToGameSessions(context.Background(), targets,
				"repro_party_dungeon_loading_release", work)
		})
		fanoutWall = time.Since(start)
	}()

	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatalf("synchronous fan-out did not finish within 25s")
	}
	t.Logf("fanOutToGameSessions(20 targets, 1 slow=%s): caller loop was parked for %s",
		slowWork, fanoutWall)

	select {
	case parked := <-callerLoopBusy:
		t.Logf("initiator loop unresponsive during the fan-out = %t (expected true on the current design)", parked)
	case <-time.After(2 * time.Second):
		t.Logf("initiator loop probe never answered")
	}

	// The other 18 members must be unaffected: they only paid 10ms each.
	t.Logf("unrelated member %s still workable = %t", other.conn.name, raid.canStillWork(other, 2*time.Second))
	raid.close()
}

// ---------------------------------------------------------------------------
// (E) The ASYNC fan-out must not park anyone (regression guard for the fix).
// ---------------------------------------------------------------------------

// TestRepro20RaidAsyncFanoutDoesNotParkInitiator is the guard for the loading
// release fix. Same shape as test D (20 targets, one of them slow) but through
// fanOutToGameSessionsAsync, which is what the loading release now uses.
//
// The synchronous form parked the caller's loop for the whole 10s budget; the
// asynchronous form must return as soon as the work is handed over, and it must
// still run `then` only after every target finished.
func TestRepro20RaidAsyncFanoutDoesNotParkInitiator(t *testing.T) {
	raid := newReproRaid(t, 20)
	time.Sleep(200 * time.Millisecond)

	const slowMember = 7
	const slowWork = 1500 * time.Millisecond

	targets := make([]sessionFanoutTarget, 0, len(raid.members))
	for i := range raid.members {
		targets = append(targets, sessionFanoutTarget{
			label:       raid.members[i].conn.name,
			characterID: raid.members[i].charID,
			session:     raid.members[i].session,
		})
	}
	slowCharID := raid.members[slowMember].charID

	thenCalled := make(chan time.Duration, 1)
	start := time.Now()
	raid.service.fanOutToGameSessionsAsync(context.Background(), targets,
		"repro_party_dungeon_loading_release",
		func(target sessionFanoutTarget) error {
			if target.characterID == slowCharID {
				time.Sleep(slowWork)
			}
			return nil
		},
		func() { thenCalled <- time.Since(start) })

	// The submit itself must come straight back: that is the whole fix.
	handover := time.Since(start)
	if handover > 500*time.Millisecond {
		t.Fatalf("async fan-out parked the initiator for %s (must hand over immediately)", handover)
	}

	// `then` (the leader-last step) must still wait for the slow member.
	select {
	case took := <-thenCalled:
		if took < slowWork {
			t.Fatalf("then ran after %s, before the slow target finished (%s): ordering contract broken", took, slowWork)
		}
		t.Logf("handover=%s then_fired=%s (slow target needed %s) — no parking, ordering kept", handover, took, slowWork)
	case <-time.After(10 * time.Second):
		t.Fatalf("then never ran: the completion barrier is broken")
	}
	raid.close()
}

// ---------------------------------------------------------------------------
// (C) Does a full queue make the SUBMITTER spin without a deadline?
// ---------------------------------------------------------------------------

// TestReproPostGameSessionEventQueueFullBudget pins the property that a
// submission must always come back: a session whose loop cannot drain (stalled
// socket + a backlog) must never park the submitter forever. It floods one
// session with more events than its queue can hold and measures the submitter's
// worst block against the wall clock.
func TestReproPostGameSessionEventQueueFullBudget(t *testing.T) {
	raid := newReproRaid(t, 4)
	victim := &raid.members[1]
	victim.conn.stallFor(8 * time.Second)

	const bursts = gameSessionEventQueueSize * 2
	var worst time.Duration
	submitter := &raid.members[0]

	t.Logf("flooding %s with %d events while its socket is stalled", victim.conn.name, bursts)
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < bursts; i++ {
		if time.Now().After(deadline) {
			t.Logf("flood stopped at %d submits: 10s wall-clock budget spent", i)
			break
		}
		start := time.Now()
		// This is the exact call the raid fan-out uses to hand work to another
		// member's loop; it must be bounded.
		err := raid.service.postGameSessionCharacterEvent(
			victim.session,
			"repro_flood",
			victim.charID,
			victim.session.characterGeneration,
			func() error { return nil },
		)
		blocked := time.Since(start)
		if blocked > worst {
			worst = blocked
		}
		if err != nil {
			t.Logf("submit %d returned %v after %s", i, err, blocked)
			break
		}
		if time.Since(start) > 3*time.Second {
			break
		}
	}
	t.Logf("worst single submitGameSession block = %s ; caller(%s) still workable=%t ; victim queue=%d",
		worst, submitter.conn.name, raid.canStillWork(submitter, 2*time.Second),
		raid.queueDepths()[1])
	victim.conn.release()
	time.Sleep(1500 * time.Millisecond)
	t.Logf("after release: victim queue=%d", raid.queueDepths()[1])
	raid.close()
}

// ---------------------------------------------------------------------------
// (F) 20 simulated clients: squad formation with one member per squad whose
//     native UDP probe answer never arrived, and with the relay enabled.
//
// This is the shape the operator reported as "组队还组不上 显示连接中": one member
// never registers a probe endpoint, so the direct op11 table cannot be built and
// the whole party used to be left unpublished. The relay is the production
// transport (instance.json enables it), so the formation must still complete.
// ---------------------------------------------------------------------------

func TestRepro20RaidSquadFormationWithUnprobedMember(t *testing.T) {
	raid := newReproRaid(t, 20)
	defer raid.close()

	portStart := testCurrentPartyUDPRelayPortStart(t)
	relay, err := newCurrentPartyUDPRelay(true, "127.0.0.1", portStart, 512, nil)
	if err != nil {
		t.Fatalf("new UDP relay: %v", err)
	}
	defer relay.Close()
	raid.service.partyUDPRelay = relay
	raid.service.gameplayTimers = newPartyPublishTestQueue()

	// Every squad's last member never probes; the other three do.
	probed := make(map[*reproMember]bool)
	for index := range raid.members {
		if index%runtimeRaidGroupSize == runtimeRaidGroupSize-1 {
			continue
		}
		registerPartyPeerEndpointForTest(raid.members[index].session, 3, uint16(40000+index))
		probed[&raid.members[index]] = true
	}

	published := 0
	started := time.Now()
	for squad := 0; squad < 5; squad++ {
		leader := &raid.members[squad*runtimeRaidGroupSize]
		for slot := 1; slot < runtimeRaidGroupSize; slot++ {
			member := &raid.members[squad*runtimeRaidGroupSize+slot]
			state, result, ok := raid.service.createOrJoinManagedRuntimeParty(member.session, leader.session)
			if !ok || state.PartyID <= 0 {
				t.Fatalf("squad %d slot %d join failed: ok=%t result=%+v", squad, slot, ok, result)
			}
			done, err := raid.service.publishManagedRuntimePartySnapshots(state)
			if err != nil {
				t.Fatalf("squad %d slot %d publish: %v", squad, slot, err)
			}
			if !done {
				t.Fatalf("squad %d slot %d formation deferred (member %d has no probe and the relay should carry it)",
					squad, slot, member.charID)
			}
			published++
		}
	}
	elapsed := time.Since(started)
	t.Logf("20 clients: %d squad joins published in %s", published, elapsed)
	if elapsed > 5*time.Second {
		t.Fatalf("squad formation took %s for 20 clients", elapsed)
	}

	// Every member of every squad owns a complete directed relay leg set, and the
	// member without a probe gets its legs too (no seed endpoint, learned live).
	for squad := 0; squad < 5; squad++ {
		leader := &raid.members[squad*runtimeRaidGroupSize]
		leaderState := runtimePartyStateSnapshot(leader.session)
		if leaderState.PartyID <= 0 || len(runtimePartyMembers(leaderState)) != runtimeRaidGroupSize {
			t.Fatalf("squad %d leader party state=%+v", squad, leaderState)
		}
		for slot := 0; slot < runtimeRaidGroupSize; slot++ {
			member := &raid.members[squad*runtimeRaidGroupSize+slot]
			for other := 0; other < runtimeRaidGroupSize; other++ {
				if other == slot {
					continue
				}
				peer := &raid.members[squad*runtimeRaidGroupSize+other]
				if _, _, ok := relay.Endpoint(uint16(leaderState.PartyID), member.charID, peer.charID); !ok {
					t.Fatalf("squad %d relay leg %d->%d missing", squad, member.charID, peer.charID)
				}
			}
		}
	}

	// No simulated client's event loop may be parked by the formation burst.
	for _, member := range raid.members {
		if !raid.canStillWork(&member, 2*time.Second) {
			t.Fatalf("client %d stopped working during squad formation", member.charID)
		}
	}

	// The unprobed members were admitted through the relay table, and their op11
	// row carries a relay port inside the configured window.
	unprobed := &raid.members[runtimeRaidGroupSize-1]
	frame := unprobed.conn.bufferConn.write.Bytes()
	if len(frame) == 0 {
		t.Fatalf("unprobed member %d received no formation frame", unprobed.charID)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, frame)
	var peerRow []byte
	for _, packet := range packets {
		if packet.Header.Classification == 0 && packet.Header.MsgID == 0x000b {
			peerRow = packet.Body
			break
		}
	}
	if len(peerRow) == 0 || peerRow[0] == 0 {
		t.Fatalf("unprobed member %d op11 body=% X", unprobed.charID, peerRow)
	}
	relayPort := uint16(peerRow[11])<<8 | uint16(peerRow[12])
	if relayPort < uint16(portStart) || relayPort >= uint16(portStart+512) {
		t.Fatalf("unprobed member op11 port %d outside the relay window %d..%d",
			relayPort, portStart, portStart+511)
	}
}

// With the relay switched off the same 20-client shape must fall back to the
// bounded publish retry: the formation defers, an attempt is armed per party, and
// the attempt publishes as soon as the missing probe answer lands.
func TestRepro20RaidSquadFormationRetriesWithoutRelay(t *testing.T) {
	raid := newReproRaid(t, 20)
	defer raid.close()
	queue := newPartyPublishTestQueue()
	raid.service.gameplayTimers = queue

	for index := range raid.members {
		if index%runtimeRaidGroupSize != runtimeRaidGroupSize-1 {
			registerPartyPeerEndpointForTest(raid.members[index].session, 3, uint16(40000+index))
		}
	}
	var lastState alignedcmd.PartyState
	var lastMember *reproMember
	for squad := 0; squad < 5; squad++ {
		leader := &raid.members[squad*runtimeRaidGroupSize]
		for slot := 1; slot < runtimeRaidGroupSize; slot++ {
			member := &raid.members[squad*runtimeRaidGroupSize+slot]
			state, _, ok := raid.service.createOrJoinManagedRuntimeParty(member.session, leader.session)
			if !ok {
				t.Fatalf("squad %d slot %d join failed", squad, slot)
			}
			done, err := raid.service.publishManagedRuntimePartySnapshots(state)
			if err != nil {
				t.Fatalf("squad %d slot %d publish: %v", squad, slot, err)
			}
			if slot < runtimeRaidGroupSize-1 {
				// Every member so far has a probe answer, so the direct table is
				// complete and the formation must publish immediately.
				if !done {
					t.Fatalf("squad %d slot %d deferred although every member had probed", squad, slot)
				}
				continue
			}
			// The last member of the squad never probed and there is no relay: the
			// formation is deferred and the bounded retry is what completes it.
			if done {
				t.Fatalf("squad %d published with an unprobed member and no relay", squad)
			}
			lastState = state
			lastMember = member
		}
	}
	names := queue.namesMatching("party-snapshot-publish-retry:")
	if len(names) != 5*len(currentPartySnapshotPublishRetryDelays) {
		t.Fatalf("armed %d publish-retry attempts for 5 squads, want %d",
			len(names), 5*len(currentPartySnapshotPublishRetryDelays))
	}
	// The last unprobed member finally answers its reachability probe.
	registerPartyPeerEndpointForTest(lastMember.session, 3, 60999)
	target := ""
	for _, name := range names {
		if name == fmt.Sprintf("party-snapshot-publish-retry:%d:0", lastState.PartyID) {
			target = name
			break
		}
	}
	if target == "" {
		t.Fatalf("no retry armed for party %d: %v", lastState.PartyID, names)
	}
	queue.fire(t, target)
	if len(lastMember.conn.bufferConn.write.Bytes()) == 0 {
		t.Fatalf("retry did not publish the formation frame for member %d", lastMember.charID)
	}
	for _, member := range raid.members {
		if !raid.canStillWork(&member, 2*time.Second) {
			t.Fatalf("client %d stopped working during the deferred formation", member.charID)
		}
	}
}
