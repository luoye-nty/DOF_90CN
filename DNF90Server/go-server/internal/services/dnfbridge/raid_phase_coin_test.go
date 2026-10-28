package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/raid"
)

func lukePhaseCoinTestService(phase byte) (*Service, *runtimeRaidState) {
	state := &runtimeRaidState{
		RaidKey:  0x00200001,
		RaidMode: runtimeRaidOwnerLuke,
		Started:  true,
		Members:  []runtimeRaidMemberState{{CharID: 42}, {CharID: 1}},
		LukeConfig: &raid.LukeConfig{Raid: &raid.AntonConfig{
			// etc/raid/luke/luke.etc: only phase two declares [PHASE COIN LIMIT].
			Phases:              []raid.AntonPhase{{}, {CoinLimit: 20}},
			PhaseTimeoutSeconds: []int{2100, 2100},
		}},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecyclePhaseActive,
			Phase:  phase,
		},
	}
	s := &Service{raids: map[uint32]*runtimeRaidState{state.RaidKey: state}}
	return s, state
}

func TestRuntimeRaidPhaseCoinUsageBodyMatchesClientReader(t *testing.T) {
	// sub_12D02A0 reads u16 member, u32 -> +0xE8, u32 -> +0xE0.
	body := buildRuntimeRaidPhaseCoinUsageBody(42, 7)
	if len(body) != 10 {
		t.Fatalf("body len=%d want 10", len(body))
	}
	if key := binary.LittleEndian.Uint16(body[0:2]); key != 42 {
		t.Fatalf("member key=%d", key)
	}
	if a, b := binary.LittleEndian.Uint32(body[2:6]), binary.LittleEndian.Uint32(body[6:10]); a != 7 || b != 7 {
		t.Fatalf("counters=%d/%d want 7/7", a, b)
	}
}

func TestRuntimeRaidPhaseCoinCapOnlyAppliesToDeclaredPhase(t *testing.T) {
	s, _ := lukePhaseCoinTestService(runtimeRaidDungeonPhaseOne)
	if _, capped := s.runtimeRaidPhaseCoinStatusForMember(42); capped {
		t.Fatal("phase one has no [PHASE COIN LIMIT] and must stay uncapped")
	}
	// No dungeon [coin limit] either: nothing caps the coin.
	if _, capped, allowed := s.commitRuntimeRaidPhaseCoin(42, lukeBirthSanctumMapID, 0); capped || !allowed {
		t.Fatalf("phase one charge capped=%t allowed=%t", capped, allowed)
	}
}

// Luke phase one: each sanctum declares [coin limit] 8, shared by the squad in
// that entry and reset when the entry is abandoned and made again.
func TestRuntimeRaidDungeonCoinLimitIsPerEntryAndResetsOnReentry(t *testing.T) {
	s, state := lukePhaseCoinTestService(runtimeRaidDungeonPhaseOne)
	state.FrozenMembers = []runtimeRaidMemberState{{CharID: 42, GroupIndex: 1}, {CharID: 1, GroupIndex: 1}}
	seat := runtimeRaidDungeonSeat{Group: 1}
	key := runtimeRaidDungeonRunKey{MapID: lukeBirthSanctumMapID, Seat: seat}
	state.Lifecycle.Dungeons = map[runtimeRaidDungeonRunKey]runtimeRaidDungeonProgress{
		key: {Phase: runtimeRaidDungeonPhaseOne, MapID: lukeBirthSanctumMapID, Seat: seat},
	}
	for i := 1; i <= 8; i++ {
		member := uint16(42)
		if i%2 == 0 {
			member = 1
		}
		status, capped, allowed := s.commitRuntimeRaidPhaseCoin(member, lukeBirthSanctumMapID, 8)
		if !capped || !allowed || status.MapUsed != i || status.MapLimit != 8 {
			t.Fatalf("coin %d: status=%+v capped=%t allowed=%t", i, status, capped, allowed)
		}
	}
	status, capped, allowed := s.commitRuntimeRaidPhaseCoin(42, lukeBirthSanctumMapID, 8)
	if !capped || allowed || status.Refused != "map" || status.MapUsed != 8 {
		t.Fatalf("9th coin: status=%+v capped=%t allowed=%t want refused by the map cap", status, capped, allowed)
	}
	if state.Lifecycle.PhaseCoinsUsed != 0 {
		t.Fatalf("an uncapped phase must not count: PhaseCoinsUsed=%d", state.Lifecycle.PhaseCoinsUsed)
	}
	// Retreat: the entry record is dropped; the next entry starts at zero.
	delete(state.Lifecycle.Dungeons, key)
	state.Lifecycle.Dungeons[key] = runtimeRaidDungeonProgress{Phase: runtimeRaidDungeonPhaseOne, MapID: lukeBirthSanctumMapID, Seat: seat}
	if status, _, allowed := s.commitRuntimeRaidPhaseCoin(42, lukeBirthSanctumMapID, 8); !allowed || status.MapUsed != 1 {
		t.Fatalf("after re-entry: status=%+v allowed=%t want a fresh count of 1", status, allowed)
	}
	// A refund gives the coin back to the entry.
	s.refundRuntimeRaidPhaseCoin(runtimeRaidPhaseCoinStatus{RaidKey: state.RaidKey, MapCapped: true, RunKey: key})
	if got := state.Lifecycle.Dungeons[key].CoinsUsed; got != 0 {
		t.Fatalf("after refund entry CoinsUsed=%d want 0", got)
	}
}

// ⚠️ 本树删除了上游的 TestRuntimeRaidMonsterStateRowCarriesPhaseCoinsUsed。
//
// 上游那条断言的是 package 级函数 runtimeRaidMonsterStateGroups(*state) —— 它按
// state.Lifecycle.PhaseCoinsUsed 直接写 0x027A 行的 Value0。本树的战况表行由
// **(s *Service).runtimeRaidMonsterStateRows** 构建（需要 service 去收集每个座位的
// 落位与客户端上报），并且 Value0 已经由我们这条线自己的复活币账本填
// （runtimeRaidReportedUsedCoinCount(used, granted)，见 raid_situation_broadcast.go:169）。
//
// 两边都在同一次花钱里各自 +1，叠加会**重复计数**；而 op775
// （runtimeRaidSquadCoinUsedNotification，下面那条用例仍在测）才是客户端真正读的那一格。
// 因此本树只保留 op775 这条通道，不动用户已经验收过的战况表行为。

func TestRuntimeRaidSquadCoinUsedIsAlwaysDefined(t *testing.T) {
	// op775 (sub_1D575D0) is one u8 written into [0x51B2748]+0x194.
	_, capped := lukePhaseCoinTestService(runtimeRaidDungeonPhaseTwo)
	capped.Lifecycle.PhaseCoinsUsed = 4
	if got := runtimeRaidSquadCoinUsedNotification(*capped); got.msgID != 775 ||
		len(got.body) != 1 || got.body[0] != 4 {
		t.Fatalf("capped phase op775=%+v want msg 775 body [4]", got)
	}
	_, uncapped := lukePhaseCoinTestService(runtimeRaidDungeonPhaseOne)
	uncapped.Lifecycle.PhaseCoinsUsed = 4
	if got := runtimeRaidSquadCoinUsedNotification(*uncapped); len(got.body) != 1 || got.body[0] != 0 {
		t.Fatalf("uncapped phase op775 body=%v want [0]", got.body)
	}
}

func TestRuntimeRaidPhaseCoinCapIsSharedAndRefusesPastTwenty(t *testing.T) {
	s, state := lukePhaseCoinTestService(runtimeRaidDungeonPhaseTwo)
	for i := 1; i <= 20; i++ {
		// Alternate members: the cap is the roster's, not a character's.
		member := uint16(42)
		if i%2 == 0 {
			member = 1
		}
		status, capped, allowed := s.commitRuntimeRaidPhaseCoin(member, lukeEnergySaveMapID, 0)
		if !capped || !allowed || status.Used != i || status.Limit != 20 {
			t.Fatalf("coin %d: status=%+v capped=%t allowed=%t", i, status, capped, allowed)
		}
	}
	status, capped, allowed := s.commitRuntimeRaidPhaseCoin(42, lukeEnergySaveMapID, 0)
	if !capped || allowed || status.Used != 20 || status.Refused != "phase" {
		t.Fatalf("21st coin: status=%+v capped=%t allowed=%t want refused at 20", status, capped, allowed)
	}
	s.refundRuntimeRaidPhaseCoin(runtimeRaidPhaseCoinStatus{RaidKey: state.RaidKey, Limit: 20})
	if state.Lifecycle.PhaseCoinsUsed != 19 {
		t.Fatalf("after refund used=%d want 19", state.Lifecycle.PhaseCoinsUsed)
	}
	if _, capped := s.runtimeRaidPhaseCoinStatusForMember(99); capped {
		t.Fatal("a non-member must not see the raid's cap")
	}
}
