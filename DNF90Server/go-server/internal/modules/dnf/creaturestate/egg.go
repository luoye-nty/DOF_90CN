package creaturestate

import "sync"

// petEggProbe is installed by dnfbridge with a runtime-PVF backed predicate
// (an egg is the only creature item that carries an `[output index]` section).
//
// Leaving it unset means "cannot prove this is an egg", which keeps the
// pre-existing behaviour exactly -- isolated package tests and any caller that
// was never wired stay on the old path instead of silently changing meaning.
var (
	petEggProbeMu sync.RWMutex
	petEggProbe   func(itemID int64) bool
)

// SetPetEggProbe installs the process-wide egg predicate. dnfbridge calls this
// once when it first routes an aligned command; later installs replace it.
func SetPetEggProbe(probe func(itemID int64) bool) {
	petEggProbeMu.Lock()
	petEggProbe = probe
	petEggProbeMu.Unlock()
}

// IsPetEgg reports whether an item template is a pet egg.
//
// An egg is NOT a pet: it owns no pet record and must not carry a creature
// serial. Treating one as a directly granted creature used to mint a pet entry
// for it (and stamp creature_serial_or_handle on the row), after which the
// item-delete gate saw a "live pet binding" and refused -- the field report
// 「宠物蛋无法丢弃」. It also matters that a real hatched pet never has an
// `[output index]`, so this predicate cannot accidentally free a live pet.
func IsPetEgg(itemID int64) bool {
	petEggProbeMu.RLock()
	probe := petEggProbe
	petEggProbeMu.RUnlock()
	return probe != nil && itemID > 0 && probe(itemID)
}
