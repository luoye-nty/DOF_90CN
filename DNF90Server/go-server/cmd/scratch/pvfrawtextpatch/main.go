// pvfrawtextpatch changes one literal inside one raw text-like PVF member.
// It supports ASCII and UTF-16LE members and never modifies the input in place.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	input := flag.String("pvf", "", "input PVF")
	output := flag.String("out", "", "output PVF")
	memberPath := flag.String("file", "", "archive member")
	oldValue := flag.String("old", "", "old literal")
	newValue := flag.String("new", "", "new literal")
	flag.Parse()
	if *input == "" || *output == "" || *memberPath == "" || *oldValue == "" || *newValue == "" {
		panic("-pvf, -out, -file, -old and -new are required")
	}
	inputAbs, err := filepath.Abs(*input)
	if err != nil {
		panic(err)
	}
	outputAbs, err := filepath.Abs(*output)
	if err != nil {
		panic(err)
	}
	if strings.EqualFold(inputAbs, outputAbs) {
		panic("output must differ from input")
	}
	archive, err := platformpvf.Open(inputAbs)
	if err != nil {
		panic(err)
	}
	raw, err := archive.ReadRaw(*memberPath)
	if err != nil {
		panic(err)
	}
	oldASCII, newASCII := []byte(*oldValue), []byte(*newValue)
	oldWide, newWide := utf16Bytes(*oldValue), utf16Bytes(*newValue)
	var oldBytes, newBytes []byte
	switch {
	case bytes.Count(raw, oldASCII) == 1:
		oldBytes, newBytes = oldASCII, newASCII
	case bytes.Count(raw, oldWide) == 1:
		oldBytes, newBytes = oldWide, newWide
	default:
		panic(fmt.Sprintf("literal %q was not found exactly once in raw member %s", *oldValue, *memberPath))
	}
	patchedRaw := bytes.Replace(raw, oldBytes, newBytes, 1)
	patched, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: *memberPath, Data: patchedRaw}})
	if err != nil {
		panic(err)
	}
	verified, err := platformpvf.OpenBytes(patched)
	if err != nil {
		panic(err)
	}
	text, err := verified.ReadText(*memberPath)
	if err != nil {
		panic(err)
	}
	if strings.Contains(text, *oldValue) || !strings.Contains(text, *newValue) {
		panic("verification failed")
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outputAbs, patched, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("patched file=%s old=%q new=%q raw_bytes=%d->%d output=%s\n", *memberPath, *oldValue, *newValue, len(raw), len(patchedRaw), outputAbs)
}

func utf16Bytes(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(out[index*2:], unit)
	}
	return out
}
