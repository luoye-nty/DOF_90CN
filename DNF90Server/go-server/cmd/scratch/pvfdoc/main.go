// pvfdoc prints one decoded PVF document by archive path, so a dropped item can
// be traced back to the exact stat block both the server and the client read.
// Usage: pvfdoc <archive path> [more paths...]
package main

import (
	"fmt"
	"os"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pvfdoc <archive path> [more paths...]")
		os.Exit(2)
	}
	pvfPath := "../runtime/data/dnf/Script.pvf"
	if override := os.Getenv("DNF90_PVF"); override != "" {
		pvfPath = override
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		panic(err)
	}
	for _, path := range os.Args[1:] {
		fmt.Printf("===== %s =====\n", path)
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			fmt.Printf("ERR: %v\n", readErr)
			continue
		}
		fmt.Println(text)
	}
}
