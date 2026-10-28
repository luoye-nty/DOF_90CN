package vending

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	receiptPrefix                   = "vending_machine_use_receipt_"
	receiptVersion            int64 = 2
	receiptRewardRawEntryMax        = 0x77
	receiptRewardRawChunkSize       = 8
)

var (
	ErrOwnerUnavailable  = errors.New("vending-machine owner is unavailable")
	ErrAccountRequired   = errors.New("vending-machine account id is required")
	ErrCharacterRequired = errors.New("vending-machine character id is required")
	ErrCommandInvalid    = errors.New("vending-machine command is invalid")
	ErrCharacterNotFound = errors.New("vending-machine character is not found")
	ErrAccountMismatch   = errors.New("vending-machine character account mismatch")
	ErrInventoryNotFound = errors.New("vending-machine inventory is not found")
	ErrMaterialMissing   = errors.New("vending-machine material is missing")
	ErrInventoryFull     = errors.New("vending-machine reward inventory is full")
	ErrProjectionInvalid = errors.New("vending-machine reward projection is invalid")
	ErrReceiptInvalid    = errors.New("vending-machine receipt is invalid")
)

// Projector grants the already selected reward into the cloned candidate
// inventory. It returns every main-inventory slot whose reward stack changed.
// Persistence and the material debit remain owned by Use's transaction.
type Projector func(candidate *dnfrepo.InventoryRecord, rewardItemID, rewardCount uint32) ([]int16, error)

type Command struct {
	AccountID      string
	CharacterID    string
	CompletionKey  string
	MaterialSlot   int16
	MaterialItemID uint32
	MaterialCount  uint32
	RewardItemID   uint32
	RewardCount    uint32
	UpdatedAt      time.Time
	Project        Projector
}

type Result struct {
	MaterialSlot      int16
	MaterialItemID    uint32
	MaterialCount     uint32
	MaterialRemaining uint32
	MaterialRemoved   bool
	RewardItemID      uint32
	RewardCount       uint32
	TouchedSlots      []int16
	RewardSnapshots   []RewardSnapshot
	Replayed          bool
}

// RewardSnapshot is the final committed state of one reward slot. Keeping the
// row source in the transaction result prevents the bridge from reloading the
// slot after commit and accidentally projecting a later inventory mutation in
// the current op215 response.
type RewardSnapshot struct {
	Slot  int16
	Stack dnfrepo.ItemStack
}

// Owner is the repository-neutral atomic boundary for one vending-machine use.
// Packet parsing, PVF selection, random choice and current-client projection
// stay outside this package.
type Owner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.CharacterAssets}, nil
}

// Use validates the selected character and exact client-provided material slot,
// debits the PVF-owned material and grants one preselected reward in the same
// CharacterAssets transaction. A non-empty CompletionKey stores a compact
// receipt in Character.Stats so an indeterminate commit can be replayed without
// consuming or granting twice.
func (o *Owner) Use(ctx context.Context, command Command) (Result, error) {
	if o == nil || o.assets == nil {
		return Result{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(command.AccountID)
	characterID := strings.TrimSpace(command.CharacterID)
	if accountID == "" {
		return Result{}, ErrAccountRequired
	}
	if characterID == "" {
		return Result{}, ErrCharacterRequired
	}
	if command.MaterialSlot < 0 ||
		dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, command.MaterialSlot) ||
		command.MaterialItemID == 0 || command.MaterialCount == 0 ||
		command.RewardItemID == 0 || command.RewardCount == 0 || command.Project == nil {
		return Result{}, ErrCommandInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	now := command.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	completionKey := strings.TrimSpace(command.CompletionKey)
	durableReceipt := completionKey != ""
	var receiptHash [sha256.Size]byte
	if durableReceipt {
		receiptHash = sha256.Sum256([]byte(completionKey))
	}

	var result Result
	err := dnfrepo.WithinCharacterAssetsScoped(ctx, o.assets, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		if characters == nil || inventories == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if strings.TrimSpace(character.AccountID) != accountID {
			return fmt.Errorf("%w: account=%s character=%s", ErrAccountMismatch, accountID, characterID)
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		if durableReceipt && receiptMatches(character.Stats, receiptHash) {
			replayed, err := receiptResult(character.Stats, command)
			if err != nil {
				return err
			}
			replayed.Replayed = true
			result = replayed
			return nil
		}

		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID || inventory.Slots == nil {
			return fmt.Errorf("%w: character=%s", ErrInventoryNotFound, characterID)
		}
		inventory = dnfrepo.CloneInventory(inventory)
		materialKey := mainInventorySlotKey(command.MaterialSlot)
		material, found := inventory.Slots[materialKey]
		if !found || material.ItemID != int64(command.MaterialItemID) ||
			material.Count < int64(command.MaterialCount) {
			return fmt.Errorf(
				"%w: slot=%d item=%d need=%d",
				ErrMaterialMissing,
				command.MaterialSlot,
				command.MaterialItemID,
				command.MaterialCount,
			)
		}
		remaining := material.Count - int64(command.MaterialCount)
		if remaining < 0 || remaining > math.MaxUint32 {
			return ErrCommandInvalid
		}
		if remaining == 0 {
			delete(inventory.Slots, materialKey)
		} else {
			material.Count = remaining
			material = stackWithCount(material, command.MaterialSlot, remaining)
			inventory.Slots[materialKey] = material
		}

		beforeReward, err := mainInventoryItemCount(inventory, command.RewardItemID)
		if err != nil {
			return err
		}
		touched, err := command.Project(&inventory, command.RewardItemID, command.RewardCount)
		if err != nil {
			return err
		}
		touched, err = validateTouchedRewardSlots(inventory, command.RewardItemID, touched)
		if err != nil {
			return err
		}
		rewardSnapshots, err := snapshotTouchedRewardSlots(inventory, command.RewardItemID, touched)
		if err != nil {
			return err
		}
		afterReward, err := mainInventoryItemCount(inventory, command.RewardItemID)
		if err != nil || afterReward < beforeReward || afterReward-beforeReward != uint64(command.RewardCount) {
			return fmt.Errorf(
				"%w: item=%d before=%d after=%d want_delta=%d",
				ErrProjectionInvalid,
				command.RewardItemID,
				beforeReward,
				afterReward,
				command.RewardCount,
			)
		}

		result = Result{
			MaterialSlot:      command.MaterialSlot,
			MaterialItemID:    command.MaterialItemID,
			MaterialCount:     command.MaterialCount,
			MaterialRemaining: uint32(remaining),
			MaterialRemoved:   remaining == 0,
			RewardItemID:      command.RewardItemID,
			RewardCount:       command.RewardCount,
			TouchedSlots:      touched,
			RewardSnapshots:   rewardSnapshots,
		}
		if durableReceipt {
			writeReceipt(character.Stats, receiptHash, result)
		}
		inventory.UpdatedAt = now
		character.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		return dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats)
	})
	result.TouchedSlots = append([]int16(nil), result.TouchedSlots...)
	result.RewardSnapshots = cloneRewardSnapshots(result.RewardSnapshots)
	if err != nil {
		// The callback can have prepared the exact final rows before a backend
		// reports an indeterminate commit outcome. Return that frozen candidate
		// with the error; a receipt-confirmed immediate replay can then publish
		// the committed rows without a post-commit inventory load race.
		return result, err
	}
	return result, nil
}

func mainInventorySlotKey(slot int16) string {
	return fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
}

func stackWithCount(stack dnfrepo.ItemStack, slot int16, count int64) dnfrepo.ItemStack {
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 2)
	}
	value := strconv.FormatInt(count, 10)
	stack.Extra["count"] = value
	stack.Extra["amount_or_count"] = value
	if len(stack.RawEntry) >= 10 && stack.ItemID > 0 && stack.ItemID <= math.MaxUint32 && count > 0 && count <= math.MaxUint32 {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint16(stack.RawEntry[0:2], uint16(slot))
		binary.LittleEndian.PutUint32(stack.RawEntry[2:6], uint32(stack.ItemID))
		binary.LittleEndian.PutUint32(stack.RawEntry[6:10], uint32(count))
	}
	return stack
}

func mainInventoryItemCount(inventory dnfrepo.InventoryRecord, itemID uint32) (uint64, error) {
	var total uint64
	for key, stack := range inventory.Slots {
		if stack.ItemID != int64(itemID) || !strings.HasPrefix(key, "0:") {
			continue
		}
		listRaw, slotRaw, ok := strings.Cut(key, ":")
		if !ok || listRaw != "0" {
			continue
		}
		slot, err := strconv.ParseInt(slotRaw, 10, 16)
		if err != nil || slot < 0 || dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(slot)) {
			continue
		}
		if stack.Count < 0 {
			return 0, ErrProjectionInvalid
		}
		count := uint64(stack.Count)
		if math.MaxUint64-total < count {
			return 0, ErrProjectionInvalid
		}
		total += count
	}
	return total, nil
}

func validateTouchedRewardSlots(
	inventory dnfrepo.InventoryRecord,
	rewardItemID uint32,
	touched []int16,
) ([]int16, error) {
	if len(touched) == 0 || len(touched) > math.MaxUint16 {
		return nil, ErrProjectionInvalid
	}
	out := append([]int16(nil), touched...)
	seen := make(map[int16]struct{}, len(out))
	for _, slot := range out {
		if slot < 0 || dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
			return nil, ErrProjectionInvalid
		}
		if _, duplicate := seen[slot]; duplicate {
			return nil, ErrProjectionInvalid
		}
		seen[slot] = struct{}{}
		stack, found := inventory.Slots[mainInventorySlotKey(slot)]
		if !found || stack.ItemID != int64(rewardItemID) || stack.Count <= 0 {
			return nil, ErrProjectionInvalid
		}
	}
	return out, nil
}

func snapshotTouchedRewardSlots(
	inventory dnfrepo.InventoryRecord,
	rewardItemID uint32,
	touched []int16,
) ([]RewardSnapshot, error) {
	if len(touched) == 0 || len(touched) > math.MaxUint16 {
		return nil, ErrProjectionInvalid
	}
	snapshots := make([]RewardSnapshot, 0, len(touched))
	seen := make(map[int16]struct{}, len(touched))
	for _, slot := range touched {
		if slot < 0 || dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, slot) {
			return nil, ErrProjectionInvalid
		}
		if _, duplicate := seen[slot]; duplicate {
			return nil, ErrProjectionInvalid
		}
		seen[slot] = struct{}{}
		stack, found := inventory.Slots[mainInventorySlotKey(slot)]
		if !found || stack.ItemID != int64(rewardItemID) || stack.Count <= 0 {
			return nil, ErrProjectionInvalid
		}
		if len(stack.RawEntry) > receiptRewardRawEntryMax {
			return nil, ErrProjectionInvalid
		}
		snapshots = append(snapshots, RewardSnapshot{Slot: slot, Stack: cloneRewardStack(stack)})
	}
	return snapshots, nil
}

func cloneRewardSnapshots(snapshots []RewardSnapshot) []RewardSnapshot {
	if len(snapshots) == 0 {
		return nil
	}
	out := make([]RewardSnapshot, len(snapshots))
	for index, snapshot := range snapshots {
		out[index] = RewardSnapshot{Slot: snapshot.Slot, Stack: cloneRewardStack(snapshot.Stack)}
	}
	return out
}

func cloneRewardStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if stack.Extra != nil {
		extra := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			extra[key] = value
		}
		stack.Extra = extra
	}
	return stack
}

func receiptMatches(stats map[string]int64, want [sha256.Size]byte) bool {
	if stats == nil || stats[receiptPrefix+"version"] != receiptVersion {
		return false
	}
	for index := 0; index < 4; index++ {
		if uint64(stats[fmt.Sprintf("%shash_%d", receiptPrefix, index)]) !=
			binary.LittleEndian.Uint64(want[index*8:(index+1)*8]) {
			return false
		}
	}
	return true
}

func writeReceipt(stats map[string]int64, hash [sha256.Size]byte, result Result) {
	stats[receiptPrefix+"version"] = receiptVersion
	for index := 0; index < 4; index++ {
		stats[fmt.Sprintf("%shash_%d", receiptPrefix, index)] =
			int64(binary.LittleEndian.Uint64(hash[index*8 : (index+1)*8]))
	}
	stats[receiptPrefix+"material_slot"] = int64(result.MaterialSlot)
	stats[receiptPrefix+"material_item_id"] = int64(result.MaterialItemID)
	stats[receiptPrefix+"material_count"] = int64(result.MaterialCount)
	stats[receiptPrefix+"material_remaining"] = int64(result.MaterialRemaining)
	stats[receiptPrefix+"reward_item_id"] = int64(result.RewardItemID)
	stats[receiptPrefix+"reward_count"] = int64(result.RewardCount)
	stats[receiptPrefix+"touched_slot_count"] = int64(len(result.TouchedSlots))
	for index, slot := range result.TouchedSlots {
		stats[fmt.Sprintf("%stouched_slot_%d", receiptPrefix, index)] = int64(slot)
	}
	stats[receiptPrefix+"reward_snapshot_count"] = int64(len(result.RewardSnapshots))
	for index, snapshot := range result.RewardSnapshots {
		base := fmt.Sprintf("%sreward_snapshot_%d_", receiptPrefix, index)
		stats[base+"slot"] = int64(snapshot.Slot)
		stats[base+"item_id"] = snapshot.Stack.ItemID
		stats[base+"count"] = snapshot.Stack.Count
		raw := snapshot.Stack.RawEntry
		stats[base+"raw_len"] = int64(len(raw))
		for chunk := 0; chunk*receiptRewardRawChunkSize < len(raw); chunk++ {
			start := chunk * receiptRewardRawChunkSize
			end := start + receiptRewardRawChunkSize
			if end > len(raw) {
				end = len(raw)
			}
			var encoded [receiptRewardRawChunkSize]byte
			copy(encoded[:], raw[start:end])
			stats[fmt.Sprintf("%sraw_%d", base, chunk)] = int64(binary.LittleEndian.Uint64(encoded[:]))
		}
	}
}

func receiptResult(stats map[string]int64, command Command) (Result, error) {
	materialSlot, slotFound := stats[receiptPrefix+"material_slot"]
	materialItemID, materialFound := stats[receiptPrefix+"material_item_id"]
	materialCount, materialCountFound := stats[receiptPrefix+"material_count"]
	remaining, remainingFound := stats[receiptPrefix+"material_remaining"]
	rewardItemID, rewardFound := stats[receiptPrefix+"reward_item_id"]
	rewardCount, rewardCountFound := stats[receiptPrefix+"reward_count"]
	touchedCount, touchedCountFound := stats[receiptPrefix+"touched_slot_count"]
	if !slotFound || !materialFound || !materialCountFound || !remainingFound ||
		!rewardFound || !rewardCountFound || !touchedCountFound ||
		materialSlot != int64(command.MaterialSlot) || materialSlot < 0 || materialSlot > math.MaxInt16 ||
		materialItemID != int64(command.MaterialItemID) || materialItemID <= 0 || materialItemID > math.MaxUint32 ||
		materialCount != int64(command.MaterialCount) || materialCount <= 0 || materialCount > math.MaxUint32 ||
		remaining < 0 || remaining > math.MaxUint32 ||
		rewardItemID != int64(command.RewardItemID) || rewardItemID <= 0 || rewardItemID > math.MaxUint32 ||
		rewardCount != int64(command.RewardCount) || rewardCount <= 0 || rewardCount > math.MaxUint32 ||
		touchedCount <= 0 || touchedCount > math.MaxUint16 {
		return Result{}, ErrReceiptInvalid
	}
	result := Result{
		MaterialSlot:      int16(materialSlot),
		MaterialItemID:    uint32(materialItemID),
		MaterialCount:     uint32(materialCount),
		MaterialRemaining: uint32(remaining),
		MaterialRemoved:   remaining == 0,
		RewardItemID:      uint32(rewardItemID),
		RewardCount:       uint32(rewardCount),
		TouchedSlots:      make([]int16, 0, int(touchedCount)),
	}
	seen := make(map[int16]struct{}, int(touchedCount))
	for index := 0; index < int(touchedCount); index++ {
		value, found := stats[fmt.Sprintf("%stouched_slot_%d", receiptPrefix, index)]
		if !found || value < 0 || value > math.MaxInt16 ||
			dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(value)) {
			return Result{}, ErrReceiptInvalid
		}
		slot := int16(value)
		if _, duplicate := seen[slot]; duplicate {
			return Result{}, ErrReceiptInvalid
		}
		seen[slot] = struct{}{}
		result.TouchedSlots = append(result.TouchedSlots, slot)
	}
	snapshotCount, snapshotCountFound := stats[receiptPrefix+"reward_snapshot_count"]
	if !snapshotCountFound || snapshotCount != touchedCount {
		return Result{}, ErrReceiptInvalid
	}
	result.RewardSnapshots = make([]RewardSnapshot, 0, int(snapshotCount))
	for index := 0; index < int(snapshotCount); index++ {
		base := fmt.Sprintf("%sreward_snapshot_%d_", receiptPrefix, index)
		slotValue, slotFound := stats[base+"slot"]
		itemValue, itemFound := stats[base+"item_id"]
		countValue, countFound := stats[base+"count"]
		rawLength, rawLengthFound := stats[base+"raw_len"]
		if !slotFound || !itemFound || !countFound || !rawLengthFound ||
			slotValue != int64(result.TouchedSlots[index]) ||
			itemValue != int64(result.RewardItemID) || countValue <= 0 ||
			rawLength < 0 || rawLength > receiptRewardRawEntryMax {
			return Result{}, ErrReceiptInvalid
		}
		raw := make([]byte, int(rawLength))
		for chunk := 0; chunk*receiptRewardRawChunkSize < len(raw); chunk++ {
			encoded, found := stats[fmt.Sprintf("%sraw_%d", base, chunk)]
			if !found {
				return Result{}, ErrReceiptInvalid
			}
			var decoded [receiptRewardRawChunkSize]byte
			binary.LittleEndian.PutUint64(decoded[:], uint64(encoded))
			start := chunk * receiptRewardRawChunkSize
			copy(raw[start:], decoded[:])
		}
		if len(raw) >= 10 &&
			(binary.LittleEndian.Uint16(raw[0:2]) != uint16(slotValue) ||
				binary.LittleEndian.Uint32(raw[2:6]) != result.RewardItemID ||
				binary.LittleEndian.Uint32(raw[6:10]) == 0) {
			return Result{}, ErrReceiptInvalid
		}
		result.RewardSnapshots = append(result.RewardSnapshots, RewardSnapshot{
			Slot: int16(slotValue),
			Stack: dnfrepo.ItemStack{
				ItemID:   itemValue,
				Count:    countValue,
				RawEntry: raw,
			},
		})
	}
	return result, nil
}
