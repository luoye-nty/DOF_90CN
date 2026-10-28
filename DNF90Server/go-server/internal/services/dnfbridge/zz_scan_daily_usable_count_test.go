package dnfbridge

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestScanDailyUsableCountDeclarations 扫描真实 Script.pvf，列出所有声明了
// [total usable count] 的 stackable 道具。
//
// 只读不改，默认跳过；设置 DNFBRIDGE_REAL_PVF_SMOKE 指向 Script.pvf 即运行：
//
//	$env:DNFBRIDGE_REAL_PVF_SMOKE='D:\test\地下城与勇士\Script.pvf'
//	go test ./internal/services/dnfbridge -run TestScanDailyUsableCountDeclarations -v
func TestScanDailyUsableCountDeclarations(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to scan the real Script.pvf")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}

	// item id -> pvf path，来自 stackable/stackable.lst
	idToPath := map[uint32]string{}
	pathToID := map[string]uint32{}
	if text, err := archive.ReadText("stackable/stackable.lst"); err == nil {
		entry := regexp.MustCompile("(?m)^\\s*([0-9]+)\\s+`([^`]+)`")
		for _, match := range entry.FindAllStringSubmatch(text, -1) {
			var id uint32
			fmt.Sscan(match[1], &id)
			path := strings.TrimSpace(match[2])
			idToPath[id] = path
			pathToID[path] = id
		}
	}
	t.Logf("stackable.lst entries=%d", len(idToPath))

	namePattern := regexp.MustCompile("(?m)^\\s*\\[name\\]\\s+`([^`]*)`")
	totalPattern := regexp.MustCompile("(?m)^\\s*\\[total usable count\\]\\s+([0-9]+)")
	actionPattern := regexp.MustCompile("(?m)^\\s*\\[action type\\]\\s+`([^`]*)`")
	stackPattern := regexp.MustCompile("(?m)^\\s*\\[stackable type\\]\\s+`([^`]*)`")
	explainPattern := regexp.MustCompile("(?m)^\\s*\\[explain\\]\\s+`([^`]*)`")

	type row struct {
		itemID uint32
		name   string
		limit  string
		stack  string
		action string
		path   string
	}
	var rows []row
	scanned := 0
	archive.RangePaths(func(path string) bool {
		lower := strings.ToLower(path)
		if !strings.HasSuffix(lower, ".stk") && !strings.HasSuffix(lower, ".stak") {
			return true
		}
		text, err := archive.ReadText(path)
		if err != nil {
			return true
		}
		scanned++
		match := totalPattern.FindStringSubmatch(text)
		if len(match) != 2 {
			return true
		}
		name := ""
		if m := namePattern.FindStringSubmatch(text); len(m) == 2 {
			name = m[1]
		}
		stack := ""
		if m := stackPattern.FindStringSubmatch(text); len(m) == 2 {
			stack = m[1]
		}
		action := ""
		if m := actionPattern.FindStringSubmatch(text); len(m) == 2 {
			action = m[1]
		}
		if name == "" {
			if m := explainPattern.FindStringSubmatch(text); len(m) == 2 {
				name = "(explain) " + m[1]
			}
		}
		rows = append(rows, row{
			itemID: pathToID[path], name: name, limit: match[1],
			stack: stack, action: action, path: path,
		})
		return true
	})

	sort.Slice(rows, func(i, j int) bool { return rows[i].itemID < rows[j].itemID })
	t.Logf("扫描 stackable 文件 %d 个，其中声明 [total usable count] 的 %d 个", scanned, len(rows))
	for _, entry := range rows {
		t.Logf("item=%-12d limit=%-3s stack=%-16s action=%-28s name=%-24s path=%s",
			entry.itemID, entry.limit, entry.stack, entry.action, entry.name, entry.path)
	}

	// DNFBRIDGE_REAL_PVF_ITEM=<itemID> 时，打印该道具的完整声明（便于确认某个
	// 具体道具的 action type / 上限 / 说明文字），不限于带 [total usable count] 的。
	lookup := os.Getenv("DNFBRIDGE_REAL_PVF_ITEM")
	if lookup == "" {
		return
	}
	suffix := "/" + strings.TrimSpace(lookup) + ".stk"
	matched := 0
	archive.RangePaths(func(path string) bool {
		if !strings.HasSuffix(strings.ToLower(path), suffix) {
			return true
		}
		text, err := archive.ReadText(path)
		if err != nil {
			return true
		}
		matched++
		t.Logf("=== item %s path=%s ===", lookup, path)
		printed := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			t.Logf("    %s", strings.TrimRight(line, "\r"))
			printed++
			if printed >= 60 {
				t.Logf("    ...(截断)")
				break
			}
		}
		return true
	})
	t.Logf("item %s 命中文件 %d 个", lookup, matched)
}

// TestScanStackablePVFByPattern 按任意 PVF 片段检索所有 stackable，用来寻找某一类
// 道具（例如所有"防虚弱药剂"版本）在 PVF 里的**共同特征**，避免按 itemID 写死。
//
//	$env:DNFBRIDGE_REAL_PVF_SMOKE='D:\test\地下城与勇士\Script.pvf'
//	$env:DNFBRIDGE_REAL_PVF_GREP='虚弱'          # 或 '\[cooltime group\]\s+5\b'
//	go test ./internal/services/dnfbridge -run TestScanStackablePVFByPattern -v
func TestScanStackablePVFByPattern(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	pattern := os.Getenv("DNFBRIDGE_REAL_PVF_GREP")
	if pvfPath == "" || pattern == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE and DNFBRIDGE_REAL_PVF_GREP to scan")
	}
	rx, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("bad DNFBRIDGE_REAL_PVF_GREP: %v", err)
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatalf("load real Script.pvf: %v", err)
	}
	// Authoritative itemID -> path map, parsed exactly like the live catalog.
	pathToID := map[string]int64{}
	if listText, err := archive.ReadText(dungeonDropStackableList); err == nil {
		if listDoc, err := dnfpvf.Parse(dungeonDropStackableList, listText); err == nil {
			for _, entry := range dnfpvf.ParseList(listDoc) {
				clean := cleanDungeonDropPath(entry.Path)
				if entry.ID <= 0 || entry.ID > math.MaxUint32 || clean == "" {
					continue
				}
				pathToID[strings.ToLower("stackable/"+clean)] = entry.ID
			}
		}
	}
	t.Logf("stackable.lst 条目=%d", len(pathToID))
	namePattern := regexp.MustCompile("(?m)^\\s*\\[name\\]\\s+`([^`]*)`")
	explainPattern := regexp.MustCompile("(?m)^\\s*\\[explain\\]\\s+`([^`]*)`")
	scanned, hits := 0, 0
	archive.RangePaths(func(path string) bool {
		lower := strings.ToLower(path)
		if !strings.HasSuffix(lower, ".stk") && !strings.HasSuffix(lower, ".stak") {
			return true
		}
		text, err := archive.ReadText(path)
		if err != nil {
			return true
		}
		scanned++
		// Match the [explain] field itself only: a whole-document match also
		// drags in gift boxes that merely mention the potion in their text.
		explain := ""
		if m := explainPattern.FindStringSubmatch(text); len(m) == 2 {
			explain = m[1]
		}
		if !rx.MatchString(explain) {
			return true
		}
		hits++
		name := ""
		if m := namePattern.FindStringSubmatch(text); len(m) == 2 {
			name = m[1]
		}
		t.Logf("ID=%d|%s|%s|%s", pathToID[lower], strings.TrimSpace(name),
			strings.TrimSpace(explain), path)
		return true
	})
	t.Logf("扫描 %d 个 stackable，explain 命中 %d 个（pattern=%q）", scanned, hits, pattern)
}
