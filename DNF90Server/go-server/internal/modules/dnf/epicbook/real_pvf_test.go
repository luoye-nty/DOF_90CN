package epicbook

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFCatalog(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to verify the runtime epic book catalog")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", path, err)
	}
	text, err := archive.ReadText(PieceInfoPath)
	if err != nil {
		t.Fatalf("ReadText(%q) error = %v", PieceInfoPath, err)
	}
	catalog, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse(real PVF) error = %v", err)
	}
	if catalog.Len() < 100 {
		t.Fatalf("real catalog entries = %d, want at least 100", catalog.Len())
	}
	entry, ok := catalog.EntryByEquipment(101020256)
	if !ok || entry.FragmentID != 10300528 || entry.Recipe.FragmentCount != 1000 {
		t.Fatalf("known entry = %+v, %v", entry, ok)
	}
}
