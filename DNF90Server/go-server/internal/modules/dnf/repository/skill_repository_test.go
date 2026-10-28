package repository

import (
	"reflect"
	"testing"
)

func TestSkillTreeHelpersKeepPlansIsolated(t *testing.T) {
	record := SkillRecord{
		Skills:  map[int64]SkillState{46: {Level: 1, Enabled: true}},
		Points:  SkillPointState{TotalSP: 100, RemainingSP: 80},
		Trees:   map[int]SkillTreeState{1: {Skills: map[int64]SkillState{47: {Level: 2, Enabled: true}}, Points: SkillPointState{TotalSP: 200, RemainingSP: 140}}},
		Layouts: map[int]SkillLayout{0: {0: 46}, 1: {198: 47}},
	}
	treeZeroBefore, _ := SkillTree(record, 0)
	treeOne, found := SkillTree(record, 1)
	if !found {
		t.Fatal("tree 1 was not found")
	}
	treeOne.Skills[47] = SkillState{Level: 3, Enabled: true}
	treeOne.Points.RemainingSP = 120
	SetSkillTree(&record, 1, treeOne)

	treeZeroAfter, _ := SkillTree(record, 0)
	if !reflect.DeepEqual(treeZeroAfter, treeZeroBefore) || record.Skills[46].Level != 1 || record.Points.RemainingSP != 80 {
		t.Fatalf("tree 1 write changed tree 0: before=%+v after=%+v record=%+v", treeZeroBefore, treeZeroAfter, record)
	}
	if record.Trees[1].Skills[47].Level != 3 || record.Trees[1].Points.RemainingSP != 120 {
		t.Fatalf("tree 1 write was not retained: %+v", record.Trees[1])
	}

	clone := CloneSkill(record)
	clone.Trees[1].Skills[47] = SkillState{Level: 9, Enabled: true}
	clone.Layouts[1][198] = 99
	if record.Trees[1].Skills[47].Level != 3 || record.Layouts[1][198] != 47 {
		t.Fatalf("CloneSkill shares alternate-tree state: original=%+v clone=%+v", record, clone)
	}
}
