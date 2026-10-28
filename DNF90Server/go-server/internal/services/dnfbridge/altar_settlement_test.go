package dnfbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/modules/dnf/worldmap"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestBuildCurrentDungeonAltarRankingBodyMatchesCurrentEXEFieldOrder(t *testing.T) {
	body := buildCurrentDungeonAltarRankingBody(
		1,
		0x11223344,
		2,
		0x55667788,
		15,
		0x99aabbcc,
	)
	want, err := hex.DecodeString("443322110100000088776655020000000f000000ccbbaa99")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("op208 body=%x want=%x", body, want)
	}
}

func TestBuildCurrentDungeonAltarRewardBodyPreservesFourCountedGroups(t *testing.T) {
	groups := [4][]currentDungeonAltarRewardEntry{
		{
			{ItemID: 0, Amount: 1000},
			{ItemID: math.MaxUint32, Amount: 0},
		},
		{{ItemID: 1251, Amount: 2}},
		nil,
		{{ItemID: 1252, Amount: 3}},
	}
	body, err := buildCurrentDungeonAltarRewardBody(7, 15, groups)
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(
		"070f02" +
			"00000000e8030000" +
			"ffffffff00000000" +
			"01e304000002000000" +
			"00" +
			"01e404000003000000",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("op209 body=%x want=%x", body, want)
	}
	overflow := groups
	overflow[2] = make([]currentDungeonAltarRewardEntry, math.MaxUint8+1)
	if _, err := buildCurrentDungeonAltarRewardBody(7, 15, overflow); !errors.Is(err, errCurrentDungeonAltarDefinitionInvalid) {
		t.Fatalf("overflow error=%v", err)
	}
}

func TestCurrentDungeonAltarRewardDefinitionParsesExactPVFSections(t *testing.T) {
	source := dungeonDropCatalogTestSource{
		currentDungeonAltarRewardDefinitionPath: "[reward item prob]\n50 47 100\n" +
			"[reward gold weight]\n7.00\n" +
			"[reward exp weight]\n0 1 2 3\n" +
			"[reward exp inout count weight]\n1.00 2.00 4.00\n" +
			"[ultimate difficulty point]\n300 200\n" +
			"[ultimate reward prob]\n2100 20 10 1900 0 0\n",
	}
	definition, err := loadCurrentDungeonAltarRewardDefinition(source)
	if err != nil {
		t.Fatal(err)
	}
	if definition.GoldCandidateWeight != 50 || definition.ItemCandidateWeight != 47 ||
		definition.RewardWeightPerProgress != 100 || definition.GoldAmountWeight != 7 ||
		!reflect.DeepEqual(definition.RewardExperienceWeights, []float64{0, 1, 2, 3}) ||
		!reflect.DeepEqual(definition.EntryCountExperienceWeight, []float64{1, 2, 4}) ||
		definition.UltimateDifficultyPoints != [2]int64{300, 200} {
		t.Fatalf("definition=%+v", definition)
	}
	if got := definition.rewardCardCount(15, 15); got != 10 {
		t.Fatalf("reward cards=%d want=10", got)
	}
	if got := definition.rewardCardCount(1, 15); got != 1 {
		t.Fatalf("minimum reward cards=%d want=1", got)
	}
	for _, test := range []struct {
		roll int
		want currentDungeonAltarRewardCandidate
	}{
		{roll: 499, want: currentDungeonAltarRewardCandidateGold},
		{roll: 500, want: currentDungeonAltarRewardCandidateItem},
		{roll: 969, want: currentDungeonAltarRewardCandidateItem},
		{roll: 970, want: currentDungeonAltarRewardCandidateEmpty},
	} {
		got, classifyErr := definition.classifyReward(15, test.roll)
		if classifyErr != nil || got != test.want {
			t.Fatalf("roll=%d candidate=%d want=%d err=%v", test.roll, got, test.want, classifyErr)
		}
	}
	point := definition.ultimatePoint([]worldmap.BloodAltarDifficulty{
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
		worldmap.BloodAltarDifficultyA,
	})
	if point != 2100 {
		t.Fatalf("ultimate point=%d want=2100", point)
	}
	if item, found := definition.ultimateRewardItem(point, 9); !found || item != 1252 {
		t.Fatalf("ultimate roll9 item=%d found=%t", item, found)
	}
	if item, found := definition.ultimateRewardItem(point, 10); !found || item != 1251 {
		t.Fatalf("ultimate roll10 item=%d found=%t", item, found)
	}
	if _, found := definition.ultimateRewardItem(point, 30); found {
		t.Fatal("ultimate roll30 fabricated an item")
	}
}

func TestCurrentDungeonAltarRewardUsesPersistedMainCapacityAndGrantsOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	seedDungeonCardCharacter(t, ctx, repositories, 100, map[string]dnfrepo.ItemStack{})
	container := newCharacterContainerStateSettings("77", time.Unix(10, 0).UTC())
	container.Values["main_list_param16"] = "24"
	if err := repositories.Settings.Save(ctx, container); err != nil {
		t.Fatal(err)
	}
	// Isolate the asset-owner fallback-placement contract: production PVF
	// rewards normally carry an explicit category range, while an item without
	// one must use (and therefore proves) the persisted MainSlotCount.
	state := &dungeonCardState{
		plan: dungeonCardRewardPlan{
			CharacterID: "77",
			Sides: [dungeonCardSideCount]dungeonCardRewardBundle{
				{Items: []dungeonCardItemReward{{ItemID: 3227, Count: 1}}},
				{},
			},
		},
		selectedMember: [dungeonCardSideCount]int8{-1, -1},
	}
	reservation, result, err := state.reserveSelection(dungeonCardSideFree, 0)
	if err != nil || result != dungeonCardSelectionAccepted || !reservation.grant {
		t.Fatalf("reservation=%+v result=%d err=%v", reservation, result, err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{selectedCharacterID: 77}
	grant, err := service.deliverCurrentDungeonAltarRewardReservation(session, state, reservation)
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.ItemSlots) != 1 || grant.ItemSlots[0] < 0 || grant.ItemSlots[0] >= 24 {
		t.Fatalf("grant=%+v, want one item inside persisted 24-slot capacity", grant)
	}

	replay, replayResult, err := state.reserveSelection(dungeonCardSideFree, 0)
	if err != nil || replayResult != dungeonCardSelectionReplay || replay.grant {
		t.Fatalf("replay=%+v result=%d err=%v", replay, replayResult, err)
	}
	if replayGrant, replayErr := service.deliverCurrentDungeonAltarRewardReservation(session, state, replay); replayErr != nil || len(replayGrant.ItemSlots) != 0 {
		t.Fatalf("replay grant=%+v err=%v", replayGrant, replayErr)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "77")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	itemCount := 0
	for _, stack := range inventory.Slots {
		if stack.ItemID == 3227 {
			itemCount += int(stack.Count)
		}
	}
	if itemCount != 1 {
		t.Fatalf("persisted item count=%d slots=%+v", itemCount, inventory.Slots)
	}
}

func TestCurrentDungeonAltarRewardReceiptReplaysIndeterminateAssetCommit(t *testing.T) {
	ctx := context.Background()
	service, session, runtime, repositories := prepareCurrentDungeonAltarSettlementStageTest(t)
	session.conn = &bufferConn{}
	container := newCharacterContainerStateSettings("99", time.Unix(10, 0).UTC())
	container.Values["main_list_param16"] = "2"
	if err := repositories.Settings.Save(ctx, container); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:0": {ItemID: 1, Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	bundle := dungeonCardRewardBundle{
		Gold: 50,
		Items: []dungeonCardItemReward{
			{ItemID: 3227, Count: 1, SlotStart: 3, SlotEnd: 3},
			{ItemID: 3228, Count: 1, SlotStart: 3, SlotEnd: 3},
		},
	}
	plan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{CharacterID: "99", DungeonID: 11007, RunSeed: runtime.Seed},
		"altar-indeterminate-asset-commit-test",
		bundle,
		dungeonCardRewardBundle{},
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newDungeonCardState(plan)
	if err != nil {
		t.Fatal(err)
	}
	settlement := runtime.altar.settlement
	settlement.phase = currentDungeonAltarSettlementRewardShown
	settlement.plan.RewardBundle = bundle
	settlement.rewardState = state
	wantErr := errors.New("altar mailbox assets committed but caller lost outcome")
	indeterminate := &altarCommitThenErrorMailboxAssets{
		base:     repositories.MailboxAssets,
		failOnce: true,
		err:      wantErr,
	}
	repositories.MailboxAssets = indeterminate
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }

	if err := service.commitCurrentDungeonAltarRewardLocked(session, runtime, settlement); !errors.Is(err, wantErr) {
		t.Fatalf("first indeterminate commit error=%v", err)
	}
	if settlement.rewardCommitted || settlement.phase != currentDungeonAltarSettlementRewardShown {
		t.Fatalf("indeterminate commit advanced settlement: committed=%t phase=%d", settlement.rewardCommitted, settlement.phase)
	}
	character, found, err := repositories.Character.Load(ctx, "99")
	if err != nil || !found || character.Stats["gold"] != 50 {
		t.Fatalf("committed gold found=%t err=%v stats=%v", found, err, character.Stats)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "99")
	if err != nil || !found || inventory.Slots["0:3"].ItemID != 3227 || inventory.Slots["0:3"].Count != 1 {
		t.Fatalf("committed inventory found=%t err=%v slots=%+v", found, err, inventory.Slots)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "99")
	if err != nil || !found || len(mailbox.Mails) != 1 {
		t.Fatalf("committed mailbox found=%t err=%v mails=%+v", found, err, mailbox.Mails)
	}

	if err := service.commitCurrentDungeonAltarRewardLocked(session, runtime, settlement); err != nil {
		t.Fatal(err)
	}
	if !settlement.rewardCommitted || !settlement.rewardCommit.Replayed ||
		settlement.phase != currentDungeonAltarSettlementExitReady {
		t.Fatalf("receipt retry settlement=%+v", settlement)
	}
	character, _, err = repositories.Character.Load(ctx, "99")
	if err != nil || character.Stats["gold"] != 50 {
		t.Fatalf("receipt retry duplicated gold: stats=%v err=%v", character.Stats, err)
	}
	inventory, _, err = repositories.Inventory.Load(ctx, "99")
	if err != nil || inventory.Slots["0:3"].Count != 1 {
		t.Fatalf("receipt retry duplicated item: slots=%+v err=%v", inventory.Slots, err)
	}
	mailbox, _, err = repositories.Mailbox.Load(ctx, "99")
	if err != nil || len(mailbox.Mails) != 1 || indeterminate.calls != 2 {
		t.Fatalf("receipt retry duplicated mail or skipped replay tx: mails=%+v calls=%d err=%v",
			mailbox.Mails, indeterminate.calls, err)
	}
}

type altarCommitThenErrorMailboxAssets struct {
	base     dnfrepo.MailboxAssetUnitOfWork
	failOnce bool
	err      error
	calls    int
}

func (unit *altarCommitThenErrorMailboxAssets) WithinMailboxAssets(
	ctx context.Context,
	accountID string,
	characterID string,
	apply func(
		dnfrepo.AccountRepository,
		dnfrepo.AccountInventoryRepository,
		dnfrepo.CharacterRepository,
		dnfrepo.InventoryRepository,
		dnfrepo.MailboxRepository,
	) error,
) error {
	unit.calls++
	err := unit.base.WithinMailboxAssets(ctx, accountID, characterID, apply)
	if err == nil && unit.failOnce {
		unit.failOnce = false
		return unit.err
	}
	return err
}

var _ dnfrepo.MailboxAssetUnitOfWork = (*altarCommitThenErrorMailboxAssets)(nil)

func TestCurrentDungeonAltarSettlementSendsOp208ThenOp209BeforeCommitAndOp72RetriesFailure(t *testing.T) {
	service, session, runtime, repositories := prepareCurrentDungeonAltarSettlementStageTest(t)
	connection := &bufferConn{}
	session.conn = connection
	service.repositoryProvider = nil

	err := service.startCurrentDungeonAltarSettlementLocked(session, runtime)
	if !errors.Is(err, errDungeonCardAssetOwnerUnavailable) {
		t.Fatalf("first settlement error=%v", err)
	}
	if runtime.altar.settlement.phase != currentDungeonAltarSettlementRewardShown {
		t.Fatalf("phase after commit failure=%d", runtime.altar.settlement.phase)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	second, rest := splitGameServerUpperPacket(t, rest)
	if first.Header.MsgID != currentDungeonAltarRankingMsgID ||
		second.Header.MsgID != currentDungeonAltarRewardMsgID || len(rest) != 0 {
		t.Fatalf("settlement packet order=%d,%d trailing=%x", first.Header.MsgID, second.Header.MsgID, rest)
	}

	bytesBeforeRetry := connection.write.Len()
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	if err := service.handleDungeonEplpCommandLocal(session, []byte{2, 0}, false, false); err != nil {
		t.Fatal(err)
	}
	if !completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("op72 retry did not reach altar ExitReady: %+v", runtime.altar.settlement)
	}
	if runtime.settlementClearRewardSent {
		t.Fatal("altar retry polluted ordinary settlementClearRewardSent")
	}
	stream := connection.write.Bytes()[bytesBeforeRetry:]
	var retryIDs []uint16
	for len(stream) != 0 {
		packet, trailing := splitGameServerUpperPacket(t, stream)
		retryIDs = append(retryIDs, packet.Header.MsgID)
		if packet.Header.MsgID == currentDungeonAltarRankingMsgID || packet.Header.MsgID == currentDungeonAltarRewardMsgID {
			t.Fatalf("commit retry resent altar UI packet %d", packet.Header.MsgID)
		}
		stream = trailing
	}
	if !reflect.DeepEqual(retryIDs, []uint16{uint16(dnfenum.CmdPacketFinishLoading), uint16(dnfenum.CmdPacketEplpCommand)}) {
		t.Fatalf("op72 commit retry packet ids=%v", retryIDs)
	}
}

func TestCurrentDungeonAltarSettlementRetriesOnlyOp209AfterItsWriteFails(t *testing.T) {
	service, session, runtime, _ := prepareCurrentDungeonAltarSettlementStageTest(t)
	wantWriteErr := errors.New("op209 write failed")
	failing := &failNthDungeonWriteConn{failAt: 2, err: wantWriteErr}
	session.conn = failing
	if err := service.startCurrentDungeonAltarSettlementLocked(session, runtime); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first settlement error=%v", err)
	}
	if runtime.altar.settlement.phase != currentDungeonAltarSettlementRankingShown {
		t.Fatalf("phase after op209 write failure=%d", runtime.altar.settlement.phase)
	}
	first, rest := splitGameServerUpperPacket(t, failing.write.Bytes())
	if first.Header.MsgID != currentDungeonAltarRankingMsgID || len(rest) != 0 {
		t.Fatalf("failed prefix msg=%d trailing=%x", first.Header.MsgID, rest)
	}

	retry := &bufferConn{}
	session.conn = retry
	if err := service.startCurrentDungeonAltarSettlementLocked(session, runtime); err != nil {
		t.Fatal(err)
	}
	if !completedCurrentDungeonAltarSettlementExitReady(runtime, session.selectedCharacterID) {
		t.Fatalf("op209 retry did not reach ExitReady: %+v", runtime.altar.settlement)
	}
	packet, stream := splitGameServerUpperPacket(t, retry.write.Bytes())
	if packet.Header.MsgID != currentDungeonAltarRewardMsgID {
		t.Fatalf("retry first msg=%d want op209", packet.Header.MsgID)
	}
	for len(stream) != 0 {
		packet, trailing := splitGameServerUpperPacket(t, stream)
		if packet.Header.MsgID == currentDungeonAltarRankingMsgID || packet.Header.MsgID == currentDungeonAltarRewardMsgID {
			t.Fatalf("retry duplicated altar UI msg=%d", packet.Header.MsgID)
		}
		stream = trailing
	}
}

func prepareCurrentDungeonAltarSettlementStageTest(
	t *testing.T,
) (*Service, *gameSession, *runtimeDungeonState, dnfrepo.Group) {
	t.Helper()
	service, session, runtime, _, _ := prepareCurrentDungeonAltarRuntimeTest(t)
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("test repository group unavailable")
	}
	if err := runtime.Session.MarkCurrentRoomScriptedCleared(); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Session.Complete(); err != nil {
		t.Fatal(err)
	}
	runtime.altar.completedRounds = runtime.altar.MaxRounds
	runtime.clearMapCompletionKey = "altar-settlement-stage-test-completion"
	plan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{CharacterID: "99", DungeonID: 11007, RunSeed: runtime.Seed},
		"altar-settlement-stage-test",
		dungeonCardRewardBundle{},
		dungeonCardRewardBundle{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rewardState, err := newDungeonCardState(plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime.altar.settlement = &currentDungeonAltarSettlementRuntime{
		phase: currentDungeonAltarSettlementPrepared,
		plan: currentDungeonAltarSettlementPlan{
			CurrentRound:         uint32(runtime.altar.MaxRounds),
			PlayTimeMilliseconds: 700,
			BestRound:            uint32(runtime.altar.MaxRounds),
			BestTimeMilliseconds: 700,
			MaxRound:             uint32(runtime.altar.MaxRounds),
			CharacterStateBody:   make([]byte, currentFinishLoadingCharacterStateBodySize),
		},
		rewardState: rewardState,
	}
	return service, session, runtime, repositories
}

func TestCurrentDungeonAltarRewardDefinitionRejectsMalformedRows(t *testing.T) {
	base := "[reward item prob]\n50 47 100\n" +
		"[reward gold weight]\n7.00\n" +
		"[reward exp weight]\n0 1\n" +
		"[reward exp inout count weight]\n1 2\n" +
		"[ultimate difficulty point]\n300 200\n"
	for name, tail := range map[string]string{
		"probability overflow": "[ultimate reward prob]\n2100 90 20\n",
		"duplicate point":      "[ultimate reward prob]\n2100 0 0 2100 0 0\n",
		"bad row width":        "[ultimate reward prob]\n2100 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			source := dungeonDropCatalogTestSource{currentDungeonAltarRewardDefinitionPath: base + tail}
			if _, err := loadCurrentDungeonAltarRewardDefinition(source); !errors.Is(err, errCurrentDungeonAltarRewardDefinitionInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentDungeonAltarSettlementRewardGateIsIdempotent(t *testing.T) {
	plan, err := newDungeonCardRewardPlan(
		dungeonCardPlanIdentity{CharacterID: "7", DungeonID: 11006, MazeIndex: 0, RunSeed: 42},
		"altar-idempotency-test",
		dungeonCardRewardBundle{Gold: 700},
		dungeonCardRewardBundle{},
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newDungeonCardState(plan)
	if err != nil {
		t.Fatal(err)
	}
	first, result, err := state.reserveSelection(dungeonCardSideFree, 0)
	if err != nil || result != dungeonCardSelectionAccepted || !first.grant || first.bundle.Gold != 700 {
		t.Fatalf("first reservation=%+v result=%d err=%v", first, result, err)
	}
	state.finishDelivery(first, true)
	replay, result, err := state.reserveSelection(dungeonCardSideFree, 0)
	if err != nil || result != dungeonCardSelectionReplay || replay.grant {
		t.Fatalf("replay reservation=%+v result=%d err=%v", replay, result, err)
	}
	if _, _, err := state.reserveSelection(dungeonCardSideFree, 1); !errors.Is(err, errDungeonCardSelectionConflict) {
		t.Fatalf("conflicting retry error=%v", err)
	}
}

func TestRealScriptPVFCurrentDungeonAltarRewardDefinition(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to verify bloodclearreward.etc")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loadCurrentDungeonAltarRewardDefinition(archive)
	if err != nil {
		t.Fatal(err)
	}
	if definition.GoldCandidateWeight != 50 || definition.ItemCandidateWeight != 47 ||
		definition.RewardWeightPerProgress != 100 || definition.GoldAmountWeight != 7 ||
		len(definition.RewardExperienceWeights) != 16 || len(definition.EntryCountExperienceWeight) != 13 ||
		definition.UltimateDifficultyPoints != [2]int64{300, 200} {
		t.Fatalf("real definition=%+v", definition)
	}
	if len(definition.UltimateRewards) == 0 {
		t.Fatal("real ultimate reward table is empty")
	}
	for point, row := range definition.UltimateRewards {
		if row.Item1251Probability != 0 || row.Item1252Probability != 0 {
			t.Fatalf("real ultimate reward point=%d unexpectedly enabled: %+v", point, row)
		}
	}
}
