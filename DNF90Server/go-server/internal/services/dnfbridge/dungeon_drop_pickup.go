package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfepicbook "longheng.io/server/internal/modules/dnf/epicbook"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// Opcode 39 is directional: C2S op39 reports monster death, while current
	// class0/S2C op39 is the ordinary scene-item pickup result.
	currentDungeonPickupResultOpcode     uint16 = 39
	currentDungeonPickupResultSize              = 19
	currentDungeonGoldPickupEffectOpcode uint16 = 557
	currentDungeonGoldPickupEffectSize          = 2
	currentDungeonPickupQuickSlotStart          = int16(3)
	currentDungeonPickupQuickSlotEnd            = int16(8)

	// Main inventory standard categories (equipment 9-64, material 65-120,
	// consumable 121-176, quest 177-232) each span 7 rows of 8 slots. New
	// characters unlock the first 4 rows (32 slots); the cash-shop expansion
	// voucher unlocks one row (8 slots) per purchase, up to 3 rows (24 slots).
	// Locked expansion slots must never be used as placement targets, otherwise
	// items land in rows the client does not render.
	currentMainInventoryBaseSlotCount     int16 = 32
	currentMainInventoryCategorySlotCount int16 = 56
)

// currentMainInventoryUnlockedEnd returns the last unlocked slot of a main
// inventory standard category. slotStart is the category's PVF first slot
// (equipment 9, material 65, consumable 121, quest 177); mainSlotCount is
// CharacterContainerState.MainSlotCount (0/8/16/24, i.e. 0/1/2/3 expanded rows).
func currentMainInventoryUnlockedEnd(slotStart int16, mainSlotCount uint16) int16 {
	return slotStart + currentMainInventoryBaseSlotCount - 1 + int16(mainSlotCount)
}

// limitCurrentMainInventoryDefinitionSlots clamps a main-inventory standard
// category's SlotEnd to the currently unlocked last slot so locked expansion
// slots are never valid placement targets. Non-56-slot ranges such as the
// quick slots (3-8) or special materials (345-359) are left unchanged.
// currentMainInventoryClampedSlotEnd returns slotEnd clamped to the currently
// unlocked last slot when the range is a main-inventory standard category.
func currentMainInventoryClampedSlotEnd(slotStart, slotEnd int16, mainSlotCount uint16) int16 {
	if slotStart >= currentDungeonPickupQuickSlotStart &&
		slotEnd-slotStart+1 == currentMainInventoryCategorySlotCount {
		unlockedEnd := currentMainInventoryUnlockedEnd(slotStart, mainSlotCount)
		if slotEnd > unlockedEnd {
			return unlockedEnd
		}
	}
	return slotEnd
}

// limitCurrentMainInventoryDefinitionSlots clamps a main-inventory standard
// category's SlotEnd to the currently unlocked last slot so locked expansion
// slots are never valid placement targets. Non-56-slot ranges such as the
// quick slots (3-8) or special materials (345-359) are left unchanged.
func limitCurrentMainInventoryDefinitionSlots(definition dungeonDropItemDefinition, mainSlotCount uint16) dungeonDropItemDefinition {
	definition.SlotEnd = currentMainInventoryClampedSlotEnd(definition.SlotStart, definition.SlotEnd, mainSlotCount)
	return definition
}

func buildCurrentEpicBookPickupResultBody(dropObjectKey uint32, pickerActorObjectKey uint16, temporarySlot uint16) ([]byte, error) {
	if dropObjectKey == 0 || pickerActorObjectKey == 0 {
		return nil, fmt.Errorf("%w: epic fragment drop=%d picker=%d", errDungeonPickupResponseInvalid, dropObjectKey, pickerActorObjectKey)
	}
	if temporarySlot > 0 {
		return buildCurrentDungeonNormalPickupResultBody(dropObjectKey, pickerActorObjectKey, temporarySlot)
	}
	var writer packetWriter
	writer.writeUint32(dropObjectKey)
	writer.writeUint16(pickerActorObjectKey)
	writer.writeBytes(make([]byte, 8))
	writer.writeUint16(0)
	writer.writeUint16(0)
	writer.writeByte(0)
	body := writer.bytes()
	if len(body) != currentDungeonPickupResultSize {
		return nil, fmt.Errorf("%w: epic fragment body_len=%d", errDungeonPickupResponseInvalid, len(body))
	}
	return body, nil
}

func buildCurrentEpicBookTemporarySlotClearBody(temporarySlot uint16) ([]byte, error) {
	if temporarySlot == 0 || temporarySlot > math.MaxInt16 {
		return nil, fmt.Errorf("%w: temporary_slot=%d", errDungeonPickupResponseInvalid, temporarySlot)
	}
	var empty currentItemListEntry
	empty.patchCore(int16(temporarySlot), math.MaxUint32, 0)
	return buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{empty}), nil
}

func currentEpicBookTemporarySlot(inventory dnfrepo.InventoryRecord, first, last int16) (uint16, bool) {
	if first <= 0 || last < first {
		return 0, false
	}
	for slot := first; slot <= last; slot++ {
		stack, occupied := inventory.Slots[currentDungeonPickupMainSlotKey(slot)]
		if !occupied || stack.ItemID <= 0 || stack.Count <= 0 {
			return uint16(slot), true
		}
	}
	return 0, false
}

var (
	errDungeonPickupRuntimeMissing     = errors.New("dnf dungeon pickup runtime is missing")
	errDungeonPickupTransactionMissing = errors.New("dnf dungeon pickup inventory transaction is missing")
	errDungeonPickupInventoryMissing   = errors.New("dnf dungeon pickup inventory record is missing")
	errDungeonPickupInventoryFull      = errors.New("dnf dungeon pickup inventory category is full")
	errDungeonPickupItemInvalid        = errors.New("dnf dungeon pickup item is invalid")
	errDungeonPickupStackLimit         = errors.New("dnf dungeon pickup exceeds real PVF stack limit")
	errDungeonPickupResponseInvalid    = errors.New("dnf dungeon pickup response values are invalid")
	errDungeonPickupDropStateInvalid   = errors.New("dnf dungeon pickup drop state is invalid")
)

type currentDungeonPickupKind byte

const (
	currentDungeonPickupItem currentDungeonPickupKind = iota + 1
	currentDungeonPickupGold
	currentDungeonPickupDiscardRestore
	currentDungeonPickupEpicFragment
)

type currentDungeonPickupPlan struct {
	kind                 currentDungeonPickupKind
	runtime              *runtimeDungeonState
	drop                 *runtimeDungeonDrop
	dropSnapshot         runtimeDungeonDrop
	attempt              uint64
	request              dungeoncmd.GetItemRequest
	room                 runtimeDungeonRoomKey
	roomLabel            string
	mapID                int64
	pickerActorObjectKey uint16
	characterID          uint16
	characterGeneration  uint64
	accountID            string
	repositories         dnfrepo.Group
	epicEntry            dnfepicbook.Entry
}

type currentDungeonPickupResult struct {
	committed       bool
	destinationSlot uint16
	goldAfter       int64
	responseBody    []byte
	itemUpdateBody  []byte
	goldEffectBody  []byte
	epicClearBody   []byte
	epicSyncBody    []byte
	assetOwner      string
	failureReason   string
	responseReason  string
	elapsed         time.Duration
	err             error
}

func buildCurrentDungeonNormalPickupResultBody(
	dropObjectKey uint32,
	pickerActorObjectKey uint16,
	destinationSlot uint16,
) ([]byte, error) {
	if dropObjectKey == 0 || pickerActorObjectKey == 0 || destinationSlot == 0 {
		return nil, fmt.Errorf(
			"%w: drop=%d picker=%d destination=%d",
			errDungeonPickupResponseInvalid,
			dropObjectKey,
			pickerActorObjectKey,
			destinationSlot,
		)
	}
	var writer packetWriter
	writer.writeUint32(dropObjectKey)
	writer.writeUint16(pickerActorObjectKey)
	writer.writeBytes(make([]byte, 8)) // read by current EXE; semantics unproved, default constructor state
	writer.writeUint16(pickerActorObjectKey)
	writer.writeUint16(destinationSlot)
	writer.writeByte(0) // read by current EXE; not consumed by the visible permission/UI path
	body := writer.bytes()
	if len(body) != currentDungeonPickupResultSize {
		return nil, fmt.Errorf("%w: body_len=%d", errDungeonPickupResponseInvalid, len(body))
	}
	return body, nil
}

func buildCurrentDungeonGoldPickupResultBody(
	dropObjectKey uint32,
	pickerActorObjectKey uint16,
) ([]byte, error) {
	if dropObjectKey == 0 || pickerActorObjectKey == 0 {
		return nil, fmt.Errorf(
			"%w: drop=%d picker=%d",
			errDungeonPickupResponseInvalid,
			dropObjectKey,
			pickerActorObjectKey,
		)
	}
	var writer packetWriter
	writer.writeUint32(dropObjectKey)
	writer.writeUint16(pickerActorObjectKey)
	writer.writeByte(1) // retained compatibility tail; current op39 itself only consumes the object key
	writer.writeBytes(make([]byte, 7))
	writer.writeUint16(pickerActorObjectKey)
	writer.writeUint16(0) // gold has no destination inventory slot
	writer.writeByte(1)   // retained compatibility tail; op557 owns the current-client gold effect
	body := writer.bytes()
	if len(body) != currentDungeonPickupResultSize {
		return nil, fmt.Errorf("%w: body_len=%d", errDungeonPickupResponseInvalid, len(body))
	}
	return body, nil
}

func buildCurrentDungeonGoldPickupEffectBody(amount uint32) ([]byte, error) {
	if amount == 0 || amount > math.MaxUint16 {
		return nil, fmt.Errorf(
			"%w: gold_pickup_amount=%d",
			errDungeonPickupResponseInvalid,
			amount,
		)
	}
	var writer packetWriter
	writer.writeUint16(uint16(amount))
	body := writer.bytes()
	if len(body) != currentDungeonGoldPickupEffectSize {
		return nil, fmt.Errorf("%w: gold_effect_body_len=%d", errDungeonPickupResponseInvalid, len(body))
	}
	return body, nil
}

// handleCurrentDungeonPickup validates and atomically claims exactly the ground
// object named by the client's op43 request. Every request therefore preserves
// stock manual-pickup behavior and leaves all other room drops on the ground.
func (s *Service) handleCurrentDungeonPickup(session *gameSession, body []byte) error {
	request, err := dungeoncmd.DecodeGetItemRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"body_len", len(body),
			"reason", "current_exe_op43_ordinary_request_malformed",
			"error", err)
		return nil
	}
	if request.FixedZero != 0 {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"fixed_byte", request.FixedZero,
			"reason", "current_exe_op43_fixed_zero_mismatch")
		return nil
	}
	if session == nil {
		return nil
	}

	session.dungeon.mu.Lock()
	locked := true
	defer func() {
		if locked {
			session.dungeon.mu.Unlock()
		}
	}()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.Room == nil || runtime.DropOwner == nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"reason", "dungeon_drop_runtime_missing",
			"error", errDungeonPickupRuntimeMissing)
		return nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"reason", "dungeon_scene_missing",
			"error", errDungeonPickupRuntimeMissing)
		return nil
	}
	pickerActorObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	drop, err := runtime.DropOwner.owned(
		request.DropObjectKey,
		runtimeDungeonRoomKeyFromScene(scene),
		pickerActorObjectKey,
	)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", request.DropObjectKey,
			"picker_actor_object_key", pickerActorObjectKey,
			"reason", "runtime_drop_owner_rejected",
			"error", err)
		return nil
	}
	if drop.Status == runtimeDungeonDropConsumed {
		if len(drop.PickupResponseBody) != currentDungeonPickupResultSize {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"drop_object_key", drop.ObjectKey,
				"reason", "consumed_drop_response_missing",
				"error", fmt.Errorf("%w: response_len=%d", errDungeonPickupDropStateInvalid, len(drop.PickupResponseBody)))
			return nil
		}
		responseBody := append([]byte(nil), drop.PickupResponseBody...)
		itemUpdateBody := append([]byte(nil), drop.PickupItemUpdateBody...)
		epicClearBody := append([]byte(nil), drop.PickupEpicBookClearBody...)
		epicSyncBody := append([]byte(nil), drop.PickupEpicBookBody...)
		goldAfter := drop.GoldAfter
		isGold := drop.isGold()
		s.logGameEvent(session, "game-dungeon-pickup-replayed",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"gold_after", goldAfter,
			"destination_slot", drop.DestinationSlot,
			"request_context", request.ObjectContext,
			"response_opcode", currentDungeonPickupResultOpcode)
		session.dungeon.mu.Unlock()
		locked = false
		return s.sendCurrentDungeonPickupProjection(
			session,
			responseBody,
			nil,
			itemUpdateBody,
			epicClearBody,
			epicSyncBody,
			isGold,
			goldAfter,
			true,
		)
	}
	if drop.Status == runtimeDungeonDropPending {
		s.logGameEvent(session, "game-dungeon-pickup-pending-replay",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"attempt", drop.PickupAttempt,
			"reason", "repository_commit_in_progress_without_session_event_loop_ownership")
		return nil
	}
	if drop.Status != runtimeDungeonDropAvailable {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", drop.ObjectKey,
			"status", drop.Status,
			"reason", "runtime_drop_state_invalid",
			"error", errDungeonPickupDropStateInvalid)
		return nil
	}

	repositories, repositoriesOK := s.repositoryGroup()
	kind := currentDungeonPickupItem
	missingReason := "character_items_transaction_missing"
	var epicEntry dnfepicbook.Entry
	if drop.isGold() {
		kind = currentDungeonPickupGold
		missingReason = "character_gold_transaction_missing"
		repositoriesOK = repositoriesOK && repositories.CharacterAssets != nil
	} else if drop.DiscardOrigin != nil {
		kind = currentDungeonPickupDiscardRestore
		missingReason = "discard_origin_account_assets_transaction_missing"
		repositoriesOK = repositoriesOK && repositories.AccountAssets != nil
	} else if repositoriesOK && repositories.EpicBookCraft != nil {
		if catalog, catalogErr := s.currentEpicBookCatalog(); catalogErr == nil {
			if entry, isFragment := catalog.EntryByFragment(drop.Item.ItemID); isFragment {
				kind = currentDungeonPickupEpicFragment
				epicEntry = entry
				missingReason = "epic_book_fragment_transaction_missing"
			}
		}
		if kind == currentDungeonPickupItem {
			repositoriesOK = repositories.CharacterItems != nil
		}
	} else {
		repositoriesOK = repositoriesOK && repositories.CharacterItems != nil
	}
	if !repositoriesOK {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"reason", missingReason,
			"error", errDungeonPickupTransactionMissing)
		return nil
	}

	dropSnapshot := *drop
	if drop.DiscardOrigin != nil {
		discardOrigin := *drop.DiscardOrigin
		dropSnapshot.DiscardOrigin = &discardOrigin
	}
	drop.PickupAttempt++
	if drop.PickupAttempt == 0 {
		drop.PickupAttempt++
	}
	drop.Status = runtimeDungeonDropPending
	plan := currentDungeonPickupPlan{
		kind:                 kind,
		runtime:              runtime,
		drop:                 drop,
		dropSnapshot:         dropSnapshot,
		attempt:              drop.PickupAttempt,
		request:              request,
		room:                 runtimeDungeonRoomKeyFromScene(scene),
		roomLabel:            scene.Coordinate.String(),
		mapID:                int64(scene.Map.Map.ID),
		pickerActorObjectKey: pickerActorObjectKey,
		characterID:          session.selectedCharacterID,
		characterGeneration:  session.characterGeneration,
		accountID:            strings.TrimSpace(s.accountIDForSession(session)),
		repositories:         repositories,
		epicEntry:            epicEntry,
	}
	s.logGameEvent(session, "game-dungeon-pickup-pending",
		"dungeon_id", runtime.Dungeon.ID,
		"room", plan.roomLabel,
		"map_id", scene.Map.Map.ID,
		"drop_object_key", drop.ObjectKey,
		"item_id", drop.Item.ItemID,
		"amount", drop.Amount,
		"attempt", plan.attempt,
		"persistence", "off_session_event_loop")
	session.dungeon.mu.Unlock()
	locked = false

	if session.events != nil && session.events.loop != nil && session.events.active.Load() {
		go s.runCurrentDungeonPickupPersistence(session, plan)
		return nil
	}
	return s.completeCurrentDungeonPickup(session, plan, s.executeCurrentDungeonPickup(session, plan))
}

func (s *Service) runCurrentDungeonPickupPersistence(session *gameSession, plan currentDungeonPickupPlan) {
	result := s.executeCurrentDungeonPickup(session, plan)
	err := s.postGameSessionCharacterEvent(
		session,
		"dungeon-pickup-persistence-complete",
		plan.characterID,
		plan.characterGeneration,
		func() error {
			return s.completeCurrentDungeonPickup(session, plan, result)
		},
	)
	if err != nil && !isClosedGameSessionEventError(err) {
		s.logPacketEvent("game-session-event-submit-failed",
			"conn_id", session.connID,
			"source", "dungeon-pickup-persistence-complete",
			"char_id", plan.characterID,
			"character_generation", plan.characterGeneration,
			"drop_object_key", plan.dropSnapshot.ObjectKey,
			"attempt", plan.attempt,
			"error", err)
	}
}

func (s *Service) executeCurrentDungeonPickup(
	session *gameSession,
	plan currentDungeonPickupPlan,
) (result currentDungeonPickupResult) {
	started := time.Now()
	defer func() { result.elapsed = time.Since(started) }()
	baseContext := context.Background()
	if session != nil && session.ctx != nil {
		baseContext = session.ctx
	}
	ctx, cancel := context.WithTimeout(baseContext, currentDungeonPickupWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(plan.characterID))
	now := time.Now().UTC()
	// 拾取落点必须落在已解锁的背包行里。角色默认只有 4 行(32 格), 另外 3 行要用
	// 背包扩展券解锁(每张 1 行, 最多 3 行); 未解锁的格子客户端不渲染, 落进去的物品
	// 看起来就"消失"了。赛丽亚商店/任务奖励/通关翻牌都走
	// limitCurrentMainInventoryDefinitionSlots, 只有拾取这条路径漏了(用户 2026-09-12
	// 报"副本内拾取物品会跑到未解锁的槽位")。
	mainSlotCount := s.currentInventoryPlacementMainSlotCount(ctx, nil, characterID)
	dropItem := limitCurrentMainInventoryDefinitionSlots(plan.dropSnapshot.Item, mainSlotCount)

	switch plan.kind {
	case currentDungeonPickupGold:
		result.assetOwner = "character_assets_uow_gold"
		result.failureReason = "character_gold_transaction_failed_drop_remains_available"
		result.responseReason = "gold_pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant"
		result.goldAfter, result.err = grantCurrentDungeonPickupGold(
			ctx,
			plan.repositories.CharacterAssets,
			characterID,
			plan.dropSnapshot.Amount,
			now,
		)
		if result.err != nil {
			return result
		}
		result.committed = true
		result.responseBody, result.err = buildCurrentDungeonGoldPickupResultBody(
			plan.dropSnapshot.ObjectKey,
			plan.pickerActorObjectKey,
		)
		if result.err == nil {
			result.goldEffectBody, _ = buildCurrentDungeonGoldPickupEffectBody(plan.dropSnapshot.Amount)
			result.itemUpdateBody = buildCurrentGoldStateBody(result.goldAfter)
		}
		return result

	case currentDungeonPickupDiscardRestore:
		result.assetOwner = "discard_origin_account_assets_uow"
		result.failureReason = "discard_origin_restore_transaction_failed_drop_remains_available"
		result.responseReason = "discard_origin_pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant"
		origin := plan.dropSnapshot.DiscardOrigin
		if origin == nil {
			result.err = errDungeonPickupDropStateInvalid
			return result
		}
		var itemUpdate currentItemListEntry
		result.destinationSlot, itemUpdate, result.err = restoreCurrentDungeonDiscardedItem(
			ctx,
			plan.repositories.AccountAssets,
			plan.accountID,
			characterID,
			*origin,
			plan.dropSnapshot.Item,
			now,
		)
		if result.err != nil {
			return result
		}
		result.committed = true
		result.itemUpdateBody = buildCurrentItemUpdateBody(
			origin.ListType,
			[]currentItemListEntry{itemUpdate},
		)

	case currentDungeonPickupEpicFragment:
		result.assetOwner = "epic_book_fragment_uow"
		result.failureReason = "epic_book_fragment_transaction_failed_drop_remains_available"
		result.responseReason = "epic_book_pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant"
		if plan.repositories.EpicBookCraft == nil || plan.epicEntry.EquipmentID == 0 {
			result.err = errDungeonPickupTransactionMissing
			return result
		}
		if plan.repositories.Inventory != nil {
			if inventory, found, loadErr := plan.repositories.Inventory.Load(ctx, characterID); loadErr == nil && found {
				result.destinationSlot, _ = currentEpicBookTemporarySlot(
					inventory,
					plan.dropSnapshot.Item.SlotStart,
					plan.dropSnapshot.Item.SlotEnd,
				)
			}
		}
		var balance uint32
		balance, result.err = dnfepicbook.AddFragment(
			ctx,
			plan.repositories.EpicBookCraft,
			plan.accountID,
			characterID,
			plan.epicEntry.EquipmentID,
			plan.dropSnapshot.Amount,
			now,
		)
		if result.err != nil {
			return result
		}
		result.committed = true
		result.responseBody, result.err = buildCurrentEpicBookPickupResultBody(
			plan.dropSnapshot.ObjectKey,
			plan.pickerActorObjectKey,
			result.destinationSlot,
		)
		if result.err != nil {
			return result
		}
		if result.destinationSlot > 0 {
			result.epicClearBody, result.err = buildCurrentEpicBookTemporarySlotClearBody(result.destinationSlot)
			if result.err != nil {
				return result
			}
		}
		result.epicSyncBody, result.err = buildCurrentEpicBookInfoBody(
			1,
			[]currentEpicBookBalanceRow{{FragmentID: plan.epicEntry.FragmentID, Balance: balance}},
		)
		return result

	default:
		var itemUpdate currentItemListEntry
		if sharedSlot, shared := currentDungeonPickupAccountSharedSlot(plan.dropSnapshot.Item.ItemID); shared {
			result.assetOwner = "account_shared_crystal_assets_uow"
			result.failureReason = "account_shared_crystal_transaction_failed_drop_remains_available"
			result.responseReason = "shared_crystal_pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant"
			result.destinationSlot, itemUpdate, result.err = grantCurrentDungeonPickupAccountShared(
				ctx,
				plan.repositories.AccountAssets,
				plan.accountID,
				characterID,
				dropItem,
				plan.dropSnapshot.Amount,
				sharedSlot,
				now,
				plan.dropSnapshot.QualitySeed,
			)
		} else {
			result.assetOwner = "character_items_uow"
			result.failureReason = "character_items_transaction_failed_drop_remains_available"
			result.responseReason = "pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant"
			result.destinationSlot, itemUpdate, result.err = s.grantCurrentDungeonPickupItem(
				ctx,
				plan.repositories.CharacterItems,
				characterID,
				dropItem,
				plan.dropSnapshot.Amount,
				now,
				currentDungeonDiscardReservedSlots(plan.runtime),
				plan.dropSnapshot.QualitySeed,
			)
		}
		if result.err != nil {
			return result
		}
		result.committed = true
		// The op14 envelope must name the list the row was actually written to.
		// A guild medal/gem announced on list 0 leaves the client showing it in
		// the main bag until the next full list rebuild.
		itemUpdateListType := dnfrepo.MainInventoryListType
		if _, shared := currentDungeonPickupAccountSharedSlot(plan.dropSnapshot.Item.ItemID); !shared {
			itemUpdateListType, _, _ = currentDungeonPickupListPlacement(plan.dropSnapshot.Item)
		}
		result.itemUpdateBody = buildCurrentItemUpdateBody(
			itemUpdateListType,
			[]currentItemListEntry{itemUpdate},
		)
	}

	result.responseBody, result.err = buildCurrentDungeonNormalPickupResultBody(
		plan.dropSnapshot.ObjectKey,
		plan.pickerActorObjectKey,
		result.destinationSlot,
	)
	return result
}

func (s *Service) completeCurrentDungeonPickup(
	session *gameSession,
	plan currentDungeonPickupPlan,
	result currentDungeonPickupResult,
) error {
	if session == nil || plan.drop == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	drop := plan.drop
	if drop.Status != runtimeDungeonDropPending || drop.PickupAttempt != plan.attempt {
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-pickup-completion-stale",
			"drop_object_key", plan.dropSnapshot.ObjectKey,
			"attempt", plan.attempt,
			"current_attempt", drop.PickupAttempt,
			"status", drop.Status,
			"committed", result.committed,
			"elapsed_ms", result.elapsed.Milliseconds())
		return nil
	}
	if result.err != nil && !result.committed {
		drop.Status = runtimeDungeonDropAvailable
		session.dungeon.mu.Unlock()
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"dungeon_id", plan.runtime.Dungeon.ID,
			"room", plan.roomLabel,
			"map_id", plan.mapID,
			"drop_object_key", plan.dropSnapshot.ObjectKey,
			"item_id", plan.dropSnapshot.Item.ItemID,
			"amount", plan.dropSnapshot.Amount,
			"attempt", plan.attempt,
			"elapsed_ms", result.elapsed.Milliseconds(),
			"reason", result.failureReason,
			"error", result.err)
		return nil
	}

	drop.Status = runtimeDungeonDropConsumed
	drop.DestinationSlot = result.destinationSlot
	drop.GoldAfter = result.goldAfter
	drop.PickupResponseBody = append([]byte(nil), result.responseBody...)
	drop.PickupItemUpdateBody = append([]byte(nil), result.itemUpdateBody...)
	drop.PickupEpicBookClearBody = append([]byte(nil), result.epicClearBody...)
	drop.PickupEpicBookBody = append([]byte(nil), result.epicSyncBody...)
	currentSceneACK := false
	if session.dungeon.runtime == plan.runtime && plan.runtime.DropOwner != nil &&
		plan.runtime.DropOwner.byObjectKey[plan.dropSnapshot.ObjectKey] == drop {
		if scene, ok := plan.runtime.Session.Scene(); ok {
			currentSceneACK = runtimeDungeonRoomKeyFromScene(scene) == plan.room
		}
	}
	session.dungeon.mu.Unlock()

	if result.err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-response-blocked-after-commit",
			"drop_object_key", plan.dropSnapshot.ObjectKey,
			"item_id", plan.dropSnapshot.Item.ItemID,
			"amount", plan.dropSnapshot.Amount,
			"destination_slot", result.destinationSlot,
			"gold_after", result.goldAfter,
			"attempt", plan.attempt,
			"elapsed_ms", result.elapsed.Milliseconds(),
			"reason", result.responseReason,
			"error", result.err)
		return nil
	}

	s.logGameEvent(session, "game-dungeon-pickup-accepted",
		"dungeon_id", plan.runtime.Dungeon.ID,
		"room", plan.roomLabel,
		"map_id", plan.mapID,
		"drop_object_key", plan.dropSnapshot.ObjectKey,
		"scene_slot", plan.dropSnapshot.SceneSlot,
		"item_id", plan.dropSnapshot.Item.ItemID,
		"amount", plan.dropSnapshot.Amount,
		"destination_slot", result.destinationSlot,
		"gold_after", result.goldAfter,
		"picker_actor_object_key", plan.pickerActorObjectKey,
		"request_context", plan.request.ObjectContext,
		"request_player_x", plan.request.PlayerX,
		"request_player_y", plan.request.PlayerY,
		"request_token0", plan.request.Token0,
		"request_drop_x", plan.request.DropX,
		"request_drop_y", plan.request.DropY,
		"request_token1", plan.request.Token1,
		"request_token2", plan.request.Token2,
		"attempt", plan.attempt,
		"elapsed_ms", result.elapsed.Milliseconds(),
		"response_opcode", currentDungeonPickupResultOpcode,
		"response_body_len", len(result.responseBody),
		"asset_owner", result.assetOwner,
		"item_update_opcode", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"item_update_body_len", len(result.itemUpdateBody),
		"scene_ack", currentSceneACK)
	if !currentSceneACK {
		s.logGameEvent(session, "game-dungeon-pickup-scene-ack-suppressed",
			"drop_object_key", plan.dropSnapshot.ObjectKey,
			"attempt", plan.attempt,
			"reason", "repository_commit_finished_after_dungeon_scene_changed")
	}
	return s.sendCurrentDungeonPickupProjection(
		session,
		result.responseBody,
		result.goldEffectBody,
		result.itemUpdateBody,
		result.epicClearBody,
		result.epicSyncBody,
		plan.kind == currentDungeonPickupGold,
		result.goldAfter,
		currentSceneACK,
	)
}

func (s *Service) sendCurrentDungeonPickupProjection(
	session *gameSession,
	responseBody []byte,
	goldEffectBody []byte,
	itemUpdateBody []byte,
	epicClearBody []byte,
	epicSyncBody []byte,
	isGold bool,
	goldAfter int64,
	sendSceneACK bool,
) error {
	if sendSceneACK {
		if err := s.sendGameUpperRawClass(session, currentDungeonPickupResultOpcode, responseBody, 0); err != nil {
			return err
		}
		if len(goldEffectBody) > 0 {
			if err := s.sendGameUpperRawClass(session, currentDungeonGoldPickupEffectOpcode, goldEffectBody, 0); err != nil {
				return err
			}
		}
	}
	if isGold && goldAfter > 0 && len(itemUpdateBody) == 0 {
		itemUpdateBody = buildCurrentGoldStateBody(goldAfter)
	}
	if len(itemUpdateBody) > 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			itemUpdateBody,
			0,
		); err != nil {
			return err
		}
	}
	if len(epicClearBody) > 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			epicClearBody,
			0,
		); err != nil {
			return err
		}
	}
	if len(epicSyncBody) > 0 {
		return s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, epicSyncBody, 0)
	}
	return nil
}

func (s *Service) handleCurrentDungeonSinglePickupLegacy(session *gameSession, body []byte) error {
	request, err := dungeoncmd.DecodeGetItemRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"body_len", len(body),
			"reason", "current_exe_op43_ordinary_request_malformed",
			"error", err)
		return nil
	}
	if request.FixedZero != 0 {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"fixed_byte", request.FixedZero,
			"reason", "current_exe_op43_fixed_zero_mismatch")
		return nil
	}
	if session == nil {
		return nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()
	runtime := session.dungeon.runtime
	if runtime == nil || runtime.Session == nil || runtime.Room == nil || runtime.DropOwner == nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"reason", "dungeon_drop_runtime_missing",
			"error", errDungeonPickupRuntimeMissing)
		return nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", request.DropObjectKey,
			"reason", "dungeon_scene_missing",
			"error", errDungeonPickupRuntimeMissing)
		return nil
	}
	pickerActorObjectKey := currentSceneActorObjectKey(session.selectedCharacterID)
	drop, err := runtime.DropOwner.owned(
		request.DropObjectKey,
		runtimeDungeonRoomKeyFromScene(scene),
		pickerActorObjectKey,
	)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", request.DropObjectKey,
			"picker_actor_object_key", pickerActorObjectKey,
			"reason", "runtime_drop_owner_rejected",
			"error", err)
		return nil
	}
	if drop.Status == runtimeDungeonDropConsumed {
		if len(drop.PickupResponseBody) != currentDungeonPickupResultSize {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"drop_object_key", drop.ObjectKey,
				"reason", "consumed_drop_response_missing",
				"error", fmt.Errorf("%w: response_len=%d", errDungeonPickupDropStateInvalid, len(drop.PickupResponseBody)))
			return nil
		}
		s.logGameEvent(session, "game-dungeon-pickup-replayed",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"gold_after", drop.GoldAfter,
			"destination_slot", drop.DestinationSlot,
			"request_context", request.ObjectContext,
			"request_player_x", request.PlayerX,
			"request_player_y", request.PlayerY,
			"request_drop_x", request.DropX,
			"request_drop_y", request.DropY,
			"response_opcode", currentDungeonPickupResultOpcode)
		if err := s.sendGameUpperRawClass(
			session,
			currentDungeonPickupResultOpcode,
			append([]byte(nil), drop.PickupResponseBody...),
			0,
		); err != nil {
			return err
		}
		if drop.isGold() && drop.GoldAfter > 0 {
			return s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				buildCurrentGoldStateBody(drop.GoldAfter),
				0,
			)
		}
		if len(drop.PickupItemUpdateBody) > 0 {
			return s.sendGameUpperRawClass(
				session,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				append([]byte(nil), drop.PickupItemUpdateBody...),
				0,
			)
		}
		if len(drop.PickupEpicBookClearBody) > 0 {
			if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), append([]byte(nil), drop.PickupEpicBookClearBody...), 0); err != nil {
				return err
			}
		}
		if len(drop.PickupEpicBookBody) > 0 {
			return s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, append([]byte(nil), drop.PickupEpicBookBody...), 0)
		}
		return nil
	}
	if drop.Status != runtimeDungeonDropAvailable {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", drop.ObjectKey,
			"status", drop.Status,
			"reason", "runtime_drop_state_invalid",
			"error", errDungeonPickupDropStateInvalid)
		return nil
	}
	if drop.isGold() {
		responseBody, err := buildCurrentDungeonGoldPickupResultBody(drop.ObjectKey, pickerActorObjectKey)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"amount", drop.Amount,
				"reason", "gold_pickup_response_invariant_failed_before_commit",
				"error", err)
			return nil
		}
		goldEffectBody, err := buildCurrentDungeonGoldPickupEffectBody(drop.Amount)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-pickup-effect-skipped",
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"amount", drop.Amount,
				"reason", "current_exe_op557_u16_amount_out_of_range_absolute_op14_remains_authoritative",
				"error", err)
		}
		repositories, ok := s.repositoryGroup()
		if !ok || repositories.CharacterAssets == nil {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"amount", drop.Amount,
				"reason", "character_gold_transaction_missing",
				"error", errDungeonPickupTransactionMissing)
			return nil
		}
		characterID := strconv.Itoa(int(session.selectedCharacterID))
		goldAfter, err := grantCurrentDungeonPickupGold(
			context.Background(),
			repositories.CharacterAssets,
			characterID,
			drop.Amount,
			time.Now().UTC(),
		)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"amount", drop.Amount,
				"reason", "character_gold_transaction_failed_drop_remains_available",
				"error", err)
			return nil
		}
		drop.Status = runtimeDungeonDropConsumed
		drop.GoldAfter = goldAfter
		drop.PickupResponseBody = append([]byte(nil), responseBody...)
		s.logGameEvent(session, "game-dungeon-pickup-accepted",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"scene_slot", drop.SceneSlot,
			"item_id", drop.Item.ItemID,
			"amount", drop.Amount,
			"gold_after", goldAfter,
			"picker_actor_object_key", pickerActorObjectKey,
			"request_context", request.ObjectContext,
			"request_player_x", request.PlayerX,
			"request_player_y", request.PlayerY,
			"request_token0", request.Token0,
			"request_drop_x", request.DropX,
			"request_drop_y", request.DropY,
			"request_token1", request.Token1,
			"request_token2", request.Token2,
			"response_opcode", currentDungeonPickupResultOpcode,
			"response_body_len", len(responseBody),
			"asset_owner", "character_assets_uow_gold")
		if err := s.sendGameUpperRawClass(session, currentDungeonPickupResultOpcode, responseBody, 0); err != nil {
			return err
		}
		if len(goldEffectBody) > 0 {
			if err := s.sendGameUpperRawClass(session, currentDungeonGoldPickupEffectOpcode, goldEffectBody, 0); err != nil {
				return err
			}
		}
		return s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentGoldStateBody(goldAfter),
			0,
		)
	}
	repositories, ok := s.repositoryGroup()
	if ok && repositories.EpicBookCraft != nil {
		if catalog, catalogErr := s.currentEpicBookCatalog(); catalogErr == nil {
			if entry, isFragment := catalog.EntryByFragment(drop.Item.ItemID); isFragment {
				var temporarySlot uint16
				if repositories.Inventory != nil {
					if inventory, found, loadErr := repositories.Inventory.Load(context.Background(), strconv.Itoa(int(session.selectedCharacterID))); loadErr == nil && found {
						temporarySlot, _ = currentEpicBookTemporarySlot(inventory, drop.Item.SlotStart, drop.Item.SlotEnd)
					}
				}
				accountID := strings.TrimSpace(s.accountIDForSession(session))
				characterID := strconv.Itoa(int(session.selectedCharacterID))
				balance, commitErr := dnfepicbook.AddFragment(context.Background(), repositories.EpicBookCraft, accountID, characterID, entry.EquipmentID, drop.Amount, time.Now().UTC())
				if commitErr != nil {
					return nil
				}
				responseBody, buildErr := buildCurrentEpicBookPickupResultBody(drop.ObjectKey, pickerActorObjectKey, temporarySlot)
				if buildErr != nil {
					return buildErr
				}
				var clearBody []byte
				if temporarySlot > 0 {
					clearBody, buildErr = buildCurrentEpicBookTemporarySlotClearBody(temporarySlot)
					if buildErr != nil {
						return buildErr
					}
				}
				syncBody, syncErr := buildCurrentEpicBookInfoBody(1, []currentEpicBookBalanceRow{{FragmentID: entry.FragmentID, Balance: balance}})
				if syncErr != nil {
					return syncErr
				}
				drop.Status = runtimeDungeonDropConsumed
				drop.DestinationSlot = temporarySlot
				drop.PickupResponseBody = append([]byte(nil), responseBody...)
				drop.PickupEpicBookClearBody = append([]byte(nil), clearBody...)
				drop.PickupEpicBookBody = append([]byte(nil), syncBody...)
				if err := s.sendGameUpperRawClass(session, currentDungeonPickupResultOpcode, responseBody, 0); err != nil {
					return err
				}
				if len(clearBody) > 0 {
					if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), clearBody, 0); err != nil {
						return err
					}
				}
				return s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, syncBody, 0)
			}
		}
	}
	if drop.DiscardOrigin != nil {
		if !ok || repositories.AccountAssets == nil {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"reason", "discard_origin_account_assets_transaction_missing",
				"error", errDungeonPickupTransactionMissing)
			return nil
		}
		characterID := strconv.Itoa(int(session.selectedCharacterID))
		destinationSlot, itemUpdate, err := restoreCurrentDungeonDiscardedItem(
			context.Background(),
			repositories.AccountAssets,
			strings.TrimSpace(s.accountIDForSession(session)),
			characterID,
			*drop.DiscardOrigin,
			drop.Item,
			time.Now().UTC(),
		)
		if err != nil {
			s.logGameEvent(session, "game-dungeon-pickup-blocked",
				"dungeon_id", runtime.Dungeon.ID,
				"room", scene.Coordinate.String(),
				"map_id", scene.Map.Map.ID,
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"amount", drop.Amount,
				"source_slot", drop.DiscardOrigin.SourceSlot,
				"account_owned", drop.DiscardOrigin.AccountOwned,
				"reason", "discard_origin_restore_transaction_failed_drop_remains_available",
				"error", err)
			return nil
		}
		responseBody, err := buildCurrentDungeonNormalPickupResultBody(
			drop.ObjectKey,
			pickerActorObjectKey,
			destinationSlot,
		)
		if err != nil {
			drop.Status = runtimeDungeonDropConsumed
			drop.DestinationSlot = destinationSlot
			s.logGameEvent(session, "game-dungeon-pickup-response-blocked-after-commit",
				"drop_object_key", drop.ObjectKey,
				"item_id", drop.Item.ItemID,
				"destination_slot", destinationSlot,
				"reason", "discard_origin_pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant",
				"error", err)
			return nil
		}
		drop.Status = runtimeDungeonDropConsumed
		drop.DestinationSlot = destinationSlot
		drop.PickupResponseBody = append([]byte(nil), responseBody...)
		itemUpdateBody := buildCurrentItemUpdateBody(
			drop.DiscardOrigin.ListType,
			[]currentItemListEntry{itemUpdate},
		)
		drop.PickupItemUpdateBody = append([]byte(nil), itemUpdateBody...)
		s.logGameEvent(session, "game-dungeon-pickup-accepted",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"scene_slot", drop.SceneSlot,
			"item_id", drop.Item.ItemID,
			"amount", drop.Amount,
			"destination_slot", destinationSlot,
			"picker_actor_object_key", pickerActorObjectKey,
			"request_context", request.ObjectContext,
			"request_player_x", request.PlayerX,
			"request_player_y", request.PlayerY,
			"request_drop_x", request.DropX,
			"request_drop_y", request.DropY,
			"response_opcode", currentDungeonPickupResultOpcode,
			"response_body_len", len(responseBody),
			"asset_owner", "discard_origin_account_assets_uow",
			"item_update_opcode", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"item_update_body_len", len(itemUpdateBody))
		if err := s.sendGameUpperRawClass(session, currentDungeonPickupResultOpcode, responseBody, 0); err != nil {
			return err
		}
		return s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			itemUpdateBody,
			0,
		)
	}
	if !ok || repositories.CharacterItems == nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"reason", "character_items_transaction_missing",
			"error", errDungeonPickupTransactionMissing)
		return nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	destinationSlot, itemUpdate, err := s.grantCurrentDungeonPickupItem(
		context.Background(),
		repositories.CharacterItems,
		characterID,
		drop.Item,
		drop.Amount,
		time.Now().UTC(),
		currentDungeonDiscardReservedSlots(runtime),
		drop.QualitySeed,
	)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-pickup-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"amount", drop.Amount,
			"reason", "character_items_transaction_failed_drop_remains_available",
			"error", err)
		return nil
	}
	responseBody, err := buildCurrentDungeonNormalPickupResultBody(
		drop.ObjectKey,
		pickerActorObjectKey,
		destinationSlot,
	)
	if err != nil {
		// The allocator only returns proved positive uint16 category slots, so
		// this is an invariant failure after a committed transaction. Consume
		// the scene object to prevent a duplicate grant even if no ACK can be sent.
		drop.Status = runtimeDungeonDropConsumed
		drop.DestinationSlot = destinationSlot
		s.logGameEvent(session, "game-dungeon-pickup-response-blocked-after-commit",
			"drop_object_key", drop.ObjectKey,
			"item_id", drop.Item.ItemID,
			"destination_slot", destinationSlot,
			"reason", "pickup_response_invariant_failed_drop_consumed_to_prevent_duplicate_grant",
			"error", err)
		return nil
	}
	drop.Status = runtimeDungeonDropConsumed
	drop.DestinationSlot = destinationSlot
	drop.PickupResponseBody = append([]byte(nil), responseBody...)
	// Same rule as the ordinary pickup route: announce the row on the list it
	// was written to, so a guild medal/gem does not appear in the main bag.
	pickupListType, _, _ := currentDungeonPickupListPlacement(drop.Item)
	itemUpdateBody := buildCurrentItemUpdateBody(
		pickupListType,
		[]currentItemListEntry{itemUpdate},
	)
	drop.PickupItemUpdateBody = append([]byte(nil), itemUpdateBody...)
	s.logGameEvent(session, "game-dungeon-pickup-accepted",
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"drop_object_key", drop.ObjectKey,
		"scene_slot", drop.SceneSlot,
		"item_id", drop.Item.ItemID,
		"amount", drop.Amount,
		"destination_slot", destinationSlot,
		"picker_actor_object_key", pickerActorObjectKey,
		"request_context", request.ObjectContext,
		"request_player_x", request.PlayerX,
		"request_player_y", request.PlayerY,
		"request_token0", request.Token0,
		"request_drop_x", request.DropX,
		"request_drop_y", request.DropY,
		"request_token1", request.Token1,
		"request_token2", request.Token2,
		"response_opcode", currentDungeonPickupResultOpcode,
		"response_body_len", len(responseBody),
		"asset_owner", "character_items_uow",
		"item_update_opcode", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"item_update_body_len", len(itemUpdateBody))
	if err := s.sendGameUpperRawClass(session, currentDungeonPickupResultOpcode, responseBody, 0); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		itemUpdateBody,
		0,
	)
}

// currentDungeonDiscardReservedSlots 给出"本局里必须留空"的主背包格位：地上还躺着
// 属于这一格的弃置物，捡回来时服务端会把它放回被丢下的那一格。
//
// 弃置物换房间后依然有效（掉落回执要活到本局结束，见 runtimeDungeonDropOwner 的说明），
// 装备又不能并进已占用的行 —— 所以只要有一件新拾取物按"最小空格优先"占了这一格，那件
// 弃置物就再也捡不起来，会在副本里一直留到本局结束。2026-09-27 的线上取证就是这条：
// 超重时客户端把越线的装备丢回 43 格 → 自动拾取把新物品放进刚空出来的 43 格 →
// 地面对象 504 永久无法拾取（pickup-blocked / discard_origin_restore_...）。
//
// 这里不改"弃置物归还原格位"这条规则，只是把它记成"已许诺"，让新行分配绕开。
// 集合按需构建：没有在途弃置物时返回 nil，不给普通拾取加分配。
func currentDungeonDiscardReservedSlots(runtime *runtimeDungeonState) map[int16]struct{} {
	if runtime == nil || runtime.DropOwner == nil {
		return nil
	}
	var reserved map[int16]struct{}
	for _, drop := range runtime.DropOwner.byObjectKey {
		if drop == nil || drop.Status != runtimeDungeonDropAvailable || drop.DiscardOrigin == nil {
			continue
		}
		origin := drop.DiscardOrigin
		if origin.ListType != dnfrepo.MainInventoryListType || origin.SourceSlot < 0 {
			continue
		}
		if reserved == nil {
			reserved = make(map[int16]struct{}, 1)
		}
		reserved[origin.SourceSlot] = struct{}{}
	}
	return reserved
}

func grantCurrentDungeonPickupItem(
	ctx context.Context,
	uow dnfrepo.CharacterItemUnitOfWork,
	characterID string,
	definition dungeonDropItemDefinition,
	amount uint32,
	now time.Time,
	reservedSlots map[int16]struct{},
	qualitySeed ...uint32,
) (uint16, currentItemListEntry, error) {
	return grantCurrentDungeonPickupItemWithFinalizer(
		ctx,
		uow,
		characterID,
		definition,
		amount,
		now,
		nil,
		reservedSlots,
		qualitySeed...,
	)
}

type currentDungeonPickupStackFinalizer func(int16, dnfrepo.ItemStack) (dnfrepo.ItemStack, error)

func (s *Service) grantCurrentDungeonPickupItem(
	ctx context.Context,
	uow dnfrepo.CharacterItemUnitOfWork,
	characterID string,
	definition dungeonDropItemDefinition,
	amount uint32,
	now time.Time,
	reservedSlots map[int16]struct{},
	qualitySeed ...uint32,
) (uint16, currentItemListEntry, error) {
	finalize, err := s.currentDungeonPickupCustomAbilityFinalizer(definition)
	if err != nil {
		return 0, currentItemListEntry{}, err
	}
	return grantCurrentDungeonPickupItemWithFinalizer(
		ctx,
		uow,
		characterID,
		definition,
		amount,
		now,
		finalize,
		reservedSlots,
		qualitySeed...,
	)
}

func grantCurrentDungeonPickupItemWithFinalizer(
	ctx context.Context,
	uow dnfrepo.CharacterItemUnitOfWork,
	characterID string,
	definition dungeonDropItemDefinition,
	amount uint32,
	now time.Time,
	finalize currentDungeonPickupStackFinalizer,
	reservedSlots map[int16]struct{},
	qualitySeed ...uint32,
) (uint16, currentItemListEntry, error) {
	if uow == nil || characterID == "" {
		return 0, currentItemListEntry{}, errDungeonPickupTransactionMissing
	}
	if definition.ItemID == 0 || definition.SlotStart <= 0 || definition.SlotEnd < definition.SlotStart || amount == 0 {
		return 0, currentItemListEntry{}, fmt.Errorf(
			"%w: item=%d slots=%d..%d amount=%d",
			errDungeonPickupItemInvalid,
			definition.ItemID,
			definition.SlotStart,
			definition.SlotEnd,
			amount,
		)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var err error
	definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return 0, currentItemListEntry{}, err
	}
	owner, err := dnfdungeon.NewOwner(dnfrepo.Group{CharacterItems: uow})
	if err != nil {
		return 0, currentItemListEntry{}, errDungeonPickupTransactionMissing
	}
	placementListType, _, _ := currentDungeonPickupListPlacement(definition)
	placement := currentDungeonPickupPlacement(definition, amount, qualitySeed...)
	// 绕开"地上那件弃置物迟早要回来"的格位；没有在途弃置物时是 nil，落点规则不变。
	placement.ReservedSlots = reservedSlots
	var itemUpdate currentItemListEntry
	result, err := owner.GrantPickupItem(ctx, dnfdungeon.PickupItemCommand{
		CharacterID: characterID,
		Placement:   placement,
		UpdatedAt:   now,
		Finalize: func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			if stack.ItemID != int64(definition.ItemID) || stack.Count <= 0 {
				return dnfrepo.ItemStack{}, errDungeonPickupItemInvalid
			}
			if finalize != nil {
				stack, err = finalize(slot, stack)
				if err != nil {
					return dnfrepo.ItemStack{}, err
				}
			}
			itemUpdate = currentItemListEntryFromStack(
				placementListType,
				slot,
				stack,
			)
			stack.RawEntry = append([]byte(nil), itemUpdate.data[:]...)
			return stack, nil
		},
	})
	if err != nil {
		return 0, currentItemListEntry{}, mapCurrentDungeonPickupOwnerError(err)
	}
	return result.Slot, itemUpdate, nil
}

// currentDungeonPickupAccountSharedSlot 给出这次掉落应当落进的账号共享固定槽。
//
// 过去这里叫 currentDungeonPickupSharedCrystalSlot，并且用 CrystalWarehouseLastSlot(359)
// 收尾 —— 只认六个晶块(354..359)，把六个灵魂(360..365)整族挡在门外，于是灵魂掉落会走
// grantCurrentDungeonPickupItem 落进**角色背包**。这与 NPC 商店金币购买那次是同一类
// "只打了一半"的缺陷（2026-09-21 一并修掉）。
//
// 底层 dnfdungeon.Owner.GrantAccountSharedPickupItem 本来就用全区间
// dnfrepo.IsAccountSharedInventorySlot(354..365) 校验，缺的只是 bridge 侧这个谓词。
// 现在直接查权威表（repository/account_shared_materials.go），不再各自维护范围判断。
func currentDungeonPickupAccountSharedSlot(itemID uint32) (int16, bool) {
	slot, ok := dnfrepo.AccountSharedWarehouseSlotForItem(int64(itemID))
	if !ok || !dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
		return 0, false
	}
	return slot, true
}

// grantCurrentDungeonPickupAccountShared 把整包掉落并进它在账号共享仓库里的固定格。
// 名字里的 Crystal 已去掉：六个晶块(354..359)与六个灵魂(360..365)走的是同一条路。
func grantCurrentDungeonPickupAccountShared(
	ctx context.Context,
	uow dnfrepo.AccountCharacterAssetUnitOfWork,
	accountID string,
	characterID string,
	definition dungeonDropItemDefinition,
	amount uint32,
	sharedSlot int16,
	now time.Time,
	qualitySeed ...uint32,
) (uint16, currentItemListEntry, error) {
	accountID = strings.TrimSpace(accountID)
	characterID = strings.TrimSpace(characterID)
	mappedSlot, mapped := currentDungeonPickupAccountSharedSlot(definition.ItemID)
	if uow == nil || accountID == "" || characterID == "" || amount == 0 || !mapped || mappedSlot != sharedSlot {
		return 0, currentItemListEntry{}, errDungeonPickupTransactionMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var err error
	definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return 0, currentItemListEntry{}, err
	}
	if definition.Kind != dungeonDropItemStackable {
		return 0, currentItemListEntry{}, errDungeonPickupItemInvalid
	}
	owner, err := dnfdungeon.NewOwner(dnfrepo.Group{AccountAssets: uow})
	if err != nil {
		return 0, currentItemListEntry{}, errDungeonPickupTransactionMissing
	}
	var itemUpdate currentItemListEntry
	result, err := owner.GrantAccountSharedPickupItem(ctx, dnfdungeon.AccountSharedPickupItemCommand{
		AccountID:   accountID,
		CharacterID: characterID,
		SharedSlot:  sharedSlot,
		Placement:   currentDungeonPickupPlacement(definition, amount, qualitySeed...),
		UpdatedAt:   now,
		Finalize: func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			if stack.ItemID != int64(definition.ItemID) || stack.Count <= 0 {
				return dnfrepo.ItemStack{}, errDungeonPickupItemInvalid
			}
			itemUpdate = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), itemUpdate.data[:]...)
			return stack, nil
		},
	})
	if err != nil {
		return 0, currentItemListEntry{}, mapCurrentDungeonPickupOwnerError(err)
	}
	return result.Slot, itemUpdate, nil
}

func addCurrentDungeonPickupToInventory(
	record *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	amount uint32,
	qualitySeed ...uint32,
) (uint16, error) {
	result, _, err := dnfdungeon.PlacePickupItem(
		record,
		currentDungeonPickupPlacement(definition, amount, qualitySeed...),
	)
	if err != nil {
		return 0, mapCurrentDungeonPickupOwnerError(err)
	}
	return result, nil
}

// currentDungeonPickupListPlacement reports the list and page a dropped item is
// picked up into. Guild medals ([flag]) and guardian gems ([flag gem]) live on
// list 38: the client can only wear a medal or embed a gem from that list, so a
// pickup written to the PVF's list-0 page sat in the bag unusable until the
// login migration (migrateLegacyGuildMedalItemsFromMainInventory) moved it -
// which is exactly the "捡到时不对, 重登才正常" report. Everything else keeps the
// ordinary list-0 page. Same rule as the NPC guild exchange
// (currentGuildMedalListPlacement), so both grant paths agree.
func currentDungeonPickupListPlacement(definition dungeonDropItemDefinition) (byte, int16, int16) {
	if start, end, ok := currentGuildMedalListPlacement(definition); ok {
		return currentGuildMedalInventoryListType, start, end
	}
	return dnfrepo.MainInventoryListType, definition.SlotStart, definition.SlotEnd
}

func currentDungeonPickupPlacement(
	definition dungeonDropItemDefinition,
	amount uint32,
	qualitySeed ...uint32,
) dnfdungeon.PickupItemPlacement {
	kind := dnfdungeon.PickupItemKind(definition.Kind)
	listType, slotStart, slotEnd := currentDungeonPickupListPlacement(definition)
	placement := dnfdungeon.PickupItemPlacement{
		ListType: listType,
		Definition: dnfdungeon.PickupItemDefinition{
			ItemID:     definition.ItemID,
			Kind:       kind,
			StackLimit: definition.StackLimit,
			SlotStart:  slotStart,
			SlotEnd:    slotEnd,
			// The quick-slot preference reaches for list-0 slots 3..8, which do
			// not exist on list 38. Keep it to the main bag.
			PreferQuickSlot: listType == dnfrepo.MainInventoryListType &&
				definition.Kind == dungeonDropItemStackable &&
				dungeonDropStackablePrefersItemQuickSlots(definition.StackableType),
		},
		Amount: amount,
	}
	if !definition.ExpirationDate.IsZero() {
		placement.NormalizeExisting = func(stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			stack, _ = applyCurrentPVFItemExpiration(stack, definition)
			return stack, nil
		}
	}
	placement.BuildNew = func(slot int16) (dnfrepo.ItemStack, error) {
		return buildCurrentDungeonPickupStack(slot, definition, amount, qualitySeed...)
	}
	return placement
}

func buildCurrentDungeonPickupStack(
	slot int16,
	definition dungeonDropItemDefinition,
	amount uint32,
	qualitySeed ...uint32,
) (dnfrepo.ItemStack, error) {
	entry, err := currentDungeonDeathDropItemState(uint16(slot), definition, amount, qualitySeed...)
	if err != nil {
		return dnfrepo.ItemStack{}, err
	}
	extra := map[string]string{
		"source":    "dungeon_pvf_drop_pickup",
		"item_kind": string(definition.Kind),
		"pvf_path":  definition.PVFPath,
	}
	if definition.StackableType != "" {
		extra["stackable_type"] = definition.StackableType
	}
	if definition.StackLimit > 0 {
		extra["stack_limit"] = strconv.FormatInt(definition.StackLimit, 10)
	}
	if definition.Durability > 0 {
		extra["durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
		extra["max_durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
	}
	if definition.EquipmentType != "" {
		extra["equipment_type"] = definition.EquipmentType
	}
	if currentDungeonDroppedEquipmentShouldBeSealed(definition) {
		extra["seal_flag"] = "1"
	}
	stack := dnfrepo.ItemStack{
		ItemID: int64(definition.ItemID),
		Count:  int64(amount),
		Extra:  extra,
	}
	if definition.Kind == dungeonDropItemEquipment {
		if err := applyCurrentEquipmentQualitySeed(&stack, binary.LittleEndian.Uint32(entry.data[6:10])); err != nil {
			return dnfrepo.ItemStack{}, err
		}
	}
	if !definition.ExpirationDate.IsZero() {
		stack, _ = applyCurrentPVFItemExpiration(stack, definition)
	}
	entry = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
	stack.RawEntry = append([]byte(nil), entry.data[:]...)
	return stack, nil
}

func insertCurrentDungeonPickup(
	record *dnfrepo.InventoryRecord,
	definition dungeonDropItemDefinition,
	amount uint32,
	slot int16,
	qualitySeed ...uint32,
) (uint16, error) {
	if record == nil {
		return 0, errDungeonPickupItemInvalid
	}
	stack, err := buildCurrentDungeonPickupStack(slot, definition, amount, qualitySeed...)
	if err != nil {
		return 0, err
	}
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	record.Slots[currentDungeonPickupMainSlotKey(slot)] = stack
	return uint16(slot), nil
}

func mapCurrentDungeonPickupOwnerError(err error) error {
	switch {
	case errors.Is(err, dnfdungeon.ErrOwnerUnavailable):
		return errDungeonPickupTransactionMissing
	case errors.Is(err, dnfdungeon.ErrInventoryNotFound):
		return errDungeonPickupInventoryMissing
	case errors.Is(err, dnfdungeon.ErrPickupInventoryFull):
		return fmt.Errorf("%w: %v", errDungeonPickupInventoryFull, err)
	case errors.Is(err, dnfdungeon.ErrPickupStackLimit):
		return fmt.Errorf("%w: %v", errDungeonPickupStackLimit, err)
	case errors.Is(err, dnfdungeon.ErrPickupItemInvalid):
		return fmt.Errorf("%w: %v", errDungeonPickupItemInvalid, err)
	default:
		return err
	}
}

func currentDungeonPickupMainSlotKey(slot int16) string {
	return "0:" + strconv.FormatInt(int64(slot), 10)
}
