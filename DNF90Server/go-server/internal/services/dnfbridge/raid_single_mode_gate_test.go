package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/raid"
)

// Single mode is enabled by the selected owner's loaded PVF, not by a
// channel-name special case. A team-only Fiend War archive therefore still
// fails closed, while the optional fiendwar_single.etc overlay enables it.
func TestRuntimeRaidOwnerSupportsSingleRaidUsesLoadedConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  runtimeRaidOwnerConfig
		want bool
	}{
		{name: "missing raid config", cfg: runtimeRaidOwnerConfig{}, want: false},
		{name: "team-only", cfg: runtimeRaidOwnerConfig{Raid: &raid.AntonConfig{}}, want: false},
		{name: "single enabled", cfg: runtimeRaidOwnerConfig{Raid: &raid.AntonConfig{SingleMode: &raid.AntonSingleModeConfig{}}}, want: true},
	}
	for _, tc := range cases {
		if got := runtimeRaidOwnerSupportsSingleRaid(tc.cfg); got != tc.want {
			t.Errorf("%s: runtimeRaidOwnerSupportsSingleRaid()=%t want %t",
				tc.name, got, tc.want)
		}
	}
}

func TestRuntimeRaidSingleRequestedMatchesTailFlagOne(t *testing.T) {
	if !runtimeRaidSingleRequested(1) {
		t.Fatal("tail_flag=1 must read as the single raid request")
	}
	if runtimeRaidSingleRequested(0) || runtimeRaidSingleRequested(2) {
		t.Fatal("only tail_flag=1 is the single raid request")
	}
}

// newRaidSingleGateTestSession builds the minimum both handlers touch before
// their gates: a bound connection, a selected character and a channel type.
func newRaidSingleGateTestSession(service *Service, channelType uint8, characterID uint16) *gameSession {
	session := &gameSession{
		conn:    &bufferConn{},
		connID:  "raid-single-gate",
		channel: channelcatalog.Channel{ServerID: 1, ID: 23, Type: channelType, Name: "gate", Port: 10023},
	}
	service.bindGameSessionCharacter(session, characterID)
	return session
}

// TestHandleOnlineCreateRaidRefusesFiendWarSingleRaid pins the create-side gate:
// on a Fiend War channel the single create is refused and no raid object is
// allocated, so the leader can never end up inside an unstartable raid.
func TestHandleOnlineCreateRaidRefusesFiendWarSingleRaid(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	session := newRaidSingleGateTestSession(service, dnfenum.FiendWarRaidChannelType, 1001)

	err := service.handleOnlineCreateRaid(
		session,
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(2, []byte("fiendwar-single"), 1),
	)
	if err != nil {
		t.Fatalf("refused Fiend War single create returned transport error: %v", err)
	}
	if len(service.raids) != 0 {
		t.Fatalf("refused Fiend War single create still allocated %d raid(s)", len(service.raids))
	}
	if session.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("refused Fiend War single create sent nothing back to the client")
	}
}

func TestHandleOnlineCreateRaidAcceptsFiendWarSingleWhenPVFEnablesIt(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
		raidFiendWar: &raid.FiendWarConfig{
			Battle: &raid.FiendWarBattleConfig{
				Raid: &raid.AntonConfig{
					PhaseMax: 1,
					SingleMode: &raid.AntonSingleModeConfig{
						Name:             "single",
						TimeLimitSeconds: 1800,
						Phase:            raid.AntonPhase{Dungeons: []raid.AntonDungeonInfo{{Index: 4953, HP: 1}}},
					},
				},
			},
		},
	}
	session := newRaidSingleGateTestSession(service, dnfenum.FiendWarRaidChannelType, 1001)

	err := service.handleOnlineCreateRaid(
		session,
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(2, []byte("fiendwar-single"), 1),
	)
	if err != nil {
		t.Fatalf("enabled Fiend War single create: %v", err)
	}
	if len(service.raids) != 1 {
		t.Fatalf("enabled Fiend War single create produced %d raid(s), want 1", len(service.raids))
	}
	for _, state := range service.raids {
		if state == nil || state.TailFlag != 1 || state.RaidMode != runtimeRaidOwnerFiendWar {
			t.Fatalf("Fiend War single state=%+v", state)
		}
	}
}

// TestHandleOnlineCreateRaidStillAcceptsAntonSingleRaid is the "do not break A
// while fixing B" nail: Anton's single raid is a real, PVF-backed mode and must
// keep reaching the create path through the same handler.
func TestHandleOnlineCreateRaidStillAcceptsAntonSingleRaid(t *testing.T) {
	service := &Service{
		options:   options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:     make(map[uint32]*runtimeRaidState),
		raidAnton: runtimeRaidBridgeTestAntonConfig(),
	}
	session := newRaidSingleGateTestSession(service, dnfenum.RaidChannelType, 1001)

	err := service.handleOnlineCreateRaid(
		session,
		uint16(dnfenum.CmdPacketCreateRaid),
		buildRaidInfoRequestForBridgeTest(0, []byte("anton-single"), 1),
	)
	if err != nil {
		t.Fatalf("Anton single create returned transport error: %v", err)
	}
	if len(service.raids) != 1 {
		t.Fatalf("Anton single create produced %d raid(s) want 1", len(service.raids))
	}
	for _, state := range service.raids {
		if state == nil || state.TailFlag != 1 || state.RaidMode != runtimeRaidOwnerAnton {
			t.Fatalf("Anton single raid state=%+v", state)
		}
	}
}

// TestHandleOnlineStartRaidRefusesFiendWarSingleRaid pins the start-side gate for
// a raid object that already carries TailFlag=1 (an older save, or one created
// before the create-side refusal landed). The start must be refused before the
// reservation, so nothing is debited and the raid is left untouched -- which is
// what stops the object from becoming unkillable.
func TestHandleOnlineStartRaidRefusesFiendWarSingleRaid(t *testing.T) {
	service := &Service{
		options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:   make(map[uint32]*runtimeRaidState),
	}
	session := newRaidSingleGateTestSession(service, dnfenum.FiendWarRaidChannelType, 1001)

	raidKey := uint32(23)<<16 | 1001
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:         raidKey,
		LeaderID:        1001,
		RaidMode:        runtimeRaidOwnerFiendWar,
		RouteOrRaidType: 2,
		TailFlag:        1,
		Members:         []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
	}

	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatalf("handleOnlineStartRaid returned transport error: %v", err)
	}
	state := service.raids[raidKey]
	if state == nil {
		t.Fatal("start gate removed the raid object; it should be left untouched")
	}
	if state.StartPending {
		t.Fatal("start gate still armed StartPending; the raid is on its way to the unkillable state")
	}
	if len(state.FrozenMembers) != 0 {
		t.Fatalf("start gate froze %d member(s) before refusing", len(state.FrozenMembers))
	}
	if state.Started || state.EntryCostPaid {
		t.Fatalf("start gate let the raid start: started=%t entry_cost_paid=%t", state.Started, state.EntryCostPaid)
	}
	if state.StartAttempt != 0 {
		t.Fatalf("start gate consumed start attempt %d; a refused start must not advance it", state.StartAttempt)
	}
	if session.conn.(*bufferConn).write.Len() == 0 {
		t.Fatal("refused Fiend War single start sent nothing back to the client")
	}
}
