package dnfbridge

func resetRuntimeRaidSessionProjection(session *gameSession) {
	if session == nil {
		return
	}
	session.raidProjectionMu.Lock()
	session.raidRejoinProjectionSent = false
	session.raidRejoinProjectionRaidKey = 0
	session.raidRejoinProjectionRevision = 0
	session.raidEndedProjectionRaidKey = 0
	session.raidProjectionMu.Unlock()
}

func clearRuntimeRaidRejoinProjection(session *gameSession) {
	if session == nil {
		return
	}
	session.raidProjectionMu.Lock()
	session.raidRejoinProjectionSent = false
	session.raidRejoinProjectionRaidKey = 0
	session.raidRejoinProjectionRevision = 0
	session.raidProjectionMu.Unlock()
}

func runtimeRaidRejoinProjectionMatches(session *gameSession, raidKey uint32, revision uint64) bool {
	if session == nil {
		return false
	}
	session.raidProjectionMu.Lock()
	defer session.raidProjectionMu.Unlock()
	return session.raidRejoinProjectionSent &&
		session.raidRejoinProjectionRaidKey == raidKey &&
		session.raidRejoinProjectionRevision == revision
}

func storeRuntimeRaidRejoinProjection(session *gameSession, raidKey uint32, revision uint64) {
	if session == nil {
		return
	}
	session.raidProjectionMu.Lock()
	session.raidRejoinProjectionSent = true
	session.raidRejoinProjectionRaidKey = raidKey
	session.raidRejoinProjectionRevision = revision
	session.raidEndedProjectionRaidKey = 0
	session.raidProjectionMu.Unlock()
}

func storeRuntimeRaidEndedProjection(session *gameSession, raidKey uint32) {
	if session == nil {
		return
	}
	session.raidProjectionMu.Lock()
	session.raidEndedProjectionRaidKey = raidKey
	session.raidProjectionMu.Unlock()
}

func runtimeRaidEndedProjectionMatches(session *gameSession, requestKey uint16) bool {
	if session == nil || requestKey == 0 {
		return false
	}
	session.raidProjectionMu.Lock()
	defer session.raidProjectionMu.Unlock()
	return session.raidEndedProjectionRaidKey != 0 &&
		uint16(session.raidEndedProjectionRaidKey) == requestKey
}
