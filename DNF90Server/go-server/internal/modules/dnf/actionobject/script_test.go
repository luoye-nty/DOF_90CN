package actionobject

import (
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func parseTestScript(t *testing.T, path, text string) Script {
	t.Helper()
	document, err := dnfpvf.Parse(path, text)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	script, err := Parse(document)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return script
}

// The gate object of 能源储藏室: it closes its own boss door while
// `storage gate open` is short of 2 and reopens it once both cylinders have
// counted in. Verbatim from
// passiveobject/actionobject/map/luke_raid/phase2/enegy_save/action/gate_control_gate.act.
const lukeEnergySaveGateScript = "" +
	"[MOTION]\n[BASE ANI]\n`../`\n[/MOTION]\n" +
	"[TRIGGER]\n[LIMIT]\n1\n[ENABLE]\n[ON]\n" +
	"[DUNGEON SYMBOL COMPARE]\n`storage gate open`\n[<]\n2\n" +
	"[DO BEHAVIOR]\n[ME]\n0\n[/TRIGGER]\n" +
	"[BEHAVIOR]\n" +
	"[NOTICE]\n`gate closed` 255 255 0 `None` 4000 1000\n[/NOTICE]\n" +
	"[GATE ENABLE]\n-1 -1 0\n[DOWN]\n[/GATE ENABLE]\n" +
	"[TRIGGER]\n1\n[ON]\n[TRIGGER]\n0\n[OFF]\n[/BEHAVIOR]\n" +
	"[TRIGGER]\n[LIMIT]\n1\n[ENABLE]\n[OFF]\n" +
	"[DUNGEON SYMBOL COMPARE]\n`storage gate open`\n[>=]\n2\n" +
	"[DO BEHAVIOR]\n[ME]\n1\n[/TRIGGER]\n" +
	"[BEHAVIOR]\n" +
	"[NOTICE]\n`gate open` 255 255 0 `None` 4000 1000\n[/NOTICE]\n" +
	"[GATE ENABLE]\n-1 -1 1\n[DOWN]\n[/GATE ENABLE]\n[/BEHAVIOR]\n"

// The left cylinder: it adds to the symbol once its room's monster is gone.
const lukeEnergySaveCylinderScript = "" +
	"[MOTION]\n[BASE ANI]\n`../`\n[/MOTION]\n" +
	"[TRIGGER]\n[LIMIT]\n1\n[WHICH]\n[MONSTER]\n[CHECKUP]\n" +
	"[IS INDEX]\n57156\n[/IS INDEX]\n[CHECKED NO]\n[<]\n1\n[/CHECKUP]\n" +
	"[DO BEHAVIOR]\n[ME]\n0\n[/TRIGGER]\n" +
	"[BEHAVIOR]\n[DUNGEON SYMBOL CALC]\n`storage gate open`\n[+]\n1\n[/BEHAVIOR]\n"

func TestParseGateScriptDecodesBothBranches(t *testing.T) {
	script := parseTestScript(t, "gate_control_gate.act", lukeEnergySaveGateScript)
	if len(script.Triggers) != 2 || len(script.Behaviors) != 2 {
		t.Fatalf("triggers=%d behaviors=%d want 2/2 (%+v)", len(script.Triggers), len(script.Behaviors), script)
	}
	if !script.Drives() {
		t.Fatal("gate script does not drive anything")
	}

	closing := script.Triggers[0]
	if closing.Limit != 1 || !closing.Enabled || closing.Behavior != 0 {
		t.Fatalf("closing trigger=%+v", closing)
	}
	if len(closing.Conditions) != 1 ||
		closing.Conditions[0] != (Condition{
			Kind: ConditionDungeonSymbol, Symbol: "storage gate open", Op: CompareLess, Value: 2,
		}) {
		t.Fatalf("closing conditions=%+v", closing.Conditions)
	}
	if len(closing.Unsupported) != 0 {
		t.Fatalf("closing trigger dropped %v", closing.Unsupported)
	}

	// The reopening trigger ships disabled and is switched on by behavior 0.
	opening := script.Triggers[1]
	if opening.Enabled || opening.Behavior != 1 {
		t.Fatalf("opening trigger=%+v", opening)
	}
	if len(opening.Conditions) != 1 || opening.Conditions[0].Op != CompareGreaterEqual {
		t.Fatalf("opening conditions=%+v", opening.Conditions)
	}

	closeBehavior := script.Behaviors[0]
	if len(closeBehavior.Operations) != 4 {
		t.Fatalf("close behavior operations=%+v", closeBehavior.Operations)
	}
	if got := closeBehavior.Operations[0]; got.Kind != OperationNotice ||
		got.Text != "gate closed" || got.Color != 0xFFFFFF00 || got.DurationMS != 4000 {
		t.Fatalf("notice=%+v", got)
	}
	if got := closeBehavior.Operations[1]; got.Kind != OperationGateEnable || got.GateEnabled ||
		len(got.GateDirections) != 1 || got.GateDirections[0] != "down" {
		t.Fatalf("gate close=%+v", got)
	}
	if got := closeBehavior.Operations[2]; got.Kind != OperationTriggerSwitch ||
		got.TriggerIndex != 1 || !got.TriggerOn {
		t.Fatalf("trigger switch on=%+v", got)
	}
	if got := closeBehavior.Operations[3]; got.Kind != OperationTriggerSwitch ||
		got.TriggerIndex != 0 || got.TriggerOn {
		t.Fatalf("trigger switch off=%+v", got)
	}

	openBehavior := script.Behaviors[1]
	if len(openBehavior.Operations) != 2 {
		t.Fatalf("open behavior operations=%+v", openBehavior.Operations)
	}
	if got := openBehavior.Operations[1]; got.Kind != OperationGateEnable || !got.GateEnabled {
		t.Fatalf("gate open=%+v", got)
	}
}

func TestParseCylinderScriptCountsTheRoomMonster(t *testing.T) {
	script := parseTestScript(t, "gate_control_left.act", lukeEnergySaveCylinderScript)
	if len(script.Triggers) != 1 || len(script.Behaviors) != 1 {
		t.Fatalf("triggers=%d behaviors=%d want 1/1", len(script.Triggers), len(script.Behaviors))
	}
	trigger := script.Triggers[0]
	if len(trigger.Conditions) != 1 ||
		trigger.Conditions[0] != (Condition{
			Kind: ConditionMonsterCount, MonsterID: 57156, Op: CompareLess, Value: 1,
		}) {
		t.Fatalf("conditions=%+v unsupported=%v", trigger.Conditions, trigger.Unsupported)
	}
	operations := script.Behaviors[0].Operations
	if len(operations) != 1 ||
		operations[0].Kind != OperationSymbolCalc ||
		operations[0].Symbol != "storage gate open" ||
		operations[0].Calc != "+" || operations[0].Value != 1 {
		t.Fatalf("operations=%+v", operations)
	}
}

func TestParseAnimationOnlyScriptDrivesNothing(t *testing.T) {
	script := parseTestScript(t, "idle.act", "[MOTION]\n[BASE ANI]\n`../a.ani`\n[/MOTION]\n")
	if script.Drives() {
		t.Fatalf("animation-only script reported as driving: %+v", script)
	}
}
