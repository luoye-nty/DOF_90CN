// statpotioncensus 只读：按 [stackable type] 关键字普查可叠加道具，并打印
// 决定它们"怎么用、用完要不要服务端记状态"的那几个字段。
//
// 现场 2026-10-03（服主报）：史诗Buff药剂（例如 490002458）使用后没反应。
// 服务端日志给出的拒绝原因是 "requires PVF-proven [waste] provenance"，
// 而它的 [stackable type] 是 [expert town potion]。要判断"放宽判据"的影响面，
// 必须先把同一族的道具全部列出来。
//
// 跑法：
//
//	go run ./cmd/scratch/statpotioncensus -pvf "...\Script.pvf" -type "expert town potion"
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type entry struct {
	id    int64
	doc   string
	path  string
	lines map[string]string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf (required)")
	typeFilter := flag.String("type", "expert town potion", "substring of [stackable type] to select")
	max := flag.Int("max", 200, "max entries to print")
	flag.Parse()
	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -pvf is required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: open pvf:", err)
		os.Exit(1)
	}

	entries := collect(archive, *typeFilter)
	fmt.Printf("匹配 [stackable type] 含 %q 的道具：%d 件\n\n", *typeFilter, len(entries))

	// 按 (stackable type, action type, 有无 stat change duration) 分组统计。
	groups := map[string]int{}
	for _, item := range entries {
		groups[groupKey(item)]++
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Printf("按 (stackable type / action type / cooltime maintenance / stat change duration) 分组：\n")
	for _, key := range keys {
		fmt.Printf("  %-110s x%d\n", key, groups[key])
	}
	fmt.Println()

	limit := *max
	if len(entries) < limit {
		limit = len(entries)
	}
	for _, item := range entries[:limit] {
		fmt.Printf("#%d  %s\n", item.id, item.path)
		for _, key := range []string{
			"name", "stackable type", "action type", "action usable place",
			"stat change duration", "effect maintenance", "cool time",
			"cooltime maintenance", "total usable count", "expiration date",
			"usable event", "minimum level", "attach type",
		} {
			if value, ok := item.lines[key]; ok {
				fmt.Printf("    %-22s %s\n", "["+key+"]", value)
			}
		}
		fmt.Println()
	}
}

// collect 扫所有 stackable .lst，读每个文档，取 [stackable type] 命中的那些。
func collect(archive *platformpvf.Archive, typeFilter string) []entry {
	want := strings.ToLower(strings.TrimSpace(typeFilter))
	out := make([]entry, 0)
	seen := map[int64]bool{}
	for _, file := range archive.Files() {
		full := file.ArchivePath
		if strings.TrimSpace(full) == "" {
			full = file.Path
		}
		lower := strings.ToLower(full)
		if !strings.HasSuffix(lower, ".lst") || !strings.Contains(lower, "stackable") {
			continue
		}
		text, err := archive.ReadText(full)
		if err != nil {
			continue
		}
		fields := strings.Fields(text)
		for i := 0; i+1 < len(fields); i++ {
			id, parseErr := strconv.ParseInt(fields[i], 10, 64)
			if parseErr != nil || seen[id] {
				continue
			}
			rel := strings.Trim(fields[i+1], "`")
			docPath := path.Clean(path.Join(path.Dir(full), strings.ReplaceAll(rel, "\\", "/")))
			body, readErr := archive.ReadText(docPath)
			if readErr != nil {
				continue
			}
			lines := indexSections(body)
			stackableType := strings.ToLower(lines["stackable type"])
			if !strings.Contains(stackableType, want) {
				continue
			}
			seen[id] = true
			out = append(out, entry{id: id, doc: body, path: docPath, lines: lines})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

var sectionStart = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

// indexSections 把文档按 [字段] 切成 key -> 值文本（值取到下一个字段之前）。
func indexSections(body string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	current := ""
	var buffer []string
	flush := func() {
		if current == "" {
			return
		}
		out[current] = strings.Join(buffer, " ")
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if match := sectionStart.FindStringSubmatch(trimmed); match != nil {
			flush()
			current = strings.ToLower(strings.TrimSpace(match[1]))
			buffer = buffer[:0]
			continue
		}
		if current == "" {
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		buffer = append(buffer, trimmed)
	}
	flush()
	return out
}

func groupKey(item entry) string {
	parts := []string{
		"type=" + strings.TrimSpace(item.lines["stackable type"]),
		"action=" + orDash(item.lines["action type"]),
		"coolmaint=" + orDash(item.lines["cooltime maintenance"]),
		"statdur=" + orDash(item.lines["stat change duration"]),
		"effectmaint=" + orDash(item.lines["effect maintenance"]),
	}
	return strings.Join(parts, " | ")
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}
