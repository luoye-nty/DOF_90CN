package quest

import (
	"fmt"
	"math"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const ConnectQuestListPath = "n_quest/connectquestlist.etc"

// StoryEntry is one ordered task/dungeon pair declared by an [episode quest]
// unit in the runtime PVF. A zero dungeon ID is valid for a non-dungeon task.
type StoryEntry struct {
	QuestID   uint16
	DungeonID uint32
}

// StoryEpisode is the PVF-owned presentation sequence for one main-story
// chapter. It carries metadata only; it does not override the active quest,
// dungeon-route, or completion state machines.
type StoryEpisode struct {
	ParentKey     int
	WorldMapIndex int
	LevelSection  []int64
	Name          string
	Entries       []StoryEntry
}

// LoadStoryEpisodes reads the optional main-story episode directory. Callers
// must include ConnectQuestListPath in their PVF index build options.
func LoadStoryEpisodes(index *dnfpvf.Index) ([]StoryEpisode, error) {
	if index == nil {
		return nil, fmt.Errorf("story episode index is required")
	}
	document, found := index.Document(ConnectQuestListPath)
	if !found || document == nil {
		return nil, fmt.Errorf("story episode document %s is missing from PVF index", ConnectQuestListPath)
	}

	episodes := make([]StoryEpisode, 0)
	for offset := 0; offset < len(document.Tokens); offset++ {
		if !storySectionEquals(document.Tokens[offset], "connect quest unit") {
			continue
		}
		end := storyUnitEnd(document.Tokens, offset+1)
		if end < 0 {
			return nil, fmt.Errorf("story episode unit %d is not closed", len(episodes))
		}
		episode, include, err := parseStoryEpisodeUnit(document.Tokens[offset+1 : end])
		if err != nil {
			return nil, fmt.Errorf("story episode unit %d: %w", len(episodes), err)
		}
		if include {
			episodes = append(episodes, episode)
		}
		offset = end
	}
	if len(episodes) == 0 {
		return nil, fmt.Errorf("story episode document %s has no episode quest units", ConnectQuestListPath)
	}
	return episodes, nil
}

// StoryEntriesFrom returns the requested task and the remaining entries in
// its own episode. It never crosses into a following episode.
func StoryEntriesFrom(episodes []StoryEpisode, questID uint16) ([]StoryEntry, bool) {
	if questID == 0 {
		return nil, false
	}
	for _, episode := range episodes {
		for index, entry := range episode.Entries {
			if entry.QuestID != questID {
				continue
			}
			return append([]StoryEntry(nil), episode.Entries[index:]...), true
		}
	}
	return nil, false
}

func StoryEpisodeName(episodes []StoryEpisode, questID uint16) string {
	if questID == 0 {
		return ""
	}
	for _, episode := range episodes {
		for _, entry := range episode.Entries {
			if entry.QuestID == questID {
				return strings.TrimSpace(episode.Name)
			}
		}
	}
	return ""
}

func parseStoryEpisodeUnit(tokens []dnfpvf.Token) (StoryEpisode, bool, error) {
	sections := storyUnitSections(tokens)
	unitType, _ := storyFirstText(sections["type"])
	if normalizeQuestTag(unitType) != "episode quest" {
		return StoryEpisode{}, false, nil
	}
	parentKey, parentOK := storyFirstInt(sections["parent key"])
	worldMapIndex, worldMapOK := storyFirstInt(sections["worldmap index"])
	if !parentOK || !worldMapOK || parentKey <= 0 || parentKey > math.MaxInt || worldMapIndex < 0 || worldMapIndex > math.MaxInt {
		return StoryEpisode{}, false, fmt.Errorf("invalid episode identity")
	}
	sequence := storyInts(sections["sequence"])
	if len(sequence) == 0 || len(sequence)%2 != 0 {
		return StoryEpisode{}, false, fmt.Errorf("invalid sequence length %d", len(sequence))
	}
	episode := StoryEpisode{
		ParentKey:     int(parentKey),
		WorldMapIndex: int(worldMapIndex),
		LevelSection:  storyInts(sections["level section"]),
	}
	episode.Name, _ = storyFirstText(sections["name"])
	for index := 0; index < len(sequence); index += 2 {
		questID, dungeonID := sequence[index], sequence[index+1]
		if questID <= 0 || questID > math.MaxUint16 || dungeonID < 0 || dungeonID > math.MaxUint32 {
			return StoryEpisode{}, false, fmt.Errorf("invalid sequence pair %d/%d", questID, dungeonID)
		}
		episode.Entries = append(episode.Entries, StoryEntry{QuestID: uint16(questID), DungeonID: uint32(dungeonID)})
	}
	return episode, true, nil
}

func storyUnitEnd(tokens []dnfpvf.Token, start int) int {
	for index := start; index < len(tokens); index++ {
		if storySectionEquals(tokens[index], "/connect quest unit") {
			return index
		}
	}
	return -1
}

func storyUnitSections(tokens []dnfpvf.Token) map[string][]dnfpvf.Token {
	sections := make(map[string][]dnfpvf.Token)
	current := ""
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenSection {
			current = normalizeQuestTag(token.Value)
			continue
		}
		if current != "" {
			sections[current] = append(sections[current], token)
		}
	}
	return sections
}

func storySectionEquals(token dnfpvf.Token, value string) bool {
	return token.Kind == dnfpvf.TokenSection && normalizeQuestTag(token.Value) == normalizeQuestTag(value)
}

func storyFirstText(tokens []dnfpvf.Token) (string, bool) {
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			continue
		}
		value := strings.TrimSpace(token.Value)
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func storyFirstInt(tokens []dnfpvf.Token) (int64, bool) {
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			return token.Int, true
		}
	}
	return 0, false
}

func storyInts(tokens []dnfpvf.Token) []int64 {
	result := make([]int64, 0, len(tokens))
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			result = append(result, token.Int)
		}
	}
	return result
}
