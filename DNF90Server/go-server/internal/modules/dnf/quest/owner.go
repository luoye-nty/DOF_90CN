package quest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrOwnerUnavailable            = errors.New("quest owner unavailable")
	ErrCharacterRequired           = errors.New("selected character id required")
	ErrQuestIDRequired             = errors.New("quest id required")
	ErrCharacterNotFound           = errors.New("character record not found")
	ErrQuestPersistVerify          = errors.New("persisted quest state verification failed")
	ErrQuestNotActive              = errors.New("quest is not active")
	ErrQuestAnswerInvalid          = errors.New("question quest answer is invalid")
	ErrQuestCannotGiveUp           = errors.New("quest cannot be given up")
	ErrAcceptSettlementUnavailable = errors.New("quest accept settlement is unavailable")
	ErrAcceptInventoryUnavailable  = errors.New("quest accept inventory is unavailable")
	ErrAcceptItemAllocatorRequired = errors.New("quest accept item allocator is required")
)

type Owner struct {
	repositories     dnfrepo.Group
	characters       dnfrepo.CharacterRepository
	quests           dnfrepo.QuestRepository
	inventory        dnfrepo.InventoryRepository
	accountInventory dnfrepo.AccountInventoryRepository
}

type PlanResult struct {
	AccountID   string
	CharacterID string
	Operation   string
	QuestID     int64
	Known       bool
	// StateChanged is true only after ApplySetTrigger has durably updated the
	// active quest state. A valid replay at the already-persisted state is not
	// a new client-success boundary.
	StateChanged      bool
	Status            string
	TriggerType       byte
	QuestionAnswer    byte
	HasQuestionAnswer bool
	ProgressValue     int64
	RewardSelectIndex int64
	HasRewardSelect   bool
	Multiplier        int64
}

type AcceptResult struct {
	AccountID   string
	CharacterID string
	QuestID     uint16
	InitTrigger uint32
	Idempotent  bool
	PVFPath     string
	QuestType   string
	EventItems  []FinishCommittedItem
}

type GiveUpResult struct {
	AccountID   string
	CharacterID string
	QuestID     uint16
	// Reclaimed 是本次真实扣减的任务专属道具。普通材料（`[material]` 等）
	// 永远不会出现在这里：它们只在任务交付时被扣。
	Reclaimed []GiveUpReclaimedItem
	// ReclaimSkipped 表示有计划要回收、但没有执行（事务单元或背包不可用）。
	// 放弃本身仍然成功；调用方应当把 SkipReason 记进日志。
	ReclaimSkipped    bool
	ReclaimSkipReason string
	// PostCommitInventory 只在真的扣了道具时给出，供桥接层重建客户端行。
	PostCommitInventory    dnfrepo.InventoryRecord
	HasPostCommitInventory bool
}

// GiveUpInput 是"放弃任务"的提交输入。
type GiveUpInput struct {
	AccountID           string
	SelectedCharacterID uint16
	QuestID             uint16
	// IsQuestItem 判定某道具是否属于 PVF `[quest]` 家族（任务专属、落在任务栏
	// 177..232 且不可交易）。nil 表示"没有任何任务专属道具"：放弃照常成功，
	// 只是不回收任何东西。
	IsQuestItem func(int64) bool
	// CommittedAt 非零时作为任务记录的更新时间，便于测试与重放对齐。
	CommittedAt time.Time
}

func NewOwner(repos dnfrepo.Group) (*Owner, error) {
	if repos.Character == nil || repos.Quest == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{
		repositories: repos, characters: repos.Character, quests: repos.Quest,
		inventory: repos.Inventory, accountInventory: repos.AccountInventory,
	}, nil
}

func (o *Owner) Plan(ctx context.Context, cmd Command) (PlanResult, error) {
	result, _, _, err := o.planWithQuestRecord(ctx, cmd)
	return result, err
}

func (o *Owner) planWithQuestRecord(ctx context.Context, cmd Command) (PlanResult, dnfrepo.QuestRecord, bool, error) {
	if o == nil || o.characters == nil || o.quests == nil {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, ErrCharacterRequired
	}
	if cmd.QuestID == 0 {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, ErrQuestIDRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, err
	}
	if !ok {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, ErrCharacterNotFound
	}
	character = dnfrepo.CloneCharacter(character)
	record, ok, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return PlanResult{}, dnfrepo.QuestRecord{}, false, err
	}
	state, known := questStateFor(record, int64(cmd.QuestID))
	result := PlanResult{
		AccountID:         character.AccountID,
		CharacterID:       characterID,
		Operation:         cmd.Operation,
		QuestID:           int64(cmd.QuestID),
		Known:             ok && known,
		Status:            state.Status,
		TriggerType:       state.TriggerType,
		ProgressValue:     state.ProgressValue,
		RewardSelectIndex: int64(cmd.RewardSelectIndex),
		HasRewardSelect:   cmd.HasRewardSelect,
		Multiplier:        int64(cmd.Multiplier),
	}
	if result.Multiplier == 0 {
		result.Multiplier = 1
	}
	if result.Status == "" {
		result.Status = "unknown"
	}
	if cmd.Operation == "set_quest_trigger" {
		result.TriggerType = cmd.TriggerType
	}
	return result, record, ok, nil
}

// ApplyAccept preserves the no-item caller API. Event-item quests require
// ApplyAcceptWithItems so the caller supplies the current-PVF slot allocator.
func (o *Owner) ApplyAccept(ctx context.Context, catalog *Catalog, eligibility CharacterEligibility, cmd Command) (AcceptResult, error) {
	return o.applyAccept(ctx, catalog, eligibility, cmd, nil)
}

// ApplyAcceptWithItems atomically commits an accepted quest and every PVF
// [depend give item] grant. The allocator mutates only the transaction-scoped
// inventory clone; the owner controls both repository writes and rollback.
func (o *Owner) ApplyAcceptWithItems(
	ctx context.Context,
	catalog *Catalog,
	eligibility CharacterEligibility,
	cmd Command,
	allocateItem FinishItemAllocator,
) (AcceptResult, error) {
	return o.applyAccept(ctx, catalog, eligibility, cmd, allocateItem)
}

func (o *Owner) applyAccept(
	ctx context.Context,
	catalog *Catalog,
	eligibility CharacterEligibility,
	cmd Command,
	allocateItem FinishItemAllocator,
) (AcceptResult, error) {
	if o == nil || o.characters == nil || o.quests == nil {
		return AcceptResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return AcceptResult{}, ErrCharacterRequired
	}
	if cmd.QuestID == 0 {
		return AcceptResult{}, ErrQuestIDRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return AcceptResult{}, err
	}
	if !ok {
		return AcceptResult{}, ErrCharacterNotFound
	}
	record, hasRecord, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AcceptResult{}, err
	}
	if !hasRecord {
		record = dnfrepo.QuestRecord{CharacterID: characterID}
	} else {
		record = dnfrepo.CloneQuest(record)
	}
	definition, definitionKnown := catalog.Find(int64(cmd.QuestID))
	if !definitionKnown {
		return AcceptResult{}, ErrQuestDefinitionMissing
	}
	eligibility.Wedding = WeddingEligible(character)
	if normalizeQuestTag(definition.Grade) == "wedding" && !eligibility.Wedding {
		return AcceptResult{}, ErrQuestNotAcceptable
	}
	if definition.HasDependGiveItem {
		return o.applyAcceptEventItemSettlement(ctx, catalog, eligibility, cmd, allocateItem)
	}
	if existing, known := questStateFor(record, int64(cmd.QuestID)); known && isActiveQuestStatus(existing.Status) {
		if existing.ProgressValue < 0 || existing.ProgressValue > int64(^uint32(0)) {
			return AcceptResult{}, ErrQuestPersistVerify
		}
		return AcceptResult{
			AccountID:   character.AccountID,
			CharacterID: characterID,
			QuestID:     cmd.QuestID,
			InitTrigger: uint32(existing.ProgressValue),
			Idempotent:  true,
			PVFPath:     definition.Path,
			QuestType:   definition.Type,
		}, nil
	}
	plan, err := catalog.PlanAccept(eligibility, record, int64(cmd.QuestID))
	if err != nil {
		return AcceptResult{}, err
	}
	applyAcceptPlan(&record, plan, definition, characterID, time.Now())
	// SaveFields is one atomic SQL upsert for the complete states JSON column.
	// Event-item quests use applyAcceptEventItemSettlement above because their
	// inventory and quest rows require the wider character transaction.
	if err := dnfrepo.SaveQuestFields(ctx, o.quests, record, dnfrepo.QuestFieldStates); err != nil {
		return AcceptResult{}, err
	}
	persisted, ok, err := o.quests.Load(ctx, characterID)
	if err != nil {
		return AcceptResult{}, err
	}
	state, known := questStateFor(persisted, int64(cmd.QuestID))
	if !ok || !known || !isActiveQuestStatus(state.Status) || state.ProgressValue != int64(plan.InitTrigger) {
		return AcceptResult{}, ErrQuestPersistVerify
	}
	for _, linked := range plan.LinkedSubQuests {
		state, known := questStateFor(persisted, linked.QuestID)
		if !known || !isActiveQuestStatus(state.Status) || state.ProgressValue != int64(linked.InitTrigger) {
			return AcceptResult{}, ErrQuestPersistVerify
		}
	}
	return AcceptResult{
		AccountID:   character.AccountID,
		CharacterID: characterID,
		QuestID:     cmd.QuestID,
		InitTrigger: plan.InitTrigger,
		PVFPath:     plan.Path,
		QuestType:   plan.Type,
	}, nil
}

func (o *Owner) applyAcceptEventItemSettlement(
	ctx context.Context,
	catalog *Catalog,
	eligibility CharacterEligibility,
	cmd Command,
	allocateItem FinishItemAllocator,
) (AcceptResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return AcceptResult{}, ErrAcceptSettlementUnavailable
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	var candidate AcceptResult
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil || tx.Inventory == nil {
			return ErrAcceptSettlementUnavailable
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID ||
			(strings.TrimSpace(cmd.AccountID) != "" && strings.TrimSpace(character.AccountID) != strings.TrimSpace(cmd.AccountID)) {
			return ErrCharacterNotFound
		}
		record, found, err := tx.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			record = dnfrepo.QuestRecord{CharacterID: characterID}
		} else {
			record = dnfrepo.CloneQuest(record)
		}
		definition, known := catalog.Find(int64(cmd.QuestID))
		if !known {
			return ErrQuestDefinitionMissing
		}
		eligibility.Wedding = WeddingEligible(character)
		if normalizeQuestTag(definition.Grade) == "wedding" && !eligibility.Wedding {
			return ErrQuestNotAcceptable
		}
		if existing, known := questStateFor(record, int64(cmd.QuestID)); known && isActiveQuestStatus(existing.Status) {
			if existing.ProgressValue < 0 || existing.ProgressValue > int64(^uint32(0)) {
				return ErrQuestPersistVerify
			}
			candidate = AcceptResult{
				AccountID: character.AccountID, CharacterID: characterID, QuestID: cmd.QuestID,
				InitTrigger: uint32(existing.ProgressValue), Idempotent: true,
				PVFPath: definition.Path, QuestType: definition.Type,
			}
			return nil
		}
		plan, err := catalog.PlanAccept(eligibility, record, int64(cmd.QuestID))
		if err != nil {
			return err
		}
		if len(plan.EventItems) == 0 {
			return fmt.Errorf("%w: quest=%d has depend give item without grants", ErrQuestRewardMalformed, cmd.QuestID)
		}
		if allocateItem == nil {
			return ErrAcceptItemAllocatorRequired
		}
		inventory, found, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID {
			return ErrAcceptInventoryUnavailable
		}
		inventory = dnfrepo.CloneInventory(inventory)
		now := time.Now().UTC()
		completionKey := fmt.Sprintf("quest-accept/%s/%d/%d", characterID, cmd.QuestID, now.UnixNano())
		items, err := allocateFinishRewardItems(&inventory, plan.EventItems, allocateItem, FinishItemGrantRequest{
			QuestID: int64(cmd.QuestID), CompletionKey: completionKey,
			Source: "quest_pvf_accept_event_item",
		})
		if err != nil {
			return err
		}
		applyAcceptPlan(&record, plan, definition, characterID, now)
		inventory.CharacterID = characterID
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		if err := dnfrepo.SaveQuestFields(ctx, tx.Quest, record, dnfrepo.QuestFieldStates); err != nil {
			return err
		}
		persistedQuest, questFound, err := tx.Quest.Load(ctx, characterID)
		if err != nil {
			return err
		}
		state, stateKnown := questStateFor(persistedQuest, int64(cmd.QuestID))
		if !questFound || !stateKnown || !isActiveQuestStatus(state.Status) || state.ProgressValue != int64(plan.InitTrigger) {
			return ErrQuestPersistVerify
		}
		for _, linked := range plan.LinkedSubQuests {
			linkedState, known := questStateFor(persistedQuest, linked.QuestID)
			if !known || !isActiveQuestStatus(linkedState.Status) || linkedState.ProgressValue != int64(linked.InitTrigger) {
				return ErrQuestPersistVerify
			}
		}
		persistedInventory, inventoryFound, err := tx.Inventory.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !inventoryFound {
			return ErrAcceptInventoryUnavailable
		}
		for _, item := range items {
			stack, exists := persistedInventory.Slots[item.SlotKey]
			if !exists || stack.ItemID != item.ItemID || stack.Count != item.PostCount {
				return ErrQuestPersistVerify
			}
		}
		candidate = AcceptResult{
			AccountID: character.AccountID, CharacterID: characterID, QuestID: cmd.QuestID,
			InitTrigger: plan.InitTrigger, PVFPath: plan.Path, QuestType: plan.Type,
			EventItems: cloneFinishCommittedItems(items),
		}
		return nil
	})
	if err != nil {
		return AcceptResult{}, err
	}
	return candidate, nil
}

func applyAcceptPlan(record *dnfrepo.QuestRecord, plan AcceptPlan, definition Definition, characterID string, now time.Time) {
	if record.States == nil {
		record.States = make(map[int64]dnfrepo.QuestState, 1+len(plan.LinkedSubQuests))
	}
	record.States[plan.QuestID] = dnfrepo.QuestState{
		Status: "active", ProgressValue: int64(plan.InitTrigger), UpdatedAt: now,
		Extra: acceptedQuestStateExtra(plan, definition),
	}
	for _, linked := range plan.LinkedSubQuests {
		record.States[linked.QuestID] = dnfrepo.QuestState{
			Status: "active", ProgressValue: int64(linked.InitTrigger), UpdatedAt: now,
			Extra: map[string]string{
				"pvf_path": linked.Path, "quest_type": linked.Type,
				"main_quest_id":                strconv.FormatInt(plan.QuestID, 10),
				"auto_activated_by_main_quest": "true",
			},
		}
	}
	record.CharacterID = characterID
	record.UpdatedAt = now
}

func cloneFinishCommittedItems(items []FinishCommittedItem) []FinishCommittedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]FinishCommittedItem, len(items))
	copy(out, items)
	for index := range out {
		out[index].RawEntry = append([]byte(nil), out[index].RawEntry...)
	}
	return out
}

func isActiveQuestStatus(status string) bool {
	switch normalizeStatus(status) {
	case "active", "accepted", "inprogress", "progress":
		return true
	default:
		return false
	}
}

// ApplyGiveUp 保持旧签名：等价于"没有任务专属道具"的放弃（GM 与既有调用点）。
func (o *Owner) ApplyGiveUp(ctx context.Context, catalog *Catalog, cmd Command) (GiveUpResult, error) {
	return o.ApplyGiveUpSettlement(ctx, catalog, GiveUpInput{
		AccountID:           cmd.AccountID,
		SelectedCharacterID: cmd.SelectedCharacterID,
		QuestID:             cmd.QuestID,
	})
}

// ApplyGiveUpSettlement 放弃一个进行中的任务。
//
// 语义对齐参考端 QuestGiveupApplicationService / QuestGiveupItemRecoveryPolicy：
//
//   - 只有 PVF 明写 `[cant giveup]` 才拒绝；收集类（`[seeking]` / `[get item]` /
//     `[seek n meet npc]`）**不再**被一刀切拒绝 —— 玩家随时可以放弃，
//     见 2026-09-22 角色 1111 的日常任务 8557（普通材料，回收费名单为空）。
//   - 任务专属道具的回收与"删除任务状态"在同一个 CharacterSettlement 事务里提交，
//     任一步失败整体回滚；普通材料（`[material]` 等）永不回收。
//   - 没有可用事务单元时退回旧路径：只删任务状态、不回收，并置 ReclaimSkipped。
//     放弃本身永远不会因为"回收不可用"而被拒绝。
func (o *Owner) ApplyGiveUpSettlement(ctx context.Context, catalog *Catalog, input GiveUpInput) (GiveUpResult, error) {
	if o == nil || o.characters == nil || o.quests == nil {
		return GiveUpResult{}, ErrOwnerUnavailable
	}
	if input.SelectedCharacterID == 0 {
		return GiveUpResult{}, ErrCharacterRequired
	}
	if input.QuestID == 0 {
		return GiveUpResult{}, ErrQuestIDRequired
	}
	if catalog == nil {
		return GiveUpResult{}, ErrQuestDefinitionMissing
	}
	if _, known := catalog.Find(int64(input.QuestID)); !known {
		return GiveUpResult{}, ErrQuestDefinitionMissing
	}
	characterID := strconv.FormatUint(uint64(input.SelectedCharacterID), 10)
	settlements := o.repositories.CharacterSettlement
	if settlements == nil {
		return o.commitGiveUp(ctx, o.repositories, catalog, characterID, input, false, "character_settlement_transaction_unavailable")
	}
	var result GiveUpResult
	err := settlements.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		committed, commitErr := o.commitGiveUp(ctx, tx, catalog, characterID, input, true, "")
		if commitErr != nil {
			return commitErr
		}
		result = committed
		return nil
	})
	if err != nil {
		if errors.Is(err, dnfrepo.ErrCharacterSettlementTransactionUnavailable) {
			// 事务单元存在但这次用不了：退回"只删任务状态"的旧路径，
			// 而不是把玩家的放弃请求变成静默失败。
			return o.commitGiveUp(ctx, o.repositories, catalog, characterID, input, false,
				"character_settlement_transaction_unavailable")
		}
		return GiveUpResult{}, err
	}
	return result, nil
}

// commitGiveUp 在给定仓储视图里完成一次放弃：删任务状态 +（可选）回收任务专属道具。
// 同一个函数同时服务事务路径与降级路径，避免两条实现漂移。
func (o *Owner) commitGiveUp(
	ctx context.Context,
	group dnfrepo.Group,
	catalog *Catalog,
	characterID string,
	input GiveUpInput,
	allowReclaim bool,
	reclaimSkipReason string,
) (GiveUpResult, error) {
	if group.Character == nil || group.Quest == nil {
		return GiveUpResult{}, ErrOwnerUnavailable
	}
	definition, known := catalog.Find(int64(input.QuestID))
	if !known {
		return GiveUpResult{}, ErrQuestDefinitionMissing
	}
	if definition.CantGiveUp {
		return GiveUpResult{}, ErrQuestCannotGiveUp
	}
	character, found, err := group.Character.Load(ctx, characterID)
	if err != nil {
		return GiveUpResult{}, err
	}
	if !found {
		return GiveUpResult{}, ErrCharacterNotFound
	}
	record, found, err := group.Quest.Load(ctx, characterID)
	if err != nil {
		return GiveUpResult{}, err
	}
	if !found {
		return GiveUpResult{}, ErrQuestNotActive
	}
	record = dnfrepo.CloneQuest(record)
	questID := int64(input.QuestID)
	state, known := questStateFor(record, questID)
	if !known || !isActiveQuestStatus(state.Status) {
		return GiveUpResult{}, ErrQuestNotActive
	}

	// 回收计划必须在删除任务状态之前算：它要读"其它进行中任务"的需求量。
	plan := PlanGiveUpReclaim(catalog, record, questID, input.IsQuestItem)

	changedFields := make([]dnfrepo.QuestField, 0, 2)
	if _, exists := record.States[questID]; exists {
		delete(record.States, questID)
		changedFields = append(changedFields, dnfrepo.QuestFieldStates)
	}
	if _, exists := record.Progress[questID]; exists {
		delete(record.Progress, questID)
		changedFields = append(changedFields, dnfrepo.QuestFieldProgress)
	}
	if len(changedFields) == 0 {
		return GiveUpResult{}, ErrQuestNotActive
	}

	result := GiveUpResult{
		AccountID:   character.AccountID,
		CharacterID: characterID,
		QuestID:     input.QuestID,
	}
	var inventory dnfrepo.InventoryRecord
	inventoryMutated := false
	switch {
	case len(plan) == 0:
		// 没有任务专属道具（例如 8557 这种只要普通材料的日常）：只删任务状态。
	case !allowReclaim, group.Inventory == nil:
		if reclaimSkipReason == "" {
			reclaimSkipReason = "inventory_repository_unavailable"
		}
		result.ReclaimSkipped = true
		result.ReclaimSkipReason = reclaimSkipReason
	default:
		loaded, inventoryFound, loadErr := group.Inventory.Load(ctx, characterID)
		if loadErr != nil {
			return GiveUpResult{}, loadErr
		}
		if !inventoryFound {
			result.ReclaimSkipped = true
			result.ReclaimSkipReason = "inventory_record_missing"
			break
		}
		inventory = dnfrepo.CloneInventory(loaded)
		// 事务内的 scoped 仓储会校验记录归属键（mysql/memory 都要求
		// record.CharacterID == 本次事务的角色），这里显式对齐，避免历史行
		// 该字段为空时把一次正常的放弃变成失败。
		inventory.CharacterID = characterID
		reclaimed := applyGiveUpReclaim(&inventory, plan)
		if len(reclaimed) == 0 {
			break
		}
		result.Reclaimed = reclaimed
		inventoryMutated = true
		result.PostCommitInventory = dnfrepo.CloneInventory(inventory)
		result.HasPostCommitInventory = true
	}

	record.CharacterID = characterID
	record.UpdatedAt = time.Now()
	if !input.CommittedAt.IsZero() {
		record.UpdatedAt = input.CommittedAt
	}
	if err := dnfrepo.SaveQuestFields(ctx, group.Quest, record, changedFields...); err != nil {
		return GiveUpResult{}, err
	}
	if inventoryMutated {
		if err := dnfrepo.SaveInventoryFields(ctx, group.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return GiveUpResult{}, err
		}
	}
	persisted, found, err := group.Quest.Load(ctx, characterID)
	if err != nil {
		return GiveUpResult{}, err
	}
	if !found {
		return GiveUpResult{}, ErrQuestPersistVerify
	}
	if persistedState, exists := questStateFor(persisted, questID); exists && isActiveQuestStatus(persistedState.Status) {
		return GiveUpResult{}, ErrQuestPersistVerify
	}
	return result, nil
}

// ApplySetTrigger applies trigger arithmetic when no PVF definition is
// available. Runtime handlers should use ApplySetTriggerWithCatalog so
// inventory-backed compound objectives can be resolved from authoritative PVF.
func (o *Owner) ApplySetTrigger(ctx context.Context, cmd Command) (PlanResult, error) {
	return o.applySetTrigger(ctx, nil, nil, cmd)
}

// ApplySetTriggerWithCatalog resolves PVF-backed compound objectives before
// falling back to the ordinary packed-channel arithmetic.
func (o *Owner) ApplySetTriggerWithCatalog(ctx context.Context, catalog *Catalog, cmd Command) (PlanResult, error) {
	var definition *Definition
	if catalog != nil {
		if candidate, ok := catalog.Find(int64(cmd.QuestID)); ok {
			definition = &candidate
		}
	}
	return o.applySetTrigger(ctx, catalog, definition, cmd)
}

func (o *Owner) applySetTrigger(ctx context.Context, catalog *Catalog, definition *Definition, cmd Command) (PlanResult, error) {
	result, record, ok, err := o.planWithQuestRecord(ctx, cmd)
	if err != nil {
		return PlanResult{}, err
	}
	if cmd.Operation != "set_quest_trigger" || !result.Known || !isActiveQuestStatus(result.Status) {
		return result, nil
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	if !ok {
		return result, nil
	}
	record = dnfrepo.CloneQuest(record)
	state, field, known := mutableQuestState(&record, int64(cmd.QuestID))
	if !known {
		return result, nil
	}

	now := time.Now()
	raidPhaseDayExtraKey := ""
	if definition != nil && normalizeQuestTag(definition.Type) == "raid phase clear" {
		channel, allowed := raidPhaseClearTriggerChannel(*definition, cmd.TriggerType, cmd.IsIncrement)
		if !allowed {
			return result, nil
		}
		raidPhaseDayExtraKey = fmt.Sprintf("raid_phase_clear_day_%d", channel)
		if state.Extra[raidPhaseDayExtraKey] == raidPhaseClearCalendarDay(now) {
			return result, nil
		}
	}

	oldProgress := state.ProgressValue
	questionAnswer, questionAnswerOK := validQuestionAnswer(catalog, definition, cmd.TriggerArgument)
	if definition != nil && normalizeQuestTag(definition.Type) == "question" && !questionAnswerOK {
		return PlanResult{}, fmt.Errorf("%w: quest=%d answer=%d", ErrQuestAnswerInvalid, definition.ID, cmd.TriggerArgument)
	}
	newProgress := int64(0)
	if !questionAnswerOK {
		newProgress, err = o.nextTriggerProgress(ctx, characterID, result.AccountID, state, definition, cmd)
	}
	if err != nil {
		return PlanResult{}, err
	}
	newProgress = clampQuestProgress(newProgress)
	oldAnswer := strings.TrimSpace(state.Extra[questAnswerExtraKey])
	wantAnswer := ""
	if questionAnswerOK {
		wantAnswer = strconv.Itoa(int(questionAnswer))
	}
	if newProgress == oldProgress && oldAnswer == wantAnswer {
		result.ProgressValue = newProgress
		return result, nil
	}

	state.TriggerType = cmd.TriggerType
	state.ProgressValue = newProgress
	state.UpdatedAt = now
	if state.Extra == nil {
		state.Extra = make(map[string]string, 2)
	}
	if raidPhaseDayExtraKey != "" {
		state.Extra[raidPhaseDayExtraKey] = raidPhaseClearCalendarDay(now)
	}
	state.Extra["last_set_trigger_type"] = strconv.Itoa(int(cmd.TriggerType))
	state.Extra["last_set_trigger_argument"] = strconv.Itoa(int(cmd.TriggerArgument))
	state.Extra["last_set_trigger_increment"] = strconv.FormatBool(cmd.IsIncrement)
	if questionAnswerOK {
		state.Extra[questAnswerExtraKey] = wantAnswer
		result.QuestionAnswer = questionAnswer
		result.HasQuestionAnswer = true
	}
	switch field {
	case dnfrepo.QuestFieldStates:
		record.States[int64(cmd.QuestID)] = state
	case dnfrepo.QuestFieldProgress:
		record.Progress[int64(cmd.QuestID)] = state
	}
	record.UpdatedAt = now
	var persistErr error
	if writer, supported := o.quests.(dnfrepo.QuestStateEntryWriter); supported {
		persistErr = writer.SaveQuestStateEntry(
			ctx, characterID, field, int64(cmd.QuestID), state, record.UpdatedAt,
		)
	} else {
		persistErr = dnfrepo.SaveQuestFields(ctx, o.quests, record, field)
	}
	if persistErr != nil {
		return PlanResult{}, persistErr
	}
	result.StateChanged = true
	result.TriggerType = state.TriggerType
	result.ProgressValue = state.ProgressValue
	return result, nil
}

func validQuestionAnswer(catalog *Catalog, definition *Definition, answer byte) (byte, bool) {
	if catalog == nil || definition == nil || normalizeQuestTag(definition.Type) != "question" {
		return 0, false
	}
	for _, candidate := range catalog.Successors(definition.ID) {
		for _, requirement := range candidate.PreRequiredAnswers {
			if requirement.QuestID == definition.ID && requirement.Answer == int64(answer) {
				return answer, true
			}
		}
	}
	return 0, false
}

func questStateFor(record dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, bool) {
	if state, ok := record.States[questID]; ok {
		return state, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, true
	}
	return dnfrepo.QuestState{}, false
}

func mutableQuestState(record *dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, dnfrepo.QuestField, bool) {
	if record == nil {
		return dnfrepo.QuestState{}, "", false
	}
	if state, ok := record.States[questID]; ok {
		return state, dnfrepo.QuestFieldStates, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, dnfrepo.QuestFieldProgress, true
	}
	return dnfrepo.QuestState{}, "", false
}

func (o *Owner) nextTriggerProgress(
	ctx context.Context,
	characterID string,
	accountID string,
	state dnfrepo.QuestState,
	definition *Definition,
	cmd Command,
) (int64, error) {
	if definition != nil && normalizeQuestTag(definition.Type) == "seek n meet npc" &&
		cmd.TriggerType == 0x10 && !cmd.IsIncrement {
		return o.seekAndMeetNPCItemProgress(ctx, characterID, accountID, state.ProgressValue, *definition)
	}
	if cmd.TriggerType == 1 {
		itemIDs := parseSeekingItemIDs(state.Extra)
		counts := parseSeekingItemCounts(state.Extra, len(itemIDs))
		if len(itemIDs) == 0 {
			itemIDs, counts = seekingRequirementItems(definition)
		}
		if len(itemIDs) > 0 && o.inventory != nil {
			return seekingMissingMask(ctx, o.inventory, characterID, itemIDs, counts), nil
		}
	}
	if channel, ok := setTriggerPackedChannel(cmd.TriggerType); ok {
		if state.ProgressValue < 0 || state.ProgressValue > int64(^uint32(0)) {
			return state.ProgressValue, nil
		}
		progress := uint32(state.ProgressValue)
		if cmd.IsIncrement {
			return int64(incrementPackedTriggerChannel(progress, channel)), nil
		}
		return int64(decrementPackedTriggerChannel(progress, channel)), nil
	}
	if cmd.IsIncrement {
		return state.ProgressValue + 1, nil
	}
	// [condition under clear] 的进度由服务端在通关结算 phase A 权威地减 1
	// （见 clear_map.go 的 decrementClearTriggerChannel），而客户端在每次通关后也会
	// 发一条同样的标量 op33 通知。这里必须原样回权威值、**不再动进度**：
	// 否则一次通关被减 2 次甚至直接清零。
	//
	// 实测（2026-10-04 婚约任务 20737）：PVF 是 [sub type] 22 + [int data] `-1 -1 5`，
	// 即"通关 5 次"。接取时 init_trigger=5 正确，但通关 1 次后 phase A 写 4、紧接着
	// 这条标量通知把它清成 0，任务当场"完成"，客户端显示 5/5。
	if definition != nil && normalizeQuestTag(definition.Type) == "condition under clear" {
		return state.ProgressValue, nil
	}
	// The scalar op33 shape (no lane mask) is the client's "this objective is
	// satisfied" notification, not a one-unit counter tick. Live 2026-09-12:
	// quest 2900 (n_quest/hell/achievement_86_hell_quake_1) held the PVF initial
	// trigger 262657 = packTriggerChannels(1, 1, 1) and every scalar notification
	// used to subtract one from the whole packed u32, so the durable value kept
	// changing (262656, 262655, ... borrows straight into the neighbouring
	// lanes) and the client fed each changed active-quest snapshot back through
	// op33. That produced 3767 identical requests at ~30 Hz across seven logins
	// (262656 -> 258892) while every town entry stayed frozen. Answering the
	// satisfied state (zero) is what the same scalar form means in the healthy
	// case (packet_log 2026-09-12T12:56:41Z reached zero in one step and the
	// client finished the quest 1.7s later), and it lets the client cross its
	// completion transition instead of retrying forever.
	return 0, nil
}

func (o *Owner) seekAndMeetNPCItemProgress(
	ctx context.Context,
	characterID string,
	accountID string,
	progress int64,
	definition Definition,
) (int64, error) {
	if len(definition.IntData) < 3 || definition.IntData[0] <= 0 || definition.IntData[1] <= 0 || definition.IntData[2] <= 0 {
		return progress, fmt.Errorf("seek n meet npc quest %d has malformed int data", definition.ID)
	}
	if progress < 0 || progress > int64(^uint32(0)) || o.inventory == nil {
		return progress, nil
	}
	inventory, found, err := o.inventory.Load(ctx, characterID)
	if err != nil {
		return progress, err
	}
	if !found {
		inventory = dnfrepo.InventoryRecord{CharacterID: characterID}
	}
	if o.accountInventory != nil && strings.TrimSpace(accountID) != "" {
		accountID = strings.TrimSpace(accountID)
		account, accountFound, loadErr := o.accountInventory.Load(ctx, accountID)
		if loadErr != nil {
			return progress, loadErr
		}
		if accountFound && strings.TrimSpace(account.AccountID) != accountID {
			return progress, fmt.Errorf("account inventory owner mismatch: got %q want %q", account.AccountID, accountID)
		}
		account.AccountID = accountID
		inventory = dnfrepo.MergeAccountSharedInventory(inventory, account)
	}
	if countItemInMainUpTo(inventory, definition.IntData[0], definition.IntData[1]) < definition.IntData[1] {
		// The client may probe the item channel before the requirement is met.
		// Preserve the PVF trigger and do not create an ACK/snapshot feedback loop.
		return progress, nil
	}
	return int64(setPackedTriggerChannel(uint32(progress), 0, 0)), nil
}

// Current-client op33 uses one-hot trigger types for the three 9-bit channels
// stored in the active quest's packed u32 trigger value. Treating those values
// as one scalar counter pollutes adjacent channels and makes the client feed
// the malformed value back through op33 indefinitely.
func setTriggerPackedChannel(triggerType byte) (int, bool) {
	switch triggerType {
	case 0x10:
		return 0, true
	case 0x20:
		return 1, true
	case 0x40:
		return 2, true
	default:
		return 0, false
	}
}

var raidPhaseClearCalendar = time.FixedZone("Asia/Shanghai", 8*60*60)

func raidPhaseClearCalendarDay(now time.Time) string {
	// DNF's service day rolls at 06:00 Beijing time, not civil midnight.
	return now.In(raidPhaseClearCalendar).Add(-6 * time.Hour).Format("2006-01-02")
}

// raidPhaseClearTriggerChannel accepts only one decrement for a PVF-declared
// phase channel. Increment requests and channels absent from [int data] are
// not authoritative raid-clear evidence.
func raidPhaseClearTriggerChannel(definition Definition, triggerType byte, increment bool) (int, bool) {
	if increment || len(definition.IntData) == 0 || len(definition.IntData)%4 != 0 {
		return 0, false
	}
	channel, ok := setTriggerPackedChannel(triggerType)
	if !ok {
		return 0, false
	}
	for offset := 0; offset+3 < len(definition.IntData); offset += 4 {
		if definition.IntData[offset] == int64(channel) && definition.IntData[offset+2] > 0 {
			return channel, true
		}
	}
	return 0, false
}

func incrementPackedTriggerChannel(trigger uint32, channel int) uint32 {
	if channel < 0 || channel > 2 {
		return trigger
	}
	shift := channel * 9
	current := (trigger >> shift) & 0x1ff
	if current == 0x1ff {
		return trigger
	}
	trigger &^= 0x1ff << shift
	trigger |= (current + 1) << shift
	return trigger
}

func setPackedTriggerChannel(trigger uint32, channel int, value uint32) uint32 {
	if channel < 0 || channel > 2 {
		return trigger
	}
	shift := channel * 9
	trigger &^= 0x1ff << shift
	trigger |= (value & 0x1ff) << shift
	return trigger
}

// clampQuestProgress ensures quest progress never goes negative (defensive
// guard against malformed client triggers or dungeon-ID leakage).
func clampQuestProgress(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func parseSeekingItemIDs(extra map[string]string) []int64 {
	for _, key := range []string{"seeking_item_ids", "seek_item_ids", "required_item_ids", "item_ids"} {
		if values := parseIntList(extra[key]); len(values) > 0 {
			return values
		}
	}
	return nil
}

// acceptedQuestStateExtra stores the PVF identity for every accepted quest and
// materialises seeking requirements when they are present. The latter is a
// cache only: nextTriggerProgress also re-derives the rules from the active
// PVF definition so existing characters accepted before this field existed
// finish through the same native item-check path.
func acceptedQuestStateExtra(plan AcceptPlan, definition Definition) map[string]string {
	extra := map[string]string{
		"pvf_path":   plan.Path,
		"quest_type": plan.Type,
	}
	itemIDs, counts := seekingRequirementItems(&definition)
	if len(itemIDs) == 0 {
		return extra
	}
	encodedIDs := make([]string, 0, len(itemIDs))
	encodedCounts := make([]string, 0, len(counts))
	for index, itemID := range itemIDs {
		encodedIDs = append(encodedIDs, strconv.FormatInt(itemID, 10))
		count := int64(1)
		if index < len(counts) && counts[index] > 0 {
			count = counts[index]
		}
		encodedCounts = append(encodedCounts, strconv.FormatInt(count, 10))
	}
	extra["seeking_item_ids"] = strings.Join(encodedIDs, ",")
	extra["seeking_item_counts"] = strings.Join(encodedCounts, ",")
	return extra
}

func seekingRequirementItems(definition *Definition) ([]int64, []int64) {
	if definition == nil || normalizeQuestTag(definition.Type) != "seeking" {
		return nil, nil
	}
	rules := itemPairRulesFromInts(definition.IntData)
	if len(rules) == 0 {
		return nil, nil
	}
	itemIDs := make([]int64, 0, len(rules))
	counts := make([]int64, 0, len(rules))
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 {
			continue
		}
		itemIDs = append(itemIDs, rule.ItemID)
		counts = append(counts, rule.Count)
	}
	return itemIDs, counts
}

func parseSeekingItemCounts(extra map[string]string, n int) []int64 {
	for _, key := range []string{"seeking_item_counts", "seek_item_counts", "required_item_counts", "item_counts"} {
		values := parseIntList(extra[key])
		if len(values) > 0 {
			for len(values) < n {
				values = append(values, 1)
			}
			return values[:n]
		}
	}
	values := make([]int64, n)
	for i := range values {
		values[i] = 1
	}
	return values
}

func parseIntList(raw string) []int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '|', ' ', '\t', '\n', '\r':
			return true
		default:
			return false
		}
	})
	out := make([]int64, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && value > 0 {
			out = append(out, value)
		}
	}
	return out
}

func seekingMissingMask(ctx context.Context, inventory dnfrepo.InventoryRepository, characterID string, itemIDs []int64, counts []int64) int64 {
	record, ok, err := inventory.Load(ctx, characterID)
	if err != nil || !ok {
		return 0
	}
	var mask int64
	for index, itemID := range itemIDs {
		required := int64(1)
		if index < len(counts) && counts[index] > 0 {
			required = counts[index]
		}
		if countItemInMain(record, itemID) < required && index < 63 {
			mask |= 1 << index
		}
	}
	return mask
}

func countItemInMain(record dnfrepo.InventoryRecord, itemID int64) int64 {
	var total int64
	for key, stack := range record.Slots {
		if !strings.HasPrefix(key, "0:") || stack.ItemID != itemID {
			continue
		}
		if stack.Count <= 0 {
			total++
			continue
		}
		total += stack.Count
	}
	return total
}

func countItemInMainUpTo(record dnfrepo.InventoryRecord, itemID int64, limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	var total int64
	for key, stack := range record.Slots {
		if !strings.HasPrefix(key, "0:") || stack.ItemID != itemID {
			continue
		}
		count := stack.Count
		if count <= 0 {
			count = 1
		}
		if count >= limit-total {
			return limit
		}
		total += count
	}
	return total
}

func planError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
