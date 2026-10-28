package dnfbridge

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestCurrentPartyActorProjectionRequiresCompleteMatchingOwnerRows(t *testing.T) {
	const characterID uint16 = 77
	mode0 := make([]byte, 78)
	mode0[0] = 0
	binary.LittleEndian.PutUint16(mode0[1:3], 1)
	mode0[3], mode0[4] = 1, 42
	binary.LittleEndian.PutUint16(mode0[76:78], characterID)
	mode1 := make([]byte, currentMode1BaseWireSize)
	mode1[0] = 1
	binary.LittleEndian.PutUint16(mode1[1:3], 1)
	mode1[3], mode1[4] = 1, 42
	binary.LittleEndian.PutUint16(mode1[21:23], characterID)

	session := &gameSession{
		selectedCharacterID: characterID,
		characterGeneration: 9,
	}
	if err := captureCurrentPartyActorProjection(session, characterID, mode0, mode1); err != nil {
		t.Fatal(err)
	}
	projection, found := currentPartyActorProjectionSnapshot(session, characterID, 9)
	if !found || projection.ownerServer != 1 || projection.ownerChannel != 42 ||
		!bytes.Equal(projection.mode0, mode0) || !bytes.Equal(projection.mode1, mode1) {
		t.Fatalf("captured projection=%+v found=%t", projection, found)
	}
	projection.mode0[10] = 0xff
	reloaded, found := currentPartyActorProjectionSnapshot(session, characterID, 9)
	if !found || reloaded.mode0[10] == 0xff {
		t.Fatal("party actor projection snapshot leaked a mutable row")
	}
	if _, found := currentPartyActorProjectionSnapshot(session, characterID, 10); found {
		t.Fatal("party actor projection survived a session generation change")
	}

	shortMode1 := append([]byte(nil), mode1[:currentMode1BaseWireSize-1]...)
	if err := captureCurrentPartyActorProjection(session, characterID, mode0, shortMode1); err == nil {
		t.Fatal("party actor projection accepted an incomplete mode1 row")
	}
	wrongOwnerMode1 := append([]byte(nil), mode1...)
	wrongOwnerMode1[4] = 43
	if err := captureCurrentPartyActorProjection(session, characterID, mode0, wrongOwnerMode1); err == nil {
		t.Fatal("party actor projection accepted different mode0/mode1 owners")
	}
}
