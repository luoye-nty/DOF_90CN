package dungeontower

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

const settingsSuffix = ":dungeon_tower_progress_v1"

var (
	ErrOwnerUnavailable = errors.New("dungeon tower progress owner is unavailable")
	ErrCommandInvalid   = errors.New("dungeon tower progress command is invalid")
	ErrCharacterMissing = errors.New("dungeon tower progress character is missing")
	ErrProgressMismatch = errors.New("dungeon tower progress does not own the cleared floor")
	ErrRewardInvalid    = errors.New("dungeon tower reward is invalid")
	ErrInventoryFull    = errors.New("dungeon tower reward inventory is full")
)

// Snapshot is the durable current floor projected to the current client.
type Snapshot struct {
	Family    dnfdungeonadmission.TowerFamily
	Floor     uint16
	DungeonID uint32
	Completed bool
	Replayed  bool
}

// RewardItem is one immutable item selected from the active PVF before the
// tower settlement transaction starts.
type RewardItem struct {
	Stack      dnfrepo.ItemStack
	Stackable  bool
	StackLimit int64
	SlotStart  int16
	SlotEnd    int16
}

type SettlementCommand struct {
	CharacterID      string
	Route            dnfdungeonadmission.TowerRoute
	ClearedDungeonID uint32
	Receipt          string
	MainSlots        uint16
	Rewards          []RewardItem
	UpdatedAt        time.Time
}

type SettlementResult struct {
	Snapshot  Snapshot
	Inventory dnfrepo.InventoryRecord
	ItemSlots []int16
	Replayed  bool
}

// Owner keeps each tower's floor on the character rather than the account's
// admission permission row. A clear receipt makes the increment idempotent.
type Owner struct {
	repositories dnfrepo.Group
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.Character == nil || repositories.Settings == nil || repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{repositories: repositories}, nil
}

func (o *Owner) Current(
	ctx context.Context,
	characterID string,
	route dnfdungeonadmission.TowerRoute,
) (Snapshot, error) {
	if o == nil || o.repositories.Character == nil || o.repositories.Settings == nil {
		return Snapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return Snapshot{}, fmt.Errorf("%w: character is required", ErrCommandInvalid)
	}
	if err := validateRoute(route); err != nil {
		return Snapshot{}, err
	}
	ctx = contextOrBackground(ctx)
	if _, found, err := o.repositories.Character.Load(ctx, characterID); err != nil {
		return Snapshot{}, err
	} else if !found {
		return Snapshot{}, ErrCharacterMissing
	}
	settings, _, err := o.repositories.Settings.Load(ctx, SettingsScope(characterID))
	if err != nil {
		return Snapshot{}, err
	}
	return snapshotFromValues(route, settings.Values)
}

// Advance commits one successful authored floor. clearedDungeonID may be a
// later route member only when the bridge has already skipped intervening PVF
// floors that have no playable solo actor layout.
func (o *Owner) Advance(
	ctx context.Context,
	characterID string,
	route dnfdungeonadmission.TowerRoute,
	clearedDungeonID uint32,
	receipt string,
	now time.Time,
) (Snapshot, error) {
	result, err := o.CommitSettlement(ctx, SettlementCommand{
		CharacterID: characterID, Route: route, ClearedDungeonID: clearedDungeonID,
		Receipt: receipt, UpdatedAt: now,
	})
	return result.Snapshot, err
}

// CommitSettlement atomically grants the already-planned tower rewards and
// advances the durable floor. The dedicated result packet is only a projection
// of this transaction and therefore cannot create rewards or progress.
func (o *Owner) CommitSettlement(ctx context.Context, command SettlementCommand) (SettlementResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return SettlementResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	receipt := strings.TrimSpace(command.Receipt)
	if characterID == "" || receipt == "" || command.ClearedDungeonID == 0 {
		return SettlementResult{}, fmt.Errorf("%w: character, dungeon and receipt are required", ErrCommandInvalid)
	}
	if err := validateRoute(command.Route); err != nil {
		return SettlementResult{}, err
	}
	if err := validateRewards(command); err != nil {
		return SettlementResult{}, err
	}
	clearedFloor := routeFloorForDungeon(command.Route, command.ClearedDungeonID)
	if clearedFloor == 0 {
		return SettlementResult{}, fmt.Errorf("%w: dungeon=%d family=%s", ErrProgressMismatch, command.ClearedDungeonID, command.Route.Family)
	}
	ctx = contextOrBackground(ctx)
	if command.UpdatedAt.IsZero() {
		command.UpdatedAt = time.Now()
	}
	now := command.UpdatedAt.UTC()
	var result SettlementResult
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Settings == nil || tx.Inventory == nil {
			return ErrOwnerUnavailable
		}
		if _, found, loadErr := tx.Character.Load(ctx, characterID); loadErr != nil {
			return loadErr
		} else if !found {
			return ErrCharacterMissing
		}
		scope := SettingsScope(characterID)
		settings, found, loadErr := tx.Settings.Load(ctx, scope)
		if loadErr != nil {
			return loadErr
		}
		settings = dnfrepo.CloneSettings(settings)
		if !found {
			settings.Scope = scope
		}
		if settings.Values == nil {
			settings.Values = make(map[string]string)
		}
		receiptKey := receiptStorageKey(command.Route.Family, receipt)
		if value := strings.TrimSpace(settings.Values[receiptKey]); value != "" {
			replayed, slots, parseErr := parseSettlementReceipt(command.Route, command.ClearedDungeonID, value)
			if parseErr != nil {
				return parseErr
			}
			replayed.Replayed = true
			inventory, _, loadErr := tx.Inventory.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			result = SettlementResult{Snapshot: replayed, Inventory: dnfrepo.CloneInventory(inventory), ItemSlots: slots, Replayed: true}
			return nil
		}
		current, currentErr := snapshotFromValues(command.Route, settings.Values)
		if currentErr != nil {
			return currentErr
		}
		if current.Completed || clearedFloor < current.Floor {
			return fmt.Errorf("%w: current_floor=%d cleared_floor=%d completed=%t", ErrProgressMismatch, current.Floor, clearedFloor, current.Completed)
		}
		inventory, found, loadErr := tx.Inventory.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			inventory = dnfrepo.InventoryRecord{CharacterID: characterID}
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		for _, reward := range command.Rewards {
			slot, addErr := addRewardItem(&inventory, command.MainSlots, reward)
			if addErr != nil {
				return addErr
			}
			result.ItemSlots = append(result.ItemSlots, slot)
		}
		nextFloor := clearedFloor
		completed := int(clearedFloor) == len(command.Route.DungeonIDs)
		if !completed {
			nextFloor++
		}
		result.Snapshot = Snapshot{
			Family: command.Route.Family, Floor: nextFloor,
			DungeonID: command.Route.DungeonIDs[int(nextFloor)-1], Completed: completed,
		}
		settings.Values[floorStorageKey(command.Route.Family)] = strconv.FormatUint(uint64(nextFloor), 10)
		if completed {
			settings.Values[completedStorageKey(command.Route.Family)] = "1"
		} else {
			delete(settings.Values, completedStorageKey(command.Route.Family))
		}
		settings.Values[receiptKey] = formatSettlementReceipt(command.ClearedDungeonID, result.Snapshot, result.ItemSlots)
		settings.UpdatedAt = now
		inventory.UpdatedAt = now
		if len(command.Rewards) > 0 {
			if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		result.Inventory = inventory
		return dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues)
	})
	if err != nil {
		return SettlementResult{}, err
	}
	return result, nil
}

func validateRewards(command SettlementCommand) error {
	for _, reward := range command.Rewards {
		if reward.Stack.ItemID <= 0 || reward.Stack.Count <= 0 || (!reward.Stackable && reward.Stack.Count != 1) ||
			reward.StackLimit < 0 || (reward.Stackable && reward.StackLimit > 0 && reward.Stack.Count > reward.StackLimit) {
			return ErrRewardInvalid
		}
		if reward.SlotStart < 3 || reward.SlotEnd < reward.SlotStart {
			if command.MainSlots == 0 {
				return ErrRewardInvalid
			}
		}
	}
	return nil
}

func addRewardItem(record *dnfrepo.InventoryRecord, mainSlots uint16, reward RewardItem) (int16, error) {
	if mainSlots > math.MaxInt16+1 && (reward.SlotStart < 3 || reward.SlotEnd < reward.SlotStart) {
		return 0, ErrRewardInvalid
	}
	// Tower revive coins go to the fixed slot-1 wallet like every other
	// channel (see dnfrevivecoin.Grant).
	if dnfrevivecoin.IsReward(reward.Stack.ItemID) && reward.Stack.Count > 0 {
		if record.Slots == nil {
			record.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if _, _, err := dnfrevivecoin.GrantSlots(
			record.Slots, reward.Stack.Count, "revive_coin_tower_reward"); err != nil {
			return 0, err
		}
		return dnfrevivecoin.WalletSlot, nil
	}
	start, end := int16(0), int16(mainSlots)-1
	if reward.SlotStart >= 3 && reward.SlotEnd >= reward.SlotStart {
		start, end = reward.SlotStart, reward.SlotEnd
	}
	// PVF [stack limit] 是这个容器里该道具的总量上限：额度按容器里同 id 总量算，
	// 容器里已经有该道具的行时不能再开第二行。装备（每件一行）与没写 [stack limit]
	// 的道具沿用旧行为。
	newRowStackLimit := int64(0)
	space := int64(math.MaxInt32)
	if reward.Stackable && reward.StackLimit > 0 {
		newRowStackLimit = reward.StackLimit
		space = dnfrepo.ContainerItemSpace(
			record.Slots,
			dnfrepo.MainInventoryListType,
			reward.Stack.ItemID,
			reward.StackLimit,
		)
	}
	if reward.Stackable {
		for cursor := int(start); cursor <= int(end); cursor++ {
			slot := int16(cursor)
			key := "0:" + strconv.FormatInt(int64(slot), 10)
			stack, found := record.Slots[key]
			if !found || stack.ItemID != reward.Stack.ItemID || stack.Bind != reward.Stack.Bind ||
				!stack.ExpireAt.Equal(reward.Stack.ExpireAt) || reward.StackLimit == 1 ||
				(reward.StackLimit > 0 && stack.Count > reward.StackLimit-reward.Stack.Count) ||
				reward.Stack.Count > space ||
				stack.Count > math.MaxInt64-reward.Stack.Count {
				continue
			}
			stack.Count += reward.Stack.Count
			updateRewardRawAmount(&stack)
			record.Slots[key] = stack
			return slot, nil
		}
	}
	for cursor := int(start); cursor <= int(end); cursor++ {
		slot := int16(cursor)
		key := "0:" + strconv.FormatInt(int64(slot), 10)
		if _, occupied := record.Slots[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(record.Slots, dnfrepo.MainInventoryListType, reward.Stack.ItemID, newRowStackLimit) {
			break
		}
		record.Slots[key] = cloneRewardStack(reward.Stack)
		return slot, nil
	}
	return 0, ErrInventoryFull
}

func cloneRewardStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if len(stack.Extra) == 0 {
		stack.Extra = nil
		return stack
	}
	extra := make(map[string]string, len(stack.Extra))
	for key, value := range stack.Extra {
		extra[key] = value
	}
	stack.Extra = extra
	return stack
}

func updateRewardRawAmount(stack *dnfrepo.ItemStack) {
	if stack == nil || stack.Count <= 0 || stack.Count > math.MaxUint32 || len(stack.RawEntry) != 0x77 {
		return
	}
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	binary.LittleEndian.PutUint32(stack.RawEntry[0x06:0x0a], uint32(stack.Count))
}

func SettingsScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + settingsSuffix
}

func validateRoute(route dnfdungeonadmission.TowerRoute) error {
	if (route.Family != dnfdungeonadmission.TowerFamilyDespair && route.Family != dnfdungeonadmission.TowerFamilyGrief) ||
		len(route.DungeonIDs) == 0 || len(route.DungeonIDs) > int(^uint16(0)) {
		return fmt.Errorf("%w: family=%q floors=%d", ErrCommandInvalid, route.Family, len(route.DungeonIDs))
	}
	seen := make(map[uint32]struct{}, len(route.DungeonIDs))
	for _, dungeonID := range route.DungeonIDs {
		if dungeonID == 0 {
			return fmt.Errorf("%w: family=%q contains dungeon 0", ErrCommandInvalid, route.Family)
		}
		if _, duplicate := seen[dungeonID]; duplicate {
			return fmt.Errorf("%w: family=%q duplicate dungeon=%d", ErrCommandInvalid, route.Family, dungeonID)
		}
		seen[dungeonID] = struct{}{}
	}
	return nil
}

func snapshotFromValues(route dnfdungeonadmission.TowerRoute, values map[string]string) (Snapshot, error) {
	floor := uint64(1)
	if raw := strings.TrimSpace(values[floorStorageKey(route.Family)]); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || parsed == 0 || parsed > uint64(len(route.DungeonIDs)) {
			return Snapshot{}, fmt.Errorf("%w: family=%s floor=%q", ErrProgressMismatch, route.Family, raw)
		}
		floor = parsed
	}
	completedRaw := strings.TrimSpace(values[completedStorageKey(route.Family)])
	if completedRaw != "" && completedRaw != "1" {
		return Snapshot{}, fmt.Errorf("%w: family=%s completed=%q", ErrProgressMismatch, route.Family, completedRaw)
	}
	return Snapshot{
		Family: route.Family, Floor: uint16(floor),
		DungeonID: route.DungeonIDs[int(floor)-1], Completed: completedRaw == "1",
	}, nil
}

func routeFloorForDungeon(route dnfdungeonadmission.TowerRoute, dungeonID uint32) uint16 {
	for index, candidate := range route.DungeonIDs {
		if candidate == dungeonID {
			return uint16(index + 1)
		}
	}
	return 0
}

func floorStorageKey(family dnfdungeonadmission.TowerFamily) string {
	return "tower:" + string(family) + ":floor"
}

func completedStorageKey(family dnfdungeonadmission.TowerFamily) string {
	return "tower:" + string(family) + ":completed"
}

func receiptStorageKey(family dnfdungeonadmission.TowerFamily, receipt string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(receipt)))
	return "tower:" + string(family) + ":receipt:" + hex.EncodeToString(digest[:16])
}

func formatReceiptSnapshot(clearedDungeonID uint32, snapshot Snapshot) string {
	completed := 0
	if snapshot.Completed {
		completed = 1
	}
	return fmt.Sprintf("%d:%d:%d", clearedDungeonID, snapshot.Floor, completed)
}

func formatSettlementReceipt(clearedDungeonID uint32, snapshot Snapshot, slots []int16) string {
	value := formatReceiptSnapshot(clearedDungeonID, snapshot)
	if len(slots) == 0 {
		return value
	}
	parts := make([]string, 0, len(slots))
	for _, slot := range slots {
		parts = append(parts, strconv.FormatInt(int64(slot), 10))
	}
	return value + "|" + strings.Join(parts, ",")
}

func parseSettlementReceipt(
	route dnfdungeonadmission.TowerRoute,
	clearedDungeonID uint32,
	value string,
) (Snapshot, []int16, error) {
	parts := strings.SplitN(value, "|", 2)
	snapshot, err := parseReceiptSnapshot(route, clearedDungeonID, parts[0])
	if err != nil || len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
		return snapshot, nil, err
	}
	values := strings.Split(parts[1], ",")
	slots := make([]int16, 0, len(values))
	for _, raw := range values {
		parsed, parseErr := strconv.ParseInt(strings.TrimSpace(raw), 10, 16)
		if parseErr != nil || parsed < 0 {
			return Snapshot{}, nil, fmt.Errorf("%w: malformed reward slots", ErrProgressMismatch)
		}
		slots = append(slots, int16(parsed))
	}
	return snapshot, slots, nil
}

func parseReceiptSnapshot(
	route dnfdungeonadmission.TowerRoute,
	clearedDungeonID uint32,
	value string,
) (Snapshot, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return Snapshot{}, fmt.Errorf("%w: malformed receipt", ErrProgressMismatch)
	}
	storedDungeon, dungeonErr := strconv.ParseUint(parts[0], 10, 32)
	floor, floorErr := strconv.ParseUint(parts[1], 10, 16)
	completed, completedErr := strconv.ParseUint(parts[2], 10, 1)
	if dungeonErr != nil || floorErr != nil || completedErr != nil || uint32(storedDungeon) != clearedDungeonID ||
		floor == 0 || floor > uint64(len(route.DungeonIDs)) {
		return Snapshot{}, fmt.Errorf("%w: conflicting receipt", ErrProgressMismatch)
	}
	return Snapshot{
		Family: route.Family, Floor: uint16(floor), DungeonID: route.DungeonIDs[int(floor)-1], Completed: completed == 1,
	}, nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
