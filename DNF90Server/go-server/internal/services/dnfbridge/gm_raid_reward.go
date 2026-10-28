package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// GMRaidRewardPoolEntry is one selectable row in a raid reward pool.
// Weight is always the original PVF weight; the GM multiplier is applied
// separately at settlement time.
type GMRaidRewardPoolEntry struct {
	Raid             string `json:"raid"`
	PhaseIndex       int    `json:"phase_index"`
	Kind             string `json:"kind"`
	DefinitionItemID uint32 `json:"definition_item_id"`
	Weight           uint32 `json:"weight"`
	Name             string `json:"name,omitempty"`
	Key              string `json:"key"`
}

type GMRaidRewardPoolGroup struct {
	Raid        string                  `json:"raid"`
	PhaseIndex  int                     `json:"phase_index"`
	Kind        string                  `json:"kind"`
	TotalWeight uint64                  `json:"total_weight"`
	Entries     []GMRaidRewardPoolEntry `json:"entries"`
}

type GMRaidRewardOverrides struct {
	Weights map[string]uint32 `json:"weights"`
	Items   map[string]uint32 `json:"items"`
}

type GMRaidRewardPools struct {
	Groups    []GMRaidRewardPoolGroup `json:"groups"`
	Overrides GMRaidRewardOverrides   `json:"overrides"`
	Notes     []string                `json:"notes,omitempty"`
}

type GMRaidRewardRate struct {
	Weights map[string]uint32 `json:"weights"`
	Items   map[string]uint32 `json:"items"`
}

type raidRewardCatalogSource struct {
	raid string
	load func() (*antonRewardPVFCatalog, error)
}

func (s *Service) raidRewardCatalogSources() []raidRewardCatalogSource {
	// These are the same loader functions used by settlement. Anton's definition
	// IDs therefore come from the current anton.etc just like Luke and Fiend War;
	// the GM page must not carry a separate hard-coded definition list.
	return []raidRewardCatalogSource{
		{raid: "anton", load: s.runtimeRaidAntonRewardCatalog},
		{raid: "luke", load: s.runtimeRaidLukeRewardCatalog},
		{raid: "fiendwar", load: s.runtimeRaidFiendWarRewardCatalog},
	}
}

func raidRewardDisplayName(raid string) string {
	switch normalizeRaidRewardRaid(raid) {
	case "anton":
		return "安徒恩"
	case "luke":
		return "卢克"
	case "fiendwar":
		return "超时空"
	default:
		return raid
	}
}

// GMRaidRewardPools returns all three raid catalogs plus the current live
// overrides. A catalog that cannot be loaded is reported as a note so the
// admin page can still show the other raids.
func (s *Service) GMRaidRewardPools(ctx context.Context) (GMRaidRewardPools, error) {
	if s == nil {
		return GMRaidRewardPools{}, fmt.Errorf("dnf90 GM service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	groups := make([]GMRaidRewardPoolGroup, 0, 16)
	notes := make([]string, 0, 4)
	for _, source := range s.raidRewardCatalogSources() {
		if err := ctx.Err(); err != nil {
			return GMRaidRewardPools{}, err
		}
		catalog, err := source.load()
		if err != nil {
			notes = append(notes, fmt.Sprintf("读取%s奖励池失败：%v", raidRewardDisplayName(source.raid), err))
			continue
		}
		for _, pool := range catalog.Pools() {
			group := GMRaidRewardPoolGroup{
				Raid:        source.raid,
				PhaseIndex:  pool.PhaseIndex,
				Kind:        pool.Kind,
				TotalWeight: pool.TotalWeight,
				Entries:     make([]GMRaidRewardPoolEntry, 0, len(pool.Entries)),
			}
			for index, entry := range pool.Entries {
				group.Entries = append(group.Entries, GMRaidRewardPoolEntry{
					Raid:             source.raid,
					PhaseIndex:       pool.PhaseIndex,
					Kind:             pool.Kind,
					DefinitionItemID: entry.DefinitionItemID,
					Weight:           entry.Weight,
					Name:             catalog.ItemName(entry.DefinitionItemID),
					Key:              raidRewardPoolKey(source.raid, pool.PhaseIndex, pool.Kind, entry.DefinitionItemID, index),
				})
			}
			groups = append(groups, group)
		}
	}
	raidOrder := map[string]int{"anton": 0, "luke": 1, "fiendwar": 2}
	sort.SliceStable(groups, func(i, j int) bool {
		left, right := groups[i], groups[j]
		if raidOrder[left.Raid] != raidOrder[right.Raid] {
			return raidOrder[left.Raid] < raidOrder[right.Raid]
		}
		if left.PhaseIndex != right.PhaseIndex {
			return left.PhaseIndex < right.PhaseIndex
		}
		return strings.Compare(left.Kind, right.Kind) < 0
	})
	weights, items := s.raidRewardOverrideTable().snapshot()
	notes = append(notes, "权重覆盖只保存在内存；重启 dnf90server 后恢复 PVF 原值。")
	return GMRaidRewardPools{
		Groups: groups,
		Overrides: GMRaidRewardOverrides{
			Weights: weights,
			Items:   items,
		},
		Notes: notes,
	}, nil
}

func (s *Service) GMRaidRewardRate(ctx context.Context) (GMRaidRewardRate, error) {
	if s == nil {
		return GMRaidRewardRate{}, fmt.Errorf("dnf90 GM service is unavailable")
	}
	weights, items := s.raidRewardOverrideTable().snapshot()
	return GMRaidRewardRate{Weights: weights, Items: items}, nil
}

// GMSetRaidRewardRate replaces the whole in-memory override table. It validates
// every pool and definition first, so a typo cannot silently make a raid
// settlement pool empty (which would block reward publishing).
func (s *Service) GMSetRaidRewardRate(ctx context.Context, request GMRaidRewardRate) (GMRaidRewardRate, error) {
	if s == nil {
		return GMRaidRewardRate{}, fmt.Errorf("dnf90 GM service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateRaidRewardOverrides(request.Weights, request.Items); err != nil {
		return GMRaidRewardRate{}, err
	}
	catalogs := make([]*antonRewardPVFCatalog, 0, 3)
	for _, source := range s.raidRewardCatalogSources() {
		if err := ctx.Err(); err != nil {
			return GMRaidRewardRate{}, err
		}
		catalog, err := source.load()
		if err != nil {
			return GMRaidRewardRate{}, fmt.Errorf("读取%s奖励池失败，未保存任何修改：%w", raidRewardDisplayName(source.raid), err)
		}
		catalogs = append(catalogs, catalog)
	}
	for _, catalog := range catalogs {
		if err := catalog.validateRaidRewardOverrides(request.Weights, request.Items); err != nil {
			return GMRaidRewardRate{}, err
		}
	}
	s.raidRewardOverrideTable().replace(request.Weights, request.Items)
	return s.GMRaidRewardRate(ctx)
}
