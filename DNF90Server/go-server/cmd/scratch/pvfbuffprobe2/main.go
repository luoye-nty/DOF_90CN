package main

import (
	"fmt"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func nm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}

type stat struct {
	files                    int
	sectionsTotal            int
	topLevel                 int
	nested                   int
	mod3bad                  int
	firstNotString           int
	docsNoCloseTag           int
	docsSkillLevelupAfterEnc int
}

func scan(archive *platformpvf.Archive, suffix string, out *stat, firstTags map[string]int, sampleBad []string) {
	paths := []string{}
	archive.RangePaths(func(p string) bool {
		if strings.HasSuffix(strings.ToLower(p), suffix) {
			paths = append(paths, p)
		}
		return true
	})
	sort.Strings(paths)
	for _, p := range paths {
		text, err := archive.ReadText(p)
		if err != nil {
			continue
		}
		if !strings.Contains(text, "[skill levelup]") {
			continue
		}
		out.files++
		doc, perr := dnfpvf.Parse(p, text)
		if perr != nil {
			continue
		}
		if !strings.Contains(text, "[/skill levelup]") {
			out.docsNoCloseTag++
		}
		sawEnchant := false
		for _, sec := range doc.Sections {
			n := nm(sec.Name)
			if n == "enchant" || n == "enchant table" || n == "enchant index" {
				sawEnchant = true
				continue
			}
			if n != "skill levelup" {
				continue
			}
			out.sectionsTotal++
			if sawEnchant {
				out.docsSkillLevelupAfterEnc++
				out.nested++
			} else {
				out.topLevel++
			}
			toks := doc.Tokens[sec.Start:sec.End]
			if len(toks)%3 != 0 {
				out.mod3bad++
				if len(sampleBad) < 12 {
					raw := ""
					for _, t := range toks {
						raw += t.Raw + "|"
					}
					sampleBad = append(sampleBad, fmt.Sprintf("%s mod3=%d :: %s", p, len(toks)%3, raw))
				}
			}
			for i := 0; i+2 < len(toks); i += 3 {
				t := toks[i]
				if t.Kind != dnfpvf.TokenString && t.Kind != dnfpvf.TokenIdent {
					out.firstNotString++
					break
				}
				firstTags[strings.ToLower(t.Value)]++
			}
		}
	}
}

func main() {
	archive, err := platformpvf.Open(`DNF_REAL_PVF`)
	if err != nil {
		panic(err)
	}
	// enchant container presence in .equ
	equPaths := []string{}
	archive.RangePaths(func(p string) bool {
		if strings.HasSuffix(strings.ToLower(p), ".equ") {
			equPaths = append(equPaths, p)
		}
		return true
	})
	withEnchant := 0
	withEnchantClose := 0
	withEnchantTable := 0
	withEnchantIndex := 0
	enchantVariantSamples := []string{}
	for _, p := range equPaths {
		text, err := archive.ReadText(p)
		if err != nil {
			continue
		}
		hasE := strings.Contains(text, "[enchant]")
		hasT := strings.Contains(text, "[enchant table]")
		hasI := strings.Contains(text, "[enchant index]")
		if hasE || hasT || hasI {
			withEnchant++
			if hasT {
				withEnchantTable++
			}
			if hasI {
				withEnchantIndex++
			}
			if strings.Contains(text, "[/enchant]") || strings.Contains(text, "[/enchant table]") || strings.Contains(text, "[/enchant index]") {
				withEnchantClose++
			} else if len(enchantVariantSamples) < 6 {
				i := strings.Index(text, "[enchant")
				end := i + 200
				if end > len(text) {
					end = len(text)
				}
				enchantVariantSamples = append(enchantVariantSamples, fmt.Sprintf("==== %s (NO CLOSE TAG) ====\n%s", p, text[i:end]))
			}
		}
	}
	fmt.Printf("[.equ] total=%d withEnchantAny=%d withEnchantTable=%d withEnchantIndex=%d withAnyCloseTag=%d\n\n",
		len(equPaths), withEnchant, withEnchantTable, withEnchantIndex, withEnchantClose)
	for _, s := range enchantVariantSamples {
		fmt.Println(s)
	}

	equ := stat{}
	tags := map[string]int{}
	bad := []string{}
	scan(archive, ".equ", &equ, tags, bad)
	fmt.Printf("\n=== .equ [skill levelup] ===\nsections=%d topLevel=%d nestedAfterEnchant=%d mod3bad=%d firstNotString=%d docsNoClose=%d\n",
		equ.sectionsTotal, equ.topLevel, equ.nested, equ.mod3bad, equ.firstNotString, equ.docsNoCloseTag)
	keys := []string{}
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-22s %d\n", k, tags[k])
	}
	fmt.Println("### mod3 samples:")
	for _, s := range bad {
		fmt.Println("  " + s)
	}

	stk := stat{}
	tags2 := map[string]int{}
	bad2 := []string{}
	scan(archive, ".stk", &stk, tags2, bad2)
	fmt.Printf("\n=== .stk [skill levelup] ===\nfiles=%d sections=%d topLevel=%d nestedAfterEnchant=%d mod3bad=%d firstNotString=%d\n",
		stk.files, stk.sectionsTotal, stk.topLevel, stk.nested, stk.mod3bad, stk.firstNotString)
	keys = keys[:0]
	for k := range tags2 {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-22s %d\n", k, tags2[k])
	}
}
