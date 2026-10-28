// Command cerashopgrep 把 PVF 里的商城文本文件按原文抽出来搜。
//
// 为什么需要它（2026-10-05）：排查"某几笔商城结算是 0 元，是免费商品还是标价配错"时，
// 现成的 `pvfcerashopinspect` **把行宽硬编码成 9**，而真实解析器
// （internal/services/dnfbridge/cera_shop_catalog.go）对不同分区用**不同行宽**：
//
//	item / premium / creature / coin / material / recoveryitem  宽 9
//	visual                                                      宽 8
//	package / regular package / community package               宽 11
//
// 于是 package（本服 484 行，绝大多数商品都在这里）会被整片解析歪掉 ——
// 打印出来的商品号是错的，用它去核对"某个商品号在不在表里"会得到**假否定**。
// 现场就是这么被误导了一次。
//
// 本工具不解析、不猜行宽，只做两件不会错的事：
//  1. 打印原文里所有匹配行，**连同它所属的 [分区] 与整行字段**（整行里就含标价）；
//  2. 可以只列分区表，确认某个分区到底存不存在。
//
// 用法：
//
//	cerashopgrep -pvf <Script.pvf> -grep 720032
//	cerashopgrep -pvf <Script.pvf> -file etc/chn_cerashop_multibonus.etc -grep 720032
//	cerashopgrep -pvf <Script.pvf> -sections
package main

import (
	"flag"
	"fmt"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", `../runtime/data/dnf/Script.pvf`, "Script.pvf path")
	file := flag.String("file", "etc/cerashop.etc", "PVF 内文件路径")
	pattern := flag.String("grep", "", "要搜的子串（商品号等）；空 = 打印全文")
	sectionsOnly := flag.Bool("sections", false, "只列分区表与每区行数")
	context := flag.Int("context", 0, "命中行的上下文行数")
	// window 是命中处前后各取多少个字符。
	//
	// **本文件的分区是"一整行"**（117 行的文件装着全部商品），所以命中一处就会把
	// 上万字符的整行打出来，真正要看的那几个字段反而被淹掉。>0 时改成按字符开窗，
	// 只打命中点周围的片段 —— 商品行本身很短（十几到几十个字段字符），足够看清标价。
	window := flag.Int("window", 0, "按字符开窗：命中处前后各取 N 个字符")
	flag.Parse()

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		panic(err)
	}
	text, err := archive.ReadText(*file)
	if err != nil {
		panic(fmt.Errorf("read %s: %w", *file, err))
	}
	lines := strings.Split(text, "\n")
	fmt.Printf("file=%s lines=%d\n", *file, len(lines))

	if *sectionsOnly {
		section := ""
		counts := map[string]int{}
		order := []string{}
		for _, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				section = strings.Trim(trimmed, "[]")
				if _, seen := counts[section]; !seen {
					order = append(order, section)
				}
				continue
			}
			if section != "" && trimmed != "" {
				counts[section]++
			}
		}
		for _, name := range order {
			fmt.Printf("  [%s] rows=%d\n", name, counts[name])
		}
		return
	}

	// 命中行连同所属分区一起打印：整行字段里就含标价，不用再猜行宽。
	section := ""
	printed := 0
	// window 模式：先定位分区（按整行跟踪），再按字符开窗打印。
	if *window > 0 && *pattern != "" {
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				section = strings.Trim(trimmed, "[]")
			}
			for at := 0; ; {
				idx := strings.Index(ln[at:], *pattern)
				if idx < 0 {
					break
				}
				start := at + idx
				at = start + len(*pattern)
				lo := start - *window
				if lo < 0 {
					lo = 0
				}
				hi := at + *window
				if hi > len(ln) {
					hi = len(ln)
				}
				fmt.Printf("L%d [%s] …%s…\n", i+1, section, strings.TrimSpace(ln[lo:hi]))
				printed++
			}
		}
		fmt.Printf("matches=%d\n", printed)
		return
	}

	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.Trim(trimmed, "[]")
		}
		if *pattern == "" || strings.Contains(ln, *pattern) {
			lo := i - *context
			if lo < 0 {
				lo = 0
			}
			hi := i + *context
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for j := lo; j <= hi; j++ {
				mark := " "
				if j == i {
					mark = ">"
				}
				fmt.Printf("%s L%-6d [%s] %s\n", mark, j+1, section, strings.TrimRight(lines[j], "\r"))
			}
			printed++
		}
	}
	fmt.Printf("matches=%d\n", printed)
}
