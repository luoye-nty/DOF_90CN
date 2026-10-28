package dungeonadmission

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestServiceWindowDailyRollsAtBeijingSix(t *testing.T) {
	policy := Policy{Key: "tower", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	before := time.Date(2026, 8, 31, 5, 59, 59, 0, serviceCalendar)
	after := time.Date(2026, 8, 31, 6, 0, 0, 0, serviceCalendar)
	beforeWindow, err := ServiceWindow(policy, before)
	if err != nil {
		t.Fatal(err)
	}
	afterWindow, err := ServiceWindow(policy, after)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := beforeWindow.Start.Format(time.RFC3339), "2026-08-30T06:00:00+08:00"; got != want {
		t.Fatalf("before reset start=%s want=%s", got, want)
	}
	if got, want := afterWindow.Start.Format(time.RFC3339), "2026-08-31T06:00:00+08:00"; got != want {
		t.Fatalf("after reset start=%s want=%s", got, want)
	}
}

func TestServiceWindowWeeklyRollsThursdayAtBeijingSix(t *testing.T) {
	policy := Policy{Key: "weekly", Scope: ScopeCharacter, Period: PeriodWeekly, Limit: 6, ResetWeekday: time.Thursday}
	before := time.Date(2026, 9, 3, 5, 59, 59, 0, serviceCalendar)
	after := time.Date(2026, 9, 3, 6, 0, 0, 0, serviceCalendar)
	beforeWindow, err := ServiceWindow(policy, before)
	if err != nil {
		t.Fatal(err)
	}
	afterWindow, err := ServiceWindow(policy, after)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := beforeWindow.Start.Format(time.RFC3339), "2026-08-27T06:00:00+08:00"; got != want {
		t.Fatalf("before weekly reset start=%s want=%s", got, want)
	}
	if got, want := afterWindow.Start.Format(time.RFC3339), "2026-09-03T06:00:00+08:00"; got != want {
		t.Fatalf("after weekly reset start=%s want=%s", got, want)
	}
}

func TestOwnerCommitIsIdempotentAndRejectsFourthTowerAdmission(t *testing.T) {
	ctx := context.Background()
	owner, repositories := newOwnerTest(t, "account-1", "11")
	policy := Policy{Key: "tower_of_despair.character.daily", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, serviceCalendar)

	first, err := owner.Commit(ctx, "11", []Policy{policy}, "run-1", now)
	if err != nil || len(first) != 1 || first[0].Used != 1 || first[0].Remaining != 2 || first[0].Replayed {
		t.Fatalf("first commit=%+v err=%v", first, err)
	}
	replay, err := owner.Commit(ctx, "11", []Policy{policy}, "run-1", now)
	if err != nil || len(replay) != 1 || replay[0].Used != 1 || !replay[0].Replayed {
		t.Fatalf("replay commit=%+v err=%v", replay, err)
	}
	for index := 2; index <= 3; index++ {
		if _, err := owner.Commit(ctx, "11", []Policy{policy}, fmt.Sprintf("run-%d", index), now); err != nil {
			t.Fatalf("commit %d: %v", index, err)
		}
	}
	if _, err := owner.Preflight(ctx, "11", []Policy{policy}, now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("full preflight err=%v want limit", err)
	}
	if _, err := owner.Commit(ctx, "11", []Policy{policy}, "run-4", now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("fourth commit err=%v want limit", err)
	}

	settings, found, err := repositories.Settings.Load(ctx, characterCounterSettingsScope("11"))
	if err != nil || !found {
		t.Fatalf("settings found=%t err=%v", found, err)
	}
	used, ok := parseStoredUint(settings.Values[storagePrefix+digestString(policy.Key)+":used"])
	if !ok || used != 3 {
		t.Fatalf("persisted used=%d ok=%t values=%v", used, ok, settings.Values)
	}
}

func TestOwnerDailyResetClearsOldReceipts(t *testing.T) {
	ctx := context.Background()
	owner, repositories := newOwnerTest(t, "account-1", "11")
	policy := Policy{Key: "tower", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	before := time.Date(2026, 8, 31, 5, 59, 0, 0, serviceCalendar)
	after := time.Date(2026, 8, 31, 6, 0, 0, 0, serviceCalendar)
	if _, err := owner.Commit(ctx, "11", []Policy{policy}, "same-runtime-token", before); err != nil {
		t.Fatal(err)
	}
	result, err := owner.Commit(ctx, "11", []Policy{policy}, "same-runtime-token", after)
	if err != nil || len(result) != 1 || result[0].Used != 1 || result[0].Replayed {
		t.Fatalf("post-reset commit=%+v err=%v", result, err)
	}
	settings, _, _ := repositories.Settings.Load(ctx, characterCounterSettingsScope("11"))
	prefix := storagePrefix + digestString(policy.Key) + ":receipt:"
	receipts := 0
	for key := range settings.Values {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			receipts++
		}
	}
	if receipts != 1 {
		t.Fatalf("receipt keys=%d values=%v", receipts, settings.Values)
	}
}

func TestOwnerRestoreIsIdempotentAndRequiresFreshRetryReceipt(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-1", "11")
	policy := Policy{Key: "tower", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, serviceCalendar)
	if _, err := owner.Commit(ctx, "11", []Policy{policy}, "attempt-1", now); err != nil {
		t.Fatal(err)
	}
	restored, err := owner.Restore(ctx, "11", []Policy{policy}, "attempt-1", now)
	if err != nil || len(restored) != 1 || restored[0].Used != 0 || !restored[0].Restored || restored[0].Replayed {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	replay, err := owner.Restore(ctx, "11", []Policy{policy}, "attempt-1", now)
	if err != nil || len(replay) != 1 || replay[0].Used != 0 || !replay[0].Restored || !replay[0].Replayed {
		t.Fatalf("restore replay=%+v err=%v", replay, err)
	}
	if _, err := owner.Commit(ctx, "11", []Policy{policy}, "attempt-1", now); !errors.Is(err, ErrReceiptRestored) {
		t.Fatalf("reusing restored receipt err=%v want restored", err)
	}
	if result, err := owner.Commit(ctx, "11", []Policy{policy}, "attempt-2", now); err != nil || len(result) != 1 || result[0].Used != 1 {
		t.Fatalf("fresh retry=%+v err=%v", result, err)
	}
}

func TestOwnerResetClearsOnlyTheSelectedPolicyScope(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-reset", "11")
	characterPolicy := Policy{Key: "raid.character.weekly", Scope: ScopeCharacter, Period: PeriodWeekly, Limit: 3, ResetWeekday: time.Thursday}
	accountPolicy := Policy{Key: "raid.account.daily", Scope: ScopeAccount, Period: PeriodDaily, Limit: 10}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, serviceCalendar)
	if _, err := owner.Commit(ctx, "11", []Policy{characterPolicy, accountPolicy}, "reset-test", now); err != nil {
		t.Fatal(err)
	}

	accountReset, err := owner.Reset(ctx, "11", []Policy{accountPolicy}, now)
	if err != nil || len(accountReset) != 1 || accountReset[0].Used != 0 || accountReset[0].Remaining != 10 {
		t.Fatalf("account reset=%+v err=%v", accountReset, err)
	}
	characterState, err := owner.Preflight(ctx, "11", []Policy{characterPolicy, accountPolicy}, now)
	stateByKey := make(map[string]Snapshot, len(characterState))
	for _, snapshot := range characterState {
		stateByKey[snapshot.Policy.Key] = snapshot
	}
	if err != nil || stateByKey[characterPolicy.Key].Used != 1 || stateByKey[accountPolicy.Key].Used != 0 {
		t.Fatalf("after account reset=%+v err=%v", characterState, err)
	}

	characterReset, err := owner.Reset(ctx, "11", []Policy{characterPolicy}, now)
	if err != nil || len(characterReset) != 1 || characterReset[0].Used != 0 || characterReset[0].Remaining != 3 {
		t.Fatalf("character reset=%+v err=%v", characterReset, err)
	}
	characterState, err = owner.Preflight(ctx, "11", []Policy{characterPolicy, accountPolicy}, now)
	stateByKey = make(map[string]Snapshot, len(characterState))
	for _, snapshot := range characterState {
		stateByKey[snapshot.Policy.Key] = snapshot
	}
	if err != nil || stateByKey[characterPolicy.Key].Used != 0 || stateByKey[accountPolicy.Key].Used != 0 {
		t.Fatalf("after character reset=%+v err=%v", characterState, err)
	}
}

func TestOwnerAccountPolicyIsSharedAcrossCharacters(t *testing.T) {
	ctx := context.Background()
	owner, repositories := newOwnerTest(t, "account-shared", "11")
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "12", AccountID: "account-shared"}); err != nil {
		t.Fatal(err)
	}
	policy := Policy{Key: "tower_of_despair.account.daily", Scope: ScopeAccount, Period: PeriodDaily, Limit: 3}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, serviceCalendar)
	for index, characterID := range []string{"11", "12", "11"} {
		if _, err := owner.Commit(ctx, characterID, []Policy{policy}, fmt.Sprintf("shared-%d", index), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Commit(ctx, "12", []Policy{policy}, "shared-4", now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("shared fourth err=%v want limit", err)
	}
	account, found, err := repositories.Account.Load(ctx, "account-shared")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	used, ok := parseStoredUint(account.Metadata[storagePrefix+digestString(policy.Key)+":used"])
	if !ok || used != 3 {
		t.Fatalf("account used=%d ok=%t metadata=%v", used, ok, account.Metadata)
	}
}

func TestOwnerConcurrentCommitNeverExceedsLimit(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-1", "11")
	policy := Policy{Key: "tower", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, serviceCalendar)
	var admitted atomic.Int32
	var limited atomic.Int32
	var unexpected atomic.Value
	var wait sync.WaitGroup
	for index := 0; index < 12; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := owner.Commit(ctx, "11", []Policy{policy}, fmt.Sprintf("concurrent-%d", index), now)
			switch {
			case err == nil:
				admitted.Add(1)
			case errors.Is(err, ErrLimitReached):
				limited.Add(1)
			default:
				unexpected.Store(err)
			}
		}(index)
	}
	wait.Wait()
	if value := unexpected.Load(); value != nil {
		t.Fatalf("unexpected concurrent error=%v", value)
	}
	if admitted.Load() != 3 || limited.Load() != 9 {
		t.Fatalf("admitted=%d limited=%d want=3,9", admitted.Load(), limited.Load())
	}
}

func TestOwnerCharacterAndAccountPoliciesCommitTogether(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-1", "11")
	policies := []Policy{
		{Key: "tower.character", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 3},
		{Key: "tower.account", Scope: ScopeAccount, Period: PeriodDaily, Limit: 1},
	}
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, serviceCalendar)
	result, err := owner.Commit(ctx, "11", policies, "run-1", now)
	if err != nil || len(result) != 2 {
		t.Fatalf("dual commit=%+v err=%v", result, err)
	}
	if _, err := owner.Commit(ctx, "11", policies, "run-2", now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("second dual err=%v want account limit", err)
	}
	characterOnly, err := owner.Preflight(ctx, "11", []Policy{policies[0]}, now)
	if err != nil || len(characterOnly) != 1 || characterOnly[0].Used != 1 {
		t.Fatalf("failed dual attempt changed character counter: snapshot=%+v err=%v", characterOnly, err)
	}
}

func TestOwnerAllowanceRaisesEffectiveDailyLimitAndResetsAtSix(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-1", "11")
	policy := Policy{Key: "dimension_gate.character.daily", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 5}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, serviceCalendar)
	for index := 0; index < 5; index++ {
		if _, err := owner.Commit(ctx, "11", []Policy{policy}, fmt.Sprintf("base-%d", index), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Preflight(ctx, "11", []Policy{policy}, now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("base limit preflight err=%v", err)
	}
	granted, err := owner.GrantAllowances(ctx, "11", []AllowanceGrant{{Policy: policy, Amount: 5, MaximumBonus: 5}}, AllowanceUseLimit{}, now, func(dnfrepo.Group) error { return nil })
	if err != nil || len(granted.Snapshots) != 1 || granted.Snapshots[0].Used != 5 || granted.Snapshots[0].Bonus != 5 ||
		granted.Snapshots[0].EffectiveLimit != 10 || granted.Snapshots[0].Remaining != 5 {
		t.Fatalf("grant=%+v err=%v", granted, err)
	}
	for index := 0; index < 5; index++ {
		if _, err := owner.Commit(ctx, "11", []Policy{policy}, fmt.Sprintf("bonus-%d", index), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Commit(ctx, "11", []Policy{policy}, "over", now); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("expanded limit err=%v", err)
	}
	nextDay := now.Add(24 * time.Hour)
	reset, err := owner.Preflight(ctx, "11", []Policy{policy}, nextDay)
	if err != nil || len(reset) != 1 || reset[0].Used != 0 || reset[0].Bonus != 0 || reset[0].EffectiveLimit != 5 || reset[0].Remaining != 5 {
		t.Fatalf("reset snapshot=%+v err=%v", reset, err)
	}
}

func TestOwnerAllowanceCharacterAndAccountGrantIsAtomicAndCapped(t *testing.T) {
	ctx := context.Background()
	owner, repositories := newOwnerTest(t, "account-1", "11")
	policies := []Policy{
		{Key: "tower_of_grief.character.daily", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 2},
		{Key: "tower_of_grief.account.daily", Scope: ScopeAccount, Period: PeriodDaily, Limit: 1},
	}
	grants := []AllowanceGrant{
		{Policy: policies[0], Amount: 1, MaximumBonus: 2},
		{Policy: policies[1], Amount: 1, MaximumBonus: 2},
	}
	useLimit := AllowanceUseLimit{Key: "tower_of_grief.recommendation.character.daily", Limit: 2}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, serviceCalendar)
	for use := 1; use <= 2; use++ {
		result, err := owner.GrantAllowances(ctx, "11", grants, useLimit, now, func(dnfrepo.Group) error { return nil })
		if err != nil || len(result.Snapshots) != 2 || result.Uses != uint32(use) || result.RemainingUses != uint32(2-use) {
			t.Fatalf("use=%d result=%+v err=%v", use, result, err)
		}
	}
	mutated := false
	if _, err := owner.GrantAllowances(ctx, "11", grants, useLimit, now, func(dnfrepo.Group) error { mutated = true; return nil }); !errors.Is(err, ErrAllowanceLimit) || mutated {
		t.Fatalf("third grant err=%v mutated=%t", err, mutated)
	}
	characterSnapshot, err := owner.Preflight(ctx, "11", []Policy{policies[0]}, now)
	if err != nil || characterSnapshot[0].Bonus != 2 || characterSnapshot[0].EffectiveLimit != 4 {
		t.Fatalf("character snapshot=%+v err=%v", characterSnapshot, err)
	}
	accountSnapshot, err := owner.Preflight(ctx, "11", []Policy{policies[1]}, now)
	if err != nil || accountSnapshot[0].Bonus != 2 || accountSnapshot[0].EffectiveLimit != 3 {
		t.Fatalf("account snapshot=%+v err=%v", accountSnapshot, err)
	}
	settings, _, _ := repositories.Settings.Load(ctx, characterCounterSettingsScope("11"))
	if settings.Values[storagePrefix+"allowance_use:"+digestString(useLimit.Key)+":used"] != "2" {
		t.Fatalf("usage state=%v", settings.Values)
	}
}

func TestOwnerAllowanceMutationFailureRollsBackEveryScope(t *testing.T) {
	ctx := context.Background()
	owner, _ := newOwnerTest(t, "account-1", "11")
	policies := []Policy{
		{Key: "tower.character", Scope: ScopeCharacter, Period: PeriodDaily, Limit: 2},
		{Key: "tower.account", Scope: ScopeAccount, Period: PeriodDaily, Limit: 1},
	}
	grants := []AllowanceGrant{
		{Policy: policies[0], Amount: 1, MaximumBonus: 2},
		{Policy: policies[1], Amount: 1, MaximumBonus: 2},
	}
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, serviceCalendar)
	wantErr := errors.New("source inventory mutation failed")
	if _, err := owner.GrantAllowances(ctx, "11", grants, AllowanceUseLimit{Key: "tower.ticket", Limit: 2}, now, func(dnfrepo.Group) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("grant err=%v", err)
	}
	for _, policy := range policies {
		snapshot, err := owner.Preflight(ctx, "11", []Policy{policy}, now)
		if err != nil || len(snapshot) != 1 || snapshot[0].Bonus != 0 || snapshot[0].EffectiveLimit != policy.Limit {
			t.Fatalf("policy=%s snapshot=%+v err=%v", policy.Key, snapshot, err)
		}
	}
}

func newOwnerTest(t *testing.T, accountID, characterID string) (*Owner, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	repositories := memory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: characterID, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	return owner, repositories
}
