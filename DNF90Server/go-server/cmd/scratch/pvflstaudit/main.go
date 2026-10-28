// pvflstaudit 审计 PVF 内 `.lst` 索引文件的结构完整性（只读，不修改任何库）。
//
// 回答四个问题：
//  1. id / 路径是否成对（token 总数是否为偶数、每对是否真的是 int + 引用）；
//  2. id 有无重复（重复会把哪一条路径先命中暴露出来）；
//  3. 每一条路径在归档里是否真实存在（同时报告「按 .lst 所在目录相对解析」的结果）；
//  4. 指定 id 列表的落点（是否存在、解析到什么文件、该文件是否存在）。
//
// 用法:
//
//	go run ./cmd/scratch/pvflstaudit -pvf <Script.pvf> -list stackable/stackable.lst -ids 202609180,202690000
//	go run ./cmd/scratch/pvflstaudit -pvf <Script.pvf> -all
//	go run ./cmd/scratch/pvflstaudit -pvf <Script.pvf> -list stackable/stackable.lst -dump out.txt
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

type entry struct {
	id    int64
	ref   string
	index int // 第几对（0 起）
}

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	listPath := flag.String("list", "stackable/stackable.lst", "list path")
	all := flag.Bool("all", false, "审计归档内全部 .lst")
	ids := flag.String("ids", "", "逗号分隔的 id，逐个报告落点")
	dump := flag.String("dump", "", "把解析出的 id/路径对照表写到本地文件")
	limit := flag.Int("limit", 30, "每类问题最多打印多少条")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	// 归档路径索引：一次遍历建好，供路径存在性检查使用。
	existing := map[string]string{} // 小写路径 -> 真实路径
	count := 0
	for _, f := range archive.Files() {
		// File.Path 是所在目录，File.ArchivePath 才是完整路径。
		full := f.ArchivePath
		if strings.TrimSpace(full) == "" {
			full = f.Path
		}
		key := strings.ToLower(strings.ReplaceAll(full, "\\", "/"))
		if _, ok := existing[key]; !ok {
			existing[key] = full
		}
		count++
	}
	fmt.Printf("归档: %s\n", *pvfPath)
	fmt.Printf("文件总数: %d\n", count)

	targets := []string{}
	if *all {
		for _, f := range archive.Files() {
			full := f.ArchivePath
			if strings.TrimSpace(full) == "" {
				full = f.Path
			}
			if strings.HasSuffix(strings.ToLower(full), ".lst") {
				targets = append(targets, full)
			}
		}
		sort.Strings(targets)
		fmt.Printf("发现 .lst: %d 个\n", len(targets))
	} else {
		targets = []string{*listPath}
	}

	wantIDs := map[int64]bool{}
	var wantList []int64
	if strings.TrimSpace(*ids) != "" {
		for _, tok := range strings.Split(*ids, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			v, convErr := strconv.ParseInt(tok, 10, 64)
			if convErr != nil {
				fmt.Fprintf(os.Stderr, "bad id %q: %v\n", tok, convErr)
				os.Exit(2)
			}
			wantIDs[v] = true
			wantList = append(wantList, v)
		}
	}

	var dumpLines []string
	totalIssues := 0

	for _, lp := range targets {
		text, readErr := archive.ReadText(lp)
		if readErr != nil {
			fmt.Printf("\n### %s\n读不到: %v\n", lp, readErr)
			totalIssues++
			continue
		}
		fmt.Printf("\n### %s  (%d 字节)\n", lp, len(text))

		tokens := tokenize(text)
		fmt.Printf("token 总数: %d  ->  %s\n", len(tokens),
			map[bool]string{true: "偶数，可完整配对 ✅", false: "奇数，末尾有孤立 token ❌"}[len(tokens)%2 == 0])

		entries := make([]entry, 0, len(tokens)/2)
		var badPairs int
		for i := 0; i+1 < len(tokens); i += 2 {
			rawID := tokens[i]
			rawRef := tokens[i+1]
			id, convErr := strconv.ParseInt(rawID, 10, 64)
			if convErr != nil {
				badPairs++
				if badPairs <= *limit {
					fmt.Printf("  非数字 id 位置 pair#%d: id=%q ref=%q\n", i/2, rawID, rawRef)
				}
				continue
			}
			entries = append(entries, entry{id: id, ref: rawRef, index: i / 2})
		}
		if badPairs > 0 {
			fmt.Printf("非数字 id 的对数: %d ❌\n", badPairs)
			totalIssues += badPairs
		}
		fmt.Printf("解析出 id/路径 对: %d\n", len(entries))

		// ---- 重复 id ----
		byID := map[int64][]entry{}
		for _, e := range entries {
			byID[e.id] = append(byID[e.id], e)
		}
		dupIDs := 0
		var dupKeys []int64
		for id, list := range byID {
			if len(list) > 1 {
				dupIDs++
				dupKeys = append(dupKeys, id)
			}
		}
		sort.Slice(dupKeys, func(i, j int) bool { return dupKeys[i] < dupKeys[j] })
		fmt.Printf("重复 id 个数: %d\n", dupIDs)
		if dupIDs > 0 {
			totalIssues += dupIDs
			for i, id := range dupKeys {
				if i >= *limit {
					fmt.Printf("  ... 其余 %d 个重复 id 省略\n", dupIDs-*limit)
					break
				}
				var refs []string
				for _, e := range byID[id] {
					refs = append(refs, fmt.Sprintf("pair#%d:%s", e.index, e.ref))
				}
				fmt.Printf("  DUPLICATE id=%d -> %s\n", id, strings.Join(refs, "  "))
			}
		}

		// ---- 路径存在性 ----
		missing := 0
		emptyRef := 0
		dir := path.Dir(strings.ReplaceAll(lp, "\\", "/"))
		if dir == "." {
			dir = ""
		}
		var missingSamples []string
		var resolvedRel int
		for _, e := range entries {
			ref := strings.TrimSpace(e.ref)
			if ref == "" {
				emptyRef++
				continue
			}
			// .lst 里的引用可能用反斜杠，先统一成正斜杠再解析。
			norm := strings.ReplaceAll(ref, "\\", "/")
			key := strings.ToLower(norm)
			if _, ok := existing[key]; ok {
				continue
			}
			// 再按「相对 .lst 所在目录」解析一次
			if dir != "" {
				rel := path.Clean(path.Join(dir, norm))
				if _, ok := existing[strings.ToLower(rel)]; ok {
					resolvedRel++
					continue
				}
			}
			missing++
			if len(missingSamples) < *limit {
				missingSamples = append(missingSamples, fmt.Sprintf("id=%d ref=%s", e.id, ref))
			}
		}
		fmt.Printf("空引用: %d\n", emptyRef)
		fmt.Printf("路径缺失(两种解析都不存在): %d\n", missing)
		if resolvedRel > 0 {
			fmt.Printf("需按 .lst 所在目录相对解析才存在的路径: %d（正常，说明是相对引用）\n", resolvedRel)
		}
		for _, s := range missingSamples {
			fmt.Printf("  MISSING %s\n", s)
		}
		totalIssues += missing + emptyRef

		// ---- 指定 id ----
		if len(wantList) > 0 {
			fmt.Printf("-- 指定 id 落点 --\n")
			for _, id := range wantList {
				list := byID[id]
				if len(list) == 0 {
					fmt.Printf("  id=%d : 未在此 .lst 注册 ❌\n", id)
					continue
				}
				for _, e := range list {
					ref := strings.TrimSpace(e.ref)
					norm := strings.ReplaceAll(ref, "\\", "/")
					status := "文件存在 ✅"
					actual := ""
					if real, ok := existing[strings.ToLower(norm)]; ok {
						actual = real
					} else if dir != "" {
						rel := path.Clean(path.Join(dir, norm))
						if real, ok := existing[strings.ToLower(rel)]; ok {
							actual = real
							status = "文件存在（相对解析）✅"
						}
					}
					if actual == "" {
						status = "文件不存在 ❌"
					}
					fmt.Printf("  id=%d : pair#%d ref=%q -> %s (%s)\n", id, e.index, ref, actual, status)
				}
			}
		}

		if *dump != "" {
			dumpLines = append(dumpLines, fmt.Sprintf("# %s  entries=%d", lp, len(entries)))
			for _, e := range entries {
				dumpLines = append(dumpLines, fmt.Sprintf("%d\t%s", e.id, e.ref))
			}
		}
	}

	if *dump != "" {
		if werr := os.WriteFile(*dump, []byte(strings.Join(dumpLines, "\n")+"\n"), 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, "dump:", werr)
		} else {
			fmt.Printf("\n已写出对照表: %s (%d 行)\n", *dump, len(dumpLines))
		}
	}

	fmt.Printf("\n===== 结构问题合计: %d =====\n", totalIssues)
	if totalIssues == 0 {
		fmt.Println("结论: 所查 .lst 结构完整（成对、无重复 id、路径全部存在）")
	}
}

// tokenize 按空白切分，并把 `path` 的反引号剥掉。
// 反引号内的内容不切分（路径理论上不含空白，这里只为稳健）。
func tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	inTick := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case r == '`':
			if inTick {
				inTick = false
				flush()
			} else {
				flush()
				inTick = true
			}
		case !inTick && (r == ' ' || r == '\t' || r == '\r' || r == '\n'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
