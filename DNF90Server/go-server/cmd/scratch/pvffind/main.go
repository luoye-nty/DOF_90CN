// pvffind 在归档的文件表里按子串搜索路径（大小写不敏感），并可选统计扩展名分布。
// 用来回答「这个包里到底有没有 stringtable.bin / n_string.lst / *.str」这类问题。
//
//	go run ./cmd/scratch/pvffind -pvf <Script.pvf> -contains stringtable
//	go run ./cmd/scratch/pvffind -pvf <Script.pvf> -ext .str
//	go run ./cmd/scratch/pvffind -pvf <Script.pvf> -ext-all
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	contains := flag.String("contains", "", "路径包含该子串（大小写不敏感）")
	ext := flag.String("ext", "", "按扩展名过滤，如 .str")
	extAll := flag.Bool("ext-all", false, "打印全部扩展名分布")
	limit := flag.Int("limit", 200, "最多打印多少条命中")
	flag.Parse()

	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	fmt.Printf("归档: %s\n格式: %s\n文件数: %d\n\n", *pvfPath, archive.Format(), archive.FileCount())

	if *extAll {
		counts := map[string]int{}
		for _, f := range archive.Files() {
			p := strings.ToLower(f.ArchivePath)
			idx := strings.LastIndex(p, ".")
			if idx < 0 || idx < strings.LastIndex(p, "/") {
				counts["(无扩展名)"]++
				continue
			}
			counts[p[idx:]]++
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
		fmt.Printf("扩展名共 %d 种：\n", len(keys))
		for _, k := range keys {
			fmt.Printf("  %-14s %d\n", k, counts[k])
		}
		return
	}

	needle := strings.ToLower(*contains)
	extKey := strings.ToLower(*ext)
	hits := 0
	for _, f := range archive.Files() {
		p := f.ArchivePath
		lower := strings.ToLower(p)
		if needle != "" && !strings.Contains(lower, needle) {
			continue
		}
		if extKey != "" && !strings.HasSuffix(lower, extKey) {
			continue
		}
		hits++
		if hits <= *limit {
			fmt.Printf("  idx=%-9d type=%-2d size=%-8d %s\n", f.Index, f.DataType, f.Size, p)
		}
	}
	if hits == 0 {
		fmt.Println("  （无命中）")
	} else {
		fmt.Printf("\n命中 %d 条\n", hits)
	}
}
