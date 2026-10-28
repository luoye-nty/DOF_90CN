// This file defines the immutable, job-scoped PVF skill catalog.
package skill

import "errors"

const DefaultList = "skill/skilllist.lst"

var (
	ErrIndexRequired = errors.New("dnf skill index is required")
	ErrListEmpty     = errors.New("dnf skill list is empty")
	ErrDocMissing    = errors.New("dnf skill document is missing")
	ErrJobOutOfRange = errors.New("dnf skill job id is out of range")
	ErrIDOutOfRange  = errors.New("dnf skill id is out of range")
	ErrPrerequisite  = errors.New("dnf skill prerequisite is invalid")
)

type Options struct {
	ListPath string
}

// Key identifies a skill in the namespace used by the current EXE and PVF.
// Skill IDs are only unique within one job list.
type Key struct {
	Job byte   `json:"job"`
	ID  uint16 `json:"id"`
}

type Prerequisite struct {
	SkillID uint16 `json:"skill_id"`
	Level   int    `json:"level"`
}

type Skill struct {
	Job                         byte               `json:"job"`
	ID                          uint16             `json:"id"`
	Path                        string             `json:"path"`
	Name                        string             `json:"name"`
	Kind                        string             `json:"kind,omitempty"`
	Active                      bool               `json:"active,omitempty"`
	RequiredLevel               int                `json:"required_level,omitempty"`
	RequiredLevelRange          int                `json:"required_level_range,omitempty"`
	MaximumLevel                int                `json:"maximum_level,omitempty"`
	FixedLevelSkill             bool               `json:"fixed_level_skill,omitempty"`
	FixedLevelBase              int                `json:"fixed_level_base,omitempty"`
	FixedLevelInterval          int                `json:"fixed_level_interval,omitempty"`
	FixedLevelIncrement         int                `json:"fixed_level_increment,omitempty"`
	SkillClass                  int                `json:"skill_class,omitempty"`
	GrowTypes                   []int              `json:"grow_types,omitempty"`
	GrowTypeMaximumLevels       []int              `json:"grow_type_maximum_levels,omitempty"`
	SecondGrowTypes             []int              `json:"second_grow_types,omitempty"`
	SecondGrowTypeMaximumLevels []int              `json:"second_grow_type_maximum_levels,omitempty"`
	FeatureSkillType            int                `json:"feature_skill_type,omitempty"`
	FeatureSkillIndex           int                `json:"feature_skill_index,omitempty"`
	Prerequisites               []Prerequisite     `json:"prerequisites,omitempty"`
	PurchaseCost                []int              `json:"purchase_cost,omitempty"`
	SpecialPurchaseCost         []int              `json:"special_purchase_cost,omitempty"`
	CoolTime                    float64            `json:"cool_time,omitempty"`
	MPCost                      int64              `json:"mp_cost,omitempty"`
	CastTime                    float64            `json:"cast_time,omitempty"`
	Command                     string             `json:"command,omitempty"`
	Icon                        string             `json:"icon,omitempty"`
	Scalars                     map[string]float64 `json:"scalars,omitempty"`
}

func (s Skill) Key() Key {
	return Key{Job: s.Job, ID: s.ID}
}

func (s Skill) SupportsGrowType(growType int) bool {
	// Explicit [skill fitness growtype] is the profession ownership marker.
	// The current PVF also gives 98 class-owned skills a positive base entry in
	// [growtype maximum level] so the client can display/preview them before a
	// class change. That preview entry must not grant server-side learnability.
	//
	// The current and both reference 2019 PVFs also contain four passives whose
	// explicit marker does not intersect their maximum vector at all: female
	// Spitfire Nitro Motor (job 5/16), male-Striker Shine Step and Strength Fire
	// Hand Strike (job 7/236,237), and Dragon Knight Dragonian Weapon
	// (job 12/126). The client tree is driven by each exclusive maximum. Treat
	// this exact, structurally provable contradiction as a malformed marker
	// instead of rejecting the class which actually owns the skill. Ambiguous
	// vectors and ordinary preview rows continue to use the explicit marker.
	if len(s.GrowTypes) != 0 {
		// Dark Knight and Creator are external professions.  Their character
		// records deliberately keep first-grow branch zero, while the current
		// PVF reuses ordinary-profession fitness markers on several skills that
		// still have a real branch-zero maximum.  Creator quick standing is
		// declared fit for branch 1, and Dark Knight TP rows commonly declare
		// branches 1..4; treating those markers as the packed character branch
		// makes both skill families impossible to learn.  Limit the compatibility
		// alias to the two external jobs and require a positive branch-zero
		// maximum, so ordinary profession previews remain non-learnable.
		if growType == 0 && isExternalProfessionJob(s.Job) &&
			len(s.GrowTypeMaximumLevels) > 0 && s.GrowTypeMaximumLevels[0] > 0 {
			return true
		}
		if corrected, ok := s.exclusiveMaximumGrowTypeWhenFitnessContradicts(); ok {
			return growType == corrected
		}
		for _, candidate := range s.GrowTypes {
			if candidate == growType {
				return true
			}
		}
		return false
	}
	hasGrowthMaximum := false
	for _, maximum := range s.GrowTypeMaximumLevels {
		if maximum > 0 {
			hasGrowthMaximum = true
			break
		}
	}
	if hasGrowthMaximum {
		return growType >= 0 && growType < len(s.GrowTypeMaximumLevels) && s.GrowTypeMaximumLevels[growType] > 0
	}
	return true
}

func isExternalProfessionJob(job byte) bool {
	return job == 9 || job == 10
}

func (s Skill) exclusiveMaximumGrowTypeWhenFitnessContradicts() (int, bool) {
	for _, declared := range s.GrowTypes {
		if declared >= 0 && declared < len(s.GrowTypeMaximumLevels) && s.GrowTypeMaximumLevels[declared] > 0 {
			return 0, false
		}
	}
	exclusive := -1
	// Index zero is the unclassed/base preview row, never a profession repair.
	for branch := 1; branch < len(s.GrowTypeMaximumLevels); branch++ {
		if s.GrowTypeMaximumLevels[branch] <= 0 {
			continue
		}
		if exclusive >= 0 {
			return 0, false
		}
		exclusive = branch
	}
	return exclusive, exclusive >= 0
}

// SupportsCharacterGrowth applies both PVF fitness dimensions to the packed
// character grow_type byte. Low four bits select the class branch and high
// four bits select the cumulative awakening stage.
func (s Skill) SupportsCharacterGrowth(growType int) bool {
	if !s.SupportsGrowType(growType & 0x0f) {
		return false
	}
	return s.awakeningStageFits((growType >> 4) & 0x0f)
}

func (s Skill) awakeningStageFits(awakeningStage int) bool {
	if len(s.SecondGrowTypes) == 0 {
		return true
	}
	for _, minimumStage := range s.SecondGrowTypes {
		if minimumStage >= 0 && awakeningStage >= minimumStage {
			return true
		}
	}
	return false
}

// SupportsCharacterAwakening applies ONLY the awakening dimension of the PVF
// fitness pair (`[skill fitness second growtype]`) to the packed grow type.
//
// The two dimensions answer different questions and must not be relaxed
// together:
//
//   - the class branch (`[skill fitness growtype]`) decides which of the job's
//     rows the character owns. The client renders every row of the JOB, so a row
//     the player can click is learnable - see MaximumLearnableLevelForTree.
//   - the awakening stage decides whether the character has awakened at all.
//     The first awakening is granted by the awakening transition and the second
//     one requires its own quest, so this dimension keeps blocking purchases:
//     an unawakened character must not be able to buy an awakening skill simply
//     because the tree draws the row.
func (s Skill) SupportsCharacterAwakening(growType int) bool {
	return s.awakeningStageFits((growType >> 4) & 0x0f)
}

func (s Skill) IsTPSkill() bool {
	return s.FeatureSkillType != 0 || len(s.SpecialPurchaseCost) != 0
}

// FixedLevelForCharacter derives the free PVF level of an automatically
// growing skill. These levels are part of the profession baseline and never
// consume SP/TP.
func (s Skill) FixedLevelForCharacter(characterLevel int) int {
	if !s.FixedLevelSkill || characterLevel < s.RequiredLevel {
		return 0
	}
	interval := s.FixedLevelInterval
	if interval <= 0 {
		interval = 1
	}
	increment := s.FixedLevelIncrement
	if increment <= 0 {
		increment = 1
	}
	level := s.FixedLevelBase + (characterLevel-s.RequiredLevel)/interval*increment
	if level < 0 {
		return 0
	}
	if s.MaximumLevel > 0 && level > s.MaximumLevel {
		return s.MaximumLevel
	}
	return level
}

// MaximumLearnableLevel derives the highest purchased skill level allowed by
// the character's effective level and packed profession growth. Positive PVF
// limits are cumulative: the global maximum, first-grow maximum, awakening
// maximum and required-level interval all constrain the result. A missing or
// zero optional limit does not override another positive limit.
func (s Skill) MaximumLearnableLevel(characterLevel int, growType int) int {
	if characterLevel < s.RequiredLevel || !s.SupportsCharacterGrowth(growType) {
		return 0
	}
	maximum := 0
	maximum = minimumPositive(maximum, s.MaximumLevel)

	firstGrowType := growType & 0x0f
	if firstGrowType >= 0 && firstGrowType < len(s.GrowTypeMaximumLevels) {
		maximum = minimumPositive(maximum, s.GrowTypeMaximumLevels[firstGrowType])
	}

	awakeningStage := (growType >> 4) & 0x0f
	if awakeningStage > 0 {
		index := firstGrowType*2 + awakeningStage - 1
		if index >= 0 && index < len(s.SecondGrowTypeMaximumLevels) {
			maximum = minimumPositive(maximum, s.SecondGrowTypeMaximumLevels[index])
		}
	}

	if s.RequiredLevelRange > 0 {
		levelMaximum := 1 + (characterLevel-s.RequiredLevel)/s.RequiredLevelRange
		maximum = minimumPositive(maximum, levelMaximum)
	}
	return maximum
}

// MaximumLearnableLevelForTree derives the highest purchased level for a skill
// the client's own skill tree already offers.
//
// The tree is the authority on what may be learned. The current client renders
// the skill rows of the character's JOB - including the rows that belong to the
// other profession branches - and lets the player buy them, so a purchase must
// not be refused for PVF profession fitness alone. The caps below still bound
// it, and they stay meaningful for a character whose own branch carries no
// positive `[growtype maximum level]` row: that case falls back to the largest
// cap the document declares instead of dropping the bound entirely.
//
// Live evidence (2026-09-13): a level-50 base Gunner (grow_type 0) was offered
// `Ez-8自爆者` (skill 28, `[skill fitness growtype] 3`,
// `[growtype maximum level] 50 0 0 50 0 0`) and the purchase was refused, which
// the player sees as "the tree shows it but learning does nothing".
func (s Skill) MaximumLearnableLevelForTree(characterLevel int, growType int) int {
	if characterLevel < s.RequiredLevel {
		return 0
	}
	maximum := 0
	maximum = minimumPositive(maximum, s.MaximumLevel)

	firstGrowType := growType & 0x0f
	maximum = minimumPositive(maximum, s.growTypeMaximumLevelForBranch(firstGrowType))

	awakeningStage := (growType >> 4) & 0x0f
	maximum = minimumPositive(maximum, s.awakeningMaximumLevelForBranch(firstGrowType, awakeningStage))

	if s.RequiredLevelRange > 0 {
		levelMaximum := 1 + (characterLevel-s.RequiredLevel)/s.RequiredLevelRange
		maximum = minimumPositive(maximum, levelMaximum)
	}
	return maximum
}

// growTypeMaximumLevelForBranch returns the `[growtype maximum level]` entry for
// the character's own branch, or the largest declared entry when that branch has
// no positive cap of its own (the PVF only fills the branches it declares fit).
func (s Skill) growTypeMaximumLevelForBranch(branch int) int {
	if branch >= 0 && branch < len(s.GrowTypeMaximumLevels) && s.GrowTypeMaximumLevels[branch] > 0 {
		return s.GrowTypeMaximumLevels[branch]
	}
	return largestPositive(s.GrowTypeMaximumLevels)
}

// awakeningMaximumLevelForBranch returns the `[skill fitness second growtype]`
// cap that applies to the character's awakening stage. It mirrors the
// fitness-aware maximum: a stage the document does not cap contributes no bound,
// because the awakening dimension is enforced as a purchase gate (see
// SupportsCharacterAwakening) rather than through a zero maximum.
func (s Skill) awakeningMaximumLevelForBranch(branch int, stage int) int {
	if stage <= 0 {
		return 0
	}
	index := branch*2 + stage - 1
	if index >= 0 && index < len(s.SecondGrowTypeMaximumLevels) {
		return s.SecondGrowTypeMaximumLevels[index]
	}
	return 0
}

func largestPositive(values []int) int {
	largest := 0
	for _, value := range values {
		if value > largest {
			largest = value
		}
	}
	return largest
}

func minimumPositive(current int, candidate int) int {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func (s Skill) LevelCost(level int) int {
	costs := s.PurchaseCost
	if s.IsTPSkill() {
		costs = s.SpecialPurchaseCost
	}
	return costAtLevel(costs, level)
}

func costAtLevel(costs []int, level int) int {
	if len(costs) == 0 || level < 0 {
		return 0
	}
	if level >= len(costs) {
		return costs[len(costs)-1]
	}
	return costs[level]
}

type jobPathKey struct {
	Job  byte
	Path string
}

type Table struct {
	skills []Skill
	byKey  map[Key]int
	byPath map[jobPathKey]int
	byJob  map[byte][]int
}

type Snapshot struct {
	Jobs   int `json:"jobs"`
	Skills int `json:"skills"`
}

// Skills returns a deep copy of the complete catalog.
func (t *Table) Skills() []Skill {
	if t == nil || len(t.skills) == 0 {
		return nil
	}
	out := make([]Skill, len(t.skills))
	for idx, definition := range t.skills {
		out[idx] = cloneSkill(definition)
	}
	return out
}

// SkillsForJob returns a deep copy of one job's definitions.
func (t *Table) SkillsForJob(job byte) []Skill {
	if t == nil || len(t.byJob[job]) == 0 {
		return nil
	}
	indexes := t.byJob[job]
	out := make([]Skill, 0, len(indexes))
	for _, idx := range indexes {
		out = append(out, cloneSkill(t.skills[idx]))
	}
	return out
}

// Find resolves a skill by its job-scoped PVF key.
func (t *Table) Find(job byte, id uint16) (Skill, bool) {
	if t == nil {
		return Skill{}, false
	}
	idx, ok := t.byKey[Key{Job: job, ID: id}]
	if !ok {
		return Skill{}, false
	}
	return cloneSkill(t.skills[idx]), true
}

// FindPath resolves a skill path inside one job namespace.
func (t *Table) FindPath(job byte, skillPath string) (Skill, bool) {
	if t == nil {
		return Skill{}, false
	}
	idx, ok := t.byPath[jobPathKey{Job: job, Path: pathKey(skillPath)}]
	if !ok {
		return Skill{}, false
	}
	return cloneSkill(t.skills[idx]), true
}

func (t *Table) Snapshot() Snapshot {
	if t == nil {
		return Snapshot{}
	}
	return Snapshot{Jobs: len(t.byJob), Skills: len(t.skills)}
}

func cloneSkill(definition Skill) Skill {
	definition.GrowTypes = append([]int(nil), definition.GrowTypes...)
	definition.GrowTypeMaximumLevels = append([]int(nil), definition.GrowTypeMaximumLevels...)
	definition.SecondGrowTypes = append([]int(nil), definition.SecondGrowTypes...)
	definition.SecondGrowTypeMaximumLevels = append([]int(nil), definition.SecondGrowTypeMaximumLevels...)
	definition.Prerequisites = append([]Prerequisite(nil), definition.Prerequisites...)
	definition.PurchaseCost = append([]int(nil), definition.PurchaseCost...)
	definition.SpecialPurchaseCost = append([]int(nil), definition.SpecialPurchaseCost...)
	if len(definition.Scalars) > 0 {
		scalars := make(map[string]float64, len(definition.Scalars))
		for key, value := range definition.Scalars {
			scalars[key] = value
		}
		definition.Scalars = scalars
	}
	return definition
}
