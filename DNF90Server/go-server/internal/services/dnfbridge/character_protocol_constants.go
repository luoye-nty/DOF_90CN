package dnfbridge

import dnfrepo "longheng.io/server/internal/modules/dnf/repository"

const (
	defaultCharacterSlots     = dnfrepo.DefaultCharacterSlotLimit
	maxCharacterSlots         = dnfrepo.MaxCharacterSlotLimit
	noPackRosterWireSlotLimit = defaultCharacterSlots
	// The account starts with 32 unlocked character slots.  The current EXE's
	// roster transport is separate: mode-2 carries slots 0..35 and op679 asks
	// for each following 36-slot batch through an op2 mode-4 body.
	currentRosterExtensionBatchSize = 36
	latestRosterRouteNormal         = 0
	latestRosterContextNormal       = 0
	currentSceneObjectRoute         = 0
	currentSceneObjectContext       = 0
	upperMaxCharacter               = 0xffff
	latestCharacterStateRoute       = 1
	latestCharacterCreateEnabled    = 1
	latestCharacterStateActive      = 1
	latestCharacterStateNormal      = 0
	rosterNameMaxBytes              = 30
	rosterWideNameUnits             = 31
	noPackRosterPreJobBytes         = 2
	noPackRosterPostGrowBytes       = 2
	rosterLinkedIDBlockSize         = 8
	// Current sub_20026C0 consumes a mandatory type-44 raw block after every
	// equipment item ID. Even an empty block has its u32 length prefix.
	noPackRosterEquipRowBytes  = 19
	noPackRosterPostEquipBytes = 38
	noPackRosterEntryTailBytes = noPackRosterPreJobBytes + 1 + 1 + noPackRosterPostGrowBytes + 1 + 4 + 4 + 1 + noPackRosterPostEquipBytes
	rosterDefaultPageCount     = 1
	createCodeDuplicated       = 0x00
	createCodeSlotFull         = 0x04
	createCodeServerError      = 0x14
	// Current sub_1CFC9C0 silently returns for check-name failure status 0.
	// Status 0x14 selects its visible duplicate-name prompt; the same value is
	// used by the nearest 90US reference after live status-0/2 responses proved
	// silent on the current client.
	checkNameCodeDuplicated = 0x14
)

func currentConnectionTownActorOwnerContext(session *gameSession) byte {
	if session == nil {
		return currentSceneObjectContext
	}
	return session.connectionTownActorOwnerChannel
}

func currentTownActorOwnerContext(session *gameSession) byte {
	if session == nil {
		return currentSceneObjectContext
	}
	return session.townActorOwnerChannel
}

// currentPartySceneOwnerContext is deliberately separate from the selected
// actor owner. The local mode0/mode1 actor uses context 0 in the current client,
// while op9/op11 party records and remote actors are keyed by the connected
// channel. Test/manual sessions may provide an explicit connection owner; live
// account-bound sessions retain the channel in session.channel even though the
// local actor field is zero.
func currentPartySceneOwnerContext(session *gameSession) byte {
	if session == nil {
		return currentSceneObjectContext
	}
	if session.connectionTownActorOwnerChannel != currentSceneObjectContext {
		return session.connectionTownActorOwnerChannel
	}
	if session.channel.ID > 0 && session.channel.ID <= 0xff {
		return byte(session.channel.ID)
	}
	return session.townActorOwnerChannel
}
