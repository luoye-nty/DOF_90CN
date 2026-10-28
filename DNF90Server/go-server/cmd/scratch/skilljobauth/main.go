// skilljobauth dumps (job, skill id, name, kind, active) for the requested skill ids from the
// real client PVF. Skill ids are job-scoped in the current data (dnfskill.Key is {Job, ID}),
// so this tool exists to prove which class actually owns an id such as 31 or 234 — i.e. to
// show that a job-scoped gate cannot touch another class.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `data/dnf/Script.pvf`, "Script.pvf path")
	ids := flag.String("ids", "31,234", "comma separated skill ids to look up across every job")
	job := flag.Int("job", -1, "optional: only print rows for this job byte")
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
	wanted := make(map[uint16]bool)
	for _, part := range strings.Split(*ids, ",") {
		value, convErr := strconv.Atoi(strings.TrimSpace(part))
		if convErr != nil {
			continue
		}
		wanted[uint16(value)] = true
	}
	rows := make([]dnfskill.Skill, 0)
	for _, definition := range catalog.Skills() {
		if !wanted[definition.ID] {
			continue
		}
		if *job >= 0 && int(definition.Job) != *job {
			continue
		}
		rows = append(rows, definition)
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].ID != rows[right].ID {
			return rows[left].ID < rows[right].ID
		}
		return rows[left].Job < rows[right].Job
	})
	for _, definition := range rows {
		fmt.Printf("id=%-6d job=%-3d active=%-5v kind=%-16s name=%s\n",
			definition.ID, definition.Job, definition.Active, definition.Kind, definition.Name)
	}
	fmt.Printf("total rows=%d\n", len(rows))
}
