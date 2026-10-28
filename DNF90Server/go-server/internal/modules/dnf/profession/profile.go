package profession

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

const DefaultCharacterList = "character/character.lst"

var (
	ErrSourceRequired = errors.New("dnf profession PVF source is required")
	ErrProfileMissing = errors.New("dnf profession profile is missing")
	ErrSkillMissing   = errors.New("dnf profession grant skill is missing")
	ErrSkillGrant     = errors.New("dnf profession skill grant is malformed")
)

var (
	growTypeSectionRE  = regexp.MustCompile(`(?i)\[\s*growtype\s+(\d+)\s*\]`)
	awakeningSectionRE = regexp.MustCompile(`(?i)\[\s*awakening\s+(\d+)\s*\]`)
	integerRE          = regexp.MustCompile(`-?\d+`)
)

type Source interface {
	ReadText(relativePath string) (string, error)
}

type Grant struct {
	SkillID uint16 `json:"skill_id"`
	Level   int    `json:"level"`
}

type jobProfile struct {
	initial            []Grant
	grow               map[byte][]Grant
	growSupported      map[byte]bool
	awakening          map[byte]map[byte][]Grant
	awakeningSupported map[byte]map[byte]bool
	displayNames       map[byte]string
	awakeningNames     map[byte]map[byte]string
}

type Profiles struct {
	jobs               map[byte]jobProfile
	missingSkillGrants int
}

type Snapshot struct {
	Jobs            int
	InitialGrants   int
	ClassGrants     int
	AwakeningGrants int
	MissingSkills   int
}

// Option is one PVF-backed base job and all class/awakening selections the
// local GM surface can safely apply.
type Option struct {
	Job      byte           `json:"job"`
	Name     string         `json:"name"`
	Branches []BranchOption `json:"branches"`
}

type BranchOption struct {
	Value      byte              `json:"value"`
	Name       string            `json:"name"`
	Awakenings []AwakeningOption `json:"awakenings"`
}

type AwakeningOption struct {
	Value byte   `json:"value"`
	Name  string `json:"name"`
}

func LoadProfiles(ctx context.Context, source Source, catalog *dnfskill.Table) (*Profiles, error) {
	if source == nil || catalog == nil {
		return nil, ErrSourceRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	listText, err := source.ReadText(DefaultCharacterList)
	if err != nil {
		return nil, err
	}
	doc, err := dnfpvf.Parse(DefaultCharacterList, listText)
	if err != nil {
		return nil, err
	}
	entries := dnfpvf.ParseList(doc)
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrProfileMissing, DefaultCharacterList)
	}
	profiles := &Profiles{jobs: make(map[byte]jobProfile, len(entries))}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.ID < 0 || entry.ID > math.MaxUint8 || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		job := byte(entry.ID)
		charPath := path.Clean(path.Join(path.Dir(DefaultCharacterList), strings.ReplaceAll(entry.Path, "\\", "/")))
		text, err := source.ReadText(charPath)
		if err != nil {
			return nil, fmt.Errorf("read profession profile job=%d path=%s: %w", job, charPath, err)
		}
		profile, err := parseJobProfile(text)
		if err != nil {
			return nil, fmt.Errorf("parse profession profile job=%d path=%s: %w", job, charPath, err)
		}
		profiles.missingSkillGrants += countMissingProfileSkills(catalog, job, profile)
		profiles.jobs[job] = profile
	}
	if len(profiles.jobs) == 0 {
		return nil, ErrProfileMissing
	}
	return profiles, nil
}

func (p *Profiles) Snapshot() Snapshot {
	if p == nil {
		return Snapshot{}
	}
	out := Snapshot{Jobs: len(p.jobs), MissingSkills: p.missingSkillGrants}
	for _, profile := range p.jobs {
		out.InitialGrants += len(profile.initial)
		for _, grants := range profile.grow {
			out.ClassGrants += len(grants)
		}
		for _, stages := range profile.awakening {
			for _, grants := range stages {
				out.AwakeningGrants += len(grants)
			}
		}
	}
	return out
}

// Options returns the administrative profession choices in stable numeric
// order, derived exclusively from the loaded Script.pvf.
func (p *Profiles) Options() []Option {
	if p == nil {
		return nil
	}
	jobs := make([]int, 0, len(p.jobs))
	for job := range p.jobs {
		jobs = append(jobs, int(job))
	}
	sort.Ints(jobs)
	out := make([]Option, 0, len(jobs))
	for _, rawJob := range jobs {
		job := byte(rawJob)
		profile := p.jobs[job]
		option := Option{Job: job, Name: strings.TrimSpace(profile.displayNames[0])}
		branches := make(map[byte]struct{}, len(profile.growSupported)+1)
		branches[0] = struct{}{}
		for branch, supported := range profile.growSupported {
			if supported {
				branches[branch] = struct{}{}
			}
		}
		branchValues := make([]int, 0, len(branches))
		for branch := range branches {
			branchValues = append(branchValues, int(branch))
		}
		sort.Ints(branchValues)
		for _, rawBranch := range branchValues {
			branch := byte(rawBranch)
			branchOption := BranchOption{Value: branch, Name: strings.TrimSpace(profile.displayNames[branch])}
			stages := make([]int, 0, len(profile.awakeningSupported[branch]))
			for stage, supported := range profile.awakeningSupported[branch] {
				if supported {
					stages = append(stages, int(stage))
				}
			}
			sort.Ints(stages)
			for _, rawStage := range stages {
				stage := byte(rawStage)
				branchOption.Awakenings = append(branchOption.Awakenings, AwakeningOption{
					Value: stage,
					Name:  strings.TrimSpace(profile.awakeningNames[branch][stage]),
				})
			}
			option.Branches = append(option.Branches, branchOption)
		}
		out = append(out, option)
	}
	return out
}

// SupportsSelection validates an administrative final profession state
// against the sections actually present in the loaded PVF.
func (p *Profiles) SupportsSelection(job byte, first byte, awakening byte) bool {
	profile, ok := p.profile(job)
	if !ok {
		return false
	}
	if first != 0 && !profile.growSupported[first] {
		return false
	}
	if awakening == 0 {
		return true
	}
	return profile.awakeningSupported[first][awakening]
}

func (p *Profiles) Initial(job byte) ([]Grant, bool) {
	profile, ok := p.profile(job)
	if !ok {
		return nil, false
	}
	return cloneGrants(profile.initial), true
}

// DisplayName returns the profession name declared by the current Script.pvf.
// The low grow-type nibble selects the class branch and the high nibble
// selects its awakening stage. No client-side profession table is guessed.
func (p *Profiles) DisplayName(job byte, growType byte) (string, bool) {
	profile, ok := p.profile(job)
	if !ok {
		return "", false
	}
	first, awakening := Decode(growType)
	if awakening > 0 {
		if name := strings.TrimSpace(profile.awakeningNames[byte(first)][byte(awakening)]); name != "" {
			return name, true
		}
	}
	name := strings.TrimSpace(profile.displayNames[byte(first)])
	return name, name != ""
}

// FreeGrants returns the raw cumulative PVF profession declaration for the
// selected branch. It intentionally preserves paid [awakening skill] rows for
// audits and transition compatibility; reset callers use FreeGrantsAtLevel.
func (p *Profiles) FreeGrants(job byte, firstGrowType byte, awakeningStage byte) ([]Grant, bool) {
	profile, ok := p.profile(job)
	if !ok {
		return nil, false
	}
	merged := make(map[uint16]int)
	mergeGrantLevels(merged, profile.initial)
	mergeGrantLevels(merged, profile.grow[firstGrowType])
	for stage := byte(1); stage <= awakeningStage; stage++ {
		mergeGrantLevels(merged, profile.awakening[firstGrowType][stage])
	}
	return sortedGrants(merged), true
}

// FreeGrantsAtLevel returns the current profession's cumulative PVF free
// baseline after applying fixed-level skill growth for the character level.
// Skill reset callers use this instead of falling back to base-job grants.
func (p *Profiles) FreeGrantsAtLevel(catalog *dnfskill.Table, job byte, growType byte, characterLevel int) ([]Grant, bool) {
	if p == nil || catalog == nil {
		return nil, false
	}
	firstGrowType, awakeningStage := Decode(growType)
	grants, ok := p.protectedGrants(catalog, job, firstGrowType, awakeningStage)
	if !ok {
		return nil, false
	}
	grants = grantsAtCharacterLevel(catalog, job, characterLevel, grants)
	return mergeCatalogSystemGrants(catalog, job, firstGrowType, awakeningStage, characterLevel, grants), true
}

// protectedGrants separates PVF profession ownership from the levels that are
// actually free and must survive a skill reset. Initial and class-change
// [skill] rows are explicit grants. [awakening skill], however, mixes free
// awakening actives with SP-purchased passives in the current PVF. A declared
// positive purchase cost therefore makes only the awakening row learnable; it
// must not become an automatic/protected level.
//
// Missing catalog definitions and rows without an explicit purchase table are
// deliberately retained. The existing missing-grant audit and the mutation
// owner's catalog validation must continue to expose incomplete PVF data
// instead of silently turning it into a smaller profession baseline.
func (p *Profiles) protectedGrants(
	catalog *dnfskill.Table,
	job byte,
	firstGrowType byte,
	awakeningStage byte,
) ([]Grant, bool) {
	profile, ok := p.profile(job)
	if !ok {
		return nil, false
	}
	merged := make(map[uint16]int)
	mergeGrantLevels(merged, profile.initial)
	mergeGrantLevels(merged, profile.grow[firstGrowType])
	for stage := byte(1); stage <= awakeningStage; stage++ {
		for _, grant := range profile.awakening[firstGrowType][stage] {
			definition, found := catalog.Find(job, grant.SkillID)
			if found && len(definition.PurchaseCost) > 0 && definition.LevelCost(0) > 0 {
				continue
			}
			if grant.Level > merged[grant.SkillID] {
				merged[grant.SkillID] = grant.Level
			}
		}
	}
	return sortedGrants(merged), true
}

func (p *Profiles) profile(job byte) (jobProfile, bool) {
	if p == nil {
		return jobProfile{}, false
	}
	profile, ok := p.jobs[job]
	return profile, ok
}

// ApplySkillTransition mutates only the detached SkillRecord. The caller owns
// the surrounding repository transaction and saves the returned fields.
func (p *Profiles) ApplySkillTransition(catalog *dnfskill.Table, job byte, characterLevel int, transition Transition, record dnfrepo.SkillRecord, points dnfrepo.SkillPointState) (dnfrepo.SkillRecord, error) {
	if p == nil || catalog == nil {
		return dnfrepo.SkillRecord{}, ErrSourceRequired
	}
	growType, err := Encode(transition.FirstGrowType, transition.AwakeningStage)
	if err != nil {
		return dnfrepo.SkillRecord{}, err
	}
	grants, ok := p.FreeGrantsAtLevel(catalog, job, growType, characterLevel)
	if !ok {
		return dnfrepo.SkillRecord{}, fmt.Errorf("%w: job=%d", ErrProfileMissing, job)
	}
	record = dnfrepo.CloneSkill(record)
	switch transition.Kind {
	case KindClassChange:
		// A class change refunds every purchased SP/TP, so the learned levels are
		// re-seeded from the PVF free baseline and bought skills must not survive.
		// The skill UI layout is a different matter: it is the arrangement the
		// player made, and BuildInitialSkillLayout is documented as the *first*
		// persisted layout for a brand-new character. Re-seeding it here silently
		// moved skills the player had placed (live 2026-09-15: 上挑/后跳 were
		// bumped off the asdfgh row and 三段斩/刀魂之卡赞 put in their cells) and
		// left the free cells 3..5 empty instead of filling them with the newly
		// granted actives. Keep every previous assignment whose skill is still in
		// the new baseline and let the same preserving allocator the login/backfill
		// path uses place the newly granted skills.
		previousLayouts := record.Layouts
		record.Cooldowns = nil
		record.Trees = nil
		record.Layouts = make(map[int]dnfrepo.SkillLayout, 2)
		points.RemainingSP = points.TotalSP
		points.RemainingTP = points.TotalTP
		for tree := 0; tree < 2; tree++ {
			state := dnfrepo.SkillTreeState{Skills: grantStates(grants), Points: points}
			layout, buildErr := skillcmd.BuildCurrentSkillLayout(catalog, job, tree, state.Skills, previousLayouts[tree])
			if buildErr != nil {
				return dnfrepo.SkillRecord{}, buildErr
			}
			dnfrepo.SetSkillTree(&record, tree, state)
			record.Layouts[tree] = layout
		}
	case KindAwakening:
		if record.Layouts == nil {
			record.Layouts = make(map[int]dnfrepo.SkillLayout, 2)
		}
		for tree := 0; tree < 2; tree++ {
			state, found := dnfrepo.SkillTree(record, tree)
			if !found {
				state, found = dnfrepo.SkillTree(record, 0)
			}
			if !found {
				return dnfrepo.SkillRecord{}, fmt.Errorf("%w: job=%d tree=%d", ErrSkillGrant, job, tree)
			}
			if state.Skills == nil {
				state.Skills = make(map[int64]dnfrepo.SkillState)
			}
			for _, grant := range grants {
				current := state.Skills[int64(grant.SkillID)]
				if current.Level < grant.Level {
					current.Level = grant.Level
				}
				current.Enabled = true
				state.Skills[int64(grant.SkillID)] = current
			}
			layout, buildErr := skillcmd.BuildCurrentSkillLayout(catalog, job, tree, state.Skills, record.Layouts[tree])
			if buildErr != nil {
				return dnfrepo.SkillRecord{}, buildErr
			}
			dnfrepo.SetSkillTree(&record, tree, state)
			record.Layouts[tree] = layout
		}
	default:
		return dnfrepo.SkillRecord{}, fmt.Errorf("%w: kind=%d", ErrTransitionInvalid, transition.Kind)
	}
	return record, nil
}

// RebuildSkillRecordForGrowType performs an explicit administrative
// profession rebase. Unlike the forward-only quest transition above, it
// replaces both skill plans with the exact cumulative PVF free baseline for
// targetGrowType, refunds all purchased SP/TP, and drops commands/cooldowns
// that belonged to the previous branch. The caller must commit the returned
// record and grow_type in one repository transaction.
func (p *Profiles) RebuildSkillRecordForGrowType(catalog *dnfskill.Table, job byte, characterLevel int, targetGrowType byte, record dnfrepo.SkillRecord, points dnfrepo.SkillPointState) (dnfrepo.SkillRecord, error) {
	if p == nil || catalog == nil {
		return dnfrepo.SkillRecord{}, ErrSourceRequired
	}
	profile, ok := p.profile(job)
	if !ok {
		return dnfrepo.SkillRecord{}, fmt.Errorf("%w: job=%d", ErrProfileMissing, job)
	}
	firstGrowType, awakeningStage := Decode(targetGrowType)
	supported := firstGrowType == 0 || profile.growSupported[firstGrowType]
	if awakeningStage > 0 {
		supported = supported && profile.awakeningSupported[firstGrowType][awakeningStage]
	}
	if !supported {
		return dnfrepo.SkillRecord{}, fmt.Errorf("%w: job=%d grow_type=0x%02x", ErrTransitionInvalid, job, targetGrowType)
	}
	grants, ok := p.FreeGrantsAtLevel(catalog, job, targetGrowType, characterLevel)
	if !ok {
		return dnfrepo.SkillRecord{}, fmt.Errorf("%w: job=%d grow_type=0x%02x", ErrProfileMissing, job, targetGrowType)
	}
	record = dnfrepo.CloneSkill(record)
	record.Commands = nil
	record.Cooldowns = nil
	record.Trees = nil
	record.Layouts = make(map[int]dnfrepo.SkillLayout, 2)
	points.RemainingSP = points.TotalSP
	points.RemainingTP = points.TotalTP
	for tree := 0; tree < 2; tree++ {
		state := dnfrepo.SkillTreeState{Skills: grantStates(grants), Points: points}
		layout, err := skillcmd.BuildInitialSkillLayout(catalog, job, tree, state.Skills)
		if err != nil {
			return dnfrepo.SkillRecord{}, err
		}
		dnfrepo.SetSkillTree(&record, tree, state)
		record.Layouts[tree] = layout
	}
	return record, nil
}

func grantsAtCharacterLevel(catalog *dnfskill.Table, job byte, characterLevel int, grants []Grant) []Grant {
	out := cloneGrants(grants)
	for index := range out {
		definition, ok := catalog.Find(job, out[index].SkillID)
		if !ok {
			continue
		}
		if fixedLevel := definition.FixedLevelForCharacter(characterLevel); fixedLevel > out[index].Level {
			out[index].Level = fixedLevel
		}
	}
	return out
}

// mergeCatalogSystemGrants closes the intentional split between the
// character profile and zero-cost profession skills. Most automatic skills
// are listed in character/*.chr, but the current PVF keeps a small set (for
// example male Slayer skill 248, Burning Blood) only in the .skl catalog.
// Their authoritative markers are a profession-specific grow-type maximum,
// a zero purchase cost, no prerequisite purchase chain and an ordinary combat
// skill class. The prerequisite distinction is material for ordinary
// first-grow rows: this PVF also uses a zero first-level cost for optional
// one-level toggles such as Mage skill 167 (Clear Flapper). Protecting one of
// those rows as a system grant makes the native refund request fail at the
// permanent floor. The small mapped awakening-helper set below may retain its
// authored prerequisite chain because branch and stage are independently
// proved there. Skill class 4 is
// deliberately excluded because it also contains guild and expert-job
// abilities which are unlocked by their own systems.
//
// Eight zero-cost awakening helpers in this compatibility PVF omit both the
// character-profile grant and a branch-bearing growtype maximum. Their
// branches are nevertheless fixed by their PVF awakening names and required
// skill chains (for example Star Pattern Explosion requires Transform
// Bellatrix). Keep that small evidence-backed mapping explicit instead of
// guessing a branch for every second-growtype skill.
type awakeningSystemGrantRule struct {
	FirstGrowType byte
}

var awakeningSystemGrantRules = map[dnfskill.Key]awakeningSystemGrantRule{
	{Job: 1, ID: 114}:  {FirstGrowType: 3}, // 禁技 : 万毒蛇窟
	{Job: 3, ID: 33}:   {FirstGrowType: 1}, // 圣灵符文
	{Job: 3, ID: 195}:  {FirstGrowType: 4}, // 魔道学助手
	{Job: 3, ID: 300}:  {FirstGrowType: 3}, // 使徒封印
	{Job: 3, ID: 301}:  {FirstGrowType: 3}, // 贝亚娜之秘
	{Job: 3, ID: 305}:  {FirstGrowType: 3}, // 星纹陨爆
	{Job: 3, ID: 314}:  {FirstGrowType: 2}, // 咒令 : 逆月之蚀
	{Job: 10, ID: 268}: {FirstGrowType: 0}, // 自我觉醒
}

func mergeCatalogSystemGrants(
	catalog *dnfskill.Table,
	job byte,
	firstGrowType byte,
	awakeningStage byte,
	characterLevel int,
	grants []Grant,
) []Grant {
	levels := make(map[uint16]int, len(grants))
	mergeGrantLevels(levels, grants)
	if catalog == nil || characterLevel <= 0 {
		return sortedGrants(levels)
	}
	for _, definition := range catalog.SkillsForJob(job) {
		if definition.RequiredLevel > characterLevel || definition.MaximumLevel <= 0 ||
			definition.SkillClass < 1 || definition.SkillClass > 3 ||
			len(definition.PurchaseCost) == 0 || definition.LevelCost(0) != 0 ||
			(len(definition.Prerequisites) != 0 && len(definition.SecondGrowTypes) == 0) ||
			!catalogSystemGrantMatchesGrowth(definition, firstGrowType, awakeningStage) {
			continue
		}
		if levels[definition.ID] < 1 {
			levels[definition.ID] = 1
		}
	}
	return sortedGrants(levels)
}

func catalogSystemGrantMatchesGrowth(definition dnfskill.Skill, firstGrowType byte, awakeningStage byte) bool {
	if len(definition.SecondGrowTypes) == 0 {
		return firstGrowType != 0 &&
			int(firstGrowType) < len(definition.GrowTypeMaximumLevels) &&
			definition.GrowTypeMaximumLevels[firstGrowType] > 0
	}
	rule, found := awakeningSystemGrantRules[definition.Key()]
	if !found || rule.FirstGrowType != firstGrowType {
		return false
	}
	minimumStage := byte(0xff)
	for _, stage := range definition.SecondGrowTypes {
		if stage > 0 && stage < int(minimumStage) {
			minimumStage = byte(stage)
		}
	}
	return minimumStage != 0xff && awakeningStage >= minimumStage
}

// PlanTransition applies one detached quest reward to the transaction's
// current packed grow_type. Section presence in character/*.chr is the source
// of truth, including branch-zero awakenings whose free-grant lists are empty.
func (p *Profiles) PlanTransition(job byte, current byte, request Request) (Transition, error) {
	profile, ok := p.profile(job)
	if !ok {
		return Transition{}, fmt.Errorf("%w: job=%d", ErrProfileMissing, job)
	}
	first, awakening := Decode(current)
	switch request.Kind {
	case KindClassChange:
		if request.ChainType != 1 || request.GrowNumber == 0 || first != 0 || awakening != 0 {
			return Transition{}, fmt.Errorf("%w: class change current=0x%02x target=%d", ErrTransitionOutOfStep, current, request.GrowNumber)
		}
		if !profile.growSupported[request.GrowNumber] {
			return Transition{}, fmt.Errorf("%w: job=%d class=%d", ErrTransitionInvalid, job, request.GrowNumber)
		}
		return Transition{
			Kind: KindClassChange, ChainType: request.ChainType, GrowNumber: request.GrowNumber,
			PreviousGrowType: current, NewGrowType: request.GrowNumber,
			FirstGrowType: request.GrowNumber, AwakeningStage: 0,
		}, nil
	case KindAwakening:
		target := request.GrowNumber
		if request.ChainType != 2 || target == 0 || target > 2 || target != awakening+1 {
			return Transition{}, fmt.Errorf("%w: awakening current=0x%02x target=%d", ErrTransitionOutOfStep, current, target)
		}
		if !profile.awakeningSupported[first][target] {
			return Transition{}, fmt.Errorf("%w: job=%d first=%d awakening=%d", ErrTransitionInvalid, job, first, target)
		}
		packed, err := Encode(first, target)
		if err != nil {
			return Transition{}, err
		}
		return Transition{
			Kind: KindAwakening, ChainType: request.ChainType, GrowNumber: target,
			PreviousGrowType: current, NewGrowType: packed,
			FirstGrowType: first, AwakeningStage: target,
		}, nil
	default:
		return Transition{}, fmt.Errorf("%w: kind=%d", ErrTransitionInvalid, request.Kind)
	}
}

func parseJobProfile(text string) (jobProfile, error) {
	profile := jobProfile{
		grow:               make(map[byte][]Grant),
		growSupported:      make(map[byte]bool),
		awakening:          make(map[byte]map[byte][]Grant),
		awakeningSupported: make(map[byte]map[byte]bool),
		displayNames:       make(map[byte]string),
		awakeningNames:     make(map[byte]map[byte]string),
	}
	doc, err := dnfpvf.Parse("character/profile.chr", text)
	if err != nil {
		return jobProfile{}, err
	}
	for index, name := range doc.Texts("growtype name") {
		if index > math.MaxUint8 {
			break
		}
		if name = strings.TrimSpace(name); name != "" && !strings.HasPrefix(name, "//") {
			profile.displayNames[byte(index)] = name
		}
	}
	growMatches := growTypeSectionRE.FindAllStringSubmatchIndex(text, -1)
	initialEnd := len(text)
	if len(growMatches) > 0 {
		initialEnd = growMatches[0][0]
	}
	profile.initial = parseGrantSection(text[:initialEnd], "skill")
	for index, match := range growMatches {
		n, err := strconv.Atoi(text[match[2]:match[3]])
		if err != nil || n < 1 || n > 16 {
			continue
		}
		firstGrowType := byte(n - 1)
		profile.growSupported[firstGrowType] = true
		segmentEnd := len(text)
		if index+1 < len(growMatches) {
			segmentEnd = growMatches[index+1][0]
		}
		segment := text[match[1]:segmentEnd]
		segmentDoc, parseErr := dnfpvf.Parse("character/profile-grow.chr", segment)
		if parseErr != nil {
			return jobProfile{}, parseErr
		}
		for awakeningIndex, name := range segmentDoc.Texts("awakening name") {
			stage := awakeningIndex + 1
			if stage > math.MaxUint8 {
				break
			}
			name = strings.TrimSpace(name)
			if name == "" || strings.HasPrefix(name, "//") {
				continue
			}
			if profile.awakeningNames[firstGrowType] == nil {
				profile.awakeningNames[firstGrowType] = make(map[byte]string)
			}
			profile.awakeningNames[firstGrowType][byte(stage)] = name
		}
		awakeningMatches := awakeningSectionRE.FindAllStringSubmatchIndex(segment, -1)
		growEnd := len(segment)
		if len(awakeningMatches) > 0 {
			growEnd = awakeningMatches[0][0]
		}
		if grants := parseGrantSection(segment[:growEnd], "skill"); n >= 2 && len(grants) > 0 {
			profile.grow[firstGrowType] = grants
		}
		for stageIndex, awakeningMatch := range awakeningMatches {
			stageValue, err := strconv.Atoi(segment[awakeningMatch[2]:awakeningMatch[3]])
			if err != nil || stageValue < 1 || stageValue > 15 {
				continue
			}
			if profile.awakeningSupported[firstGrowType] == nil {
				profile.awakeningSupported[firstGrowType] = make(map[byte]bool)
			}
			profile.awakeningSupported[firstGrowType][byte(stageValue)] = true
			awakeningEnd := len(segment)
			if stageIndex+1 < len(awakeningMatches) {
				awakeningEnd = awakeningMatches[stageIndex+1][0]
			}
			grants := parseGrantSection(segment[awakeningMatch[1]:awakeningEnd], "awakening skill")
			if len(grants) == 0 {
				continue
			}
			if profile.awakening[firstGrowType] == nil {
				profile.awakening[firstGrowType] = make(map[byte][]Grant)
			}
			profile.awakening[firstGrowType][byte(stageValue)] = grants
		}
	}
	if len(profile.initial) == 0 {
		return jobProfile{}, fmt.Errorf("%w: missing [initial value] [skill] grants", ErrSkillGrant)
	}
	return profile, nil
}

func parseGrantSection(text string, name string) []Grant {
	open := regexp.MustCompile(`(?i)\[\s*` + regexp.QuoteMeta(name) + `\s*\]`).FindStringIndex(text)
	if open == nil {
		return nil
	}
	close := regexp.MustCompile(`(?i)\[\s*/\s*` + regexp.QuoteMeta(name) + `\s*\]`).FindStringIndex(text[open[1]:])
	bodyEnd := len(text)
	if close != nil {
		bodyEnd = open[1] + close[0]
	}
	values := integerRE.FindAllString(text[open[1]:bodyEnd], -1)
	levels := make(map[uint16]int)
	for index := 0; index+1 < len(values); index += 2 {
		skillID, skillErr := strconv.ParseInt(values[index], 10, 64)
		level, levelErr := strconv.Atoi(values[index+1])
		if skillErr != nil || levelErr != nil || skillID <= 0 || skillID > math.MaxUint16 || level <= 0 {
			continue
		}
		id := uint16(skillID)
		if level > levels[id] {
			levels[id] = level
		}
	}
	return sortedGrants(levels)
}

func countMissingProfileSkills(catalog *dnfskill.Table, job byte, profile jobProfile) int {
	missing := make(map[uint16]struct{})
	check := func(grants []Grant) {
		for _, grant := range grants {
			if _, ok := catalog.Find(job, grant.SkillID); !ok {
				missing[grant.SkillID] = struct{}{}
			}
		}
	}
	check(profile.initial)
	for _, grants := range profile.grow {
		check(grants)
	}
	for _, stages := range profile.awakening {
		for _, grants := range stages {
			check(grants)
		}
	}
	return len(missing)
}

func grantStates(grants []Grant) map[int64]dnfrepo.SkillState {
	states := make(map[int64]dnfrepo.SkillState, len(grants))
	for _, grant := range grants {
		states[int64(grant.SkillID)] = dnfrepo.SkillState{Level: grant.Level, Enabled: true}
	}
	return states
}

func mergeGrantLevels(target map[uint16]int, grants []Grant) {
	for _, grant := range grants {
		if grant.Level > target[grant.SkillID] {
			target[grant.SkillID] = grant.Level
		}
	}
}

func sortedGrants(levels map[uint16]int) []Grant {
	ids := make([]int, 0, len(levels))
	for id := range levels {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	out := make([]Grant, 0, len(ids))
	for _, id := range ids {
		out = append(out, Grant{SkillID: uint16(id), Level: levels[uint16(id)]})
	}
	return out
}

func cloneGrants(grants []Grant) []Grant {
	return append([]Grant(nil), grants...)
}
