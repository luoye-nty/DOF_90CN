package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `D:/test/地下城与勇士/Script.pvf`, "Script.pvf path")
	docPath := flag.String("doc", "etc/cerashop.etc", "document path inside the archive")
	outPath := flag.String("out", "", "output text path")
	filter := flag.String("filter", "装扮栏", "print only lines containing this text")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	text, err := archive.ReadText(*docPath)
	if err != nil {
		panic(err)
	}
	if *outPath != "" {
		if err := os.WriteFile(*outPath, []byte(text), 0644); err != nil {
			panic(err)
		}
	}
	count := 0
	for _, line := range strings.Split(text, "\n") {
		if *filter != "" && !strings.Contains(line, *filter) {
			continue
		}
		fmt.Println(strings.TrimRight(line, "\r"))
		count++
	}
	fmt.Fprintf(os.Stderr, "lines=%d filter=%q\n", count, *filter)
}
