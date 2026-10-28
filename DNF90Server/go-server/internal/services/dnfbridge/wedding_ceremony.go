// 本文件负责 PVF 教堂预约、入场和持久化仪式阶段的计时恢复。
package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

func (s *Service) weddingRoom() (byte, error) {
	for room := byte(8); room <= 38; room++ {
		if area, found := s.townArea(39, int64(room)); found && strings.EqualFold(strings.ReplaceAll(area.MapPath, "\\", "/"), "Church/Church.map") {
			return room, nil
		}
	}
	return 0, fmt.Errorf("wedding church missing from runtime PVF")
}

func weddingTownInstance(c dnfrepo.CharacterRecord, room byte, channel uint16) (uint32, bool) {
	state, err := wedding.Read(c)
	if err != nil || (state.Status != 2 && state.Status != 3) || state.Room != room || state.ChannelID != channel {
		return 0, false
	}
	return state.PairID, true
}

// currentWeddingGuildDungeon 对应上游的 runtime.guildDungeonID != 0。
//
// 工作区的 runtimeDungeonState 不记"这是不是公会地下城"这个标志：公会净化是通关后
// 由 creditCurrentGuildDungeonClear 用同一份目录反查入账的。所以这里复用同一个
// 判据（目录里能查到该副本 id 即为公会地下城），保持"公会本不算夫妻通关"的语义。
// 目录加载失败时返回 false —— 宁可漏排除，也不要因为读目录失败而吞掉正常通关。
//
// 判据本体已提到 currentGuildDungeonRun（公会贡献度那条规则也要用同一个判据），
// 这里只是保留夫妻系统原有的函数名与语义，行为逐字节不变。
func (s *Service) currentWeddingGuildDungeon(runtime *runtimeDungeonState) bool {
	return s.currentGuildDungeonRun(runtime)
}

// The dungeon owner supplies the actual completion roster, not a town party list.
func (s *Service) currentWeddingDungeonCouple(ctx context.Context, runtime *runtimeDungeonState) (bool, error) {
	if runtime == nil || runtime.ProtocolMode != currentDungeonProtocolModeNormal ||
		runtime.raidDungeon.RaidKey != 0 || s.currentWeddingGuildDungeon(runtime) || runtime.altar != nil ||
		runtime.tournament != nil || runtime.Dungeon.Metadata.NoFatigue ||
		runtime.Dungeon.Metadata.TutorialDungeon.Value != 0 || runtime.Dungeon.Metadata.InfernoDungeon {
		return false, nil
	}
	kind := normalizeDungeonPVFSymbol(runtime.Dungeon.Metadata.DungeonType)
	if kind != "" && kind != "normal" {
		return false, nil
	}
	owner := runtime.partyDungeonOwner
	if owner == nil {
		return false, nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	// Freeze both recipients' eligibility at the first authoritative completion,
	// so the first member returning to town does not invalidate the other's result.
	if owner.weddingClearKnown {
		return owner.weddingClearCouple, nil
	}
	if owner.retired || owner.entryMemberCount != 2 {
		return false, nil
	}
	members := owner.members
	if len(members) != 2 {
		return false, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return false, dnfrepo.ErrRepoMissing
	}
	var states [2]wedding.State
	containsActor := false
	for i, member := range members {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.userID)
		if !online || identity.generation != member.generation {
			return false, nil
		}
		id := strconv.Itoa(int(member.userID))
		containsActor = containsActor || id == runtime.Character.CharacterID
		character, found, err := repos.Character.Load(ctx, id)
		if err != nil {
			return false, err
		}
		if !found || character.AccountID != s.accountIDForSession(identity.session) || character.Stats["delete_flag"] != 0 {
			return false, nil
		}
		states[i], err = wedding.Read(character)
		if err != nil || states[i].Status != 1 {
			return false, nil
		}
	}
	owner.weddingClearCouple = containsActor && states[0].PairID == states[1].PairID &&
		states[0].PartnerID == members[1].userID && states[1].PartnerID == members[0].userID
	owner.weddingClearKnown = true
	return owner.weddingClearCouple, nil
}

func (s *Service) weddingGiftAllocator(cat *wedding.Catalog, kind byte) (func(*dnfrepo.InventoryRecord, wedding.Item) error, error) {
	if kind > 2 {
		return nil, wedding.Invalid
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	gift := cat.Ceremonies[kind].Gift
	definition, err := items.ResolveItem(uint32(gift.ID))
	if err != nil {
		return nil, err
	}
	return func(bag *dnfrepo.InventoryRecord, item wedding.Item) error {
		if item != gift {
			return wedding.Invalid
		}
		_, err := s.grantCurrentWeddingMainProduct(bag, definition, uint32(item.Count))
		return err
	}, nil
}

func (s *Service) restoreWeddingCeremony(session *gameSession, c dnfrepo.CharacterRecord) error {
	state, err := wedding.Read(c)
	if err != nil {
		return err
	}
	if state.Status != 2 && state.Status != 3 {
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return dnfrepo.ErrRepoMissing
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	partner, found, err := repos.Character.Load(ctx, strconv.Itoa(int(state.PartnerID)))
	if err != nil {
		return err
	}
	if !found {
		return wedding.Missing
	}
	var publishErr error
	if state.Status == 2 && s.gameplayNow().Before(time.Unix(state.StartAt, 0)) {
		publishErr = s.publishWeddingReservation([2]dnfrepo.CharacterRecord{c, partner})
	}
	return errors.Join(publishErr, s.scheduleWeddingCeremony(c))
}

func weddingReservationBody(chars [2]dnfrepo.CharacterRecord, state wedding.State, channel uint32, invitations uint16) []byte {
	var w packetWriter
	w.writeUint32(state.PairID)
	w.writeUint32(uint32(state.Room))
	w.writeUint32(uint32(state.StartAt))
	w.writeUint32(channel)
	w.writeByte(state.Ceremony)
	for _, c := range chars {
		name := rosterNameBytes(c.Name)
		w.writeBytes(name)
		w.writeZero(30 - len(name))
	}
	w.writeUint16(invitations)
	return w.bytes()
}

func (s *Service) publishWeddingReservation(chars [2]dnfrepo.CharacterRecord) error {
	state, err := wedding.Read(chars[0])
	if err != nil {
		return err
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return err
	}
	for _, c := range chars {
		if len(rosterNameBytes(c.Name)) > 29 {
			return wedding.Invalid
		}
	}
	for _, c := range chars {
		id, _ := strconv.ParseUint(c.CharacterID, 10, 16)
		identity, online := s.onlineGameSessionCharacterSnapshot(uint16(id))
		if online {
			err = errors.Join(err, s.sendGameUpperRawClass(identity.session, currentWeddingReservationMsgID, weddingReservationBody(chars, state, uint32(state.ChannelID), uint16(cat.Ceremonies[state.Ceremony].Invitations)), 0))
		}
	}
	return err
}

func (s *Service) sendWeddingInventorySnapshot(session *gameSession, bag dnfrepo.InventoryRecord) error {
	if bag.CharacterID == "" {
		return nil
	}
	// Only main-bag contents changed; retain all other native containers and equipped objects.
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	body, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, 0)
	if !ok {
		return fmt.Errorf("wedding inventory snapshot missing")
	}
	return s.sendGameUpperRawClass(session, 13, body, 0)
}

func (s *Service) scheduleWeddingCeremony(c dnfrepo.CharacterRecord) error {
	state, err := wedding.Read(c)
	if err != nil {
		return err
	}
	if state.Status != 2 && state.Status != 3 {
		return nil
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return err
	}
	at := state.StartAt
	if state.Status == 3 {
		at += cat.EndSeconds
	}
	key := fmt.Sprintf("dnf-wedding:%d:%d", state.PairID, state.StartAt)
	return s.ensureGameplayTimeQueue().ScheduleAfter(key, max(time.Duration(0), time.Unix(at, 0).Sub(s.gameplayNow())), func(time.Time) {
		if err := s.advanceWeddingCeremony(c.CharacterID, state.PairID, state.StartAt); err != nil {
			s.logWarn("wedding ceremony deferred", "character_id", c.CharacterID, "error", err)
			// A full bag or transient DB failure leaves the stage intact; retry without charging again.
			_ = s.ensureGameplayTimeQueue().ScheduleAfter(key, 30*time.Second, func(time.Time) {
				repos, ok := s.repositoryGroup()
				if !ok || repos.Character == nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
				defer cancel()
				fresh, found, e := repos.Character.Load(ctx, c.CharacterID)
				if e == nil && found {
					_ = s.scheduleWeddingCeremony(fresh)
				}
			})
		}
	})
}

func (s *Service) advanceWeddingCeremony(characterID string, pair uint32, start int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return dnfrepo.ErrRepoMissing
	}
	c, found, err := repos.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return wedding.Missing
	}
	state, err := wedding.Read(c)
	if err != nil {
		return err
	}
	if state.PairID != pair || state.StartAt != start || (state.Status != 2 && state.Status != 3) {
		return nil
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return err
	}
	cmd := wedding.Command{ActorID: characterID, PartnerID: strconv.Itoa(int(state.PartnerID)), AccountID: c.AccountID, Operation: 3, Now: s.gameplayNow()}
	if state.Status == 3 {
		cmd.Operation = 4
		cmd.Grant, err = s.weddingGiftAllocator(cat, state.Ceremony)
		if err != nil {
			return err
		}
	}
	result, err := wedding.Commit(ctx, repos, cat, cmd)
	if err != nil {
		return err
	}
	var publishErr error
	if cmd.Operation == 3 {
		publishErr = s.publishWeddingCommit(result, 3)
		for _, peer := range s.weddingCeremonyPeers(state) {
			var w packetWriter
			w.writeUint32(pair)
			w.writeUint32(uint32(state.Room))
			for _, character := range result.Characters {
				name := rosterNameBytes(character.Name)
				w.writeBytes(name)
				w.writeZero(30 - len(name))
			}
			publishErr = errors.Join(publishErr, s.sendGameUpperRawClass(peer, currentWeddingStartMsgID, w.bytes(), 0))
		}
		return errors.Join(publishErr, s.scheduleWeddingCeremony(result.Characters[0]))
	}
	publishErr = s.publishWeddingCommit(result, 4)
	for i, character := range result.Characters {
		id, _ := strconv.ParseUint(character.CharacterID, 10, 16)
		if identity, online := s.onlineGameSessionCharacterSnapshot(uint16(id)); online {
			publishErr = errors.Join(publishErr, s.sendWeddingInventorySnapshot(identity.session, result.Inventories[i]))
		}
	}
	// sub_1837CC0 end notification removes the ceremony object and invokes the native exit route.
	for _, peer := range s.weddingCeremonyPeers(state) {
		var w packetWriter
		w.writeByte(1)
		w.writeByte(state.Ceremony)
		for _, character := range result.Characters {
			name := rosterNameBytes(character.Name)
			w.writeBytes(name)
			w.writeZero(30 - len(name))
		}
		w.writeUint32(uint32(state.Room))
		w.writeByte(state.Ceremony)
		w.writeUint32(uint32(peer.channel.ID))
		publishErr = errors.Join(publishErr, s.sendGameUpperRawClass(peer, currentWeddingEndMsgID, w.bytes(), 0))
	}
	return publishErr
}

func (s *Service) weddingCeremonyPeers(state wedding.State) []*gameSession {
	if s.onlinePlayers == nil {
		return nil
	}
	var peers []*gameSession
	for _, p := range s.onlinePlayers.GetAreaInstancePlayers(state.ChannelID, 39, state.Room, state.PairID) {
		if p.Session != nil {
			peers = append(peers, p.Session)
		}
	}
	return peers
}

func (s *Service) handleWeddingEnter(session *gameSession, body []byte) error {
	if len(body) != 22 {
		return nil
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	c, err := s.weddingCharacter(ctx, identity)
	state, stateErr := wedding.Read(c)
	if err == nil {
		err = stateErr
	}
	if err == nil && (body[21] != 0 || state.Status != 2 || uint16(session.channel.ID) != state.ChannelID || binary.LittleEndian.Uint32(body[13:17]) != state.PairID || binary.LittleEndian.Uint32(body[17:21]) != uint32(state.Room) || !s.gameplayNow().Before(time.Unix(state.StartAt, 0))) {
		err = wedding.Error(-6)
	}
	cat, catErr := s.currentWeddingCatalog()
	if err == nil {
		err = catErr
	}
	if err == nil {
		index := 0
		if c.CharacterID > strconv.Itoa(int(state.PartnerID)) {
			index = 1
		}
		pos := cat.Positions[index]
		err = s.handleTownSetUserArea(session, buildCurrentSoloTeleportMoveBody(currentSoloTeleportRequest{TownID: 39, AreaID: state.Room, PositionX: int16(pos[0]), PositionY: int16(pos[1]), Direction: 5}, uint16(c.Stats["town_id"])))
		if err == nil {
			session.townMu.Lock()
			p := session.townPositionSnapshot
			session.townMu.Unlock()
			if p.TownID != 39 || p.AreaID != state.Room {
				err = wedding.Error(-6)
			}
		}
	}
	var code wedding.Error
	if err != nil && !errors.As(err, &code) {
		code = wedding.Error(-6)
	}
	var w packetWriter
	w.writeInt32(int(code))
	return s.sendGameUpperSuccess(session, 1039, w.bytes())
}
