package dnfbridge

import (
	"strings"
	"unicode/utf16"

	"golang.org/x/text/encoding/simplifiedchinese"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func rosterSlot(value int) int {
	if value < 0 {
		return 0
	}
	if value > 0xffff {
		return 0xffff
	}
	return value
}

func rosterByteValue(value int64, fallback int) byte {
	if value == 0 {
		value = int64(fallback)
	}
	if value < 0 {
		return 0
	}
	if value > 0xff {
		return 0xff
	}
	return byte(value)
}

func rosterHeaderByte(value int64, fallback int) byte {
	if value < 0 || value > 0xff {
		value = int64(fallback)
	}
	return byte(value)
}

func rosterUint16Value(value int64, fallback int) uint16 {
	if value == 0 {
		value = int64(fallback)
	}
	if value < 0 {
		return 0
	}
	if value > 0xffff {
		return 0xffff
	}
	return uint16(value)
}

func clampRosterUint16(value int64) uint16 {
	if value < 0 {
		return 0
	}
	if value > 0xffff {
		return 0xffff
	}
	return uint16(value)
}

func rosterUint32Value(value int64, fallback int) uint32 {
	if value == 0 {
		value = int64(fallback)
	}
	if value < 0 {
		return 0
	}
	if value > 0xffffffff {
		return 0xffffffff
	}
	return uint32(value)
}

func rosterNameBytes(name string) []byte {
	if name == "" {
		return nil
	}
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(name))
	if err != nil || len(encoded) == 0 {
		encoded = []byte(name)
	}
	if len(encoded) > rosterNameMaxBytes {
		return encoded[:rosterNameMaxBytes]
	}
	return encoded
}

func rosterRawNameBytes(character dnfrepo.CharacterRecord) []byte {
	return rosterNameBytes(character.Name)
}

func currentCharacterRenameRosterName(character dnfrepo.CharacterRecord) string {
	if character.Stats == nil || character.Stats[currentCharacterRenameEntitlementStat] <= 0 {
		return character.Name
	}
	var projected strings.Builder
	projected.WriteString(currentCharacterRenameRosterMarker)
	wideUnits := len(utf16.Encode([]rune(currentCharacterRenameRosterMarker)))
	encodedBytes := len(currentCharacterRenameRosterMarker)
	for _, value := range character.Name {
		valueBytes, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(string(value)))
		if err != nil {
			break
		}
		valueWideUnits := utf16.RuneLen(value)
		if valueWideUnits < 1 || wideUnits+valueWideUnits > currentCharacterRenameRosterWideUnits ||
			encodedBytes+len(valueBytes) > rosterNameMaxBytes {
			break
		}
		projected.WriteRune(value)
		wideUnits += valueWideUnits
		encodedBytes += len(valueBytes)
	}
	return projected.String()
}

func currentCharacterRenameRosterNameBytes(character dnfrepo.CharacterRecord) []byte {
	return rosterNameBytes(currentCharacterRenameRosterName(character))
}

func rosterLevel(character dnfrepo.CharacterRecord) byte {
	if character.Level <= 0 {
		return 1
	}
	if character.Level > 255 {
		return 255
	}
	return byte(character.Level)
}
