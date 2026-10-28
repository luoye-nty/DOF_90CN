package dnfbridge

import (
	"encoding/binary"
	"strconv"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func buildCurrentGuildWarMissionsBody(catalog *dnfguild.WarMissionCatalog, state dnfguild.WarMissionState) []byte {
	var w packetWriter
	// E90600 reads daily then season, each with a u32 count and packed 9-byte
	// rows. E90D2E tests the last byte against 1 (already claimed), then compares
	// progress with the required count from PVF. Send every configured mission.
	for _, season := range []bool{false, true} {
		count := uint32(0)
		for _, m := range catalog.Missions {
			if m.Season == season {
				count++
			}
		}
		w.writeUint32(count)
		for _, m := range catalog.Missions {
			if m.Season != season {
				continue
			}
			v := state.Values[m.ID]
			w.writeUint32(m.ID)
			w.writeUint32(min(v.Progress, m.Required))
			w.writeByte(boolByte(v.Claimed))
		}
	}
	return w.bytes()
}

func (s *Service) currentGuildWarMissionsCatalog() (*dnfguild.WarMissionCatalog, error) {
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	return dnfguild.LoadWarMissionsCatalog(items.source)
}

func (s *Service) handleCurrentGuildWarInfo(session *gameSession, body []byte) error {
	const opcode = uint16(1345)
	// u32(2) is also emitted at the native post-op24 boundary. Retain the
	// already-owned scene tail before interpreting the same packet as a query.
	if isCurrentLegacyTownSceneReadyAck(body) {
		if err := s.sendCurrentLegacyTownSceneReadyBoundary(session, "legacy_type1345_scene_ready_ack"); err != nil {
			return err
		}
	}
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if ready, _ := s.currentSceneTransitionReadyForState(session); !ready {
		return nil
	}
	if len(body) != 4 || binary.LittleEndian.Uint32(body) > 3 {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	mode := binary.LittleEndian.Uint32(body)
	// The scene tail remains usable in isolated bootstrap/no-repository states.
	if repos, ok := s.repositoryGroup(); !ok || repos.Settings == nil || repos.Character == nil {
		return nil
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var guild *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(repos dnfrepo.Group, r dnfguild.Registry) error {
		c, e := selectedGuildCharacter(ctx, repos, session)
		if e != nil {
			return e
		}
		if c.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		guild, _, _ = r.GuildForCharacter(c.CharacterID)
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	// Login also emits these queries without a guild. Leave their established
	// scene-tail behavior intact; no guild-war window is accessible then.
	if guild == nil {
		return nil
	}
	catalog, err := s.currentGuildWarMissionsCatalog()
	if err != nil {
		s.logGameEvent(session, "game-guild-war-missions-catalog-failed", "error", err)
		if mode == 2 {
			return nil
		}
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	repos, _ := s.repositoryGroup()
	state, err := dnfguild.LoadWarMissions(ctx, repos.Settings, strconv.Itoa(int(session.selectedCharacterID)), catalog, s.gameplayNow())
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	if err = s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	if mode != 2 {
		if err = s.writeCurrentGuildWarOverview(session, guild); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-guild-war-info-sent", "query", mode, "guild_id", guild.ID, "missions", len(catalog.Missions))
	return s.sendGameUpperRawClass(session, 1283, buildCurrentGuildWarMissionsBody(catalog, state), 0)
}

func currentGuildWarMissionError(err error) byte {
	return currentGuildWarError(err)
}

func (s *Service) writeCurrentGuildWarMissions(session *gameSession) error {
	catalog, err := s.currentGuildWarMissionsCatalog()
	if err != nil {
		return err
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfguild.ErrRepositoryUnavailable
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	state, err := dnfguild.LoadWarMissions(ctx, repos.Settings, strconv.Itoa(int(session.selectedCharacterID)), catalog, s.gameplayNow())
	if err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, 1283, buildCurrentGuildWarMissionsBody(catalog, state), 0)
}

func (s *Service) handleCurrentGuildWarMissionReward(session *gameSession, body []byte) error {
	const opcode = uint16(1346)
	if len(body) != 4 {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	if ready, _ := s.currentSceneTransitionReadyForState(session); !ready {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	id := binary.LittleEndian.Uint32(body)
	catalog, err := s.currentGuildWarMissionsCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 253)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var state dnfguild.WarMissionState
	var updates []currentItemListEntry
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, r *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		if _, _, found := r.GuildForCharacter(c.CharacterID); !found {
			return dnfguild.ErrNotMember
		}
		var e error
		state, e = dnfguild.LoadWarMissions(ctx, tx.Settings, c.CharacterID, catalog, s.gameplayNow())
		if e != nil {
			return e
		}
		rewards, e := state.Claim(catalog, id, s.gameplayNow())
		if e != nil {
			return e
		}
		if len(rewards) == 0 {
			return nil
		}
		if tx.Inventory == nil {
			return dnfguild.ErrRepositoryUnavailable
		}
		inventory, found, e := tx.Inventory.Load(ctx, c.CharacterID)
		if e != nil {
			return e
		}
		if !found {
			return dnfguild.ErrRepositoryUnavailable
		}
		inventory = dnfrepo.CloneInventory(inventory)
		changed := make(map[string]bool)
		for _, reward := range rewards {
			definition, e := items.ResolveItem(reward.ItemID)
			if e != nil {
				return e
			}
			if definition.Kind != dungeonDropItemStackable || definition.InventoryListType != dnfrepo.MainInventoryListType {
				return errDungeonPickupItemInvalid
			}
			slots, e := grantCurrentCeraShopProduct(&inventory, definition, reward.Count, s.currentInventoryPlacementMainSlotCount(ctx, &inventory, c.CharacterID))
			if e != nil {
				return e
			}
			for _, slot := range slots {
				changed[currentCeraShopInventorySlotKey(definition.InventoryListType, int16(slot))] = true
			}
		}
		for key := range changed {
			list, slot, valid := parseSceneInventorySlotKey(key)
			if !valid {
				return errDungeonPickupItemInvalid
			}
			stack := inventory.Slots[key]
			entry := currentItemListEntryFromStack(list, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[key] = stack
			updates = append(updates, entry)
		}
		if e = tx.Inventory.Save(ctx, inventory); e != nil {
			return e
		}
		return dnfguild.SaveWarMissions(ctx, tx.Settings, c.CharacterID, state, s.gameplayNow())
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-war-mission-rejected", "mission", id, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildWarMissionError(err))
	}
	if err = s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	if len(updates) > 0 {
		sortCurrentItemListEntries(updates)
		if err = s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, updates), 0); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-guild-war-mission-claimed", "mission", id, "updated_slots", len(updates))
	return s.sendGameUpperRawClass(session, 1283, buildCurrentGuildWarMissionsBody(catalog, state), 0)
}
