package epicbook

import (
	"errors"
	"testing"
)

func TestParseCatalog(t *testing.T) {
	catalog, err := Parse(`[equipment piece drop info]
[piece list]
101020256 10300528 101020258 10300529
[/piece list]
[/equipment piece drop info]
[equipment recipe info]
[info]
[output]
101020256
[recipe]
10300528 1000 10099775 44 10158124 6
[/recipe]
[/info]
[info]
[output]
101020258
[recipe]
10300529 1000 10099775 49 10158124 8
[/recipe]
[/info]
[/equipment recipe info]`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if catalog.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", catalog.Len())
	}
	entry, ok := catalog.EntryByEquipment(101020256)
	if !ok || entry.FragmentID != 10300528 || entry.Recipe.FragmentCount != 1000 || len(entry.Recipe.Materials) != 2 {
		t.Fatalf("EntryByEquipment() = %+v, %v", entry, ok)
	}
	if entry.Recipe.Materials[0] != (Material{ItemID: 10099775, Count: 44}) {
		t.Fatalf("first material = %+v", entry.Recipe.Materials[0])
	}
	reverse, ok := catalog.EntryByFragment(10300529)
	if !ok || reverse.EquipmentID != 101020258 {
		t.Fatalf("EntryByFragment() = %+v, %v", reverse, ok)
	}
}

func TestParseCatalogRejectsFragmentMismatch(t *testing.T) {
	_, err := Parse(`[equipment piece drop info]
[piece list]
101020256 10300528
[/piece list]
[/equipment piece drop info]
[equipment recipe info]
[info]
[output]
101020256
[recipe]
10300529 1000
[/recipe]
[/info]
[/equipment recipe info]`)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Parse() error = %v, want ErrCatalogInvalid", err)
	}
}

func TestParseCatalogRejectsDuplicateFragment(t *testing.T) {
	_, err := Parse(`[equipment piece drop info]
[piece list]
1 10 2 10
[/piece list]
[/equipment piece drop info]
[equipment recipe info]
[/equipment recipe info]`)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Parse() error = %v, want ErrCatalogInvalid", err)
	}
}
