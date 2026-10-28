// pvflukemodepatch aligns the Luke single/normal mode selector with the Luke
// frame groups already present in raid_areaimg.img.
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

const memberPath = "ui/raidwindow/raidselectsinglemodewindow.xui"

type replacement struct {
	old string
	new string
}

var replacements = []replacement{
	{
		old: `DisableImg="NUM:5" DownImg="NUM:4" ID="STR:BUTTON_ONE"`,
		new: `DisableImg="NUM:13" DownImg="NUM:12" ID="STR:BUTTON_ONE"`,
	},
	{
		old: `ID="STR:BUTTON_ONE" ImagePack="STR:Interface/newstyle/windows/party/raid_areaimg/raid_areaimg.img" OverImg="NUM:3"`,
		new: `ID="STR:BUTTON_ONE" ImagePack="STR:Interface/newstyle/windows/party/raid_areaimg/raid_areaimg.img" OverImg="NUM:11"`,
	},
	{
		old: `Pos="VECTOR:3, 3" Size="VECTOR:264, 48" UpImg="NUM:2" patternImgPack="STR:Interface/windowcommon.img"`,
		new: `Pos="VECTOR:3, 3" Size="VECTOR:264, 48" UpImg="NUM:10" patternImgPack="STR:Interface/windowcommon.img"`,
	},
	{
		old: `DisableImg="NUM:9" DownImg="NUM:8" ID="STR:BUTTON_TWO"`,
		new: `DisableImg="NUM:17" DownImg="NUM:16" ID="STR:BUTTON_TWO"`,
	},
	{
		old: `ID="STR:BUTTON_TWO" ImagePack="STR:Interface/newstyle/windows/party/raid_areaimg/raid_areaimg.img" OverImg="NUM:7"`,
		new: `ID="STR:BUTTON_TWO" ImagePack="STR:Interface/newstyle/windows/party/raid_areaimg/raid_areaimg.img" OverImg="NUM:15"`,
	},
	{
		old: `Pos="VECTOR:3, 52" Size="VECTOR:264, 48" UpImg="NUM:6" />`,
		new: `Pos="VECTOR:3, 52" Size="VECTOR:264, 48" UpImg="NUM:14" />`,
	},
}

func main() {
	input := flag.String("pvf", "", "input PVF")
	output := flag.String("out", "", "output PVF")
	flag.Parse()
	if *input == "" || *output == "" {
		panic("-pvf and -out are required")
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
	raw, err := archive.ReadRaw(memberPath)
	if err != nil {
		panic(err)
	}
	patched := append([]byte(nil), raw...)
	for _, item := range replacements {
		patched = replaceExactlyOnce(patched, item.old, item.new)
	}
	outputBytes, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: memberPath, Data: patched}})
	if err != nil {
		panic(err)
	}
	verified, err := platformpvf.OpenBytes(outputBytes)
	if err != nil {
		panic(err)
	}
	text, err := verified.ReadText(memberPath)
	if err != nil {
		panic(err)
	}
	for _, item := range replacements {
		if strings.Contains(text, item.old) || !strings.Contains(text, item.new) {
			panic(fmt.Sprintf("verification failed for %q", item.old))
		}
	}
	if err := os.MkdirAll(filepath.Dir(outputAbs), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(outputAbs, outputBytes, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("patched file=%s replacements=%d raw_bytes=%d output=%s\n", memberPath, len(replacements), len(patched), outputAbs)
}

func replaceExactlyOnce(raw []byte, oldValue, newValue string) []byte {
	oldASCII, newASCII := []byte(oldValue), []byte(newValue)
	oldWide, newWide := utf16Bytes(oldValue), utf16Bytes(newValue)
	switch {
	case bytes.Count(raw, oldASCII) == 1:
		return bytes.Replace(raw, oldASCII, newASCII, 1)
	case bytes.Count(raw, oldWide) == 1:
		return bytes.Replace(raw, oldWide, newWide, 1)
	default:
		panic(fmt.Sprintf("literal %q was not found exactly once", oldValue))
	}
}

func utf16Bytes(value string) []byte {
	units := utf16.Encode([]rune(value))
	out := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(out[index*2:], unit)
	}
	return out
}
