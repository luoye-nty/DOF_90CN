// monsterinfo 按怪物代码反查 PVF 定义（名称/等级/rank/路径），用于把刷图日志里的
// monster_id 还原成怪物名，例如判定「房间 1:0 里掉 3330 的 420 到底是不是蜘蛛怪」。
//
//	go run ./cmd/scratch/monsterinfo -pvf <Script.pvf> -ids 420,400,600
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/monster"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	ids := flag.String("ids", "", "逗号分隔的怪物 ID")
	listPath := flag.String("list", monster.DefaultList, "怪物表路径")
	flag.Parse()
	if *pvfPath == "" || *ids == "" {
		fmt.Fprintln(os.Stderr, "-pvf and -ids are required")
		os.Exit(2)
	}
	want := make([]int64, 0, 8)
	for _, field := range strings.Split(*ids, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad id:", field)
			os.Exit(2)
		}
		want = append(want, value)
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{*listPath}, Paths: []string{*listPath}})
	if err != nil {
		panic(err)
	}
	table, err := monster.Load(context.Background(), index, monster.Options{ListPath: *listPath})
	if err != nil {
		panic(err)
	}
	rows := make([]monster.Monster, 0, len(want))
	for _, id := range want {
		entry, found := table.Find(id)
		if !found {
			rows = append(rows, monster.Monster{ID: id, Name: "(未在怪物表中找到)"})
			continue
		}
		rows = append(rows, entry)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rows); err != nil {
		panic(err)
	}
}
