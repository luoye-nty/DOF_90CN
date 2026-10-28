package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

func newRuntimeRaidSquadSyncFixture(t *testing.T, members []runtimeRaidMemberState) runtimeRaidDisconnectFixture {
	t.Helper()
	ids := make([]uint16, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.CharID)
	}
	fixture := newRuntimeRaidDisconnectFixture(t, ids...)
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Members = append([]runtimeRaidMemberState(nil), members...)
	fixture.service.raidMu.Unlock()
	return fixture
}

func (f runtimeRaidDisconnectFixture) group(t *testing.T, id uint16) byte {
	t.Helper()
	state := f.state(t)
	index := runtimeRaidMemberIndex(state, id)
	if index < 0 {
		t.Fatalf("member %d missing", id)
	}
	return state.Members[index].GroupIndex
}

func TestRuntimeRaidSquadFollowsOrdinaryPartyFormation(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 1002, GroupIndex: 2, SlotOrder: 0, UserState: 1},
		{CharID: 1003, GroupIndex: 3, SlotOrder: 0, UserState: 1},
	})
	fixture.sessions[1003].conn.(*bufferConn).write.Reset()

	// 1001 and 1002 form an ordinary party; a stranger (4242) in that party is
	// ignored. They end up in one squad, the one 1001 already had room in.
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 1002, 4242}) {
		t.Fatal("ordinary party formation did not change the raid roster")
	}
	if fixture.group(t, 1001) != fixture.group(t, 1002) || fixture.group(t, 1001) != 1 {
		t.Fatalf("squads after formation: 1001=%d 1002=%d", fixture.group(t, 1001), fixture.group(t, 1002))
	}
	if fixture.group(t, 1003) != 3 {
		t.Fatal("a member outside the ordinary party was moved")
	}
	assertRuntimeRaidGridUnique(t, fixture.state(t))
	if _, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, fixture.sessions[1003]), raid.RaidMemberRefreshMsgID); !ok {
		t.Fatal("other members received no roster refresh after the squad sync")
	}
	// Publishing the same party again is a no-op.
	if fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 1002}) {
		t.Fatal("an unchanged ordinary party re-labeled the squad")
	}
}

func TestRuntimeRaidSquadSyncRespectsCapacityAndFrozenRoster(t *testing.T) {
	// Squad 1 already holds three others, so 1001 cannot host the party there;
	// the partner's squad 3 has room and inherits the label.
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 1002, GroupIndex: 3, SlotOrder: 0, UserState: 1},
		{CharID: 2001, GroupIndex: 1, SlotOrder: 1, UserState: 1},
		{CharID: 2002, GroupIndex: 1, SlotOrder: 2, UserState: 1},
		{CharID: 2003, GroupIndex: 1, SlotOrder: 3, UserState: 1},
	})
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 1002}) {
		t.Fatal("party formation with a full home squad did not relabel")
	}
	if fixture.group(t, 1001) != 3 || fixture.group(t, 1002) != 3 {
		t.Fatalf("squads after capacity-limited formation: 1001=%d 1002=%d", fixture.group(t, 1001), fixture.group(t, 1002))
	}
	// Two members whose squads are both full end up in the lowest free squad.
	fixture.service.raidMu.Lock()
	fixture.service.raids[fixture.raidKey].Members = []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 2001, GroupIndex: 1, SlotOrder: 1, UserState: 1},
		{CharID: 2002, GroupIndex: 1, SlotOrder: 2, UserState: 1},
		{CharID: 2003, GroupIndex: 1, SlotOrder: 3, UserState: 1},
		{CharID: 1002, GroupIndex: 2, SlotOrder: 0, UserState: 1},
		{CharID: 1003, GroupIndex: 2, SlotOrder: 1, UserState: 1},
		{CharID: 2004, GroupIndex: 2, SlotOrder: 2, UserState: 1},
		{CharID: 2005, GroupIndex: 2, SlotOrder: 3, UserState: 1},
	}
	fixture.service.raidMu.Unlock()
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 1002, 1003}) {
		t.Fatal("party across two full squads did not relabel")
	}
	if fixture.group(t, 1001) != 3 || fixture.group(t, 1002) != 3 || fixture.group(t, 1003) != 3 {
		t.Fatalf("squads after both home squads were full: %d/%d/%d", fixture.group(t, 1001), fixture.group(t, 1002), fixture.group(t, 1003))
	}
	assertRuntimeRaidGridUnique(t, fixture.state(t))

	// During an active attack the raid window's own squad controls are frozen, but
	// an ordinary party still drags the roster: it is the only way left to pull a
	// member into a squad once the raid has started.
	fixture.service.raidMu.Lock()
	state := fixture.service.raids[fixture.raidKey]
	state.Started = true
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	fixture.service.raidMu.Unlock()
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1002, 2001}) {
		t.Fatal("active attack refused a squad relabel from an ordinary party")
	}
	active := fixture.state(t)
	live := active.Members[runtimeRaidMemberIndex(active, 2001)]
	frozen, _ := runtimeRaidFrozenMember(active, 2001)
	if live.GroupIndex != fixture.group(t, 1002) {
		t.Fatalf("invited member group=%d partner_group=%d", live.GroupIndex, fixture.group(t, 1002))
	}
	if live.GroupIndex != frozen.GroupIndex || live.SlotOrder != frozen.SlotOrder {
		t.Fatalf("the frozen roster did not follow the live one: live=%+v frozen=%+v", live, frozen)
	}
	assertRuntimeRaidGridUnique(t, active)

	// A settlement in progress has already frozen the reward roster, so the party
	// no longer reshuffles squads.
	fixture.service.raidMu.Lock()
	state = fixture.service.raids[fixture.raidKey]
	state.Lifecycle.Status = runtimeRaidLifecycleAwaitingRewards
	fixture.service.raidMu.Unlock()
	if fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 2002}) {
		t.Fatal("a settlement in progress accepted a squad relabel")
	}

	// In phase-one standby the projection resumes.
	fixture.service.raidMu.Lock()
	state.Lifecycle.Status = runtimeRaidLifecycleStandby
	fixture.service.raidMu.Unlock()
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 2002}) {
		t.Fatal("standby refused a squad relabel from an ordinary party")
	}
	after := fixture.state(t)
	standbyLive := after.Members[runtimeRaidMemberIndex(after, 2002)]
	standbyFrozen, _ := runtimeRaidFrozenMember(after, 2002)
	if standbyLive.GroupIndex != standbyFrozen.GroupIndex || standbyLive.SlotOrder != standbyFrozen.SlotOrder || standbyLive.GroupIndex != fixture.group(t, 1001) {
		t.Fatalf("live=%+v frozen=%+v partner_group=%d", standbyLive, standbyFrozen, fixture.group(t, 1001))
	}
	assertRuntimeRaidGridUnique(t, after)
}

func TestRuntimeRaidSquadSyncIgnoresCrossRaidParties(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 1002, GroupIndex: 2, SlotOrder: 0, UserState: 1},
	})
	otherKey := fixture.raidKey + 1
	fixture.service.raidMu.Lock()
	fixture.service.raids[otherKey] = &runtimeRaidState{
		RaidKey:  otherKey,
		LeaderID: 3001,
		TailFlag: 5,
		Members:  []runtimeRaidMemberState{{CharID: 3001, GroupIndex: 1, SlotOrder: 0, UserState: 1}},
	}
	fixture.service.raidMu.Unlock()
	if fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1001, 3001}) {
		t.Fatal("members of two different raids were relabeled through one ordinary party")
	}
	if fixture.group(t, 1001) != 1 {
		t.Fatal("cross-raid party moved a member")
	}
}

// TestRuntimeRaidSquadKeepsItsNumberWhenAKickedMemberIsStillLabelled is the live
// 2026-09-20 report "从1队踢了个人 然后由2队队长邀请进队 进了2队 然后2队踢出这个人
// 2队变3队了 这种情况应保持2队不变", raid 1507329:
//
//	13:12:02 game-raid-squad-synced-from-ordinary-party squad=2 members=[21 22 20]
//	13:12:09 game-raid-squad-synced-from-ordinary-party squad=3 members=[21 22]
//	13:12:09 game-raid-squad-unassigned-on-party-leave char_id=20 previous_group=2
//
// The kick clears the leaver's label, but the party snapshot is published first, so squad
// 2 still named him when the sync ran and the party was relocated. The party's own squad
// must win as long as the party still fits in it.
func TestRuntimeRaidSquadKeepsItsNumberWhenAKickedMemberIsStillLabelled(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 21, GroupIndex: 2, SlotOrder: 0, UserState: 1},
		{CharID: 22, GroupIndex: 2, SlotOrder: 1, UserState: 1},
		{CharID: 20, GroupIndex: 2, SlotOrder: 2, UserState: 1},
		{CharID: 1, GroupIndex: 1, SlotOrder: 0, UserState: 1},
	})
	// The sync must leave both members where they are: nothing to change, and above all
	// no relocation.
	fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{21, 22})
	if got := fixture.group(t, 21); got != 2 {
		t.Fatalf("the party moved to squad %d, want to keep squad 2", got)
	}
	if got := fixture.group(t, 22); got != 2 {
		t.Fatalf("the partner moved to squad %d, want to keep squad 2", got)
	}
	assertRuntimeRaidGridUnique(t, fixture.state(t))
}

// TestRuntimeRaidSquadKeepsTheLeadersNumberOverAnotherMembers pins the "队长则保持队伍号
// 不变" half of the rule, and that the leaderless wrapper still answers with the lowest
// squad the party holds.
func TestRuntimeRaidSquadKeepsTheLeadersNumberOverAnotherMembers(t *testing.T) {
	state := runtimeRaidState{Members: []runtimeRaidMemberState{
		{CharID: 21, GroupIndex: 3, SlotOrder: 0, UserState: 1},
		{CharID: 22, GroupIndex: 1, SlotOrder: 0, UserState: 1},
	}}
	if got, ok := runtimeRaidSquadForOrdinaryPartyLedBy(state, []uint16{21, 22}, 21); !ok || got != 3 {
		t.Fatalf("the leader's squad was not kept: squad=%d ok=%t, want 3", got, ok)
	}
	if got, ok := runtimeRaidSquadForOrdinaryParty(state, []uint16{21, 22}); !ok || got != 1 {
		t.Fatalf("without a leader the lowest held squad must win: squad=%d ok=%t, want 1", got, ok)
	}
}

// TestRuntimeRaidSquadForOrdinaryPartyFollowsTheOperatorsNumbering states the
// operator's numbering rules directly (2026-09-20):
//
//	"单人和单人的组队 应该找一个最小的数字组成新的小队 比如1 2队都被占用 就自动组成3队"
//	"1 2 3小队都被占用 2队解散了 原2队成员全部变单人 此时 3队不要动 还是3队 …
//	 再重新组队还是从2队开始"
//	"队长则保持队伍号不变"
func TestRuntimeRaidSquadForOrdinaryPartyFollowsTheOperatorsNumbering(t *testing.T) {
	stateWith := func(members ...runtimeRaidMemberState) runtimeRaidState {
		return runtimeRaidState{Members: members}
	}
	squad := func(id uint16, group byte) runtimeRaidMemberState {
		return runtimeRaidMemberState{CharID: id, GroupIndex: group, UserState: 1}
	}

	// Two unassigned solos: squad 1 is free, so they become squad 1. An unassigned
	// member occupies nothing -- folding GroupIndex 0 onto squad 1 is exactly what
	// used to drag every fresh pairing into squad 1.
	if got, ok := runtimeRaidSquadForOrdinaryParty(
		stateWith(squad(1, 0), squad(2, 0), squad(3, 0)),
		[]uint16{1, 2},
	); !ok || got != 1 {
		t.Fatalf("two solos landed in squad %d (ok=%t), want 1", got, ok)
	}

	// Squads 1 and 2 are held by two other parties: the new party becomes squad 3,
	// never a free slot inside somebody else's squad.
	if got, ok := runtimeRaidSquadForOrdinaryParty(
		stateWith(squad(11, 1), squad(12, 1), squad(21, 2), squad(31, 0), squad(32, 0)),
		[]uint16{31, 32},
	); !ok || got != 3 {
		t.Fatalf("a new party next to squads 1 and 2 landed in squad %d (ok=%t), want 3", got, ok)
	}

	// 1, 2 and 3 occupied and squad 2 disbands: squad 3 is left alone and the next
	// party starts again from the freed number 2.
	disbanded := stateWith(squad(11, 1), squad(21, 0), squad(22, 0), squad(31, 3), squad(41, 0), squad(42, 0))
	if got, ok := runtimeRaidSquadForOrdinaryParty(disbanded, []uint16{41, 42}); !ok || got != 2 {
		t.Fatalf("after squad 2 disbanded the next party landed in squad %d (ok=%t), want 2", got, ok)
	}
	// With every squad held by another party there is no home left: the roster is
	// left alone rather than folding the party into somebody else's squad.
	full := runtimeRaidState{}
	for group := byte(1); group <= runtimeRaidMaxGroups; group++ {
		full.Members = append(full.Members, runtimeRaidMemberState{
			CharID: uint16(100) + uint16(group), GroupIndex: group, UserState: 1,
		})
	}
	if got, ok := runtimeRaidSquadForOrdinaryParty(full, []uint16{901, 902}); ok {
		t.Fatalf("a party was folded into the occupied squad %d although every squad was taken", got)
	}

	// A party that already holds a squad keeps that number even though a lower one
	// is free ("队长则保持队伍号不变"), as long as no outsider stands in it.
	if got, ok := runtimeRaidSquadForOrdinaryParty(
		stateWith(squad(61, 2), squad(62, 2), squad(71, 4)),
		[]uint16{61, 62},
	); !ok || got != 2 {
		t.Fatalf("a party holding squad 2 moved to squad %d (ok=%t), want to stay in 2", got, ok)
	}

	// ...but a squad shared with an outsider is not the party's home: the party
	// moves out to the lowest squad nobody else occupies.
	if got, ok := runtimeRaidSquadForOrdinaryParty(
		stateWith(squad(61, 1), squad(62, 1), squad(63, 0), squad(64, 0)),
		[]uint16{63, 64},
	); !ok || got != 2 {
		t.Fatalf("a party joined a squad that already held an outsider (squad %d), want 2", got)
	}
}

// TestRuntimeRaidSquadSyncNeverFoldsANewPartyIntoAnOccupiedSquad is the live
// 2026-09-20 regression, raid 1507338:
//
//	10:56:51 game-raid-squad-synced-from-ordinary-party squad=1 members=[13 9]
//	10:56:55 game-raid-squad-synced-from-ordinary-party squad=1 members=[10 12]  ← wrong
//
// 13 and 9 were a different ordinary party and were standing in squad 1 when
// [10 12] formed, so the roster ended up claiming one squad for two parties --
// the operator's "多队伍成员错误显示为同属一支队伍".
func TestRuntimeRaidSquadSyncNeverFoldsANewPartyIntoAnOccupiedSquad(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 9, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 13, GroupIndex: 1, SlotOrder: 1, UserState: 1},
		{CharID: 10, GroupIndex: 0, SlotOrder: 0, UserState: 1},
		{CharID: 12, GroupIndex: 0, SlotOrder: 0, UserState: 1},
	})

	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{10, 12}) {
		t.Fatal("the new ordinary party did not change the raid roster")
	}
	if got := fixture.group(t, 10); got != 2 {
		t.Fatalf("the new party landed in squad %d, want squad 2 beside the party already in squad 1", got)
	}
	if got := fixture.group(t, 12); got != 2 {
		t.Fatalf("the partner landed in squad %d, want squad 2", got)
	}
	if fixture.group(t, 9) != 1 || fixture.group(t, 13) != 1 {
		t.Fatalf("the squad already holding a party was disturbed: 9=%d 13=%d",
			fixture.group(t, 9), fixture.group(t, 13))
	}
	assertRuntimeRaidGridUnique(t, fixture.state(t))
}

// TestRuntimeRaidSquadNumberSurvivesMemberDepartureAndIsReused pins the other half
// of the operator's rule: "被踢出的人变单人 而队长则保持队伍号不变 … 队长才会变单人
// 小队号这就空出来了", and the freed number is the first one a later party takes.
func TestRuntimeRaidSquadNumberSurvivesMemberDepartureAndIsReused(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 2, SlotOrder: 0, UserState: 1},
		{CharID: 1002, GroupIndex: 2, SlotOrder: 1, UserState: 1},
		{CharID: 2001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 3001, GroupIndex: 3, SlotOrder: 0, UserState: 1},
		{CharID: 4001, GroupIndex: 0, SlotOrder: 0, UserState: 1},
		{CharID: 4002, GroupIndex: 0, SlotOrder: 0, UserState: 1},
	})

	// The member walks out of the squad's party: only he becomes solo, the leader
	// keeps squad 2.
	if !fixture.service.unassignRuntimeRaidSquadOnPartyLeave(1002) {
		t.Fatal("the departing party member kept its squad label")
	}
	if got := fixture.group(t, 1001); got != 2 {
		t.Fatalf("the squad leader moved to squad %d, want to keep 2", got)
	}
	if got := fixture.group(t, 1002); got != 0 {
		t.Fatalf("the member who left the party stayed in squad %d, want solo (0)", got)
	}

	// A fresh party takes the lowest number nobody else occupies: 1, 2 and 3 are
	// all taken, so it becomes squad 4.
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{4001, 4002}) {
		t.Fatal("a fresh party of two solos did not change the raid roster")
	}
	if got := fixture.group(t, 4001); got != 4 {
		t.Fatalf("a fresh party took squad %d while 1/2/3 were occupied, want 4", got)
	}

	// The leader leaves too: squad 2 is now empty and the next party reuses it.
	if !fixture.service.unassignRuntimeRaidSquadOnPartyLeave(1001) {
		t.Fatal("the squad leader kept its squad label after leaving the party")
	}
	if got := fixture.group(t, 1001); got != 0 {
		t.Fatalf("the leader who left alone stayed in squad %d, want solo (0)", got)
	}
	if !fixture.service.syncRuntimeRaidSquadsWithOrdinaryParty([]uint16{1002, 1001}) {
		t.Fatal("the party re-formed around the freed squad number")
	}
	if got := fixture.group(t, 1001); got != 2 {
		t.Fatalf("the re-formed party took squad %d, want the freed 2", got)
	}
	if got := fixture.group(t, 1002); got != 2 {
		t.Fatalf("the partner joined squad %d, want 2", got)
	}
	assertRuntimeRaidGridUnique(t, fixture.state(t))
}

// TestRuntimeRaidSquadClearedOnSessionLeaveInsideAGate pins the deliberate
// difference between the two clears. An ordinary party leave is REFUSED while the
// member stands inside a raid gate, because the party object there is republished
// by the entry/barrier machinery rather than by a player gesture. A connection
// going away is a real leave even inside one: its gate run is released a few
// statements later in the same teardown (noteRuntimeRaidSessionDisconnected ->
// releaseRuntimeRaidRunsOwnedByLocked), so the guard would make every raid
// disconnect a no-op. Operator rule 2026-09-27: 「攻坚队被顶号 掉线 本来就要掉编制」.
func TestRuntimeRaidSquadClearedOnSessionLeaveInsideAGate(t *testing.T) {
	fixture := newRuntimeRaidSquadSyncFixture(t, []runtimeRaidMemberState{
		{CharID: 1001, GroupIndex: 1, SlotOrder: 0, UserState: 1},
		{CharID: 1002, GroupIndex: 1, SlotOrder: 1, UserState: 1},
	})
	service := fixture.service
	service.raidMu.Lock()
	state := service.raids[fixture.raidKey]
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	service.raidMu.Unlock()

	// 1002 is fighting inside one of the raid's own gates.
	session := fixture.sessions[1002]
	session.dungeon.mu.Lock()
	runtime := &runtimeDungeonState{}
	runtime.Character.CharacterID = "1002"
	runtime.raidDungeon.RaidKey = fixture.raidKey
	session.dungeon.runtime = runtime
	session.dungeon.mu.Unlock()

	// The party-leave variant refuses: nothing about that gesture says he left.
	if service.unassignRuntimeRaidSquadOnPartyLeave(1002) {
		t.Fatal("the party-leave clear unassigned a member standing inside a gate")
	}
	if got := fixture.group(t, 1002); got != 1 {
		t.Fatalf("group=%d after the refused clear, want 1", got)
	}

	// A connection going away is a leave regardless of where he stands.
	if !service.unassignRuntimeRaidSquadOnSessionLeave(1002, "test_session_leave") {
		t.Fatal("the session-leave clear did not unassign a member inside a gate")
	}
	if got := fixture.group(t, 1002); got != 0 {
		t.Fatalf("group=%d after the session leave, want standby 0", got)
	}
	if got := fixture.group(t, 1001); got != 1 {
		t.Fatalf("the squadmate lost its squad: %d", got)
	}
	// Live row only: the frozen phase roster keeps crediting him while he still
	// holds a seat inside the disconnect grace window.
	after := fixture.state(t)
	if frozen, found := runtimeRaidFrozenMember(after, 1002); !found || frozen.GroupIndex != 1 {
		t.Fatalf("frozen roster=%+v found=%t, want the phase row kept at squad 1", frozen, found)
	}
	if !service.unassignRuntimeRaidSquadOnSessionLeave(1002, "test_session_leave_again") {
		// Already standby: a second teardown must simply report no change.
		return
	}
	t.Fatal("an already standby member was unassigned twice by the session leave")
}
