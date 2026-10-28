// pvfdirtree 对比两个 PVF 中指定前缀（目录）下的文件，并以「解码后的脚本文本」为准导出，
// 便于直接 diff。type-1 是「字符串池 + 5 字节令牌流」，不同重打包工具会得到语义相同、
// 字节不同的令牌流，所以原始字节比对会产生假差异，必须用解码文本。
//
//	go run ./cmd/scratch/pvfdirtree -a A.pvf -b B.pvf -prefix etc/raid -list
//	go run ./cmd/scratch/pvfdirtree -a A.pvf -b B.pvf -prefix etc/raid -out D:\tmp\cmp
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	aPath := flag.String("a", "", "PVF A")
	bPath := flag.String("b", "", "PVF B")
	prefix := flag.String("prefix", "", "只处理该前缀（大小写不敏感的路径前缀）")
	out := flag.String("out", "", "导出目录")
	list := flag.Bool("list", false, "只列出差异路径，不导出")
	verbose := flag.Bool("v", false, "同时打印未变化路径")
	flag.Parse()

	if *aPath == "" || *bPath == "" {
		fmt.Fprintln(os.Stderr, "-a 与 -b 必填")
		os.Exit(2)
	}
	archA, err := platformpvf.Open(*aPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open A:", err)
		os.Exit(1)
	}
	archB, err := platformpvf.Open(*bPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open B:", err)
		os.Exit(1)
	}

	key := strings.ToLower(strings.ReplaceAll(*prefix, "\\", "/"))

	var matched []string
	seen := map[string]bool{}
	add := func(p string) {
		lp := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
		if key != "" && !strings.HasPrefix(lp, key) {
			return
		}
		if seen[lp] {
			return
		}
		seen[lp] = true
		matched = append(matched, p)
	}
	for _, f := range archA.Files() {
		add(f.ArchivePath)
	}
	for _, f := range archB.Files() {
		add(f.ArchivePath)
	}
	sort.Strings(matched)

	fmt.Printf("前缀 %q 命中路径 %d 条\n", *prefix, len(matched))

	same, diff, onlyA, onlyB := 0, 0, 0, 0
	for _, p := range matched {
		textA, errA := archA.ReadText(p)
		textB, errB := archB.ReadText(p)
		switch {
		case errA != nil && errB != nil:
			fmt.Printf("  ERR-BOTH %s\n", p)
			continue
		case errA != nil:
			onlyB++
			fmt.Printf("  ONLY-B   %s\n", p)
			continue
		case errB != nil:
			onlyA++
			fmt.Printf("  ONLY-A   %s\n", p)
			continue
		}
		if textA == textB {
			same++
			if *verbose {
				fmt.Printf("  SAME     %s\n", p)
			}
			continue
		}
		diff++
		fmt.Printf("  DIFF     %-70s lenA=%d lenB=%d\n", p, len(textA), len(textB))
		if !*list && *out != "" {
			writeTree(*out, "A", p, textA)
			writeTree(*out, "B", p, textB)
		}
	}

	fmt.Printf("\n合计: 文本相同 %d  文本不同 %d  A独有 %d  B独有 %d\n", same, diff, onlyA, onlyB)
}

func writeTree(out, side, archivePath, text string) {
	rel := strings.TrimPrefix(strings.ReplaceAll(archivePath, "\\", "/"), "/")
	target := filepath.Join(out, side, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		return
	}
	if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
	}
}
