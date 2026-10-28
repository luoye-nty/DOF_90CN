package dnfbridge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Leaving the ordinary party has to clear that member's attack-squad label too.
// The client draws the roster row from the last push it received, so a label left
// behind keeps claiming a squad the player is no longer in. 86JP says the same:
// RaidHandler.HandleNormalPartyLeftAsync assigns the leaver to party index 0 and
// rebroadcasts the raid object and the member list (RaidHandler.cs:251-266).
func TestRuntimeRaidPartyLeaveClearsTheSquadLabel(t *testing.T) {
	fixture := newRuntimeRaidDisconnectFixture(t, 1001, 1002)
	service := fixture.service
	logPath := filepath.Join(t.TempDir(), "packet.log")
	logger, err := openPacketLogger(logPath)
	if err != nil {
		t.Fatal(err)
	}
	service.packetLog.Store(logger)

	service.raidMu.Lock()
	state := service.raids[fixture.raidKey]
	state.Started = true
	state.EntryCostPaid = true
	state.Lifecycle = newRuntimeRaidLifecycleState(time.Now().UTC())
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseOne
	// Both members share one squad, so the leaver really owns a label.
	for i := range state.Members {
		state.Members[i].GroupIndex = 2
		state.Members[i].SlotOrder = byte(i)
	}
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.Members...)
	service.raidMu.Unlock()

	if !service.unassignRuntimeRaidSquadOnPartyLeave(1002) {
		t.Fatal("the leaver kept its squad label")
	}
	after := fixture.state(t)
	member := after.Members[runtimeRaidMemberIndex(after, 1002)]
	if member.GroupIndex != 0 || member.SlotOrder != 0 {
		t.Fatalf("leaver group=%d slot=%d want standby 0/0", member.GroupIndex, member.SlotOrder)
	}
	frozen, found := runtimeRaidFrozenMember(after, 1002)
	if !found || frozen.GroupIndex != 0 || frozen.SlotOrder != 0 {
		t.Fatalf("frozen roster kept the label: %+v found=%t", frozen, found)
	}
	if other := after.Members[runtimeRaidMemberIndex(after, 1001)]; other.GroupIndex != 2 {
		t.Fatalf("the member that stayed lost its squad: %+v", other)
	}
	assertRuntimeRaidGridUnique(t, after)

	// A second gesture is a no-op: the label is already standby.
	if service.unassignRuntimeRaidSquadOnPartyLeave(1002) {
		t.Fatal("an already standby member was unassigned twice")
	}

	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	// 包日志自 2026-09-30 起落盘即密文；断言前先解密。
	text := packetLogPlaintext(t, logPath)
	if !strings.Contains(text, "game-raid-squad-unassigned-on-party-leave") ||
		!strings.Contains(text, "char_id=1002") {
		t.Fatalf("the unassign was not logged:\n%s", text)
	}
	// The panel (mode0) and the roster (mode3) both travel as 0x024F.
	if strings.Count(text, "msg_id=591") < 2 {
		t.Fatalf("the raid object and the roster were not both republished:\n%s", text)
	}
}

// The 218 -> hatchery warp keeps the energy-interception attempt valid on purpose
// and emits no lifecycle event for 218, so the 战况表 keeps drawing the squad inside
// 能量阻截战. That was reported on 2026-09-17 and deliberately left alone after the
// user rolled the fix back ("不用修这玩意"): the row is cosmetic, the attempt really
// is still open, and re-entering 218 republishes op=0 + op=1 anyway.
