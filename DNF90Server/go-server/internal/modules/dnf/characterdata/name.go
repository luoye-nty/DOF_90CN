package characterdata

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	CharacterNameMinBytes = 2
	CharacterNameMaxBytes = 30
	// CharacterNameMaxSpaces 名字里最多允许几个空格（半角 U+0020 与全角 U+3000 各算一个）。
	//
	// 2026-10-17 运营要求放开"DIY 名字带空格"（对照另一款服的 v1 空格上限 3 个）。
	// 这里只放行人名里常见的这两种空格；NBSP、制表符、换行、行/段分隔符等空白继续拒绝，
	// 并且仍然要求至少有一个可见字符（不能是纯空格名）。
	CharacterNameMaxSpaces = 3
)

var ErrCharacterNameInvalid = errors.New("character name is invalid")

// ValidateCharacterName owns the durable-name safety boundary shared by
// create, duplicate-check, and rename. It keeps the current client's GB18030
// byte limits while rejecting invisible/control data and punctuation already
// unsafe in the current NoPack UI/protocol paths.
//
// 空白策略：只放行最多 CharacterNameMaxSpaces 个 U+0020/U+3000 空格，其余空白一律拒绝。
func ValidateCharacterName(name string) error {
	if name == "" || !utf8.ValidString(name) {
		return fmt.Errorf("%w: name must be non-empty valid UTF-8", ErrCharacterNameInvalid)
	}
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(name))
	if err != nil {
		return fmt.Errorf("%w: encode GB18030: %v", ErrCharacterNameInvalid, err)
	}
	if len(encoded) < CharacterNameMinBytes || len(encoded) > CharacterNameMaxBytes {
		return fmt.Errorf("%w: encoded length %d is outside %d..%d",
			ErrCharacterNameInvalid, len(encoded), CharacterNameMinBytes, CharacterNameMaxBytes)
	}
	spaces, visible := 0, 0
	for _, value := range name {
		if value == ' ' || value == '\u3000' {
			spaces++
			if spaces > CharacterNameMaxSpaces {
				return fmt.Errorf("%w: name contains more than %d spaces",
					ErrCharacterNameInvalid, CharacterNameMaxSpaces)
			}
			continue
		}
		if unicode.IsSpace(value) || unicode.IsControl(value) ||
			unicode.Is(unicode.Cf, value) || !unicode.IsGraphic(value) {
			return fmt.Errorf("%w: name contains whitespace, control, or invisible data", ErrCharacterNameInvalid)
		}
		switch value {
		case '\'', '\\', '%', '<', '>', '"', '|':
			return fmt.Errorf("%w: name contains forbidden character %q", ErrCharacterNameInvalid, value)
		}
		visible++
	}
	if visible == 0 {
		return fmt.Errorf("%w: name must contain at least one visible character", ErrCharacterNameInvalid)
	}
	return nil
}
