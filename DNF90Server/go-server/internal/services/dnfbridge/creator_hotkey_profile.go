package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var currentPVFHotkeyVer2RE = regexp.MustCompile("(?is)\\[key\\]\\s*`([^`]*)`\\s*`([^`]*)`\\s*`[^`]*`\\s*`[^`]*`\\s+(-?\\d+)")

type currentPVFHotkeyEntry struct {
	name        string
	identifier  string
	externalKey int
}

type currentCreatorHotkeyProfile struct {
	slots  []byte
	keymap []byte
}

var currentExeInternalHotkeyByExternalOverride = map[int]uint16{
	// Do not override physical A (VK 65). A live Creator op198 save echoes
	// move-left I02 as internal index 55; index 40 renders as Numpad_5 in the
	// current client. The normal PVF/default table already resolves A to 55.
	// K02 is the dungeon-special Tab binding. Non-I entries have no slot in
	// the inherited table, while the current EXE persists Tab as index 31.
	9: 31,
}

func parseCurrentPVFHotkeyVer2Entries(text string) ([]currentPVFHotkeyEntry, error) {
	matches := currentPVFHotkeyVer2RE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("hotkey ver2 PVF has no [key] entries")
	}
	entries := make([]currentPVFHotkeyEntry, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		identifier := strings.ToUpper(strings.TrimSpace(match[2]))
		if identifier == "" {
			return nil, fmt.Errorf("hotkey ver2 PVF contains an empty identifier")
		}
		if _, exists := seen[identifier]; exists {
			return nil, fmt.Errorf("hotkey ver2 PVF contains duplicate identifier %s", identifier)
		}
		externalKey, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, fmt.Errorf("hotkey ver2 PVF identifier %s has invalid key %q", identifier, match[3])
		}
		seen[identifier] = struct{}{}
		entries = append(entries, currentPVFHotkeyEntry{
			name:        strings.TrimSpace(match[1]),
			identifier:  identifier,
			externalKey: externalKey,
		})
	}
	return entries, nil
}

func currentPVFHotkeyIIndex(identifier string) (int, bool) {
	identifier = strings.ToUpper(strings.TrimSpace(identifier))
	if len(identifier) < 2 || identifier[0] != 'I' {
		return 0, false
	}
	value, err := strconv.Atoi(identifier[1:])
	if err != nil || value <= 0 {
		return 0, false
	}
	return value - 1, true
}

func currentDefaultInternalHotkey(slots []byte, identifier string) (uint16, bool) {
	index, ok := currentPVFHotkeyIIndex(identifier)
	if !ok || index*2+2 > len(slots) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(slots[index*2 : index*2+2]), true
}

func buildCurrentCreatorHotkeyProfile(
	normalEntries []currentPVFHotkeyEntry,
	creatorEntries []currentPVFHotkeyEntry,
	defaultSlots []byte,
) (currentCreatorHotkeyProfile, error) {
	if len(defaultSlots) == 0 || len(defaultSlots)%2 != 0 {
		return currentCreatorHotkeyProfile{}, fmt.Errorf("normal hotkey slot table length %d is invalid", len(defaultSlots))
	}
	normalByID := make(map[string]currentPVFHotkeyEntry, len(normalEntries))
	externalToInternal := make(map[int]uint16, len(normalEntries))
	for _, entry := range normalEntries {
		normalByID[entry.identifier] = entry
		internal, ok := currentDefaultInternalHotkey(defaultSlots, entry.identifier)
		if ok && entry.externalKey >= 0 {
			externalToInternal[entry.externalKey] = internal
		}
	}
	for external, internal := range currentExeInternalHotkeyByExternalOverride {
		externalToInternal[external] = internal
	}

	creatorByID := make(map[string]currentPVFHotkeyEntry, len(creatorEntries))
	for _, entry := range creatorEntries {
		creatorByID[entry.identifier] = entry
	}
	// Creator owns five skill selectors. The operator-requested 1..5 layout is
	// taken from the same PVF's item-shortcut rows, keeping physical-key data in
	// Script.pvf rather than embedding platform key codes in the bridge.
	for offset := 0; offset < 5; offset++ {
		skillID := fmt.Sprintf("I%02d", 21+offset)
		digitID := fmt.Sprintf("I%02d", 33+offset)
		skill, skillFound := creatorByID[skillID]
		digit, digitFound := creatorByID[digitID]
		if !skillFound || !digitFound || digit.externalKey < 0 {
			return currentCreatorHotkeyProfile{}, fmt.Errorf("creator PVF cannot map %s from %s", skillID, digitID)
		}
		skill.externalKey = digit.externalKey
		creatorByID[skillID] = skill
	}

	slots := append([]byte(nil), defaultSlots...)
	var keymap bytes.Buffer
	for _, original := range creatorEntries {
		entry := creatorByID[original.identifier]
		internal := uint16(csharpHotkeyUnassignedKey)
		if entry.externalKey >= 0 {
			resolved, ok := externalToInternal[entry.externalKey]
			if !ok {
				if _, sameID := normalByID[entry.identifier]; sameID {
					resolved, ok = currentDefaultInternalHotkey(defaultSlots, entry.identifier)
				}
			}
			if !ok {
				// Some Creator-only legacy commands use physical key codes which
				// have no representation in the current normal profile. Leaving
				// those optional commands unassigned is safer than sending a raw
				// Windows key code as a current-EXE internal key index.
				resolved = csharpHotkeyUnassignedKey
			}
			internal = resolved
		}
		if index, ok := currentPVFHotkeyIIndex(entry.identifier); ok && index*2+2 <= len(slots) {
			binary.LittleEndian.PutUint16(slots[index*2:index*2+2], internal)
		}
		fmt.Fprintf(&keymap, "%s\t%d\n", entry.identifier, internal)
	}
	if keymap.Len() == 0 {
		return currentCreatorHotkeyProfile{}, fmt.Errorf("creator hotkey keymap is empty")
	}
	return currentCreatorHotkeyProfile{slots: slots, keymap: keymap.Bytes()}, nil
}

func (s *Service) currentCreatorHotkeyProfile(ctx context.Context) (currentCreatorHotkeyProfile, error) {
	if s == nil {
		return currentCreatorHotkeyProfile{}, fmt.Errorf("dnfbridge service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return currentCreatorHotkeyProfile{}, err
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return currentCreatorHotkeyProfile{}, err
	}
	normalText, _, err := readInitialPVFText(archive, csharpNormalHotkeyPVFPath)
	if err != nil {
		return currentCreatorHotkeyProfile{}, err
	}
	creatorText, _, err := readInitialPVFText(archive, csharpCreatorHotkeyPVFPath)
	if err != nil {
		return currentCreatorHotkeyProfile{}, err
	}
	normalEntries, err := parseCurrentPVFHotkeyVer2Entries(normalText)
	if err != nil {
		return currentCreatorHotkeyProfile{}, fmt.Errorf("parse %s: %w", csharpNormalHotkeyPVFPath, err)
	}
	creatorEntries, err := parseCurrentPVFHotkeyVer2Entries(creatorText)
	if err != nil {
		return currentCreatorHotkeyProfile{}, fmt.Errorf("parse %s: %w", csharpCreatorHotkeyPVFPath, err)
	}
	return buildCurrentCreatorHotkeyProfile(normalEntries, creatorEntries, csharpDefaultHotkeySlots)
}

func currentSpecialJobOwnsCharacterKeymap(job byte) bool {
	return job == csharpCreatorMageJob
}
