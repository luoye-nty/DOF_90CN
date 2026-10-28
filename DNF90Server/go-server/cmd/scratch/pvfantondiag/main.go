// pvfantondiag 用服务端真实的安东攻坚 PVF 解析器做最小变异实验，
// 把「桌面 PVF 开不了团」的根因收敛到单一行改动上。
//
//	go run ./cmd/scratch/pvfantondiag -pvf runtime/data/dnf/Script.pvf
package main

import (
	"flag"
	"fmt"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/raid"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type memSource struct{ text string }

func (m *memSource) ReadText(string) (string, error) { return m.text, nil }

func check(label, text string) {
	config, err := raid.LoadAntonConfig(&memSource{text: text})
	if err != nil {
		fmt.Printf("  [%s] 拒绝: %v\n", label, err)
		return
	}
	fmt.Printf("  [%s] 通过: phaseMax=%d memberMax=%d startMin=%d 阶段数=%d\n",
		label, config.PhaseMax, config.MemberMax, config.StartMinimumMembers, len(config.Phases))
	for i, p := range config.Phases {
		fmt.Printf("         阶段%d %q 奖励%d 条:", i+1, p.Name, len(p.Rewards))
		for _, r := range p.Rewards {
			fmt.Printf(" %s/%d/%d/%d", r.Kind, r.Weight, r.DefinitionItemID, r.Flag)
		}
		fmt.Println()
	}
}

func main() {
	pvfPath := flag.String("pvf", "", "基线 Script.pvf")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	base, err := archive.ReadText(raid.AntonPVFPath)
	if err != nil {
		fmt.Println("read:", err)
		return
	}
	if _, err := dnfpvf.Parse(raid.AntonPVFPath, base); err != nil {
		fmt.Println("parse:", err)
		return
	}

	fmt.Println("基线（服务端现用 etc/raid/anton.etc）:")
	check("baseline", base)

	fmt.Println("\n变异 1：只把 `squad_item` 1 10157775 1 的权重改成 0")
	m1 := strings.Replace(base, "`squad_item` -1 1 10157775 1", "`squad_item` -1 0 10157775 1", 1)
	if m1 == base {
		fmt.Println("  !! 未命中替换目标")
	} else {
		check("weight=0", m1)
	}

	fmt.Println("\n变异 2：只删掉 `squad_item` -1 50 10157773 0 这一条（权重不动）")
	m2 := strings.Replace(base, "[STATE REWARD]\n`squad_item` -1 50 10157773 0 \n", "", 1)
	m2 = strings.Replace(m2, "[STATE REWARD]\n`squad_item` -1 50 10157773 0\n", "", 1)
	if m2 == base {
		fmt.Println("  !! 未命中替换目标")
	} else {
		check("删一条奖励", m2)
	}

	fmt.Println("\n变异 3：权重 0 + 删除那一条（= 桌面 PVF 的改法）")
	if m1 != base && m2 != base {
		m3 := strings.Replace(m2, "`squad_item` -1 1 10157775 1", "`squad_item` -1 0 10157775 1", 1)
		check("两者同时", m3)
	}
}
