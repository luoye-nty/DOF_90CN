package purchaselimit

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestParseScopeAndPeriodCoverPVFValues(t *testing.T) {
	for raw, want := range map[string]Scope{
		"account":   ScopeAccount,
		"`account`": 0,
		"character": ScopeCharacter,
		"ACCOUNT":   ScopeAccount,
	} {
		got, ok := ParseScope(raw)
		if raw == "`account`" {
			if ok {
				t.Fatalf("ParseScope(%q) accepted backticks", raw)
			}
			continue
		}
		if !ok || got != want {
			t.Fatalf("ParseScope(%q)=%v,%t want %v", raw, got, ok, want)
		}
	}
	for raw, want := range map[string]Period{
		"accumulate":    PeriodAccumulate,
		"weekly":        PeriodWeekly,
		"daily":         PeriodDaily,
		"agitwarseason": PeriodSeason,
	} {
		got, ok := ParsePeriod(raw)
		if !ok || got != want {
			t.Fatalf("ParsePeriod(%q)=%v,%t want %v", raw, got, ok, want)
		}
	}
	if _, ok := ParsePeriod("monthly"); ok {
		t.Fatal("ParsePeriod accepted an unknown period")
	}
}

// 服务日 06:00 边界：北京 05:59 仍属于前一天，06:00 起属于新的一天。
func TestDailyWindowRollsAtBeijingSixAM(t *testing.T) {
	policy := Policy{ItemID: 1, Scope: ScopeCharacter, Period: PeriodDaily, Limit: 1}
	before := time.Date(2026, 9, 14, 5, 59, 0, 0, serviceCalendar)
	after := time.Date(2026, 9, 14, 6, 0, 1, 0, serviceCalendar)
	beforeWindow := policy.Window(before)
	afterWindow := policy.Window(after)
	if !beforeWindow.Start.Equal(time.Date(2026, 9, 13, 6, 0, 0, 0, serviceCalendar)) {
		t.Fatalf("before window start=%s", beforeWindow.Start)
	}
	if !afterWindow.Start.Equal(time.Date(2026, 9, 14, 6, 0, 0, 0, serviceCalendar)) {
		t.Fatalf("after window start=%s", afterWindow.Start)
	}
}

// 周窗口按周四 06:00 重置（与 raid 周重置一致）。
func TestWeeklyWindowResetsOnThursdaySixAM(t *testing.T) {
	policy := Policy{ItemID: 1, Scope: ScopeAccount, Period: PeriodWeekly, Limit: 10}
	wednesday := time.Date(2026, 9, 16, 20, 0, 0, 0, serviceCalendar)
	thursday := time.Date(2026, 9, 17, 6, 0, 1, 0, serviceCalendar)
	if got := policy.Window(wednesday).Start; !got.Equal(time.Date(2026, 9, 10, 6, 0, 0, 0, serviceCalendar)) {
		t.Fatalf("wednesday window start=%s", got)
	}
	if got := policy.Window(thursday).Start; !got.Equal(time.Date(2026, 9, 17, 6, 0, 0, 0, serviceCalendar)) {
		t.Fatalf("thursday window start=%s", got)
	}
}

// accumulate 与赛季都没有窗口（永不清零）。
func TestAccumulateAndSeasonHaveNoWindow(t *testing.T) {
	for _, period := range []Period{PeriodAccumulate, PeriodSeason} {
		policy := Policy{ItemID: 1, Scope: ScopeCharacter, Period: period, Limit: 1}
		if window := policy.Window(time.Now()); !window.Start.IsZero() {
			t.Fatalf("period=%s window start=%s want zero", period, window.Start)
		}
		if period.Periodic() && period != PeriodSeason {
			t.Fatalf("period=%s reported periodic", period)
		}
	}
}

func TestPolicyValidRejectsZeroLimit(t *testing.T) {
	if err := (Policy{ItemID: 1, Scope: ScopeAccount, Period: PeriodWeekly}).Valid(); err == nil {
		t.Fatal("zero limit accepted")
	}
	if err := (Policy{ItemID: 1, Scope: ScopeAccount, Period: PeriodWeekly, Limit: 1}).Valid(); err != nil {
		t.Fatal(err)
	}
}

func TestConsumeCharacterAccumulateNeverResets(t *testing.T) {
	stats := map[string]int64{}
	policy := Policy{ItemID: 490004806, Scope: ScopeCharacter, Period: PeriodAccumulate, Limit: 2}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	if err := ConsumeCharacter(stats, policy, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeCharacter(stats, policy, 1, now.AddDate(1, 0, 0)); err != nil {
		t.Fatalf("accumulate reset unexpectedly: %v", err)
	}
	err := ConsumeCharacter(stats, policy, 1, now.AddDate(2, 0, 0))
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || !errors.Is(err, ErrLimitReached) {
		t.Fatalf("第三件未被拦截: %v", err)
	}
	if limitErr.State.Used != 2 || limitErr.Policy.Scope != ScopeCharacter {
		t.Fatalf("limit error state=%+v", limitErr.State)
	}
}

func TestConsumeAccountWeeklyResetsInNextWindow(t *testing.T) {
	metadata := map[string]string{}
	policy := Policy{ItemID: 490004806, Scope: ScopeAccount, Period: PeriodWeekly, Limit: 10}
	first := time.Date(2026, 9, 14, 12, 0, 0, 0, serviceCalendar)
	if err := ConsumeAccount(metadata, policy, 10, first); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeAccount(metadata, policy, 1, first); !errors.Is(err, ErrLimitReached) {
		t.Fatalf("同周第 11 件未被拦截: %v", err)
	}
	if state := ReadAccount(metadata, policy, first); state.Used != 10 || state.Remaining != 0 {
		t.Fatalf("state=%+v", state)
	}
	// 下一周（跨周四 06:00）窗口变化即归零。
	next := time.Date(2026, 9, 21, 12, 0, 0, 0, serviceCalendar)
	if state := ReadAccount(metadata, policy, next); state.Used != 0 || state.Remaining != 10 {
		t.Fatalf("next week state=%+v", state)
	}
	if err := ConsumeAccount(metadata, policy, 10, next); err != nil {
		t.Fatal(err)
	}
}

func TestReadCharacterIgnoresStaleWindowCounter(t *testing.T) {
	policy := Policy{ItemID: 7, Scope: ScopeCharacter, Period: PeriodDaily, Limit: 5}
	stats := map[string]int64{
		CountKey(policy):  5,
		WindowKey(policy): time.Date(2026, 9, 13, 6, 0, 0, 0, serviceCalendar).Unix(),
	}
	if state := ReadCharacter(stats, policy, time.Date(2026, 9, 14, 12, 0, 0, 0, serviceCalendar)); state.Used != 0 {
		t.Fatalf("stale window kept used=%d", state.Used)
	}
}

// 计数键必须对每一种周期都能"写得进、读得回"。
//
// 这个往返曾经断在季节上：CountKey 用 Period.String()（"season"）拼键，
// 而 ParsePeriod 当时只认 PVF 的 "agitwarseason"，于是赛季计数键读不回来，
// op715 下发时被当成"没有这条计数"——拦截仍生效，但客户端永远显示 0/N。
func TestCountKeyRoundTripsForEveryPeriod(t *testing.T) {
	for _, period := range []Period{PeriodAccumulate, PeriodDaily, PeriodWeekly, PeriodSeason} {
		policy := Policy{ItemID: 490004806, Scope: ScopeAccount, Period: period, Limit: 10}
		itemID, got, ok := ParseCountKey(CountKey(policy))
		if !ok || itemID != policy.ItemID || got != period {
			t.Fatalf("period=%s key=%s -> ok=%t item=%d period=%s", period, CountKey(policy), ok, itemID, got)
		}
		if _, _, ok := ParseCountKey(WindowKey(policy)); ok {
			t.Fatalf("window key %s should not parse as a count key", WindowKey(policy))
		}
	}
}

func TestUsedCountsEnumeratesBothScopesAndSkipsExpired(t *testing.T) {
	accountPolicy := Policy{ItemID: 490004806, Scope: ScopeAccount, Period: PeriodWeekly, Limit: 10}
	characterPolicy := Policy{ItemID: 10154547, Scope: ScopeCharacter, Period: PeriodDaily, Limit: 1}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, serviceCalendar)
	stats := map[string]int64{CountKey(characterPolicy): 1, WindowKey(characterPolicy): accountPolicy.Window(now).Start.Unix()}
	metadata := map[string]string{
		CountKey(accountPolicy):  "3",
		WindowKey(accountPolicy): strconv.FormatInt(accountPolicy.Window(now).Start.Unix(), 10),
	}
	// 角色侧窗口写成别的周 → 视为过期，不再出现在枚举里。
	stats[WindowKey(characterPolicy)] = now.AddDate(0, 0, -30).Unix()
	entries := UsedCounts(stats, metadata, now)
	if len(entries) != 1 {
		t.Fatalf("entries=%+v", entries)
	}
	if entries[0].ItemID != 490004806 || entries[0].Scope != ScopeAccount || entries[0].Used != 3 {
		t.Fatalf("entry=%+v", entries[0])
	}
	itemID, period, ok := ParseCountKey(CountKey(accountPolicy))
	if !ok || itemID != 490004806 || period != PeriodWeekly {
		t.Fatalf("ParseCountKey ok=%t item=%d period=%s", ok, itemID, period)
	}
}
