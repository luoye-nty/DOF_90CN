package main

// DNF90 运维工具 —— 落盘日志 + 崩溃转储（免费版/赞助版共用，无 build tag）。
//
// 为什么必须有这个文件：
//
//	改造前 logf 只往内存环形缓冲追加，再刷到 GUI 的 EDIT 控件；进程一退（正常退出、
//	panic、被任务管理器结束）日志就没了，偏偏这个工具又是 windowsgui 构建、没有 console，
//	闪退时连一行遗言都留不下。实测"用着用着整窗口消失、什么提示都没有"就是这个形态。
//	所以这里做三件事：
//	  ① 每一行日志同时落盘（不带缓冲，Write 直接进内核，进程被强杀也留得住）；
//	  ② 写盘前脱敏（adminToken / 数据库口令 / 配对码 / internal token / ticket 等）；
//	  ③ panic 与未捕获异常把堆栈单独写 crash.log，并把当次日志尾部一并附上。
//
// 脱敏只作用于落盘文件，GUI 日志框始终显示原文——本地看日志的人本来就有这些信息，
// 但落盘文件会被复制、发群、发给别人排查，那才是需要保密的对象。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/config"
)

// fileLogger 落盘日志器：单文件超限或跨天自动轮转，并只保留最近 N 个文件。
type fileLogger struct {
	mu       sync.Mutex
	dir      string
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	keep     int
	day      string
	redact   bool
	// seq 进程内轮转序号，保证同一毫秒内连续轮转也能拿到不同的文件名。
	seq uint64
	// failed 落盘连续失败后置位，避免每个日志行都去撞一次同样的错误。
	failed bool
}

// newFileLogger 按配置创建落盘日志器；Dir 为空时用 baseDir/logs。
// 创建失败返回 nil（调用方按"没有文件日志"降级，不影响工具运行）。
func newFileLogger(cfg *config.LogFileConfig, baseDir string) *fileLogger {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "logs"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(baseDir, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	maxMB := cfg.MaxFileMB
	if maxMB <= 0 {
		maxMB = 16
	}
	keep := cfg.KeepFiles
	if keep <= 0 {
		keep = 20
	}
	logger := &fileLogger{
		dir:      dir,
		maxBytes: int64(maxMB) << 20,
		keep:     keep,
		redact:   cfg.Redact,
	}
	if err := logger.rotateLocked(); err != nil {
		return nil
	}
	return logger
}

// Path 当前日志文件绝对路径。
func (l *fileLogger) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

// Dir 日志目录。
func (l *fileLogger) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Size 当前日志文件字节数。
func (l *fileLogger) Size() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Write 追加一行日志（自动加时间戳、按需轮转、按需脱敏）。
func (l *fileLogger) Write(level, message string) {
	if l == nil {
		return
	}
	line := message
	if l.redact {
		line = redactSecrets(line)
	}
	stamp := time.Now().Format("2006-01-02 15:04:05.000")
	payload := fmt.Sprintf("%s [%s] %s\n", stamp, level, line)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed {
		return
	}
	today := time.Now().Format("20060102")
	if l.file == nil || l.day != today || (l.size+int64(len(payload)) > l.maxBytes) {
		if err := l.rotateLocked(); err != nil {
			l.failed = true
			return
		}
	}
	written, err := l.file.WriteString(payload)
	if err != nil {
		l.failed = true
		return
	}
	l.size += int64(written)
	// 不做 Sync：Windows 下 Write 已进入系统缓存，进程被强杀不会丢；
	// 真断电才可能需要 fsync，而这个场景下性能比掉最后几行更值钱。
}

// rotateLocked 关闭当前文件并开新文件（调用方已持锁或处于构造期）。
func (l *fileLogger) rotateLocked() error {
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return err
	}
	now := time.Now()
	l.day = now.Format("20060102")
	// 文件名 = 毫秒时间戳 + 进程内自增序号。
	// 只带时间戳不够：日志洪峰下同一毫秒内可能连续轮转多次，O_APPEND 会把"新文件"
	// 重新打开成同一个文件，结果是文件大小永远突破上限、轮转形同虚设（实测踩过）。
	l.seq++
	name := fmt.Sprintf("ops-%s-%05d.log", now.Format("20060102-150405.000"), l.seq)
	path := filepath.Join(l.dir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	size := int64(0)
	if statErr == nil {
		size = info.Size()
	}
	l.file = file
	l.path = path
	l.size = size
	l.pruneLocked()
	return nil
}

// pruneLocked 只保留最近 keep 个 ops-*.log。
// 按修改时间排序而不是按文件名：轮转文件名里带序号，字典序与时间序并不总是一致。
func (l *fileLogger) pruneLocked() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	type logFileEntry struct {
		name    string
		modTime time.Time
	}
	files := make([]logFileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "ops-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		modTime := time.Time{}
		if info, err := entry.Info(); err == nil {
			modTime = info.ModTime()
		}
		files = append(files, logFileEntry{name: name, modTime: modTime})
	}
	if len(files) <= l.keep {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].name < files[j].name
		}
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, file := range files[:len(files)-l.keep] {
		// 绝不删当前正在写的文件（时间戳可能与其他文件相同导致排序边界不准）。
		if file.name == filepath.Base(l.path) {
			continue
		}
		_ = os.Remove(filepath.Join(l.dir, file.name))
	}
}

// Close 关闭文件句柄（退出前调用）。
func (l *fileLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// ================= 脱敏 =================

// 脱敏只在这些关键字出现时才跑正则，避免日志洪峰下每行都做十几次正则匹配。
//
// 比对前必须 ToLower：值正则 reKeyValue 是大小写不敏感的（(?i)），而闸门早期是大小写敏感的，
// 于是 `Password=`、`TICKET=`、`SECRET=`、`rescue_code=` 这类拼写差异会让闸门不命中、
// 正则根本不跑，凭据原样落盘——安全控制被拼写绕过。闸门只能是"性能优化"，
// 不能成为正确性的一部分。
var redactTriggers = []string{
	"adm_", "password", "passwd", "pwd", "token",
	"ticket", "pairing", "secret", "@tcp(", "cdk", "rescue",
}

var (
	// adminToken：instance.json / dnfbridge.toml 里的 adm_xxx。
	reAdminToken = regexp.MustCompile(`adm_[0-9a-fA-F]{8,}`)
	// 长的 hex 串（internal token / 配对码 salt / sha256 摘要）。
	// 前缀 (dnf:)? 是为了**放行账号 ID**：account_id 形如 dnf:<sha256(username)>，
	// 它是不可逆的哈希、不是凭据，而且是排查时唯一能把日志与账号对上的键，
	// 抹掉它等于把日志的可用性换掉。没有这个前缀时，`\b` 同样会匹配到冒号后的 hex，
	// 所以必须显式把 dnf: 一起纳入匹配再原样返回。
	reLongHex = regexp.MustCompile(`(dnf:)?\b[0-9a-fA-F]{32,}\b`)
	// key=value / "key": "value" 形态的口令、令牌、配对码。
	// 值字符集刻意排除 & 与 ;：否则 "?token=abc&limit=10" 这种一行多参会被整段吞掉，
	// 脱敏过度会让日志失去排查价值。代价是口令里若含 & 只会被抹掉 & 之前的部分。
	reKeyValue = regexp.MustCompile(`(?i)("?(?:password|passwd|pwd|token|admin_?token|internal_?token|pairing_?code(?:_plain|_hash)?|secret|ticket|rescue_?code|cdk)"?\s*[:=]\s*"?)([^\s",;&]+)`)
	// MySQL DSN：user:password@tcp(host:port)/db。
	reMySQLDSN = regexp.MustCompile(`([A-Za-z0-9_]+):([^@\s/]{1,128})@tcp\(`)
	// URL query 里的 token/code 参数。
	reURLQuery = regexp.MustCompile(`(?i)([?&](?:token|code|ticket|key)=)([^&\s]+)`)
)

const redactMask = "***"

// redactSecrets 把一行日志里的凭据类内容替换成 ***。
// 宁可多抹一点也不要漏：这些日志的用途是排查崩溃与流程，不是审计具体密钥值。
func redactSecrets(line string) string {
	if line == "" {
		return line
	}
	lower := strings.ToLower(line)
	hit := false
	for _, trigger := range redactTriggers {
		if strings.Contains(lower, trigger) {
			hit = true
			break
		}
	}
	if !hit {
		return line
	}
	out := reKeyValue.ReplaceAllString(line, "${1}"+redactMask)
	out = reMySQLDSN.ReplaceAllString(out, "${1}:"+redactMask+"@tcp(")
	out = reURLQuery.ReplaceAllString(out, "${1}"+redactMask)
	out = reAdminToken.ReplaceAllString(out, "adm_"+redactMask)
	// 账号 ID（dnf:<sha256>）原样保留：它不是凭据，而且是排查时唯一能把日志与账号对上的键。
	out = reLongHex.ReplaceAllStringFunc(out, func(match string) string {
		if strings.HasPrefix(match, "dnf:") {
			return match
		}
		return redactMask
	})
	return out
}

// ================= 崩溃转储 =================

// crashLogName 崩溃日志文件名（与普通日志同目录，方便一起打包）。
const crashLogName = "crash.log"

// crashDir 崩溃日志目录（与普通日志一致）。
func (a *App) crashDir() string {
	if a != nil && a.fileLog != nil {
		return a.fileLog.Dir()
	}
	if a != nil && a.cfg != nil {
		return filepath.Join(filepath.Dir(a.cfg.Path()), "logs")
	}
	return "logs"
}

// writeCrashLog 把一次 panic 的现场写到 crash.log。
//
// 内容刻意写全：时间、panic 值、完整堆栈、当时 goroutine 数、内存概况，
// 以及主日志缓冲的最后若干行。panic 之后进程就没了，这些是唯一能带走的信息。
func (a *App) writeCrashLog(where string, recovered any, stack []byte) {
	dir := a.crashDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, crashLogName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()

	var builder strings.Builder
	fmt.Fprintf(&builder, "\n================ CRASH %s ================\n", time.Now().Format("2006-01-02 15:04:05.000"))
	fmt.Fprintf(&builder, "位置: %s\n", where)
	fmt.Fprintf(&builder, "panic: %v\n", recovered)
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	fmt.Fprintf(&builder, "Go 运行时: goroutines=%d heapAlloc=%dMB heapSys=%dMB numGC=%d\n",
		runtime.NumGoroutine(), mem.HeapAlloc>>20, mem.HeapSys>>20, mem.NumGC)
	fmt.Fprintf(&builder, "堆栈:\n%s\n", string(stack))
	// 附上主日志尾部：崩溃前最后几行往往就是根因线索。
	if tail := a.logTail(80); tail != "" {
		fmt.Fprintf(&builder, "--- 崩溃前主日志尾部（已脱敏）---\n%s\n", redactSecrets(tail))
	}
	fmt.Fprintf(&builder, "--- 建议：把本文件与同目录 ops-*.log 一起保留 ---\n")
	_, _ = file.WriteString(builder.String())
}

// debugStack 当前 goroutine 的调用栈文本（panic 现场与常规诊断都用它）。
func debugStack() []byte { return debug.Stack() }

// logTail 返回主日志缓冲最后 n 行（不含时间戳前缀，调用方自行加）。
func (a *App) logTail(n int) string {
	if a == nil {
		return ""
	}
	a.logMu.Lock()
	defer a.logMu.Unlock()
	lines := a.logBuf
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(append([]string(nil), lines...), "\n")
}

// installCrashGuard 给一个 goroutine 体套上 panic 兜底：
// 把现场写进 crash.log 并返回一个 error，绝不把 panic 抛给 runtime。
//
// 返回的 error 为 nil 表示正常结束；调用方按需处理（多数调用点只需记日志）。
func (a *App) guardGoroutine(name string, body func()) error {
	var recovered any
	var stack []byte
	func() {
		defer func() {
			if r := recover(); r != nil {
				recovered = r
				stack = debug.Stack()
			}
		}()
		body()
	}()
	if recovered == nil {
		return nil
	}
	a.writeCrashLog(name, recovered, stack)
	err := fmt.Errorf("%s 内部异常: %v（已写入 crash.log）", name, recovered)
	a.logf("[严重] %v", err)
	return err
}
