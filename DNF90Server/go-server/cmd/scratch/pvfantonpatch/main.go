// pvfantonpatch 一次性把「安徒恩团本奖励改造」写进 Script.pvf，并自动回读校验。
//
// 设计要点：
//   - 只做**整数令牌**手术（修改/插入/删除 type-0 令牌），字符串池一个字节都不碰。
//   - 定位 [int data] 段用 internal/modules/dnf/pvf 的 Parse，它给出的 token 下标
//     与 5 字节原始令牌一一对应；工具会硬断言 len(Tokens) == len(raw)/5，
//     断言失败就直接拒绝执行（防止下标错位把文件写坏）。
//   - 全部改动在一次 RepackRaw 里完成，最后回读新归档、比对解码文本。
//
// 用法：pvfantonpatch -pvf <输入> -out <输出>
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type tok struct {
	kind byte
	i32  int32
}

func decode(raw []byte) []tok {
	out := make([]tok, 0, len(raw)/5)
	for i := 0; i+5 <= len(raw); i += 5 {
		v := int32(uint32(raw[i+1]) | uint32(raw[i+2])<<8 | uint32(raw[i+3])<<16 | uint32(raw[i+4])<<24)
		out = append(out, tok{kind: raw[i], i32: v})
	}
	return out
}

func encode(tokens []tok) []byte {
	out := make([]byte, 0, len(tokens)*5)
	for _, t := range tokens {
		v := uint32(t.i32)
		out = append(out, t.kind, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	return out
}

// rowIndices 返回所有「排名 权重 定义ID 标志」整行的**排名令牌**下标，顺序即出现顺序。
func rowIndices(tokens []tok) []int {
	var out []int
	for i := 2; i+3 < len(tokens); i++ {
		if tokens[i].kind != 0 || tokens[i+1].kind != 0 || tokens[i+2].kind != 0 || tokens[i+3].kind != 0 {
			continue
		}
		if tokens[i-2].kind != 3 || tokens[i-1].kind != 6 {
			continue
		}
		out = append(out, i)
	}
	return out
}

// patchIntData 把某个 .stk 的 [int data] 内容整体换成 newValues。
func patchIntData(path string, raw []byte, text string, newValues []int32) ([]byte, error) {
	doc, err := dnfpvf.Parse(path, text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	tokens := decode(raw)
	if len(doc.Tokens) != len(tokens) {
		return nil, fmt.Errorf("%s: token 下标对不上（Parse=%d, raw=%d），拒绝写入", path, len(doc.Tokens), len(tokens))
	}
	// parse.go 的 addToken/closeSection：Section.Start = 段标记之后第一个内容令牌的下标，
	// Section.End = 下一个段标记令牌的下标。所以 [int data] 的内容区间就是 [Start, End)。
	start, contentEnd := -1, -1
	for _, s := range doc.Sections {
		if strings.ToLower(strings.TrimSpace(s.Name)) == "int data" {
			start, contentEnd = s.Start, s.End
		}
	}
	if start < 0 || contentEnd <= start {
		return nil, fmt.Errorf("%s: 找不到 [int data] 段（start=%d end=%d）", path, start, contentEnd)
	}
	for i := start; i < contentEnd; i++ {
		if tokens[i].kind != 0 {
			return nil, fmt.Errorf("%s: [int data] 内第 %d 个令牌不是整数（kind=%d）", path, i-start, tokens[i].kind)
		}
	}
	replacement := make([]tok, 0, len(newValues))
	for _, v := range newValues {
		replacement = append(replacement, tok{kind: 0, i32: v})
	}
	rest := append([]tok(nil), tokens[contentEnd:]...)
	tokens = append(tokens[:start], replacement...)
	tokens = append(tokens, rest...)
	fmt.Printf("  %-46s [int data] %d 个令牌 -> %d 个\n", path, contentEnd-start, len(newValues))
	return encode(tokens), nil
}

func main() {
	pvfPath := flag.String("pvf", "", "输入 PVF")
	outPath := flag.String("out", "", "输出 PVF")
	flag.Parse()
	if *pvfPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf 与 -out 必填")
		os.Exit(2)
	}

	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}

	repl := map[string][]byte{}
	expect := map[string]string{}

	// ---- A. 6 个奖励定义的 [int data] 改造 ----
	fmt.Println("A. 奖励定义内容改造：")
	intDataTargets := []struct {
		path   string
		values []int32
		note   string
	}{
		{"stackable/10157001/10157773.stk", []int32{490002019, 100, 1}, "深渊票 -> 3000礼盒（阶段1）"},
		{"stackable/10157001/10157775.stk", []int32{490002019, 100, 1}, "传说装备 -> 3000礼盒（阶段1，连带单人）"},
		{"stackable/10157001/10157776.stk", []int32{490002019, 100, 1}, "传说卡片 -> 3000礼盒（阶段1，连带单人）"},
		{"stackable/10157001/10157777.stk", []int32{490002019, 100, 1}, "史诗装备 -> 3000礼盒（阶段1）"},
		{"stackable/10157001/10157778.stk", []int32{490005021, 100, 1}, "深渊票 -> 8888礼盒（阶段2）"},
		{"stackable/10157001/10157786.stk", []int32{490712331, 100, 1}, "心脏卡片 -> 花篮（阶段2，连带单人）"},
	}
	for _, target := range intDataTargets {
		raw, err := archive.ReadRaw(target.path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", target.path, err)
			os.Exit(1)
		}
		text, err := archive.ReadText(target.path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "text:", target.path, err)
			os.Exit(1)
		}
		patched, err := patchIntData(target.path, raw, text, target.values)
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
		repl[target.path] = patched
		expect[target.path] = target.note
	}

	// ---- B. etc/raid/anton.etc 权重与定义 ID ----
	fmt.Println("\nB. etc/raid/anton.etc 奖励行：")
	const antonPath = "etc/raid/anton.etc"
	raw, err := archive.ReadRaw(antonPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read anton:", err)
		os.Exit(1)
	}
	tokens := decode(raw)

	// 行序（桌面 PVF 现状，21 行）：
	//  0 10094735  1 10094733  2 10157775  3 10157776  4 10157777
	//  5 3330      6 10094733  7 10161936  8 10157783  9 10157784
	// 10 10157785 11 10157786 12..20 单人段
	rows := rowIndices(tokens)
	if len(rows) != 21 {
		fmt.Fprintf(os.Stderr, "预期 21 行奖励，实际 %d 行，拒绝写入\n", len(rows))
		os.Exit(1)
	}
	wantDefs := []int32{10094735, 10094733, 10157775, 10157776, 10157777, 3330, 10094733,
		10161936, 10157783, 10157784, 10157785, 10157786,
		10094735, 10164076, 10164077, 10157783, 10157784, 10157785, 10157775, 10157776, 10157786}
	for i, at := range rows {
		if tokens[at+2].i32 != wantDefs[i] {
			fmt.Fprintf(os.Stderr, "第 %d 行定义 ID 预期 %d 实际 %d，拒绝写入（防止按行号改错）\n",
				i, wantDefs[i], tokens[at+2].i32)
			os.Exit(1)
		}
	}

	// 1) 先补回被删掉的 `squad_item` -1 50 10157773 0（用第 2 行做模板复制 [段名][名字] 令牌）
	tpl := rows[2]
	insert := []tok{tokens[tpl-2], tokens[tpl-1],
		{kind: 0, i32: -1}, {kind: 0, i32: 50}, {kind: 0, i32: 10157773}, {kind: 0, i32: 0}}
	at := tpl + 4
	restTokens := append([]tok(nil), tokens[at:]...)
	tokens = append(tokens[:at], insert...)
	tokens = append(tokens, restTokens...)
	fmt.Println("  + 补回阶段1 squad_item 10157773 权重 50")
	rows = rowIndices(tokens)
	if len(rows) != 22 {
		fmt.Fprintf(os.Stderr, "插入后应为 22 行，实际 %d 行，拒绝写入\n", len(rows))
		os.Exit(1)
	}

	// 2) 按行号改权重 / 定义 ID（插入后行序：
	//  0 10094735  1 10094733  2 10157775  3 10157773  4 10157776  5 10157777
	//  6 3330      7 10094733  8 10161936  9 10157783 10 10157784 11 10157785 12 10157786
	// 13..21 单人段）
	type rowEdit struct {
		row    int
		def    int32 // <0 表示不改
		weight int32
		label  string
	}
	edits := []rowEdit{
		{row: 2, def: -1, weight: 1, label: "阶段1 10157775 权重 0 -> 1"},
		{row: 3, def: -1, weight: 50, label: "阶段1 10157773 权重 50（新补）"},
		{row: 4, def: -1, weight: 1, label: "阶段1 10157776 权重 0 -> 1"},
		{row: 5, def: -1, weight: 48, label: "阶段1 10157777 权重 5000 -> 48"},
		{row: 6, def: 10157778, weight: 100, label: "阶段2 party_card 定义 3330 -> 10157778，权重 10 -> 100"},
		{row: 8, def: -1, weight: 701, label: "阶段2 10161936 权重 40 -> 701"},
		{row: 9, def: -1, weight: 131, label: "阶段2 10157783 权重 10 -> 131"},
		{row: 10, def: -1, weight: 45, label: "阶段2 10157784 权重 20 -> 45"},
		{row: 11, def: -1, weight: 23, label: "阶段2 10157785 权重 20 -> 23"},
		{row: 12, def: -1, weight: 100, label: "阶段2 10157786 权重 20 -> 100（花篮 10%）"},
	}
	for _, edit := range edits {
		at := rows[edit.row]
		if edit.def >= 0 {
			tokens[at+2].i32 = edit.def
		}
		tokens[at+1].i32 = edit.weight
		fmt.Printf("  ~ %s\n", edit.label)
	}
	// 校验阶段2 squad 权重和 = 1000
	sum := tokens[rows[8]+1].i32 + tokens[rows[9]+1].i32 + tokens[rows[10]+1].i32 +
		tokens[rows[11]+1].i32 + tokens[rows[12]+1].i32
	if sum != 1000 {
		fmt.Fprintf(os.Stderr, "阶段2 squad_item 权重和 = %d，应为 1000，拒绝写入\n", sum)
		os.Exit(1)
	}
	fmt.Printf("  阶段2 squad_item 权重和 = %d（花篮 %d/1000 = %.2f%%）\n",
		sum, tokens[rows[12]+1].i32, float64(tokens[rows[12]+1].i32)/float64(sum)*100)
	repl[antonPath] = encode(tokens)

	// ---- C. 一次重打包 ----
	fmt.Printf("\nC. 重打包 %d 个文件 -> %s\n", len(repl), *outPath)
	replacements := make([]platformpvf.RawReplacement, 0, len(repl))
	for path, data := range repl {
		replacements = append(replacements, platformpvf.RawReplacement{Path: path, Data: data})
	}
	packed, err := archive.RepackRaw(replacements)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repack:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*outPath, packed, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("   写出 %d 字节\n", len(packed))

	// ---- D. 回读校验 ----
	fmt.Println("\nD. 回读校验（新归档 vs 原归档的解码文本）:")
	verify, err := platformpvf.Open(*outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reopen:", err)
		os.Exit(1)
	}
	fail := false
	for path := range repl {
		before, _ := archive.ReadText(path)
		after, _ := verify.ReadText(path)
		if before == after {
			fmt.Printf("  !! %s 文本没变化，改动可能没生效\n", path)
			fail = true
			continue
		}
		if path != antonPath {
			if !strings.Contains(after, "[int data]") {
				fmt.Printf("  !! %s 解码后缺少 [int data]\n", path)
				fail = true
				continue
			}
		}
		fmt.Printf("  OK %-46s %d -> %d 字符\n", path, len(before), len(after))
	}
	// 汇总新归档的奖励行
	rawAfter, err := verify.ReadRaw(antonPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read after:", err)
		os.Exit(1)
	}
	after := decode(rawAfter)
	rowsAfter := rowIndices(after)
	fmt.Printf("\n改后奖励行（共 %d 行）:\n", len(rowsAfter))
	for i, at := range rowsAfter {
		fmt.Printf("  #%-3d 权重=%-6d 定义ID=%-10d 标志=%d\n", i, after[at+1].i32, after[at+2].i32, after[at+3].i32)
	}
	if fail {
		os.Exit(1)
	}
	fmt.Println("\n完成。")
}
