package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"slices"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

const (
	currentDespairTowerOpponentMsgID    uint16 = 347
	currentDespairTowerClearRewardMsgID uint16 = 348
	currentGriefTowerClearRewardMsgID   uint16 = 1263
)

// The current EXE waits at 0x3423D22 for byte_51B2871 on PVF [npc layer]
// floors. op347/sub_19B1250 sets that flag; op28/op29 alone never release it.
func (s *Service) sendCurrentTowerOfDespairOpponent(session *gameSession, runtime *runtimeDungeonState) error {
	if runtime == nil || !currentDungeonIsTowerOfDespair(runtime.Dungeon) {
		return nil
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return err
	}
	floor, err := resolveCurrentDungeonTowerFloor(archive, runtime.Dungeon, currentDespairTowerClearRewardMsgID)
	if err != nil {
		return err
	}
	document, err := parseDungeonCardPVFDocument(archive, "etc/towerofdespair.etc")
	if err != nil {
		return err
	}
	layers, err := dungeonCardPVFSectionInts(document, "npc layer")
	if err != nil {
		return err
	}
	if !slices.Contains(layers, int64(floor)) {
		return nil
	}
	// Native empty-name fallback (0x19B1508 -> sub_17F0950) copies the current
	// player's real name, level, job/growth, equipment and pet. The existing
	// PVF dummy APC 31505 is resolved from that descriptor at 0x2363556.
	// Wire: active:u8, name:str32, level/job/growth:3*u8, str32, u32,
	// equipment:26*u32, str32, pet:u32, tail:u8. All strings are empty.
	body := make([]byte, 129)
	body[0] = 1
	if err := s.sendGameUpperRawClass(session, currentDespairTowerOpponentMsgID, body, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-despair-tower-opponent-sent",
		"dungeon_id", runtime.Dungeon.ID, "floor", floor,
		"msg_id", currentDespairTowerOpponentMsgID, "body_len", len(body),
		"source", "current_exe_empty_name_self_opponent",
		"packet_order", "op28-op347-op29")
	return nil
}

// resolveCurrentDungeonTowerFloor maps a tower dungeon ID back to its PVF floor
// using [top layer] and [each layer matching dungeon]. A dungeon mapped twice,
// or not mapped at all, is a PVF fault rather than a silent default.
func resolveCurrentDungeonTowerFloor(source dnfpvf.Source, dungeon worldmap.Dungeon, msgID uint16) (uint16, error) {
	path := "etc/towerofdespair.etc"
	if msgID == currentGriefTowerClearRewardMsgID {
		path = "etc/towerofgrief.etc"
	} else if msgID != currentDespairTowerClearRewardMsgID {
		return 0, errCurrentDungeonSettlementPVFInvalid
	}
	document, err := parseDungeonCardPVFDocument(source, path)
	if err != nil {
		return 0, err
	}
	top, ok := document.Int("top layer")
	rows := document.Ints("each layer matching dungeon")
	if !ok || top <= 0 || top > math.MaxUint16 || len(rows) == 0 || len(rows)%3 != 0 {
		return 0, fmt.Errorf("%w: tower layer table %s", errCurrentDungeonSettlementPVFInvalid, path)
	}
	var floor uint16
	for i := 0; i < len(rows); i += 3 {
		if rows[i] <= 0 || rows[i] > top || rows[i+1] <= 0 {
			return 0, fmt.Errorf("%w: tower layer row %d", errCurrentDungeonSettlementPVFInvalid, i/3)
		}
		if rows[i+1] == dungeon.ID {
			if floor != 0 {
				return 0, fmt.Errorf("%w: duplicate tower dungeon %d", errCurrentDungeonSettlementPVFInvalid, dungeon.ID)
			}
			floor = uint16(rows[i])
		}
	}
	if floor == 0 {
		return 0, fmt.Errorf("%w: tower dungeon %d is not mapped", errCurrentDungeonSettlementPVFInvalid, dungeon.ID)
	}
	return floor, nil
}

// resolveCurrentTowerOfDespairSoloDungeon advances over PVF floors that are
// defined for the official player-challenge service but have no complete
// single-player map/hostile actor in Script.pvf. The decision is derived from
// the resolved PVF layout, so the implementation does not maintain a floor-ID
// blacklist.
func resolveCurrentTowerOfDespairSoloDungeon(
	table *worldmap.Table,
	resolver *worldmap.Resolver,
	requestedID int64,
) (int64, []int64, error) {
	if table == nil || resolver == nil {
		return requestedID, nil, errDungeonWorldMapUnavailable
	}
	dungeon, found := table.FindDungeon(requestedID)
	if !found || !currentDungeonIsTowerOfDespair(dungeon) {
		return requestedID, nil, nil
	}
	skipped := make([]int64, 0, 4)
	for candidateID := requestedID; ; candidateID++ {
		candidate, ok := table.FindDungeon(candidateID)
		if !ok || !currentDungeonIsTowerOfDespair(candidate) {
			return requestedID, skipped, fmt.Errorf("%w: tower floor after dungeon=%d", worldmap.ErrDungeonNotIndexed, candidateID-1)
		}
		playable, err := currentTowerOfDespairFloorSoloPlayable(resolver, candidate)
		if err == nil && playable {
			return candidateID, skipped, nil
		}
		if err != nil && !errors.Is(err, worldmap.ErrMapPoolEmpty) && !errors.Is(err, worldmap.ErrMapUnresolved) {
			return requestedID, skipped, err
		}
		skipped = append(skipped, candidateID)
	}
}

func currentDungeonIsTowerOfDespair(dungeon worldmap.Dungeon) bool {
	if dungeon.Metadata.Flags["tower of despair"] {
		return true
	}
	for _, section := range dungeon.SourceSections {
		if normalizeDungeonPVFSymbol(section.Name) == "tower of despair" {
			return true
		}
	}
	return false
}

func currentTowerOfDespairFloorSoloPlayable(resolver *worldmap.Resolver, dungeon worldmap.Dungeon) (bool, error) {
	topology, err := worldmap.BuildDungeonLayout(
		resolver,
		dungeon.ID,
		0,
		func(choice worldmap.DungeonMapChoice) (int64, error) {
			if len(choice.Candidates) == 0 {
				return 0, worldmap.ErrMapChoiceRequired
			}
			return choice.Candidates[0].ID, nil
		},
	)
	if err != nil {
		return false, err
	}
	for _, room := range topology.Rooms() {
		if room.Map == nil {
			continue
		}
		if len(room.Map.Map.Monsters) != 0 || len(room.Map.Map.MonsterTeam) != 0 {
			return true, nil
		}
		for _, actor := range room.Map.Map.AICharacters {
			if normalizeDungeonPVFSymbol(actor.Faction) == "monster" {
				return true, nil
			}
		}
	}
	return false, nil
}
