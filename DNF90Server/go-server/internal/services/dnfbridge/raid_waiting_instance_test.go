package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestOnlinePlayerManagerSeparatesAntonWaitingInstancesByRaidKey(t *testing.T) {
	manager := newOnlinePlayerManager()
	const (
		firstRaidKey  = uint32(0x00170001)
		secondRaidKey = uint32(0x00170002)
	)
	first := &onlinePlayerInfo{CharacterID: 1001, TownID: byte(currentAntonRaidTownID), AreaID: currentAntonRaidWaitingAreaID, InstanceKey: firstRaidKey}
	ally := &onlinePlayerInfo{CharacterID: 1002, TownID: byte(currentAntonRaidTownID), AreaID: currentAntonRaidWaitingAreaID, InstanceKey: firstRaidKey}
	otherRaid := &onlinePlayerInfo{CharacterID: 2001, TownID: byte(currentAntonRaidTownID), AreaID: currentAntonRaidWaitingAreaID, InstanceKey: secondRaidKey}

	if peers := manager.EnterArea(first); len(peers) != 0 {
		t.Fatalf("first raid member saw unexpected waiting peers: %+v", peers)
	}
	if peers := manager.EnterArea(ally); len(peers) != 1 || peers[0].CharacterID != first.CharacterID {
		t.Fatalf("same raid waiting peers=%+v", peers)
	}
	if peers := manager.EnterArea(otherRaid); len(peers) != 0 {
		t.Fatalf("different raid leaked into waiting peers: %+v", peers)
	}
	if !manager.PeerInSameArea(first.CharacterID, ally.CharacterID) {
		t.Fatal("same raid waiting members are not co-present")
	}
	if manager.PeerInSameArea(first.CharacterID, otherRaid.CharacterID) {
		t.Fatal("different raid waiting members are visible to each other")
	}
	if got := manager.GetAreaInstancePlayers(
		0, byte(currentAntonRaidTownID), currentAntonRaidWaitingAreaID, firstRaidKey); len(got) != 2 {
		t.Fatalf("first raid waiting roster=%+v", got)
	}
	if got := manager.GetAreaInstancePlayers(
		0, byte(currentAntonRaidTownID), currentAntonRaidWaitingAreaID, secondRaidKey); len(got) != 1 || got[0].CharacterID != otherRaid.CharacterID {
		t.Fatalf("second raid waiting roster=%+v", got)
	}
}

func TestCurrentRaidTownSpecSeparatesAntonAndLukeChannels(t *testing.T) {
	tests := []struct {
		name        string
		channelType byte
		want        runtimeRaidTownSpec
		ok          bool
	}{
		{
			name:        "anton",
			channelType: dnfenum.RaidChannelType,
			want: runtimeRaidTownSpec{
				townID: currentAntonRaidTownID, areaID: currentAntonRaidAreaID,
				waitingArea: currentAntonRaidWaitingAreaID, ownerName: "anton",
			},
			ok: true,
		},
		{
			name:        "luke",
			channelType: dnfenum.HiddenRaidType,
			want: runtimeRaidTownSpec{
				townID: currentLukeRaidTownID, areaID: currentLukeRaidAreaID,
				waitingArea: currentLukeRaidWaitingAreaID, ownerName: "luke",
			},
			ok: true,
		},
		{name: "ordinary", channelType: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &gameSession{channel: channelcatalog.Channel{Type: test.channelType}}
			got, ok := currentRaidTownSpecForSession(session)
			if ok != test.ok || got != test.want {
				t.Fatalf("raid town spec=(%+v,%t), want=(%+v,%t)", got, ok, test.want, test.ok)
			}
		})
	}
}

// TestCurrentRaidTownSpecPinsFiendWarChannelToItsOwnTown covers the third raid
// owner.  Before this, a type-42 channel fell through currentRaidTownSpecForSession
// to the ordinary-town default, so entering one of the three `[fiendwar]` channels
// landed the character in whatever town their record held rather than town 98 --
// and all 22 call sites that ask for the spec agreed on that wrong answer.
//
// Town 98 is `180621_Cosmofiend.twn` (town/town.lst); its area 0 is
// `GContents/180621_Fiendwar/Town/Gate_Fiend.map` with gate (474,234), the same
// gate coordinate the Anton and Luke raid towns publish.
func TestCurrentRaidTownSpecPinsFiendWarChannelToItsOwnTown(t *testing.T) {
	session := &gameSession{channel: channelcatalog.Channel{ID: 184, Type: dnfenum.FiendWarRaidChannelType}}
	got, ok := currentRaidTownSpecForSession(session)
	want := runtimeRaidTownSpec{
		townID: currentFiendWarRaidTownID, areaID: currentFiendWarRaidAreaID,
		waitingArea: currentFiendWarWaitingAreaID, ownerName: "fiendwar",
	}
	if !ok || got != want {
		t.Fatalf("fiendwar raid town spec=(%+v,%t), want=(%+v,%t)", got, ok, want, true)
	}
	if !isCurrentFiendWarRaidChannel(session) || !isCurrentImplementedRaidChannel(session) {
		t.Fatal("fiendwar channel is not recognised as an implemented raid channel")
	}
	// The three raid owners must stay disjoint.  Adding Fiend War must not move
	// an Anton or Luke channel off its own town, and must not classify either as
	// Fiend War.
	for _, owner := range []struct {
		name        string
		channelType byte
		townID      int64
	}{
		{name: "anton", channelType: dnfenum.RaidChannelType, townID: currentAntonRaidTownID},
		{name: "luke", channelType: dnfenum.HiddenRaidType, townID: currentLukeRaidTownID},
	} {
		peer := &gameSession{channel: channelcatalog.Channel{Type: owner.channelType}}
		spec, found := currentRaidTownSpecForSession(peer)
		if !found || spec.townID != owner.townID {
			t.Fatalf("%s town spec=(%+v,%t), want town %d", owner.name, spec, found, owner.townID)
		}
		if isCurrentFiendWarRaidChannel(peer) {
			t.Fatalf("channel type %d was classified as Fiend War", owner.channelType)
		}
	}
}

func TestRuntimeLukeRaidSelectorAdmissionRequiresPrivateWaitingInstance(t *testing.T) {
	const (
		raidKey = uint32(0x00200001)
		charID  = uint16(1001)
	)
	session := &gameSession{
		conn:                &bufferConn{},
		channel:             channelcatalog.Channel{ID: 241, Type: dnfenum.HiddenRaidType, Name: "luke", Port: 10241},
		selectedCharacterID: charID,
	}
	state := &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      charID,
		RaidMode:      runtimeRaidOwnerLuke,
		Started:       true,
		EntryCostPaid: true,
		Members:       []runtimeRaidMemberState{{CharID: charID, GroupIndex: 1, SlotOrder: 0}},
		FrozenMembers: []runtimeRaidMemberState{{CharID: charID, GroupIndex: 1, SlotOrder: 0}},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive,
			Phase:  runtimeRaidDungeonPhaseOne,
		},
	}
	service := &Service{
		raids:         map[uint32]*runtimeRaidState{raidKey: state},
		onlinePlayers: newOnlinePlayerManager(),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: charID,
		TownID:      byte(currentLukeRaidTownID),
		AreaID:      currentLukeRaidWaitingAreaID,
		InstanceKey: raidKey,
		Session:     session,
	})

	if admitted, allowed, reason := service.runtimeRaidSelectorAdmission(session); !allowed || reason != "raid_phase_active" || admitted.RaidKey != raidKey {
		t.Fatalf("Luke private waiting admission allowed=%t reason=%q state=%+v", allowed, reason, admitted)
	}

	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: charID,
		TownID:      byte(currentLukeRaidTownID),
		AreaID:      1,
		Session:     session,
	})
	if _, allowed, reason := service.runtimeRaidSelectorAdmission(session); allowed || reason != "raid_member_not_in_private_waiting_instance" {
		t.Fatalf("Luke public lobby admission allowed=%t reason=%q", allowed, reason)
	}
}

func TestRuntimeRaidSelectorAdmissionRequiresActivePrivateWaitingInstance(t *testing.T) {
	const (
		raidKey = uint32(0x00170001)
		charID  = uint16(1001)
	)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		channel:             channelcatalog.Channel{ID: 23, Type: 23, Name: "anton", Port: 10023},
		selectedCharacterID: charID,
	}
	state := &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  charID,
		Members:   []runtimeRaidMemberState{{CharID: charID, GroupIndex: 1, SlotOrder: 0}},
		Lifecycle: runtimeRaidLifecycleState{Status: runtimeRaidLifecycleCreated},
	}
	service := &Service{
		raids:         map[uint32]*runtimeRaidState{raidKey: state},
		onlinePlayers: newOnlinePlayerManager(),
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: charID,
		TownID:      byte(currentAntonRaidTownID),
		AreaID:      currentAntonRaidWaitingAreaID,
		InstanceKey: raidKey,
		Session:     session,
	})

	if _, allowed, reason := service.runtimeRaidSelectorAdmission(session); allowed || reason != "raid_not_started" {
		t.Fatalf("created raid admission allowed=%t reason=%q", allowed, reason)
	}
	if err := service.sendEnterSelectDungeonState(session, "test_unstarted_raid", false, true); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unstarted raid selector wrote %d bytes", connection.write.Len())
	}

	state.Started = true
	state.EntryCostPaid = true
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	state.Lifecycle.Status = runtimeRaidLifecyclePreparing
	if _, allowed, reason := service.runtimeRaidSelectorAdmission(session); allowed || reason != "raid_phase_not_active" {
		t.Fatalf("preparing raid admission allowed=%t reason=%q", allowed, reason)
	}

	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: charID,
		TownID:      byte(currentAntonRaidTownID),
		AreaID:      1,
		Session:     session,
	})
	if _, allowed, reason := service.runtimeRaidSelectorAdmission(session); allowed || reason != "raid_member_not_in_private_waiting_instance" {
		t.Fatalf("public lobby admission allowed=%t reason=%q", allowed, reason)
	}

	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: charID,
		TownID:      byte(currentAntonRaidTownID),
		AreaID:      currentAntonRaidWaitingAreaID,
		InstanceKey: raidKey,
		Session:     session,
	})
	if admitted, allowed, reason := service.runtimeRaidSelectorAdmission(session); !allowed || reason != "raid_phase_active" || admitted.RaidKey != raidKey {
		t.Fatalf("active private waiting admission allowed=%t reason=%q state=%+v", allowed, reason, admitted)
	}
}

func TestRuntimeRaidStateIsSharedAcrossRaidChannelIDs(t *testing.T) {
	const raidKey = uint32(0x00170001)
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids: map[uint32]*runtimeRaidState{
			raidKey: {
				RaidKey:   raidKey,
				LeaderID:  1001,
				NameBytes: []byte("cross-channel"),
				Members:   []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
			},
		},
	}
	joining := &gameSession{
		conn:    &bufferConn{},
		channel: channelcatalog.Channel{ID: 200, Type: dnfenum.RaidChannelType, Name: "anton-200", Port: 10200},
	}
	service.bindGameSessionCharacter(joining, 1002)
	request := make([]byte, 4)
	binary.LittleEndian.PutUint32(request, raidKey)
	if err := service.handleOnlineRejoinRaid(joining, uint16(dnfenum.CmdPacketRejoinRaid), request); err != nil {
		t.Fatal(err)
	}
	state := service.raids[raidKey]
	if state == nil || runtimeRaidMemberIndex(*state, 1002) < 0 {
		t.Fatalf("channel 200 did not join raid created under channel 23: %+v", state)
	}
}

// The 0x0279 raid Buff body publishes the party topology that gives the client
// its team-channel Buff registration owner, and it must not register a single
// active row: each row's two u32 are activation deadlines, so a row means "this
// buff is running for that member".
//
// Live report 2026-09-16: the player entered a gate already carrying a damage
// buff. The bodies this server sent until then attached one row per member with
// both deadlines zero, which 86JP would read as an ATTACK BONUS activation (its
// group byte is a buff type: 1 = ATTACK BONUS). The rows are therefore gone while
// the group/type byte itself is still being proven against the 90 client -- do not
// re-add member rows to a registration, and do not empty the group count: that
// leaves the client with no Buff registration owner at all.
func TestRuntimeRaidBuffRegistrationKeepsTopologyWithoutActiveRows(t *testing.T) {
	state := runtimeRaidState{
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
			{CharID: 1002, GroupIndex: 1, SlotOrder: 1},
			{CharID: 2001, GroupIndex: 2, SlotOrder: 0},
		},
	}
	body := mustRuntimeRaidBuffRegistrationSnapshotBody(t, state)
	// The 90 client's slot byte is the table key (sub_15C6AD0 walks five fixed
	// slots), so the registration must name all five with rowCount 0. A row means
	// "running", and only resultType-style deadlines above `now` ever light up:
	// rowCount 0 ends in the clean false return at 0x017B5869.
	wantSlots := []byte{2, 3, 4, 1, 0}
	if len(body) != 1+2*len(wantSlots) || body[0] != byte(len(wantSlots)) {
		t.Fatalf("Buff registration header len=%d body=%x", len(body), body)
	}
	for index, slot := range wantSlots {
		if body[1+index*2] != slot || body[2+index*2] != 0 {
			t.Fatalf("Buff registration slot %d=%d rows=%d want slot=%d rows=0 body=%x",
				index, body[1+index*2], body[2+index*2], slot, body)
		}
	}
}
