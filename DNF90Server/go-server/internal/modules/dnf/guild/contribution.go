package guild

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	ContributionCoinID     uint32 = 490003262
	ContributionDungeon           = "guild dungeon clear"
	ContributionChat              = "guild chat"
	ContributionAttendance        = "guild attendance"
	ContributionParty             = "party dungeon clear"
)

var ErrContributionInventoryFull = errors.New("guild contribution coin inventory is full")

type ContributionRule struct{ Required, Points uint32 }
type ContributionCatalog map[string]ContributionRule

// Point amounts and activity parameters come exclusively from the deployed PVF.
func LoadContributionCatalog(source dnfpvf.Source) (ContributionCatalog, error) {
	doc, err := readWarDocument(source, "etc/chn_hongbao/chn_hongbao.etc")
	if err != nil {
		return nil, err
	}
	t, ok := doc.Section("hongbao point reward")
	if !ok || len(t) != 12 {
		return nil, ErrInvalidContent
	}
	c := make(ContributionCatalog)
	for i := 0; i < len(t); i += 3 {
		kind := t[i].Value
		switch kind {
		case ContributionDungeon, ContributionChat, ContributionAttendance, ContributionParty:
		default:
			return nil, ErrInvalidContent
		}
		v, e := warUnsigned(t[i+1 : i+3])
		if e != nil || len(v) != 2 || v[0] == 0 || v[1] == 0 || v[0] > 10000 || v[1] > 10000 || c[kind].Required != 0 {
			return nil, ErrInvalidContent
		}
		c[kind] = ContributionRule{v[0], v[1]}
	}
	return c, nil
}

// These economy choices are local server policy, not fields inferred from PVF.
type ContributionPolicy struct {
	CoinsPerPoint            uint32 `json:"coins_per_point"`
	RewardsPerActivityPerDay uint32 `json:"rewards_per_activity_per_day"`
	ResetHour                int    `json:"reset_hour"`
}

const ContributionPolicyScope = "global:guild_contribution_policy:v1"

func LoadContributionPolicy(ctx context.Context, repo dnfrepo.SettingsRepository) (*ContributionPolicy, error) {
	if repo == nil {
		return nil, ErrRepositoryUnavailable
	}
	r, found, err := repo.Load(ctx, ContributionPolicyScope)
	if err != nil || !found {
		return nil, err
	}
	var p ContributionPolicy
	if err = json.Unmarshal([]byte(r.Values["state"]), &p); err != nil {
		return nil, err
	}
	if !p.valid() {
		return nil, ErrInvalidContent
	}
	return &p, nil
}
func SaveContributionPolicy(ctx context.Context, repo dnfrepo.SettingsRepository, p ContributionPolicy, now time.Time) error {
	if repo == nil || !p.valid() {
		return ErrInvalidContent
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return repo.Save(ctx, dnfrepo.SettingsRecord{Scope: ContributionPolicyScope, Values: map[string]string{"state": string(b)}, UpdatedAt: now})
}
func (p ContributionPolicy) valid() bool {
	return p.CoinsPerPoint > 0 && p.CoinsPerPoint <= 10000 && p.RewardsPerActivityPerDay > 0 && p.RewardsPerActivityPerDay <= 100 && p.ResetHour >= 0 && p.ResetHour < 24
}
func (p ContributionPolicy) Day(now time.Time) time.Time {
	n := now.In(time.FixedZone("CST", 8*3600))
	d := time.Date(n.Year(), n.Month(), n.Day(), p.ResetHour, 0, 0, 0, n.Location())
	if n.Before(d) {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// The character ledger survives leaving a guild: rejoining does not reset daily
// limits or erase earned, undelivered coins. It never imports guild Mileage.
type ContributionState struct {
	Version      int               `json:"version"`
	DayStart     int64             `json:"day_start"`
	Progress     map[string]uint32 `json:"progress,omitempty"`
	Rewarded     map[string]uint32 `json:"rewarded,omitempty"`
	Receipts     map[string]bool   `json:"receipts,omitempty"`
	Points       uint64            `json:"points"`
	PendingCoins uint32            `json:"pending_coins"`
	PaidCoins    uint64            `json:"paid_coins"`
}

func contributionScope(id string) string { return "character:" + id + ":guild_contribution:v1" }
func LoadContribution(ctx context.Context, repo dnfrepo.SettingsRepository, id string) (ContributionState, error) {
	s := ContributionState{Version: 1}
	if repo == nil || id == "" {
		return s, ErrRepositoryUnavailable
	}
	r, found, err := repo.Load(ctx, contributionScope(id))
	if err != nil {
		return s, err
	}
	if found && r.Values["state"] != "" {
		if err = json.Unmarshal([]byte(r.Values["state"]), &s); err != nil {
			return s, err
		}
		if s.Version != 1 {
			return s, ErrInvalidContent
		}
	}
	return s, nil
}
func SaveContribution(ctx context.Context, repo dnfrepo.SettingsRepository, id string, s ContributionState, now time.Time) error {
	if repo == nil || id == "" || s.Version != 1 {
		return ErrRepositoryUnavailable
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return repo.Save(ctx, dnfrepo.SettingsRecord{Scope: contributionScope(id), Values: map[string]string{"state": string(b)}, UpdatedAt: now})
}

// Observe accepts server-owned events. The party parameter is the distinct
// same-guild participant count; other activities count successful occurrences.
func (s *ContributionState) Observe(c ContributionCatalog, p ContributionPolicy, kind, receipt string, partyMembers uint32, occurredAt, now time.Time) (uint32, error) {
	rule, ok := c[kind]
	if s == nil || !p.valid() || !ok || rule.Required == 0 || rule.Points == 0 || receipt == "" || occurredAt.After(now) {
		return 0, ErrInvalidContent
	}
	day := p.Day(now).Unix()
	if occurredAt.Unix() < day || s.DayStart > day {
		return 0, nil
	}
	if s.DayStart != day {
		s.DayStart = day
		s.Progress = nil
		s.Rewarded = nil
		s.Receipts = nil
	}
	if s.Progress == nil {
		s.Progress = make(map[string]uint32)
	}
	if s.Rewarded == nil {
		s.Rewarded = make(map[string]uint32)
	}
	if s.Receipts == nil {
		s.Receipts = make(map[string]bool)
	}
	key := kind + ":" + receipt
	if s.Receipts[key] || s.Rewarded[kind] >= p.RewardsPerActivityPerDay {
		return 0, nil
	}
	if kind == ContributionParty && partyMembers < rule.Required {
		return 0, nil
	}
	if len(s.Receipts) >= 10000 {
		return 0, ErrInvalidContent
	}
	s.Receipts[key] = true
	s.Progress[kind]++
	required := rule.Required
	if kind == ContributionParty {
		required = 1
	}
	if s.Progress[kind] < required {
		return 0, nil
	}
	coins := uint64(rule.Points) * uint64(p.CoinsPerPoint)
	if coins > math.MaxUint32-uint64(s.PendingCoins) || s.Points > math.MaxUint64-uint64(rule.Points) {
		return 0, ErrInvalidContent
	}
	s.Progress[kind] = 0
	s.Rewarded[kind]++
	s.Points += uint64(rule.Points)
	s.PendingCoins += uint32(coins)
	s.Version = 1
	return rule.Points, nil
}

// Deliver is called within the same repository transaction as the ledger save.
// A grant may partially modify its input before reporting full; clone first.
func (s *ContributionState) Deliver(inventory *dnfrepo.InventoryRecord, grant func(*dnfrepo.InventoryRecord, uint32) error) (bool, error) {
	if s.PendingCoins == 0 {
		return false, nil
	}
	if inventory == nil || grant == nil || s.PaidCoins > math.MaxUint64-uint64(s.PendingCoins) {
		return false, ErrInvalidContent
	}
	next := dnfrepo.CloneInventory(*inventory)
	if err := grant(&next, s.PendingCoins); err != nil {
		if errors.Is(err, ErrContributionInventoryFull) {
			return false, nil
		}
		return false, err
	}
	*inventory = next
	s.PaidCoins += uint64(s.PendingCoins)
	s.PendingCoins = 0
	return true, nil
}

type ContributionEntry struct {
	GuildID     uint32
	CharacterID string
	Members     map[string]time.Time
}

func (r Registry) ContributionEntry(id string) ContributionEntry {
	g, _, ok := r.GuildForCharacter(id)
	if !ok {
		return ContributionEntry{}
	}
	e := ContributionEntry{GuildID: g.ID, CharacterID: id, Members: make(map[string]time.Time, len(g.Members))}
	for id, m := range g.Members {
		e.Members[id] = m.JoinedAt
	}
	return e
}
func (r Registry) ContributionEligible(e ContributionEntry, party []string) (bool, uint32) {
	g, m, ok := r.GuildForCharacter(e.CharacterID)
	if !ok || g.ID != e.GuildID || !m.JoinedAt.Equal(e.Members[e.CharacterID]) {
		return false, 0
	}
	seen := make(map[string]bool)
	for _, id := range party {
		member, ok := g.Members[id]
		joined, entered := e.Members[id]
		if ok && entered && member.JoinedAt.Equal(joined) {
			seen[id] = true
		}
	}
	if !seen[e.CharacterID] {
		return true, 0
	}
	return true, uint32(len(seen))
}
func (r *Registry) AddContributionPoints(id string, points uint32, now time.Time) error {
	g, m, ok := r.GuildForCharacter(id)
	if !ok {
		return ErrNotMember
	}
	if points > math.MaxUint32-m.ContributionPoints {
		return ErrInvalidContent
	}
	m.ContributionPoints += points
	g.Members[id] = m
	g.UpdatedAt = now
	return nil
}
