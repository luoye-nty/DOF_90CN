package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// The dungeon-selection page deliberately removes its opener from the TOWN
// roster, and that decision is about town visibility only. The same retirement
// also silenced the party-scoped actor fan-out, because
// broadcastCurrentPartyActorProjection resolves peers through the roster - so the
// opening player's teammates never received his scene actor and he never received
// theirs. That is the client-side 队友不在附近 refusal the operator reports after a
// return ("出副本有时候再进会提示队友不在附近"), and the server-side twin of that
// check (game-party-dungeon-entry-copresence-refused) never fires for it.
func TestSelectorPagePartyCoPresenceProjectsBothDirectionsForRetiredOwner(t *testing.T) {
	service, leader, follower := prepareCurrentPartySelectorEntryTest(t)
	leaderConn := leader.conn.(*bufferConn)
	followerConn := follower.conn.(*bufferConn)

	leaderPresence, resolved := service.currentTownActorAreaDetachPresence(leader)
	if !resolved {
		t.Fatal("fixture leader has no town anchor")
	}
	// The completed-run route: the run is retired into the other-dungeon selector
	// page and the character is off the town roster, so its anchor is the memo.
	service.onlinePlayers.LeaveAreaSession(selectedCharacterID(leader), leader)
	if _, registered := service.onlinePlayers.PlayerForCharacter(selectedCharacterID(leader)); registered {
		t.Fatal("selector opener stayed on the town roster")
	}
	if !service.bindCompletedRunSelectorPresence(leader, currentTownPositionSnapshot{
		CharacterID:   leaderPresence.CharacterID,
		TownID:        leaderPresence.TownID,
		AreaID:        leaderPresence.AreaID,
		PositionX:     leaderPresence.PositionX,
		PositionY:     leaderPresence.PositionY,
		MovementCode:  leaderPresence.Direction,
		PositionValid: true,
	}, "test_completed_run") {
		t.Fatal("completed-run selector presence was not bound")
	}
	leaderConn.write.Reset()
	followerConn.write.Reset()

	if projected := service.rebuildRuntimePartySelectorPageCoPresence(leader, "test_selector_page"); projected != 1 {
		t.Fatalf("selector co-presence projected=%d want=1", projected)
	}
	// The opener renders the teammate...
	assertSelectorPartyActorProjection(t, "selector opener", leaderConn.write.Bytes(), service, leader)
	// ...and the teammate renders the opener, who is no longer on the town roster.
	assertSelectorPartyActorProjection(t, "party member", followerConn.write.Bytes(), service, follower)
	if _, registered := service.onlinePlayers.PlayerForCharacter(selectedCharacterID(leader)); registered {
		t.Fatal("selector co-presence re-registered the opener in town")
	}
}

func TestSelectorPagePartyCoPresenceStaysQuietWithoutAPeerInTheSameArea(t *testing.T) {
	service, leader, follower := prepareCurrentPartySelectorEntryTest(t)
	leaderConn := leader.conn.(*bufferConn)
	followerConn := follower.conn.(*bufferConn)

	// A solo party opening the page is a high-frequency action: it must not write a
	// packet or a log line.
	storeRuntimePartyState(leader, alignedcmd.PartyState{
		PartyID: 99, UserID: 99,
		Members: []alignedcmd.PartyMemberState{{UserID: 99, UserState: 1, HPPercent: 100, MPPercent: 100}},
	})
	service.leaveTownPresenceForDungeonSelector(leader, "test_selector_open_solo")
	leaderConn.write.Reset()
	followerConn.write.Reset()
	if projected := service.rebuildRuntimePartySelectorPageCoPresence(leader, "test_solo"); projected != 0 {
		t.Fatalf("solo selector page projected=%d want=0", projected)
	}
	if leaderConn.write.Len() != 0 || followerConn.write.Len() != 0 {
		t.Fatalf("solo selector page wrote packets: leader=%x member=%x",
			leaderConn.write.Bytes(), followerConn.write.Bytes())
	}

	// A member standing in another area is genuinely not nearby. The projection must
	// not paper over that: the client's refusal is correct there, and the server's
	// entry preflight refuses it too.
	storeRuntimePartyState(leader, alignedcmd.PartyState{
		PartyID: 99, UserID: 99,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 99, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 100, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	})
	service.onlinePlayers.LeaveAreaSession(100, follower)
	service.onlinePlayers.EnterArea(&onlinePlayerInfo{
		CharacterID: 100, TownID: 38, AreaID: 4, Session: follower,
	})
	leaderConn.write.Reset()
	followerConn.write.Reset()
	if projected := service.rebuildRuntimePartySelectorPageCoPresence(leader, "test_split_areas"); projected != 0 {
		t.Fatalf("split-area selector page projected=%d want=0", projected)
	}
	if leaderConn.write.Len() != 0 || followerConn.write.Len() != 0 {
		t.Fatalf("split-area selector page wrote packets: leader=%x member=%x",
			leaderConn.write.Bytes(), followerConn.write.Bytes())
	}
}

// assertSelectorPartyActorProjection checks one receiver's stream for the
// party-scoped co-presence shape: remote mode0, the party display record and the
// remote area notification -- and, just as importantly, no op11 and no mode1.
//
// op11 is a formation handshake: replaying it makes the client tear a healthy peer
// link down and re-enter the connecting state, which is why a scene reconstruction
// uses op9 + op153 only. mode1 is the receiver's own stat/equipment snapshot: it
// has no consumer for a remote peer, the client applies it to the actor it
// controls, and it is the expensive half of the projection (PVF attribute
// recompute per body) -- see projectSelectorPartyActorPair.
func assertSelectorPartyActorProjection(
	t *testing.T,
	label string,
	stream []byte,
	service *Service,
	receiver *gameSession,
) {
	t.Helper()
	if len(stream) == 0 {
		t.Fatalf("%s received no selector co-presence packets", label)
	}
	packets := splitAllCurrentGameServerUpperPackets(t, stream)
	var mode0, mode1, roster, area bool
	for _, packet := range packets {
		if packet.Header.Classification != 0 {
			continue
		}
		switch {
		case packet.Header.MsgID == 0x000b:
			t.Fatalf("%s received a formation op11 replay", label)
		case packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 0:
			mode0 = true
		case packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) && len(packet.Body) > 0 && packet.Body[0] == 1:
			mode1 = true
		case packet.Header.MsgID == uint16(dnfenum.CmdPacketRecoverStamina):
			roster = true
		case packet.Header.MsgID == currentTownUserAreaNotificationMsgID:
			area = true
		}
	}
	if !mode0 || !roster || !area {
		t.Fatalf("%s selector co-presence mode0=%t roster=%t area=%t count=%d char=%d",
			label, mode0, roster, area, len(packets), selectedCharacterID(receiver))
	}
	if mode1 {
		t.Fatalf("%s selector co-presence carried a mode1 binding", label)
	}
}
