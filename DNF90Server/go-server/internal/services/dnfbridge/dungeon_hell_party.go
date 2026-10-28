package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfhellparty "longheng.io/server/internal/modules/dnf/hellparty"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentHellPartyMonsterInfoWireOpcode uint16 = 679
	currentHellPartyModeA                 byte   = 1
)

var (
	errCurrentHellPartyUnavailable = errors.New("dnf hell party is unavailable")
	errCurrentHellPartyActor       = errors.New("dnf hell party actor is invalid")
	errCurrentHellPartyMoveBlocked = errors.New("dnf hell party encounter is incomplete")
	errCurrentHellPartyLocked      = errors.New("dnf hell party is not unlocked for this region")
)

type currentDungeonHellPartyRewardPlan struct {
	ActorObjectKey uint32
	RollCount      uint32
	Suppressed     bool
	Selected       []currentDungeonHellPartySelectedDrop
}

func (s *Service) authorizeCurrentDungeonHellParty(ctx context.Context, session *gameSession, table *worldmap.Table, dungeonID int64) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || table == nil {
		return errCurrentHellPartyLocked
	}
	area, found := table.FindAreaByDungeon(dungeonID)
	if !found || !area.HellDungeon.Set || area.HellDungeon.Value != 1 || len(area.HellQuestIDs) == 0 {
		return errCurrentHellPartyLocked
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.HellPartyPermission == nil {
		return errCurrentHellPartyLocked
	}
	// The same predicate backs the selector page's hell-party button
	// (currentHellPartyButtonBlockedSlots), so the button projected to the
	// client can never disagree with this gate.
	granted, err := s.currentHellPartyAreaGranted(ctx, repositories, session.selectedCharacterID, area)
	if err != nil {
		return err
	}
	if granted {
		return nil
	}
	characterID := fmt.Sprintf("%d", session.selectedCharacterID)
	return fmt.Errorf("%w: area=%d character=%s quests=%v", errCurrentHellPartyLocked, area.ID, characterID, area.HellQuestIDs)
}

func currentHellQuestRecordUnlocksArea(record dnfrepo.QuestRecord, area worldmap.Area) bool {
	_, cleared := dungeonQuestStateSets(record)
	for _, questID := range area.HellQuestIDs {
		if questID <= 0 {
			continue
		}
		if _, ok := cleared[questID]; ok {
			return true
		}
	}
	return false
}

type currentDungeonHellPartyActor struct {
	Identity   dnfhellparty.ActorIdentity
	Code       uint32
	EntityType uint8
	Level      byte
}

type currentDungeonHellPartyOwner struct {
	Room            dnfhellparty.Room
	Runtime         *dnfhellparty.Runtime
	Mode            byte
	Actors          map[uint32]currentDungeonHellPartyActor
	MonsterInfoRows map[uint32]uint32
	MonsterInfoSent bool
	RewardPlans     []currentDungeonHellPartyRewardPlan
	DeathResponses  map[uint32][]byte
	PillarObjectKey uint32
}

func (s *Service) currentHellPartyCatalog() (*dnfhellparty.Catalog, error) {
	if s == nil {
		return nil, errCurrentHellPartyUnavailable
	}
	s.hellPartyMu.Lock()
	defer s.hellPartyMu.Unlock()
	if s.hellPartyCatalog != nil || s.hellPartyCatalogLoadErr != nil {
		return s.hellPartyCatalog, s.hellPartyCatalogLoadErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.hellPartyCatalog, err = dnfhellparty.Load(archive)
	}
	s.hellPartyCatalogLoadErr = err
	return s.hellPartyCatalog, s.hellPartyCatalogLoadErr
}

func (s *Service) preloadHellPartyCatalog(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	catalog, err := s.currentHellPartyCatalog()
	if err != nil {
		return fmt.Errorf("preload current hell party catalog: %w", err)
	}
	rules, groups, actors := catalog.Snapshot()
	s.logPacketEvent("dnf-hell-party-catalog-loaded",
		"difficulty_rules", rules,
		"groups", groups,
		"actors", actors,
		"elapsed_ms", time.Since(started).Milliseconds())
	return nil
}

// currentDungeonHellPartyRequest accepts only the two fixed 21-byte request
// shapes documented by the current select-dungeon writer. Other nonzero mode
// combinations remain fail-closed.
func currentDungeonHellPartyRequest(request dungeoncmd.SelectDungeonRequest) bool {
	if request.EntryOption != 0 || request.RuntimeState != 0 || request.Reserved != 0 ||
		request.PartyState != 0 || len(request.OpaqueTail) != 0 {
		return false
	}
	dedicated := request.SelectionMode == 1 && request.RuntimeToken == 0xffff && request.SpecialMode == 0
	manual := request.SelectionMode == 0 && request.RuntimeToken == 0xffff && request.SpecialMode == 1
	return dedicated || manual
}

// currentDungeonSelectedMazeEscapeHell identifies the third PVF-owned hell
// route: a [hell dungeon] whose selected [maze info] carries [escape hell].
// The marker is intentionally evaluated only for the selected maze. Scanning
// every maze would misclassify an ordinary route merely because a sibling
// route owns the escape-hell state machine.
func currentDungeonSelectedMazeEscapeHell(
	request dungeoncmd.SelectDungeonRequest,
	dungeon worldmap.Dungeon,
	mazeIndex int,
) bool {
	if !currentDungeonHellPartyRequest(request) || mazeIndex < 0 || mazeIndex >= len(dungeon.Mazes) {
		return false
	}
	hellDungeon, found := dungeon.Metadata.IntegerValues["hell dungeon"]
	if !found || hellDungeon != 1 {
		return false
	}
	escapeHell := dungeon.Mazes[mazeIndex].EscapeHell
	return escapeHell.Set && escapeHell.Value == 1
}

// currentDungeonExplicitHellPartyRequest separates the shared selector wire
// shape from the dungeon's PVF-owned runtime. Inferno, "particular hell", and
// selected escape-hell mazes use selection_mode=1/token=UINT16_MAX as their
// standalone entry request; they do not own a seal-door room and must not be
// rewritten into the optional in-dungeon hell-party encounter.
func currentDungeonExplicitHellPartyRequest(
	request dungeoncmd.SelectDungeonRequest,
	dungeon worldmap.Dungeon,
	mazeIndex int,
) bool {
	return currentDungeonHellPartyRequest(request) &&
		!currentDungeonStandaloneRiftForSelection(request, dungeon, mazeIndex)
}

// currentDungeonStandaloneRift identifies PVF-owned hell routes whose boss is
// the reward owner. They share parts of the selector and drop tables with an
// optional in-dungeon hell party, but do not contain a seal-door encounter.
func currentDungeonStandaloneRift(dungeon worldmap.Dungeon) bool {
	return dungeon.Metadata.InfernoDungeon ||
		normalizeDungeonPVFSymbol(dungeon.Metadata.DungeonType) == "particular hell"
}

func currentDungeonStandaloneRiftForSelection(
	request dungeoncmd.SelectDungeonRequest,
	dungeon worldmap.Dungeon,
	mazeIndex int,
) bool {
	return currentDungeonStandaloneRift(dungeon) ||
		currentDungeonSelectedMazeEscapeHell(request, dungeon, mazeIndex)
}

func currentDungeonStandaloneRiftRuntime(runtime *runtimeDungeonState) bool {
	return runtime != nil && currentDungeonStandaloneRiftForSelection(
		runtime.Request,
		runtime.Dungeon,
		runtime.MazeIndex,
	)
}

// currentDungeonTimeRiftRuntime is narrower than the boss-reward predicate:
// it includes Time Rift and selected escape-hell mazes, but deliberately
// excludes Magic Rift's [particular hell] route. Direction-specific features
// such as epic-wish snapshots must not leak onto dungeon 316 merely because
// all three routes share the standalone boss reward owner.
func currentDungeonTimeRiftRuntime(runtime *runtimeDungeonState) bool {
	if runtime == nil || normalizeDungeonPVFSymbol(runtime.Dungeon.Metadata.DungeonType) == "particular hell" {
		return false
	}
	return runtime.Dungeon.Metadata.InfernoDungeon || currentDungeonSelectedMazeEscapeHell(
		runtime.Request,
		runtime.Dungeon,
		runtime.MazeIndex,
	)
}

func (s *Service) currentDungeonStandaloneRiftRewardRollCount() (uint32, error) {
	catalog, err := s.currentHellPartyCatalog()
	if err != nil {
		return 0, err
	}
	rule, found := catalog.Difficulty("A")
	if !found || rule.RewardRollCount == 0 {
		return 0, fmt.Errorf("%w: standalone rift difficulty A is unavailable", errCurrentHellPartyUnavailable)
	}
	return rule.RewardRollCount, nil
}

func (s *Service) prepareCurrentDungeonHellParty(
	table *worldmap.Table,
	topology *worldmap.DungeonTopology,
	dungeon worldmap.Dungeon,
	mazeIndex int,
	seed uint32,
) (*currentDungeonHellPartyOwner, error) {
	catalog, err := s.currentHellPartyCatalog()
	if err != nil {
		return nil, err
	}
	room, err := dnfhellparty.ResolveRoom(table, topology, dungeon.ID, mazeIndex)
	if err != nil {
		return nil, err
	}
	plan, err := dnfhellparty.BuildPlan(catalog, dnfhellparty.PlanOptions{
		GroupKey: "A",
		Seed:     uint64(seed),
		Entries:  room.Entries,
		SuppressGroupReward: func(code uint32, entityType uint8) bool {
			return s.currentDungeonHellMonsterScript(code, entityType)
		},
	})
	if err != nil {
		return nil, err
	}
	runtime, err := dnfhellparty.NewRuntime(plan)
	if err != nil {
		return nil, err
	}
	if err := dnfhellparty.ApplyRoom(topology, room); err != nil {
		return nil, err
	}
	return &currentDungeonHellPartyOwner{
		Room:            room,
		Runtime:         runtime,
		Mode:            currentHellPartyModeA,
		Actors:          make(map[uint32]currentDungeonHellPartyActor),
		MonsterInfoRows: make(map[uint32]uint32),
		DeathResponses:  make(map[uint32][]byte),
	}, nil
}

func (s *Service) currentDungeonHellMonsterScript(code uint32, entityType uint8) bool {
	if code == 0 {
		return false
	}
	if entityType == 0 {
		catalog, err := s.dungeonMonsterCatalog()
		if err != nil {
			return false
		}
		value, err := catalog.HellMonster(int64(code))
		return err == nil && value
	}
	catalog, err := s.dungeonAICharacterCatalog()
	if err != nil {
		return false
	}
	definition, found, err := catalog.Find(int64(code))
	if err != nil || !found {
		return false
	}
	for _, section := range definition.Sections {
		if normalizeDungeonPVFSymbol(section.Name) != "hell monster" {
			continue
		}
		for _, token := range section.Tokens {
			if token.Int == 1 {
				return true
			}
		}
	}
	return false
}

func (s *Service) appendCurrentDungeonHellPartyActors(
	plan runtimeDungeonExtendedActorPlan,
	owner *currentDungeonHellPartyOwner,
	scene worldmap.DungeonRoomScene,
	basis worldmap.OptionalInt,
	monsterCatalog *pvfDungeonMonsterCatalog,
	aiCatalog *pvfDungeonAICharacterCatalog,
) (runtimeDungeonExtendedActorPlan, error) {
	if owner == nil || owner.Runtime == nil || scene.Coordinate != owner.Room.Coordinate || scene.Map.Map.ID != owner.Room.HellMap.ID {
		return plan, nil
	}
	packetIndex := len(scene.Monsters) + len(plan.Actors)
	for _, wave := range owner.Runtime.Plan().Waves {
		if wave.Order == 0 || wave.Order > uint32(^uint16(0)) {
			return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: wave order=%d", errCurrentHellPartyActor, wave.Order)
		}
		for _, planned := range wave.Actors {
			level := byte(0)
			actorType := byte(0)
			switch planned.EntityType {
			case 0:
				if monsterCatalog == nil {
					return runtimeDungeonExtendedActorPlan{}, errDungeonMonsterCatalogUnavailable
				}
				if _, found, err := monsterCatalog.Find(int64(planned.Code)); err != nil || !found {
					if err != nil {
						return runtimeDungeonExtendedActorPlan{}, err
					}
					return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: monster=%d", errDungeonMonsterDefinitionMiss, planned.Code)
				}
				if !basis.Set || basis.Value <= 0 || basis.Value > 0xff {
					return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: basis_level=%d set=%t", errCurrentHellPartyActor, basis.Value, basis.Set)
				}
				level = byte(basis.Value)
			case 1:
				if aiCatalog == nil {
					var err error
					aiCatalog, err = s.dungeonAICharacterCatalog()
					if err != nil {
						return runtimeDungeonExtendedActorPlan{}, err
					}
				}
				definition, found, err := aiCatalog.Find(int64(planned.Code))
				if err != nil {
					return runtimeDungeonExtendedActorPlan{}, err
				}
				if !found {
					return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: AI=%d", errDungeonAICharacterDefinitionMiss, planned.Code)
				}
				level = definition.Level
				actorType = 5
			default:
				return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: entity_type=%d", errCurrentHellPartyActor, planned.EntityType)
			}
			actor, err := newRuntimeDungeonExtendedActor(
				runtimeDungeonActorHellParty,
				plan.NextObjectKey,
				int64(planned.Code),
				currentDungeonStartMapActor{
					TemplateOrder: uint16(wave.Order),
					PacketIndex:   uint32(packetIndex),
					Level:         level,
					Type:          actorType,
					Flag0:         1,
					Flag1:         0xff,
					Blocking:      1,
				},
			)
			if err != nil {
				return runtimeDungeonExtendedActorPlan{}, err
			}
			if _, exists := owner.Actors[actor.ObjectKey]; exists {
				return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: duplicate object_key=%d", errCurrentHellPartyActor, actor.ObjectKey)
			}
			owner.Actors[actor.ObjectKey] = currentDungeonHellPartyActor{
				Identity:   planned.Identity,
				Code:       planned.Code,
				EntityType: planned.EntityType,
				Level:      level,
			}
			if _, exists := owner.MonsterInfoRows[planned.Code]; !exists {
				owner.MonsterInfoRows[planned.Code] = uint32(level)
			}
			plan.Actors = append(plan.Actors, actor)
			plan.NextObjectKey++
			packetIndex++
		}
	}
	for _, actor := range plan.Actors {
		if actor.Kind == runtimeDungeonActorSpecialObject && uint32(actor.Packet.Code) == uint32(owner.Room.Pillar.ObjectID) {
			owner.PillarObjectKey = actor.ObjectKey
			break
		}
	}
	if len(scene.Monsters)+len(plan.Actors) > 0xff {
		return runtimeDungeonExtendedActorPlan{}, fmt.Errorf("%w: normal=%d extended=%d", errDungeonStartMapActorCount, len(scene.Monsters), len(plan.Actors))
	}
	return plan, nil
}

func buildCurrentHellPartyMonsterInfoBody(rows map[uint32]uint32) []byte {
	codes := make([]uint32, 0, len(rows))
	for code := range rows {
		if code != 0 {
			codes = append(codes, code)
		}
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	var writer packetWriter
	writer.writeUint32(uint32(len(codes)))
	for _, code := range codes {
		level := rows[code]
		if level == 0 {
			level = 1
		}
		writer.writeUint32(code)
		writer.writeUint32(level)
	}
	return writer.bytes()
}

func (s *Service) sendCurrentHellPartyMonsterInfo(session *gameSession, runtime *runtimeDungeonState, scene worldmap.DungeonRoomScene, source string) error {
	if runtime == nil || runtime.HellParty == nil || runtime.HellParty.MonsterInfoSent ||
		scene.Coordinate != runtime.HellParty.Room.Coordinate || scene.Map.Map.ID != runtime.HellParty.Room.HellMap.ID {
		return nil
	}
	body := buildCurrentHellPartyMonsterInfoBody(runtime.HellParty.MonsterInfoRows)
	if len(body) <= 4 {
		return nil
	}
	if err := s.sendGameUpperRawClass(session, currentHellPartyMonsterInfoWireOpcode, body, 0); err != nil {
		return err
	}
	runtime.HellParty.MonsterInfoSent = true
	s.logGameEvent(session, "game-dungeon-hell-party-monster-info-sent",
		"source", source,
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"msg_id", currentHellPartyMonsterInfoWireOpcode,
		"classification", 0,
		"body_len", len(body),
		"row_count", len(runtime.HellParty.MonsterInfoRows))
	return nil
}

// sendCurrentHellPartyMonsterInfoAfterFinishLoading publishes the typed Hell
// Party monster table only after the encounter room's op37/op30 boundary.
// Current-EXE sub_1D39E60 proves op679 is only a count followed by code/level
// rows; a zero-row body has no reset semantics and must not be invented for
// ordinary, story, altar, or non-encounter rooms.
func (s *Service) sendCurrentHellPartyMonsterInfoAfterFinishLoading(session *gameSession, source string) error {
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil {
		return nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return nil
	}
	if runtime.HellParty != nil {
		if scene.Coordinate == runtime.HellParty.Room.Coordinate &&
			scene.Map.Map.ID == runtime.HellParty.Room.HellMap.ID {
			return s.sendCurrentHellPartyMonsterInfo(session, runtime, scene, source)
		}
	}
	return nil
}

func currentDungeonHellPartyMoveBlocked(runtime *runtimeDungeonState, source worldmap.DungeonRoomScene) bool {
	return runtime != nil && runtime.HellParty != nil && runtime.HellParty.Runtime != nil &&
		source.Coordinate == runtime.HellParty.Room.Coordinate &&
		source.Map.Map.ID == runtime.HellParty.Room.HellMap.ID &&
		runtime.HellParty.Runtime.Phase() == dnfhellparty.PhaseStarted
}
