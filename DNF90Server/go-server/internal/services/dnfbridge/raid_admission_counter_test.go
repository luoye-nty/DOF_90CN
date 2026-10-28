package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestRuntimeRaidAdmissionCounterIsSharedByAccount(t *testing.T) {
	service, repositories, catalog, _ := newRuntimeRaidAdmissionCounterTest(t, 2, 1001, 1002)
	for index, characterID := range []uint16{1001, 1002} {
		state := runtimeRaidState{
			RaidKey:      uint32(index + 1),
			StartAttempt: 1,
			FrozenMembers: []runtimeRaidMemberState{{
				CharID: characterID,
			}},
		}
		if batch, err := service.reserveRuntimeRaidAdmissionCounters(
			context.Background(), state, runtimeRaidOwnerAnton, time.Now().UTC()); err != nil || batch == nil {
			t.Fatalf("character=%d batch=%+v err=%v", characterID, batch, err)
		}
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 2)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1002", "raid.anton.account.daily", 2)
}

// 现在一次开团同时收三条 policy（账号日 / 账号周 / 角色周），所以"哪个先到顶"
// 取决于三者中最紧的那个。用真档形状（安徒恩 [DAILY ACCOUNT ENTER LIMIT] 10、
// [WEEKLY ENTER COUNT] 30 3 = 账号周 30 / 角色周 3）时，**同一个角色**最紧的是
// 角色周 3 —— 第 4 次就该被拒，而不是第 11 次。
func TestRuntimeRaidAdmissionCounterRejectsTheFourthCharacterWeeklyEntry(t *testing.T) {
	service, repositories, catalog, _ := newRuntimeRaidAdmissionCounterTest(t, 1, 1001)
	enter := func(raidKey uint32) error {
		t.Helper()
		state := runtimeRaidState{
			RaidKey:       raidKey,
			StartAttempt:  1,
			FrozenMembers: []runtimeRaidMemberState{{CharID: 1001}},
		}
		_, err := service.reserveRuntimeRaidAdmissionCounters(
			context.Background(), state, runtimeRaidOwnerAnton, time.Now().UTC())
		return err
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := enter(uint32(attempt)); err != nil {
			t.Fatalf("attempt=%d should be allowed (character weekly limit is 3): %v", attempt, err)
		}
	}
	err := enter(4)
	if !errors.Is(err, dnfdungeonadmission.ErrLimitReached) {
		t.Fatalf("fourth entry err=%v want ErrLimitReached", err)
	}
	if !strings.Contains(err.Error(), "raid.anton.character.weekly") {
		t.Fatalf("fourth entry was refused by the wrong policy: %v", err)
	}
	// 被拒的那次不能把计数往前推，而且账号日/账号周都只走到 3 —— 这正是
	// "角色周先绑定" 的证据。角色周自己无法用这个 helper 读：它此刻正好卡在
	// 3/3，而 Owner.Preflight 对已用尽的 policy 是**拒绝**（LimitError）而不是
	// 回报快照 —— 上面那条 err 里已经印出了 used=3 limit=3。
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 3)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.weekly", 3)
}

// 同一个账号下每个角色各进一次：角色周（每人 3）与账号周（共 30）都不绑定，
// 最紧的是**账号日 10**，所以第 11 个角色被拒。这条钉住"账号日这条老限制没被
// 周限制悄悄取代"。
func TestRuntimeRaidAdmissionCounterRejectsTheEleventhAccountDailyEntryAcrossCharacters(t *testing.T) {
	characterIDs := make([]uint16, 0, 12)
	for index := 0; index < 12; index++ {
		characterIDs = append(characterIDs, uint16(1001+index))
	}
	service, _, _, _ := newRuntimeRaidAdmissionCounterTest(t, 1, characterIDs...)
	enter := func(characterID uint16, raidKey uint32) error {
		t.Helper()
		state := runtimeRaidState{
			RaidKey:       raidKey,
			StartAttempt:  1,
			FrozenMembers: []runtimeRaidMemberState{{CharID: characterID}},
		}
		_, err := service.reserveRuntimeRaidAdmissionCounters(
			context.Background(), state, runtimeRaidOwnerAnton, time.Now().UTC())
		return err
	}
	for index, characterID := range characterIDs[:10] {
		if err := enter(characterID, uint32(index+1)); err != nil {
			t.Fatalf("character=%d entry %d should be allowed: %v", characterID, index+1, err)
		}
	}
	err := enter(characterIDs[10], 11)
	if !errors.Is(err, dnfdungeonadmission.ErrLimitReached) {
		t.Fatalf("eleventh entry err=%v want ErrLimitReached", err)
	}
	if !strings.Contains(err.Error(), "raid.anton.account.daily") {
		t.Fatalf("eleventh entry was refused by the wrong policy: %v", err)
	}
}

func TestRuntimeRaidStartMaterialFailureRestoresAdmissionCounter(t *testing.T) {
	service, repositories, catalog, session := newRuntimeRaidAdmissionCounterTest(t, 0, 1001)
	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	state, ok := service.runtimeRaidForMember(1001)
	if !ok || state.Started || state.StartPending || state.EntryCostPaid {
		t.Fatalf("failed start state=%+v found=%t", state, ok)
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 0)
}

func TestRuntimeRaidSuccessfulStartCountsOnceAndReplayDoesNotCountAgain(t *testing.T) {
	service, repositories, catalog, session := newRuntimeRaidAdmissionCounterTest(t, 2, 1001)
	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	if err := service.handleOnlineStartRaid(session, uint16(dnfenum.CmdPacketStartRaid), nil); err != nil {
		t.Fatal(err)
	}
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 1)
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
}

func TestRuntimeRaidCommitFailureCanRestoreTicketAndAdmissionCounter(t *testing.T) {
	service, repositories, catalog, _ := newRuntimeRaidAdmissionCounterTest(t, 1, 1001)
	reservation, err := service.reserveRuntimeRaidStart(1001)
	if err != nil {
		t.Fatal(err)
	}
	state := reservation.State
	state.AntonConfig = service.raidAnton
	counters, err := service.reserveRuntimeRaidAdmissionCounters(
		context.Background(), state, runtimeRaidOwnerAnton, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	entryCost, err := service.consumeRuntimeRaidEntryCostBatch(state)
	if err != nil {
		t.Fatal(err)
	}
	service.raidMu.Lock()
	service.raids[state.RaidKey].StartAttempt++
	service.raidMu.Unlock()
	if _, committed := service.commitRuntimeRaidStart(reservation, entryCost.updates, service.raidAnton, time.Now().UTC()); committed {
		t.Fatal("sabotaged raid start unexpectedly committed")
	}
	var restoreErr error
	if err := service.restoreRuntimeRaidEntryCostBatch(entryCost); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if err := service.restoreCurrentDungeonAdmissionCounterBatch(counters); err != nil {
		restoreErr = errors.Join(restoreErr, err)
	}
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	assertRuntimeRaidTicketCount(t, repositories, "1001", 1)
	assertRuntimeRaidAdmissionUsed(t, repositories, catalog, "1001", "raid.anton.account.daily", 0)
}

func newRuntimeRaidAdmissionCounterTest(
	t *testing.T,
	ticketCount int64,
	characterIDs ...uint16,
) (*Service, dnfrepo.Group, *dnfdungeonadmission.Catalog, *gameSession) {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "raid-counter-account"}); err != nil {
		t.Fatal(err)
	}
	for _, characterID := range characterIDs {
		characterKey := fmt.Sprint(characterID)
		if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
			CharacterID: characterKey,
			AccountID:   "raid-counter-account",
		}); err != nil {
			t.Fatal(err)
		}
		slots := map[string]dnfrepo.ItemStack{}
		if ticketCount > 0 {
			slots["0:0"] = dnfrepo.ItemStack{ItemID: runtimeRaidEntryItemID, Count: ticketCount}
		}
		if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
			CharacterID: characterKey,
			Slots:       slots,
		}); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := dnfdungeonadmission.LoadCatalog(dungeonAdmissionBridgeCatalogSource())
	if err != nil {
		t.Fatal(err)
	}
	config := runtimeRaidBridgeTestAntonConfig()
	config.StartMinimumMembers = 1
	config.SingleStartMinimumMembers = 1
	config.StartDelaySeconds = 3600
	service := &Service{
		options:                 options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		raids:                   make(map[uint32]*runtimeRaidState),
		raidAnton:               config,
		gameplayTimers:          &raidStartRecordingTimeQueue{now: time.Now().UTC()},
		dungeonAdmissionCatalog: catalog,
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
	}
	if len(characterIDs) == 0 {
		return service, repositories, catalog, nil
	}
	leaderID := characterIDs[0]
	session := &gameSession{
		conn:    &bufferConn{},
		channel: channelcatalog.Channel{ID: 200, Type: dnfenum.RaidChannelType},
	}
	service.bindGameSessionCharacter(session, leaderID)
	member := runtimeRaidMemberState{CharID: leaderID, GroupIndex: 1, SlotOrder: 0}
	raidKey := uint32(200)<<16 | uint32(leaderID)
	// The start command refuses a roster that is not standing in the raid waiting
	// area (see raid_start_waiting_area.go).
	enterRuntimeRaidWaitingAreaPresenceForTest(t, service, session, testAntonWaitingAreaSpec(t), raidKey)
	service.raids[raidKey] = &runtimeRaidState{
		RaidKey: raidKey, LeaderID: leaderID, RaidMode: runtimeRaidOwnerAnton,
		AntonConfig: config, Members: []runtimeRaidMemberState{member},
	}
	return service, repositories, catalog, session
}

func assertRuntimeRaidAdmissionUsed(
	t *testing.T,
	repositories dnfrepo.Group,
	catalog *dnfdungeonadmission.Catalog,
	characterID string,
	policyKey string,
	want uint32,
) {
	t.Helper()
	policy, found := catalog.Policy(policyKey)
	if !found {
		t.Fatalf("policy %s missing", policyKey)
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	state, err := owner.Preflight(context.Background(), characterID, []dnfdungeonadmission.Policy{policy}, time.Now().UTC())
	if err != nil || len(state) != 1 || state[0].Used != want {
		t.Fatalf("character=%s policy=%s state=%+v err=%v want=%d", characterID, policyKey, state, err, want)
	}
}
