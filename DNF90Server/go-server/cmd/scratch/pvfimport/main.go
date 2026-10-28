// Scratch: 把「文本形式」的脚本编码成客户端能解析的 type-1 令牌流，并安装进 Script.pvf。
//
// 背景（证据见 NOTES-pvf-new-item-encoding.md）：
//   - 游戏自带的 .stk 都是 dataType==1，内容是 5 字节一条的令牌流：
//     第 1 字节是令牌类型，后面 4 字节是 int32（数字，或字符串池偏移）。
//   - 第三方 PVF 工具「新增文件」时写的是 dataType==3 的 UTF-16LE 纯文本（带 #PVF_File 头、
//     制表符缩进、CRLF）。服务端两种都读，客户端只认 type-1 令牌流，
//     于是新增道具在客户端解析失败（邮件附件空白 / 背包看不见），而改现有道具没问题。
//
// 用法:
//
//	go run ./cmd/scratch/pvfimport -pvf <Script.pvf> -mode roundtrip -prefix stackable/10000001/ -limit 50
//	go run ./cmd/scratch/pvfimport -pvf <Script.pvf> -mode encode -src <文本文件> -out <令牌流文件>
//	go run ./cmd/scratch/pvfimport -pvf <Script.pvf> -mode install -src <文本文件路径> -dst <同一路径> -out <新 pvf> [-dry-run]
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// 令牌类型，与 internal/platform/pvf/decode.go 的 decodeScript 一一对应。
const (
	tokenInt    = 0 // 整数
	tokenFloat  = 2 // 浮点数
	tokenLine   = 3 // 独占一行的字符串（[name]、[/usable job] 这类关键字）
	tokenSix    = 5 // {5=``}，解码端不透明的占位令牌
	tokenString = 6 // 反引号字符串
	tokenSeven  = 7 // {7=``}
)

type token struct {
	kind int
	i32  int32
	f32  float32
	str  string
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	mode := flag.String("mode", "roundtrip", "roundtrip | encode | install")
	prefix := flag.String("prefix", "stackable/10000001/", "roundtrip: 扫描前缀")
	limit := flag.Int("limit", 50, "roundtrip: 最多检查多少个文件")
	src := flag.String("src", "", "encode/install: 取文本的文件路径（PVF 内路径）")
	textFile := flag.String("text-file", "", "encode: 直接读本地文本文件")
	dst := flag.String("dst", "", "install: 目标文件路径（通常与 -src 相同）")
	out := flag.String("out", "", "输出路径（encode 的令牌流 / install 的新 pvf）")
	dryRun := flag.Bool("dry-run", false, "install: 只验证、不写文件")
	flag.Parse()

	switch *mode {
	case "roundtrip":
		runRoundTrip(*pvfPath, *prefix, *limit)
	case "encode":
		runEncode(*pvfPath, *src, *textFile, *out)
	case "install":
		runInstall(*pvfPath, *src, *dst, *out, *dryRun)
	case "audit":
		runAudit(*pvfPath, *prefix, *limit)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

// extStat 按扩展名汇总往返审计结果。
type extStat struct {
	files          int
	byType         map[int]int
	sampled        int
	byteExact      int
	rawDiff        int
	tokenSame      int
	tokenDiff      int
	skipMissing    int
	skipUnknownTok int
	skipOther      int
	unknownTypes   map[byte]int
	examples       []string
}

func pathExt(p string) string {
	slash := strings.LastIndexByte(p, '/')
	name := p
	if slash >= 0 {
		name = p[slash+1:]
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return "(no-ext)"
	}
	return strings.ToLower(name[dot:])
}

// scanTokenTypes 收集一段令牌流里出现的令牌类型，并指出是否存在我们未支持的类型。
// 未支持类型在解码文本里是「看不见」的，只有扫原始字节才能发现——
// 这正是「写回后内容变样」的高危信号。
func scanTokenTypes(raw []byte, counts map[byte]int) bool {
	unknown := false
	for off := 0; off+5 <= len(raw); off += 5 {
		tokenType := raw[off]
		counts[tokenType]++
		switch tokenType {
		case 0, 2, 3, 5, 6, 7:
		default:
			unknown = true
		}
	}
	return unknown
}

// runAudit 按扩展名抽样验证「解码→重新编码」能否逐字节还原原文件。
// 能逐字节还原的类型才可以安全地写回；不能还原的类型必须拒绝写，而不是猜着写。
func runAudit(pvfPath, prefix string, perExt int) {
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	prefix = strings.ToLower(strings.TrimPrefix(prefix, "data/dnf/"))
	// -prefix all / * 表示不做前缀过滤（审计整个库）。
	if prefix == "all" || prefix == "*" {
		prefix = ""
	}
	stats := make(map[string]*extStat)
	totalFiles, tokenFiles := 0, 0

	for _, f := range archive.Files() {
		p := strings.ToLower(strings.TrimPrefix(f.ArchivePath, "data/dnf/"))
		if prefix != "" && !strings.HasPrefix(p, prefix) {
			continue
		}
		totalFiles++
		ext := pathExt(p)
		st := stats[ext]
		if st == nil {
			st = &extStat{byType: make(map[int]int), unknownTypes: make(map[byte]int)}
			stats[ext] = st
		}
		st.files++
		st.byType[f.DataType]++
		// 只审计令牌流（dataType 1）；dataType 3 是纯文本，读写没有编码歧义。
		if f.DataType != 1 {
			continue
		}
		tokenFiles++
		if perExt > 0 && st.sampled >= perExt {
			continue
		}
		st.sampled++
		raw, err := archive.ReadRaw(f.ArchivePath)
		if err != nil {
			st.skipOther++
			continue
		}
		if scanTokenTypes(raw, st.unknownTypes) {
			st.skipUnknownTok++
		}
		text, err := archive.ReadText(f.ArchivePath)
		if err != nil {
			st.skipOther++
			continue
		}
		tokens, values, err := tokenizeText(text)
		if err != nil {
			st.skipOther++
			continue
		}
		resolve := archive.FindStringMagicOffsets(values)
		encoded, missing := encodeTokens(tokens, resolve)
		if len(missing) > 0 {
			st.skipMissing++
			continue
		}
		if string(encoded) == string(raw) {
			st.byteExact++
		} else {
			st.rawDiff++
		}
		reTokens, _, err := tokenizeText(archive.DecodeScriptBytes(encoded))
		if err == nil && tokensEqual(tokens, reTokens) {
			st.tokenSame++
		} else {
			st.tokenDiff++
			if len(st.examples) < 3 {
				st.examples = append(st.examples, f.ArchivePath)
			}
		}
	}

	type row struct {
		ext string
		st  *extStat
	}
	rows := make([]row, 0, len(stats))
	for ext, st := range stats {
		rows = append(rows, row{ext, st})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].st.files != rows[j].st.files {
			return rows[i].st.files > rows[j].st.files
		}
		return rows[i].ext < rows[j].ext
	})

	fmt.Printf("archive=%s files=%d (令牌流 dataType=1: %d) 每扩展名抽样=%d\n\n",
		pvfPath, totalFiles, tokenFiles, perExt)
	fmt.Printf("%-12s %9s %-22s %6s %9s %8s %8s %8s %s\n",
		"扩展名", "文件数", "dataType 分布", "抽样", "逐字节相同", "原文不同", "令牌相同", "跳过", "未知令牌类型")
	var unsafe []string
	for _, r := range rows {
		st := r.st
		if st.sampled == 0 {
			continue
		}
		dist := ""
		for _, t := range []int{1, 3} {
			if n := st.byType[t]; n > 0 {
				if dist != "" {
					dist += " "
				}
				dist += fmt.Sprintf("t%d:%d", t, n)
			}
		}
		fmt.Printf("%-12s %9d %-22s %6d %9d %8d %8d %8d %s\n",
			r.ext, st.files, dist, st.sampled, st.byteExact, st.rawDiff, st.tokenSame,
			st.skipMissing+st.skipUnknownTok+st.skipOther, formatTokenTypes(st.unknownTypes))
		if st.rawDiff > 0 || st.tokenDiff > 0 || st.skipUnknownTok > 0 {
			unsafe = append(unsafe, r.ext)
			for _, example := range st.examples {
				fmt.Printf("    例：%s\n", example)
			}
		}
	}
	fmt.Printf("\n无法逐字节复现的类型（写回时必须拒绝，不能猜）：%v\n", unsafe)
}

func formatTokenTypes(counts map[byte]int) string {
	if len(counts) == 0 {
		return "-"
	}
	keys := make([]int, 0, len(counts))
	for k := range counts {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d×%d", k, counts[byte(k)]))
	}
	return strings.Join(parts, ",")
}

// tokenizeText 把文本脚本切成令牌。
// 规则来自 decodeScript 的输出形式：关键字独占一行、字符串带反引号、
// 数字/浮点直接写出来。制表符、CRLF、多余空格都只是排版，全部忽略。
func tokenizeText(text string) ([]token, []string, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var tokens []token
	var literals []string
	addString := func(value string) {
		for _, existing := range literals {
			if existing == value {
				return
			}
		}
		literals = append(literals, value)
	}

	i := 0
	for i < len(text) {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '`':
			end := strings.IndexByte(text[i+1:], '`')
			if end < 0 {
				return nil, nil, fmt.Errorf("未闭合的反引号（偏移 %d）", i)
			}
			value := text[i+1 : i+1+end]
			tokens = append(tokens, token{kind: tokenString, str: value})
			addString(value)
			i += end + 2
		case c == '[':
			// 关键字可能和数字写在同一行（[cera] 1 100 1 [/cera]），所以只取到配对的 ']'。
			end := strings.IndexByte(text[i:], ']')
			if end < 0 {
				return nil, nil, fmt.Errorf("未闭合的方括号（偏移 %d）", i)
			}
			value := text[i : i+end+1]
			tokens = append(tokens, token{kind: tokenLine, str: value})
			addString(value)
			i += end + 1
		default:
			j := i
			for j < len(text) && text[j] != ' ' && text[j] != '\t' && text[j] != '\n' &&
				text[j] != '`' && text[j] != '[' {
				j++
			}
			word := text[i:j]
			i = j
			switch {
			case word == "#PVF_File":
				// 编辑器导出文本的头部，令牌流里没有这一项（真实 .stk 也不含）。
			case strings.HasPrefix(word, "//"):
				// 注释在令牌流里不存在，直接跳到行尾。
				if nl := strings.IndexByte(text[i:], '\n'); nl >= 0 {
					i += nl + 1
				} else {
					i = len(text)
				}
			case word == "{5=``}":
				tokens = append(tokens, token{kind: tokenSix})
			case word == "{7=``}":
				tokens = append(tokens, token{kind: tokenSeven})
			default:
				if value, err := strconv.ParseInt(word, 10, 32); err == nil {
					tokens = append(tokens, token{kind: tokenInt, i32: int32(value)})
					continue
				}
				if value, err := strconv.ParseFloat(word, 32); err == nil {
					tokens = append(tokens, token{kind: tokenFloat, f32: float32(value)})
					continue
				}
				// 少数脚本会把关键字写成不带方括号的裸词，按行字符串处理。
				tokens = append(tokens, token{kind: tokenLine, str: word})
				addString(word)
			}
		}
	}
	return tokens, literals, nil
}

// encodeTokens 按 5 字节一条写回令牌流，字符串用字符串池里的 magic offset 表示。
func encodeTokens(tokens []token, resolve map[string]int) ([]byte, []string) {
	out := make([]byte, 0, len(tokens)*5)
	var missing []string
	for _, t := range tokens {
		switch t.kind {
		case tokenInt:
			out = append(out, tokenInt, byte(t.i32), byte(t.i32>>8), byte(t.i32>>16), byte(t.i32>>24))
		case tokenFloat:
			bits := math.Float32bits(t.f32)
			out = append(out, tokenFloat, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
		case tokenLine, tokenString:
			offset, ok := resolve[t.str]
			if !ok {
				missing = append(missing, t.str)
				continue
			}
			out = append(out, byte(t.kind), byte(offset), byte(offset>>8), byte(offset>>16), byte(offset>>24))
		default:
			out = append(out, byte(t.kind), 0, 0, 0, 0)
		}
	}
	return out, missing
}

// tokensEqual 用于验证：重新解码后的文本必须切出完全相同的令牌序列。
func tokensEqual(left, right []token) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].kind != right[i].kind || left[i].i32 != right[i].i32 || left[i].str != right[i].str {
			return false
		}
	}
	return true
}

func runRoundTrip(pvfPath, prefix string, limit int) {
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	prefix = strings.ToLower(prefix)
	checked, exactBytes, sameTokens, skipped := 0, 0, 0, 0
	for _, f := range archive.Files() {
		path := strings.ToLower(strings.TrimPrefix(f.ArchivePath, "data/dnf/"))
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".stk") || f.DataType != 1 {
			continue
		}
		if checked >= limit {
			break
		}
		checked++
		text, err := archive.ReadText(f.ArchivePath)
		if err != nil {
			skipped++
			continue
		}
		tokens, values, err := tokenizeText(text)
		if err != nil {
			fmt.Printf("  tokenize fail %s: %v\n", f.ArchivePath, err)
			skipped++
			continue
		}
		resolve := archive.FindStringMagicOffsets(values)
		encoded, missing := encodeTokens(tokens, resolve)
		if len(missing) > 0 {
			skipped++
			continue
		}
		raw, err := archive.ReadRaw(f.ArchivePath)
		if err == nil && string(raw) == string(encoded) {
			exactBytes++
		}
		retext := archive.DecodeScriptBytes(encoded)
		reTokens, _, err := tokenizeText(retext)
		if err != nil {
			skipped++
			continue
		}
		if tokensEqual(tokens, reTokens) {
			sameTokens++
		} else {
			fmt.Printf("  TOKEN MISMATCH %s (orig=%d new=%d)\n", f.ArchivePath, len(tokens), len(reTokens))
		}
	}
	fmt.Printf("roundtrip: checked=%d byte-exact=%d token-identical=%d skipped=%d\n",
		checked, exactBytes, sameTokens, skipped)
}

func runEncode(pvfPath, src, textFile, out string) {
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	var text string
	if textFile != "" {
		raw, err := os.ReadFile(textFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		text = string(raw)
	} else {
		value, err := archive.ReadText(src)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ReadText:", err)
			os.Exit(1)
		}
		text = value
	}
	tokens, values, err := tokenizeText(text)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokenize:", err)
		os.Exit(1)
	}
	resolve := archive.FindStringMagicOffsets(values)
	encoded, missing := encodeTokens(tokens, resolve)
	fmt.Printf("tokens=%d strings=%d resolved=%d missing=%d bytes=%d\n",
		len(tokens), len(values), len(resolve), len(missing), len(encoded))
	for _, value := range missing {
		fmt.Printf("  missing string: %q\n", value)
	}
	if out != "" && len(missing) == 0 {
		if err := os.WriteFile(out, encoded, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Println("written:", out)
	}
}

func runInstall(pvfPath, src, dst, out string, dryRun bool) {
	if dst == "" {
		dst = src
	}
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	text, err := archive.ReadText(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ReadText:", err)
		os.Exit(1)
	}
	tokens, values, err := tokenizeText(text)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tokenize:", err)
		os.Exit(1)
	}
	fmt.Printf("source %s: text=%d bytes, tokens=%d, strings=%d\n", src, len(text), len(tokens), len(values))

	resolve := archive.FindStringMagicOffsets(values)
	var appends []platformpvf.StringAppend
	for _, value := range values {
		if _, ok := resolve[value]; ok {
			continue
		}
		// 非 ASCII（中文名/说明）走 UTF-16 池，纯 ASCII 走 ANSI 池。
		wide := false
		for _, r := range value {
			if r > 127 {
				wide = true
				break
			}
		}
		appends = append(appends, platformpvf.StringAppend{Value: value, Wide: wide})
	}
	work := archive
	if len(appends) > 0 {
		fmt.Printf("appending %d new strings to the pool\n", len(appends))
		for _, appendItem := range appends {
			fmt.Printf("  + %q wide=%v\n", appendItem.Value, appendItem.Wide)
		}
		bytesWithStrings, err := archive.RepackAppendStrings(appends)
		if err != nil {
			fmt.Fprintln(os.Stderr, "RepackAppendStrings:", err)
			os.Exit(1)
		}
		work, err = platformpvf.OpenBytes(bytesWithStrings)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reopen after append:", err)
			os.Exit(1)
		}
		resolve = work.FindStringMagicOffsets(values)
	}
	encoded, missing := encodeTokens(tokens, resolve)
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "无法解析的字符串 %d 个: %v\n", len(missing), missing)
		os.Exit(1)
	}
	fmt.Printf("encoded token stream: %d bytes\n", len(encoded))

	repacked, err := work.RepackRaw([]platformpvf.RawReplacement{{Path: dst, Data: encoded}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "RepackRaw:", err)
		os.Exit(1)
	}
	reopened, err := platformpvf.OpenBytes(repacked)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reopen:", err)
		os.Exit(1)
	}
	// RepackRaw 保留原 dataType（工具写下的 3），必须改成 1 客户端才当脚本解析。
	final, err := reopened.PatchFileDataType(dst, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "PatchFileDataType:", err)
		os.Exit(1)
	}

	verify, err := platformpvf.OpenBytes(final)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify open:", err)
		os.Exit(1)
	}
	entry, _ := verify.FindFile(dst)
	fmt.Printf("verify: idx=%d type=%d size=%d\n", entry.Index, entry.DataType, entry.Size)
	verifyText, err := verify.ReadText(dst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify ReadText:", err)
		os.Exit(1)
	}
	verifyTokens, _, err := tokenizeText(verifyText)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify tokenize:", err)
		os.Exit(1)
	}
	if !tokensEqual(tokens, verifyTokens) {
		fmt.Fprintf(os.Stderr, "验证失败：回读令牌 %d 个，期望 %d 个\n", len(verifyTokens), len(tokens))
		os.Exit(1)
	}
	fmt.Printf("verify: 回读令牌一致 (%d 个)\n", len(verifyTokens))
	preview := strings.Join(strings.Fields(strings.ReplaceAll(verifyText, "`", "")), " ")
	if len(preview) > 200 {
		preview = preview[:200]
	}
	fmt.Printf("verify preview: %s\n", preview)

	if dryRun {
		fmt.Println("dry-run: 未写文件")
		return
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if err := os.WriteFile(out, final, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("written: %s (%d bytes)\n", out, len(final))
}
