package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
)

// buildCurrentItemLockSnapshotForSession reads the durable lock owner after
// the selected actor's containers have been built. The client does not infer
// this state from an item row, so even an all-unlocked inventory must receive
// the explicit empty op251 list.
func (s *Service) buildCurrentItemLockSnapshotForSession(ctx context.Context, session *gameSession) ([]byte, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, false
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return nil, false
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	record, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		s.logPacketEvent("game-upper-select-item-lock-snapshot-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"char_id", session.selectedCharacterID,
			"reason", "inventory_load_failed",
			"err", err)
		return nil, false
	}
	if !found {
		return dnfitemlock.BuildLockListSnapshot(record), true
	}
	return dnfitemlock.BuildLockListSnapshot(record), true
}

func (s *Service) sendCurrentSelectItemLockSnapshot(session *gameSession, source string, body []byte) error {
	if session == nil {
		return fmt.Errorf("selected item-lock snapshot requires a game session")
	}
	s.logPacketEvent("game-upper-select-item-lock-snapshot-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"msg_id", dnfitemlock.LockListMessageID,
		"classification", 0,
		"body_len", len(body),
		"sequence", "current_exe_op13_selected_containers_then_op251_complete_lock_state")
	return s.sendGameUpperRawClass(session, dnfitemlock.LockListMessageID, body, 0)
}

func (s *Service) sendCurrentSelectedItemLockSnapshot(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	body, ok := s.buildCurrentItemLockSnapshotForSession(ctx, session)
	if !ok {
		return fmt.Errorf("selected character %d item-lock snapshot unavailable", session.selectedCharacterID)
	}
	return s.sendCurrentSelectItemLockSnapshot(session, source, body)
}

// sendCurrentSelectedItemLockSnapshotIfAny replays the lock list only when at
// least one row is actually locked.
//
// The client keeps the blue lock icons in its own op251 list, which an op14 row
// refresh makes it rebuild and drop (live 2026-09-15 07:36: opening a package
// cleared the equipment and avatar locks until the player sorted the bag, whose
// container refresh does re-send op251). So any path that hands out or rewrites
// rows has to replay the list afterwards - but replaying an empty list costs a
// packet for every character who has nothing locked, and an all-unlocked client
// already holds the correct (empty) state, since the explicit empty sentinel is
// sent on character select. This variant therefore skips the empty case.
func (s *Service) sendCurrentSelectedItemLockSnapshotIfAny(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	body, ok := s.buildCurrentItemLockSnapshotForSession(ctx, session)
	if !ok {
		return fmt.Errorf("selected character %d item-lock snapshot unavailable", session.selectedCharacterID)
	}
	// Layout: u16 entry count, then 5 bytes (or 7 with the pending remainder)
	// per locked row. The empty-list sentinel carries a zero count only.
	if len(body) < 2 || (body[0] == 0 && body[1] == 0) {
		return nil
	}
	return s.sendCurrentSelectItemLockSnapshot(session, source+"_had_locked_rows", body)
}
