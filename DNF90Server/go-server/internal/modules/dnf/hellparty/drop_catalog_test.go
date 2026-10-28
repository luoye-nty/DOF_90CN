package hellparty

import (
	"errors"
	"reflect"
	"testing"
)

const testDropCatalog = `
[drop prob count]
	2
[dungeon difficulty drop prob]
	1 45 100 200 300 400 500
	46 90 600 700 800 900 1000
[basis of rarity dicision]
	2
	700000 900000 980000 995000 1000000 1000000 1000000
	600000 850000 950000 990000 1000000 1000000 1000000
[item drop ref table]
	45 2 3
	46 3 4
`

func TestParseDropCatalog(t *testing.T) {
	catalog, err := ParseDropCatalog(testDropCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if probabilityRows, rarityRows, gradeRows := catalog.Snapshot(); probabilityRows != 2 || rarityRows != 2 || gradeRows != 2 {
		t.Fatalf("snapshot=%d/%d/%d", probabilityRows, rarityRows, gradeRows)
	}
	if rate, ok := catalog.DropRate(46, 4); !ok || rate != 1000 {
		t.Fatalf("rate=%d found=%t", rate, ok)
	}
	if rate, ok := catalog.DropRate(45, -1); !ok || rate != 100 {
		t.Fatalf("clamped rate=%d found=%t", rate, ok)
	}
	if window, ok := catalog.GradeWindow(45); !ok || window != (GradeWindow{Down: 2, Up: 3}) {
		t.Fatalf("grade window=%+v found=%t", window, ok)
	}
	weights, ok := catalog.RarityWeights(1)
	if !ok || !reflect.DeepEqual(weights, [7]uint32{700000, 200000, 80000, 15000, 5000, 0, 0}) {
		t.Fatalf("rarity weights=%v found=%t", weights, ok)
	}
	if rarity, ok := catalog.RarityForRoll(1, 699999); !ok || rarity != 0 {
		t.Fatalf("rarity=%d found=%t", rarity, ok)
	}
	if rarity, ok := catalog.RarityForRoll(1, 700000); !ok || rarity != 1 {
		t.Fatalf("rarity=%d found=%t", rarity, ok)
	}
}

func TestParseDropCatalogRejectsMalformedShapes(t *testing.T) {
	tests := []string{
		"[drop prob count]\n1\n[dungeon difficulty drop prob]\n1 90 1\n[basis of rarity dicision]\n1 1 2 3 4 5 6 7\n[item drop ref table]\n1 0 1\n",
		"[drop prob count]\n1\n[dungeon difficulty drop prob]\n1 90 1 2 3 4 5\n[basis of rarity dicision]\n1 2 1 3 4 5 6 7\n[item drop ref table]\n1 0 1\n",
		"[drop prob count]\n1\n[dungeon difficulty drop prob]\n1 90 1 2 3 4 5\n[basis of rarity dicision]\n1 1 2 3 4 5 6 7\n[item drop ref table]\n1 0 0\n",
	}
	for index, input := range tests {
		if _, err := ParseDropCatalog(input); !errors.Is(err, ErrDropCatalogInvalid) {
			t.Fatalf("case=%d error=%v", index, err)
		}
	}
}
