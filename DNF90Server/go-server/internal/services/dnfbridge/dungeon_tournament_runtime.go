package dnfbridge

import (
	"errors"
	"fmt"

	"longheng.io/server/internal/modules/dnf/worldmap"
)

var errTournamentObjectKeyRange = errors.New("dnf tournament actor object key exceeds current EXE range")

func (s *Service) prepareCurrentTournamentRuntime(
	table *worldmap.Table,
	dungeon worldmap.Dungeon,
	scene worldmap.DungeonRoomScene,
	nextObjectKey uint32,
) (*worldmap.TournamentRuntime, uint32, error) {
	if dungeon.Metadata.Flags == nil || !dungeon.Metadata.Flags["tournament dungeon"] {
		return nil, nextObjectKey, nil
	}
	if table == nil || scene.Map.Map.ID <= 0 {
		return nil, nextObjectKey, errTournamentProtocolRuntime
	}
	definition, err := table.TournamentDefinition(dungeon.ID, scene.Map.Map.ID, 1, nil)
	if err != nil {
		return nil, nextObjectKey, err
	}
	tournament, err := worldmap.NewTournamentRuntime(definition, s.chooseDungeonIndex)
	if err != nil {
		return nil, nextObjectKey, err
	}
	if nextObjectKey == 0 || nextObjectKey > uint32(^uint16(0)) ||
		len(tournament.PathActors) == 0 ||
		nextObjectKey+uint32(len(tournament.PathActors))-1 > uint32(^uint16(0)) {
		return nil, nextObjectKey, fmt.Errorf("%w: first=%d count=%d", errTournamentObjectKeyRange, nextObjectKey, len(tournament.PathActors))
	}
	if !tournament.BindFirstActorSequence(uint16(nextObjectKey)) {
		return nil, nextObjectKey, fmt.Errorf("%w: first=%d", errTournamentObjectKeyRange, nextObjectKey)
	}
	return tournament, nextObjectKey + uint32(len(tournament.PathActors)), nil
}

// appendCurrentTournamentActors materializes the four PVF-selected path
// opponents in the ordinary op29 actor table. They are deliberately extended
// actors without a HostileReference: tournament progress is owned by
// TournamentRuntime, not by ordinary room-clear accounting.
func appendCurrentTournamentActors(
	plan *runtimeDungeonExtendedActorPlan,
	runtime *worldmap.TournamentRuntime,
) error {
	if plan == nil || runtime == nil {
		return nil
	}
	first := runtime.FirstActorSequence()
	if first == 0 || len(runtime.PathActors) == 0 {
		return worldmap.ErrTournamentSequenceUnbound
	}
	if plan.NextObjectKey != uint32(first) {
		return fmt.Errorf("tournament actor sequence does not match extended plan: first=%d next=%d", first, plan.NextObjectKey)
	}
	for index, actor := range runtime.PathActors {
		objectKey := uint32(first) + uint32(index)
		packet := currentDungeonStartMapActor{
			PacketIndex: uint32(index),
			ObjectKey:   uint16(objectKey),
			Code:        actor.Code,
			Level:       actor.Level,
			Type:        actor.Type,
		}
		if objectKey == 0 || objectKey > uint32(^uint16(0)) {
			return fmt.Errorf("%w: object_key=%d", errTournamentObjectKeyRange, objectKey)
		}
		plan.Actors = append(plan.Actors, runtimeDungeonExtendedActor{
			Kind:      runtimeDungeonActorTournament,
			ObjectKey: objectKey,
			Packet:    packet,
			State:     runtimeDungeonMonsterPlanned,
		})
	}
	plan.NextObjectKey += uint32(len(runtime.PathActors))
	return nil
}

func (s *Service) sendCurrentTournamentEntryProjection(
	session *gameSession,
	runtime *runtimeDungeonState,
	scene worldmap.DungeonRoomScene,
) error {
	if runtime == nil || runtime.tournament == nil {
		return nil
	}
	infoBody, err := buildCurrentTournamentInfoBody(runtime.tournament, runtime.Request.Difficulty)
	if err != nil {
		return err
	}
	if scene.Coordinate.X < 0 || scene.Coordinate.X > 0xff || scene.Coordinate.Y < 0 || scene.Coordinate.Y > 0xff {
		return fmt.Errorf("%w: room=%s", errDungeonStartMapCoordinateRange, scene.Coordinate)
	}
	mapBody, err := (currentTournamentMapInfo{
		RoomX:     byte(scene.Coordinate.X),
		RoomY:     byte(scene.Coordinate.Y),
		Seed:      runtime.Seed,
		RoomState: 0,
		Operation: 1,
		MapID:     runtime.tournament.Definition.MapID,
	}).Build()
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentTournamentInfoNotification, infoBody, 0); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentTournamentMapInfoNotification, mapBody, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-tournament-entry-projection-sent",
		"dungeon_id", runtime.Dungeon.ID,
		"map_id", scene.Map.Map.ID,
		"room", scene.Coordinate.String(),
		"first_actor_sequence", runtime.tournament.FirstActorSequence(),
		"path_actor_count", len(runtime.tournament.PathActors),
		"op386_body_len", len(infoBody),
		"op387_body_len", len(mapBody),
		"sequence", "current_exe_op28_op29_then_op386_op387")
	return nil
}

func currentTournamentDeathResponseKind(actor worldmap.TournamentActor) currentDungeonDeathResponseKind {
	if actor.Kind == worldmap.TournamentActorAICharacter {
		return currentDungeonDeathResponseAICharacter
	}
	return currentDungeonDeathResponseMonster
}
