// Scratch: 把两个 PVF 的差异按「扩展名 + 文本差异签名」聚合，用于判断改动是什么、影响多大。
// 只读，不修改任何库。
//
//	go run ./cmd/scratch/pvfdiffsum -a <基线.pvf> -b <改后.pvf> [-sample 300] [-top 20]
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func extOf(p string) string {
	slash := strings.LastIndexByte(p, '/')
	name := p
	if slash >= 0 {
		name = p[slash+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return "(no-ext)"
	}
	return strings.ToLower(name[dot:])
}

// maskDigits 把数字换成 #，让「同一个模板、只是数值不同」的改动归到同一签名。
func maskDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteByte('#')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func lines(text string) []string {
	raw := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		trimmed := strings.TrimSpace(strings.ReplaceAll(line, "`", ""))
		if trimmed != "" {
			out = append(out, maskDigits(trimmed))
		}
	}
	return out
}

// signature 用「只在改后出现的行」与「只在基线出现的行」构成签名（均已掩码数字并排序去重）。
func signature(base, mod string) (string, int, int) {
	countA := map[string]int{}
	for _, line := range lines(base) {
		countA[line]++
	}
	countB := map[string]int{}
	for _, line := range lines(mod) {
		countB[line]++
	}
	var added, removed []string
	for line, n := range countB {
		if countA[line] < n {
			added = append(added, line)
		}
	}
	for line, n := range countA {
		if countB[line] < n {
			removed = append(removed, line)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sig := "+[" + strings.Join(added, " | ") + "] -[" + strings.Join(removed, " | ") + "]"
	if len(sig) > 150 {
		sig = sig[:150] + "…"
	}
	return sig, len(added), len(removed)
}

func main() {
	aPath := flag.String("a", "", "基线 PVF")
	bPath := flag.String("b", "", "改后 PVF")
	sample := flag.Int("sample", 300, "最多对多少个差异文件做文本签名分析")
	top := flag.Int("top", 20, "打印前多少组")
	flag.Parse()

	archiveA, err := platformpvf.Open(*aPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open A:", err)
		os.Exit(1)
	}
	archiveB, err := platformpvf.Open(*bPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open B:", err)
		os.Exit(1)
	}

	type group struct {
		ext     string
		sig     string
		count   int
		added   int
		removed int
		example string
	}
	groups := map[string]*group{}
	perExt := map[string]int{}
	differing := 0
	type pending struct {
		path string
		ext  string
	}
	var sampled []pending
	perExtSampled := map[string]int{}

	for _, fileA := range archiveA.Files() {
		path := fileA.ArchivePath
		_, ok := archiveB.FindFile(path)
		if !ok {
			continue
		}
		rawA, errA := archiveA.ReadRaw(path)
		rawB, errB := archiveB.ReadRaw(path)
		if errA != nil || errB != nil {
			continue
		}
		if string(rawA) == string(rawB) {
			continue
		}
		differing++
		ext := extOf(strings.ToLower(path))
		perExt[ext]++
		if perExtSampled[ext] < *sample {
			perExtSampled[ext]++
			sampled = append(sampled, pending{path, ext})
		}
	}
	fmt.Printf("差异文件总数: %d\n\n按扩展名:\n", differing)
	exts := make([]string, 0, len(perExt))
	for ext := range perExt {
		exts = append(exts, ext)
	}
	sort.Slice(exts, func(i, j int) bool { return perExt[exts[i]] > perExt[exts[j]] })
	for _, ext := range exts {
		fmt.Printf("  %-12s %d\n", ext, perExt[ext])
	}

	for _, item := range sampled {
		textA, errA := archiveA.ReadText(item.path)
		if errA != nil {
			continue
		}
		textB, errB := archiveB.ReadText(item.path)
		if errB != nil {
			continue
		}
		sig, added, removed := signature(textA, textB)
		key := item.ext + "\x00" + sig
		g := groups[key]
		if g == nil {
			g = &group{ext: item.ext, sig: sig, added: added, removed: removed, example: item.path}
			groups[key] = g
		}
		g.count++
	}

	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].ext < list[j].ext
	})

	fmt.Printf("\n差异签名分组（抽样 %d 个文件，共 %d 组，显示前 %d 组）:\n", len(sampled), len(list), *top)
	for idx, g := range list {
		if idx >= *top {
			break
		}
		fmt.Printf("\n[%d] %s × %d  (加 %d 行 / 删 %d 行)  例: %s\n    %s\n",
			idx+1, g.ext, g.count, g.added, g.removed, g.example, g.sig)
	}
}
