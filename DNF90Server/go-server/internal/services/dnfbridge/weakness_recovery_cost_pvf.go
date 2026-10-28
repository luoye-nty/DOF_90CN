// weakness_recovery_cost_pvf.go reads the gold price of clearing 虚弱 from the
// runtime PVF rather than hard-coding a curve.
//
//	etc/serverparameter.etc
//	[stamina recovery cost]
//	0 0 ... 0 5000 5210 5420 ... 61780       (one entry per level)
//	[/stamina recovery cost]
//
// The row is indexed by character level, one entry per level starting at 1, and
// the first nineteen are zero - the live table only starts charging at level
// 20, which is also where the penalty first matters. An out-of-range level
// clamps into the table instead of failing the recovery.
package dnfbridge

import (
	"errors"
	"fmt"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	currentWeaknessCostArchivePath = "etc/serverparameter.etc"
	currentWeaknessCostSection     = "stamina recovery cost"
)

var (
	errCurrentWeaknessCostUnavailable = errors.New("current weakness recovery cost table is unavailable")
	errCurrentWeaknessCostShape       = errors.New("current weakness recovery cost table is invalid")
)

type currentWeaknessCostTable struct {
	basePrices []int64
}

func (s *Service) currentWeaknessCostTable() (*currentWeaknessCostTable, error) {
	if s == nil {
		return nil, errCurrentWeaknessCostUnavailable
	}
	s.weaknessCostMu.Lock()
	defer s.weaknessCostMu.Unlock()
	if s.weaknessCostTable != nil {
		return s.weaknessCostTable, nil
	}
	if s.weaknessCostErr != nil {
		return nil, s.weaknessCostErr
	}
	itemCatalog, err := s.currentPVFItemCatalog()
	if err != nil {
		s.weaknessCostErr = err
		return nil, err
	}
	if itemCatalog == nil || itemCatalog.source == nil {
		s.weaknessCostErr = errCurrentWeaknessCostUnavailable
		return nil, s.weaknessCostErr
	}
	table, err := parseCurrentWeaknessCostTable(itemCatalog.source)
	if err != nil {
		s.weaknessCostErr = err
		return nil, err
	}
	s.weaknessCostTable = table
	return table, nil
}

func parseCurrentWeaknessCostTable(source dnfpvf.Source) (*currentWeaknessCostTable, error) {
	if source == nil {
		return nil, errCurrentWeaknessCostUnavailable
	}
	document, err := parseDungeonCardPVFDocument(source, currentWeaknessCostArchivePath)
	if err != nil {
		return nil, errors.Join(errCurrentWeaknessCostUnavailable, err)
	}
	prices := document.Ints(currentWeaknessCostSection)
	if len(prices) == 0 {
		return nil, fmt.Errorf("%w: section=[%s] is empty",
			errCurrentWeaknessCostShape, currentWeaknessCostSection)
	}
	for index, price := range prices {
		if price < 0 {
			return nil, fmt.Errorf("%w: section=[%s] index=%d price=%d",
				errCurrentWeaknessCostShape, currentWeaknessCostSection, index, price)
		}
	}
	return &currentWeaknessCostTable{basePrices: prices}, nil
}

// basePrice returns the level's base price, clamping instead of failing so a
// level outside the shipped table can still recover.
func (t *currentWeaknessCostTable) basePrice(level int64) int64 {
	if t == nil || len(t.basePrices) == 0 {
		return 0
	}
	index := level - 1
	if index < 0 {
		index = 0
	}
	if index >= int64(len(t.basePrices)) {
		index = int64(len(t.basePrices)) - 1
	}
	return t.basePrices[index]
}

// recoveryCost prices one full clear: the base price scaled by how far the
// condition fell below healthy, over the 90-point span the ten penalty levels
// cover (100 - 10*9 = 10).
func (t *currentWeaknessCostTable) recoveryCost(level int64, characterLevel int64) int64 {
	if t == nil || level <= 0 {
		return 0
	}
	if level > currentWeaknessMaximumLevel {
		level = currentWeaknessMaximumLevel
	}
	lost := currentWeaknessHealthyValue - currentWeaknessCondition(level)
	cost := t.basePrice(characterLevel) * lost / (currentWeaknessMaximumLevel * currentWeaknessConditionStep)
	if cost < 0 {
		return 0
	}
	return cost
}
