package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

// The "空血条" root cause, pinned: an area actor projection is a kind-0 op9 carrying
// the ACTOR's party model. When the observer stands in that same ordinary party the
// record is the observer's own frame, and op153 is the only carrier of the per-member
// HP/MP rows (party.BuildSingleMemberRealtimeInfo) -- so the observer's frame must be
// closed with its own rows, not left on the bare op9.
//
// Live 2026-09-20T13:16:52Z, conn game-000169 (char 1 entering raid gate 224):
//
//	game-state-send source=raid_dungeon_loaded_raid_mate_actors target_char_id=1
//	game-current-party-frame-op9-send … char_id=20 party_model_key=4
//	game-current-party-frame-op9-send … char_id=22 party_model_key=4
//	game-current-party-frame-op9-send … char_id=21 party_model_key=4     ← no op153 after any
//
// which is the operator's "一阶段结束 在副本里队伍 强制回城后 队员血条全空" and
// "战况表回城后 双方显示都是空血条".
func TestSamePartyAreaProjectionClosesTheObserverFrameWithOp153(t *testing.T) {
	service := &Service{
		options:       options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		onlinePlayers: newOnlinePlayerManager(),
	}
	channel := channelcatalog.Channel{ServerID: 1, ID: 23, Type: 23, Name: "raid", Port: 10023}
	actor := &gameSession{conn: &bufferConn{}, connID: "area-actor", channel: channel, residentChannel: channel, selectedCharacterID: 20}
	observer := &gameSession{conn: &bufferConn{}, connID: "area-observer", channel: channel, residentChannel: channel, selectedCharacterID: 1}
	for _, session := range []*gameSession{actor, observer} {
		service.bindGameSessionCharacter(session, session.selectedCharacterID)
		service.onlinePlayers.EnterArea(&onlinePlayerInfo{
			CharacterID: session.selectedCharacterID,
			ChannelID:   uint16(channel.ID),
			TownID:      19,
			AreaID:      2,
			Session:     session,
		})
	}
	// Both stand in the same squad party, exactly like a raid gate's members do.
	shared := alignedcmd.PartyState{
		PartyID: 4, UserID: 20, IsLeader: true, MaxMembers: 4,
		Members: []alignedcmd.PartyMemberState{
			{UserID: 20, Slot: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{UserID: 1, Slot: 1, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
	}
	storeRuntimePartyState(actor, shared)
	storeRuntimePartyState(observer, shared)

	if err := service.broadcastCurrentPartyActorProjection(actor, shared, "test_area_actor_projection"); err != nil {
		t.Fatalf("area actor projection: %v", err)
	}
	packets := runtimeRaidPeerTake(t, observer)
	tail, bare := partyFrameTail(packets)
	if tail != 0x0099 {
		t.Fatalf("the observer's party frame ends on msg_id=%d, want the op153 health rows: "+
			"teammates' HP/MP bars are empty without them", tail)
	}
	if bare != 0 {
		t.Fatalf("the observer holds %d op9 area records with no op153 behind them", bare)
	}
	realtime, ok := runtimeRaidPeerFind(packets, 0x0099)
	if !ok || realtime.Body[0] != 2 {
		t.Fatalf("the closing op153 rows = % X, want one row per party member (2)", realtime.Body)
	}
}
