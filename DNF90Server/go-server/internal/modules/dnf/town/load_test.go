package town

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type memorySource map[string]string

func (s memorySource) ReadText(name string) (string, error) {
	value, ok := s[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	return value, nil
}

func TestLoadTownAreasAndPermissions(t *testing.T) {
	townName := "\u827e\u5c14\u6587\u9632\u7ebf"
	source := memorySource{
		"town/town.lst": "38 `new_Elvengard.twn`",
		"town/new_Elvengard.twn": strings.Join([]string{
			"[area]",
			"0 `Cataclysm/Town/Elvengard/new_Elvengard.map`",
			"[permission]",
			"[need level]",
			"1",
			"[/permission]",
			"`[normal]`",
			"[/area]",
			"[area]",
			"1 `Cataclysm/Town/Elvengard/new_seria_room.map`",
			"[permission]",
			"[need level]",
			"1",
			"[/permission]",
			"`[gate]` 450 234",
			"[/area]",
			"[area]",
			"3 `Cataclysm/Town/Elvengard/Elvengard_hendon.map`",
			"[permission]",
			"[need level]",
			"12",
			"[need quest]",
			"3155",
			"[/need quest]",
			"[/permission]",
			"`[dungeon gate]` 2",
			"[/area]",
			"[name]",
			"`" + townName + "`",
		}, "\n"),
	}
	table, err := Load(context.Background(), source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := table.Snapshot(); snapshot.Towns != 1 || snapshot.Areas != 3 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	town, ok := table.Find(38)
	if !ok || town.Name != townName || len(town.Areas) != 3 {
		t.Fatalf("town=(%+v,%t)", town, ok)
	}
	area, ok := table.FindArea(38, 1)
	if !ok || area.MapPath != "Cataclysm/Town/Elvengard/new_seria_room.map" ||
		area.Kind != "gate" || area.Gate == nil || area.Gate.X != 450 || area.Gate.Y != 234 || area.MinLevel != 1 {
		t.Fatalf("seria area=(%+v,%t)", area, ok)
	}
	gateArea, ok := table.FindGateArea(38)
	if !ok || gateArea.ID != 1 || gateArea.MapPath != "Cataclysm/Town/Elvengard/new_seria_room.map" ||
		gateArea.Gate == nil || gateArea.Gate.X != 450 || gateArea.Gate.Y != 234 {
		t.Fatalf("gate area=(%+v,%t)", gateArea, ok)
	}
	gateArea.Gate.X = 999
	gateAgain, ok := table.FindGateArea(38)
	if !ok || gateAgain.Gate == nil || gateAgain.Gate.X != 450 {
		t.Fatalf("gate area clone leaked mutation=(%+v,%t)", gateAgain, ok)
	}
	area, ok = table.FindArea(38, 3)
	if !ok || area.MinLevel != 12 || len(area.NeedQuests) != 1 || area.NeedQuests[0] != 3155 ||
		area.DungeonGate == nil || *area.DungeonGate != 2 {
		t.Fatalf("quest area=(%+v,%t)", area, ok)
	}
}

func TestLoadBuildsPVFProvenNPCEnterTownRoute(t *testing.T) {
	source := memorySource{
		"town/town.lst": "107 `event.twn`",
		"town/event.twn": strings.Join([]string{
			"[area]",
			"0 `Town/Event/event.map` `[dungeon gate]` 114",
			"[/area]",
		}, "\n"),
		"npc/npc.lst":              "20275 `entry.npc` 20276 `exit.npc` 20277 `unrelated.npc`",
		"npc/entry.npc":            "[role]\n`[enter town]` 20276\n[/role]",
		"npc/exit.npc":             "[role]\n`[exit town]` 0\n[/role]",
		"npc/unrelated.npc":        "[role]\n`[exit town]` 0\n[/role]",
		"map/Town/Event/event.map": "[NPC]\n20276 `[left]` 100 200 0 20277 `[left]` 300 400 0\n[/NPC]",
	}
	table, err := Load(context.Background(), source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !table.AllowsNPCEnterTown(107, 0, 20276) {
		t.Fatal("PVF [enter town] target with target-map [exit town] NPC was not indexed")
	}
	if table.AllowsNPCEnterTown(107, 0, 20277) || table.AllowsNPCEnterTown(107, 0, 99999) ||
		table.AllowsNPCEnterTown(107, 1, 20276) {
		t.Fatal("unproved NPC enter-town target was accepted")
	}
	area, ok := table.FindArea(107, 0)
	if !ok || len(area.NPCs) != 2 || area.NPCs[0].NPCID != 20276 || area.NPCs[0].X != 100 || area.NPCs[0].Y != 200 ||
		area.NPCs[1].NPCID != 20277 || area.NPCs[1].X != 300 || area.NPCs[1].Y != 400 {
		t.Fatalf("town NPC spawns=%+v found=%t", area.NPCs, ok)
	}
	area.NPCs[0].X = 999
	again, _ := table.FindArea(107, 0)
	if again.NPCs[0].X != 100 {
		t.Fatalf("town NPC clone leaked mutation: %+v", again.NPCs)
	}
}
