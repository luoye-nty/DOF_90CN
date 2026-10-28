package characterdata

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateCharacterNameAcceptsCurrentClientNames(t *testing.T) {
	for _, name := range []string{
		"Test",
		"南下就2s",
		"阿拉德勇士",
		strings.Repeat("a", CharacterNameMaxBytes),
		// 2026-10-17 DIY 名字：装饰符号本来就放行，空格最多 3 个（半角/全角各算一个）。
		"①②③",
		"星★空☆间",
		"Test Name",
		"A B C D",
		"姓　名　吧",
	} {
		if err := ValidateCharacterName(name); err != nil {
			t.Fatalf("ValidateCharacterName(%q) error=%v", name, err)
		}
	}
}

func TestValidateCharacterNameRejectsUnsafeOrOutOfRangeNames(t *testing.T) {
	for _, name := range []string{
		"",
		"a",
		strings.Repeat("a", CharacterNameMaxBytes+1),
		"A    B",           // 4 个空格：超过上限
		"A　　　　B",         // 全角空格同样计数，4 个即超限
		"   ",              // 纯空格名
		"Name\u00a0Here",   // NBSP 不是被放行的两种空格
		"Name\tHere",       // 制表符
		"Name\nHere",       // 换行
		"Test\u200bName",
		"Test%Name",
		"Test\\Name",
		"Test<Name",
	} {
		if err := ValidateCharacterName(name); !errors.Is(err, ErrCharacterNameInvalid) {
			t.Fatalf("ValidateCharacterName(%q) error=%v, want ErrCharacterNameInvalid", name, err)
		}
	}
}
