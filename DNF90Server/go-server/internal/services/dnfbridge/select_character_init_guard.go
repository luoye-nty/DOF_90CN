package dnfbridge

// beginCurrentSelectCharacterInit claims the current character-selection
// lifecycle. A repeated msg=4 for the same character is a retry of the active
// lifecycle and must not rebuild the scene stream a second time. A different
// character replaces the claim and starts a fresh lifecycle.
func beginCurrentSelectCharacterInit(session *gameSession, characterID uint16) bool {
	if session == nil || characterID == 0 {
		return false
	}
	session.selectCharacterInitMu.Lock()
	defer session.selectCharacterInitMu.Unlock()
	if session.selectCharacterInitActive && session.selectCharacterInitCharacterID == characterID {
		return true
	}
	session.selectCharacterInitActive = true
	session.selectCharacterInitCharacterID = characterID
	return false
}

func abortCurrentSelectCharacterInit(session *gameSession, characterID uint16) {
	if session == nil {
		return
	}
	session.selectCharacterInitMu.Lock()
	if session.selectCharacterInitActive && session.selectCharacterInitCharacterID == characterID {
		session.selectCharacterInitActive = false
		session.selectCharacterInitCharacterID = 0
	}
	session.selectCharacterInitMu.Unlock()
}

func resetCurrentSelectCharacterInit(session *gameSession) {
	if session == nil {
		return
	}
	session.selectCharacterInitMu.Lock()
	session.selectCharacterInitActive = false
	session.selectCharacterInitCharacterID = 0
	session.selectCharacterInitMu.Unlock()
}
