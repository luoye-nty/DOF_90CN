// pvftokedit 直接操作 type-1 成员（etc/raid/anton.etc 这类）的 5 字节令牌流。
//
// 令牌格式（与 internal/platform/pvf/decode.go 的 decodeScript 一一对应）：
//
//	每 5 字节 = [类型 byte][int32 LE]
//	类型 0 = 整数（值内联，不碰字符串池）
//	类型 2 = float
//	类型 3 = 独占一行的字符串（[STATE REWARD] 这种段名，值是字符串池偏移）
//	类型 6 = 反引号字符串（`squad_item` 这种，值是字符串池偏移）
//
// 关键点：**整数是内联的**，所以改权重/道具 ID / 增删整行都只动整数令牌，
// 唯一例外是「新起一个池名」需要往字符串池里加字符串（本工具不支持，用模板复制规避）。
//
// 用法：
//
//	pvftokedit -pvf A.pvf -file etc/raid/anton.etc -dump 60
//	pvftokedit -pvf A.pvf -file etc/raid/anton.etc -set-int 10157773 490002019 -out B.pvf
//	pvftokedit -pvf A.pvf -file etc/raid/anton.etc -del-row 10157775 -out B.pvf
//	pvftokedit -pvf A.pvf -file etc/raid/anton.etc -add-row 10157778 -rank -1 -weight 10 -def 490712331 -flag 1 -out B.pvf
package main

import (
	"flag"
	"fmt"
	"os"

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

// isRowStart 判断 index 是否是一条 `类型 排名 权重 定义ID 标志` 的整行起点（含段名与名字 2 个令牌）。
func isRowStart(tokens []tok, index int) bool {
	if index < 2 || index+4 >= len(tokens) {
		return false
	}
	return tokens[index-2].kind == 3 && tokens[index-1].kind == 6 &&
		tokens[index].kind == 0 && tokens[index+1].kind == 0 &&
		tokens[index+2].kind == 0 && tokens[index+3].kind == 0
}

func main() {
	pvfPath := flag.String("pvf", "", "输入 PVF")
	member := flag.String("file", "etc/raid/anton.etc", "成员路径")
	out := flag.String("out", "", "输出 PVF（不填则只读）")
	dump := flag.Int("dump", 0, "打印前 N 个令牌")
	dumpRows := flag.Bool("rows", false, "只列出 [STATE REWARD] 行的语义")
	setOld := flag.Int64("set-int", 0, "把值等于它的整数令牌改成 -set-new")
	setNew := flag.Int64("set-new", 0, "替换目标值")
	delRow := flag.Int64("del-row", 0, "删掉定义 ID 等于它的整行")
	addRow := flag.Int64("add-row", 0, "以该定义 ID 的行做模板，在其后插入一行")
	rank := flag.Int64("rank", -1, "add-row: 排名")
	weight := flag.Int64("weight", 100, "add-row: 权重")
	def := flag.Int64("def", 0, "add-row: 新的定义道具 ID")
	flagName := flag.Int64("flag", 0, "add-row: 标志")
	flag.Parse()

	if *pvfPath == "" {
		fmt.Fprintln(os.Stderr, "-pvf 必填")
		os.Exit(2)
	}
	archive, err := platformpvf.Open(*pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	raw, err := archive.ReadRaw(*member)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	if len(raw)%5 != 0 {
		fmt.Fprintf(os.Stderr, "%s 不是 5 字节对齐的令牌流（%d 字节），可能是 type-3 文本\n", *member, len(raw))
		os.Exit(1)
	}
	tokens := decode(raw)
	fmt.Printf("%s: %d 字节 -> %d 个令牌\n", *member, len(raw), len(tokens))

	if *dump > 0 {
		n := *dump
		if n > len(tokens) {
			n = len(tokens)
		}
		fmt.Println("  idx  kind  value")
		for i := 0; i < n; i++ {
			fmt.Printf("  %-5d %-5d %d\n", i, tokens[i].kind, tokens[i].i32)
		}
	}

	if *dumpRows {
		fmt.Println("\n奖励行（定义ID = 第 3 个整数令牌）:")
		count := 0
		for i := 2; i+3 < len(tokens); i++ {
			if !isRowStart(tokens, i) {
				continue
			}
			count++
			fmt.Printf("  token#%-5d 排名=%-4d 权重=%-8d 定义ID=%-10d 标志=%d\n",
				i-2, tokens[i].i32, tokens[i+1].i32, tokens[i+2].i32, tokens[i+3].i32)
		}
		fmt.Printf("  共 %d 行\n", count)
	}

	changed := false

	if *setOld != 0 {
		hits := 0
		for i := range tokens {
			if tokens[i].kind == 0 && int64(tokens[i].i32) == *setOld {
				tokens[i].i32 = int32(*setNew)
				hits++
			}
		}
		fmt.Printf("\nset-int %d -> %d: 命中 %d 个令牌\n", *setOld, *setNew, hits)
		changed = changed || hits > 0
	}

	if *delRow != 0 {
		hits := 0
		for i := 2; i+3 < len(tokens); i++ {
			if !isRowStart(tokens, i) || int64(tokens[i+2].i32) != *delRow {
				continue
			}
			tokens = append(tokens[:i-2], tokens[i+4:]...)
			hits++
			i -= 2
		}
		fmt.Printf("\ndel-row %d: 删除 %d 行\n", *delRow, hits)
		changed = changed || hits > 0
	}

	if *addRow != 0 {
		hits := 0
		for i := 2; i+3 < len(tokens); i++ {
			if !isRowStart(tokens, i) || int64(tokens[i+2].i32) != *addRow {
				continue
			}
			// 复制模板行的 [段名][名字] 两个令牌（字符串池偏移直接复用，不改字符串池）
			insert := []tok{
				tokens[i-2], tokens[i-1],
				{kind: 0, i32: int32(*rank)},
				{kind: 0, i32: int32(*weight)},
				{kind: 0, i32: int32(*def)},
				{kind: 0, i32: int32(*flagName)},
			}
			at := i + 4
			rest := append([]tok(nil), tokens[at:]...)
			tokens = append(tokens[:at], insert...)
			tokens = append(tokens, rest...)
			hits++
			i = at + len(insert)
		}
		fmt.Printf("\nadd-row (模板 %d): 插入 %d 行 排名=%d 权重=%d 定义ID=%d 标志=%d\n",
			*addRow, hits, *rank, *weight, *def, *flagName)
		changed = changed || hits > 0
	}

	if *out == "" {
		if !changed {
			fmt.Println("\n（只读模式，未写出文件）")
		} else {
			fmt.Println("\n（未指定 -out，改动未落盘）")
		}
		return
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "\n没有任何改动，拒绝写出（避免产出无意义的副本）")
		os.Exit(1)
	}
	patched, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: *member, Data: encode(tokens)}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "repack:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, patched, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("\n已写出: %s (%d 字节)\n", *out, len(patched))
}
