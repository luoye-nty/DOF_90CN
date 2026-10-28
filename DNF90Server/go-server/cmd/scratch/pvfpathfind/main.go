// Scratch tool: find PVF paths containing a substring, or dump one file's text.
//
//	go run ./cmd/scratch/pvfpathfind <Script.pvf> agit
//	go run ./cmd/scratch/pvfpathfind <Script.pvf> @town/town.lst | head -80
package main

import (
	"fmt"
	"os"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: pvfpathfind <Script.pvf> <substring|@exact/path>")
		return
	}
	bundle, err := pvf.Load(pvf.Options{Path: os.Args[1], MaxBytes: pvf.NoMaxBytes})
	if err != nil {
		fmt.Println("load:", err)
		return
	}
	archive, err := pvf.OpenArchive(bundle)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	needle := strings.ToLower(os.Args[2])
	if strings.HasPrefix(needle, "@") {
		text, err := archive.ReadText(os.Args[2][1:])
		if err != nil {
			fmt.Println("read:", err)
			return
		}
		fmt.Println(text)
		return
	}
	count := 0
	archive.RangePaths(func(p string) bool {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(p, "\\", "/")), needle) {
			count++
			if count <= 200 {
				fmt.Println(p)
			}
		}
		return true
	})
	fmt.Println("matches:", count)
}
