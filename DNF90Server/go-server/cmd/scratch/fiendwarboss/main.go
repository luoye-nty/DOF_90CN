// fiendwarboss 把 PVF 里某个目录（或某个文件）的原始文本倒到磁盘，用于人工比对
// 超时空 BOSS 的机制定义（罗什的四属性分身顺序、机制条 boss pattern state 的数字位）。
//
//	go run ./cmd/scratch/fiendwarboss -pvf <Script.pvf> -out <dir> -prefix monster/newmonsters/gcontents/180621_fiendwar/boss_cosmofiend_black
//	go run ./cmd/scratch/fiendwarboss -pvf <Script.pvf> -out <dir> -files etc/raid/fiendwar/fiendwar.etc
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	out := flag.String("out", "", "导出目录")
	prefix := flag.String("prefix", "", "按路径前缀导出（可多次用逗号分隔）")
	files := flag.String("files", "", "按完整路径导出（逗号分隔）")
	flag.Parse()
	if *pvfPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-pvf and -out are required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	prefixes := splitList(*prefix)
	exact := splitList(*files)
	dumped, scanned := 0, 0
	for _, file := range archive.Files() {
		full := file.ArchivePath
		if strings.TrimSpace(full) == "" {
			full = file.Path
		}
		lower := strings.ToLower(full)
		match := false
		for _, want := range exact {
			if lower == strings.ToLower(want) {
				match = true
				break
			}
		}
		if !match {
			for _, want := range prefixes {
				if strings.HasPrefix(lower, strings.ToLower(want)) {
					match = true
					break
				}
			}
		}
		if !match {
			continue
		}
		scanned++
		text, readErr := archive.ReadText(full)
		if readErr != nil {
			fmt.Printf("SKIP %s: %v\n", full, readErr)
			continue
		}
		target := filepath.Join(*out, filepath.FromSlash(full))
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", mkErr)
			os.Exit(1)
		}
		if writeErr := os.WriteFile(target, []byte(text), 0o644); writeErr != nil {
			fmt.Fprintln(os.Stderr, "write:", writeErr)
			os.Exit(1)
		}
		dumped++
		fmt.Printf("DUMP %s (%d bytes)\n", full, len(text))
	}
	fmt.Printf("scanned=%d dumped=%d\n", scanned, dumped)
}

func splitList(value string) []string {
	out := make([]string, 0, 4)
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}
