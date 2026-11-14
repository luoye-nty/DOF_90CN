package dnfbridge

import (
	"encoding/binary"
	"fmt"
	"path"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	currentLocationHotkeyMagic         = uint32(0x31434F4C) // "LOC1"
	currentLocationHotkeyVersion       = byte(1)
	currentLocationHotkeyRequestLength = 8
)

type currentLocationHotkeyReply struct {
	ChannelID   uint16
	InDungeon   bool
	DungeonID   int64
	RoomX       int64
	RoomY       int64
	TownID      uint8
	AreaID      uint8
	PositionX   uint16
	PositionY   uint16
	HasPosition bool
}

func isCurrentLocationHotkeyEnvelope(body []byte) bool {
	return len(body) >= 4 &&
		binary.LittleEndian.Uint32(body[:4]) == currentLocationHotkeyMagic
}

func parseCurrentLocationHotkeyRequest(body []byte) (uint16, bool) {
	if len(body) != currentLocationHotkeyRequestLength ||
		binary.LittleEndian.Uint32(body[0:4]) != currentLocationHotkeyMagic ||
		body[4] != currentLocationHotkeyVersion ||
		body[5] != 0 {
		return 0, false
	}
	token := binary.LittleEndian.Uint16(body[6:8])
	return token, token != 0
}

func (s *Service) currentLocationHotkeySnapshot(session *gameSession, characterID uint16) (currentLocationHotkeyReply, bool) {
	if s == nil || session == nil || characterID == 0 || session.selectedCharacterID != characterID {
		return currentLocationHotkeyReply{}, false
	}
	reply := currentLocationHotkeyReply{ChannelID: uint16(session.channel.ID)}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	if runtime != nil {
		reply.InDungeon = true
		reply.DungeonID = runtime.Dungeon.ID
		reply.RoomX, reply.RoomY = -1, -1
		if runtime.Session != nil {
			if scene, ok := runtime.Session.Scene(); ok {
				reply.RoomX = scene.Coordinate.X
				reply.RoomY = scene.Coordinate.Y
			}
		}
		reply.PositionX = runtime.actorPositionX
		reply.PositionY = runtime.actorPositionY
		reply.HasPosition = runtime.actorPositionValid
	}
	session.dungeon.mu.Unlock()
	if reply.InDungeon {
		return reply, reply.HasPosition || reply.RoomX >= 0
	}

	session.townMu.Lock()
	snapshot := session.townPositionSnapshot
	readyCharacterID := session.townSceneReadyCharacterID
	session.townMu.Unlock()
	if readyCharacterID == characterID && snapshot.CharacterID == characterID && snapshot.PositionValid {
		reply.TownID = snapshot.TownID
		reply.AreaID = snapshot.AreaID
		reply.PositionX = snapshot.PositionX
		reply.PositionY = snapshot.PositionY
		reply.HasPosition = true
		return reply, true
	}

	if s.onlinePlayers != nil {
		if player, ok := s.onlinePlayers.PlayerForCharacter(characterID); ok {
			reply.ChannelID = player.ChannelID
			reply.TownID = player.TownID
			reply.AreaID = player.AreaID
			reply.PositionX = player.PositionX
			reply.PositionY = player.PositionY
			reply.HasPosition = true
			return reply, true
		}
	}
	return reply, false
}

func (s *Service) currentLocationHotkeyText(reply currentLocationHotkeyReply) string {
	if reply.InDungeon {
		if reply.HasPosition {
			return fmt.Sprintf("位置：副本%d 房间(%d,%d) 坐标(%d,%d) 频道%d",
				reply.DungeonID, reply.RoomX, reply.RoomY, reply.PositionX, reply.PositionY, reply.ChannelID)
		}
		return fmt.Sprintf("位置：副本%d 房间(%d,%d) 频道%d",
			reply.DungeonID, reply.RoomX, reply.RoomY, reply.ChannelID)
	}

	townLabel := strconv.Itoa(int(reply.TownID))
	areaLabel := strconv.Itoa(int(reply.AreaID))
	if s != nil {
		s.townCatalogMu.RLock()
		table := s.townCatalog
		s.townCatalogMu.RUnlock()
		if table != nil {
			if town, ok := table.Find(int64(reply.TownID)); ok && town.Name != "" {
				townLabel = town.Name
			}
			if area, ok := table.FindArea(int64(reply.TownID), int64(reply.AreaID)); ok && area.MapPath != "" {
				areaLabel = path.Base(strings.ReplaceAll(area.MapPath, "\\", "/"))
			}
		}
	}
	return fmt.Sprintf("位置：%s (城镇%d) 区域%d %s 坐标(%d,%d) 频道%d",
		townLabel, reply.TownID, reply.AreaID, areaLabel, reply.PositionX, reply.PositionY, reply.ChannelID)
}

func encodeCurrentLocationHotkeyText(text string) []byte {
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))
	if err != nil || len(encoded) == 0 {
		encoded = []byte(text)
	}
	if len(encoded) > currentTownChatDecodedMessageLimit {
		encoded = encoded[:currentTownChatDecodedMessageLimit]
	}
	return encoded
}

// handleCurrentLocationHotkey returns the server-owned location for the selected
// character through the same class-0 whisper notification path used by chat.
// The request is read-only and cannot address another character.
func (s *Service) handleCurrentLocationHotkey(session *gameSession, classification byte, body []byte) error {
	if s == nil || session == nil {
		return nil
	}
	if classification != dnfproto.DefaultChannelClassification {
		s.logGameEvent(session, "game-location-hotkey-rejected", "reason", "class1_private_request_required")
		return nil
	}
	token, ok := parseCurrentLocationHotkeyRequest(body)
	if !ok {
		s.logGameEvent(session, "game-location-hotkey-rejected", "reason", "invalid_loc1_envelope")
		return nil
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		s.logGameEvent(session, "game-location-hotkey-rejected", "token", token, "reason", "no_selected_character")
		return nil
	}
	location, ok := s.currentLocationHotkeySnapshot(session, characterID)
	if !ok {
		s.logGameEvent(session, "game-location-hotkey-rejected",
			"token", token, "character_id", characterID, "reason", "town_scene_location_unavailable")
		return nil
	}

	state := buildCurrentPeerChatStateBody(characterID, currentTownChatWhisperMode)
	if err := s.sendGameUpperRawClassCodec(session, currentPeerChatStateOpcode, state, 0, true); err != nil {
		return err
	}
	text := encodeCurrentLocationHotkeyText(s.currentLocationHotkeyText(location))
	notice := buildCurrentGuildChatNoticeBody(
		currentTownChatWhisperMode,
		currentChatServerGroup,
		byte(location.ChannelID),
		0,
		rosterNameBytes("位置助手"),
		text,
	)
	if err := s.sendGameUpperRawClassCodec(session, currentPeerChatNotifyOpcode, notice, 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-location-hotkey-sent",
		"token", token,
		"character_id", characterID,
		"channel_id", location.ChannelID,
		"in_dungeon", location.InDungeon,
		"town_id", location.TownID,
		"area_id", location.AreaID,
		"position_x", location.PositionX,
		"position_y", location.PositionY)
	return nil
}
