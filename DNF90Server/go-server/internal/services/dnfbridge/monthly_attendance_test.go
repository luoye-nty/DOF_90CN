package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestMonthlyAttendanceRefreshPublishesOnlyChangesAndRetriesWrites(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = time.Date(2026, 9, 8, 12, 0, 0, 0, dnfonlineevent.ChinaCalendar)
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"7", "8"} {
		if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{AccountID: "a", CharacterID: id}); err != nil {
			t.Fatal(err)
		}
	}
	catalog := &dnfonlineevent.MonthlyCatalog{Thresholds: [3]uint32{7, 14, 21}}
	for i := range catalog.Daily {
		catalog.Daily[i] = dnfonlineevent.AttendancePVFItemReward{ItemID: 100, Count: 1}
	}
	for i := range catalog.Bonus {
		catalog.Bonus[i] = dnfonlineevent.AttendancePVFItemReward{ItemID: 100, Count: 1}
	}
	service := &Service{
		gameplayTimers: queue, monthlyAttendanceCatalog: catalog,
		joustCatalog: mustTestJoustEventCatalog(t),
		pvfItemCatalog: &pvfDungeonDropCatalog{source: bridgePVFSource{}, itemCache: map[uint32]dungeonDropItemDefinition{
			100: {ItemID: 100, Kind: dungeonDropItemStackable},
		}},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, accountID: "a", selectedCharacterID: 7, characterGeneration: 1}
	check := func(wantProgress bool, days, clears uint32) {
		t.Helper()
		conn.write.Reset()
		if err := service.sendCurrentMonthlyAttendance(session, false); err != nil {
			t.Fatal(err)
		}
		if !wantProgress {
			if conn.write.Len() != 0 {
				t.Fatalf("unchanged attendance replayed %d bytes", conn.write.Len())
			}
			return
		}
		packet, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.MsgID != 1206 || binary.LittleEndian.Uint32(packet.Body) != 2370 ||
			binary.LittleEndian.Uint32(packet.Body[5:]) != days || binary.LittleEndian.Uint32(packet.Body[21:]) != clears {
			t.Fatalf("attendance packet=%+v body=%x", packet.Header, packet.Body)
		}
	}
	clear := func(at time.Time) {
		t.Helper()
		account, _, err := repos.Account.Load(ctx, "a")
		if err != nil {
			t.Fatal(err)
		}
		if err := dnfonlineevent.RecordMonthlyClear(&account, at); err != nil {
			t.Fatal(err)
		}
		if err := repos.Account.Save(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	check(true, 0, 0)
	check(false, 0, 0)
	clear(queue.now)
	check(true, 0, 1)
	clear(queue.now)
	check(true, 1, 2)
	for range 4 {
		queue.now = queue.now.Add(30 * time.Second)
		check(false, 1, 2)
	}
	conn.write.Reset()
	if err := service.handleCurrentMonthlyAttendanceClaim(session, make([]byte, 13)); err != nil {
		t.Fatal(err)
	}
	ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Header.MsgID != 1294 || ack.Body[0] != 1 || len(rest) != 0 {
		t.Fatalf("unchanged claim should ACK without repeating completion: ack=%+v rest=%x", ack.Header, rest)
	}
	for day := 1; day < 7; day++ {
		at := queue.now.AddDate(0, 0, -day)
		clear(at)
		clear(at)
	}
	check(true, 7, 2)
	conn.write.Reset()
	if err := service.handleCurrentMonthlyAttendanceClaim(session, make([]byte, 13)); err != nil {
		t.Fatal(err)
	}
	_, rest = splitGameServerUpperPacket(t, conn.write.Bytes())
	progress, _ := splitGameServerUpperPacket(t, rest)
	if progress.Header.MsgID != 1206 || binary.LittleEndian.Uint32(progress.Body[9:]) != 2 {
		t.Fatalf("claimed bonus did not update: %x", progress.Body)
	}
	check(false, 7, 2)
	session.selectedCharacterID = 8
	check(true, 7, 2)
	session.characterGeneration++
	check(true, 7, 2)
	queue.now = time.Date(2026, 9, 9, 6, 0, 0, 0, dnfonlineevent.ChinaCalendar)
	check(true, 7, 0)
	queue.now = queue.now.Add(24 * time.Hour)
	check(true, 7, 0) // A new service day still needs its initial projection.
	queue.now = time.Date(2026, 10, 1, 6, 0, 0, 0, dnfonlineevent.ChinaCalendar)
	wantErr := errors.New("attendance write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	if err := service.sendCurrentMonthlyAttendance(session, false); !errors.Is(err, wantErr) {
		t.Fatalf("write error=%v", err)
	}
	session.conn = conn
	check(true, 0, 0)
	check(false, 0, 0)
	conn.write.Reset()
	spend := &currentSpendTimeRuntimeCatalog{rewardItemIDs: []uint32{100}, totalStageSeconds: 10800}
	if sent, err := service.sendCurrentSpendTimeProtocolState(session, spend, 0, 0, true); err != nil || !sent {
		t.Fatalf("descriptor replay sent=%t err=%v", sent, err)
	}
	found := false
	for wire := conn.write.Bytes(); len(wire) > 0; {
		packet, tail := splitGameServerUpperPacket(t, wire)
		if packet.Header.MsgID == 1206 && binary.LittleEndian.Uint32(packet.Body) == 2370 {
			found = true
		}
		wire = tail
	}
	if !found {
		t.Fatal("activity catalog replay did not restore attendance state")
	}
	check(false, 0, 0)
}

func TestMonthlyAttendanceNativeProgressAndRealRewards(t *testing.T) {
	body := buildCurrentMonthlyAttendanceBody(dnfonlineevent.MonthlySnapshot{Days: 14, Bonus: [3]uint32{2, 1, 0}, Clears: 2})
	if len(body) != 37 || binary.LittleEndian.Uint32(body) != 2370 || body[4] != 0 {
		t.Fatalf("monthly progress envelope=%x", body)
	}
	for i, want := range []uint32{14, 2, 1, 0, 2, 0, 0, 0} {
		if binary.LittleEndian.Uint32(body[5+i*4:]) != want {
			t.Fatalf("monthly native field[%d]=%x", i, body)
		}
	}
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		return
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfonlineevent.LoadMonthlyCatalog(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Thresholds != [3]uint32{7, 14, 21} {
		t.Fatalf("thresholds=%v", catalog.Thresholds)
	}
	service := &Service{initialEquipmentArchive: archive}
	items, err := service.currentPVFItemCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, reward := range append(catalog.Daily[:], catalog.Bonus[:]...) {
		definition, err := items.ResolveItem(uint32(reward.ItemID))
		if err != nil || definition.Kind != dungeonDropItemStackable || (!definition.ExpirationDate.IsZero() && !definition.ExpirationDate.After(time.Now())) {
			t.Fatalf("reward=%+v definition=%+v err=%v", reward, definition, err)
		}
	}
}

func TestMonthlyAttendanceRealPVFClearSettlementAndMail(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service, runtime, session, conn, _ := prepareCompletedSettlementRuntime(t)
	service.initialEquipmentArchive = archive
	session.accountID = "account-1"
	repos, _ := service.repositoryGroup()
	ctx := context.Background()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	character, _, _ := repos.Character.Load(ctx, "99")
	character.Level = 1
	character.Stats["exp"] = 90
	if err := repos.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repos.Skill.Save(ctx, dnfrepo.SkillRecord{CharacterID: "99", Points: dnfrepo.SkillPointState{TotalSP: 10, RemainingSP: 10, SyncedLevel: 1}}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = character
	runtime.entryCharacterLevel = 1
	runtime.ordinaryFinalRoomClearAccepted = true
	runtime.ProtocolMode = currentDungeonProtocolModeNormal
	runtime.Dungeon.Metadata.RecommendedLevels = []int64{1, 1}
	tables := currentDungeonSettlementTestResources(t).Progression
	for run := 1; run <= 2; run++ {
		runtime.clearMapCompletionKey = fmt.Sprintf("monthly-clear-%d", run)
		gain := uint32(0)
		if run == 1 {
			gain = 20
		}
		result, err := service.commitCurrentDungeonSettlement(ctx, session, runtime, tables, gain)
		if err != nil {
			t.Fatal(err)
		}
		runtime.Character = result.Character
		if !currentDungeonIsRecommendedClear(runtime) {
			t.Fatal("level gain suppressed attendance publication")
		}
		replay, err := service.commitCurrentDungeonSettlement(ctx, session, runtime, tables, gain)
		if err != nil || !replay.Replayed {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
		conn.write.Reset()
		if err := service.sendCurrentMonthlyAttendance(session, false); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.MsgID != 1206 || binary.LittleEndian.Uint32(packet.Body[5:]) != uint32(run/2) || binary.LittleEndian.Uint32(packet.Body[21:]) != uint32(run) {
			t.Fatalf("run=%d progress=%x", run, packet.Body)
		}
		mailbox, _, err := repos.Mailbox.Load(ctx, "99")
		if err != nil || len(mailbox.Mails) != run/2 {
			t.Fatalf("run=%d mails=%d err=%v", run, len(mailbox.Mails), err)
		}
		runtime.entryCharacterLevel = runtime.Character.Level
		runtime.Dungeon.Metadata.RecommendedLevels = []int64{2, 2}
	}
	if err := service.handleCurrentMonthlyAttendanceClaim(session, make([]byte, 13)); err != nil {
		t.Fatal(err)
	}
	mailbox, _, err := repos.Mailbox.Load(ctx, "99")
	if err != nil || len(mailbox.Mails) != 1 {
		t.Fatalf("first PVF reward mailbox=%+v err=%v", mailbox, err)
	}
	for _, mail := range mailbox.Mails {
		if len(mail.Attachments) != 1 || mail.Attachments[0].ItemID != 490008975 {
			t.Fatalf("first PVF reward=%+v", mail)
		}
	}
}
