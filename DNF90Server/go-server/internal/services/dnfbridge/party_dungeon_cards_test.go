package dnfbridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRuntimePartyDungeonCardSelectionUsesStableSlotAndSynchronizesPeers(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	sourceConn := &bufferConn{}
	peerConn := &bufferConn{}
	source := &gameSession{conn: sourceConn, selectedCharacterID: 1001}
	peer := &gameSession{conn: peerConn, selectedCharacterID: 1002}
	service.bindGameSessionCharacter(source, 1001)
	service.bindGameSessionCharacter(peer, 1002)
	owner := &runtimePartyDungeonOwner{
		partyID: 2, leaderID: 1001, leaderSlot: 0,
		members: []runtimePartyDungeonMember{
			{slot: 0, userID: 1001, generation: source.characterGeneration},
			{slot: 1, userID: 1002, generation: peer.characterGeneration},
		},
	}
	sourceRuntime := &runtimeDungeonState{
		partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 0,
	}
	peerRuntime := &runtimeDungeonState{
		partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: 1,
		settlementCardLayoutSent: true,
	}
	source.dungeon.runtime = sourceRuntime
	peer.dungeon.runtime = peerRuntime

	if got := runtimePartyDungeonCardMemberIndex(peerRuntime); got != 1 {
		t.Fatalf("peer stable card slot=%d want=1", got)
	}
	body := []byte{1, 0, 0xff, 0, 0}
	if err := service.publishRuntimePartyDungeonCardSelection(
		source, sourceRuntime, dungeonCardSideFree, 0, body,
	); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, peerConn.write.Bytes())
	if packet.Header.Classification != currentDungeonCardResponseClass ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketSelectCard) ||
		!bytes.Equal(packet.Body, body) || len(trailing) != 0 {
		t.Fatalf("peer card projection class=%d msg=%d body=%x trailing=%x",
			packet.Header.Classification, packet.Header.MsgID, packet.Body, trailing)
	}
	if sourceConn.write.Len() != 0 {
		t.Fatalf("source received duplicate public card projection=%x", sourceConn.write.Bytes())
	}

	if err := service.publishRuntimePartyDungeonCardSelection(
		source, sourceRuntime, dungeonCardSideFree, 0, body,
	); err != nil {
		t.Fatal(err)
	}
	if peerConn.write.Len() == 0 {
		t.Fatal("peer card projection unexpectedly disappeared")
	}
	_, duplicate := splitGameServerUpperPacket(t, peerConn.write.Bytes())
	if len(duplicate) != 0 {
		t.Fatalf("duplicate card selection emitted trailing packet=%x", duplicate)
	}

	peerConn.write.Reset()
	peerRuntime.settlementCardLayoutSent = false
	queuedBody := []byte{1, 0xff, 0, 0, 0}
	if err := service.publishRuntimePartyDungeonCardSelection(
		source, sourceRuntime, dungeonCardSidePaid, 0, queuedBody,
	); err != nil {
		t.Fatal(err)
	}
	if peerConn.write.Len() != 0 || len(peerRuntime.partyCardSelectionPending) != 1 {
		t.Fatalf("pre-layout projection bytes=%x pending=%d", peerConn.write.Bytes(), len(peerRuntime.partyCardSelectionPending))
	}
	peerRuntime.settlementCardLayoutSent = true
	if err := service.flushRuntimePartyDungeonCardSelectionsLocked(peer, peerRuntime); err != nil {
		t.Fatal(err)
	}
	queued, trailing := splitGameServerUpperPacket(t, peerConn.write.Bytes())
	if queued.Header.MsgID != uint16(dnfenum.CmdPacketSelectCard) ||
		!bytes.Equal(queued.Body, queuedBody) || len(trailing) != 0 ||
		len(peerRuntime.partyCardSelectionPending) != 0 {
		t.Fatalf("queued card projection msg=%d body=%x trailing=%x pending=%d",
			queued.Header.MsgID, queued.Body, trailing, len(peerRuntime.partyCardSelectionPending))
	}
}

func TestCurrentDungeonSelectedCardSlotsPreserveRemoteMemberRow(t *testing.T) {
	runtime := &runtimeDungeonState{
		settlementCardRewardState: &dungeonCardState{
			plan: dungeonCardRewardPlan{
				Sides: [dungeonCardSideCount]dungeonCardRewardBundle{
					{Gold: 19},
					{Gold: 27},
				},
			},
		},
		partyDungeonOwner: &runtimePartyDungeonOwner{
			partyID: 2,
			members: []runtimePartyDungeonMember{
				{slot: 0, userID: 1001, generation: 1},
				{slot: 1, userID: 1002, generation: 1},
			},
		},
		settlementCardSideSelectionKnown: [dungeonCardSideCount]bool{true, false},
		settlementCardSideMember:         [dungeonCardSideCount]byte{1, 1},
	}

	slots, rewardTupleCount := currentDungeonSelectedCardSlots(runtime)
	if slots[1].StateA != 1 || slots[1].StateB != 0xff {
		t.Fatalf("2P card row state=%d/%d want=1/255", slots[1].StateA, slots[1].StateB)
	}
	if len(slots[1].Rewards) != 0 {
		t.Fatalf("2P free public reward=%+v", slots[1].Rewards)
	}
	if rewardTupleCount != 0 {
		t.Fatalf("free op71 tuple count=%d want=0; free rewards belong to op35", rewardTupleCount)
	}
	for index, slot := range slots {
		if index == 1 {
			continue
		}
		if slot.StateA != 0xff || slot.StateB != 0xff || len(slot.Rewards) != 0 {
			t.Fatalf("unselected card row %d=%+v", index, slot)
		}
	}
	runtime.settlementCardSideSelectionKnown[dungeonCardSidePaid] = true
	slots, count := currentDungeonSelectedCardSlots(runtime)
	if slots[1].StateA != 1 || slots[1].StateB != 1 || count != 1 ||
		!reflect.DeepEqual(slots[1].Rewards, []currentDungeonOp71RewardTuple{{ValueA: 0, ValueB: 27}}) {
		t.Fatalf("paid response must use StateB and type3 tuples: %+v count=%d", slots[1], count)
	}
}

func newPartyCardResultTest(t *testing.T, slots ...byte) (*Service, *runtimePartyDungeonOwner, []*gameSession) {
	t.Helper()
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	owner := &runtimePartyDungeonOwner{partyID: 9}
	var sessions []*gameSession
	for i, slot := range slots {
		id := uint16(1001 + i)
		session := &gameSession{conn: &bufferConn{}, selectedCharacterID: id}
		service.bindGameSessionCharacter(session, id)
		owner.members = append(owner.members, runtimePartyDungeonMember{slot: slot, userID: id, generation: session.characterGeneration})
		character := dnfrepo.CharacterRecord{CharacterID: fmt.Sprint(id), Name: fmt.Sprint(id), Level: 5, Job: "0", Stats: map[string]int64{}}
		runtime := &runtimeDungeonState{Character: character, partyDungeonOwner: owner, partyMemberIndexed: true, partyMemberIndex: slot,
			settlementPlayResultReceived: true, settlementPhase: currentDungeonSettlementPhaseClearEnabled,
			settlementPlayResultBody: make([]byte, currentDungeonPlayResultBaseSize)}
		runtime.settlementCardRewardState = &dungeonCardState{plan: dungeonCardRewardPlan{
			CharacterID: character.CharacterID,
			Sides:       [dungeonCardSideCount]dungeonCardRewardBundle{{Gold: int64(10 + i)}, {Gold: int64(100 + i)}},
		}}
		reward := currentDungeonClearRewardSnapshot{CharacterID: id, CompletionKey: "party-test-clear", Source: "committed_test", Committed: true,
			Base: [4]uint32{uint32(70 + i)}, CardSlots: currentDungeonClearRewardCardSlotsFromRuntime(runtime),
			Tail: currentDungeonClearRewardTail{ShowResult: true}}
		// Give each member a distinct black-diamond result and deliberately
		// different unused rows, as in the captured private op35 snapshots.
		for j := 0; j < dungeonCardSlotsPerSide; j++ {
			reward.AuxGroupsB[j] = []currentDungeonClearRewardPair{{Key: uint32(30000 + i*10 + j), Value: 1}}
		}
		plan, err := buildCurrentDungeonSettlementPacketPlan(0, currentDungeonPlayResultNotice{
			Participants: []currentDungeonPlayResultParticipant{{ObjectKey: id}},
		}, reward, character, dnfrepo.SkillPointState{})
		if err != nil {
			t.Fatal(err)
		}
		runtime.settlementResultPlan = &plan
		session.dungeon.runtime = runtime
		sessions = append(sessions, session)
	}
	owner.leaderID, owner.leaderSlot = owner.members[len(slots)-1].userID, slots[len(slots)-1]
	return service, owner, sessions
}

func readPartyCardResultTables(t *testing.T, session *gameSession) [3][8][]currentDungeonClearRewardPair {
	t.Helper()
	data := session.conn.(*bufferConn).write.Bytes()
	var body []byte
	for _, opcode := range []uint16{34, 37, 35} {
		packet, rest := splitGameServerUpperPacket(t, data)
		if packet.Header.MsgID != opcode {
			t.Fatalf("result order: got %d want %d", packet.Header.MsgID, opcode)
		}
		body, data = packet.Body, rest
	}
	if len(data) != 0 {
		t.Fatalf("unexpected packets after op35: %x", data)
	}
	r := settlementProtocolReader{body: body, offset: 17 + 22*4}
	for i := 0; i < 2; i++ {
		r.offset += int(r.u8()) * 8
	}
	r.offset += 7*4 + 4*4
	r.offset += int(r.u32()) * 8
	r.offset += int(r.u8()) * 15
	var tables [3][8][]currentDungeonClearRewardPair
	for kind := range tables {
		if kind == 1 {
			r.u32()
		}
		for slot := range tables[kind] {
			count := int(r.u8())
			for i := 0; i < count; i++ {
				tables[kind][slot] = append(tables[kind][slot], currentDungeonClearRewardPair{Key: r.u32(), Value: r.u32()})
			}
		}
	}
	if r.offset+26 != len(body) {
		t.Fatalf("op35 reader boundary=%d body=%d", r.offset, len(body))
	}
	return tables
}

func TestPartyCardResultBarrierSharesNativeMemberTables(t *testing.T) {
	for _, slots := range [][]byte{{0, 1}, {1, 3}, {0, 1, 2, 3}} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			service, owner, sessions := newPartyCardResultTest(t, slots...)
			for i := len(sessions) - 1; i >= 0; i-- {
				if err := service.sendCurrentDungeonSettlementResultsLocked(sessions[i], sessions[i].dungeon.runtime, "test"); err != nil {
					t.Fatal(err)
				}
				if i > 0 {
					for _, session := range sessions {
						if session.conn.(*bufferConn).write.Len() != 0 {
							t.Fatal("op35 sent before every member's reward was ready")
						}
					}
				}
			}
			var want [3][8][]currentDungeonClearRewardPair
			for i, slot := range slots {
				want[0][slot] = []currentDungeonClearRewardPair{{Key: 0, Value: uint32(10 + i)}}
				want[2][slot] = []currentDungeonClearRewardPair{{Key: uint32(30000 + i*10 + int(slot)), Value: 1}}
			}
			for _, session := range sessions {
				if got := readPartyCardResultTables(t, session); !reflect.DeepEqual(got, want) {
					t.Fatalf("char=%d tables=%+v want=%+v", session.selectedCharacterID, got, want)
				}
				written := session.conn.(*bufferConn).write.Len()
				if err := service.sendCurrentDungeonSettlementResultsLocked(session, session.dungeon.runtime, "replay"); err != nil {
					t.Fatal(err)
				}
				if session.conn.(*bufferConn).write.Len() != written {
					t.Fatal("replayed settlement duplicated reward UI")
				}
			}
			if !owner.cardRewardsFrozen {
				t.Fatal("public table was not frozen")
			}
		})
	}
}

func TestPartyCardResultBarrierReleasesOnDisconnectAndRejectsStaleMember(t *testing.T) {
	service, _, sessions := newPartyCardResultTest(t, 1, 3)
	first, leaving := sessions[0], sessions[1]
	if err := service.sendCurrentDungeonSettlementResultsLocked(first, first.dungeon.runtime, "first"); err != nil {
		t.Fatal(err)
	}
	leaving.characterGeneration++
	if _, err := service.prepareRuntimePartyDungeonCardRewardsLocked(leaving, leaving.dungeon.runtime); err == nil {
		t.Fatal("stale generation published reward")
	}
	leaving.characterGeneration--
	service.releaseDisconnectedDungeonAttempt(leaving, "test_disconnect_before_op46")
	tables := readPartyCardResultTables(t, first)
	if len(tables[0][1]) != 1 || len(tables[0][3]) != 0 {
		t.Fatalf("disconnect fabricated/moved rewards: %+v", tables)
	}
	if leaving.conn.(*bufferConn).write.Len() != 0 {
		t.Fatal("departed member received settlement")
	}
}

func TestRuntimePartyDungeonCardSelectionWriteFailureKeepsRetry(t *testing.T) {
	service, _, sessions := newPartyCardResultTest(t, 0, 1)
	source, peer := sessions[0], sessions[1]
	wantErr := errors.New("peer write failed")
	body := []byte{1, 0, 255, 0, 0}
	peer.dungeon.runtime.settlementCardLayoutSent = true
	peer.conn = &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	if err := service.publishRuntimePartyDungeonCardSelection(source, source.dungeon.runtime, dungeonCardSideFree, 0, body); !errors.Is(err, wantErr) {
		t.Fatalf("first send=%v", err)
	}
	if peer.dungeon.runtime.partyCardSelectionSeen[dungeonCardSideFree][0] {
		t.Fatal("failed send marked peer notification delivered")
	}
	peer.conn = &bufferConn{}
	if err := service.publishRuntimePartyDungeonCardSelection(source, source.dungeon.runtime, dungeonCardSideFree, 0, body); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
	if !bytes.Equal(packet.Body, body) || len(rest) != 0 {
		t.Fatalf("retry body=%x rest=%x", packet.Body, rest)
	}
	peer.dungeon.runtime.settlementCardLayoutSent = false
	if err := service.publishRuntimePartyDungeonCardSelection(source, source.dungeon.runtime, dungeonCardSidePaid, 0, body); err != nil {
		t.Fatal(err)
	}
	peer.dungeon.runtime.settlementCardLayoutSent = true
	peer.conn = &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	if err := service.flushRuntimePartyDungeonCardSelectionsLocked(peer, peer.dungeon.runtime); !errors.Is(err, wantErr) {
		t.Fatalf("queued send=%v", err)
	}
	if len(peer.dungeon.runtime.partyCardSelectionPending) != 1 {
		t.Fatal("failed send discarded queued flip")
	}
	peer.conn = &bufferConn{}
	if err := service.flushRuntimePartyDungeonCardSelectionsLocked(peer, peer.dungeon.runtime); err != nil {
		t.Fatal(err)
	}
	if len(peer.dungeon.runtime.partyCardSelectionPending) != 0 {
		t.Fatal("successful retry retained queued flip")
	}
}

func TestPartyCardClickedPositionsSynchronizeImmediatelyAndRewardTheirOwners(t *testing.T) {
	service, owner, sessions := newPartyCardResultTest(t, 0, 1)
	repos := dnfrepomemory.NewMemoryGroup()
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repos, true }
	ctx := context.Background()
	for _, session := range sessions {
		runtime := session.dungeon.runtime
		runtime.settlementCardLayoutSent = true
		runtime.settlementPhase = currentDungeonSettlementPhaseCardsRevealed
		plan, err := newDungeonCardRewardPlan(dungeonCardPlanIdentity{CharacterID: runtime.Character.CharacterID, DungeonID: 3},
			"party_screen_test", runtime.settlementCardRewardState.plan.Sides[0], runtime.settlementCardRewardState.plan.Sides[1])
		if err != nil {
			t.Fatal(err)
		}
		runtime.settlementCardRewardState, err = newDungeonCardState(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := repos.Character.Save(ctx, runtime.Character); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: runtime.Character.CharacterID, Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: runtime.Character.CharacterID, Entries: map[string]dnfrepo.EquipmentEntry{}}); err != nil {
			t.Fatal(err)
		}
	}
	reset := func() {
		for _, s := range sessions {
			s.conn.(*bufferConn).write.Reset()
		}
	}
	click := func(member, side, slot byte) {
		t.Helper()
		if err := service.handleDungeonSelectCard(sessions[member], []byte{side, 0x80 | slot<<4 | member}); err != nil {
			t.Fatal(err)
		}
	}
	for side := byte(0); side < 2; side++ {
		reset()
		click(0, side, 3) // A chooses card 4, regardless of member slot 0.
		peerPacket, rest := splitGameServerUpperPacket(t, sessions[1].conn.(*bufferConn).write.Bytes())
		slots := decodeCurrentDungeonOp71SlotsForTest(t, peerPacket.Body)
		if len(rest) != 0 || peerPacket.Header.MsgID != 71 || peerPacket.Header.Classification != 1 ||
			(side == 0 && slots[3].StateA != 0) || (side == 1 && slots[3].StateB != 0) {
			t.Fatalf("A card 4 was not immediately projected to B: side=%d slots=%+v rest=%x", side, slots, rest)
		}
		reset()
		click(1, side, 3) // A wins the race; B must choose again, with no grant.
		rejected, rest := splitGameServerUpperPacket(t, sessions[1].conn.(*bufferConn).write.Bytes())
		if !bytes.Equal(rejected.Body, []byte{0, 254, side, 3}) || len(rest) != 0 || sessions[0].conn.(*bufferConn).write.Len() != 0 ||
			sessions[1].dungeon.runtime.settlementCardSideSelectionKnown[side] {
			t.Fatalf("collision was not rejected: body=%x rest=%x", rejected.Body, rest)
		}
		reset()
		click(1, side, 0) // B chooses card 1, regardless of member slot 1.
		peerPacket, rest = splitGameServerUpperPacket(t, sessions[0].conn.(*bufferConn).write.Bytes())
		slots = decodeCurrentDungeonOp71SlotsForTest(t, peerPacket.Body)
		if len(rest) != 0 || (side == 0 && slots[0].StateA != 1) || (side == 1 && slots[0].StateB != 1) {
			t.Fatalf("B card 1 was not immediately projected to A: side=%d slots=%+v", side, slots)
		}
		if side == 1 && (!reflect.DeepEqual(slots[0].Rewards, []currentDungeonOp71RewardTuple{{ValueA: 0, ValueB: 101}}) || len(slots[1].Rewards) != 0) {
			t.Fatalf("paid reward lost its real member owner: %+v", slots)
		}
		reset()
		click(0, side, 2) // A replay cannot move card 4 or grant again.
		if owner.cardScreenOwners[side][3] != 1001 || owner.cardScreenOwners[side][0] != 1002 || owner.cardScreenOwners[side][2] != 0 || sessions[1].conn.(*bufferConn).write.Len() != 0 {
			t.Fatalf("replay reordered cards or duplicated peer flip: %+v", owner.cardScreenOwners)
		}
		for i, session := range sessions {
			character, found, err := repos.Character.Load(ctx, session.dungeon.runtime.Character.CharacterID)
			want := int64(10 + i)
			if side == 1 {
				want += int64(100 + i)
			}
			if err != nil || !found || character.Stats["gold"] != want || !session.dungeon.runtime.settlementCardSideRewardCommitted[side] {
				t.Fatalf("member %d reward=%v want=%d found=%v err=%v", i, character.Stats, want, found, err)
			}
		}
	}
}

func TestPartyCardScreenRetryAutoSelectionAndInvalidMember(t *testing.T) {
	service, owner, sessions := newPartyCardResultTest(t, 0, 1)
	for _, session := range sessions {
		session.dungeon.runtime.settlementCardLayoutSent = true
		session.dungeon.runtime.settlementPhase = currentDungeonSettlementPhaseCardsRevealed
	}
	a, b := sessions[0], sessions[1]
	if err := service.handleDungeonSelectCard(a, []byte{0, 0xB1}); err != nil {
		t.Fatal(err)
	}
	if a.conn.(*bufferConn).write.Len() != 0 || owner.cardScreenOwners[0][3] != 0 {
		t.Fatal("spoofed member selected a card")
	}
	wantErr := errors.New("first card ACK failed")
	a.conn = &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	if err := service.handleDungeonSelectCard(a, []byte{0, 0x90}); !errors.Is(err, wantErr) {
		t.Fatalf("first ACK err=%v", err)
	}
	if owner.cardScreenOwners[0][1] != 1001 || a.dungeon.runtime.settlementCardSideSelectionSent[0] {
		t.Fatal("failed ACK lost reservation or marked sent")
	}
	a.conn = &bufferConn{}
	if err := service.handleDungeonSelectCard(a, []byte{0, 0xB0}); err != nil {
		t.Fatal(err)
	}
	if a.dungeon.runtime.settlementCardSidePosition[0] != 1 || owner.cardScreenOwners[0][3] != 0 {
		t.Fatal("retry moved original card")
	}
	if err := service.selectCurrentDungeonCardLocked(b, b.dungeon.runtime, dungeonCardSideFree, 1, "timer_test"); err != nil {
		t.Fatal(err)
	}
	if b.dungeon.runtime.settlementCardSidePosition[0] != 0 || owner.cardScreenOwners[0][1] != 1001 {
		t.Fatal("automatic flip overwrote A's card")
	}
	// Full rows must fail without overwriting another character's reservation.
	owner.cardScreenOwners[1] = [4]uint16{2001, 2002, 2003, 2004}
	if err := service.selectCurrentDungeonCardLocked(b, b.dungeon.runtime, dungeonCardSidePaid, 1, "full_row_test"); !errors.Is(err, errDungeonCardSelectionInvalid) {
		t.Fatalf("full row err=%v", err)
	}
	if owner.cardScreenOwners[1] != [4]uint16{2001, 2002, 2003, 2004} {
		t.Fatal("full row overwritten")
	}
	for wire := 0; wire < 256; wire++ {
		request, err := decodeCurrentDungeonOp71Request([]byte{0, byte(wire)})
		if wire&0x80 == 0 {
			if err != nil || request.HasScreenSlot {
				t.Fatalf("old request %x: %+v %v", wire, request, err)
			}
			continue
		}
		valid := wire&15 < 4 && (wire>>4)&7 < 4
		if valid && (err != nil || !request.HasScreenSlot || request.ValueB != byte(wire&15) || request.ScreenSlot != byte((wire>>4)&7)) {
			t.Fatalf("tagged request %x: %+v %v", wire, request, err)
		}
		if !valid && err == nil {
			t.Fatalf("accepted invalid tagged request %x", wire)
		}
	}
}
