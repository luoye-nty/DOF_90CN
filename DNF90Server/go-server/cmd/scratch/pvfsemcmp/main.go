// pvfsemcmp 逐路径比较两个 PVF「解码后的文本」，而不是原始字节。
//
// 动机：字符串池被重排（只追加 vs 全量重建）会让每个字符串 token 的 magic offset 变化，
// 于是几乎所有文件的原始字节都不同——但解码后的文本可能完全一致（说明重排是自洽的、无害的），
// 也可能变成乱码（说明池和 token 不再对应，客户端必然解析失败）。
//
// 用法:
//
//	go run ./cmd/scratch/pvfsemcmp -a <A.pvf> -b <B.pvf> [-max-examples 20] [-prefix stackable/]
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	aPath := flag.String("a", "", "基线 PVF")
	bPath := flag.String("b", "", "改后 PVF")
	maxExamples := flag.Int("max-examples", 20, "每类最多打印多少条示例")
	prefix := flag.String("prefix", "", "只比较该前缀（可选）")
	limit := flag.Int("limit", 0, "最多比较多少个文件（0=全部）")
	flag.Parse()
	if *aPath == "" || *bPath == "" {
		fmt.Fprintln(os.Stderr, "-a and -b are required")
		os.Exit(2)
	}

	a, err := platformpvf.Open(*aPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open A:", err)
		os.Exit(2)
	}
	b, err := platformpvf.Open(*bPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open B:", err)
		os.Exit(2)
	}

	prefixKey := strings.ToLower(*prefix)
	bIdx := map[string]string{}
	for _, f := range b.Files() {
		bIdx[strings.ToLower(f.ArchivePath)] = f.ArchivePath
	}

	var compared, identical, different, onlyA, readErr int
	var examples []string

	for _, fa := range a.Files() {
		key := strings.ToLower(fa.ArchivePath)
		if prefixKey != "" && !strings.HasPrefix(key, prefixKey) {
			continue
		}
		bp, ok := bIdx[key]
		if !ok {
			onlyA++
			continue
		}
		if *limit > 0 && compared >= *limit {
			break
		}
		ta, errA := a.ReadText(fa.ArchivePath)
		tb, errB := b.ReadText(bp)
		if errA != nil || errB != nil {
			readErr++
			continue
		}
		compared++
		if ta == tb {
			identical++
			continue
		}
		different++
		if len(examples) < *maxExamples {
			examples = append(examples, fmt.Sprintf("  - %s\n      A: %s\n      B: %s",
				fa.ArchivePath, clip(ta), clip(tb)))
		}
	}

	fmt.Printf("比较文件数: %d\n", compared)
	fmt.Printf("解码后文本完全相同: %d\n", identical)
	fmt.Printf("解码后文本不同:     %d\n", different)
	fmt.Printf("只在 A 存在:        %d\n", onlyA)
	fmt.Printf("读取失败:           %d\n", readErr)
	if different > 0 {
		fmt.Printf("\n文本不同的示例:\n%s\n", strings.Join(examples, "\n"))
	}
	if different == 0 && readErr == 0 {
		fmt.Println("\n结论: 两个库语义一致 —— 字节差异只来自字符串池重排/重压缩，解码结果未变。")
	}
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
