package dnfbridge

import (
	"fmt"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/onlineevent"
)

// Current DNF.exe registers sub_1D89E70 at notification 0x357 (855).
// Its four u32 reads are type, state, content ID, and the extra schedule
// value. sub_1DBCD80 stores {ID, state, type, extra}; the portal's
// sub_34097D0 -> sub_1DBCBD0 accepts dungeon/type 0 with state 1 or 2.
const currentDungeonScheduleNotificationID uint16 = 855

// The town's seven co-located gate NPCs use [open visible event] 2418..2424.
// 0x23A8CC5 / 0x23B030D query base-directory membership via sub_2041F70,
// not the twelve-parameter activity state. Include only today's gate in op108:
// adding all seven base rows would draw overlapping native ANI/ALS assemblies.
// Production callers supply the service clock when constructing the directory.
func currentMysteriousRuinsPortalEventID(now time.Time) uint16 {
	weekday := now.In(onlineevent.ChinaCalendar).Weekday()
	return 2418 + uint16((int(weekday)+6)%7) // Monday..Sunday
}

var currentMysteriousRuinsDungeons = [...]struct {
	id   uint32
	path string
}{
	{4906, "dungeon/gcontents/171116_dayofdungeon/chn_daydungeon_01.dgn"},
	{4907, "dungeon/gcontents/171116_dayofdungeon/chn_daydungeon_02.dgn"},
	{4908, "dungeon/gcontents/171116_dayofdungeon/chn_daydungeon_03.dgn"},
	{4909, "dungeon/gcontents/171116_dayofdungeon/chn_daydungeon_04.dgn"},
	{4910, "dungeon/gcontents/171116_dayofdungeon/chn_daydungeon_05.dgn"},
}

func currentMysteriousRuinsScheduleOpen(now time.Time, scheduleIDs []int64) bool {
	today := int64(currentMysteriousRuinsPortalEventID(now))
	for _, scheduleID := range scheduleIDs {
		if scheduleID == today {
			return true
		}
	}
	return false
}

func buildCurrentMysteriousRuinsScheduleBody(dungeonID uint32, state uint32) []byte {
	var writer packetWriter
	writer.writeUint32(0) // native dungeon schedule category
	writer.writeUint32(state)
	writer.writeUint32(dungeonID)
	writer.writeUint32(0)
	return writer.bytes()
}

func (s *Service) sendCurrentMysteriousRuinsSchedule(session *gameSession, townID, areaID byte, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 || townID != 107 || areaID != 0 {
		return nil
	}
	if s.options.pvfPath == "" {
		return nil // services without a runtime PVF do not own this activity
	}
	table, _, err := s.dungeonWorldMap()
	if err != nil {
		return err
	}
	now := s.gameplayNow()
	todayScheduleID := currentMysteriousRuinsPortalEventID(now)
	openDungeonIDs := make([]uint32, 0, len(currentMysteriousRuinsDungeons))
	closedDungeonIDs := make([]uint32, 0, len(currentMysteriousRuinsDungeons))
	states := make(map[uint32]uint32, len(currentMysteriousRuinsDungeons))
	// Validate the whole scope before the first write. Never reinterpret an ID
	// belonging to a different PVF, or invent a dungeon to complete the list.
	for _, spec := range currentMysteriousRuinsDungeons {
		dungeon, found := table.FindDungeon(int64(spec.id))
		if !found || !strings.EqualFold(strings.ReplaceAll(dungeon.Path, "\\", "/"), spec.path) {
			return fmt.Errorf("mysterious ruins schedule: dungeon %d does not match runtime PVF %s", spec.id, spec.path)
		}
		if len(dungeon.Metadata.ScheduleIDs) == 0 {
			return fmt.Errorf("mysterious ruins schedule: dungeon %d has no PVF schedule", spec.id)
		}
		if currentMysteriousRuinsScheduleOpen(now, dungeon.Metadata.ScheduleIDs) {
			states[spec.id] = 1
			openDungeonIDs = append(openDungeonIDs, spec.id)
			continue
		}
		states[spec.id] = 0
		closedDungeonIDs = append(closedDungeonIDs, spec.id)
	}
	for _, spec := range currentMysteriousRuinsDungeons {
		state := states[spec.id]
		if err := s.sendCurrentSceneFixedClass0Packet(session, currentDungeonScheduleNotificationID,
			buildCurrentMysteriousRuinsScheduleBody(spec.id, state), source+"_mysterious_ruins_schedule"); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-mysterious-ruins-schedule-published", "char_id", session.selectedCharacterID,
		"town_id", townID, "area_id", areaID, "notification", currentDungeonScheduleNotificationID,
		"schedule_id", todayScheduleID, "open_dungeon_ids", openDungeonIDs,
		"closed_dungeon_ids", closedDungeonIDs, "source", source,
		"policy", "runtime_pvf_weekday_schedule")
	return nil
}
