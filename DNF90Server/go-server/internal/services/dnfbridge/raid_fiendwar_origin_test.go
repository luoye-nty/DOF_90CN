package dnfbridge

import (
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// TestFreezeFiendWarDungeonReturnOriginUsesTheOperationArea pins the live
// incident of 2026-10-01.
//
// A Fiend War squad walked into operation area 9 and every encounter attempt was
// silently refused -- 160 consecutive tries, one per second, all
// entered=false:
//
//	game-dungeon-select-blocked dungeon_id=4957 maze_index=4
//	  reason=ordinary_dungeon_town_return_origin_unavailable
//	  error=... origin 1/98/9 does not match runtime 1/98/2
//
// The client only saw "the party was disbanded" repeated, because the rejected
// encounter immediately tore the managed party down again.
//
// Root cause: currentDungeonTownReturnOriginOwner returned the fixed waiting
// area 98/2 for Fiend War, while freeze/commit validate that answer against the
// session's bound selector origin -- which is the operation area the squad is
// actually standing in, 98/9. The two must be the same value, so Fiend War has
// to answer with the operation area.
func TestFreezeFiendWarDungeonReturnOriginUsesTheOperationArea(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.channel.Type = dnfenum.FiendWarRaidChannelType
	// etc/raid/fiendwar/fiendwar.etc: [AREA INDEX] 9 -> [DUNGEON INDEX] 4957.
	runtime.Dungeon.ID = 4957
	runtime.Request.DungeonID = 4957
	bindDungeonSelectorOriginForTestAt(
		t,
		service,
		session,
		byte(currentFiendWarRaidTownID),
		9,
		105,
		320,
	)

	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("freeze Fiend War operation-area origin: %v", err)
	}
	origin := runtime.townReturnOrigin
	if !origin.PositionValid ||
		origin.CharacterID != session.selectedCharacterID ||
		origin.TownID != byte(currentFiendWarRaidTownID) ||
		origin.AreaID != 9 {
		t.Fatalf("Fiend War return origin=%+v, want character %d town %d area 9",
			origin, session.selectedCharacterID, currentFiendWarRaidTownID)
	}
	if err := validateCurrentDungeonRuntimeTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("commit validation rejected the operation-area origin: %v", err)
	}
}

// TestFreezeFiendWarDungeonReturnOriginKeepsTheWaitingAreaInsideIt states the
// other half: while the squad is still in the waiting room, the waiting area IS
// the operation area, and the answer must not drift to some other area.
func TestFreezeFiendWarDungeonReturnOriginKeepsTheWaitingAreaInsideIt(t *testing.T) {
	service, session, runtime := newBackToVillageRuntime(t)
	session.channel.Type = dnfenum.FiendWarRaidChannelType
	runtime.Dungeon.ID = 4957
	runtime.Request.DungeonID = 4957
	bindDungeonSelectorOriginForTestAt(
		t,
		service,
		session,
		byte(currentFiendWarRaidTownID),
		currentFiendWarWaitingAreaID,
		474,
		234,
	)

	if err := freezeCurrentDungeonTownReturnOrigin(session, runtime); err != nil {
		t.Fatalf("freeze Fiend War waiting-area origin: %v", err)
	}
	if origin := runtime.townReturnOrigin; !origin.PositionValid ||
		origin.TownID != byte(currentFiendWarRaidTownID) ||
		origin.AreaID != currentFiendWarWaitingAreaID {
		t.Fatalf("Fiend War waiting-area origin=%+v, want town %d area %d",
			origin, currentFiendWarRaidTownID, currentFiendWarWaitingAreaID)
	}
	// Anton must keep its own fixed waiting area: adding Fiend War must not move it.
	antonSession := session
	antonSession.channel.Type = dnfenum.RaidChannelType
	if area, ok := currentFiendWarOperationArea(antonSession, session.selectedCharacterID, runtimeRaidTownSpec{
		townID: currentAntonRaidTownID, waitingArea: currentAntonRaidWaitingAreaID,
	}, runtime); ok {
		t.Fatalf("Anton channel resolved a Fiend War operation area %d", area)
	}
}
