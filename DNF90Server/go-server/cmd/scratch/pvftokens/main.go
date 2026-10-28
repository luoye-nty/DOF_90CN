// Scratch: 检查 Script.pvf 里文件的真实编码，并把已有的 type-1（令牌流）脚本
// 安装到一个新文件的槽位上。
//
// 背景：客户端只把 dataType==1 的脚本当成脚本来解析（5 字节一条的令牌流），
// 而第三方 PVF 工具「新增文件」时写的是 dataType==3 的 UTF-16 纯文本。
// 于是新增道具在客户端解析失败（邮件附件空白、背包看不见），改现有道具却没问题。
//
// 用法:
//
//	go run ./cmd/scratch/pvftokens -pvf <Script.pvf> -mode dump  -path <文件> [-n 128]
//	go run ./cmd/scratch/pvftokens -pvf <Script.pvf> -mode grep  -text <关键字> [-prefix stackable/] [-limit 10]
//	go run ./cmd/scratch/pvftokens -pvf <Script.pvf> -mode install -src <已有可用文件> -dst <新文件> -out <输出 pvf>
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

func normalize(path string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(path, "/"), "data/dnf/"))
}

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf 路径")
	mode := flag.String("mode", "dump", "dump | grep | install")
	path := flag.String("path", "", "dump: 要看的文件路径")
	n := flag.Int("n", 128, "dump: 打印多少原始字节")
	text := flag.String("text", "", "grep: 关键字")
	prefix := flag.String("prefix", "stackable/", "grep: 扫描前缀")
	suffix := flag.String("suffix", ".stk", "grep: 只扫描该后缀")
	limit := flag.Int("limit", 10, "grep: 结果上限")
	src := flag.String("src", "", "install: 已有可用文件（取它的令牌流）")
	dst := flag.String("dst", "", "install: 目标文件（新道具）")
	out := flag.String("out", "", "install: 输出 PVF")
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
	fmt.Printf("archive: %s files=%d format=%s\n", *pvfPath, archive.FileCount(), archive.Format())

	switch *mode {
	case "dump":
		runDump(archive, *path, *n)
	case "grep":
		runGrep(archive, *prefix, *suffix, *text, *limit)
	case "install":
		runInstall(archive, *pvfPath, *src, *dst, *out)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

func findEntry(archive *platformpvf.Archive, want string) (platformpvf.File, bool) {
	want = normalize(want)
	for _, f := range archive.Files() {
		if normalize(f.Path) == want || normalize(f.ArchivePath) == want {
			return f, true
		}
	}
	return platformpvf.File{}, false
}

func runDump(archive *platformpvf.Archive, want string, n int) {
	entry, ok := findEntry(archive, want)
	if !ok {
		fmt.Printf("NOT FOUND: %s\n", want)
		return
	}
	fmt.Printf("dir=%q name=%q archive=%q idx=%d type=%d size=%d\n",
		entry.Path, entry.Name, entry.ArchivePath, entry.Index, entry.DataType, entry.Size)
	raw, err := archive.ReadRaw(entry.ArchivePath)
	if err != nil {
		fmt.Println("ReadRaw:", err)
		return
	}
	if n > len(raw) {
		n = len(raw)
	}
	// 打印原始字节，并用 ASCII 预览判断是「令牌流」还是「UTF-16 文本」。
	// UTF-16LE 文本每隔一字节就是 0x00，令牌流则是 5 字节一组、首字节是令牌类型。
	fmt.Printf("raw len=%d first %d bytes:\n", len(raw), n)
	for off := 0; off < n; off += 16 {
		end := off + 16
		if end > n {
			end = n
		}
		var hexPart, asciiPart strings.Builder
		for i := off; i < end; i++ {
			fmt.Fprintf(&hexPart, "%02X ", raw[i])
			if raw[i] >= 32 && raw[i] < 127 {
				asciiPart.WriteByte(raw[i])
			} else {
				asciiPart.WriteByte('.')
			}
		}
		fmt.Printf("  %06X  %-48s %s\n", off, hexPart.String(), asciiPart.String())
	}
	if decoded, err := archive.ReadText(entry.ArchivePath); err == nil {
		preview := decoded
		if len(preview) > 400 {
			preview = preview[:400]
		}
		fmt.Printf("decoded(len=%d) preview:\n%s\n", len(decoded), preview)
	}
}

func runGrep(archive *platformpvf.Archive, prefix, suffix, needle string, limit int) {
	prefix = strings.ToLower(prefix)
	suffix = strings.ToLower(suffix)
	scanned, matches := 0, 0
	for _, f := range archive.Files() {
		// 注意：File.Path 是文件表里的「目录」字段，完整路径在 ArchivePath（目录/文件名）。
		p := normalize(f.ArchivePath)
		if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, suffix) {
			continue
		}
		scanned++
		body, err := archive.ReadText(f.ArchivePath)
		if err != nil || !strings.Contains(body, needle) {
			continue
		}
		matches++
		preview := strings.Join(strings.Fields(strings.ReplaceAll(body, "`", "")), " ")
		if len(preview) > 220 {
			preview = preview[:220]
		}
		fmt.Printf("idx=%d type=%d size=%d path=%q\n    %s\n", f.Index, f.DataType, f.Size, f.ArchivePath, preview)
		if limit > 0 && matches >= limit {
			break
		}
	}
	fmt.Printf("scanned=%d matches=%d (limit=%d)\n", scanned, matches, limit)
}

func runInstall(archive *platformpvf.Archive, pvfPath, src, dst, out string) {
	if src == "" || dst == "" || out == "" {
		fmt.Fprintln(os.Stderr, "install needs -src -dst -out")
		os.Exit(2)
	}
	srcEntry, ok := findEntry(archive, src)
	if !ok {
		fmt.Printf("src NOT FOUND: %s\n", src)
		os.Exit(1)
	}
	dstEntry, ok := findEntry(archive, dst)
	if !ok {
		fmt.Printf("dst NOT FOUND: %s\n", dst)
		os.Exit(1)
	}
	raw, err := archive.ReadRaw(srcEntry.ArchivePath)
	if err != nil {
		fmt.Println("ReadRaw src:", err)
		os.Exit(1)
	}
	fmt.Printf("src idx=%d type=%d size=%d path=%q\n", srcEntry.Index, srcEntry.DataType, srcEntry.Size, srcEntry.ArchivePath)
	fmt.Printf("dst idx=%d type=%d size=%d path=%q\n", dstEntry.Index, dstEntry.DataType, dstEntry.Size, dstEntry.ArchivePath)
	if srcEntry.DataType != 1 {
		fmt.Printf("warning: src type=%d, 期待 1\n", srcEntry.DataType)
	}

	repacked, err := archive.RepackRaw([]platformpvf.RawReplacement{{Path: dstEntry.ArchivePath, Data: raw}})
	if err != nil {
		fmt.Println("RepackRaw:", err)
		os.Exit(1)
	}
	// RepackRaw 只替换内容、保留原 dataType（第 3 方工具写成的新值），
	// 所以还要把文件表里的 dataType 改成 1，客户端才会当脚本解析。
	reopened, err := platformpvf.OpenBytes(repacked)
	if err != nil {
		fmt.Println("reopen:", err)
		os.Exit(1)
	}
	final, err := reopened.PatchFileDataType(dstEntry.ArchivePath, 1)
	if err != nil {
		fmt.Println("PatchFileDataType:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, final, 0o644); err != nil {
		fmt.Println("write:", err)
		os.Exit(1)
	}
	fmt.Printf("written: %s (%d bytes) from %s\n", out, len(final), pvfPath)

	verify, err := platformpvf.OpenBytes(final)
	if err != nil {
		fmt.Println("verify open:", err)
		os.Exit(1)
	}
	if entry, ok := findEntry(verify, dst); ok {
		fmt.Printf("verify: idx=%d type=%d size=%d\n", entry.Index, entry.DataType, entry.Size)
	}
	if body, err := verify.ReadText(dstEntry.ArchivePath); err == nil {
		preview := body
		if len(preview) > 200 {
			preview = preview[:200]
		}
		fmt.Printf("verify decoded preview: %s\n", strings.Join(strings.Fields(preview), " "))
	} else {
		fmt.Println("verify ReadText:", err)
	}
}
