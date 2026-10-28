package dnfbridge

import (
	"net"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The two bodies a remote peer is installed with must carry that peer's own actor
// key: mode0 writes it after its fixed 0x47-byte actor state (offset 76) and mode1
// after its 21-byte prefix. This is the contract sendTownRemoteActorState now
// enforces before it sends anything, and it is what makes "the body belongs to the
// installed character" a checked property instead of an assumption -- the log line
// that event emits prints its arguments, not the encoded bytes.
func TestRemoteActorBodiesCarryTheInstalledCharactersOwnKey(t *testing.T) {
	character := dnfrepo.CharacterRecord{
		CharacterID: "1002",
		Name:        "peer",
		Level:       90,
		Job:         "0",
		Stats:       map[string]int64{"grow_type": 1},
	}
	mode0 := buildCurrentSceneObjectListBodyInContext(1002, character, true, "peer", 23)
	mode1 := buildCurrentActorBindingMode1BodyInContext(1002, 0, 23)

	ownerServer, ownerChannel, err := validateCurrentPartyActorProjectionRows(1002, mode0, mode1)
	if err != nil {
		t.Fatalf("the installed character's own key was rejected: %v", err)
	}
	if ownerServer != mode0[3] || ownerChannel != 23 {
		t.Fatalf("owner server=%d channel=%d want the channel-owned context 23", ownerServer, ownerChannel)
	}
	// Claiming the body belongs to another character must be rejected: that is the
	// shape in which the client binds a peer's skill pages to its own actor.
	if _, _, err := validateCurrentPartyActorProjectionRows(1001, mode0, mode1); err == nil {
		t.Fatal("a body built for another character was accepted as the installed one")
	}
}

// A member whose gate run is still open is inside a dungeon instance, and that is
// the one client state a channel-context actor body must not be installed into.
// A completed run no longer counts, so the boundary that clears a gate keeps
// installing.
func TestRuntimeRaidMemberInsideOpenDungeonRunOnlyCountsOpenRuns(t *testing.T) {
	state := runtimeRaidState{Lifecycle: newRuntimeRaidLifecycleState(time.Now().UTC())}
	key := runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}
	state.Lifecycle.Dungeons[key] = runtimeRaidDungeonProgress{
		Phase:          runtimeRaidDungeonPhaseOne,
		MapID:          210,
		Seat:           runtimeRaidDungeonSeat{Group: 1},
		CharacterID:    1001,
		LifecycleToken: 11,
		Status:         runtimeRaidDungeonProgressSelected,
	}
	if !runtimeRaidMemberInsideOpenDungeonRun(state, 1001) {
		t.Fatal("a selected gate run did not mark its owner as inside the instance")
	}
	if runtimeRaidMemberInsideOpenDungeonRun(state, 1002) {
		t.Fatal("a member with no run of its own was marked as inside the instance")
	}
	state.Lifecycle.Dungeons[key] = runtimeRaidDungeonProgress{
		Phase:          runtimeRaidDungeonPhaseOne,
		MapID:          210,
		Seat:           runtimeRaidDungeonSeat{Group: 1},
		CharacterID:    1001,
		LifecycleToken: 11,
		Status:         runtimeRaidDungeonProgressCompleted,
	}
	if runtimeRaidMemberInsideOpenDungeonRun(state, 1001) {
		t.Fatal("a completed run still marked its owner as inside the instance")
	}
}

// The install mode is the receiving client's own scene combined with the raid's
// run status, and it selects how much of the peer record travels -- never whether
// it travels at all.
//
// A completed run no longer proves the client left the instance: live
// 2026-09-17T07:14:02.843Z installed character 22 into character 1 with
// target_local_owner=0 while 210 was still being rendered, and that client's own
// settlement entry (op31) followed 85ms later. The other boundary runs the other
// way: a gate that was just selected has not moved its client yet, so its
// committed scene owner is still the channel.
//
// Both of those clients still need the actor record -- the member rendering the
// Anton instance is exactly the client whose roster portraits were the default
// ones because nothing created the peers in its user table. Only the mode1
// binding, which carries the peer's skill pages, is withheld there.
func TestRuntimeRaidPeerActorInstallModeFollowsTargetClientScene(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	state := runtimeRaidState{Lifecycle: newRuntimeRaidLifecycleState(time.Now().UTC())}
	key := runtimeRaidDungeonRunKey{MapID: 210, Seat: runtimeRaidDungeonSeat{Group: 1}}

	// Inside the instance: townActorOwnerChannel was reset by the dungeon entry
	// (resetDungeonEntrySceneGates) and the run is already Completed.
	insideInstance := &gameSession{conn: serverConn, selectedCharacterID: 1001}
	insideInstance.channel.ID = 23
	if runtimeRaidTargetSceneChannelOwned(insideInstance) {
		t.Fatal("a client whose committed scene owner is 0 was treated as channel-owned")
	}
	state.Lifecycle.Dungeons[key] = runtimeRaidDungeonProgress{
		Phase:          runtimeRaidDungeonPhaseOne,
		MapID:          210,
		Seat:           runtimeRaidDungeonSeat{Group: 1},
		CharacterID:    1001,
		LifecycleToken: 11,
		Status:         runtimeRaidDungeonProgressCompleted,
	}
	if mode := runtimeRaidPeerActorInstallModeFor(state, insideInstance); mode != runtimeRaidPeerActorInstallRecord {
		t.Fatalf("a client painting its settlement got mode=%d, want the record-only install", mode)
	}

	// Waiting area: the channel-owned scene is committed and no run is open.
	waitingArea := &gameSession{conn: clientConn, selectedCharacterID: 1002}
	waitingArea.channel.ID = 23
	waitingArea.townActorOwnerChannel = 23
	// This used to be the one shape that took the full (mode0+mode1) install. It
	// used to be the one shape that took the full (mode0+mode1) install. It no
	// longer does: live raid 1507329 (8 members, own build) measured the SAME
	// fan-out at 1.0 s per client with mode1 and 0.37 s without it -- all eight
	// sessions frozen 981ms..1.025s in the waiting area, then 351..377ms on the
	// same loops five minutes later once the installs were record-only -- and the
	// operator's rule is that too much mode1 on the wire stutters the client. The
	// town co-presence path dropped mode1 on 2026-09-26 for the same reason and
	// peers render fine there.
	if !runtimeRaidTargetSceneChannelOwned(waitingArea) {
		t.Fatal("the channel-owned town scene owner was rejected")
	}
	if mode := runtimeRaidPeerActorInstallModeFor(state, waitingArea); mode != runtimeRaidPeerActorInstallRecord {
		t.Fatalf("a member standing in the waiting area got mode=%d, want the record-only install", mode)
	}

	// The opposite boundary: run Selected, client has not entered yet. Its scene
	// is still the channel, so the record goes in -- the mode1 binding does not.
	state.Lifecycle.Dungeons[key] = runtimeRaidDungeonProgress{
		Phase:          runtimeRaidDungeonPhaseOne,
		MapID:          210,
		Seat:           runtimeRaidDungeonSeat{Group: 1},
		CharacterID:    1002,
		LifecycleToken: 11,
		Status:         runtimeRaidDungeonProgressSelected,
	}
	if mode := runtimeRaidPeerActorInstallModeFor(state, waitingArea); mode != runtimeRaidPeerActorInstallRecord {
		t.Fatalf("a member whose gate was already selected got mode=%d, want the record-only install", mode)
	}
}
