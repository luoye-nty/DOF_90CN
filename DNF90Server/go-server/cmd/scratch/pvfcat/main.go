// Scratch tool: print PVF text documents by path, or locate an item id by
// scanning every .lst in the archive.
//
//	pvfcat [-pvf path] <doc path>...
//	pvfcat [-pvf path] -find <item id>
package main

import (
	"flag"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	find := flag.String("find", "", "item id to locate through .lst files")
	maxBytes := flag.Int("max", 40000, "max bytes to print per document")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	if *find != "" {
		target, convErr := strconv.ParseInt(*find, 10, 64)
		if convErr != nil {
			fmt.Fprintln(os.Stderr, "bad id:", convErr)
			os.Exit(2)
		}
		found := false
		for _, file := range archive.Files() {
			// 注意：File.Path 是文件表里的「目录」字段，完整路径在 File.ArchivePath。
			// 以前这里用 file.Path 判 .lst 后缀，导致每个 .lst 都被跳过、-find 永远报
			// 「not found in any .lst」——一个会骗人的静默 bug。
			full := file.ArchivePath
			if strings.TrimSpace(full) == "" {
				full = file.Path
			}
			if !strings.HasSuffix(strings.ToLower(full), ".lst") {
				continue
			}
			text, readErr := archive.ReadText(full)
			if readErr != nil {
				continue
			}
			fields := strings.Fields(text)
			for i := 0; i+1 < len(fields); i++ {
				id, parseErr := strconv.ParseInt(fields[i], 10, 64)
				if parseErr != nil || id != target {
					continue
				}
				rel := strings.Trim(fields[i+1], "`")
				// .lst 里的路径是**相对 .lst 所在目录**的，所以基准要用完整路径的目录，
				// 而不是 file.Path（那是「stackable」本身，path.Dir 会得到 "."，相对解析就丢了）。
				docPath := path.Clean(path.Join(path.Dir(full), strings.ReplaceAll(rel, "\\", "/")))
				fmt.Printf("LST: %s -> %s\n", full, docPath)
				if body, docErr := archive.ReadText(docPath); docErr == nil {
					printDoc(docPath, body, *maxBytes)
				} else {
					fmt.Println("read:", docErr)
				}
				found = true
			}
		}
		if !found {
			fmt.Println("not found in any .lst")
		}
		return
	}

	for _, docPath := range flag.Args() {
		body, readErr := archive.ReadText(strings.ReplaceAll(docPath, "\\", "/"))
		if readErr != nil {
			fmt.Printf("===== %s =====\nread: %v\n", docPath, readErr)
			continue
		}
		printDoc(docPath, body, *maxBytes)
	}
}

func printDoc(docPath string, body string, maxBytes int) {
	fmt.Printf("===== %s =====\n", docPath)
	if len(body) > maxBytes {
		fmt.Print(body[:maxBytes])
		fmt.Printf("\n... truncated at %d bytes (total %d)\n", maxBytes, len(body))
		return
	}
	fmt.Print(body)
	if !strings.HasSuffix(body, "\n") {
		fmt.Println()
	}
}
