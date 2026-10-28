package raid

import (
	"errors"
	"fmt"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// Luke phase-one raid monster list.
//
// etc/raid/luke/luke.etc.buff [RAID MONSTER LIST] is the table that decides which
// named monster and which boss each maze of each raid dungeon actually spawns:
//
//	[RAID MONSTER LIST]
//	[DUNGEON]
//	5054
//	[MAZE]
//	0
//	[BOSS]
//	56063
//	[/MAZE]
//	[MAZE]
//	1
//	[NAMED]
//	56674
//	[BOSS]
//	56700
//	[/MAZE]
//	...
//	[/DUNGEON]
//	...
//	[/RAID MONSTER LIST]
//
// One dungeon owns fifteen [MAZE] blocks. They form five groups of three
// consecutive mazes -- the same five groups luke.etc [CARD SET MAP INFO] names by
// their first maze (0 0 0 3 3 3 6 6 6 9 9 9 12 12 12) -- and the third maze of
// every group carries the sanctum's own boss (5054: mazes 2/5/8/11/14 all end in
// 56673), while the first two carry the group's named monster and the shared mid
// boss. See raid_luke_sanctum_maze.go for how the day's group is drawn.
//
// MonsterIDs keeps the source order of one maze, which is [NAMED] first and then
// [BOSS], because the tarot card slots are filled in that order (see
// runtimeRaidLukeTarotCardsFromMazeMonsters).
const LukeRaidMonsterListPVFPath = "etc/raid/luke/luke.etc.buff"

var (
	// ErrLukeRaidMonsterListSourceRequired is returned for a nil PVF source.
	ErrLukeRaidMonsterListSourceRequired = errors.New("luke raid monster list PVF source is required")
	// ErrLukeRaidMonsterListDocumentInvalid is returned for any document that is
	// not a usable [RAID MONSTER LIST].
	ErrLukeRaidMonsterListDocumentInvalid = errors.New("luke raid monster list document is invalid")
)

// LukeRaidMonsterListMaze is one [MAZE] block. Index is the maze index inside its
// dungeon's .dgn, and MonsterIDs are the monsters that maze spawns, in the order
// the document lists them.
type LukeRaidMonsterListMaze struct {
	Index      int
	MonsterIDs []uint32
}

// LukeRaidMonsterListDungeon is one [DUNGEON] block.
type LukeRaidMonsterListDungeon struct {
	DungeonID uint32
	Mazes     []LukeRaidMonsterListMaze
}

// LukeRaidMonsterListConfig owns etc/raid/luke/luke.etc.buff [RAID MONSTER LIST].
type LukeRaidMonsterListConfig struct {
	Dungeons []LukeRaidMonsterListDungeon
}

// LoadLukeRaidMonsterList reads and parses the raid monster list out of one PVF
// source.
func LoadLukeRaidMonsterList(source dnfpvf.Source) (*LukeRaidMonsterListConfig, error) {
	if source == nil {
		return nil, ErrLukeRaidMonsterListSourceRequired
	}
	text, err := source.ReadText(LukeRaidMonsterListPVFPath)
	if err != nil {
		return nil, fmt.Errorf("read luke raid monster list PVF %q: %w", LukeRaidMonsterListPVFPath, err)
	}
	document, err := dnfpvf.Parse(LukeRaidMonsterListPVFPath, text)
	if err != nil {
		return nil, fmt.Errorf("parse luke raid monster list PVF %q: %w", LukeRaidMonsterListPVFPath, err)
	}
	return ParseLukeRaidMonsterList(document)
}

// ParseLukeRaidMonsterList reads [RAID MONSTER LIST] out of an already parsed
// document. The path must be the buff document, so that luke.etc cannot be
// mistaken for it.
func ParseLukeRaidMonsterList(document *dnfpvf.Document) (*LukeRaidMonsterListConfig, error) {
	if document == nil || antonPathKey(document.Path) != antonPathKey(LukeRaidMonsterListPVFPath) {
		path := ""
		if document != nil {
			path = document.Path
		}
		return nil, fmt.Errorf("%w: path=%q want=%q",
			ErrLukeRaidMonsterListDocumentInvalid, path, LukeRaidMonsterListPVFPath)
	}
	lists, err := antonFindBlocks(document, "raid monster list", 0, len(document.Sections))
	if err != nil {
		return nil, fmt.Errorf("%w: raid monster list block: %v", ErrLukeRaidMonsterListDocumentInvalid, err)
	}
	if len(lists) != 1 {
		return nil, fmt.Errorf("%w: [RAID MONSTER LIST] blocks=%d want=1",
			ErrLukeRaidMonsterListDocumentInvalid, len(lists))
	}
	dungeonBlocks, err := antonFindBlocks(document, "dungeon", lists[0].open+1, lists[0].close)
	if err != nil {
		return nil, fmt.Errorf("%w: dungeon blocks: %v", ErrLukeRaidMonsterListDocumentInvalid, err)
	}
	if len(dungeonBlocks) == 0 {
		return nil, fmt.Errorf("%w: no [DUNGEON] block", ErrLukeRaidMonsterListDocumentInvalid)
	}
	config := &LukeRaidMonsterListConfig{
		Dungeons: make([]LukeRaidMonsterListDungeon, 0, len(dungeonBlocks)),
	}
	seenDungeons := make(map[uint32]bool, len(dungeonBlocks))
	for _, block := range dungeonBlocks {
		dungeon, parseErr := parseLukeRaidMonsterListDungeon(document, block)
		if parseErr != nil {
			return nil, parseErr
		}
		if seenDungeons[dungeon.DungeonID] {
			return nil, fmt.Errorf("%w: duplicate [DUNGEON] %d",
				ErrLukeRaidMonsterListDocumentInvalid, dungeon.DungeonID)
		}
		seenDungeons[dungeon.DungeonID] = true
		config.Dungeons = append(config.Dungeons, dungeon)
	}
	return config, nil
}

// Dungeon returns one dungeon's maze table.
func (c *LukeRaidMonsterListConfig) Dungeon(dungeonID uint32) (LukeRaidMonsterListDungeon, bool) {
	if c == nil {
		return LukeRaidMonsterListDungeon{}, false
	}
	for _, dungeon := range c.Dungeons {
		if dungeon.DungeonID == dungeonID {
			return dungeon, true
		}
	}
	return LukeRaidMonsterListDungeon{}, false
}

// MazeCount is how many [MAZE] blocks one dungeon declares: the dungeon's maze
// count, which is what the day's group start has to stay inside of. ok is false
// for a dungeon the table does not describe.
func (c *LukeRaidMonsterListConfig) MazeCount(dungeonID uint32) (int, bool) {
	dungeon, ok := c.Dungeon(dungeonID)
	if !ok {
		return 0, false
	}
	return len(dungeon.Mazes), true
}

// MazeMonsters returns one maze's monsters in source order. ok is false when the
// table does not describe that dungeon, and a described maze without monsters
// returns an empty slice and true -- "this maze spawns nothing", which is not the
// same answer as "this table does not know the maze".
func (c *LukeRaidMonsterListConfig) MazeMonsters(dungeonID uint32, mazeIndex int) ([]uint32, bool) {
	dungeon, ok := c.Dungeon(dungeonID)
	if !ok {
		return nil, false
	}
	for _, maze := range dungeon.Mazes {
		if maze.Index == mazeIndex {
			return maze.MonsterIDs, true
		}
	}
	return nil, false
}

func parseLukeRaidMonsterListDungeon(
	document *dnfpvf.Document,
	block antonSectionBlock,
) (LukeRaidMonsterListDungeon, error) {
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return LukeRaidMonsterListDungeon{}, fmt.Errorf("%w: dungeon key: %v",
			ErrLukeRaidMonsterListDocumentInvalid, err)
	}
	if len(tokens) != 1 {
		return LukeRaidMonsterListDungeon{}, fmt.Errorf("%w: dungeon key values=%d want=1",
			ErrLukeRaidMonsterListDocumentInvalid, len(tokens))
	}
	dungeonID, err := antonPositiveUint32Token(tokens[0], "raid monster list dungeon")
	if err != nil {
		return LukeRaidMonsterListDungeon{}, fmt.Errorf("%w: dungeon: %v",
			ErrLukeRaidMonsterListDocumentInvalid, err)
	}
	mazeBlocks, err := antonFindBlocks(document, "maze", block.open+1, block.close)
	if err != nil {
		return LukeRaidMonsterListDungeon{}, fmt.Errorf("%w: dungeon=%d maze blocks: %v",
			ErrLukeRaidMonsterListDocumentInvalid, dungeonID, err)
	}
	// A [DUNGEON] block without [MAZE] is a real shape: the table also covers the
	// phase-two dungeons, and 5310 ships one that only lists [PASSIVE] objects.
	// That is "this dungeon has no maze list", not a broken document.
	dungeon := LukeRaidMonsterListDungeon{
		DungeonID: dungeonID,
		Mazes:     make([]LukeRaidMonsterListMaze, 0, len(mazeBlocks)),
	}
	if len(mazeBlocks) == 0 {
		return dungeon, nil
	}
	seen := make(map[int]bool, len(mazeBlocks))
	for _, mazeBlock := range mazeBlocks {
		maze, parseErr := parseLukeRaidMonsterListMaze(document, dungeonID, mazeBlock)
		if parseErr != nil {
			return LukeRaidMonsterListDungeon{}, parseErr
		}
		if seen[maze.Index] {
			return LukeRaidMonsterListDungeon{}, fmt.Errorf("%w: dungeon=%d duplicate [MAZE] %d",
				ErrLukeRaidMonsterListDocumentInvalid, dungeonID, maze.Index)
		}
		seen[maze.Index] = true
		dungeon.Mazes = append(dungeon.Mazes, maze)
	}
	return dungeon, nil
}

// parseLukeRaidMonsterListMaze reads one `[MAZE] <index>` block. Only [NAMED],
// [BOSS] and [PASSIVE] may appear inside; [PASSIVE] is deliberately not a
// monster -- it names a map passive object, and the [CARD] table that the tarot
// display is keyed by only ever holds monsters.
func parseLukeRaidMonsterListMaze(
	document *dnfpvf.Document,
	dungeonID uint32,
	block antonSectionBlock,
) (LukeRaidMonsterListMaze, error) {
	tokens, err := antonSectionTokens(document, block.open)
	if err != nil {
		return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze key: %v",
			ErrLukeRaidMonsterListDocumentInvalid, dungeonID, err)
	}
	if len(tokens) != 1 {
		return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze key values=%d want=1",
			ErrLukeRaidMonsterListDocumentInvalid, dungeonID, len(tokens))
	}
	index, err := antonIntToken(tokens[0], "raid monster list maze index")
	if err != nil {
		return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze: %v",
			ErrLukeRaidMonsterListDocumentInvalid, dungeonID, err)
	}
	if index < 0 {
		return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze index=%d want_nonnegative",
			ErrLukeRaidMonsterListDocumentInvalid, dungeonID, index)
	}
	maze := LukeRaidMonsterListMaze{Index: index, MonsterIDs: make([]uint32, 0, 2)}
	for sectionIndex := block.open + 1; sectionIndex < block.close; sectionIndex++ {
		name := antonSectionName(document.Sections[sectionIndex].Name)
		switch name {
		case "named", "boss":
		case "passive":
			// Not a monster: a map passive object, never a [CARD] key.
			continue
		default:
			return LukeRaidMonsterListMaze{}, fmt.Errorf(
				"%w: dungeon=%d maze=%d unexpected section=%q",
				ErrLukeRaidMonsterListDocumentInvalid, dungeonID, index,
				document.Sections[sectionIndex].Name)
		}
		sectionTokens, tokenErr := antonSectionTokens(document, sectionIndex)
		if tokenErr != nil {
			return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze=%d %s: %v",
				ErrLukeRaidMonsterListDocumentInvalid, dungeonID, index, name, tokenErr)
		}
		if len(sectionTokens) != 1 {
			return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze=%d %s values=%d want=1",
				ErrLukeRaidMonsterListDocumentInvalid, dungeonID, index, name, len(sectionTokens))
		}
		monsterID, monsterErr := antonPositiveUint32Token(sectionTokens[0], "raid monster list monster")
		if monsterErr != nil {
			return LukeRaidMonsterListMaze{}, fmt.Errorf("%w: dungeon=%d maze=%d %s: %v",
				ErrLukeRaidMonsterListDocumentInvalid, dungeonID, index, name, monsterErr)
		}
		maze.MonsterIDs = append(maze.MonsterIDs, monsterID)
	}
	return maze, nil
}
