// 本文件定义 DNF 技能仓储接口、记录和字段保存入口。
package repository

import (
	"context"
	"strings"
	"time"

	"longheng.io/server/internal/platform/db"
)

// SkillRepository 保存角色技能等级、释放指令和冷却状态。
type SkillRepository interface {
	db.Store[SkillRecord]
}

// SkillRecord 是角色技能仓储记录。
type SkillRecord struct {
	CharacterID string                 `json:"character_id"`
	Skills      map[int64]SkillState   `json:"skills,omitempty"`
	Points      SkillPointState        `json:"points,omitempty"`
	Trees       map[int]SkillTreeState `json:"trees,omitempty"`
	Layouts     map[int]SkillLayout    `json:"layouts,omitempty"`
	Commands    map[int64][]byte       `json:"commands,omitempty"`
	Cooldowns   map[int64]time.Time    `json:"cooldowns,omitempty"`
	UpdatedAt   time.Time              `json:"updated_at,omitempty"`
}

// SkillTreeState owns the learned levels and SP/TP ledger for one alternate
// current-EXE skill plan. Tree zero remains in SkillRecord.Skills/Points so
// existing character data keeps its original storage and meaning.
type SkillTreeState struct {
	Skills map[int64]SkillState `json:"skills,omitempty"`
	Points SkillPointState      `json:"points,omitempty"`
}

// SkillState 是单个技能的可变状态。
type SkillState struct {
	Level   int  `json:"level,omitempty"`
	Enabled bool `json:"enabled,omitempty"`
}

// SkillPointState is the authoritative per-character SP/TP ledger.
type SkillPointState struct {
	TotalSP     int `json:"total_sp,omitempty"`
	RemainingSP int `json:"remaining_sp,omitempty"`
	TotalTP     int `json:"total_tp,omitempty"`
	RemainingTP int `json:"remaining_tp,omitempty"`
	SyncedLevel int `json:"synced_level,omitempty"`
}

// SkillLayout maps a current-EXE skill UI slot to a job-scoped skill ID.
type SkillLayout map[int]uint16

// SkillField 表示技能记录可局部保存的字段。
type SkillField string

const (
	SkillFieldSkills    SkillField = "skills"
	SkillFieldPoints    SkillField = "points"
	SkillFieldTrees     SkillField = "trees"
	SkillFieldLayouts   SkillField = "layouts"
	SkillFieldCommands  SkillField = "commands"
	SkillFieldCooldowns SkillField = "cooldowns"
)

// SaveSkillFields 保存技能指定字段；底层不支持局部保存时退化为整条保存。
func SaveSkillFields(ctx context.Context, repo SkillRepository, record SkillRecord, fields ...SkillField) error {
	return db.SaveFields(ctx, repo, record, SkillFields.Normalize, fields...)
}

// CloneSkill 拷贝技能记录，避免技能表和冷却表与调用方共享。
func CloneSkill(record SkillRecord) SkillRecord {
	record.Skills = cloneSkillMap(record.Skills)
	record.Trees = cloneSkillTrees(record.Trees)
	record.Layouts = cloneSkillLayouts(record.Layouts)
	record.Commands = cloneByteMap(record.Commands)
	record.Cooldowns = cloneTimeMap(record.Cooldowns)
	return record
}

// SkillTree returns an isolated view of one skill plan. Tree zero is backed by
// the legacy top-level fields; alternate plans are backed by Trees.
func SkillTree(record SkillRecord, tree int) (SkillTreeState, bool) {
	if tree == 0 {
		if len(record.Skills) == 0 {
			return SkillTreeState{}, false
		}
		return SkillTreeState{Skills: cloneSkillMap(record.Skills), Points: record.Points}, true
	}
	state, ok := record.Trees[tree]
	if !ok {
		return SkillTreeState{}, false
	}
	state.Skills = cloneSkillMap(state.Skills)
	return state, true
}

// SetSkillTree writes one isolated skill plan without changing any other tree.
func SetSkillTree(record *SkillRecord, tree int, state SkillTreeState) {
	if record == nil {
		return
	}
	state.Skills = cloneSkillMap(state.Skills)
	if tree == 0 {
		record.Skills = state.Skills
		record.Points = state.Points
		return
	}
	if record.Trees == nil {
		record.Trees = make(map[int]SkillTreeState)
	}
	record.Trees[tree] = state
}

func cloneSkillTrees(trees map[int]SkillTreeState) map[int]SkillTreeState {
	if trees == nil {
		return nil
	}
	out := make(map[int]SkillTreeState, len(trees))
	for tree, state := range trees {
		state.Skills = cloneSkillMap(state.Skills)
		out[tree] = state
	}
	return out
}

func cloneByteMap(values map[int64][]byte) map[int64][]byte {
	if values == nil {
		return nil
	}
	out := make(map[int64][]byte, len(values))
	for key, value := range values {
		out[key] = append([]byte(nil), value...)
	}
	return out
}

func cloneSkillLayouts(layouts map[int]SkillLayout) map[int]SkillLayout {
	if layouts == nil {
		return nil
	}
	out := make(map[int]SkillLayout, len(layouts))
	for tree, layout := range layouts {
		copyLayout := make(SkillLayout, len(layout))
		for slot, skillID := range layout {
			copyLayout[slot] = skillID
		}
		out[tree] = copyLayout
	}
	return out
}

func SkillKey(record SkillRecord) string {
	return strings.TrimSpace(record.CharacterID)
}
