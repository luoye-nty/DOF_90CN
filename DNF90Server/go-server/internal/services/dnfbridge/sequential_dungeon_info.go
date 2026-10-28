package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentSequentialDungeonPVFPath       = "etc/sequential_dungeon_info.etc"
	currentSequentialDungeonProgressMsgID = 0x025b
	currentSequentialDungeonRequestSize   = 4
	currentSequentialDungeonSuccessFlag   = 1
)

var errCurrentSequentialDungeonCatalogInvalid = errors.New("current sequential-dungeon PVF catalog is invalid")

type currentSequentialDungeonDefinition struct {
	ID         uint32
	DungeonIDs []uint32
}

type currentSequentialDungeonCatalog struct {
	definitions map[uint32]currentSequentialDungeonDefinition
}

func (catalog *currentSequentialDungeonCatalog) Find(id uint32) (currentSequentialDungeonDefinition, bool) {
	if catalog == nil {
		return currentSequentialDungeonDefinition{}, false
	}
	definition, found := catalog.definitions[id]
	return definition, found
}

func loadCurrentSequentialDungeonCatalog(source dnfpvf.Source) (*currentSequentialDungeonCatalog, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: source required", errCurrentSequentialDungeonCatalogInvalid)
	}
	text, err := source.ReadText(currentSequentialDungeonPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", currentSequentialDungeonPVFPath, err)
	}
	document, err := dnfpvf.Parse(currentSequentialDungeonPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", currentSequentialDungeonPVFPath, err)
	}
	return parseCurrentSequentialDungeonCatalog(document)
}

func parseCurrentSequentialDungeonCatalog(document *dnfpvf.Document) (*currentSequentialDungeonCatalog, error) {
	if document == nil {
		return nil, fmt.Errorf("%w: document required", errCurrentSequentialDungeonCatalogInvalid)
	}
	catalog := &currentSequentialDungeonCatalog{definitions: make(map[uint32]currentSequentialDungeonDefinition)}
	var current *currentSequentialDungeonDefinition
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		values, err := currentSequentialDungeonSectionInts(document, section)
		if err != nil {
			return nil, err
		}
		switch name {
		case "sequential dungeon":
			if len(values) != 1 || values[0] <= 0 || values[0] > math.MaxUint32 {
				return nil, fmt.Errorf("%w: path=%s section=%q values=%v", errCurrentSequentialDungeonCatalogInvalid, document.Path, section.Name, values)
			}
			id := uint32(values[0])
			if _, duplicate := catalog.definitions[id]; duplicate {
				return nil, fmt.Errorf("%w: path=%s duplicate_sequence=%d", errCurrentSequentialDungeonCatalogInvalid, document.Path, id)
			}
			definition := currentSequentialDungeonDefinition{ID: id}
			catalog.definitions[id] = definition
			current = &definition
		case "dungeon index check":
			if current == nil {
				return nil, fmt.Errorf("%w: path=%s dungeon list precedes sequence", errCurrentSequentialDungeonCatalogInvalid, document.Path)
			}
			if len(values) == 0 || len(values) > math.MaxUint8 {
				return nil, fmt.Errorf("%w: path=%s sequence=%d dungeon_count=%d", errCurrentSequentialDungeonCatalogInvalid, document.Path, current.ID, len(values))
			}
			seen := make(map[uint32]struct{}, len(values))
			dungeonIDs := make([]uint32, 0, len(values))
			for _, value := range values {
				if value <= 0 || value > math.MaxUint32 {
					return nil, fmt.Errorf("%w: path=%s sequence=%d dungeon_id=%d", errCurrentSequentialDungeonCatalogInvalid, document.Path, current.ID, value)
				}
				dungeonID := uint32(value)
				if _, duplicate := seen[dungeonID]; duplicate {
					return nil, fmt.Errorf("%w: path=%s sequence=%d duplicate_dungeon=%d", errCurrentSequentialDungeonCatalogInvalid, document.Path, current.ID, dungeonID)
				}
				seen[dungeonID] = struct{}{}
				dungeonIDs = append(dungeonIDs, dungeonID)
			}
			current.DungeonIDs = dungeonIDs
			catalog.definitions[current.ID] = *current
		}
	}
	if len(catalog.definitions) == 0 {
		return nil, fmt.Errorf("%w: path=%s no sequences", errCurrentSequentialDungeonCatalogInvalid, document.Path)
	}
	for id, definition := range catalog.definitions {
		if len(definition.DungeonIDs) == 0 {
			return nil, fmt.Errorf("%w: path=%s sequence=%d has no dungeon index check", errCurrentSequentialDungeonCatalogInvalid, document.Path, id)
		}
	}
	return catalog, nil
}

func currentSequentialDungeonSectionInts(document *dnfpvf.Document, section dnfpvf.Section) ([]int64, error) {
	if section.Start < 0 || section.Start > section.End || section.End > len(document.Tokens) {
		return nil, fmt.Errorf("%w: path=%s section=%q range=%d:%d", errCurrentSequentialDungeonCatalogInvalid, document.Path, section.Name, section.Start, section.End)
	}
	values := make([]int64, 0, section.End-section.Start)
	for _, token := range document.Tokens[section.Start:section.End] {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values, nil
}

func (s *Service) currentSequentialDungeonDefinitions() (*currentSequentialDungeonCatalog, error) {
	if s == nil {
		return nil, errCurrentSequentialDungeonCatalogInvalid
	}
	s.sequentialDungeonMu.Lock()
	defer s.sequentialDungeonMu.Unlock()
	if s.sequentialDungeonCatalog != nil {
		return s.sequentialDungeonCatalog, nil
	}
	if s.sequentialDungeonCatalogErr != nil {
		return nil, s.sequentialDungeonCatalogErr
	}
	archive, err := s.initialEquipmentArchiveLocked()
	if err == nil {
		s.sequentialDungeonCatalog, err = loadCurrentSequentialDungeonCatalog(archive)
	}
	if err != nil {
		s.sequentialDungeonCatalogErr = err
		return nil, err
	}
	return s.sequentialDungeonCatalog, nil
}

func buildCurrentSequentialDungeonProgressBody(
	definition currentSequentialDungeonDefinition,
	permission dnfrepo.DungeonPermissionRecord,
) []byte {
	cleared := make(map[uint32]bool, len(permission.Entries))
	for _, entry := range permission.Entries {
		if entry.DungeonID != 0 && entry.ClearState != 0 {
			cleared[entry.DungeonID] = true
		}
	}
	var writer packetWriter
	// Current EXE sub_1D25180 success/full-snapshot grammar:
	// u8 mode(1), u32 sequence id, u8 row count, then u8 index + u8 value.
	writer.writeByte(1)
	writer.writeUint32(definition.ID)
	writer.writeByte(byte(len(definition.DungeonIDs)))
	for index, dungeonID := range definition.DungeonIDs {
		writer.writeByte(byte(index))
		if cleared[dungeonID] {
			writer.writeByte(1)
		} else {
			writer.writeByte(0)
		}
	}
	return writer.bytes()
}

func (s *Service) handleCurrentSequentialDungeonInfo(session *gameSession, body []byte) error {
	if len(body) != currentSequentialDungeonRequestSize {
		s.logGameEvent(session, "game-current-sequential-dungeon-op869-rejected",
			"body_len", len(body),
			"reason", "current_exe_u32_sequence_id_required")
		return s.sendCurrentSequentialDungeonInfoAck(session, false)
	}
	sequenceID := binary.LittleEndian.Uint32(body)
	catalog, err := s.currentSequentialDungeonDefinitions()
	if err != nil {
		s.logGameEvent(session, "game-current-sequential-dungeon-op869-rejected",
			"sequence_id", sequenceID,
			"reason", "pvf_catalog_unavailable",
			"error", err)
		return s.sendCurrentSequentialDungeonInfoAck(session, false)
	}
	definition, found := catalog.Find(sequenceID)
	if !found || session == nil || session.selectedCharacterID == 0 {
		s.logGameEvent(session, "game-current-sequential-dungeon-op869-rejected",
			"sequence_id", sequenceID,
			"reason", "unknown_pvf_sequence_or_no_selected_character")
		return s.sendCurrentSequentialDungeonInfoAck(session, false)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.DungeonPermission == nil {
		s.logGameEvent(session, "game-current-sequential-dungeon-op869-rejected",
			"sequence_id", sequenceID,
			"reason", "dungeon_permission_repository_unavailable")
		return s.sendCurrentSequentialDungeonInfoAck(session, false)
	}
	snapshot, err := s.currentAccountDungeonPermissionSnapshot(
		context.Background(),
		session,
		repositories,
	)
	if err != nil {
		s.logGameEvent(session, "game-current-sequential-dungeon-op869-rejected",
			"sequence_id", sequenceID,
			"char_id", session.selectedCharacterID,
			"reason", "account_dungeon_permission_load_failed",
			"error", err)
		return s.sendCurrentSequentialDungeonInfoAck(session, false)
	}

	progressBody := buildCurrentSequentialDungeonProgressBody(definition, snapshot.Record)
	// op5 in classification 1 is CREATE_CHARACTER for the current EXE. The
	// complete classification-0 ordinary-difficulty authority was already sent
	// when entering the selector, so emitting a partial op5 here is both
	// redundant and displays the native "character created" dialog. Current
	// EXE sub_1D25180 consumes this sequential projection as classification 0.
	if err := s.sendGameUpperRawClass(session, currentSequentialDungeonProgressMsgID, progressBody, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-current-sequential-dungeon-op603-snapshot-send",
		"sequence_id", sequenceID,
		"account_id", snapshot.AccountID,
		"char_id", session.selectedCharacterID,
		"msg_id", currentSequentialDungeonProgressMsgID,
		"classification", 0,
		"row_count", len(definition.DungeonIDs),
		"body_len", len(progressBody),
		"body_source", "current_exe_sub_1D25180_mode1_u32_sequence_u8_count_u8_index_u8_value",
		"state_source", "pvf_dungeon_index_check_intersect_account_shared_dungeon_permission")
	return s.sendCurrentSequentialDungeonInfoAck(session, true)
}

func (s *Service) sendCurrentSequentialDungeonInfoAck(session *gameSession, success bool) error {
	body := []byte{0, 0}
	if success {
		body[0] = currentSequentialDungeonSuccessFlag
	}
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketSequentialDungeonInfo),
		body,
		dnfproto.DefaultChannelClassification,
	)
}
