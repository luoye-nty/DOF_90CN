package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
)

var (
	errDebugSessionNotFound  = errors.New("debug game session not found")
	errDebugDungeonNotActive = errors.New("debug dungeon is not active")
)

// DebugSessionInfo is the minimal session identity exposed to the local
// test-server admin endpoint. It contains no socket or client memory data.
type DebugSessionInfo struct {
	AccountID      string `json:"account_id,omitempty"`
	ClientPID      uint32 `json:"client_pid,omitempty"`
	CharacterID    uint16 `json:"character_id"`
	ConnectionID   string `json:"connection_id"`
	InDungeon      bool   `json:"in_dungeon"`
	DungeonID      int64  `json:"dungeon_id,omitempty"`
	RoomX          int64  `json:"room_x,omitempty"`
	RoomY          int64  `json:"room_y,omitempty"`
	MapID          int64  `json:"map_id,omitempty"`
	RoomCleared    bool   `json:"room_cleared,omitempty"`
	AnnouncedEnemy int    `json:"announced_enemy_count,omitempty"`
}

// DebugDungeonResult describes a server-side test operation and the resulting
// current-room snapshot. The operation never writes client memory or invents
// a packet format; it calls the same validated bridge handlers as the client.
type DebugDungeonResult struct {
	Operation     string `json:"operation"`
	CharacterID   uint16 `json:"character_id"`
	DungeonID     int64  `json:"dungeon_id,omitempty"`
	BeforeRoomX   int64  `json:"before_room_x,omitempty"`
	BeforeRoomY   int64  `json:"before_room_y,omitempty"`
	BeforeMapID   int64  `json:"before_map_id,omitempty"`
	AfterRoomX    int64  `json:"after_room_x,omitempty"`
	AfterRoomY    int64  `json:"after_room_y,omitempty"`
	AfterMapID    int64  `json:"after_map_id,omitempty"`
	KilledEnemies int    `json:"killed_enemies,omitempty"`
	RoomCleared   bool   `json:"room_cleared,omitempty"`
	Moved         bool   `json:"moved,omitempty"`
	NextX         byte   `json:"next_x,omitempty"`
	NextY         byte   `json:"next_y,omitempty"`
}

// DebugSessionsSnapshot returns only currently bound game sessions. The
// caller must still authenticate at the admin boundary; this method itself
// does not expose a public network listener.
func (s *Service) DebugSessionsSnapshot() []DebugSessionInfo {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	sessions := make([]*gameSession, 0, len(s.gameSessions))
	seen := make(map[*gameSession]struct{}, len(s.gameSessions))
	for _, session := range s.gameSessions {
		if session == nil {
			continue
		}
		if _, ok := seen[session]; ok {
			continue
		}
		seen[session] = struct{}{}
		sessions = append(sessions, session)
	}
	s.mu.Unlock()

	result := make([]DebugSessionInfo, 0, len(sessions))
	for _, session := range sessions {
		info := DebugSessionInfo{
			AccountID:    session.accountID,
			ClientPID:    session.clientPID,
			CharacterID:  session.selectedCharacterID,
			ConnectionID: session.connID,
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		if runtime != nil && runtime.Session != nil {
			if scene, ok := runtime.Session.Scene(); ok {
				info.InDungeon = true
				info.DungeonID = runtime.Dungeon.ID
				info.RoomX = scene.Coordinate.X
				info.RoomY = scene.Coordinate.Y
				info.MapID = scene.Map.Map.ID
				info.RoomCleared = scene.Cleared
				snapshot := runtime.Room.Snapshot()
				for _, monster := range snapshot.Monsters {
					if monster.State == runtimeDungeonMonsterAnnounced {
						info.AnnouncedEnemy++
					}
				}
				for _, actor := range snapshot.ExtendedActors {
					if actor.State == runtimeDungeonMonsterAnnounced && actor.HostileReference != nil {
						info.AnnouncedEnemy++
					}
				}
			}
		}
		session.dungeon.mu.Unlock()
		result = append(result, info)
	}
	return result
}

// DebugKillCurrentRoom applies the existing server-side monster death path to
// every currently announced hostile in the selected room. Boss handling,
// rewards, quest triggers, and settlement remain owned by the normal bridge.
func (s *Service) DebugKillCurrentRoom(ctx context.Context, characterID uint16) (DebugDungeonResult, error) {
	session, ok := s.debugSessionByCharacter(characterID)
	if !ok {
		return DebugDungeonResult{}, errDebugSessionNotFound
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := DebugDungeonResult{Operation: "kill-current-room", CharacterID: characterID}
	err := s.callGameSession(ctx, session, "admin-debug-kill-current-room", func() error {
		before, ok := debugDungeonSceneSnapshot(session)
		if !ok {
			return errDebugDungeonNotActive
		}
		result.DungeonID = before.dungeonID
		result.BeforeRoomX = before.x
		result.BeforeRoomY = before.y
		result.BeforeMapID = before.mapID

		keys := debugAnnouncedHostileKeys(session)
		for _, objectKey := range keys {
			body := make([]byte, dungeoncmd.DieMonsterRequestSize)
			binary.LittleEndian.PutUint32(body[0:4], objectKey)
			binary.LittleEndian.PutUint16(body[4:6], currentSceneActorObjectKey(characterID))
			if err := s.handleDungeonMonsterDeath(session, body); err != nil {
				return err
			}
		}

		after, ok := debugDungeonSceneSnapshot(session)
		if !ok {
			return errDebugDungeonNotActive
		}
		result.AfterRoomX = after.x
		result.AfterRoomY = after.y
		result.AfterMapID = after.mapID
		result.RoomCleared = after.cleared
		result.KilledEnemies = debugDefeatedHostileCount(session)
		return nil
	})
	if err != nil {
		return DebugDungeonResult{}, err
	}
	return result, nil
}

// DebugMoveCurrentRoom requests a topology-validated room transition through
// the same op45 handler used by the current client. It cannot jump to an
// arbitrary map or bypass the current-room clear gate.
func (s *Service) DebugMoveCurrentRoom(ctx context.Context, characterID uint16, nextX, nextY byte) (DebugDungeonResult, error) {
	session, ok := s.debugSessionByCharacter(characterID)
	if !ok {
		return DebugDungeonResult{}, errDebugSessionNotFound
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := DebugDungeonResult{
		Operation:   "move-room",
		CharacterID: characterID,
		NextX:       nextX,
		NextY:       nextY,
	}
	err := s.callGameSession(ctx, session, "admin-debug-move-room", func() error {
		before, ok := debugDungeonSceneSnapshot(session)
		if !ok {
			return errDebugDungeonNotActive
		}
		result.DungeonID = before.dungeonID
		result.BeforeRoomX = before.x
		result.BeforeRoomY = before.y
		result.BeforeMapID = before.mapID

		body := make([]byte, dungeoncmd.MoveMapRequestSize)
		body[0] = nextX
		body[1] = nextY
		if err := s.handleDungeonMoveMap(session, body); err != nil {
			return err
		}
		after, ok := debugDungeonSceneSnapshot(session)
		if !ok {
			return errDebugDungeonNotActive
		}
		result.AfterRoomX = after.x
		result.AfterRoomY = after.y
		result.AfterMapID = after.mapID
		result.Moved = before.x != after.x || before.y != after.y || before.mapID != after.mapID
		result.RoomCleared = after.cleared
		return nil
	})
	if err != nil {
		return DebugDungeonResult{}, err
	}
	return result, nil
}

type debugDungeonScene struct {
	dungeonID int64
	x         int64
	y         int64
	mapID     int64
	cleared   bool
}

func (s *Service) debugSessionByCharacter(characterID uint16) (*gameSession, bool) {
	if s == nil || characterID == 0 {
		return nil, false
	}
	return s.onlineGameSession(characterID)
}

func debugDungeonSceneSnapshot(session *gameSession) (debugDungeonScene, bool) {
	if session == nil {
		return debugDungeonScene{}, false
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil {
		return debugDungeonScene{}, false
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return debugDungeonScene{}, false
	}
	return debugDungeonScene{
		dungeonID: runtime.Dungeon.ID,
		x:         scene.Coordinate.X,
		y:         scene.Coordinate.Y,
		mapID:     scene.Map.Map.ID,
		cleared:   scene.Cleared,
	}, true
}

func debugAnnouncedHostileKeys(session *gameSession) []uint32 {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Room == nil {
		return nil
	}
	snapshot := runtime.Room.Snapshot()
	keys := make([]uint32, 0, len(snapshot.Monsters)+len(snapshot.ExtendedActors))
	for _, monster := range snapshot.Monsters {
		if monster.State == runtimeDungeonMonsterAnnounced {
			keys = append(keys, monster.ObjectKey)
		}
	}
	for _, actor := range snapshot.ExtendedActors {
		if actor.State == runtimeDungeonMonsterAnnounced && actor.HostileReference != nil {
			keys = append(keys, actor.ObjectKey)
		}
	}
	return keys
}

func debugDefeatedHostileCount(session *gameSession) int {
	if session == nil {
		return 0
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Room == nil {
		return 0
	}
	snapshot := runtime.Room.Snapshot()
	count := 0
	for _, monster := range snapshot.Monsters {
		if monster.State == runtimeDungeonMonsterDefeated {
			count++
		}
	}
	for _, actor := range snapshot.ExtendedActors {
		if actor.State == runtimeDungeonMonsterDefeated && actor.HostileReference != nil {
			count++
		}
	}
	return count
}

// DebugResolveCharacterID accepts the selector forms used by the local admin
// handler and deliberately rejects ambiguous or unbound sessions.
func (s *Service) DebugResolveCharacterID(characterID uint16, accountID string, clientPID uint32) (uint16, error) {
	if characterID != 0 {
		if _, ok := s.debugSessionByCharacter(characterID); !ok {
			return 0, errDebugSessionNotFound
		}
		return characterID, nil
	}
	accountID = strings.TrimSpace(accountID)
	var match uint16
	for _, session := range s.DebugSessionsSnapshot() {
		if (accountID != "" && session.AccountID == accountID) ||
			(clientPID != 0 && session.ClientPID == clientPID) {
			if match != 0 && match != session.CharacterID {
				return 0, fmt.Errorf("debug selector matched multiple sessions")
			}
			match = session.CharacterID
		}
	}
	if match == 0 {
		return 0, errDebugSessionNotFound
	}
	return match, nil
}
