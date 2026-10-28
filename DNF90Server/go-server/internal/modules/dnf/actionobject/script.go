// Package actionobject parses the PVF action-object scripts that a dungeon's
// passive objects run.
//
// A placed `[passive object]` resolves through passiveobject/passiveobject.lst
// to an `.obj` definition whose `[basic action]` names an `.act` script. The
// script is a flat, ordered sequence of `[TRIGGER]` / `[BEHAVIOR]` blocks: a
// trigger tests dungeon symbols and room actors and names the behavior it runs,
// a behavior mutates dungeon symbols, opens or closes room gates and posts
// messages.
//
// Only the operations the server can actually honour are decoded. Everything
// else is recorded by name on the owning block so a caller can log what it
// skipped rather than silently mis-running a script.
package actionobject

import (
	"errors"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

var ErrDocumentRequired = errors.New("dnf action object document is required")

// CompareOp is the comparison a `[CHECKUP]` or `[DUNGEON SYMBOL COMPARE]`
// applies. The script spells it as its own section, e.g. `[<]` then the operand.
type CompareOp string

const (
	CompareLess         CompareOp = "<"
	CompareLessEqual    CompareOp = "<="
	CompareEqual        CompareOp = "="
	CompareNotEqual     CompareOp = "!="
	CompareGreaterEqual CompareOp = ">="
	CompareGreater      CompareOp = ">"
)

// Satisfied reports whether left <op> right holds.
func (op CompareOp) Satisfied(left, right int64) bool {
	switch op {
	case CompareLess:
		return left < right
	case CompareLessEqual:
		return left <= right
	case CompareEqual:
		return left == right
	case CompareNotEqual:
		return left != right
	case CompareGreaterEqual:
		return left >= right
	case CompareGreater:
		return left > right
	default:
		return false
	}
}

type ConditionKind string

const (
	// ConditionDungeonSymbol is `[DUNGEON SYMBOL COMPARE] name` + `[<] n`.
	ConditionDungeonSymbol ConditionKind = "dungeon_symbol"
	// ConditionMonsterCount is `[WHICH][MONSTER][CHECKUP][IS INDEX] id
	// [/IS INDEX][CHECKED NO][<] n [/CHECKUP]`: how many live monsters of that
	// index the room still holds.
	ConditionMonsterCount ConditionKind = "monster_count"
)

type Condition struct {
	Kind      ConditionKind
	Symbol    string
	MonsterID int64
	Op        CompareOp
	Value     int64
}

type OperationKind string

const (
	OperationSymbolCalc    OperationKind = "dungeon_symbol_calc"
	OperationSymbolSet     OperationKind = "dungeon_symbol_set"
	OperationGateEnable    OperationKind = "gate_enable"
	OperationNotice        OperationKind = "notice"
	OperationCenterMessage OperationKind = "center_msg"
	OperationTriggerSwitch OperationKind = "trigger_switch"
	OperationDestroy       OperationKind = "destroy"
)

type Operation struct {
	Kind   OperationKind
	Symbol string
	// Calc is `+` or `-` for OperationSymbolCalc.
	Calc  string
	Value int64
	// GateEnabled and GateDirections carry `[GATE ENABLE] x y enable` plus the
	// `[DOWN]` / `[LEFT]` / `[RIGHT]` / `[UP]` sections inside its block.
	GateEnabled    bool
	GateDirections []string
	Text           string
	// Color is the ARGB the script's `r g b` triple builds, opaque.
	Color        uint32
	DurationMS   int64
	TriggerIndex int
	TriggerOn    bool
}

type Trigger struct {
	Index int
	// Limit is how many times the trigger may fire; 0 means unlimited.
	Limit int
	// Enabled is the trigger's state before any behavior switches it. A script
	// writes `[ENABLE][OFF]` for a trigger another behavior turns on later.
	Enabled     bool
	Conditions  []Condition
	Behavior    int
	Unsupported []string
}

type Behavior struct {
	Index       int
	Operations  []Operation
	Unsupported []string
}

type Script struct {
	Path      string
	Triggers  []Trigger
	Behaviors []Behavior
}

// Drives reports whether the script touches anything the dungeon runtime owns.
// A script that only plays animations is not worth instantiating.
func (s Script) Drives() bool {
	for _, behavior := range s.Behaviors {
		for _, operation := range behavior.Operations {
			switch operation.Kind {
			case OperationSymbolCalc, OperationSymbolSet, OperationGateEnable,
				OperationNotice, OperationCenterMessage:
				return true
			}
		}
	}
	return false
}

func compareOpFor(name string) (CompareOp, bool) {
	switch CompareOp(name) {
	case CompareLess, CompareLessEqual, CompareEqual, CompareNotEqual, CompareGreaterEqual, CompareGreater:
		return CompareOp(name), true
	case "==":
		return CompareEqual, true
	case "<>":
		return CompareNotEqual, true
	default:
		return "", false
	}
}

func gateDirectionFor(name string) (string, bool) {
	switch name {
	case "up", "down", "left", "right":
		return name, true
	default:
		return "", false
	}
}

// parseState is the cursor the section walk carries. `[TRIGGER]` is overloaded:
// at the top level it opens a trigger block, inside a `[BEHAVIOR]` it is the
// "switch trigger n on/off" operation, told apart by the integer operand.
type parseState struct {
	script          Script
	trigger         *Trigger
	behavior        *Behavior
	pendingCompare  *Condition
	pendingMonster  int64
	monsterSelected bool
	checkupOpen     bool
	checkedNo       bool
	enablePending   bool
	gateOpen        bool
	doBehavior      bool
	triggerSwitch   bool
	// pendingCalcIndex is the behavior operation waiting for its `[+] 1`
	// operator section; -1 when none is open.
	pendingCalcIndex int
}

// Parse decodes one `.act` document.
func Parse(document *dnfpvf.Document) (Script, error) {
	if document == nil {
		return Script{}, ErrDocumentRequired
	}
	state := parseState{script: Script{Path: document.Path}, pendingCalcIndex: -1}
	for _, section := range document.Sections {
		name := strings.ToLower(strings.TrimSpace(section.Name))
		tokens := sectionTokens(document, section)
		state.consume(name, tokens)
	}
	state.closeTrigger()
	state.closeBehavior()
	return state.script, nil
}

func sectionTokens(document *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
		return nil
	}
	return document.Tokens[section.Start:section.End]
}

func (p *parseState) consume(name string, tokens []dnfpvf.Token) {
	switch name {
	case "motion", "/motion", "base ani":
		return
	case "trigger":
		// Inside a behavior this is `[TRIGGER] n` + `[ON]`/`[OFF]`.
		if p.behavior != nil {
			p.triggerSwitch = true
			p.behavior.Operations = append(p.behavior.Operations, Operation{
				Kind:         OperationTriggerSwitch,
				TriggerIndex: int(firstInt(tokens)),
				TriggerOn:    true,
			})
			return
		}
		p.closeTrigger()
		p.trigger = &Trigger{Index: len(p.script.Triggers), Enabled: true, Behavior: -1}
	case "/trigger":
		p.closeTrigger()
	case "behavior":
		p.closeBehavior()
		p.behavior = &Behavior{Index: len(p.script.Behaviors)}
	case "/behavior":
		p.closeBehavior()
	case "limit":
		if p.trigger != nil {
			p.trigger.Limit = int(firstInt(tokens))
		}
	case "enable":
		p.enablePending = true
	case "on", "off":
		p.applySwitch(name == "on")
	case "which":
		p.monsterSelected = false
	case "monster":
		if p.trigger != nil {
			p.monsterSelected = true
		}
	case "checkup":
		p.checkupOpen = true
		p.checkedNo = false
		p.pendingMonster = 0
	case "/checkup":
		p.checkupOpen = false
		p.checkedNo = false
		p.monsterSelected = false
		p.pendingMonster = 0
	case "is index":
		p.pendingMonster = firstInt(tokens)
	case "/is index":
	case "checked no":
		p.checkedNo = true
	case "dungeon symbol compare":
		p.pendingCompare = &Condition{Kind: ConditionDungeonSymbol, Symbol: firstText(tokens)}
	case "dungeon symbol calc":
		p.appendSymbolCalc(tokens)
	case "dungeon symbol set":
		if p.behavior != nil {
			p.behavior.Operations = append(p.behavior.Operations, Operation{
				Kind:   OperationSymbolSet,
				Symbol: firstText(tokens),
				Value:  firstInt(tokens),
			})
		}
	case "gate enable":
		p.openGate(tokens)
	case "/gate enable":
		p.gateOpen = false
	case "notice":
		p.appendMessage(OperationNotice, tokens)
	case "/notice":
	case "center msg":
		p.appendMessage(OperationCenterMessage, tokens)
	case "destroy":
		if p.behavior != nil {
			p.behavior.Operations = append(p.behavior.Operations, Operation{Kind: OperationDestroy})
		}
	case "do behavior":
		p.doBehavior = true
	case "me":
		p.applyMe(tokens)
	case "frame":
		// A frame gate is animation timing, not a gameplay condition: the
		// server has no animation clock, so the trigger is treated as always
		// reachable and its other conditions decide.
	default:
		p.consumeUnknown(name, tokens)
	}
}

func (p *parseState) consumeUnknown(name string, tokens []dnfpvf.Token) {
	// `[DUNGEON SYMBOL CALC] name` is finished by its operator section, e.g.
	// `[+]` then 1. `[=]` is also a comparison, so the open calc wins while one
	// is pending and the trigger's comparison table is consulted otherwise.
	if p.pendingCalcIndex >= 0 && p.behavior != nil &&
		(name == "+" || name == "-" || name == "=") {
		operation := &p.behavior.Operations[p.pendingCalcIndex]
		if name == "=" {
			operation.Kind = OperationSymbolSet
		} else {
			operation.Calc = name
		}
		operation.Value = firstInt(tokens)
		p.pendingCalcIndex = -1
		return
	}
	if op, ok := compareOpFor(name); ok {
		p.applyCompare(op, firstInt(tokens))
		return
	}
	if direction, ok := gateDirectionFor(name); ok && p.gateOpen && p.behavior != nil {
		last := len(p.behavior.Operations) - 1
		if last >= 0 && p.behavior.Operations[last].Kind == OperationGateEnable {
			p.behavior.Operations[last].GateDirections = append(
				p.behavior.Operations[last].GateDirections, direction)
		}
		return
	}
	switch {
	case p.behavior != nil:
		p.behavior.Unsupported = appendUnique(p.behavior.Unsupported, name)
	case p.trigger != nil:
		p.trigger.Unsupported = appendUnique(p.trigger.Unsupported, name)
	}
}

// applyCompare closes whichever comparison was left open: a dungeon symbol
// compare, or the live-monster count of a `[CHECKUP]`.
func (p *parseState) applyCompare(op CompareOp, value int64) {
	switch {
	case p.pendingCompare != nil:
		condition := *p.pendingCompare
		condition.Op = op
		condition.Value = value
		p.pendingCompare = nil
		if p.trigger != nil {
			p.trigger.Conditions = append(p.trigger.Conditions, condition)
		}
	case p.checkupOpen && p.checkedNo && p.monsterSelected && p.trigger != nil:
		p.trigger.Conditions = append(p.trigger.Conditions, Condition{
			Kind:      ConditionMonsterCount,
			MonsterID: p.pendingMonster,
			Op:        op,
			Value:     value,
		})
		p.checkedNo = false
	case p.trigger != nil:
		p.trigger.Unsupported = appendUnique(p.trigger.Unsupported, string(op))
	}
}

func (p *parseState) applySwitch(on bool) {
	switch {
	case p.triggerSwitch && p.behavior != nil:
		last := len(p.behavior.Operations) - 1
		if last >= 0 && p.behavior.Operations[last].Kind == OperationTriggerSwitch {
			p.behavior.Operations[last].TriggerOn = on
		}
		p.triggerSwitch = false
	case p.enablePending && p.trigger != nil:
		p.trigger.Enabled = on
		p.enablePending = false
	}
}

func (p *parseState) applyMe(tokens []dnfpvf.Token) {
	if p.doBehavior && p.trigger != nil {
		p.trigger.Behavior = int(firstInt(tokens))
	}
	p.doBehavior = false
}

func (p *parseState) appendSymbolCalc(tokens []dnfpvf.Token) {
	if p.behavior == nil {
		return
	}
	p.behavior.Operations = append(p.behavior.Operations, Operation{
		Kind:   OperationSymbolCalc,
		Symbol: firstText(tokens),
		Calc:   "+",
	})
	// The operator and operand arrive as their own sections, e.g. `[+]` then 1.
	p.pendingCalcIndex = len(p.behavior.Operations) - 1
	p.pendingCompare = nil
}

func (p *parseState) openGate(tokens []dnfpvf.Token) {
	if p.behavior == nil {
		return
	}
	// `[GATE ENABLE] x y enable`: the coordinate pair is -1 -1 for "this room"
	// in every shipped script, and only the third field decides open/closed.
	values := allInts(tokens)
	enabled := false
	if len(values) > 0 && values[len(values)-1] != 0 {
		enabled = true
	}
	p.gateOpen = true
	p.behavior.Operations = append(p.behavior.Operations, Operation{
		Kind:        OperationGateEnable,
		GateEnabled: enabled,
	})
}

func (p *parseState) appendMessage(kind OperationKind, tokens []dnfpvf.Token) {
	if p.behavior == nil {
		return
	}
	operation := Operation{Kind: kind, Text: firstText(tokens), Color: 0xFFFFFFFF}
	values := allInts(tokens)
	switch kind {
	case OperationNotice:
		// `[NOTICE] text r g b sound fade duration`.
		if len(values) >= 3 {
			operation.Color = 0xFF000000 |
				uint32(clampByte(values[0]))<<16 |
				uint32(clampByte(values[1]))<<8 |
				uint32(clampByte(values[2]))
		}
		if len(values) >= 5 {
			operation.DurationMS = values[3]
		}
	case OperationCenterMessage:
		// `[CENTER MSG] text duration`.
		if len(values) >= 1 {
			operation.DurationMS = values[0]
		}
	}
	p.behavior.Operations = append(p.behavior.Operations, operation)
}

func (p *parseState) closeTrigger() {
	if p.trigger == nil {
		return
	}
	p.script.Triggers = append(p.script.Triggers, *p.trigger)
	p.trigger = nil
	p.pendingCompare = nil
	p.monsterSelected = false
	p.checkupOpen = false
	p.checkedNo = false
	p.enablePending = false
	p.doBehavior = false
}

func (p *parseState) closeBehavior() {
	if p.behavior == nil {
		return
	}
	p.script.Behaviors = append(p.script.Behaviors, *p.behavior)
	p.behavior = nil
	p.gateOpen = false
	p.triggerSwitch = false
	p.pendingCalcIndex = -1
}

func firstInt(tokens []dnfpvf.Token) int64 {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int
		}
	}
	return 0
}

func allInts(tokens []dnfpvf.Token) []int64 {
	values := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	return values
}

func firstText(tokens []dnfpvf.Token) string {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenString {
			return token.Value
		}
	}
	return ""
}

func clampByte(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return value
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
