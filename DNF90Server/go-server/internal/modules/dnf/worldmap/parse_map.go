package worldmap

import (
	"fmt"
	"math"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

func ParseMap(id int64, mapPath string, doc *dnfpvf.Document) Map {
	result := Map{ID: id, Path: mapPath}
	sections, diagnostics := rawSections(doc)
	result.SourceSections = sections
	result.Diagnostics = append(result.Diagnostics, diagnostics...)

	inBackground := false
	backgroundIndex := -1
	for _, section := range sections {
		key := sectionKey(section.Name)
		if isClosingSection(key) {
			if key == "/background animation" {
				inBackground = false
				backgroundIndex = -1
			}
			continue
		}
		if inBackground {
			switch key {
			case "ani info":
				result.BackgroundAnimations = append(result.BackgroundAnimations, BackgroundAnimation{})
				backgroundIndex = len(result.BackgroundAnimations) - 1
			case "filename", "layer", "order":
				if backgroundIndex < 0 {
					result.Diagnostics = append(result.Diagnostics, diagnostic(section, "background field appears outside ani info"))
					continue
				}
				value, ok := firstText(section)
				if !ok {
					result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected text value"))
					continue
				}
				switch key {
				case "filename":
					result.BackgroundAnimations[backgroundIndex].Filename = value
				case "layer":
					result.BackgroundAnimations[backgroundIndex].Layer = value
				case "order":
					result.BackgroundAnimations[backgroundIndex].Order = value
				}
			}
			continue
		}
		if scopeContains(section.Scope, "summon") || (key == "summon" && section.Block) {
			continue
		}

		if _, ok := knownMapSections[key]; !ok {
			result.UnknownSections = append(result.UnknownSections, cloneRawSection(section))
			result.Extensions = append(result.Extensions, structureExtension(section))
			continue
		}
		if key == "tournament enemies" {
			if !parseTournamentEnemies(&result, section) {
				result.TournamentMalformed = true
			}
			continue
		}
		if key == "tournament start area" {
			if !parseTournamentStartAreas(&result, section) {
				result.TournamentMalformed = true
			}
			continue
		}
		if isBloodAltarMapSection(key) {
			// These four mutually-referencing tables are parsed together after
			// all sections have been collected, so phase indexes can be checked
			// against the complete corresponding monster table.
			continue
		}
		if _, ok := opaqueMapSections[key]; ok {
			result.OpaqueSections = append(result.OpaqueSections, cloneRawSection(section))
			result.Extensions = append(result.Extensions, structureExtension(section))
			continue
		}

		switch key {
		case "map name":
			setText(&result.Name, section, &result.Diagnostics)
		case "player number":
			result.PlayerNumber = appendInts(result.PlayerNumber, section, &result.Diagnostics)
		case "pvp start area":
			result.PVPStartArea = appendInts(result.PVPStartArea, section, &result.Diagnostics)
		case "dungeon":
			result.DungeonIDs = appendInts(result.DungeonIDs, section, &result.Diagnostics)
			if len(result.DungeonIDs) > 0 && !result.DungeonID.Set {
				result.DungeonID = OptionalInt{Value: result.DungeonIDs[0], Set: true}
			}
		case "type":
			setText(&result.Type, section, &result.Diagnostics)
		case "greed":
			setText(&result.Greed, section, &result.Diagnostics)
		case "tile":
			result.Tiles = append(result.Tiles, texts(section)...)
		case "tile files":
			result.TileFiles = append(result.TileFiles, parseTileFiles(section, &result.Diagnostics)...)
		case "tile map":
			row, valid := ints(section)
			if !valid {
				result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected integer tile-map row"))
			}
			result.TileMapRows = append(result.TileMapRows, row)
		case "far sight scroll":
			setOptionalInt(&result.FarSightScroll, section, &result.Diagnostics)
		case "middle sight scroll":
			setOptionalInt(&result.MiddleSightScroll, section, &result.Diagnostics)
		case "near sight scroll":
			setOptionalInt(&result.NearSightScroll, section, &result.Diagnostics)
		case "background animation":
			inBackground = true
			backgroundIndex = -1
		case "pathgate pos":
			result.PathgatePositions = append(result.PathgatePositions, parsePoints(section, &result.Diagnostics)...)
		case "pathgate object":
			result.PathgateObjects = appendInts(result.PathgateObjects, section, &result.Diagnostics)
		case "sound":
			result.Sounds = append(result.Sounds, texts(section)...)
		case "animation":
			result.AnimationObjects = append(result.AnimationObjects, parseAnimationObjects(section, &result.Diagnostics)...)
		case "passive object":
			result.PassiveObjects = append(result.PassiveObjects, parsePassiveObjects(section, &result.Diagnostics)...)
		case "special passive object":
			result.SpecialPassiveObjects = append(result.SpecialPassiveObjects, parseSpecialPassiveObjects(section, &result.Diagnostics)...)
		case "monster":
			result.Monsters = append(result.Monsters, parseMonsters(section, &result.Diagnostics)...)
		case "event monster position":
			result.EventMonsterPositions = append(result.EventMonsterPositions, parsePoints3(section, &result.Diagnostics)...)
		case "npc":
			result.NPCs = append(result.NPCs, parseNPCs(section, &result.Diagnostics)...)
		case "ai character":
			result.AICharacters = append(result.AICharacters, parseAICharacters(section, &result.Diagnostics)...)
		case "town movable area":
			result.TownMovableAreas = append(result.TownMovableAreas, parseMovableAreas(section, &result.Diagnostics)...)
		case "dungeon movable area":
			result.DungeonMovableAreas = append(result.DungeonMovableAreas, parseDungeonMovableAreas(section, &result.Diagnostics)...)
		case "fix champion":
			setOptionalInt(&result.FixChampion, section, &result.Diagnostics)
		case "heroes mode map index":
			setOptionalInt(&result.HeroesModeMapIndex, section, &result.Diagnostics)
		case "background correction":
			setOptionalInt(&result.BackgroundCorrection, section, &result.Diagnostics)
		case "background pos":
			setOptionalInt(&result.BackgroundPos, section, &result.Diagnostics)
		case "foreground pattern alpha":
			setOptionalInt(&result.ForegroundPatternAlpha, section, &result.Diagnostics)
		case "apc random point":
			setOptionalInt(&result.APCRandomPoint, section, &result.Diagnostics)
		case "monster lock":
			setOptionalInt(&result.MonsterLock, section, &result.Diagnostics)
		case "draw monster count":
			setOptionalInt(&result.DrawMonsterCount, section, &result.Diagnostics)
		case "sort bottom":
			setOptionalInt(&result.SortBottom, section, &result.Diagnostics)
		case "add gravity":
			setOptionalInt(&result.AddGravity, section, &result.Diagnostics)
		case "jump power rate":
			setOptionalInt(&result.JumpPowerRate, section, &result.Diagnostics)
		case "dungeon start area":
			result.DungeonStartArea = appendInts(result.DungeonStartArea, section, &result.Diagnostics)
		case "screen pos":
			result.ScreenPosition = appendInts(result.ScreenPosition, section, &result.Diagnostics)
		case "monster team":
			result.MonsterTeam = appendInts(result.MonsterTeam, section, &result.Diagnostics)
		case "pvp practice start area":
			result.PVPPracticeStartArea = appendInts(result.PVPPracticeStartArea, section, &result.Diagnostics)
		case "virtual movable area":
			result.VirtualMovableArea = appendInts(result.VirtualMovableArea, section, &result.Diagnostics)
		case "opening bgm":
			result.OpeningBGM = append(result.OpeningBGM, texts(section)...)
		case "map loading image path":
			setText(&result.MapLoadingImagePath, section, &result.Diagnostics)
		case "basic action":
			setText(&result.BasicAction, section, &result.Diagnostics)
		case "absolute start path":
			result.AbsoluteStartPath = append(result.AbsoluteStartPath, texts(section)...)
		case "random start map":
			setOptionalInt(&result.RandomStartMap, section, &result.Diagnostics)
		case "pathgate recognize range":
			setOptionalInt(&result.PathgateRecognizeRange, section, &result.Diagnostics)
		default:
			setMapFlag(&result.Flags, key)
		}
	}
	parseBloodAltarMapSections(&result, sections)
	result.Summons = parseMapSummons(sections, &result.Diagnostics)
	attachHellParty(sections, &result)
	result.Portals = buildPortals(result.PathgatePositions, result.PathgateObjects)
	return result
}

func isBloodAltarMapSection(key string) bool {
	switch sectionKey(key) {
	case "blood monster", "blood phase time", "ultimate monster", "ultimate phase time":
		return true
	default:
		return false
	}
}

func parseBloodAltarMapSections(result *Map, sections []RawSection) {
	if result == nil {
		return
	}
	result.BloodMonsters = parseBloodAltarMonsterSections(result, sections, "blood monster")
	result.UltimateMonsters = parseBloodAltarMonsterSections(result, sections, "ultimate monster")
	result.BloodPhases = parseBloodAltarPhaseSections(
		result, sections, "blood phase time", result.BloodMonsters, false,
	)
	result.UltimatePhases = parseBloodAltarPhaseSections(
		result, sections, "ultimate phase time", result.UltimateMonsters, true,
	)
}

func parseBloodAltarMonsterSections(result *Map, sections []RawSection, name string) []BloodAltarMonster {
	var parsed []BloodAltarMonster
	valid := true
	for _, section := range sections {
		if sectionKey(section.Name) != name {
			continue
		}
		rows, err := parseBloodAltarMonsterTable(section)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, err.Error()))
			valid = false
			continue
		}
		parsed = append(parsed, rows...)
	}
	if !valid {
		return nil
	}
	return parsed
}

func parseBloodAltarMonsterTable(section RawSection) ([]BloodAltarMonster, error) {
	const width = 10
	tokens := values(section)
	if len(tokens) == 0 || len(tokens)%width != 0 {
		return nil, fmt.Errorf("blood altar monster table must contain complete 10-field rows; got %d fields", len(tokens))
	}
	parsed := make([]BloodAltarMonster, 0, len(tokens)/width)
	for offset := 0; offset < len(tokens); offset += width {
		row := tokens[offset : offset+width]
		for field, token := range row {
			if !bloodAltarInt32Token(token) {
				return nil, fmt.Errorf("blood altar monster row %d field %d must be an int32", offset/width, field)
			}
		}
		if row[0].Int <= 0 ||
			!bloodAltarCoordinate(row[2].Int) ||
			!bloodAltarCoordinate(row[3].Int) ||
			!bloodAltarCoordinate(row[4].Int) ||
			row[5].Int < 0 || row[6].Int < 0 || row[7].Int < 0 || row[9].Int < 0 {
			return nil, fmt.Errorf("blood altar monster row %d contains an invalid code, coordinate, duration or count", offset/width)
		}
		parsed = append(parsed, BloodAltarMonster{
			MonsterCode:               row[0].Int,
			TemplateType:              row[1].Int,
			Position:                  Point3{X: row[2].Int, Y: row[3].Int, Z: row[4].Int},
			DurationMilliseconds:      row[5].Int,
			SpawnIntervalMilliseconds: row[6].Int,
			BaseSpawnCount:            row[7].Int,
			SpawnCountIncrement:       row[8].Int,
			BatchCount:                row[9].Int,
		})
	}
	return parsed, nil
}

func parseBloodAltarPhaseSections(
	result *Map,
	sections []RawSection,
	name string,
	monsters []BloodAltarMonster,
	ultimate bool,
) []BloodAltarPhase {
	var matching []RawSection
	for _, section := range sections {
		if sectionKey(section.Name) == name {
			matching = append(matching, section)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if len(monsters) == 0 {
		for _, section := range matching {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "blood altar phase table requires a valid matching monster table"))
		}
		return nil
	}

	var parsed []BloodAltarPhase
	valid := true
	for _, section := range matching {
		rows, err := parseBloodAltarPhaseTable(section, len(monsters), ultimate)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, err.Error()))
			valid = false
			continue
		}
		parsed = append(parsed, rows...)
	}
	if !valid {
		return nil
	}
	return parsed
}

func parseBloodAltarPhaseTable(section RawSection, monsterCount int, ultimate bool) ([]BloodAltarPhase, error) {
	width := 6
	if ultimate {
		width = 7
	}
	tokens := values(section)
	if len(tokens) == 0 || len(tokens)%width != 0 {
		return nil, fmt.Errorf("blood altar phase table must contain complete %d-field rows; got %d fields", width, len(tokens))
	}
	parsed := make([]BloodAltarPhase, 0, len(tokens)/width)
	for offset := 0; offset < len(tokens); offset += width {
		row := tokens[offset : offset+width]
		for _, field := range []int{0, 1, 2, 4, 5} {
			if !bloodAltarInt32Token(row[field]) {
				return nil, fmt.Errorf("blood altar phase row %d field %d must be an int32", offset/width, field)
			}
		}
		scale, ok := bloodAltarScale(row[3])
		if !ok {
			return nil, fmt.Errorf("blood altar phase row %d scale must be a finite positive number", offset/width)
		}
		if row[0].Int < 0 || row[1].Int < 0 || row[1].Int >= int64(monsterCount) ||
			row[2].Int < 0 || row[5].Int < 0 || row[5].Int > 10 {
			return nil, fmt.Errorf("blood altar phase row %d contains an invalid round, monster index, delay or concurrency", offset/width)
		}
		phase := BloodAltarPhase{
			Round:                row[0].Int,
			MonsterTemplateIndex: row[1].Int,
			DelayMilliseconds:    row[2].Int,
			Scale:                scale,
			Flag:                 row[4].Int,
			ConcurrentPhaseCount: row[5].Int,
		}
		if ultimate {
			if !isText(row[6]) {
				return nil, fmt.Errorf("blood altar phase row %d difficulty must be A or B", offset/width)
			}
			switch strings.ToUpper(strings.TrimSpace(row[6].Value)) {
			case string(BloodAltarDifficultyA):
				phase.Difficulty = BloodAltarDifficultyA
			case string(BloodAltarDifficultyB):
				phase.Difficulty = BloodAltarDifficultyB
			default:
				return nil, fmt.Errorf("blood altar phase row %d difficulty must be A or B", offset/width)
			}
		}
		parsed = append(parsed, phase)
	}
	return parsed, nil
}

func bloodAltarInt32Token(token dnfpvf.Token) bool {
	const (
		minInt32 = int64(-1 << 31)
		maxInt32 = int64(1<<31 - 1)
	)
	return token.Kind == dnfpvf.TokenInt && token.Int >= minInt32 && token.Int <= maxInt32
}

func bloodAltarCoordinate(value int64) bool {
	return value >= 0 && value <= 1<<16-1
}

func bloodAltarScale(token dnfpvf.Token) (float64, bool) {
	var value float64
	switch token.Kind {
	case dnfpvf.TokenInt:
		value = float64(token.Int)
	case dnfpvf.TokenFloat:
		value = token.Float
	default:
		return 0, false
	}
	return value, value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func parseTournamentEnemies(result *Map, section RawSection) bool {
	if result == nil || len(section.Tokens) < 5 {
		return false
	}
	values := make([]dnfpvf.Token, 0, len(section.Tokens))
	for _, token := range section.Tokens {
		if token.Kind != dnfpvf.TokenSymbol {
			values = append(values, token)
		}
	}
	if len(values) < 5 || values[0].Kind != dnfpvf.TokenInt ||
		(values[1].Kind != dnfpvf.TokenIdent && values[1].Kind != dnfpvf.TokenString) || (len(values)-2)%3 != 0 {
		return false
	}
	partyCount := values[0].Int
	kind := strings.TrimSpace(values[1].Value)
	if partyCount <= 0 || (!strings.EqualFold(kind, "[monster]") && !strings.EqualFold(kind, "[apc]")) {
		return false
	}
	for i := 2; i < len(values); i += 3 {
		if values[i].Kind != dnfpvf.TokenInt || values[i+1].Kind != dnfpvf.TokenInt {
			return false
		}
		name := strings.TrimSpace(values[i+2].Value)
		if name == "" || (values[i+2].Kind != dnfpvf.TokenIdent && values[i+2].Kind != dnfpvf.TokenString) {
			return false
		}
		result.TournamentEnemies = append(result.TournamentEnemies, TournamentEnemyCandidate{
			PartyCount: partyCount, Kind: kind, Code: values[i].Int, Strength: values[i+1].Int, Name: name,
		})
	}
	return len(result.TournamentEnemies) > 0
}

func parseTournamentStartAreas(result *Map, section RawSection) bool {
	if result == nil {
		return false
	}
	values := make([]int64, 0, len(section.Tokens))
	for _, token := range section.Tokens {
		if token.Kind == dnfpvf.TokenInt {
			values = append(values, token.Int)
		}
	}
	if len(values) == 0 || len(values)%4 != 0 {
		return false
	}
	for i := 0; i < len(values); i += 4 {
		if values[i] <= 0 || values[i+1] < 0 || values[i+2] < 0 || values[i+3] < 0 {
			return false
		}
		result.TournamentStartAreas = append(result.TournamentStartAreas, TournamentStartArea{
			PartyCount: values[i], X: values[i+1], Y: values[i+2], Direction: values[i+3],
		})
	}
	return len(result.TournamentStartAreas) > 0
}

func parseTileFiles(section RawSection, diagnostics *[]Diagnostic) []TileFile {
	tokens := values(section)
	var out []TileFile
	for index := 0; index < len(tokens); {
		if index+1 >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt || !isText(tokens[index+1]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed tile-file pair at token %d", index)))
			index++
			continue
		}
		out = append(out, TileFile{Index: tokens[index].Int, Path: tokens[index+1].Value})
		index += 2
	}
	return out
}

func parseMapSummons(sections []RawSection, diagnostics *[]Diagnostic) []MapSummon {
	var out []MapSummon
	current := -1
	for _, section := range sections {
		key := sectionKey(section.Name)
		if key == "summon" && section.Block {
			out = append(out, MapSummon{})
			current = len(out) - 1
			out[current].SourceSections = append(out[current].SourceSections, cloneRawSection(section))
			continue
		}
		if current < 0 {
			continue
		}
		if key == "/summon" {
			out[current].SourceSections = append(out[current].SourceSections, cloneRawSection(section))
			current = -1
			continue
		}
		if isClosingSection(key) {
			out[current].SourceSections = append(out[current].SourceSections, cloneRawSection(section))
			continue
		}
		if !scopeContains(section.Scope, "summon") {
			continue
		}
		summon := &out[current]
		summon.SourceSections = append(summon.SourceSections, cloneRawSection(section))
		extension := structureExtension(section)
		summon.Extensions = append(summon.Extensions, extension)
		if scopeContains(section.Scope, "info") || key == "info" {
			summon.Info = append(summon.Info, extension)
		}
		switch key {
		case "summon key":
			setOptionalInt(&summon.Key, section, diagnostics)
		case "position":
			summon.Position = appendInts(summon.Position, section, diagnostics)
		case "type":
			setText(&summon.Type, section, diagnostics)
		case "itemcount":
			setOptionalInt(&summon.ItemCount, section, diagnostics)
		case "index":
			setOptionalInt(&summon.Index, section, diagnostics)
		case "life time":
			setOptionalInt(&summon.LifeTime, section, diagnostics)
		case "info", "distance":
		default:
			summon.UnknownSections = append(summon.UnknownSections, cloneRawSection(section))
		}
	}
	return out
}

func scopeContains(scope []string, name string) bool {
	want := sectionKey(name)
	for _, value := range scope {
		if sectionKey(value) == want {
			return true
		}
	}
	return false
}

func setOptionalInt(target *OptionalInt, section RawSection, diagnostics *[]Diagnostic) {
	value, ok := firstInt(section)
	if !ok {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected integer value"))
		return
	}
	target.Value = value
	target.Set = true
}

func setText(target *string, section RawSection, diagnostics *[]Diagnostic) {
	value, ok := firstText(section)
	if !ok {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected text value"))
		return
	}
	*target = value
}

func appendInts(target []int64, section RawSection, diagnostics *[]Diagnostic) []int64 {
	parsed, valid := ints(section)
	if !valid {
		*diagnostics = append(*diagnostics, diagnostic(section, "contains non-integer values"))
	}
	return append(target, parsed...)
}

func parsePoints(section RawSection, diagnostics *[]Diagnostic) []Point {
	parsed, valid := ints(section)
	if !valid || len(parsed)%2 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected x/y integer pairs"))
	}
	out := make([]Point, 0, len(parsed)/2)
	for i := 0; i+1 < len(parsed); i += 2 {
		out = append(out, Point{X: parsed[i], Y: parsed[i+1]})
	}
	return out
}

func parsePoints3(section RawSection, diagnostics *[]Diagnostic) []Point3 {
	parsed, valid := ints(section)
	if !valid || len(parsed)%3 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected x/y/z integer records"))
	}
	out := make([]Point3, 0, len(parsed)/3)
	for i := 0; i+2 < len(parsed); i += 3 {
		out = append(out, Point3{X: parsed[i], Y: parsed[i+1], Z: parsed[i+2]})
	}
	return out
}

func parseAnimationObjects(section RawSection, diagnostics *[]Diagnostic) []AnimationObject {
	tokens := values(section)
	var out []AnimationObject
	for i := 0; i+4 < len(tokens); i += 5 {
		if !isText(tokens[i]) || !isText(tokens[i+1]) || !areInts(tokens[i+2:i+5]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed animation record at token %d", i)))
			break
		}
		out = append(out, AnimationObject{
			Filename: tokens[i].Value, Layer: tokens[i+1].Value,
			Position: Point3{X: tokens[i+2].Int, Y: tokens[i+3].Int, Z: tokens[i+4].Int},
		})
	}
	if len(tokens)%5 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "animation section has trailing tokens"))
	}
	return out
}

func parsePassiveObjects(section RawSection, diagnostics *[]Diagnostic) []PassiveObject {
	parsed, valid := ints(section)
	if !valid || len(parsed)%4 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected object_id/x/y/flags integer records"))
	}
	out := make([]PassiveObject, 0, len(parsed)/4)
	for i := 0; i+3 < len(parsed); i += 4 {
		out = append(out, PassiveObject{ObjectID: parsed[i], X: parsed[i+1], Y: parsed[i+2], Flags: parsed[i+3]})
	}
	return out
}

func parseSpecialPassiveObjects(section RawSection, diagnostics *[]Diagnostic) []SpecialPassiveObject {
	tokens := values(section)
	if out, ok := parseZeroSpawnSpecialPassiveObjectRows(tokens); ok {
		return out
	}
	if len(tokens)%4 == 0 && areInts(tokens) {
		out := make([]SpecialPassiveObject, 0, len(tokens)/4)
		for i := 0; i+3 < len(tokens); i += 4 {
			out = append(out, SpecialPassiveObject{PassiveObject: PassiveObject{
				ObjectID: tokens[i].Int, X: tokens[i+1].Int, Y: tokens[i+2].Int, Flags: tokens[i+3].Int,
			}})
		}
		return out
	}
	var out []SpecialPassiveObject
	for i := 0; i < len(tokens); {
		if i+3 >= len(tokens) || !areInts(tokens[i:i+4]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed special object at token %d", i)))
			break
		}
		obj := SpecialPassiveObject{PassiveObject: PassiveObject{
			ObjectID: tokens[i].Int, X: tokens[i+1].Int, Y: tokens[i+2].Int, Flags: tokens[i+3].Int,
		}}
		i += 4
		if i >= len(tokens) || tokens[i].Kind != dnfpvf.TokenInt {
			out = append(out, obj)
			continue
		}
		spawnCount := tokens[i].Int
		if spawnCount < 0 || spawnCount > int64((len(tokens)-i-1)/6) {
			out = append(out, obj)
			continue
		}
		i++
		for n := int64(0); n < spawnCount; n++ {
			if i < len(tokens) && isHellPartyOpen(tokens[i]) {
				i++
				for {
					if i >= len(tokens) {
						*diagnostics = append(*diagnostics, diagnostic(section, "hellparty block is not closed"))
						return out
					}
					if isHellPartyClose(tokens[i]) {
						i++
						break
					}
					if i+2 >= len(tokens) || !areInts(tokens[i:i+3]) {
						*diagnostics = append(*diagnostics, diagnostic(section, "malformed hellparty entry"))
						return out
					}
					obj.HellParty = append(obj.HellParty, HellPartyEntry{
						GroupID: tokens[i].Int,
						Rate:    tokens[i+1].Int,
						Order:   tokens[i+2].Int,
					})
					i += 3
				}
				continue
			}
			if i+5 >= len(tokens) || !isText(tokens[i]) || !areInts(tokens[i+1:i+6]) {
				*diagnostics = append(*diagnostics, diagnostic(section, "malformed special object spawn"))
				return out
			}
			obj.Spawns = append(obj.Spawns, SpecialObjectSpawn{
				Kind: tokens[i].Value, Code: tokens[i+1].Int, Level: tokens[i+2].Int,
				Params: [3]int64{tokens[i+3].Int, tokens[i+4].Int, tokens[i+5].Int},
			})
			i += 6
		}
		out = append(out, obj)
	}
	return out
}

func isHellPartyOpen(token dnfpvf.Token) bool {
	return isText(token) && strings.EqualFold(strings.TrimSpace(token.Value), "[hellparty]")
}

func isHellPartyClose(token dnfpvf.Token) bool {
	return isText(token) && strings.EqualFold(strings.TrimSpace(token.Value), "[/hellparty]")
}

// Some story maps encode every special-object header as five integers on its
// own line, with the fifth integer being a zero child count. When four such
// rows are flattened, the token count is also divisible by four; line-aware
// recognition must therefore run before the legacy four-field flat grammar.
func parseZeroSpawnSpecialPassiveObjectRows(tokens []dnfpvf.Token) ([]SpecialPassiveObject, bool) {
	if len(tokens) == 0 {
		return nil, false
	}
	var out []SpecialPassiveObject
	for start := 0; start < len(tokens); {
		line := tokens[start].Line
		end := start + 1
		for end < len(tokens) && tokens[end].Line == line {
			end++
		}
		row := tokens[start:end]
		if len(row) != 5 || !areInts(row) || row[4].Int != 0 {
			return nil, false
		}
		out = append(out, SpecialPassiveObject{PassiveObject: PassiveObject{
			ObjectID: row[0].Int,
			X:        row[1].Int,
			Y:        row[2].Int,
			Flags:    row[3].Int,
		}})
		start = end
	}
	return out, true
}

func parseMonsters(section RawSection, diagnostics *[]Diagnostic) []MonsterSpawn {
	tokens := values(section)
	var out []MonsterSpawn
	for i := 0; i < len(tokens); {
		if len(tokens)-i < 10 {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("monster section has %d trailing tokens at token %d", len(tokens)-i, i)))
			break
		}
		if !areInts(tokens[i:i+8]) || !isText(tokens[i+8]) || !isText(tokens[i+9]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed monster record at token %d", i)))
			break
		}
		spawn := MonsterSpawn{
			MonsterID: tokens[i].Int, Level: tokens[i+1].Int, AutoLevel: tokens[i+2].Int,
			Position:        Point3{X: tokens[i+3].Int, Y: tokens[i+4].Int, Z: tokens[i+5].Int},
			RandomDropCount: tokens[i+6].Int, FixedDropCount: tokens[i+7].Int,
			Placement: tokens[i+8].Value, Rank: tokens[i+9].Value,
		}
		i += 10
		if i < len(tokens) && isMonsterSuffixMarker(tokens[i]) {
			spawn.SuffixMarker = tokens[i].Value
			i++
		}
		out = append(out, spawn)
	}
	return out
}

func isMonsterSuffixMarker(token dnfpvf.Token) bool {
	return token.Kind == dnfpvf.TokenString && token.Value == "[boss]"
}

func parseNPCs(section RawSection, diagnostics *[]Diagnostic) []NPCSpawn {
	tokens := values(section)
	var out []NPCSpawn
	for i := 0; i < len(tokens); {
		if i+4 < len(tokens) && tokens[i].Kind == dnfpvf.TokenInt && isText(tokens[i+1]) && areInts(tokens[i+2:i+5]) {
			out = append(out, NPCSpawn{
				NPCID: tokens[i].Int, Direction: tokens[i+1].Value,
				Position: Point3{X: tokens[i+2].Int, Y: tokens[i+3].Int, Z: tokens[i+4].Int},
			})
			i += 5
			continue
		}
		if i+3 < len(tokens) && areInts(tokens[i:i+4]) {
			out = append(out, NPCSpawn{NPCID: tokens[i].Int, Position: Point3{X: tokens[i+1].Int, Y: tokens[i+2].Int, Z: tokens[i+3].Int}})
			i += 4
			continue
		}
		*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed npc record at token %d", i)))
		break
	}
	return out
}

func parseAICharacters(section RawSection, diagnostics *[]Diagnostic) []AICharacter {
	tokens := values(section)
	var out []AICharacter
	for i := 0; i < len(tokens); {
		if i+5 >= len(tokens) || !areInts(tokens[i:i+4]) || !isText(tokens[i+4]) || !isText(tokens[i+5]) {
			*diagnostics = append(*diagnostics, diagnostic(section, fmt.Sprintf("malformed ai character at token %d", i)))
			break
		}
		entry := AICharacter{
			Code: tokens[i].Int, Position: Point{X: tokens[i+1].Int, Y: tokens[i+2].Int},
			Direction: tokens[i+3].Int, Faction: tokens[i+4].Value, AIType: tokens[i+5].Value,
		}
		i += 6
		for len(entry.Params) < 2 && i < len(tokens) && tokens[i].Kind == dnfpvf.TokenInt {
			entry.Params = append(entry.Params, tokens[i].Int)
			i++
		}
		out = append(out, entry)
	}
	return out
}

func parseMovableAreas(section RawSection, diagnostics *[]Diagnostic) []MovableArea {
	parsed, valid := ints(section)
	if !valid || len(parsed)%6 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected six-integer movable-area records"))
	}
	out := make([]MovableArea, 0, len(parsed)/6)
	for i := 0; i+5 < len(parsed); i += 6 {
		out = append(out, MovableArea{
			X: parsed[i], Y: parsed[i+1], Width: parsed[i+2], Height: parsed[i+3],
			Params: [2]int64{parsed[i+4], parsed[i+5]},
		})
	}
	return out
}

func parseDungeonMovableAreas(section RawSection, diagnostics *[]Diagnostic) []MovableArea {
	parsed, valid := ints(section)
	if !valid || len(parsed)%4 != 0 {
		*diagnostics = append(*diagnostics, diagnostic(section, "expected four-integer dungeon movable-area records"))
	}
	out := make([]MovableArea, 0, len(parsed)/4)
	for i := 0; i+3 < len(parsed); i += 4 {
		out = append(out, MovableArea{X: parsed[i], Y: parsed[i+1], Width: parsed[i+2], Height: parsed[i+3]})
	}
	return out
}

func attachHellParty(sections []RawSection, result *Map) {
	for _, section := range sections {
		if sectionKey(section.Name) != "hellparty" || len(result.SpecialPassiveObjects) == 0 {
			continue
		}
		parsed, valid := ints(section)
		if !valid || len(parsed)%3 != 0 {
			result.Diagnostics = append(result.Diagnostics, diagnostic(section, "expected group/rate/order records"))
		}
		last := &result.SpecialPassiveObjects[len(result.SpecialPassiveObjects)-1]
		for i := 0; i+2 < len(parsed); i += 3 {
			last.HellParty = append(last.HellParty, HellPartyEntry{GroupID: parsed[i], Rate: parsed[i+1], Order: parsed[i+2]})
		}
	}
}

func buildPortals(positions []Point, objects []int64) []Portal {
	count := max(len(positions), len(objects))
	out := make([]Portal, count)
	for i := 0; i < count; i++ {
		out[i].Slot = i
		if i < len(positions) {
			position := positions[i]
			out[i].Position = &position
		}
		if i < len(objects) {
			out[i].ObjectID = OptionalInt{Value: objects[i], Set: true}
		}
	}
	return out
}

func setMapFlag(flags *MapFlags, key string) {
	switch key {
	case "block use stackable item":
		flags.BlockUseStackableItem = true
	case "block use active skill":
		flags.BlockUseActiveSkill = true
	case "visible on dungeon clear":
		flags.VisibleOnDungeonClear = true
	case "loop y axis":
		flags.LoopYAxis = true
	case "all dead case passable":
		flags.AllDeadCasePassable = true
	case "disable item escape stuck":
		flags.DisableItemEscapeStuck = true
	case "disable character escape stuck":
		flags.DisableCharacterEscapeStuck = true
	case "cannot use coin map":
		flags.CannotUseCoinMap = true
	case "no revival timer limit":
		flags.NoRevivalTimerLimit = true
	case "ignore diehard":
		flags.IgnoreDiehard = true
	case "disable rebirth":
		flags.DisableRebirth = true
	case "preserve player corpse":
		flags.PreservePlayerCorpse = true
	case "cannot use resolution change zoom":
		flags.CannotUseResolutionChangeZoom = true
	case "center fixed camera":
		flags.CenterFixedCamera = true
	case "force draw pattern":
		flags.ForceDrawPattern = true
	case "is revival":
		flags.IsRevival = true
	case "is moive end", "is movie end":
		flags.IsMovieEnd = true
	case "quest start map":
		flags.QuestStartMap = true
	case "hide monster":
		flags.HideMonster = true
	case "show dust":
		flags.ShowDust = true
	}
}

func isText(token dnfpvf.Token) bool {
	return token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent
}

func areInts(tokens []dnfpvf.Token) bool {
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenInt {
			return false
		}
	}
	return true
}

func cloneRawSection(section RawSection) RawSection {
	section.Tokens = append([]dnfpvf.Token(nil), section.Tokens...)
	section.Scope = append([]string(nil), section.Scope...)
	return section
}
