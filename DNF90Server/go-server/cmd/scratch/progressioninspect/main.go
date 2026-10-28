package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"longheng.io/server/internal/modules/dnf/progression"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "../runtime/data/dnf/Script.pvf", "runtime Script.pvf")
	maxLevel := flag.Int("max-level", 20, "last progression level to print")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tables, err := progression.Load(context.Background(), archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("snapshot=%+v\n", tables.Snapshot())
	for level := 1; level <= *maxLevel; level++ {
		threshold, thresholdErr := tables.ThresholdToNext(level)
		sp, spOK := tables.SkillPointsAtLevel(level)
		tp, tpOK := tables.TechniquePointsAtLevel(level)
		fmt.Printf("level=%d threshold=%d threshold_err=%v sp=%d sp_ok=%t tp=%d tp_ok=%t\n",
			level, threshold, thresholdErr, sp, spOK, tp, tpOK)
	}
}
