package dnfbridge

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

const raidRewardOverrideMaxPercent = 1_000_000

// raidRewardOverrides is the process-local GM override table for raid reward
// weights. Empty maps mean "use the PVF values exactly"; the table is not
// persisted, so restarting dnf90server always returns to the source values.
type raidRewardOverrides struct {
	mu      sync.RWMutex
	weights map[string]uint32
	items   map[string]uint32
}

func newRaidRewardOverrides() *raidRewardOverrides {
	return &raidRewardOverrides{
		weights: make(map[string]uint32),
		items:   make(map[string]uint32),
	}
}

func (s *Service) raidRewardOverrideTable() *raidRewardOverrides {
	if s == nil {
		return nil
	}
	if s.raidRewardOverrides != nil {
		return s.raidRewardOverrides
	}
	s.raidRewardOverridesMu.Lock()
	defer s.raidRewardOverridesMu.Unlock()
	if s.raidRewardOverrides == nil {
		s.raidRewardOverrides = newRaidRewardOverrides()
	}
	return s.raidRewardOverrides
}

func (s *Service) attachRaidRewardOverrides(catalog *antonRewardPVFCatalog, raid string) {
	if s == nil || catalog == nil {
		return
	}
	catalog.raid = normalizeRaidRewardRaid(raid)
	catalog.overrides = s.raidRewardOverrideTable()
}

func (o *raidRewardOverrides) replace(weights, items map[string]uint32) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.weights = cloneRaidRewardOverrides(weights)
	o.items = cloneRaidRewardOverrides(items)
}

func (o *raidRewardOverrides) snapshot() (map[string]uint32, map[string]uint32) {
	if o == nil {
		return map[string]uint32{}, map[string]uint32{}
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return cloneRaidRewardOverrides(o.weights), cloneRaidRewardOverrides(o.items)
}

func (o *raidRewardOverrides) poolPercent(raid string, phase int, kind string, definitionID uint32, index int) (uint32, bool) {
	if o == nil {
		return 0, false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return lookupRaidRewardPercent(o.weights,
		raidRewardPoolKey(raid, phase, kind, definitionID, index),
		raidRewardPoolWildcardKey(raid, phase, kind, definitionID),
	)
}

func (o *raidRewardOverrides) itemPercent(raid string, definitionID, itemID uint32, index int) (uint32, bool) {
	if o == nil {
		return 0, false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return lookupRaidRewardPercent(o.items,
		raidRewardItemKey(raid, definitionID, itemID, index),
		raidRewardItemWildcardKey(raid, definitionID, itemID),
		"D|"+strconv.FormatUint(uint64(definitionID), 10)+"|"+strconv.FormatUint(uint64(itemID), 10),
	)
}

func cloneRaidRewardOverrides(source map[string]uint32) map[string]uint32 {
	out := make(map[string]uint32, len(source))
	for key, value := range source {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = value
	}
	return out
}

func lookupRaidRewardPercent(values map[string]uint32, keys ...string) (uint32, bool) {
	for _, key := range keys {
		if percent, ok := values[key]; ok {
			return percent, true
		}
	}
	return 0, false
}

func raidRewardPoolKey(raid string, phase int, kind string, definitionID uint32, index int) string {
	return fmt.Sprintf("P|%s|%d|%s|%d|%d",
		normalizeRaidRewardRaid(raid), phase, normalizeRaidRewardKind(kind), definitionID, index)
}

func raidRewardPoolWildcardKey(raid string, phase int, kind string, definitionID uint32) string {
	return fmt.Sprintf("P|%s|%d|%s|%d",
		normalizeRaidRewardRaid(raid), phase, normalizeRaidRewardKind(kind), definitionID)
}

func raidRewardItemKey(raid string, definitionID, itemID uint32, index int) string {
	return fmt.Sprintf("D|%s|%d|%d|%d",
		normalizeRaidRewardRaid(raid), definitionID, itemID, index)
}

func raidRewardItemWildcardKey(raid string, definitionID, itemID uint32) string {
	return fmt.Sprintf("D|%s|%d|%d",
		normalizeRaidRewardRaid(raid), definitionID, itemID)
}

func normalizeRaidRewardRaid(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeRaidRewardKind(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validateRaidRewardOverrides(weights, items map[string]uint32) error {
	for key, percent := range weights {
		if !strings.HasPrefix(strings.TrimSpace(key), "P|") {
			return fmt.Errorf("权重键必须以 P| 开头: %q", key)
		}
		if percent > raidRewardOverrideMaxPercent {
			return fmt.Errorf("权重 %s 超出范围 0~%d", key, raidRewardOverrideMaxPercent)
		}
	}
	for key, percent := range items {
		if !strings.HasPrefix(strings.TrimSpace(key), "D|") {
			return fmt.Errorf("道具权重键必须以 D| 开头: %q", key)
		}
		if percent > raidRewardOverrideMaxPercent {
			return fmt.Errorf("道具权重 %s 超出范围 0~%d", key, raidRewardOverrideMaxPercent)
		}
	}
	return nil
}

func (c *antonRewardPVFCatalog) validateRaidRewardOverrides(weights, items map[string]uint32) error {
	if c == nil {
		return nil
	}
	candidate := &raidRewardOverrides{weights: weights, items: items}
	for _, pool := range c.Pools() {
		var total uint64
		for index, entry := range pool.Entries {
			weight := uint64(entry.Weight)
			if percent, ok := candidate.poolPercent(c.raid, pool.PhaseIndex, pool.Kind, entry.DefinitionItemID, index); ok {
				weight = weight * uint64(percent) / 100
			}
			total += weight
		}
		if total == 0 {
			return fmt.Errorf("团本 %s 阶段 %d 的奖池 %s 在覆盖后总权重为 0，请至少保留一个非零条目",
				c.raid, pool.PhaseIndex+1, pool.Kind)
		}
	}
	for definitionID, definition := range c.definitions {
		if definition.DirectFinal {
			continue
		}
		var total uint64
		for index, entry := range definition.Entries {
			weight := uint64(entry.Weight)
			if percent, ok := candidate.itemPercent(c.raid, definitionID, entry.ItemID, index); ok {
				weight = weight * uint64(percent) / 100
			}
			total += weight
		}
		if total == 0 {
			return fmt.Errorf("团本 %s 奖励盒 %d 在覆盖后总权重为 0，请至少保留一个非零条目", c.raid, definitionID)
		}
	}
	return nil
}
