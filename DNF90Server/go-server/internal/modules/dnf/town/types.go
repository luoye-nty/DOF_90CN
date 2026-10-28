package town

import (
	"errors"
	"sort"
)

const DefaultList = "town/town.lst"

var (
	ErrSourceRequired = errors.New("dnf town pvf source is required")
	ErrListEmpty      = errors.New("dnf town list is empty")
	ErrDocumentEmpty  = errors.New("dnf town document is empty")
)

type Source interface {
	ReadText(string) (string, error)
}

type Options struct {
	ListPath string
}

type Gate struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type NPCSpawn struct {
	NPCID     int64  `json:"npc_id"`
	Direction string `json:"direction,omitempty"`
	X         int64  `json:"x"`
	Y         int64  `json:"y"`
	Z         int64  `json:"z"`
}

// MovableArea preserves a client-authored [town movable area] rectangle.  In
// the current PVF many rows are narrow portal/transition trigger regions, not
// a complete map-wide walkable polygon, so callers must not treat their union
// as unrestricted actor placement space.
type MovableArea struct {
	X      int64 `json:"x"`
	Y      int64 `json:"y"`
	Width  int64 `json:"width"`
	Height int64 `json:"height"`
}

type Area struct {
	ID          int64   `json:"id"`
	MapPath     string  `json:"map_path"`
	Kind        string  `json:"kind,omitempty"`
	Gate        *Gate   `json:"gate,omitempty"`
	DungeonGate *int64  `json:"dungeon_gate,omitempty"`
	MinLevel    int64   `json:"min_level,omitempty"`
	NeedQuests  []int64 `json:"need_quests,omitempty"`
	// QuestConditions are the repeated [quest condition] owners declared by
	// this area's map. Unlike NeedQuests they do not block entering the area;
	// the client consults them while constructing quest-gated passages and map
	// animations for the scene, so finishing one of them needs a same-area
	// scene rebuild before the passage appears.
	QuestConditions      []int64       `json:"quest_conditions,omitempty"`
	TownExitTargetNPCIDs []int64       `json:"town_exit_target_npc_ids,omitempty"`
	NPCs                 []NPCSpawn    `json:"npcs,omitempty"`
	MovableAreas         []MovableArea `json:"movable_areas,omitempty"`
	// SeriaRoomReturn is the safe town-side landing point derived from this
	// area's portal into the shared Seria room (town 38 area 1): the nearest
	// point outside the portal trigger rectangle and inside [virtual movable
	// area]. Landing inside the trigger keeps the client's overlap latch armed
	// and re-enters the room. Nil when the map declares no such portal.
	SeriaRoomReturn *Gate `json:"seria_room_return,omitempty"`
	// WeddingRoomReturn is the safe landing point on this area's side of the
	// portal into the couple room (town 101). Leaving the couple room re-enters
	// this scene, so landing inside the trigger rectangle would bounce the
	// character straight back. Nil when the map declares no such portal.
	WeddingRoomReturn *Gate `json:"wedding_room_return,omitempty"`
}

type Town struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	Name  string `json:"name"`
	Areas []Area `json:"areas"`
}

type Table struct {
	towns map[int64]Town
}

type Snapshot struct {
	Towns int `json:"towns"`
	Areas int `json:"areas"`
}

func (t *Table) Find(townID int64) (Town, bool) {
	if t == nil {
		return Town{}, false
	}
	value, ok := t.towns[townID]
	return cloneTown(value), ok
}

// Towns returns a deterministic cloned snapshot of every loaded PVF town.
func (t *Table) Towns() []Town {
	if t == nil || len(t.towns) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(t.towns))
	for id := range t.towns {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]Town, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneTown(t.towns[id]))
	}
	return out
}

func (t *Table) FindArea(townID, areaID int64) (Area, bool) {
	town, ok := t.Find(townID)
	if !ok {
		return Area{}, false
	}
	for _, area := range town.Areas {
		if area.ID == areaID {
			return cloneArea(area), true
		}
	}
	return Area{}, false
}

// AllowsNPCEnterTown reports whether the target NPC is proven by the runtime
// PVF as an [enter town] destination, owns an [exit town] role, and is spawned
// on the requested town-area map.
func (t *Table) AllowsNPCEnterTown(townID, areaID, targetNPCID int64) bool {
	if targetNPCID <= 0 {
		return false
	}
	area, ok := t.FindArea(townID, areaID)
	if !ok {
		return false
	}
	for _, npcID := range area.TownExitTargetNPCIDs {
		if npcID == targetNPCID {
			return true
		}
	}
	return false
}

// FindGateArea returns the first gate area declared by the requested town.
// Gate coordinates and area ownership come from the runtime PVF catalog.
func (t *Table) FindGateArea(townID int64) (Area, bool) {
	town, ok := t.Find(townID)
	if !ok {
		return Area{}, false
	}
	for _, area := range town.Areas {
		if area.Kind == "gate" && area.Gate != nil {
			return cloneArea(area), true
		}
	}
	return Area{}, false
}

// FindSeriaRoomReturnArea returns the town area that owns the PVF portal into
// the shared Seria room. A Seria-room exit request carries no destination, so
// callers use this reverse edge and its safe landing point rather than the
// character's unrelated pre-login area and coordinates.
func (t *Table) FindSeriaRoomReturnArea(townID int64) (Area, bool) {
	town, ok := t.Find(townID)
	if !ok {
		return Area{}, false
	}
	for _, area := range town.Areas {
		if area.SeriaRoomReturn != nil {
			return cloneArea(area), true
		}
	}
	return Area{}, false
}

func (t *Table) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	snapshot := Snapshot{Towns: len(t.towns)}
	for _, town := range t.towns {
		snapshot.Areas += len(town.Areas)
	}
	return snapshot
}

func cloneTown(value Town) Town {
	value.Areas = append([]Area(nil), value.Areas...)
	for idx := range value.Areas {
		value.Areas[idx] = cloneArea(value.Areas[idx])
	}
	return value
}

func cloneArea(value Area) Area {
	value.NeedQuests = append([]int64(nil), value.NeedQuests...)
	if value.QuestConditions != nil {
		value.QuestConditions = append([]int64(nil), value.QuestConditions...)
	}
	value.TownExitTargetNPCIDs = append([]int64(nil), value.TownExitTargetNPCIDs...)
	value.NPCs = append([]NPCSpawn(nil), value.NPCs...)
	value.MovableAreas = append([]MovableArea(nil), value.MovableAreas...)
	if value.Gate != nil {
		gate := *value.Gate
		value.Gate = &gate
	}
	if value.DungeonGate != nil {
		dungeonGate := *value.DungeonGate
		value.DungeonGate = &dungeonGate
	}
	if value.SeriaRoomReturn != nil {
		seriaReturn := *value.SeriaRoomReturn
		value.SeriaRoomReturn = &seriaReturn
	}
	if value.WeddingRoomReturn != nil {
		weddingReturn := *value.WeddingRoomReturn
		value.WeddingRoomReturn = &weddingReturn
	}
	return value
}
