package raid

import (
	"fmt"
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 临时探针：把 PVF 里与 BOSS 机制有关的规则原样打出来，看 **boss pattern init**
// 到底挂在什么条件下、以及是否存在"无条件规则"（空条件会在每一个事件上重放）。
func TestZZProbeFiendWarBossRules(t *testing.T) {
	path := os.Getenv("DNF_FIENDWAR_REAL_PVF")
	if path == "" {
		t.Skip("DNF_FIENDWAR_REAL_PVF not set")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadFiendWarConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.Battle == nil {
		t.Fatal("no battle config")
	}
	show := func(i FiendWarInstruction) string {
		args := make([]string, 0, len(i.Args))
		for _, a := range i.Args {
			args = append(args, fmt.Sprintf("%d/%q", a.Int, a.Value))
		}
		return fmt.Sprintf("op=%s sym=%d args=%v", i.Op, i.Symbol, args)
	}
	for index, rule := range c.Battle.Rules {
		interest := false
		for _, a := range rule.Actions {
			if a.Op == "boss pattern init" || a.Op == "clear phase" || a.Op == "fail fiendwar" {
				interest = true
			}
		}
		for _, cond := range rule.Conditions {
			if cond.Op == "on enter map" {
				interest = true
			}
		}
		if len(rule.Conditions) == 0 {
			interest = true
			t.Logf("rule %d: **** UNCONDITIONAL (conditions empty) ****", index)
		}
		if !interest {
			continue
		}
		t.Logf("rule %d: %d conditions", index, len(rule.Conditions))
		for _, cond := range rule.Conditions {
			t.Logf("    IF   %s", show(cond))
		}
		for _, a := range rule.Actions {
			t.Logf("    THEN %s", show(a))
		}
	}
	t.Logf("total rules=%d", len(c.Battle.Rules))
	// BOSS 区域的时限：Enter 会把 area.Deadline = now + LimitSeconds，超时 Tick 会
	// **Abandon**（而 Abandon 对 boss 区域会 BossPatternDone 清零、pattern 归零）。
	for id, area := range c.Battle.Areas {
		if area.Kind == "top boss" || area.Kind == "bot boss" {
			t.Logf("area %d: kind=%q dungeon=%d limit_seconds=%d member_max=%d",
				id, area.Kind, area.Dungeon, area.LimitSeconds, area.MemberMax)
		}
	}
	for name, id := range c.Battle.Symbols {
		if name == "top boss state" || name == "bot boss state" || name == "boss pattern state" ||
			name == "top boss hp" || name == "bot boss hp" {
			t.Logf("symbol %q = %d", name, id)
		}
	}
}
