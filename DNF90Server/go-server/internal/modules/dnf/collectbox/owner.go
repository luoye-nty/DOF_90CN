package collectbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	stateValue  = "state_json"
	MetadataKey = "collectbox_state_v1"
)

var (
	ErrOwnerUnavailable       = errors.New("collect box owner unavailable")
	ErrCharacterRequired      = errors.New("collect box character required")
	ErrInventoryMissing       = errors.New("collect box inventory missing")
	ErrDefinitionInvalid      = errors.New("collect box definition invalid")
	ErrSourceMissing          = errors.New("collect box source item missing")
	ErrSourceMismatch         = errors.New("collect box source item mismatch")
	ErrBoxItemAlreadyStored   = errors.New("collect box item already stored")
	ErrBoxItemMissing         = errors.New("collect box item missing")
	ErrInventoryFull          = errors.New("collect box inventory full")
	ErrReturnPlacementInvalid = errors.New("collect box return placement invalid")
)

// Definition is the PVF-owned configuration for one collection box.
type Definition struct {
	Index              byte
	BaseExpirationDate string
	MaxExpirationDate  string
	ExtendItemID       uint32
	ExtendItemCount    int64
	Slots              []Slot
}

type Slot struct {
	ItemID   uint32
	Count    int64
	BuffType int64
	Bonus    Bonus
}

type Bonus struct {
	PhysicalAttack    int64
	PhysicalDefense   int64
	MagicalAttack     int64
	MagicalDefense    int64
	IndependentAttack int64
}

func (b *Bonus) Add(other Bonus) {
	if b == nil {
		return
	}
	b.PhysicalAttack += other.PhysicalAttack
	b.PhysicalDefense += other.PhysicalDefense
	b.MagicalAttack += other.MagicalAttack
	b.MagicalDefense += other.MagicalDefense
	b.IndependentAttack += other.IndependentAttack
}

type State struct {
	Boxes      map[string]map[string]uint32 `json:"boxes,omitempty"`
	Expiration map[string]time.Time         `json:"expiration,omitempty"`
	// Selected records the box each character is currently looking at. The
	// native collection box applies the attributes of the *selected* box, so a
	// login or actor-generation publication has to restore this instead of a
	// default. The account-metadata copy of this state is shared by every
	// character of the account, which is why the map is keyed by character ID.
	Selected map[string]byte `json:"selected,omitempty"`
}

// SelectedBox returns the recorded box for one character, or zero when the
// character never picked one.
func (s State) SelectedBox(characterID string) byte {
	index, ok := s.Selected[strings.TrimSpace(characterID)]
	if !ok {
		return 0
	}
	return index
}

// SetSelectedBox records the box for one character; zero clears the record.
func (s *State) SetSelectedBox(characterID string, index byte) {
	if s == nil {
		return
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return
	}
	if index == 0 {
		delete(s.Selected, characterID)
		return
	}
	if s.Selected == nil {
		s.Selected = make(map[string]byte)
	}
	s.Selected[characterID] = index
}

type InsertCommand struct {
	CharacterID string
	Definition  Definition
	SourceSlot  int16
	ItemID      uint32
	UpdatedAt   time.Time
}

type InsertResult struct {
	BoxIndex             byte
	BoxSlot              int
	ItemID               uint32
	RequiredCount        int64
	SourceSlot           int16
	SourceRemainingCount int64
	SourceRemoved        bool
}

type RemoveCommand struct {
	CharacterID     string
	Definition      Definition
	ItemID          uint32
	ReturnPlacement ReturnPlacement
	UpdatedAt       time.Time
}

// ReturnPlacement is the active-PVF main-inventory page for an item that is
// taken back out of the collection box.  The box state intentionally stores
// only item IDs, so the bridge resolves this placement before the durable
// transaction.  Keeping it explicit prevents a material/jewel from falling
// back into the equipment page merely because that page has a free slot.
type ReturnPlacement struct {
	SlotStart  int16
	SlotEnd    int16
	StackLimit int64
}

type RemoveResult struct {
	BoxIndex      byte
	BoxSlot       int
	ItemID        uint32
	RequiredCount int64
	TargetSlot    int16
	TargetStack   dnfrepo.ItemStack
}

type ExtendCommand struct {
	CharacterID string
	Definition  Definition
	ItemID      uint32
	UpdatedAt   time.Time
}

type ExtendResult struct {
	BoxIndex        byte
	ItemID          uint32
	RequiredCount   int64
	SourceSlot      int16
	SourceRemaining int64
	SourceRemoved   bool
	Expiration      time.Time
}

type Owner struct {
	settlements dnfrepo.CharacterSettlementUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{settlements: repositories.CharacterSettlement}, nil
}

func Scope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + ":collectbox"
}

func LoadState(ctx context.Context, repository dnfrepo.SettingsRepository, characterID string) (State, error) {
	if repository == nil || strings.TrimSpace(characterID) == "" {
		return State{}, ErrOwnerUnavailable
	}
	record, found, err := repository.Load(ctx, Scope(characterID))
	if err != nil {
		return State{}, err
	}
	if !found {
		return State{Boxes: make(map[string]map[string]uint32), Expiration: make(map[string]time.Time)}, nil
	}
	raw := strings.TrimSpace(record.Values[stateValue])
	if raw == "" {
		return State{Boxes: make(map[string]map[string]uint32), Expiration: make(map[string]time.Time)}, nil
	}
	return DecodeState(raw)
}

func SaveState(ctx context.Context, repository dnfrepo.SettingsRepository, characterID string, state State, updatedAt time.Time) error {
	if repository == nil || strings.TrimSpace(characterID) == "" {
		return ErrOwnerUnavailable
	}
	raw, err := EncodeState(state)
	if err != nil {
		return err
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	return repository.Save(ctx, dnfrepo.SettingsRecord{
		Scope:     Scope(characterID),
		Values:    map[string]string{stateValue: string(raw)},
		UpdatedAt: updatedAt.UTC(),
	})
}

func DecodeState(raw string) (State, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return normalizeState(State{}), nil
	}
	var state State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return State{}, fmt.Errorf("decode collect box state: %w", err)
	}
	return normalizeState(state), nil
}

func EncodeState(state State) (string, error) {
	raw, err := json.Marshal(normalizeState(state))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func LoadAccountState(ctx context.Context, repository dnfrepo.AccountRepository, accountID string) (State, bool, error) {
	if repository == nil || strings.TrimSpace(accountID) == "" {
		return State{}, false, ErrOwnerUnavailable
	}
	account, found, err := repository.Load(ctx, strings.TrimSpace(accountID))
	if err != nil || !found {
		return State{}, false, err
	}
	raw := strings.TrimSpace(account.Metadata[MetadataKey])
	if raw == "" {
		return normalizeState(State{}), false, nil
	}
	state, err := DecodeState(raw)
	return state, true, err
}

func Snapshot(definition Definition, state State) []uint32 {
	if !validDefinition(definition) {
		return nil
	}
	box := state.Boxes[strconv.Itoa(int(definition.Index))]
	items := make([]uint32, 0, len(box))
	for slot := range definition.Slots {
		if itemID := box[strconv.Itoa(slot)]; itemID != 0 {
			items = append(items, itemID)
		}
	}
	return items
}

func (o *Owner) Insert(ctx context.Context, command InsertCommand) (InsertResult, error) {
	if o == nil || o.settlements == nil {
		return InsertResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return InsertResult{}, ErrCharacterRequired
	}
	slotIndex, requirement, err := resolveSlot(command.Definition, command.ItemID)
	if err != nil {
		return InsertResult{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return InsertResult{}, err
	}
	if command.UpdatedAt.IsZero() {
		command.UpdatedAt = time.Now().UTC()
	}
	var result InsertResult
	err = o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Inventory == nil || tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		inventory, found, loadErr := tx.Inventory.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
			return ErrInventoryMissing
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		state, account, stateErr := loadStateForSettlement(ctx, tx, characterID)
		if stateErr != nil {
			return stateErr
		}
		box := ensureBox(&state, command.Definition.Index)
		slotKey := strconv.Itoa(slotIndex)
		if box[slotKey] != 0 {
			return ErrBoxItemAlreadyStored
		}
		for _, stored := range box {
			if stored == command.ItemID {
				return ErrBoxItemAlreadyStored
			}
		}
		sourceKey := inventoryKey(dnfrepo.MainInventoryListType, command.SourceSlot)
		source, ok := inventory.Slots[sourceKey]
		if !ok || source.ItemID <= 0 {
			return fmt.Errorf("%w: slot=%d", ErrSourceMissing, command.SourceSlot)
		}
		if source.ItemID != int64(command.ItemID) {
			return fmt.Errorf("%w: want=%d got=%d", ErrSourceMismatch, command.ItemID, source.ItemID)
		}
		if source.Count < requirement.Count {
			return fmt.Errorf("%w: need=%d have=%d", ErrSourceMissing, requirement.Count, source.Count)
		}
		source.Count -= requirement.Count
		removed := source.Count <= 0
		if removed {
			delete(inventory.Slots, sourceKey)
		} else {
			inventory.Slots[sourceKey] = source
		}
		box[slotKey] = command.ItemID
		inventory.UpdatedAt = command.UpdatedAt.UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := saveStateForSettlement(ctx, tx, characterID, account, state, command.UpdatedAt); err != nil {
			return err
		}
		result = InsertResult{
			BoxIndex:             command.Definition.Index,
			BoxSlot:              slotIndex,
			ItemID:               command.ItemID,
			RequiredCount:        requirement.Count,
			SourceSlot:           command.SourceSlot,
			SourceRemainingCount: source.Count,
			SourceRemoved:        removed,
		}
		return nil
	})
	if err != nil {
		return InsertResult{}, err
	}
	return result, nil
}

func (o *Owner) Remove(ctx context.Context, command RemoveCommand) (RemoveResult, error) {
	if o == nil || o.settlements == nil {
		return RemoveResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return RemoveResult{}, ErrCharacterRequired
	}
	slotIndex, requirement, err := resolveSlot(command.Definition, command.ItemID)
	if err != nil {
		return RemoveResult{}, err
	}
	if !validReturnPlacement(command.ReturnPlacement) {
		return RemoveResult{}, ErrReturnPlacementInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if command.UpdatedAt.IsZero() {
		command.UpdatedAt = time.Now().UTC()
	}
	var result RemoveResult
	err = o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Inventory == nil || tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		inventory, found, loadErr := tx.Inventory.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
			return ErrInventoryMissing
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		state, account, stateErr := loadStateForSettlement(ctx, tx, characterID)
		if stateErr != nil {
			return stateErr
		}
		box := ensureBox(&state, command.Definition.Index)
		stored, ok := box[strconv.Itoa(slotIndex)]
		if !ok || stored != command.ItemID {
			return ErrBoxItemMissing
		}
		targetSlot, target, ok := findTargetSlot(inventory.Slots, command.ItemID, requirement.Count, command.ReturnPlacement)
		if !ok {
			return ErrInventoryFull
		}
		if target.ItemID == 0 {
			target = dnfrepo.ItemStack{ItemID: int64(command.ItemID), Count: requirement.Count}
		} else {
			target.Count += requirement.Count
		}
		inventory.Slots[inventoryKey(dnfrepo.MainInventoryListType, targetSlot)] = target
		delete(box, strconv.Itoa(slotIndex))
		inventory.UpdatedAt = command.UpdatedAt.UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := saveStateForSettlement(ctx, tx, characterID, account, state, command.UpdatedAt); err != nil {
			return err
		}
		result = RemoveResult{
			BoxIndex:      command.Definition.Index,
			BoxSlot:       slotIndex,
			ItemID:        command.ItemID,
			RequiredCount: requirement.Count,
			TargetSlot:    targetSlot,
			TargetStack:   target,
		}
		return nil
	})
	if err != nil {
		return RemoveResult{}, err
	}
	return result, nil
}

func (o *Owner) Extend(ctx context.Context, command ExtendCommand) (ExtendResult, error) {
	if o == nil || o.settlements == nil {
		return ExtendResult{}, ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return ExtendResult{}, ErrCharacterRequired
	}
	if !validDefinition(command.Definition) || command.Definition.ExtendItemID == 0 || command.ItemID != command.Definition.ExtendItemID {
		return ExtendResult{}, ErrDefinitionInvalid
	}
	required := command.Definition.ExtendItemCount
	if required <= 0 {
		required = 1
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ExtendResult{}, err
	}
	if command.UpdatedAt.IsZero() {
		command.UpdatedAt = time.Now().UTC()
	}
	var result ExtendResult
	err := o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Inventory == nil || tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
			return ErrInventoryMissing
		}
		inventory = dnfrepo.CloneInventory(inventory)
		state, account, err := loadStateForSettlement(ctx, tx, characterID)
		if err != nil {
			return err
		}
		var sourceKey string
		var sourceSlot int16
		var source dnfrepo.ItemStack
		for key, stack := range inventory.Slots {
			list, slot, ok := parseInventoryKey(key)
			if ok && list == dnfrepo.MainInventoryListType && stack.ItemID == int64(command.ItemID) && stack.Count >= required {
				sourceKey, sourceSlot, source = key, slot, stack
				break
			}
		}
		if sourceKey == "" {
			return fmt.Errorf("%w: item=%d", ErrSourceMissing, command.ItemID)
		}
		source.Count -= required
		removed := source.Count <= 0
		if removed {
			delete(inventory.Slots, sourceKey)
		} else {
			inventory.Slots[sourceKey] = source
		}
		expiration := state.Expiration[strconv.Itoa(int(command.Definition.Index))]
		if expiration.IsZero() {
			expiration, _ = parseExpiration(command.Definition.BaseExpirationDate)
		}
		if expiration.IsZero() {
			expiration = command.UpdatedAt.UTC()
		}
		// The ticket advances the current deadline by one day, capped at PVF max.
		expiration = expiration.Add(24 * time.Hour)
		if max, ok := parseExpiration(command.Definition.MaxExpirationDate); ok && expiration.After(max) {
			expiration = max
		}
		if state.Expiration == nil {
			state.Expiration = make(map[string]time.Time)
		}
		state.Expiration[strconv.Itoa(int(command.Definition.Index))] = expiration
		inventory.UpdatedAt = command.UpdatedAt.UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := saveStateForSettlement(ctx, tx, characterID, account, state, command.UpdatedAt); err != nil {
			return err
		}
		result = ExtendResult{BoxIndex: command.Definition.Index, ItemID: command.ItemID, RequiredCount: required, SourceSlot: sourceSlot, SourceRemaining: source.Count, SourceRemoved: removed, Expiration: expiration}
		return nil
	})
	if err != nil {
		return ExtendResult{}, err
	}
	return result, nil
}

func normalizeState(state State) State {
	if state.Boxes == nil {
		state.Boxes = make(map[string]map[string]uint32)
	}
	if state.Expiration == nil {
		state.Expiration = make(map[string]time.Time)
	}
	for box, slots := range state.Boxes {
		if slots == nil {
			state.Boxes[box] = make(map[string]uint32)
		}
	}
	return state
}

// SelectCommand records the box the player is looking at. The native client
// applies only the selected box's attributes, so this is durable player intent
// rather than a cache: every login/actor-generation publication restores it.
type SelectCommand struct {
	CharacterID string
	BoxIndex    byte
	UpdatedAt   time.Time
}

func (o *Owner) Select(ctx context.Context, command SelectCommand) error {
	if o == nil || o.settlements == nil {
		return ErrOwnerUnavailable
	}
	characterID := strings.TrimSpace(command.CharacterID)
	if characterID == "" {
		return ErrCharacterRequired
	}
	if command.BoxIndex == 0 {
		return fmt.Errorf("%w: box index required", ErrDefinitionInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if command.UpdatedAt.IsZero() {
		command.UpdatedAt = time.Now().UTC()
	}
	return o.settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		state, account, stateErr := loadStateForSettlement(ctx, tx, characterID)
		if stateErr != nil {
			return stateErr
		}
		if state.SelectedBox(characterID) == command.BoxIndex {
			return nil
		}
		state.SetSelectedBox(characterID, command.BoxIndex)
		return saveStateForSettlement(ctx, tx, characterID, account, state, command.UpdatedAt)
	})
}

func loadStateForSettlement(ctx context.Context, tx dnfrepo.Group, characterID string) (State, dnfrepo.AccountRecord, error) {
	if tx.Character == nil {
		return State{}, dnfrepo.AccountRecord{}, ErrOwnerUnavailable
	}
	if tx.Account == nil {
		if tx.Settings == nil {
			return State{}, dnfrepo.AccountRecord{}, ErrOwnerUnavailable
		}
		state, err := LoadState(ctx, tx.Settings, characterID)
		return state, dnfrepo.AccountRecord{}, err
	}
	character, found, err := tx.Character.Load(ctx, characterID)
	if err != nil {
		return State{}, dnfrepo.AccountRecord{}, err
	}
	if !found || strings.TrimSpace(character.AccountID) == "" {
		return State{}, dnfrepo.AccountRecord{}, ErrOwnerUnavailable
	}
	account, found, err := tx.Account.Load(ctx, character.AccountID)
	if err != nil {
		return State{}, dnfrepo.AccountRecord{}, err
	}
	if !found {
		if tx.Settings == nil {
			return State{}, dnfrepo.AccountRecord{}, ErrOwnerUnavailable
		}
		state, settingsErr := LoadState(ctx, tx.Settings, characterID)
		return state, dnfrepo.AccountRecord{}, settingsErr
	}
	raw := strings.TrimSpace(account.Metadata[MetadataKey])
	if raw != "" {
		state, err := DecodeState(raw)
		return state, account, err
	}
	if tx.Settings == nil {
		return normalizeState(State{}), account, nil
	}
	state, err := LoadState(ctx, tx.Settings, characterID)
	return state, account, err
}

func saveStateForSettlement(ctx context.Context, tx dnfrepo.Group, characterID string, account dnfrepo.AccountRecord, state State, updatedAt time.Time) error {
	if tx.Account == nil {
		if tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		return SaveState(ctx, tx.Settings, characterID, state, updatedAt)
	}
	if strings.TrimSpace(account.AccountID) == "" {
		if tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		return SaveState(ctx, tx.Settings, characterID, state, updatedAt)
	}
	raw, err := EncodeState(state)
	if err != nil {
		return err
	}
	account = dnfrepo.CloneAccount(account)
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	account.Metadata[MetadataKey] = raw
	account.UpdatedAt = updatedAt.UTC()
	return tx.Account.Save(ctx, account)
}

func parseExpiration(raw string) (time.Time, bool) {
	raw = strings.Trim(strings.TrimSpace(raw), "`")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02", "2006.01.02 15:04:05", "2006.01.02"} {
		if parsed, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func ensureBox(state *State, index byte) map[string]uint32 {
	if state.Boxes == nil {
		state.Boxes = make(map[string]map[string]uint32)
	}
	key := strconv.Itoa(int(index))
	box := state.Boxes[key]
	if box == nil {
		box = make(map[string]uint32)
		state.Boxes[key] = box
	}
	return box
}

func validDefinition(definition Definition) bool {
	if len(definition.Slots) == 0 {
		return false
	}
	for _, slot := range definition.Slots {
		if slot.ItemID == 0 {
			return false
		}
	}
	return true
}

func resolveSlot(definition Definition, itemID uint32) (int, Slot, error) {
	if !validDefinition(definition) || itemID == 0 {
		return 0, Slot{}, ErrDefinitionInvalid
	}
	for index, slot := range definition.Slots {
		if slot.ItemID == itemID {
			if slot.Count <= 0 {
				slot.Count = 1
			}
			return index, slot, nil
		}
	}
	return 0, Slot{}, fmt.Errorf("%w: item=%d", ErrSourceMismatch, itemID)
}

func SnapshotBonus(definition Definition, state State, now time.Time) Bonus {
	if !validDefinition(definition) {
		return Bonus{}
	}
	expiration := state.Expiration[strconv.Itoa(int(definition.Index))]
	if expiration.IsZero() {
		expiration, _ = parseExpiration(definition.BaseExpirationDate)
	}
	if expiration.IsZero() {
		expiration, _ = parseExpiration(definition.MaxExpirationDate)
	}
	if !expiration.IsZero() && !now.Before(expiration) {
		return Bonus{}
	}
	stored := state.Boxes[strconv.Itoa(int(definition.Index))]
	var result Bonus
	for index, slot := range definition.Slots {
		if stored[strconv.Itoa(index)] == slot.ItemID {
			result.Add(slot.Bonus)
		}
	}
	return result
}

func validReturnPlacement(placement ReturnPlacement) bool {
	return placement.SlotStart >= 9 &&
		placement.SlotEnd >= placement.SlotStart &&
		placement.SlotEnd < dnfrepo.CrystalWarehouseFirstSlot
}

func findTargetSlot(slots map[string]dnfrepo.ItemStack, itemID uint32, count int64, placement ReturnPlacement) (int16, dnfrepo.ItemStack, bool) {
	if !validReturnPlacement(placement) || count <= 0 {
		return 0, dnfrepo.ItemStack{}, false
	}
	stackLimit := placement.StackLimit
	if stackLimit <= 0 {
		stackLimit = math.MaxInt32
	}
	// PVF [stack limit] 是这个容器里该道具的总量上限：额度按容器里同 id 总量算，
	// 容器里已经有该道具的行时不能再开第二行（没写 [stack limit] 时不受约束）。
	space := dnfrepo.ContainerItemSpace(slots, dnfrepo.MainInventoryListType, int64(itemID), placement.StackLimit)
	for slot := placement.SlotStart; slot <= placement.SlotEnd; slot++ {
		key := inventoryKey(dnfrepo.MainInventoryListType, slot)
		stack, occupied := slots[key]
		if !occupied || stack.ItemID != int64(itemID) || stack.Count < 0 || stack.Count > stackLimit-count ||
			count > space {
			continue
		}
		return slot, stack, true
	}
	for slot := placement.SlotStart; slot <= placement.SlotEnd; slot++ {
		key := inventoryKey(dnfrepo.MainInventoryListType, slot)
		if _, occupied := slots[key]; !occupied {
			if !dnfrepo.CanOpenContainerItemRow(slots, dnfrepo.MainInventoryListType, int64(itemID), placement.StackLimit) {
				return 0, dnfrepo.ItemStack{}, false
			}
			return slot, dnfrepo.ItemStack{}, true
		}
	}
	return 0, dnfrepo.ItemStack{}, false
}

func inventoryKey(listType byte, slot int16) string {
	return fmt.Sprintf("%d:%d", listType, slot)
}

func parseInventoryKey(key string) (byte, int16, bool) {
	parts := strings.Split(strings.TrimSpace(key), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	list, err := strconv.ParseUint(parts[0], 10, 8)
	if err != nil {
		return 0, 0, false
	}
	slot, err := strconv.ParseInt(parts[1], 10, 16)
	if err != nil {
		return 0, 0, false
	}
	return byte(list), int16(slot), true
}
