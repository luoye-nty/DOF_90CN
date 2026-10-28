package town

import (
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// The private Seria room every town portals into.
const (
	sharedSeriaRoomTownID = 38
	sharedSeriaRoomAreaID = 1
)

// townPortalReturnMargin is how far outside the portal trigger rectangle the
// landing point is placed. The trigger is a thin doorway strip; 32 units clears
// it without leaving the walkable band around it.
const townPortalReturnMargin int64 = 32

type townMovableArea struct {
	x, y, width, height int64
	targetTown          int64
	targetArea          int64
}

// seriaRoomPortalTrigger finds the [town movable area] row that moves into the
// shared Seria room. Current maps author it in the flat six-int form; older
// maps nest it as [town movable area with animation] > [movable area] >
// [area] x y w h / [move town] town area, which the flat section reader does
// not see, so the nested form is walked token by token.
func seriaRoomPortalTrigger(doc *dnfpvf.Document) (townMovableArea, bool) {
	if doc == nil {
		return townMovableArea{}, false
	}
	if values, ok := sectionInts(doc, "town movable area"); ok {
		for offset := 0; offset+5 < len(values); offset += 6 {
			candidate := townMovableArea{
				x:          values[offset],
				y:          values[offset+1],
				width:      values[offset+2],
				height:     values[offset+3],
				targetTown: values[offset+4],
				targetArea: values[offset+5],
			}
			if candidate.width > 0 && candidate.height > 0 &&
				candidate.targetTown == sharedSeriaRoomTownID &&
				candidate.targetArea == sharedSeriaRoomAreaID {
				return candidate, true
			}
		}
	}

	insideMovableArea := false
	candidate := townMovableArea{}
	hasArea := false
	for idx, token := range doc.Tokens {
		if token.Kind != dnfpvf.TokenSection {
			continue
		}
		switch sectionName(token.Value) {
		case "movable area":
			insideMovableArea = true
			candidate = townMovableArea{}
			hasArea = false
		case "/movable area":
			insideMovableArea = false
			candidate = townMovableArea{}
			hasArea = false
		case "area":
			if !insideMovableArea {
				continue
			}
			values := followingInts(doc.Tokens, idx+1, 4)
			if len(values) == 4 && values[2] > 0 && values[3] > 0 {
				candidate.x = values[0]
				candidate.y = values[1]
				candidate.width = values[2]
				candidate.height = values[3]
				hasArea = true
			}
		case "move town":
			if !insideMovableArea || !hasArea {
				continue
			}
			values := followingInts(doc.Tokens, idx+1, 2)
			if len(values) == 2 && values[0] == sharedSeriaRoomTownID && values[1] == sharedSeriaRoomAreaID {
				candidate.targetTown = values[0]
				candidate.targetArea = values[1]
				return candidate, true
			}
		}
	}
	return townMovableArea{}, false
}

// townPortalReturnPoint picks the nearest point outside the trigger rectangle
// that still lies inside one of the map's [virtual movable area] rectangles,
// trying below, above, right, then left of the doorway.
func townPortalReturnPoint(trigger townMovableArea, virtual []int64) (Gate, bool) {
	if trigger.width <= 0 || trigger.height <= 0 {
		return Gate{}, false
	}
	centerX := trigger.x + trigger.width/2
	centerY := trigger.y + trigger.height/2
	candidates := []Gate{
		{X: centerX, Y: trigger.y + trigger.height + townPortalReturnMargin},
		{X: centerX, Y: trigger.y - townPortalReturnMargin},
		{X: trigger.x + trigger.width + townPortalReturnMargin, Y: centerY},
		{X: trigger.x - townPortalReturnMargin, Y: centerY},
	}
	for _, candidate := range candidates {
		if pointInsideAnyMovableArea(candidate, virtual) {
			return candidate, true
		}
	}
	// Older maps may omit [virtual movable area]. Their Seria portals use a
	// horizontal doorway, so the first safe point below the trigger is the
	// closest non-overlapping fallback.
	if len(virtual) < 4 {
		return candidates[0], true
	}
	return Gate{}, false
}

func pointInsideAnyMovableArea(point Gate, virtual []int64) bool {
	for offset := 0; offset+3 < len(virtual); offset += 4 {
		x, y := virtual[offset], virtual[offset+1]
		width, height := virtual[offset+2], virtual[offset+3]
		if width <= 0 || height <= 0 {
			continue
		}
		if point.X >= x && point.X <= x+width && point.Y >= y && point.Y <= y+height {
			return true
		}
	}
	return false
}

// sectionInts returns the integer tokens of the first section named name.
func sectionInts(doc *dnfpvf.Document, name string) ([]int64, bool) {
	tokens, ok := doc.Section(name)
	if !ok {
		return nil, false
	}
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values, len(values) > 0
}
