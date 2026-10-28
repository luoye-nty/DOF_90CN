package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

// 现场 2026-09-26（raid 1507329）：名册里那一位（wire field 66）过去填的是"你是不是团长"
// （state.LeaderID），所以团长自己单独回城、队长顺延给队友之后，客户端仍然把团长画成队长：
// 被顺延的人没有队长标志、战况表没有按钮，回城的人两样都还在（packet_log 15:03:14.782
// 解出来的名册：char 1 group 0 LEADER=1；char 20 group 1 slot 1 LEADER=0）。
//
// 客户端侧证据（tools/ida_work/batch3.txt + 本机反汇编）：sub_17B6EF0 把该 u32 读成
// `value == 1` 的布尔存到记录 +0x42；客户端问"我是不是队长"就是拿自己的记录读这一位
// （0x51b2748 → 0x593cf0 取自己的 key → 0x13b29c0 取自己的记录 → cmp byte ptr [rec+42h],0）。
func TestRuntimeRaidMemberLeadsSquadRule(t *testing.T) {
	started := func(members []runtimeRaidMemberState, leaders map[byte]uint16) runtimeRaidState {
		return runtimeRaidState{LeaderID: 99, Started: true, Members: members, SquadPartyLeaders: leaders}
	}
	members := []runtimeRaidMemberState{
		{CharID: 20, GroupIndex: 1, SlotOrder: 1},
		{CharID: 22, GroupIndex: 1, SlotOrder: 3},
		{CharID: 30, GroupIndex: 2, SlotOrder: 2},
		{CharID: 99, GroupIndex: 0, SlotOrder: 0},
	}
	flag := func(state runtimeRaidState, charID uint16) bool {
		index := runtimeRaidMemberIndex(state, charID)
		if index < 0 {
			t.Fatalf("char %d missing", charID)
		}
		return runtimeRaidMemberLeadsSquad(state, state.Members[index])
	}

	// 开团前：没有小队、也没有普通队可领，"是团长"就是旧行为，一个字不改。
	beforeStart := runtimeRaidState{LeaderID: 99, Members: members}
	if !flag(beforeStart, 99) || flag(beforeStart, 20) {
		t.Fatal("before the raid starts the flag must stay \"is the 团长\"")
	}
	// 单人攻坚：没有小队（applyRuntimeRaidParties 直接返回），同样维持旧行为。
	single := runtimeRaidState{LeaderID: 99, Started: true, TailFlag: 1, Members: members}
	if !flag(single, 99) || flag(single, 20) {
		t.Fatal("a single raid keeps \"is the 团长\"")
	}

	// 运行中的多人团：1 队的队长是普通队队长（20），2 队没有记录 → 落到最低槽位（30）。
	state := started(members, map[byte]uint16{1: 20})
	if !flag(state, 20) || flag(state, 22) {
		t.Fatal("squad 1 must be led by the ordinary party's leader")
	}
	if !flag(state, 30) {
		t.Fatal("a squad with no recorded leader falls back to its lowest occupied slot")
	}
	if flag(state, 99) {
		t.Fatal("a member in no squad is nobody's squad leader")
	}

	// 记录里的队长已经不在那一队了（比如他自己退了队）：当作没有记录，落回最低槽位。
	stale := started(members, map[byte]uint16{1: 99})
	if !flag(stale, 20) || flag(stale, 99) {
		t.Fatal("a recorded leader outside its squad must not keep the flag")
	}
}

// 普通队伍每次发布都会把队长镜像进它所在的小队：这就是"队长顺延"后客户端立刻拿到新队长的
// 唯一途径（同步那条路在副本里会被跳过，而顺延恰恰发生在副本里）。
func TestRaidSquadLeaderFollowsOrdinaryPartyPublication(t *testing.T) {
	service := &Service{
		options:      options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		gameSessions: make(map[uint16]*gameSession),
		raids:        make(map[uint32]*runtimeRaidState),
	}
	channel := channelcatalog.Channel{ID: 200, Type: 23, Name: "anton", Port: 10200}
	sessions := make(map[uint16]*gameSession, 2)
	for _, characterID := range []uint16{20, 22} {
		session := &gameSession{conn: &bufferConn{}, channel: channel, selectedCharacterID: characterID}
		service.bindGameSessionCharacter(session, characterID)
		sessions[characterID] = session
	}
	raidKey := uint32(0x00c80600)
	service.raidMu.Lock()
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey: raidKey, LeaderID: 20, TailFlag: 5, NameBytes: []byte("anton"),
		Started: true,
		Members: []runtimeRaidMemberState{
			{CharID: 20, GroupIndex: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
			{CharID: 22, GroupIndex: 0, UserState: 1, HPPercent: 100, MPPercent: 100},
		},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive, Phase: runtimeRaidDungeonPhaseOne,
		},
	}
	service.raidMu.Unlock()

	// 22 建队、20 加入：发布时两人被同步进 1 队，队长 22 必须被记到那一队上。
	if _, result, joined := service.createOrJoinManagedRuntimeParty(sessions[20], sessions[22]); !joined || !result.OK {
		t.Fatalf("fixture party result=%+v", result)
	}
	state := raidStateForTest(t, service, raidKey)
	if leader := state.SquadPartyLeaders[1]; leader != 22 {
		t.Fatalf("squad 1 leader = %d, want the party leader 22 (table=%v)", leader, state.SquadPartyLeaders)
	}
	record := func(charID uint16) uint32 {
		index := runtimeRaidMemberIndex(state, charID)
		if index < 0 {
			t.Fatalf("char %d missing from the roster", charID)
		}
		return runtimeRaidMemberRecord(state, state.Members[index]).Field66BoolSource
	}
	if record(22) != 1 || record(20) != 0 {
		t.Fatalf("leader flag = 22:%d 20:%d, want 1/0", record(22), record(20))
	}
}
