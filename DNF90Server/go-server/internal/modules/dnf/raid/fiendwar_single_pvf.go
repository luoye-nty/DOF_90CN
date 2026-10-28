package raid

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const FiendWarSinglePVFPath = "etc/raid/fiendwar/fiendwar_single.etc"

// FiendWarSingleConfig is the server-owned overlay for a one-player Fiend War
// run. The ordinary [PHASE] graph and trigger rules remain the single source of
// truth; this document only selects one route, one final boss, a time limit,
// a monster-level ceiling, and an independent reward pool.
type FiendWarSingleConfig struct {
	TimeLimitSeconds int
	MonsterLevel     byte
	Routes           []FiendWarSingleRoute
	BossAreas        []int
}

type FiendWarSingleRoute struct {
	Name          string
	RequiredAreas []int
	OptionalAreas []int
	// BossGateAreas is the narrow condition that opens the selected final boss.
	// Route areas still control movement and which board cells are visible, but
	// the client's quantum portal only asks whether the command centers are
	// destroyed. Requiring every traversed ordinary room here made a cleared
	// command center leave the boss permanently sealed.
	BossGateAreas []int
}

func loadFiendWarSingle(source dnfpvf.Source, config *FiendWarConfig) (*FiendWarSingleConfig, error) {
	if source == nil || config == nil || config.Battle == nil || config.Battle.Raid == nil || len(config.Battle.Raid.Phases) != 1 {
		return nil, fmt.Errorf("fiendwar single base configuration missing")
	}
	text, present, err := fiendWarOptionalText(source, FiendWarSinglePVFPath)
	if err != nil || !present {
		return nil, err
	}
	document, err := dnfpvf.Parse(FiendWarSinglePVFPath, text)
	if err != nil {
		return nil, err
	}
	single, err := parseFiendWarSingleConfig(document, config)
	if err != nil {
		return nil, err
	}
	return single, nil
}

func fiendWarOptionalText(source dnfpvf.Source, path string) (string, bool, error) {
	text, err := source.ReadText(path)
	if err == nil {
		return text, true, nil
	}
	if errors.Is(err, dnfpvf.ErrDocNotFound) || strings.Contains(strings.ToLower(err.Error()), "file not found") {
		return "", false, nil
	}
	return "", false, err
}

func parseFiendWarSingleConfig(document *dnfpvf.Document, config *FiendWarConfig) (*FiendWarSingleConfig, error) {
	if document == nil || antonPathKey(document.Path) != antonPathKey(FiendWarSinglePVFPath) {
		return nil, fmt.Errorf("invalid fiendwar single PVF document")
	}
	enabled, err := antonReadPositiveIntSection(document, "enabled", 0, len(document.Sections))
	if err != nil || enabled != 1 {
		return nil, fmt.Errorf("fiendwar single enabled: %v", err)
	}
	phases, err := antonFindBlocks(document, "single raid phase", 0, len(document.Sections))
	if err != nil || len(phases) != 1 {
		return nil, fmt.Errorf("fiendwar single phase: %v", err)
	}
	phaseBlock := phases[0]
	if err := antonRequireEmptySection(document, phaseBlock.open); err != nil {
		return nil, err
	}
	if err := antonRequireEmptySection(document, phaseBlock.close); err != nil {
		return nil, err
	}
	nameIndex, err := antonUniqueSection(document, "phase name", phaseBlock.open+1, phaseBlock.close)
	if err != nil {
		return nil, err
	}
	nameTokens, err := antonSectionTokens(document, nameIndex)
	if err != nil || len(nameTokens) != 1 || nameTokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(nameTokens[0].Value) == "" {
		return nil, fmt.Errorf("fiendwar single phase name: %v", err)
	}
	timeLimit, err := antonReadPositiveIntSection(document, "single raid time over", phaseBlock.open+1, phaseBlock.close)
	if err != nil || timeLimit > 24*60*60 {
		return nil, fmt.Errorf("fiendwar single time over: %v", err)
	}
	minimumClear, err := antonReadPositiveIntSection(document, "minimal dungeon clear count", phaseBlock.open+1, phaseBlock.close)
	if err != nil {
		return nil, err
	}
	level, err := antonReadPositiveIntSection(document, "monster level", phaseBlock.open+1, phaseBlock.close)
	if err != nil || level > 200 {
		return nil, fmt.Errorf("fiendwar single monster level: %v", err)
	}
	rewardBlocks, err := antonFindBlocks(document, "clear reward item", phaseBlock.open+1, phaseBlock.close)
	if err != nil || len(rewardBlocks) != 1 {
		return nil, fmt.Errorf("fiendwar single rewards: %v", err)
	}
	rewards, err := antonParseRewards(document, rewardBlocks[0], 1)
	if err != nil || len(rewards) == 0 {
		return nil, fmt.Errorf("fiendwar single rewards: %v", err)
	}
	bossAreas, err := fiendWarSingleAreaList(document, "single raid boss area", phaseBlock.open+1, phaseBlock.close)
	if err != nil || len(bossAreas) != 2 {
		return nil, fmt.Errorf("fiendwar single boss areas: %v", err)
	}
	routeBlocks, err := antonFindBlocks(document, "single raid route", phaseBlock.open+1, phaseBlock.close)
	if err != nil || len(routeBlocks) != 3 {
		return nil, fmt.Errorf("fiendwar single routes: %v", err)
	}

	single := &FiendWarSingleConfig{
		TimeLimitSeconds: timeLimit,
		MonsterLevel:     byte(level),
		BossAreas:        bossAreas,
		Routes:           make([]FiendWarSingleRoute, 0, len(routeBlocks)),
	}
	seenNames := make(map[string]bool, len(routeBlocks))
	for _, block := range routeBlocks {
		route, parseErr := parseFiendWarSingleRoute(document, block)
		if parseErr != nil {
			return nil, parseErr
		}
		if seenNames[route.Name] {
			return nil, fmt.Errorf("fiendwar duplicate single route %q", route.Name)
		}
		seenNames[route.Name] = true
		single.Routes = append(single.Routes, route)
	}
	for _, area := range bossAreas {
		boss, ok := config.Battle.Areas[area]
		if !ok || !strings.HasSuffix(boss.Kind, "boss") {
			return nil, fmt.Errorf("fiendwar single boss area %d is not a boss", area)
		}
	}
	for i := range single.Routes {
		route := &single.Routes[i]
		if len(route.RequiredAreas) == 0 {
			return nil, fmt.Errorf("fiendwar single route %q has no required area", route.Name)
		}
		for _, area := range append(append([]int(nil), route.RequiredAreas...), route.OptionalAreas...) {
			if _, ok := config.Battle.Areas[area]; !ok {
				return nil, fmt.Errorf("fiendwar single route %q references unknown area %d", route.Name, area)
			}
		}
		if len(route.BossGateAreas) == 0 {
			for _, area := range route.RequiredAreas {
				if strings.HasSuffix(config.Battle.Areas[area].Kind, "command center") {
					route.BossGateAreas = append(route.BossGateAreas, area)
				}
			}
		}
		if len(route.BossGateAreas) != 1 {
			return nil, fmt.Errorf("fiendwar single route %q has %d boss gate areas, want 1", route.Name, len(route.BossGateAreas))
		}
		gate := route.BossGateAreas[0]
		if !strings.HasSuffix(config.Battle.Areas[gate].Kind, "command center") {
			return nil, fmt.Errorf("fiendwar single route %q boss gate area %d is not a command center", route.Name, gate)
		}
		if !slices.Contains(route.RequiredAreas, gate) && !slices.Contains(route.OptionalAreas, gate) {
			return nil, fmt.Errorf("fiendwar single route %q boss gate area %d is outside the route", route.Name, gate)
		}
	}

	phase := config.Battle.Raid.Phases[0]
	phase.Name = nameTokens[0].Value
	phase.MinimalDungeonClearCount = minimumClear
	phase.Rewards = rewards
	config.Battle.Raid.SingleStartMinimumMembers = 1
	config.Battle.Raid.SingleMode = &AntonSingleModeConfig{
		Name:             phase.Name,
		TimeLimitSeconds: timeLimit,
		Phase:            phase,
	}
	return single, nil
}

func parseFiendWarSingleRoute(document *dnfpvf.Document, block antonSectionBlock) (FiendWarSingleRoute, error) {
	nameIndex, err := antonUniqueSection(document, "name", block.open+1, block.close)
	if err != nil {
		return FiendWarSingleRoute{}, err
	}
	nameTokens, err := antonSectionTokens(document, nameIndex)
	if err != nil || len(nameTokens) != 1 || nameTokens[0].Kind != dnfpvf.TokenString || strings.TrimSpace(nameTokens[0].Value) == "" {
		return FiendWarSingleRoute{}, fmt.Errorf("fiendwar single route name: %v", err)
	}
	required, err := fiendWarSingleAreaList(document, "required area", block.open+1, block.close)
	if err != nil {
		return FiendWarSingleRoute{}, err
	}
	optional, err := fiendWarSingleAreaList(document, "optional area", block.open+1, block.close)
	if err != nil {
		return FiendWarSingleRoute{}, err
	}
	bossGate, err := fiendWarSingleAreaList(document, "boss gate area", block.open+1, block.close)
	if err != nil {
		return FiendWarSingleRoute{}, err
	}
	return FiendWarSingleRoute{
		Name:          strings.TrimSpace(nameTokens[0].Value),
		RequiredAreas: required,
		OptionalAreas: optional,
		BossGateAreas: bossGate,
	}, nil
}

func fiendWarSingleAreaList(document *dnfpvf.Document, name string, start, end int) ([]int, error) {
	index, found, err := antonOptionalUniqueSection(document, name, start, end)
	if err != nil || !found {
		return nil, err
	}
	tokens, err := antonSectionTokens(document, index)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(tokens))
	for _, token := range tokens {
		value, convErr := antonIntToken(token, name)
		if convErr != nil || value < 1 || value > 127 {
			return nil, fmt.Errorf("fiendwar single %s area: %v", name, convErr)
		}
		out = append(out, value)
	}
	return out, nil
}
