package worldmap

import (
	"fmt"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func ParseDungeon(id int64, dungeonPath string, doc *dnfpvf.Document) Dungeon {
	result := Dungeon{ID: id, Path: dungeonPath}
	sections, diagnostics := rawSections(doc)
	result.SourceSections = sections
	result.Diagnostics = append(result.Diagnostics, diagnostics...)

	currentMaze := -1
	complexBlock := ""
	for _, section := range sections {
		key := sectionKey(section.Name)
		if key == "maze info" {
			result.Mazes = append(result.Mazes, Maze{Index: len(result.Mazes)})
			currentMaze = len(result.Mazes) - 1
			result.Mazes[currentMaze].SourceSections = append(result.Mazes[currentMaze].SourceSections, cloneRawSection(section))
			continue
		}
		if currentMaze >= 0 && key == "escape hell" {
			// [escape hell] is physically emitted after the [maze info] it
			// qualifies. Keep it on that selected-maze route even though older
			// dungeon metadata handling also recognises the same section name.
			parseMazeSection(&result.Mazes[currentMaze], section)
			continue
		}
		if currentMaze >= 0 && complexBlock != "" {
			parseMazeSection(&result.Mazes[currentMaze], section)
			if key == "/"+complexBlock {
				complexBlock = ""
			}
			continue
		}
		if key == "special passive object item" && currentMaze >= 0 {
			// This section is positional in the live PVF. Inferno dungeons place
			// it after the maze it belongs to, while standalone particular-hell
			// dungeons place one trailing definition after all mazes. Keep the
			// positional owner here and normalize the latter form after parsing.
			parseMazeSection(&result.Mazes[currentMaze], section)
			continue
		}
		if key == "boss room entrance condition" && currentMaze >= 0 {
			// This gate is physically placed after the maze it owns. Its value is
			// one flat token stream, despite the bracketed labels looking like
			// nested PVF sections.
			parseMazeSection(&result.Mazes[currentMaze], section)
			continue
		}
		if currentMaze >= 0 && scopeOwnedByDungeonMetadata(section.Scope) {
			// Flag-style dungeon metadata is represented by an open PVF section
			// without an explicit close.  Later root-level metadata can therefore
			// retain that flag in its syntactic scope.  Keep recognised metadata
			// (notably Time Rift's [inferno quest]) in the dungeon model instead
			// of demoting it to an opaque extension.
			if isDungeonMetadataSection(key) {
				parseDungeonMetadata(&result, section)
			} else {
				parseDungeonExtension(&result, section)
			}
			continue
		}
		if currentMaze >= 0 && (key == "randomized object creation" || key == "clear condition") {
			complexBlock = key
			parseMazeSection(&result.Mazes[currentMaze], section)
			continue
		}
		if currentMaze >= 0 && !isDungeonMetadataSection(key) {
			parseMazeSection(&result.Mazes[currentMaze], section)
			continue
		}
		parseDungeonMetadata(&result, section)
	}
	parseScopedDungeonMechanismMetadata(&result, sections)
	normalizeDungeonPassiveObjectItemOwnership(&result)
	for index := range result.Mazes {
		parseMazeComplexSections(&result.Mazes[index])
	}
	parseWarpMapConditions(&result, sections)
	parseTournamentDungeonSections(&result, sections)
	return result
}

func normalizeDungeonPassiveObjectItemOwnership(result *Dungeon) {
	if result == nil {
		return
	}
	for index := range result.Mazes {
		result.Mazes[index].SpecialPassiveObjectItems = uniqueSpecialPassiveObjectItems(
			result.Mazes[index].SpecialPassiveObjectItems,
		)
	}
	result.Metadata.SpecialPassiveObjectItems = uniqueSpecialPassiveObjectItems(
		result.Metadata.SpecialPassiveObjectItems,
	)
	// The table is dungeon-wide, wherever the file physically places it. The
	// live PVF writes it after the first [maze info] (格兰迪发电站 7136: the
	// table follows maze 0 while the twelve-pillar map 53462 sits on maze 1),
	// after the final [maze info] (particular hell, coloured [special dungeon]),
	// repeated identically under several mazes (发电站 7130-7135, inferno
	// 7527-7542) or before any maze (3080, 164). 86JP's DungeonFile keeps one
	// per-dungeon vector for all of them; promote every route-local copy into
	// the dungeon table and dedupe identical rows so repeated copies do not
	// stack their rates.
	for index := range result.Mazes {
		result.Metadata.SpecialPassiveObjectItems = append(
			result.Metadata.SpecialPassiveObjectItems,
			result.Mazes[index].SpecialPassiveObjectItems...,
		)
		result.Mazes[index].SpecialPassiveObjectItems = nil
	}
	result.Metadata.SpecialPassiveObjectItems = uniqueSpecialPassiveObjectItems(
		result.Metadata.SpecialPassiveObjectItems,
	)
}

func uniqueSpecialPassiveObjectItems(rows []SpecialPassiveObjectItem) []SpecialPassiveObjectItem {
	if len(rows) < 2 {
		return rows
	}
	type key struct {
		index, level, itemID, rate int64
	}
	seen := make(map[key]struct{}, len(rows))
	out := make([]SpecialPassiveObjectItem, 0, len(rows))
	for _, row := range rows {
		identity := key{row.Index, row.LevelOverride, row.ItemID, row.DropRate}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, row)
	}
	return out
}

// Some legacy dungeon files place dungeon-wide mechanism settings inside a
// syntactic [clear condition] block. They still configure the whole run rather
// than one destroy/hunt condition; preserve that real-PVF ownership explicitly
// instead of depending on the section's physical position after [maze info].
func parseScopedDungeonMechanismMetadata(result *Dungeon, sections []RawSection) {
	if result == nil {
		return
	}
	for _, section := range sections {
		if sectionKey(section.Name) != "limit time" || !scopeContains(section.Scope, "clear condition") {
			continue
		}
		values, valid := ints(section)
		if !valid || len(values) != 2 || values[0] <= 0 || values[1] <= 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "limit time must contain exactly two positive second durations"))
			continue
		}
		result.Metadata.LimitTime = DungeonLimitTime{
			PrimarySeconds: values[0], SecondarySeconds: values[1], Set: true,
		}
	}
}

func parseWarpMapConditions(result *Dungeon, sections []RawSection) {
	if result == nil {
		return
	}
	currentMaze := -1
	pendingMaze := -1
	var pending *Point
	for _, section := range sections {
		if sectionKey(section.Name) == "maze info" {
			if pending != nil {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{
					Section: "warp map condition", Message: "warp source has no destination",
				})
			}
			currentMaze++
			pendingMaze = -1
			pending = nil
			continue
		}
		if !scopeContains(section.Scope, "warp map condition") {
			continue
		}
		key := sectionKey(section.Name)
		if key != "source grid pos" && key != "dest grid pos" {
			continue
		}
		values, valid := ints(section)
		if !valid || len(values) != 2 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "warp grid position must contain exactly two integers"))
			if key == "source grid pos" {
				pending = nil
			}
			continue
		}
		point := Point{X: values[0], Y: values[1]}
		if key == "source grid pos" {
			if pending != nil {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, "warp source replaced before a destination was declared"))
			}
			pending = &point
			pendingMaze = currentMaze
			continue
		}
		if pending == nil {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "warp destination has no preceding source"))
			continue
		}
		condition := WarpMapCondition{
			Source: *pending, Destination: point,
		}
		result.WarpMapConditions = append(result.WarpMapConditions, condition)
		if pendingMaze >= 0 && pendingMaze < len(result.Mazes) {
			result.Mazes[pendingMaze].WarpMapConditions = append(
				result.Mazes[pendingMaze].WarpMapConditions,
				condition,
			)
		}
		pendingMaze = -1
		pending = nil
	}
	if pending != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{
			Section: "warp map condition", Message: "warp source has no destination",
		})
	}
}

func scopeOwnedByDungeonMetadata(scope []string) bool {
	for _, name := range scope {
		if isDungeonMetadataSection(name) {
			return true
		}
	}
	return false
}

func parseDungeonExtension(result *Dungeon, section RawSection) {
	if isClosingSection(section.Name) {
		return
	}
	result.UnknownSections = append(result.UnknownSections, cloneRawSection(section))
	result.Extensions = append(result.Extensions, structureExtension(section))
}

func parseDungeonMetadata(result *Dungeon, section RawSection) {
	key := sectionKey(section.Name)
	if isClosingSection(key) {
		return
	}
	if key == "blood dungeon" {
		parseBloodAltarDungeonKind(result, section)
		return
	}
	if key == "blood max round" {
		parseBloodAltarMaxRounds(result, section)
		return
	}
	// The current PVF writes Time Rift's opening quest as an explicitly closed
	// list.  It is still scalar dungeon metadata, not an opaque composite
	// structure; parse it before the generic block preservation below.
	if key == "inferno quest" {
		parseInfernoQuestIDs(result, section)
		return
	}
	if key == "special passive object item" {
		result.Metadata.SpecialPassiveObjectItems = append(
			result.Metadata.SpecialPassiveObjectItems,
			parseSpecialPassiveObjectItems(section, &result.Diagnostics)...,
		)
		return
	}
	if key == "event monster" || key == "event monster2" || key == "event monster3" {
		parsed, valid := ints(section)
		if !valid || len(parsed) == 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "event-monster definition must contain at least one integer"))
			return
		}
		result.Metadata.EventMonsters = append(result.Metadata.EventMonsters, DungeonEventMonsterDefinition{
			Kind: key, Values: append([]int64(nil), parsed...),
		})
		return
	}
	if key == "event monster random map" {
		parsed, valid := ints(section)
		if !valid || len(parsed) != 1 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "event-monster random-map selector must contain exactly one integer"))
			return
		}
		result.Metadata.EventMonsterRandomMap = OptionalInt{Value: parsed[0], Set: true}
		return
	}
	if key == "schedule" {
		parsed, valid := ints(section)
		if !valid || len(parsed) == 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "schedule must contain at least one integer"))
			return
		}
		result.Metadata.ScheduleIDs = append(result.Metadata.ScheduleIDs, parsed...)
		return
	}
	if section.Block {
		result.OpaqueSections = append(result.OpaqueSections, cloneRawSection(section))
		result.Extensions = append(result.Extensions, structureExtension(section))
		return
	}
	if _, ok := opaqueDungeonSections[key]; ok {
		result.OpaqueSections = append(result.OpaqueSections, cloneRawSection(section))
		return
	}
	if _, ok := dungeonIntegerSections[key]; ok {
		value, ok := firstInt(section)
		if !ok {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected integer metadata value"))
			return
		}
		if result.Metadata.IntegerValues == nil {
			result.Metadata.IntegerValues = make(map[string]int64)
		}
		result.Metadata.IntegerValues[key] = value
		switch key {
		case "coin limit":
			// [coin limit]：一次进图最多能用几个复活币。卢克三个圣所各写 8，
			// 二阶段的 PHASE COIN LIMIT 20 是另一层（全队总次数）。
			// 仅新增这一条，上游同版本里的其它分支（遗忘之地 fever time 等）本树没有，不引入。
			result.Metadata.CoinLimit = OptionalInt{Value: value, Set: true}
		case "hell party required item count":
			result.Metadata.HellPartyRequiredItemCount = OptionalInt{Value: value, Set: true}
		case "tournament round fatigue":
			result.Metadata.TournamentRoundFatigue = value
		case "tournament clear reward gold rate":
			result.Metadata.TournamentGoldRate = value
		}
		return
	}
	if _, ok := dungeonNumberSections[key]; ok {
		value, ok := firstNumber(section)
		if !ok {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected numeric metadata value"))
			return
		}
		if result.Metadata.NumberValues == nil {
			result.Metadata.NumberValues = make(map[string]float64)
		}
		result.Metadata.NumberValues[key] = value
		return
	}
	if _, ok := dungeonFlagSections[key]; ok {
		if result.Metadata.Flags == nil {
			result.Metadata.Flags = make(map[string]bool)
		}
		result.Metadata.Flags[key] = true
		switch key {
		case "inferno dungeon":
			result.Metadata.InfernoDungeon = true
		case "select dungeon normal disable":
			result.Metadata.SelectDungeonNormalDisabled = true
		}
		return
	}
	if _, ok := dungeonTextSections[key]; ok {
		parsed := texts(section)
		if len(parsed) == 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected text metadata value"))
			return
		}
		if result.Metadata.TextValues == nil {
			result.Metadata.TextValues = make(map[string][]string)
		}
		result.Metadata.TextValues[key] = append(result.Metadata.TextValues[key], parsed...)
		return
	}
	if _, ok := dungeonCoreSections[key]; !ok {
		result.UnknownSections = append(result.UnknownSections, cloneRawSection(section))
		result.Extensions = append(result.Extensions, structureExtension(section))
		return
	}

	switch key {
	case "name":
		setText(&result.Metadata.Name, section, &result.Diagnostics)
	case "explain":
		setText(&result.Metadata.Explain, section, &result.Diagnostics)
	case "cutscene image":
		if value, ok := firstText(section); ok {
			result.Metadata.CutsceneImage.Path = value
		} else {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected cutscene image path"))
		}
		parsed, _ := ints(section)
		result.Metadata.CutsceneImage.Params = append(result.Metadata.CutsceneImage.Params, parsed...)
	case "minimap image":
		setText(&result.Metadata.MinimapImage, section, &result.Diagnostics)
	case "entering title":
		setText(&result.Metadata.EnteringTitle, section, &result.Diagnostics)
	case "minimum required level":
		setOptionalInt(&result.Metadata.MinimumRequiredLevel, section, &result.Diagnostics)
	case "basis level":
		setOptionalInt(&result.Metadata.BasisLevel, section, &result.Diagnostics)
	case "experience increasing point":
		setOptionalNumber(&result.Metadata.ExperienceIncreasingPoint, section, &result.Diagnostics)
	case "background pos":
		setOptionalInt(&result.Metadata.BackgroundPosition, section, &result.Diagnostics)
	case "dungeon type":
		setText(&result.Metadata.DungeonType, section, &result.Diagnostics)
	case "champion":
		result.Metadata.Champion = appendInts(result.Metadata.Champion, section, &result.Diagnostics)
	case "pathgate object":
		result.Metadata.PathgateObjects = appendInts(result.Metadata.PathgateObjects, section, &result.Diagnostics)
	case "worldmap pattern info":
		result.Metadata.WorldMapPatternInfo = append(result.Metadata.WorldMapPatternInfo, cloneRawSection(section))
	case "worldmap info":
		result.Metadata.WorldMapInfo = append(result.Metadata.WorldMapInfo, cloneRawSection(section))
	case "difficulty":
		setOptionalInt(&result.Metadata.Difficulty, section, &result.Diagnostics)
	case "difficulty level":
		result.Metadata.DifficultyLevels = appendInts(result.Metadata.DifficultyLevels, section, &result.Diagnostics)
	case "designate dungeon difficulty":
		result.Metadata.DesignatedDifficulties = appendInts(result.Metadata.DesignatedDifficulties, section, &result.Diagnostics)
	case "tutorial dungeon":
		setOptionalInt(&result.Metadata.TutorialDungeon, section, &result.Diagnostics)
	case "no fatigue":
		result.Metadata.NoFatigue = true
	case "named monster":
		result.Metadata.NamedMonsters = appendInts(result.Metadata.NamedMonsters, section, &result.Diagnostics)
	case "recommended level":
		result.Metadata.RecommendedLevels = appendInts(result.Metadata.RecommendedLevels, section, &result.Diagnostics)
	case "limit party count":
		setOptionalInt(&result.Metadata.LimitPartyCount, section, &result.Diagnostics)
	case "lost land parameters":
		values, valid := ints(section)
		if !valid || len(values) != 2 || values[0] <= 0 || values[1] <= 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "lost land parameters must contain exactly two positive millisecond durations"))
			return
		}
		result.Metadata.LostLandParameters = LostLandParameters{
			DayMilliseconds: values[0], NightMilliseconds: values[1], Set: true,
		}
	case "limit time":
		values, valid := ints(section)
		if !valid || len(values) != 2 || values[0] <= 0 || values[1] <= 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "limit time must contain exactly two positive second durations"))
			return
		}
		result.Metadata.LimitTime = DungeonLimitTime{
			PrimarySeconds: values[0], SecondarySeconds: values[1], Set: true,
		}
	}
}

func parseBloodAltarDungeonKind(result *Dungeon, section RawSection) {
	if result.Metadata.Flags == nil {
		result.Metadata.Flags = make(map[string]bool)
	}
	result.Metadata.Flags["blood dungeon"] = true

	tokens := values(section)
	if len(tokens) != 1 || !bloodAltarInt32Token(tokens[0]) {
		result.Diagnostics = append(result.Diagnostics, diagnostic(section, "blood dungeon kind must be exactly one integer (1 or 2)"))
		return
	}
	kind := BloodAltarDungeonKind(tokens[0].Int)
	if kind != BloodAltarDungeonKindEndless && kind != BloodAltarDungeonKindUltimate {
		result.Diagnostics = append(result.Diagnostics, diagnostic(section, "blood dungeon kind must be 1 (Endless) or 2 (Ultimate)"))
		return
	}
	result.BloodAltarKind = kind
}

func parseBloodAltarMaxRounds(result *Dungeon, section RawSection) {
	tokens := values(section)
	if len(tokens) != 1 || !bloodAltarInt32Token(tokens[0]) || tokens[0].Int <= 0 || tokens[0].Int > 255 {
		result.Diagnostics = append(result.Diagnostics, diagnostic(section, "blood max round must be exactly one integer in the range 1..255"))
		return
	}
	if result.Metadata.IntegerValues == nil {
		result.Metadata.IntegerValues = make(map[string]int64)
	}
	result.Metadata.IntegerValues["blood max round"] = tokens[0].Int
	result.BloodMaxRounds = tokens[0].Int
}

func parseTournamentDungeonSections(result *Dungeon, sections []RawSection) {
	if result == nil {
		return
	}
	for _, section := range sections {
		key := sectionKey(section.Name)
		values := make([]int64, 0, len(section.Tokens))
		for _, token := range section.Tokens {
			if token.Kind == dnfpvf.TokenInt {
				values = append(values, token.Int)
			}
		}
		switch key {
		case "/reward item rate":
			if !scopeContains(section.Scope, "result card") || len(values) == 0 {
				continue
			}
			if len(values)%4 != 0 {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, "tournament result card rows must have width 4"))
				continue
			}
			for i := 0; i < len(values); i += 4 {
				result.Metadata.TournamentResultCards = append(result.Metadata.TournamentResultCards, TournamentResultCard{
					ResultKey: values[i], GoldWeight: values[i+1], ItemWeight: values[i+2], EmptyWeight: values[i+3],
				})
			}
		case "reward item rate":
			if !scopeContains(section.Scope, "result card") {
				continue
			}
			if len(values) == 0 || len(values)%3 != 0 {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, "tournament reward item rows must have width 3"))
				continue
			}
			for i := 0; i < len(values); i += 3 {
				result.Metadata.TournamentRewardItemRates = append(result.Metadata.TournamentRewardItemRates, TournamentRewardItemRate{
					ItemID: values[i], Weight: values[i+1], Count: values[i+2],
				})
			}
		case "tournament clear reward exp":
			if len(values) == 0 || len(values)%2 != 0 {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, "tournament clear reward experience rows must have width 2"))
				continue
			}
			for i := 0; i < len(values); i += 2 {
				result.Metadata.TournamentClearRewardExp = append(result.Metadata.TournamentClearRewardExp, TournamentRoundExperience{
					CompletedRounds: values[i], Experience: values[i+1],
				})
			}
		case "required item", "event required item", "added required item":
			if len(values) == 0 || len(values)%3 != 0 {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, key+" rows must have width 3"))
				continue
			}
			target := &result.Metadata.RequiredItems
			switch key {
			case "event required item":
				target = &result.Metadata.EventRequiredItems
			case "added required item":
				target = &result.Metadata.AddedRequiredItems
			}
			for i := 0; i < len(values); i += 3 {
				if values[i] > 0 && values[i+1] > 0 {
					*target = append(*target, TicketItem{ItemID: values[i], Count: values[i+1]})
				}
			}
		}
	}
}

func parseInfernoQuestIDs(result *Dungeon, section RawSection) {
	values, valid := ints(section)
	if !valid || len(values) == 0 {
		result.Diagnostics = append(result.Diagnostics, diagnostic(section, "inferno quest must contain one or more quest IDs"))
		return
	}
	for _, value := range values {
		if value > 0 {
			result.Metadata.InfernoQuestIDs = append(result.Metadata.InfernoQuestIDs, value)
		}
	}
}

func parseMazeSection(maze *Maze, section RawSection) {
	maze.SourceSections = append(maze.SourceSections, cloneRawSection(section))
	key := sectionKey(section.Name)
	if isClosingSection(key) {
		return
	}
	if key == "boss room entrance condition" {
		// Keep the raw source beside the typed form, matching the other complex
		// maze mechanisms while avoiding its former dungeon-wide ownership.
		maze.OpaqueSections = append(maze.OpaqueSections, cloneRawSection(section))
		occurrences := 0
		for _, source := range maze.SourceSections {
			if sectionKey(source.Name) == key {
				occurrences++
			}
		}
		if occurrences != 1 {
			maze.BossRoomEntranceCondition = nil
			maze.Diagnostics = append(maze.Diagnostics, diagnostic(section, "maze must contain at most one boss-room entrance condition"))
			return
		}
		condition, err := parseBossRoomEntranceCondition(section)
		if err != nil {
			maze.Diagnostics = append(maze.Diagnostics, diagnostic(section, err.Error()))
			return
		}
		maze.BossRoomEntranceCondition = &condition
		return
	}
	if _, ok := knownMazeSections[key]; !ok {
		maze.UnknownSections = append(maze.UnknownSections, cloneRawSection(section))
		maze.Extensions = append(maze.Extensions, structureExtension(section))
		return
	}
	if _, ok := opaqueMazeSections[key]; ok {
		maze.OpaqueSections = append(maze.OpaqueSections, cloneRawSection(section))
		switch key {
		case "boss map specification":
			maze.BossMapSpecifications = append(maze.BossMapSpecifications, cloneRawSection(section))
		case "layered map specification":
			maze.LayeredMapSpecifications = append(maze.LayeredMapSpecifications, cloneRawSection(section))
		}
		return
	}
	switch key {
	case "size":
		parsed, valid := ints(section)
		if !valid || len(parsed) < 2 {
			maze.Diagnostics = append(maze.Diagnostics, diagnostic(section, "expected maze width and height"))
			return
		}
		maze.Width = OptionalInt{Value: parsed[0], Set: true}
		maze.Height = OptionalInt{Value: parsed[1], Set: true}
		if len(parsed) > 2 {
			maze.Diagnostics = append(maze.Diagnostics, diagnostic(section, "maze size has trailing values"))
		}
	case "maze chance rate":
		setOptionalInt(&maze.ChanceRate, section, &maze.Diagnostics)
	case "inferno special":
		maze.InfernoSpecial = true
	case "escape hell":
		setOptionalInt(&maze.EscapeHell, section, &maze.Diagnostics)
	case "greed":
		setText(&maze.Greed, section, &maze.Diagnostics)
	case "map specification":
		maze.MapSpecifications = append(maze.MapSpecifications, parseMapSpecifications(section, &maze.Diagnostics)...)
	case "start map":
		maze.Start = parseMazePoint(section, &maze.Diagnostics)
	case "boss map":
		maze.Boss = parseMazePoint(section, &maze.Diagnostics)
	case "hit count":
		maze.HitCount = appendInts(maze.HitCount, section, &maze.Diagnostics)
	case "seal door appear rate":
		setOptionalInt(&maze.SealDoorAppearRate, section, &maze.Diagnostics)
	case "seal door map index":
		setOptionalInt(&maze.SealDoorMapIndex, section, &maze.Diagnostics)
	case "seal door pos":
		maze.SealDoorPosition = appendInts(maze.SealDoorPosition, section, &maze.Diagnostics)
	case "quest connection":
		maze.QuestConnection = appendInts(maze.QuestConnection, section, &maze.Diagnostics)
	case "special passive object item":
		maze.SpecialPassiveObjectItems = append(
			maze.SpecialPassiveObjectItems,
			parseSpecialPassiveObjectItems(section, &maze.Diagnostics)...,
		)
	}
}

func parseBossRoomEntranceCondition(section RawSection) (BossRoomEntranceCondition, error) {
	tokens := values(section)
	position := 0
	readTag := func(expected string) (string, error) {
		if position >= len(tokens) {
			return "", fmt.Errorf("boss-room entrance condition is missing [%s]", expected)
		}
		tag, ok := bossRoomEntranceTag(tokens[position])
		if !ok || tag != expected {
			return "", fmt.Errorf("boss-room entrance condition expected [%s] at token %d", expected, position)
		}
		position++
		return tag, nil
	}
	readInt := func(label string) (int64, error) {
		if position >= len(tokens) || tokens[position].Kind != dnfpvf.TokenInt {
			return 0, fmt.Errorf("boss-room entrance condition expected integer %s at token %d", label, position)
		}
		value := tokens[position].Int
		position++
		return value, nil
	}

	action, err := readTag("clear action")
	if err != nil {
		return BossRoomEntranceCondition{}, err
	}
	if _, err := readTag("time condition"); err != nil {
		return BossRoomEntranceCondition{}, err
	}
	timeFirst, err := readInt("time-condition value 1")
	if err != nil {
		return BossRoomEntranceCondition{}, err
	}
	timeSecond, err := readInt("time-condition value 2")
	if err != nil {
		return BossRoomEntranceCondition{}, err
	}
	if _, err := readTag("hunt monster"); err != nil {
		return BossRoomEntranceCondition{}, err
	}
	targetCount, err := readInt("hunt-monster target count")
	if err != nil {
		return BossRoomEntranceCondition{}, err
	}
	remaining := len(tokens) - position
	if targetCount <= 0 || targetCount > int64(remaining/3) || remaining != int(targetCount)*3 {
		return BossRoomEntranceCondition{}, fmt.Errorf(
			"boss-room entrance condition hunt-monster count %d does not match %d trailing values",
			targetCount, remaining,
		)
	}

	condition := BossRoomEntranceCondition{
		Action:        action,
		TimeCondition: []int64{timeFirst, timeSecond},
		HuntMonsters:  make([]BossRoomEntranceHuntMonster, 0, targetCount),
	}
	for targetIndex := int64(0); targetIndex < targetCount; targetIndex++ {
		targetID, targetErr := readInt(fmt.Sprintf("hunt-monster target %d id", targetIndex))
		if targetErr != nil {
			return BossRoomEntranceCondition{}, targetErr
		}
		opaque, opaqueErr := readInt(fmt.Sprintf("hunt-monster target %d opaque value", targetIndex))
		if opaqueErr != nil {
			return BossRoomEntranceCondition{}, opaqueErr
		}
		count, countErr := readInt(fmt.Sprintf("hunt-monster target %d count", targetIndex))
		if countErr != nil {
			return BossRoomEntranceCondition{}, countErr
		}
		if targetID <= 0 || count <= 0 {
			return BossRoomEntranceCondition{}, fmt.Errorf(
				"boss-room entrance condition hunt-monster target %d must have positive id and count",
				targetIndex,
			)
		}
		condition.HuntMonsters = append(condition.HuntMonsters, BossRoomEntranceHuntMonster{
			TargetID: targetID,
			Opaque:   opaque,
			Count:    count,
		})
	}
	return condition, nil
}

func bossRoomEntranceTag(token dnfpvf.Token) (string, bool) {
	if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
		return "", false
	}
	value := strings.TrimSpace(token.Value)
	if len(value) < 3 || value[0] != '[' || value[len(value)-1] != ']' {
		return "", false
	}
	value = sectionKey(value[1 : len(value)-1])
	return value, value != ""
}

func parseSpecialPassiveObjectItems(section RawSection, diagnostics *[]Diagnostic) []SpecialPassiveObjectItem {
	values, valid := ints(section)
	if !valid {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected integer passive-object item groups"))
		return nil
	}
	// Record layout (86JP PvfLib DungeonFile.ParseSpecialPassiveObjectItem and
	// the official dungeonsample_new_drop.dgn): `<group label> <level override>
	// <item count>` followed by `<item id> <drop rate>` pairs. A map [item]
	// child selects a record by its position in this vector, so Index is the
	// record position; the live PVF lists records sequentially (including
	// empty ones such as `0 -1 0`) and the leading label matches the position
	// everywhere except dungeon 315, whose single record is labelled 1 while
	// its maps roll code 0.
	position := 0
	groupIndex := int64(0)
	var rows []SpecialPassiveObjectItem
	for position < len(values) {
		if position+3 > len(values) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("truncated passive-object item group at token %d", position)))
			return rows
		}
		levelOverride, count := values[position+1], values[position+2]
		position += 3
		if count < 0 || count > int64((len(values)-position)/2) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("invalid passive-object item count %d in group %d", count, groupIndex)))
			return rows
		}
		for itemIndex := int64(0); itemIndex < count; itemIndex++ {
			itemID, dropRate := values[position], values[position+1]
			position += 2
			if itemID <= 0 || dropRate < 0 || dropRate > 10000 {
				*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("invalid passive-object item in group %d at item %d", groupIndex, itemIndex)))
				continue
			}
			rows = append(rows, SpecialPassiveObjectItem{
				Index: groupIndex, LevelOverride: levelOverride,
				ItemID: itemID, DropRate: dropRate,
			})
		}
		groupIndex++
	}
	return rows
}

func parseMapSpecifications(section RawSection, diagnostics *[]Diagnostic) []MapSpecification {
	tokens := values(section)
	var out []MapSpecification
	for i := 0; i < len(tokens); {
		if !isText(tokens[i]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("expected specification type at token %d", i)))
			i++
			continue
		}
		if i+3 >= len(tokens) || !areInts(tokens[i+1:i+4]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed map specification at token %d", i)))
			i++
			continue
		}
		spec := MapSpecification{
			Type:              tokens[i].Value,
			Coordinate:        Point{X: tokens[i+1].Int, Y: tokens[i+2].Int},
			MapIDs:            []int64{tokens[i+3].Int},
			SectionOccurrence: section.Occurrence,
		}
		i += 4
		for i < len(tokens) && tokens[i].Kind == dnfpvf.TokenInt {
			spec.MapIDs = append(spec.MapIDs, tokens[i].Int)
			i++
		}
		out = append(out, spec)
	}
	return out
}

func parseMazeComplexSections(maze *Maze) {
	for pos := 0; pos < len(maze.SourceSections); pos++ {
		section := maze.SourceSections[pos]
		switch sectionKey(section.Name) {
		case "boss map specification":
			maze.BossSpecifications = append(
				maze.BossSpecifications,
				parseTypedOrImplicitSpecifications(section, "boss", &maze.Diagnostics)...,
			)
		case "layered map specification":
			maze.LayeredSpecifications = append(
				maze.LayeredSpecifications,
				parseTypedOrImplicitSpecifications(section, "layered", &maze.Diagnostics)...,
			)
		case "randomized object creation":
			script, next := parseRandomizedObjectScript(maze.SourceSections, pos, &maze.Diagnostics)
			maze.RandomizedObjects = append(maze.RandomizedObjects, script)
			pos = next
		case "clear condition":
			conditions, next := parseClearConditions(maze.SourceSections, pos, &maze.Diagnostics)
			maze.ClearConditions = append(maze.ClearConditions, conditions...)
			pos = next
		}
	}
}

func parseTypedOrImplicitSpecifications(section RawSection, implicitType string, diagnostics *[]Diagnostic) []MapSpecification {
	tokens := values(section)
	if len(tokens) > 0 && isText(tokens[0]) {
		return parseMapSpecifications(section, diagnostics)
	}
	if len(tokens) < 3 || !areInts(tokens[:3]) {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected x/y/map-id specification values"))
		return nil
	}
	mapIDs := make([]int64, 0, len(tokens)-2)
	for _, token := range tokens[2:] {
		if token.Kind != dnfpvf.TokenInt {
			*diagnostics = append(*diagnostics, diagnostic(section, "implicit specification contains non-integer map id"))
			return nil
		}
		mapIDs = append(mapIDs, token.Int)
	}
	return []MapSpecification{{
		Type: implicitType, Coordinate: Point{X: tokens[0].Int, Y: tokens[1].Int},
		MapIDs: mapIDs, SectionOccurrence: section.Occurrence,
	}}
}

func parseRandomizedObjectScript(sections []RawSection, start int, diagnostics *[]Diagnostic) (RandomizedObjectScript, int) {
	script := RandomizedObjectScript{}
	end := start
	for pos := start; pos < len(sections); pos++ {
		section := sections[pos]
		key := sectionKey(section.Name)
		script.SourceSections = append(script.SourceSections, cloneRawSection(section))
		end = pos
		if key == "/randomized object creation" {
			break
		}
		switch key {
		case "randomized object creation":
		case "select":
			setOptionalInt(&script.SelectCount, section, diagnostics)
		case "regenerate":
			setOptionalInt(&script.Regenerate, section, diagnostics)
		case "minimap icon":
			setOptionalInt(&script.MinimapIcon, section, diagnostics)
		case "object":
			object, objectEnd := parseRidableObject(sections, pos, diagnostics)
			script.Objects = append(script.Objects, object)
			for nested := pos + 1; nested <= objectEnd && nested < len(sections); nested++ {
				script.SourceSections = append(script.SourceSections, cloneRawSection(sections[nested]))
			}
			pos = objectEnd
			end = objectEnd
		default:
			if !isClosingSection(key) {
				script.UnknownSections = append(script.UnknownSections, cloneRawSection(section))
				script.Extensions = append(script.Extensions, structureExtension(section))
			}
		}
	}
	return script, end
}

func parseRidableObject(sections []RawSection, start int, diagnostics *[]Diagnostic) (RidableObject, int) {
	object := RidableObject{}
	end := start
	for pos := start; pos < len(sections); pos++ {
		section := sections[pos]
		key := sectionKey(section.Name)
		object.SourceSections = append(object.SourceSections, cloneRawSection(section))
		end = pos
		if key == "/object" {
			break
		}
		switch key {
		case "object", "team":
		case "map":
			object.MazeCoordinate = parsePoint(section, "maze map coordinate", diagnostics)
		case "index":
			setOptionalInt(&object.ObjectIndex, section, diagnostics)
		case "pos":
			object.Position = parsePoint(section, "object position", diagnostics)
		case "monster", "neutral", "character":
			object.Team = key
		default:
			if !isClosingSection(key) {
				object.UnknownSections = append(object.UnknownSections, cloneRawSection(section))
				object.Extensions = append(object.Extensions, structureExtension(section))
			}
		}
	}
	return object, end
}

func parseClearConditions(sections []RawSection, start int, diagnostics *[]Diagnostic) ([]ClearCondition, int) {
	var out []ClearCondition
	end := start
	for pos := start + 1; pos < len(sections); pos++ {
		section := sections[pos]
		key := sectionKey(section.Name)
		end = pos
		if key == "/clear condition" {
			break
		}
		switch key {
		case "destroy object", "seeking", "hunt monster", "hunt apc", "hunt boss":
			parsed, valid := ints(section)
			if !valid || len(parsed)%2 != 0 {
				*diagnostics = append(*diagnostics, diagnostic(section, "expected clear-condition target/count pairs"))
			}
			for index := 0; index+1 < len(parsed); index += 2 {
				out = append(out, ClearCondition{Type: key, TargetID: parsed[index], Count: parsed[index+1]})
			}
		}
	}
	return out, end
}

func parsePoint(section RawSection, label string, diagnostics *[]Diagnostic) *Point {
	parsed, valid := ints(section)
	if !valid || len(parsed) < 2 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected "+label+" x/y values"))
		return nil
	}
	if len(parsed) > 2 {
		*diagnostics = append(*diagnostics, diagnostic(section, label+" has trailing values"))
	}
	return &Point{X: parsed[0], Y: parsed[1]}
}

func parseMazePoint(section RawSection, diagnostics *[]Diagnostic) *MazePoint {
	parsed, valid := ints(section)
	if !valid || len(parsed) < 2 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected maze x/y coordinates"))
		return nil
	}
	return &MazePoint{X: parsed[0], Y: parsed[1], Params: append([]int64(nil), parsed[2:]...)}
}

func setOptionalNumber(target *OptionalNumber, section RawSection, diagnostics *[]Diagnostic) {
	value, ok := firstNumber(section)
	if !ok {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected numeric value"))
		return
	}
	target.Value = value
	target.Set = true
}

func firstNumber(section RawSection) (float64, bool) {
	for _, token := range section.Tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			return float64(token.Int), true
		case dnfpvf.TokenFloat:
			return token.Float, true
		}
	}
	return 0, false
}

func structureExtension(section RawSection) ExtensionSection {
	extension := ExtensionSection{
		Name: section.Name, Occurrence: section.Occurrence,
		Scope: append([]string(nil), section.Scope...), Block: section.Block,
		Tokens: append([]dnfpvf.Token(nil), section.Tokens...),
	}
	nonSymbols := 0
	allIntegers := true
	allNumbers := true
	allTexts := true
	for _, token := range section.Tokens {
		switch token.Kind {
		case dnfpvf.TokenInt:
			nonSymbols++
			extension.Integers = append(extension.Integers, token.Int)
			extension.Numbers = append(extension.Numbers, float64(token.Int))
			allTexts = false
		case dnfpvf.TokenFloat:
			nonSymbols++
			extension.Numbers = append(extension.Numbers, token.Float)
			allIntegers = false
			allTexts = false
		case dnfpvf.TokenString, dnfpvf.TokenIdent:
			nonSymbols++
			extension.Texts = append(extension.Texts, token.Value)
			allIntegers = false
			allNumbers = false
		case dnfpvf.TokenSymbol:
			extension.Symbols = append(extension.Symbols, token.Value)
		}
	}
	switch {
	case nonSymbols == 0 && len(extension.Symbols) == 0:
		extension.Kind = SectionFlag
	case allIntegers:
		extension.Kind = SectionIntegers
	case allNumbers:
		extension.Kind = SectionNumbers
	case allTexts:
		extension.Kind = SectionTexts
	default:
		extension.Kind = SectionMixed
	}
	return extension
}
