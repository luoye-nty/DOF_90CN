package skillcmd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var (
	ErrOwnerUnavailable              = errors.New("skill owner unavailable")
	ErrCharacterRequired             = errors.New("selected character id required")
	ErrCharacterNotFound             = errors.New("character record not found")
	ErrCharacterOwner                = errors.New("selected character does not belong to account")
	ErrSkillRulesMissing             = errors.New("skill PVF rules are unavailable")
	ErrSkillNotFound                 = errors.New("job-scoped skill is not present in PVF")
	ErrSkillDelta                    = errors.New("skill level delta is invalid")
	ErrSkillLevel                    = errors.New("skill level rule rejected mutation")
	ErrSkillGrowType                 = errors.New("skill grow type rule rejected mutation")
	ErrSkillPrerequisite             = errors.New("skill prerequisite rule rejected mutation")
	ErrSkillPointLedger              = errors.New("skill point ledger is invalid or stale")
	ErrSkillPoints                   = errors.New("insufficient skill points")
	ErrSkillTree                     = errors.New("skill tree is not proven by current EXE evidence")
	ErrSkillTreeUnavailable          = errors.New("alternate skill tree is unavailable")
	ErrSkillSlot                     = errors.New("skill UI slot allocation failed")
	ErrSkillSlotContext              = errors.New("skill slot context is outside the current EXE writer range")
	ErrSkillSlotMode                 = errors.New("skill slot mode is outside the current EXE writer values")
	ErrSkillRefundUnavailable        = errors.New("skill refund requires the settlement transaction boundary")
	ErrSkillRefundConsumableRequired = errors.New("skill refund requires one 遗忘河之水 (item 3) in the main inventory")
	ErrSkillRefundContractRequired   = errors.New("contract-backed skill refund requires an active 遗忘河水契约")
	ErrSkillCommandEmpty             = errors.New("skill command request has no records")
	ErrSkillCommandUnknown           = errors.New("skill command targets an unknown skill")
)

const (
	currentEXEProvenBuySkillTree      byte = 0
	currentEXESingleSkillMode         byte = 0
	currentEXELetheWaterMode          byte = 1
	currentEXELetheContractMode       byte = 3
	currentEXESkillSlotCount               = 204
	currentEXEReservedSlotStart            = 138
	currentEXEReservedSlotEnd              = 150
	currentEXEPrimaryQuickSlotEnd          = 6
	currentEXEExtensionQuickSlotStart      = 198
	retiredExtendedQuickSlotStart          = 204
	retiredExtendedQuickSlotEnd            = 206
	currentEXESkillTreeCount               = 2
	currentEXESkillTreeIndexStat           = "skill_tree_index"
	// forgetRiverWaterItemID is the 遗忘河之水 skill-reset consumable
	// (stackable/cash/river_lethe.stk, 86JP ForgetRiverWaterItemTemplateId).
	forgetRiverWaterItemID int64 = 3
)

type Owner struct {
	characters dnfrepo.CharacterRepository
	skills     dnfrepo.SkillRepository
	skillTx    dnfrepo.CharacterSkillUnitOfWork
	progressTx dnfrepo.CharacterProgressionUnitOfWork
	accounts   dnfrepo.AccountRepository
	settlement dnfrepo.CharacterSettlementUnitOfWork
	catalog    *dnfskill.Table
	initial    map[uint16]int
	baseline   *dnfrepo.SkillPointState
}

type OwnerOptions struct {
	Catalog       *dnfskill.Table
	InitialLevels map[uint16]int
	PointBaseline *dnfrepo.SkillPointState
}

type PlanResult struct {
	AccountID         string
	CharacterID       string
	Operation         string
	Known             bool
	SkillCount        int
	CooldownCount     int
	RequestedSkillIDs []int64
	RefundCount       int
}

type MutationEntry struct {
	SkillID     uint16
	Level       int
	TP          bool
	Slot        int
	CommandData []byte
}

type MutationResult struct {
	CharacterID string
	SkillTree   byte
	FinalMode   byte
	Points      dnfrepo.SkillPointState
	Entries     []MutationEntry
	// ConsumedRefundItem marks that the batch unlearned skills and consumed
	// one 遗忘河之水 from ConsumedRefundItemSlot (main list) in the same
	// transaction. False when the 遗忘河水契约 made the refund free.
	ConsumedRefundItem     bool
	ConsumedRefundItemSlot int16
	// ExpiredContractSkillsReset marks that the 达人契约 expiry sweep reset
	// over-level skills (and broken dependents) before this batch; the client
	// needs a full skill refresh to see the removals.
	ExpiredContractSkillsReset bool
}

type SlotMutationResult struct {
	CharacterID string
	SkillTree   byte
	From        byte
	To          byte
	FromSkillID uint16
	ToSkillID   uint16
	ToOccupied  bool
}

type ResetMutationResult struct {
	CharacterID string
	SkillTree   byte
	Mode        byte
	SkillCount  int
	Points      dnfrepo.SkillPointState
}

type TreeSwitchMutationResult struct {
	CharacterID string
	Current     byte
	Target      byte
}

type CommandMutationResult struct {
	CharacterID string
	RecordCount int
}

func NewOwner(repos dnfrepo.Group, options ...OwnerOptions) (*Owner, error) {
	if repos.Character == nil || repos.Skill == nil {
		return nil, ErrOwnerUnavailable
	}
	owner := &Owner{
		characters: repos.Character,
		skills:     repos.Skill,
		skillTx:    repos.CharacterSkills,
		progressTx: repos.CharacterProgression,
		accounts:   repos.Account,
		settlement: repos.CharacterSettlement,
	}
	if len(options) > 0 {
		owner.catalog = options[0].Catalog
		owner.initial = cloneInitialLevels(options[0].InitialLevels)
		owner.baseline = clonePointBaseline(options[0].PointBaseline)
	}
	return owner, nil
}

// ApplyTreeSwitch treats the live request's current tree as authoritative and
// commits the opposite index before the op260 success response is exposed.
// CharacterProgression is used so this write serializes with skill/SP/TP
// mutations for the same character.
func (o *Owner) ApplyTreeSwitch(ctx context.Context, cmd Command) (TreeSwitchMutationResult, error) {
	if o == nil || o.characters == nil || o.skills == nil || o.progressTx == nil {
		return TreeSwitchMutationResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return TreeSwitchMutationResult{}, ErrCharacterRequired
	}
	if cmd.SkillTree >= currentEXESkillTreeCount {
		return TreeSwitchMutationResult{}, fmt.Errorf("%w: request current=%d", ErrSkillTree, cmd.SkillTree)
	}

	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	result := TreeSwitchMutationResult{CharacterID: characterID}
	err := o.progressTx.WithinCharacterProgression(ctx, characterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.SkillRepository) error {
		character, ok, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrCharacterNotFound
		}
		if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
			return ErrCharacterOwner
		}

		current, owned := character.Stats[currentEXESkillTreeIndexStat]
		if !owned || current < 0 {
			return ErrSkillTreeUnavailable
		}
		if current >= currentEXESkillTreeCount {
			return fmt.Errorf("%w: persisted current=%d", ErrSkillTree, current)
		}
		target := cmd.SkillTree ^ 1
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		character.Stats[currentEXESkillTreeIndexStat] = int64(target)
		character.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		result.Current = cmd.SkillTree
		result.Target = target
		return nil
	})
	if err != nil {
		return TreeSwitchMutationResult{}, err
	}
	return result, nil
}

// premiumActive reports whether the account currently holds an active premium
// contract of the given type. It is a read-only gate: the worst case of a
// stale read is one purchase against a just-expired contract, matching the
// 86JP lazy-expiry model. A missing account repository (tests) means inactive.
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

// sweepExpiredOverSkill resets skills that could only have been learned while
// the 达人契约 was active: with the contract inactive, every learned skill
// whose PVF required level exceeds the raw character level is returned to its
// PVF initial floor with a full SP/TP refund of the levels above the floor.
// Dependents whose prerequisites were broken by a removal are cascaded out
// the same way. It returns true when any skill changed so the handler can
// schedule a full skill refresh.
func (o *Owner) sweepExpiredOverSkill(ctx context.Context, cmd Command, character dnfrepo.CharacterRecord, job byte, tree int) (bool, error) {
	if o == nil || o.skillTx == nil || o.catalog == nil {
		return false, nil
	}
	if o.premiumActive(ctx, character.AccountID, premium.TypeOverSkill) {
		return false, nil
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	characterLevel := int(character.Level)
	swept := false
	err := o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
		record, exists, err := repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		record = dnfrepo.CloneSkill(record)
		state, found := dnfrepo.SkillTree(record, tree)
		if !found || len(state.Skills) == 0 {
			return nil
		}
		layout := record.Layouts[tree]
		changed := false
		// Removing one skill can break another's prerequisite, so rescan until
		// nothing is over-level and nothing is broken.
		for {
			offender := int64(-1)
			for rawID, skillState := range state.Skills {
				if skillState.Level <= 0 || rawID < 0 || rawID > 0xffff {
					continue
				}
				// PVF initial levels are permanent profession grants. They may
				// legitimately sit above the character's current learn level (for
				// example immediately after a class change), but the expired
				// contract sweep is only allowed to remove purchased levels above
				// that floor. Selecting an already-floor skill here used to rescan
				// the same no-op offender forever and left op29 without an ACK.
				if skillState.Level <= o.initialSkillFloor(uint16(rawID)) {
					continue
				}
				definition, ok := o.catalog.Find(job, uint16(rawID))
				if !ok {
					continue
				}
				if int(definition.RequiredLevel) > characterLevel || skillPrerequisitesBroken(state.Skills, definition) {
					offender = rawID
					break
				}
			}
			if offender < 0 {
				break
			}
			mutated, err := o.unlearnSkillToFloor(&state, layout, job, uint16(offender))
			if err != nil {
				return err
			}
			if !mutated {
				// Defensive no-progress guard. The scan above should make this
				// unreachable, but a future catalog/floor rule must never be able
				// to turn a client request into an unbounded server loop again.
				break
			}
			changed = true
		}
		if !changed {
			return nil
		}
		if state.Points.RemainingSP > state.Points.TotalSP {
			state.Points.RemainingSP = state.Points.TotalSP
		}
		if state.Points.RemainingTP > state.Points.TotalTP {
			state.Points.RemainingTP = state.Points.TotalTP
		}
		dnfrepo.SetSkillTree(&record, tree, state)
		record.Layouts[tree] = layout
		record.UpdatedAt = time.Now().UTC()
		fields := append(skillTreeStateFields(tree), dnfrepo.SkillFieldLayouts)
		if err := dnfrepo.SaveSkillFields(ctx, repo, record, fields...); err != nil {
			return err
		}
		swept = true
		return nil
	})
	return swept, err
}

func skillPrerequisitesBroken(states map[int64]dnfrepo.SkillState, definition dnfskill.Skill) bool {
	for _, prerequisite := range definition.Prerequisites {
		if states[int64(prerequisite.SkillID)].Level < prerequisite.Level {
			return true
		}
	}
	return false
}

// unlearnSkillToFloor refunds one skill down to its PVF initial level (or
// fully unlearns it when the floor is zero) and releases its quickbar slot.
func (o *Owner) initialSkillFloor(skillID uint16) int {
	if o != nil && o.initial != nil && o.initial[skillID] > 0 {
		return o.initial[skillID]
	}
	return 0
}

func (o *Owner) unlearnSkillToFloor(state *dnfrepo.SkillTreeState, layout dnfrepo.SkillLayout, job byte, skillID uint16) (bool, error) {
	definition, ok := o.catalog.Find(job, skillID)
	if !ok {
		return false, nil
	}
	skillState := state.Skills[int64(skillID)]
	floor := o.initialSkillFloor(skillID)
	if skillState.Level <= floor {
		return false, nil
	}
	pointDelta, err := mutationPointDelta(definition, skillState.Level, floor)
	if err != nil {
		return false, err
	}
	if definition.IsTPSkill() {
		state.Points.RemainingTP -= pointDelta
	} else {
		state.Points.RemainingSP -= pointDelta
	}
	if floor == 0 {
		delete(state.Skills, int64(skillID))
		for slot, layoutID := range layout {
			if layoutID == skillID {
				delete(layout, slot)
			}
		}
		return true, nil
	}
	state.Skills[int64(skillID)] = dnfrepo.SkillState{Level: floor, Enabled: true}
	return true, nil
}

func (o *Owner) Plan(ctx context.Context, cmd Command) (PlanResult, error) {
	if o == nil || o.characters == nil || o.skills == nil {
		return PlanResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return PlanResult{}, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return PlanResult{}, err
	}
	if !ok {
		return PlanResult{}, ErrCharacterNotFound
	}
	character = dnfrepo.CloneCharacter(character)
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return PlanResult{}, ErrCharacterOwner
	}
	record, ok, err := o.skills.Load(ctx, characterID)
	if err != nil {
		return PlanResult{}, err
	}
	record = dnfrepo.CloneSkill(record)
	return PlanResult{
		AccountID:         character.AccountID,
		CharacterID:       characterID,
		Operation:         cmd.Operation,
		Known:             ok,
		SkillCount:        len(record.Skills),
		CooldownCount:     len(record.Cooldowns),
		RequestedSkillIDs: append([]int64(nil), cmd.SkillIDs...),
		RefundCount:       cmd.RefundCount,
	}, nil
}

// ApplyCommands persists only the user-defined release-command bytes. It does
// not change learned levels, SP/TP, quick slots, or cooldown state.
func (o *Owner) ApplyCommands(ctx context.Context, cmd Command) (CommandMutationResult, error) {
	if o == nil || o.characters == nil || o.skills == nil || o.skillTx == nil {
		return CommandMutationResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return CommandMutationResult{}, ErrCharacterRequired
	}
	if len(cmd.CommandRecords) == 0 {
		return CommandMutationResult{}, ErrSkillCommandEmpty
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return CommandMutationResult{}, err
	}
	if !found {
		return CommandMutationResult{}, ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return CommandMutationResult{}, ErrCharacterOwner
	}
	result := CommandMutationResult{CharacterID: characterID, RecordCount: len(cmd.CommandRecords)}
	err = o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
		record, exists, err := repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrSkillCommandUnknown
		}
		record = dnfrepo.CloneSkill(record)
		if record.Commands == nil {
			record.Commands = make(map[int64][]byte, len(cmd.CommandRecords))
		}
		for _, command := range cmd.CommandRecords {
			// op331 starts with a record count, not a skill-tree selector. The
			// durable command field and class0/op19 projection are keyed by skill
			// ID, so validate ownership across every learned page.
			if !learnedCommandSkill(record, int64(command.SkillID)) {
				return fmt.Errorf("%w: skill=%d", ErrSkillCommandUnknown, command.SkillID)
			}
			if len(command.CommandBytes) == 0 {
				delete(record.Commands, int64(command.SkillID))
				continue
			}
			record.Commands[int64(command.SkillID)] = append([]byte(nil), command.CommandBytes...)
		}
		record.UpdatedAt = time.Now().UTC()
		return dnfrepo.SaveSkillFields(ctx, repo, record, dnfrepo.SkillFieldCommands)
	})
	if err != nil {
		return CommandMutationResult{}, err
	}
	return result, nil
}

func learnedCommandSkill(record dnfrepo.SkillRecord, skillID int64) bool {
	if state, known := record.Skills[skillID]; known && state.Enabled && state.Level > 0 {
		return true
	}
	for _, tree := range record.Trees {
		if state, known := tree.Skills[skillID]; known && state.Enabled && state.Level > 0 {
			return true
		}
	}
	return false
}

// ApplyAllCommandsDefault removes only release-command customizations for the
// selected character. Learned skills and their layouts remain unchanged.
func (o *Owner) ApplyAllCommandsDefault(ctx context.Context, cmd Command) (CommandMutationResult, error) {
	if o == nil || o.characters == nil || o.skills == nil || o.skillTx == nil {
		return CommandMutationResult{}, ErrOwnerUnavailable
	}
	if cmd.SelectedCharacterID == 0 {
		return CommandMutationResult{}, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, found, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return CommandMutationResult{}, err
	}
	if !found {
		return CommandMutationResult{}, ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return CommandMutationResult{}, ErrCharacterOwner
	}
	result := CommandMutationResult{CharacterID: characterID}
	err = o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
		record, exists, err := repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		record = dnfrepo.CloneSkill(record)
		record.Commands = nil
		record.UpdatedAt = time.Now().UTC()
		return dnfrepo.SaveSkillFields(ctx, repo, record, dnfrepo.SkillFieldCommands)
	})
	if err != nil {
		return CommandMutationResult{}, err
	}
	return result, nil
}

func (o *Owner) ApplyBuy(ctx context.Context, cmd Command) (MutationResult, error) {
	if o == nil {
		return MutationResult{}, ErrOwnerUnavailable
	}
	if err := validateBuySkillTree(cmd.SkillTree); err != nil {
		return MutationResult{}, err
	}
	if o.characters == nil || o.skills == nil || o.skillTx == nil {
		return MutationResult{}, ErrOwnerUnavailable
	}
	if o.catalog == nil {
		return MutationResult{}, ErrSkillRulesMissing
	}
	if o.initial == nil {
		return MutationResult{}, ErrSkillRulesMissing
	}
	if cmd.SelectedCharacterID == 0 {
		return MutationResult{}, ErrCharacterRequired
	}
	if len(cmd.BuyEntries) == 0 {
		return MutationResult{}, ErrSkillDelta
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return MutationResult{}, err
	}
	if !ok {
		return MutationResult{}, ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return MutationResult{}, ErrCharacterOwner
	}
	jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
	if err != nil {
		return MutationResult{}, fmt.Errorf("%w: job=%q", ErrSkillRulesMissing, character.Job)
	}
	job := byte(jobValue)
	growType := int(character.Stats["grow_type"])
	// 达人契约 (premium type 27): skills can be learned five character levels
	// early. The PVF required-level rule applies to the effective level.
	effectiveLevel := int(character.Level)
	if o.premiumActive(ctx, character.AccountID, premium.TypeOverSkill) {
		effectiveLevel += 5
	}
	// When the contract is inactive, any learned skill whose PVF required
	// level exceeds the raw character level could only have been learned via
	// the contract: reset it and refund SP/TP (official expiry rule).
	tree := cmd.storageSkillTree()
	swept, err := o.sweepExpiredOverSkill(ctx, cmd, character, job, tree)
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{
		CharacterID:                characterID,
		SkillTree:                  cmd.SkillTree,
		FinalMode:                  cmd.FinalMode,
		ExpiredContractSkillsReset: swept,
	}
	letheContractActive := o.premiumActive(ctx, character.AccountID, premium.TypeLethe)
	if cmd.RefundCount > 0 && cmd.FinalMode == currentEXELetheContractMode && !cmd.DojoSkills && !letheContractActive {
		return MutationResult{}, fmt.Errorf("%w: mode=%d premium_type=%d", ErrSkillRefundContractRequired, cmd.FinalMode, premium.TypeLethe)
	}
	// Current EXE SendSingleSkillLevelMutation_Op29 emits refund_flag=1 with
	// mode 0 and performs no Lethe-Water check. Only the batch paid-reset path
	// emits mode 1; mode 3 is the contract-backed equivalent. Keep item 3
	// consumption atomic with an effective mode-1 refund.
	if cmd.RefundCount > 0 && cmd.FinalMode == currentEXELetheWaterMode && !cmd.DojoSkills && !letheContractActive {
		if o.settlement == nil {
			return MutationResult{}, ErrSkillRefundUnavailable
		}
		err = o.settlement.WithinCharacterSettlement(ctx, characterID, func(group dnfrepo.Group) error {
			if group.Skill == nil || group.Inventory == nil {
				return ErrSkillRefundUnavailable
			}
			mutation, err := o.applyBuyMutation(ctx, cmd, character, job, growType, effectiveLevel, tree, group.Skill, func() error {
				slot, err := consumeForgetRiverWater(ctx, group.Inventory, characterID)
				if err != nil {
					return err
				}
				result.ConsumedRefundItemSlot = slot
				return nil
			})
			if err != nil {
				return err
			}
			result.Points = mutation.Points
			result.Entries = mutation.Entries
			result.ConsumedRefundItem = true
			return nil
		})
	} else {
		err = o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
			mutation, err := o.applyBuyMutation(ctx, cmd, character, job, growType, effectiveLevel, tree, repo, nil)
			if err != nil {
				return err
			}
			result.Points = mutation.Points
			result.Entries = mutation.Entries
			return nil
		})
	}
	if err != nil {
		return MutationResult{}, err
	}
	return result, nil
}

// applyBuyMutation runs one validated buy/refund batch against the scoped
// skill repository. consumeRefund, when non-nil, is invoked exactly once
// after every entry validates and before the skill fields are saved, so the
// refund consumable and the skill mutation commit or roll back together.
func (o *Owner) applyBuyMutation(
	ctx context.Context,
	cmd Command,
	character dnfrepo.CharacterRecord,
	job byte,
	growType int,
	effectiveLevel int,
	tree int,
	repo dnfrepo.SkillRepository,
	consumeRefund func() error,
) (MutationResult, error) {
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	result := MutationResult{
		CharacterID: characterID,
		SkillTree:   cmd.SkillTree,
		FinalMode:   cmd.FinalMode,
	}
	record, exists, err := repo.Load(ctx, characterID)
	if err != nil {
		return MutationResult{}, err
	}
	if !exists {
		return MutationResult{}, ErrSkillPointLedger
	}
	record = dnfrepo.CloneSkill(record)
	state, found := dnfrepo.SkillTree(record, tree)
	if !found {
		return MutationResult{}, fmt.Errorf("%w: tree=%d", ErrSkillTreeUnavailable, tree)
	}
	pointTarget, err := skillPointTargetForCharacter(o.baseline, character)
	if err != nil {
		return MutationResult{}, err
	}
	points, err := synchronizePointLedger(state.Points, pointTarget, character.Level, o.catalog, job, o.initial, state.Skills)
	if err != nil {
		return MutationResult{}, err
	}
	state.Points = points
	if state.Skills == nil {
		state.Skills = make(map[int64]dnfrepo.SkillState)
	}
	if record.Layouts == nil {
		record.Layouts = make(map[int]dnfrepo.SkillLayout)
	}
	layout, err := ensureSkillLayout(o.catalog, job, tree, state.Skills, record.Layouts[tree])
	if err != nil {
		return MutationResult{}, err
	}
	record.Layouts[tree] = layout
	changed := make(map[uint16]dnfskill.Skill)
	changedSlots := make(map[uint16]int)
	hasEffectiveRefund := false
	for _, entry := range cmd.BuyEntries {
		if entry.LevelDelta == 0 {
			return MutationResult{}, fmt.Errorf("%w: skill=%d delta=0", ErrSkillDelta, entry.SkillID)
		}
		definition, ok := o.catalog.Find(job, entry.SkillID)
		if !ok {
			return MutationResult{}, fmt.Errorf("%w: job=%d skill=%d", ErrSkillNotFound, job, entry.SkillID)
		}
		if effectiveLevel < int(definition.RequiredLevel) {
			return MutationResult{}, fmt.Errorf("%w: skill=%d character=%d effective=%d required=%d", ErrSkillLevel, entry.SkillID, character.Level, effectiveLevel, definition.RequiredLevel)
		}
		skillState := state.Skills[int64(entry.SkillID)]
		currentLevel := skillState.Level
		targetLevel := currentLevel + entry.SignedDelta()
		// Only the AWAKENING dimension of the PVF fitness pair gates a purchase.
		// The class branch is the client skill tree's call: it renders every row
		// of the character's JOB, so a row the player can click must be learnable
		// (see MaximumLearnableLevelForTree). Awakening stays server-enforced -
		// the first awakening is granted by its own transition and the second one
		// requires its quest - so an unawakened character cannot buy an awakening
		// skill just because the tree draws the row.
		if !definition.SupportsCharacterAwakening(growType) {
			return MutationResult{}, fmt.Errorf("%w: skill=%d grow=%d", ErrSkillGrowType, entry.SkillID, growType)
		}
		floor := o.initial[entry.SkillID]
		learnableMaximum := definition.MaximumLearnableLevelForTree(effectiveLevel, growType)
		// Preserve compatibility with executable/helper skill rows from older
		// PVFs that declare no positive maximum at all. A positive derived cap is
		// authoritative; zero means this PVF did not provide a level-bound rule.
		if targetLevel < floor || targetLevel < 0 || (learnableMaximum > 0 && targetLevel > learnableMaximum) {
			return MutationResult{}, fmt.Errorf("%w: skill=%d current=%d target=%d floor=%d learnable_max=%d effective_level=%d", ErrSkillLevel, entry.SkillID, currentLevel, targetLevel, floor, learnableMaximum, effectiveLevel)
		}
		pointDelta, err := mutationPointDelta(definition, currentLevel, targetLevel)
		if err != nil {
			return MutationResult{}, err
		}
		if definition.IsTPSkill() {
			state.Points.RemainingTP -= pointDelta
			if state.Points.RemainingTP < 0 || state.Points.RemainingTP > state.Points.TotalTP {
				return MutationResult{}, fmt.Errorf("%w: skill=%d tp_delta=%d remaining=%d total=%d", ErrSkillPoints, entry.SkillID, pointDelta, state.Points.RemainingTP, state.Points.TotalTP)
			}
		} else {
			state.Points.RemainingSP -= pointDelta
			if state.Points.RemainingSP < 0 || state.Points.RemainingSP > state.Points.TotalSP {
				return MutationResult{}, fmt.Errorf("%w: skill=%d sp_delta=%d remaining=%d total=%d", ErrSkillPoints, entry.SkillID, pointDelta, state.Points.RemainingSP, state.Points.TotalSP)
			}
		}
		slot, hasSlot := findSkillSlot(layout, entry.SkillID)
		if currentLevel > 0 && !hasSlot {
			return MutationResult{}, fmt.Errorf("%w: learned skill=%d has no slot", ErrSkillSlot, entry.SkillID)
		}
		if currentLevel == 0 && targetLevel > 0 {
			slot = allocateSkillSlot(job, definition, tree, layout)
			if slot < 0 {
				return MutationResult{}, fmt.Errorf("%w: skill=%d group=%d", ErrSkillSlot, entry.SkillID, currentSlotGroup(definition))
			}
			layout[slot] = entry.SkillID
			hasSlot = true
		}
		if !hasSlot {
			return MutationResult{}, fmt.Errorf("%w: skill=%d target=%d", ErrSkillSlot, entry.SkillID, targetLevel)
		}
		changedSlots[entry.SkillID] = slot
		if targetLevel == 0 {
			delete(state.Skills, int64(entry.SkillID))
			delete(layout, slot)
		} else {
			state.Skills[int64(entry.SkillID)] = dnfrepo.SkillState{Level: targetLevel, Enabled: true}
		}
		if entry.RefundFlag != 0 && targetLevel < currentLevel {
			hasEffectiveRefund = true
		}
		changed[entry.SkillID] = definition
	}
	if err := validateLearnedPrerequisites(o.catalog, job, state.Skills, o.initial); err != nil {
		return MutationResult{}, err
	}
	if hasEffectiveRefund && consumeRefund != nil {
		if err := consumeRefund(); err != nil {
			return MutationResult{}, err
		}
	}
	dnfrepo.SetSkillTree(&record, tree, state)
	record.UpdatedAt = time.Now().UTC()
	fields := append(skillTreeStateFields(tree), dnfrepo.SkillFieldLayouts)
	if err := dnfrepo.SaveSkillFields(ctx, repo, record, fields...); err != nil {
		return MutationResult{}, err
	}
	result.Points = state.Points
	result.Entries = make([]MutationEntry, 0, len(changed))
	for _, entry := range cmd.BuyEntries {
		definition, first := changed[entry.SkillID]
		if !first {
			continue
		}
		delete(changed, entry.SkillID)
		result.Entries = append(result.Entries, MutationEntry{
			SkillID: entry.SkillID,
			Level:   state.Skills[int64(entry.SkillID)].Level,
			TP:      definition.IsTPSkill(),
			Slot:    changedSlots[entry.SkillID],
		})
	}
	return result, nil
}

// consumeForgetRiverWater decrements one 遗忘河之水 (item 3) stack from the
// character's main inventory list and returns the consumed slot. Missing
// water fails the enclosing transaction, so no skill refund is persisted.
func consumeForgetRiverWater(ctx context.Context, inventoryRepo dnfrepo.InventoryRepository, characterID string) (int16, error) {
	record, found, err := inventoryRepo.Load(ctx, characterID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrSkillRefundConsumableRequired
	}
	record = dnfrepo.CloneInventory(record)
	for key, stack := range record.Slots {
		if !strings.HasPrefix(key, "0:") || stack.ItemID != forgetRiverWaterItemID || stack.Count <= 0 {
			continue
		}
		slot, err := strconv.Atoi(key[2:])
		if err != nil {
			return 0, err
		}
		if stack.Count == 1 {
			delete(record.Slots, key)
		} else {
			stack.Count--
			record.Slots[key] = stack
		}
		record.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveInventoryFields(ctx, inventoryRepo, record, dnfrepo.InventoryFieldSlots); err != nil {
			return 0, err
		}
		return int16(slot), nil
	}
	return 0, ErrSkillRefundConsumableRequired
}

// ApplyReset atomically restores the current tree to the runtime PVF initial
// levels, refunds every spent SP/TP point, and rebuilds the initial quickbar.
// Cooldowns are deliberately preserved because reset does not own that field.
func (o *Owner) ApplyReset(ctx context.Context, cmd Command) (ResetMutationResult, error) {
	if o == nil || o.characters == nil || o.skills == nil || o.skillTx == nil {
		return ResetMutationResult{}, ErrOwnerUnavailable
	}
	if err := validateSkillInitCommand(cmd); err != nil {
		return ResetMutationResult{}, err
	}
	if o.catalog == nil || o.initial == nil || o.baseline == nil {
		return ResetMutationResult{}, ErrSkillRulesMissing
	}
	if cmd.SelectedCharacterID == 0 {
		return ResetMutationResult{}, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return ResetMutationResult{}, err
	}
	if !ok {
		return ResetMutationResult{}, ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return ResetMutationResult{}, ErrCharacterOwner
	}
	jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
	if err != nil {
		return ResetMutationResult{}, fmt.Errorf("%w: job=%q", ErrSkillRulesMissing, character.Job)
	}
	job := byte(jobValue)
	resetTarget, err := skillPointTargetForCharacter(o.baseline, character)
	if err != nil {
		return ResetMutationResult{}, err
	}
	points := *resetTarget
	if err := validatePointLedger(points, character.Level); err != nil {
		return ResetMutationResult{}, err
	}
	states := make(map[int64]dnfrepo.SkillState, len(o.initial))
	for skillID, level := range o.initial {
		if level < 0 {
			return ResetMutationResult{}, fmt.Errorf("%w: initial skill=%d level=%d", ErrSkillLevel, skillID, level)
		}
		if level == 0 {
			continue
		}
		if _, ok := o.catalog.Find(job, skillID); !ok {
			return ResetMutationResult{}, fmt.Errorf("%w: initial job=%d skill=%d", ErrSkillNotFound, job, skillID)
		}
		states[int64(skillID)] = dnfrepo.SkillState{Level: level, Enabled: true}
	}
	layout, err := BuildInitialSkillLayout(o.catalog, job, int(cmd.SkillTree), states)
	if err != nil {
		return ResetMutationResult{}, err
	}
	result := ResetMutationResult{
		CharacterID: characterID,
		SkillTree:   cmd.SkillTree,
		Mode:        cmd.Mode,
		SkillCount:  len(states),
		Points:      points,
	}
	err = o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
		record, exists, err := repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrSkillPointLedger
		}
		record = dnfrepo.CloneSkill(record)
		tree := cmd.storageSkillTree()
		if _, found := dnfrepo.SkillTree(record, tree); !found {
			return fmt.Errorf("%w: tree=%d", ErrSkillTreeUnavailable, tree)
		}
		dnfrepo.SetSkillTree(&record, tree, dnfrepo.SkillTreeState{Skills: states, Points: points})
		if record.Layouts == nil {
			record.Layouts = make(map[int]dnfrepo.SkillLayout)
		}
		record.Layouts[tree] = layout
		record.UpdatedAt = time.Now().UTC()
		fields := append(skillTreeStateFields(tree), dnfrepo.SkillFieldLayouts)
		return dnfrepo.SaveSkillFields(ctx, repo, record, fields...)
	})
	if err != nil {
		return ResetMutationResult{}, err
	}
	return result, nil
}

// ApplySlot atomically swaps two entries in the persisted current-EXE skill
// vector. The op28 success reader performs the same swap locally, so the ACK is
// emitted only after this transaction commits and no proactive op19 refresh is
// needed.
func (o *Owner) ApplySlot(ctx context.Context, cmd Command) (SlotMutationResult, error) {
	if o == nil || o.characters == nil || o.skills == nil || o.skillTx == nil {
		return SlotMutationResult{}, ErrOwnerUnavailable
	}
	if o.catalog == nil {
		return SlotMutationResult{}, ErrSkillRulesMissing
	}
	if err := validateSkillSlotCommand(cmd); err != nil {
		return SlotMutationResult{}, err
	}
	if cmd.SelectedCharacterID == 0 {
		return SlotMutationResult{}, ErrCharacterRequired
	}
	characterID := strconv.FormatUint(uint64(cmd.SelectedCharacterID), 10)
	character, ok, err := o.characters.Load(ctx, characterID)
	if err != nil {
		return SlotMutationResult{}, err
	}
	if !ok {
		return SlotMutationResult{}, ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(cmd.AccountID); accountID != "" && accountID != strings.TrimSpace(character.AccountID) {
		return SlotMutationResult{}, ErrCharacterOwner
	}
	jobValue, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
	if err != nil {
		return SlotMutationResult{}, fmt.Errorf("%w: job=%q", ErrSkillRulesMissing, character.Job)
	}
	job := byte(jobValue)
	if job == darkcombo.Job && (darkcombo.IsExclusiveComboSlot(int(cmd.From)) || darkcombo.IsExclusiveComboSlot(int(cmd.To))) {
		return SlotMutationResult{}, fmt.Errorf(
			"%w: dark knight ASDFGH slot from=%d to=%d is owned by the native combo manager",
			ErrSkillSlot,
			cmd.From,
			cmd.To,
		)
	}
	result := SlotMutationResult{
		CharacterID: characterID,
		SkillTree:   cmd.SkillTree,
		From:        cmd.From,
		To:          cmd.To,
	}
	err = o.skillTx.WithinCharacterSkill(ctx, characterID, func(repo dnfrepo.SkillRepository) error {
		record, exists, err := repo.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !exists {
			return ErrSkillSlot
		}
		record = dnfrepo.CloneSkill(record)
		tree := cmd.storageSkillTree()
		state, found := dnfrepo.SkillTree(record, tree)
		if !found || len(state.Skills) == 0 {
			return fmt.Errorf("%w: tree=%d", ErrSkillTreeUnavailable, tree)
		}
		var existing dnfrepo.SkillLayout
		if record.Layouts != nil {
			existing = record.Layouts[tree]
		}
		layout, err := ensureSkillLayout(o.catalog, job, tree, state.Skills, existing)
		if err != nil {
			return err
		}
		from := resolveCurrentEXESkillSlotSource(layout, cmd.From, cmd.To)
		to := int(cmd.To)
		fromSkillID, fromOccupied := layout[from]
		if !fromOccupied {
			return fmt.Errorf("%w: source slot=%d is empty", ErrSkillSlot, from)
		}
		toSkillID, toOccupied := layout[to]
		layout[to] = fromSkillID
		clientFrom := int(cmd.From)
		if from != clientFrom {
			delete(layout, from)
		}
		if toOccupied {
			layout[clientFrom] = toSkillID
		} else {
			delete(layout, clientFrom)
		}
		if err := validateQuickSlotAssignments(o.catalog, job, layout); err != nil {
			return err
		}
		if record.Layouts == nil {
			record.Layouts = make(map[int]dnfrepo.SkillLayout)
		}
		record.Layouts[tree] = layout
		record.UpdatedAt = time.Now().UTC()
		if err := dnfrepo.SaveSkillFields(ctx, repo, record, dnfrepo.SkillFieldLayouts); err != nil {
			return err
		}
		result.FromSkillID = fromSkillID
		result.ToSkillID = toSkillID
		result.ToOccupied = toOccupied
		return nil
	})
	if err != nil {
		return SlotMutationResult{}, err
	}
	return result, nil
}

// A previous mismatched ACK can leave the persisted tree entry behind the
// client's source coordinate. Locate the nearest occupied entry in the same
// tree group, then ApplySlot writes the swapped destination back to the client
// coordinate so the two vectors converge in one operation.
func resolveCurrentEXESkillSlotSource(layout dnfrepo.SkillLayout, from byte, to byte) int {
	source := int(from)
	if source <= currentEXEPrimaryQuickSlotEnd || source >= currentEXEExtensionQuickSlotStart ||
		!isCurrentEXEQuickSlot(int(to)) {
		return source
	}
	if _, occupied := layout[source]; occupied {
		return source
	}
	start := currentEXESkillTreeGroupStart(source)
	for candidate := source - 1; candidate >= start; candidate-- {
		if _, occupied := layout[candidate]; occupied {
			return candidate
		}
	}
	return source
}

func currentEXESkillTreeGroupStart(slot int) int {
	switch {
	case slot >= 150:
		return 150
	case slot >= 102:
		return 102
	case slot >= 54:
		return 54
	default:
		return currentEXEPrimaryQuickSlotEnd
	}
}

func isCurrentEXEQuickSlot(slot int) bool {
	return (slot >= 0 && slot < currentEXEPrimaryQuickSlotEnd) ||
		(slot >= currentEXEExtensionQuickSlotStart && slot < currentEXESkillSlotCount)
}

func validateSkillSlotCommand(cmd Command) error {
	if cmd.SkillTree >= currentEXESkillTreeCount {
		return fmt.Errorf("%w: tree=%d", ErrSkillTree, cmd.SkillTree)
	}
	if int(cmd.From) >= currentEXESkillSlotCount || int(cmd.To) >= currentEXESkillSlotCount {
		return fmt.Errorf("%w: from=%d to=%d count=%d", ErrSkillSlot, cmd.From, cmd.To, currentEXESkillSlotCount)
	}
	if int(cmd.To) >= currentEXEReservedSlotStart && int(cmd.To) < currentEXEReservedSlotEnd {
		return fmt.Errorf("%w: destination slot=%d is reserved", ErrSkillSlot, cmd.To)
	}
	if cmd.ContextIndex < -1 || cmd.ContextIndex > 2 {
		return fmt.Errorf("%w: context=%d", ErrSkillSlotContext, cmd.ContextIndex)
	}
	if cmd.Mode != 0 && cmd.Mode != 2 {
		return fmt.Errorf("%w: mode=%d", ErrSkillSlotMode, cmd.Mode)
	}
	return nil
}

func validateQuickSlotAssignments(catalog *dnfskill.Table, job byte, layout dnfrepo.SkillLayout) error {
	for slot, skillID := range layout {
		if slot < 0 || slot >= currentEXESkillSlotCount {
			return fmt.Errorf("%w: persisted slot=%d count=%d", ErrSkillSlot, slot, currentEXESkillSlotCount)
		}
		if !isCurrentEXEQuickSlot(slot) {
			continue
		}
		definition, ok := catalog.Find(job, skillID)
		if !ok {
			return fmt.Errorf("%w: quick slot=%d job=%d skill=%d", ErrSkillNotFound, slot, job, skillID)
		}
		if !definition.Active {
			return fmt.Errorf("%w: passive skill=%d cannot occupy quick slot=%d", ErrSkillSlot, skillID, slot)
		}
	}
	return nil
}

func validateBuySkillTree(tree byte) error {
	if tree >= currentEXESkillTreeCount {
		return fmt.Errorf("%w: tree=%d", ErrSkillTree, tree)
	}
	return nil
}

func skillTreeStateFields(tree int) []dnfrepo.SkillField {
	if tree == 0 {
		return []dnfrepo.SkillField{dnfrepo.SkillFieldSkills, dnfrepo.SkillFieldPoints}
	}
	return []dnfrepo.SkillField{dnfrepo.SkillFieldTrees}
}

func validateSkillInitCommand(cmd Command) error {
	if err := validateBuySkillTree(cmd.SkillTree); err != nil {
		return err
	}
	if cmd.Mode > 3 {
		return fmt.Errorf("%w: init mode=%d", ErrSkillSlotMode, cmd.Mode)
	}
	return nil
}

const (
	initialPrimaryQuickSlotCount = 3
)

func ensureSkillLayout(catalog *dnfskill.Table, job byte, tree int, states map[int64]dnfrepo.SkillState, existing dnfrepo.SkillLayout) (dnfrepo.SkillLayout, error) {
	layout := make(dnfrepo.SkillLayout)
	assigned := make(map[uint16]struct{})
	for slot, skillID := range existing {
		if slot >= retiredExtendedQuickSlotStart && slot < retiredExtendedQuickSlotEnd {
			// The client is back on its stock six-column HUD. Preserve skills
			// formerly assigned to the local 7+7 extension by leaving them
			// unassigned here; the allocation pass below moves them into an
			// available native quick slot or their ordinary learned-skill group.
			continue
		}
		if slot < 0 || slot >= currentEXESkillSlotCount {
			return nil, fmt.Errorf("%w: slot=%d skill=%d", ErrSkillSlot, slot, skillID)
		}
		if states[int64(skillID)].Level <= 0 {
			continue
		}
		if _, duplicate := assigned[skillID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate skill=%d", ErrSkillSlot, skillID)
		}
		if isCurrentEXEQuickSlot(slot) {
			definition, found := catalog.Find(job, skillID)
			if found && !definition.Active {
				continue
			}
		}
		layout[slot] = skillID
		assigned[skillID] = struct{}{}
	}
	ids := make([]int, 0, len(states))
	for rawID, state := range states {
		if state.Level > 0 && rawID >= 0 && rawID <= 0xffff {
			ids = append(ids, int(rawID))
		}
	}
	sort.Ints(ids)
	for _, rawID := range ids {
		skillID := uint16(rawID)
		if _, ok := assigned[skillID]; ok {
			continue
		}
		definition, ok := catalog.Find(job, skillID)
		if !ok {
			slot := allocateMissingPVFGrantSlot(job, layout)
			if slot < 0 {
				return nil, fmt.Errorf("%w: layout job=%d missing_skill=%d", ErrSkillSlot, job, skillID)
			}
			layout[slot] = skillID
			assigned[skillID] = struct{}{}
			continue
		}
		slot := allocateSkillSlot(job, definition, tree, layout)
		if slot < 0 {
			return nil, fmt.Errorf("%w: layout skill=%d group=%d", ErrSkillSlot, skillID, currentSlotGroup(definition))
		}
		layout[slot] = skillID
		assigned[skillID] = struct{}{}
	}
	if job == darkcombo.Job {
		normalized, _, err := darkcombo.NormalizeFixedComboRootSlots(catalog, states, layout)
		return normalized, err
	}
	return layout, nil
}

// BuildCurrentSkillLayout reconstructs the current EXE's skill-tree slots from
// persisted learned skills and any existing assignments. Active skills occupy
// the current tree's primary/extension quick slots; passive skills stay in the
// grouped skill-tree ranges.
func BuildCurrentSkillLayout(catalog *dnfskill.Table, job byte, tree int, states map[int64]dnfrepo.SkillState, existing dnfrepo.SkillLayout) (dnfrepo.SkillLayout, error) {
	return ensureSkillLayout(catalog, job, tree, states, existing)
}

// BuildInitialSkillLayout builds the first persisted layout for PVF starter
// skills. Only the first three active skills are placed on the primary
// quickbar; every other learned skill remains available in its tree group.
func BuildInitialSkillLayout(catalog *dnfskill.Table, job byte, _ int, states map[int64]dnfrepo.SkillState) (dnfrepo.SkillLayout, error) {
	if catalog == nil {
		return nil, fmt.Errorf("%w: initial layout job=%d", ErrSkillNotFound, job)
	}
	layout := make(dnfrepo.SkillLayout)
	ids := make([]int, 0, len(states))
	for rawID, state := range states {
		if state.Level > 0 && rawID >= 0 && rawID <= 0xffff {
			ids = append(ids, int(rawID))
		}
	}
	sort.Ints(ids)
	primaryActive := 0
	for _, rawID := range ids {
		skillID := uint16(rawID)
		definition, ok := catalog.Find(job, skillID)
		if !ok {
			slot := allocateMissingPVFGrantSlot(job, layout)
			if slot < 0 {
				return nil, fmt.Errorf("%w: initial layout job=%d missing_skill=%d", ErrSkillSlot, job, skillID)
			}
			layout[slot] = skillID
			continue
		}
		slot := -1
		if definition.Active && primaryActive < initialPrimaryQuickSlotCount {
			slot = firstFreeSkillSlot(layout, 0, initialPrimaryQuickSlotCount)
			if slot >= 0 {
				primaryActive++
			}
		}
		if slot < 0 {
			group := currentSlotGroup(definition)
			start := []int{6, 54, 102, 150}[group]
			slot = firstFreeSkillSlot(layout, start, start+48)
		}
		if slot < 0 {
			return nil, fmt.Errorf("%w: initial layout skill=%d group=%d", ErrSkillSlot, skillID, currentSlotGroup(definition))
		}
		layout[slot] = skillID
	}
	if job == darkcombo.Job {
		normalized, _, err := darkcombo.NormalizeFixedComboRootSlots(catalog, states, layout)
		return normalized, err
	}
	return layout, nil
}

func allocateSkillSlot(job byte, definition dnfskill.Skill, _ int, occupied dnfrepo.SkillLayout) int {
	// The active client uses the stock six-column HUD: primary quickbar slots
	// 0..5 and extension quickbar slots 198..203. Learned-skill groups remain
	// 6..197; retired local-extension slots 204/205 are never allocated.
	if definition.Active {
		if job != darkcombo.Job {
			if slot := firstFreeSkillSlot(occupied, 0, currentEXEPrimaryQuickSlotEnd); slot >= 0 {
				return slot
			}
		}
		if slot := firstFreeSkillSlot(occupied, currentEXEExtensionQuickSlotStart, currentEXESkillSlotCount); slot >= 0 {
			return slot
		}
	}
	group := currentSlotGroup(definition)
	start := []int{6, 54, 102, 150}[group]
	return firstFreeSkillSlot(occupied, start, start+48)
}

func currentSlotGroup(definition dnfskill.Skill) int {
	// sub_3059AC0 maps the current skill object's active marker, skill class,
	// and growtype-pair count to groups 3, 2, 1, and 0 respectively.
	if definition.Active {
		return 3
	}
	if definition.SkillClass == 4 {
		return 2
	}
	if len(definition.GrowTypes) <= 2 {
		return 1
	}
	return 0
}

func firstFreeSkillSlot(occupied dnfrepo.SkillLayout, start int, end int) int {
	for slot := start; slot < end; slot++ {
		if _, exists := occupied[slot]; !exists {
			return slot
		}
	}
	return -1
}

// Some real character/*.chr free grants intentionally have no job-scoped
// .skl row. Keep the real PVF grant visible in the first passive/group range,
// matching the reference client's missing-definition fallback, without
// inventing costs, prerequisites, or an active quick-slot classification.
func allocateMissingPVFGrantSlot(job byte, occupied dnfrepo.SkillLayout) int {
	if slot := firstFreeSkillSlot(occupied, currentEXEPrimaryQuickSlotEnd, 54); slot >= 0 {
		return slot
	}
	if job == darkcombo.Job {
		return firstFreeSkillSlot(occupied, 54, currentEXEExtensionQuickSlotStart)
	}
	return firstFreeSkillSlot(occupied, 0, currentEXESkillSlotCount)
}

func findSkillSlot(layout dnfrepo.SkillLayout, skillID uint16) (int, bool) {
	for slot, assigned := range layout {
		if assigned == skillID {
			return slot, true
		}
	}
	return 0, false
}

func validatePointLedger(points dnfrepo.SkillPointState, characterLevel int) error {
	if points.SyncedLevel != characterLevel || points.TotalSP < 0 || points.RemainingSP < 0 || points.RemainingSP > points.TotalSP || points.TotalTP < 0 || points.RemainingTP < 0 || points.RemainingTP > points.TotalTP {
		return fmt.Errorf("%w: character_level=%d ledger=%+v", ErrSkillPointLedger, characterLevel, points)
	}
	return nil
}

// skillPointTargetForCharacter is the authoritative ledger target for the skill
// plan mutations this Owner performs (op29 buy/refund and op491 reset): the
// ordinary PVF baseline handed in OwnerOptions.PointBaseline PLUS the persisted
// +SP/+TP book bonus carried on the character record.
//
// The bonus is added here rather than in the caller's baseline on purpose. The
// SP/TP book owner (inventory.useTPBook -> skillPointBookTarget) receives that
// same baseline through alignedcmd.Request.SkillPointBaseline and adds the bonus
// itself, so folding the bonus into the shared baseline made every book count
// the already-accumulated bonus a second time and granted quadratic free SP.
//
// The arithmetic deliberately mirrors dnfbridge.currentPVFSkillPointTarget,
// which is what the login/reselect repair rebases the ledger to: if the two ever
// disagree, buying a skill and logging in would fight each other over TotalSP.
func skillPointTargetForCharacter(baseline *dnfrepo.SkillPointState, character dnfrepo.CharacterRecord) (*dnfrepo.SkillPointState, error) {
	if baseline == nil {
		return nil, nil
	}
	target := *baseline
	bonusSP := character.Stats["bonus_sp"]
	bonusTP := character.Stats["bonus_tp"]
	if target.TotalSP < 0 || target.TotalTP < 0 ||
		bonusSP < 0 || bonusTP < 0 ||
		bonusSP > int64(math.MaxInt) || bonusTP > int64(math.MaxInt) {
		return nil, fmt.Errorf("%w: character=%s baseline=%+v bonus_sp=%d bonus_tp=%d", ErrSkillPointLedger, character.CharacterID, target, bonusSP, bonusTP)
	}
	if target.TotalSP > math.MaxInt-int(bonusSP) || target.TotalTP > math.MaxInt-int(bonusTP) {
		return nil, fmt.Errorf("%w: character=%s baseline=%+v bonus_sp=%d bonus_tp=%d", ErrSkillPointLedger, character.CharacterID, target, bonusSP, bonusTP)
	}
	target.TotalSP += int(bonusSP)
	target.TotalTP += int(bonusTP)
	return &target, nil
}

func synchronizePointLedger(current dnfrepo.SkillPointState, baseline *dnfrepo.SkillPointState, characterLevel int, catalog *dnfskill.Table, job byte, initial map[uint16]int, states map[int64]dnfrepo.SkillState) (dnfrepo.SkillPointState, error) {
	if baseline == nil {
		return current, validatePointLedger(current, characterLevel)
	}
	target := *baseline
	if err := validatePointLedger(target, characterLevel); err != nil {
		return dnfrepo.SkillPointState{}, err
	}
	if isEmptyPointLedger(current) {
		spentSP, spentTP, err := learnedPointCost(catalog, job, initial, states)
		if err != nil {
			return dnfrepo.SkillPointState{}, err
		}
		target.RemainingSP = target.TotalSP - spentSP
		target.RemainingTP = target.TotalTP - spentTP
		if err := validatePointLedger(target, characterLevel); err != nil {
			return dnfrepo.SkillPointState{}, err
		}
		return target, nil
	}
	if current.SyncedLevel <= 0 || current.SyncedLevel > characterLevel || current.TotalSP < 0 || current.RemainingSP < 0 || current.RemainingSP > current.TotalSP || current.TotalTP < 0 || current.RemainingTP < 0 || current.RemainingTP > current.TotalTP {
		return dnfrepo.SkillPointState{}, fmt.Errorf("%w: character_level=%d ledger=%+v", ErrSkillPointLedger, characterLevel, current)
	}
	current.RemainingSP += target.TotalSP - current.TotalSP
	current.RemainingTP += target.TotalTP - current.TotalTP
	current.TotalSP = target.TotalSP
	current.TotalTP = target.TotalTP
	current.SyncedLevel = target.SyncedLevel
	if err := validatePointLedger(current, characterLevel); err != nil {
		return dnfrepo.SkillPointState{}, err
	}
	return current, nil
}

func learnedPointCost(catalog *dnfskill.Table, job byte, initial map[uint16]int, states map[int64]dnfrepo.SkillState) (int, int, error) {
	spentSP := 0
	spentTP := 0
	for rawID, state := range states {
		if state.Level <= 0 {
			continue
		}
		if rawID < 0 || rawID > 0xffff {
			return 0, 0, fmt.Errorf("%w: job=%d skill=%d", ErrSkillNotFound, job, rawID)
		}
		skillID := uint16(rawID)
		definition, ok := catalog.Find(job, skillID)
		if !ok {
			return 0, 0, fmt.Errorf("%w: job=%d skill=%d", ErrSkillNotFound, job, skillID)
		}
		floor := initial[skillID]
		if state.Level < floor {
			return 0, 0, fmt.Errorf("%w: skill=%d level=%d floor=%d", ErrSkillLevel, skillID, state.Level, floor)
		}
		cost, err := mutationPointDelta(definition, floor, state.Level)
		if err != nil {
			return 0, 0, err
		}
		if definition.IsTPSkill() {
			spentTP += cost
		} else {
			spentSP += cost
		}
	}
	return spentSP, spentTP, nil
}

func isEmptyPointLedger(points dnfrepo.SkillPointState) bool {
	return points == (dnfrepo.SkillPointState{})
}

func clonePointBaseline(points *dnfrepo.SkillPointState) *dnfrepo.SkillPointState {
	if points == nil {
		return nil
	}
	copy := *points
	return &copy
}

func mutationPointDelta(definition dnfskill.Skill, currentLevel int, targetLevel int) (int, error) {
	total := 0
	if targetLevel > currentLevel {
		for level := currentLevel; level < targetLevel; level++ {
			cost := definition.LevelCost(level)
			if cost < 0 {
				return 0, fmt.Errorf("%w: skill=%d level=%d cost=%d", ErrSkillPointLedger, definition.ID, level, cost)
			}
			total += cost
		}
		return total, nil
	}
	for level := currentLevel - 1; level >= targetLevel; level-- {
		cost := definition.LevelCost(level)
		if cost < 0 {
			return 0, fmt.Errorf("%w: skill=%d level=%d cost=%d", ErrSkillPointLedger, definition.ID, level, cost)
		}
		total -= cost
	}
	return total, nil
}

func validateLearnedPrerequisites(catalog *dnfskill.Table, job byte, states map[int64]dnfrepo.SkillState, initialLevels map[uint16]int) error {
	for rawID, state := range states {
		if state.Level <= 0 || rawID < 0 || rawID > 0xffff {
			continue
		}
		// The active PVF contains a small set of cumulative profession grants
		// whose own prerequisite is intentionally absent from the same branch
		// (nine reachable awakening states in this client). The client accepts
		// those grants as permanent baseline skills. Exempt only the protected
		// floor itself; any player-purchased levels above it still have to prove
		// the ordinary prerequisite chain.
		if floor := initialLevels[uint16(rawID)]; floor > 0 && state.Level <= floor {
			continue
		}
		definition, ok := catalog.Find(job, uint16(rawID))
		if !ok {
			continue
		}
		for _, prerequisite := range definition.Prerequisites {
			actual := states[int64(prerequisite.SkillID)].Level
			if actual < prerequisite.Level {
				return fmt.Errorf("%w: skill=%d requires=%d level=%d actual=%d", ErrSkillPrerequisite, definition.ID, prerequisite.SkillID, prerequisite.Level, actual)
			}
		}
	}
	return nil
}

func cloneInitialLevels(values map[uint16]int) map[uint16]int {
	if values == nil {
		return nil
	}
	out := make(map[uint16]int, len(values))
	for skillID, level := range values {
		out[skillID] = level
	}
	return out
}

func planError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
