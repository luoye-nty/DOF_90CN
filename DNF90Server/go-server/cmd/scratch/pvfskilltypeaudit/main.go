// pvfskilltypeaudit lists current-PVF skill documents that omit [type] but
// still expose executable states. Those are castable skills and must remain
// eligible for the server's normal quick-slot validation.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `D:/DNF/runtime/data/dnf/Script.pvf`, "Script.pvf path")
	flag.Parse()
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		panic(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{dnfskill.DefaultList}})
	if err != nil {
		panic(err)
	}
	catalog, err := dnfskill.Load(context.Background(), index, dnfskill.Options{})
	if err != nil {
		panic(err)
	}
	rows := make([]dnfskill.Skill, 0)
	for _, definition := range catalog.Skills() {
		if strings.TrimSpace(definition.Kind) == "" && definition.Active {
			rows = append(rows, definition)
		}
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].Job != rows[right].Job {
			return rows[left].Job < rows[right].Job
		}
		return rows[left].ID < rows[right].ID
	})
	for _, definition := range rows {
		fmt.Printf("job=%d skill=%d level=%d class=%d name=%s path=%s\n",
			definition.Job, definition.ID, definition.RequiredLevel, definition.SkillClass, definition.Name, definition.Path)
	}
	fmt.Printf("total=%d\n", len(rows))
}
