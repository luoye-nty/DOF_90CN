package worldmap

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

const tournamentRoundCount = 4

var (
	ErrTournamentDefinitionInvalid = errors.New("dnf tournament definition is invalid")
	ErrTournamentRandomInvalid     = errors.New("dnf tournament random source returned an invalid index")
	ErrTournamentSequenceUnbound   = errors.New("dnf tournament actor sequence is not bound")
)

const (
	tournamentCardTypeCount = 2
	tournamentCardsPerType  = 2
)

// TournamentReward is the frozen face of one Yellow Dragon reward card.
// ItemID==0 represents a gold/empty wire tuple; Empty disambiguates the
// zero-value empty card from a real gold card with a zero amount.
type TournamentReward struct {
	ItemID uint32
	Count  uint32
	Gold   bool
	Empty  bool
}

// TournamentRewardPlan mirrors the current client card topology: two cards
// in the upper group and, for champions only, two cards in the lower group.
// The plan is created once after the terminal actor death and then only moves
// through reservation/delivery states. It is intentionally independent of
// packet encoding and inventory persistence.
type TournamentRewardPlan struct {
	CompletedRounds byte
	Experience      uint32
	Champion        bool
	Upper           [tournamentCardsPerType]TournamentReward
	Lower           [tournamentCardsPerType]TournamentReward
	selected        [tournamentCardTypeCount]int8
	delivered       [tournamentCardTypeCount]bool
}

func (p *TournamentRewardPlan) initSelection() {
	if p == nil {
		return
	}
	for index := range p.selected {
		p.selected[index] = -1
	}
}

func (p TournamentRewardPlan) CardCount(side byte) byte {
	if side == 0 {
		return tournamentCardsPerType
	}
	if side == 1 && p.Champion {
		return tournamentCardsPerType
	}
	return 0
}

func (p TournamentRewardPlan) Card(side, index byte) (TournamentReward, bool) {
	if index >= tournamentCardsPerType || index >= p.CardCount(side) {
		return TournamentReward{}, false
	}
	if side == 0 {
		return p.Upper[index], true
	}
	return p.Lower[index], true
}

// SelectedCard reports the committed card for one visible reward group.
// A reserved but not yet delivered selection is intentionally not projected
// to the client as owned.
func (p TournamentRewardPlan) SelectedCard(side byte) (byte, bool) {
	if side >= tournamentCardTypeCount || !p.delivered[side] || p.selected[side] < 0 {
		return 0, false
	}
	index := byte(p.selected[side])
	if index >= p.CardCount(side) {
		return 0, false
	}
	return index, true
}

// ReserveSelection performs the S4A12-style two-phase selection transition.
// The boolean replay result is true only for an already delivered identical
// selection, allowing a retry to be acknowledged without a second grant.
func (p *TournamentRewardPlan) ReserveSelection(side, index byte) (TournamentReward, bool, bool) {
	if p == nil || index >= p.CardCount(side) || side >= tournamentCardTypeCount {
		return TournamentReward{}, false, false
	}
	if p.delivered[side] {
		// A retry is idempotent only when it repeats the exact card that was
		// committed. A different card index must be rejected instead of being
		// acknowledged with a mismatched reward row.
		if p.selected[side] != int8(index) {
			return TournamentReward{}, false, false
		}
		reward, ok := p.Card(side, index)
		return reward, false, ok
	}
	if p.selected[side] >= 0 {
		return TournamentReward{}, false, false
	}
	p.selected[side] = int8(index)
	reward, _ := p.Card(side, index)
	return reward, true, false
}

func (p *TournamentRewardPlan) CommitSelection(side, index byte) bool {
	if p == nil || side >= tournamentCardTypeCount || index >= p.CardCount(side) || p.selected[side] != int8(index) || p.delivered[side] {
		return false
	}
	p.delivered[side] = true
	return true
}

func (p *TournamentRewardPlan) RollbackSelection(side, index byte) bool {
	if p == nil || side >= tournamentCardTypeCount || index >= p.CardCount(side) || p.selected[side] != int8(index) || p.delivered[side] {
		return false
	}
	p.selected[side] = -1
	return true
}

func (p TournamentRewardPlan) IsSelectionComplete() bool {
	if p.selected[0] < 0 || !p.delivered[0] {
		return false
	}
	return !p.Champion || (p.selected[1] >= 0 && p.delivered[1])
}

func (p TournamentRewardPlan) Cardless() bool {
	for side := byte(0); side < tournamentCardTypeCount; side++ {
		for index := byte(0); index < p.CardCount(side); index++ {
			reward, ok := p.Card(side, index)
			if !ok || !reward.Empty {
				return false
			}
		}
	}
	return true
}

func (p *TournamentRewardPlan) commitCardless() bool {
	if p == nil || !p.Cardless() {
		return false
	}
	if p.IsSelectionComplete() {
		return true
	}
	for side := byte(0); side < tournamentCardTypeCount; side++ {
		if p.CardCount(side) == 0 {
			continue
		}
		p.selected[side] = 0
		p.delivered[side] = true
	}
	return p.IsSelectionComplete()
}

type TournamentActorKind byte

const (
	TournamentActorMonster TournamentActorKind = iota
	TournamentActorAICharacter
)

type TournamentActor struct {
	Kind     TournamentActorKind `json:"kind"`
	Code     uint32              `json:"code"`
	Strength uint16              `json:"strength"`
	Name     string              `json:"name"`
	Level    byte                `json:"level"`
	Type     byte                `json:"type"`
}

type TournamentTeam struct {
	Position byte              `json:"position"`
	Members  []TournamentActor `json:"members"`
	Player   bool              `json:"player"`
}

type TournamentRound struct {
	Number byte             `json:"number"`
	Teams  []TournamentTeam `json:"teams"`
}

type TournamentDefinition struct {
	DungeonID       uint32
	MapID           uint32
	DifficultyLevel byte
	PartyCount      byte
	RoundFatigue    int64
	GoldRate        int64
	Experience      map[byte]uint32
	ResultCards     map[byte]TournamentResultCard
	RewardItems     []TournamentRewardItemRate
	Candidates      []TournamentActor
	StartAreas      []TournamentStartArea
}

type TournamentActorProjector func(candidate TournamentEnemyCandidate, basisLevel byte) (level byte, actorType byte, err error)

func (t *Table) TournamentDefinition(dungeonID, mapID int64, partyCount int, projector TournamentActorProjector) (TournamentDefinition, error) {
	dungeon, ok := t.FindDungeon(dungeonID)
	if !ok {
		return TournamentDefinition{}, fmt.Errorf("%w: dungeon=%d is missing", ErrTournamentDefinitionInvalid, dungeonID)
	}
	tournamentMap, ok := t.FindMap(mapID)
	if !ok {
		return TournamentDefinition{}, fmt.Errorf("%w: map=%d is missing", ErrTournamentDefinitionInvalid, mapID)
	}
	return buildTournamentDefinition(dungeon, tournamentMap, partyCount, projector)
}

func buildTournamentDefinition(dungeon Dungeon, tournamentMap Map, partyCount int, projector TournamentActorProjector) (TournamentDefinition, error) {
	if dungeon.Metadata.Flags == nil || !dungeon.Metadata.Flags["tournament dungeon"] {
		return TournamentDefinition{}, fmt.Errorf("%w: dungeon=%d is not marked tournament", ErrTournamentDefinitionInvalid, dungeon.ID)
	}
	if dungeon.ID <= 0 || dungeon.ID > int64(^uint32(0)) || tournamentMap.ID <= 0 || tournamentMap.ID > int64(^uint32(0)) {
		return TournamentDefinition{}, fmt.Errorf("%w: dungeon=%d map=%d exceeds wire range", ErrTournamentDefinitionInvalid, dungeon.ID, tournamentMap.ID)
	}
	if !tournamentMap.DungeonID.Set || tournamentMap.DungeonID.Value != dungeon.ID {
		return TournamentDefinition{}, fmt.Errorf("%w: map=%d belongs to dungeon=%d", ErrTournamentDefinitionInvalid, tournamentMap.ID, tournamentMap.DungeonID.Value)
	}
	if tournamentMap.TournamentMalformed || partyCount < 1 || partyCount > 2 {
		return TournamentDefinition{}, fmt.Errorf("%w: party_count=%d malformed_map=%t", ErrTournamentDefinitionInvalid, partyCount, tournamentMap.TournamentMalformed)
	}
	if !dungeon.Metadata.LimitPartyCount.Set || dungeon.Metadata.LimitPartyCount.Value != int64(partyCount) {
		return TournamentDefinition{}, fmt.Errorf("%w: party_count=%d limit=%d", ErrTournamentDefinitionInvalid, partyCount, dungeon.Metadata.LimitPartyCount.Value)
	}
	if !dungeon.Metadata.BasisLevel.Set || dungeon.Metadata.BasisLevel.Value < 1 || dungeon.Metadata.BasisLevel.Value > 0xff {
		return TournamentDefinition{}, fmt.Errorf("%w: basis_level=%d", ErrTournamentDefinitionInvalid, dungeon.Metadata.BasisLevel.Value)
	}
	basisLevel := byte(dungeon.Metadata.BasisLevel.Value)
	definition := TournamentDefinition{
		DungeonID: uint32(dungeon.ID), MapID: uint32(tournamentMap.ID), PartyCount: byte(partyCount),
		DifficultyLevel: basisLevel, RoundFatigue: dungeon.Metadata.TournamentRoundFatigue,
		GoldRate: dungeon.Metadata.TournamentGoldRate, Experience: make(map[byte]uint32, 5),
		ResultCards: make(map[byte]TournamentResultCard, 6),
		RewardItems: append([]TournamentRewardItemRate(nil), dungeon.Metadata.TournamentRewardItemRates...),
		StartAreas:  append([]TournamentStartArea(nil), tournamentMap.TournamentStartAreas...),
	}
	for _, row := range dungeon.Metadata.TournamentClearRewardExp {
		if row.CompletedRounds < 0 || row.CompletedRounds > tournamentRoundCount || row.Experience < 0 || row.Experience > int64(^uint32(0)) {
			return TournamentDefinition{}, fmt.Errorf("%w: reward experience row=%+v", ErrTournamentDefinitionInvalid, row)
		}
		key := byte(row.CompletedRounds)
		if _, exists := definition.Experience[key]; exists {
			return TournamentDefinition{}, fmt.Errorf("%w: duplicate reward experience round=%d", ErrTournamentDefinitionInvalid, key)
		}
		definition.Experience[key] = uint32(row.Experience)
	}
	for round := byte(0); round <= tournamentRoundCount; round++ {
		if _, ok := definition.Experience[round]; !ok {
			return TournamentDefinition{}, fmt.Errorf("%w: reward experience round=%d is missing", ErrTournamentDefinitionInvalid, round)
		}
	}
	for _, card := range dungeon.Metadata.TournamentResultCards {
		if card.ResultKey < 0 || card.ResultKey > 5 || card.GoldWeight < 0 || card.ItemWeight < 0 || card.EmptyWeight < 0 || card.GoldWeight+card.ItemWeight+card.EmptyWeight <= 0 {
			return TournamentDefinition{}, fmt.Errorf("%w: result card=%+v", ErrTournamentDefinitionInvalid, card)
		}
		key := byte(card.ResultKey)
		if _, exists := definition.ResultCards[key]; exists {
			return TournamentDefinition{}, fmt.Errorf("%w: duplicate result card=%d", ErrTournamentDefinitionInvalid, key)
		}
		definition.ResultCards[key] = card
	}
	for key := byte(0); key <= 5; key++ {
		if _, ok := definition.ResultCards[key]; !ok {
			return TournamentDefinition{}, fmt.Errorf("%w: result card=%d is missing", ErrTournamentDefinitionInvalid, key)
		}
	}
	if len(definition.RewardItems) == 0 || definition.RoundFatigue < 0 || definition.GoldRate <= 0 {
		return TournamentDefinition{}, fmt.Errorf("%w: reward metadata is incomplete", ErrTournamentDefinitionInvalid)
	}
	for _, reward := range definition.RewardItems {
		if reward.ItemID <= 0 || reward.Weight <= 0 || reward.Count <= 0 {
			return TournamentDefinition{}, fmt.Errorf("%w: reward item=%+v", ErrTournamentDefinitionInvalid, reward)
		}
	}
	for _, candidate := range tournamentMap.TournamentEnemies {
		if candidate.PartyCount != int64(partyCount) {
			continue
		}
		if candidate.Code <= 0 || candidate.Code > int64(^uint32(0)) || candidate.Strength < 0 || candidate.Strength > 0xffff || strings.TrimSpace(candidate.Name) == "" {
			return TournamentDefinition{}, fmt.Errorf("%w: actor=%+v", ErrTournamentDefinitionInvalid, candidate)
		}
		actor := TournamentActor{Code: uint32(candidate.Code), Strength: uint16(candidate.Strength), Name: candidate.Name, Level: basisLevel}
		switch strings.ToLower(strings.TrimSpace(candidate.Kind)) {
		case "[monster]":
			actor.Kind = TournamentActorMonster
		case "[apc]":
			if projector == nil {
				return TournamentDefinition{}, fmt.Errorf("%w: APC code=%d requires a projector", ErrTournamentDefinitionInvalid, candidate.Code)
			}
			level, actorType, err := projector(candidate, basisLevel)
			if err != nil {
				return TournamentDefinition{}, fmt.Errorf("%w: project APC code=%d: %v", ErrTournamentDefinitionInvalid, candidate.Code, err)
			}
			actor.Kind, actor.Level, actor.Type = TournamentActorAICharacter, level, actorType
		default:
			return TournamentDefinition{}, fmt.Errorf("%w: actor kind=%q", ErrTournamentDefinitionInvalid, candidate.Kind)
		}
		definition.Candidates = append(definition.Candidates, actor)
	}
	if len(definition.Candidates) < 15*partyCount {
		return TournamentDefinition{}, fmt.Errorf("%w: candidates=%d required=%d", ErrTournamentDefinitionInvalid, len(definition.Candidates), 15*partyCount)
	}
	startCount := 0
	for _, area := range definition.StartAreas {
		if area.PartyCount == int64(partyCount) {
			startCount++
		}
	}
	if startCount < 2 {
		return TournamentDefinition{}, fmt.Errorf("%w: start areas=%d required=2", ErrTournamentDefinitionInvalid, startCount)
	}
	return definition, nil
}

type TournamentRandom func(exclusiveMaximum int) (int, error)

type TournamentTerminalResult byte

const (
	TournamentActive TournamentTerminalResult = iota
	TournamentChampion
	TournamentEliminated
)

type TournamentDeathDisposition byte

const (
	TournamentDeathRejected TournamentDeathDisposition = iota
	TournamentDeathDuplicate
	TournamentDeathAccepted
	TournamentRoundCompleted
	TournamentCompleted
)

type TournamentDeathTransition struct {
	Disposition    TournamentDeathDisposition
	CompletedRound byte
	CurrentRound   byte
}

type TournamentRuntime struct {
	mu sync.Mutex

	Definition TournamentDefinition
	Rounds     []TournamentRound
	PathActors []TournamentActor

	firstSequence     uint16
	currentRound      byte
	completedRounds   byte
	terminal          TournamentTerminalResult
	acceptedEvents    map[string]uint16
	acceptedSequences map[uint16]struct{}
	rewardPlan        *TournamentRewardPlan
}

// PrepareRewardPlan freezes the PVF-selected card faces exactly once after a
// tournament becomes terminal. The caller supplies the already-authoritative
// gold/experience amounts because those values belong to the bridge's reward
// and persistence layers, not to the world-map parser.
func (r *TournamentRuntime) PrepareRewardPlan(experience, gold uint32, random TournamentRandom) (TournamentRewardPlan, bool, error) {
	if r == nil || random == nil {
		return TournamentRewardPlan{}, false, fmt.Errorf("%w: reward random source is missing", ErrTournamentDefinitionInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal == TournamentActive {
		return TournamentRewardPlan{}, false, fmt.Errorf("%w: tournament is not terminal", ErrTournamentDefinitionInvalid)
	}
	if r.rewardPlan != nil {
		return *r.rewardPlan, false, nil
	}
	plan := TournamentRewardPlan{
		CompletedRounds: r.completedRounds,
		Experience:      experience,
		Champion:        r.terminal == TournamentChampion,
	}
	plan.initSelection()
	upperKey := r.completedRounds
	for index := byte(0); index < tournamentCardsPerType; index++ {
		reward, err := tournamentRollReward(r.Definition, upperKey, gold, random)
		if err != nil {
			return TournamentRewardPlan{}, false, err
		}
		plan.Upper[index] = reward
	}
	if plan.Champion {
		for index := byte(0); index < tournamentCardsPerType; index++ {
			reward, err := tournamentRollReward(r.Definition, 5, gold, random)
			if err != nil {
				return TournamentRewardPlan{}, false, err
			}
			plan.Lower[index] = reward
		}
	}
	r.rewardPlan = &plan
	return plan, true, nil
}

func (r *TournamentRuntime) RewardPlanSnapshot() (TournamentRewardPlan, bool) {
	if r == nil {
		return TournamentRewardPlan{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rewardPlan == nil {
		return TournamentRewardPlan{}, false
	}
	return *r.rewardPlan, true
}

func (r *TournamentRuntime) ReserveRewardSelection(side, index byte) (TournamentReward, bool, bool) {
	if r == nil {
		return TournamentReward{}, false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rewardPlan == nil {
		return TournamentReward{}, false, false
	}
	return r.rewardPlan.ReserveSelection(side, index)
}

func (r *TournamentRuntime) CommitRewardSelection(side, index byte) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rewardPlan != nil && r.rewardPlan.CommitSelection(side, index)
}

func (r *TournamentRuntime) RollbackRewardSelection(side, index byte) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rewardPlan != nil && r.rewardPlan.RollbackSelection(side, index)
}

func (r *TournamentRuntime) RewardSelectionComplete() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rewardPlan != nil && r.rewardPlan.IsSelectionComplete()
}

// CommitCardlessRewardPlan closes a PVF reward plan whose visible faces are
// all empty. The current client represents this as zero card groups in op388,
// so there is no op458 request that could otherwise cross the delivery gate.
func (r *TournamentRuntime) CommitCardlessRewardPlan() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rewardPlan != nil && r.rewardPlan.commitCardless()
}

// RewardSideSelectionComplete reports whether one visible card group has
// already crossed the durable delivery boundary. It is used by timeout
// coordinators to fill only client selections that never arrived.
func (r *TournamentRuntime) RewardSideSelectionComplete(side byte) bool {
	if r == nil || side >= tournamentCardTypeCount {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rewardPlan != nil && r.rewardPlan.delivered[side]
}

func tournamentRollReward(definition TournamentDefinition, resultKey byte, gold uint32, random TournamentRandom) (TournamentReward, error) {
	card, ok := definition.ResultCards[resultKey]
	if !ok {
		return TournamentReward{}, fmt.Errorf("%w: reward result key=%d is missing", ErrTournamentDefinitionInvalid, resultKey)
	}
	total := card.GoldWeight + card.ItemWeight + card.EmptyWeight
	if total <= 0 {
		return TournamentReward{}, fmt.Errorf("%w: reward result key=%d has no weight", ErrTournamentDefinitionInvalid, resultKey)
	}
	roll, err := tournamentRandomIndex(random, int(total))
	if err != nil {
		return TournamentReward{}, err
	}
	if int64(roll) < card.GoldWeight {
		return TournamentReward{Gold: true, Count: gold}, nil
	}
	if int64(roll) < card.GoldWeight+card.ItemWeight {
		reward, err := tournamentRollRewardItem(definition.RewardItems, random)
		if err != nil {
			return TournamentReward{}, err
		}
		return reward, nil
	}
	return TournamentReward{Empty: true}, nil
}

func tournamentRollRewardItem(items []TournamentRewardItemRate, random TournamentRandom) (TournamentReward, error) {
	var total int64
	for _, item := range items {
		if item.ItemID <= 0 || item.Weight <= 0 || item.Count <= 0 {
			continue
		}
		total += item.Weight
	}
	if total <= 0 || total > int64(^uint(0)>>1) {
		return TournamentReward{}, fmt.Errorf("%w: reward item rates are empty", ErrTournamentDefinitionInvalid)
	}
	roll, err := tournamentRandomIndex(random, int(total))
	if err != nil {
		return TournamentReward{}, err
	}
	var cumulative int64
	for _, item := range items {
		if item.ItemID <= 0 || item.Weight <= 0 || item.Count <= 0 {
			continue
		}
		cumulative += item.Weight
		if int64(roll) < cumulative {
			return TournamentReward{ItemID: uint32(item.ItemID), Count: uint32(item.Count)}, nil
		}
	}
	return TournamentReward{}, fmt.Errorf("%w: reward item roll did not resolve", ErrTournamentDefinitionInvalid)
}

func NewTournamentRuntime(definition TournamentDefinition, random TournamentRandom) (*TournamentRuntime, error) {
	if random == nil {
		return nil, fmt.Errorf("%w: random source is nil", ErrTournamentDefinitionInvalid)
	}
	partyCount := int(definition.PartyCount)
	if partyCount < 1 || partyCount > 2 || len(definition.Candidates) < 15*partyCount {
		return nil, fmt.Errorf("%w: party=%d candidates=%d", ErrTournamentDefinitionInvalid, partyCount, len(definition.Candidates))
	}
	candidates := append([]TournamentActor(nil), definition.Candidates...)
	if err := shuffleTournamentActors(candidates, random); err != nil {
		return nil, err
	}
	npcTeams := make([]TournamentTeam, 15)
	for index := range npcTeams {
		members := append([]TournamentActor(nil), candidates[index*partyCount:(index+1)*partyCount]...)
		npcTeams[index] = TournamentTeam{Members: members}
	}
	for i := 1; i < len(npcTeams); i++ {
		for j := i; j > 0 && tournamentTeamStrength(npcTeams[j]) < tournamentTeamStrength(npcTeams[j-1]); j-- {
			npcTeams[j], npcTeams[j-1] = npcTeams[j-1], npcTeams[j]
		}
	}
	playerPosition, err := tournamentRandomIndex(random, 16)
	if err != nil {
		return nil, err
	}
	opponentPosition := playerPosition ^ 1
	firstRound := make([]TournamentTeam, 16)
	firstRound[playerPosition] = TournamentTeam{Position: byte(playerPosition), Members: make([]TournamentActor, partyCount), Player: true}
	firstRound[opponentPosition] = tournamentTeamAt(npcTeams[0], opponentPosition)
	remaining := append([]TournamentTeam(nil), npcTeams[1:]...)
	if err := shuffleTournamentTeams(remaining, random); err != nil {
		return nil, err
	}
	remainingIndex := 0
	for position := range firstRound {
		if position == playerPosition || position == opponentPosition {
			continue
		}
		firstRound[position] = tournamentTeamAt(remaining[remainingIndex], position)
		remainingIndex++
	}
	rounds := []TournamentRound{{Number: 1, Teams: firstRound}}
	previous := firstRound
	for round := byte(2); round <= tournamentRoundCount; round++ {
		nextRound := make([]TournamentTeam, len(previous)/2)
		for match := range nextRound {
			left, right := previous[match*2], previous[match*2+1]
			winner := left
			if !left.Player && right.Player {
				winner = right
			} else if !left.Player && !right.Player {
				leftStrength, rightStrength := tournamentTeamStrength(left), tournamentTeamStrength(right)
				if rightStrength > leftStrength {
					winner = right
				} else if rightStrength == leftStrength {
					pick, pickErr := tournamentRandomIndex(random, 2)
					if pickErr != nil {
						return nil, pickErr
					}
					if pick == 1 {
						winner = right
					}
				}
			}
			nextRound[match] = tournamentTeamAt(winner, match)
		}
		rounds = append(rounds, TournamentRound{Number: round, Teams: nextRound})
		previous = nextRound
	}
	pathActors := make([]TournamentActor, 0, tournamentRoundCount*partyCount)
	for _, round := range rounds {
		playerIndex := -1
		for index, team := range round.Teams {
			if team.Player {
				playerIndex = index
				break
			}
		}
		if playerIndex < 0 {
			return nil, fmt.Errorf("%w: round=%d has no player", ErrTournamentDefinitionInvalid, round.Number)
		}
		for _, actor := range round.Teams[playerIndex^1].Members {
			if round.Number == tournamentRoundCount {
				if actor.Kind == TournamentActorAICharacter {
					actor.Type = 8
				} else {
					actor.Type = 3
				}
			}
			pathActors = append(pathActors, actor)
		}
	}
	return &TournamentRuntime{
		Definition: definition, Rounds: rounds, PathActors: pathActors, currentRound: 1,
		acceptedEvents: make(map[string]uint16), acceptedSequences: make(map[uint16]struct{}),
	}, nil
}

func (r *TournamentRuntime) BindFirstActorSequence(sequence uint16) bool {
	if r == nil || sequence == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if int(sequence)+len(r.PathActors)-1 > int(^uint16(0)) {
		return false
	}
	if r.firstSequence == 0 {
		r.firstSequence = sequence
	}
	return r.firstSequence == sequence
}

func (r *TournamentRuntime) FirstActorSequence() uint16 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstSequence
}

func (r *TournamentRuntime) ActorForSequence(sequence uint16) (TournamentActor, bool) {
	if r == nil || sequence == 0 {
		return TournamentActor{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstSequence == 0 || sequence < r.firstSequence {
		return TournamentActor{}, false
	}
	index := int(sequence - r.firstSequence)
	if index < 0 || index >= len(r.PathActors) {
		return TournamentActor{}, false
	}
	return r.PathActors[index], true
}

func (r *TournamentRuntime) Snapshot() (currentRound, completedRounds byte, terminal TournamentTerminalResult) {
	if r == nil {
		return 0, 0, TournamentEliminated
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentRound, r.completedRounds, r.terminal
}

func (r *TournamentRuntime) ApplyActorDeath(eventID string, sequence uint16) TournamentDeathTransition {
	if r == nil || strings.TrimSpace(eventID) == "" {
		return TournamentDeathTransition{Disposition: TournamentDeathRejected}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if accepted, ok := r.acceptedEvents[eventID]; ok {
		disposition := TournamentDeathRejected
		if accepted == sequence {
			disposition = TournamentDeathDuplicate
		}
		return TournamentDeathTransition{Disposition: disposition, CurrentRound: r.currentRound}
	}
	if _, ok := r.acceptedSequences[sequence]; ok {
		return TournamentDeathTransition{Disposition: TournamentDeathDuplicate, CurrentRound: r.currentRound}
	}
	if r.firstSequence == 0 || r.terminal != TournamentActive {
		return TournamentDeathTransition{Disposition: TournamentDeathRejected, CurrentRound: r.currentRound}
	}
	first := int(r.firstSequence) + (int(r.currentRound)-1)*int(r.Definition.PartyCount)
	end := first + int(r.Definition.PartyCount)
	if int(sequence) < first || int(sequence) >= end {
		return TournamentDeathTransition{Disposition: TournamentDeathRejected, CurrentRound: r.currentRound}
	}
	r.acceptedEvents[eventID] = sequence
	r.acceptedSequences[sequence] = struct{}{}
	for current := first; current < end; current++ {
		if _, ok := r.acceptedSequences[uint16(current)]; !ok {
			return TournamentDeathTransition{Disposition: TournamentDeathAccepted, CurrentRound: r.currentRound}
		}
	}
	completed := r.currentRound
	r.completedRounds = completed
	if completed == tournamentRoundCount {
		r.terminal = TournamentChampion
		return TournamentDeathTransition{Disposition: TournamentCompleted, CompletedRound: completed, CurrentRound: completed}
	}
	r.currentRound++
	return TournamentDeathTransition{Disposition: TournamentRoundCompleted, CompletedRound: completed, CurrentRound: r.currentRound}
}

func (r *TournamentRuntime) Eliminate() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminal != TournamentActive {
		return false
	}
	r.completedRounds = r.currentRound - 1
	r.terminal = TournamentEliminated
	return true
}

func tournamentTeamAt(team TournamentTeam, position int) TournamentTeam {
	return TournamentTeam{Position: byte(position), Members: append([]TournamentActor(nil), team.Members...), Player: team.Player}
}

func tournamentTeamStrength(team TournamentTeam) uint64 {
	var total uint64
	for _, actor := range team.Members {
		total += uint64(actor.Strength)
	}
	return total
}

func tournamentRandomIndex(random TournamentRandom, maximum int) (int, error) {
	value, err := random(maximum)
	if err != nil {
		return 0, err
	}
	if value < 0 || value >= maximum {
		return 0, fmt.Errorf("%w: value=%d maximum=%d", ErrTournamentRandomInvalid, value, maximum)
	}
	return value, nil
}

func shuffleTournamentActors(values []TournamentActor, random TournamentRandom) error {
	for index := len(values) - 1; index > 0; index-- {
		swap, err := tournamentRandomIndex(random, index+1)
		if err != nil {
			return err
		}
		values[index], values[swap] = values[swap], values[index]
	}
	return nil
}

func shuffleTournamentTeams(values []TournamentTeam, random TournamentRandom) error {
	for index := len(values) - 1; index > 0; index-- {
		swap, err := tournamentRandomIndex(random, index+1)
		if err != nil {
			return err
		}
		values[index], values[swap] = values[swap], values[index]
	}
	return nil
}
