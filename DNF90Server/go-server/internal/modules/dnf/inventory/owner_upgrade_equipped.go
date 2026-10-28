package inventory

import (
	"errors"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// ErrUpgradeEquipmentOwnerUnavailable is returned when an op50 request names a
// worn equipment slot (mode word high byte 3) but the owner has no equipment
// repository in scope. Both upgrade transaction wrappers provide one, so this
// only fires for a directly constructed owner.
var ErrUpgradeEquipmentOwnerUnavailable = errors.New("equipment owner unavailable for worn-slot upgrade target")

// equippedUpgradeLevel reads the packed upgrade level of a worn entry through
// the same projection the owners use, so the handler's pre-transaction PVF
// policy lookup sees the level the transaction will act on.
func equippedUpgradeLevel(entry dnfrepo.EquipmentEntry) byte {
	return upgradeLevelOf(forgingStackFromEquipmentEntry(entry))
}
