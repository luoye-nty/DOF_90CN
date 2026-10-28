package dnfbridge

import (
	"context"
	"fmt"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentPVPTownID              int64 = 10
	currentPVPAreaID              int64 = 0
	currentAntonRaidTownID        int64 = 19
	currentAntonRaidAreaID        int64 = 0
	currentAntonRaidWaitingAreaID byte  = 2
	currentAntonRaidDirection           = newCharacterInitialDirection
	currentAntonRaidAreaState           = newCharacterInitialAreaState
	currentLukeRaidTownID         int64 = 32
	currentLukeRaidAreaID         int64 = 0
	currentLukeRaidWaitingAreaID  byte  = 4
	// 超时空攻坚战 (FiendWar).  The etc declares [WAITING ROOM] `98 2`, and the
	// channel-entry landmark is town 98 area 0 exactly like town 19 and town 32:
	// town/town.lst maps 98 -> `180621_Cosmofiend.twn`, whose area 0 is
	// `GContents/180621_Fiendwar/Town/Gate_Fiend.map` with gate (474,234) --
	// the same gate coordinate the other two raid towns publish.
	currentFiendWarRaidTownID    int64 = 98
	currentFiendWarRaidAreaID    int64 = 0
	currentFiendWarWaitingAreaID byte  = 2
)

type runtimeRaidTownSpec struct {
	townID      int64
	areaID      int64
	waitingArea byte
	ownerName   string
}

// isCurrentAntonRaidChannel identifies the public Anton team-channel
// environment. The channel type is the authoritative discriminator; channel
// IDs are deployment data and may differ between channel_info.etc variants.
func isCurrentAntonRaidChannel(session *gameSession) bool {
	return session != nil && session.channel.Type == dnfenum.RaidChannelType
}

func isCurrentLukeRaidChannel(session *gameSession) bool {
	return session != nil && session.channel.Type == dnfenum.HiddenRaidType
}

// isCurrentFiendWarRaidChannel identifies the public Fiend War (超时空攻坚战)
// team-channel environment.  As with Anton and Luke the channel type is the
// discriminator and the ids stay deployment data: this deployment publishes the
// three `[fiendwar]` rows as 184..186, remapped from the stock 261..263 because
// a published id above 254 leaves the client's u8 town-actor owner uncommitted
// (see channel_info.etc).
//
// Recognising the channel is what routes it to town 98 instead of the character's
// ordinary town.  It deliberately does NOT make the raid runnable: the owner
// runtime, roster and board land in a later step, so a channel that reaches here
// today can build the raid town scene and nothing more.
func isCurrentFiendWarRaidChannel(session *gameSession) bool {
	return session != nil && session.channel.Type == dnfenum.FiendWarRaidChannelType
}

func isCurrentImplementedRaidChannel(session *gameSession) bool {
	return isCurrentAntonRaidChannel(session) || isCurrentLukeRaidChannel(session) || isCurrentFiendWarRaidChannel(session)
}

func currentRaidTownSpecForSession(session *gameSession) (runtimeRaidTownSpec, bool) {
	switch {
	case isCurrentAntonRaidChannel(session):
		return runtimeRaidTownSpec{townID: currentAntonRaidTownID, areaID: currentAntonRaidAreaID, waitingArea: currentAntonRaidWaitingAreaID, ownerName: "anton"}, true
	case isCurrentLukeRaidChannel(session):
		return runtimeRaidTownSpec{townID: currentLukeRaidTownID, areaID: currentLukeRaidAreaID, waitingArea: currentLukeRaidWaitingAreaID, ownerName: "luke"}, true
	case isCurrentFiendWarRaidChannel(session):
		return runtimeRaidTownSpec{townID: currentFiendWarRaidTownID, areaID: currentFiendWarRaidAreaID, waitingArea: currentFiendWarWaitingAreaID, ownerName: "fiendwar"}, true
	default:
		return runtimeRaidTownSpec{}, false
	}
}

// currentAntonRaidTownTransition builds the temporary team-channel scene
// from the runtime PVF. The raid town is session-scoped: its location is sent
// to the client but is deliberately not written into the character's durable
// ordinary-town stats.
func (s *Service) currentAntonRaidTownTransition(
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
	locationSource string,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	return s.currentRaidTownTransition(session, characterID, character, locationSource, runtimeRaidTownSpec{
		townID: currentAntonRaidTownID, areaID: currentAntonRaidAreaID, waitingArea: currentAntonRaidWaitingAreaID, ownerName: "anton",
	})
}

func (s *Service) currentLukeRaidTownTransition(
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
	locationSource string,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	return s.currentRaidTownTransition(session, characterID, character, locationSource, runtimeRaidTownSpec{
		townID: currentLukeRaidTownID, areaID: currentLukeRaidAreaID, waitingArea: currentLukeRaidWaitingAreaID, ownerName: "luke",
	})
}

func (s *Service) currentPVPTownTransition(
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
	locationSource string,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	return s.currentRaidTownTransition(session, characterID, character, locationSource, runtimeRaidTownSpec{
		townID: currentPVPTownID, areaID: currentPVPAreaID, ownerName: "pvp",
	})
}

func (s *Service) currentRaidTownTransition(
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
	locationSource string,
	spec runtimeRaidTownSpec,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	if characterID == 0 || character.Stats == nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", fmt.Errorf("selected character raid location is unavailable")
	}
	// 超时空：登录/重连/换频道进超时空频道时，先去问战场"这个人现在该落在哪" ——
	// 它会在等待室 NPC 位与战场记录之间给出答案，并把不在场的成员拉回等待室。
	// 只有这三个来源会问：普通掷骰进频道仍走下面的 PVF 大门。
	if spec.ownerName == "fiendwar" && (locationSource == "selected_character_fiendwar_raid_channel_runtime_pvf_gate" ||
		locationSource == "channel_reconnect_fiendwar_raid_channel_runtime_pvf_gate" ||
		locationSource == "character_list_login_fiendwar_raid_channel_runtime_pvf_gate") {
		if row, areaID, path, restored, err := s.runtimeFiendWarRejoinLocation(session, characterID); restored || err != nil {
			return character, byte(spec.townID), areaID, row, path, err
		}
	}
	area, found := s.townArea(spec.townID, spec.areaID)
	if !found || area.MapPath == "" || area.Gate == nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", fmt.Errorf(
			"runtime PVF %s raid town %d area %d has no gate",
			spec.ownerName,
			spec.townID,
			spec.areaID,
		)
	}
	if area.MinLevel > int64(character.Level) {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", fmt.Errorf(
			"%s raid town %d area %d requires level %d, character level %d",
			spec.ownerName,
			spec.townID,
			spec.areaID,
			area.MinLevel,
			character.Level,
		)
	}
	if area.Gate.X < -1<<15 || area.Gate.X > 1<<15-1 || area.Gate.Y < -1<<15 || area.Gate.Y > 1<<15-1 {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", fmt.Errorf(
			"runtime PVF %s raid gate (%d,%d) is outside i16",
			spec.ownerName,
			area.Gate.X,
			area.Gate.Y,
		)
	}
	if locationSource == "" {
		locationSource = spec.ownerName + "_raid_channel_runtime_pvf_gate"
	}
	townID := byte(spec.townID)
	areaID := byte(spec.areaID)
	row := currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
		Value1:              uint16(int16(area.Gate.X)),
		Value2:              uint16(int16(area.Gate.Y)),
		Value3:              currentAntonRaidDirection,
		Value4:              currentAntonRaidAreaState,
	}
	s.logGameEvent(session, "game-raid-town-location-used",
		"raid_owner", spec.ownerName,
		"char_id", characterID,
		"channel_id", session.channel.ID,
		"channel_type", session.channel.Type,
		"town_id", townID,
		"area_id", areaID,
		"position_x", int16(row.Value1),
		"position_y", int16(row.Value2),
		"direction", row.Value3,
		"area_state", row.Value4,
		"map_path", area.MapPath,
		"min_level", area.MinLevel,
		"durable_character_location_written", false,
		"source", locationSource)
	// 卢克团队频道入场：官方系统说明「塔罗卡牌在团队频道入场时可以看见1次，之后
	// 可以在攻坚战进行中的状况表上确认」。这条就是"登录/重连落进卢克团队频道城镇"
	// 的真实入口 —— 本函数是 game-raid-town-location-used 的唯一发出点，线上实测
	// 2026-09-29T10:19:44Z / 10:19:47Z 两个角色正是从这里落进 town 32。
	// 安徒恩（town 19）与 PVP 频道（town 10）在这一行就被 townID 挡掉，一个字不改。
	if spec.townID == currentLukeRaidTownID {
		s.publishRuntimeRaidLukeCardDisplay(session, 0, "luke_raid_channel_town_"+locationSource)
	}
	return character, townID, areaID, row, area.MapPath, nil
}

func (s *Service) currentInitialTownTransition(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	if session != nil && isPVPChannel(session.channel) {
		return s.currentPVPTownTransition(
			session,
			characterID,
			character,
			"selected_character_pvp_channel_runtime_pvf_gate",
		)
	}
	if spec, ok := currentRaidTownSpecForSession(session); ok {
		return s.currentRaidTownTransition(
			session,
			characterID,
			character,
			"selected_character_"+spec.ownerName+"_raid_channel_runtime_pvf_gate",
			spec,
		)
	}
	return s.currentPersistedTownTransition(ctx, session, characterID, character, "selected_character_persisted_location_read_only")
}

func (s *Service) currentChannelReconnectTownTransition(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	if session != nil && isPVPChannel(session.channel) {
		return s.currentPVPTownTransition(
			session,
			characterID,
			character,
			"channel_reconnect_pvp_channel_runtime_pvf_gate",
		)
	}
	if spec, ok := currentRaidTownSpecForSession(session); ok {
		return s.currentRaidTownTransition(
			session,
			characterID,
			character,
			"channel_reconnect_"+spec.ownerName+"_raid_channel_runtime_pvf_gate",
			spec,
		)
	}
	if session != nil && session.channelReconnectNewCharacter {
		s.logGameEvent(session, "game-channel-reconnect-new-character-login",
			"char_id", characterID,
			"target_channel", session.channel.ID,
			"source", "registered_client_changed_character_before_reconnect")
		return s.currentCharacterListLoginTransition(ctx, session, characterID, character)
	}
	// Earlier type-23 op36 handling accidentally persisted its temporary town
	// 19 location. Town 19 has no ordinary-channel ownership, so repair only
	// that impossible durable state through the proven ordinary login owner
	// before constructing the reconnect scene.
	if character.Stats != nil && character.Stats["town_id"] == currentAntonRaidTownID {
		s.logGameEvent(session, "game-channel-reconnect-repair-leaked-anton-town",
			"char_id", characterID,
			"channel_id", session.channel.ID,
			"channel_type", session.channel.Type,
			"leaked_town_id", currentAntonRaidTownID,
			"recovery_town_id", newCharacterInitialTownID,
			"recovery_area_id", newCharacterInitialAreaID,
			"source", "type23_op36_prevented_from_persisting_temporary_raid_town")
		return s.currentCharacterListLoginTransition(ctx, session, characterID, character)
	}
	return s.currentPersistedTownTransition(ctx, session, characterID, character, "channel_reconnect_persisted_location_read_only")
}

func (s *Service) currentPersistedTownTransition(
	ctx context.Context,
	session *gameSession,
	characterID uint16,
	character dnfrepo.CharacterRecord,
	locationSource string,
) (dnfrepo.CharacterRecord, byte, byte, currentSceneTransitionRow, string, error) {
	townID, err := requiredCurrentInitialTownU8(character, "town_id")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	areaID, err := requiredCurrentInitialTownU8(character, "area_id")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	positionX, err := requiredCurrentInitialTownCoordinate(character, "pos_x")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	positionY, err := requiredCurrentInitialTownCoordinate(character, "pos_y")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	direction, err := requiredCurrentInitialTownU8(character, "direction")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	areaState, err := requiredCurrentInitialTownU8(character, "area_state")
	if err != nil {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", err
	}
	area, found := s.townArea(int64(townID), int64(areaID))
	if !found || area.MapPath == "" {
		return dnfrepo.CharacterRecord{}, 0, 0, currentSceneTransitionRow{}, "", fmt.Errorf(
			"persisted town %d area %d has no runtime PVF map path",
			townID, areaID,
		)
	}
	if locationSource == "" {
		locationSource = "persisted_location_read_only"
	}
	s.logGameEvent(session, "game-initial-town-persisted-location-used",
		"char_id", characterID,
		"town_id", townID,
		"area_id", areaID,
		"position_x", int16(positionX),
		"position_y", int16(positionY),
		"direction", direction,
		"area_state", areaState,
		"map_path", area.MapPath,
		"need_quest_unlock_allowed", false,
		"source", locationSource)
	return character, townID, areaID, currentSceneTransitionRow{
		ObjectOrResourceKey: currentSceneActorObjectKey(characterID),
		Value1:              positionX,
		Value2:              positionY,
		Value3:              direction,
		Value4:              areaState,
	}, area.MapPath, nil
}

func requiredCurrentInitialTownCoordinate(character dnfrepo.CharacterRecord, key string) (uint16, error) {
	if character.Stats == nil {
		return 0, fmt.Errorf("selected character stats not loaded")
	}
	value, found := character.Stats[key]
	if !found {
		return 0, fmt.Errorf("selected character %s not loaded", key)
	}
	if value < -1<<15 || value > 1<<15-1 {
		return 0, fmt.Errorf("selected character %s %d is outside i16", key, value)
	}
	return uint16(int16(value)), nil
}

func requiredCurrentInitialTownU8(character dnfrepo.CharacterRecord, key string) (byte, error) {
	if character.Stats == nil {
		return 0, fmt.Errorf("selected character stats not loaded")
	}
	value, found := character.Stats[key]
	if !found {
		return 0, fmt.Errorf("selected character %s not loaded", key)
	}
	if value < 0 || value > int64(^byte(0)) {
		return 0, fmt.Errorf("selected character %s %d is outside u8", key, value)
	}
	return byte(value), nil
}
