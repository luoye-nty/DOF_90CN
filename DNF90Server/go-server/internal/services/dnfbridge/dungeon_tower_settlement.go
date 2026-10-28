package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// Current DNF.exe registers the tower reward notifications in sub_19B2A30.
// sub_19B20A0/sub_19B2570 display the tower window and set the clear flag;
// ordinary op69/70/71 card selection is not part of that window's lifecycle.
const (
	currentAdventureTowerCountsMsgID uint16 = 1294
	currentTowerCharacterEntryMsgID  uint16 = 546
	currentTowerEmptyRewardItemID    uint32 = math.MaxUint32
)

// sub_1D58B70 reads four u8 values, writes selector+0xB44 and refreshes
// the native tooltip. This class-0 notification is not the op1294 request.
func (s *Service) sendCurrentAdventureTowerCounts(ctx context.Context, session *gameSession, source string) error {
	summary, err := s.currentAccountAdventureGroupSummary(ctx, dnfrepo.CharacterRecord{}, false, session)
	if err != nil {
		return err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return errAdventureGroupRosterUnavailable
	}
	account, found, err := repositories.Account.Load(ctx, s.accountIDForSession(session))
	if err != nil {
		return err
	}
	if !found {
		return errAdventureGroupRosterUnavailable
	}
	var projection adventuregroup.Projection
	s.projectAdventureTowerCounts(&projection, account, true, summary.ManageLevel)
	character, err := s.dungeonCharacter(ctx, session.selectedCharacterID, session)
	if err != nil {
		return err
	}
	// op546 updates actor+0x66E0. Send both character entries before op1294
	// rebuilds the tooltip, so the two independently consumed pools agree.
	var characterRemaining [2]byte
	now := s.gameplayNow()
	for index, kind := range [...]dnfdungeon.TowerKind{dnfdungeon.TowerDespair, dnfdungeon.TowerGrief} {
		characterRemaining[index] = dnfdungeon.TowerCharacterDailyRemaining(character, kind, now)
		var entry packetWriter
		entry.writeUint32([2]uint32{11008, 5115}[index])
		entry.writeByte(characterRemaining[index])
		if err := s.sendGameUpperRawClass(session, currentTowerCharacterEntryMsgID, entry.bytes(), 0); err != nil {
			return err
		}
	}
	var writer packetWriter
	writeCurrentAdventureTowerCounters(&writer, projection)
	if err := s.sendGameUpperRawClass(session, currentAdventureTowerCountsMsgID, writer.bytes(), 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-adventure-tower-counts-sent", "source", source,
		"msg_id", currentAdventureTowerCountsMsgID, "adventure_level", summary.ManageLevel,
		"remaining", projection.TowerRemaining, "limits", projection.TowerLimits,
		"character_remaining", characterRemaining)
	return nil
}

func currentDungeonTowerKind(runtime *runtimeDungeonState) dnfdungeon.TowerKind {
	switch currentDungeonTowerRewardMsgID(runtime) {
	case currentDespairTowerClearRewardMsgID:
		return dnfdungeon.TowerDespair
	case currentGriefTowerClearRewardMsgID:
		return dnfdungeon.TowerGrief
	default:
		return ""
	}
}

// currentDungeonTowerClear builds the clear for this run. Despair grants what
// the player picked up; grief draws its ten PVF card slots from the run's
// server-owned completion key so a retry produces the same rewards.
func (s *Service) currentDungeonTowerClear(runtime *runtimeDungeonState) (*dnfdungeon.TowerClear, error) {
	kind := currentDungeonTowerKind(runtime)
	if kind == "" {
		return nil, nil
	}
	floor, err := s.currentDungeonTowerFloor(runtime)
	if err != nil {
		return nil, err
	}
	clear := &dnfdungeon.TowerClear{Kind: kind, Floor: floor}
	s.initialEquipmentMu.Lock()
	archive, archiveErr := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if archiveErr != nil {
		return nil, archiveErr
	}
	if kind == dnfdungeon.TowerGrief {
		clear.Rewards, err = dnfdungeon.GriefTowerRewards(archive, floor, runtime.clearMapCompletionKey)
		if err != nil {
			return nil, err
		}
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			return nil, err
		}
		clear.Grant = func(inventory *dnfrepo.InventoryRecord, itemID, count uint32) error {
			definition, err := items.ResolveItem(itemID)
			if err != nil {
				return err
			}
			if definition.Kind != dungeonDropItemStackable || definition.InventoryListType != dnfrepo.MainInventoryListType {
				return errDungeonPickupItemInvalid
			}
			_, err = currentQuestDropGrantMainStack(inventory, definition, count)
			return err
		}
	}
	return clear, nil
}

func currentDungeonTowerDisplayRewards(runtime *runtimeDungeonState, character dnfrepo.CharacterRecord, pickups []currentDungeonClearRewardPair) ([]currentDungeonClearRewardPair, error) {
	if currentDungeonTowerKind(runtime) != dnfdungeon.TowerGrief {
		return pickups, nil
	}
	rewards, err := dnfdungeon.TowerLastRewards(character, dnfdungeon.TowerGrief)
	if err != nil {
		return nil, err
	}
	out := make([]currentDungeonClearRewardPair, len(rewards))
	for i, reward := range rewards {
		out[i] = currentDungeonClearRewardPair{Key: reward.ItemID, Value: reward.Count}
		if reward.ItemID == 0 {
			out[i].Key = currentTowerEmptyRewardItemID
		}
	}
	return out, nil
}

// Both native selector icons may send their first-floor base dungeon. Resolve
// that alias using the persisted floor before applying the existing PVF route.
func (s *Service) resolveCurrentTowerDungeonSelection(character dnfrepo.CharacterRecord, dungeon worldmap.Dungeon) (int64, error) {
	kind := currentDungeonTowerKind(&runtimeDungeonState{Dungeon: dungeon})
	if kind == "" {
		return dungeon.ID, nil
	}
	floor, err := dnfdungeon.TowerFloor(character, kind)
	if err != nil {
		return 0, err
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return 0, err
	}
	want, err := dnfdungeon.TowerDungeonID(archive, kind, floor)
	if err != nil {
		return 0, err
	}
	base, err := dnfdungeon.TowerDungeonID(archive, kind, 1)
	if err != nil {
		return 0, err
	}
	if dungeon.ID != base && dungeon.ID != want {
		return 0, fmt.Errorf("tower %s floor mismatch: requested=%d expected=%d", kind, dungeon.ID, want)
	}
	return want, nil
}

func (s *Service) currentTowerSelectorContext(ctx context.Context, session *gameSession) (currentDungeonContext, error) {
	packet := currentDungeonContext{}
	character, err := s.dungeonCharacter(ctx, session.selectedCharacterID, session)
	if err != nil {
		return packet, err
	}
	packet.ContextValue1, err = dnfdungeon.TowerFloor(character, dnfdungeon.TowerDespair)
	if err != nil {
		return packet, err
	}
	packet.CappedValue, err = dnfdungeon.TowerFloor(character, dnfdungeon.TowerGrief)
	return packet, err
}

func currentDungeonTowerRewardMsgID(runtime *runtimeDungeonState) uint16 {
	if runtime == nil || currentDungeonRuntimeIsPractice(runtime) {
		return 0
	}
	if currentDungeonIsTowerOfDespair(runtime.Dungeon) {
		return currentDespairTowerClearRewardMsgID
	}
	if runtime.Dungeon.Metadata.Flags["tower of grief"] {
		return currentGriefTowerClearRewardMsgID
	}
	for _, section := range runtime.Dungeon.SourceSections {
		if normalizeDungeonPVFSymbol(section.Name) == "tower of grief" {
			return currentGriefTowerClearRewardMsgID
		}
	}
	return 0
}

func (s *Service) currentDungeonTowerFloor(runtime *runtimeDungeonState) (uint16, error) {
	msgID := currentDungeonTowerRewardMsgID(runtime)
	if msgID == 0 {
		return 0, nil
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return 0, err
	}
	return resolveCurrentDungeonTowerFloor(archive, runtime.Dungeon, msgID)
}

// Project only actual pickups that still have a committed inventory receipt.
// Do not manufacture ordinary free/paid/Black Diamond card rewards for a tower.
// Amount is this run's pickup delta, not the destination stack's total count.
func currentDungeonTowerPickupRewards(runtime *runtimeDungeonState, receipts []currentDungeonClearRewardDrop) []currentDungeonClearRewardPair {
	if runtime == nil || runtime.DropOwner == nil {
		return nil
	}
	valid := make(map[uint16]currentDungeonClearRewardDrop, len(receipts))
	for _, receipt := range receipts {
		valid[receipt.ObjectKey] = receipt
	}
	bySlot := make(map[uint16]uint32)
	for _, drop := range runtime.DropOwner.byObjectKey {
		if drop == nil || drop.Status != runtimeDungeonDropConsumed || drop.Item.ItemID == 0 ||
			drop.Amount == 0 || valid[drop.DestinationSlot].TemplateID != drop.Item.ItemID || len(drop.PickupEpicBookBody) != 0 {
			continue
		}
		bySlot[drop.DestinationSlot] = saturatingCurrentDungeonUint32Add(bySlot[drop.DestinationSlot], drop.Amount)
	}
	discarded := currentDungeonActiveDiscardAmounts(runtime)
	amounts := make(map[uint32]uint32)
	for slot, amount := range bySlot {
		receipt := valid[slot]
		removed := discarded[currentDungeonDiscardReceiptKey{SourceSlot: int16(slot), ItemID: receipt.TemplateID}]
		if removed >= uint64(amount) {
			continue
		}
		amount = min(amount-uint32(removed), receipt.StackCount)
		if amount != 0 {
			amounts[receipt.TemplateID] = saturatingCurrentDungeonUint32Add(amounts[receipt.TemplateID], amount)
		}
	}
	rewards := make([]currentDungeonClearRewardPair, 0, len(amounts))
	for itemID, count := range amounts {
		rewards = append(rewards, currentDungeonClearRewardPair{Key: itemID, Value: count})
	}
	sort.Slice(rewards, func(i, j int) bool { return rewards[i].Key < rewards[j].Key })
	return rewards
}

func buildCurrentDungeonTowerRewardBody(elapsedMS uint32, floor uint16, rewards []currentDungeonClearRewardPair) ([]byte, error) {
	if floor == 0 || floor > 100 || len(rewards) > dnfdungeon.TowerRewardSlots {
		return nil, errCurrentDungeonSettlementPlanShape
	}
	// sub_18035F8 initializes empty cards to -1; sub_1803D77 draws their
	// native blank face. Even a rewardless clear needs a participant panel.
	rows := make([]currentDungeonClearRewardPair, dnfdungeon.TowerRewardSlots)
	for i := range rows {
		rows[i].Key = currentTowerEmptyRewardItemID
	}
	copy(rows, rewards)
	var writer packetWriter
	writer.writeUint32(elapsedMS)
	writer.writeUint16(floor)
	writer.writeByte(byte(len(rows)))
	for _, reward := range rows {
		if reward.Key == 0 || (reward.Key != currentTowerEmptyRewardItemID && reward.Value == 0) || (reward.Key == currentTowerEmptyRewardItemID && reward.Value != 0) {
			return nil, errCurrentDungeonSettlementPlanShape
		}
		writer.writeUint32(reward.Key)
		writer.writeUint32(reward.Value)
	}
	return writer.bytes(), nil
}

func currentDungeonTowerPlanValid(runtime *runtimeDungeonState, plan *currentDungeonSettlementPacketPlan) bool {
	msgID := currentDungeonTowerRewardMsgID(runtime)
	if msgID == 0 {
		return plan.TowerRewardMsgID == 0 && len(plan.TowerRewardBody) == 0
	}
	body := plan.TowerRewardBody
	if plan.TowerRewardMsgID != msgID || len(body) != 7+8*dnfdungeon.TowerRewardSlots || body[6] != dnfdungeon.TowerRewardSlots {
		return false
	}
	floor := binary.LittleEndian.Uint16(body[4:6])
	if floor == 0 || floor > 100 {
		return false
	}
	for offset := 7; offset < len(body); offset += 8 {
		id, count := binary.LittleEndian.Uint32(body[offset:]), binary.LittleEndian.Uint32(body[offset+4:])
		if id == 0 || (id == currentTowerEmptyRewardItemID && count != 0) || (id != currentTowerEmptyRewardItemID && count == 0) {
			return false
		}
	}
	return true
}

func completedCurrentDungeonTowerSettlementExitReady(runtime *runtimeDungeonState, characterID uint16) bool {
	return currentDungeonTowerRewardMsgID(runtime) != 0 && runtime.settlementTowerRewardSent &&
		completedDungeonSettlementResultReady(runtime, characterID)
}

// currentQuestDropGrantMainStack places a stackable into the main inventory
// list, matching the ordinary quest-material grant contract.
func currentQuestDropGrantMainStack(inventory *dnfrepo.InventoryRecord, definition dungeonDropItemDefinition, count uint32) (int16, error) {
	if definition.InventoryListType != dnfrepo.MainInventoryListType {
		return 0, fmt.Errorf("quest material grant received list %d", definition.InventoryListType)
	}
	return currentQuestDropGrantStack(inventory, definition, count)
}
