package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfepicwish "longheng.io/server/internal/modules/dnf/epicwish"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentEpicWishSnapshotWireOpcode uint16 = 1288

var (
	errCurrentEpicWishRequestInvalid = errors.New("dnf epic wish request is invalid")
	errCurrentEpicWishUnavailable    = errors.New("dnf epic wish state is unavailable")
)

func decodeCurrentEpicWishUpdate(body []byte) ([]uint32, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: body is empty", errCurrentEpicWishRequestInvalid)
	}
	count := int(body[0])
	if count > dnfepicwish.MaximumItems || len(body) != 1+count*4 {
		return nil, fmt.Errorf(
			"%w: count=%d body_len=%d",
			errCurrentEpicWishRequestInvalid,
			count,
			len(body),
		)
	}
	items := make([]uint32, count)
	seen := make(map[uint32]struct{}, count)
	for index := range items {
		items[index] = binary.LittleEndian.Uint32(body[1+index*4:])
		if items[index] == 0 {
			continue
		}
		if _, duplicate := seen[items[index]]; duplicate {
			return nil, fmt.Errorf(
				"%w: duplicate item=%d",
				errCurrentEpicWishRequestInvalid,
				items[index],
			)
		}
		seen[items[index]] = struct{}{}
	}
	return items, nil
}

func buildCurrentEpicWishSnapshotBody(items []uint32) ([]byte, error) {
	if len(items) > dnfepicwish.MaximumItems {
		return nil, fmt.Errorf(
			"%w: count=%d",
			errCurrentEpicWishRequestInvalid,
			len(items),
		)
	}
	body := make([]byte, 1+len(items)*4)
	body[0] = byte(len(items))
	for index, itemID := range items {
		binary.LittleEndian.PutUint32(body[1+index*4:], itemID)
	}
	return body, nil
}

func (s *Service) validateCurrentEpicWishItems(items []uint32) error {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return fmt.Errorf("%w: load item catalog: %v", errCurrentEpicWishUnavailable, err)
	}
	for _, itemID := range items {
		if itemID == 0 {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil || definition.Kind != dungeonDropItemEquipment || definition.Rarity != 4 {
			return fmt.Errorf(
				"%w: item=%d is not PVF epic equipment: %v",
				errCurrentEpicWishRequestInvalid,
				itemID,
				resolveErr,
			)
		}
	}
	return nil
}

// currentEpicWishItemsForCharacter revalidates durable IDs against the active
// PVF before they can be projected or influence a reward. A removed or
// downgraded item keeps its client slot as zero; a malformed persisted list or
// an unavailable catalog fails closed to an empty preference list.
func (s *Service) currentEpicWishItemsForCharacter(character dnfrepo.CharacterRecord) ([]uint32, error) {
	items, err := dnfepicwish.ItemsFromCharacter(character)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	items = append([]uint32(nil), items...)
	for index, itemID := range items {
		if itemID == 0 {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil || definition.Kind != dungeonDropItemEquipment || definition.Rarity != 4 {
			items[index] = 0
		}
	}
	return items, nil
}

func (s *Service) sendCurrentEpicWishSnapshot(
	session *gameSession,
	characterID string,
	items []uint32,
	source string,
) error {
	body, err := buildCurrentEpicWishSnapshotBody(items)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentEpicWishSnapshotWireOpcode,
		body,
		0,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-epic-wish-snapshot-sent",
		"source", source,
		"character_id", characterID,
		"item_count", len(items),
		"wire_opcode", currentEpicWishSnapshotWireOpcode,
		"classification", 0)
	return nil
}

func (s *Service) sendCurrentDungeonEpicWishSnapshot(
	session *gameSession,
	runtime *runtimeDungeonState,
	source string,
) error {
	if runtime == nil || !currentDungeonTimeRiftRuntime(runtime) {
		return nil
	}
	items, err := s.currentEpicWishItemsForCharacter(runtime.Character)
	if err != nil {
		// The op29 scene is already committed to the socket at this boundary.
		// Publish an empty authoritative vector instead of tearing down a valid
		// dungeon session because an old Stats row or its PVF item disappeared.
		s.logGameEvent(session, "game-epic-wish-snapshot-sanitized",
			"source", source,
			"character_id", runtime.Character.CharacterID,
			"reason", err)
		items = nil
	}
	return s.sendCurrentEpicWishSnapshot(
		session,
		runtime.Character.CharacterID,
		items,
		source,
	)
}

func (s *Service) sendCurrentEpicWishUpdateFailure(session *gameSession, reason error) error {
	if session == nil {
		if reason != nil {
			return reason
		}
		return errCurrentEpicWishUnavailable
	}
	s.logGameEvent(session, "game-epic-wish-update-blocked",
		"character_id", session.selectedCharacterID,
		"reason", reason)
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketUpdateWishItem),
		[]byte{0},
		dnfproto.DefaultChannelClassification,
	)
}

func (s *Service) handleCurrentEpicWishUpdate(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return s.sendCurrentEpicWishUpdateFailure(session, errCurrentEpicWishUnavailable)
	}
	items, err := decodeCurrentEpicWishUpdate(body)
	if err != nil {
		return s.sendCurrentEpicWishUpdateFailure(session, err)
	}
	if err := s.validateCurrentEpicWishItems(items); err != nil {
		return s.sendCurrentEpicWishUpdateFailure(session, err)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.sendCurrentEpicWishUpdateFailure(session, errCurrentEpicWishUnavailable)
	}
	owner, err := dnfepicwish.NewOwner(repositories)
	if err != nil {
		return s.sendCurrentEpicWishUpdateFailure(session, err)
	}
	baseContext := context.Background()
	if session.ctx != nil {
		baseContext = session.ctx
	}
	ctx, cancel := context.WithTimeout(baseContext, createWriteTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	result, err := owner.Apply(ctx, dnfepicwish.Command{
		CharacterID: characterID,
		AccountID:   strings.TrimSpace(s.accountIDForSession(session)),
		Items:       items,
	})
	if err != nil {
		return s.sendCurrentEpicWishUpdateFailure(session, err)
	}

	session.dungeon.mu.Lock()
	if runtime := session.dungeon.runtime; dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		runtime.Character = dnfrepo.CloneCharacter(result.Character)
	}
	session.dungeon.mu.Unlock()

	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketUpdateWishItem),
		[]byte{1},
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	if err := s.sendCurrentEpicWishSnapshot(
		session,
		result.Character.CharacterID,
		result.Items,
		"op1352_commit",
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-epic-wish-update-applied",
		"character_id", characterID,
		"item_count", len(result.Items),
		"changed", result.Changed)
	return nil
}
