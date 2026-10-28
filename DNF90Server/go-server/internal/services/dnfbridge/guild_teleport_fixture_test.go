package dnfbridge

import (
	"context"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// newGuildTeleportTest is the upstream guild-teleport fixture, ported so the
// 公会战 / 代表角色 tests that share it can run here.
//
// Two adaptations, both because this tree's op434/435 is our own party-teleport
// state machine rather than upstream's currentGuildTeleport (see party_teleport.go
// and decision D4):
//
//   - cleanup calls endCurrentPartyTeleportForSession instead of upstream's
//     closeCurrentGuildTeleportSession, which does not exist here;
//   - nothing else changes: in particular the guild is still created with an
//     active 公会传送 (type 7) benefit, which is the precondition both forks now
//     require when a request passes the gate.
//
// Upstream's own TestCurrentGuildTeleportNativeLifecycle is deliberately NOT
// ported: it drives upstream's state machine, which this tree does not use.
func newGuildTeleportTest(t *testing.T) (*Service, *gameSession, *gameSession, dnfrepo.Group) {
	t.Helper()
	s, leader, old := newTownMoveTest(t)
	s.gameplayTimers = newFakeCurrentDungeonDeathTimerQueue()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character, _, _ := old.Character.Load(ctx, "29")
	character.Name = "master"
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	guest := dnfrepo.CloneCharacter(character)
	guest.CharacterID, guest.AccountID, guest.Name = "30", "dnf:2", "guest"
	if err := repositories.Character.Save(ctx, guest); err != nil {
		t.Fatal(err)
	}
	s.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	s.options.gameUpperBodyCodec = gameUpperBodyCodecPlain
	leader.accountID = character.AccountID
	member := &gameSession{conn: &bufferConn{}, accountID: guest.AccountID, selectedCharacterID: 30, characterGeneration: 2,
		channel: leader.channel, residentChannel: leader.channel, townSceneReadyCharacterID: 30,
		connectionTownActorOwnerChannel: byte(leader.channel.ID), townActorOwnerChannel: byte(leader.channel.ID)}
	s.gameSessions = map[uint16]*gameSession{29: leader, 30: member}
	enableTownMoveSkillProjection(t, s, repositories, "29")
	enableTownMoveSkillProjection(t, s, repositories, "30")
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(character, "guild", s.gameplayNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Benefits = map[byte]dnfguild.Benefit{7: {Type: 7, Value: 10, Mode: 1, ExpiresAt: s.gameplayNow().Add(time.Hour)}}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, s.gameplayNow()); err != nil {
		t.Fatal(err)
	}
	identity, _ := s.boundGameSessionCharacterSnapshot(leader)
	m, _ := s.runtimePartyMemberForIdentity(identity)
	created := s.runtimePartyManagerForService().EnsureLeader(m, alignedcmd.PartyState{})
	identity, _ = s.boundGameSessionCharacterSnapshot(member)
	m, _ = s.runtimePartyMemberForIdentity(identity)
	joined := s.runtimePartyManagerForService().Join(created.Party.ID, m)
	if !joined.OK {
		t.Fatalf("party: %+v", joined)
	}
	s.publishRuntimePartySnapshot(joined.Party)
	t.Cleanup(func() { s.endCurrentPartyTeleportForSession(leader) })
	return s, leader, member, repositories
}
