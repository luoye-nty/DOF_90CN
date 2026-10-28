// pvfmember 只读：打印一个 PVF 成员的全部文本，或只打印命中关键字的窗口。
//
// 为什么要单独写：pvfcat 对每个文档有 40KB 硬上限，而
// etc/variousbufflist.etc（buff 定义表）有 201KB、etc/variousbuffstackablelist.etc
// （道具→buff 表）也不小，关键条目正好落在被截断的那一段里。这个工具不做截断，
// 并且支持只取某个 id 附近的上下文，避免把几十万字符刷进终端。
//
// 跑法：
//
//	go run ./cmd/scratch/pvfmember -pvf "...\Script.pvf" -file etc/variousbufflist.etc -grep 1136 -ctx 25
//	go run ./cmd/scratch/pvfmember -pvf "...\Script.pvf" -file etc/variousbuffstackablelist.etc -grep 490002458
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf (required)")
	member := flag.String("file", "", "archive member path (required)")
	grep := flag.String("grep", "", "optional substring; only print windows around matches")
	ctx := flag.Int("ctx", 20, "lines of context around each match")
	maxMatches := flag.Int("max", 8, "max matches to print")
	flag.Parse()
	if *pvfPath == "" || *member == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -pvf and -file are required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: open pvf:", err)
		os.Exit(1)
	}
	body, err := archive.ReadText(strings.ReplaceAll(*member, "\\", "/"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED: read member:", err)
		os.Exit(1)
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	if *grep == "" {
		fmt.Printf("===== %s (%d lines) =====\n", *member, len(lines))
		for index, line := range lines {
			fmt.Printf("%5d  %s\n", index+1, line)
		}
		return
	}
	printed := 0
	lastPrinted := -1
	for index, line := range lines {
		if !strings.Contains(line, *grep) {
			continue
		}
		if printed >= *maxMatches {
			fmt.Printf("… 还有更多命中未打印（-max 调整）\n")
			break
		}
		start := index - *ctx
		if start < 0 {
			start = 0
		}
		end := index + *ctx
		if end >= len(lines) {
			end = len(lines) - 1
		}
		if start <= lastPrinted {
			start = lastPrinted + 1
		}
		fmt.Printf("===== %s  match %q @line %d =====\n", *member, *grep, index+1)
		for i := start; i <= end; i++ {
			fmt.Printf("%5d  %s\n", i+1, lines[i])
		}
		lastPrinted = end
		printed++
	}
	if printed == 0 {
		fmt.Printf("no match for %q in %s (%d lines)\n", *grep, *member, len(lines))
	}
}
