package dnfbridge

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentExpertJobProjectionRepublishesOnEveryExperienceGain pins the
// decision that made 附魔师 crafting look like it did nothing: the profession's
// experience only reaches the client through class-0 op205, so an operation
// that moved the bar without crossing a level threshold still has to
// republish. Gating on LevelChanged alone left the gain invisible until the
// next character select replayed the login push.
func TestCurrentExpertJobProjectionRepublishesOnEveryExperienceGain(t *testing.T) {
	for name, probe := range map[string]struct {
		gain         int64
		levelChanged bool
		want         bool
	}{
		"experience_without_level_change": {gain: 12, levelChanged: false, want: true},
		"experience_with_level_change":    {gain: 12, levelChanged: true, want: true},
		"level_change_without_experience": {gain: 0, levelChanged: true, want: true},
		"nothing_changed":                 {gain: 0, levelChanged: false, want: false},
	} {
		if got := currentExpertJobProjectionNeedsRepublish(probe.gain, probe.levelChanged); got != probe.want {
			t.Fatalf("%s: republish=%t, want %t", name, got, probe.want)
		}
	}
}

// TestRealScriptPVFExpertJobInfoCarriesExperienceWithoutLevelChange proves the
// republish is worth sending: two pushes for the same enchanter at the same
// level must differ on the wire, because the experience rides in the EJXP
// projection trailer the compatibility DLL writes into the current actor.
func TestRealScriptPVFExpertJobInfoCarriesExperienceWithoutLevelChange(t *testing.T) {
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
	config, ok := expert.Config(dnfexpertjob.EnchanterType)
	if !ok {
		t.Fatal("active PVF has no enchanter config")
	}
	// Two experience values inside the same level band, so only the trailer
	// moves and every level-derived field stays identical.
	const before, after = int64(1), int64(2)
	if config.Level(before) != config.Level(after) {
		t.Fatalf("probe experiences straddle a level threshold: %d vs %d", config.Level(before), config.Level(after))
	}

	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "account-1", Name: "Enchanter",
		Stats: map[string]int64{"expert_job_type": int64(dnfexpertjob.EnchanterType), "expert_job_exp": before},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID: "account-1", gameUpperHeader: gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		expertJobCatalog:   expert,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 7, accountID: "account-1"}

	if err := service.sendCurrentExpertJobInfoForSelectedCharacter(session, false); err != nil {
		t.Fatal(err)
	}
	first, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || first.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("first projection header=%+v trailing=%d", first.Header, len(rest))
	}
	if got := currentExpertJobTrailerExperience(t, first.Body); got != uint32(before) {
		t.Fatalf("first projection experience=%d, want %d", got, before)
	}

	// A craft that does not cross a threshold: the durable row moves, and the
	// forced republish has to carry it even though the session already saw
	// this character.
	character.Stats["expert_job_exp"] = after
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSelectedCharacter(session, true); err != nil {
		t.Fatal(err)
	}
	second, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(rest) != 0 || second.Header.MsgID != currentExpertJobInfoNotification {
		t.Fatalf("second projection header=%+v trailing=%d", second.Header, len(rest))
	}
	if got := currentExpertJobTrailerExperience(t, second.Body); got != uint32(after) {
		t.Fatalf("second projection experience=%d, want %d", got, after)
	}

	// Without force the same character is suppressed, which is exactly why the
	// craft path has to ask for force=true.
	connection.write.Reset()
	if err := service.sendCurrentExpertJobInfoForSelectedCharacter(session, false); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unforced projection re-sent %d bytes", connection.write.Len())
	}
}

func currentExpertJobTrailerExperience(t *testing.T, body []byte) uint32 {
	t.Helper()
	if len(body) < 8 {
		t.Fatalf("expert job projection body too short: %x", body)
	}
	trailer := body[len(body)-8:]
	if magic := binary.LittleEndian.Uint32(trailer[0:4]); magic != currentExpertJobProjectionTrailerMagic {
		t.Fatalf("expert job projection trailer magic=%08x body=%x", magic, body)
	}
	return binary.LittleEndian.Uint32(trailer[4:8])
}
