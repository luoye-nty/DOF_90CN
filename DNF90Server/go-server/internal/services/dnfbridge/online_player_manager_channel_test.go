package dnfbridge

import "testing"

// A channel is a separate copy of the same town. Two characters standing on the
// same map in different channels are not peers: they must not appear in each
// other's area roster, receive each other's movement, or satisfy the party
// dungeon-entry co-presence check. Before areaKey carried the channel id they
// keyed into one shared area, so a character in the arena and a character in an
// ordinary town could see each other while genuine arena peers could not.
func TestOnlinePlayerManagerSeparatesChannelsSharingATownArea(t *testing.T) {
	manager := newOnlinePlayerManager()
	first := &onlinePlayerInfo{CharacterID: 11, ChannelID: 191, TownID: 10, AreaID: 1}
	sameChannel := &onlinePlayerInfo{CharacterID: 12, ChannelID: 191, TownID: 10, AreaID: 1}
	otherChannel := &onlinePlayerInfo{CharacterID: 13, ChannelID: 192, TownID: 10, AreaID: 1}

	if peers := manager.EnterArea(first); len(peers) != 0 {
		t.Fatalf("first character saw unexpected peers: %+v", peers)
	}
	if peers := manager.EnterArea(otherChannel); len(peers) != 0 {
		t.Fatalf("another channel leaked into the area roster: %+v", peers)
	}
	peers := manager.EnterArea(sameChannel)
	if len(peers) != 1 || peers[0].CharacterID != first.CharacterID {
		t.Fatalf("same-channel peers=%+v, want only character %d", peers, first.CharacterID)
	}

	if !manager.PeerInSameArea(first.CharacterID, sameChannel.CharacterID) {
		t.Fatal("same-channel characters are not co-present")
	}
	if manager.PeerInSameArea(first.CharacterID, otherChannel.CharacterID) {
		t.Fatal("characters in different channels are visible to each other")
	}

	if got := manager.GetAreaPlayers(191, 10, 1); len(got) != 2 {
		t.Fatalf("channel 191 roster=%+v, want two characters", got)
	}
	if got := manager.GetAreaPlayers(192, 10, 1); len(got) != 1 ||
		got[0].CharacterID != otherChannel.CharacterID {
		t.Fatalf("channel 192 roster=%+v, want only character %d", got, otherChannel.CharacterID)
	}
	if got := manager.GetAreaPlayers(0, 10, 1); len(got) != 0 {
		t.Fatalf("channel 0 must not collect a real channel's actors: %+v", got)
	}
}
