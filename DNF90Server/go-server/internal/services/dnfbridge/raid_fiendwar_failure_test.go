package dnfbridge

import (
	"context"
	"os"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/raid"
)

// TestFiendWarFailureKeepsTheAttackPartyAndPresentsTheBannerSequence pins the two
// operator rulings Fix D and Fix C implement together (2026-10-01):
//
//	「攻坚失败后 所有人应被传送回待机区 并且攻坚队不应该被解散」
//
// Three things have to hold at once, and each of them was broken before:
//
//  1. The 攻坚队 row must SURVIVE the failure with its squad labels intact. It used
//     to go through abandonRuntimeRaid, which deletes the row: town_move then
//     refused every op36 with AreaID >= 2 (fiendwar_membership_required), including
//     the waiting area, so a member standing in a town operation area could not
//     even walk back.
//  2. op665 must NOT be sent. sendRuntimeSingleRaidEnded(session, 665, ...) is what
//     makes the client print 46791「已经强制退出攻坚队。」 (raid_online.go:1501-1503)
//     -- the popup the operator reported as the party being disbanded.
//  3. The failure presentation must publish the 587/601 pair in that order. 601 is
//     the only packet in the whole client that writes raidObj+0x2C, and the banner
//     gate is the conjunction of the 587 state byte and that ResultType byte.
//
// The release that follows the hold is asserted too: it must put the client's start
// gate back to state 0 WITHOUT touching the party, because START_RAID is only
// accepted from state 0 or 5.
func TestFiendWarFailureKeepsTheAttackPartyAndPresentsTheBannerSequence(t *testing.T) {
	f := newRuntimeRaidRewardTestFixture(t, true)
	s := f.service
	state := s.raids[f.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	// Both members fight in squad 1 on both roster rows: the 编制 the failure must
	// hand back untouched.
	for i := range state.Members {
		state.Members[i].GroupIndex, state.Members[i].SlotOrder = 1, byte(i)
	}
	for i := range state.FrozenMembers {
		state.FrozenMembers[i].GroupIndex, state.FrozenMembers[i].SlotOrder = 1, byte(i)
	}
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		4962: {Phase: runtimeRaidDungeonPhaseOne, MapID: 4962, State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1},
	}
	queue := &runtimeRaidPhaseTestQueue{now: f.now}
	s.gameplayTimers = queue

	failed, err := s.failRuntimeFiendWarRaid(state.RaidKey, f.now)
	if err != nil {
		t.Fatalf("fail the Fiend War run: %v", err)
	}
	if failed.Lifecycle.Status != runtimeRaidLifecycleFailed {
		t.Fatalf("failure status = %d, want failed", failed.Lifecycle.Status)
	}
	// (1) The 攻坚队 survives, with every squad label it had.
	current := s.raids[state.RaidKey]
	if current == nil {
		t.Fatal("the failure deleted the 攻坚队 row: town_move then refuses every op36 with AreaID >= 2")
	}
	if len(current.Members) != 2 || countRuntimeRaidGroupedMembers(*current) != 2 {
		t.Fatalf("the failure damaged the roster: members=%d grouped=%d, want 2/2",
			len(current.Members), countRuntimeRaidGroupedMembers(*current))
	}
	if current.Members[0].GroupIndex != 1 || current.Members[1].GroupIndex != 1 {
		t.Fatalf("the failure cleared a squad label: %d/%d",
			current.Members[0].GroupIndex, current.Members[1].GroupIndex)
	}

	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.conn.(*bufferConn).write.Reset()
	}
	if err := s.publishFiendWarFailurePresentation(failed); err != nil {
		t.Fatalf("publish failure presentation: %v", err)
	}
	// 601 **必须排在 1453{6,0} 之前**。客户端反汇编（DNF.exe 8A7AF614…AA4D）：
	// 1453 的分派器 0x009B2910(byte* body) 按 body[0] ∈ 1..6 走跳表
	// [eax*4 + 0x9B2B7C]；case 6 的实体 0x009B2B18 读 raidObj+0x2C 决定发哪条文案
	// （== 2 → 75518 破坏成功；== 3 → 75503 未能破坏），而 raidObj+0x2C 全客户端只有
	// 601 的处理器会写（sub_1D6B840 尾部 0x01D6C3E8）。601 排在后面时，case 6 读到的是
	// 构造函数留下的旧值 —— 真机上就是"攻坚失败一条提示都没有"。
	want := []uint16{raid.RaidResultMsgID, 1453, raid.RaidDungeonStateMsgID, raid.RaidRemainTimeMsgID, raid.RaidStateMsgID}
	for _, peer := range []*gameSession{f.leader, f.member} {
		got := upperMsgIDSequence(t, peer.conn.(*bufferConn).write.Bytes())
		// (2) No disband popup, ever.
		for _, msgID := range got {
			if msgID == 665 {
				t.Fatalf("char %d received op665: that is the 「已经强制退出攻坚队。」 popup",
					peer.selectedCharacterID)
			}
		}
		if !isMsgIDSubsequence(got, want) {
			t.Fatalf("char %d payload order = %v, want the subsequence %v (banner result FIRST, then board close, closed gates, zeroed clock, banner state)",
				peer.selectedCharacterID, got, want)
		}
	}
	// 结果类型本身也要钉：1 是安东/卢克那个通用值，超时空的失败文案要的是 3。
	if runtimeRaidFiendWarFailResultType != 3 {
		t.Fatalf("超时空失败的结果类型 = %d, want 3 —— 客户端 case 5/6 只认 2(成功)/3(失败)，"+
			"发 1 时 75503「未能破坏量子传送站。」永远不会出现", runtimeRaidFiendWarFailResultType)
	}
	// The state byte is asserted against the client's own value, not against the
	// named constant: the client gate needs state 6 and the constant must therefore
	// BE 6. (A test that only compares the constant with itself cannot fail when the
	// constant is wrong.)
	if runtimeRaidSettlementStateFinished != 6 || runtimeRaidFiendWarFailureState != runtimeRaidSettlementStateFinished {
		t.Fatalf("the Fiend War failure state byte = %d, want the client's state 6",
			runtimeRaidFiendWarFailureState)
	}
	if got := encodedRaidStateForTest(t, f.leader, raid.RaidStateMsgID); got != runtimeRaidSettlementStateFinished {
		t.Fatalf("587 state = %d, want the client's failure state 6", got)
	}
	// The hold is a named constant and the release is armed, not immediate.
	if queue.scheduleCalls == 0 || queue.scheduledDelay != runtimeRaidFiendWarFailureHold {
		t.Fatalf("release scheduled calls=%d delay=%v, want the named hold %v",
			queue.scheduleCalls, queue.scheduledDelay, runtimeRaidFiendWarFailureHold)
	}
	// (3) The release re-opens the start gate and leaves the party alone.
	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.conn.(*bufferConn).write.Reset()
	}
	queue.callback(queue.now.Add(runtimeRaidFiendWarFailureHold))
	for _, peer := range []*gameSession{f.leader, f.member} {
		if got := encodedRaidStateForTest(t, peer, raid.RaidStateMsgID); got != runtimeRaidFiendWarFailureReleaseState {
			t.Fatalf("char %d release state = %d, want %d (only 0 and 5 permit START_RAID)",
				peer.selectedCharacterID, got, runtimeRaidFiendWarFailureReleaseState)
		}
	}
	after := s.raids[state.RaidKey]
	if after == nil {
		t.Fatal("the release deleted the 攻坚队 row")
	}
	if len(after.Members) != 2 || countRuntimeRaidGroupedMembers(*after) != 2 {
		t.Fatalf("the release damaged the roster: members=%d grouped=%d, want 2/2",
			len(after.Members), countRuntimeRaidGroupedMembers(*after))
	}
	if after.Started || after.StartPending || after.EntryCostPaid || len(after.FrozenMembers) != 0 {
		t.Fatalf("the release did not return the raid to its lobby: started=%t pending=%t paid=%t frozen=%d",
			after.Started, after.StartPending, after.EntryCostPaid, len(after.FrozenMembers))
	}
}

// TestRealPVFFiendWarFailureReturnsEveryoneToTheWaitingArea covers the half of Fix D
// the unit test cannot: publishFailedFiendWar's TOWN branch.
//
// The old tail handled only members physically inside a dungeon
// (raid_fiendwar_battle.go returned early when session.dungeon.runtime was nil) and
// answered everybody else with op665. A member standing in a Fiend War town operation
// area therefore never moved, and because the tail had also deleted the 攻坚队 row,
// town_move refused every later op36 with AreaID >= 2 (fiendwar_membership_required)
// -- including the waiting area itself -- so his client hung where it stood.
//
// This drives the real composition (failRuntimeFiendWarRaid + publishFailedFiendWar)
// against the deployed Script.pvf and asserts that both members end in the waiting
// room 98/2 with the 攻坚队 and its 编制 intact and no disband popup.
func TestRealPVFFiendWarFailureReturnsEveryoneToTheWaitingArea(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	f := newRuntimeRaidRewardTestFixture(t, true)
	s := f.service
	s.options.pvfPath = path
	if err := s.preloadTownCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, err := s.runtimeRaidFiendWarConfig()
	if err != nil {
		t.Fatal(err)
	}
	state := s.raids[f.raidKey]
	state.RaidMode, state.RouteOrRaidType = runtimeRaidOwnerFiendWar, 2
	state.AntonConfig, state.FiendWarConfig = nil, config
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.FiendWar, err = raid.NewFiendWarBattle(config.Battle)
	if err != nil {
		t.Fatal(err)
	}
	state.Lifecycle.FiendWar.GroupAreas[1] = int(currentFiendWarWaitingAreaID)
	for i := range state.Members {
		state.Members[i].GroupIndex, state.Members[i].SlotOrder = 1, byte(i)
	}
	for i := range state.FrozenMembers {
		state.FrozenMembers[i].GroupIndex, state.FrozenMembers[i].SlotOrder = 1, byte(i)
	}
	s.onlinePlayers = newOnlinePlayerManager()
	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.channel = channelcatalog.Channel{ID: 261, Type: 42}
		peer.conn.(*bufferConn).write.Reset()
	}
	// Both members are in the town scene, not inside a dungeon: exactly the case the
	// old tail never touched.
	bindDungeonSelectorOriginForTestAt(t, s, f.leader, 98, 6, 177, 597)
	bindDungeonSelectorOriginForTestAt(t, s, f.member, 98, 9, 105, 320)

	failed, err := s.failRuntimeFiendWarRaid(state.RaidKey, f.now)
	if err != nil {
		t.Fatalf("fail the Fiend War run: %v", err)
	}
	if err := s.publishFailedFiendWar(failed); err != nil {
		t.Fatalf("publish the Fiend War failure: %v", err)
	}

	after := s.raids[state.RaidKey]
	if after == nil {
		t.Fatal("攻坚失败后攻坚队被解散了：the raid row was deleted")
	}
	if len(after.Members) != 2 || countRuntimeRaidGroupedMembers(*after) != 2 {
		t.Fatalf("the failure damaged the 编制: members=%d grouped=%d, want 2/2",
			len(after.Members), countRuntimeRaidGroupedMembers(*after))
	}
	for _, peer := range []*gameSession{f.leader, f.member} {
		peer.townMu.Lock()
		position := peer.townPositionSnapshot
		ready := peer.townSceneReadyCharacterID
		peer.townMu.Unlock()
		if position.TownID != byte(currentFiendWarRaidTownID) ||
			position.AreaID != currentFiendWarWaitingAreaID ||
			!position.PositionValid {
			t.Fatalf("char %d landed at town %d area %d (valid=%t), want the waiting room %d/%d",
				peer.selectedCharacterID, position.TownID, position.AreaID, position.PositionValid,
				currentFiendWarRaidTownID, currentFiendWarWaitingAreaID)
		}
		if ready != peer.selectedCharacterID {
			t.Fatalf("char %d waiting-area scene was not finalized (ready=%d)",
				peer.selectedCharacterID, ready)
		}
		for _, msgID := range upperMsgIDSequence(t, peer.conn.(*bufferConn).write.Bytes()) {
			if msgID == 665 {
				t.Fatalf("char %d received op665: that is the 「已经强制退出攻坚队。」 popup",
					peer.selectedCharacterID)
			}
		}
	}
}

// upperMsgIDSequence lists the message ids one session received, in order.
func upperMsgIDSequence(t *testing.T, raw []byte) []uint16 {
	t.Helper()
	var ids []uint16
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		ids = append(ids, packet.Header.MsgID)
	}
	return ids
}

// isMsgIDSubsequence reports whether want appears in got in that relative order.
func isMsgIDSubsequence(got, want []uint16) bool {
	index := 0
	for _, id := range got {
		if index < len(want) && id == want[index] {
			index++
		}
	}
	return index == len(want)
}

// encodedRaidStateForTest reads the last 0x024B state byte one session received.
func encodedRaidStateForTest(t *testing.T, session *gameSession, msgID uint16) byte {
	t.Helper()
	raw := append([]byte(nil), session.conn.(*bufferConn).write.Bytes()...)
	state := byte(0xff)
	for len(raw) != 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		raw = rest
		if packet.Header.MsgID != msgID || len(packet.Body) < 1 {
			continue
		}
		state = packet.Body[0]
	}
	if state == 0xff {
		t.Fatalf("char %d never received a %d notification", session.selectedCharacterID, msgID)
	}
	return state
}
