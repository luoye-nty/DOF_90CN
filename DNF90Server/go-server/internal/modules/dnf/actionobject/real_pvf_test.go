package actionobject

import (
	"os"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestRealScriptPVFEnergySaveGateScripts decodes the shipped 能源储藏室 scripts
// rather than the hand-written fixtures above, so a grammar the fixtures do not
// reproduce cannot pass unnoticed.
func TestRealScriptPVFEnergySaveGateScripts(t *testing.T) {
	pvfPath := os.Getenv("DNF_WORLDMAP_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNF_WORLDMAP_REAL_PVF_SMOKE to decode the shipped action-object scripts")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	const base = "passiveobject/actionobject/map/luke_raid/phase2/enegy_save/action/"
	read := func(name string) Script {
		t.Helper()
		text, readErr := archive.ReadText(base + name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		document, parseErr := dnfpvf.Parse(base+name, text)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		script, decodeErr := Parse(document)
		if decodeErr != nil {
			t.Fatalf("decode %s: %v", name, decodeErr)
		}
		return script
	}

	gate := read("gate_control_gate.act")
	if len(gate.Triggers) != 2 || len(gate.Behaviors) != 2 {
		t.Fatalf("gate script=%+v", gate)
	}
	for index, want := range []Condition{
		{Kind: ConditionDungeonSymbol, Symbol: "storage gate open", Op: CompareLess, Value: 2},
		{Kind: ConditionDungeonSymbol, Symbol: "storage gate open", Op: CompareGreaterEqual, Value: 2},
	} {
		conditions := gate.Triggers[index].Conditions
		if len(conditions) != 1 || conditions[0] != want {
			t.Fatalf("gate trigger %d conditions=%+v want %+v (unsupported=%v)",
				index, conditions, want, gate.Triggers[index].Unsupported)
		}
	}
	if gate.Triggers[0].Enabled == gate.Triggers[1].Enabled {
		t.Fatalf("gate triggers must ship in opposite states: %+v", gate.Triggers)
	}
	opened := false
	for _, operation := range gate.Behaviors[1].Operations {
		if operation.Kind == OperationGateEnable && operation.GateEnabled &&
			len(operation.GateDirections) == 1 && operation.GateDirections[0] == "down" {
			opened = true
		}
	}
	if !opened {
		t.Fatalf("gate behavior 1 never opens the down gate: %+v", gate.Behaviors[1])
	}

	for name, monsterID := range map[string]int64{
		"gate_control_left.act":  57156,
		"gate_control_right.act": 56068,
	} {
		cylinder := read(name)
		if len(cylinder.Triggers) != 1 || len(cylinder.Behaviors) != 1 {
			t.Fatalf("%s=%+v", name, cylinder)
		}
		conditions := cylinder.Triggers[0].Conditions
		want := Condition{Kind: ConditionMonsterCount, MonsterID: monsterID, Op: CompareLess, Value: 1}
		if len(conditions) != 1 || conditions[0] != want {
			t.Fatalf("%s conditions=%+v want %+v (unsupported=%v)",
				name, conditions, want, cylinder.Triggers[0].Unsupported)
		}
		operations := cylinder.Behaviors[0].Operations
		if len(operations) != 1 || operations[0].Kind != OperationSymbolCalc ||
			operations[0].Symbol != "storage gate open" ||
			operations[0].Calc != "+" || operations[0].Value != 1 {
			t.Fatalf("%s operations=%+v", name, operations)
		}
	}
}
