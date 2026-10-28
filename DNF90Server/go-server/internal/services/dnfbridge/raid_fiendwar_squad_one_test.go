package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

// 超时空的小队编制：**8 个编制、每队只能 1 人**，而且把 B 拖进已经有 A 的队时
// **A 被挤成单人**（不是整脚拒绝）。
//
// 操作员 2026-10-02 的定案：
//
//	「超时空总共有8个编制 跟其他攻坚战不一样的是 每个小队编制仅可放1人
//	  最有力的证明是 小队编制界面 每个小队后面三格都是锁住的
//	  就比如说 团长给A放在2队 然后再给B放在2队 A就应该变单人了
//	  一个小队仅能有1人（所以进图就是我们做的同区域的人一起进图 仅超时空这样）」
//
// 安东/卢克一个字不改：满队仍然是整脚拒绝（TestRuntimeRaidManagerMoveRejectsFullSubgroup
// 钉着那一半）。
func TestFiendWarSquadHoldsExactlyOneMemberAndEvictsThePrevious(t *testing.T) {
	const raidKey = uint32(0x00c80410)
	channel := channelcatalog.Channel{ID: 184, Type: 42}
	service := &Service{raids: make(map[uint32]*runtimeRaidState), gameSessions: map[uint16]*gameSession{}}
	members := []runtimeRaidMemberState{
		{CharID: 1001, Name: "A", GroupIndex: 2, SlotOrder: 0},
		{CharID: 1002, Name: "B", GroupIndex: 0, SlotOrder: 0},
		{CharID: 1003, Name: "C", GroupIndex: 1, SlotOrder: 0},
	}
	state := &runtimeRaidState{
		RaidKey:         raidKey,
		LeaderID:        1001,
		RaidMode:        runtimeRaidOwnerFiendWar,
		RouteOrRaidType: 2,
		TailFlag:        0,
		Members:         append([]runtimeRaidMemberState(nil), members...),
		FrozenMembers:   append([]runtimeRaidMemberState(nil), members...),
	}
	service.raids[raidKey] = state
	leader := &gameSession{channel: channel, conn: &bufferConn{}}
	service.bindGameSessionCharacter(leader, 1001)

	// 尺子：超时空 8 个编制、每队 1 人。
	if got := runtimeRaidGroupSizeFor(*state); got != 1 {
		t.Fatalf("超时空每队容量 = %d，want 1", got)
	}
	if got := runtimeRaidGroupMaxFor(*state); got != currentFiendWarSquadCount {
		t.Fatalf("超时空编制数 = %d，want %d", got, currentFiendWarSquadCount)
	}
	// 同一个团的安东口径没有被带偏。
	anton := *state
	anton.RaidMode = runtimeRaidOwnerAnton
	if got := runtimeRaidGroupSizeFor(anton); got != runtimeRaidGroupSize {
		t.Fatalf("安东每队容量被改了：%d，want %d", got, runtimeRaidGroupSize)
	}

	// [本用例的核心] 把 B 拖进 A 所在的 2 队：B 进 2 队，A 被挤成单人。
	moved, ok := service.moveRuntimeRaidMember(1001, 1002, 2)
	if !ok {
		t.Fatal("把 B 放进已经有人的 2 队被拒了 —— 超时空应当挤人，不是拒绝")
	}
	bIndex := runtimeRaidMemberIndex(moved, 1002)
	aIndex := runtimeRaidMemberIndex(moved, 1001)
	if bIndex < 0 || moved.Members[bIndex].GroupIndex != 2 {
		t.Fatalf("B 没有落到 2 队：%+v", moved.Members[bIndex])
	}
	if aIndex < 0 || moved.Members[aIndex].GroupIndex != 0 || moved.Members[aIndex].SlotOrder != 0 {
		t.Fatalf("A 没有被挤成单人（应回到未编队）：%+v", moved.Members[aIndex])
	}
	// 冻结名册（进图名单/清数记账/战况表都读它）必须一起改。
	frozenA, okA := runtimeRaidFrozenMember(moved, 1001)
	if !okA || frozenA.GroupIndex != 0 {
		t.Fatalf("冻结名册里的 A 还挂在 2 队：%+v", frozenA)
	}
	// 一个编制里只能有一个人。
	if got := runtimeRaidGroupCountExcluding(moved, 2, 0); got != 1 {
		t.Fatalf("2 队里有 %d 个人，want 1", got)
	}

	// 9 队不是超时空的编制：越界的拖拽必须被拒（原来 9/10 是给 40 人格子留的）。
	if _, ok := service.moveRuntimeRaidMember(1001, 1002, currentFiendWarSquadCount+1); ok {
		t.Fatalf("超时空接受了第 %d 个编制", currentFiendWarSquadCount+1)
	}
	// 第 8 个编制仍然可用。
	if _, ok := service.moveRuntimeRaidMember(1001, 1002, currentFiendWarSquadCount); !ok {
		t.Fatalf("超时空拒绝了最后一个编制 %d", currentFiendWarSquadCount)
	}
	// 拉回未编队照旧。
	if _, ok := service.moveRuntimeRaidMember(1001, 1002, 0); !ok {
		t.Fatal("把成员拉回未编队被拒了")
	}
}

// 超时空里两个人在镇上组了普通队伍：**绝不能**被塞进同一个编制。
//
// 注意这条路在超时空本来就是关着的（runtimeRaidSquadsFollowOrdinaryParty 对
// Fiend War 直接返回 false，理由是操作员那句「组个临时队伍 但是攻坚小队编制不变」）。
// 这条用例是保险：万一以后有别的调用点来问"这支队伍该算几队"，
// 超时空的答案必须是"一队一人、装不下"，而不是像安东/卢克那样并成一队。
func TestFiendWarOrdinaryPartyTakesOneSquadPerMember(t *testing.T) {
	state := runtimeRaidState{
		RaidKey:  0x00c80411,
		RaidMode: runtimeRaidOwnerFiendWar,
		Members: []runtimeRaidMemberState{
			{CharID: 1001, GroupIndex: 1, SlotOrder: 0},
			{CharID: 1002, GroupIndex: 0, SlotOrder: 0},
		},
	}
	// 两个人一队：超时空每队 1 人，装不下 ⇒ 不给小队号（调用方保持原样）。
	if group, ok := runtimeRaidSquadForOrdinaryPartyLedBy(state, []uint16{1001, 1002}, 1001); ok {
		t.Fatalf("两个人的普通队伍在超时空拿到了同一编制 %d（每队只该有 1 人）", group)
	}
	// 单人队伍照旧：队长保持自己的号。
	if group, ok := runtimeRaidSquadForOrdinaryPartyLedBy(state, []uint16{1001}, 1001); !ok || group != 1 {
		t.Fatalf("单人队伍拿到编制 %d/%t，want 1/true", group, ok)
	}
	// 同一个团的安东口径不动：4 人一队仍然装得下。
	anton := state
	anton.RaidMode = runtimeRaidOwnerAnton
	if group, ok := runtimeRaidSquadForOrdinaryPartyLedBy(anton, []uint16{1001, 1002}, 1001); !ok || group != 1 {
		t.Fatalf("安东的两人队伍被拆了：%d/%t", group, ok)
	}
}

// 自动落位（第一个空位）也要按团的尺子走：超时空第 1 个人占 1 队 0 号位，
// 第 2 个人必须去 2 队，而不是坐进 1 队 1 号位。
func TestFiendWarFirstAvailablePositionUsesOneSlotSquads(t *testing.T) {
	state := runtimeRaidState{
		RaidMode: runtimeRaidOwnerFiendWar,
		Members:  []runtimeRaidMemberState{{CharID: 1001, GroupIndex: 1, SlotOrder: 0}},
	}
	group, slot, ok := runtimeRaidFirstAvailablePosition(state)
	if !ok || group != 2 || slot != 0 {
		t.Fatalf("超时空下一个空位 = %d/%d/%t，want 2/0/true", group, slot, ok)
	}
	anton := state
	anton.RaidMode = runtimeRaidOwnerAnton
	group, slot, ok = runtimeRaidFirstAvailablePosition(anton)
	if !ok || group != 1 || slot != 1 {
		t.Fatalf("安东下一个空位 = %d/%d/%t，want 1/1/true", group, slot, ok)
	}
}
