package dnfbridge

import (
	"testing"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

// testGuildMemberListNow is a fixed clock for the member-list builders. The row
// carries elapsed-offline seconds, so the tests need a stable "now".
func testGuildMemberListNow() time.Time {
	return time.Date(2026, 9, 8, 22, 0, 0, 0, time.UTC)
}

// The elapsed-offline u32 is the only field in a member row that separates an
// offline member from an online one: the channel byte 0xff just becomes
// member+0x34 == -1, which the window turns into a flag rather than dropping
// the row. Sending zero for an offline member -- which this tree did, because
// the slot carried Mileage -- makes it indistinguishable from an online one.
func TestCurrentGuildMemberOfflineSecondsSeparatesOnlineFromOffline(t *testing.T) {
	now := testGuildMemberListNow()
	member := dnfguild.Member{
		CharacterID:  "42",
		JoinedAt:     now.Add(-72 * time.Hour),
		LastOnlineAt: now.Add(-90 * time.Minute),
	}

	if got := currentGuildMemberOfflineSeconds(member, true, now); got != 0 {
		t.Errorf("online member offline seconds = %d, want 0", got)
	}
	if got := currentGuildMemberOfflineSeconds(member, false, now); got != 5400 {
		t.Errorf("offline member offline seconds = %d, want 5400", got)
	}

	// A member that was never stamped online falls back to its join time, so
	// the value stays non-zero. Reporting zero would look online.
	neverSeen := dnfguild.Member{CharacterID: "1", JoinedAt: now.Add(-2 * time.Hour)}
	if got := currentGuildMemberOfflineSeconds(neverSeen, false, now); got != 7200 {
		t.Errorf("never-online member offline seconds = %d, want 7200", got)
	}

	// A clock that moved backwards must not wrap the unsigned field.
	future := dnfguild.Member{CharacterID: "7", LastOnlineAt: now.Add(time.Hour)}
	if got := currentGuildMemberOfflineSeconds(future, false, now); got != 0 {
		t.Errorf("future last-online offline seconds = %d, want 0", got)
	}
	if got := currentGuildMemberOfflineSeconds(dnfguild.Member{}, false, now); got != 0 {
		t.Errorf("unknown member offline seconds = %d, want 0", got)
	}
}
