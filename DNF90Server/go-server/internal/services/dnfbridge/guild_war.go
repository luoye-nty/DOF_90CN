package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func (s *Service) currentGuildWarRules() (*dnfguild.WarRules, error) {
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return dnfguild.LoadWarRules(items.source)
}

func (s *Service) applyCurrentGuildWarGuardian(ctx context.Context, guildID uint32, key string, dungeonID int64, topology *worldmap.DungeonTopology) error {
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return err
	}
	catalog, err := dnfguild.LoadWarGuardians(items.source)
	if err != nil {
		return err
	}
	return s.readCurrentGuild(ctx, func(_ dnfrepo.Group, r dnfguild.Registry) error {
		g := r.Guilds[guildID]
		if g == nil || g.War.Battle == nil || g.War.Battle.Key != key {
			return dnfguild.ErrWarNotActive
		}
		guardian := g.War.Battle.OpponentGuardian
		if dungeonID == 8510 {
			guardian = g.War.Battle.OwnGuardian
		}
		monster, err := catalog.Monster(guardian)
		if err != nil {
			return err
		}
		monsters, err := s.dungeonMonsterCatalog()
		if err != nil {
			return err
		}
		if _, found, err := monsters.Find(int64(monster)); err != nil {
			return err
		} else if !found {
			return dnfguild.ErrInvalidContent
		}
		topology.ReplaceMonster(int64(catalog.Dummy), int64(monster), int64(guardian.Level))
		return nil
	})
}

// Native E81CA0 counts up to forty packed 49-byte rows at raw+2E55.
// D14F10 reads name +4, mark +27 and result +45. D19E50 reads power +28,
// victory +32, defence +36, active guardian ID +40 and combined level +44.
func buildCurrentGuildWarOverviewBody(g *dnfguild.Guild, r dnfguild.Registry) []byte {
	b := buildCurrentGuildGuardianStateBody(g)
	if g == nil {
		return b
	}
	w := g.War
	for _, x := range []struct {
		off   int
		value uint32
	}{{7, w.AttackWins}, {11, w.AttackLosses}, {15, w.DefenceWins}, {19, w.DefenceLosses}, {72, uint32(len(w.Results))}, {76, uint32(len(w.Rivals))}} {
		binary.LittleEndian.PutUint32(b[x.off:], x.value)
	}
	if w.Battle != nil {
		binary.LittleEndian.PutUint32(b[60:], w.Battle.Opponent)
	}
	row := 0
	for _, id := range w.Rivals {
		other := r.Guilds[id]
		if other == nil || row >= 40 {
			continue
		}
		off := 80 + 49*row
		row++
		binary.LittleEndian.PutUint32(b[off:], id)
		copy(b[off+4:off+26], encodeCurrentGuildWarName(other.Name))
		b[off+27] = other.Mark
		if guardian, ok := other.WarGuardian(); ok {
			binary.LittleEndian.PutUint32(b[off+40:], guardian.ID)
			b[off+44] = guardian.Level
		}
		binary.LittleEndian.PutUint32(b[off+45:], w.Results[id])
	}
	return b
}

func encodeCurrentGuildWarName(name string) []byte {
	// Same codepage as op70; preserve complete characters within the native
	// 23-byte, NUL-terminated field.
	var out []byte
	for _, r := range name {
		part := rosterNameBytes(string(r))
		if len(out)+len(part) > 21 {
			break
		}
		out = append(out, part...)
	}
	return out
}

func (s *Service) writeCurrentGuildWarOverview(session *gameSession, _ *dnfguild.Guild) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var r dnfguild.Registry
	var g *dnfguild.Guild
	err := s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, registry *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		registry.AdvanceWars(s.gameplayNow())
		r = *registry
		g, _, _ = r.GuildForCharacter(c.CharacterID)
		return nil
	})
	if err != nil {
		return err
	}
	if err = s.sendGameUpperRawClass(session, 1297, buildCurrentGuildWarOverviewBody(g, r), 0); err != nil {
		return err
	}
	personal := make([]byte, 42)
	personal[8], personal[13] = 1, 1
	if g != nil {
		binary.LittleEndian.PutUint32(personal[4:], g.War.Participants[strconv.Itoa(int(session.selectedCharacterID))])
	}
	if err = s.sendGameUpperRawClass(session, 784, personal, 0); err != nil {
		return err
	}
	if g != nil && g.War.Battle != nil {
		if err = s.scheduleCurrentGuildWar(g); err != nil {
			return err
		}
		rules, err := s.currentGuildWarRules()
		if err != nil {
			return err
		}
		target := r.Guilds[g.War.Battle.Opponent]
		if target == nil {
			return dnfguild.ErrWarNoOpponent
		}
		frozenTarget := *target
		frozenTarget.ActiveGuardian = g.War.Battle.OpponentGuardian.ID
		frozenTarget.Guardians = []dnfguild.Guardian{g.War.Battle.OpponentGuardian}
		target = &frozenTarget
		if err = s.sendGameUpperRawClass(session, 779, buildCurrentGuildWarOpponentBody(target), 0); err != nil {
			return err
		}
		return s.sendGameUpperRawClass(session, 1261, buildCurrentGuildWarBattleBody(g, target, rules), 0)
	}
	return nil
}

// E8FE80 reads 0x369F bytes. The 23-byte name is followed by the mark at
// +17h (D0C208), reserved fields, 0x2E00 building bytes and the 1297 block.
func buildCurrentGuildWarOpponentBody(g *dnfguild.Guild) []byte {
	b := make([]byte, 0x369F)
	if g == nil {
		return b
	}
	copy(b[:23], encodeCurrentGuildWarName(g.Name))
	// D0C208/D111B9 read the mark at +17h before calling 26AEA50.
	b[23] = g.Mark
	copy(b[0x2E1D:], buildCurrentGuildGuardianStateBody(g))
	return b
}

// E8B2F0 copies a 64-byte HUD record. D0B8C0 subtracts current Unix time
// from +0; D0F46D/5F5E4E7 displays our HP at +4, D0F370/5F5E4E8
// displays enemy HP at +36. D0E2C0 skips countdowns in state 2; state 3 with
// a zero timestamp renders a negative Unix-time countdown.
func buildCurrentGuildWarBattleBody(g, target *dnfguild.Guild, rules *dnfguild.WarRules) []byte {
	b := make([]byte, 68) // trailing u32 dungeon-status row count
	battle := g.War.Battle
	binary.LittleEndian.PutUint32(b, uint32(battle.EndsAt))
	binary.LittleEndian.PutUint32(b[4:], rules.FriendlyHP)
	binary.LittleEndian.PutUint32(b[8:], target.ID)
	copy(b[12:35], encodeCurrentGuildWarName(target.Name))
	binary.LittleEndian.PutUint32(b[36:], battle.EnemyHP)
	binary.LittleEndian.PutUint32(b[40:], 2)
	binary.LittleEndian.PutUint32(b[48:], 2)
	return b
}

// 240..255 are a versioned DLL compatibility extension for missing/misleading
// native error strings, decoded only for these guild-war command IDs.
func currentGuildWarError(err error) byte {
	switch {
	case errors.Is(err, dnfguild.ErrWarGuardian):
		return 240
	case errors.Is(err, dnfguild.ErrWarActive):
		return 241
	case errors.Is(err, dnfguild.ErrWarLimit):
		return 242
	case errors.Is(err, dnfguild.ErrInsufficientGuardian):
		return 243
	case errors.Is(err, dnfguild.ErrWarMissionIncomplete):
		return 251
	case errors.Is(err, errDungeonPickupInventoryFull):
		return 252
	case errors.Is(err, dnfguild.ErrWarNoOpponent):
		return 254
	case errors.Is(err, dnfguild.ErrWarClosed), errors.Is(err, dnfguild.ErrWarNotActive):
		return 255
	case errors.Is(err, dnfguild.ErrPermissionDenied):
		return 244
	case errors.Is(err, dnfguild.ErrNotMember):
		return 245
	case errors.Is(err, dnfguild.ErrInvalidContent):
		return 250
	default:
		return 253
	}
}

func (s *Service) handleCurrentGuildWarRivals(session *gameSession, body []byte) error {
	return s.handleCurrentGuildWarAction(session, 1364, body)
}
func (s *Service) handleCurrentGuildWarMatching(session *gameSession, body []byte) error {
	return s.handleCurrentGuildWarAction(session, 841, body)
}
func (s *Service) handleCurrentGuildWarCancel(session *gameSession, body []byte) error {
	return s.handleCurrentGuildWarAction(session, 842, body)
}
func (s *Service) handleCurrentGuildWarAction(session *gameSession, op uint16, body []byte) error {
	if ready, _ := s.currentSceneTransitionReadyForState(session); !ready {
		return s.sendGameUpperFailure(session, op, 250)
	}
	// E8E430 writes opcode 1364, then E8E44C calls the one-byte writer
	// 3454FC0. Captured initial/paid requests are exactly 01 / 00.
	valid := (op == 1364 && len(body) == 1 && body[0] <= 1) || (op == 841 && len(body) == 21 && binary.LittleEndian.Uint32(body[13:]) == 1) || (op == 842 && (len(body) == 0 || len(body) == 13))
	if !valid {
		s.logGameEvent(session, "game-guild-war-action-invalid", "opcode", op, "body_len", len(body))
		return s.sendGameUpperFailure(session, op, 250)
	}
	rules, err := s.currentGuildWarRules()
	if err != nil {
		return s.sendGameUpperFailure(session, op, 253)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var g *dnfguild.Guild
	err = s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, r *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		var e error
		switch op {
		case 1364:
			g, e = r.AddWarRivals(c.CharacterID, body[0] == 1, rules, s.gameplayNow())
		case 841:
			g, e = r.StartWar(c.CharacterID, binary.LittleEndian.Uint32(body[17:]), rules, s.gameplayNow())
		case 842:
			g, e = r.CancelWar(c.CharacterID, s.gameplayNow())
		}
		return e
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-war-action-rejected", "opcode", op, "error", err)
		return s.sendGameUpperFailure(session, op, currentGuildWarError(err))
	}
	if err = s.sendGameUpperSuccess(session, op, nil); err != nil {
		return err
	}
	s.logGameEvent(session, "game-guild-war-action-saved", "opcode", op, "guild_id", g.ID)
	if op == 842 && g.War.LastBattle != nil {
		if err = s.publishCurrentGuildWarEnd(g, g.War.LastBattle.Key); err != nil {
			return err
		}
	}
	for id := range g.Members {
		if id == strconv.Itoa(int(session.selectedCharacterID)) {
			continue
		}
		if target, ok := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(id))); ok {
			if e := s.postGameSessionCharacterEvent(target.session, "guild-war-refresh", target.character, target.generation, func() error {
				if ready, _ := s.currentSceneTransitionReadyForState(target.session); ready {
					return s.writeCurrentGuildWarOverview(target.session, g)
				}
				return nil
			}); e != nil {
				s.logGameEvent(target.session, "game-guild-war-refresh-failed", "error", e)
			}
		}
	}
	return s.writeCurrentGuildWarOverview(session, g)
}

func isCurrentGuildWarDungeon(d worldmap.Dungeon) bool {
	return d.Metadata.IntegerValues["agit war dungeon"] != 0 || (d.ID >= 8510 && d.ID <= 8520)
}
func (s *Service) currentGuildWarAdmission(ctx context.Context, session *gameSession, d worldmap.Dungeon, practice bool) (uint32, string, error) {
	if !isCurrentGuildWarDungeon(d) {
		return 0, "", nil
	}
	rules, err := s.currentGuildWarRules()
	if err != nil {
		return 0, "", err
	}
	if !rules.Dungeons[uint32(d.ID)] {
		return 0, "", dnfguild.ErrInvalidContent
	}
	var id uint32
	var key string
	err = s.readCurrentGuild(ctx, func(repos dnfrepo.Group, r dnfguild.Registry) error {
		c, e := selectedGuildCharacter(ctx, repos, session)
		if e != nil {
			return e
		}
		if c.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		g, _, ok := r.GuildForCharacter(c.CharacterID)
		if !ok {
			return dnfguild.ErrNotMember
		}
		if practice {
			return dnfguild.ErrInvalidContent
		}
		g, e = r.WarAdmission(c.CharacterID, uint32(d.ID), rules, s.gameplayNow())
		if e != nil {
			return e
		}
		for _, member := range runtimePartyMembers(runtimePartyStateSnapshot(session)) {
			if member.UserID != 0 {
				if _, e = r.WarAdmission(strconv.Itoa(int(member.UserID)), uint32(d.ID), rules, s.gameplayNow()); e != nil {
					return e
				}
				if _, ok := g.Members[strconv.Itoa(int(member.UserID))]; !ok {
					return dnfguild.ErrNotMember
				}
			}
		}
		id, key = g.ID, g.War.Battle.Key
		return nil
	})
	return id, key, err
}

func (s *Service) commitCurrentGuildWarEntry(session *gameSession, runtime *runtimeDungeonState) error {
	if runtime.guildWarID == 0 {
		return nil
	}
	rules, err := s.currentGuildWarRules()
	if err != nil {
		return err
	}
	missions, err := s.currentGuildWarMissionsCatalog()
	if err != nil {
		return err
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	return s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, r *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		joined, e := r.JoinWar(c.CharacterID, runtime.guildWarKey, uint32(runtime.Dungeon.ID), rules, s.gameplayNow())
		if e != nil || !joined {
			return e
		}
		state, e := dnfguild.LoadWarMissions(ctx, tx.Settings, c.CharacterID, missions, s.gameplayNow())
		if e != nil {
			return e
		}
		if e = state.Observe(missions, "join", uint32(runtime.Dungeon.ID), runtime.guildWarKey, s.gameplayNow(), s.gameplayNow()); e != nil {
			return e
		}
		return dnfguild.SaveWarMissions(ctx, tx.Settings, c.CharacterID, state, s.gameplayNow())
	})
}
