// Scratch: compare a newly added PVF file entry against existing neighbours.
//
//	go run ./cmd/scratch/pvfcmp -pvf <path> -new stackable/10000001/_test123.stk
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	newPath := flag.String("new", "", "path of the newly added file")
	neigh := flag.Int("neigh", 4, "how many neighbours to print")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	files := archive.Files()
	fmt.Printf("total files: %d\n", len(files))
	target := -1
	want := strings.ToLower(strings.TrimPrefix(*newPath, "data/dnf/"))
	for i, f := range files {
		if strings.ToLower(f.Path) == want || strings.ToLower(f.ArchivePath) == want {
			target = i
			break
		}
	}
	if target < 0 {
		fmt.Printf("NOT FOUND in file table: %s\n", *newPath)
		// show the tail of the table, where an appended file usually lands
		for _, f := range files[len(files)-*neigh:] {
			fmt.Printf("  tail idx=%d type=%d size=%d path=%q archive=%q\n",
				f.Index, f.DataType, f.Size, f.Path, f.ArchivePath)
		}
		return
	}
	lo := target - *neigh
	if lo < 0 {
		lo = 0
	}
	hi := target + *neigh + 1
	if hi > len(files) {
		hi = len(files)
	}
	for _, f := range files[lo:hi] {
		mark := "   "
		if f.Path == files[target].Path && f.Index == files[target].Index {
			mark = "-> "
		}
		fmt.Printf("%sidx=%d type=%d size=%d path=%q archive=%q\n",
			mark, f.Index, f.DataType, f.Size, f.Path, f.ArchivePath)
	}
}
