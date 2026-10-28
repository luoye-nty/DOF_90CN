// fiendwarareas 打印超时空的区域图（PVF [AREA INFO]：kind / dungeon / member max / next），
// 用于回答"某一格与哪几格相邻"这类走位问题。
//
//	go run ./cmd/scratch/fiendwarareas -pvf <Script.pvf> [-area 7,11,16]
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	dnfraid "longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	areaList := flag.String("area", "", "只打印这些区域（逗号分隔），空 = 全部")
	flag.Parse()
	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	config, err := dnfraid.LoadFiendWarConfig(archive)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	if config.Battle == nil {
		fmt.Fprintln(os.Stderr, "no battle config")
		os.Exit(1)
	}
	want := map[int]bool{}
	for _, field := range strings.Split(*areaList, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, convErr := strconv.Atoi(field)
		if convErr != nil {
			fmt.Fprintln(os.Stderr, "bad area:", field)
			os.Exit(2)
		}
		want[value] = true
	}
	ids := make([]int, 0, len(config.Battle.Areas))
	for id := range config.Battle.Areas {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		if len(want) > 0 && !want[id] {
			continue
		}
		area := config.Battle.Areas[id]
		next := append([]int(nil), area.Next...)
		sort.Ints(next)
		fmt.Printf("area %2d  kind=%-12s dungeon=%-5d member_max=%d  next=%v\n",
			id, area.Kind, area.Dungeon, area.MemberMax, next)
	}
}
