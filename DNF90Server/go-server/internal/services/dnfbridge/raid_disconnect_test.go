package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

type runtimeRaidDisconnectFixture struct {
	service  *Service
	queue    *fakeCurrentDungeonDeathTimerQueue
	raidKey  uint32
	sessions map[uint16]*gameSession
}

func newRuntimeRaidDisconnectFixture(t *testing.T, memberIDs ...uint16) runtimeRaidDisconnectFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		onlinePlayers:  newOnlinePlayerManager(),
		gameplayTimers: queue,
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10200}
	fixture := runtimeRaidDisconnectFixture{
		service:  service,
		queue:    queue,
		raidKey:  0x00c80600,
		sessions: make(map[uint16]*gameSession, len(memberIDs)),
	}
	members := make([]runtimeRaidMemberState, 0, len(memberIDs))
	for i, id := range memberIDs {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, id)
		fixture.sessions[id] = session
		members = append(members, runtimeRaidMemberState{
			CharID:     id,
			GroupIndex: byte(i/runtimeRaidGroupSize) + 1,
			SlotOrder:  byte(i % runtimeRaidGroupSize),
			UserState:  runtimeRaidMemberUserStateOnline,
			HPPercent:  100,
			MPPercent:  100,
		})
	}
	service.raids[fixture.raidKey] = &runtimeRaidState{
		RaidKey:   fixture.raidKey,
		LeaderID:  memberIDs[0],
		TailFlag:  5,
		NameBytes: []byte("anton"),
		Members:   members,
	}
	return fixture
}

func (f runtimeRaidDisconnectFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	state := f.service.raids[f.raidKey]
	if state == nil {
		return runtimeRaidState{}
	}
	return cloneRuntimeRaidState(*state)
}

func (f runtimeRaidDisconnectFixture) userState(t *testing.T, id uint16) byte {
	t.Helper()
	state := f.state(t)
	index := runtimeRaidMemberIndex(state, id)
	if index < 0 {
		t.Fatalf("member %d is not in the roster: %+v", id, state.Members)
	}
	return state.Members[index].UserState
}

func (f runtimeRaidDisconnectFixture) disconnect(t *testing.T, id uint16) *fakeCurrentDungeonDeathTimerTask {
	t.Helper()
	before, _, _ := f.queue.counts()
	f.service.unbindGameSession(f.sessions[id])
	after, _, _ := f.queue.counts()
	if after != before+1 {
		t.Fatalf("disconnect of %d scheduled %d timers, want 1", id, after-before)
	}
	task := f.queue.task(t, after-1)
	if task.name != runtimeRaidDisconnectTimerName(f.raidKey, id) || task.delay != runtimeRaidDisconnectGrace {
		t.Fatalf("disconnect timer=%s delay=%s want=%s/%s", task.name, task.delay, runtimeRaidDisconnectTimerName(f.raidKey, id), runtimeRaidDisconnectGrace)
	}
	return task
}

func TestRuntimeRaidDisconnectReservesSeatAndMarksRowOffline(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	leaderConn := fixture.sessions[1001].conn.(*bufferConn)
	leaderConn.write.Reset()

	fixture.disconnect(t, 1002)

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1002) < 0 {
		t.Fatal("disconnect removed the member immediately")
	}
	if fixture.userState(t, 1002) != runtimeRaidMemberUserStateOffline {
		t.Fatal("disconnected member row was not marked offline")
	}
	if reservation, ok := fixture.service.raidDisconnects[1002]; !ok || reservation.RaidKey != fixture.raidKey {
		t.Fatalf("no disconnect reservation recorded: %+v", fixture.service.raidDisconnects)
	}
	// A dropped connection leaves the ordinary party, and the squad label is
	// derived from that party: the row goes back to standby. Operator rule
	// 2026-09-27: 「攻坚队被顶号 掉线 本来就要掉编制」. The member keeps his seat (and
	// his frozen phase row) inside the grace window -- only the live roster the
	// 攻击队 window draws is rewritten.
	if index := runtimeRaidMemberIndex(state, 1002); state.Members[index].GroupIndex != 0 {
		t.Fatalf("disconnected member kept squad %d, want standby 0", state.Members[index].GroupIndex)
	}
	packets := runtimeRaidPeerTake(t, fixture.sessions[1001])
	// The teardown publishes two refreshes: the squad clear, then the offline
	// row. The roster the survivors are left looking at is the last one.
	var refresh dnfproto.ChannelPacket
	found := false
	for _, packet := range packets {
		if packet.Header.MsgID == raid.RaidMemberRefreshMsgID {
			refresh, found = packet, true
		}
	}
	if !found {
		t.Fatal("remaining members received no roster refresh after the disconnect")
	}
	want, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		t.Fatal(err)
	}
	if string(refresh.Body) != string(want) {
		t.Fatalf("refresh body=%x want=%x", refresh.Body, want)
	}
}

func TestRuntimeRaidReconnectRestoresSeatAndCancelsGrace(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	task := fixture.disconnect(t, 1002)

	replacement := &gameSession{conn: &bufferConn{}, channel: fixture.sessions[1001].channel}
	fixture.service.bindGameSessionCharacter(replacement, 1002)
	fixture.service.restoreRuntimeRaidPresence(replacement, "test_reconnect")

	if fixture.userState(t, 1002) != runtimeRaidMemberUserStateOnline {
		t.Fatal("reconnected member row stayed offline")
	}
	if _, reserved := fixture.service.raidDisconnects[1002]; reserved {
		t.Fatal("reconnect left the disconnect reservation in place")
	}
	if _, cancelled, _ := fixture.queue.counts(); cancelled != 1 {
		t.Fatalf("reconnect cancelled %d timers, want 1", cancelled)
	}
	// A stale callback from the retired reservation must not remove anyone.
	fixture.queue.fire(task, true)
	if runtimeRaidMemberIndex(fixture.state(t), 1002) < 0 {
		t.Fatal("stale grace callback removed the reconnected member")
	}
}

func TestRuntimeRaidGraceExpiryRemovesUnstartedMemberAndPrefersConnectedLeader(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	// The leader and the next roster member both drop; only 1003 stays live.
	leaderTask := fixture.disconnect(t, 1001)
	fixture.disconnect(t, 1002)
	fixture.sessions[1003].conn.(*bufferConn).write.Reset()

	// The fake queue fires with due = now + delay, i.e. exactly at ExpiresAt.
	fixture.queue.fire(leaderTask, false)

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1001) >= 0 {
		t.Fatal("expired leader was not removed")
	}
	if state.LeaderID != 1003 {
		t.Fatalf("leader=%d want the connected survivor 1003 (1002 is still in its grace window)", state.LeaderID)
	}
	if _, reserved := fixture.service.raidDisconnects[1001]; reserved {
		t.Fatal("expired reservation was not cleared")
	}
	if _, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.sessions[1003]), raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("survivor received no roster refresh after the expiry")
	}
}

// The grace window is the whole of the drop rule: it runs out the same way for a
// started raid as for a recruiting one.
//
// This used to defer forever once StartPending/Started was set, because removal
// itself was refused for a started raid at the time. Now that a leave always goes
// through, the deferral would keep a seat nobody can use and leave the survivors
// looking at a member who is not coming back.
func TestRuntimeRaidGraceExpiryRemovesAStartedRaidMember(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	task := fixture.disconnect(t, 1002)
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Started = true
	fixture.service.raidMu.Unlock()

	fixture.queue.fire(task, false)

	if runtimeRaidMemberIndex(fixture.state(t), 1002) >= 0 {
		t.Fatal("the grace expiry left a started raid's dropped member in the roster")
	}
	if _, reserved := fixture.service.raidDisconnects[1002]; reserved {
		t.Fatal("the grace expiry left its reservation behind")
	}
	if fixture.state(t).LeaderID != 1001 {
		t.Fatalf("leader=%d want the surviving 1001", fixture.state(t).LeaderID)
	}
}

// A leader who never comes back hands the raid to the next member in roster order.
func TestRuntimeRaidGraceExpiryHandsTheRaidToTheNextMember(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	task := fixture.disconnect(t, 1001)

	fixture.queue.fire(task, false)

	state := fixture.state(t)
	if runtimeRaidMemberIndex(state, 1001) >= 0 {
		t.Fatal("the dropped leader stayed on the roster")
	}
	if state.LeaderID != 1002 {
		t.Fatalf("leader=%d want the next member in roster order 1002", state.LeaderID)
	}
}

func TestRuntimeRaidGraceExpiryDisbandsLastMember(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001)
	task := fixture.disconnect(t, 1001)

	fixture.queue.fire(task, false)

	fixture.service.raidMu.Lock()
	_, alive := fixture.service.raids[fixture.raidKey]
	fixture.service.raidMu.Unlock()
	if alive {
		t.Fatal("last member's grace expiry left an empty raid behind")
	}
	if len(fixture.service.raidDisconnects) != 0 {
		t.Fatalf("reservations survived the disband: %+v", fixture.service.raidDisconnects)
	}
}

// An explicit leave by the raid leader hands the raid to the next member in
// roster order; only a leave that empties the roster disbands it.
//
// Refusing the leave once the raid had started (the old StartPending/Started
// guard) left the leader inside a raid they could not leave and unable to create
// another one, while the old object stayed on screen listing them as its leader.
func TestRuntimeRaidExplicitLeaderLeaveHandsTheRaidToTheNextMember(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)
	fixture.disconnect(t, 1002)

	state, removed, disbanded := fixture.service.removeRuntimeRaidMember(1001)
	if !removed || disbanded {
		t.Fatalf("leader leave removed=%t disbanded=%t", removed, disbanded)
	}
	if state.LeaderID != 1003 {
		t.Fatalf("leader=%d want connected 1003 over disconnected 1002", state.LeaderID)
	}
	if len(state.Members) != 2 {
		t.Fatalf("leader leave left %d members: %+v", len(state.Members), state.Members)
	}
}

// The last member leaving still disbands.
func TestRuntimeRaidExplicitLastMemberLeaveDisbands(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001)

	state, removed, disbanded := fixture.service.removeRuntimeRaidMember(1001)
	if !removed || !disbanded {
		t.Fatalf("last member leave removed=%t disbanded=%t", removed, disbanded)
	}
	if state.RaidKey != 0 || len(fixture.service.raids) != 0 {
		t.Fatalf("an empty roster survived: %+v", fixture.service.raids)
	}
}

// A member who is not the leader leaves without touching the raid.
func TestRuntimeRaidExplicitMemberLeaveKeepsTheRaid(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002, 1003)

	state, removed, disbanded := fixture.service.removeRuntimeRaidMember(1002)
	if !removed || disbanded {
		t.Fatalf("member leave removed=%t disbanded=%t", removed, disbanded)
	}
	if state.LeaderID != 1001 {
		t.Fatalf("member leave changed the leader to %d", state.LeaderID)
	}
	if len(state.Members) != 2 {
		t.Fatalf("member leave left %d members: %+v", len(state.Members), state.Members)
	}
}
