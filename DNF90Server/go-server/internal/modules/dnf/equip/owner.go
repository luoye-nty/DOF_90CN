// 本文件负责角色穿戴装备 owner 的状态修改入口。
// 它只处理装备 raw entry 的可靠写入，不生成旧客户端成功 ACK。
package equip

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/creaturestate"
	"longheng.io/server/internal/modules/dnf/itemquality"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnftitlebook "longheng.io/server/internal/modules/dnf/titlebook"
)

var (
	ErrOwnerUnavailable        = errors.New("equipment owner unavailable")
	ErrCharacterRequired       = errors.New("selected character id required")
	ErrEquipmentNotFound       = errors.New("equipment record not found")
	ErrSlotNotFound            = errors.New("equipped slot not found")
	ErrRawEntryTooShort        = errors.New("equipped raw entry is too short")
	ErrDurabilityMissing       = errors.New("equipped durability evidence is missing")
	ErrDurabilityInvalid       = errors.New("equipped durability evidence is invalid")
	ErrRepairCostMissing       = errors.New("equipped repair cost evidence is missing")
	ErrRepairCostInvalid       = errors.New("equipped repair cost evidence is invalid")
	ErrRepairNotRepairable     = errors.New("equipped item is not repairable by PVF durability evidence")
	ErrRepairGoldInsufficient  = errors.New("equipped repair gold is insufficient")
	ErrWalletTxnRequired       = errors.New("wallet transaction is required")
	ErrRepairAllRequiresOwners = errors.New("repair all requires equipment and wallet owners")
	ErrInventoryUnavailable    = errors.New("inventory repository unavailable")
	ErrInventoryNotFound       = errors.New("inventory record not found")
	ErrMoveUnsupported         = errors.New("equipment move is unsupported")
	ErrMoveSlotNotFound        = errors.New("equipment move slot not found")
	ErrMoveRawEntryMissing     = errors.New("equipment raw entry evidence is missing")
	ErrMoveRawEntryInvalid     = errors.New("equipment raw entry evidence is invalid")
	ErrMoveStackCountInvalid   = errors.New("equipment move stack count is invalid")
	ErrMoveTransactionRequired = errors.New("equipment move transaction is required")
	ErrMoveSlotOutOfRange      = errors.New("equipment move slot is outside the current EXE range")
	ErrMoveSlotMismatch        = errors.New("equipment entry key and slot do not match")
	ErrMoveValidatorRequired   = errors.New("equipment placement validator is required")
	ErrEquipmentTxnRequired    = errors.New("equipment mutation requires a character item transaction")
	ErrLegacyPVFSlotCollision  = errors.New("legacy PVF equipment slot migration collides with current equipment slot")
	ErrPetRepositoryRequired   = errors.New("pet repository is required for a creature equipment move")
	ErrPetMoveTxnRequired      = errors.New("pet equipment move requires a character pet transaction")
	ErrPetRecordNotFound       = errors.New("pet record not found for creature equipment move")
	ErrPetEntryNotFound        = errors.New("pet entry not found for creature equipment move")
	ErrPetOwnershipMismatch    = errors.New("pet inventory, equipment, and creature record do not match")
)

// Owner 是穿戴装备和耐久的 durable owner 边界。
type Owner struct {
	equipment          dnfrepo.EquipmentRepository
	inventory          dnfrepo.InventoryRepository
	character          dnfrepo.CharacterRepository
	accounts           dnfrepo.AccountRepository
	items              dnfrepo.CharacterItemUnitOfWork
	assets             dnfrepo.CharacterAssetUnitOfWork
	pets               dnfrepo.PetRepository
	petItems           dnfrepo.CharacterPetUnitOfWork
	placementValidator PlacementValidator
	avatarClassifier   func(itemID uint32) (isAvatar bool, isClone bool)
	// auroraLookReplaceClassifier 判定 转职光环（PVF `[aurora avatar]` 且
	// `[item category]` 为 `clear avatar`）。它和 avatarClassifier 分开是有意的：
	// 转职光环同样属于克隆分类，但语义相反——它替代被覆盖光环的外观，因此不能走
	// 克隆外观继承，只能把被覆盖光环记成 USERINFO 的关联来源。为 nil 时保持旧行为。
	auroraLookReplaceClassifier func(itemID uint32) bool
	nameTagChecker              func(itemID uint32) bool
	inItemTransaction           bool
}

// RepairCommand 描述修理穿戴装备的最小命令。
type RepairCommand struct {
	SelectedCharacterID uint16
	AccountID           string
	SlotIndex           int16
	QuickRepair         bool
	AutoRepair          bool
	// ContractAutoRepair 标记"服务端自己发起的魔王契约自动修理"：某件佩戴装备的耐久
	// 刚被消耗到 0 时由 op48 处理器调用。它与 AutoRepair(客户端带 auto 标志的修理请求)
	// 的区别是**永不收费**：没有生效中的契约、或该角色当天 6 次已经用完时，本命令是空操作
	// (ContractSkipped)，而不会退化成玩家没点过的收费修理。调用方必须同时置 AutoRepair。
	ContractAutoRepair bool
}

// RepairResult 描述穿戴装备耐久修理的写库结果；当前只允许零金币单件修理由上层回成功 ACK。
type RepairResult struct {
	CharacterID   string
	SlotIndex     int16
	ItemID        int64
	OldDurability uint16
	NewDurability uint16
	Cost          int64
	UpdatedGold   int64
	Changed       bool
	// FreeRepair marks the 魔王契约 auto-repair path (premium type 586):
	// body[5]=1 plus an active contract makes the repair cost zero.
	FreeRepair bool
	// ContractSkipped marks a ContractAutoRepair command that was refused
	// because the contract is inactive or the character's six daily uses are
	// already spent. Nothing was repaired and no gold was taken.
	ContractSkipped bool
	// RepairedCount is set by RepairAll: how many entries were restored.
	RepairedCount int
	// RepairedSlots is set by RepairAll after the transaction commits. It lets
	// the transport publish only the changed rows instead of replaying the
	// crash-prone complete equipped table.
	RepairedSlots []RepairSlot
}

type RepairSlot struct {
	Equipped  bool
	SlotIndex int16
}

// DecreaseDurabilityCommand describes the native dungeon durability report.
// The current client sends the worn equipment slot as a one-byte value; the
// server applies one point of loss to that exact slot.
type DecreaseDurabilityCommand struct {
	SelectedCharacterID uint16
	SlotIndex           int16
	Amount              uint16
	Resolve             alignedcmd.RepairCostResolver
}

// DecreaseDurabilityResult is the durable result of one dungeon durability
// report. A non-repairable or already-zero item is returned unchanged.
type DecreaseDurabilityResult struct {
	CharacterID   string
	SlotIndex     int16
	ItemID        int64
	OldDurability uint16
	NewDurability uint16
	Changed       bool
}

// MoveCommand 描述装备槽和物品栏之间移动的最小命令。
type MoveCommand struct {
	SelectedCharacterID      uint16
	SourceListType           byte
	SourceSlotIndex          int16
	SourceInstanceValue      int32
	MoveCount                int32
	DestinationListType      byte
	DestinationSlotIndex     int16
	DestinationInstanceValue int32
}

// Placement is the typed input required to authorize an item entering an
// equipment slot. The validator owns PVF and character compatibility rules.
type Placement struct {
	CharacterID     string
	ItemID          int64
	SourceListType  byte
	SourceSlotIndex int16
	TargetSlotIndex int16
}

// PlacementValidator validates PVF item type, character restrictions, and the
// exact target worn slot. Owner refuses ordinary equipment writes without it.
type PlacementValidator interface {
	ValidateEquipmentPlacement(context.Context, Placement) error
}

// PlacementValidatorFunc adapts a function to PlacementValidator.
type PlacementValidatorFunc func(context.Context, Placement) error

func (f PlacementValidatorFunc) ValidateEquipmentPlacement(ctx context.Context, placement Placement) error {
	if f == nil {
		return ErrMoveValidatorRequired
	}
	return f(ctx, placement)
}

// MoveResult 描述穿戴/卸下写库结果；它不是旧客户端成功 ACK 许可。
type MoveResult struct {
	CharacterID          string
	SourceListType       byte
	SourceSlotIndex      int16
	DestinationListType  byte
	DestinationSlotIndex int16
	ItemID               int64
	SwappedItemID        int64
	Mode                 string
	Changed              bool
	InventorySlots       map[string]dnfrepo.ItemStack
}

// NewOwner 创建穿戴装备 owner；缺少装备仓储时拒绝处理。
func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.Equipment == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{
		equipment: repos.Equipment,
		inventory: repos.Inventory,
		character: repos.Character,
		accounts:  repos.Account,
		items:     repos.CharacterItems,
		assets:    repos.CharacterAssets,
		pets:      repos.Pet,
		petItems:  repos.CharacterPets,
	}, nil
}

// NewOwnerWithPlacementValidator enables ordinary equipment writes only when
// the caller supplies the typed PVF/character placement owner.
func NewOwnerWithPlacementValidator(repos dnfrepo.Group, validator PlacementValidator) (*Owner, error) {
	if validator == nil {
		return nil, ErrMoveValidatorRequired
	}
	owner, err := NewOwner(repos)
	if err != nil {
		return nil, err
	}
	owner.placementValidator = validator
	return owner, nil
}

// DecreaseDurability applies one native dungeon durability report. When the
// repository group exposes CharacterItems, the equipment write is scoped to
// that character transaction; the direct repository path is retained for
// lightweight/local repository groups used by older fixtures.
func (o *Owner) DecreaseDurability(ctx context.Context, cmd DecreaseDurabilityCommand) (DecreaseDurabilityResult, error) {
	if o == nil || o.equipment == nil {
		return DecreaseDurabilityResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return DecreaseDurabilityResult{}, ErrCharacterRequired
	}
	if cmd.SlotIndex < 0 {
		return DecreaseDurabilityResult{}, fmt.Errorf("%w: slot=%d", ErrSlotNotFound, cmd.SlotIndex)
	}
	if cmd.Amount == 0 {
		cmd.Amount = 1
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result DecreaseDurabilityResult
	apply := func(equipment dnfrepo.EquipmentRepository) error {
		var err error
		result, err = decreaseDurabilityInRepository(ctx, equipment, characterID, cmd)
		return err
	}
	if o.items != nil {
		if err := o.items.WithinCharacterItems(ctx, characterID, func(_ dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
			return apply(equipment)
		}); err != nil {
			return DecreaseDurabilityResult{}, err
		}
		return result, nil
	}
	if err := apply(o.equipment); err != nil {
		return DecreaseDurabilityResult{}, err
	}
	return result, nil
}

func decreaseDurabilityInRepository(
	ctx context.Context,
	equipment dnfrepo.EquipmentRepository,
	characterID string,
	cmd DecreaseDurabilityCommand,
) (DecreaseDurabilityResult, error) {
	record, found, err := equipment.Load(ctx, characterID)
	if err != nil {
		return DecreaseDurabilityResult{}, err
	}
	if !found {
		return DecreaseDurabilityResult{}, ErrEquipmentNotFound
	}
	record = dnfrepo.CloneEquipment(record)
	record.CharacterID = characterID
	key := entryKey(cmd.SlotIndex)
	entry, found := record.Entries[key]
	if !found || entry.SlotIndex != cmd.SlotIndex {
		return DecreaseDurabilityResult{CharacterID: characterID, SlotIndex: cmd.SlotIndex}, nil
	}
	result := DecreaseDurabilityResult{
		CharacterID: characterID,
		SlotIndex:   cmd.SlotIndex,
		ItemID:      entry.ItemID,
	}
	durabilityOffset, ok := equippedDurabilityOffset(entry)
	if !ok {
		return result, nil
	}
	current := binary.LittleEndian.Uint16(entry.RawEntry[durabilityOffset : durabilityOffset+2])
	result.OldDurability = current
	result.NewDurability = current
	if current == 0 {
		return result, nil
	}
	if cmd.Resolve != nil {
		evidence, resolveErr := cmd.Resolve(entry.ItemID)
		if resolveErr != nil {
			return DecreaseDurabilityResult{}, resolveErr
		}
		if evidence.MaxDurability <= 0 {
			return result, nil
		}
	}
	amount := cmd.Amount
	if amount > current {
		amount = current
	}
	updated := current - amount
	entry.RawEntry = append([]byte(nil), entry.RawEntry...)
	binary.LittleEndian.PutUint16(entry.RawEntry[durabilityOffset:durabilityOffset+2], updated)
	if entry.Extra == nil {
		entry.Extra = make(map[string]string, 1)
	}
	entry.Extra["durability"] = strconv.FormatUint(uint64(updated), 10)
	record.Entries[key] = entry
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveEquipmentFields(ctx, equipment, record, dnfrepo.EquipmentFieldEntries); err != nil {
		return DecreaseDurabilityResult{}, err
	}
	result.NewDurability = updated
	result.Changed = updated != current
	return result, nil
}

func equippedDurabilityOffset(entry dnfrepo.EquipmentEntry) (int, bool) {
	return durabilityOffsetForRaw(entry.ItemID, entry.RawEntry)
}

func durabilityOffsetForRaw(itemID int64, raw []byte) (int, bool) {
	// Current item-list rows are 0x77 (119) bytes and store durability at
	// +0x0B. The item-id guard prevents a random long raw blob from being
	// mistaken for the current row format. Historical rows use +0x0A.
	if len(raw) == currentEquipmentRawEntryWireSize && itemID > 0 &&
		itemID <= int64(^uint32(0)) && binary.LittleEndian.Uint32(raw[2:6]) == uint32(itemID) {
		return 0x0B, true
	}
	if len(raw) >= 12 {
		return 10, true
	}
	return 0, false
}

// SetNameTagChecker registers a PVF-backed predicate that identifies name tag
// cards. When set, unequip of a name tag card is blocked (PR #239).
func (o *Owner) SetNameTagChecker(checker func(itemID uint32) bool) {
	if o != nil {
		o.nameTagChecker = checker
	}
}

// SetAvatarClassifier registers the runtime-PVF authority used to distinguish
// ordinary and clone avatars while an op19 equipment transaction is active.
func (o *Owner) SetAvatarClassifier(classifier func(itemID uint32) (isAvatar bool, isClone bool)) {
	if o != nil {
		o.avatarClassifier = classifier
	}
}

// SetAuroraLookReplaceClassifier registers the runtime-PVF authority used to
// distinguish 转职光环 (aura look-replace avatars) from 透明克隆时装. Leaving it
// unset keeps the legacy clone-only behaviour.
func (o *Owner) SetAuroraLookReplaceClassifier(classifier func(itemID uint32) bool) {
	if o != nil {
		o.auroraLookReplaceClassifier = classifier
	}
}

// isAuroraLookReplace reports whether itemID is a 转职光环 under the active PVF.
// A missing classifier (isolated domain tests, pet-only transactions) is not an
// error: it must fall back to false so the clone path stays byte-for-byte the
// same as before this classifier existed.
func (o *Owner) isAuroraLookReplace(itemID uint32) bool {
	return o != nil && o.auroraLookReplaceClassifier != nil && itemID != 0 &&
		o.auroraLookReplaceClassifier(itemID)
}

// Repair 按 86JP TryRepairSingleEquipped 修理穿戴槽：PVF 证据定价（repair
// price/grade/durability + pricetable 全局费率），金币与耐久在同一角色资产
// 事务内提交；body[5]=1 且魔王契约(586)生效时免费；body[7]=1 快速修理按
// [quick repair cost rate] 加价。slot=-1 转 RepairAll。
func (o *Owner) Repair(ctx context.Context, cmd RepairCommand, costResolver alignedcmd.RepairCostResolver) (RepairResult, error) {
	if o == nil || o.assets == nil {
		return RepairResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return RepairResult{}, ErrCharacterRequired
	}
	if cmd.SlotIndex == -1 {
		return o.RepairAll(ctx, cmd, costResolver)
	}
	if costResolver == nil {
		return RepairResult{}, ErrRepairCostMissing
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result RepairResult
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		var err error
		result, err = o.repairSingleEquipped(ctx, cmd, characterID, characters, equipment, costResolver)
		return err
	})
	return result, err
}

func (o *Owner) repairSingleEquipped(
	ctx context.Context,
	cmd RepairCommand,
	characterID string,
	characters dnfrepo.CharacterRepository,
	equipment dnfrepo.EquipmentRepository,
	costResolver alignedcmd.RepairCostResolver,
) (RepairResult, error) {
	record, ok, err := equipment.Load(ctx, characterID)
	if err != nil {
		return RepairResult{}, err
	}
	if !ok {
		return RepairResult{}, ErrEquipmentNotFound
	}
	record = dnfrepo.CloneEquipment(record)
	record.CharacterID = characterID
	if record.Entries == nil {
		record.Entries = make(map[string]dnfrepo.EquipmentEntry)
	}

	key := entryKey(cmd.SlotIndex)
	entry, ok := record.Entries[key]
	if !ok {
		return RepairResult{}, fmt.Errorf("%w: slot=%d", ErrSlotNotFound, cmd.SlotIndex)
	}
	durabilityOffset, ok := durabilityOffsetForRaw(entry.ItemID, entry.RawEntry)
	if !ok {
		return RepairResult{}, fmt.Errorf("%w: slot=%d len=%d", ErrRawEntryTooShort, cmd.SlotIndex, len(entry.RawEntry))
	}
	current := int64(binary.LittleEndian.Uint16(entry.RawEntry[durabilityOffset : durabilityOffset+2]))
	evidence, err := costResolver(entry.ItemID)
	if err != nil {
		return RepairResult{}, err
	}
	if evidence.MaxDurability <= 0 {
		return RepairResult{}, fmt.Errorf("%w: slot=%d item=%d", ErrRepairNotRepairable, cmd.SlotIndex, entry.ItemID)
	}
	maxDurability := evidence.MaxDurability
	character, found, err := characters.Load(ctx, characterID)
	if err != nil {
		return RepairResult{}, err
	}
	if !found {
		return RepairResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		return RepairResult{}, fmt.Errorf("%w: character=%s has no stats", ErrWalletTxnRequired, characterID)
	}
	gold := character.Stats["gold"]
	result := RepairResult{
		CharacterID:   characterID,
		SlotIndex:     cmd.SlotIndex,
		ItemID:        entry.ItemID,
		OldDurability: uint16(current),
		NewDurability: uint16(maxDurability),
	}
	if current >= maxDurability {
		result.NewDurability = uint16(current)
		result.UpdatedGold = gold
		return result, nil
	}
	cost := CalcRepairCost(evidence, current, upgradeLevelOfRawEntry(entry.RawEntry), cmd.QuickRepair)
	freeRepair := false
	statsChanged := false
	if cost != 0 && cmd.AutoRepair && o.premiumActive(ctx, cmd.AccountID, premium.DevilSlotType(premium.DevilSlotAutoRepair)) {
		// 魔王契约 auto repair: the client sent body[5]=1 and the account
		// holds an active slot-6 contract, so the repair is free (86JP
		// freeRepair = autoRepair && HasActiveAutoRepairForAccount).
		if premium.TryConsumeDaily(&character, premium.DevilSlotAutoRepair, time.Now().UTC()) {
			cost = 0
			freeRepair = true
			statsChanged = true
		}
	}
	if cost > gold {
		return RepairResult{}, fmt.Errorf("%w: cost=%d gold=%d", ErrRepairGoldInsufficient, cost, gold)
	}

	if cost > 0 {
		character.Stats["gold"] = gold - cost
		statsChanged = true
	}
	if statsChanged {
		character.UpdatedAt = time.Now()
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return RepairResult{}, err
		}
	}
	entry.RawEntry = append([]byte(nil), entry.RawEntry...)
	binary.LittleEndian.PutUint16(entry.RawEntry[durabilityOffset:durabilityOffset+2], uint16(maxDurability))
	if entry.Extra == nil {
		entry.Extra = make(map[string]string, 1)
	}
	entry.Extra["durability"] = strconv.FormatInt(maxDurability, 10)
	record.Entries[key] = entry
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveEquipmentFields(ctx, equipment, record, dnfrepo.EquipmentFieldEntries); err != nil {
		return RepairResult{}, err
	}
	result.Cost = cost
	result.UpdatedGold = gold - cost
	result.Changed = true
	result.FreeRepair = freeRepair
	return result, nil
}

// RepairAll 按 86JP TryRepairAll 全部修理：扫描穿戴装备与主列表快捷栏
// (slot 3..8)，只处理 13 类可修理装备且耐久未满的条目，合计费用一次扣除，
// ACK 槽位 0xFFFF；上层必须再发布实际变更行，因为当前客户端不会据此本地拉满。
func (o *Owner) RepairAll(ctx context.Context, cmd RepairCommand, costResolver alignedcmd.RepairCostResolver) (RepairResult, error) {
	if o == nil || o.assets == nil {
		return RepairResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return RepairResult{}, ErrCharacterRequired
	}
	if costResolver == nil {
		return RepairResult{}, ErrRepairCostMissing
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var result RepairResult
	err := o.assets.WithinCharacterAssets(ctx, characterID, func(characters dnfrepo.CharacterRepository, inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
		var err error
		result, err = o.repairAllInTransaction(ctx, cmd, characterID, characters, inventory, equipment, costResolver)
		return err
	})
	return result, err
}

type repairAllTarget struct {
	equipped         bool
	key              string
	slot             int16
	itemID           int64
	maxDura          int64
	cost             int64
	durabilityOffset int
}

func (o *Owner) repairAllInTransaction(
	ctx context.Context,
	cmd RepairCommand,
	characterID string,
	characters dnfrepo.CharacterRepository,
	inventory dnfrepo.InventoryRepository,
	equipment dnfrepo.EquipmentRepository,
	costResolver alignedcmd.RepairCostResolver,
) (RepairResult, error) {
	character, found, err := characters.Load(ctx, characterID)
	if err != nil {
		return RepairResult{}, err
	}
	if !found {
		return RepairResult{}, fmt.Errorf("%w: character=%s", ErrWalletTxnRequired, characterID)
	}
	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		return RepairResult{}, fmt.Errorf("%w: character=%s has no stats", ErrWalletTxnRequired, characterID)
	}
	equipRecord, equipFound, err := equipment.Load(ctx, characterID)
	if err != nil {
		return RepairResult{}, err
	}
	equipRecord = dnfrepo.CloneEquipment(equipRecord)
	equipRecord.CharacterID = characterID
	if equipRecord.Entries == nil {
		equipRecord.Entries = make(map[string]dnfrepo.EquipmentEntry)
	}
	invRecord, invFound, err := inventory.Load(ctx, characterID)
	if err != nil {
		return RepairResult{}, err
	}
	invRecord = dnfrepo.CloneInventory(invRecord)
	invRecord.CharacterID = characterID
	if invRecord.Slots == nil {
		invRecord.Slots = make(map[string]dnfrepo.ItemStack)
	}

	autoRepairActive := cmd.AutoRepair && o.premiumActive(ctx, cmd.AccountID, premium.DevilSlotType(premium.DevilSlotAutoRepair))
	freeRepair := false
	targets := make([]repairAllTarget, 0, 16)
	total := int64(0)
	if equipFound {
		for key, entry := range equipRecord.Entries {
			durabilityOffset, ok := durabilityOffsetForRaw(entry.ItemID, entry.RawEntry)
			if !ok {
				continue
			}
			slot, err := strconv.Atoi(key)
			if err != nil {
				continue
			}
			evidence, err := costResolver(entry.ItemID)
			if err != nil {
				return RepairResult{}, err
			}
			if evidence.MaxDurability <= 0 || !RepairAllEligible(evidence.EquipmentType) {
				continue
			}
			current := int64(binary.LittleEndian.Uint16(entry.RawEntry[durabilityOffset : durabilityOffset+2]))
			if current >= evidence.MaxDurability {
				continue
			}
			cost := CalcRepairCost(evidence, current, upgradeLevelOfRawEntry(entry.RawEntry), cmd.QuickRepair)
			targets = append(targets, repairAllTarget{equipped: true, key: key, slot: int16(slot), itemID: entry.ItemID, maxDura: evidence.MaxDurability, cost: cost, durabilityOffset: durabilityOffset})
			total += cost
		}
	}
	if invFound {
		for slot := int16(3); slot <= 8; slot++ {
			key := "0:" + strconv.Itoa(int(slot))
			stack, ok := invRecord.Slots[key]
			if !ok || stack.ItemID <= 0 {
				continue
			}
			evidence, err := costResolver(stack.ItemID)
			if err != nil {
				return RepairResult{}, err
			}
			if evidence.MaxDurability <= 0 || !RepairAllEligible(evidence.EquipmentType) {
				continue
			}
			current, err := repairBagDurabilityOf(stack)
			if err != nil || current >= evidence.MaxDurability {
				continue
			}
			upgradeLevel := 0
			if len(stack.RawEntry) > 0x0A {
				upgradeLevel = int(stack.RawEntry[0x0A] & 0x1F)
			}
			cost := CalcRepairCost(evidence, current, upgradeLevel, cmd.QuickRepair)
			targets = append(targets, repairAllTarget{key: key, slot: slot, itemID: stack.ItemID, maxDura: evidence.MaxDurability, cost: cost})
			total += cost
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].equipped != targets[j].equipped {
			return targets[i].equipped
		}
		return targets[i].slot < targets[j].slot
	})

	gold := character.Stats["gold"]
	result := RepairResult{
		CharacterID: characterID,
		SlotIndex:   -1,
		FreeRepair:  freeRepair,
	}
	if len(targets) == 0 {
		result.UpdatedGold = gold
		return result, nil
	}
	if cmd.ContractAutoRepair {
		// 服务端触发的契约自动修理：先花掉角色当天的一次额度，花不出去就什么都不做。
		// 顺序很重要 —— 先扣额度再修，保证"用完 6 次"与"不收费"两条不会互相打架。
		if !autoRepairActive || !premium.TryConsumeDaily(&character, premium.DevilSlotAutoRepair, time.Now().UTC()) {
			result.ContractSkipped = true
			result.UpdatedGold = gold
			return result, nil
		}
		freeRepair = true
		result.FreeRepair = true
		total = 0
		for index := range targets {
			targets[index].cost = 0
		}
	} else if autoRepairActive && premium.TryConsumeDaily(&character, premium.DevilSlotAutoRepair, time.Now().UTC()) {
		freeRepair = true
		result.FreeRepair = true
		total = 0
		for index := range targets {
			targets[index].cost = 0
		}
	}
	if total > gold {
		return RepairResult{}, fmt.Errorf("%w: cost=%d gold=%d", ErrRepairGoldInsufficient, total, gold)
	}
	if total > 0 || freeRepair {
		character.Stats["gold"] = gold - total
		character.UpdatedAt = time.Now()
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return RepairResult{}, err
		}
	}
	for _, target := range targets {
		if target.equipped {
			entry := equipRecord.Entries[target.key]
			entry.RawEntry = append([]byte(nil), entry.RawEntry...)
			binary.LittleEndian.PutUint16(entry.RawEntry[target.durabilityOffset:target.durabilityOffset+2], uint16(target.maxDura))
			if entry.Extra == nil {
				entry.Extra = make(map[string]string, 1)
			}
			entry.Extra["durability"] = strconv.FormatInt(target.maxDura, 10)
			equipRecord.Entries[target.key] = entry
			continue
		}
		stack := invRecord.Slots[target.key]
		stack = cloneItemStackForRepair(stack)
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 2)
		}
		stack.Extra["durability"] = strconv.FormatInt(target.maxDura, 10)
		if durabilityOffset, ok := durabilityOffsetForRaw(stack.ItemID, stack.RawEntry); ok {
			stack.RawEntry = append([]byte(nil), stack.RawEntry...)
			binary.LittleEndian.PutUint16(stack.RawEntry[durabilityOffset:durabilityOffset+2], uint16(target.maxDura))
		}
		invRecord.Slots[target.key] = stack
	}
	if len(targets) > 0 {
		equipRecord.UpdatedAt = time.Now()
		if equipFound {
			if err := dnfrepo.SaveEquipmentFields(ctx, equipment, equipRecord, dnfrepo.EquipmentFieldEntries); err != nil {
				return RepairResult{}, err
			}
		}
		invRecord.UpdatedAt = time.Now()
		if invFound {
			if err := dnfrepo.SaveInventoryFields(ctx, inventory, invRecord, dnfrepo.InventoryFieldSlots); err != nil {
				return RepairResult{}, err
			}
		}
	}
	result.Cost = total
	result.UpdatedGold = gold - total
	result.Changed = true
	result.RepairedCount = len(targets)
	result.RepairedSlots = make([]RepairSlot, 0, len(targets))
	for _, target := range targets {
		result.RepairedSlots = append(result.RepairedSlots, RepairSlot{
			Equipped:  target.equipped,
			SlotIndex: target.slot,
		})
	}
	return result, nil
}

func upgradeLevelOfRawEntry(raw []byte) int {
	if len(raw) > 0x0A {
		return int(raw[0x0A] & 0x1F)
	}
	return 0
}

func repairBagDurabilityOf(stack dnfrepo.ItemStack) (int64, error) {
	if stack.Extra == nil {
		return 0, ErrRepairCostMissing
	}
	raw := strings.TrimSpace(stack.Extra["durability"])
	if raw == "" {
		return 0, ErrRepairCostMissing
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%w: %q", ErrRepairCostInvalid, raw)
	}
	return value, nil
}

func cloneItemStackForRepair(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	if stack.Extra != nil {
		extra := make(map[string]string, len(stack.Extra)+1)
		for key, value := range stack.Extra {
			extra[key] = value
		}
		stack.Extra = extra
	}
	return stack
}

// premiumActive reports whether the account currently holds an active premium
// contract of the given type. A missing account repository or record reads as
// inactive, so only the explicit free-repair bonus is gated by it.
func (o *Owner) premiumActive(ctx context.Context, accountID string, premiumType int64) bool {
	if o == nil || o.accounts == nil || strings.TrimSpace(accountID) == "" {
		return false
	}
	account, ok, err := o.accounts.Load(ctx, accountID)
	if err != nil || !ok {
		return false
	}
	return premium.Active(account, premiumType, time.Now().UTC())
}

// Move 在装备槽和背包/装扮/宠物物品栏之间迁移 raw entry。
// Ordinary placement requires structural bounds plus a typed PVF/character validator.
func (o *Owner) Move(ctx context.Context, cmd MoveCommand) (MoveResult, error) {
	if o == nil || o.equipment == nil {
		return MoveResult{}, ErrOwnerUnavailable
	}
	if o.inventory == nil {
		return MoveResult{}, ErrInventoryUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return MoveResult{}, ErrCharacterRequired
	}
	kind, err := validateMoveStructure(cmd)
	if err != nil {
		return MoveResult{}, err
	}
	creatureMove := kind == moveKindPet
	petAggregateMove := creatureMove || kind == moveKindArtifact
	// Current NoPack's Vagabond "change sub weapon" button reuses op19 for
	// the worn weapon endpoints 12 and 24, but encodes the equipment-slot swap
	// with count=0. Ordinary equip/unequip requests still use count=1. Accept
	// only this exact dual-weapon endpoint pair so a zero count cannot broaden
	// any inventory, avatar, pet, or unrelated equipment mutation. The class /
	// grow-type / beamsword gate for slot 24 stays in the placement validator
	// (currentEquipmentPlacementAllowsBladeDancerOffhand).
	if cmd.MoveCount != 1 && !isDualWeaponToggleMove(cmd) {
		return MoveResult{}, fmt.Errorf("%w: count=%d", ErrMoveStackCountInvalid, cmd.MoveCount)
	}
	if !creatureMove && o.placementValidator == nil {
		return MoveResult{}, ErrMoveValidatorRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !o.inItemTransaction {
		var result MoveResult
		if petAggregateMove {
			if o.pets == nil {
				return MoveResult{}, ErrPetRepositoryRequired
			}
			if o.petItems == nil {
				return MoveResult{}, ErrPetMoveTxnRequired
			}
			err = o.petItems.WithinCharacterPets(ctx, characterID, func(inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository, pets dnfrepo.PetRepository) error {
				txOwner := &Owner{
					equipment:          equipment,
					inventory:          inventory,
					character:          o.character,
					pets:               pets,
					placementValidator: o.placementValidator,
					inItemTransaction:  true,
				}
				var err error
				result, err = txOwner.Move(ctx, cmd)
				return err
			})
		} else {
			if o.items == nil {
				return MoveResult{}, ErrMoveTransactionRequired
			}
			err = o.items.WithinCharacterItems(ctx, characterID, func(inventory dnfrepo.InventoryRepository, equipment dnfrepo.EquipmentRepository) error {
				txOwner := &Owner{
					equipment:                   equipment,
					inventory:                   inventory,
					character:                   o.character,
					placementValidator:          o.placementValidator,
					avatarClassifier:            o.avatarClassifier,
					auroraLookReplaceClassifier: o.auroraLookReplaceClassifier,
					inItemTransaction:           true,
				}
				var err error
				result, err = txOwner.Move(ctx, cmd)
				return err
			})
		}
		return result, err
	}
	if !petAggregateMove {
		if _, err := o.normalizeLegacyPVFInitialEquipmentSlots(ctx, characterID); err != nil {
			return MoveResult{}, err
		}
	} else if creatureMove {
		if _, err := creaturestate.ReconcileInventory(ctx, characterID, o.inventory, o.equipment, o.pets); err != nil {
			return MoveResult{}, err
		}
	}

	// Current NoPack op19 carries two endpoint addresses, not a reliable move
	// direction. Equip and unequip requests keep the inventory/avatar/cargo
	// endpoint first and the worn endpoint second; only the endpoint instance
	// values and authoritative server state change. Resolve the direction from
	// the transaction-scoped DB snapshot so the decision and mutation are
	// serialized by CharacterItemUnitOfWork.
	resolved, reversed, err := o.resolveMoveEndpoints(ctx, characterID, cmd)
	if err != nil {
		return MoveResult{}, err
	}
	var result MoveResult
	if resolved.SourceListType == wireListEquipment && resolved.DestinationListType == wireListEquipment {
		result, err = o.swapEquipSlots(ctx, resolved)
	} else if resolved.SourceListType == wireListEquipment {
		result, err = o.unequip(ctx, resolved)
	} else {
		result, err = o.equip(ctx, resolved)
	}
	if err != nil {
		return MoveResult{}, err
	}
	switch kind {
	case moveKindPet:
		if err := o.syncPetEquipmentMove(ctx, characterID, result); err != nil {
			return MoveResult{}, err
		}
	case moveKindArtifact:
		if err := o.syncPetArtifactEquipmentMove(ctx, characterID, result); err != nil {
			return MoveResult{}, err
		}
	}
	if reversed {
		// Keep the request's A/B list direction, but return the authoritative
		// inventory slot selected by the owner. Cross-list op19 accepts a
		// redirected final slot; this is required when the client's stale bag
		// view nominates an already occupied unequip destination.
		result.SourceListType = cmd.SourceListType
		result.SourceSlotIndex = result.DestinationSlotIndex
		result.DestinationListType = cmd.DestinationListType
		result.DestinationSlotIndex = cmd.DestinationSlotIndex
	}
	return result, nil
}

type legacyPVFInitialEquipmentSlotMove struct {
	sourceKey string
	targetKey string
	target    int16
	entry     dnfrepo.EquipmentEntry
}

// normalizeLegacyPVFInitialEquipmentSlots converts only untouched
// source=pvf_create_equipment_list records from the legacy worn-slot keys to
// the current EXE actor-slot keys used by op19. It runs inside the same
// CharacterItemUnitOfWork as the requested move, so a later placement/save
// failure rolls the migration back with the move. Runtime rows and already
// migrated PVF rows carry current_exe_runtime_move and are never reinterpreted.
func (o *Owner) normalizeLegacyPVFInitialEquipmentSlots(ctx context.Context, characterID string) (bool, error) {
	record, found, err := o.equipment.Load(ctx, characterID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrEquipmentNotFound
	}
	record = ensureEquipmentRecord(dnfrepo.CloneEquipment(record), characterID)

	plans := make(map[string]legacyPVFInitialEquipmentSlotMove)
	targets := make(map[string]string)
	for key, entry := range record.Entries {
		if !isUntouchedLegacyPVFInitialEquipment(entry) {
			continue
		}
		target, ok := currentActorSlotForLegacyPVFInitialSlot(entry.SlotIndex)
		if !ok {
			continue
		}
		sourceKey := entryKey(entry.SlotIndex)
		if key != sourceKey {
			return false, fmt.Errorf("%w: key=%q entry=%d", ErrMoveSlotMismatch, key, entry.SlotIndex)
		}
		targetKey := entryKey(target)
		if previous, exists := targets[targetKey]; exists {
			return false, fmt.Errorf(
				"%w: source=%s other_source=%s target=%s",
				ErrLegacyPVFSlotCollision,
				sourceKey,
				previous,
				targetKey,
			)
		}
		targets[targetKey] = sourceKey
		plans[sourceKey] = legacyPVFInitialEquipmentSlotMove{
			sourceKey: sourceKey,
			targetKey: targetKey,
			target:    target,
			entry:     entry,
		}
	}
	if len(plans) == 0 {
		return false, nil
	}

	// A target occupied by another planned legacy row is safe because every
	// source key is deleted before any target key is inserted. Any other
	// occupant is authoritative current/runtime state and must fail closed.
	for _, plan := range plans {
		if _, occupied := record.Entries[plan.targetKey]; !occupied {
			continue
		}
		if _, movingAway := plans[plan.targetKey]; !movingAway {
			return false, fmt.Errorf(
				"%w: source=%s target=%s",
				ErrLegacyPVFSlotCollision,
				plan.sourceKey,
				plan.targetKey,
			)
		}
	}

	for sourceKey := range plans {
		delete(record.Entries, sourceKey)
	}
	for _, plan := range plans {
		entry := plan.entry
		entry.SlotIndex = plan.target
		entry.Extra = cloneExtra(entry.Extra)
		entry.Extra["equipped_slot"] = strconv.Itoa(int(plan.target))
		entry.Extra["current_exe_equipment_type"] = strconv.Itoa(int(plan.target))
		entry.Extra["current_exe_runtime_move"] = "1"
		record.Entries[plan.targetKey] = entry
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, record, dnfrepo.EquipmentFieldEntries); err != nil {
		return false, err
	}
	return true, nil
}

func isUntouchedLegacyPVFInitialEquipment(entry dnfrepo.EquipmentEntry) bool {
	if !strings.EqualFold(strings.TrimSpace(entry.Extra["source"]), "pvf_create_equipment_list") {
		return false
	}
	raw := strings.TrimSpace(strings.ToLower(entry.Extra["current_exe_runtime_move"]))
	switch raw {
	case "", "0", "false", "no", "off":
		return true
	default:
		// Unknown non-empty markers also fail closed: they may represent a
		// newer runtime migration state that this owner must not reinterpret.
		return false
	}
}

func currentActorSlotForLegacyPVFInitialSlot(slot int16) (int16, bool) {
	switch {
	case slot == 11:
		return 12, true
	case slot >= 13 && slot <= 22:
		return slot + 1, true
	case slot == 23:
		return 25, true
	default:
		return 0, false
	}
}

func (o *Owner) resolveMoveEndpoints(ctx context.Context, characterID string, cmd MoveCommand) (MoveCommand, bool, error) {
	var inventory dnfrepo.InventoryRecord
	if cmd.SourceListType != wireListEquipment || cmd.DestinationListType != wireListEquipment {
		loaded, ok, err := o.inventory.Load(ctx, characterID)
		if err != nil {
			return MoveCommand{}, false, err
		}
		if !ok {
			return MoveCommand{}, false, ErrInventoryNotFound
		}
		inventory = ensureInventoryRecord(dnfrepo.CloneInventory(loaded), characterID)
	}
	equipment, ok, err := o.equipment.Load(ctx, characterID)
	if err != nil {
		return MoveCommand{}, false, err
	}
	if !ok {
		return MoveCommand{}, false, ErrEquipmentNotFound
	}
	equipment = ensureEquipmentRecord(dnfrepo.CloneEquipment(equipment), characterID)
	aOccupied, err := moveEndpointOccupied(&inventory, &equipment, cmd.SourceListType, cmd.SourceSlotIndex)
	if err != nil {
		return MoveCommand{}, false, err
	}
	bOccupied, err := moveEndpointOccupied(&inventory, &equipment, cmd.DestinationListType, cmd.DestinationSlotIndex)
	if err != nil {
		return MoveCommand{}, false, err
	}
	if !aOccupied && !bOccupied {
		return MoveCommand{}, false, fmt.Errorf(
			"%w: endpoints=(%d,%d),(%d,%d)",
			ErrMoveSlotNotFound,
			cmd.SourceListType,
			cmd.SourceSlotIndex,
			cmd.DestinationListType,
			cmd.DestinationSlotIndex,
		)
	}
	// The current 28-byte op19 body carries the live instance value for both
	// endpoints. A zero A instance and nonzero B instance is the observed
	// unequip form. Prefer that direction even when the server still has an
	// item in A because a missed/early list0 snapshot can leave the client's
	// chosen empty destination stale. The owner will relocate to the next real
	// empty slot in the same proven inventory page instead of corrupting the
	// worn slot with A.
	if cmd.SourceInstanceValue == 0 && cmd.DestinationInstanceValue != 0 && bOccupied {
		return reverseMoveEndpoints(cmd), true, nil
	}
	if cmd.SourceInstanceValue != 0 && cmd.DestinationInstanceValue == 0 && aOccupied {
		return cmd, false, nil
	}
	if aOccupied {
		return cmd, false, nil
	}
	return reverseMoveEndpoints(cmd), true, nil
}

func moveEndpointOccupied(inventory *dnfrepo.InventoryRecord, equipment *dnfrepo.EquipmentRecord, listType byte, slotIndex int16) (bool, error) {
	if listType == wireListEquipment {
		entry, occupied := equipment.Entries[entryKey(slotIndex)]
		if occupied && entry.SlotIndex != slotIndex {
			return false, fmt.Errorf("%w: key=%d entry=%d", ErrMoveSlotMismatch, slotIndex, entry.SlotIndex)
		}
		return occupied, nil
	}
	items, _, ok := inventoryMap(inventory, listType)
	if !ok {
		return false, fmt.Errorf("%w: endpoint list=%d", ErrMoveUnsupported, listType)
	}
	_, occupied := items[inventoryKey(listType, slotIndex)]
	return occupied, nil
}

func reverseMoveEndpoints(cmd MoveCommand) MoveCommand {
	return MoveCommand{
		SelectedCharacterID:      cmd.SelectedCharacterID,
		SourceListType:           cmd.DestinationListType,
		SourceSlotIndex:          cmd.DestinationSlotIndex,
		SourceInstanceValue:      cmd.DestinationInstanceValue,
		MoveCount:                cmd.MoveCount,
		DestinationListType:      cmd.SourceListType,
		DestinationSlotIndex:     cmd.SourceSlotIndex,
		DestinationInstanceValue: cmd.SourceInstanceValue,
	}
}

func (o *Owner) equip(ctx context.Context, cmd MoveCommand) (MoveResult, error) {
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	inventory, equipment, err := o.loadMoveRecords(ctx, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	srcItems, srcField, ok := inventoryMap(&inventory, cmd.SourceListType)
	if !ok {
		return MoveResult{}, fmt.Errorf("%w: source list=%d", ErrMoveUnsupported, cmd.SourceListType)
	}
	if cmd.SourceListType == wireListGuildMedal && !guildMedalInventorySlotContains(cmd.SourceSlotIndex) {
		return MoveResult{}, fmt.Errorf("%w: guild-medal source slot=%d", ErrMoveUnsupported, cmd.SourceSlotIndex)
	}
	srcKey := inventoryKey(cmd.SourceListType, cmd.SourceSlotIndex)
	source, ok := srcItems[srcKey]
	if !ok {
		return MoveResult{}, fmt.Errorf("%w: source list=%d slot=%d", ErrMoveSlotNotFound, cmd.SourceListType, cmd.SourceSlotIndex)
	}
	if !isEquipmentMoveUnit(source, cmd.SourceListType) {
		return MoveResult{}, fmt.Errorf("%w: count=%d", ErrMoveStackCountInvalid, source.Count)
	}
	entry, err := entryFromStack(source, cmd.SourceListType, cmd.DestinationSlotIndex, cmd.SourceInstanceValue)
	if err != nil {
		return MoveResult{}, err
	}
	entry = removeSealOnFirstEquipmentUse(entry, source)
	if err := o.validatePlacement(ctx, characterID, source.ItemID, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationSlotIndex); err != nil {
		return MoveResult{}, err
	}

	result := MoveResult{
		CharacterID:          characterID,
		SourceListType:       cmd.SourceListType,
		SourceSlotIndex:      cmd.SourceSlotIndex,
		DestinationListType:  cmd.DestinationListType,
		DestinationSlotIndex: cmd.DestinationSlotIndex,
		ItemID:               source.ItemID,
		Mode:                 "equip",
	}
	dstKey := entryKey(cmd.DestinationSlotIndex)
	previous, occupied := equipment.Entries[dstKey]
	entry = o.withCloneAvatarAppearance(entry, previous, occupied, cmd.DestinationSlotIndex,
		inventoryKey(cmd.SourceListType, cmd.SourceSlotIndex))
	saveFields := []dnfrepo.InventoryField{srcField}
	if occupied {
		if previous.SlotIndex != cmd.DestinationSlotIndex {
			return MoveResult{}, fmt.Errorf("%w: key=%d entry=%d", ErrMoveSlotMismatch, cmd.DestinationSlotIndex, previous.SlotIndex)
		}
		displaced := stackFromEntry(clearCloneAvatarAppearance(previous))
		// 【2026-09-28 修复】被顶下来的若是「从称号簿穿上来的」成就称号，必须回称号簿，
		// 而不是落进玩家请求里的那个背包格。玩家「直接穿背包里的旧称号」时客户端声明的
		// 落点就是那个背包格，成就称号因此掉进背包（客户端字符串 48178：成就称号无法
		// 移动到物品栏或金库）。改写落点后，新称号原来占的背包格要显式清掉。
		if bookList, bookSlot, isBookTitle := titleBookOriginDestination(previous); isBookTitle {
			bookItems, bookField, mapOK := inventoryMap(&inventory, bookList)
			if !mapOK {
				return MoveResult{}, fmt.Errorf("%w: title book destination list=%d", ErrMoveUnsupported, bookList)
			}
			resolved, slotOK := titleBookReturnSlot(bookItems, bookList, bookSlot)
			if !slotOK {
				return MoveResult{}, fmt.Errorf(
					"%w: title book category full for equip slot=%d",
					ErrMoveSlotNotFound, cmd.DestinationSlotIndex,
				)
			}
			bookItems[inventoryKey(bookList, resolved)] = displaced
			delete(srcItems, srcKey)
			if bookField != srcField {
				saveFields = append(saveFields, bookField)
			}
		} else {
			srcItems[srcKey] = displaced
		}
		result.SwappedItemID = previous.ItemID
		result.Mode = "equip_swap"
	} else {
		delete(srcItems, srcKey)
	}
	equipment.Entries[dstKey] = entry
	return o.saveMove(ctx, inventory, equipment, saveFields, result)
}

func removeSealOnFirstEquipmentUse(entry dnfrepo.EquipmentEntry, source dnfrepo.ItemStack) dnfrepo.EquipmentEntry {
	// The current 0x77 item row's byte +0x0D is the visible sealed-state
	// byte. Runtime-PVF [attach type] [sealing] grants persist it as the
	// explicit seal_flag extra. The first successful equip consumes that
	// state permanently; otherwise unequipping copies the old flag back and
	// the client asks to remove the seal every time.
	if firstExtraInt(source.Extra, 0, "seal_flag", "seal") == 0 {
		return entry
	}
	if len(entry.RawEntry) > 0x0D {
		entry.RawEntry[0x0D] = 0
	}
	if entry.Extra == nil {
		entry.Extra = make(map[string]string, 4)
	}
	for _, key := range []string{"seal_flag", "seal", "equipment_update_seal_flag", "update_seal_flag"} {
		delete(entry.Extra, key)
	}
	entry.Extra["seal_removed_by_first_equip"] = "1"
	entry.Extra["trade_locked_by_first_equip"] = "1"
	return entry
}

func (o *Owner) unequip(ctx context.Context, cmd MoveCommand) (MoveResult, error) {
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	inventory, equipment, err := o.loadMoveRecords(ctx, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	dstItems, dstField, ok := inventoryMap(&inventory, cmd.DestinationListType)
	if !ok {
		return MoveResult{}, fmt.Errorf("%w: destination list=%d", ErrMoveUnsupported, cmd.DestinationListType)
	}
	if cmd.DestinationListType == wireListGuildMedal && !guildMedalInventorySlotContains(cmd.DestinationSlotIndex) {
		return MoveResult{}, fmt.Errorf("%w: guild-medal destination slot=%d", ErrMoveUnsupported, cmd.DestinationSlotIndex)
	}
	srcKey := entryKey(cmd.SourceSlotIndex)
	source, ok := equipment.Entries[srcKey]
	if !ok {
		return MoveResult{}, fmt.Errorf("%w: equip slot=%d", ErrMoveSlotNotFound, cmd.SourceSlotIndex)
	}
	if source.SlotIndex != cmd.SourceSlotIndex {
		return MoveResult{}, fmt.Errorf("%w: key=%d entry=%d", ErrMoveSlotMismatch, cmd.SourceSlotIndex, source.SlotIndex)
	}
	// PR #239: name tag cards can only be replaced by purchase, not manually unequipped.
	if o.nameTagChecker != nil && source.ItemID > 0 && o.nameTagChecker(uint32(source.ItemID)) {
		return MoveResult{}, fmt.Errorf("%w: name tag card cannot be unequipped", ErrMoveUnsupported)
	}
	// 【2026-09-28 修复】簿内资产（成就称号）的两处收口：
	//
	//  ① 卡死行放行：achievement.grantTitle 入簿时不写 RawEntry，titlebook.Equip
	//     过去又原样把空 raw 搬进装备行，于是这里第二道门
	//     `len(source.RawEntry) == 0 → ErrMoveRawEntryMissing` 会把"摘称号"永久
	//     挡掉 —— 玩家看到的就是"称号展示栏卡了一个成就称号，摘不下来"（礼白666）。
	//     簿内资产我们知道它的物品号和槽位，就地补一行合法行数据放行，玩家重登后
	//     正常摘一次即自愈，不需要改库。
	//  ② 落点改写：成就称号只能在称号簿 ↔ 装备栏之间移动（客户端字符串 48178
	//     「通过成就任务获得的称号，无法移动到物品栏或金库。」），摘下来必须回簿，
	//     绝不能落进客户端声明的那个背包格。
	bookList, bookSlot, isBookTitle := titleBookOriginDestination(source)
	if len(source.RawEntry) == 0 {
		if !isBookTitle {
			return MoveResult{}, ErrMoveRawEntryMissing
		}
		source.RawEntry = buildCurrentTitleEquipmentRawEntry(source.ItemID, cmd.SourceSlotIndex)
	}
	if isBookTitle {
		bookItems, bookField, mapOK := inventoryMap(&inventory, bookList)
		if !mapOK {
			return MoveResult{}, fmt.Errorf("%w: title book destination list=%d", ErrMoveUnsupported, bookList)
		}
		resolved, slotOK := titleBookReturnSlot(bookItems, bookList, bookSlot)
		if !slotOK {
			return MoveResult{}, fmt.Errorf(
				"%w: title book category full for equip slot=%d",
				ErrMoveSlotNotFound, cmd.SourceSlotIndex,
			)
		}
		cmd.DestinationListType = bookList
		cmd.DestinationSlotIndex = resolved
		dstItems, dstField = bookItems, bookField
	}

	result := MoveResult{
		CharacterID:          characterID,
		SourceListType:       cmd.SourceListType,
		SourceSlotIndex:      cmd.SourceSlotIndex,
		DestinationListType:  cmd.DestinationListType,
		DestinationSlotIndex: cmd.DestinationSlotIndex,
		ItemID:               source.ItemID,
		Mode:                 "unequip",
	}
	dstKey := inventoryKey(cmd.DestinationListType, cmd.DestinationSlotIndex)
	if _, occupied := dstItems[dstKey]; occupied && cmd.DestinationInstanceValue == 0 {
		actualSlot, found := nextEmptyUnequipInventorySlot(dstItems, cmd.DestinationListType, cmd.DestinationSlotIndex)
		if !found {
			return MoveResult{}, fmt.Errorf("%w: no empty unequip destination after list=%d slot=%d", ErrMoveSlotNotFound, cmd.DestinationListType, cmd.DestinationSlotIndex)
		}
		cmd.DestinationSlotIndex = actualSlot
		result.DestinationSlotIndex = actualSlot
		dstKey = inventoryKey(cmd.DestinationListType, actualSlot)
	}
	if previous, occupied := dstItems[dstKey]; occupied {
		entry, err := entryFromStack(previous, cmd.DestinationListType, cmd.SourceSlotIndex, cmd.DestinationInstanceValue)
		if err != nil {
			return MoveResult{}, err
		}
		if err := o.validatePlacement(ctx, characterID, previous.ItemID, cmd.DestinationListType, cmd.DestinationSlotIndex, cmd.SourceSlotIndex); err != nil {
			return MoveResult{}, err
		}
		entry = o.withCloneAvatarAppearance(entry, source, true, cmd.SourceSlotIndex,
			inventoryKey(cmd.DestinationListType, cmd.DestinationSlotIndex))
		equipment.Entries[srcKey] = entry
		result.SwappedItemID = previous.ItemID
		result.Mode = "unequip_swap"
	} else {
		delete(equipment.Entries, srcKey)
	}
	dstItems[dstKey] = stackFromEntry(clearCloneAvatarAppearance(source))
	return o.saveMove(ctx, inventory, equipment, []dnfrepo.InventoryField{dstField}, result)
}

// currentAuraEquipmentSlot 是客户端唯一消费 USERINFO 关联块的穿戴槽。A21/90CN
// 客户端的装备 entry 读取器只在 slot == 9 时读 `u32 长度 + 原实例镶嵌`（见
// tools 里的逆向结论），所以转职光环只在槽 9 记录关联来源；放到其它槽（例如
// 光环幻化栏 11）时它既不需要也不应该带关联。
const currentAuraEquipmentSlot int16 = 9

func (o *Owner) withCloneAvatarAppearance(entry, previous dnfrepo.EquipmentEntry, occupied bool, slotIndex int16, linkedSource string) dnfrepo.EquipmentEntry {
	entry.Extra = cloneExtra(entry.Extra)
	delete(entry.Extra, dnfrepo.CloneAvatarAppearanceExtraKey)
	delete(entry.Extra, dnfrepo.AuraLinkedTemplateExtraKey)
	delete(entry.Extra, dnfrepo.AuraLinkedSourceExtraKey)
	if entry.ItemID <= 0 || entry.ItemID > int64(^uint32(0)) {
		return entry
	}
	// 转职光环走独立分支，且必须在 avatarClassifier 判空之前判断：它同样属于
	// `clear avatar`，但语义与克隆装扮相反——外观保留自己，被覆盖的光环只作为
	// 关联来源。旧代码把它当克隆装扮处理，于是把被覆盖光环的模板写进外观 key，
	// 客户端渲染回原光环，正是"原光环外观没有被替代"的现象。
	if o.isAuroraLookReplace(uint32(entry.ItemID)) {
		if slotIndex != currentAuraEquipmentSlot {
			return entry
		}
		if !occupied || previous.ItemID <= 0 || previous.ItemID > int64(^uint32(0)) {
			return entry
		}
		// 被覆盖的必须是一件普通光环；转职光环自己不能充当属性来源（参考实现的
		// IsAuroraStatSourceAvatar 同一口径）。绑定失效时什么都不写，绝不猜。
		if o.isAuroraLookReplace(uint32(previous.ItemID)) {
			return entry
		}
		if o.avatarClassifier == nil {
			return entry
		}
		if isAvatar, _ := o.avatarClassifier(uint32(previous.ItemID)); !isAvatar {
			return entry
		}
		entry.Extra[dnfrepo.AuraLinkedTemplateExtraKey] = strconv.FormatInt(previous.ItemID, 10)
		// 记录被覆盖光环的落点，投影时据此实时取它的镶嵌字节（不能穿戴时快照：
		// 玩家完全可能穿好转职光环之后才给那件光环打徽章）。没有落点时只保留
		// 模板，不编造来源。
		if source := strings.TrimSpace(linkedSource); source != "" {
			entry.Extra[dnfrepo.AuraLinkedSourceExtraKey] = source
		}
		return entry
	}
	if o == nil || o.avatarClassifier == nil {
		return entry
	}
	_, isClone := o.avatarClassifier(uint32(entry.ItemID))
	if !isClone || !occupied || previous.ItemID <= 0 || previous.ItemID > int64(^uint32(0)) {
		return entry
	}
	isAvatar, previousIsClone := o.avatarClassifier(uint32(previous.ItemID))
	if !isAvatar {
		return entry
	}
	appearanceID := previous.ItemID
	if previousIsClone {
		appearanceID = firstExtraInt(previous.Extra, 0, dnfrepo.CloneAvatarAppearanceExtraKey)
	}
	if appearanceID <= 0 || appearanceID > int64(^uint32(0)) {
		return entry
	}
	entry.Extra[dnfrepo.CloneAvatarAppearanceExtraKey] = strconv.FormatInt(appearanceID, 10)
	return entry
}

func clearCloneAvatarAppearance(entry dnfrepo.EquipmentEntry) dnfrepo.EquipmentEntry {
	entry.Extra = cloneExtra(entry.Extra)
	delete(entry.Extra, dnfrepo.CloneAvatarAppearanceExtraKey)
	delete(entry.Extra, dnfrepo.AuraLinkedTemplateExtraKey)
	delete(entry.Extra, dnfrepo.AuraLinkedSourceExtraKey)
	return entry
}

const (
	mainEquipmentInventorySlotStart int16 = 9
	mainEquipmentInventorySlotEnd   int16 = 64
	avatarInventorySlotStart        int16 = 0
	avatarInventorySlotEnd          int16 = 209
	petBodyInventorySlotStart       int16 = 0
	petBodyInventorySlotEnd         int16 = 139
	petArtifactInventorySlotStart   int16 = 140
	petArtifactInventorySlotEnd     int16 = 188
	guildMedalInventorySlotStart    int16 = 0
	guildMedalInventorySlotEnd      int16 = 48
)

func nextEmptyUnequipInventorySlot(items map[string]dnfrepo.ItemStack, listType byte, requested int16) (int16, bool) {
	start, end := mainEquipmentInventorySlotStart, mainEquipmentInventorySlotEnd
	switch listType {
	case wireListMain:
		// Equipment page only. Slots 0..8 are the main-list quick bar and the
		// later PVF pages own consumables, materials, quests, and professions.
	case wireListAvatar:
		// Current NoPack list-1 sort/page reader owns the 0..209 avatar page.
		start, end = avatarInventorySlotStart, avatarInventorySlotEnd
	case wireListPet:
		// Current NoPack splits list 7 into creature bodies 0..139 and
		// creature artifacts 140..188. Never redirect an unequip across those
		// pages; slots 189..238 are pet consumables and are not worn endpoints.
		switch {
		case requested >= petBodyInventorySlotStart && requested <= petBodyInventorySlotEnd:
			start, end = petBodyInventorySlotStart, petBodyInventorySlotEnd
		case requested >= petArtifactInventorySlotStart && requested <= petArtifactInventorySlotEnd:
			start, end = petArtifactInventorySlotStart, petArtifactInventorySlotEnd
		default:
			return 0, false
		}
	case wireListGuildMedal:
		// Current NoPack sub_E9AC00/sub_E9AC30 split the shared 98-slot
		// list-38 container into two 49-slot pages. Medal equip/unequip owns
		// only the first page; slots 49..97 belong to guardian gems.
		start, end = guildMedalInventorySlotStart, guildMedalInventorySlotEnd
	default:
		return 0, false
	}
	if requested < start || requested > end {
		return 0, false
	}
	for slot := requested; slot <= end; slot++ {
		if _, occupied := items[inventoryKey(listType, slot)]; !occupied {
			return slot, true
		}
	}
	for slot := start; slot < requested; slot++ {
		if _, occupied := items[inventoryKey(listType, slot)]; !occupied {
			return slot, true
		}
	}
	return 0, false
}

func guildMedalInventorySlotContains(slot int16) bool {
	return slot >= guildMedalInventorySlotStart && slot <= guildMedalInventorySlotEnd
}

func (o *Owner) swapEquipSlots(ctx context.Context, cmd MoveCommand) (MoveResult, error) {
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	record, ok, err := o.equipment.Load(ctx, characterID)
	if err != nil {
		return MoveResult{}, err
	}
	if !ok {
		return MoveResult{}, ErrEquipmentNotFound
	}
	record = ensureEquipmentRecord(dnfrepo.CloneEquipment(record), characterID)
	srcKey := entryKey(cmd.SourceSlotIndex)
	dstKey := entryKey(cmd.DestinationSlotIndex)
	source, sourceOK := record.Entries[srcKey]
	destination, destinationOK := record.Entries[dstKey]
	if !sourceOK {
		return MoveResult{}, fmt.Errorf("%w: equip slot=%d", ErrMoveSlotNotFound, cmd.SourceSlotIndex)
	}
	if source.SlotIndex != cmd.SourceSlotIndex {
		return MoveResult{}, fmt.Errorf("%w: key=%d entry=%d", ErrMoveSlotMismatch, cmd.SourceSlotIndex, source.SlotIndex)
	}
	if len(source.RawEntry) == 0 {
		return MoveResult{}, ErrMoveRawEntryMissing
	}
	if destinationOK && destination.SlotIndex != cmd.DestinationSlotIndex {
		return MoveResult{}, fmt.Errorf("%w: key=%d entry=%d", ErrMoveSlotMismatch, cmd.DestinationSlotIndex, destination.SlotIndex)
	}
	if destinationOK && len(destination.RawEntry) == 0 {
		return MoveResult{}, ErrMoveRawEntryMissing
	}
	if cmd.SourceSlotIndex == cmd.DestinationSlotIndex {
		return MoveResult{CharacterID: characterID, SourceListType: cmd.SourceListType, SourceSlotIndex: cmd.SourceSlotIndex, DestinationListType: cmd.DestinationListType, DestinationSlotIndex: cmd.DestinationSlotIndex, ItemID: source.ItemID, Mode: "noop"}, nil
	}
	if err := o.validatePlacement(ctx, characterID, source.ItemID, wireListEquipment, cmd.SourceSlotIndex, cmd.DestinationSlotIndex); err != nil {
		return MoveResult{}, err
	}
	if destinationOK {
		if err := o.validatePlacement(ctx, characterID, destination.ItemID, wireListEquipment, cmd.DestinationSlotIndex, cmd.SourceSlotIndex); err != nil {
			return MoveResult{}, err
		}
	}
	source = equipmentEntryMovedToSlot(source, cmd.DestinationSlotIndex)
	record.Entries[dstKey] = source
	if destinationOK {
		destination = equipmentEntryMovedToSlot(destination, cmd.SourceSlotIndex)
		record.Entries[srcKey] = destination
	} else {
		delete(record.Entries, srcKey)
	}
	record.UpdatedAt = time.Now()
	if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, record, dnfrepo.EquipmentFieldEntries); err != nil {
		return MoveResult{}, err
	}
	result := MoveResult{
		CharacterID:          characterID,
		SourceListType:       cmd.SourceListType,
		SourceSlotIndex:      cmd.SourceSlotIndex,
		DestinationListType:  cmd.DestinationListType,
		DestinationSlotIndex: cmd.DestinationSlotIndex,
		ItemID:               source.ItemID,
		Mode:                 "equip_slot_swap",
		Changed:              true,
	}
	if destinationOK {
		result.SwappedItemID = destination.ItemID
	}
	return result, nil
}

// equipmentEntryMovedToSlot keeps the authoritative map key/SlotIndex and the
// current-client runtime metadata in lockstep. The Vagabond dual-weapon button
// swaps slots 12 and 24 in-place; leaving the old metadata attached to either
// item makes later appearance projections see two slot-12 weapons after one of
// them is replaced from the inventory.
func equipmentEntryMovedToSlot(entry dnfrepo.EquipmentEntry, slot int16) dnfrepo.EquipmentEntry {
	entry.SlotIndex = slot
	entry.Extra = cloneExtra(entry.Extra)
	slotText := strconv.Itoa(int(slot))
	entry.Extra["equipment_slot"] = slotText
	entry.Extra["equipped_slot"] = slotText
	entry.Extra["current_exe_equipment_type"] = slotText
	entry.Extra["current_exe_runtime_move"] = "1"
	return entry
}

func (o *Owner) loadMoveRecords(ctx context.Context, characterID string) (dnfrepo.InventoryRecord, dnfrepo.EquipmentRecord, error) {
	inventory, ok, err := o.inventory.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.InventoryRecord{}, dnfrepo.EquipmentRecord{}, err
	}
	if !ok {
		return dnfrepo.InventoryRecord{}, dnfrepo.EquipmentRecord{}, ErrInventoryNotFound
	}
	equipment, ok, err := o.equipment.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.InventoryRecord{}, dnfrepo.EquipmentRecord{}, err
	}
	if !ok {
		return dnfrepo.InventoryRecord{}, dnfrepo.EquipmentRecord{}, ErrEquipmentNotFound
	}
	return ensureInventoryRecord(dnfrepo.CloneInventory(inventory), characterID), ensureEquipmentRecord(dnfrepo.CloneEquipment(equipment), characterID), nil
}

func (o *Owner) saveMove(ctx context.Context, inventory dnfrepo.InventoryRecord, equipment dnfrepo.EquipmentRecord, fields []dnfrepo.InventoryField, result MoveResult) (MoveResult, error) {
	now := time.Now()
	inventory.UpdatedAt = now
	equipment.UpdatedAt = now
	if err := dnfrepo.SaveInventoryFields(ctx, o.inventory, inventory, fields...); err != nil {
		return MoveResult{}, err
	}
	if err := dnfrepo.SaveEquipmentFields(ctx, o.equipment, equipment, dnfrepo.EquipmentFieldEntries); err != nil {
		return MoveResult{}, err
	}
	result.Changed = true
	result.InventorySlots = cloneItemMap(inventory.Slots)
	return result, nil
}

func entryKey(slotIndex int16) string {
	return strconv.FormatInt(int64(slotIndex), 10)
}

const currentEquipmentSlotCount int16 = 33

func validateMoveStructure(cmd MoveCommand) (moveKind, error) {
	if cmd.SourceListType != wireListEquipment && cmd.DestinationListType != wireListEquipment {
		return moveKindOrdinary, ErrMoveUnsupported
	}
	if cmd.SourceSlotIndex < 0 || cmd.DestinationSlotIndex < 0 {
		return moveKindOrdinary, fmt.Errorf("%w: src=%d dst=%d", ErrMoveSlotOutOfRange, cmd.SourceSlotIndex, cmd.DestinationSlotIndex)
	}
	if cmd.SourceListType == wireListEquipment && cmd.SourceSlotIndex >= currentEquipmentSlotCount {
		return moveKindOrdinary, fmt.Errorf("%w: source=%d max=%d", ErrMoveSlotOutOfRange, cmd.SourceSlotIndex, currentEquipmentSlotCount-1)
	}
	if cmd.DestinationListType == wireListEquipment && cmd.DestinationSlotIndex >= currentEquipmentSlotCount {
		return moveKindOrdinary, fmt.Errorf("%w: destination=%d max=%d", ErrMoveSlotOutOfRange, cmd.DestinationSlotIndex, currentEquipmentSlotCount-1)
	}

	petMove := (cmd.SourceListType == wireListPet &&
		cmd.DestinationListType == wireListEquipment &&
		cmd.SourceSlotIndex <= 139 &&
		cmd.DestinationSlotIndex == petCreatureWornSlot) ||
		(cmd.SourceListType == wireListEquipment &&
			cmd.SourceSlotIndex == petCreatureWornSlot &&
			cmd.DestinationListType == wireListPet &&
			cmd.DestinationSlotIndex <= 139)
	artifactMove := (cmd.SourceListType == wireListPet &&
		cmd.DestinationListType == wireListEquipment &&
		cmd.SourceSlotIndex >= petArtifactSourceSlotMin && cmd.SourceSlotIndex <= petArtifactSourceSlotMax &&
		isPetArtifactTarget(cmd.DestinationSlotIndex)) ||
		(cmd.SourceListType == wireListEquipment &&
			isPetArtifactTarget(cmd.SourceSlotIndex) &&
			cmd.DestinationListType == wireListPet &&
			cmd.DestinationSlotIndex >= petArtifactSourceSlotMin && cmd.DestinationSlotIndex <= petArtifactSourceSlotMax)
	if cmd.SourceListType == wireListPet || cmd.DestinationListType == wireListPet {
		if petMove {
			return moveKindPet, nil
		}
		if artifactMove {
			return moveKindArtifact, nil
		}
		return moveKindOrdinary, fmt.Errorf("%w: pet move src=(%d,%d) dst=(%d,%d)", ErrMoveUnsupported, cmd.SourceListType, cmd.SourceSlotIndex, cmd.DestinationListType, cmd.DestinationSlotIndex)
	}
	if !isOrdinaryMoveList(cmd.SourceListType) || !isOrdinaryMoveList(cmd.DestinationListType) {
		return moveKindOrdinary, fmt.Errorf("%w: src=%d dst=%d", ErrMoveUnsupported, cmd.SourceListType, cmd.DestinationListType)
	}
	return moveKindOrdinary, nil
}

// isDualWeaponToggleMove recognizes the Vagabond "change sub weapon" op19:
// worn weapon slots 12 <-> 24 with count=0. Nothing else may bypass the
// count=1 rule.
func isDualWeaponToggleMove(cmd MoveCommand) bool {
	if cmd.MoveCount != 0 || cmd.SourceListType != wireListEquipment || cmd.DestinationListType != wireListEquipment {
		return false
	}
	return (cmd.SourceSlotIndex == 12 && cmd.DestinationSlotIndex == 24) ||
		(cmd.SourceSlotIndex == 24 && cmd.DestinationSlotIndex == 12)
}

type moveKind int

const (
	moveKindOrdinary moveKind = iota
	moveKindPet
	moveKindArtifact
)

// Current NoPack closes the artifact family as runtime targets 27/28/29
// (sub_35FC900 -> equip_artifactred/blue/green) and the shared list-7 source
// range 140..188 (sub_30A4950's unequip search); it is not one slot per color.
const (
	petArtifactSourceSlotMin int16 = 140
	petArtifactSourceSlotMax int16 = 188
)

func isPetArtifactTarget(slot int16) bool {
	return slot >= 27 && slot <= 29
}

func isOrdinaryMoveList(listType byte) bool {
	switch listType {
	case wireListMain, wireListAvatar, wireListPersonalCargo, wireListEquipment, wireListGuildMedal:
		return true
	default:
		return false
	}
}

func (o *Owner) validatePlacement(ctx context.Context, characterID string, itemID int64, sourceListType byte, sourceSlotIndex int16, targetSlotIndex int16) error {
	// Current NoPack op19 closes the creature body path as list-7 slots
	// 0..139 <-> actor worn target 26.
	if sourceListType == wireListPet && sourceSlotIndex >= 0 && sourceSlotIndex <= 139 && targetSlotIndex == petCreatureWornSlot {
		return nil
	}
	if o == nil || o.placementValidator == nil {
		return ErrMoveValidatorRequired
	}
	return o.placementValidator.ValidateEquipmentPlacement(ctx, Placement{
		CharacterID:     characterID,
		ItemID:          itemID,
		SourceListType:  sourceListType,
		SourceSlotIndex: sourceSlotIndex,
		TargetSlotIndex: targetSlotIndex,
	})
}

const (
	wireListMain          byte  = 0
	wireListAvatar        byte  = 1
	wireListPersonalCargo byte  = 2
	wireListEquipment     byte  = 3
	wireListPet           byte  = 7
	wireListGuildMedal    byte  = 38
	petCreatureWornSlot   int16 = 26
)

func ensureInventoryRecord(record dnfrepo.InventoryRecord, characterID string) dnfrepo.InventoryRecord {
	record.CharacterID = characterID
	if record.Slots == nil {
		record.Slots = make(map[string]dnfrepo.ItemStack)
	}
	if record.Warehouse == nil {
		record.Warehouse = make(map[string]dnfrepo.ItemStack)
	}
	return record
}

func ensureEquipmentRecord(record dnfrepo.EquipmentRecord, characterID string) dnfrepo.EquipmentRecord {
	record.CharacterID = characterID
	if record.Entries == nil {
		record.Entries = make(map[string]dnfrepo.EquipmentEntry)
	}
	return record
}

func inventoryMap(record *dnfrepo.InventoryRecord, listType byte) (map[string]dnfrepo.ItemStack, dnfrepo.InventoryField, bool) {
	switch listType {
	case wireListMain, wireListAvatar, wireListPet, wireListGuildMedal, dnftitlebook.ListType:
		// 称号簿（list 100）的条目和背包/装扮共用 InventoryRecord.Slots，
		// 只是 key 前缀不同（"0:<slot>" vs "100:<slot>"，见 titlebook.BookSlotKey），
		// 所以它同样落在 InventoryFieldSlots 上。
		return record.Slots, dnfrepo.InventoryFieldSlots, true
	case wireListPersonalCargo:
		return record.Warehouse, dnfrepo.InventoryFieldWarehouse, true
	default:
		return nil, "", false
	}
}

// titleBookOriginDestination 判断一件装备行是不是「从称号簿穿上来」的成就称号，
// 并给出它该回到的称号簿槽位。
//
// 官方语义（客户端字符串 48178 / 0xBC32）：「通过成就任务获得的称号，无法移动到
// 物品栏或金库。」也就是说成就称号是簿内资产，只允许在称号簿 ↔ 装备栏之间移动。
// titlebook.Owner.Equip 在把称号穿上时打了 source=title_book_equip +
// title_book_category/title_book_index，这里就是它的消费点。
//
// 背景（2026-09-28 玩家报障）：从簿里「右键使用」穿上成就称号后，再穿回原来的
// 称号，成就称号会掉进背包。原因是 equip/unequip 的写回路径从不看这些标记，
// 一律写到客户端声明的那个背包格。
func titleBookOriginDestination(entry dnfrepo.EquipmentEntry) (byte, int16, bool) {
	// 2026-09-28 线上回退：本函数一生效就会改写 op19/op413 的落点，而线上实测出现
	// 「称号簿右键报错 / 成就称号仍摘不下 / 成就条件不完成」三个回归。短路后行为与改动前
	// 完全一致；诊断时把这三行删掉即可逐条打开。
	return 0, 0, false
	if entry.Extra == nil {
		return 0, 0, false
	}
	if !strings.EqualFold(strings.TrimSpace(entry.Extra["source"]), "title_book_equip") &&
		!strings.EqualFold(strings.TrimSpace(entry.Extra["source"]), "title_book_get") {
		return 0, 0, false
	}
	category, categoryErr := strconv.ParseInt(strings.TrimSpace(entry.Extra["title_book_category"]), 10, 32)
	index, indexErr := strconv.ParseInt(strings.TrimSpace(entry.Extra["title_book_index"]), 10, 32)
	// 上界与 currentTitleBookStackNeedsRawEntry 保持同一口径：越界就不改写落点，
	// 退回改动前的行为，绝不猜一个可能不存在的分类。
	if categoryErr != nil || indexErr != nil ||
		category < 0 || category >= 5 ||
		index < 0 || index >= int64(dnftitlebook.MaxPerCategory) {
		return 0, 0, false
	}
	return dnftitlebook.ListType,
		int16(category)*dnftitlebook.SlotBase + int16(index), true
}

// titleBookReturnSlot 给出称号簿里的真实可用落点：优先回原来那一格，被占则在该
// category 内找第一个空格。找不到就失败——成就称号宁可操作不成功，也不能掉进背包
// （客户端本来就会拒绝这种移动）。
func titleBookReturnSlot(items map[string]dnfrepo.ItemStack, listType byte, slot int16) (int16, bool) {
	if _, occupied := items[inventoryKey(listType, slot)]; !occupied {
		return slot, true
	}
	base := (slot / dnftitlebook.SlotBase) * dnftitlebook.SlotBase
	for index := int16(0); index < dnftitlebook.MaxPerCategory; index++ {
		candidate := base + index
		if _, occupied := items[inventoryKey(listType, candidate)]; !occupied {
			return candidate, true
		}
	}
	return 0, false
}

func inventoryKey(listType byte, slotIndex int16) string {
	return fmt.Sprintf("%d:%d", listType, slotIndex)
}

func entryFromStack(stack dnfrepo.ItemStack, sourceListType byte, equipSlot int16, sourceInstanceValue int32) (dnfrepo.EquipmentEntry, error) {
	if sourceListType == wireListPet && equipSlot == petCreatureWornSlot {
		return petCreatureEntryFromStack(stack, equipSlot)
	}
	raw := append([]byte(nil), stack.RawEntry...)
	if len(raw) == 0 {
		if currentTitleBookStackNeedsRawEntry(stack, sourceListType, equipSlot) {
			raw = buildCurrentTitleEquipmentRawEntry(stack.ItemID, equipSlot)
		} else if rebuilt, ok := buildCurrentEquipmentRawEntryFromMoveEvidence(stack, sourceListType, equipSlot, sourceInstanceValue); ok {
			raw = rebuilt
		} else {
			var err error
			raw, err = rawEntryFromExtra(stack.Extra)
			if err != nil {
				return dnfrepo.EquipmentEntry{}, err
			}
		}
	}
	raw = overlayEquipmentTailDataFromExtra(raw, stack.Extra)
	extra := cloneExtra(stack.Extra)
	// A permanent avatar is a real, occupied list-1 item whose current-EXE
	// amount field is zero.  Count==0 therefore cannot mean "empty slot" for
	// this container. Preserve the repository value explicitly while the item
	// is worn, otherwise an unequip would silently turn it into Count==1.
	//
	// This marker is internal persistence state only; the outward 0x77 amount
	// remains reconstructed from amount_or_count / the real ItemStack when a
	// list snapshot is emitted.
	extra["current_exe_inventory_count"] = strconv.FormatInt(stack.Count, 10)
	extra["equipped_slot"] = strconv.Itoa(int(equipSlot))
	// Persist the current-EXE actor slot chosen by the validated op19 move.
	// Initial PVF records use legacy worn-slot numbering and deliberately do
	// not carry this marker; once such an item is moved by the runtime, the
	// explicit slot must win on later op13/mode1 rebuilds.
	extra["current_exe_equipment_type"] = strconv.Itoa(int(equipSlot))
	extra["current_exe_runtime_move"] = "1"
	return dnfrepo.EquipmentEntry{
		SlotIndex: equipSlot,
		ItemID:    stack.ItemID,
		Bind:      stack.Bind,
		ExpireAt:  stack.ExpireAt,
		RawEntry:  raw,
		Extra:     extra,
	}, nil
}

const currentEquipmentRawEntryWireSize = 0x77

// buildCurrentEquipmentRawEntryFromMoveEvidence repairs only the historical
// relational rows that already have enough current-client/PVF evidence to be
// rendered as equipment but lost their opaque 0x77 row during import.  The
// live op19 source instance is required; no identity is invented when the
// client did not prove one.
func buildCurrentEquipmentRawEntryFromMoveEvidence(
	stack dnfrepo.ItemStack,
	sourceListType byte,
	equipSlot int16,
	sourceInstanceValue int32,
) ([]byte, bool) {
	if (sourceListType != wireListMain && sourceListType != wireListAvatar) ||
		stack.ItemID <= 0 || stack.ItemID > int64(^uint32(0)) || stack.Count != 1 ||
		sourceInstanceValue == 0 || stack.Extra == nil ||
		!strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "equipment") ||
		!strings.HasPrefix(strings.ToLower(strings.TrimSpace(stack.Extra["pvf_path"])), "equipment/") {
		return nil, false
	}
	qualitySeed := uint32(firstExtraInt(stack.Extra, 0, "quality_seed"))
	durability := firstExtraInt(stack.Extra, -1, "durability", "current_durability")
	if durability < 0 && currentZeroDurabilityEquipmentMoveEvidence(stack, sourceInstanceValue) {
		// Accessory / title PVFs can have no [durability] entry, so the granting
		// path never writes the "durability" extra. A server-minted equipment
		// instance still carries the PVF type/path, the quality seed and the live
		// client item-id echo, which is enough to restore its zero word safely.
		durability = 0
	}
	if !itemquality.Valid(qualitySeed) || durability < 0 || durability > int64(^uint16(0)) {
		return nil, false
	}
	raw := make([]byte, currentEquipmentRawEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], uint16(equipSlot))
	binary.LittleEndian.PutUint32(raw[2:6], uint32(stack.ItemID))
	binary.LittleEndian.PutUint32(raw[6:10], qualitySeed)
	raw[0x0A] = byte(firstExtraInt(stack.Extra, 0, "packed_flag_byte", "packed_flag"))
	binary.LittleEndian.PutUint16(raw[0x0B:0x0D], uint16(durability))
	raw[0x0D] = byte(firstExtraInt(stack.Extra, 0, "seal_flag", "seal"))
	binary.LittleEndian.PutUint32(raw[0x0E:0x12], uint32(sourceInstanceValue))
	return raw, true
}

// 2026-10-16：把 [upgrade limit cube]（称号随机变更券）产出的装备也纳进来。
// 变更券产出的称号（source=limited_cube_result）走的是同一套 PVF 分类投影，
// 而 title 类 PVF 没有 [durability] 条目 → 投影不写 durability extra →
// 这里原来只认 dungeon_card_reward，于是"变更券变出来的称号右键穿不上"：
// 服务端回 op19 错误码 17，客户端弹「物品栏已满，或该道具超过可持有上限」。
func currentZeroDurabilityEquipmentMoveEvidence(stack dnfrepo.ItemStack, sourceInstanceValue int32) bool {
	if stack.Extra == nil || sourceInstanceValue <= 0 || int64(sourceInstanceValue) != stack.ItemID {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Extra["source"])) {
	case "dungeon_card_reward", "limited_cube_result":
	default:
		return false
	}
	return strings.TrimSpace(stack.Extra["equipment_type"]) != ""
}

func currentTitleBookStackNeedsRawEntry(stack dnfrepo.ItemStack, sourceListType byte, equipSlot int16) bool {
	if sourceListType != wireListMain ||
		equipSlot != 13 ||
		stack.ItemID <= 0 ||
		stack.ItemID > int64(^uint32(0)) ||
		!strings.EqualFold(strings.TrimSpace(stack.Extra["source"]), "title_book_get") {
		return false
	}
	category, categoryErr := strconv.ParseInt(strings.TrimSpace(stack.Extra["title_book_category"]), 10, 32)
	index, indexErr := strconv.ParseInt(strings.TrimSpace(stack.Extra["title_book_index"]), 10, 32)
	return categoryErr == nil && category >= 0 && category < 5 &&
		indexErr == nil && index >= 0 && index < 200
}

func buildCurrentTitleEquipmentRawEntry(itemID int64, equipSlot int16) []byte {
	// Current selected-equipment records use the same 43-byte constructor
	// summary as initial equipment. A title has no durability; byte 0 is the
	// validated actor slot, +1 is the PVF item ID, and +5 is the constructor's
	// required create scalar 1. The remaining fields are zero.
	// 实现在 titlebook 包里，equip 与 titlebook 共用同一份，避免两份构造规则漂移。
	return dnftitlebook.EquipmentRawEntry(itemID, equipSlot)
}

func petCreatureEntryFromStack(stack dnfrepo.ItemStack, equipSlot int16) (dnfrepo.EquipmentEntry, error) {
	serial := firstExtraInt(stack.Extra, 0, "creature_serial_or_handle", "creature_serial", "pet_serial", "serial", "handle", "instance_value", "item_uid")
	if serial <= 0 {
		return dnfrepo.EquipmentEntry{}, ErrMoveRawEntryMissing
	}
	enchantCardID, enchantUpgradeCount, err := petEnchantFieldsFromStack(stack)
	if err != nil {
		return dnfrepo.EquipmentEntry{}, err
	}
	raw := buildPetCreatureEquipEntry(equipSlot, stack.ItemID, serial)
	binary.LittleEndian.PutUint32(raw[0x10:0x14], enchantCardID)
	raw[0x14] = enchantUpgradeCount
	extra := cloneExtra(stack.Extra)
	extra["equipped_slot"] = strconv.Itoa(int(equipSlot))
	extra["pet_enchant_card_item_id"] = strconv.FormatUint(uint64(enchantCardID), 10)
	extra["enchant_card_id"] = strconv.FormatUint(uint64(enchantCardID), 10)
	extra["enchant_upgrade_count"] = strconv.FormatUint(uint64(enchantUpgradeCount), 10)
	extra["value_a"] = strconv.FormatUint(uint64(enchantCardID), 10)
	extra["byte_12"] = strconv.FormatUint(uint64(enchantUpgradeCount), 10)
	extra["raw_entry_hex"] = hex.EncodeToString(raw)
	return dnfrepo.EquipmentEntry{
		SlotIndex: equipSlot,
		ItemID:    stack.ItemID,
		Bind:      stack.Bind,
		ExpireAt:  stack.ExpireAt,
		RawEntry:  raw,
		Extra:     extra,
	}, nil
}

func petEnchantFieldsFromStack(stack dnfrepo.ItemStack) (uint32, byte, error) {
	cardID := firstPetEnchantExtraInt(stack.Extra, -1, "pet_enchant_card_item_id", "enchant_card_id", "value_a")
	if cardID < 0 && len(stack.RawEntry) >= 0x13 {
		cardID = int64(binary.LittleEndian.Uint32(stack.RawEntry[0x0E:0x12]))
	}
	if cardID < 0 {
		cardID = 0
	}
	if cardID > int64(^uint32(0)) {
		return 0, 0, ErrMoveRawEntryInvalid
	}
	upgradeCount := firstExtraInt(stack.Extra, -1, "enchant_upgrade_count", "pet_enchant_upgrade_count", "byte_12")
	if upgradeCount < 0 && len(stack.RawEntry) >= 0x13 {
		upgradeCount = int64(stack.RawEntry[0x12])
	}
	if upgradeCount < 0 {
		upgradeCount = 0
	}
	if upgradeCount > int64(^byte(0)) {
		return 0, 0, ErrMoveRawEntryInvalid
	}
	return uint32(cardID), byte(upgradeCount), nil
}

func firstPetEnchantExtraInt(extra map[string]string, fallback int64, keys ...string) int64 {
	first := fallback
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 0, 64)
		if err != nil {
			continue
		}
		if first == fallback {
			first = value
		}
		if value > 0 {
			return value
		}
	}
	return first
}

func stackFromEntry(entry dnfrepo.EquipmentEntry) dnfrepo.ItemStack {
	extra := cloneExtra(entry.Extra)
	raw := overlayEquipmentTailDataFromExtra(append([]byte(nil), entry.RawEntry...), entry.Extra)
	if len(raw) > 0 {
		extra["raw_entry_hex"] = hex.EncodeToString(raw)
	}
	extra["equipment_slot"] = strconv.Itoa(int(entry.SlotIndex))
	if entry.SlotIndex == petCreatureWornSlot {
		if serial := petSerialFromEquippedRaw(raw); serial > 0 {
			extra["creature_serial_or_handle"] = strconv.FormatInt(serial, 10)
		}
	}
	return dnfrepo.ItemStack{
		ItemID:   entry.ItemID,
		Count:    equipmentEntryInventoryCount(entry),
		Bind:     entry.Bind,
		ExpireAt: entry.ExpireAt,
		RawEntry: raw,
		Extra:    extra,
	}
}

const (
	equipmentSocketCollectionOffset = 0x27
	equipmentSocketCollectionBytes  = 17
	equipmentLegacyTailDataOffset   = 0x2F
	equipmentTailDataBytes          = 37
)

var equipmentTailDataExtraKeys = []string{"tail_data_2f", "tailData2F", "tail2f", "raw_data_2f"}

func overlayEquipmentTailDataFromExtra(raw []byte, extra map[string]string) []byte {
	// Current NoPack parses raw+0x27..+0x37 as a live fixed-row vector.
	// Earlier code overlaid a legacy C# tail representation on that vector
	// during ordinary equip moves.  The current EXE has not proved that either
	// structure represents a normal-equipment socket, so preserve repository
	// raw bytes exactly until the real mutation is closed.
	_ = extra
	return raw
}

func equipmentSocketPrimaryValue(value []byte, visible bool) uint32 {
	if !visible {
		return 0
	}
	if len(value) >= 4 {
		if raw := binary.LittleEndian.Uint32(value[:4]); raw != 0 {
			return raw
		}
	}
	return ^uint32(0)
}

func equipmentTailDataFromExtra(extra map[string]string) []byte {
	if len(extra) == 0 {
		return nil
	}
	var fallback []byte
	for _, key := range equipmentTailDataExtraKeys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) == 0 {
			continue
		}
		out := make([]byte, equipmentTailDataBytes)
		copy(out, decoded)
		if equipmentTailHasSocketData(out) {
			return out
		}
		if fallback == nil {
			fallback = out
		}
	}
	return fallback
}

func equipmentTailHasSocketData(tail []byte) bool {
	if len(tail) == 0 {
		return false
	}
	if tail[0] != 0 {
		return true
	}
	if len(tail) >= 9 {
		return binary.LittleEndian.Uint32(tail[1:5]) != 0 || binary.LittleEndian.Uint32(tail[5:9]) != 0
	}
	for _, value := range tail {
		if value != 0 {
			return true
		}
	}
	return false
}

// isEquipmentMoveUnit accepts exactly one ordinary equipment item. The
// current client represents a permanent avatar in list 1 with amount/count
// zero, but it is still a single wearable object (its non-empty item id and
// later PVF placement check are authoritative). Do not broaden this exception
// to normal inventory lists, where zero remains invalid for an equip move.
func isEquipmentMoveUnit(stack dnfrepo.ItemStack, listType byte) bool {
	if stack.Count == 1 {
		return true
	}
	return listType == wireListAvatar && stack.Count == 0
}

// equipmentEntryInventoryCount restores the exact container count recorded at
// equip time. Historical worn rows predate that marker and are normal
// equipment, so they retain the proven single-item fallback of one.
func equipmentEntryInventoryCount(entry dnfrepo.EquipmentEntry) int64 {
	if raw := strings.TrimSpace(entry.Extra["current_exe_inventory_count"]); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value >= 0 {
			return value
		}
	}
	return 1
}

func rawEntryFromExtra(extra map[string]string) ([]byte, error) {
	if extra == nil {
		return nil, ErrMoveRawEntryMissing
	}
	raw := strings.TrimSpace(extra["raw_entry_hex"])
	if raw == "" {
		raw = strings.TrimSpace(extra["equipment_raw_entry_hex"])
	}
	if raw == "" {
		return nil, ErrMoveRawEntryMissing
	}
	raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("%w: odd hex length", ErrMoveRawEntryInvalid)
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) == 0 {
		return nil, fmt.Errorf("%w: %v", ErrMoveRawEntryInvalid, err)
	}
	return decoded, nil
}

func buildPetCreatureEquipEntry(slot int16, itemID int64, serial int64) []byte {
	var buf bytes.Buffer
	writeByte := func(value byte) {
		_ = buf.WriteByte(value)
	}
	writeUint16 := func(value uint16) {
		var tmp [2]byte
		binary.LittleEndian.PutUint16(tmp[:], value)
		buf.Write(tmp[:])
	}
	writeUint32 := func(value uint32) {
		var tmp [4]byte
		binary.LittleEndian.PutUint32(tmp[:], value)
		buf.Write(tmp[:])
	}

	writeByte(byte(slot))
	writeUint32(uint32(clampInt32(itemID)))
	writeUint32(uint32(clampInt32(serial)))
	writeByte(0)
	writeUint16(0)
	writeUint32(0)
	writeUint32(0)
	writeByte(0)
	writeByte(0)
	writeUint16(0)
	writeUint32(uint32(clampInt32(serial)))
	writeByte(0)
	writeUint32(0)
	writeByte(0)
	writeUint16(0)
	writeByte(0)
	buf.Write(make([]byte, 10))
	return buf.Bytes()
}

func petSerialFromEquippedRaw(raw []byte) int64 {
	// The current pet constructor repeats the creature serial/handle at +24.
	// Do not accept the ordinary equipment instance scalar at +5. The current
	// creature row carries its authoritative serial at +24, while ordinary
	// equipment instance values are not creature identities.
	// identity. buildPetCreatureEquipEntry always writes the authoritative +24
	// field, so a shorter historical/ordinary row must fail closed here.
	if len(raw) >= 28 {
		value := int64(binary.LittleEndian.Uint32(raw[24:28]))
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstExtraInt(extra map[string]string, fallback int64, keys ...string) int64 {
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 0, 64)
		if err == nil {
			return value
		}
	}
	return fallback
}

func clampInt32(value int64) int32 {
	if value > int64(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	if value < -int64(^uint32(0)>>1)-1 {
		return -int32(^uint32(0)>>1) - 1
	}
	return int32(value)
}

func cloneExtra(extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return make(map[string]string)
	}
	out := make(map[string]string, len(extra)+2)
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func cloneItemMap(in map[string]dnfrepo.ItemStack) map[string]dnfrepo.ItemStack {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]dnfrepo.ItemStack, len(in))
	for key, value := range in {
		value.Extra = cloneExtra(value.Extra)
		out[key] = value
	}
	return out
}
