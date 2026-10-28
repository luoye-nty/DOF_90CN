package dnfbridge

import "testing"

// consumeBackToVillageEnterSelectFinalizerAfterTownPosition is the op35-side
// half the old tree added and dnf-source was missing. In dnf-source the
// back-to-village selector-return marker used to be cleared only by op15, but a
// party follower never walks to the gate and so never sends op15;
// currentPartyTownReturnSceneReady gates on this field, so a follower's marker
// stayed set forever, the party co-presence rebuild never fired, and the
// leader's first gate op15 failed to finalize the return - the reported "walk
// to the entrance twice", reproduced only in a party. A town-side op35 (which
// every member sends on arrival) now consumes it.
func TestConsumeBackToVillageFinalizerClearsMarkerAndUnblocksPartyGate(t *testing.T) {
	service := &Service{}
	session := &gameSession{selectedCharacterID: 42}
	session.backToVillageEnterSelectPending = true
	session.townSceneReadyCharacterID = 42
	session.townPositionSnapshot = currentTownPositionSnapshot{
		CharacterID:   42,
		TownID:        7,
		AreaID:        3,
		PositionValid: true,
	}

	// While the marker is set the party-return scene gate is closed.
	if currentPartyTownReturnSceneReady(session) {
		t.Fatal("party return gate should be closed while the finalizer marker is set")
	}

	if err := service.consumeBackToVillageEnterSelectFinalizerAfterTownPosition(session, "test"); err != nil {
		t.Fatal(err)
	}
	if session.backToVillageEnterSelectPending {
		t.Fatal("consumer did not clear the marker")
	}
	// With the marker cleared and readiness present, the gate now passes.
	if !currentPartyTownReturnSceneReady(session) {
		t.Fatalf("party return gate still closed after consuming the marker: ready_char=%d snapshot=%+v",
			session.townSceneReadyCharacterID, session.townPositionSnapshot)
	}

	// A second call on an already-clear marker is inert (solo/no-party rebuild
	// is a no-op).
	if err := service.consumeBackToVillageEnterSelectFinalizerAfterTownPosition(session, "test_again"); err != nil {
		t.Fatalf("second call on a clear marker was not inert: %v", err)
	}
}
