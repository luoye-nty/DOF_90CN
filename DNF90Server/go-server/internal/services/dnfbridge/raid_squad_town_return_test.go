package dnfbridge

import (
	"bytes"
	"context"
	"testing"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 现场 2026-09-26 22:05:18-22:07:26（+08:00，raid 1507329，party 5 = [1,2,20,21] 全在 1 队）：
//
//	22:05:18  char 20 在 219 里按返回城镇（current_exe_bodyless_op42_active_run_giveup）
//	          → game-party-dungeon-member-town-return-detached（退队 ✓，编制没动 ✗）
//	22:05:33  char 21 同样
//	22:05:36  char 2 同样 → party 5 掉到 1 人解散
//	22:07:26  队长在城镇 op14 踢掉 char 20
//	          → game-raid-squad-unassigned-on-party-leave previous_group=1
//	          ← 20 自己回城之后过了 1 分 53 秒，编制仍然挂在 1 队
//
// 服主定的规则：自己单独返回城镇 = 退队 = 同时退出编制（队长也一样，"按现有逻辑 队长顺延"），
// 但全队一起回城（战况表带队返回 / 阶段结束全队返回 / 通关后返回城镇）绝对不能动编制。

// newRaidSquadTownReturnRouteFixture 用真实的 op132 回城路线造出同形状态：
//
//   - 99 的 session 持有真正的 worldmap 副本 run（也就是"自己返回城镇"的那个人；
//     返回城镇必须由 run 的属主发出，别人的 session 拿不到自己的 town transition），
//   - 100 / 101 与 99 组成一支普通队伍（partyLeaderID 决定谁是队长），
//   - 三人先在同一支运行中的攻坚队里被普通组队同步进 1 队（真实路径：编制来自组队），
//   - 三人共享同一个 party dungeon owner —— 现场那几发 detach 就是这条路。
func newRaidSquadTownReturnRouteFixture(
	t *testing.T,
	partyLeaderID uint16,
) (*Service, map[uint16]*gameSession, uint32) {
	t.Helper()
	service, runOwner, runOwnerRuntime := newBackToVillageRuntime(t)
	service.onlinePlayers = newOnlinePlayerManager()
	service.bindGameSessionCharacter(runOwner, 99)

	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	sessions := map[uint16]*gameSession{99: runOwner}
	for _, characterID := range []uint16{100, 101} {
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: characterIDString(characterID),
			AccountID:   "account-1",
			Name:        "member",
			Job:         "0",
			Level:       20,
			Stats: map[string]int64{
				"fatigue": 100, "town_id": 7, "area_id": 3,
				"pos_x": 500, "pos_y": 234, "direction": 5, "area_state": 3,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterIDString(characterID),
			Slots:       map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
		session := &gameSession{
			conn:                &bufferConn{},
			connID:              "raid-squad-return-" + characterIDString(characterID),
			channel:             runOwner.channel,
			residentChannel:     runOwner.residentChannel,
			selectedCharacterID: characterID,
		}
		service.bindGameSessionCharacter(session, characterID)
		sessions[characterID] = session
	}

	// 攻坚队先存在，三人都还没编制；随后真实组队会把三人同步进 1 队，和现场一样
	// （packet_log: game-raid-squad-synced-from-ordinary-party squad=1 members=[1 2 20 21]）。
	raidKey := uint32(0x00c80600)
	members := make([]runtimeRaidMemberState, 0, 3)
	for _, characterID := range []uint16{99, 100, 101} {
		members = append(members, runtimeRaidMemberState{
			CharID: characterID, GroupIndex: 0, SlotOrder: 0,
			UserState: runtimeRaidMemberUserStateOnline, HPPercent: 100, MPPercent: 100,
		})
	}
	service.raidMu.Lock()
	service.raids = map[uint32]*runtimeRaidState{
		raidKey: {
			RaidKey:       raidKey,
			LeaderID:      99,
			TailFlag:      5,
			NameBytes:     []byte("anton"),
			Started:       true,
			Members:       append([]runtimeRaidMemberState(nil), members...),
			FrozenMembers: append([]runtimeRaidMemberState(nil), members...),
			Lifecycle: runtimeRaidLifecycleState{
				Status: runtimeRaidLifecyclePhaseActive,
				Phase:  runtimeRaidDungeonPhaseTwo,
			},
		},
	}
	service.raidMu.Unlock()

	anchor := sessions[partyLeaderID]
	for _, joinerID := range []uint16{99, 100, 101} {
		if joinerID == partyLeaderID {
			continue
		}
		if _, result, joined := service.createOrJoinManagedRuntimeParty(sessions[joinerID], anchor); !joined || !result.OK {
			t.Fatalf("party join %d -> %d joined=%t result=%+v", joinerID, partyLeaderID, joined, result)
		}
	}
	state := runtimePartyStateSnapshot(anchor)
	if len(runtimePartyMembers(state)) != 3 {
		t.Fatalf("fixture party = %+v, want 3 members", state)
	}
	raidState := raidStateForTest(t, service, raidKey)
	for _, characterID := range []uint16{99, 100, 101} {
		if liveGroup, _ := raidSquadRowsForTest(t, raidState, characterID); liveGroup != 1 {
			t.Fatalf("the ordinary party did not project char %d into squad 1: %d", characterID, liveGroup)
		}
	}
	snapshot, found := service.runtimePartyManagerForService().SnapshotByID(uint16(state.PartyID))
	if !found {
		t.Fatal("managed party snapshot unavailable")
	}

	owner, err := newRuntimePartyDungeonOwner(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	runOwnerRuntime.partyDungeonOwner = owner
	runOwnerRuntime.partyMemberIndexed = true
	runOwnerRuntime.partyMemberIndex = 1
	for slot, characterID := range []uint16{99, 100, 101} {
		if characterID == 99 {
			continue
		}
		sessions[characterID].dungeon.runtime = &runtimeDungeonState{
			Session:            runOwnerRuntime.Session,
			partyDungeonOwner:  owner,
			partyMemberIndexed: true,
			partyMemberIndex:   byte(slot),
			townReturnOrigin: currentTownPositionSnapshot{
				CharacterID: characterID, TownID: 7, AreaID: 3,
				PositionX: 500, PositionY: 234, MovementCode: 5, PositionValid: true,
			},
		}
	}
	return service, sessions, raidKey
}

func characterIDString(characterID uint16) string {
	return string([]byte{byte('0' + characterID/100), byte('0' + (characterID/10)%10), byte('0' + characterID%10)})
}

// pressBackToVillage 走真实入口：客户端 bodyless op132（返回城镇）。
func pressBackToVillage(t *testing.T, service *Service, session *gameSession) {
	t.Helper()
	request, err := dnfproto.BuildChannelPacket(
		uint16(dnfenum.CmdPacketBack2Village), nil, 0, dnfproto.DefaultChannelClassification,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameUpper(session, request); err != nil {
		t.Fatalf("op132 from char %d: %v", session.selectedCharacterID, err)
	}
}

func raidStateForTest(t *testing.T, service *Service, raidKey uint32) runtimeRaidState {
	t.Helper()
	service.raidMu.Lock()
	defer service.raidMu.Unlock()
	state := service.raids[raidKey]
	if state == nil {
		t.Fatalf("raid %d is gone", raidKey)
	}
	return cloneRuntimeRaidState(*state)
}

// raidSquadRowsForTest 返回同一个人的两行编制：实时名单（客户端画编制的那份）与冻结名单
// （阶段通关代记 / 战况表 / 结算名单的那份）。
func raidSquadRowsForTest(t *testing.T, state runtimeRaidState, characterID uint16) (byte, byte) {
	t.Helper()
	index := runtimeRaidMemberIndex(state, characterID)
	if index < 0 {
		t.Fatalf("char %d is missing from the live roster: %+v", characterID, state.Members)
	}
	frozen, ok := runtimeRaidFrozenMember(state, characterID)
	if !ok {
		t.Fatalf("char %d is missing from the frozen roster: %+v", characterID, state.FrozenMembers)
	}
	return state.Members[index].GroupIndex, frozen.GroupIndex
}

// 自己单独返回城镇 → 编制清掉；队友的编制不动；冻结名单不动。
func TestRaidSquadLabelFollowsASoloTownReturn(t *testing.T) {
	for _, test := range []struct {
		name          string
		partyLeaderID uint16
		survivor      uint16
	}{
		// 现场那一次：char 20 是队员，队长 char 1 留在副本里。
		{name: "a party member returns alone", partyLeaderID: 100, survivor: 100},
		// 服主明确要求的方向：队长自己单独返回也算退队，队长顺延给队友。
		{name: "the party leader returns alone", partyLeaderID: 99, survivor: 100},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, sessions, raidKey := newRaidSquadTownReturnRouteFixture(t, test.partyLeaderID)
			returning := sessions[99]
			survivor := sessions[test.survivor]
			survivor.conn.(*bufferConn).write.Reset()

			pressBackToVillage(t, service, returning)

			// 退队本身是既有行为（服主："退队没问题"），先确认它没被影响。
			if got := runtimePartyStateSnapshot(returning); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
				t.Fatalf("returning member kept its party projection: %+v", got)
			}
			// 队长自己单独返回 → 按现有逻辑顺延给队友（3 人队剩下 2 人，队伍还在）。
			survivorState := runtimePartyStateSnapshot(survivor)
			if survivorState.PartyID <= 0 || survivorState.UserID != test.survivor || !survivorState.IsLeader {
				t.Fatalf("party leadership did not end up with %d: %+v", test.survivor, survivorState)
			}

			state := raidStateForTest(t, service, raidKey)
			liveGroup, frozenGroup := raidSquadRowsForTest(t, state, 99)
			if liveGroup != 0 {
				t.Fatalf("char 99 is still drawn in squad %d: a solo town return is already a party "+
					"leave, so the 编制 has to follow it", liveGroup)
			}
			if frozenGroup != 1 {
				t.Fatalf("char 99 left the frozen roster (squad %d): that roster owns the phase-clear "+
					"credit and the settlement board, so dropping the member from it silently takes the "+
					"phase reward away", frozenGroup)
			}
			for _, survivorID := range []uint16{100, 101} {
				live, frozen := raidSquadRowsForTest(t, state, survivorID)
				if live != 1 || frozen != 1 {
					t.Fatalf("survivor %d lost its squad: live=%d frozen=%d", survivorID, live, frozen)
				}
			}

			// 客户端那一位"我是不是队长"（wire field 66）必须跟着小队走：回城的人不再是
			// 任何小队的队长，被顺延的 100 必须是。现场（raid 1507329，15:03:14.782）这里
			// 是反的：回城的团长 LEADER=1、被顺延的人 LEADER=0，于是被顺延的人没有队长标志、
			// 战况表也没有按钮（进入地下城/回到城镇），而回城的人两样都还留着。
			recordFlag := func(characterID uint16) uint32 {
				index := runtimeRaidMemberIndex(state, characterID)
				if index < 0 {
					t.Fatalf("char %d missing from the roster", characterID)
				}
				return runtimeRaidMemberRecord(state, state.Members[index]).Field66BoolSource
			}
			if got := recordFlag(100); got != 1 {
				t.Fatalf("the promoted squad leader's 队长标志 = %d, want 1", got)
			}
			if got := recordFlag(99); got != 0 {
				t.Fatalf("the returned leader still carries the 队长标志 (flag=%d)", got)
			}
			if got := recordFlag(101); got != 0 {
				t.Fatalf("a plain squad member carries the 队长标志 (char 101 flag=%d)", got)
			}

			// 只有推送能让客户端真的重画那一行。
			want, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
			if err != nil {
				t.Fatal(err)
			}
			sent := false
			for _, packet := range runtimeRaidPeerTake(t, survivor) {
				if packet.Header.MsgID == raid.RaidMemberRefreshMsgID && bytes.Equal(packet.Body, want) {
					sent = true
				}
			}
			if !sent {
				t.Fatal("no raid roster refresh carrying the cleared 编制 reached the survivor")
			}
		})
	}
}

// 全队一起回城（runtime.townReturnKeepParty：战况表带队返回 / 阶段结束全队返回 / 通关后返回城镇）
// 必须一个编制都不动 —— 服主划的红线："全队一起回城绝对不要清"。
func TestRaidSquadLabelSurvivesAWholePartyTownReturn(t *testing.T) {
	service, sessions, raidKey := newRaidSquadTownReturnRouteFixture(t, 99)
	returning := sessions[99]
	returning.dungeon.mu.Lock()
	returning.dungeon.runtime.townReturnKeepParty = true
	returning.dungeon.mu.Unlock()

	pressBackToVillage(t, service, returning)

	// 整队保留：队伍还在、三人都在队里（party_return_keeps_squad_test.go 钉住的是
	// 同一个标记的另一半，这里确认编制也没被误清）。
	if got := runtimePartyStateSnapshot(returning); got.PartyID <= 0 || len(runtimePartyMembers(got)) != 3 {
		t.Fatalf("a whole-party return dissolved the party: %+v", got)
	}
	state := raidStateForTest(t, service, raidKey)
	for _, characterID := range []uint16{99, 100, 101} {
		liveGroup, frozenGroup := raidSquadRowsForTest(t, state, characterID)
		if liveGroup != 1 || frozenGroup != 1 {
			t.Fatalf("char %d lost its 编制 on a whole-party return: live=%d frozen=%d",
				characterID, liveGroup, frozenGroup)
		}
	}
}

// 不在攻坚队里（数据缺失）时返回城镇照旧 —— 红线 3：缺数据 = 完全等于现在的行为。
func TestSoloTownReturnWithoutARaidIsUnchanged(t *testing.T) {
	service, sessions, _ := newRaidSquadTownReturnRouteFixture(t, 99)
	service.raidMu.Lock()
	service.raids = nil
	service.raidMu.Unlock()

	pressBackToVillage(t, service, sessions[99])

	if got := runtimePartyStateSnapshot(sessions[99]); got.PartyID != 0 || len(runtimePartyMembers(got)) != 0 {
		t.Fatalf("leader kept its party projection: %+v", got)
	}
	if got := runtimePartyStateSnapshot(sessions[100]); got.PartyID <= 0 || got.UserID != 100 || !got.IsLeader {
		t.Fatalf("survivor party = %+v, want the surviving party led by 100", got)
	}
	if len(service.raids) != 0 {
		t.Fatalf("a raid appeared out of nowhere: %+v", service.raids)
	}
}
