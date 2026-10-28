// Command stringdump lists and dumps the client string-table entries of a
// runtime Script.pvf. The client resolves UI/error texts through
// `GetString(id)` (DNF.exe VA 0x0367BE00); ids such as 0x7947 (party-teleport
// consent text) or 0x7958 (menu refusal) are looked up in a table shipped inside
// the PVF, so the text of a packet-driven dialog can be read without guessing.
//
// Usage:
//
//	stringdump -pvf <Script.pvf> -list string
//	stringdump -pvf <Script.pvf> -file <relative/path> -head 40
//	stringdump -pvf <Script.pvf> -file <relative/path> -id 0x7947 -id 0x7958
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "path to Script.pvf")
	list := flag.String("list", "", "print file paths containing this substring (case-insensitive)")
	file := flag.String("file", "", "relative path to dump")
	head := flag.Int("head", 20, "lines to print when dumping a file")
	ids := flag.String("ids", "", "comma-separated string ids to search for in the dumped file")
	flag.Parse()

	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "missing -pvf")
		os.Exit(2)
	}
	archive, err := pvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("archive files=%d\n", archive.FileCount())

	if *list != "" {
		needle := strings.ToLower(*list)
		printed := 0
		archive.RangePaths(func(path string) bool {
			if strings.Contains(strings.ToLower(path), needle) {
				fmt.Println(path)
				printed++
			}
			return printed < 400
		})
		fmt.Printf("matched=%d\n", printed)
		return
	}

	if *file == "" {
		return
	}
	text, err := archive.ReadText(*file)
	if err != nil {
		raw, rawErr := archive.ReadRaw(*file)
		if rawErr != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v / %v\n", *file, err, rawErr)
			os.Exit(1)
		}
		fmt.Printf("raw bytes=%d\n", len(raw))
		dumpHex(raw, *head*32)
		return
	}
	lines := strings.Split(text, "\n")
	fmt.Printf("path=%s text_bytes=%d lines=%d\n", *file, len(text), len(lines))
	for i, line := range lines {
		if i >= *head {
			break
		}
		fmt.Printf("%4d| %s\n", i, strings.TrimRight(line, "\r"))
	}
	if *ids == "" {
		return
	}
	for _, token := range strings.Split(*ids, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		value, convErr := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(token), "0x"), 16, 64)
		if convErr != nil {
			fmt.Fprintf(os.Stderr, "bad id %q\n", token)
			continue
		}
		reportID(text, value)
	}
}

var numberRun = regexp.MustCompile(`\d+`)

// reportID finds the lines that mention one id, which is how the client string
// tables are written (`<id>\t<string>` or `<index>\t<id>\t<string>`).
func reportID(text string, id int64) {
	needles := []string{strconv.FormatInt(id, 10), fmt.Sprintf("0x%x", id)}
	var hits []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		fields := strings.Split(trimmed, "\t")
		if len(fields) < 2 {
			continue
		}
		for _, needle := range needles {
			if strings.TrimSpace(fields[0]) == needle {
				hits = append(hits, trimmed)
				break
			}
		}
		if len(hits) >= 3 {
			break
		}
	}
	fmt.Printf("id=0x%x (%d) hits=%d\n", id, id, len(hits))
	for _, hit := range hits {
		fmt.Printf("    %s\n", hit)
	}
	_ = numberRun
}

func dumpHex(raw []byte, limit int) {
	if limit > len(raw) {
		limit = len(raw)
	}
	for offset := 0; offset < limit; offset += 16 {
		end := offset + 16
		if end > limit {
			end = limit
		}
		var hex strings.Builder
		var text strings.Builder
		for _, value := range raw[offset:end] {
			fmt.Fprintf(&hex, "%02X ", value)
			if value >= 0x20 && value < 0x7F {
				text.WriteByte(value)
			} else {
				text.WriteByte('.')
			}
		}
		fmt.Printf("%08X  %-48s %s\n", offset, hex.String(), text.String())
	}
}
