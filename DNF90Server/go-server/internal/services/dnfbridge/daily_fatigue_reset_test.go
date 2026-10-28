package dnfbridge

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestSelectedCharacterForEnterReconcilesInitializedDailyFatigue(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 1, 0, 0, time.UTC)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "daily-reset", Stats: map[string]int64{
			"fatigue": 0, "fatigue_limit": dnffatigue.BlackDiamondLimit,
			dnffatigue.DailyRecoveryDayStatKey:             premium.ServiceDayID(before),
			dnffatigue.PotionUsageDayStatKey:               premium.ServiceDayID(before),
			dnffatigue.PotionUsageStatPrefix + "490702338": 3,
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     &processGameplayTimeQueue{now: func() time.Time { return after }},
	}
	session := &gameSession{selectedCharacterID: 19, accountID: "account-1"}
	characterID, _, character, found := service.selectedCharacterForEnter(ctx, session)
	if !found || characterID != 19 || character.Stats["fatigue"] != dnffatigue.BlackDiamondLimit ||
		character.Stats[dnffatigue.PotionUsageStatPrefix+"490702338"] != 0 {
		t.Fatalf("character_id=%d found=%t character=%+v", characterID, found, character)
	}
	stored, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || stored.Stats[dnffatigue.DailyRecoveryDayStatKey] != premium.ServiceDayID(after) {
		t.Fatalf("stored=%+v found=%t err=%v", stored, found, err)
	}
}

func TestDailyFatigueTimerResetsOnlineCharacterAtBeijingSixAndRefreshesHUD(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC)
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "daily-reset", Stats: map[string]int64{
			"fatigue": 3, "fatigue_limit": dnffatigue.BlackDiamondLimit, "fatigue_display_update": 185,
			dnffatigue.DailyRecoveryDayStatKey:             premium.ServiceDayID(before),
			dnffatigue.PotionUsageDayStatKey:               premium.ServiceDayID(before),
			dnffatigue.PotionUsageStatPrefix + "490702338": 2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = before
	connection := &bufferConn{}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:     queue,
	}
	session := &gameSession{
		conn: connection, connID: "daily-fatigue", accountID: "account-1",
		selectedCharacterID: 19, characterGeneration: 7,
	}
	if err := service.armCurrentDailyFatigueReset(session); err != nil {
		t.Fatal(err)
	}
	first := queue.task(t, 0)
	if first.delay != time.Minute || first.cancelled {
		t.Fatalf("first timer delay=%v cancelled=%t", first.delay, first.cancelled)
	}
	queue.mu.Lock()
	queue.now = after
	queue.mu.Unlock()
	first.callback(after)

	stored, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("stored character found=%t err=%v", found, err)
	}
	if stored.Stats["fatigue"] != dnffatigue.DefaultLimit || stored.Stats["fatigue_display_update"] != 0 ||
		stored.Stats[dnffatigue.PotionUsageStatPrefix+"490702338"] != 0 ||
		stored.Stats[dnffatigue.DailyRecoveryDayStatKey] != premium.ServiceDayID(after) {
		t.Fatalf("stored stats=%+v", stored.Stats)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentFatigueMsgID ||
		!bytes.Equal(packet.Body, buildCurrentFatigueBody(stored, true)) || len(trailing) != 0 {
		t.Fatalf("fatigue packet header=%+v body=%x trailing=%d", packet.Header, packet.Body, len(trailing))
	}
	second := queue.task(t, 1)
	if second.delay != 24*time.Hour || second.cancelled {
		t.Fatalf("second timer delay=%v cancelled=%t", second.delay, second.cancelled)
	}
}

func TestDailyResetTimerGrantsMissingTowerInvitationsAndDoesNotStackExistingOne(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	before := time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC)
	after := time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC)
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Stats: map[string]int64{
			"fatigue": 0, "fatigue_limit": dnffatigue.DefaultLimit,
			dnffatigue.DailyRecoveryDayStatKey: premium.ServiceDayID(before),
			dnffatigue.PotionUsageDayStatKey:   premium.ServiceDayID(before),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: currentTowerOfGriefInvitationItemID, Count: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := dungeonAdmissionBridgeCatalogSource()
	source["monster/monster.lst"] = ""
	source[dungeonDropStackableList] = "4473 `invitation_drapyus2.stk`\n10161065 `10161001/10161065.stk`\n"
	source[dungeonDropEquipmentList] = ""
	source[currentTowerOfGriefInvitationPVFPath] = "[name]\n`悲叹之邀请函`\n[explain]\n`通关成功后当天无法再次进入悲叹之塔。`\n[stackable type]\n`[material]` 0\n"
	itemCatalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	admissionCatalog, err := dnfdungeonadmission.LoadCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	queue := newFakeCurrentDungeonDeathTimerQueue()
	queue.now = before
	connection := &bufferConn{}
	service := &Service{
		repositoryProvider:      func() (dnfrepo.Group, bool) { return repositories, true },
		gameplayTimers:          queue,
		pvfItemCatalog:          itemCatalog,
		dungeonAdmissionCatalog: admissionCatalog,
	}
	session := &gameSession{
		conn: connection, connID: "daily-dungeon", accountID: "account-1",
		selectedCharacterID: 19, characterGeneration: 7,
	}
	if err := service.armCurrentDailyFatigueReset(session); err != nil {
		t.Fatal(err)
	}
	queue.mu.Lock()
	queue.now = after
	queue.mu.Unlock()
	queue.task(t, 0).callback(after)

	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if inventory.Slots["0:121"].ItemID != currentTowerOfGriefInvitationItemID || inventory.Slots["0:121"].Count != 2 {
		t.Fatalf("existing grief invitation was stacked: %+v", inventory.Slots)
	}
	if inventory.Slots["0:177"].ItemID != currentTowerOfDespairInvitationItemID || inventory.Slots["0:177"].Count != 1 {
		t.Fatalf("missing despair invitation was not granted: %+v", inventory.Slots)
	}
	if _, materialPageGrant := inventory.Slots["0:122"]; materialPageGrant {
		t.Fatalf("daily invitation was granted to the material page: %+v", inventory.Slots)
	}

	first, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if first.Header.MsgID != currentFatigueMsgID {
		t.Fatalf("first packet=%+v", first.Header)
	}
	second, trailing := splitGameServerUpperPacket(t, trailing)
	if second.Header.Classification != 0 || second.Header.MsgID != 14 {
		t.Fatalf("invitation refresh packet=%+v body=%x", second.Header, second.Body)
	}
	third, trailing := splitGameServerUpperPacket(t, trailing)
	if third.Header.Classification != 0 || third.Header.MsgID != currentRaidDailyAvailabilityMsgID {
		t.Fatalf("daily count packet=%+v trailing=%x", third.Header, trailing)
	}
	// The same service-window boundary rolls the weekly admission counters back to
	// zero, so the reset must re-push the three op1206 raid records immediately
	// after the op1303 rows -- otherwise a player who is online at 06:00 keeps
	// reading last week's used count until they relog.
	for _, wantEventID := range []uint32{currentAntonRaidEventID, currentLukeRaidEventID, currentFiendWarEventID} {
		schedule, rest := splitGameServerUpperPacket(t, trailing)
		want, err := buildCurrentRaidScheduleProgressBody(wantEventID, currentRaidScheduleCounters{})
		if err != nil {
			t.Fatal(err)
		}
		if schedule.Header.Classification != 0 || schedule.Header.MsgID != currentSpendTimeProgressMsgID ||
			!bytes.Equal(schedule.Body, want) {
			t.Fatalf("weekly reset op1206 event=%d header=%+v body=%x want=%x", wantEventID, schedule.Header, schedule.Body, want)
		}
		trailing = rest
	}
	if len(trailing) != 0 {
		t.Fatalf("reset trailing=%x", trailing)
	}
}

func TestCurrentDailyDungeonInvitationsRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify daily tower invitations")
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	invitations, err := service.currentDailyDungeonInvitations()
	if err != nil {
		t.Fatal(err)
	}
	if len(invitations) != 2 ||
		invitations[0].ItemID != currentTowerOfDespairInvitationItemID || invitations[0].PVFPath != currentTowerOfDespairInvitationPVFPath ||
		invitations[1].ItemID != currentTowerOfGriefInvitationItemID || invitations[1].PVFPath != currentTowerOfGriefInvitationPVFPath {
		t.Fatalf("invitations=%+v", invitations)
	}
	for _, invitation := range invitations {
		if invitation.Minimum != 1 || invitation.ListType != dnfrepo.MainInventoryListType ||
			invitation.CheckSlotStart != 121 || invitation.CheckSlotEnd != 232 || invitation.SlotStart != 177 || invitation.SlotEnd != 232 ||
			normalizeDungeonDropStackableType(invitation.StackableType) != "[material]" {
			t.Fatalf("invitation=%+v", invitation)
		}
	}
}

func TestDailyFatigueTimerStopsAcrossCharacterGenerationChange(t *testing.T) {
	queue := newFakeCurrentDungeonDeathTimerQueue()
	service := &Service{gameplayTimers: queue}
	session := &gameSession{connID: "daily-stale", selectedCharacterID: 19, characterGeneration: 3}
	if err := service.armCurrentDailyFatigueReset(session); err != nil {
		t.Fatal(err)
	}
	task := queue.task(t, 0)
	service.stopCurrentDailyFatigueReset(session, "test_character_change")
	session.selectedCharacterID = 20
	session.characterGeneration = 4
	task.callback(queue.Now().Add(task.delay))
	if !task.cancelled || len(queue.tasks) != 1 {
		t.Fatalf("cancelled=%t tasks=%d", task.cancelled, len(queue.tasks))
	}
}

func TestNextDailyFatigueResetBoundaryUsesBeijingSix(t *testing.T) {
	tests := []struct {
		now  time.Time
		want time.Time
	}{
		{
			now:  time.Date(2026, 8, 30, 21, 59, 0, 0, time.UTC),
			want: time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC),
		},
		{
			now:  time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC),
			want: time.Date(2026, 8, 31, 22, 0, 0, 0, time.UTC),
		},
	}
	for _, test := range tests {
		if got := nextCurrentDailyFatigueResetBoundary(test.now); !got.Equal(test.want) {
			t.Fatalf("now=%v boundary=%v want=%v", test.now, got, test.want)
		}
	}
}
