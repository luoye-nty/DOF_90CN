// pvftokendump 把 protected PVF 里某个脚本的令牌流逐条打印出来：
// 序号、令牌类型、4 字节负载、以及字符串负载解出来的文本。
//
// 用途：看清「某个段的值是空的」在令牌流里到底是【没有 token】还是【一个指向空串/别处的 token】。
// 这正是「工具往空字段里塞看不见的字符串引用」那类问题的判据。
//
//	go run ./cmd/scratch/pvftokendump -pvf <Script.pvf> -path stackable/newstk/2026091800.stk
//	go run ./cmd/scratch/pvftokendump -pvf <Script.pvf> -path <文件> -around "[booster info]"
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	path := flag.String("path", "", "要看的文件归档路径")
	around := flag.String("around", "", "只打印包含该字符串的 token 前后各若干条")
	resolveList := flag.String("resolve", "", "逗号分隔的 magic offset，直接解析成字符串（不需要 -path）")
	ctx := flag.Int("ctx", 4, "around 的前后条数")
	flag.Parse()
	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf is required")
		os.Exit(2)
	}
	if *path == "" && *resolveList == "" {
		fmt.Fprintln(os.Stderr, "-path（或 -resolve）is required")
		os.Exit(2)
	}

	// -resolve 模式：只把给定的 magic offset 解出来，用来回答「偏移 0 指向哪个字符串」。
	if *resolveList != "" {
		archive, err := platformpvf.Open(*pvfPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(1)
		}
		fmt.Printf("归档: %s\n\n", *pvfPath)
		for _, tok := range strings.Split(*resolveList, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			var v int
			if _, e := fmt.Sscanf(tok, "%d", &v); e != nil {
				fmt.Fprintf(os.Stderr, "  bad offset %q\n", tok)
				continue
			}
			pool := "ANSI池"
			if v&1 != 0 {
				pool = "UTF16池"
			}
			fmt.Printf("  magic offset %-12d -> %-8s @%-12d %q\n", v, pool, v>>1, archive.ResolveString(v))
		}
		return
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	raw, err := archive.ReadRaw(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读不到 %s: %v\n", *path, err)
		os.Exit(1)
	}

	n := len(raw) / 5
	fmt.Printf("文件: %s\n原始字节: %d   令牌数: %d   （%d 字节不是 5 的整数倍则有余数）\n\n",
		*path, len(raw), n, len(raw)%5)
	if len(raw)%5 != 0 {
		fmt.Printf("【注意】余 %d 字节：令牌流长度不是 5 的倍数，这本身可疑。\n\n", len(raw)%5)
	}

	type tok struct {
		i     int
		kind  byte
		value int32
		text  string
	}
	toks := make([]tok, 0, n)
	for i := 0; i < n; i++ {
		off := i * 5
		kind := raw[off]
		v := int32(uint32(raw[off+1]) | uint32(raw[off+2])<<8 | uint32(raw[off+3])<<16 | uint32(raw[off+4])<<24)
		t := tok{i: i, kind: kind, value: v}
		switch kind {
		case 0, 2:
		case 3, 5, 6, 7:
			t.text = archive.ResolveString(int(v))
		}
		toks = append(toks, t)
	}

	kindName := func(k byte) string {
		switch k {
		case 0:
			return "整数"
		case 2:
			return "浮点"
		case 3:
			return "行字符串"
		case 5:
			return "{5=``}"
		case 6:
			return "反引号串"
		case 7:
			return "{7=``}"
		default:
			return "未知"
		}
	}
	poolOf := func(v int32) string {
		if v < 0 {
			return "-"
		}
		if v&1 != 0 {
			return "UTF16池"
		}
		return "ANSI池"
	}

	show := func(i int) {
		t := toks[i]
		extra := ""
		switch t.kind {
		case 0:
			extra = fmt.Sprintf("%d", t.value)
		case 2:
			extra = fmt.Sprintf("bits=0x%08X", uint32(t.value))
		default:
			// 空串要显式标出来 —— 这正是我们关心的
			if t.text == "" {
				extra = fmt.Sprintf("%s@%d  \"\"  ←【空串】", poolOf(t.value), t.value>>1)
			} else {
				extra = fmt.Sprintf("%s@%d  %q", poolOf(t.value), t.value>>1, t.text)
			}
		}
		fmt.Printf("  #%-4d  type=%-2d %-8s raw=0x%08X  %s\n", t.i, t.kind, kindName(t.kind), uint32(t.value), extra)
	}

	if *around != "" {
		hit := false
		for i, t := range toks {
			if strings.Contains(t.text, *around) {
				hit = true
				lo, hi := i-*ctx, i+*ctx
				if lo < 0 {
					lo = 0
				}
				if hi >= len(toks) {
					hi = len(toks) - 1
				}
				fmt.Printf("=== 命中 %q（token #%d），显示 #%d..#%d ===\n", *around, i, lo, hi)
				for j := lo; j <= hi; j++ {
					show(j)
				}
				fmt.Println()
			}
		}
		if !hit {
			fmt.Printf("没有 token 的解码文本包含 %q\n", *around)
		}
		return
	}

	for i := range toks {
		show(i)
	}
}
