package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestWeeklyAttendanceProtocolClaimsRefreshAndRetry(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = dnfonlineevent.WeeklyStarts()
	c := &dnfonlineevent.WeeklyCatalog{RequiredClears: 4, CatchupClears: 20, CatchupLimit: 2, Thresholds: [3]uint32{4, 8, 11}}
	for i := range c.Rewards {
		c.Rewards[i] = dnfonlineevent.AttendancePVFItemReward{ItemID: 100, Count: 1}
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"7", "8"} {
		if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{AccountID: "a", CharacterID: id}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{gameplayTimers: queue, weeklyAttendanceCatalog: c,
		pvfItemCatalog:     &pvfDungeonDropCatalog{source: bridgePVFSource{}, itemCache: map[uint32]dungeonDropItemDefinition{100: {ItemID: 100, Kind: dungeonDropItemStackable}}},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, accountID: "a", selectedCharacterID: 7, characterGeneration: 1}
	check := func(changed bool, total, clears, week uint32) {
		t.Helper()
		conn.write.Reset()
		if err := service.sendCurrentWeeklyAttendance(session, false, false); err != nil {
			t.Fatal(err)
		}
		if !changed {
			if conn.write.Len() != 0 {
				t.Fatal("unchanged weekly state replayed")
			}
			return
		}
		packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if packet.Header.MsgID != 1093 || packet.Header.Classification != 0 || len(packet.Body) != 459 || len(rest) != 0 ||
			binary.LittleEndian.Uint32(packet.Body[4:]) != total || binary.LittleEndian.Uint32(packet.Body[0x1b4:]) != clears || binary.LittleEndian.Uint32(packet.Body[0x1c4:]) != week {
			t.Fatalf("weekly state=%+v body=%x", packet.Header, packet.Body)
		}
	}
	check(true, 0, 0, 1)
	check(false, 0, 0, 1)
	for clear := 1; clear <= 4; clear++ {
		a, _, _ := repos.Account.Load(ctx, "a")
		if err := dnfonlineevent.RecordWeeklyClear(&a, c, queue.now); err != nil {
			t.Fatal(err)
		}
		if err := repos.Account.Save(ctx, a); err != nil {
			t.Fatal(err)
		}
		check(true, 0, uint32(clear), 1)
	}
	for attempt := 0; attempt < 2; attempt++ {
		conn.write.Reset()
		frame, err := dnfproto.BuildChannelPacket(1113, bytes.Repeat([]byte{0xff}, 13), 0, dnfproto.DefaultChannelClassification)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.handleGameUpper(session, frame); err != nil {
			t.Fatal(err)
		}
		ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if ack.Header.MsgID != 1113 || ack.Header.Classification != 1 || !bytes.Equal(ack.Body, []byte{1, byte(attempt), 0, 0, 0}) {
			t.Fatalf("claim ack=%+v body=%x", ack.Header, ack.Body)
		}
		state, _ := splitGameServerUpperPacket(t, rest)
		if state.Header.MsgID != 1093 || binary.LittleEndian.Uint32(state.Body[4:]) != 1 || binary.LittleEndian.Uint32(state.Body[0x1c0:]) != 1 || state.Body[0x1c8] != 0 {
			t.Fatalf("claimed state=%x", state.Body)
		}
	}
	m, _, _ := repos.Mailbox.Load(ctx, "7")
	if len(m.Mails) != 1 {
		t.Fatalf("duplicate mail count=%d", len(m.Mails))
	}
	check(false, 1, 0, 1)
	session.selectedCharacterID = 8
	check(true, 1, 0, 1)
	session.characterGeneration++
	check(true, 1, 0, 1)
	queue.now = queue.now.AddDate(0, 0, 7)
	wantErr := errors.New("weekly write failed")
	session.conn = &failNthDungeonWriteConn{failAt: 1, err: wantErr}
	if err := service.sendCurrentWeeklyAttendance(session, false, false); !errors.Is(err, wantErr) {
		t.Fatalf("write err=%v", err)
	}
	session.conn = conn
	check(true, 1, 0, 2)
	check(false, 1, 0, 2)
	bonus := make([]byte, 16)
	binary.LittleEndian.PutUint32(bonus, 2208)
	conn.write.Reset()
	if err := service.handleCurrentWeeklyAttendanceBonus(session, bonus); err != nil {
		t.Fatal(err)
	}
	ack, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Header.MsgID != 689 || !bytes.Equal(ack.Body, []byte{1, 0xa0, 8, 0, 0}) {
		t.Fatalf("bonus ack=%x", ack.Body)
	}
	for _, invalid := range [][]byte{nil, make([]byte, 13), append([]byte{0xa0, 8, 0, 0, 1}, make([]byte, 11)...)} {
		conn.write.Reset()
		if err := service.handleCurrentWeeklyAttendanceBonus(session, invalid); err != nil {
			t.Fatal(err)
		}
		ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
		if ack.Body[0] != 0 || len(rest) != 0 {
			t.Fatal("malformed bonus accepted")
		}
	}
	session.selectedCharacterID = 0
	conn.write.Reset()
	if err := service.handleCurrentWeeklyAttendanceBonus(session, bonus); err != nil {
		t.Fatal(err)
	}
	ack, _ = splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Body[0] != 0 {
		t.Fatal("unselected character claim accepted")
	}
}

func TestWeeklyAttendanceNativeLayoutAndRealRewards(t *testing.T) {
	s := dnfonlineevent.WeeklySnapshot{Total: 11, BonusMask: 1<<4 | 1<<8 | 1<<11, Week: 12, Clears: 20, Catchups: 1, SignedThisWeek: true, CanSign: true, CanCatchup: true, CanBonus: true, SignedWeeks: [20]byte{1, 2, 3}}
	b := buildCurrentWeeklyAttendanceBody(s)
	for offset, want := range map[int]uint32{4: 11, 8: 0x910, 0x1b4: 20, 0x1bc: 1, 0x1c0: 1, 0x1c4: 12} {
		if binary.LittleEndian.Uint32(b[offset:]) != want {
			t.Fatalf("native field %#x=%x", offset, b[offset:offset+4])
		}
	}
	if b[0x19c] != 1 || b[0x19e] != 3 || !bytes.Equal(b[0x1c8:], []byte{1, 1, 1}) {
		t.Fatal("native arrays or flags invalid")
	}
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		return
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := dnfonlineevent.LoadWeeklyCatalog(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequiredClears != 4 || c.CatchupClears != 20 || c.CatchupLimit != 2 || c.Thresholds != [3]uint32{4, 8, 11} {
		t.Fatalf("weekly catalog=%+v", c)
	}
	service := &Service{initialEquipmentArchive: archive}
	items, err := service.currentPVFItemCatalog()
	if err != nil {
		t.Fatal(err)
	}
	rewards := append(append([]dnfonlineevent.AttendancePVFItemReward{}, c.Rewards[:]...), c.Bonus[:]...)
	for i, reward := range rewards {
		definition, err := items.ResolveItem(uint32(reward.ItemID))
		if reward.ItemID != int64(490009009+i) || reward.Count != 1 || err != nil || definition.Kind != dungeonDropItemStackable || (!definition.ExpirationDate.IsZero() && !definition.ExpirationDate.After(dnfonlineevent.WeeklyEnds().Add(24*time.Hour))) {
			t.Fatalf("reward=%+v definition=%+v err=%v", reward, definition, err)
		}
	}
}
