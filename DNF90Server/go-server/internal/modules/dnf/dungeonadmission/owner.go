package dungeonadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	characterSettingsSuffix = ":dungeon_admission_counters_v1"
	storagePrefix           = "dungeon_admission_counter_v1:"
)

const (
	receiptActive   = "active"
	receiptRestored = "restored"
)

// Owner is the database-neutral admission-counter transaction boundary. It
// reuses the existing character-settlement unit of work so account and
// character policies for one admission commit together in memory, SQLite and
// MySQL without adding a backend-specific side table.
type Owner struct {
	repositories dnfrepo.Group
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.Character == nil || repositories.Account == nil || repositories.Settings == nil || repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{repositories: repositories}, nil
}

// Preflight performs a read-only early rejection. Commit repeats the same
// limit check inside the durable transaction and remains authoritative under
// concurrent admissions.
func (o *Owner) Preflight(ctx context.Context, characterID string, policies []Policy, now time.Time) ([]Snapshot, error) {
	if o == nil || o.repositories.Character == nil || o.repositories.Account == nil || o.repositories.Settings == nil {
		return nil, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	policies, err := normalizePolicies(policies)
	if err != nil || characterID == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: character is required", ErrCommandInvalid)
	}
	ctx = contextOrBackground(ctx)
	character, found, err := o.repositories.Character.Load(ctx, characterID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrCharacterMissing
	}
	accountID := strings.TrimSpace(character.AccountID)
	if accountID == "" {
		return nil, ErrAccountMissing
	}
	account, found, err := o.repositories.Account.Load(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrAccountMissing
	}
	settingsScope := characterCounterSettingsScope(characterID)
	settings, _, err := o.repositories.Settings.Load(ctx, settingsScope)
	if err != nil {
		return nil, err
	}
	return evaluatePolicies(policies, now, settings.Values, account.Metadata, "", false)
}

// Commit atomically increments every supplied policy once. Receipt must be a
// stable successful-admission identity (the dungeon lifecycle token is the
// intended bridge input). Repeating the same receipt in one service window is
// read-only and returns Replayed snapshots.
func (o *Owner) Commit(ctx context.Context, characterID string, policies []Policy, receipt string, now time.Time) ([]Snapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	policies, err := normalizePolicies(policies)
	if err != nil || characterID == "" || receipt == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: character and receipt are required", ErrCommandInvalid)
	}
	ctx = contextOrBackground(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	var snapshots []Snapshot
	err = o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Account == nil || tx.Settings == nil {
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

		var evaluateErr error
		snapshots, evaluateErr = evaluatePolicies(policies, now, settings.Values, account.Metadata, receipt, true)
		if evaluateErr != nil {
			return evaluateErr
		}
		settings.UpdatedAt = now
		account.UpdatedAt = now
		if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); saveErr != nil {
			return saveErr
		}
		return tx.Account.Save(ctx, account)
	})
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

// Reset clears the durable state of the supplied policies for the current
// service window. It is the administrative counterpart to Commit: character
// policies are deleted from the character settings record, while account
// policies are deleted from the owning account metadata. Receipts and bonuses
// under the same policy prefix are cleared with the usage, so the next raid
// starts from a clean counter and cannot reuse an old admission receipt.
func (o *Owner) Reset(ctx context.Context, characterID string, policies []Policy, now time.Time) ([]Snapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	policies, err := normalizePolicies(policies)
	if err != nil || characterID == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: character is required", ErrCommandInvalid)
	}
	ctx = contextOrBackground(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	var snapshots []Snapshot
	err = o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Account == nil || tx.Settings == nil {
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

		characterChanged := false
		accountChanged := false
		snapshots = make([]Snapshot, 0, len(policies))
		for _, policy := range policies {
			window, windowErr := ServiceWindow(policy, now)
			if windowErr != nil {
				return windowErr
			}
			values := settings.Values
			if policy.Scope == ScopeAccount {
				values = account.Metadata
			}
			prefix := storagePrefix + digestString(policy.Key) + ":"
			if clearPolicyState(values, prefix) {
				if policy.Scope == ScopeAccount {
					accountChanged = true
				} else {
					characterChanged = true
				}
			}
			snapshots = append(snapshots, Snapshot{
				Policy:         policy,
				Window:         window,
				Used:           0,
				Bonus:          0,
				EffectiveLimit: policy.Limit,
				Remaining:      policy.Limit,
			})
		}
		if characterChanged {
			settings.UpdatedAt = now
			if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); saveErr != nil {
				return saveErr
			}
		}
		if accountChanged {
			account.UpdatedAt = now
			if saveErr := tx.Account.Save(ctx, account); saveErr != nil {
				return saveErr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

func evaluatePolicies(
	policies []Policy,
	now time.Time,
	characterValues map[string]string,
	accountValues map[string]string,
	receipt string,
	mutate bool,
) ([]Snapshot, error) {
	snapshots := make([]Snapshot, 0, len(policies))
	receiptDigest := digestString(receipt)
	for _, policy := range policies {
		window, err := ServiceWindow(policy, now)
		if err != nil {
			return nil, err
		}
		values := characterValues
		if policy.Scope == ScopeAccount {
			values = accountValues
		}
		if values == nil {
			values = map[string]string{}
		}
		prefix := storagePrefix + digestString(policy.Key) + ":"
		periodKey := prefix + "period"
		usedKey := prefix + "used"
		periodUnix := window.Start.Unix()
		storedPeriod, periodOK := parseStoredInt(values[periodKey])
		used64, usedOK := parseStoredUint(values[usedKey])
		periodCurrent := periodOK && storedPeriod == periodUnix && usedOK
		if !periodCurrent {
			used64 = 0
			if mutate {
				clearPolicyState(values, prefix)
				values[periodKey] = strconv.FormatInt(periodUnix, 10)
				values[usedKey] = "0"
			}
		}
		bonus64, bonusOK := parseStoredUint(values[prefix+"bonus"])
		if !periodCurrent || !bonusOK || bonus64 > uint64(^uint32(0)-policy.Limit) {
			bonus64 = 0
		}
		effectiveLimit := policy.Limit + uint32(bonus64)
		if used64 > uint64(effectiveLimit) {
			used64 = uint64(effectiveLimit)
		}
		replayed := false
		receiptKey := prefix + "receipt:" + receiptDigest
		receiptValue := values[receiptKey]
		activeReceiptValue := formatReceiptValue(periodUnix, receiptActive)
		restoredReceiptValue := formatReceiptValue(periodUnix, receiptRestored)
		if receipt != "" && receiptValue == activeReceiptValue {
			replayed = true
		} else if receipt != "" && receiptValue == restoredReceiptValue {
			return nil, fmt.Errorf("%w: policy=%s", ErrReceiptRestored, policy.Key)
		}
		used := uint32(used64)
		snapshot := Snapshot{
			Policy: policy, Window: window, Used: used, Bonus: uint32(bonus64),
			EffectiveLimit: effectiveLimit, Remaining: effectiveLimit - used, Replayed: replayed,
		}
		if !replayed && used >= effectiveLimit {
			return nil, &LimitError{Snapshot: snapshot}
		}
		if mutate && !replayed {
			used++
			values[usedKey] = strconv.FormatUint(uint64(used), 10)
			values[receiptKey] = activeReceiptValue
			snapshot.Used = used
			snapshot.Remaining = effectiveLimit - used
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

// GrantAllowances atomically applies every bounded service-window allowance
// and the caller's source-item mutation. A rejected cap, invalid character,
// inventory mutation failure, or repository save failure commits neither side.
func (o *Owner) GrantAllowances(
	ctx context.Context,
	characterID string,
	grants []AllowanceGrant,
	useLimit AllowanceUseLimit,
	now time.Time,
	mutate func(dnfrepo.Group) error,
) (AllowanceResult, error) {
	if o == nil || o.repositories.CharacterSettlement == nil || mutate == nil {
		return AllowanceResult{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	if characterID == "" || len(grants) == 0 {
		return AllowanceResult{}, fmt.Errorf("%w: character and grants are required", ErrCommandInvalid)
	}
	policies := make([]Policy, 0, len(grants))
	grantByIdentity := make(map[string]AllowanceGrant, len(grants))
	for _, grant := range grants {
		if grant.Amount == 0 || grant.MaximumBonus == 0 || grant.Amount > grant.MaximumBonus {
			return AllowanceResult{}, fmt.Errorf("%w: invalid allowance amount=%d maximum=%d", ErrCommandInvalid, grant.Amount, grant.MaximumBonus)
		}
		policies = append(policies, grant.Policy)
		grantByIdentity[policyIdentity(grant.Policy)] = grant
	}
	var err error
	policies, err = normalizePolicies(policies)
	if err != nil {
		return AllowanceResult{}, err
	}
	useLimit.Key = strings.TrimSpace(useLimit.Key)
	if (useLimit.Key == "") != (useLimit.Limit == 0) {
		return AllowanceResult{}, fmt.Errorf("%w: allowance use key and limit must both be set", ErrCommandInvalid)
	}
	ctx = contextOrBackground(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	var result AllowanceResult
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

		if useLimit.Limit > 0 {
			window, windowErr := ServiceWindow(Policy{Key: useLimit.Key, Scope: ScopeCharacter, Period: PeriodDaily, Limit: useLimit.Limit}, now)
			if windowErr != nil {
				return windowErr
			}
			prefix := storagePrefix + "allowance_use:" + digestString(useLimit.Key) + ":"
			periodUnix := window.Start.Unix()
			storedPeriod, periodOK := parseStoredInt(settings.Values[prefix+"period"])
			used64, usedOK := parseStoredUint(settings.Values[prefix+"used"])
			if !periodOK || storedPeriod != periodUnix || !usedOK {
				clearPolicyState(settings.Values, prefix)
				settings.Values[prefix+"period"] = strconv.FormatInt(periodUnix, 10)
				used64 = 0
			}
			if used64 >= uint64(useLimit.Limit) {
				return fmt.Errorf("%w: ticket=%s used=%d limit=%d", ErrAllowanceLimit, useLimit.Key, used64, useLimit.Limit)
			}
			used64++
			settings.Values[prefix+"used"] = strconv.FormatUint(used64, 10)
			result.Uses = uint32(used64)
			result.RemainingUses = useLimit.Limit - result.Uses
		}

		result.Snapshots = make([]Snapshot, 0, len(policies))
		for _, policy := range policies {
			grant, ok := grantByIdentity[policyIdentity(policy)]
			if !ok {
				return fmt.Errorf("%w: allowance policy %s is missing", ErrCommandInvalid, policy.Key)
			}
			window, windowErr := ServiceWindow(policy, now)
			if windowErr != nil {
				return windowErr
			}
			values := settings.Values
			if policy.Scope == ScopeAccount {
				values = account.Metadata
			}
			prefix := storagePrefix + digestString(policy.Key) + ":"
			periodUnix := window.Start.Unix()
			storedPeriod, periodOK := parseStoredInt(values[prefix+"period"])
			used64, usedOK := parseStoredUint(values[prefix+"used"])
			bonus64, bonusOK := parseStoredUint(values[prefix+"bonus"])
			if !periodOK || storedPeriod != periodUnix || !usedOK {
				clearPolicyState(values, prefix)
				values[prefix+"period"] = strconv.FormatInt(periodUnix, 10)
				values[prefix+"used"] = "0"
				used64 = 0
				bonus64 = 0
			} else if !bonusOK {
				bonus64 = 0
			}
			if bonus64 > uint64(grant.MaximumBonus) || uint64(grant.Amount) > uint64(grant.MaximumBonus)-bonus64 {
				return fmt.Errorf("%w: policy=%s bonus=%d add=%d maximum=%d", ErrAllowanceLimit, policy.Key, bonus64, grant.Amount, grant.MaximumBonus)
			}
			newBonus := uint32(bonus64) + grant.Amount
			if newBonus > ^uint32(0)-policy.Limit {
				return fmt.Errorf("%w: policy=%s allowance overflow", ErrCommandInvalid, policy.Key)
			}
			effectiveLimit := policy.Limit + newBonus
			if used64 > uint64(effectiveLimit) {
				return fmt.Errorf("%w: policy=%s used=%d effective_limit=%d", ErrReceiptState, policy.Key, used64, effectiveLimit)
			}
			values[prefix+"bonus"] = strconv.FormatUint(uint64(newBonus), 10)
			result.Snapshots = append(result.Snapshots, Snapshot{
				Policy: policy, Window: window, Used: uint32(used64), Bonus: newBonus,
				EffectiveLimit: effectiveLimit, Remaining: effectiveLimit - uint32(used64),
			})
		}

		if mutationErr := mutate(tx); mutationErr != nil {
			return mutationErr
		}
		settings.UpdatedAt = now
		account.UpdatedAt = now
		if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); saveErr != nil {
			return saveErr
		}
		return tx.Account.Save(ctx, account)
	})
	if err != nil {
		return AllowanceResult{}, err
	}
	return result, nil
}

func policyIdentity(policy Policy) string {
	return strings.TrimSpace(policy.Key) + ":" + strconv.Itoa(int(policy.Scope))
}

// Restore compensates a committed counter reservation when a later material,
// party-member, runtime-attach, or runtime-commit step fails. It decrements
// only the exact active receipt in the same service window and leaves a
// restored tombstone so repeating the compensation cannot decrement twice.
// A caller must allocate a fresh receipt before retrying the admission.
func (o *Owner) Restore(ctx context.Context, characterID string, policies []Policy, receipt string, now time.Time) ([]Snapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	policies, err := normalizePolicies(policies)
	if err != nil || characterID == "" || receipt == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: character and receipt are required", ErrCommandInvalid)
	}
	ctx = contextOrBackground(ctx)
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	var snapshots []Snapshot
	err = o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Account == nil || tx.Settings == nil {
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

		snapshots, loadErr = restorePolicies(policies, now, settings.Values, account.Metadata, receipt)
		if loadErr != nil {
			return loadErr
		}
		settings.UpdatedAt = now
		account.UpdatedAt = now
		if saveErr := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); saveErr != nil {
			return saveErr
		}
		return tx.Account.Save(ctx, account)
	})
	if err != nil {
		return nil, err
	}
	return snapshots, nil
}

func restorePolicies(policies []Policy, now time.Time, characterValues, accountValues map[string]string, receipt string) ([]Snapshot, error) {
	type restoreState struct {
		policy     Policy
		window     Window
		values     map[string]string
		usedKey    string
		receiptKey string
		used       uint32
		bonus      uint32
		status     string
	}
	states := make([]restoreState, 0, len(policies))
	receiptDigest := digestString(receipt)
	activeCount := 0
	restoredCount := 0
	missingCount := 0
	for _, policy := range policies {
		window, err := ServiceWindow(policy, now)
		if err != nil {
			return nil, err
		}
		values := characterValues
		if policy.Scope == ScopeAccount {
			values = accountValues
		}
		prefix := storagePrefix + digestString(policy.Key) + ":"
		periodUnix := window.Start.Unix()
		storedPeriod, periodOK := parseStoredInt(values[prefix+"period"])
		used64, usedOK := parseStoredUint(values[prefix+"used"])
		bonus64, bonusOK := parseStoredUint(values[prefix+"bonus"])
		periodCurrent := periodOK && storedPeriod == periodUnix && usedOK
		if !periodCurrent || !bonusOK || bonus64 > uint64(^uint32(0)-policy.Limit) {
			bonus64 = 0
		}
		effectiveLimit := policy.Limit + uint32(bonus64)
		if !periodCurrent || used64 > uint64(effectiveLimit) {
			used64 = 0
		}
		receiptKey := prefix + "receipt:" + receiptDigest
		status := ""
		switch values[receiptKey] {
		case formatReceiptValue(periodUnix, receiptActive):
			status = receiptActive
			activeCount++
		case formatReceiptValue(periodUnix, receiptRestored):
			status = receiptRestored
			restoredCount++
		default:
			missingCount++
		}
		states = append(states, restoreState{policy: policy, window: window, values: values, usedKey: prefix + "used", receiptKey: receiptKey, used: uint32(used64), bonus: uint32(bonus64), status: status})
	}
	if activeCount != 0 && (restoredCount != 0 || missingCount != 0) || restoredCount != 0 && missingCount != 0 {
		return nil, fmt.Errorf("%w: active=%d restored=%d missing=%d", ErrReceiptState, activeCount, restoredCount, missingCount)
	}
	snapshots := make([]Snapshot, 0, len(states))
	for _, state := range states {
		effectiveLimit := state.policy.Limit + state.bonus
		snapshot := Snapshot{
			Policy: state.policy, Window: state.window, Used: state.used, Bonus: state.bonus,
			EffectiveLimit: effectiveLimit, Remaining: effectiveLimit - state.used,
		}
		switch state.status {
		case receiptActive:
			if state.used == 0 {
				return nil, fmt.Errorf("%w: policy=%s active receipt has zero use", ErrReceiptState, state.policy.Key)
			}
			state.used--
			state.values[state.usedKey] = strconv.FormatUint(uint64(state.used), 10)
			state.values[state.receiptKey] = formatReceiptValue(state.window.Start.Unix(), receiptRestored)
			snapshot.Used = state.used
			snapshot.Remaining = effectiveLimit - state.used
			snapshot.Restored = true
		case receiptRestored:
			snapshot.Replayed = true
			snapshot.Restored = true
		default:
			snapshot.Replayed = true
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func formatReceiptValue(periodUnix int64, status string) string {
	return strconv.FormatInt(periodUnix, 10) + ":" + status
}

func clearPolicyState(values map[string]string, prefix string) bool {
	cleared := false
	for key := range values {
		if strings.HasPrefix(key, prefix) {
			delete(values, key)
			cleared = true
		}
	}
	return cleared
}

func characterCounterSettingsScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + characterSettingsSuffix
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func parseStoredInt(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed, err == nil
}

func parseStoredUint(value string) (uint64, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	return parsed, err == nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
