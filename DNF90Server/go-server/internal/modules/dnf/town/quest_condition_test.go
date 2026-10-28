package town

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type questConditionTestSource map[string]string

func (source questConditionTestSource) ReadText(name string) (string, error) {
	value, ok := source[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}

// Town maps declare one [quest condition] block per gating quest. The loader
// must gather every block, in order, without duplicates, and an area whose map
// declares none must carry nil rather than an empty slice.
func TestLoadCollectsRepeatedQuestConditionBlocksPerArea(t *testing.T) {
	table, err := Load(context.Background(), questConditionTestSource{
		"town/town.lst":   "39 `hendon.twn` 40 `plain.twn`",
		"town/hendon.twn": "[area]\n0 `hendon_main.map` `[normal]`\n[/area]\n[name]\n`Hendon`",
		"town/plain.twn":  "[area]\n0 `plain.map` `[normal]`\n[/area]\n[name]\n`Plain`",
		"map/hendon_main.map": "[quest condition]\n3186\n[/quest condition]\n" +
			"[quest condition]\n10100\n[/quest condition]\n" +
			"[quest condition]\n10100\n[/quest condition]\n",
		"map/plain.map": "[name]\n`plain`\n",
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	area, ok := table.FindArea(39, 0)
	if !ok || !reflect.DeepEqual(area.QuestConditions, []int64{3186, 10100}) {
		t.Fatalf("hendon area=(%+v,%t), want quest conditions [3186 10100]", area, ok)
	}
	plain, ok := table.FindArea(40, 0)
	if !ok || plain.QuestConditions != nil {
		t.Fatalf("plain area=(%+v,%t), want no quest conditions", plain, ok)
	}
	// A caller mutating the returned slice must not reach into the table.
	area.QuestConditions[0] = 999
	again, _ := table.FindArea(39, 0)
	if !reflect.DeepEqual(again.QuestConditions, []int64{3186, 10100}) {
		t.Fatalf("table quest conditions were mutated through a lookup result: %v", again.QuestConditions)
	}
}
