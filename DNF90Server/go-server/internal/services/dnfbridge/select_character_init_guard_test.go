package dnfbridge

import "testing"

func TestCurrentSelectCharacterInitGuardAllowsOnlyOneLoadPerLifecycle(t *testing.T) {
	session := &gameSession{}
	if beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("first selection was treated as a duplicate")
	}
	if !beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("same-lifecycle selection was not suppressed")
	}

	resetCurrentSelectCharacterInit(session)
	if beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("selection after op7 reset was treated as a duplicate")
	}
	if !beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("second duplicate in the new lifecycle was not suppressed")
	}
}

func TestCurrentSelectCharacterInitGuardSwitchesCharacters(t *testing.T) {
	session := &gameSession{}
	if beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("first selection was treated as a duplicate")
	}
	if beginCurrentSelectCharacterInit(session, 47) {
		t.Fatal("different character was treated as a duplicate")
	}
	if !beginCurrentSelectCharacterInit(session, 47) {
		t.Fatal("duplicate after character switch was not suppressed")
	}
}

func TestCurrentSelectCharacterInitGuardAbortAllowsRetry(t *testing.T) {
	session := &gameSession{}
	if beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("first selection was treated as a duplicate")
	}
	abortCurrentSelectCharacterInit(session, 46)
	if beginCurrentSelectCharacterInit(session, 46) {
		t.Fatal("retry after failed initialization was treated as a duplicate")
	}
}
