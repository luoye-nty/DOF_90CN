package dnfbridge

import (
	"fmt"
	"math"
	"time"

	"longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/packageitem"
)

const (
	// The legacy command enum names both values from the opposite packet
	// direction. Keep current-EXE notification semantics local to this owner.
	currentSpendTimeEventInfoMsgID uint16 = 108
	currentSpendTimeProgressMsgID  uint16 = 1206
	currentSpendTimeEventID        uint32 = 2347
	currentSpendTimeSumEventID     uint32 = 2348
	currentJoustEventID            uint32 = 2365
	currentJonathanEventID         uint32 = 2395
	currentLukeRaidEventID         uint32 = 282
	currentLukeRaidOpenEventID     uint16 = 2417
	currentFiendWarEventID         uint32 = 2449
	// currentAntonRaidEventID completes the family. The client's op1206 reader
	// (file 0xA713E0) dispatches 181 / 282 / 2449 onto the mode-1 / -2 / -3 window
	// elements -- `cmp eax,0xB5` is the Anton arm, and the 0x991 arm jumps into
	// the very same setter call -- so omitting 181 leaves Anton's weekly counters
	// with no writer at all.
	currentAntonRaidEventID    uint32 = 181
	currentGuildHongbaoEventID uint16 = 2378
	currentGuildWarEventID     uint16 = 2400
	currentDecisionEventID     uint16 = 222
	currentMileageShopEventID  uint16 = 266

	currentSpendTimeDescriptorParamCount = 12
	currentSpendTimeMaxRewardItems       = 10
	currentSpendTimeProgressRawSize      = 0x20
	currentRaidScheduleInitialUsedCount  = uint32(0)
	currentRaidScheduleInitialDayMask    = uint32(0)
	currentSpendTimeUnusedParam          = math.MaxUint32
	currentSpendTimeBaseCatalogValue     = 1
	currentSpendTimeEventInfoCodec       = "current_op108_spend_time_and_all_day_joust_catalog_zlib"
	currentSpendTimeEventRoute           = "spend time event/0/0"
)

// buildCurrentSpendTimeEventInfoBody matches the current NoPack key-9 op108
// body consumed by sub_20457F0:
//
//	u16 base_catalog_count,
//	base_catalog_count * { u16 event_id, u32 value, dstr title,
//	                          dstr description, u8 has_extended, ... },
//	u8 active_count, active_count * { u16 event_id, 12 * u32 param }
//
// sub_2043290 reads a retired list only when the already-installed base table
// contains special key 9. The seven native activity-calendar rows are part of
// this same final base catalog because the current client replaces the catalog
// whenever it receives op108. They remain base-only records, though: the
// verified calendar envelope terminates with reward_count=0, and calendar IDs
// are not consumed by the twelve-parameter active-state handlers. Synthetic
// calendar active rows make the native date grid open with no entries. Only
// the six live event families below belong in op108's active-state map.
//
// Event 2037 is the native Seria-luck HUD gate. sub_1FC6210 only makes the
// BOOSTER_GAGE control visible after it finds that event in the active table;
// a later class-0/op412 update alone is therefore intentionally invisible.
// Its first active parameter carries the durable stage ceiling shared with the
// package-item state machine. The control itself retains ownership of the
// display-unit maximum and animation.
//
// The base row is required even though event 2347 owns a fixed client UI.
// sub_2045130 inserts it into the global event table; without that row,
// sub_2045E30 never places 2347 in the active-event map and sub_F3EDF0 hides
// the HUD entry. Event 2347 also requires its exact extended action route.
// sub_2036D50 maps "spend time event" to action 0x6AD, and sub_2042550 marks
// the row clickable and stores its two route arguments. Without that route the
// compact HUD entry is visible, but clicking it cannot open the fixed
// four-stage ItemReward.xui panel.
//
// Event 2347 owns params 0..N-1 as reward item IDs, param 10 as N, and param
// 11 as the total duration of all four stages. Current sub_F3FDA0 treats -1,
// not zero, as the unused reward-slot sentinel.
//
// Event 2395 is the PVF [open visible event] gate on NPC 476 (Red-tail
// Jonathan) in Central Park. The NPC's ambient voice is map-owned and can play
// while the actor remains hidden; installing this schedule-free base/active
// row makes the native town loader instantiate the NPC after the Magic Rift
// access quest has made that town content reachable.
//
// Events 282 and 2449 are the current EXE's Luke and Fiend War ESC/team-raid
// entrance gates. The client can preconnect their authoritative type-32/type-42
// channels while still hiding the cards when these events are absent from the
// installed activity table. Both use the same schedule-free base/active grammar
// as event 2365; op1206 below supplies their separate schedule state. The
// following op1303 projection supplies each current PVF account daily limit;
// this activity-table owner remains separate from raid creation and settlement.
func buildCurrentSpendTimeEventInfoBody(
	rewardItemIDs []uint32,
	totalStageSeconds uint32,
) ([]byte, error) {
	return buildCurrentSpendTimeEventInfoBodyWithSum(rewardItemIDs, nil, totalStageSeconds)
}

func buildCurrentSpendTimeEventInfoBodyWithSum(
	rewardItemIDs []uint32,
	sumRewardItemIDs []uint32,
	totalStageSeconds uint32,
) ([]byte, error) {
	return buildCurrentOnlineEventInfoBody(rewardItemIDs, sumRewardItemIDs, totalStageSeconds, false)
}

func buildCurrentOnlineEventInfoBody(rewardItemIDs, sumRewardItemIDs []uint32, totalStageSeconds uint32, nativePCRoom bool) ([]byte, error) {
	// 这两个薄封装代表"活动表全量"参考布局（222 行在），仅测试与参考对照使用；
	// 生产路径一律走 buildCurrentOnlineEventInfoBodyAt 并传入 222 的真实窗口。
	return buildCurrentOnlineEventInfoBodyAt(rewardItemIDs, sumRewardItemIDs, totalStageSeconds, nativePCRoom, true, nil, time.Time{})
}

// levelAimingActive 决定 events 222（全职Lv预约 / 等级预约）是否出现在 op108 的
// base catalog 里。客户端一旦在事件表里看到 222 就会画出对应信息按钮
// （ui/event/informationbuttoninfo.evn 的 id 96 / 1072），窗口过期只会把它换成
// "不可参与"那套图（无"！"），按钮本身不会消失。所以过期活动必须整行省略。
func buildCurrentOnlineEventInfoBodyAt(rewardItemIDs, sumRewardItemIDs []uint32, totalStageSeconds uint32, nativePCRoom bool, levelAimingActive bool, labor *onlineevent.LaborDayCatalog, now time.Time) ([]byte, error) {
	laborActive := labor != nil && labor.Policy.ActiveAt(now)
	laborBase := laborActive || (labor == nil && len(sumRewardItemIDs) > 0)
	if len(rewardItemIDs) == 0 || len(rewardItemIDs) > currentSpendTimeMaxRewardItems {
		return nil, fmt.Errorf("current spend-time reward count=%d is outside 1..%d", len(rewardItemIDs), currentSpendTimeMaxRewardItems)
	}
	if totalStageSeconds == 0 {
		return nil, fmt.Errorf("current spend-time total stage seconds is zero")
	}
	params := [currentSpendTimeDescriptorParamCount]uint32{}
	for index := 0; index < currentSpendTimeMaxRewardItems; index++ {
		params[index] = currentSpendTimeUnusedParam
	}
	for index, itemID := range rewardItemIDs {
		if itemID == 0 {
			return nil, fmt.Errorf("current spend-time reward[%d] item id is zero", index)
		}
		params[index] = itemID
	}
	params[10] = uint32(len(rewardItemIDs))
	params[11] = totalStageSeconds

	var writer packetWriter
	if suppressAllEventRows {
		// 排查开关：只保留锚点行 event 9，活跃行为 0。
		writer.writeUint16(1)
		writer.writeUint16(9)
		writer.writeUint32(0)
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeByte(0)
		writer.writeByte(0)
		return writer.bytes(), nil
	}
	allCalendarRecords := currentActivityCalendarRecords()
	calendarRecords := make([]activityCalendarRecord, 0, len(allCalendarRecords))
	for _, record := range allCalendarRecords {
		if eventSuppressed(record.eventID) {
			continue
		}
		calendarRecords = append(calendarRecords, record)
	}
	// Event 9 is the native activity-table anchor. Event 181 is the Anton team
	// tab; the client gates that tab with the completed q12829 bit from op356.
	// Both rows must be present in the final post-op356 catalog before op1206
	// and op1303 are replayed.
	baseRows := len(calendarRecords) + 15
	if laborBase {
		baseRows++
	}
	if !levelAimingActive {
		// 活动 222 的 base 行被省略，计数必须跟着减一，否则客户端把后面
		// 的行读成活动 ID、剩余字节读成截断行。
		baseRows--
	}
	// 运营屏蔽列表（DNF90_SUPPRESS_EVENT_IDS）：这里把"会被整行跳过"的行
	// 提前从计数里扣掉，保证 baseRows 与实际写入行数严格一致。
	for _, eventID := range []uint16{
		uint16(currentJoustEventID), uint16(currentJonathanEventID),
		uint16(currentLukeRaidEventID), uint16(currentFiendWarEventID),
		uint16(currentSeriaLuckEventID),
		currentLukeRaidOpenEventID, currentMonthlyAttendanceEventID,
		currentWeeklyAttendanceEventID,
		currentGuildWarEventID, currentMileageShopEventID,
	} {
		if eventSuppressed(eventID) {
			baseRows--
		}
	}
	writer.writeUint16(uint16(baseRows)) // event 9 + calendar + event rows
	writer.writeUint16(9)
	writer.writeUint32(0)
	writer.writeRawDstr(nil)
	writer.writeRawDstr(nil)
	writer.writeByte(0)
	for _, record := range calendarRecords {
		if err := writeCurrentActivityCalendarRecord(&writer, record); err != nil {
			return nil, err
		}
	}
	// The guild window checks the native activity directory for 2378 before
	// enabling its hongbao panel. Keep the gate in the base catalog; it has no
	// active-state payload or synthetic reward data.
	writer.writeUint16(currentGuildHongbaoEventID)
	writer.writeUint32(currentSpendTimeBaseCatalogValue)
	writer.writeRawDstr(nil)
	writer.writeRawDstr(nil)
	writer.writeByte(0)
	if nativePCRoom {
		// 0x2FDEADA and 0x1C08FC0 query active event 228. The model reads
		// rewards and its complete gold-frame UI directly from PCRoomTimePoint.etc.
		writer.writeUint16(currentPCRoomTimePointEventID)
		writer.writeUint32(currentSpendTimeBaseCatalogValue)
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeByte(0)
	} else {
		writer.writeUint16(uint16(currentSpendTimeEventID))
		writer.writeUint32(currentSpendTimeBaseCatalogValue)
		writer.writeRawDstr(nil) // generic title; the fixed event UI owns visible text
		writer.writeRawDstr(nil) // generic description
		writer.writeByte(1)      // has_extended: installs the native click action below
		writer.writeByte(0)      // generic category flags
		writer.writeByte(0)      // generic presentation flags
		writer.writeRawDstr(nil) // optional title override
		writer.writeRawDstr(nil) // optional subtitle override
		writer.writeRawDstr(nil) // optional description override
		writer.writeUint32(0)    // optional start timestamp
		writer.writeUint32(0)    // optional end timestamp
		writer.writeAsciiDstr(currentSpendTimeEventRoute)
		writer.writeRawDstr(nil) // optional status text
		writer.writeByte(0)      // optional presentation tail
	}
	if laborBase {
		writer.writeUint16(uint16(currentSpendTimeSumEventID))
		writer.writeUint32(currentSpendTimeBaseCatalogValue)
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeByte(0)
	}
	for _, eventID := range []uint16{
		uint16(currentJoustEventID), uint16(currentJonathanEventID),
		uint16(currentLukeRaidEventID), uint16(currentFiendWarEventID),
		currentSeriaLuckEventID,
	} {
		if eventSuppressed(eventID) {
			continue
		}
		writer.writeUint16(eventID)
		writer.writeUint32(currentSpendTimeBaseCatalogValue)
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeByte(0)
	}
	// sub_17B1B70 accepts active event 2417 for the native open Luke entrance.
	// Keep real quest/item records and the raid entry-cost transaction intact.
	calendarEventIDs := []uint16{currentLukeRaidOpenEventID, currentMonthlyAttendanceEventID}
	if levelAimingActive {
		calendarEventIDs = append(calendarEventIDs, currentDecisionEventID)
	}
	calendarEventIDs = append(calendarEventIDs, currentWeeklyAttendanceEventID)
	for _, eventID := range calendarEventIDs {
		if eventSuppressed(eventID) {
			continue
		}
		// EventListWindow.evt owns the native rows and popup routes.
		record := activityCalendarRecord{eventID: eventID, field0: currentSpendTimeBaseCatalogValue}
		if err := writeCurrentActivityCalendarRecord(&writer, record); err != nil {
			return nil, err
		}
	}
	// E8E2A0 requires event 2400 before consulting PVF [agitwar day] and
	// [war limit time]. Its absence forced the waiting page on every day.
	if !eventSuppressed(currentGuildWarEventID) {
		writer.writeUint16(currentGuildWarEventID)
		writer.writeUint32(currentSpendTimeBaseCatalogValue)
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeByte(0)
	}
	// Current EXE 0x2045237 recognizes base event 266; 0x204574C passes
	// its presence to 0x16AB7B0, which retains the mileage category (4).
	// No active row is needed for visibility: event 266's active parameters
	// belong to a separate controller and are not a wallet balance.
	if !eventSuppressed(currentMileageShopEventID) {
		if err := writeCurrentActivityCalendarRecord(&writer, activityCalendarRecord{
			eventID: currentMileageShopEventID, field0: currentSpendTimeBaseCatalogValue,
		}); err != nil {
			return nil, err
		}
	}
	// The ruins gate is a native NPC with an [open visible event] condition.
	// Its full frame/glow assembly comes from PVF ANI/ALS and the existing NPK.
	// Only one weekday belongs in the base directory; no active parameters or
	// second op108 send are needed for this visibility predicate.
	if err := writeCurrentActivityCalendarRecord(&writer, activityCalendarRecord{
		eventID: currentMysteriousRuinsPortalEventID(now), field0: currentSpendTimeBaseCatalogValue,
	}); err != nil {
		return nil, err
	}
	activeRows := 10
	if laborActive {
		activeRows++
	}
	if suppressActiveEventRows {
		// 排查开关：活跃行为 0，基础行照旧 —— 用来判断图标是否由"活跃"位驱动。
		activeRows = 0
	}
	writer.writeByte(byte(activeRows))
	if !suppressActiveEventRows {
		if nativePCRoom {
			writer.writeUint16(currentPCRoomTimePointEventID)
			for range params {
				writer.writeUint32(0)
			}
		} else {
			writer.writeUint16(uint16(currentSpendTimeEventID))
			for _, value := range params {
				writer.writeUint32(value)
			}
		}
		// IDA: 0x2043BCB routes 2348 to owner 0x41A / 0xF3F220.
		// 0xF40D10 binds exactly three item controls. Sending a fourth item would
		// overrun that native control array. param[10] is the item count.
		if laborActive {
			writer.writeUint16(uint16(currentSpendTimeSumEventID))
			for i := 0; i < 12; i++ {
				value := uint32(currentSpendTimeUnusedParam)
				if i < 3 {
					value = uint32(labor.Login[i].ItemID)
				}
				if i == 10 {
					value = 3
				}
				if i == 11 {
					value = 0
				}
				writer.writeUint32(value)
			}
		}

		writer.writeUint16(uint16(currentJoustEventID))
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(uint16(currentJonathanEventID))
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(uint16(currentLukeRaidEventID))
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(uint16(currentFiendWarEventID))
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(currentSeriaLuckEventID)
		writer.writeUint32(uint32(packageitem.SeriaLuckValueMax))
		for index := 1; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(currentMonthlyAttendanceEventID)
		// sub_2043290 -> sub_1829EE0 stores param[0] as required clear count.
		writer.writeUint32(onlineevent.MonthlyRequiredClears)
		for index := 1; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(currentLukeRaidOpenEventID)
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(currentGuildWarEventID)
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
		writer.writeUint16(currentWeeklyAttendanceEventID)
		for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
			writer.writeUint32(0)
		}
	}
	return writer.bytes(), nil
}

// currentRaidScheduleCounters is the live state one raid-schedule record carries.
//
// The three u32s are NOT free-form. The client hands raw[0]/raw[1]/raw[2] to
// sub_C39140 (file 0xC39140) in that order, and that setter -- the ONLY writer of
// the per-mode window element's counters in the whole EXE -- stores them as:
//
//	raw[0] -> element+0x30   account WEEKLY used    (rendered as 账号周入场次数)
//	raw[1] -> element+0x08   character WEEKLY used  (rendered as 角色周入场次数)
//	raw[2] -> element+0x04   seven-day mask, and the 7 cells at +0xc..+0x24
//
// Both weekly readouts print (limit - used), which is why byte 3 of the op1303
// row can only ever move the limit and the probe of 2026-09-30 saw 43/43 and
// 3/3: those were used == 0, not "the weekly counter is limit-only".
type currentRaidScheduleCounters struct {
	AccountWeeklyUsed   uint32
	CharacterWeeklyUsed uint32
	DayMask             uint32
}

// buildCurrentRaidScheduleProgressBody matches the current narrow op1206
// RaidScheduleEvent records for the Anton, Luke and Fiend War entrance events.
// raw[0]/raw[1] are the account and character weekly USED counts and raw[2] is
// the seven-day mask; a player who has not entered this week is therefore all
// zero, but a player who has must see their own number or the entry window's
// 角色周入场次数 shows limit/limit while the start command refuses on the same
// counter (raid_admission_counter.go charges exactly these two policies).
func buildCurrentRaidScheduleProgressBody(
	eventID uint32,
	counters currentRaidScheduleCounters,
) ([]byte, error) {
	switch eventID {
	case currentAntonRaidEventID, currentLukeRaidEventID, currentFiendWarEventID:
	default:
		return nil, fmt.Errorf("current raid schedule event id=%d is unsupported", eventID)
	}
	var writer packetWriter
	writer.writeUint32(eventID)
	writer.writeByte(0) // narrow 0x20-byte record
	writer.writeUint32(counters.AccountWeeklyUsed)
	writer.writeUint32(counters.CharacterWeeklyUsed)
	writer.writeUint32(counters.DayMask)
	writer.writeBytes(make([]byte, currentSpendTimeProgressRawSize-12))
	return writer.bytes(), nil
}

// buildCurrentSpendTimeProgressBody matches current NoPack op1206 dispatch:
//
//	u32 event_id, u8 wide_flag, raw[wide_flag ? 0xA0 : 0x20]
//
// Event 2347 uses the narrow record. Its first two u32 values are authoritative
// elapsed seconds and the number of automatically completed reward stages.
func buildCurrentSpendTimeProgressBody(elapsedSeconds uint64, completedStages uint32) ([]byte, error) {
	if elapsedSeconds > math.MaxUint32 {
		return nil, fmt.Errorf("current spend-time elapsed seconds=%d overflows u32", elapsedSeconds)
	}
	var writer packetWriter
	writer.writeUint32(currentSpendTimeEventID)
	writer.writeByte(0) // narrow 0x20-byte record
	writer.writeUint32(uint32(elapsedSeconds))
	writer.writeUint32(completedStages)
	writer.writeBytes(make([]byte, currentSpendTimeProgressRawSize-8))
	return writer.bytes(), nil
}
