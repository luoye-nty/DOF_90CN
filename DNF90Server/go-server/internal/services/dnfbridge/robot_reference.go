package dnfbridge

import (
	"path"
	"sort"
	"strconv"
	"strings"

	dnftown "longheng.io/server/internal/modules/dnf/town"
)

type RobotReferenceChannel struct {
	ID    int    `json:"id"`
	Group string `json:"group"`
	Type  uint8  `json:"type"`
}

type RobotReferenceArea struct {
	ID      int    `json:"id"`
	MapPath string `json:"map_path"`
	Name    string `json:"name"`
	Kind    string `json:"kind,omitempty"`
	X       uint16 `json:"safe_x"`
	Y       uint16 `json:"safe_y"`
}

type RobotReferenceTown struct {
	ID    int                  `json:"id"`
	Name  string               `json:"name"`
	Areas []RobotReferenceArea `json:"areas"`
}

type RobotReferenceCatalog struct {
	Channels []RobotReferenceChannel `json:"channels"`
	Towns    []RobotReferenceTown    `json:"towns"`
}

type RobotPlacementBounds struct {
	MinX uint16
	MaxX uint16
	MinY uint16
	MaxY uint16
}

// RobotReferenceCatalog exposes the channel and PVF town/area catalogs needed
// by the GM robot editor. Area names are intentionally derived from map paths:
// the PVF has no authoritative area display-name field.
func (s *Service) RobotReferenceCatalog() RobotReferenceCatalog {
	result := RobotReferenceCatalog{}
	if s == nil {
		return result
	}
	if catalog := s.currentCatalog(); catalog != nil {
		channels := catalog.Channels()
		result.Channels = make([]RobotReferenceChannel, 0, len(channels))
		for _, channel := range channels {
			if channel.ID <= 0 {
				continue
			}
			result.Channels = append(result.Channels, RobotReferenceChannel{
				ID: channel.ID, Group: channel.Group, Type: channel.Type,
			})
		}
		sort.Slice(result.Channels, func(left, right int) bool {
			return result.Channels[left].ID < result.Channels[right].ID
		})
	}
	s.townCatalogMu.RLock()
	table := s.townCatalog
	s.townCatalogMu.RUnlock()
	if table == nil {
		return result
	}
	towns := table.Towns()
	result.Towns = make([]RobotReferenceTown, 0, len(towns))
	for _, town := range towns {
		if town.ID <= 0 || town.ID > 255 {
			continue
		}
		name := strings.TrimSpace(town.Name)
		if name == "" {
			name = "城镇" + strconv.Itoa(int(town.ID))
		}
		entry := RobotReferenceTown{ID: int(town.ID), Name: name}
		for _, area := range town.Areas {
			if area.ID < 0 || area.ID > 255 {
				continue
			}
			x, y := robotAreaSafePoint(area)
			entry.Areas = append(entry.Areas, RobotReferenceArea{
				ID: int(area.ID), MapPath: area.MapPath,
				Name: path.Base(strings.ReplaceAll(area.MapPath, "\\", "/")),
				Kind: area.Kind, X: x, Y: y,
			})
		}
		sort.Slice(entry.Areas, func(left, right int) bool {
			return entry.Areas[left].ID < entry.Areas[right].ID
		})
		if len(entry.Areas) != 0 {
			result.Towns = append(result.Towns, entry)
		}
	}
	return result
}

// ValidateRobotPlacement rejects coordinates outside every authored movable
// rectangle. The GM editor fills a safe point from RobotReferenceCatalog, but
// the server repeats this check so direct API callers cannot create robots in
// walls or sealed transition trigger areas.
func (s *Service) ValidateRobotPlacement(townID, areaID, x, y int) bool {
	if s == nil || townID < 0 || townID > 255 || areaID < 0 || areaID > 255 || x < 0 || x > 65535 || y < 0 || y > 65535 {
		return false
	}
	s.townCatalogMu.RLock()
	table := s.townCatalog
	s.townCatalogMu.RUnlock()
	if table == nil {
		return false
	}
	area, ok := table.FindArea(int64(townID), int64(areaID))
	if !ok {
		return false
	}
	if len(area.MovableAreas) == 0 {
		return true
	}
	for _, box := range area.MovableAreas {
		if int64(x) >= box.X && int64(x) < box.X+box.Width &&
			int64(y) >= box.Y && int64(y) < box.Y+box.Height {
			return true
		}
	}
	return false
}

// RobotPlacementBounds returns the authored movable rectangle containing the
// requested spawn. The robot scheduler then keeps automatic wandering inside
// this rectangle instead of walking toward a wall or sealed trigger.
func (s *Service) RobotPlacementBounds(townID, areaID, x, y int) (RobotPlacementBounds, bool) {
	if s == nil || townID < 0 || townID > 255 || areaID < 0 || areaID > 255 || x < 0 || x > 65535 || y < 0 || y > 65535 {
		return RobotPlacementBounds{}, false
	}
	s.townCatalogMu.RLock()
	table := s.townCatalog
	s.townCatalogMu.RUnlock()
	if table == nil {
		return RobotPlacementBounds{}, false
	}
	area, ok := table.FindArea(int64(townID), int64(areaID))
	if !ok || len(area.MovableAreas) == 0 {
		return RobotPlacementBounds{}, false
	}
	for _, box := range area.MovableAreas {
		if int64(x) < box.X || int64(x) >= box.X+box.Width || int64(y) < box.Y || int64(y) >= box.Y+box.Height {
			continue
		}
		return RobotPlacementBounds{
			MinX: clampRobotReferenceCoordinate(box.X),
			MaxX: clampRobotReferenceCoordinate(box.X + box.Width - 1),
			MinY: clampRobotReferenceCoordinate(box.Y),
			MaxY: clampRobotReferenceCoordinate(box.Y + box.Height - 1),
		}, true
	}
	return RobotPlacementBounds{}, false
}

// RobotMovementBoundsAround returns the authored rectangle containing the
// requested point, or a small local rectangle when the coordinate falls
// outside the PVF trigger-rectangle list. The GM editor intentionally permits
// arbitrary X/Y; the fallback only prevents automatic wandering from becoming
// unbounded.
func (s *Service) RobotMovementBoundsAround(townID, areaID, x, y int) (RobotPlacementBounds, bool) {
	if x < 0 || x > 65535 || y < 0 || y > 65535 {
		return RobotPlacementBounds{}, false
	}
	if authored, ok := s.RobotPlacementBounds(townID, areaID, x, y); ok {
		return authored, true
	}
	const radius = int64(24)
	return RobotPlacementBounds{
		MinX: clampRobotReferenceCoordinate(int64(x) - radius),
		MaxX: clampRobotReferenceCoordinate(int64(x) + radius),
		MinY: clampRobotReferenceCoordinate(int64(y) - radius),
		MaxY: clampRobotReferenceCoordinate(int64(y) + radius),
	}, true
}

func robotAreaSafePoint(area dnftown.Area) (uint16, uint16) {
	var best dnftown.MovableArea
	bestArea := int64(-1)
	for _, box := range area.MovableAreas {
		areaSize := box.Width * box.Height
		if areaSize > bestArea {
			best, bestArea = box, areaSize
		}
	}
	if bestArea > 0 {
		return clampRobotReferenceCoordinate(best.X + best.Width/2), clampRobotReferenceCoordinate(best.Y + best.Height/2)
	}
	if area.Gate != nil {
		return clampRobotReferenceCoordinate(area.Gate.X), clampRobotReferenceCoordinate(area.Gate.Y)
	}
	if len(area.NPCs) != 0 {
		return clampRobotReferenceCoordinate(area.NPCs[0].X), clampRobotReferenceCoordinate(area.NPCs[0].Y)
	}
	return 0, 0
}

func clampRobotReferenceCoordinate(value int64) uint16 {
	if value < 0 {
		return 0
	}
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}
