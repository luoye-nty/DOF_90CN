package dnfbridge

import (
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	currentInitialTownProgress                        = uint32(36)
	currentInitialTownEntryDelay                      = time.Duration(0)
	currentChannelReconnectProbeSize                  = 31
	observed90CNShortEndpointBodySize                 = 586
	currentChannelReconnectDisplayProbeSize           = 590
	reference90CNChannelReconnectDisplayProbeBodySize = 598
	observed90CNCrackEndpointBodySize                 = 610
	observed90CNShortEndpointMaxBodySize              = 630
	current90CNEndpointShortVariantSize               = 4
	current90CNEndpointAdapterRecordSize              = 32
	current90CNEndpointMaxAdapterRecords              = 32
)

type currentInitialTownRouteStage byte

type currentInitialTownRoutePolicy struct {
	includeReportClientSpec bool
	includeSecondFullMode1  bool
	skipPrerequisiteMode1   bool
	commitTransitionEarly   bool
	earlyActorDisplayBody   []byte
	// actorMode0Body is the exact selected-actor row already written by the
	// route. Cold login intentionally delays its first complete mode1 until the
	// durable skill snapshot is ready; retain this row so that later successful
	// mode1 can form the owner-produced party admission projection. State-only
	// expert-job op205 follows the ordinary op19.
	actorMode0Body []byte
	// The first character-list town route must seed the native clear-quest
	// bitmap after the local position/area notifications. Normal movement and
	// channel reconnect retain their existing policy/order.
	deferClearQuestListUntilLocationNotifications bool
}

const (
	currentInitialTownRouteIdle currentInitialTownRouteStage = iota
	currentInitialTownRouteArmed
	currentInitialTownRouteActionTableSent
	currentInitialTownRouteObjectSent
	currentInitialTownRouteActorBound
	// These cursors are used by the completed-dungeon return route. The skill
	// acquisition reader remains suppressed there; the durable expert-job state
	// is projected only after the retained skill manager and final mode1 are safe.
	currentInitialTownRouteSkillStateSent
	currentInitialTownRouteEquipmentMode1Sent
	currentInitialTownRouteSkinAppearanceRefreshSent
	currentInitialTownRouteBuffRegistrationSent
	currentInitialTownRouteProfessionStateSent
	currentInitialTownRoutePlayerStatePrepared
	// Completed dungeon returns create a fresh town actor but do not run the
	// ordinary post-op24 generation. Keep the dedicated slot-26 creature
	// rebind in the route cursor so a retry cannot publish it twice.
	currentInitialTownRouteCreatureStateSent
	currentInitialTownRouteTransitionSent
	currentInitialTownRoutePlayerStateSent
)

func isCurrentChannelReconnectLifecycleBody(msgID uint16, classification byte, bodyLen int) bool {
	if classification != dnfproto.DefaultChannelClassification {
		return false
	}
	switch dnfenum.UpperMsg(msgID) {
	case dnfenum.UpperMsgGameEndpoint:
		return isCurrentChannelReconnectDisplayProbeBodyLen(bodyLen)
	case dnfenum.UpperMsgCharacterRoster:
		return bodyLen == currentChannelReconnectProbeSize
	default:
		return false
	}
}

func isCurrentChannelReconnectDisplayProbeBodyLen(bodyLen int) bool {
	if bodyLen >= observed90CNShortEndpointBodySize &&
		bodyLen <= observed90CNShortEndpointMaxBodySize &&
		(bodyLen-observed90CNShortEndpointBodySize)%current90CNEndpointShortVariantSize == 0 {
		return true
	}
	return bodyLen >= reference90CNChannelReconnectDisplayProbeBodySize+current90CNEndpointAdapterRecordSize &&
		bodyLen <= reference90CNChannelReconnectDisplayProbeBodySize+current90CNEndpointAdapterRecordSize*current90CNEndpointMaxAdapterRecords &&
		(bodyLen-reference90CNChannelReconnectDisplayProbeBodySize)%current90CNEndpointAdapterRecordSize == 0
}

func markCurrentChannelReconnect(session *gameSession, bodyLen int) bool {
	if session == nil || bodyLen != currentChannelReconnectProbeSize ||
		session.rosterRequested || currentTownSceneReady(session) || currentDungeonSceneActive(session) {
		return false
	}
	armCurrentChannelReconnect(session)
	return true
}

func markCurrentChannelReconnectOnSelection(session *gameSession) bool {
	if session == nil || session.channelReconnect || session.rosterRequested ||
		session.selectedCharacterID != 0 || currentTownSceneReady(session) || currentDungeonSceneActive(session) {
		return false
	}
	armCurrentChannelReconnect(session)
	return true
}

func armCurrentChannelReconnect(session *gameSession) {
	if session == nil {
		return
	}
	session.channelReconnect = true
	session.sceneBootstrapTailDeferred = false
	session.sceneBootstrapTailSent = false
	session.townActorOwnerChannel = currentConnectionTownActorOwnerContext(session)
}

func currentTownSceneReady(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.townMu.Lock()
	ready := session.townSceneReadyCharacterID != 0 ||
		session.initialTownRouteStage >= currentInitialTownRouteTransitionSent
	session.townMu.Unlock()
	return ready
}

func currentDungeonSceneActive(session *gameSession) bool {
	if session == nil {
		return false
	}
	session.dungeon.mu.Lock()
	active := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	return active
}

func (s *Service) resetCurrentInitialTownRoute(session *gameSession) {
	if session == nil {
		return
	}
	session.townMu.Lock()
	resetCurrentTownPostTransitionPlayerState(session)
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.initialTownGuardianGemRefreshSent = false
	session.initialTownGuardianShieldDeckSent = false
	session.initialTownGuardianShieldAppearanceSent = false
	session.chatEmoticonCatalogSent = false
	session.initialTownActorSceneSnapshotSent = false
	session.initialTownActorDisplaySent = false
	session.initialTownActorPlacementSent = false
	session.initialTownDungeonPermissionSent = false
	session.initialTownLocationNotificationsSent = false
	session.initialTownQuestSnapshotsSent = false
	session.initialTownQuestHUDRefreshSent = false
	session.initialTownGuildStateProjectionSent = false
	session.initialTownSkillInfoPrepared = false
	session.initialTownSkillInfoSent = false
	session.initialTownSkillInfo = currentSceneSkillInfoProjection{}
	session.initialTownMode3RefreshPending = false
	session.completedTownReturnBuffRegistrationPending = false
	session.initialTownLegacySceneReadyAccepted = false
	session.initialTownAdventureOverheadRefreshSent = false
	session.initialTownCombatPowerAffixesSent = false
	session.initialTownMoodProjectionSent = false
	session.crystalContractMu.Lock()
	session.crystalContractTownUIReadyStateSent = false
	session.crystalContractMu.Unlock()
	session.auraSkinMu.Lock()
	session.auraSkinTownUIReadyStateSent = false
	session.auraSkinMu.Unlock()
	session.townActorOwnerChannel = currentConnectionTownActorOwnerContext(session)
	session.townMu.Unlock()
}
