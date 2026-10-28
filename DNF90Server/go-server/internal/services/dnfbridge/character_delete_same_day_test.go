package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestCurrentCharacterDeleteSameDayBlockedUsesTheServiceDayBoundary pins which
// "day" the restriction means. Every other daily counter on this server rolls
// at premium.ServiceDayID (06:00 Beijing), so 当天创建 is the same service day
// the rest of the game counts rather than a rolling 24 hours.
func TestCurrentCharacterDeleteSameDayBlockedUsesTheServiceDayBoundary(t *testing.T) {
	// 2026-09-10 08:00 UTC is 16:00 Beijing, inside the service day that
	// started at 06:00 Beijing (22:00 UTC on 09-09).
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	for name, probe := range map[string]struct {
		createdAt time.Time
		want      bool
	}{
		"created_minutes_ago":             {createdAt: now.Add(-30 * time.Minute), want: true},
		"created_at_the_day_start":        {createdAt: time.Date(2026, 9, 9, 22, 0, 0, 0, time.UTC), want: true},
		"created_just_before_day_start":   {createdAt: time.Date(2026, 9, 9, 21, 59, 59, 0, time.UTC), want: false},
		"created_yesterday":               {createdAt: now.Add(-24 * time.Hour), want: false},
		"created_at_unknown_time_is_free": {createdAt: time.Time{}, want: false},
	} {
		if got := currentCharacterDeleteSameDayBlocked(probe.createdAt, now); got != probe.want {
			t.Fatalf("%s: blocked=%t, want %t (created_day=%d now_day=%d)",
				name, got, probe.want,
				premium.ServiceDayID(probe.createdAt.UTC()), premium.ServiceDayID(now.UTC()))
		}
	}
}

// TestHandleDeleteCharacterRefusesACharacterCreatedTodayWithoutMutation drives
// op6 end to end: a character created inside the current service day must come
// back as the dedicated refusal code, and the roster must be untouched.
func TestHandleDeleteCharacterRefusesACharacterCreatedTodayWithoutMutation(t *testing.T) {
	repos := testRepositoryGroup()
	store := repos.Character.(*fakeCharacterStore)
	store.records["10"] = dnfrepo.CharacterRecord{CharacterID: "10", AccountID: "dnf:1", Slot: 0, Name: "keep"}
	store.records["20"] = dnfrepo.CharacterRecord{
		CharacterID: "20", AccountID: "dnf:1", Slot: 1, Name: "fresh",
		CreatedAt: time.Now().UTC(),
	}
	service := &Service{
		options:            options{accountPrefix: "dnf:", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repos, true },
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection}
	if err := service.handleGameUpper(session, buildDeleteCharacterFrameForTest(t, 1, "fresh")); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketDeleteCharacter) ||
		!bytes.Equal(packet.Body, []byte{0, currentDeleteCharacterRefusedError}) {
		t.Fatalf("same-day delete response header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
	fresh := store.records["20"]
	if fakeCharacterDeleteFlag(fresh) != 0 || fresh.Slot != 1 || store.records["10"].Slot != 0 {
		t.Fatalf("refused delete mutated the roster: fresh=%+v keep=%+v", fresh, store.records["10"])
	}

	// The same request for a character created before today still deletes, so
	// the guard is not a blanket delete block.
	connection.write.Reset()
	store.records["30"] = dnfrepo.CharacterRecord{
		CharacterID: "30", AccountID: "dnf:1", Slot: 2, Name: "old",
		CreatedAt: time.Now().UTC().Add(-72 * time.Hour),
	}
	if err := service.handleGameUpper(session, buildDeleteCharacterFrameForTest(t, 2, "old")); err != nil {
		t.Fatal(err)
	}
	packet, rest = splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || !bytes.Equal(packet.Body, []byte{1, 0, 2, 0}) {
		t.Fatalf("older character delete response body=%x trailing=%x", packet.Body, rest)
	}
	if fakeCharacterDeleteFlag(store.records["30"]) != 1 {
		t.Fatalf("older character was not deleted: %+v", store.records["30"])
	}
}

func buildDeleteCharacterFrameForTest(t *testing.T, slot uint16, name string) []byte {
	t.Helper()
	raw := []byte(name)
	body := make([]byte, 6+len(raw))
	binary.LittleEndian.PutUint16(body[:2], slot)
	binary.LittleEndian.PutUint32(body[2:6], uint32(len(raw)))
	copy(body[6:], raw)
	frame, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketDeleteCharacter),
		body,
		0,
		dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatalf("build delete packet: %v", err)
	}
	return frame
}
