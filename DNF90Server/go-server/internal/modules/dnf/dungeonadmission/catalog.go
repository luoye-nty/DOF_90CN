package dungeonadmission

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const (
	CrackOfDimensionPath                    = "etc/crackofdimensionlist.etc"
	DimensionDungeonInfoPath                = "etc/dungeonetc/dimensiondungeoninfo.etc"
	LicenseDungeonInfoPath                  = "etc/dungeonetc/licensedungeoninfo.etc"
	DimensionGatePath                       = "etc/dimensiongate.etc"
	WeeklyDungeonPath                       = "etc/weeklydungeon.etc"
	AccountWeeklyDungeonPath                = "etc/accountcharacdungeonincountchn.etc"
	TowerOfDespairPath                      = "etc/towerofdespair.etc"
	TowerOfDespairInvitationPath            = "stackable/invitation_drapyus2.stk"
	SAOTowerOfDespairPath                   = "etc/sao_towerofdespair.etc"
	TowerOfGriefPath                        = "etc/towerofgrief.etc"
	BeastDungeonPath                        = "etc/beastdungeon/beastdungeon.etc"
	AntonRaidPath                           = "etc/raid/anton.etc"
	LukeRaidPath                            = "etc/raid/luke/luke.etc"
	FiendWarRaidPath                        = "etc/raid/fiendwar/fiendwar.etc"
	DungeonListPath                         = "dungeon/dungeon.lst"
	AccountCharacterDungeonAccountPolicyKey = "account_character_dungeon.account.daily"
)

// Raid weekly admission policy keys, one pair (account + character) per raid.
//
// These are raid-START limits: like the raid daily keys they carry no
// DungeonIDs, so they are looked up by semantic key and are never applied once
// per internal raid dungeon. dnfbridge charges them at start
// (raid_admission_counter.go) and renders them in the team-channel window
// (raid_daily_availability.go).
const (
	AntonWeeklyAccountPolicyKey   = "raid.anton.account.weekly"
	AntonWeeklyCharacterPolicyKey = "raid.anton.character.weekly"
	LukeWeeklyAccountPolicyKey    = "raid.luke.account.weekly"
	// LukeWeeklyCharacterPolicyKey owns Luke's per-character weekly entry count.
	// Kept as a named constant because dnfbridge's per-gate observer
	// (raid_luke_weekly_observation.go) still quotes it; it is now charged and
	// rendered like every other raid weekly policy.
	LukeWeeklyCharacterPolicyKey     = "raid.luke.character.weekly"
	FiendWarWeeklyAccountPolicyKey   = "raid.fiend_war.account.weekly"
	FiendWarWeeklyCharacterPolicyKey = "raid.fiend_war.character.weekly"
)

var (
	ErrCatalogSourceRequired = errors.New("dungeon admission catalog source is required")
	ErrCatalogInvalid        = errors.New("dungeon admission catalog is invalid")
)

// Evidence records a structured limit found in the active PVF. A zero Period
// means the available source set does not prove a daily/weekly reset and is
// deliberately not converted into an enforceable Policy. This prevents values
// such as `[recently enter dungeon count] 3` from being guessed as limits; the
// two tower families become daily only because their companion recommendation
// actions carry service-window use counters for those exact entry pools.
type Evidence struct {
	Key           string
	SourcePath    string
	SourceSection string
	Scope         Scope
	Period        Period
	Limit         uint32
	ResetWeekday  time.Weekday
	DungeonIDs    []uint32
	Enforced      bool
	Reason        string
	RawValues     []int64
}

// Catalog is the immutable active-PVF admission-limit view. Policies without
// DungeonIDs (currently raid-start limits) are looked up by semantic key and
// must not be applied once per internal raid dungeon.
type Catalog struct {
	policies                              map[string]Policy
	byDungeon                             map[uint32][]string
	evidence                              []Evidence
	towerRoute                            map[TowerFamily]TowerRoute
	accountCharacterDungeonBaseLimit      uint32
	accountCharacterDungeonAdventureBonus []accountCharacterDungeonAdventureBonus
}

type accountCharacterDungeonAdventureBonus struct {
	minimumLevel uint32
	maximumLevel uint32
	amount       uint32
}

func LoadCatalog(source dnfpvf.Source) (*Catalog, error) {
	if source == nil {
		return nil, ErrCatalogSourceRequired
	}
	documents := make(map[string]*dnfpvf.Document)
	for _, sourcePath := range []string{
		CrackOfDimensionPath,
		DimensionDungeonInfoPath,
		LicenseDungeonInfoPath,
		DimensionGatePath,
		WeeklyDungeonPath,
		AccountWeeklyDungeonPath,
		TowerOfDespairPath,
		TowerOfDespairInvitationPath,
		SAOTowerOfDespairPath,
		TowerOfGriefPath,
		BeastDungeonPath,
		AntonRaidPath,
		LukeRaidPath,
		FiendWarRaidPath,
		DungeonListPath,
	} {
		text, err := source.ReadText(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrCatalogInvalid, sourcePath, err)
		}
		document, err := dnfpvf.Parse(sourcePath, text)
		if err != nil {
			return nil, fmt.Errorf("%w: parse %s: %v", ErrCatalogInvalid, sourcePath, err)
		}
		documents[sourcePath] = document
	}

	catalog := &Catalog{
		policies:   make(map[string]Policy),
		byDungeon:  make(map[uint32][]string),
		towerRoute: make(map[TowerFamily]TowerRoute),
	}
	if err := catalog.loadExplicitDaily(documents); err != nil {
		return nil, err
	}
	if err := catalog.loadExplicitWeekly(documents); err != nil {
		return nil, err
	}
	if err := catalog.loadUnprovedPeriodEvidence(documents); err != nil {
		return nil, err
	}
	catalog.sort()
	return catalog, nil
}

// AccountCharacterDungeonDailyLimit applies the adventure-group tiers from
// etc/accountcharacdungeonincountchn.etc to its account-wide daily base.
func (c *Catalog) AccountCharacterDungeonDailyLimit(manageLevel uint32) uint32 {
	if c == nil {
		return 0
	}
	for _, bonus := range c.accountCharacterDungeonAdventureBonus {
		if manageLevel >= bonus.minimumLevel && manageLevel <= bonus.maximumLevel {
			return c.accountCharacterDungeonBaseLimit + bonus.amount
		}
	}
	return c.accountCharacterDungeonBaseLimit
}

func (c *Catalog) PoliciesForDungeon(dungeonID uint32) []Policy {
	if c == nil || dungeonID == 0 {
		return nil
	}
	keys := c.byDungeon[dungeonID]
	out := make([]Policy, 0, len(keys))
	for _, key := range keys {
		if policy, ok := c.policies[key]; ok {
			out = append(out, clonePolicy(policy))
		}
	}
	return out
}

// TowerRoute returns a detached copy of the PVF-authored ordered layer map.
func (c *Catalog) TowerRoute(family TowerFamily) (TowerRoute, bool) {
	if c == nil {
		return TowerRoute{}, false
	}
	route, found := c.towerRoute[family]
	if !found {
		return TowerRoute{}, false
	}
	route.DungeonIDs = append([]uint32(nil), route.DungeonIDs...)
	route.UseAllowItems = append([]TowerUseItem(nil), route.UseAllowItems...)
	return route, true
}

func (c *Catalog) Policy(key string) (Policy, bool) {
	if c == nil {
		return Policy{}, false
	}
	policy, ok := c.policies[strings.TrimSpace(key)]
	return clonePolicy(policy), ok
}

func (c *Catalog) Policies() []Policy {
	if c == nil {
		return nil
	}
	keys := make([]string, 0, len(c.policies))
	for key := range c.policies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Policy, 0, len(keys))
	for _, key := range keys {
		out = append(out, clonePolicy(c.policies[key]))
	}
	return out
}

func (c *Catalog) Evidence() []Evidence {
	if c == nil {
		return nil
	}
	out := make([]Evidence, len(c.evidence))
	for index, evidence := range c.evidence {
		evidence.DungeonIDs = append([]uint32(nil), evidence.DungeonIDs...)
		evidence.RawValues = append([]int64(nil), evidence.RawValues...)
		out[index] = evidence
	}
	return out
}

func (c *Catalog) EvidenceForDungeon(dungeonID uint32) []Evidence {
	if c == nil || dungeonID == 0 {
		return nil
	}
	out := make([]Evidence, 0)
	for _, evidence := range c.evidence {
		for _, candidate := range evidence.DungeonIDs {
			if candidate == dungeonID {
				evidence.DungeonIDs = append([]uint32(nil), evidence.DungeonIDs...)
				evidence.RawValues = append([]int64(nil), evidence.RawValues...)
				out = append(out, evidence)
				break
			}
		}
	}
	return out
}

func (c *Catalog) loadExplicitDaily(documents map[string]*dnfpvf.Document) error {
	crackID, err := crackOfDimensionDungeonID(documents[DungeonListPath])
	if err != nil {
		return err
	}
	if err := c.addSectionPolicy(documents[CrackOfDimensionPath], Policy{
		Key: "crack_of_dimension.character.daily", SourcePath: CrackOfDimensionPath, SourceSection: "daily enter count",
		Scope: ScopeCharacter, Period: PeriodDaily, DungeonIDs: []uint32{crackID},
	}, "daily enter count"); err != nil {
		return err
	}

	dimensionIDs, err := tupleDungeonIDs(documents[DimensionDungeonInfoPath], "dimension dungeon info", 2, 1)
	if err != nil {
		return err
	}
	if err := c.addSectionPolicy(documents[DimensionDungeonInfoPath], Policy{
		Key: "dimension_dungeon.character.daily", SourcePath: DimensionDungeonInfoPath, SourceSection: "max daily enter count",
		Scope: ScopeCharacter, Period: PeriodDaily, DungeonIDs: dimensionIDs,
	}, "max daily enter count"); err != nil {
		return err
	}

	licenseIDs, err := tupleDungeonIDs(documents[LicenseDungeonInfoPath], "dungeon license info", 5, 1)
	if err != nil {
		return err
	}
	if err := c.addSectionPolicy(documents[LicenseDungeonInfoPath], Policy{
		Key: "license_dungeon.character.daily", SourcePath: LicenseDungeonInfoPath, SourceSection: "daily enter count",
		Scope: ScopeCharacter, Period: PeriodDaily, DungeonIDs: licenseIDs,
	}, "daily enter count"); err != nil {
		return err
	}

	dimensionGateIDs, err := tupleDungeonIDs(documents[DimensionGatePath], "dimensiondungeon list", 3, 0)
	if err != nil {
		return err
	}
	if err := c.addSectionPolicy(documents[DimensionGatePath], Policy{
		Key: "dimension_gate.character.daily", SourcePath: DimensionGatePath, SourceSection: "daily default enter count",
		Scope: ScopeCharacter, Period: PeriodDaily, DungeonIDs: dimensionGateIDs,
	}, "daily default enter count"); err != nil {
		return err
	}
	// This nearby value is a selector-history length, not an admission limit.
	c.addEvidence(Evidence{
		Key: "dimension_gate.recent_history", SourcePath: DimensionGatePath, SourceSection: "recently enter dungeon count",
		Limit: positiveSectionValue(documents[DimensionGatePath], "recently enter dungeon count"), DungeonIDs: dimensionGateIDs,
		Reason:    "selector recent-history count has no limit/reset ownership",
		RawValues: documents[DimensionGatePath].Ints("recently enter dungeon count"),
	})

	// The current PVF's recommendation actions carry daily use counters and add
	// the matching tower entry pools. Tower of Despair additionally proves its
	// same-day boundary in the required invitation's [explain] text. Together
	// these close the period gap left by the two tower ETC files alone while
	// preserving their authored character/account caps and exact membership.
	if err := validateTowerOfDespairDailyEvidence(documents[TowerOfDespairInvitationPath]); err != nil {
		return err
	}
	for _, tower := range []struct {
		path   string
		family TowerFamily
	}{
		{path: TowerOfDespairPath, family: TowerFamilyDespair},
		{path: TowerOfGriefPath, family: TowerFamilyGrief},
	} {
		route, towerErr := parseTowerRoute(documents[tower.path], tower.family)
		if towerErr != nil {
			return towerErr
		}
		c.towerRoute[tower.family] = route
		for _, policy := range []Policy{
			{
				Key: string(tower.family) + ".character.daily", SourcePath: tower.path, SourceSection: "enterable count",
				Scope: ScopeCharacter, Period: PeriodDaily, DungeonIDs: route.DungeonIDs,
			},
			{
				Key: string(tower.family) + ".account.daily", SourcePath: tower.path, SourceSection: "account enterable max count",
				Scope: ScopeAccount, Period: PeriodDaily, DungeonIDs: route.DungeonIDs,
			},
		} {
			if err := c.addSectionPolicy(documents[tower.path], policy, policy.SourceSection); err != nil {
				return err
			}
		}
	}

	for _, raid := range []struct {
		key  string
		path string
	}{
		{key: "raid.anton.account.daily", path: AntonRaidPath},
		{key: "raid.luke.account.daily", path: LukeRaidPath},
		{key: "raid.fiend_war.account.daily", path: FiendWarRaidPath},
	} {
		if err := c.addSectionPolicy(documents[raid.path], Policy{
			Key: raid.key, SourcePath: raid.path, SourceSection: "daily account enter limit", Scope: ScopeAccount, Period: PeriodDaily,
		}, "daily account enter limit"); err != nil {
			return err
		}
	}
	if err := c.loadAccountCharacterDungeonDaily(documents); err != nil {
		return err
	}
	return nil
}

func parseTowerRoute(document *dnfpvf.Document, family TowerFamily) (TowerRoute, error) {
	if document == nil || (family != TowerFamilyDespair && family != TowerFamilyGrief) {
		return TowerRoute{}, fmt.Errorf("%w: invalid tower route family=%q", ErrCatalogInvalid, family)
	}
	values := document.Ints("each layer matching dungeon")
	if len(values) == 0 || len(values)%3 != 0 || len(values)/3 > int(^uint16(0)) {
		return TowerRoute{}, catalogSectionError(document.Path, "each layer matching dungeon", values)
	}
	route := TowerRoute{Family: family, DungeonIDs: make([]uint32, 0, len(values)/3)}
	seen := make(map[uint32]struct{}, len(values)/3)
	for offset := 0; offset < len(values); offset += 3 {
		floor := offset/3 + 1
		dungeonID := values[offset+1]
		if values[offset] != int64(floor) || dungeonID <= 0 || dungeonID > int64(^uint32(0)) {
			return TowerRoute{}, catalogSectionError(document.Path, "each layer matching dungeon", values)
		}
		id := uint32(dungeonID)
		if _, duplicate := seen[id]; duplicate {
			return TowerRoute{}, fmt.Errorf("%w: %s duplicate tower dungeon=%d", ErrCatalogInvalid, document.Path, id)
		}
		seen[id] = struct{}{}
		route.DungeonIDs = append(route.DungeonIDs, id)
	}
	itemValues := document.Ints("use allow item list in dungeon")
	if family == TowerFamilyGrief && (len(itemValues) == 0 || len(itemValues)%2 != 0 || len(itemValues)/2 > int(^uint8(0))) {
		return TowerRoute{}, catalogSectionError(document.Path, "use allow item list in dungeon", itemValues)
	}
	if len(itemValues)%2 != 0 || len(itemValues)/2 > int(^uint8(0)) {
		return TowerRoute{}, catalogSectionError(document.Path, "use allow item list in dungeon", itemValues)
	}
	for offset := 0; offset < len(itemValues); offset += 2 {
		itemID, count := itemValues[offset], itemValues[offset+1]
		if itemID <= 0 || itemID > int64(^uint32(0)) || count < int64(-1<<31) || count > int64(1<<31-1) {
			return TowerRoute{}, catalogSectionError(document.Path, "use allow item list in dungeon", itemValues)
		}
		route.UseAllowItems = append(route.UseAllowItems, TowerUseItem{ItemID: uint32(itemID), Count: int32(count)})
	}
	return route, nil
}

func (c *Catalog) loadExplicitWeekly(documents map[string]*dnfpvf.Document) error {
	resetValues := documents[WeeklyDungeonPath].Ints("reset day index")
	if len(resetValues) != 1 || resetValues[0] < int64(time.Sunday) || resetValues[0] > int64(time.Saturday) {
		return catalogSectionError(WeeklyDungeonPath, "reset day index", resetValues)
	}
	resetDay := time.Weekday(resetValues[0])
	c.addEvidence(Evidence{
		Key: "weekly_dungeon.default_enter_count", SourcePath: WeeklyDungeonPath, SourceSection: "weekly default enter count",
		Period: PeriodWeekly, Limit: positiveSectionValue(documents[WeeklyDungeonPath], "weekly default enter count"), ResetWeekday: resetDay,
		Reason:    "no dungeon identity or durable owner is declared in this document",
		RawValues: documents[WeeklyDungeonPath].Ints("weekly default enter count"),
	})
	return nil
}

func (c *Catalog) loadAccountCharacterDungeonDaily(documents map[string]*dnfpvf.Document) error {
	document := documents[AccountWeeklyDungeonPath]
	maxValues := document.Ints("dungeon enter max count")
	if len(maxValues) == 0 || len(maxValues)%2 != 0 {
		return catalogSectionError(AccountWeeklyDungeonPath, "dungeon enter max count", maxValues)
	}
	indexValues := document.Ints("weekly dungeon index")
	if len(indexValues) == 0 || len(indexValues)%2 != 0 {
		return catalogSectionError(AccountWeeklyDungeonPath, "weekly dungeon index", indexValues)
	}
	indexed := make(map[uint32]struct{})
	for offset := 0; offset < len(indexValues); offset += 2 {
		if indexValues[offset] < 0 || indexValues[offset] > 6 ||
			indexValues[offset+1] <= 0 || indexValues[offset+1] > int64(^uint32(0)) {
			return catalogSectionError(AccountWeeklyDungeonPath, "weekly dungeon index", indexValues)
		}
		indexed[uint32(indexValues[offset+1])] = struct{}{}
	}

	defaultValues := document.Ints("default enterable count")
	if len(defaultValues) != 1 || defaultValues[0] <= 0 || defaultValues[0] > int64(^uint32(0)) {
		return catalogSectionError(AccountWeeklyDungeonPath, "default enterable count", defaultValues)
	}
	bonusValues := document.Ints("adventure group add enterable count chn")
	if len(bonusValues)%3 != 0 {
		return catalogSectionError(AccountWeeklyDungeonPath, "adventure group add enterable count chn", bonusValues)
	}

	dungeonIDs := make([]uint32, 0, len(maxValues)/2)
	seenDungeons := make(map[uint32]struct{})
	for offset := 0; offset < len(maxValues); offset += 2 {
		dungeonID, limit := maxValues[offset], maxValues[offset+1]
		if dungeonID <= 0 || dungeonID > int64(^uint32(0)) || limit <= 0 || limit > int64(^uint32(0)) {
			return catalogSectionError(AccountWeeklyDungeonPath, "dungeon enter max count", maxValues)
		}
		id := uint32(dungeonID)
		if _, ok := indexed[id]; !ok {
			return fmt.Errorf("%w: %s dungeon=%d absent from weekly dungeon index", ErrCatalogInvalid, AccountWeeklyDungeonPath, id)
		}
		if _, duplicate := seenDungeons[id]; duplicate {
			return fmt.Errorf("%w: %s duplicate dungeon=%d", ErrCatalogInvalid, AccountWeeklyDungeonPath, id)
		}
		seenDungeons[id] = struct{}{}
		dungeonIDs = append(dungeonIDs, id)
		if err := c.addPolicy(Policy{
			Key:           fmt.Sprintf("account_character_dungeon.%d.character.daily", id),
			SourcePath:    AccountWeeklyDungeonPath,
			SourceSection: "dungeon enter max count",
			Scope:         ScopeCharacter,
			Period:        PeriodDaily,
			Limit:         uint32(limit),
			DungeonIDs:    []uint32{id},
		}); err != nil {
			return err
		}
	}

	c.accountCharacterDungeonBaseLimit = uint32(defaultValues[0])
	c.accountCharacterDungeonAdventureBonus = c.accountCharacterDungeonAdventureBonus[:0]
	for offset := 0; offset < len(bonusValues); offset += 3 {
		minimum, maximum, amount := bonusValues[offset], bonusValues[offset+1], bonusValues[offset+2]
		if minimum < 0 || maximum < minimum || amount < 0 ||
			maximum > int64(^uint32(0)) || amount > int64(^uint32(0)) {
			return catalogSectionError(AccountWeeklyDungeonPath, "adventure group add enterable count chn", bonusValues)
		}
		c.accountCharacterDungeonAdventureBonus = append(c.accountCharacterDungeonAdventureBonus,
			accountCharacterDungeonAdventureBonus{
				minimumLevel: uint32(minimum),
				maximumLevel: uint32(maximum),
				amount:       uint32(amount),
			})
	}
	return c.addPolicy(Policy{
		Key:           AccountCharacterDungeonAccountPolicyKey,
		SourcePath:    AccountWeeklyDungeonPath,
		SourceSection: "default enterable count",
		Scope:         ScopeAccount,
		Period:        PeriodDaily,
		Limit:         c.accountCharacterDungeonBaseLimit,
		DungeonIDs:    dungeonIDs,
	})
}

func (c *Catalog) loadUnprovedPeriodEvidence(documents map[string]*dnfpvf.Document) error {
	for _, tower := range []struct {
		key, path string
		scope     Scope
		section   string
		idSection string
	}{
		{key: "sao_tower_of_despair.character", path: SAOTowerOfDespairPath, scope: ScopeCharacter, section: "enterable count", idSection: "each layer matching dungeon"},
	} {
		ids, err := tupleDungeonIDs(documents[tower.path], tower.idSection, 3, 1)
		if err != nil {
			return err
		}
		values := documents[tower.path].Ints(tower.section)
		limit, err := onePositive(values)
		if err != nil {
			return catalogSectionError(tower.path, tower.section, values)
		}
		c.addEvidence(Evidence{
			Key: tower.key, SourcePath: tower.path, SourceSection: tower.section, Scope: tower.scope, Limit: limit, DungeonIDs: ids,
			Reason:    "PVF proves the cap and dungeon membership but does not declare a daily/weekly reset period",
			RawValues: values,
		})
	}
	beastValues := documents[BeastDungeonPath].Ints("limit enter count")
	beastLimit, err := onePositive(beastValues)
	if err != nil {
		return catalogSectionError(BeastDungeonPath, "limit enter count", beastValues)
	}
	c.addEvidence(Evidence{
		Key: "beast_dungeon.unbound", SourcePath: BeastDungeonPath, SourceSection: "limit enter count", Limit: beastLimit,
		Reason: "PVF declares a cap but no daily/weekly reset period in the same structured config", RawValues: beastValues,
	})
	// [WEEKLY ENTER COUNT] is a two-operand row on every raid document, and the
	// operand meaning is now READ OFF THE SHIPPED CLIENT rather than inferred:
	//
	//   etc/raid/anton.etc          [WEEKLY ENTER COUNT] 30 3
	//   etc/raid/luke/luke.etc      [WEEKLY ENTER COUNT] 20 2
	//   etc/raid/fiendwar/….etc     [WEEKLY ENTER COUNT] 20 2
	//
	// and the client's two raid windows render exactly those pairs:
	//
	//   window A "选择团队频道"   帐号周入场场数: 30/30 | 20/20 | 20/20
	//   window B "团队模式入场信息" 角色周入场次数:  3/3  |  2/2  |  2/2
	//
	// (Window screenshots supplied by the server owner 2026-09-30; the earlier
	// reading -- first operand = account-wide ceiling, second = per-character
	// share, from a 2017-11 China-service notice -- is thereby confirmed by the
	// client itself rather than by the notice alone.)
	//
	// So each raid gets BOTH scopes as real Policies. Two properties are
	// deliberate:
	//
	//   - No DungeonIDs, exactly like the raid daily policies: these are
	//     raid-START limits looked up by semantic key, and PoliciesForDungeon must
	//     never apply them once per internal raid dungeon.
	//   - ResetWeekday is this repository's established weekly boundary
	//     (Thursday 06:00, the same node etc/weeklydungeon.etc [reset day index]
	//     feeds). The raid documents declare no reset weekday of their own, so
	//     this remains an explicit assumption -- but a CHARGED one now, so every
	//     raid start logs the window it was charged to and the real node can be
	//     falsified from production traffic.
	for _, raid := range []struct {
		path         string
		accountKey   string
		characterKey string
	}{
		{path: AntonRaidPath, accountKey: AntonWeeklyAccountPolicyKey, characterKey: AntonWeeklyCharacterPolicyKey},
		{path: LukeRaidPath, accountKey: LukeWeeklyAccountPolicyKey, characterKey: LukeWeeklyCharacterPolicyKey},
		{path: FiendWarRaidPath, accountKey: FiendWarWeeklyAccountPolicyKey, characterKey: FiendWarWeeklyCharacterPolicyKey},
	} {
		accountLimit, characterLimit, values, err := raidWeeklyEnterCounts(documents[raid.path])
		if err != nil {
			return err
		}
		for _, policy := range []Policy{
			{
				Key: raid.accountKey, SourcePath: raid.path, SourceSection: "weekly enter count",
				Scope: ScopeAccount, Period: PeriodWeekly, Limit: accountLimit, ResetWeekday: time.Thursday,
			},
			{
				Key: raid.characterKey, SourcePath: raid.path, SourceSection: "weekly enter count",
				Scope: ScopeCharacter, Period: PeriodWeekly, Limit: characterLimit, ResetWeekday: time.Thursday,
			},
		} {
			if err := c.addPolicy(policy); err != nil {
				return err
			}
		}
		c.addEvidence(Evidence{
			Key: "raid_weekly_enter_count." + strings.ReplaceAll(raid.path, "/", "."), SourcePath: raid.path,
			SourceSection: "weekly enter count", Period: PeriodWeekly, ResetWeekday: time.Thursday,
			Limit: accountLimit, RawValues: values, Enforced: true,
		})
	}
	return nil
}

// raidWeeklyEnterCounts reads one raid document's [WEEKLY ENTER COUNT] row and
// returns (account weekly, character weekly).
//
// The shipped fiendwar.etc declares the section TWICE, so Ints() yields
// `20 2 20 2`. Repeating the same pair is harmless and is tolerated; a repeated
// section whose operands DISAGREE is not a document this server will guess
// about, so it fails loud instead of silently taking one of the two readings.
func raidWeeklyEnterCounts(document *dnfpvf.Document) (uint32, uint32, []int64, error) {
	if document == nil {
		return 0, 0, nil, fmt.Errorf("%w: raid weekly document is missing", ErrCatalogInvalid)
	}
	values := document.Ints("weekly enter count")
	if len(values) < 2 || len(values)%2 != 0 {
		return 0, 0, values, catalogSectionError(document.Path, "weekly enter count", values)
	}
	accountLimit, characterLimit := values[0], values[1]
	for offset := 2; offset < len(values); offset += 2 {
		if values[offset] != accountLimit || values[offset+1] != characterLimit {
			return 0, 0, values, catalogSectionError(document.Path, "weekly enter count", values)
		}
	}
	if accountLimit <= 0 || accountLimit > int64(^uint32(0)) ||
		characterLimit <= 0 || characterLimit > int64(^uint32(0)) {
		return 0, 0, values, catalogSectionError(document.Path, "weekly enter count", values)
	}
	return uint32(accountLimit), uint32(characterLimit), values, nil
}

func validateTowerOfDespairDailyEvidence(document *dnfpvf.Document) error {
	if document == nil {
		return fmt.Errorf("%w: %s document is missing", ErrCatalogInvalid, TowerOfDespairInvitationPath)
	}
	stackableType, found := document.Text("stackable type")
	if !found || !strings.EqualFold(strings.TrimSpace(stackableType), "[material]") {
		return fmt.Errorf("%w: %s [stackable type]=%q want [material]", ErrCatalogInvalid, TowerOfDespairInvitationPath, stackableType)
	}
	explain := strings.Join(document.Texts("explain"), "\n")
	if !strings.Contains(explain, "绝望之塔") || !strings.Contains(explain, "当日") {
		return fmt.Errorf("%w: %s does not prove same-day Tower of Despair entry semantics", ErrCatalogInvalid, TowerOfDespairInvitationPath)
	}
	return nil
}

func (c *Catalog) addSectionPolicy(document *dnfpvf.Document, policy Policy, section string) error {
	values := document.Ints(section)
	limit, err := onePositive(values)
	if err != nil {
		return catalogSectionError(policy.SourcePath, section, values)
	}
	policy.Limit = limit
	return c.addPolicy(policy)
}

func (c *Catalog) addPolicy(policy Policy) error {
	if err := validatePolicy(policy); err != nil {
		return fmt.Errorf("%w: %v", ErrCatalogInvalid, err)
	}
	if _, duplicate := c.policies[policy.Key]; duplicate {
		return fmt.Errorf("%w: duplicate policy=%s", ErrCatalogInvalid, policy.Key)
	}
	seen := make(map[uint32]struct{}, len(policy.DungeonIDs))
	for _, dungeonID := range policy.DungeonIDs {
		if dungeonID == 0 {
			return fmt.Errorf("%w: policy=%s contains dungeon 0", ErrCatalogInvalid, policy.Key)
		}
		if _, duplicate := seen[dungeonID]; duplicate {
			return fmt.Errorf("%w: policy=%s duplicate dungeon=%d", ErrCatalogInvalid, policy.Key, dungeonID)
		}
		seen[dungeonID] = struct{}{}
	}
	policy.DungeonIDs = sortedUint32(policy.DungeonIDs)
	c.policies[policy.Key] = policy
	for _, dungeonID := range policy.DungeonIDs {
		c.byDungeon[dungeonID] = append(c.byDungeon[dungeonID], policy.Key)
	}
	c.addEvidence(Evidence{
		Key: policy.Key, SourcePath: policy.SourcePath, SourceSection: policy.SourceSection, Scope: policy.Scope,
		Period: policy.Period, Limit: policy.Limit, ResetWeekday: policy.ResetWeekday, DungeonIDs: policy.DungeonIDs, Enforced: true,
	})
	return nil
}

func (c *Catalog) addEvidence(evidence Evidence) {
	evidence.DungeonIDs = sortedUint32(evidence.DungeonIDs)
	evidence.RawValues = append([]int64(nil), evidence.RawValues...)
	c.evidence = append(c.evidence, evidence)
}

func (c *Catalog) sort() {
	for dungeonID := range c.byDungeon {
		sort.Strings(c.byDungeon[dungeonID])
	}
	sort.Slice(c.evidence, func(left, right int) bool {
		if c.evidence[left].SourcePath != c.evidence[right].SourcePath {
			return c.evidence[left].SourcePath < c.evidence[right].SourcePath
		}
		if c.evidence[left].SourceSection != c.evidence[right].SourceSection {
			return c.evidence[left].SourceSection < c.evidence[right].SourceSection
		}
		return c.evidence[left].Key < c.evidence[right].Key
	})
}

func tupleDungeonIDs(document *dnfpvf.Document, section string, width, dungeonOffset int) ([]uint32, error) {
	values := document.Ints(section)
	if width <= 0 || dungeonOffset < 0 || dungeonOffset >= width || len(values) == 0 || len(values)%width != 0 {
		return nil, catalogSectionError(document.Path, section, values)
	}
	ids := make([]uint32, 0, len(values)/width)
	seen := make(map[uint32]struct{})
	for offset := 0; offset < len(values); offset += width {
		value := values[offset+dungeonOffset]
		if value <= 0 || value > int64(^uint32(0)) {
			return nil, catalogSectionError(document.Path, section, values)
		}
		id := uint32(value)
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: %s section=%s duplicate dungeon=%d", ErrCatalogInvalid, document.Path, section, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return sortedUint32(ids), nil
}

func crackOfDimensionDungeonID(document *dnfpvf.Document) (uint32, error) {
	var found uint32
	for _, entry := range dnfpvf.ParseList(document) {
		path := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(entry.Path), "\\", "/"))
		if path != "crackofdimension/crackofdimension.dgn" {
			continue
		}
		if found != 0 || entry.ID <= 0 || entry.ID > int64(^uint32(0)) {
			return 0, fmt.Errorf("%w: ambiguous crack-of-dimension dungeon list row", ErrCatalogInvalid)
		}
		found = uint32(entry.ID)
	}
	if found == 0 {
		return 0, fmt.Errorf("%w: crack-of-dimension dungeon list row missing", ErrCatalogInvalid)
	}
	return found, nil
}

func positiveSectionValue(document *dnfpvf.Document, section string) uint32 {
	value, _ := onePositive(document.Ints(section))
	return value
}

func onePositive(values []int64) (uint32, error) {
	if len(values) != 1 || values[0] <= 0 || values[0] > int64(^uint32(0)) {
		return 0, ErrCatalogInvalid
	}
	return uint32(values[0]), nil
}

func catalogSectionError(path, section string, values []int64) error {
	return fmt.Errorf("%w: %s section=%s values=%v", ErrCatalogInvalid, path, section, values)
}

func sortedUint32(values []uint32) []uint32 {
	out := append([]uint32(nil), values...)
	sort.Slice(out, func(left, right int) bool { return out[left] < out[right] })
	return out
}

func clonePolicy(policy Policy) Policy {
	policy.DungeonIDs = append([]uint32(nil), policy.DungeonIDs...)
	return policy
}
