// raidsurveyscripts 扫描 Script.pvf，找出与攻坚队/频道相关的脚本文件，并在其中搜索关键词。
//
// 用途：客户端 EXE 里没有中文 UI 文案（文案在脚本包/数据文件里）。要判断"跨频道加入攻坚队后
// 客户端会不会自己换频道"，需要看 UI 侧脚本是否含"移动频道/切换频道"这类逻辑。
//
// 用法：
//
//	go run ./cmd/scratch/raidsurveyscripts -pvf "runtime\data\dnf\Script.pvf"
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `runtime\data\dnf\Script.pvf`, "Script.pvf path")
	pattern := flag.String("match", "raid", "comma separated path substrings (case-insensitive)")
	keyword := flag.String("keyword", "频道,移动,攻坚", "comma separated keywords to search inside matched files")
	maxFiles := flag.Int("max", 400, "max matched files to inspect")
	flag.Parse()

	archive, err := pvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open pvf:", err)
		os.Exit(1)
	}
	files := archive.Files()
	fmt.Printf("archive files=%d readable=%t\n", len(files), archive.CanReadFileData())

	patterns := strings.Split(strings.ToLower(*pattern), ",")
	keywords := strings.Split(*keyword, ",")

	matched := make([]pvf.File, 0, 64)
	for _, file := range files {
		lower := strings.ToLower(file.Path)
		for _, p := range patterns {
			p = strings.TrimSpace(p)
			if p != "" && strings.Contains(lower, p) {
				matched = append(matched, file)
				break
			}
		}
	}
	fmt.Printf("path matches=%d\n", len(matched))
	show := matched
	if len(show) > *maxFiles {
		show = show[:*maxFiles]
	}
	for _, file := range show {
		fmt.Printf("  %s (%d bytes)\n", file.Path, file.Size)
	}

	fmt.Println("--- keyword hits ---")
	hits := 0
	for _, file := range show {
		text, err := archive.ReadText(file.Path)
		if err != nil {
			continue
		}
		for _, kw := range keywords {
			kw = strings.TrimSpace(kw)
			if kw == "" || !strings.Contains(text, kw) {
				continue
			}
			lineNo := 0
			for _, line := range strings.Split(text, "\n") {
				lineNo++
				if strings.Contains(line, kw) {
					fmt.Printf("%s:%d: %s\n", file.Path, lineNo, strings.TrimSpace(line))
					hits++
				}
			}
		}
	}
	fmt.Printf("keyword hits=%d\n", hits)
}
