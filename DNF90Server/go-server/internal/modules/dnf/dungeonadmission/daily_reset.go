package dungeonadmission

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrDailyInvitationInvalid         = errors.New("daily dungeon invitation definition is invalid")
	ErrDailyInvitationInventoryFull   = errors.New("daily dungeon invitation inventory page is full")
	ErrDailyInvitationInventoryAbsent = errors.New("daily dungeon invitation inventory is missing")
)

// DailyInvitation is an active-PVF-resolved admission item that is checked
// once per Beijing service day. Minimum is a floor, not an additive reward:
// an existing stack satisfies the daily login grant without increasing it.
type DailyInvitation struct {
	ItemID         int64
	Minimum        int64
	StackLimit     int64
	ListType       byte
	CheckSlotStart int16
	CheckSlotEnd   int16
	SlotStart      int16
	SlotEnd        int16
	PVFPath        string
	StackableType  string
}

type DailyInvitationResult struct {
	Invitation   DailyInvitation
	HeldBefore   int64
	Granted      int64
	ChangedSlots []int16
	Replayed     bool
}

// DailyResetResult describes one idempotent login/06:00 reconciliation. Only
// PeriodDaily policies are normalized; weekly state is deliberately ignored.
type DailyResetResult struct {
	Window                  Window
	Snapshots               []Snapshot
	ResetPolicyKeys         []string
	Invitations             []DailyInvitationResult
	CountersChanged         bool
	InvitationChecksChanged bool
	InventoryChanged        bool
	Changed                 bool
}

// ReconcileDailyReset atomically advances every supplied daily admission
// counter to the current Beijing-06:00 window and performs the once-per-day
// login invitation check. A service-day marker is committed with the item
// mutation, so consuming the item and relogging cannot mint another copy.
func (o *Owner) ReconcileDailyReset(
	ctx context.Context,
	characterID string,
	policies []Policy,
	invitations []DailyInvitation,
	now time.Time,
) (DailyResetResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return DailyResetResult{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return DailyResetResult{}, fmt.Errorf("%w: character is required", ErrCommandInvalid)
	}
	dailyPolicies, err := normalizeDailyResetPolicies(policies)
	if err != nil {
		return DailyResetResult{}, err
	}
	for _, invitation := range invitations {
		if err := validateDailyInvitation(invitation); err != nil {
			return DailyResetResult{}, err
		}
	}
	if len(dailyPolicies) == 0 && len(invitations) == 0 {
		return DailyResetResult{}, nil
	}
	ctx = contextOrBackground(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	window, err := ServiceWindow(Policy{
		Key: "daily_reset.service_window", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 1,
	}, now)
	if err != nil {
		return DailyResetResult{}, err
	}
	result := DailyResetResult{Window: window}
	err = o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Account == nil || tx.Settings == nil || tx.Inventory == nil {
			return ErrOwnerUnavailable
		}
		character, found, loadErr := tx.Character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrCharacterMissing
		}
		accountID := strings.TrimSpace(character.AccountID)
		if accountID == "" {
			return ErrAccountMissing
		}
		account, found, loadErr := tx.Account.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrAccountMissing
		}
		account = dnfrepo.CloneAccount(account)
		if account.Metadata == nil {
			account.Metadata = make(map[string]string)
		}

		settingsScope := characterCounterSettingsScope(characterID)
		settings, settingsFound, loadErr := tx.Settings.Load(ctx, settingsScope)
		if loadErr != nil {
			return loadErr
		}
		settings = dnfrepo.CloneSettings(settings)
		if !settingsFound {
			settings.Scope = settingsScope
		}
		if settings.Values == nil {
			settings.Values = make(map[string]string)
		}

		result.Snapshots = make([]Snapshot, 0, len(dailyPolicies))
		for _, policy := range dailyPolicies {
			policyWindow, windowErr := ServiceWindow(policy, now)
			if windowErr != nil {
				return windowErr
			}
			values := settings.Values
			if policy.Scope == ScopeAccount {
				values = account.Metadata
			}
			prefix := storagePrefix + digestString(policy.Key) + ":"
			periodUnix := policyWindow.Start.Unix()
			storedPeriod, periodOK := parseStoredInt(values[prefix+"period"])
			used64, usedOK := parseStoredUint(values[prefix+"used"])
			bonus64, bonusOK := parseStoredUint(values[prefix+"bonus"])
			periodCurrent := periodOK && storedPeriod == periodUnix && usedOK
			if !periodCurrent {
				clearPolicyState(values, prefix)
				values[prefix+"period"] = strconv.FormatInt(periodUnix, 10)
				values[prefix+"used"] = "0"
				used64 = 0
				bonus64 = 0
				bonusOK = true
				result.CountersChanged = true
				result.ResetPolicyKeys = append(result.ResetPolicyKeys, policy.Key)
			}
			if !bonusOK || bonus64 > uint64(math.MaxUint32-policy.Limit) {
				bonus64 = 0
				if _, present := values[prefix+"bonus"]; present {
					delete(values, prefix+"bonus")
					result.CountersChanged = true
				}
			}
			effectiveLimit := policy.Limit + uint32(bonus64)
			if used64 > uint64(effectiveLimit) {
				used64 = uint64(effectiveLimit)
				values[prefix+"used"] = strconv.FormatUint(used64, 10)
				result.CountersChanged = true
			}
			result.Snapshots = append(result.Snapshots, Snapshot{
				Policy: policy, Window: policyWindow, Used: uint32(used64), Bonus: uint32(bonus64),
				EffectiveLimit: effectiveLimit, Remaining: effectiveLimit - uint32(used64),
			})
		}

		inventory, inventoryFound, loadErr := tx.Inventory.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !inventoryFound {
			return ErrDailyInvitationInventoryAbsent
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		result.Invitations = make([]DailyInvitationResult, 0, len(invitations))
		for _, invitation := range invitations {
			markerKey := dailyInvitationMarkerKey(invitation)
			markerPeriod, markerOK := parseStoredInt(settings.Values[markerKey])
			if markerOK && markerPeriod == window.Start.Unix() {
				result.Invitations = append(result.Invitations, DailyInvitationResult{Invitation: invitation, Replayed: true})
				continue
			}
			invitationResult, ensureErr := ensureDailyInvitation(&inventory, invitation)
			if ensureErr != nil {
				return ensureErr
			}
			settings.Values[markerKey] = strconv.FormatInt(window.Start.Unix(), 10)
			result.InvitationChecksChanged = true
			result.Invitations = append(result.Invitations, invitationResult)
			if invitationResult.Granted > 0 {
				result.InventoryChanged = true
			}
		}

		result.Changed = result.CountersChanged || result.InvitationChecksChanged || result.InventoryChanged
		if result.InventoryChanged {
			inventory.UpdatedAt = now
			if saveErr := dnfrepo.SaveInventoryFields(ctx, tx.Inventory, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
				return saveErr
			}
		}
		// A first daily invitation check changes only its marker when the item
		// already exists, so settings must still be saved even without a grant.
		if result.CountersChanged || result.InvitationChecksChanged {
			settings.UpdatedAt = now
			if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); saveErr != nil {
				return saveErr
			}
		}
		if result.CountersChanged {
			account.UpdatedAt = now
			if saveErr := tx.Account.Save(ctx, account); saveErr != nil {
				return saveErr
			}
		}
		return nil
	})
	if err != nil {
		return DailyResetResult{}, err
	}
	return result, nil
}

func normalizeDailyResetPolicies(policies []Policy) ([]Policy, error) {
	if len(policies) == 0 {
		return nil, nil
	}
	normalized, err := normalizePolicies(policies)
	if err != nil {
		return nil, err
	}
	daily := make([]Policy, 0, len(normalized))
	for _, policy := range normalized {
		if policy.Period == PeriodDaily {
			daily = append(daily, policy)
		}
	}
	return daily, nil
}

func validateDailyInvitation(invitation DailyInvitation) error {
	checkSlotStart, checkSlotEnd := dailyInvitationCheckSlotRange(invitation)
	if invitation.ItemID <= 0 || invitation.Minimum <= 0 || invitation.StackLimit < 0 ||
		invitation.ListType != dnfrepo.MainInventoryListType || invitation.SlotStart < 0 || invitation.SlotEnd < invitation.SlotStart ||
		checkSlotStart < 0 || checkSlotEnd < checkSlotStart ||
		strings.TrimSpace(invitation.PVFPath) == "" || strings.TrimSpace(invitation.StackableType) == "" {
		return fmt.Errorf("%w: item=%d minimum=%d stack_limit=%d list=%d check_slots=%d..%d target_slots=%d..%d path=%q type=%q",
			ErrDailyInvitationInvalid, invitation.ItemID, invitation.Minimum, invitation.StackLimit,
			invitation.ListType, checkSlotStart, checkSlotEnd, invitation.SlotStart, invitation.SlotEnd,
			invitation.PVFPath, invitation.StackableType)
	}
	return nil
}

func dailyInvitationCheckSlotRange(invitation DailyInvitation) (int16, int16) {
	if invitation.CheckSlotStart == 0 && invitation.CheckSlotEnd == 0 {
		return invitation.SlotStart, invitation.SlotEnd
	}
	return invitation.CheckSlotStart, invitation.CheckSlotEnd
}

func dailyInvitationMarkerKey(invitation DailyInvitation) string {
	identity := strconv.FormatInt(invitation.ItemID, 10) + ":" + strings.ToLower(strings.TrimSpace(invitation.PVFPath))
	return storagePrefix + "daily_invitation:" + digestString(identity) + ":period"
}

func ensureDailyInvitation(inventory *dnfrepo.InventoryRecord, invitation DailyInvitation) (DailyInvitationResult, error) {
	result := DailyInvitationResult{Invitation: invitation}
	checkSlotStart, checkSlotEnd := dailyInvitationCheckSlotRange(invitation)
	for slot := checkSlotStart; slot <= checkSlotEnd; slot++ {
		stack, found := inventory.Slots[dailyInvitationSlotKey(invitation.ListType, slot)]
		if found && stack.ItemID == invitation.ItemID && stack.Count > 0 {
			if result.HeldBefore > math.MaxInt64-stack.Count {
				result.HeldBefore = math.MaxInt64
			} else {
				result.HeldBefore += stack.Count
			}
		}
	}
	remaining := invitation.Minimum - result.HeldBefore
	if remaining <= 0 {
		return result, nil
	}
	// PVF [stack limit] 是这个容器里该道具的总量上限：额度按整个容器里同 id 总量算
	// （不能只看 check 范围），容器里已经有该道具的行时不能再开第二行。
	newRowStackLimit := int64(0)
	space := dnfrepo.ContainerUncappedSpace
	if invitation.StackLimit > 0 {
		newRowStackLimit = invitation.StackLimit
		space = dnfrepo.ContainerItemSpace(inventory.Slots, invitation.ListType, invitation.ItemID, invitation.StackLimit)
	}
	markChanged := func(slot int16) {
		for _, changed := range result.ChangedSlots {
			if changed == slot {
				return
			}
		}
		result.ChangedSlots = append(result.ChangedSlots, slot)
	}
	for slot := invitation.SlotStart; slot <= invitation.SlotEnd && remaining > 0; slot++ {
		key := dailyInvitationSlotKey(invitation.ListType, slot)
		stack, found := inventory.Slots[key]
		if !found || stack.ItemID != invitation.ItemID || stack.Count <= 0 || invitation.StackLimit == 1 {
			continue
		}
		capacity := remaining
		if invitation.StackLimit > 0 {
			capacity = invitation.StackLimit - stack.Count
		}
		if capacity > space {
			capacity = space
		}
		if capacity <= 0 {
			continue
		}
		add := remaining
		if add > capacity {
			add = capacity
		}
		stack = cloneDailyInvitationStack(stack)
		stack.Count += add
		inventory.Slots[key] = stack
		remaining -= add
		space -= add
		result.Granted += add
		markChanged(slot)
	}
	for slot := invitation.SlotStart; slot <= invitation.SlotEnd && remaining > 0; slot++ {
		key := dailyInvitationSlotKey(invitation.ListType, slot)
		if _, occupied := inventory.Slots[key]; occupied {
			continue
		}
		if !dnfrepo.CanOpenContainerItemRow(inventory.Slots, invitation.ListType, invitation.ItemID, newRowStackLimit) {
			break
		}
		insert := remaining
		if invitation.StackLimit > 0 && insert > invitation.StackLimit {
			insert = invitation.StackLimit
		}
		if insert > space {
			insert = space
		}
		if insert <= 0 {
			break
		}
		inventory.Slots[key] = dnfrepo.ItemStack{
			ItemID: invitation.ItemID,
			Count:  insert,
			Extra: map[string]string{
				"item_kind":      "stackable",
				"pvf_path":       invitation.PVFPath,
				"stackable_type": invitation.StackableType,
			},
		}
		remaining -= insert
		space -= insert
		result.Granted += insert
		markChanged(slot)
	}
	if remaining > 0 {
		return DailyInvitationResult{}, fmt.Errorf("%w: item=%d remaining=%d slots=%d..%d",
			ErrDailyInvitationInventoryFull, invitation.ItemID, remaining, invitation.SlotStart, invitation.SlotEnd)
	}
	return result, nil
}

func dailyInvitationSlotKey(listType byte, slot int16) string {
	return fmt.Sprintf("%d:%d", listType, slot)
}

func cloneDailyInvitationStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
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
