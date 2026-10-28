// fiendwarsinglemobs creates server-remapped Fiend War monster variants.
//
//	go run ./cmd/scratch/fiendwarsinglemobs \
//	  -pvf <Script.pvf> -out <PVF/Script> -hp 40 -attack 60 -defense 60
//
// A variant keeps the original .mob in its original directory and changes only
// its file name, so relative Action/.act/.ai/.ani references remain valid. The
// tool writes the variants and the single-mode replacement table.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

const (
	fiendWarMonsterPathFragment = "/180621_fiendwar/"
	// These IDs were manually verified as unused in the active monster table.
	singleMonsterIDStart = int64(110720)
)

type monsterEntry struct {
	ID          int64
	Path        string
	VariantID   int64
	VariantPath string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	outRoot := flag.String("out", "", "PVF Script staging root")
	hpRate := flag.Int("hp", 40, "single-mode HP percent")
	attackRate := flag.Int("attack", 60, "single-mode physical/magical attack percent")
	defenseRate := flag.Int("defense", 60, "single-mode physical/magical defense percent")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" || strings.TrimSpace(*outRoot) == "" ||
		*hpRate <= 0 || *attackRate <= 0 || *defenseRate <= 0 {
		fmt.Fprintln(os.Stderr, "-pvf, -out and positive stat percentages are required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	fail(err)
	entries, err := fiendWarMonsters(archive)
	fail(err)
	existing := make(map[int64]bool)
	for _, entry := range entries {
		existing[entry.ID] = true
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	for index := range entries {
		entry := &entries[index]
		entry.VariantID = singleMonsterIDStart + int64(index)
		if existing[entry.VariantID] {
			fail(fmt.Errorf("variant monster ID %d already exists", entry.VariantID))
		}
		ext := path.Ext(entry.Path)
		entry.VariantPath = strings.TrimSuffix(entry.Path, ext) + "_single" + ext
	}
	for _, entry := range entries {
		source := path.Join("monster", entry.Path)
		text, readErr := archive.ReadText(source)
		if readErr != nil {
			fail(fmt.Errorf("read %s: %w", source, readErr))
		}
		scaled, changed := scaleMonsterText(text, *hpRate, *attackRate, *defenseRate)
		if !changed {
			fmt.Fprintf(os.Stderr, "warning: no ability values changed in %s\n", source)
		}
		formatted := pvfEditorText(scaled)
		if !samePVFTokens(scaled, formatted) {
			fail(fmt.Errorf("formatting changed PVF tokens for %s", source))
		}
		target := filepath.Join(*outRoot, filepath.FromSlash(path.Join("monster", entry.VariantPath)))
		fail(os.MkdirAll(filepath.Dir(target), 0o755))
		fail(os.WriteFile(target, []byte(formatted), 0o644))
	}

	var mapping strings.Builder
	mapping.WriteString("#PVF_File\n")
	for _, entry := range entries {
		mapping.WriteString("\n[MONSTER VARIANT]\n\t")
		mapping.WriteString(strconv.FormatInt(entry.ID, 10))
		mapping.WriteByte('\t')
		mapping.WriteString(strconv.FormatInt(entry.VariantID, 10))
		mapping.WriteString("\n[/MONSTER VARIANT]\n")
	}
	mappingTarget := filepath.Join(*outRoot, "etc", "raid", "fiendwar", "fiendwar_single_monster.etc")
	fail(os.MkdirAll(filepath.Dir(mappingTarget), 0o755))
	fail(os.WriteFile(mappingTarget, []byte(mapping.String()), 0o644))

	fmt.Printf("monsters=%d hp=%d%% attack=%d%% defense=%d%%\n", len(entries), *hpRate, *attackRate, *defenseRate)
	fmt.Printf("mapping=%s\n", mappingTarget)
}

func pvfEditorText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	if strings.HasPrefix(text, "#PVF_File") {
		return text
	}
	text = expandMobTokenRows(strings.TrimLeft(text, "\n"))
	return "#PVF_File\n\n" + indentPVFText(text)
}

func expandMobTokenRows(text string) string {
	text = expandFlatMobBlocks(text, map[string]int{
		"ability category":          3,
		"attack action":             1,
		"attack info":               1,
		"category":                  1,
		"common champion drop item": 1,
		"enable states":             1,
		"etc action":                1,
		"etc attack info":           1,
		"etc motion":                1,
		"immune active status":      1,
	})
	return expandNestedMobBlocks(text, "ai pattern")
}

func expandFlatMobBlocks(text string, blockTokensPerRow map[string]int) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := strings.TrimSpace(lines[lineIndex])
		out = append(out, lines[lineIndex])
		blockName, ok := pvfBlockName(line, false)
		tokensPerRow, expand := blockTokensPerRow[blockName]
		if !ok || !expand {
			continue
		}
		closeIndex := lineIndex + 1
		for closeIndex < len(lines) {
			closeName, closeFound := pvfBlockName(strings.TrimSpace(lines[closeIndex]), true)
			if closeFound && closeName == blockName {
				break
			}
			closeIndex++
		}
		if closeIndex == len(lines) {
			continue
		}
		tokens := abilityCategoryTokens(strings.Join(lines[lineIndex+1:closeIndex], " "))
		if len(tokens) == 0 || len(tokens)%tokensPerRow != 0 {
			continue
		}
		out = out[:len(out)-1]
		out = append(out, lines[lineIndex])
		for tokenIndex := 0; tokenIndex < len(tokens); tokenIndex += tokensPerRow {
			out = append(out, strings.Join(tokens[tokenIndex:tokenIndex+tokensPerRow], "\t"))
		}
		out = append(out, lines[closeIndex])
		lineIndex = closeIndex
	}
	return strings.Join(out, "\n")
}

func expandNestedMobBlocks(text, parentName string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := strings.TrimSpace(lines[lineIndex])
		name, open := pvfBlockName(line, false)
		out = append(out, lines[lineIndex])
		if !open || name != parentName {
			continue
		}
		parentClose := findPVFBlockClose(lines, lineIndex, parentName)
		if parentClose < 0 {
			continue
		}
		for childIndex := lineIndex + 1; childIndex < parentClose; childIndex++ {
			childName, childOpen := pvfBlockName(strings.TrimSpace(lines[childIndex]), false)
			if !childOpen {
				continue
			}
			childClose := findPVFBlockClose(lines, childIndex, childName)
			if childClose < 0 || childClose >= parentClose {
				continue
			}
			tokens := abilityCategoryTokens(strings.Join(lines[childIndex+1:childClose], " "))
			out = append(out, lines[childIndex])
			for _, token := range tokens {
				out = append(out, token)
			}
			out = append(out, lines[childClose])
			childIndex = childClose
		}
		out = append(out, lines[parentClose])
		lineIndex = parentClose
	}
	return strings.Join(out, "\n")
}

func findPVFBlockClose(lines []string, openIndex int, blockName string) int {
	for index := openIndex + 1; index < len(lines); index++ {
		name, close := pvfBlockName(strings.TrimSpace(lines[index]), true)
		if close && name == blockName {
			return index
		}
	}
	return -1
}

func pvfBlockName(line string, closing bool) (string, bool) {
	if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
		return "", false
	}
	if closing {
		if !strings.HasPrefix(line, "[/") {
			return "", false
		}
		return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[/"), "]"))), true
	}
	if strings.HasPrefix(line, "[/") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))), true
}

func abilityCategoryTokens(text string) []string {
	pattern := regexp.MustCompile("`[^`]*`|[^\\s`]+")
	return pattern.FindAllString(text, -1)
}

func joinPVFValueTokens(text string) string {
	return strings.Join(abilityCategoryTokens(text), "\t")
}

func samePVFTokens(left, right string) bool {
	leftTokens := pvfSemanticTokens(left)
	rightTokens := pvfSemanticTokens(right)
	if len(leftTokens) != len(rightTokens) {
		return false
	}
	for index := range leftTokens {
		if leftTokens[index] != rightTokens[index] {
			return false
		}
	}
	return true
}

func pvfSemanticTokens(text string) []string {
	pattern := regexp.MustCompile("`[^`]*`|[^\\s`]+")
	tokens := pattern.FindAllString(text, -1)
	out := tokens[:0]
	for _, token := range tokens {
		if token != "#PVF_File" {
			out = append(out, token)
		}
	}
	return out
}

func indentPVFText(text string) string {
	lines := strings.Split(text, "\n")
	type tagEvent struct {
		line  int
		close bool
	}
	events := make(map[string][]tagEvent)
	for lineIndex, raw := range lines {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		isClose := strings.HasPrefix(line, "[/")
		prefix := "["
		if isClose {
			prefix = "[/"
		}
		name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, prefix), "]")))
		if name == "" {
			continue
		}
		events[name] = append(events[name], tagEvent{line: lineIndex, close: isClose})
	}
	isContainer := make(map[int]bool)
	for _, tagEvents := range events {
		for i := 0; i+1 < len(tagEvents); i++ {
			if !tagEvents[i].close && tagEvents[i+1].close {
				isContainer[tagEvents[i].line] = true
			}
		}
	}

	stack := make([]string, 0, 8)
	out := make([]string, 0, len(lines))
	contentDepth := 0
	for lineIndex, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "[/") && strings.HasSuffix(line, "]"):
			name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[/"), "]")))
			if len(stack) > 0 && stack[len(stack)-1] == name {
				stack = stack[:len(stack)-1]
			}
			out = append(out, strings.Repeat("\t", len(stack))+line)
			contentDepth = len(stack) + 1
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			depth := len(stack)
			if depth == 0 && len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, strings.Repeat("\t", depth)+line)
			name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")))
			contentDepth = depth + 1
			if isContainer[lineIndex] {
				stack = append(stack, name)
			}
		default:
			out = append(out, strings.Repeat("\t", contentDepth)+joinPVFValueTokens(line))
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}

func fiendWarMonsters(archive *platformpvf.Archive) ([]monsterEntry, error) {
	listText, err := archive.ReadText("monster/monster.lst")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile("(?s)([0-9]+)\\s+`([^`]+\\.mob)`")
	all := re.FindAllStringSubmatch(listText, -1)
	entries := make([]monsterEntry, 0, 64)
	for _, match := range all {
		listPath := strings.ReplaceAll(match[2], "\\", "/")
		normalizedPath := strings.ToLower("/" + listPath)
		if !strings.Contains(normalizedPath, fiendWarMonsterPathFragment) ||
			strings.HasSuffix(normalizedPath, "_single.mob") {
			continue
		}
		id, convErr := strconv.ParseInt(match[1], 10, 64)
		if convErr != nil || id <= 0 {
			return nil, fmt.Errorf("invalid monster id %q", match[1])
		}
		entries = append(entries, monsterEntry{ID: id, Path: listPath})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no 180621_FiendWar monster rows found")
	}
	return entries, nil
}

func scaleMonsterText(text string, hpRate, attackRate, defenseRate int) (string, bool) {
	changed := false
	out := text
	for _, field := range []struct {
		name string
		rate int
	}{
		{"HP MAX", hpRate},
		{"EQUIPMENT_PHYSICAL_ATTACK", attackRate},
		{"EQUIPMENT_MAGICAL_ATTACK", attackRate},
		{"EQUIPMENT_PHYSICAL_DEFENSE", defenseRate},
		{"EQUIPMENT_MAGICAL_DEFENSE", defenseRate},
	} {
		pattern := regexp.MustCompile(`(?i)(` + "`?" + `\[` + regexp.QuoteMeta(field.name) + `\]` + "`?" + `\s*(?:` + "`\\*`" + `)?\s*)([0-9]+(?:\.[0-9]+)?)`)
		out = pattern.ReplaceAllStringFunc(out, func(line string) string {
			match := pattern.FindStringSubmatch(line)
			if len(match) != 3 {
				return line
			}
			value, err := strconv.ParseFloat(match[2], 64)
			if err != nil || value <= 0 {
				return line
			}
			scaled := int64(math.Floor(value * float64(field.rate) / 100))
			if scaled < 1 {
				scaled = 1
			}
			if scaled == int64(value) {
				return line
			}
			changed = true
			return match[1] + strconv.FormatInt(scaled, 10)
		})
	}
	return out, changed
}

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
