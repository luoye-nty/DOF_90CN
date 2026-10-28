package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

const targetDate = "2048-12-31 06:00:00"

var dateReplacements = map[string]string{
	"2017-01-19 06:00:00": targetDate,
	"2017-04-20 06:00:00": targetDate,
	"2017-06-01 06:00:00": targetDate,
	"2017-08-03 06:00:00": targetDate,
	"2017-11-02 06:00:00": targetDate,
	"2017-12-28 06:00:00": targetDate,
	"2018-01-01 06:00:00": targetDate,
	"2018-09-27 06:00:00": targetDate,
	"2018-09-30 06:00:00": targetDate,
	"2019-01-01 06:00:00": targetDate,
}

var oldDates = []string{
	"2017-01-19 06:00:00", "2017-04-20 06:00:00", "2017-06-01 06:00:00",
	"2017-08-03 06:00:00", "2017-11-02 06:00:00", "2017-12-28 06:00:00",
	"2018-01-01 06:00:00", "2018-09-27 06:00:00", "2018-09-30 06:00:00",
	"2019-01-01 06:00:00",
}

type report struct {
	Output        string   `json:"output"`
	Paths         []string `json:"paths"`
	ChangedTokens int      `json:"changed_tokens"`
	Bytes         int      `json:"bytes"`
}

func main() {
	in := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "input PVF")
	out := flag.String("out", `D:/DNF/runtime/tmp/damage-font-pvf-patch/Script.damage-font-20y.pvf`, "output PVF")
	flag.Parse()
	inAbs, err := filepath.Abs(*in)
	if err != nil {
		panic(err)
	}
	outAbs, err := filepath.Abs(*out)
	if err != nil {
		panic(err)
	}
	if strings.EqualFold(inAbs, outAbs) {
		panic("output must differ from input")
	}
	archive, err := platformpvf.Open(inAbs)
	if err != nil {
		panic(err)
	}
	paths, err := damageFontPaths(archive)
	if err != nil {
		panic(err)
	}
	changed := 0
	for _, path := range paths {
		for _, old := range oldDates {
			text, readErr := archive.ReadText(path)
			if readErr != nil {
				panic(readErr)
			}
			if !strings.Contains(text, old) {
				continue
			}
			newDate := dateReplacements[old]
			patched, patchReport, patchErr := archive.PatchScriptString([]string{path}, old, newDate)
			if patchErr != nil {
				panic(fmt.Errorf("patch %s %s: %w", path, old, patchErr))
			}
			archive, err = platformpvf.OpenBytes(patched)
			if err != nil {
				panic(fmt.Errorf("reopen %s: %w", path, err))
			}
			changed += patchReport.TokensChanged
		}
	}
	for _, path := range paths {
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			panic(readErr)
		}
		for _, old := range oldDates {
			if strings.Contains(text, old) {
				panic(fmt.Sprintf("old date remains in %s: %s", path, old))
			}
		}
		if !strings.Contains(text, targetDate) {
			continue
		}
	}
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		panic(err)
	}
	bytes := archive.Bytes()
	if err := os.WriteFile(outAbs, bytes, 0o644); err != nil {
		panic(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report{Output: outAbs, Paths: paths, ChangedTokens: changed, Bytes: len(bytes)}); err != nil {
		panic(err)
	}
}

func damageFontPaths(archive *platformpvf.Archive) ([]string, error) {
	listText, err := archive.ReadText("stackable/stackable.lst")
	if err != nil {
		return nil, err
	}
	listDoc, err := dnfpvf.Parse("stackable/stackable.lst", listText)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range dnfpvf.ParseList(listDoc) {
		ref := strings.Trim(strings.TrimSpace(entry.Path), "`\"'")
		path := filepath.ToSlash(filepath.Join("stackable", strings.TrimPrefix(ref, "stackable/")))
		text, readErr := archive.ReadText(path)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(path, text)
		if parseErr != nil {
			continue
		}
		action, _ := doc.Text("action type")
		if strings.EqualFold(strings.TrimSpace(action), "[add damage font skin]") {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
