// pvfgrep 只读：在 Script.pvf 里按文本行搜字面量（regex），打印命中文档的路径与上下文。
//
// 现场 2026-10-03：确认 `0 1000000` 这段 [int data] 属于哪个文件（任务？道具？）——
// 任务表里扫不到，所以要按扩展名全库扫。
//
// 只读。跑法：
//
//	go run ./cmd/scratch/pvfgrep -pvf "...\Script.pvf" -re "^\s*0\s+1000000\b" -ext qst,stk
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf (required)")
	pattern := flag.String("re", "", "line regex (required)")
	extFlag := flag.String("ext", "qst,stk,equ,etc", "comma separated extensions to scan")
	contextLines := flag.Int("ctx", 2, "context lines around a match")
	maxFiles := flag.Int("max", 20, "max matching documents to print")
	onlyPath := flag.String("only", "", "only paths containing this substring")
	flag.Parse()
	if *pvfPath == "" || *pattern == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -pvf and -re are required")
		os.Exit(2)
	}
	re, err := regexp.Compile(*pattern)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: bad regex:", err)
		os.Exit(2)
	}
	exts := make([]string, 0, 4)
	for _, ext := range strings.Split(*extFlag, ",") {
		trimmed := strings.ToLower(strings.TrimSpace(ext))
		if trimmed == "" {
			continue
		}
		exts = append(exts, "."+strings.TrimPrefix(trimmed, "."))
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: open pvf:", err)
		os.Exit(1)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{
		Lists: []string{"n_quest/quest.lst"},
		Prefixes: []string{
			"n_quest/", "stackable/", "equipment/", "etc/", "character/", "cash/",
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: build index:", err)
		os.Exit(1)
	}
	allPaths := index.Paths()
	paths := make([]string, 0, len(allPaths))
	for _, path := range allPaths {
		lower := strings.ToLower(path)
		keep := false
		for _, ext := range exts {
			if strings.HasSuffix(lower, ext) {
				keep = true
				break
			}
		}
		if keep {
			paths = append(paths, path)
		}
	}
	fmt.Fprintf(os.Stderr, "索引文档=%d，按扩展名过滤后=%d，扩展名=%v\n", len(allPaths), len(paths), exts)

	matched, scanned := 0, 0
	for _, path := range paths {
		lower := strings.ToLower(path)
		if *onlyPath != "" && !strings.Contains(lower, strings.ToLower(*onlyPath)) {
			continue
		}
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			continue
		}
		scanned++
		lines := strings.Split(text, "\n")
		hits := make([]int, 0, 4)
		for index, line := range lines {
			if re.MatchString(line) {
				hits = append(hits, index)
			}
		}
		if len(hits) == 0 {
			continue
		}
		matched++
		if matched > *maxFiles {
			continue
		}
		fmt.Printf("=== %s  (%d hit) ===\n", path, len(hits))
		for _, at := range hits {
			from := at - *contextLines
			if from < 0 {
				from = 0
			}
			to := at + *contextLines
			if to >= len(lines) {
				to = len(lines) - 1
			}
			for line := from; line <= to; line++ {
				marker := "  "
				if line == at {
					marker = ">>"
				}
				fmt.Printf("%s %s\n", marker, strings.TrimRight(lines[line], "\r"))
			}
			fmt.Println("  ---")
		}
	}
	fmt.Printf("\n扫描文档=%d  命中文件=%d\n", scanned, matched)
}
