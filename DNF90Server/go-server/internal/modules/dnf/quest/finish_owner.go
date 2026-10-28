package quest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/carrygold"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	"longheng.io/server/internal/modules/dnf/profession"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var (
	ErrFinishSettlementUnavailable      = errors.New("dnf quest finish settlement is unavailable")
	ErrFinishQuestNotPending            = errors.New("dnf quest is not pending a finish reward")
	ErrFinishQuestCompletionConflict    = errors.New("dnf quest finish completion key conflicts with persisted state")
	ErrFinishQuestSettlementStale       = errors.New("dnf quest finish replay receipt is stale")
	ErrFinishQuestSettlementCorrupt     = errors.New("dnf quest finish settlement receipt is corrupt")
	ErrFinishQuestMultiplierUnsupported = errors.New("dnf quest finish multiplier is unsupported")
	ErrFinishQuestCommitTimeRequired    = errors.New("dnf quest finish commit time is required")
	ErrFinishItemAllocatorRequired      = errors.New("dnf quest finish item allocator is required")
	ErrFinishProgressionUnproven        = errors.New("dnf quest finish progression is outside proven PVF thresholds")
	ErrFinishExperienceBonusInvalid     = errors.New("dnf quest finish experience bonus is invalid")
	ErrFinishProfessionUnavailable      = errors.New("dnf quest finish profession resources are unavailable")
	ErrFinishRequiredItemsMissing       = errors.New("dnf quest finish required items are missing")
)

const (
	finishRewardGranted          = "granted"
	finishSettlementReceiptKey   = "settlement_receipt_v1"
	finishSettlementReceiptLevel = 1
)

// FinishItemGrantRequest is a PVF-backed item delta which must be placed into
// the transaction-scoped inventory clone. The bridge supplies the allocator
// because current item category ranges and raw entries are current-EXE/PVF
// concerns, while the quest owner retains the atomic transaction boundary.
type FinishItemGrantRequest struct {
	QuestID       int64
	CompletionKey string
	Source        string
	ItemID        int64
	Count         int64
	// AccountInventory 是**本次结算事务内**的账号共享仓记录（事务不可用时为 nil）。
	//
	// 为什么发放器必须看得到它：无色小晶块 / 灵魂 这一类"账号共享道具"在同一个账号里
	// 只有一个家 —— list 0 的 354..365 固定格（权威表见
	// dnfrepo/account_shared_materials.go）。任务奖励过去只落角色背包，于是同一个
	// list-0 报文里会出现两行同 itemID（背包一行 + 账号格一行），客户端把背包那行
	// 显示成仓库的数量 —— 2026-09-21 的事故就是这个。
	//
	// 约定：
	//   · 非 nil 时，发放器**必须**先把这个道具并进它的固定格，装不下的余量才落背包；
	//     它可以直接改这条记录（调用方负责在同一事务里 Save）。
	//   · nil 时发放器必须退回"只落角色背包"（改造前的行为），
	//     **绝不能**因为账号仓不可用让整笔任务结算失败。
	//
	// 目前只有任务通关奖励路径（[reward int data]）会给出这条记录：实测真实
	// Script.pvf 里晶块/灵魂只出现在 [reward int data] / [reward selection int data]
	// 与 [int data]（后者是交付要求，走消耗），而 [depend give item]（接任务时的
	// 携带道具）里一个都没有 —— 所以接任务那条路径仍然按老样子只发背包。
	AccountInventory *dnfrepo.AccountInventoryRecord
}

// FinishCommittedItem is the exact post-allocation item receipt. RawEntry is a
// deep copy of the current 0x77 inventory row after the grant was applied.
type FinishCommittedItem struct {
	SlotKey     string `json:"slot_key"`
	SlotIndex   uint16 `json:"slot_index"`
	ItemID      int64  `json:"item_id"`
	Delta       int64  `json:"delta"`
	PostCount   int64  `json:"post_count"`
	CountOrSeed uint32 `json:"count_or_seed"`
	RawEntry    []byte `json:"raw_entry"`
}

type FinishCommittedCurrency struct {
	Name      string `json:"name"`
	Delta     int64  `json:"delta"`
	PostValue int64  `json:"post_value"`
}

// FinishConsumedItem records one quest-submit material stack consumed by the
// finish transaction. SlotKey is the current list:slot owner key; the current
// EXE finish ACK subtracts Delta from SlotIndex, while post-finish refreshes
// use RawEntry when the stack remains or a delete row when PostCount is zero.
type FinishConsumedItem struct {
	SlotKey   string `json:"slot_key"`
	SlotIndex uint16 `json:"slot_index"`
	ItemID    int64  `json:"item_id"`
	Delta     int64  `json:"delta"`
	PostCount int64  `json:"post_count"`
	RawEntry  []byte `json:"raw_entry,omitempty"`
}

type FinishItemAllocator func(*dnfrepo.InventoryRecord, FinishItemGrantRequest) (FinishCommittedItem, error)

type HellPartyUnlock struct {
	AreaID  int64 `json:"area_id"`
	QuestID int64 `json:"quest_id"`
}

type FinishCommitInput struct {
	AccountID              string
	CharacterID            string
	QuestID                int64
	RewardSelectIndex      uint16
	HasRewardSelect        bool
	Multiplier             uint16
	ExpectedCompletionKey  string
	CommittedAt            time.Time
	Progression            *progression.Tables
	ExperienceBonusPercent int64
	AllocateItem           FinishItemAllocator
	ProfessionProfiles     *profession.Profiles
	SkillCatalog           *dnfskill.Table
	HellPartyUnlocks       []HellPartyUnlock
	// AdventureRuntime 是当前运行时 PVF 的冒险团配置（商店点数换算 / 成长胶囊槽）。
	// 满级角色做任务时，越过等级上限的那部分经验要同事务记进账号级冒险团状态
	// （骁勇结晶、成长胶囊槽、荣誉经验）；没有它就只能整笔丢掉。
	AdventureRuntime *adventuregroup.RuntimeConfig
}

// FinishCommitResult is a detached post-commit snapshot. Every map, slice and
// raw row is cloned before it crosses the domain boundary. It contains no
// opcode or packet fields and is shared by town op34 and dungeon settlement.
type FinishCommitResult struct {
	CharacterID               string
	CompletionKey             string
	Source                    string
	QuestID                   int64
	AtomicCommitted           bool
	Replayed                  bool
	BaseExperienceDelta       uint32
	ExperienceBonusDelta      uint32
	ContinuityExperienceDelta uint32
	ExperienceDelta           uint32
	PreviousLevel             int
	NewLevel                  int
	PreviousExperience        uint32
	NewExperience             uint32
	SPDelta                   int
	TPDelta                   int
	Items                     []FinishCommittedItem
	ConsumedItems             []FinishConsumedItem
	Currency                  []FinishCommittedCurrency
	// VoucherCredits 是本次任务奖励里被直接入账号钱包的代币券 / 欢乐代币券数量。
	//
	// 这两件券在道具 PVF 里是普通 `[type] [material]`、**没有 `[action type]`**，
	// 客户端根本没有任何途径把它们换成钱包余额；当普通道具发下去，玩家只会拿到一件
	// 永远用不掉的"材料"（现场 2026-09-19 的抱怨，见 cerashop/voucher.go）。
	VoucherCredits dnfcerashop.VoucherCounts
	// VoucherWallet 是入账后的账号钱包（nil = 本次奖励里没有券），
	// 供桥接层原样推给客户端。
	VoucherWallet              *dnfcerashop.Wallet
	PostCommitCharacter        dnfrepo.CharacterRecord
	PostCommitSkill            dnfrepo.SkillRecord
	PostCommitInventory        dnfrepo.InventoryRecord
	PostCommitAccountInventory dnfrepo.AccountInventoryRecord
	PostCommitQuest            dnfrepo.QuestRecord
	Profession                 profession.Transition
	ProfessionGrants           []profession.Grant
	HasProfession              bool
	ExpertJobType              byte
	HasExpertJob               bool
	HasSlotExpansion           bool
	SlotExpansionIndex         uint32
	SlotExpansionBit           byte
}

type finishSettlementReceipt struct {
	Version                   int                       `json:"version"`
	CharacterID               string                    `json:"character_id"`
	CompletionKey             string                    `json:"completion_key"`
	Source                    string                    `json:"source"`
	QuestID                   int64                     `json:"quest_id"`
	BaseExperienceDelta       uint32                    `json:"base_experience_delta,omitempty"`
	ExperienceBonusDelta      uint32                    `json:"experience_bonus_delta,omitempty"`
	ContinuityExperienceDelta uint32                    `json:"continuity_experience_delta,omitempty"`
	ExperienceDelta           uint32                    `json:"experience_delta"`
	PreviousLevel             int                       `json:"previous_level"`
	NewLevel                  int                       `json:"new_level"`
	PreviousExperience        uint32                    `json:"previous_experience"`
	NewExperience             uint32                    `json:"new_experience"`
	SPDelta                   int                       `json:"sp_delta"`
	TPDelta                   int                       `json:"tp_delta"`
	PostSkillPoints           dnfrepo.SkillPointState   `json:"post_skill_points"`
	Items                     []FinishCommittedItem     `json:"items,omitempty"`
	ConsumedItems             []FinishConsumedItem      `json:"consumed_items,omitempty"`
	Currency                  []FinishCommittedCurrency `json:"currency,omitempty"`
	// VoucherCredits 必须进收据：重放时只从收据重建结果、不再发放，
	// 所以入账数量必须与"已发放"的事实一起持久化。
	VoucherCredits     dnfcerashop.VoucherCounts `json:"voucher_credits,omitempty"`
	Profession         profession.Transition     `json:"profession,omitempty"`
	ProfessionGrants   []profession.Grant        `json:"profession_grants,omitempty"`
	HasProfession      bool                      `json:"has_profession,omitempty"`
	ExpertJobType      byte                      `json:"expert_job_type,omitempty"`
	HasExpertJob       bool                      `json:"has_expert_job,omitempty"`
	HasSlotExpansion   bool                      `json:"has_slot_expansion,omitempty"`
	SlotExpansionIndex uint32                    `json:"slot_expansion_index,omitempty"`
	SlotExpansionBit   byte                      `json:"slot_expansion_bit,omitempty"`
	HellPartyUnlocks   []HellPartyUnlock         `json:"hell_party_unlocks,omitempty"`
}

// ApplyFinishSettlement validates the Phase-A pending marker and atomically
// commits quest state, PVF experience/SP, and PVF item rewards. A same-key
// replay returns the persisted receipt without granting twice; a different or
// no-longer-current receipt fails closed.
func (o *Owner) ApplyFinishSettlement(
	ctx context.Context,
	catalog *Catalog,
	input FinishCommitInput,
) (FinishCommitResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil || catalog == nil || input.Progression == nil {
		return FinishCommitResult{}, ErrFinishSettlementUnavailable
	}
	input.AccountID = strings.TrimSpace(input.AccountID)
	input.CharacterID = strings.TrimSpace(input.CharacterID)
	input.ExpectedCompletionKey = strings.TrimSpace(input.ExpectedCompletionKey)
	if input.AccountID == "" || input.CharacterID == "" {
		return FinishCommitResult{}, ErrCharacterRequired
	}
	if input.QuestID <= 0 || input.QuestID > math.MaxUint16 {
		return FinishCommitResult{}, ErrQuestIDRequired
	}
	if input.Multiplier != 1 {
		return FinishCommitResult{}, fmt.Errorf("%w: multiplier=%d", ErrFinishQuestMultiplierUnsupported, input.Multiplier)
	}
	if input.CommittedAt.IsZero() {
		return FinishCommitResult{}, ErrFinishQuestCommitTimeRequired
	}
	if input.ExperienceBonusPercent < 0 {
		return FinishCommitResult{}, fmt.Errorf("%w: percent=%d", ErrFinishExperienceBonusInvalid, input.ExperienceBonusPercent)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var candidate FinishCommitResult
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, input.CharacterID, func(tx dnfrepo.Group) error {
		if tx.AccountInventory == nil || tx.Character == nil || tx.Quest == nil || tx.Skill == nil || tx.Inventory == nil || tx.Equipment == nil {
			return ErrFinishSettlementUnavailable
		}
		character, found, err := tx.Character.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterNotFound
		}
		character = dnfrepo.CloneCharacter(character)
		if strings.TrimSpace(character.CharacterID) != input.CharacterID || strings.TrimSpace(character.AccountID) != input.AccountID {
			return fmt.Errorf("%w: character=%q account=%q", ErrCharacterNotFound, character.CharacterID, character.AccountID)
		}
		accountInventory, accountInventoryFound, err := tx.AccountInventory.Load(ctx, input.AccountID)
		if err != nil {
			return err
		}
		if accountInventoryFound && strings.TrimSpace(accountInventory.AccountID) != input.AccountID {
			return fmt.Errorf("%w: account inventory owner=%q", ErrFinishSettlementUnavailable, accountInventory.AccountID)
		}
		accountInventory = dnfrepo.CloneAccountInventory(accountInventory)
		accountInventory.AccountID = input.AccountID

		quests, found, err := tx.Quest.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(quests.CharacterID) != input.CharacterID {
			return ErrFinishQuestNotPending
		}
		quests = dnfrepo.CloneQuest(quests)
		state, field, known := mutableQuestState(&quests, input.QuestID)
		if !known {
			return ErrFinishQuestNotPending
		}

		skill, found, err := tx.Skill.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(skill.CharacterID) != input.CharacterID {
			return fmt.Errorf("%w: skill record", ErrFinishSettlementUnavailable)
		}
		skill = dnfrepo.CloneSkill(skill)
		previousSkill := dnfrepo.CloneSkill(skill)
		previousSkillPoints := skill.Points
		inventory, found, err := tx.Inventory.Load(ctx, input.CharacterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != input.CharacterID {
			return fmt.Errorf("%w: inventory record", ErrFinishSettlementUnavailable)
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if equipment, equipmentFound, err := tx.Equipment.Load(ctx, input.CharacterID); err != nil {
			return err
		} else if equipmentFound && strings.TrimSpace(equipment.CharacterID) != input.CharacterID {
			return fmt.Errorf("%w: equipment owner=%q", ErrFinishSettlementUnavailable, equipment.CharacterID)
		}

		now := input.CommittedAt.UTC()
		questFields := []dnfrepo.QuestField{field}
		// A subtype-2 parent can own a final reward-bearing child. Keep that
		// child active until the atomic settlement below consumes its materials,
		// grants its reward, and completes both rows. The legacy quest-clear
		// helper is reserved for no-reward linked objectives.
		rewardChildPending := false
		if jobValue, parseErr := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8); parseErr == nil &&
			jobmap.Valid(int(jobValue)) && character.Level > 0 {
			growTypeValue := character.Stats["grow_type"]
			if growTypeValue >= 0 && growTypeValue <= math.MaxUint8 {
				_, _, rewardChildPending = catalog.FinishRewardDefinition(
					quests,
					CharacterEligibility{Level: character.Level, Job: int(jobValue), GrowType: int(growTypeValue)},
					input.QuestID,
				)
			}
		}
		if isActiveQuestStatus(state.Status) && !rewardChildPending {
			if questClearTrigger, linkedFields, questClear := finishQuestClearTrigger(catalog, &quests, input.QuestID, now); questClear {
				if questClearTrigger != 0 {
					return ErrFinishQuestNotPending
				}
				for _, linkedField := range linkedFields {
					questFields = mergeQuestChangedField(questFields, linkedField)
				}
				if state.ProgressValue != 0 {
					state.ProgressValue = 0
					state.UpdatedAt = now
					switch field {
					case dnfrepo.QuestFieldStates:
						quests.States[input.QuestID] = state
					case dnfrepo.QuestFieldProgress:
						quests.Progress[input.QuestID] = state
					default:
						return ErrFinishQuestNotPending
					}
				}
			}
		}

		completionKey := strings.TrimSpace(state.Extra["completion_key"])
		if completionKey == "" && isActiveQuestStatus(state.Status) && state.ProgressValue == 0 && strings.TrimSpace(state.Extra["reward_state"]) == "" {
			// NPC/dialogue profession quests do not pass through dungeon clear-map
			// Phase A. Their durable completion proof is the accepted quest row
			// itself with a zero trigger. Derive the key from persisted acceptance
			// state so a rolled-back/retried op34 produces the same identity.
			proofTime := state.UpdatedAt.UTC().UnixNano()
			if proofTime == 0 {
				proofTime = quests.UpdatedAt.UTC().UnixNano()
			}
			completionKey = fmt.Sprintf("quest-finish/%s/%d/%d", input.CharacterID, input.QuestID, proofTime)
			if state.Extra == nil {
				state.Extra = make(map[string]string, 8)
			}
			state.Extra["completion_key"] = completionKey
			state.Extra["completion_kind"] = "active_trigger_zero_op34"
			state.Extra["reward_state"] = clearMapRewardPending
		}
		if completionKey == "" {
			return ErrFinishQuestNotPending
		}
		if input.ExpectedCompletionKey != "" && input.ExpectedCompletionKey != completionKey {
			return fmt.Errorf("%w: expected=%q persisted=%q", ErrFinishQuestCompletionConflict, input.ExpectedCompletionKey, completionKey)
		}
		if strings.EqualFold(strings.TrimSpace(state.Extra["reward_state"]), finishRewardGranted) {
			replayed, err := finishResultFromReceipt(ctx, input, tx.HellPartyPermission, state, character, skill, inventory, accountInventory, quests)
			if err != nil {
				return err
			}
			candidate = replayed
			return nil
		}
		if !isActiveQuestStatus(state.Status) || state.ProgressValue != 0 || strings.TrimSpace(state.Extra["reward_state"]) != clearMapRewardPending {
			return ErrFinishQuestNotPending
		}

		jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
		if err != nil || !jobmap.Valid(int(jobValue)) || character.Level <= 0 {
			return fmt.Errorf("%w: level=%d job=%q", ErrQuestNotAcceptable, character.Level, character.Job)
		}
		growTypeValue := character.Stats["grow_type"]
		if growTypeValue < 0 || growTypeValue > math.MaxUint8 {
			return fmt.Errorf("%w: grow_type=%d", ErrQuestNotAcceptable, growTypeValue)
		}
		growType := int(growTypeValue)
		eligibility := CharacterEligibility{Level: character.Level, Job: int(jobValue), GrowType: growType, Wedding: WeddingEligible(character)}
		_, definitionKnown := catalog.Find(input.QuestID)
		if !definitionKnown {
			return ErrQuestDefinitionMissing
		}
		rewardDefinition, additionalCompleted, rewardChild := catalog.FinishRewardDefinition(quests, eligibility, input.QuestID)
		rewardQuestID := input.QuestID
		if rewardChild {
			rewardQuestID = rewardDefinition.ID
		}
		reward, err := catalog.PlanFinishReward(eligibility, rewardQuestID, input.RewardSelectIndex, input.HasRewardSelect)
		if err != nil {
			return err
		}
		var hellPartyUnlocks []HellPartyUnlock
		if reward.HasHellPartyUnlock {
			hellPartyUnlocks, err = validFinishHellPartyUnlocks(input.HellPartyUnlocks, input.QuestID)
			if err != nil || tx.HellPartyPermission == nil {
				return ErrFinishSettlementUnavailable
			}
			for _, unlock := range hellPartyUnlocks {
				if _, _, err := tx.HellPartyPermission.Upsert(ctx, input.CharacterID, unlock.AreaID, unlock.QuestID); err != nil {
					return err
				}
			}
		}
		if reward.HasProfession && (input.ProfessionProfiles == nil || input.SkillCatalog == nil) {
			return ErrFinishProfessionUnavailable
		}
		if reward.HasProfession && reward.HasExpertJob {
			return ErrQuestRewardMalformed
		}
		if reward.HasProfession {
			reward.Profession, err = input.ProfessionProfiles.PlanTransition(byte(jobValue), byte(growType), reward.ProfessionRequest)
			if err != nil {
				return err
			}
		}
		baseExperienceDelta, err := input.Progression.QuestExperience(character.Level, reward.QuestLevel, reward.Difficulty, reward.IgnoreLevel4Exp)
		if err != nil {
			return err
		}
		experienceDelta, experienceBonusDelta := finishExperienceWithBonus(baseExperienceDelta, input.ExperienceBonusPercent)
		progressPlan, err := planFinishProgression(input.Progression, character, skill, experienceDelta)
		if err != nil {
			return err
		}
		continuityExperienceDelta := uint32(0)
		continuityEligibility := eligibility
		if reward.HasProfession {
			continuityEligibility.GrowType = int(reward.Profession.NewGrowType)
		}
		completedQuestIDs := append([]int64{input.QuestID}, additionalCompleted...)
		if targetLevel, applies := catalog.MainContinuityLevelFloor(continuityEligibility, quests, completedQuestIDs...); applies {
			if targetLevel > progression.MaximumCharacterLevel {
				targetLevel = progression.MaximumCharacterLevel
			}
			if progressPlan.Experience.NewLevel < targetLevel {
				threshold, thresholdErr := input.Progression.ThresholdToNext(targetLevel - 1)
				if thresholdErr != nil {
					return fmt.Errorf("%w: main continuity target_level=%d: %v", ErrFinishProgressionUnproven, targetLevel, thresholdErr)
				}
				if threshold > progressPlan.Experience.NewExperience {
					continuityExperienceDelta = threshold - progressPlan.Experience.NewExperience
					experienceDelta += continuityExperienceDelta
					progressPlan, err = planFinishProgression(input.Progression, character, skill, experienceDelta)
					if err != nil {
						return err
					}
				}
			}
		}

		if character.Stats == nil {
			character.Stats = make(map[string]int64, 2)
		}
		// 任务经验现在**任意等级**都要镜像进账号账本：全额进骁勇结晶，
		// 其中越过等级上限的部分才进成长胶囊槽与名誉经验。口径与怪物击杀、
		// 通关结算完全一致（同一个 adventuregroup.CreditExperience），
		// 且与角色经验同事务提交。
		creditAccountID := strings.TrimSpace(input.AccountID)
		if input.AdventureRuntime != nil && tx.Account != nil && creditAccountID != "" && experienceDelta > 0 {
			previousExperience, present := character.Stats["exp"]
			if !present || previousExperience < 0 || uint64(previousExperience) > math.MaxUint32 {
				return fmt.Errorf("%w: exp=%d present=%t", ErrFinishProgressionUnproven, previousExperience, present)
			}
			postCapExperience, shareErr := dnfhonor.PostCapExperienceShare(
				input.Progression,
				character.Level,
				uint32(previousExperience),
				experienceDelta,
				progression.MaximumCharacterLevel,
			)
			if shareErr != nil {
				return shareErr
			}
			if _, creditErr := adventuregroup.CreditExperience(
				ctx,
				tx.Account,
				*input.AdventureRuntime,
				creditAccountID,
				adventuregroup.ExperienceCredit{
					AwardedExperience:      uint64(experienceDelta),
					MaximumLevelExperience: uint64(postCapExperience),
				},
				now,
			); creditErr != nil {
				// 账号行缺失时的取舍与怪物击杀/通关结算一致：有满级部分就必须落账
				// （整笔失败），只有未满级经验镜像进结晶时跳过记账、照常发任务奖励。
				if !errors.Is(creditErr, adventuregroup.ErrCreditAccountNotFound) || postCapExperience > 0 {
					return creditErr
				}
			}
		}
		requiredItems := finishRequiredItemRules(catalog, rewardDefinition, eligibility)
		consumedItems, err := consumeFinishRequiredItems(&inventory, &accountInventory, requiredItems)
		if err != nil {
			return err
		}
		currency, err := chargeFinishGoldCost(&character, reward)
		if err != nil {
			return err
		}
		rewardCurrency, err := applyFinishRewardCurrency(&character, input.Progression, reward)
		if err != nil {
			return err
		}
		currency = append(currency, rewardCurrency...)

		source := "quest_pvf:" + reward.PVFPath
		// 代币券 / 欢乐代币券不落背包：它们在道具 PVF 里是普通 `[material]`，
		// 没有 `[action type]`，客户端根本无法把它们换成钱包余额，当道具发下去
		// 只会得到一件永远用不掉的"材料"。
		//
		// 做法是先把券从奖励规则里**摘出来**，剩下的规则照原样走既有落格逻辑
		// （非券奖励的行为一个字节都不变），券改成入账号钱包 —— 且与任务结算
		// **同一事务**：要么任务完成且券到账，要么两者都没发生。
		voucherRules, itemRules := splitFinishVoucherRules(reward.Items)
		// ⚠ 只有"有券 **且** 账号仓储可用 **且** 账号号非空"时才把券入钱包。
		//
		// 账号号为空时必须整体退回道具发放：`tx.Account.Load(ctx, "")` 会因为
		// TransactionKeyError 直接报错，那会把整笔任务结算打成硬失败 —— 改动前这种
		// 情况只是把券当普通道具发下去，任务照常完成。这是"静默改变失败模式"，
		// 属于绝不能引入的那一类。
		//
		// `allocateFinishRewardItems` 会直接改 `inventory`（落格），所以**只能调用一次**：
		// 早先的写法是先用 itemRules 落一次、发现不可用再用全部规则落第二次 ——
		// 那会把非券奖励**发两份**。这种"边界条件下静默多发"比直接报错危险得多。
		accountID := strings.TrimSpace(input.AccountID)
		creditVouchers := len(voucherRules) > 0 && tx.Account != nil && accountID != ""
		if !creditVouchers {
			itemRules = reward.Items
		}
		items, err := allocateFinishRewardItems(&inventory, itemRules, input.AllocateItem, FinishItemGrantRequest{
			QuestID: input.QuestID, CompletionKey: completionKey, Source: source,
			// 账号共享仓随请求一起交给发放器：无色/灵魂类奖励的唯一落点是它的固定格。
			// 这条记录已经在本事务里 Load + Clone 过，发放器改的就是它，
			// 下面的 Save 条件再决定要不要落库。
			AccountInventory: &accountInventory,
		})
		if err != nil {
			return err
		}
		var voucherCredits dnfcerashop.VoucherCounts
		var voucherWallet *dnfcerashop.Wallet
		if creditVouchers {
			voucherCredits, err = finishVoucherCounts(voucherRules)
			if err != nil {
				return err
			}
			account, accountFound, accountErr := tx.Account.Load(ctx, accountID)
			if accountErr != nil {
				return accountErr
			}
			if !accountFound {
				return ErrFinishSettlementUnavailable
			}
			account = dnfrepo.CloneAccount(account)
			// 溢出 = 整笔结算失败：客户端把这三个字段按 i32 读，截断会显示成负数；
			// 而"任务标记完成却没发奖励"同样不可接受，所以宁可让玩家重试。
			updated, creditErr := dnfcerashop.CreditVoucherCounts(&account, voucherCredits)
			if creditErr != nil {
				return fmt.Errorf("%w: voucher credit: %v", ErrQuestRewardMalformed, creditErr)
			}
			account.UpdatedAt = now
			if err := tx.Account.Save(ctx, account); err != nil {
				return err
			}
			wallet := updated
			voucherWallet = &wallet
		}
		character.Level = progressPlan.Experience.NewLevel
		character.Stats["exp"] = int64(progressPlan.Experience.NewExperience)
		character.UpdatedAt = now
		skill.Points = progressPlan.SkillPoints.New
		if reward.HasProfession {
			character.Stats["grow_type"] = int64(reward.Profession.NewGrowType)
			skill, err = input.ProfessionProfiles.ApplySkillTransition(
				input.SkillCatalog,
				byte(jobValue),
				progressPlan.Experience.NewLevel,
				reward.Profession,
				skill,
				progressPlan.SkillPoints.New,
			)
			if err != nil {
				return err
			}
		}
		professionGrants, err := finishProfessionGrants(previousSkill, skill, reward.HasProfession)
		if err != nil {
			return err
		}
		if reward.HasExpertJob {
			// characters.expert_job_type is the repository-backed source used by
			// the current USERINFO subtype0 builder. The op34 chain-20 ACK mirrors
			// this same committed value for the online client.
			character.Stats["expert_job_type"] = int64(reward.ExpertJobType)
		}
		if reward.HasSlotExpansion {
			character.Stats["ex_equip_slot_stat"] = character.Stats["ex_equip_slot_stat"] | int64(reward.SlotExpansionBit)
		}
		skill.UpdatedAt = now
		inventory.UpdatedAt = now
		// 账号记录既可能被"交材料"改到，也可能被"发奖励"改到（无色/灵魂落 354..365
		// 固定格），两者任意一种发生都要在同一事务里落库。
		accountSharedTouched := finishConsumedAccountSharedItems(consumedItems) || finishGrantedAccountSharedItems(items)
		if accountSharedTouched {
			accountInventory.UpdatedAt = now
		}

		receipt := finishSettlementReceipt{
			Version: finishSettlementReceiptLevel, CharacterID: input.CharacterID,
			CompletionKey: completionKey, Source: source, QuestID: input.QuestID,
			BaseExperienceDelta: baseExperienceDelta, ExperienceBonusDelta: experienceBonusDelta,
			ContinuityExperienceDelta: continuityExperienceDelta,
			ExperienceDelta:           experienceDelta,
			PreviousLevel:             progressPlan.Experience.PreviousLevel, NewLevel: progressPlan.Experience.NewLevel,
			PreviousExperience: progressPlan.Experience.PreviousExperience, NewExperience: progressPlan.Experience.NewExperience,
			SPDelta:            skill.Points.RemainingSP - previousSkillPoints.RemainingSP,
			TPDelta:            skill.Points.RemainingTP - previousSkillPoints.RemainingTP,
			PostSkillPoints:    skill.Points,
			Items:              cloneFinishItems(items),
			ConsumedItems:      cloneFinishConsumedItems(consumedItems),
			Currency:           append([]FinishCommittedCurrency(nil), currency...),
			VoucherCredits:     voucherCredits,
			Profession:         reward.Profession,
			ProfessionGrants:   cloneProfessionGrants(professionGrants),
			HasProfession:      reward.HasProfession,
			ExpertJobType:      reward.ExpertJobType,
			HasExpertJob:       reward.HasExpertJob,
			HasSlotExpansion:   reward.HasSlotExpansion,
			SlotExpansionIndex: reward.SlotExpansionIndex,
			SlotExpansionBit:   reward.SlotExpansionBit,
			HellPartyUnlocks:   cloneFinishHellPartyUnlocks(hellPartyUnlocks),
		}
		receiptJSON, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		state.Status = "completed"
		state.ProgressValue = 0
		state.RewardSelectIndex = int64(input.RewardSelectIndex)
		state.Multiplier = 1
		state.UpdatedAt = now
		if state.Extra == nil {
			state.Extra = make(map[string]string, 8)
		}
		state.Extra["reward_state"] = finishRewardGranted
		state.Extra["settlement_source"] = source
		state.Extra[finishSettlementReceiptKey] = string(receiptJSON)
		switch field {
		case dnfrepo.QuestFieldStates:
			quests.States[input.QuestID] = state
		case dnfrepo.QuestFieldProgress:
			quests.Progress[input.QuestID] = state
		default:
			return ErrFinishQuestNotPending
		}
		quests.UpdatedAt = now
		for _, childID := range additionalCompleted {
			childState, childField, childKnown := mutableQuestState(&quests, childID)
			if !childKnown || !isActiveQuestStatus(childState.Status) || childState.ProgressValue != 0 {
				return ErrFinishQuestNotPending
			}
			childState.Status = "completed"
			childState.UpdatedAt = now
			if childState.Extra == nil {
				childState.Extra = make(map[string]string, 6)
			}
			childState.Extra["reward_state"] = finishRewardGranted
			childState.Extra["completed_by_parent"] = strconv.FormatInt(input.QuestID, 10)
			switch childField {
			case dnfrepo.QuestFieldStates:
				quests.States[childID] = childState
			case dnfrepo.QuestFieldProgress:
				quests.Progress[childID] = childState
			default:
				return ErrFinishQuestNotPending
			}
			questFields = mergeQuestChangedField(questFields, childField)
		}
		for _, parentField := range syncActiveQuestClearParentProgress(catalog, &quests, now) {
			questFields = mergeQuestChangedField(questFields, parentField)
		}

		// 回归冒险家（onlineevent returnee）的"完成任务"计数：它是回归任务链的
		// 推进量，只在这里累加（与任务交付同事务，重放同一交付不会重复计入）。
		// is_return_user 非 0 才算回归账号。
		if character.Stats["is_return_user"] != 0 {
			character.Stats[dnfonlineevent.ReturneeQuestCountKey] =
				min(max(character.Stats[dnfonlineevent.ReturneeQuestCountKey], 0), math.MaxInt64-1) + 1
		}
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		skillFields := []dnfrepo.SkillField{dnfrepo.SkillFieldPoints}
		if reward.HasProfession {
			skillFields = append(
				skillFields,
				dnfrepo.SkillFieldSkills,
				dnfrepo.SkillFieldTrees,
				dnfrepo.SkillFieldLayouts,
				dnfrepo.SkillFieldCooldowns,
			)
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill, skillFields...); err != nil {
			return err
		}
		if err := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); err != nil {
			return err
		}
		// 账号记录既可能被"扣材料"改到，也可能被"发奖励"改到（无色/灵魂落固定格），
		// 两者任意一种发生都要在同一事务里落库。
		if accountSharedTouched {
			if err := tx.AccountInventory.Save(ctx, accountInventory); err != nil {
				return err
			}
		}
		if err := dnfrepo.SaveQuestFields(ctx, tx.Quest, quests, questFields...); err != nil {
			return err
		}

		candidate = finishResultFromCommitted(receipt, false, character, skill, inventory, accountInventory, quests)
		candidate.VoucherWallet = voucherWallet
		return nil
	})
	if err != nil {
		return FinishCommitResult{}, err
	}
	candidate.AtomicCommitted = true
	return cloneFinishCommitResult(candidate), nil
}

func finishQuestClearTrigger(catalog *Catalog, record *dnfrepo.QuestRecord, questID int64, completedAt time.Time) (int64, []dnfrepo.QuestField, bool) {
	if catalog == nil || record == nil {
		return 0, nil, false
	}
	definition, known := catalog.Find(questID)
	if !known {
		return 0, nil, false
	}
	tag := normalizeQuestTag(definition.Type)
	if tag != "quest clear" && tag != "clear quest" {
		return 0, nil, false
	}
	completed, _ := questStateSets(*record)
	trigger := questClearParentTrigger(definition.IntData, completed)
	if trigger == 0 {
		return 0, nil, true
	}

	// The current client finishes the final linked subquest differently from
	// intermediate linked steps: after its trigger channels reach zero it sends
	// op33 for the parent and then op34 for the parent, without a child op34.
	// Accept that terminal state only when every unresolved PVF child belongs to
	// this parent and is durably active at exactly zero progress.
	type terminalChild struct {
		questID int64
		field   dnfrepo.QuestField
		state   dnfrepo.QuestState
	}
	children := make([]terminalChild, 0, len(definition.IntData))
	for _, childID := range definition.IntData {
		if childID <= 0 {
			continue
		}
		if _, done := completed[childID]; done {
			continue
		}
		childDefinition, childKnown := catalog.Find(childID)
		childState, childField, stateKnown := mutableQuestState(record, childID)
		if !childKnown || childDefinition.MainQuestID != questID || !linkedObjectiveQuest(childDefinition) || !stateKnown ||
			!isActiveQuestStatus(childState.Status) || childState.ProgressValue != 0 {
			return trigger, nil, true
		}
		children = append(children, terminalChild{questID: childID, field: childField, state: childState})
	}
	if len(children) == 0 {
		return trigger, nil, true
	}

	changedFields := make([]dnfrepo.QuestField, 0, 2)
	for _, child := range children {
		child.state.Status = "completed"
		child.state.ProgressValue = 0
		child.state.UpdatedAt = completedAt
		if child.state.Extra == nil {
			child.state.Extra = make(map[string]string, 6)
		}
		child.state.Extra["reward_state"] = finishRewardGranted
		child.state.Extra["auto_completed"] = "true"
		child.state.Extra["auto_complete_reason"] = "quest_clear_parent_terminal_zero_trigger"
		child.state.Extra["auto_completed_by_parent"] = strconv.FormatInt(questID, 10)
		switch child.field {
		case dnfrepo.QuestFieldStates:
			record.States[child.questID] = child.state
		case dnfrepo.QuestFieldProgress:
			record.Progress[child.questID] = child.state
		default:
			return trigger, nil, true
		}
		changedFields = mergeQuestChangedField(changedFields, child.field)
	}
	return 0, changedFields, true
}

// syncActiveQuestClearParentProgress ports only the C# completion-transaction
// rule: after a quest becomes cleared, every currently active quest-clear
// parent recomputes its remaining prerequisite count in that same transaction.
// It intentionally changes no protocol field; the existing post-commit quest
// snapshots serialize the persisted trigger values for the current EXE.
func syncActiveQuestClearParentProgress(
	catalog *Catalog,
	record *dnfrepo.QuestRecord,
	now time.Time,
) []dnfrepo.QuestField {
	if catalog == nil || record == nil {
		return nil
	}
	completed, _ := questStateSets(*record)
	changed := make(map[dnfrepo.QuestField]bool, 2)
	syncField := func(field dnfrepo.QuestField, states map[int64]dnfrepo.QuestState) {
		if len(states) == 0 {
			return
		}
		ids := make([]int64, 0, len(states))
		for questID := range states {
			ids = append(ids, questID)
		}
		sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
		for _, questID := range ids {
			state := states[questID]
			if !isActiveQuestStatus(state.Status) {
				continue
			}
			definition, known := catalog.Find(questID)
			if !known {
				continue
			}
			tag := normalizeQuestTag(definition.Type)
			if tag != "quest clear" && tag != "clear quest" {
				continue
			}
			next := questClearParentTrigger(definition.IntData, completed)
			if next < 0 {
				next = 0
			}
			if state.ProgressValue == next {
				continue
			}
			state.ProgressValue = next
			state.UpdatedAt = now
			states[questID] = state
			changed[field] = true
		}
	}
	syncField(dnfrepo.QuestFieldStates, record.States)
	syncField(dnfrepo.QuestFieldProgress, record.Progress)

	fields := make([]dnfrepo.QuestField, 0, len(changed))
	for _, field := range []dnfrepo.QuestField{dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress} {
		if changed[field] {
			fields = append(fields, field)
		}
	}
	return fields
}

// questClearParentTrigger deliberately differs from the accept-time helper:
// an active quest-clear parent with real prerequisites is finishable when all
// of them are completed, so its trigger becomes zero. A malformed/empty
// prerequisite list keeps the existing C# fallback of one.
func questClearParentTrigger(values []int64, completed map[int64]struct{}) int64 {
	hasRequired := false
	missing := int64(0)
	for _, questID := range values {
		if questID <= 0 {
			continue
		}
		hasRequired = true
		if _, exists := completed[questID]; !exists {
			missing++
		}
	}
	if !hasRequired {
		return 1
	}
	return int64(boundedTriggerChannel(missing))
}

func planFinishProgression(
	tables *progression.Tables,
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	gain uint32,
) (progression.ExperienceSkillPointPlan, error) {
	if tables == nil || character.Level <= 0 || character.Level > progression.MaximumCharacterLevel ||
		skill.Points.SyncedLevel != character.Level {
		return progression.ExperienceSkillPointPlan{}, fmt.Errorf("%w: level=%d synced=%d", ErrFinishProgressionUnproven, character.Level, skill.Points.SyncedLevel)
	}
	experienceValue, exists := character.Stats["exp"]
	if !exists || experienceValue < 0 || experienceValue > math.MaxUint32 {
		return progression.ExperienceSkillPointPlan{}, fmt.Errorf("%w: exp=%d present=%t", ErrFinishProgressionUnproven, experienceValue, exists)
	}
	newTotal := uint64(experienceValue) + uint64(gain)
	if newTotal > math.MaxUint32 {
		return progression.ExperienceSkillPointPlan{}, progression.ErrExperienceOutOfRange
	}
	result := progression.ExperienceResult{
		PreviousLevel: character.Level, PreviousExperience: uint32(experienceValue), Gain: gain,
		NewLevel: character.Level, NewExperience: uint32(newTotal),
	}
	for result.NewLevel < progression.MaximumCharacterLevel {
		threshold, err := tables.ThresholdToNext(result.NewLevel)
		if err != nil {
			return progression.ExperienceSkillPointPlan{}, fmt.Errorf("%w: level=%d: %v", ErrFinishProgressionUnproven, result.NewLevel, err)
		}
		if result.NewExperience < threshold {
			break
		}
		if _, ok := tables.SkillPointsAtLevel(result.NewLevel + 1); !ok {
			return progression.ExperienceSkillPointPlan{}, fmt.Errorf("%w: missing SP row level=%d", ErrFinishProgressionUnproven, result.NewLevel+1)
		}
		result.NewLevel++
	}
	// Ordinary character EXP is pinned to the cumulative threshold that enters
	// level 90. ExpTable/SP rows above 90 are compatibility data, not permission
	// for a quest finish to persist level 91.
	if result.NewLevel == progression.MaximumCharacterLevel {
		entryThreshold, err := tables.ThresholdToNext(progression.MaximumCharacterLevel - 1)
		if err != nil {
			return progression.ExperienceSkillPointPlan{}, fmt.Errorf(
				"%w: level=%d: %v",
				ErrFinishProgressionUnproven,
				progression.MaximumCharacterLevel,
				err,
			)
		}
		if result.NewExperience > entryThreshold {
			result.NewExperience = entryThreshold
			result.Saturated = true
		}
	}
	result.LevelsGained = result.NewLevel - result.PreviousLevel
	points, err := tables.AdvanceSkillPoints(skill.Points, result.NewLevel)
	if err != nil {
		return progression.ExperienceSkillPointPlan{}, err
	}
	return progression.ExperienceSkillPointPlan{Experience: result, SkillPoints: points}, nil
}

func finishExperienceWithBonus(base uint32, percent int64) (uint32, uint32) {
	if base == 0 || percent <= 0 {
		return base, 0
	}
	limit := uint64(math.MaxUint32 - base)
	if uint64(percent) > math.MaxUint64/uint64(base) {
		return math.MaxUint32, uint32(limit)
	}
	bonus := uint64(base) * uint64(percent) / 100
	if bonus > limit {
		bonus = limit
	}
	return base + uint32(bonus), uint32(bonus)
}

// splitFinishVoucherRules 把任务奖励规则拆成"券"和"普通道具"两组，保持原有顺序。
//
// 券（代币券 2681762 / 欢乐代币券 2681917）不落背包，直接入账号钱包；其余规则原样
// 交给既有的落格逻辑，所以非券奖励的行为完全不变。
func splitFinishVoucherRules(rules []RewardItemRule) (vouchers, items []RewardItemRule) {
	for _, rule := range rules {
		if dnfcerashop.IsVoucher(rule.ItemID) {
			vouchers = append(vouchers, rule)
			continue
		}
		items = append(items, rule)
	}
	return vouchers, items
}

// finishVoucherCounts 把券规则折成入账数量。
//
// 这里对每条规则做与 allocateFinishRewardItems 相同的字段校验：券既然不走落格路径，
// 就没人替我们挡住 `Count <= 0` / 超出 uint32 这类坏值了，必须自己挡。
func finishVoucherCounts(rules []RewardItemRule) (dnfcerashop.VoucherCounts, error) {
	var counts dnfcerashop.VoucherCounts
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 || rule.Count > math.MaxUint32 {
			return dnfcerashop.VoucherCounts{}, ErrQuestRewardMalformed
		}
		switch dnfcerashop.VoucherKindOf(rule.ItemID) {
		case dnfcerashop.VoucherTokenCera:
			if counts.TokenCera > math.MaxInt32-rule.Count {
				return dnfcerashop.VoucherCounts{}, fmt.Errorf("%w: token cera voucher overflow", ErrQuestRewardMalformed)
			}
			counts.TokenCera += rule.Count
		case dnfcerashop.VoucherHappyTokenCera:
			if counts.HappyTokenCera > math.MaxInt32-rule.Count {
				return dnfcerashop.VoucherCounts{}, fmt.Errorf("%w: happy token cera voucher overflow", ErrQuestRewardMalformed)
			}
			counts.HappyTokenCera += rule.Count
		default:
			return dnfcerashop.VoucherCounts{}, fmt.Errorf("%w: item %d is not a voucher", ErrQuestRewardMalformed, rule.ItemID)
		}
	}
	return counts, nil
}

func allocateFinishRewardItems(
	record *dnfrepo.InventoryRecord,
	rules []RewardItemRule,
	allocator FinishItemAllocator,
	base FinishItemGrantRequest,
) ([]FinishCommittedItem, error) {
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 || rule.Count > math.MaxUint32 {
			return nil, ErrQuestRewardMalformed
		}
	}
	if len(rules) != 0 && allocator == nil {
		return nil, ErrFinishItemAllocatorRequired
	}
	items := make([]FinishCommittedItem, 0, len(rules))
	positions := make(map[string]int, len(rules))
	for _, rule := range rules {
		request := base
		request.ItemID = rule.ItemID
		request.Count = rule.Count
		item, err := allocator(record, request)
		if err != nil {
			return nil, err
		}
		if item.ItemID != rule.ItemID || item.Delta != rule.Count || item.SlotKey == "" || item.PostCount < item.Delta || item.CountOrSeed == 0 || len(item.RawEntry) == 0 {
			return nil, fmt.Errorf("%w: invalid allocator receipt item=%d", ErrQuestRewardMalformed, rule.ItemID)
		}
		if index, exists := positions[item.SlotKey]; exists {
			previous := items[index]
			if previous.ItemID != item.ItemID || previous.SlotIndex != item.SlotIndex ||
				previous.Delta > math.MaxUint32-item.Delta ||
				previous.CountOrSeed > math.MaxUint32-item.CountOrSeed ||
				previous.PostCount > math.MaxInt64-item.Delta ||
				item.PostCount != previous.PostCount+item.Delta {
				return nil, fmt.Errorf("%w: inconsistent allocator receipt slot=%s item=%d", ErrQuestRewardMalformed, item.SlotKey, rule.ItemID)
			}
			item.Delta += previous.Delta
			item.CountOrSeed += previous.CountOrSeed
			items[index] = item
			continue
		}
		positions[item.SlotKey] = len(items)
		items = append(items, item)
	}
	return items, nil
}

// finishRequiredItemRules 汇总这次交付要消耗的**道具**。
//
// 口径（服主 2026-10-03）：[int data] 是 (ID, 数量) 成对，**ID 0 = 金币**。
// 金币**不在这里**处理 —— 它走 FinishRewardPlan.GoldCost + chargeFinishGoldCost，
// 原因有二：
//  1. 这里的规则表会被当作"背包里要有多少件"去盘点（consumeFinishRequiredItems），
//     把 0 塞进去只会得到一条 item=0 的"道具不足"，玩家看不懂；
//  2. 这个表还会经 itemPairRulesFromInts 喂给客户端的 seeking_item_ids，
//     金币混进去会让任务目标列表多出一个 id=0 的假道具需求。
//
// 但 `itemID <= 0` 这条旧过滤**确实**是"金币任务不扣钱"的根因之一：它把 (0, 金额)
// 整条丢掉了，连"有金币要求"这个事实都没留下来。现在这件事由 finishDefinitionGoldCost
// 单独承担。
func finishRequiredItemRules(catalog *Catalog, definition Definition, character CharacterEligibility) []RewardItemRule {
	_ = character
	coalesced := make(map[int64]int64, 4)
	add := func(itemID, count int64) {
		if itemID <= 0 || count <= 0 {
			return
		}
		current := coalesced[itemID]
		if count > math.MaxUint32 || current > math.MaxUint32-count {
			coalesced[itemID] = math.MaxUint32 + 1
			return
		}
		coalesced[itemID] = current + count
	}
	// 交付物的**唯一权威来源**：finishDeliveryRules。金币的统计
	// （finishDefinitionGoldCost）走同一个函数，两边的遍历顺序不可能再各自漂移。
	carryForward := finishCarryForwardEventItemIDs(catalog, definition)
	for _, rule := range finishDeliveryRules(definition) {
		if carryForward[rule.ItemID] {
			continue
		}
		add(rule.ItemID, rule.Count)
	}
	if len(coalesced) == 0 {
		return nil
	}
	itemIDs := make([]int64, 0, len(coalesced))
	for itemID := range coalesced {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(left, right int) bool { return itemIDs[left] < itemIDs[right] })
	out := make([]RewardItemRule, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		out = append(out, RewardItemRule{ItemID: itemID, Count: coalesced[itemID]})
	}
	return out
}

// finishDeliveryRules 汇总"交付要求"里的全部条目，**保留 ID 0（金币）**。
//
// 两个调用方共用它，这是刻意的：`finishRequiredItemRules`（扣道具）与
// `finishDefinitionGoldCost`（扣金币）必须看同一份交付定义 —— 早先它们各写一遍遍历，
// 服主 2026-10-03 追问"金币不在首位还能不能认"时就暴露出这个隐患：
// `[seeking]` 的交付物来自 **[depend give item]**，而金币统计当时只看了
// finishDefinitionSeekingRules，于是"金币只写在 depend give item 里"的任务会被漏掉。
//
// 不含金币过滤：**语义由类型决定**（只有 seeking 家族的 [int data] 是交付物，
// [use skill] / [clear quest by grade] 等类型的 [int data] 是条件参数，不在这里），
// 由调用方各自决定怎么处理 ID 0。
func finishDeliveryRules(definition Definition) []RewardItemRule {
	rules := append([]RewardItemRule(nil), finishDefinitionSeekingRules(definition)...)
	// [depend give item] 是接任务时发放、交任务时回收的那一节（既有语义，见
	// finishRequiredItemRules 与 give_up_reclaim.go），同样是"交付要求"。
	rules = append(rules, itemPairRulesAllowingGold(definition.DependGiveItemData)...)
	return rules
}

// finishGoldRequiredItemID 是 [int data] 里金币的"ID"（服主 2026-10-03 口径）。
const finishGoldRequiredItemID int64 = 0

// finishDefinitionGoldCost 汇总这个任务交付要的金币总量。
//
// 只看**交付要求**（finishDeliveryRules：seeking 家族的 [int data] + [depend give item]），
// 绝不碰 [reward int data]：那里的 `0 0` 是"无奖励"占位，而真实的金币奖励由
// [gold multiple] 标记 + 等级表算出来（applyFinishRewardCurrency），是另一条路径。
// 把两者混起来会凭空给一堆任务加上金币消耗。
//
// 金币的位置无意义：只要 (0, 金额) 成对出现，在首位、中间还是末位都算
// （用例 TestOwnerApplyFinishSettlementChargesGoldAnywhereInIntData）。
func finishDefinitionGoldCost(definition Definition) int64 {
	total := int64(0)
	for _, rule := range finishDeliveryRules(definition) {
		if rule.ItemID != finishGoldRequiredItemID || rule.Count <= 0 {
			continue
		}
		if total > math.MaxInt64-rule.Count {
			return math.MaxInt64
		}
		total += rule.Count
	}
	return total
}

func finishDefinitionSeekingRules(definition Definition) []RewardItemRule {
	switch normalizeQuestTag(definition.Type) {
	case "seek n meet npc":
		if len(definition.IntData) < 2 {
			return nil
		}
		return []RewardItemRule{{ItemID: definition.IntData[0], Count: definition.IntData[1]}}
	case "seeking":
		// 保留 ID 0：金币在 [int data] 里就是 0。规则表在这里只用于"数金币"，
		// 真正扣款走 chargeFinishGoldCost（见 finishRequiredItemRules 的说明）。
		return itemPairRulesAllowingGold(definition.IntData)
	default:
		return nil
	}
}

// itemPairRulesAllowingGold 与 itemPairRulesFromInts 同构，但**保留** ID 0（金币）。
// 只用于交付要求这条路径的量算；喂给客户端的列表仍然用 itemPairRulesFromInts。
func itemPairRulesAllowingGold(values []int64) []RewardItemRule {
	if len(values) < 2 {
		return nil
	}
	rules := make([]RewardItemRule, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		itemID, count := values[index], values[index+1]
		if itemID < 0 || count <= 0 {
			continue
		}
		rules = append(rules, RewardItemRule{ItemID: itemID, Count: count})
	}
	return rules
}

func itemPairRulesFromInts(values []int64) []RewardItemRule {
	if len(values) < 2 {
		return nil
	}
	rules := make([]RewardItemRule, 0, len(values)/2)
	for index := 0; index+1 < len(values); index += 2 {
		itemID, count := values[index], values[index+1]
		if itemID <= 0 || count <= 0 {
			continue
		}
		rules = append(rules, RewardItemRule{ItemID: itemID, Count: count})
	}
	return rules
}

func finishCarryForwardEventItemIDs(catalog *Catalog, definition Definition) map[int64]bool {
	carryForward := make(map[int64]bool)
	if catalog == nil || !definition.HasDependGiveItem {
		return carryForward
	}
	eventRules := itemPairRulesFromInts(definition.DependGiveItemData)
	if len(eventRules) == 0 {
		return carryForward
	}
	for _, successor := range catalog.Successors(definition.ID) {
		nextNeeds := make(map[int64]bool)
		for _, rule := range finishDefinitionSeekingRules(successor) {
			nextNeeds[rule.ItemID] = true
		}
		if len(nextNeeds) == 0 {
			continue
		}
		nextGives := make(map[int64]bool)
		for _, rule := range itemPairRulesFromInts(successor.DependGiveItemData) {
			nextGives[rule.ItemID] = true
		}
		for _, rule := range eventRules {
			if nextNeeds[rule.ItemID] && !nextGives[rule.ItemID] {
				carryForward[rule.ItemID] = true
			}
		}
	}
	return carryForward
}

func consumeFinishRequiredItems(
	record *dnfrepo.InventoryRecord,
	account *dnfrepo.AccountInventoryRecord,
	rules []RewardItemRule,
) ([]FinishConsumedItem, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if record == nil || account == nil {
		return nil, ErrFinishSettlementUnavailable
	}
	required := make(map[int64]int64, len(rules))
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 || rule.Count > math.MaxUint32 {
			return nil, ErrQuestRewardMalformed
		}
		if required[rule.ItemID] > math.MaxUint32-rule.Count {
			return nil, ErrQuestRewardMalformed
		}
		required[rule.ItemID] += rule.Count
	}
	if len(required) == 0 {
		return nil, nil
	}
	type candidate struct {
		key           string
		slot          uint16
		accountShared bool
		stack         dnfrepo.ItemStack
	}
	candidatesByItem := make(map[int64][]candidate, len(required))
	for key, stack := range record.Slots {
		slot, ok := parseFinishMainInventorySlotKey(key)
		if !ok || finishSlotIsAccountShared(slot) || stack.ItemID <= 0 {
			continue
		}
		if _, needed := required[stack.ItemID]; !needed {
			continue
		}
		candidatesByItem[stack.ItemID] = append(candidatesByItem[stack.ItemID], candidate{
			key: key, slot: slot, stack: stack,
		})
	}
	for key, stack := range account.Slots {
		slot, ok := parseFinishMainInventorySlotKey(key)
		if !ok || !finishSlotIsAccountShared(slot) || stack.ItemID <= 0 {
			continue
		}
		if _, needed := required[stack.ItemID]; !needed {
			continue
		}
		candidatesByItem[stack.ItemID] = append(candidatesByItem[stack.ItemID], candidate{
			key: key, slot: slot, accountShared: true, stack: stack,
		})
	}
	for itemID, candidates := range candidatesByItem {
		sort.Slice(candidates, func(left, right int) bool {
			return candidates[left].slot < candidates[right].slot
		})
		candidatesByItem[itemID] = candidates
	}
	for itemID, count := range required {
		held := int64(0)
		for _, candidate := range candidatesByItem[itemID] {
			held += finishRequiredItemAvailableCount(candidate.stack, candidate.accountShared)
			if held >= count {
				break
			}
		}
		if held < count {
			return nil, fmt.Errorf("%w: item=%d required=%d held=%d", ErrFinishRequiredItemsMissing, itemID, count, held)
		}
	}
	itemIDs := make([]int64, 0, len(required))
	for itemID := range required {
		itemIDs = append(itemIDs, itemID)
	}
	sort.Slice(itemIDs, func(left, right int) bool { return itemIDs[left] < itemIDs[right] })

	consumed := make([]FinishConsumedItem, 0, len(required))
	for _, itemID := range itemIDs {
		remaining := required[itemID]
		for _, candidate := range candidatesByItem[itemID] {
			if remaining <= 0 {
				break
			}
			slots := record.Slots
			if candidate.accountShared {
				slots = account.Slots
			}
			stack, exists := slots[candidate.key]
			if !exists || stack.ItemID != itemID {
				return nil, ErrFinishRequiredItemsMissing
			}
			available := finishRequiredItemAvailableCount(stack, candidate.accountShared)
			if available <= 0 {
				continue
			}
			take := available
			if take > remaining {
				take = remaining
			}
			post := available - take
			entry := FinishConsumedItem{
				SlotKey:   candidate.key,
				SlotIndex: candidate.slot,
				ItemID:    itemID,
				Delta:     take,
				PostCount: post,
			}
			if post <= 0 {
				delete(slots, candidate.key)
			} else {
				stack.Count = post
				stack = finishStackWithUpdatedCount(stack, candidate.slot, itemID, post)
				slots[candidate.key] = stack
				entry.RawEntry = append([]byte(nil), stack.RawEntry...)
			}
			consumed = append(consumed, entry)
			remaining -= take
		}
		if remaining != 0 {
			return nil, ErrFinishRequiredItemsMissing
		}
	}
	return consumed, nil
}

func finishSlotIsAccountShared(slot uint16) bool {
	return slot <= math.MaxInt16 && dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, int16(slot))
}

func finishConsumedAccountSharedItems(items []FinishConsumedItem) bool {
	for _, item := range items {
		if finishSlotIsAccountShared(item.SlotIndex) {
			return true
		}
	}
	return false
}

// finishGrantedAccountSharedItems reports whether any granted reward landed in an
// account-shared warehouse cell (list 0 slot 354..365) instead of the backpack.
// Such a grant mutates the account record, so the caller must save it inside the
// same settlement transaction -- mirroring finishConsumedAccountSharedItems.
func finishGrantedAccountSharedItems(items []FinishCommittedItem) bool {
	for _, item := range items {
		if finishSlotIsAccountShared(item.SlotIndex) {
			return true
		}
	}
	return false
}

func parseFinishMainInventorySlotKey(key string) (uint16, bool) {
	listRaw, slotRaw, ok := strings.Cut(key, ":")
	if !ok || strings.TrimSpace(listRaw) != "0" {
		return 0, false
	}
	slot, err := strconv.ParseUint(strings.TrimSpace(slotRaw), 10, 16)
	if err != nil || slot > math.MaxInt16 {
		return 0, false
	}
	return uint16(slot), true
}

func finishStackAvailableCount(stack dnfrepo.ItemStack) int64 {
	if stack.Count > 0 {
		return stack.Count
	}
	return 1
}

func finishRequiredItemAvailableCount(stack dnfrepo.ItemStack, accountShared bool) int64 {
	if accountShared && stack.Count <= 0 {
		return 0
	}
	return finishStackAvailableCount(stack)
}

func finishStackWithUpdatedCount(stack dnfrepo.ItemStack, slot uint16, itemID, count int64) dnfrepo.ItemStack {
	if stack.Extra != nil {
		for _, key := range []string{"amount_or_count", "amount", "count", "stack", "quantity"} {
			if _, exists := stack.Extra[key]; exists {
				stack.Extra[key] = strconv.FormatInt(count, 10)
			}
		}
	}
	if len(stack.RawEntry) == currentQuestFinishRawEntrySize && itemID > 0 && itemID <= math.MaxUint32 && count > 0 && count <= math.MaxUint32 {
		raw := append([]byte(nil), stack.RawEntry...)
		binary.LittleEndian.PutUint16(raw[0:2], slot)
		binary.LittleEndian.PutUint32(raw[2:6], uint32(itemID))
		binary.LittleEndian.PutUint32(raw[6:10], uint32(count))
		stack.RawEntry = raw
	}
	return stack
}

const currentQuestFinishRawEntrySize = 0x77

func finishResultFromReceipt(
	ctx context.Context,
	input FinishCommitInput,
	hellPartyPermissions dnfrepo.HellPartyPermissionRepository,
	state dnfrepo.QuestState,
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	inventory dnfrepo.InventoryRecord,
	accountInventory dnfrepo.AccountInventoryRecord,
	quests dnfrepo.QuestRecord,
) (FinishCommitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	raw := strings.TrimSpace(state.Extra[finishSettlementReceiptKey])
	if raw == "" {
		return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
	}
	var receipt finishSettlementReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return FinishCommitResult{}, fmt.Errorf("%w: %v", ErrFinishQuestSettlementCorrupt, err)
	}
	if receipt.Version != finishSettlementReceiptLevel || receipt.CharacterID != input.CharacterID || receipt.QuestID != input.QuestID ||
		receipt.CompletionKey == "" || receipt.CompletionKey != strings.TrimSpace(state.Extra["completion_key"]) || receipt.Source == "" {
		return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
	}
	if len(receipt.HellPartyUnlocks) != 0 {
		unlock, err := validFinishHellPartyUnlocks(receipt.HellPartyUnlocks, input.QuestID)
		if err != nil || hellPartyPermissions == nil {
			return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
		}
		permissions, found, err := hellPartyPermissions.Load(ctx, input.CharacterID)
		if err != nil {
			return FinishCommitResult{}, err
		}
		if !found || !finishHellPartyPermissionsContain(permissions, unlock) {
			return FinishCommitResult{}, ErrFinishQuestSettlementStale
		}
	}
	if input.ExpectedCompletionKey != "" && input.ExpectedCompletionKey != receipt.CompletionKey {
		return FinishCommitResult{}, ErrFinishQuestCompletionConflict
	}
	exp, ok := character.Stats["exp"]
	if !ok || character.Level != receipt.NewLevel || exp != int64(receipt.NewExperience) || skill.Points != receipt.PostSkillPoints {
		return FinishCommitResult{}, ErrFinishQuestSettlementStale
	}
	if receipt.HasProfession && character.Stats["grow_type"] != int64(receipt.Profession.NewGrowType) {
		return FinishCommitResult{}, ErrFinishQuestSettlementStale
	}
	if !validFinishProfessionGrants(receipt.ProfessionGrants, receipt.HasProfession) {
		return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
	}
	if receipt.HasExpertJob && character.Stats["expert_job_type"] != int64(receipt.ExpertJobType) {
		return FinishCommitResult{}, ErrFinishQuestSettlementStale
	}
	if receipt.HasSlotExpansion {
		expectedBit, validIndex := ExEquipSlotBitForPVFIndex(receipt.SlotExpansionIndex)
		if !validIndex ||
			receipt.SlotExpansionBit != expectedBit ||
			character.Stats["ex_equip_slot_stat"]&int64(receipt.SlotExpansionBit) == 0 {
			return FinishCommitResult{}, ErrFinishQuestSettlementStale
		}
	}
	for _, item := range receipt.Items {
		// 奖励行的家可能在账号共享仓（无色/灵魂落 354..365 固定格），
		// 与下面 ConsumedItems 的判定同一口径：先看槽号属于哪一侧，再去那一侧找。
		// 少了这个分支，重放会把已经落库的奖励判成 stale，
		// 于是重连后的重放 ACK 永远发不出去。
		slots := inventory.Slots
		if finishSlotIsAccountShared(item.SlotIndex) {
			slots = accountInventory.Slots
		}
		stack, exists := slots[item.SlotKey]
		if !exists || stack.ItemID != item.ItemID || stack.Count != item.PostCount || !bytes.Equal(stack.RawEntry, item.RawEntry) {
			return FinishCommitResult{}, ErrFinishQuestSettlementStale
		}
	}
	for _, item := range receipt.ConsumedItems {
		if item.SlotKey == "" || item.SlotIndex > math.MaxInt16 || item.ItemID <= 0 || item.Delta <= 0 || item.Delta > math.MaxUint32 || item.PostCount < 0 {
			return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
		}
		slots := inventory.Slots
		if finishSlotIsAccountShared(item.SlotIndex) {
			slots = accountInventory.Slots
		}
		stack, exists := slots[item.SlotKey]
		if item.PostCount == 0 {
			if exists && stack.ItemID == item.ItemID && stack.Count > 0 {
				return FinishCommitResult{}, ErrFinishQuestSettlementStale
			}
			continue
		}
		if !exists || stack.ItemID != item.ItemID || stack.Count != item.PostCount || !bytes.Equal(stack.RawEntry, item.RawEntry) {
			return FinishCommitResult{}, ErrFinishQuestSettlementStale
		}
	}
	for _, currency := range receipt.Currency {
		switch strings.ToLower(strings.TrimSpace(currency.Name)) {
		case "gold":
			// 金币条目的 Delta **可以是负数**：交付金币（PVF [int data] 的 ID 0，
			// 服主口径 2026-10-03）走的就是这一条，例如耳环槽位任务扣 100 万。
			// 因此判据不能是"Delta < 0 即损坏"，而要看它自洽且没把钱包扣成负数：
			//   PostValue >= 0                       —— 钱包不能为负；
			//   |Delta| <= PostValue                 —— 扣款额不可能超过扣完的余额；
			//   PostValue == 落库后的金币            —— 防陈旧（重放/并发改钱）。
			if currency.PostValue < 0 {
				return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
			}
			delta := currency.Delta
			if delta < 0 {
				delta = -delta
			}
			if delta > currency.PostValue {
				return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
			}
			if character.Stats["gold"] != currency.PostValue {
				return FinishCommitResult{}, ErrFinishQuestSettlementStale
			}
		default:
			return FinishCommitResult{}, ErrFinishQuestSettlementCorrupt
		}
	}
	return finishResultFromCommitted(receipt, true, character, skill, inventory, accountInventory, quests), nil
}

func validFinishHellPartyUnlocks(unlocks []HellPartyUnlock, questID int64) ([]HellPartyUnlock, error) {
	if questID <= 0 || len(unlocks) == 0 {
		return nil, ErrFinishSettlementUnavailable
	}
	seen := make(map[int64]struct{}, len(unlocks))
	result := make([]HellPartyUnlock, 0, len(unlocks))
	for _, unlock := range unlocks {
		if unlock.AreaID <= 0 || unlock.QuestID != questID {
			return nil, ErrFinishSettlementUnavailable
		}
		if _, exists := seen[unlock.AreaID]; exists {
			return nil, ErrFinishSettlementUnavailable
		}
		seen[unlock.AreaID] = struct{}{}
		result = append(result, unlock)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].AreaID < result[j].AreaID })
	return result, nil
}

func cloneFinishHellPartyUnlocks(unlocks []HellPartyUnlock) []HellPartyUnlock {
	return append([]HellPartyUnlock(nil), unlocks...)
}

func finishHellPartyPermissionsContain(record dnfrepo.HellPartyPermissionRecord, unlocks []HellPartyUnlock) bool {
	if len(unlocks) == 0 {
		return true
	}
	byArea := make(map[int64]int64, len(record.Entries))
	for _, entry := range record.Entries {
		if entry.AreaID > 0 && entry.QuestID > 0 {
			byArea[entry.AreaID] = entry.QuestID
		}
	}
	for _, unlock := range unlocks {
		if _, ok := byArea[unlock.AreaID]; !ok {
			return false
		}
	}
	return true
}

// chargeFinishGoldCost 扣掉这次交付要求的金币（非纯金币任务 = 0，直接跳过）。
//
// 口径（服主 2026-10-03）：PVF [int data] 里 **ID 0 就是金币**，例如开耳环槽位任务的
// `0 1000000 3034 1000 ...` 要求 100 万金币。此前 (0, 金额) 被当成非法 itemID 整条丢掉，
// 全库 68 个任务的交付金币从来没扣过。
//
// 位置在金币**奖励**之前：先交钱再领奖。金币不够就整笔拒绝（ErrFinishRequiredItemsMissing），
// 复用既有的 op34 失败语义 —— 任务不会完成、材料不会被吃掉。
func chargeFinishGoldCost(
	character *dnfrepo.CharacterRecord,
	reward FinishRewardPlan,
) ([]FinishCommittedCurrency, error) {
	if character == nil || reward.GoldCost <= 0 {
		return nil, nil
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 1)
	}
	before := character.Stats["gold"]
	if before < reward.GoldCost {
		return nil, fmt.Errorf("%w: gold required=%d held=%d quest=%d",
			ErrFinishRequiredItemsMissing, reward.GoldCost, before, reward.QuestID)
	}
	after := before - reward.GoldCost
	character.Stats["gold"] = after
	// Delta 为负：收据里这是"扣"，而 applyFinishRewardCurrency 那条是"发"。
	// finishSettlementReceipt.HasCommitted 的一致性检查读的是**最后一条** gold 的
	// PostValue，所以扣款条目必须排在奖励条目之前 —— 见调用点。
	return []FinishCommittedCurrency{{
		Name:      "gold",
		Delta:     -reward.GoldCost,
		PostValue: after,
	}}, nil
}

func applyFinishRewardCurrency(
	character *dnfrepo.CharacterRecord,
	tables *progression.Tables,
	reward FinishRewardPlan,
) ([]FinishCommittedCurrency, error) {
	if character == nil || !reward.HasGoldReward {
		return nil, nil
	}
	goldDelta, err := tables.QuestGold(character.Level, reward.QuestLevel, reward.GoldMultiple, reward.IgnoreLevel4Exp)
	if err != nil {
		return nil, err
	}
	if goldDelta == 0 {
		return nil, nil
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 1)
	}
	before := character.Stats["gold"]
	delta := int64(goldDelta)
	if before < 0 || before > (1<<63-1)-delta {
		return nil, ErrQuestRewardMalformed
	}
	// The carry cap the character has bought bites on the quest faucet too. A
	// quest reward cannot be handed back once the quest is finished, so the
	// excess is discarded and the committed delta reports what was actually
	// granted.
	after, discarded := carrygold.Credit(*character, delta)
	character.Stats["gold"] = after
	return []FinishCommittedCurrency{{
		Name:      "gold",
		Delta:     delta - discarded,
		PostValue: after,
	}}, nil
}

func finishResultFromCommitted(
	receipt finishSettlementReceipt,
	replayed bool,
	character dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	inventory dnfrepo.InventoryRecord,
	accountInventory dnfrepo.AccountInventoryRecord,
	quests dnfrepo.QuestRecord,
) FinishCommitResult {
	return FinishCommitResult{
		CharacterID: receipt.CharacterID, CompletionKey: receipt.CompletionKey, Source: receipt.Source,
		QuestID: receipt.QuestID, Replayed: replayed,
		BaseExperienceDelta: receipt.BaseExperienceDelta, ExperienceBonusDelta: receipt.ExperienceBonusDelta,
		ContinuityExperienceDelta: receipt.ContinuityExperienceDelta,
		ExperienceDelta:           receipt.ExperienceDelta,
		PreviousLevel:             receipt.PreviousLevel, NewLevel: receipt.NewLevel,
		PreviousExperience: receipt.PreviousExperience, NewExperience: receipt.NewExperience,
		SPDelta: receipt.SPDelta, TPDelta: receipt.TPDelta,
		Items: cloneFinishItems(receipt.Items), ConsumedItems: cloneFinishConsumedItems(receipt.ConsumedItems),
		Currency: append([]FinishCommittedCurrency(nil), receipt.Currency...),
		// 券的入账数量从收据原样带出：重放路径必须报告与首次结算完全相同的事实。
		VoucherCredits: receipt.VoucherCredits,
		Profession:     receipt.Profession, ProfessionGrants: cloneProfessionGrants(receipt.ProfessionGrants),
		HasProfession: receipt.HasProfession,
		ExpertJobType: receipt.ExpertJobType, HasExpertJob: receipt.HasExpertJob,
		HasSlotExpansion: receipt.HasSlotExpansion, SlotExpansionIndex: receipt.SlotExpansionIndex,
		SlotExpansionBit:    receipt.SlotExpansionBit,
		PostCommitCharacter: dnfrepo.CloneCharacter(character), PostCommitSkill: dnfrepo.CloneSkill(skill),
		PostCommitInventory:        dnfrepo.CloneInventory(inventory),
		PostCommitAccountInventory: dnfrepo.CloneAccountInventory(accountInventory),
		PostCommitQuest:            dnfrepo.CloneQuest(quests),
	}
}

func finishProfessionGrants(before dnfrepo.SkillRecord, after dnfrepo.SkillRecord, enabled bool) ([]profession.Grant, error) {
	if !enabled {
		return nil, nil
	}
	grants := make([]profession.Grant, 0)
	for rawSkillID, state := range after.Skills {
		previous := before.Skills[rawSkillID]
		if !state.Enabled || (previous.Enabled && previous.Level >= state.Level) {
			continue
		}
		if rawSkillID <= 0 || rawSkillID > math.MaxUint16 || state.Level <= 0 || state.Level > math.MaxUint8 {
			return nil, fmt.Errorf("%w: profession skill=%d level=%d", ErrQuestRewardMalformed, rawSkillID, state.Level)
		}
		grants = append(grants, profession.Grant{SkillID: uint16(rawSkillID), Level: state.Level})
	}
	sort.Slice(grants, func(i, j int) bool {
		return grants[i].SkillID < grants[j].SkillID
	})
	return grants, nil
}

func validFinishProfessionGrants(grants []profession.Grant, hasProfession bool) bool {
	if !hasProfession {
		return len(grants) == 0
	}
	var previous uint16
	for index, grant := range grants {
		if grant.SkillID == 0 || grant.Level <= 0 || grant.Level > math.MaxUint8 ||
			(index != 0 && grant.SkillID <= previous) {
			return false
		}
		previous = grant.SkillID
	}
	return len(grants) <= math.MaxUint8
}

func cloneProfessionGrants(grants []profession.Grant) []profession.Grant {
	if len(grants) == 0 {
		return nil
	}
	return append([]profession.Grant(nil), grants...)
}

func cloneFinishItems(items []FinishCommittedItem) []FinishCommittedItem {
	if len(items) == 0 {
		return nil
	}
	out := append([]FinishCommittedItem(nil), items...)
	for index := range out {
		out[index].RawEntry = append([]byte(nil), out[index].RawEntry...)
	}
	return out
}

func cloneFinishConsumedItems(items []FinishConsumedItem) []FinishConsumedItem {
	if len(items) == 0 {
		return nil
	}
	out := append([]FinishConsumedItem(nil), items...)
	for index := range out {
		out[index].RawEntry = append([]byte(nil), out[index].RawEntry...)
	}
	return out
}

func cloneFinishCommitResult(result FinishCommitResult) FinishCommitResult {
	result.Items = cloneFinishItems(result.Items)
	result.ConsumedItems = cloneFinishConsumedItems(result.ConsumedItems)
	result.Currency = append([]FinishCommittedCurrency(nil), result.Currency...)
	if result.VoucherWallet != nil {
		wallet := *result.VoucherWallet
		result.VoucherWallet = &wallet
	}
	result.PostCommitCharacter = dnfrepo.CloneCharacter(result.PostCommitCharacter)
	result.PostCommitSkill = dnfrepo.CloneSkill(result.PostCommitSkill)
	result.PostCommitInventory = dnfrepo.CloneInventory(result.PostCommitInventory)
	result.PostCommitAccountInventory = dnfrepo.CloneAccountInventory(result.PostCommitAccountInventory)
	result.PostCommitQuest = dnfrepo.CloneQuest(result.PostCommitQuest)
	return result
}
