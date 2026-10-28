package dnfbridge

// currentTownPrevVillageDoorOrigin projects a Seria-room exit origin onto the
// PVF door of the town it returns to. The exit request (op1425, or op36 back
// into 38) carries no destination; without this the character lands on the
// coordinates it had before entering the room. Those are the entry-trigger
// rectangle itself when the room was entered on foot, which re-arms the
// client's overlap latch and walks the character straight back in, or an
// unrelated pre-login area when the room was entered from character select.
//
// The projection keeps the town and character identity of origin and replaces
// area and position with the map-derived landing point next to the portal.
// Towns whose maps declare no Seria portal (or one that fails wire bounds)
// leave the origin untouched, so nothing regresses where the catalog is silent.
func (s *Service) currentTownPrevVillageDoorOrigin(
	origin currentTownPositionSnapshot,
) (currentTownPositionSnapshot, string, bool) {
	if s == nil || origin.CharacterID == 0 || !origin.PositionValid {
		return origin, "", false
	}
	area, found := s.townSeriaRoomReturnArea(int64(origin.TownID))
	if !found || area.SeriaRoomReturn == nil ||
		area.ID < 0 || area.ID > int64(^byte(0)) ||
		area.SeriaRoomReturn.X < 0 || area.SeriaRoomReturn.X > int64(^uint16(0)) ||
		area.SeriaRoomReturn.Y < 0 || area.SeriaRoomReturn.Y > int64(^uint16(0)) {
		return origin, "", false
	}
	projected := origin
	projected.AreaID = byte(area.ID)
	projected.PositionX = uint16(area.SeriaRoomReturn.X)
	projected.PositionY = uint16(area.SeriaRoomReturn.Y)
	projected.MovementCode = newCharacterInitialDirection
	projected.PositionValid = true
	return projected, area.MapPath, true
}
