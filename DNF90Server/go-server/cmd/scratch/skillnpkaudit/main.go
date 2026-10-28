// skillnpkaudit lists every skill assigned to one PVF grow-type branch,
// including both awakening stages from the paired second-growtype slots.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type row struct {
	ID                   int64   `json:"id"`
	Path                 string  `json:"path"`
	Name                 string  `json:"name,omitempty"`
	SkillClass           int64   `json:"skill_class,omitempty"`
	GrowtypeMaximum      []int64 `json:"growtype_maximum,omitempty"`
	SecondGrowtypeMax    []int64 `json:"second_growtype_maximum,omitempty"`
	SkillFitnessGrowtype []int64 `json:"skill_fitness_growtype,omitempty"`
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path")
	listPath := flag.String("list", "", "skill list path")
	branch := flag.Int("branch", -1, "zero-based growtype branch")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" || strings.TrimSpace(*listPath) == "" || *branch < 0 {
		fmt.Fprintln(os.Stderr, "-pvf, -list and non-negative -branch are required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	listText, err := archive.ReadText(*listPath)
	if err != nil {
		panic(err)
	}
	listDocument, err := dnfpvf.Parse(*listPath, listText)
	if err != nil {
		panic(err)
	}
	rows := make([]row, 0, 64)
	for _, entry := range dnfpvf.ParseList(listDocument) {
		docPath := resolve(archive, *listPath, entry.Path)
		if docPath == "" {
			continue
		}
		text, readErr := archive.ReadText(docPath)
		if readErr != nil {
			continue
		}
		doc, parseErr := dnfpvf.Parse(docPath, text)
		if parseErr != nil {
			continue
		}
		primary := doc.Ints("growtype maximum level")
		second := doc.Ints("second growtype maximum level")
		fitness := doc.Ints("skill fitness growtype")
		if !enabled(primary, second, fitness, *branch) {
			continue
		}
		name, _ := doc.Text("name")
		skillClass, _ := doc.Int("skill class")
		rows = append(rows, row{
			ID: entry.ID, Path: docPath, Name: name, SkillClass: skillClass,
			GrowtypeMaximum: primary, SecondGrowtypeMax: second,
			SkillFitnessGrowtype: fitness,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rows); err != nil {
		panic(err)
	}
}

func enabled(primary, second, fitness []int64, branch int) bool {
	if branch < len(primary) && primary[branch] > 0 {
		return true
	}
	firstAwakening := branch * 2
	if firstAwakening < len(second) && second[firstAwakening] > 0 {
		return true
	}
	if secondAwakening := firstAwakening + 1; secondAwakening < len(second) && second[secondAwakening] > 0 {
		return true
	}
	for _, value := range fitness {
		if int(value) == branch {
			return true
		}
	}
	return false
}

func resolve(archive *platformpvf.Archive, listPath, reference string) string {
	reference = strings.TrimSpace(strings.Trim(reference, "`\"'"))
	for _, candidate := range []string{reference, path.Join(path.Dir(listPath), reference)} {
		if file, ok := archive.FindFile(candidate); ok {
			return file.ArchivePath
		}
	}
	return ""
}
