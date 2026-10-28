package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	dnfjoust "longheng.io/server/internal/modules/dnf/joust"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/packageitem"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentSpendTimeNextServiceBoundaryUsesShanghaiSixAM(t *testing.T) {
	location := dnfonlineevent.ChinaCalendar
	definition := dnfonlineevent.Definition{Calendar: location}
	before := time.Date(2026, 8, 4, 5, 59, 59, 0, location)
	if got, want := currentSpendTimeNextServiceBoundary(definition, before),
		time.Date(2026, 8, 4, 6, 0, 0, 0, location); !got.Equal(want) {
		t.Fatalf("before-boundary next=%s want=%s", got, want)
	}
	atBoundary := time.Date(2026, 8, 4, 6, 0, 0, 0, location)
	if got, want := currentSpendTimeNextServiceBoundary(definition, atBoundary),
		time.Date(2026, 8, 5, 6, 0, 0, 0, location); !got.Equal(want) {
		t.Fatalf("at-boundary next=%s want=%s", got, want)
	}
}

func TestSettleCurrentSpendTimeClockSplitsSixAMAndClaimsBothServiceDays(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7",
		AccountID:   "account-1",
		Stats:       map[string]int64{"gold": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "7",
		Slots:       make(map[string]dnfrepo.ItemStack),
	}); err != nil {
		t.Fatal(err)
	}

	const rewardID uint32 = 490006574
	itemCatalog := &pvfDungeonDropCatalog{
		source: bridgePVFSource{},
		itemCache: map[uint32]dungeonDropItemDefinition{rewardID: {
			ItemID: rewardID, Kind: dungeonDropItemStackable, StackLimit: 1000,
			SlotStart: 9, SlotEnd: 56,
		}},
	}
	definition := dnfonlineevent.Definition{
		ID:       "2347-test",
		Calendar: dnfonlineevent.ChinaCalendar,
		Stages: []dnfonlineevent.Stage{{
			ID: "stage-1", RequiredSeconds: 10,
			Items: []dnfonlineevent.ItemReward{{ItemID: int64(rewardID), Count: 1}},
		}},
	}
	service := &Service{
		pvfItemCatalog: itemCatalog,
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repositories, true
		},
	}
	boundary := time.Date(2026, 8, 4, 6, 0, 0, 0, dnfonlineevent.ChinaCalendar)
	session := &gameSession{}
	session.spendTime.characterID = 7
	session.spendTime.accountID = "account-1"
	session.spendTime.anchor = boundary.Add(-20 * time.Second)
	session.spendTime.catalog = &currentSpendTimeRuntimeCatalog{
		definition: definition,
	}

	settlement, err := service.settleCurrentSpendTimeClockLocked(session, boundary.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if settlement.claimErr != nil || !settlement.changed || len(settlement.rewardItems) != 2 {
		t.Fatalf("settlement=%+v", settlement)
	}
	if !session.spendTime.anchor.Equal(boundary.Add(10*time.Second)) ||
		session.spendTime.onlineSeconds != 10 || session.spendTime.completedStages != 1 {
		t.Fatalf("clock anchor=%s online=%d completed=%d",
			session.spendTime.anchor,
			session.spendTime.onlineSeconds,
			session.spendTime.completedStages)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "7")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if stack := inventory.Slots["0:9"]; stack.ItemID != int64(rewardID) || stack.Count != 2 {
		t.Fatalf("reward stack=%+v inventory=%+v", stack, inventory.Slots)
	}
}

func TestBuildCurrentSpendTimeEventInfoBodyMatchesCurrentNoPackFirstProcessDescriptor(t *testing.T) {
	rewards := []uint32{490006574, 490006575, 490006576, 490006577}
	body, err := buildCurrentSpendTimeEventInfoBody(rewards, 10800)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 960 {
		t.Fatalf("op108 body len=%d body=%x", len(body), body)
	}
	if binary.LittleEndian.Uint16(body[:2]) != 22 {
		t.Fatalf("op108 base count=%d", binary.LittleEndian.Uint16(body[:2]))
	}
	calendarRecords := currentActivityCalendarRecords()
	offset := 2
	if got := binary.LittleEndian.Uint16(body[offset : offset+2]); got != 9 ||
		binary.LittleEndian.Uint32(body[offset+2:offset+6]) != 0 ||
		binary.LittleEndian.Uint32(body[offset+6:offset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[offset+10:offset+14]) != 0 || body[offset+14] != 0 {
		t.Fatalf("op108 activity-table anchor=%x", body[offset:offset+15])
	}
	offset += 15
	for index, record := range calendarRecords {
		if got := binary.LittleEndian.Uint16(body[offset : offset+2]); got != record.eventID {
			t.Fatalf("op108 calendar row[%d]=%d want=%d", index, got, record.eventID)
		}
		var row packetWriter
		if err := writeCurrentActivityCalendarRecord(&row, record); err != nil {
			t.Fatal(err)
		}
		offset += len(row.bytes())
	}
	guildHongbaoOffset := offset
	if binary.LittleEndian.Uint16(body[guildHongbaoOffset:guildHongbaoOffset+2]) != currentGuildHongbaoEventID ||
		binary.LittleEndian.Uint32(body[guildHongbaoOffset+2:guildHongbaoOffset+6]) != currentSpendTimeBaseCatalogValue ||
		body[guildHongbaoOffset+14] != 0 {
		t.Fatalf("op108 guild hongbao gate=%x", body[guildHongbaoOffset:guildHongbaoOffset+15])
	}
	eventBaseOffset := guildHongbaoOffset + 15
	extendedOffset := eventBaseOffset + 15
	joustBaseOffset := eventBaseOffset + 66
	jonathanBaseOffset := joustBaseOffset + 15
	lukeBaseOffset := jonathanBaseOffset + 15
	fiendWarBaseOffset := lukeBaseOffset + 15
	seriaLuckBaseOffset := fiendWarBaseOffset + 15
	lukeOpenBaseOffset := seriaLuckBaseOffset + 15
	if binary.LittleEndian.Uint16(body[lukeOpenBaseOffset:]) != 2417 || body[lukeOpenBaseOffset+14] != 0 {
		t.Fatal("native Luke open entrance base row missing")
	}
	monthlyBaseOffset := lukeOpenBaseOffset + 15
	levelBaseOffset := monthlyBaseOffset + 15
	if binary.LittleEndian.Uint16(body[levelBaseOffset:]) != 222 || body[levelBaseOffset+14] != 0 {
		t.Fatalf("native HUD 96 LV UP activity gate is invalid")
	}
	weeklyBaseOffset := levelBaseOffset + 15
	if binary.LittleEndian.Uint16(body[weeklyBaseOffset:]) != 2208 || body[weeklyBaseOffset+14] != 0 {
		t.Fatal("weekly attendance base row missing")
	}
	warBaseOffset := weeklyBaseOffset + 15
	if binary.LittleEndian.Uint16(body[warBaseOffset:]) != 2400 || body[warBaseOffset+14] != 0 {
		t.Fatal("guild war event gate missing")
	}
	mileageBaseOffset := warBaseOffset + 15
	if binary.LittleEndian.Uint16(body[mileageBaseOffset:]) != 266 ||
		binary.LittleEndian.Uint32(body[mileageBaseOffset+2:]) != 1 ||
		!bytes.Equal(body[mileageBaseOffset+6:mileageBaseOffset+15], make([]byte, 9)) {
		t.Fatal("native mileage shop category 4 gate missing or has a synthetic payload")
	}
	portalBaseOffset := mileageBaseOffset + 15
	if binary.LittleEndian.Uint16(body[portalBaseOffset:]) != 2418 ||
		binary.LittleEndian.Uint32(body[portalBaseOffset+2:]) != 1 ||
		!bytes.Equal(body[portalBaseOffset+6:portalBaseOffset+15], make([]byte, 9)) {
		t.Fatal("native ruins portal directory row missing")
	}
	activeOffset := portalBaseOffset + 15
	eventActiveOffset := activeOffset + 1
	paramsOffset := eventActiveOffset + 2
	joustActiveOffset := paramsOffset + 48
	joustParamsOffset := joustActiveOffset + 2
	jonathanActiveOffset := joustParamsOffset + 48
	jonathanParamsOffset := jonathanActiveOffset + 2
	lukeActiveOffset := jonathanParamsOffset + 48
	lukeParamsOffset := lukeActiveOffset + 2
	fiendWarActiveOffset := lukeParamsOffset + 48
	fiendWarParamsOffset := fiendWarActiveOffset + 2
	seriaLuckActiveOffset := fiendWarParamsOffset + 48
	seriaLuckParamsOffset := seriaLuckActiveOffset + 2
	if binary.LittleEndian.Uint16(body[eventBaseOffset:eventBaseOffset+2]) != uint16(currentSpendTimeEventID) ||
		binary.LittleEndian.Uint32(body[eventBaseOffset+2:eventBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[eventBaseOffset+6:eventBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[eventBaseOffset+10:eventBaseOffset+14]) != 0 || body[eventBaseOffset+14] != 1 {
		t.Fatalf("op108 event base prefix=%x", body[eventBaseOffset:eventBaseOffset+15])
	}
	if body[extendedOffset] != 0 || body[extendedOffset+1] != 0 ||
		binary.LittleEndian.Uint32(body[extendedOffset+2:extendedOffset+6]) != 0 ||
		binary.LittleEndian.Uint32(body[extendedOffset+6:extendedOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[extendedOffset+10:extendedOffset+14]) != 0 ||
		binary.LittleEndian.Uint32(body[extendedOffset+14:extendedOffset+18]) != 0 ||
		binary.LittleEndian.Uint32(body[extendedOffset+18:extendedOffset+22]) != 0 {
		t.Fatalf("op108 extended prefix=%x", body[extendedOffset:joustBaseOffset])
	}
	routeLength := int(binary.LittleEndian.Uint32(body[extendedOffset+22 : extendedOffset+26]))
	if routeLength != len(currentSpendTimeEventRoute) ||
		string(body[extendedOffset+26:extendedOffset+26+routeLength]) != currentSpendTimeEventRoute ||
		binary.LittleEndian.Uint32(body[extendedOffset+26+routeLength:extendedOffset+30+routeLength]) != 0 ||
		body[extendedOffset+30+routeLength] != 0 {
		t.Fatalf("op108 click route extension=%x", body[extendedOffset:joustBaseOffset])
	}
	if binary.LittleEndian.Uint16(body[joustBaseOffset:joustBaseOffset+2]) != uint16(currentJoustEventID) ||
		binary.LittleEndian.Uint32(body[joustBaseOffset+2:joustBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[joustBaseOffset+6:joustBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[joustBaseOffset+10:joustBaseOffset+14]) != 0 ||
		body[joustBaseOffset+14] != 0 {
		t.Fatalf("op108 joust base row=%x", body[joustBaseOffset:jonathanBaseOffset])
	}
	if binary.LittleEndian.Uint16(body[jonathanBaseOffset:jonathanBaseOffset+2]) != uint16(currentJonathanEventID) ||
		binary.LittleEndian.Uint32(body[jonathanBaseOffset+2:jonathanBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[jonathanBaseOffset+6:jonathanBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[jonathanBaseOffset+10:jonathanBaseOffset+14]) != 0 ||
		body[jonathanBaseOffset+14] != 0 {
		t.Fatalf("op108 Jonathan base row=%x", body[jonathanBaseOffset:lukeBaseOffset])
	}
	if binary.LittleEndian.Uint16(body[lukeBaseOffset:lukeBaseOffset+2]) != uint16(currentLukeRaidEventID) ||
		binary.LittleEndian.Uint32(body[lukeBaseOffset+2:lukeBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[lukeBaseOffset+6:lukeBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[lukeBaseOffset+10:lukeBaseOffset+14]) != 0 ||
		body[lukeBaseOffset+14] != 0 {
		t.Fatalf("op108 Luke raid base row=%x", body[lukeBaseOffset:fiendWarBaseOffset])
	}
	if binary.LittleEndian.Uint16(body[fiendWarBaseOffset:fiendWarBaseOffset+2]) != uint16(currentFiendWarEventID) ||
		binary.LittleEndian.Uint32(body[fiendWarBaseOffset+2:fiendWarBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[fiendWarBaseOffset+6:fiendWarBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[fiendWarBaseOffset+10:fiendWarBaseOffset+14]) != 0 ||
		body[fiendWarBaseOffset+14] != 0 {
		t.Fatalf("op108 Fiend War base row=%x", body[fiendWarBaseOffset:seriaLuckBaseOffset])
	}
	if binary.LittleEndian.Uint16(body[seriaLuckBaseOffset:seriaLuckBaseOffset+2]) != currentSeriaLuckEventID ||
		binary.LittleEndian.Uint32(body[seriaLuckBaseOffset+2:seriaLuckBaseOffset+6]) != currentSpendTimeBaseCatalogValue ||
		binary.LittleEndian.Uint32(body[seriaLuckBaseOffset+6:seriaLuckBaseOffset+10]) != 0 ||
		binary.LittleEndian.Uint32(body[seriaLuckBaseOffset+10:seriaLuckBaseOffset+14]) != 0 ||
		body[seriaLuckBaseOffset+14] != 0 {
		t.Fatalf("op108 seria-luck base row=%x", body[seriaLuckBaseOffset:activeOffset])
	}
	if body[activeOffset] != 10 {
		t.Fatalf("op108 active prefix=%x", body[activeOffset:paramsOffset])
	}
	for _, record := range calendarRecords {
		for rowOffset := eventActiveOffset; rowOffset < len(body); rowOffset += 50 {
			if binary.LittleEndian.Uint16(body[rowOffset:rowOffset+2]) == record.eventID {
				t.Fatalf("op108 calendar event %d leaked into active-state map", record.eventID)
			}
		}
	}
	if binary.LittleEndian.Uint16(body[eventActiveOffset:paramsOffset]) != uint16(currentSpendTimeEventID) {
		t.Fatalf("op108 event active prefix=%x", body[eventActiveOffset:paramsOffset])
	}
	monthlyActiveOffset := seriaLuckParamsOffset + 48
	weeklyActiveOffset := eventActiveOffset + 9*50
	if binary.LittleEndian.Uint16(body[weeklyActiveOffset:]) != 2208 || !bytes.Equal(body[weeklyActiveOffset+2:], make([]byte, 48)) {
		t.Fatal("weekly attendance active row missing")
	}
	if binary.LittleEndian.Uint16(body[monthlyBaseOffset:]) != currentMonthlyAttendanceEventID ||
		binary.LittleEndian.Uint16(body[monthlyActiveOffset:]) != currentMonthlyAttendanceEventID ||
		binary.LittleEndian.Uint32(body[monthlyActiveOffset+2:]) != dnfonlineevent.MonthlyRequiredClears ||
		!bytes.Equal(body[monthlyActiveOffset+6:monthlyActiveOffset+50], make([]byte, 44)) {
		t.Fatalf("monthly activity gate or clear threshold is invalid")
	}
	for index, want := range rewards {
		if got := binary.LittleEndian.Uint32(body[paramsOffset+index*4 : paramsOffset+(index+1)*4]); got != want {
			t.Fatalf("op108 param[%d]=%d want=%d", index, got, want)
		}
	}
	for index := len(rewards); index < 10; index++ {
		if got := binary.LittleEndian.Uint32(body[paramsOffset+index*4 : paramsOffset+(index+1)*4]); got != currentSpendTimeUnusedParam {
			t.Fatalf("op108 param[%d]=%#x want=%#x", index, got, currentSpendTimeUnusedParam)
		}
	}
	if got := binary.LittleEndian.Uint32(body[paramsOffset+40 : paramsOffset+44]); got != uint32(len(rewards)) {
		t.Fatalf("op108 reward count=%d", got)
	}
	if got := binary.LittleEndian.Uint32(body[paramsOffset+44 : paramsOffset+48]); got != 10800 {
		t.Fatalf("op108 total stage seconds=%d", got)
	}
	if binary.LittleEndian.Uint16(body[joustActiveOffset:joustParamsOffset]) != uint16(currentJoustEventID) {
		t.Fatalf("op108 joust active row=%x", body[joustActiveOffset:])
	}
	for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
		if got := binary.LittleEndian.Uint32(body[joustParamsOffset+index*4 : joustParamsOffset+(index+1)*4]); got != 0 {
			t.Fatalf("op108 joust param[%d]=%d want=0", index, got)
		}
	}
	if binary.LittleEndian.Uint16(body[jonathanActiveOffset:jonathanParamsOffset]) != uint16(currentJonathanEventID) {
		t.Fatalf("op108 Jonathan active row=%x", body[jonathanActiveOffset:])
	}
	for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
		if got := binary.LittleEndian.Uint32(body[jonathanParamsOffset+index*4 : jonathanParamsOffset+(index+1)*4]); got != 0 {
			t.Fatalf("op108 Jonathan param[%d]=%d want=0", index, got)
		}
	}
	if binary.LittleEndian.Uint16(body[lukeActiveOffset:lukeParamsOffset]) != uint16(currentLukeRaidEventID) {
		t.Fatalf("op108 Luke raid active row=%x", body[lukeActiveOffset:])
	}
	for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
		if got := binary.LittleEndian.Uint32(body[lukeParamsOffset+index*4 : lukeParamsOffset+(index+1)*4]); got != 0 {
			t.Fatalf("op108 Luke raid param[%d]=%d want=0", index, got)
		}
	}
	if binary.LittleEndian.Uint16(body[fiendWarActiveOffset:fiendWarParamsOffset]) != uint16(currentFiendWarEventID) {
		t.Fatalf("op108 Fiend War active row=%x", body[fiendWarActiveOffset:])
	}
	for index := 0; index < currentSpendTimeDescriptorParamCount; index++ {
		if got := binary.LittleEndian.Uint32(body[fiendWarParamsOffset+index*4 : fiendWarParamsOffset+(index+1)*4]); got != 0 {
			t.Fatalf("op108 Fiend War param[%d]=%d want=0", index, got)
		}
	}
	if binary.LittleEndian.Uint16(body[seriaLuckActiveOffset:seriaLuckParamsOffset]) != currentSeriaLuckEventID {
		t.Fatalf("op108 seria-luck active row=%x", body[seriaLuckActiveOffset:])
	}
	if got := binary.LittleEndian.Uint32(body[seriaLuckParamsOffset : seriaLuckParamsOffset+4]); got != uint32(packageitem.SeriaLuckValueMax) {
		t.Fatalf("op108 seria-luck max stage=%d want=%d", got, packageitem.SeriaLuckValueMax)
	}
	for index := 1; index < currentSpendTimeDescriptorParamCount; index++ {
		if got := binary.LittleEndian.Uint32(body[seriaLuckParamsOffset+index*4 : seriaLuckParamsOffset+(index+1)*4]); got != 0 {
			t.Fatalf("op108 seria-luck param[%d]=%d want=0", index, got)
		}
	}
}

func TestBuildCurrentSpendTimeEventInfoBodyWithSumRetains2348BaseOnly(t *testing.T) {
	body, err := buildCurrentSpendTimeEventInfoBodyWithSum(
		[]uint32{490006574, 490006575, 490006576, 490006577},
		[]uint32{490006578, 490006579, 490006580, 490006581},
		10800,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte{0x2c, 0x09}) {
		t.Fatalf("2348 base event id missing from op108 body")
	}
	for _, itemID := range []uint32{490006578, 490006579, 490006580, 490006581} {
		encoded := make([]byte, 4)
		binary.LittleEndian.PutUint32(encoded, itemID)
		if bytes.Contains(body, encoded) {
			t.Fatalf("2348 cumulative reward item %d must not be sent as an unproved active parameter", itemID)
		}
	}
	if bytes.Contains(body, []byte{11}) {
		t.Fatalf("op108 active count must not include 2348")
	}
}

// 活动 222（全职Lv预约）的入口图标来自 op108 事件表：只要 base catalog 里带了
// 222，客户端就会画出 ui/event/informationbuttoninfo.evn 的 96/1072 那一对按钮。
// 所以"活动过期"必须表现为整行省略，而不是换一个"不可参与"的图标继续挂着。
func TestBuildCurrentOnlineEventInfoBodyFollowsLevelAimingWindow(t *testing.T) {
	rewards := []uint32{490006574, 490006575, 490006576, 490006577}
	open, err := buildCurrentOnlineEventInfoBodyAt(rewards, nil, 10800, false, true, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := buildCurrentOnlineEventInfoBodyAt(rewards, nil, 10800, false, false, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var levelAimingRow packetWriter
	if err := writeCurrentActivityCalendarRecord(&levelAimingRow, activityCalendarRecord{eventID: currentDecisionEventID, field0: currentSpendTimeBaseCatalogValue}); err != nil {
		t.Fatal(err)
	}
	if len(levelAimingRow.bytes()) != 15 {
		t.Fatalf("222 base row len=%d want=15", len(levelAimingRow.bytes()))
	}
	if got, want := len(open), len(closed)+len(levelAimingRow.bytes()); got != want {
		t.Fatalf("closed level-aiming window must drop exactly the 222 row: open=%d closed=%d row=%d", got, want, len(levelAimingRow.bytes()))
	}
	if got := binary.LittleEndian.Uint16(open[:2]); got != 22 {
		t.Fatalf("open window base count=%d want=22", got)
	}
	if got := binary.LittleEndian.Uint16(closed[:2]); got != 21 {
		t.Fatalf("closed window base count=%d want=21", got)
	}
	if !bytes.Contains(open, levelAimingRow.bytes()) {
		t.Fatal("open window must install the 222 row")
	}
	if bytes.Contains(closed, levelAimingRow.bytes()) {
		t.Fatal("closed window must not install the 222 row")
	}
	// 同一批日历里的另外三档与 222 无关，必须照旧下发。
	for _, eventID := range []uint16{currentLukeRaidOpenEventID, currentMonthlyAttendanceEventID, currentWeeklyAttendanceEventID} {
		var marker packetWriter
		if err := writeCurrentActivityCalendarRecord(&marker, activityCalendarRecord{eventID: eventID, field0: currentSpendTimeBaseCatalogValue}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(closed, marker.bytes()) {
			t.Fatalf("closed window dropped unrelated calendar row %d", eventID)
		}
	}
}

func TestLevelAimingWindowActiveUsesPVFControlPanelDates(t *testing.T) {
	catalog := &dnfonlineevent.LevelAimingCatalog{
		Starts: time.Date(2016, 12, 15, 6, 0, 0, 0, time.UTC),
		Ends:   time.Date(2017, 1, 17, 6, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{name: "before window", now: time.Date(2016, 12, 15, 5, 59, 59, 0, time.UTC), want: false},
		{name: "window start", now: time.Date(2016, 12, 15, 6, 0, 0, 0, time.UTC), want: true},
		{name: "inside window", now: time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC), want: true},
		{name: "window end", now: time.Date(2017, 1, 17, 6, 0, 0, 0, time.UTC), want: false},
		{name: "long expired", now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), want: false},
		{name: "zero clock", now: time.Time{}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := levelAimingWindowActive(catalog, test.now); got != test.want {
				t.Fatalf("levelAimingWindowActive(%s)=%t want=%t", test.now, got, test.want)
			}
		})
	}
	if levelAimingWindowActive(nil, time.Date(2016, 12, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("missing catalog must never report the activity window as open")
	}
}

func TestBuildCurrentSpendTimeEventInfoBodyRejectsUnprovedDescriptor(t *testing.T) {
	tests := []struct {
		name    string
		rewards []uint32
		seconds uint32
	}{
		{name: "empty rewards", seconds: 10800},
		{name: "too many rewards", rewards: make([]uint32, 11), seconds: 10800},
		{name: "zero item", rewards: []uint32{490006574, 0}, seconds: 10800},
		{name: "zero duration", rewards: []uint32{490006574}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := buildCurrentSpendTimeEventInfoBody(test.rewards, test.seconds); err == nil {
				t.Fatal("descriptor unexpectedly accepted")
			}
		})
	}
}

func TestBuildCurrentSpendTimeProgressBodyMatchesCurrentNoPackNarrowRecord(t *testing.T) {
	body, err := buildCurrentSpendTimeProgressBody(7199, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 37 {
		t.Fatalf("op1206 body len=%d body=%x", len(body), body)
	}
	if binary.LittleEndian.Uint32(body[:4]) != currentSpendTimeEventID || body[4] != 0 {
		t.Fatalf("op1206 prefix=%x", body[:5])
	}
	if binary.LittleEndian.Uint32(body[5:9]) != 7199 || binary.LittleEndian.Uint32(body[9:13]) != 2 {
		t.Fatalf("op1206 progress=%x", body[5:13])
	}
	for index, value := range body[13:] {
		if value != 0 {
			t.Fatalf("op1206 raw padding[%d]=%d", index+8, value)
		}
	}
	if _, err := buildCurrentSpendTimeProgressBody(uint64(math.MaxUint32)+1, 0); err == nil {
		t.Fatal("op1206 accepted elapsed overflow")
	}
}

func TestBuildCurrentRaidScheduleProgressBodyMatchesCurrentNoPackNarrowRecord(t *testing.T) {
	zero := currentRaidScheduleCounters{}
	// 参考实现只覆盖 282/2449 两档；本树刻意把 181（安东）也算进来：181 的记录
	// 里 raw[0]/raw[1] 是账号/角色本周已用次数，缺了它安东周计数就没有写入方。
	for _, eventID := range []uint32{currentAntonRaidEventID, currentLukeRaidEventID, currentFiendWarEventID} {
		body, err := buildCurrentRaidScheduleProgressBody(eventID, zero)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != 37 || binary.LittleEndian.Uint32(body[:4]) != eventID || body[4] != 0 ||
			binary.LittleEndian.Uint32(body[5:9]) != 0 ||
			binary.LittleEndian.Uint32(body[9:13]) != currentRaidScheduleInitialUsedCount ||
			binary.LittleEndian.Uint32(body[13:17]) != currentRaidScheduleInitialDayMask {
			t.Fatalf("raid op1206 event=%d body=%x", eventID, body)
		}
		for index, value := range body[17:] {
			if value != 0 {
				t.Fatalf("raid op1206 event=%d padding[%d]=%d", eventID, index+12, value)
			}
		}
	}
	// 真实计数必须原样落到 raw[0]/raw[1]/raw[2]：客户端按
	// (上限 - 已用) 渲染两个周次读数。
	live, err := buildCurrentRaidScheduleProgressBody(currentAntonRaidEventID, currentRaidScheduleCounters{
		AccountWeeklyUsed: 3, CharacterWeeklyUsed: 2, DayMask: 0x7f,
	})
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(live[5:9]) != 3 ||
		binary.LittleEndian.Uint32(live[9:13]) != 2 ||
		binary.LittleEndian.Uint32(live[13:17]) != 0x7f {
		t.Fatalf("raid op1206 live counters=%x", live[5:17])
	}
	if _, err := buildCurrentRaidScheduleProgressBody(999, zero); err == nil {
		t.Fatal("raid op1206 accepted unknown event")
	}
}

func TestSendCurrentSpendTimeProtocolStateKeepsProtectedDescriptorAndProgressAdjacent(t *testing.T) {
	connection := &bufferConn{}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = time.Unix(7200*321, 0).UTC().Add(10 * time.Minute)
	service := &Service{joustCatalog: mustTestJoustEventCatalog(t), gameplayTimers: queue}
	session := &gameSession{conn: connection, connID: "spend-time-protocol"}
	catalog := &currentSpendTimeRuntimeCatalog{
		rewardItemIDs:     []uint32{490006574, 490006575, 490006576, 490006577},
		totalStageSeconds: 10800,
	}
	descriptorSent, err := service.sendCurrentSpendTimeProtocolState(session, catalog, 7199, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if !descriptorSent {
		t.Fatal("first descriptor was not reported as sent")
	}

	wire := connection.write.Bytes()
	descriptor, rest := splitGameServerUpperPacketWithHeader(t, wire, dnfproto.GameServerUpperHeaderSize16)
	if descriptor.Header.Classification != 0 || descriptor.Header.MsgID != currentSpendTimeEventInfoMsgID ||
		descriptor.Header.Checksum != 1 || wire[15] != 1 {
		t.Fatalf("op108 protected header=%+v raw=%x", descriptor.Header, wire[:dnfproto.GameServerUpperHeaderSize16])
	}
	plainDescriptor, err := zlibDecompress(descriptor.Body)
	if err != nil {
		t.Fatalf("decompress op108: %v", err)
	}
	// 该测试的 Service 没有 PVF 来源，所以读不到 222 的活动窗口，生产路径按
	// "窗口已过期"处理，事件表里不带 222 行。
	wantDescriptor, err := buildCurrentOnlineEventInfoBodyAt(catalog.rewardItemIDs, nil, catalog.totalStageSeconds, false, false, nil, queue.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plainDescriptor) != len(wantDescriptor) || !bytes.Equal(plainDescriptor, wantDescriptor) {
		t.Fatalf("op108 plain=%x want=%x", plainDescriptor, wantDescriptor)
	}
	progress, rest := splitGameServerUpperPacket(t, rest)
	if progress.Header.Classification != 0 || progress.Header.MsgID != currentSpendTimeProgressMsgID {
		t.Fatalf("op1206 header=%+v", progress.Header)
	}
	wantProgress, err := buildCurrentSpendTimeProgressBody(7199, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(progress.Body, wantProgress) {
		t.Fatalf("op1206 body=%x want=%x", progress.Body, wantProgress)
	}
	// 三档按 181 / 282 / 2449 顺序连发：安东那一条是本树相对参考实现多出来的，
	// 少了它客户端安东页的"账号/角色周入场次数"没有写入方。
	antonSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantAntonSchedule, err := buildCurrentRaidScheduleProgressBody(currentAntonRaidEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if antonSchedule.Header.Classification != 0 || antonSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(antonSchedule.Body, wantAntonSchedule) {
		t.Fatalf("Anton raid op1206 header=%+v body=%x want=%x", antonSchedule.Header, antonSchedule.Body, wantAntonSchedule)
	}
	lukeSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantLukeSchedule, err := buildCurrentRaidScheduleProgressBody(currentLukeRaidEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if lukeSchedule.Header.Classification != 0 || lukeSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(lukeSchedule.Body, wantLukeSchedule) {
		t.Fatalf("Luke raid op1206 header=%+v body=%x want=%x", lukeSchedule.Header, lukeSchedule.Body, wantLukeSchedule)
	}
	fiendWarSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantFiendWarSchedule, err := buildCurrentRaidScheduleProgressBody(currentFiendWarEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if fiendWarSchedule.Header.Classification != 0 || fiendWarSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(fiendWarSchedule.Body, wantFiendWarSchedule) {
		t.Fatalf("Fiend War op1206 header=%+v body=%x want=%x", fiendWarSchedule.Header, fiendWarSchedule.Body, wantFiendWarSchedule)
	}
	dailyAvailability, rest := splitGameServerUpperPacket(t, rest)
	wantDailyAvailability, err := buildCurrentRaidDailyAvailabilityBody(defaultCurrentRaidDailyAvailabilityRows())
	if err != nil {
		t.Fatal(err)
	}
	if dailyAvailability.Header.Classification != 0 || dailyAvailability.Header.MsgID != currentRaidDailyAvailabilityMsgID ||
		!bytes.Equal(dailyAvailability.Body, wantDailyAvailability) {
		t.Fatalf("op1303 header=%+v body=%x want=%x", dailyAvailability.Header, dailyAvailability.Body, wantDailyAvailability)
	}
	joustState, rest := splitGameServerUpperPacket(t, rest)
	wantJoustState := buildCurrentJoustState(dnfjoust.TimelineAt(queue.Now()))
	if joustState.Header.Classification != 0 || joustState.Header.MsgID != currentJoustStatePushMsgID ||
		!bytes.Equal(joustState.Body, wantJoustState) {
		t.Fatalf("op1240 header=%+v body=%x want=%x", joustState.Header, joustState.Body, wantJoustState)
	}
	joustRoster, rest := splitGameServerUpperPacket(t, rest)
	if joustRoster.Header.Classification != 0 || joustRoster.Header.MsgID != currentJoustRosterPushMsgID {
		t.Fatalf("op1241 header=%+v", joustRoster.Header)
	}
	joustPool, rest := splitGameServerUpperPacket(t, rest)
	if joustPool.Header.Classification != 0 || joustPool.Header.MsgID != currentJoustPoolPushMsgID {
		t.Fatalf("op1242 header=%+v", joustPool.Header)
	}
	if len(rest) != 0 {
		t.Fatalf("town-init packets trailing=%x", rest)
	}
}

func TestCurrentSpendTimeInitialStateDoesNotReplayDescriptorAfterSceneReady(t *testing.T) {
	connection := &bufferConn{}
	service := &Service{}
	catalog := &currentSpendTimeRuntimeCatalog{
		rewardItemIDs:     []uint32{490006574, 490006575, 490006576, 490006577},
		totalStageSeconds: 10800,
	}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 7,
		characterGeneration: 3,
	}
	session.spendTime.characterID = session.selectedCharacterID
	session.spendTime.characterGeneration = session.characterGeneration
	session.spendTime.initializedCharacterGeneration = session.characterGeneration
	session.spendTime.catalog = catalog
	session.spendTime.anchor = time.Unix(1, 0)
	session.spendTime.onlineSeconds = 7199
	session.spendTime.completedStages = 2

	if err := service.sendCurrentSpendTimeInitialStateOnce(session, "town_after_scene_init_events"); err != nil {
		t.Fatal(err)
	}
	if got := connection.write.Len(); got != 0 {
		t.Fatalf("scene-ready callback replayed %d bytes; descriptor must remain one-shot", got)
	}
}

func TestSendCurrentSpendTimeProtocolStatePartialWriteNeverRequiresFirstDescriptorReplay(t *testing.T) {
	connection := &failNthDungeonWriteConn{failAt: 3, err: errors.New("Luke raid schedule write failed")}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = time.Unix(7200*321, 0).UTC().Add(10 * time.Minute)
	service := &Service{clientAccounts: newClientAccountRegistry(), joustCatalog: mustTestJoustEventCatalog(t), gameplayTimers: queue}
	session := &gameSession{conn: connection, connID: "spend-time-partial", clientPID: 101}
	if err := service.RegisterClientAccount(session.clientPID, "account-1"); err != nil {
		t.Fatal(err)
	}
	catalog := &currentSpendTimeRuntimeCatalog{
		rewardItemIDs:     []uint32{490006574, 490006575, 490006576, 490006577},
		totalStageSeconds: 10800,
	}
	sendDescriptor, ready := service.beginCurrentSpendTimeEventInfo(session)
	if !sendDescriptor || !ready {
		t.Fatalf("descriptor reservation send=%t ready=%t", sendDescriptor, ready)
	}
	descriptorSent, err := service.sendCurrentSpendTimeProtocolState(session, catalog, 1, 0, sendDescriptor)
	if err == nil || !descriptorSent {
		t.Fatalf("first send descriptor_sent=%t err=%v", descriptorSent, err)
	}
	service.finishCurrentSpendTimeEventInfo(session, descriptorSent)
	if !session.spendTime.eventInfoSent {
		t.Fatal("successful op108 partial write did not advance process-lifetime gate")
	}
	sendDescriptor, ready = service.beginCurrentSpendTimeEventInfo(session)
	if sendDescriptor || !ready {
		t.Fatalf("retry descriptor reservation send=%t ready=%t", sendDescriptor, ready)
	}
	descriptorSent, err = service.sendCurrentSpendTimeProtocolState(
		session,
		catalog,
		1,
		0,
		sendDescriptor,
	)
	if err != nil || descriptorSent {
		t.Fatalf("progress retry descriptor_sent=%t err=%v", descriptorSent, err)
	}

	wire := connection.bufferConn.write.Bytes()
	descriptor, rest := splitGameServerUpperPacketWithHeader(t, wire, dnfproto.GameServerUpperHeaderSize16)
	if descriptor.Header.MsgID != currentSpendTimeEventInfoMsgID {
		t.Fatalf("first packet msg=%d", descriptor.Header.MsgID)
	}
	progress, rest := splitGameServerUpperPacket(t, rest)
	if progress.Header.MsgID != currentSpendTimeProgressMsgID {
		t.Fatalf("first progress packet msg=%d", progress.Header.MsgID)
	}
	progress, rest = splitGameServerUpperPacket(t, rest)
	if progress.Header.MsgID != currentSpendTimeProgressMsgID {
		t.Fatalf("retry progress packet msg=%d", progress.Header.MsgID)
	}
	antonSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantAntonSchedule, err := buildCurrentRaidScheduleProgressBody(currentAntonRaidEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if antonSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(antonSchedule.Body, wantAntonSchedule) {
		t.Fatalf("retry Anton schedule packet msg=%d body=%x", antonSchedule.Header.MsgID, antonSchedule.Body)
	}
	lukeSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantLukeSchedule, err := buildCurrentRaidScheduleProgressBody(currentLukeRaidEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if lukeSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(lukeSchedule.Body, wantLukeSchedule) {
		t.Fatalf("retry Luke schedule packet msg=%d body=%x", lukeSchedule.Header.MsgID, lukeSchedule.Body)
	}
	fiendWarSchedule, rest := splitGameServerUpperPacket(t, rest)
	wantFiendWarSchedule, err := buildCurrentRaidScheduleProgressBody(currentFiendWarEventID, currentRaidScheduleCounters{})
	if err != nil {
		t.Fatal(err)
	}
	if fiendWarSchedule.Header.MsgID != currentSpendTimeProgressMsgID ||
		!bytes.Equal(fiendWarSchedule.Body, wantFiendWarSchedule) {
		t.Fatalf("retry Fiend War schedule packet msg=%d body=%x", fiendWarSchedule.Header.MsgID, fiendWarSchedule.Body)
	}
	dailyAvailability, rest := splitGameServerUpperPacket(t, rest)
	if dailyAvailability.Header.MsgID != currentRaidDailyAvailabilityMsgID {
		t.Fatalf("retry daily availability packet msg=%d", dailyAvailability.Header.MsgID)
	}
	joustState, rest := splitGameServerUpperPacket(t, rest)
	if joustState.Header.MsgID != currentJoustStatePushMsgID {
		t.Fatalf("retry joust state packet msg=%d", joustState.Header.MsgID)
	}
	joustRoster, rest := splitGameServerUpperPacket(t, rest)
	if joustRoster.Header.MsgID != currentJoustRosterPushMsgID {
		t.Fatalf("retry joust roster packet msg=%d", joustRoster.Header.MsgID)
	}
	joustPool, rest := splitGameServerUpperPacket(t, rest)
	if joustPool.Header.MsgID != currentJoustPoolPushMsgID {
		t.Fatalf("retry joust pool packet msg=%d", joustPool.Header.MsgID)
	}
	if len(rest) != 0 {
		t.Fatalf("retry town-init packets trailing=%x", rest)
	}
}

func TestBuildCurrentRaidDailyAvailabilityBodyMatchesCurrentNoPackRows(t *testing.T) {
	body, err := buildCurrentRaidDailyAvailabilityBody(defaultCurrentRaidDailyAvailabilityRows())
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		currentRaidAntonMode, 0, currentRaidAntonDailyLimit, 0, 0,
		currentRaidLukeMode, 0, currentRaidLukeDailyLimit, 0, 0,
		currentRaidFiendWarMode, 0, currentRaidFiendWarDailyLimit, 0, 0,
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("op1303 body=%x want=%x", body, want)
	}
}

func TestCurrentSpendTimeClaimItemsForCharacterRejectsReplayAndOtherCharacter(t *testing.T) {
	item := dnfonlineevent.CommittedItem{SlotKey: "0:9", SlotIndex: 9, ItemID: 490006574, Delta: 1, PostCount: 1, RawEntry: []byte{1, 2, 3}}
	if got := currentSpendTimeClaimItemsForCharacter(dnfonlineevent.ClaimResult{
		CharacterID: "7", Replayed: true, Items: []dnfonlineevent.CommittedItem{item},
	}, "7"); len(got) != 0 {
		t.Fatalf("replayed receipt leaked %d item updates", len(got))
	}
	if got := currentSpendTimeClaimItemsForCharacter(dnfonlineevent.ClaimResult{
		CharacterID: "8", Items: []dnfonlineevent.CommittedItem{item},
	}, "7"); len(got) != 0 {
		t.Fatalf("other-character receipt leaked %d item updates", len(got))
	}
	got := currentSpendTimeClaimItemsForCharacter(dnfonlineevent.ClaimResult{
		CharacterID: "7", Items: []dnfonlineevent.CommittedItem{item},
	}, "7")
	if len(got) != 1 || got[0].ItemID != item.ItemID {
		t.Fatalf("fresh same-character receipt=%+v", got)
	}
	got[0].RawEntry[0] = 9
	if item.RawEntry[0] != 1 {
		t.Fatal("claim item raw entry was not detached")
	}
}
