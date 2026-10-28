package dnfbridge

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAvatarCompoundBridgeDelegatesMutationToDomainOwner(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(currentFile)
	files := []string{
		"avatar_compound_owner.go",
		"avatar_compound_protocol.go",
		"avatar_compound_pvf.go",
		"gameplay_module_avatar_compound.go",
	}
	for _, name := range files {
		source, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		for _, banned := range []string{"WithinCharacter", "WithinAccount", "SaveInventoryFields(", ".Save("} {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains direct repository mutation %q", name, banned)
			}
		}
	}
	ownerSource, err := os.ReadFile(filepath.Join(root, "avatar_compound_owner.go"))
	if err != nil {
		t.Fatal(err)
	}
	ownerText := string(ownerSource)
	for _, delegation := range []string{"internal/modules/dnf/avatarcompound", "owner.Compound"} {
		if !strings.Contains(ownerText, delegation) {
			t.Errorf("avatar_compound_owner.go does not delegate through %s", delegation)
		}
	}
}
