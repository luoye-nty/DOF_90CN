package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	archive, err := platformpvf.Open(`../runtime/data/dnf/Script.pvf`)
	if err != nil {
		panic(err)
	}
	target, _ := strconv.ParseInt(os.Args[1], 10, 64)
	lst, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		panic(err)
	}
	fields := strings.Fields(lst)
	for i := 0; i+1 < len(fields); i++ {
		id, parseErr := strconv.ParseInt(fields[i], 10, 64)
		if parseErr != nil || id != target {
			continue
		}
		rel := strings.Trim(fields[i+1], "`")
		docPath := "stackable/" + rel
		fmt.Println("PATH:", docPath)
		text, docErr := archive.ReadText(docPath)
		if docErr != nil {
			panic(docErr)
		}
		fmt.Println(text)
		return
	}
	fmt.Println("not found in stackable.lst")
}
