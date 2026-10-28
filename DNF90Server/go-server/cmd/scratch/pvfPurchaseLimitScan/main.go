// 扫描 Script.pvf 里所有带 [purchase limit] 的脚本，打印路径 + 道具名 + 该字段原文，
// 用于确认"账号永久/账号每周/角色永久/角色每周"四种限购在 PVF 里的实际写法。
package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `D:/test/地下城与勇士/Script.pvf`, "Script.pvf path")
	limit := flag.Int("limit", 60, "max files to print")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	type row struct {
		path  string
		name  string
		block string
	}
	rows := make([]row, 0, 256)
	shapes := map[string]int{}
	files := 0
	for _, file := range archive.Files() {
		text, readErr := archive.ReadText(file.ArchivePath)
		if readErr != nil {
			continue
		}
		lower := strings.ToLower(text)
		idx := strings.Index(lower, "[purchase limit]")
		if idx < 0 {
			continue
		}
		files++
		// [name] 块
		name := ""
		if n := strings.Index(lower, "[name]"); n >= 0 {
			rest := text[n+len("[name]"):]
			if a := strings.Index(rest, "`"); a >= 0 {
				if b := strings.Index(rest[a+1:], "`"); b >= 0 {
					name = strings.TrimSpace(rest[a+1 : a+1+b])
				}
			}
		}
		// [purchase limit] 之后到下一个 '[' 之前的内容
		rest := text[idx:]
		end := len(rest)
		if e := strings.Index(rest[len("[purchase limit]"):], "\n["); e >= 0 {
			end = len("[purchase limit]") + e
		}
		block := strings.Join(strings.Fields(rest[:end]), " ")
		rows = append(rows, row{path: file.ArchivePath, name: name, block: block})
		shapes[block]++
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	fmt.Printf("带 [purchase limit] 的文件数=%d，不同写法=%d 种\n\n", files, len(shapes))
	fmt.Println("=== 不同字段写法(按出现次数) ===")
	type kv struct {
		k string
		n int
	}
	list := make([]kv, 0, len(shapes))
	for k, n := range shapes {
		list = append(list, kv{k, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	for _, e := range list {
		fmt.Printf("%4d 次  |  %s\n", e.n, e.k)
	}
	fmt.Printf("\n=== 前 %d 个文件明细 ===\n", *limit)
	for i, r := range rows {
		if i >= *limit {
			break
		}
		fmt.Printf("%-60s %-28s %s\n", r.path, r.name, r.block)
	}
}
