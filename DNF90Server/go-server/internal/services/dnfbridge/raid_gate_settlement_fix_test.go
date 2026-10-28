package dnfbridge

import (
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The corner-room wall_open warp out of an active energy-interception run (218)
// into a hatchery the phase has opened sends the ordinary op15. The portal used to
// refuse it because the member is by definition not in the private waiting
// instance, and that refusal answers nothing at all: live 2026-09-17 07:25:12Z the
// client got no op15 ACK, no fatigue row and no op27 selector page for
// dungeon_id=222, and was then left unable to move (game-dungeon-move-blocked
// reason=dungeon_runtime_missing) or to leave
// (game-dungeon-giveup-game-blocked reason=active_dungeon_runtime_owner_missing).
func TestRuntimeRaidSelectorAdmissionAdmitsTheHatcheryWarp(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		antonEnergyInterceptionMapID: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: antonEnergyInterceptionMapID,
			State: raid.AntonDungeonHide, MaxHP: 1, RemainingHP: 1,
		},
		222: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: 222,
			State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
		},
	}
	service.onlinePlayers = newOnlinePlayerManager()
	channel := channelcatalog.Channel{ID: runtimeRaidChannelID(raidKey), Type: 23, Name: "anton", Port: 10023}
	session := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: 1001}
	service.bindGameSessionCharacter(session, 1001)
	runtime := &runtimeDungeonState{
		Request:   dungeoncmd.SelectDungeonRequest{DungeonID: antonEnergyInterceptionMapID},
		Dungeon:   worldmap.Dungeon{ID: antonEnergyInterceptionMapID},
		Character: dnfrepo.CharacterRecord{CharacterID: "1001"},
	}
	session.dungeon = dungeonSessionState{runtime: runtime}

	admitted, allowed, reason := service.runtimeRaidSelectorAdmission(session)
	if !allowed || reason != "hatchery_transfer_warp" || admitted.RaidKey != raidKey {
		t.Fatalf("hatchery warp admission allowed=%t reason=%q state=%+v", allowed, reason, admitted)
	}

	// The same member standing anywhere else keeps the original refusal.
	session.dungeon = dungeonSessionState{}
	if _, allowed, reason := service.runtimeRaidSelectorAdmission(session); allowed || reason != "raid_member_not_in_private_waiting_instance" {
		t.Fatalf("a member outside the energy-interception gate was admitted: allowed=%t reason=%q", allowed, reason)
	}
}

// The warp is an in-run transfer: the member leaves its own 218 run and enters the
// hatchery, so the squad keeps exactly one gate and its occupancy moves with the
// member. Refusing it with "runtime raid group already occupies a dungeon:
// group=1 map=218" (live 2026-09-17 07:25:12Z, both attempts) is what left the
// hatcheries unreachable.
func TestRuntimeRaidHatcheryWarpMovesTheGroupsGateOccupancy(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		antonEnergyInterceptionMapID: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: antonEnergyInterceptionMapID,
			State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
		},
		222: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: 222,
			State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
		},
	}
	energy, err := service.beginRuntimeRaidDungeon(1001, antonEnergyInterceptionMapID, 12)
	if err != nil {
		t.Fatalf("energy interception entry: %v", err)
	}
	if energy.MapID != antonEnergyInterceptionMapID {
		t.Fatalf("energy interception binding=%+v", energy)
	}

	// The same member warps into the hatchery. One squad, one gate.
	if _, err := service.beginRuntimeRaidDungeon(1001, 222, 13); err != nil {
		t.Fatalf("hatchery warp was refused: %v", err)
	}
	if len(state.Lifecycle.Dungeons) != 1 {
		t.Fatalf("the warp left %d runs for one squad: %+v", len(state.Lifecycle.Dungeons), state.Lifecycle.Dungeons)
	}
	if _, exists := state.Lifecycle.Dungeons[runtimeRaidDungeonRunKey{MapID: 222, Seat: runtimeRaidDungeonSeat{Group: 1}}]; !exists {
		t.Fatalf("the warp did not register the hatchery run: %+v", state.Lifecycle.Dungeons)
	}
	occupied, exists := state.Lifecycle.GroupOccupancy[runtimeRaidDungeonSeat{Group: 1}]
	if !exists || occupied.MapID != 222 {
		t.Fatalf("the squad's occupancy did not move to the hatchery: %+v", state.Lifecycle.GroupOccupancy)
	}

	// A squadmate that never opened 218 cannot move the squad's occupancy with the
	// warp: the second member of the squad re-opening 218 stays refused.
	if _, err := service.beginRuntimeRaidDungeon(1002, 222, 14); err != nil {
		t.Fatalf("the squad's second member was refused the occupied hatchery: %v", err)
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, antonEnergyInterceptionMapID, 15); err == nil {
		t.Fatal("the squad held two gates at once after the warp")
	}
}

// A run lives inside its owner's session, so a client that froze inside a gate
// never reports anything again and its record keeps the squad's occupancy forever.
// Live 2026-09-17: character 1 loaded 218 at 07:24:57Z, its client froze at
// 07:25:12Z, and the raid's phase-two movie could not open while character 22
// cleared the heart (220) five times.
func TestRuntimeRaidStaleGateOccupancyIsReleasedForASessionWithoutItsRun(t *testing.T) {
	service, raidKey := newRuntimeRaidLifecycleService(time.Now().UTC())
	state := service.raids[raidKey]
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	state.Lifecycle.Objectives = map[int]runtimeRaidDungeonObjective{
		antonEnergyInterceptionMapID: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: antonEnergyInterceptionMapID,
			State: raid.AntonDungeonOpen, MaxHP: 1, RemainingHP: 1,
		},
		antonHeartMapID: {
			Phase: runtimeRaidDungeonPhaseTwo, MapID: antonHeartMapID,
			State: raid.AntonDungeonCleared, MaxHP: 1, RemainingHP: 0,
		},
	}
	if _, err := service.beginRuntimeRaidDungeon(1001, antonEnergyInterceptionMapID, 12); err != nil {
		t.Fatalf("energy interception entry: %v", err)
	}
	if len(state.Lifecycle.GroupOccupancy) != 1 {
		t.Fatalf("run was not registered: %+v", state.Lifecycle.GroupOccupancy)
	}
	// The phase is over, but the frozen client still holds its gate.
	if _, err := service.openRuntimeRaidPhaseMovie(raidKey, runtimeRaidDungeonPhaseTwo, time.Now().UTC()); err == nil {
		t.Fatal("the movie opened while a gate was still occupied")
	}

	if released := service.releaseRuntimeRaidStaleGateOccupancy(raidKey); released != 1 {
		t.Fatalf("released %d stale gates, want the one orphaned run", released)
	}
	if len(state.Lifecycle.Dungeons) != 0 || len(state.Lifecycle.GroupOccupancy) != 0 {
		t.Fatalf("the orphaned run kept its gate: dungeons=%+v occupancy=%+v",
			state.Lifecycle.Dungeons, state.Lifecycle.GroupOccupancy)
	}
	// Releasing the orphaned run is what lets the finished phase close.
	if _, err := service.openRuntimeRaidPhaseMovie(raidKey, runtimeRaidDungeonPhaseTwo, time.Now().UTC()); err != nil {
		t.Fatalf("movie after the gate was released: %v", err)
	}
}

// The per-squad split belongs to the reveal board only. The whole-raid team row
// (resultType 3, one row per member, no PartyIndex filter -- 86JP
// RaidHandler.Rewards.cs:599-622) is what the 20-cell attack-party board is drawn
// from, and splitting it removed every other squad's cells from that board.
func TestRuntimeRaidRewardSquadBodiesNeverSplitsTheTeamRow(t *testing.T) {
	delivery := runtimeRaidSettlementRewardDelivery{
		Raid: runtimeRaidState{FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1},
			{CharID: 1002, GroupIndex: 2},
		}},
		Rewards: raid.RaidSettlementRewards{
			ResultType: runtimeRaidRewardResultType,
			Members: []raid.RaidSettlementMemberRewards{
				{MemberKey: 1001, CardIndex: 2, Rewards: []raid.RaidSettlementReward{
					{ItemIDOrValue: 150001, CountOrAux: 5},
					{ItemIDOrValue: 150002, CountOrAux: 1},
				}},
				{MemberKey: 1002, CardIndex: 3, Rewards: []raid.RaidSettlementReward{
					{ItemIDOrValue: 150001, CountOrAux: 5},
					{ItemIDOrValue: 150003, CountOrAux: 1},
				}},
			},
		},
	}
	revealBody, err := raid.BuildRaidSettlementRewardsBody(delivery.Rewards)
	if err != nil {
		t.Fatalf("build reveal body: %v", err)
	}
	squadBodies, err := runtimeRaidRewardSquadBodies(delivery, revealBody)
	if err != nil {
		t.Fatalf("split reveal body: %v", err)
	}
	if len(squadBodies) != 2 {
		t.Fatalf("the two squads produced %d reveal bodies: %+v", len(squadBodies), squadBodies)
	}

	teamBody, ok, err := runtimeRaidTeamSquadRewardBody(delivery)
	if err != nil || !ok {
		t.Fatalf("team reward body ok=%t err=%v", ok, err)
	}
	if len(teamBody) == 0 || teamBody[0] != runtimeRaidTeamRewardResultType {
		t.Fatalf("team reward body=% X want resultType %d", teamBody, runtimeRaidTeamRewardResultType)
	}
	// The whole-raid row keeps every member's cell.
	if int(teamBody[1]) != len(delivery.Rewards.Members) {
		t.Fatalf("team reward body=% X want one row per settled member", teamBody)
	}
	if bodies, err := runtimeRaidRewardSquadBodies(delivery, teamBody); err != nil || bodies != nil {
		t.Fatalf("the team row was split into %+v (err=%v)", bodies, err)
	}
}

// A settlement fought in one squad still receives the whole-raid team row. The
// reveal body stays byte-for-byte identical (one squad is already its own body),
// but the attack-party board is not optional: 86JP raises it for whatever roster
// the phase settled (RaidHandler.Rewards.cs:599-622), and live 2026-09-17T12:46Z
// the phase-two settlement of raid 1507329 -- two members that had merged onto the
// default squad -- arrived without it, which is the settlement whose card flip
// came up with an empty 团队展示 stage.
func TestRuntimeRaidTeamRewardBodyIsRaisedForOneSquad(t *testing.T) {
	delivery := runtimeRaidSettlementRewardDelivery{
		Raid: runtimeRaidState{FrozenMembers: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1},
			{CharID: 1002, GroupIndex: 1},
		}},
		Rewards: raid.RaidSettlementRewards{
			ResultType: runtimeRaidRewardResultType,
			Members: []raid.RaidSettlementMemberRewards{
				{MemberKey: 1001, CardIndex: 2, Rewards: []raid.RaidSettlementReward{{ItemIDOrValue: 150001, CountOrAux: 5}}},
				{MemberKey: 1002, CardIndex: 3, Rewards: []raid.RaidSettlementReward{{ItemIDOrValue: 150001, CountOrAux: 5}}},
			},
		},
	}
	revealBody, err := raid.BuildRaidSettlementRewardsBody(delivery.Rewards)
	if err != nil {
		t.Fatalf("build reveal body: %v", err)
	}
	if bodies, err := runtimeRaidRewardSquadBodies(delivery, revealBody); err != nil || bodies != nil {
		t.Fatalf("the one-squad reveal body was rewritten: %+v (err=%v)", bodies, err)
	}
	body, ok, err := runtimeRaidTeamSquadRewardBody(delivery)
	if err != nil || !ok {
		t.Fatalf("a one-squad settlement produced no team row ok=%t err=%v", ok, err)
	}
	if len(body) < 2 || body[0] != runtimeRaidTeamRewardResultType || int(body[1]) != len(delivery.Rewards.Members) {
		t.Fatalf("team row=% X want resultType %d and one row per settled member", body, runtimeRaidTeamRewardResultType)
	}
}

// The clear banner is a property of the raid, not of how many times its completion
// was observed. 86JP raises it once per phase; live 2026-09-17 07:26:44Z pushed
// msg 0x0272 to both members twice, 92ms apart, because the phase stayed active
// (its movie could not open) and every further clear re-entered the announcement.
func TestRuntimeRaidClearAnnouncementFiresOnceWhileThePhaseStaysActive(t *testing.T) {
	const raidKey = uint32(23)<<16 | 1001
	channel := channelcatalog.Channel{ID: 23, Type: 23}
	conn := &bufferConn{}
	leader := &gameSession{conn: conn, channel: channel}
	service := &Service{
		gameSessions:  map[uint16]*gameSession{1001: leader},
		onlinePlayers: newOnlinePlayerManager(),
	}
	service.bindGameSessionCharacter(leader, 1001)
	state := &runtimeRaidState{
		RaidKey:   raidKey,
		LeaderID:  1001,
		NameBytes: []byte("我的攻坚队"),
		Members: []runtimeRaidMemberState{
			{CharID: 1001, Name: "团长", GroupIndex: 1},
		},
		Lifecycle: newRuntimeRaidLifecycleState(time.Now().UTC()),
	}
	state.Lifecycle.Status = runtimeRaidLifecyclePhaseActive
	state.Lifecycle.Phase = runtimeRaidDungeonPhaseTwo
	service.raids = map[uint32]*runtimeRaidState{raidKey: state}

	service.announceRuntimeRaidClearIfNeeded(*state, runtimeRaidDungeonPhaseTwo)
	first, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if first.Header.MsgID != raid.RaidClearAnnouncementMsgID || len(rest) != 0 {
		t.Fatalf("first announcement packet=%+v trailing=%d", first.Header, len(rest))
	}
	conn.write.Reset()

	// The phase is still active, so the announcement is re-entered with the same
	// state; the raid's own claim is what stops the second banner.
	service.announceRuntimeRaidClearIfNeeded(*state, runtimeRaidDungeonPhaseTwo)
	if conn.write.Len() != 0 {
		t.Fatal("the clear banner was announced twice for one phase")
	}
}
