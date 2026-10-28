package dnfbridge

import (
	"context"
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// enterRuntimeRaidWaitingAreaPresenceForTest registers one session in the raid's
// waiting area exactly the way town_move.go does when the client walks through
// the team channel's portal: town + waiting area + InstanceKey == RaidKey.
//
// The start-time gate reads that registry, so every fixture that expects a start
// to be accepted has to place its members there.
func enterRuntimeRaidWaitingAreaPresenceForTest(
	t *testing.T,
	service *Service,
	session *gameSession,
	spec runtimeRaidTownSpec,
	raidKey uint32,
) {
	t.Helper()
	if service.onlinePlayers == nil {
		service.onlinePlayers = newOnlinePlayerManager()
	}
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: session.selectedCharacterID,
		TownID:      byte(spec.townID),
		AreaID:      spec.waitingArea,
		ChannelID:   townPresenceChannelID(session),
		InstanceKey: raidKey,
		Session:     session,
	})
}

func testAntonWaitingAreaSpec(t *testing.T) runtimeRaidTownSpec {
	t.Helper()
	spec, ok := runtimeRaidTownSpecForOwnerMode(runtimeRaidOwnerAnton)
	if !ok {
		t.Fatal("Anton owner has no waiting-area spec")
	}
	return spec
}

func testLukeWaitingAreaSpec(t *testing.T) runtimeRaidTownSpec {
	t.Helper()
	spec, ok := runtimeRaidTownSpecForOwnerMode(runtimeRaidOwnerLuke)
	if !ok {
		t.Fatal("Luke owner has no waiting-area spec")
	}
	return spec
}

// readCurrentClientNoticeText decodes the op413 type-4 notice envelope, so a test
// can assert the exact sentence a player would read.
func readCurrentClientNoticeText(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) < currentClientNoticeHeaderBytes {
		t.Fatalf("notice body too short: % X", body)
	}
	if body[0] != currentClientNoticeType {
		t.Fatalf("notice body type=%d want=%d (body=% X)", body[0], currentClientNoticeType, body)
	}
	header := currentClientNoticeHeaderBytes
	if body[1] == currentClientNoticeVersionStyled {
		header = currentClientNoticeStyledHeaderBytes
	}
	units := int(binary.LittleEndian.Uint16(body[2:4]))
	encoded := body[header:]
	decoded := make([]uint16, 0, units)
	for index := 0; index+1 < len(encoded) && len(decoded) < units; index += 2 {
		decoded = append(decoded, binary.LittleEndian.Uint16(encoded[index:index+2]))
	}
	return string(utf16.Decode(decoded))
}

// runtimeRaidStartNoticeTextsForTest decodes every client notice the session was
// sent, in wire order.
func runtimeRaidStartNoticeTextsForTest(t *testing.T, session *gameSession) []string {
	t.Helper()
	conn, ok := session.conn.(*bufferConn)
	if !ok {
		t.Fatalf("session conn is %T, want *bufferConn", session.conn)
	}
	texts := make([]string, 0, 2)
	rest := conn.write.Bytes()
	for len(rest) > 0 {
		packet, remainder := splitGameServerUpperPacket(t, rest)
		rest = remainder
		if packet.Header.MsgID != currentClientNoticeMsgID {
			continue
		}
		texts = append(texts, readCurrentClientNoticeText(t, packet.Body))
	}
	return texts
}

type runtimeRaidWaitingAreaStartFixture struct {
	service      *Service
	repositories dnfrepo.Group
	raidKey      uint32
	leader       *gameSession
	member       *gameSession
	leaderConn   *bufferConn
	memberConn   *bufferConn
}

func newRuntimeRaidWaitingAreaStartFixture(t *testing.T) runtimeRaidWaitingAreaStartFixture {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, characterID := range []string{"1001", "1002"} {
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterID,
			Slots: map[string]dnfrepo.ItemStack{
				"0:0": {ItemID: runtimeRaidEntryItemID, Count: 2},
			},
		}); err != nil {
			t.Fatalf("seed ticket for %s: %v", characterID, err)
		}
	}
	config := runtimeRaidBridgeTestAntonConfig()
	config.StartMinimumMembers = 2
	config.SingleStartMinimumMembers = 1
	config.StartDelaySeconds = 3600
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:              make(map[uint32]*runtimeRaidState),
		raidAnton:          config,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "raid", Port: 10038}
	leaderConn := &bufferConn{}
	memberConn := &bufferConn{}
	leader := &gameSession{conn: leaderConn, channel: channel}
	member := &gameSession{conn: memberConn, channel: channel}
	service.bindGameSessionCharacter(leader, 1001)
	service.bindGameSessionCharacter(member, 1002)
	raidKey := uint32(200)<<16 | 1001
	roster := []runtimeRaidMemberState{
		{CharID: 1001, Name: "leader", GroupIndex: 1, SlotOrder: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
		{CharID: 1002, Name: "member", GroupIndex: 1, SlotOrder: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
	}
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey:       raidKey,
		LeaderID:      1001,
		RaidMode:      runtimeRaidOwnerAnton,
		NameBytes:     []byte("anton"),
		Members:       append([]runtimeRaidMemberState(nil), roster...),
		FrozenMembers: append([]runtimeRaidMemberState(nil), roster...),
	}
	return runtimeRaidWaitingAreaStartFixture{
		service:      service,
		repositories: repositories,
		raidKey:      raidKey,
		leader:       leader,
		member:       member,
		leaderConn:   leaderConn,
		memberConn:   memberConn,
	}
}

// TestRuntimeRaidStartWaitsForEveryMemberInTheWaitingArea is the operator's rule
// end to end: a member standing on the public deck (same channel, same town,
// instance 0) refuses the start, no entry ticket is spent, and every member --
// not only the leader -- is told which member the start is waiting for.
func TestRuntimeRaidStartWaitsForEveryMemberInTheWaitingArea(t *testing.T) {
	fixture := newRuntimeRaidWaitingAreaStartFixture(t)
	spec := testAntonWaitingAreaSpec(t)

	// The leader is in the area, the member is not: the member's client is the one
	// that used to freeze 0.24 s after the phase activated (see
	// raid_start_waiting_area.go), so the start must be refused.
	enterRuntimeRaidWaitingAreaPresenceForTest(t, fixture.service, fixture.leader, spec, fixture.raidKey)
	fixture.service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: fixture.member.selectedCharacterID,
		TownID:      byte(currentAntonRaidTownID),
		AreaID:      1, // the public deck: Town/BattleShip/BattleShip_Main.map
		ChannelID:   townPresenceChannelID(fixture.member),
		InstanceKey: 0,
		Session:     fixture.member,
	})

	if err := fixture.service.handleOnlineStartRaid(
		fixture.leader,
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	); err != nil {
		t.Fatalf("refused start returned error: %v", err)
	}
	state := fixture.service.raids[fixture.raidKey]
	if state == nil || state.Started || state.StartPending || state.EntryCostPaid {
		t.Fatalf("start was accepted or left half-open: %+v", state)
	}
	if state.FrozenMembers != nil {
		t.Fatalf("refused start kept a frozen roster: %+v", state.FrozenMembers)
	}
	assertRuntimeRaidTicketCount(t, fixture.repositories, "1001", 2)
	assertRuntimeRaidTicketCount(t, fixture.repositories, "1002", 2)

	// Both members get the red reason, and it names the member that is missing.
	wantText := currentRaidStartWaitingAreaNoticePrefix + "<member>"
	for _, receiver := range []*gameSession{fixture.leader, fixture.member} {
		texts := runtimeRaidStartNoticeTextsForTest(t, receiver)
		if len(texts) == 0 {
			t.Fatalf("char %d received no start notice", receiver.selectedCharacterID)
		}
		found := false
		for _, text := range texts {
			if text == wantText {
				found = true
			}
		}
		if !found {
			t.Fatalf("char %d notices=%q want %q", receiver.selectedCharacterID, texts, wantText)
		}
	}

	// Walking the member into the waiting area is what unblocks the start.
	fixture.leaderConn.write.Reset()
	fixture.memberConn.write.Reset()
	enterRuntimeRaidWaitingAreaPresenceForTest(t, fixture.service, fixture.member, spec, fixture.raidKey)
	if err := fixture.service.handleOnlineStartRaid(
		fixture.leader,
		uint16(dnfenum.CmdPacketStartRaid),
		nil,
	); err != nil {
		t.Fatalf("start after the member walked in: %v", err)
	}
	state = fixture.service.raids[fixture.raidKey]
	if state == nil || !state.Started || !state.EntryCostPaid {
		t.Fatalf("start after the member walked in: %+v", state)
	}
	assertRuntimeRaidTicketCount(t, fixture.repositories, "1001", 1)
	assertRuntimeRaidTicketCount(t, fixture.repositories, "1002", 1)
}

// TestRuntimeRaidStartWaitingAreaTreatsMissingPresenceAsOutside pins both halves
// of the evidence rule: an ONLINE member whose presence row is missing (between
// scenes) is not provably in the area and must refuse the start, while a member
// with no bound session never blocks it.
func TestRuntimeRaidStartWaitingAreaTreatsMissingPresenceAsOutside(t *testing.T) {
	fixture := newRuntimeRaidWaitingAreaStartFixture(t)
	spec := testAntonWaitingAreaSpec(t)
	enterRuntimeRaidWaitingAreaPresenceForTest(t, fixture.service, fixture.leader, spec, fixture.raidKey)

	outside := fixture.service.runtimeRaidMembersOutsideWaitingArea(
		*fixture.service.raids[fixture.raidKey], spec)
	if len(outside) != 1 || outside[0].CharID != fixture.member.selectedCharacterID {
		t.Fatalf("outside=%+v want only the member without a presence row", outside)
	}

	// The member drops: a client that is not there cannot be pulled into the
	// activation burst, so it must not strand the raid.
	fixture.service.unbindGameSession(fixture.member)
	outside = fixture.service.runtimeRaidMembersOutsideWaitingArea(
		*fixture.service.raids[fixture.raidKey], spec)
	if len(outside) != 0 {
		t.Fatalf("offline member blocked the start: %+v", outside)
	}
}

// TestRuntimeRaidWaitingAreaNoticeNamesEveryMissingMember keeps the composed
// sentence stable: the fixed prefix (which the operator specified) followed by
// each missing member's name wrapped in <>, and a fallback name for a roster row
// that has none.
func TestRuntimeRaidWaitingAreaNoticeNamesEveryMissingMember(t *testing.T) {
	text := runtimeRaidWaitingAreaNoticeText([]runtimeRaidMemberState{
		{CharID: 1002, Name: "memberA"},
		{CharID: 1003},
	})
	want := currentRaidStartWaitingAreaNoticePrefix + "<memberA><角色1003>"
	if text != want {
		t.Fatalf("notice text=%q want %q", text, want)
	}
}
