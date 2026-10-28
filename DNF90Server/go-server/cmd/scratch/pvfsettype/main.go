// Scratch: force one PVF file-table entry's dataType (e.g. 3 -> 1) in place.
//
//	go run ./cmd/scratch/pvfsettype -pvf <Script.pvf> -path stackable/10000001/_test123.stk -type 1
//	go run ./cmd/scratch/pvfsettype -pvf <Script.pvf> -path <...> -type 1 -dry-run
package main

import (
	"flag"
	"fmt"
	"os"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	target := flag.String("path", "", "archive path of the entry to patch")
	want := flag.Int("type", 1, "desired dataType")
	dryRun := flag.Bool("dry-run", false, "do not write, only report")
	flag.Parse()

	if *target == "" {
		fmt.Fprintln(os.Stderr, "-path is required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	for _, file := range archive.Files() {
		if file.ArchivePath == *target {
			fmt.Printf("before: idx=%d type=%d size=%d path=%q\n",
				file.Index, file.DataType, file.Size, file.ArchivePath)
		}
	}

	patched, err := archive.PatchFileDataType(*target, *want)
	if err != nil {
		fmt.Fprintln(os.Stderr, "patch:", err)
		os.Exit(1)
	}
	reopened, err := platformpvf.OpenBytes(patched)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify open:", err)
		os.Exit(1)
	}
	for _, file := range reopened.Files() {
		if file.ArchivePath == *target {
			fmt.Printf("after : idx=%d type=%d size=%d path=%q\n",
				file.Index, file.DataType, file.Size, file.ArchivePath)
		}
	}
	if *dryRun {
		fmt.Println("dry-run: not written")
		return
	}
	if err := os.WriteFile(*pvfPath, patched, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("written: %s (%d bytes)\n", *pvfPath, len(patched))
}
