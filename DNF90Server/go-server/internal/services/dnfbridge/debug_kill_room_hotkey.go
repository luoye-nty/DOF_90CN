package dnfbridge

import (
	"context"
	"encoding/binary"
	"sync/atomic"
	"time"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	// The hotkey request reuses the intentional op1456 enum hole, exactly like
	// the "ACK1" capability announcement and the "BMU1" boss-mutation control.
	// "KRM1" (kill-room magic 1) is a distinct little-endian marker.
	currentDebugKillRoomMagic         = uint32(0x314D524B) // "KRM1"
	currentDebugKillRoomVersion       = byte(1)
	currentDebugKillRoomRequestLength = 8

	// The server owns the death transaction end to end, so one request can take
	// a moment on a full boss room. The bound only exists so a stuck session
	// cannot leave the single-flight flag set forever.
	currentDebugKillRoomTimeout = 10 * time.Second
)

// logDebugKillRoom writes to both the structured packet-event stream and the
// normal service log. Packet logging is off in production deployments, and a
// silently rejected hotkey would otherwise be impossible to diagnose.
func (s *Service) logDebugKillRoom(session *gameSession, event string, fields ...any) {
	s.logGameEvent(session, event, fields...)
	s.logInfo(event, fields...)
}

func isCurrentDebugKillRoomEnvelope(body []byte) bool {
	return len(body) >= 4 &&
		binary.LittleEndian.Uint32(body[:4]) == currentDebugKillRoomMagic
}

// parseCurrentDebugKillRoomRequest accepts exactly the 8-byte body written by
// the standalone LYPlugins hotkey DLL:
//
//	u32 "KRM1" | u8 version | u8 reserved | u16 token
//
// The token only exists so client and server log lines can be correlated; it
// carries no authority.
func parseCurrentDebugKillRoomRequest(body []byte) (uint16, bool) {
	if len(body) != currentDebugKillRoomRequestLength ||
		binary.LittleEndian.Uint32(body[0:4]) != currentDebugKillRoomMagic ||
		body[4] != currentDebugKillRoomVersion ||
		body[5] != 0 {
		return 0, false
	}
	token := binary.LittleEndian.Uint16(body[6:8])
	if token == 0 {
		return 0, false
	}
	return token, true
}

// handleCurrentDebugKillRoom runs the same validated monster-death transaction
// the admin "kill current room" debug endpoint uses, so drops, quest triggers,
// boss handling and room settlement stay owned by the normal bridge.
//
// Gated twice: the process must be started with DNFBRIDGE_DEBUG_KILL_ROOM=true
// and one request at a time is accepted per process. A client that never sends
// "KRM1" cannot reach this path.
func (s *Service) handleCurrentDebugKillRoom(
	session *gameSession,
	classification byte,
	body []byte,
) error {
	if session == nil {
		return nil
	}
	if classification != dnfproto.DefaultChannelClassification {
		s.logDebugKillRoom(session, "game-debug-kill-room-rejected",
			"classification", classification,
			"body_len", len(body),
			"reason", "class1_private_request_required")
		return nil
	}
	token, ok := parseCurrentDebugKillRoomRequest(body)
	if !ok {
		s.logDebugKillRoom(session, "game-debug-kill-room-rejected",
			"classification", classification,
			"body_len", len(body),
			"reason", "invalid_krm1_envelope")
		return nil
	}
	if !s.debugKillRoomHotkey {
		s.logDebugKillRoom(session, "game-debug-kill-room-rejected",
			"token", token,
			"reason", "debug_kill_room_disabled")
		return nil
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		s.logDebugKillRoom(session, "game-debug-kill-room-rejected",
			"token", token,
			"reason", "no_selected_character")
		return nil
	}
	if !atomic.CompareAndSwapInt32(&s.debugKillRoomInFlight, 0, 1) {
		s.logDebugKillRoom(session, "game-debug-kill-room-rejected",
			"token", token,
			"character_id", characterID,
			"reason", "previous_request_in_flight")
		return nil
	}

	s.logDebugKillRoom(session, "game-debug-kill-room-accepted",
		"token", token,
		"character_id", characterID)

	// DebugKillCurrentRoom marshals onto the game-session goroutine and waits.
	// This handler runs on that same goroutine, so the call must stay async.
	go func() {
		defer atomic.StoreInt32(&s.debugKillRoomInFlight, 0)
		ctx, cancel := context.WithTimeout(context.Background(), currentDebugKillRoomTimeout)
		defer cancel()
		result, err := s.DebugKillCurrentRoom(ctx, characterID)
		if err != nil {
			s.logDebugKillRoom(session, "game-debug-kill-room-deferred",
				"token", token,
				"character_id", characterID,
				"reason", "server_room_death_transaction_failed",
				"error", err)
			return
		}
		s.logDebugKillRoom(session, "game-debug-kill-room-applied",
			"token", token,
			"character_id", characterID,
			"dungeon_id", result.DungeonID,
			"room_x", result.BeforeRoomX,
			"room_y", result.BeforeRoomY,
			"map_id", result.BeforeMapID,
			"killed_enemies", result.KilledEnemies,
			"room_cleared", result.RoomCleared)
	}()
	return nil
}
