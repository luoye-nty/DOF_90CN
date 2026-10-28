package dnfbridge

import (
	"fmt"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// TestSwallowPeerFanoutWriteKeepsTheOriginFailureBoundary pins the rule that a
// fan-out write failure belongs to the session that was being written:
//
//   - another session's failed write is recorded and swallowed, so a stale peer
//     can never tear down the session that is serving the player;
//   - the origin's OWN failed write still reports the error, because
//     game_session_events.go relies on that error to release a blocked Read and
//     run connection cleanup;
//   - an unattributable target (no origin known) also keeps the error, which is
//     the behaviour every caller had before this rule existed.
func TestSwallowPeerFanoutWriteKeepsTheOriginFailureBoundary(t *testing.T) {
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	origin := &gameSession{connID: "origin", selectedCharacterID: 1}
	target := &gameSession{connID: "game-000333", selectedCharacterID: 37}
	writeErr := fmt.Errorf("set tcp 10.1.0.16:10023: use of closed network connection")

	if err := service.swallowPeerFanoutWrite(origin, target, "unit", uint16(dnfenum.CmdPacketRequestPeer), nil); err != nil {
		t.Fatalf("a write that did not fail must stay nil, got %v", err)
	}
	if err := service.swallowPeerFanoutWrite(origin, target, "unit", uint16(dnfenum.CmdPacketRequestPeer), writeErr); err != nil {
		t.Fatalf("another session's failed write must not fail the origin, got %v", err)
	}
	if err := service.swallowPeerFanoutWrite(origin, origin, "unit", uint16(dnfenum.CmdPacketRequestPeer), writeErr); err != writeErr {
		t.Fatalf("the origin's own failed write must keep its error, got %v", err)
	}
	if err := service.swallowPeerFanoutWrite(nil, target, "unit", uint16(dnfenum.CmdPacketRequestPeer), writeErr); err != writeErr {
		t.Fatalf("an unattributable target must keep the error, got %v", err)
	}
	if err := service.swallowPeerFanoutWrite(origin, nil, "unit", uint16(dnfenum.CmdPacketRequestPeer), writeErr); err != writeErr {
		t.Fatalf("a missing target must keep the error, got %v", err)
	}
}
