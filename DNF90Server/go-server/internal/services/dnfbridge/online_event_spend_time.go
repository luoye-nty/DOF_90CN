package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSpendTimeStageCount   = 4
	currentSpendTimeObserveTick  = 30 * time.Second
	currentSpendTimeResetHour    = 6
	currentSpendTimeWriteTimeout = 10 * time.Second
)

// The established 2347 definition retains the durable daily observation clock.
// Production loads nativePCRoom from its separate ETC file and activates 228,
// whose manual mail rewards replace the old automatic inventory awards.
type currentSpendTimeRuntimeCatalog struct {
	laborDay          *dnfonlineevent.LaborDayCatalog
	nativePCRoom      *dnfonlineevent.PCRoomTimePointCatalog
	definition        dnfonlineevent.Definition
	rewardItemIDs     []uint32
	sumRewardItemIDs  []uint32
	totalStageSeconds uint32
}

// currentSpendTimeClockState owns one selected-character observation cursor.
// Its timer callback is routed through the per-session event loop and also
// carries characterGeneration, so a stale callback cannot credit a replacement
// character or write to its socket.
type currentSpendTimeClockState struct {
	laborDayActive                 bool
	mu                             sync.Mutex
	eventInfoSent                  bool
	eventInfoDescriptorGeneration  uint64
	eventInfoReplayAfterTeamReturn bool
	characterID                    uint16
	characterGeneration            uint64
	initializedCharacterGeneration uint64
	accountID                      string
	anchor                         time.Time
	generation                     uint64
	timerName                      string
	onlineSeconds                  uint64
	completedStages                uint32
	catalog                        *currentSpendTimeRuntimeCatalog
}

type currentSpendTimeSettlementResult struct {
	changed     bool
	rewardItems []dnfonlineevent.CommittedItem
	claimErr    error
}

func buildCurrentSpendTimeRuntimeCatalog(catalog *dnfonlineevent.AttendancePVFCatalog) (*currentSpendTimeRuntimeCatalog, error) {
	if catalog == nil {
		return nil, fmt.Errorf("current spend-time attendance catalog is nil")
	}
	snapshot := catalog.Snapshot()
	if len(snapshot.ProcessDurationsSeconds) != currentSpendTimeStageCount ||
		len(snapshot.RewardItems) != currentSpendTimeStageCount {
		return nil, fmt.Errorf(
			"current spend-time PVF shape durations=%d rewards=%d want=%d/%d",
			len(snapshot.ProcessDurationsSeconds),
			len(snapshot.RewardItems),
			currentSpendTimeStageCount,
			currentSpendTimeStageCount,
		)
	}

	definition := dnfonlineevent.Definition{
		ID:     strconv.FormatUint(uint64(currentSpendTimeEventID), 10),
		Stages: make([]dnfonlineevent.Stage, 0, currentSpendTimeStageCount),
	}
	rewardItemIDs := make([]uint32, 0, currentSpendTimeStageCount)
	sumRewardItemIDs := make([]uint32, 0, currentSpendTimeStageCount)
	var cumulative uint64
	for index := 0; index < currentSpendTimeStageCount; index++ {
		duration := snapshot.ProcessDurationsSeconds[index]
		reward := snapshot.RewardItems[index]
		if duration <= 0 || reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 {
			return nil, fmt.Errorf(
				"current spend-time PVF stage=%d duration=%d item=%d count=%d",
				index,
				duration,
				reward.ItemID,
				reward.Count,
			)
		}
		cumulative += uint64(duration)
		if cumulative > math.MaxUint32 {
			return nil, fmt.Errorf("current spend-time cumulative seconds=%d overflows u32", cumulative)
		}
		definition.Stages = append(definition.Stages, dnfonlineevent.Stage{
			ID:              fmt.Sprintf("stage-%d", index+1),
			RequiredSeconds: cumulative,
			Items: []dnfonlineevent.ItemReward{{
				ItemID: reward.ItemID,
				Count:  reward.Count,
			}},
		})
		rewardItemIDs = append(rewardItemIDs, uint32(reward.ItemID))
	}
	for _, reward := range snapshot.RewardItemsForSum {
		if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 {
			return nil, fmt.Errorf("current spend-time sum reward item=%d count=%d", reward.ItemID, reward.Count)
		}
		sumRewardItemIDs = append(sumRewardItemIDs, uint32(reward.ItemID))
	}
	if err := definition.Validate(); err != nil {
		return nil, fmt.Errorf("validate current spend-time definition: %w", err)
	}
	return &currentSpendTimeRuntimeCatalog{
		definition:        definition,
		rewardItemIDs:     append([]uint32(nil), rewardItemIDs...),
		sumRewardItemIDs:  append([]uint32(nil), sumRewardItemIDs...),
		totalStageSeconds: uint32(cumulative),
	}, nil
}

// currentSpendTimeRuntimeCatalogIfAvailable lazily projects the same archive
// already loaded by the startup PVF stages. Hand-assembled unit-test services
// without an archive keep the feature absent unless the test injects a proved
// projection; production Start always establishes the archive first.
func (s *Service) currentSpendTimeRuntimeCatalogIfAvailable(ctx context.Context) (*currentSpendTimeRuntimeCatalog, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}

	s.spendTimeCatalogMu.Lock()
	defer s.spendTimeCatalogMu.Unlock()
	if s.spendTimeCatalogState != nil {
		return s.spendTimeCatalogState, true, nil
	}
	if s.spendTimeCatalogLoadErr != nil {
		return nil, true, s.spendTimeCatalogLoadErr
	}

	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return nil, false, nil
	}
	rawCatalog, err := dnfonlineevent.LoadAttendancePVFCatalog(ctx, archive)
	if err == nil {
		s.spendTimeCatalogState, err = buildCurrentSpendTimeRuntimeCatalog(rawCatalog)
	}
	if err == nil {
		s.spendTimeCatalogState.nativePCRoom, err = dnfonlineevent.LoadPCRoomTimePointCatalog(ctx, archive)
		if err != nil {
			s.spendTimeCatalogState = nil
		}
	}
	if err == nil {
		s.spendTimeCatalogState.laborDay, err = s.currentLaborDayCatalog(ctx)
	}
	s.spendTimeCatalogLoadErr = err
	if err != nil {
		return nil, true, err
	}
	s.logPacketEvent("dnf-spend-time-catalog-loaded",
		"event_id", currentSpendTimeEventID,
		"reward_count", len(s.spendTimeCatalogState.rewardItemIDs),
		"total_stage_seconds", s.spendTimeCatalogState.totalStageSeconds,
		"source", dnfonlineevent.AttendanceEventPVFPath)
	return s.spendTimeCatalogState, true, nil
}

func (s *Service) currentSpendTimeOwner() (*dnfonlineevent.Owner, error) {
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfonlineevent.ErrOwnerUnavailable
	}
	return dnfonlineevent.NewOwner(repositories)
}

// Publish the activity directory once per actor generation, followed by the
// active owner's state (561 for native 228), then start the durable clock.
func (s *Service) sendCurrentSpendTimeInitialStateOnce(
	session *gameSession,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := session.selectedCharacterID
	characterGeneration := session.characterGeneration

	session.spendTime.mu.Lock()
	alreadyInitialized := session.spendTime.characterID == characterID &&
		session.spendTime.initializedCharacterGeneration == characterGeneration &&
		session.spendTime.catalog != nil && !session.spendTime.anchor.IsZero()
	session.spendTime.mu.Unlock()
	if alreadyInitialized {
		// The descriptor is a one-shot activity-table replacement.  Replaying
		// op108 from the scene-ready tail resets the native owner and makes the
		// same online-time entries render a second time.  The first town route
		// already sends op108 followed by op1206; later callbacks only refresh
		// their authoritative state through the normal progress path.
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	catalog, available, err := s.currentSpendTimeRuntimeCatalogIfAvailable(ctx)
	if err != nil {
		return fmt.Errorf("load current spend-time catalog: %w", err)
	}
	if !available || catalog == nil {
		s.logGameEvent(session, "game-spend-time-initialization-deferred",
			"source", source,
			"char_id", characterID,
			"reason", "runtime_pvf_archive_not_loaded")
		return nil
	}
	owner, err := s.currentSpendTimeOwner()
	if err != nil {
		return fmt.Errorf("create current spend-time owner: %w", err)
	}
	now := s.gameplayNow()
	accountID := s.accountIDForSession(session)
	characterIDText := strconv.Itoa(int(characterID))

	session.spendTime.mu.Lock()
	defer session.spendTime.mu.Unlock()
	if session.spendTime.characterID == characterID &&
		session.spendTime.initializedCharacterGeneration == characterGeneration &&
		session.spendTime.catalog != nil && !session.spendTime.anchor.IsZero() {
		return nil
	}
	if session.spendTime.characterID != 0 && session.spendTime.characterID != characterID {
		return fmt.Errorf("current spend-time clock still owns character %d", session.spendTime.characterID)
	}
	snapshot, err := owner.Status(ctx, dnfonlineevent.StatusCommand{
		AccountID:   accountID,
		CharacterID: characterIDText,
		Definition:  catalog.definition,
		ObservedAt:  now,
	})
	if err != nil {
		return fmt.Errorf("load current spend-time status: %w", err)
	}
	snapshot, rewardItems, claimErr := s.claimCurrentSpendTimeUnlockedStages(
		ctx,
		owner,
		accountID,
		characterIDText,
		catalog,
		snapshot,
		now,
	)
	if claimErr != nil {
		s.logGameEvent(session, "game-spend-time-initial-auto-award-deferred",
			"source", source,
			"char_id", characterID,
			"online_seconds", snapshot.OnlineSeconds,
			"error", claimErr)
	}
	completedStages := currentSpendTimeClaimedStages(catalog.definition, snapshot)
	if err := s.settleCurrentLaborDay(ctx, session, catalog.laborDay, time.Time{}, time.Time{}, true); err != nil {
		return err
	}
	// op108 is not merely process-global metadata: it creates the two floating
	// activity controls in the selected actor's town HUD. Rebuild it once for
	// every new actor generation. Keep the legacy registry only for a retry in
	// the same generation, where a descriptor may already have reached the EXE
	// before a later packet write failed.
	sceneDescriptorRequired := !session.spendTime.eventInfoSent ||
		session.spendTime.eventInfoDescriptorGeneration != characterGeneration
	registrySendDescriptor, descriptorReady := false, true
	if !sceneDescriptorRequired {
		registrySendDescriptor, descriptorReady = s.beginCurrentSpendTimeEventInfo(session)
		if !descriptorReady {
			return fmt.Errorf("current spend-time descriptor retry is being sent by another session for client pid=%d", session.clientPID)
		}
	}
	forceDescriptorReplay := session.spendTime.eventInfoReplayAfterTeamReturn
	sendDescriptor := sceneDescriptorRequired || registrySendDescriptor || forceDescriptorReplay
	descriptorSent, sendErr := s.sendCurrentSpendTimeProtocolState(
		session,
		catalog,
		snapshot.OnlineSeconds,
		completedStages,
		sendDescriptor,
	)
	if registrySendDescriptor || descriptorSent {
		// Commit immediately after a successful op108 write, even when this
		// scene-generation send bypassed the legacy registry. A later op1206
		// progress or joust snapshot failure must not let a retry rediscover an
		// unsent process-level descriptor and duplicate it in this same scene.
		s.finishCurrentSpendTimeEventInfo(session, descriptorSent)
	}
	if descriptorSent {
		session.spendTime.eventInfoSent = true
		session.spendTime.eventInfoDescriptorGeneration = characterGeneration
	}
	if forceDescriptorReplay && descriptorSent {
		// The full op108 write is the commit boundary. A later op1206 failure is
		// retried progress-only because replaying the base catalog twice after the
		// team-return repair would repeat the same client-side owner reset.
		session.spendTime.eventInfoReplayAfterTeamReturn = false
		s.logGameEvent(session, "game-spend-time-descriptor-replayed-after-team-return",
			"source", source,
			"char_id", characterID,
			"client_pid", session.clientPID,
			"reason", "team_channel_return_rebuilds_client_activity_table")
	}
	if sendErr != nil {
		return sendErr
	}
	if !sendDescriptor {
		s.logGameEvent(session, "game-spend-time-descriptor-skipped",
			"source", source,
			"char_id", characterID,
			"client_pid", session.clientPID,
			"already_sent", session.spendTime.eventInfoSent,
			"reason", "descriptor_already_written_for_current_actor_generation_progress_only")
	}
	if err := s.sendCurrentSpendTimeRewardItemUpdates(session, rewardItems, source+"_initial_auto_award"); err != nil {
		return err
	}
	// 参考实现这里调 cera_shop_buycount.go 的 sendCurrentCeraShopBuyCountInitialState。
	// 本树把同一件事实现成了 sendCurrentCeraShopBuyCountGiftProjection（同一个
	// 0x040B opcode、同一个 u32 实购买次数体），只是计数落在角色 Stats 而不是账号
	// 元数据上，所以这里直接复用本树那一个，不引入第二份"已购买次数"下发。
	if err := s.sendCurrentCeraShopBuyCountGiftProjection(ctx, session, "spend_time_initial_state"); err != nil {
		return err
	}
	if err := s.sendCurrentMileageInitialState(ctx, session); err != nil {
		return err
	}

	session.spendTime.characterID = characterID
	session.spendTime.characterGeneration = characterGeneration
	session.spendTime.initializedCharacterGeneration = characterGeneration
	session.spendTime.accountID = accountID
	// Keep the exact observation start. onlineevent.Owner conservatively
	// ceil(from)/floor(to); truncating here would manufacture up to one second.
	session.spendTime.anchor = now
	session.spendTime.generation++
	session.spendTime.onlineSeconds = snapshot.OnlineSeconds
	session.spendTime.completedStages = completedStages
	session.spendTime.catalog = catalog
	session.spendTime.laborDayActive = catalog.laborDay != nil && catalog.laborDay.Policy.ActiveAt(now)
	if err := s.armCurrentSpendTimeTickLocked(session); err != nil {
		session.spendTime.characterID = 0
		session.spendTime.characterGeneration = 0
		session.spendTime.initializedCharacterGeneration = 0
		session.spendTime.accountID = ""
		session.spendTime.anchor = time.Time{}
		session.spendTime.timerName = ""
		session.spendTime.onlineSeconds = 0
		session.spendTime.completedStages = 0
		session.spendTime.catalog = nil
		return err
	}
	s.logGameEvent(session, "game-spend-time-initial-state-sent",
		"source", source,
		"char_id", characterID,
		"event_id", currentSpendTimeEventID,
		"online_seconds", snapshot.OnlineSeconds,
		"completed_stages", completedStages,
		"reward_item_updates", len(rewardItems),
		"sequence", "class0_op108_descriptor_then_class0_op1206_progress_then_class0_op1206_luke_schedule_then_class0_op1206_fiendwar_schedule_then_class0_op1303_raid_availability_then_atomic_reward_op14_before_typed_op24")
	return nil
}

func (s *Service) sendCurrentSpendTimeProtocolState(
	session *gameSession,
	catalog *currentSpendTimeRuntimeCatalog,
	onlineSeconds uint64,
	completedStages uint32,
	includeFirstProcessDescriptor bool,
) (bool, error) {
	if s == nil || session == nil || catalog == nil {
		return false, nil
	}
	var eventInfoTransport []byte
	levelAimingRowInstalled := false
	if includeFirstProcessDescriptor {
		// op108 的事件表里 222（全职Lv预约）只在它自己的 PVF 窗口内出现，
		// 否则客户端会一直画着已经过期的活动入口。
		now := s.gameplayNow()
		levelAimingCtx, levelAimingCancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
		levelAimingCatalog, levelAimingErr := s.currentLevelAimingCatalog(levelAimingCtx)
		levelAimingCancel()
		if levelAimingErr != nil {
			s.logGameEvent(session, "game-level-aiming-catalog-unavailable", "error", levelAimingErr)
		}
		levelAimingRowInstalled = levelAimingWindowActive(levelAimingCatalog, now)
		if !levelAimingRowInstalled {
			// 证据行：窗口外不写 222，客户端不该再画"全职Lv预约得好礼"入口。
			s.logGameEvent(session, "game-level-aiming-row-omitted",
				"source", "op108_event_table",
				"event_id", currentDecisionEventID,
				"reason", "level_aiming_window_closed")
		}
		eventInfoBody, err := buildCurrentOnlineEventInfoBodyAt(
			catalog.rewardItemIDs,
			catalog.sumRewardItemIDs,
			catalog.totalStageSeconds,
			catalog.nativePCRoom != nil,
			levelAimingRowInstalled,
			catalog.laborDay, now,
		)
		if err != nil {
			return false, err
		}
		eventInfoTransport, err = zlibCompress(eventInfoBody)
		if err != nil {
			return false, fmt.Errorf("compress current spend-time op108: %w", err)
		}
	}
	progressBody, err := buildCurrentSpendTimeProgressBody(onlineSeconds, completedStages)
	if err != nil {
		return false, err
	}
	descriptorSent := false
	if includeFirstProcessDescriptor {
		if err := s.sendCurrentProtectedClass0Packet(
			session,
			currentSpendTimeEventInfoMsgID,
			eventInfoTransport,
			currentSpendTimeEventInfoCodec,
			"current_op108_spend_time_catalog",
		); err != nil {
			return false, err
		}
		descriptorSent = true
		// op108 makes the weekday gate visible, but the client's area-move
		// check reads the separate op855 schedule table before it opens the
		// dungeon list. Install all five local-profile rows while the selected
		// character is still receiving its initial event state, rather than
		// waiting until town 107 has already been entered.
		if err := s.sendCurrentMysteriousRuinsSchedule(
			session, 107, 0, "spend_time_after_op108",
		); err != nil {
			// A custom PVF mismatch must not prevent login or replace the native
			// op108 state with a half-initialized scene.
			s.logGameEvent(session, "game-mysterious-ruins-schedule-deferred", "error", err)
		}
		// Replacing the native event table invalidates its attendance snapshot.
		session.monthlyAttendance = currentMonthlyAttendanceProjection{}
		session.weeklyAttendance = currentWeeklyAttendanceProjection{}
	}
	// Install the directory before the selected online owner's state. The
	// original four-plus-four panel is 228/561; 2347/1206 is a different HUD.
	if catalog.nativePCRoom != nil {
		if err := s.sendCurrentPCRoomTimePoint(session, catalog, 0, -1); err != nil {
			return descriptorSent, err
		}
	} else if err := s.sendGameUpperRawClass(session, currentSpendTimeProgressMsgID, progressBody, 0); err != nil {
		return descriptorSent, err
	}
	// Events 181/282/2449 own the Anton, Luke and Fiend War cards in the native
	// ESC/team-raid window. Their authoritative channels may already be
	// connected, but the cards remain hidden until the matching narrow schedule
	// records are applied after the final op108 catalog.
	//
	// 本树刻意比参考实现多推一条：参考版只发 282/2449 两档，而 181 的记录里
	// raw[0]/raw[1] 是账号/角色**本周已用**次数——客户端把这两项渲染成
	// "账号周入场次数 / 角色周入场次数"，且显示的是 (上限 - 已用)。少了 181
	// 这条记录，已经进过本的玩家会看到"上限/上限"，而 op1303 的入场门禁却按
	// 同一个计数拒绝他。sendCurrentRaidWeeklySchedules 一次把三档都发出去，
	// 计数直接取自 raid_admission_counter 的费用账本。
	if err := s.sendCurrentRaidWeeklySchedules(session, "spend_time_after_op108"); err != nil {
		return descriptorSent, err
	}
	// The current client consumes the activity collection and weekly-state
	// projection before it reads the per-raid daily availability table. Keep
	// this immediately after all op1206 records so Anton, Luke and Fiend War
	// observe the final post-op356 event table before evaluating availability.
	if err := s.sendCurrentRaidDailyAvailability(session, "spend_time_after_op1206"); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentJoustOpeningState(session); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentJoustOpeningRoster(session); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentMonthlyAttendance(session, false); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentWeeklyAttendance(session, false, false); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentReturneeState(session, false); err != nil {
		return descriptorSent, err
	}
	if err := s.sendCurrentLevelAiming(session); err != nil {
		s.logGameEvent(session, "game-level-aiming-refresh-deferred", "error", err)
	}
	return descriptorSent, nil
}

func currentSpendTimeReachedStages(definition dnfonlineevent.Definition, onlineSeconds uint64) uint32 {
	var completed uint32
	for _, stage := range definition.Stages {
		if onlineSeconds < stage.RequiredSeconds {
			break
		}
		completed++
	}
	return completed
}

func currentSpendTimeClaimedStages(definition dnfonlineevent.Definition, snapshot dnfonlineevent.Snapshot) uint32 {
	claimed := make(map[string]struct{}, len(snapshot.ClaimedStageIDs))
	for _, stageID := range snapshot.ClaimedStageIDs {
		claimed[stageID] = struct{}{}
	}
	var completed uint32
	for _, stage := range definition.Stages {
		if _, ok := claimed[stage.ID]; !ok {
			break
		}
		completed++
	}
	return completed
}

func (s *Service) claimCurrentSpendTimeUnlockedStages(
	ctx context.Context,
	owner *dnfonlineevent.Owner,
	accountID string,
	characterID string,
	catalog *currentSpendTimeRuntimeCatalog,
	snapshot dnfonlineevent.Snapshot,
	claimedAt time.Time,
) (dnfonlineevent.Snapshot, []dnfonlineevent.CommittedItem, error) {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	if s == nil || owner == nil || accountID == "" || characterID == "" || catalog == nil {
		return snapshot, nil, dnfonlineevent.ErrOwnerUnavailable
	}
	if catalog.nativePCRoom != nil {
		// Preserve the existing durable observation clock; native 228 owns
		// manual mail rewards and cumulative completions from this point on.
		_, err := s.settleCurrentPCRoomTimePoint(ctx, accountID, characterID, catalog, claimedAt, 0, -1)
		return snapshot, nil, err
	}
	claimed := make(map[string]struct{}, len(snapshot.ClaimedStageIDs))
	for _, stageID := range snapshot.ClaimedStageIDs {
		claimed[stageID] = struct{}{}
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return snapshot, nil, err
	}
	items := make([]dnfonlineevent.CommittedItem, 0, currentSpendTimeStageCount)
	for _, stage := range catalog.definition.Stages {
		if snapshot.OnlineSeconds < stage.RequiredSeconds {
			break
		}
		if _, alreadyClaimed := claimed[stage.ID]; alreadyClaimed {
			continue
		}
		claim, claimErr := owner.Claim(ctx, dnfonlineevent.ClaimCommand{
			AccountID:   accountID,
			CharacterID: characterID,
			Definition:  catalog.definition,
			StageID:     stage.ID,
			ClaimedAt:   claimedAt,
			Allocate:    currentSpendTimeItemAllocator(itemCatalog, claimedAt, stage.ID),
		})
		if claimErr != nil {
			return snapshot, items, claimErr
		}
		snapshot = claim.PostSnapshot
		claimed[stage.ID] = struct{}{}
		items = append(items, currentSpendTimeClaimItemsForCharacter(claim, characterID)...)
	}
	return snapshot, items, nil
}

func currentSpendTimeClaimItemsForCharacter(
	claim dnfonlineevent.ClaimResult,
	characterID string,
) []dnfonlineevent.CommittedItem {
	if claim.Replayed || claim.CharacterID != characterID {
		return nil
	}
	items := make([]dnfonlineevent.CommittedItem, len(claim.Items))
	for index, item := range claim.Items {
		items[index] = item
		items[index].RawEntry = append([]byte(nil), item.RawEntry...)
	}
	return items
}

func currentSpendTimeItemAllocator(
	catalog *pvfDungeonDropCatalog,
	grantedAt time.Time,
	stageID string,
) dnfonlineevent.ItemAllocator {
	return func(record *dnfrepo.InventoryRecord, reward dnfonlineevent.ItemReward) (dnfonlineevent.CommittedItem, error) {
		if catalog == nil || record == nil || reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 ||
			reward.Count <= 0 || reward.Count > math.MaxUint32 {
			return dnfonlineevent.CommittedItem{}, errDungeonPickupItemInvalid
		}
		definition, err := catalog.ResolveItem(uint32(reward.ItemID))
		if err != nil {
			return dnfonlineevent.CommittedItem{}, err
		}
		definition, err = currentPVFItemDefinitionForGrantAt(definition, grantedAt.UTC())
		if err != nil {
			return dnfonlineevent.CommittedItem{}, err
		}
		slot, err := addCurrentDungeonPickupToInventory(record, definition, uint32(reward.Count))
		if err != nil {
			return dnfonlineevent.CommittedItem{}, err
		}
		key := currentCeraShopInventorySlotKey(definition.InventoryListType, int16(slot))
		stack, found := record.Slots[key]
		if !found || stack.ItemID != reward.ItemID || stack.Count < reward.Count {
			return dnfonlineevent.CommittedItem{}, errDungeonPickupItemInvalid
		}
		entry := currentItemListEntryFromStack(definition.InventoryListType, int16(slot), stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 5)
		}
		if strings.TrimSpace(stack.Extra["source"]) == "" {
			stack.Extra["source"] = "online_event_spend_time"
		}
		stack.Extra["last_grant_source"] = "online_event_spend_time"
		stack.Extra["last_grant_event_id"] = strconv.FormatUint(uint64(currentSpendTimeEventID), 10)
		stack.Extra["last_grant_stage_id"] = stageID
		record.Slots[key] = stack
		return dnfonlineevent.CommittedItem{
			SlotKey: key, SlotIndex: slot, ItemID: reward.ItemID,
			Delta: reward.Count, PostCount: stack.Count,
			RawEntry: append([]byte(nil), entry.data[:]...),
		}, nil
	}
}

func (s *Service) sendCurrentSpendTimeRewardItemUpdates(
	session *gameSession,
	items []dnfonlineevent.CommittedItem,
	source string,
) error {
	if len(items) == 0 {
		return nil
	}
	entries := make([]currentItemListEntry, 0, len(items))
	refreshMedals := false
	for _, item := range items {
		if item.ItemID <= 0 || item.ItemID > math.MaxUint32 || item.PostCount < item.Delta ||
			len(item.RawEntry) != currentItemListEntryWireSize ||
			binary.LittleEndian.Uint16(item.RawEntry[:2]) != item.SlotIndex ||
			binary.LittleEndian.Uint32(item.RawEntry[2:6]) != uint32(item.ItemID) {
			return fmt.Errorf("current spend-time committed item has invalid wire receipt: %+v", item)
		}
		if listType, _, ok := parseSceneInventorySlotKey(item.SlotKey); ok && listType == currentGuildMedalInventoryListType {
			refreshMedals = true
			continue
		}
		var entry currentItemListEntry
		copy(entry.data[:], item.RawEntry)
		entries = append(entries, entry)
	}
	body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, entries)
	s.logGameEvent(session, "game-spend-time-reward-item-update-send",
		"source", source,
		"event_id", currentSpendTimeEventID,
		"entry_count", len(entries),
		"body_len", len(body),
		"body_source", "online_event_atomic_claim_receipts_op14_raw77")
	if len(entries) > 0 {
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return err
		}
	}
	if refreshMedals {
		return s.sendCurrentGuardianGemList38Refresh(session, source)
	}
	return nil
}

func (s *Service) armCurrentSpendTimeTickLocked(session *gameSession) error {
	if s == nil || session == nil || session.spendTime.characterID == 0 || session.spendTime.catalog == nil {
		return nil
	}
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return errGameplayTimeQueueUnavailable
	}
	characterID, characterGeneration, err := gameSessionCharacterEventIdentity(session, session.spendTime.characterID)
	if err != nil {
		return err
	}
	timerName := fmt.Sprintf("dnf-spend-time:%s:%d", session.connID, characterID)
	generation := session.spendTime.generation
	session.spendTime.timerName = timerName
	return queue.ScheduleAfter(timerName, currentSpendTimeObserveTick, func(time.Time) {
		err := s.postGameSessionCharacterEvent(
			session,
			"spend-time-observation-tick",
			characterID,
			characterGeneration,
			func() error {
				s.runCurrentSpendTimeTick(session, generation)
				return nil
			},
		)
		if err != nil && !isClosedGameSessionEventError(err) {
			s.logPacketEvent("game-session-event-submit-failed",
				"conn_id", session.connID,
				"source", "spend-time-observation-tick",
				"char_id", characterID,
				"character_generation", characterGeneration,
				"error", err)
		}
	})
}

func (s *Service) runCurrentSpendTimeTick(session *gameSession, generation uint64) {
	if s == nil || session == nil {
		return
	}
	now := s.gameplayNow()
	session.spendTime.mu.Lock()
	defer session.spendTime.mu.Unlock()
	if session.spendTime.characterID == 0 || session.spendTime.catalog == nil ||
		session.spendTime.generation != generation {
		return
	}
	settlement, err := s.settleCurrentSpendTimeClockLocked(session, now)
	if err != nil {
		s.logGameEvent(session, "game-spend-time-observation-deferred",
			"char_id", session.spendTime.characterID,
			"generation", generation,
			"error", err)
	} else if session.spendTime.catalog.nativePCRoom != nil {
		if settlement.claimErr == nil {
			if sendErr := s.sendCurrentPCRoomTimePoint(session, session.spendTime.catalog, 0, -1); sendErr != nil {
				s.logGameEvent(session, "game-pcroom-timepoint-refresh-failed", "error", sendErr)
			}
		}
	} else if settlement.changed {
		body, buildErr := buildCurrentSpendTimeProgressBody(
			session.spendTime.onlineSeconds,
			session.spendTime.completedStages,
		)
		if buildErr != nil {
			s.logGameEvent(session, "game-spend-time-progress-build-failed", "error", buildErr)
		} else if sendErr := s.sendGameUpperRawClass(session, currentSpendTimeProgressMsgID, body, 0); sendErr != nil {
			s.logGameEvent(session, "game-spend-time-progress-send-failed", "error", sendErr)
		}
		if sendErr := s.sendCurrentSpendTimeRewardItemUpdates(
			session,
			settlement.rewardItems,
			"online_observation_tick",
		); sendErr != nil {
			s.logGameEvent(session, "game-spend-time-reward-item-update-failed", "error", sendErr)
		}
	}
	if settlement.claimErr != nil {
		s.logGameEvent(session, "game-spend-time-auto-award-deferred",
			"char_id", session.spendTime.characterID,
			"online_seconds", session.spendTime.onlineSeconds,
			"error", settlement.claimErr)
	}
	if err := s.sendCurrentMonthlyAttendance(session, false); err != nil {
		s.logGameEvent(session, "game-monthly-attendance-refresh-deferred", "error", err)
	}
	if err := s.sendCurrentWeeklyAttendance(session, false, false); err != nil {
		s.logGameEvent(session, "game-weekly-attendance-refresh-deferred", "error", err)
	}
	if err := s.sendCurrentReturneeState(session, false); err != nil {
		s.logGameEvent(session, "game-returnee-refresh-deferred", "error", err)
	}
	// 参考实现在这里调 item_lock_snapshot.go 的 refreshCurrentExpiredItemLocks。
	// 本树的 itemlock 是**另一套锁模型**（排序锁 + 账号金库锁，`sort_lock.go` /
	// `sort_lock_account_cargo.go`），没有参考版那套 72 小时 `UnlockWait` /
	// `equipment_lock_unlock_at` / HasExpiredLocks / ReconcileExpired，所以这里
	// 刻意不调用任何等价物：在活动 tick 里凭空造一个"过期解锁"语义，会把本树
	// 已有的排序锁状态机搅乱。真要那条语义，应当整套移植 itemlock 的 expiry.go
	// 并在 game_aligned.go 的登录路径上调用，而不是挂在活动时钟上。
	if err := s.sendCurrentLevelAiming(session); err != nil {
		s.logGameEvent(session, "game-level-aiming-refresh-deferred", "error", err)
	}
	if err := s.armCurrentSpendTimeTickLocked(session); err != nil {
		s.logGameEvent(session, "game-spend-time-observation-rearm-deferred",
			"char_id", session.spendTime.characterID,
			"generation", generation,
			"error", err)
	}
}

func (s *Service) settleCurrentSpendTimeClockLocked(
	session *gameSession,
	now time.Time,
) (currentSpendTimeSettlementResult, error) {
	if session == nil || session.spendTime.characterID == 0 || session.spendTime.catalog == nil ||
		session.spendTime.anchor.IsZero() {
		return currentSpendTimeSettlementResult{}, nil
	}
	intervalTo := now.Truncate(time.Second)
	if !session.spendTime.anchor.Before(intervalTo) {
		return currentSpendTimeSettlementResult{}, nil
	}
	owner, err := s.currentSpendTimeOwner()
	if err != nil {
		return currentSpendTimeSettlementResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	// Labor Day has its own interval union and mail receipts. Settle before
	// advancing the shared observation cursor, including logout and 06:00.
	if err := s.settleCurrentLaborDay(ctx, session, session.spendTime.catalog.laborDay, session.spendTime.anchor, intervalTo, false); err != nil {
		return currentSpendTimeSettlementResult{}, err
	}
	definition := session.spendTime.catalog.definition
	characterID := strconv.Itoa(int(session.spendTime.characterID))
	cursor := session.spendTime.anchor
	previousCompleted := session.spendTime.completedStages
	changed := false
	rewardItems := make([]dnfonlineevent.CommittedItem, 0, currentSpendTimeStageCount)
	var snapshot dnfonlineevent.Snapshot
	var claimErr error
	for cursor.Before(intervalTo) {
		nextBoundary := currentSpendTimeNextServiceBoundary(definition, cursor)
		segmentEnd := intervalTo
		endsAtBoundary := false
		if !nextBoundary.After(segmentEnd) {
			segmentEnd = nextBoundary
			endsAtBoundary = true
		}
		result, err := owner.Observe(ctx, dnfonlineevent.ObserveCommand{
			AccountID:    session.spendTime.accountID,
			CharacterID:  characterID,
			Definition:   definition,
			IntervalFrom: cursor,
			IntervalTo:   segmentEnd,
		})
		if err != nil {
			return currentSpendTimeSettlementResult{}, err
		}
		changed = changed || result.Changed
		claimAt := segmentEnd
		if endsAtBoundary {
			// Claim the just-finished service day before a following Observe or
			// Status resets its daily ledger at exactly 06:00.
			claimAt = segmentEnd.Add(-time.Nanosecond)
		}
		var segmentItems []dnfonlineevent.CommittedItem
		snapshot, segmentItems, claimErr = s.claimCurrentSpendTimeUnlockedStages(
			ctx,
			owner,
			session.spendTime.accountID,
			characterID,
			session.spendTime.catalog,
			result.Snapshot,
			claimAt,
		)
		rewardItems = append(rewardItems, segmentItems...)
		session.spendTime.onlineSeconds = snapshot.OnlineSeconds
		session.spendTime.completedStages = currentSpendTimeClaimedStages(definition, snapshot)
		if claimErr != nil {
			// Do not cross the reset boundary until the old day's unlocked
			// rewards are durably settled. Re-observing the retained interval is
			// idempotent because the owner stores an interval union.
			if !endsAtBoundary {
				session.spendTime.anchor = segmentEnd
			}
			return currentSpendTimeSettlementResult{
				changed:     changed || previousCompleted != session.spendTime.completedStages || len(rewardItems) != 0,
				rewardItems: rewardItems,
				claimErr:    claimErr,
			}, nil
		}
		session.spendTime.anchor = segmentEnd
		cursor = segmentEnd

		if endsAtBoundary && !cursor.Before(intervalTo) {
			priorDate := snapshot.CalendarDate
			snapshot, err = owner.Status(ctx, dnfonlineevent.StatusCommand{
				AccountID:   session.spendTime.accountID,
				CharacterID: characterID,
				Definition:  definition,
				ObservedAt:  segmentEnd,
			})
			if err != nil {
				return currentSpendTimeSettlementResult{}, err
			}
			changed = changed || snapshot.CalendarDate != priorDate
			session.spendTime.onlineSeconds = snapshot.OnlineSeconds
			session.spendTime.completedStages = currentSpendTimeClaimedStages(definition, snapshot)
		}
	}
	return currentSpendTimeSettlementResult{
		changed:     changed || previousCompleted != session.spendTime.completedStages || len(rewardItems) != 0,
		rewardItems: rewardItems,
		claimErr:    claimErr,
	}, nil
}

func currentSpendTimeNextServiceBoundary(
	definition dnfonlineevent.Definition,
	at time.Time,
) time.Time {
	location := definition.Calendar
	if location == nil {
		location = dnfonlineevent.ChinaCalendar
	}
	boundary := dnfonlineevent.DailyBoundary{Hour: currentSpendTimeResetHour}
	if definition.Boundary != nil {
		boundary = *definition.Boundary
	}
	local := at.In(location)
	next := time.Date(
		local.Year(),
		local.Month(),
		local.Day(),
		boundary.Hour,
		boundary.Minute,
		boundary.Second,
		0,
		location,
	)
	if !at.Before(next) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

func (s *Service) stopCurrentSpendTimeClock(session *gameSession, source string) {
	if s == nil || session == nil {
		return
	}
	session.spendTime.mu.Lock()
	if session.spendTime.characterID == 0 || session.spendTime.catalog == nil {
		session.spendTime.mu.Unlock()
		return
	}
	settlement, settleErr := s.settleCurrentSpendTimeClockLocked(session, s.gameplayNow())
	characterID := session.spendTime.characterID
	timerName := session.spendTime.timerName
	session.spendTime.generation++
	session.spendTime.characterID = 0
	session.spendTime.characterGeneration = 0
	session.spendTime.initializedCharacterGeneration = 0
	session.spendTime.accountID = ""
	session.spendTime.anchor = time.Time{}
	session.spendTime.timerName = ""
	session.spendTime.onlineSeconds = 0
	session.spendTime.completedStages = 0
	session.spendTime.catalog = nil
	session.spendTime.mu.Unlock()
	if timerName != "" {
		if queue := s.ensureGameplayTimeQueue(); queue != nil {
			queue.Cancel(timerName)
		}
	}
	if settleErr != nil {
		s.logGameEvent(session, "game-spend-time-stop-settlement-failed",
			"char_id", characterID,
			"source", source,
			"error", settleErr)
	}
	if settlement.claimErr != nil {
		s.logGameEvent(session, "game-spend-time-stop-auto-award-deferred",
			"char_id", characterID,
			"source", source,
			"error", settlement.claimErr)
	}
}
