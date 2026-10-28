package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/onlineevent"
)

func TestCurrentMysteriousRuinsScheduleMatchesPVFWeekdays(t *testing.T) {
	// 2026-10-26 is Monday. These rows are copied from the five current
	// chn_daydungeon_0N.dgn [schedule] sections.
	monday := time.Date(2026, 10, 26, 12, 0, 0, 0, onlineevent.ChinaCalendar)
	schedules := []struct {
		dungeonID uint32
		ids       []int64
	}{
		{4906, []int64{2418, 2423}},
		{4907, []int64{2421, 2424}},
		{4908, []int64{2420, 2423}},
		{4909, []int64{2422, 2424}},
		{4910, []int64{2419, 2423}},
	}
	want := [][]uint32{
		{4906},
		{4910},
		{4908},
		{4907},
		{4909},
		{4906, 4908, 4910},
		{4907, 4909},
	}
	for day, wantIDs := range want {
		now := monday.AddDate(0, 0, day)
		var got []uint32
		for _, schedule := range schedules {
			if currentMysteriousRuinsScheduleOpen(now, schedule.ids) {
				got = append(got, schedule.dungeonID)
			}
		}
		if len(got) != len(wantIDs) {
			t.Fatalf("%s open dungeons=%v want=%v", now.Weekday(), got, wantIDs)
		}
		for index := range got {
			if got[index] != wantIDs[index] {
				t.Fatalf("%s open dungeons=%v want=%v", now.Weekday(), got, wantIDs)
			}
		}
	}
}
