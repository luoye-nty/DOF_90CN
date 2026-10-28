package dnfbridge

import (
	"context"
	"path/filepath"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// newSceneRebuildExpertJobTest opens the active runtime PVF and returns a
// service whose expert-job catalog is the real one, because the scene-rebuild
// path builds the same profession-carrying body the live server sends.
func newSceneRebuildExpertJobTest(t *testing.T) (*Service, *gameSession, *bufferConn, dnfrepo.CharacterRecord) {
	t.Helper()
	ctx := context.Background()
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	expert, err := dnfexpertjob.Load(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	character := dnfrepo.CharacterRecord{
		CharacterID: "7",
		AccountID:   "account-1",
		Name:        "Enchanter",
		Stats: map[string]int64{
			"expert_job_type": int64(dnfexpertjob.EnchanterType),
			"expert_job_exp":  1,
		},
	}
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		expertJobCatalog:   expert,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 7, accountID: "account-1"}
	return service, session, connection, character
}

// TestSceneRebuildSuppressesUnchangedExpertJobProjection pins the fix for the
// "副职业资格认证 4/4" banner: on this 90CN client class-0 op205 is an
// acquisition/mutation notification, and a scene reconstruction that
// deliberately does not replay op19 must therefore not replay an unchanged
// profession either. A real durable change still publishes.
func TestSceneRebuildSuppressesUnchangedExpertJobProjection(t *testing.T) {
	service, session, connection, character := newSceneRebuildExpertJobTest(t)

	// Login / dungeon-entry boundary: the client accepts the profession body.
	if err := service.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
		t.Fatalf("initial expert-job publish: %v", err)
	}
	published, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || published.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("initial publish header=%+v trailing=%d", published.Header, len(rest))
	}

	// Completed dungeon return with identical durable state: nothing may be
	// written. Anything written here is what opened the qualification banner.
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSceneRebuild(session, character, "completed_dungeon_return"); err != nil {
		t.Fatalf("unchanged scene rebuild: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unchanged scene rebuild replayed %d bytes", connection.write.Len())
	}

	// A durable change since the last accepted body must still reach the client.
	character.Stats["expert_job_exp"] = 2
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSceneRebuild(session, character, "completed_dungeon_return"); err != nil {
		t.Fatalf("changed scene rebuild: %v", err)
	}
	changed, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || changed.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("changed scene rebuild header=%+v trailing=%d", changed.Header, len(rest))
	}
	if got := currentExpertJobTrailerExperience(t, changed.Body); got != 2 {
		t.Fatalf("changed scene rebuild experience=%d, want 2", got)
	}

	// That body is now the accepted state, so the next rebuild is quiet again.
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSceneRebuild(session, character, "completed_dungeon_return"); err != nil {
		t.Fatalf("repeat scene rebuild: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("repeat scene rebuild replayed %d bytes", connection.write.Len())
	}
}

// TestSceneRebuildKeepsClearedProfessionProjection proves the suppression is
// scoped to a character that actually has an auxiliary profession: the
// type-zero cleared form is not acquisition-shaped, so it keeps its previous
// unconditional behaviour byte for byte.
func TestSceneRebuildKeepsClearedProfessionProjection(t *testing.T) {
	service, session, connection, character := newSceneRebuildExpertJobTest(t)
	character.Stats = map[string]int64{}

	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSceneRebuild(session, character, "completed_dungeon_return"); err != nil {
		t.Fatalf("cleared scene rebuild: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || packet.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("cleared scene rebuild header=%+v trailing=%d", packet.Header, len(rest))
	}
	if len(packet.Body) != 18 || packet.Body[1] != 0 ||
		currentExpertJobProjectionMode(t, packet.Body) != currentExpertJobProjectionSilent {
		t.Fatalf("cleared scene rebuild body=%x", packet.Body)
	}
}

// TestExpertJobProjectionModeSeparatesAcquisitionFromReconstruction pins the
// exact contract the compatibility DLL relies on: the qualification window is
// decided by the 4-byte mode word, and the two modes differ in NOTHING else.
//
// The native op205 reader cannot tell a real acquisition from a re-projection of
// state the client already holds, so it raises 「副职业资格认证」 for both - that
// is the relogin popup. Marking the reconstruction silent is what suppresses
// it, and only the quest-completion path may announce.
func TestExpertJobProjectionModeSeparatesAcquisitionFromReconstruction(t *testing.T) {
	service, session, connection, character := newSceneRebuildExpertJobTest(t)

	// Every state-reconstruction send is silent.
	if err := service.sendCurrentExpertJobInfoForCharacter(session, character, false); err != nil {
		t.Fatalf("silent publish: %v", err)
	}
	silent, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || silent.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("silent publish header=%+v trailing=%d", silent.Header, len(rest))
	}
	if mode := currentExpertJobProjectionMode(t, silent.Body); mode != currentExpertJobProjectionSilent {
		t.Fatalf("state reconstruction mode=%d, want silent(%d)", mode, currentExpertJobProjectionSilent)
	}
	if got := currentExpertJobTrailerExperience(t, silent.Body); got != 1 {
		t.Fatalf("silent experience=%d, want 1", got)
	}

	// The real acquisition path first republishes the actor record and only then
	// publishes the same op205 body marked as announcing.
	//
	// The order is part of the contract, not an implementation detail: op205 does
	// not carry the sub-profession into the client's actor record, so an
	// announcement that arrives before the record refresh lands on a record whose
	// +0x368 still reads "no profession" and both the profession row and the
	// acquisition feedback are dropped until the next relogin.
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForCharacterAnnouncing(session, character, true); err != nil {
		t.Fatalf("announcing publish: %v", err)
	}
	packets := connection.write.Bytes()
	refresh, afterRefresh := splitGameServerUpperPacket(t, packets)
	if refresh.Header.Classification != 0 || refresh.Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) ||
		len(refresh.Body) == 0 || refresh.Body[0] != 0 {
		t.Fatalf("acquisition must refresh the actor record first: header=%+v body=%x",
			refresh.Header, refresh.Body)
	}
	if got := refresh.Body[len(refresh.Body)-sceneObjectExpertJobTypeFromEnd]; got != byte(character.Stats["expert_job_type"]) {
		t.Fatalf("actor refresh record+0x368 = %d, want %d", got, character.Stats["expert_job_type"])
	}
	announcing, rest := splitGameServerUpperPacket(t, afterRefresh)
	if len(rest) != 0 || announcing.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("announcing publish header=%+v trailing=%d", announcing.Header, len(rest))
	}
	if mode := currentExpertJobProjectionMode(t, announcing.Body); mode != currentExpertJobProjectionAnnounce {
		t.Fatalf("quest completion mode=%d, want announce(%d)", mode, currentExpertJobProjectionAnnounce)
	}
	if got := currentExpertJobTrailerExperience(t, announcing.Body); got != 1 {
		t.Fatalf("announcing experience=%d, want 1", got)
	}

	// Same length, and identical everywhere except the mode word: the native
	// reader must see byte-identical state in both modes.
	if len(announcing.Body) != len(silent.Body) {
		t.Fatalf("mode changed body length %d -> %d", len(silent.Body), len(announcing.Body))
	}
	modeOffset := len(silent.Body) - 12
	for i := range silent.Body {
		if i >= modeOffset && i < modeOffset+4 {
			continue
		}
		if silent.Body[i] != announcing.Body[i] {
			t.Fatalf("mode changed body byte %d: silent=%02x announcing=%02x",
				i, silent.Body[i], announcing.Body[i])
		}
	}
}
