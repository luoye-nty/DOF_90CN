package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type randomTitleEnchantMemorySource map[string]string

func (s randomTitleEnchantMemorySource) ReadText(path string) (string, error) {
	text, ok := s[path]
	if !ok {
		return "", os.ErrNotExist
	}
	return text, nil
}

func TestParseCurrentRandomTitleEnchantCatalog(t *testing.T) {
	source := randomTitleEnchantMemorySource{
		currentRandomTitleEnchantPath: "[index]\n10001 1 1 20 `[swordman]` 4 1 2 2 30 `[swordman]` 6 1 `[swordman]` 9 2 3 1 10 `[swordman]` 10 1 4 1 0 `[swordman]` 11 1\n[/index]\n[index]\n10001 1 1 99 `[swordman]` 88 1\n[/index]",
	}
	catalog, err := parseCurrentRandomTitleEnchantCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	config := catalog.Configs[10001]
	if len(config.Rows) != 4 || config.Rows[0].ID != 1 || config.Rows[1].ID != 2 || config.Rows[2].ID != 3 || config.Rows[3].ID != 4 {
		t.Fatalf("config rows = %+v", config.Rows)
	}
	if config.Rows[1].Weight != 30 || len(config.Rows[1].Options) != 2 {
		t.Fatalf("two-option row = %+v", config.Rows[1])
	}
	if config.Rows[3].Weight != 0 {
		t.Fatalf("zero-weight row = %+v", config.Rows[3])
	}
	if got := chooseCurrentRandomTitleEnchantRow(config.Rows, 0); got.ID != 1 {
		t.Fatalf("roll 0 selected row %d", got.ID)
	}
	if got := chooseCurrentRandomTitleEnchantRow(config.Rows, 20); got.ID != 2 {
		t.Fatalf("roll 20 selected row %d", got.ID)
	}
}

func TestParseCurrentRandomTitleEnchantCatalogRealPVF(t *testing.T) {
	const path = `F:\DNF\90ORIGON\PVF\Script-服务�?pvf`
	if _, err := os.Stat(path); err != nil {
		t.Skipf("real server PVF unavailable: %v", err)
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseCurrentRandomTitleEnchantCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	for _, configID := range []uint16{10001, 10002, 10015} {
		config, ok := catalog.Configs[configID]
		if !ok || len(config.Rows) == 0 {
			t.Fatalf("real config %d missing or empty", configID)
		}
		if _, err := catalog.choose(configID); err != nil {
			t.Fatalf("real config %d choose: %v", configID, err)
		}
	}
}
