package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
)

func TestDebugResetSelectedItemBootstrap(t *testing.T) {
	service := &Service{
		options:       options{gameUpperHeader: gameUpperHeaderServer16},
		gameSessions:  make(map[uint16]*gameSession),
		onlinePlayers: newOnlinePlayerManager(),
	}
	session := &gameSession{
		conn:                                 &bufferConn{},
		connID:                               "debug",
		channel:                              channelcatalog.Channel{ID: 19},
		selectedCharacterID:                  19,
		selectedItemListRefreshSent:          true,
		selectedItemListBootstrapCharacterID: 19,
		selectedEquipmentUpdateSent:          true,
	}
	service.gameSessions[19] = session
	_, _ = service.resetGameSessionForCharacterSelect(session)
	t.Logf("after reset: refresh=%t bootstrap=%d equipUpdate=%t",
		session.selectedItemListRefreshSent,
		session.selectedItemListBootstrapCharacterID,
		session.selectedEquipmentUpdateSent)
}
