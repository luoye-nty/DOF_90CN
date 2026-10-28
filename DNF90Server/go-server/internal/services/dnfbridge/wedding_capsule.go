package dnfbridge

import (
	"context"
	"errors"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

func (s *Service) currentWeddingCapsuleCatalog() (*wedding.CapsuleCatalog, error) {
	s.weddingCatalogMu.Lock()
	defer s.weddingCatalogMu.Unlock()
	if s.weddingCapsuleCatalog != nil || s.weddingCapsuleCatalogErr != nil {
		return s.weddingCapsuleCatalog, s.weddingCapsuleCatalogErr
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err == nil {
		s.weddingCapsuleCatalog, err = wedding.LoadCapsuleCatalog(archive)
	}
	s.weddingCapsuleCatalogErr = err
	return s.weddingCapsuleCatalog, err
}

// Current EXE sub_1831F30: u32 progress, u8 own, u8 pending, u8 spousePending.
func weddingCapsuleStateBody(state wedding.CapsuleState, spousePending bool) []byte {
	var w packetWriter
	w.writeUint32(uint32(state.Progress))
	w.writeByte(byte(state.Own))
	w.writeByte(byte(state.Taken))
	if spousePending {
		w.writeByte(1)
	} else {
		w.writeByte(0)
	}
	return w.bytes()
}

func weddingCapsuleAckBody(opcode uint16, code int32, state wedding.CapsuleState) []byte {
	var w packetWriter
	w.writeInt32(int(code))
	if code == 0 {
		switch opcode {
		case 1052:
			w.writeUint32(uint32(state.Progress))
			w.writeByte(byte(state.Own))
		case 1053:
			w.writeByte(byte(state.Own))
		case 1054:
			w.writeByte(byte(state.Taken))
		}
	}
	return w.bytes()
}

func weddingCapsuleErrorCode(opcode uint16, err error) int32 {
	if err == nil {
		return 0
	}
	if errors.Is(err, wedding.ErrCapsuleEnergy) {
		return -4
	}
	if errors.Is(err, wedding.ErrCapsuleEmpty) && opcode == 1053 {
		return -4
	}
	if errors.Is(err, wedding.ErrCapsuleBag) {
		return -6
	}
	if errors.Is(err, wedding.ErrCapsuleLimit) {
		return map[uint16]int32{1052: -5, 1053: -6, 1054: -7}[opcode]
	}
	// Several native branches have mismatched localized text (e.g. take -4
	// asks to enter the chapel). Use its generic wedding failure for those.
	return -1
}

func (s *Service) weddingCapsuleCommand(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 0 || opcode < 1052 || opcode > 1054 {
		return nil
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	rejectTown := func() error {
		code := map[uint16]int32{1052: -6, 1053: -7, 1054: -8}[opcode]
		return s.sendGameUpperSuccess(session, opcode, weddingCapsuleAckBody(opcode, code, wedding.CapsuleState{}))
	}
	// Settlement owns dungeon -> wedding order; never take these in reverse.
	if !session.dungeon.mu.TryLock() {
		return rejectTown()
	}
	defer session.dungeon.mu.Unlock()
	if session.dungeon.runtime != nil {
		return rejectTown()
	}
	if !session.townMu.TryLock() {
		return rejectTown()
	}
	pos, ready := session.townPositionSnapshot, session.townSceneReadyCharacterID == identity.character
	session.townMu.Unlock()
	if !ready || pos.CharacterID != identity.character || (opcode == 1054 && pos.TownID != 101) {
		return rejectTown()
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	var result wedding.CapsuleResult
	if err == nil {
		result, err = s.commitWeddingCapsule(ctx, session, c, opcode)
	}
	ackErr := s.sendGameUpperSuccess(session, opcode, weddingCapsuleAckBody(opcode, weddingCapsuleErrorCode(opcode, err), result.States[0]))
	if err != nil {
		s.logGameEvent(session, "game-wedding-capsule-rejected", "opcode", opcode, "char_id", identity.character, "error", err)
		return ackErr
	}
	if result.AssetsChanged {
		ackErr = errors.Join(ackErr, s.sendWeddingInventorySnapshot(session, result.Inventories[0]))
	}
	if opcode != 1052 {
		for i, c := range result.Characters {
			id, _ := strconv.ParseUint(c.CharacterID, 10, 16)
			peer, online := s.onlineGameSessionCharacterSnapshot(uint16(id))
			if !online {
				continue
			}
			ackErr = errors.Join(ackErr, s.sendGameUpperRawClass(peer.session, 1015, weddingCapsuleStateBody(result.States[i], result.States[1-i].Taken > 0), 0))
		}
	}
	s.logGameEvent(session, "game-wedding-capsule-committed", "opcode", opcode, "char_id", identity.character, "progress", result.States[0].Progress, "own", result.States[0].Own, "pending", result.States[0].Taken, "granted", result.Granted)
	return ackErr
}

func (s *Service) commitWeddingCapsule(ctx context.Context, session *gameSession, c dnfrepo.CharacterRecord, opcode uint16) (wedding.CapsuleResult, error) {
	state, err := wedding.Read(c)
	if err != nil {
		return wedding.CapsuleResult{}, err
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return wedding.CapsuleResult{}, err
	}
	rules, err := s.currentWeddingCapsuleCatalog()
	if err != nil {
		return wedding.CapsuleResult{}, err
	}
	cmd := wedding.CapsuleCommand{ActorID: c.CharacterID, PartnerID: strconv.Itoa(int(state.PartnerID)), AccountID: s.accountIDForSession(session), Opcode: opcode, Now: s.gameplayNow()}
	if opcode == 1054 {
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			return wedding.CapsuleResult{}, err
		}
		def, err := items.ResolveItem(uint32(rules.ItemID))
		if err != nil {
			return wedding.CapsuleResult{}, err
		}
		cmd.Grant = func(bag *dnfrepo.InventoryRecord, item wedding.Item) error {
			_, err := s.grantCurrentWeddingMainProduct(bag, def, uint32(item.Count))
			return err
		}
	}
	repos, _ := s.repositoryGroup()
	return wedding.CommitCapsule(ctx, repos, cat, rules, cmd)
}

func (s *Service) sendWeddingCapsuleState(session *gameSession, c dnfrepo.CharacterRecord) error {
	w, err := wedding.Read(c)
	if err != nil {
		return err
	}
	if w.Status != 4 {
		return s.sendGameUpperRawClass(session, 1015, weddingCapsuleStateBody(wedding.CapsuleState{}, false), 0)
	}
	rules, err := s.currentWeddingCapsuleCatalog()
	if err != nil {
		return err
	}
	state, err := wedding.ReadCapsule(c, rules)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return wedding.Invalid
	}
	spouse, found, err := repos.Character.Load(ctx, strconv.Itoa(int(w.PartnerID)))
	if err != nil {
		return err
	}
	if !found {
		return wedding.Missing
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return err
	}
	if err := wedding.Validate(c, spouse, cat, 9, s.gameplayNow()); err != nil {
		return err
	}
	other, err := wedding.ReadCapsule(spouse, rules)
	if err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, 1015, weddingCapsuleStateBody(state, other.Taken > 0), 0)
}
