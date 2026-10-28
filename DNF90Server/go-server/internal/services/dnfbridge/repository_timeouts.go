package dnfbridge

import "time"

const (
	createWriteTimeout = 3 * time.Second
	// Dungeon pickup commits must remain durable before the client receives the
	// native scene-item removal. SQLite deliberately serializes every mutation
	// through one writer gate; a busy but healthy writer can therefore outlive
	// the ordinary interaction deadline. Keep this scope narrow: scene pickup
	// waits longer rather than reverting the ground object after exactly three
	// seconds, while still retaining a bounded failure path.
	currentDungeonPickupWriteTimeout = 12 * time.Second
	// Consumables are interactive and must always reach a success or failure
	// ACK. A healthy SQLite writer may briefly be busy with its periodic durable
	// checkpoint, so this route gets the same bounded wait as scene pickup.
	currentConsumableWriteTimeout = 12 * time.Second
	// Selected-character initialization builds several real repository
	// snapshots before the first scene is usable. It must not share the short
	// mutation/write deadline: a cold PVF/catalog or MySQL pool wakeup can
	// otherwise cancel the complete actor-bound container bootstrap.
	currentRepositorySnapshotTimeout   = 15 * time.Second
	currentItemListPVFReconcileTimeout = 2 * time.Second
)
