package worldmap

import dnfpvf "longheng.io/server/internal/modules/dnf/pvf"

type SectionValueKind string

const (
	SectionFlag     SectionValueKind = "flag"
	SectionIntegers SectionValueKind = "integers"
	SectionNumbers  SectionValueKind = "numbers"
	SectionTexts    SectionValueKind = "texts"
	SectionMixed    SectionValueKind = "mixed"
)

type ExtensionSection struct {
	Name       string           `json:"name"`
	Occurrence int              `json:"occurrence"`
	Scope      []string         `json:"scope,omitempty"`
	Block      bool             `json:"block,omitempty"`
	Kind       SectionValueKind `json:"kind"`
	Integers   []int64          `json:"integers,omitempty"`
	Numbers    []float64        `json:"numbers,omitempty"`
	Texts      []string         `json:"texts,omitempty"`
	Symbols    []string         `json:"symbols,omitempty"`
	Tokens     []dnfpvf.Token   `json:"tokens,omitempty"`
}

type OptionalNumber struct {
	Value float64 `json:"value"`
	Set   bool    `json:"set"`
}

// BloodAltarDungeonKind is the value carried by PVF [blood dungeon].
type BloodAltarDungeonKind uint8

const (
	BloodAltarDungeonKindEndless  BloodAltarDungeonKind = 1
	BloodAltarDungeonKindUltimate BloodAltarDungeonKind = 2
)

// LostLandParameters is the pair of PVF millisecond phase durations consumed
// by the current client's Forgotten Land day/night UI.
type LostLandParameters struct {
	DayMilliseconds   int64 `json:"day_milliseconds"`
	NightMilliseconds int64 `json:"night_milliseconds"`
	Set               bool  `json:"set"`
}

// DungeonLimitTime preserves both PVF [limit time] values. The current
// Forgotten Land client reads PrimarySeconds as its countdown duration; the
// second value is retained without assigning an unsupported semantic name.
type DungeonLimitTime struct {
	PrimarySeconds   int64 `json:"primary_seconds"`
	SecondarySeconds int64 `json:"secondary_seconds"`
	Set              bool  `json:"set"`
}

// DungeonEventMonsterDefinition preserves one dungeon-wide PVF
// [event monster], [event monster2], or [event monster3] row. The integer
// layout is client-owned and varies between generations, so Values remains an
// ordered typed vector rather than assigning unverified field names.
type DungeonEventMonsterDefinition struct {
	Kind   string  `json:"kind"`
	Values []int64 `json:"values"`
}

type DungeonMetadata struct {
	Name                      string         `json:"name,omitempty"`
	Explain                   string         `json:"explain,omitempty"`
	CutsceneImage             Resource       `json:"cutscene_image"`
	MinimapImage              string         `json:"minimap_image,omitempty"`
	EnteringTitle             string         `json:"entering_title,omitempty"`
	MinimumRequiredLevel      OptionalInt    `json:"minimum_required_level"`
	BasisLevel                OptionalInt    `json:"basis_level"`
	ExperienceIncreasingPoint OptionalNumber `json:"experience_increasing_point"`
	BackgroundPosition        OptionalInt    `json:"background_position"`
	DungeonType               string         `json:"dungeon_type,omitempty"`
	Champion                  []int64        `json:"champion,omitempty"`
	PathgateObjects           []int64        `json:"pathgate_objects,omitempty"`
	WorldMapPatternInfo       []RawSection   `json:"worldmap_pattern_info,omitempty"`
	WorldMapInfo              []RawSection   `json:"worldmap_info,omitempty"`
	Difficulty                OptionalInt    `json:"difficulty"`
	DifficultyLevels          []int64        `json:"difficulty_levels,omitempty"`
	DesignatedDifficulties    []int64        `json:"designated_difficulties,omitempty"`
	TutorialDungeon           OptionalInt    `json:"tutorial_dungeon"`
	// CoinLimit is [coin limit]: how many revive coins one entry of this
	// dungeon may consume (Luke's three sanctums declare 8).
	// 上游 bca733b/8b465b5 这条线加的字段；卢克一阶段「每圣所单次进图 8 次」用它。
	CoinLimit                 OptionalInt                 `json:"coin_limit"`
	NoFatigue                 bool                        `json:"no_fatigue,omitempty"`
	NamedMonsters             []int64                     `json:"named_monsters,omitempty"`
	RecommendedLevels         []int64                     `json:"recommended_levels,omitempty"`
	LimitPartyCount           OptionalInt                 `json:"limit_party_count"`
	ScheduleIDs               []int64                     `json:"schedule_ids,omitempty"`
	LostLandParameters        LostLandParameters          `json:"lost_land_parameters"`
	LimitTime                 DungeonLimitTime            `json:"limit_time"`
	IntegerValues             map[string]int64            `json:"integer_values,omitempty"`
	NumberValues              map[string]float64          `json:"number_values,omitempty"`
	Flags                     map[string]bool             `json:"flags,omitempty"`
	TextValues                map[string][]string         `json:"text_values,omitempty"`
	TournamentRoundFatigue    int64                       `json:"tournament_round_fatigue"`
	TournamentGoldRate        int64                       `json:"tournament_gold_rate"`
	TournamentClearRewardExp  []TournamentRoundExperience `json:"tournament_clear_reward_exp,omitempty"`
	TournamentResultCards     []TournamentResultCard      `json:"tournament_result_cards,omitempty"`
	TournamentRewardItemRates []TournamentRewardItemRate  `json:"tournament_reward_item_rates,omitempty"`
	RequiredItems             []TicketItem                `json:"required_items,omitempty"`
	// EventRequiredItems and AddedRequiredItems remain separate evidence. The
	// active selectable dungeon list does not route either section, and the
	// current op16 request has no proven event/schedule selector. They must not
	// be silently conjoined with the ordinary [required item] contract.
	EventRequiredItems []TicketItem `json:"event_required_items,omitempty"`
	AddedRequiredItems []TicketItem `json:"added_required_items,omitempty"`
	// SpecialPassiveObjectItems is dungeon-wide in the current PVF. Every maze
	// route consumes the same reward groups; placing the section on the last
	// parsed maze makes earlier routes silently lose their configured drops.
	SpecialPassiveObjectItems []SpecialPassiveObjectItem `json:"special_passive_object_items,omitempty"`
	// InfernoDungeon and InfernoQuestIDs are the current PVF contract for
	// Time Rift. They are intentionally separate from ordinary hell-party
	// region metadata: the dungeon itself owns the route table and opening
	// quest.
	InfernoDungeon              bool                            `json:"inferno_dungeon,omitempty"`
	InfernoQuestIDs             []int64                         `json:"inferno_quest_ids,omitempty"`
	SelectDungeonNormalDisabled bool                            `json:"select_dungeon_normal_disabled,omitempty"`
	HellPartyRequiredItemCount  OptionalInt                     `json:"hell_party_required_item_count"`
	EventMonsterRandomMap       OptionalInt                     `json:"event_monster_random_map"`
	EventMonsters               []DungeonEventMonsterDefinition `json:"event_monsters,omitempty"`
}

type MazePoint struct {
	X      int64   `json:"x"`
	Y      int64   `json:"y"`
	Params []int64 `json:"params,omitempty"`
}

type MapSpecification struct {
	Type              string  `json:"type"`
	Coordinate        Point   `json:"coordinate"`
	MapIDs            []int64 `json:"map_ids"`
	SectionOccurrence int     `json:"section_occurrence"`
}

type RidableObject struct {
	MazeCoordinate  *Point             `json:"maze_coordinate,omitempty"`
	ObjectIndex     OptionalInt        `json:"object_index"`
	Team            string             `json:"team,omitempty"`
	Position        *Point             `json:"position,omitempty"`
	SourceSections  []RawSection       `json:"source_sections,omitempty"`
	UnknownSections []RawSection       `json:"unknown_sections,omitempty"`
	Extensions      []ExtensionSection `json:"extensions,omitempty"`
}

type RandomizedObjectScript struct {
	SelectCount     OptionalInt        `json:"select_count"`
	Regenerate      OptionalInt        `json:"regenerate"`
	MinimapIcon     OptionalInt        `json:"minimap_icon"`
	Objects         []RidableObject    `json:"objects,omitempty"`
	SourceSections  []RawSection       `json:"source_sections,omitempty"`
	UnknownSections []RawSection       `json:"unknown_sections,omitempty"`
	Extensions      []ExtensionSection `json:"extensions,omitempty"`
}

type ClearCondition struct {
	Type     string `json:"type"`
	TargetID int64  `json:"target_id"`
	Count    int64  `json:"count"`
}

// BossRoomEntranceHuntMonster is one PVF [hunt monster] target from a
// per-maze [boss room entrance condition]. Opaque preserves the middle value
// of the current three-integer row without assigning an unverified meaning.
type BossRoomEntranceHuntMonster struct {
	TargetID int64 `json:"target_id"`
	Opaque   int64 `json:"opaque"`
	Count    int64 `json:"count"`
}

// BossRoomEntranceCondition is the typed per-maze gate that precedes a boss
// room. The current PVF encodes its action and condition labels as bracketed
// text tokens inside one section rather than as nested sections.
type BossRoomEntranceCondition struct {
	Action        string                        `json:"action"`
	TimeCondition []int64                       `json:"time_condition"`
	HuntMonsters  []BossRoomEntranceHuntMonster `json:"hunt_monsters"`
}

// WarpMapCondition is one PVF [warp map condition] source/destination pair.
// The source is a logical trigger coordinate. It is intentionally not treated
// as an ordinary maze map assignment: several conditions may share one source
// and the active destination is selected by the dungeon mechanism at runtime.
type WarpMapCondition struct {
	Source      Point `json:"source"`
	Destination Point `json:"destination"`
}

// SpecialPassiveObjectItem is one weighted candidate of a dgn
// [special passive object item] group. The section is a list of records
// `<group index> <level override> <item count> (<item id> <drop rate>)...`;
// a map [special passive object] child of kind [item] selects the record by
// its spawn code, so Index is the record's position in that vector and not a
// passive-object template id. DropRate is a 0..10000 slice of one roll: a
// group's rates never renormalize, so a group summing below 10000 can roll
// "no drop", and one roll yields at most one item.
type SpecialPassiveObjectItem struct {
	Index         int64 `json:"index"`
	LevelOverride int64 `json:"level_override"`
	ItemID        int64 `json:"item_id"`
	DropRate      int64 `json:"drop_rate"`
}

type Maze struct {
	Index                     int                        `json:"index"`
	ChanceRate                OptionalInt                `json:"chance_rate"`
	InfernoSpecial            bool                       `json:"inferno_special,omitempty"`
	EscapeHell                OptionalInt                `json:"escape_hell"`
	Width                     OptionalInt                `json:"width"`
	Height                    OptionalInt                `json:"height"`
	Greed                     string                     `json:"greed,omitempty"`
	MapSpecifications         []MapSpecification         `json:"map_specifications,omitempty"`
	Start                     *MazePoint                 `json:"start,omitempty"`
	Boss                      *MazePoint                 `json:"boss,omitempty"`
	HitCount                  []int64                    `json:"hit_count,omitempty"`
	SealDoorAppearRate        OptionalInt                `json:"seal_door_appear_rate"`
	SealDoorMapIndex          OptionalInt                `json:"seal_door_map_index"`
	SealDoorPosition          []int64                    `json:"seal_door_position,omitempty"`
	QuestConnection           []int64                    `json:"quest_connection,omitempty"`
	SpecialPassiveObjectItems []SpecialPassiveObjectItem `json:"special_passive_object_items,omitempty"`
	BossMapSpecifications     []RawSection               `json:"boss_map_specifications,omitempty"`
	LayeredMapSpecifications  []RawSection               `json:"layered_map_specifications,omitempty"`
	BossSpecifications        []MapSpecification         `json:"boss_specifications,omitempty"`
	LayeredSpecifications     []MapSpecification         `json:"layered_specifications,omitempty"`
	RandomizedObjects         []RandomizedObjectScript   `json:"randomized_objects,omitempty"`
	ClearConditions           []ClearCondition           `json:"clear_conditions,omitempty"`
	BossRoomEntranceCondition *BossRoomEntranceCondition `json:"boss_room_entrance_condition,omitempty"`
	WarpMapConditions         []WarpMapCondition         `json:"warp_map_conditions,omitempty"`
	SourceSections            []RawSection               `json:"source_sections,omitempty"`
	OpaqueSections            []RawSection               `json:"opaque_sections,omitempty"`
	UnknownSections           []RawSection               `json:"unknown_sections,omitempty"`
	Extensions                []ExtensionSection         `json:"extensions,omitempty"`
	Diagnostics               []Diagnostic               `json:"diagnostics,omitempty"`
}

type Dungeon struct {
	ID                int64                 `json:"id"`
	Path              string                `json:"path"`
	Metadata          DungeonMetadata       `json:"metadata"`
	BloodAltarKind    BloodAltarDungeonKind `json:"blood_altar_kind,omitempty"`
	BloodMaxRounds    int64                 `json:"blood_max_rounds,omitempty"`
	Mazes             []Maze                `json:"mazes,omitempty"`
	WarpMapConditions []WarpMapCondition    `json:"warp_map_conditions,omitempty"`
	SourceSections    []RawSection          `json:"source_sections,omitempty"`
	OpaqueSections    []RawSection          `json:"opaque_sections,omitempty"`
	UnknownSections   []RawSection          `json:"unknown_sections,omitempty"`
	Extensions        []ExtensionSection    `json:"extensions,omitempty"`
	Diagnostics       []Diagnostic          `json:"diagnostics,omitempty"`
}
