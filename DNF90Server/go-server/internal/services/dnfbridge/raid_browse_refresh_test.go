package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// runtimeRaidBrowseFixture models the reported defect: a leader and one member
// hold a recruiting team raid, and a third character has the attack-party window
// open on that raid without having joined it.
type runtimeRaidBrowseFixture struct {
	service *Service
	queue   *fakeCurrentDungeonDeathTimerQueue
	raidKey uint32
	leader  *gameSession
	member  *gameSession
	outside *gameSession
}

func newRuntimeRaidBrowseFixture(t *testing.T) runtimeRaidBrowseFixture {
	t.Helper()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{
		options:        options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:          make(map[uint32]*runtimeRaidState),
		onlinePlayers:  newOnlinePlayerManager(),
		gameplayTimers: queue,
	}
	channel := channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 1023}
	newSession := func(id uint16) *gameSession {
		session := &gameSession{conn: &bufferConn{}, channel: channel}
		service.bindGameSessionCharacter(session, id)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{CharacterID: id, TownID: 19, AreaID: 0, Session: session})
		return session
	}
	fixture := runtimeRaidBrowseFixture{
		service: service,
		queue:   queue,
		raidKey: 0x00170001,
		leader:  newSession(1001),
		member:  newSession(1002),
		outside: newSession(1003),
	}
	// TailFlag 0 marks the 20-member team raid, which is the only shape the
	// browsing window can be open on.
	service.raids[fixture.raidKey] = &runtimeRaidState{
		RaidKey:   fixture.raidKey,
		LeaderID:  1001,
		TailFlag:  0,
		NameBytes: []byte("anton"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1, UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100},
		},
	}
	return fixture
}

// browse opens the outsider's attack-party window through the real op1359 path.
func (f runtimeRaidBrowseFixture) browse(t *testing.T) {
	t.Helper()
	f.outside.conn.(*bufferConn).write.Reset()
	if err := f.service.sendRuntimeRaidBrowseList(f.outside, raid.RequestRaidInfoRequest{RaidKey: f.raidKey, Mode: 1}); err != nil {
		t.Fatalf("browse: %v", err)
	}
	if f.outside.raidBrowseTarget != f.raidKey {
		t.Fatalf("browse target=%d want %d", f.outside.raidBrowseTarget, f.raidKey)
	}
	runtimeRaidPeerTake(t, f.outside)
}

func (f runtimeRaidBrowseFixture) state(t *testing.T) runtimeRaidState {
	t.Helper()
	f.service.raidMu.Lock()
	defer f.service.raidMu.Unlock()
	state := f.service.raids[f.raidKey]
	if state == nil {
		return runtimeRaidState{}
	}
	return cloneRuntimeRaidState(*state)
}

// moveMember drives the real 669 subgroup-edit request, so the test covers the
// leader handler and the broadcast it performs rather than the state mutation alone.
func (f runtimeRaidBrowseFixture) moveMember(t *testing.T, memberID uint16, group byte) {
	t.Helper()
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:4], runtimeRaidManagerWorkAssignGroup)
	binary.LittleEndian.PutUint32(body[4:8], uint32(memberID))
	binary.LittleEndian.PutUint32(body[8:12], uint32(group))
	if err := f.service.handleOnlineRaidManagerWork(f.leader, uint16(dnfenum.CmdPacketRaidManagerWork), body); err != nil {
		t.Fatalf("669 regroup: %v", err)
	}
	state := f.state(t)
	index := runtimeRaidMemberIndex(state, memberID)
	if index < 0 || state.Members[index].GroupIndex != group {
		t.Fatalf("member %d was not moved to group %d: %+v", memberID, group, state.Members)
	}
}

func TestRuntimeRaidBrowseRefreshPushesRegroupToOpenWindow(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	fixture.browse(t)

	// The leader regroups the second member while the outsider keeps the window
	// open. Before the browse subscription existed this produced nothing at all
	// for a non-member and the outsider only saw it after reopening the window.
	fixture.moveMember(t, 1002, 3)

	packets := runtimeRaidPeerTake(t, fixture.outside)
	refresh, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID)
	if !ok {
		t.Fatal("browsing outsider received no 0x024F roster after the regroup")
	}
	want, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(fixture.state(t)))
	if err != nil {
		t.Fatal(err)
	}
	if string(refresh.Body) != string(want) {
		t.Fatalf("browsed roster body=%x want=%x", refresh.Body, want)
	}
	// A pure regroup keeps the member id set, so the identity half must not be
	// replayed: the outsider already holds every actor record and key list row.
	for _, msgID := range []uint16{raid.RaidWaitingListMsgID, raid.RaidOtherChannelUserinfoMsgID, raid.RaidEntryCostMsgID} {
		if _, resent := runtimeRaidPeerFind(packets, msgID); resent {
			t.Fatalf("regroup replayed identity packet %d", msgID)
		}
	}
}

func TestRuntimeRaidBrowseRefreshSkipsClosedWindowAndMembers(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	fixture.browse(t)

	// A closed window stops polling op1359, so its subscription ages out.
	fixture.outside.raidBrowseAt = fixture.queue.Now().Add(-2 * runtimeRaidBrowseTTL)
	fixture.leader.conn.(*bufferConn).write.Reset()
	fixture.member.conn.(*bufferConn).write.Reset()
	fixture.outside.conn.(*bufferConn).write.Reset()

	fixture.moveMember(t, 1002, 4)

	if packets := runtimeRaidPeerTake(t, fixture.outside); len(packets) != 0 {
		ids := make([]uint16, 0, len(packets))
		for _, packet := range packets {
			ids = append(ids, packet.Header.MsgID)
		}
		t.Fatalf("stale browse subscription received %d packets: %v", len(packets), ids)
	}
	// Members still get exactly one mode=3 roster push -- the browse pass must not
	// duplicate the packet for a member that also happens to browse its own raid.
	// The mode=0 raid object that op669 now publishes alongside it is a different
	// packet on the same opcode: the current client materializes the attack-party
	// panel from mode=0, so the leader's own window only moves when it is re-sent.
	for _, member := range []*gameSession{fixture.leader, fixture.member} {
		packets := runtimeRaidPeerTake(t, member)
		refreshes, objects := 0, 0
		for _, packet := range packets {
			if packet.Header.MsgID != raid.RaidMemberRefreshMsgID {
				continue
			}
			if len(packet.Body) >= 8 && binary.LittleEndian.Uint32(packet.Body[4:8]) == raid.RaidMemberRefreshMode3 {
				refreshes++
				continue
			}
			objects++
		}
		if refreshes != 1 || objects != 1 {
			t.Fatalf("member received %d mode3 rosters and %d mode0 objects, want 1 and 1", refreshes, objects)
		}
	}
}

func TestRuntimeRaidBrowseRefreshReplaysIdentityWhenMemberSetChanges(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	fixture.browse(t)

	// An admission changes the member id set, so the new row has no key record on
	// the browsing client and would render as a nameless level-0 entry unless the
	// identity half is replayed with it.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Members = append(
		fixture.service.raids[fixture.raidKey].Members,
		runtimeRaidMemberState{CharID: 1004, GroupIndex: 2, SlotOrder: 0, UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100},
	)
	state := cloneRuntimeRaidState(*fixture.service.raids[fixture.raidKey])
	fixture.service.raidMu.Unlock()

	if err := fixture.service.broadcastRuntimeRaidRefresh(state); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	packets := runtimeRaidPeerTake(t, fixture.outside)
	if _, ok := runtimeRaidPeerFind(packets, raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("browsing outsider received no roster after the admission")
	}
	waiting, ok := runtimeRaidPeerFind(packets, raid.RaidWaitingListMsgID)
	if !ok {
		t.Fatal("browsing outsider received no 0x0250 key list after the admission")
	}
	if want := runtimeRaidWaitingListBody(state); string(waiting.Body) != string(want) {
		t.Fatalf("waiting body=%x want=%x", waiting.Body, want)
	}
	userinfo, ok := runtimeRaidPeerFind(packets, raid.RaidOtherChannelUserinfoMsgID)
	if !ok {
		t.Fatal("browsing outsider received no 819 key list after the admission")
	}
	if want := raid.BuildOtherChannelUserinfoBody([]uint32{1001, 1002, 1004}); string(userinfo.Body) != string(want) {
		t.Fatalf("819 body=%x want=%x", userinfo.Body, want)
	}
}

func TestRuntimeRaidBrowseTargetClearedWhenRaidIsNoLongerOpen(t *testing.T) {
	fixture := newRuntimeRaidBrowseFixture(t)
	fixture.browse(t)

	// Once the raid starts it leaves the directory, so the next poll must drop the
	// subscription instead of leaving the outsider on a stale roster.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].StartPending = true
	fixture.service.raidMu.Unlock()

	if err := fixture.service.sendRuntimeRaidBrowseList(fixture.outside, raid.RequestRaidInfoRequest{RaidKey: fixture.raidKey, Mode: 1}); err != nil {
		t.Fatalf("browse after close: %v", err)
	}
	if fixture.outside.raidBrowseTarget != 0 || !fixture.outside.raidBrowseAt.IsZero() {
		t.Fatalf("browse subscription survived a closed raid: target=%d at=%s", fixture.outside.raidBrowseTarget, fixture.outside.raidBrowseAt)
	}
}

func TestRuntimeRaidMemberFingerprintTracksMemberSetOnly(t *testing.T) {
	base := runtimeRaidState{Members: []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
		{CharID: 1002, GroupIndex: 1, SlotOrder: 1},
	}}
	regrouped := runtimeRaidState{Members: []runtimeRaidMemberState{
		{CharID: 1002, GroupIndex: 3, SlotOrder: 0},
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
	}}
	if runtimeRaidMemberFingerprint(base) != runtimeRaidMemberFingerprint(regrouped) {
		t.Fatal("a subgroup move changed the member id fingerprint")
	}
	grown := runtimeRaidState{Members: append(append([]runtimeRaidMemberState(nil), base.Members...), runtimeRaidMemberState{CharID: 1003})}
	if runtimeRaidMemberFingerprint(base) == runtimeRaidMemberFingerprint(grown) {
		t.Fatal("an admission left the member id fingerprint unchanged")
	}
}
