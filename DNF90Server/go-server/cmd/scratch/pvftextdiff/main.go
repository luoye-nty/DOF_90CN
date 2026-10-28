// pvftextdiff 对两个 PVF 做「语义」全量差异统计：
//   - type-1 / type-3 文件按解码后的文本比较（原始字节在不同打包器之间不可比）
//   - 解码文本为空的二进制文件回落到原始字节比较
//
// 输出：差异清单文件 + 按顶层目录/扩展名的聚合。
//
//	go run ./cmd/scratch/pvftextdiff -a A.pvf -b B.pvf -list out.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func topDir(p string) string {
	l := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	if i := strings.Index(l, "/"); i > 0 {
		return l[:i]
	}
	return "(root)"
}

func extOf(p string) string {
	l := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	slash := strings.LastIndexByte(l, '/')
	name := l
	if slash >= 0 {
		name = l[slash+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return "(no-ext)"
	}
	return name[dot:]
}

func main() {
	aPath := flag.String("a", "", "PVF A")
	bPath := flag.String("b", "", "PVF B")
	listPath := flag.String("list", "", "差异清单输出文件")
	flag.Parse()

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

	var list *os.File
	var w *bufio.Writer
	if *listPath != "" {
		list, err = os.Create(*listPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create list:", err)
			os.Exit(1)
		}
		defer list.Close()
		w = bufio.NewWriter(list)
		defer w.Flush()
	}

	byTop := map[string]int{}
	byExt := map[string]int{}
	textDiff, binDiff := 0, 0
	common := 0

	for _, fileA := range archA.Files() {
		p := fileA.ArchivePath
		if _, ok := archB.FindFile(p); !ok {
			continue
		}
		common++
		textA, errA := archA.ReadText(p)
		textB, errB := archB.ReadText(p)
		kind := ""
		switch {
		case errA == nil && errB == nil && textA != textB:
			kind = "TEXT"
			textDiff++
		default:
			// 解码文本相同或不可解码：回落到原始字节。
			rawA, rawErrA := archA.ReadRaw(p)
			rawB, rawErrB := archB.ReadRaw(p)
			if rawErrA == nil && rawErrB == nil && string(rawA) != string(rawB) && textA == textB {
				kind = "BIN"
				binDiff++
			}
		}
		if kind == "" {
			continue
		}
		byTop[topDir(p)]++
		byExt[extOf(p)]++
		if w != nil {
			fmt.Fprintf(w, "%s\t%s\n", kind, p)
		}
	}

	// B 独有路径
	bOnly := 0
	bOnlyTop := map[string]int{}
	bOnlyExt := map[string]int{}
	for _, fileB := range archB.Files() {
		p := fileB.ArchivePath
		if archA.FindFileIndex(strings.ToLower(strings.ReplaceAll(p, "\\", "/"))) >= 0 {
			continue
		}
		if _, ok := archA.FindFile(p); ok {
			continue
		}
		bOnly++
		bOnlyTop[topDir(p)]++
		bOnlyExt[extOf(p)]++
		if w != nil {
			fmt.Fprintf(w, "ONLY-B\t%s\n", p)
		}
	}

	fmt.Printf("公共路径 %d ；语义不同（文本）%d ；不同（二进制）%d ；B 独有 %d\n", common, textDiff, binDiff, bOnly)

	dump := func(m map[string]int, title string, n int) {
		type kv struct {
			k string
			v int
		}
		a := make([]kv, 0, len(m))
		for k, v := range m {
			a = append(a, kv{k, v})
		}
		sort.Slice(a, func(i, j int) bool {
			if a[i].v != a[j].v {
				return a[i].v > a[j].v
			}
			return a[i].k < a[j].k
		})
		fmt.Printf("\n== %s ==\n", title)
		for i, e := range a {
			if i >= n {
				break
			}
			fmt.Printf("  %-24s %d\n", e.k, e.v)
		}
	}
	dump(byTop, "语义不同 · 按顶层目录", 30)
	dump(byExt, "语义不同 · 按扩展名", 25)
	dump(bOnlyTop, "B 独有 · 按顶层目录", 30)
	dump(bOnlyExt, "B 独有 · 按扩展名", 25)
}
