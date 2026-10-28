package logcrypt

import (
	"bufio"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 默认参数。MaxBytes/RetentionDays 可由部署环境覆盖。
const (
	// DefaultMaxBytes 是单个日志文件的上限（100 MB）。
	DefaultMaxBytes = 100 << 20
	// DefaultRetentionDays 是日志保留天数。
	DefaultRetentionDays = 7
	// DefaultQueueSize 是有界队列长度；队列满时丢行而不是阻塞游戏。
	DefaultQueueSize = 8192
	// DefaultBufferSize 是 bufio 缓冲。
	DefaultBufferSize = 256 * 1024
	// DefaultFlushInterval 是强制 flush 间隔（也是掉电最多丢多少日志）。
	DefaultFlushInterval = 100 * time.Millisecond
	// currentPointerName 指向"当前正在写的文件"，滚动时重写。
	// 解密工具默认读它，因此外部脚本不必关心轮转命名。
	currentPointerName = "current.txt"
)

// ErrClosed 表示写入器已关闭。
var ErrClosed = errors.New("logcrypt: writer is closed")

// Config 是写入器配置。
type Config struct {
	// Dir 是日志根目录；每一天在它下面建一个 <YYYYMMDD>/ 子目录。
	Dir string
	// Prefix 是文件名前缀，例如 "packet"。
	Prefix string
	// Ext 是文件扩展名，例如 ".log.enc"。
	Ext string
	// Recipient 是收件人公钥。**必填**：没有它就不写任何日志（绝不退化成明文）。
	Recipient *Recipient
	// MaxBytes 是单文件上限，<=0 时用 DefaultMaxBytes。
	MaxBytes int64
	// RetentionDays 是保留天数，<=0 时用 DefaultRetentionDays。
	RetentionDays int
	// QueueSize / BufferSize / FlushInterval <=0 时用默认值。
	QueueSize     int
	BufferSize    int
	FlushInterval time.Duration
	// DropMarkerKind 是队列溢出时写入的标记事件名。
	DropMarkerKind string
	// Now 可注入时钟（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// Random 可注入随机源（测试用）；nil 时用 crypto/rand。
	Random io.Reader
	// Logger 可选，用于报告轮转、清理与写入错误。
	Logger *slog.Logger
}

// Writer 是"落盘即密文 + 按天分目录 + 按大小轮转 + 过期清理"的异步日志写入器。
//
// 并发模型与既有的 packetLogger 保持一致：
//   - Write 只做入队，永不阻塞调用方（游戏线程）；队列满则丢弃并计数。
//   - 单写 goroutine 独占文件句柄，加密、缓冲、轮转、清理都在它里面完成，
//     因此轮转不需要任何锁。
//
// 每个文件一把随机 DEK；**重启后一定另起新分片**（DEK 不落盘，无法续写旧文件）。
type Writer struct {
	cfg    Config
	header []byte // 当前文件的头（含被公钥封装的 DEK），同时用作记录 AAD

	queue chan string
	stop  chan struct{}
	done  chan struct{}

	dropped   atomic.Uint64
	pruning   atomic.Bool
	writeFail atomic.Uint64

	mu       sync.RWMutex
	closed   bool
	closeErr error

	// 以下字段只由 run goroutine 访问。
	file          *os.File
	buf           *bufio.Writer
	aead          cipher.AEAD
	path          string
	day           string
	index         int
	bytesInFile   int64
	recordsInFile int
}

// Open 创建写入器并启动写 goroutine。
//
// 收件人公钥缺失时返回 ErrRecipientMissing——调用方据此关闭该日志通道，
// **绝不写明文**。
func Open(cfg Config) (*Writer, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("logcrypt: Dir is required")
	}
	if cfg.Prefix == "" {
		return nil, errors.New("logcrypt: Prefix is required")
	}
	if cfg.Recipient == nil || cfg.Recipient.pub == nil {
		return nil, ErrRecipientMissing
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = DefaultRetentionDays
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultBufferSize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = defaultRandom()
	}
	if cfg.DropMarkerKind == "" {
		cfg.DropMarkerKind = "log-backpressure-dropped"
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("logcrypt: create log dir: %w", err)
	}
	writer := &Writer{
		cfg:   cfg,
		queue: make(chan string, cfg.QueueSize),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go writer.run()
	return writer, nil
}

// Write 入队一行（行尾换行可选）。永不阻塞。
func (w *Writer) Write(line string) {
	if w == nil || line == "" {
		return
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return
	}
	select {
	case w.queue <- line:
	default:
		// 诊断日志绝不能拖住游戏：队列满就丢，由写 goroutine 补一条计数标记。
		w.dropped.Add(1)
	}
}

// Path 返回当前正在写的文件路径（滚动后变化；Close 之后仍返回最后一个）。
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.path
}

// KeyID 当前文件使用的收件人公钥标识。
func (w *Writer) KeyID() string {
	if w == nil || w.cfg.Recipient == nil {
		return ""
	}
	return w.cfg.Recipient.keyID
}

// Dropped 返回因队列溢出被丢弃的行数。
func (w *Writer) Dropped() uint64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Close 停止写入器：排空队列、flush、关闭文件。可重复调用。
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if w.closed {
		done := w.done
		w.mu.Unlock()
		<-done
		w.mu.RLock()
		defer w.mu.RUnlock()
		return w.closeErr
	}
	w.closed = true
	close(w.stop)
	done := w.done
	w.mu.Unlock()
	<-done
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.closeErr
}

func (w *Writer) now() time.Time { return w.cfg.Now() }

func (w *Writer) warn(msg string, args ...any) {
	if w.cfg.Logger != nil {
		w.cfg.Logger.Warn(msg, args...)
	}
}

func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	writeDropMarker := func() {
		if count := w.dropped.Swap(0); count > 0 {
			w.appendLocked(fmt.Sprintf("EVENT kind=%s count=%d\n", w.cfg.DropMarkerKind, count))
		}
	}
	flush := func() {
		writeDropMarker()
		if w.buf != nil {
			_ = w.buf.Flush()
		}
	}

	for {
		select {
		case line := <-w.queue:
			writeDropMarker()
			w.appendLocked(line)
		case <-ticker.C:
			flush()
		case <-w.stop:
			for {
				select {
				case line := <-w.queue:
					writeDropMarker()
					w.appendLocked(line)
				default:
					flush()
					if err := w.closeFileLocked(); err != nil {
						w.warn("logcrypt: close failed", "error", err)
						w.mu.Lock()
						if w.closeErr == nil {
							w.closeErr = err
						}
						w.mu.Unlock()
					}
					return
				}
			}
		}
	}
}

// appendLocked 写一条记录；必要时先轮转。任何失败都只计数并告警，绝不向上抛，
// 以免日志问题影响游戏。
func (w *Writer) appendLocked(line string) {
	now := w.now()
	day := now.Format("20060102")
	if w.file == nil || w.day != day ||
		(w.recordsInFile > 0 && w.bytesInFile >= w.cfg.MaxBytes) {
		if err := w.rollLocked(now); err != nil {
			if n := w.writeFail.Add(1); n == 1 {
				w.warn("logcrypt: rotate failed", "dir", w.cfg.Dir, "error", err)
			}
			return
		}
	}
	record, err := appendRecord(nil, w.aead, w.header, []byte(line), w.cfg.Random)
	if err != nil {
		if n := w.writeFail.Add(1); n == 1 {
			w.warn("logcrypt: encrypt failed", "error", err)
		}
		return
	}
	if _, err := w.buf.Write(record); err != nil {
		if n := w.writeFail.Add(1); n == 1 {
			w.warn("logcrypt: write failed", "path", w.path, "error", err)
		}
		return
	}
	w.bytesInFile += int64(len(record))
	w.recordsInFile++
}

// rollLocked 关闭当前文件并打开新文件（跨天或超过大小上限）。
//
// 一定新建：DEK 不落盘，续写旧文件无法解密，所以重启/滚动都另起分片。
func (w *Writer) rollLocked(now time.Time) error {
	previous := w.path
	if err := w.closeFileLocked(); err != nil {
		return err
	}
	day := now.Format("20060102")
	path, index, err := w.nextTarget(day)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304：路径来自部署配置。
	if err != nil {
		return err
	}
	w.file = file
	w.buf = bufio.NewWriterSize(file, w.cfg.BufferSize)
	w.path = path
	w.index = index
	w.day = day
	w.bytesInFile = 0
	w.recordsInFile = 0
	w.aead = nil
	w.header = nil

	dek, ephemeralPublic, wrapNonce, wrappedDEK, err := w.cfg.Recipient.wrapDEK(w.cfg.Random)
	if err != nil {
		return err
	}
	header := buildHeader(w.cfg.Recipient, ephemeralPublic, wrapNonce, wrappedDEK)
	aead, err := newAEAD(dek)
	if err != nil {
		return err
	}
	w.header = header
	w.aead = aead
	if _, err := w.buf.Write(header); err != nil {
		return err
	}
	w.bytesInFile += int64(len(header))

	w.writePointerLocked()
	if previous != "" && previous != path {
		w.schedulePruneLocked(now)
	}
	return nil
}

// nextTarget 返回当天应新建的分片路径：取磁盘上最大序号 +1。
func (w *Writer) nextTarget(day string) (string, int, error) {
	dir := filepath.Join(w.cfg.Dir, day)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, fmt.Errorf("logcrypt: create day dir: %w", err)
	}
	pattern := filepath.Join(dir, fmt.Sprintf("%s-%s-*%s", w.cfg.Prefix, day, w.cfg.Ext))
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", 0, fmt.Errorf("logcrypt: glob day dir: %w", err)
	}
	maxIndex := 0
	for _, match := range matches {
		base := strings.TrimSuffix(filepath.Base(match), w.cfg.Ext)
		parts := strings.Split(base, "-")
		index, convErr := strconv.Atoi(parts[len(parts)-1])
		if convErr != nil || index <= 0 {
			continue
		}
		if index > maxIndex {
			maxIndex = index
		}
	}
	index := maxIndex + 1
	name := fmt.Sprintf("%s-%s-%03d%s", w.cfg.Prefix, day, index, w.cfg.Ext)
	return filepath.Join(dir, name), index, nil
}

func (w *Writer) closeFileLocked() error {
	if w.buf != nil {
		if err := w.buf.Flush(); err != nil {
			w.warn("logcrypt: flush failed", "path", w.path, "error", err)
		}
		w.buf = nil
	}
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		// 刻意保留 w.path / w.header：Close 之后 Path() 仍应返回"最后写入的文件"，
		// 调用方与测试都依赖它来定位产物。rollLocked 会在赋值前先取 previous。
		return err
	}
	return nil
}

// writePointerLocked 重写 current.txt，供解密工具/脚本定位"当前文件"。
func (w *Writer) writePointerLocked() {
	relative, err := filepath.Rel(w.cfg.Dir, w.path)
	if err != nil {
		relative = w.path
	}
	pointer := filepath.Join(w.cfg.Dir, currentPointerName)
	_ = os.WriteFile(pointer, []byte(filepath.ToSlash(relative)+"\n"), 0o640)
}

// schedulePruneLocked 异步清理过期目录。只删 <YYYYMMDD> 形状且严格早于截止日的目录，
// 绝不触碰当天目录；同一时刻只允许一次清理在跑。
func (w *Writer) schedulePruneLocked(now time.Time) {
	if w.cfg.RetentionDays <= 0 || !w.pruning.CompareAndSwap(false, true) {
		return
	}
	today := now.Format("20060102")
	cutoff := now.AddDate(0, 0, -w.cfg.RetentionDays).Format("20060102")
	dir := w.cfg.Dir
	logger := w.cfg.Logger
	go func() {
		defer w.pruning.Store(false)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if len(name) != 8 || !isDigits(name) || name == today || name >= cutoff {
				continue
			}
			full := filepath.Join(dir, name)
			if removeErr := os.RemoveAll(full); removeErr != nil {
				if logger != nil {
					logger.Warn("logcrypt: prune failed", "dir", full, "error", removeErr)
				}
				continue
			}
			if logger != nil {
				logger.Info("logcrypt: pruned expired log dir", "dir", full, "retention_days", w.cfg.RetentionDays)
			}
		}
	}()
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

// CurrentPath 读取 <dir>/current.txt 指向的当前文件绝对路径。
// 找不到时返回空字符串；供解密工具与外部脚本使用。
func CurrentPath(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, currentPointerName))
	if err != nil {
		return ""
	}
	relative := strings.TrimSpace(string(raw))
	if relative == "" {
		return ""
	}
	if filepath.IsAbs(relative) {
		return relative
	}
	return filepath.Join(dir, filepath.FromSlash(relative))
}

// OpenFiles 列出 dir 下所有容器文件（按天目录排序）。
// prefix 为空时匹配全部。
func OpenFiles(dir string, prefix string) ([]string, error) {
	var out []string
	days, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pattern := "*"
	if prefix != "" {
		pattern = prefix + "-*"
	}
	for _, day := range days {
		if !day.IsDir() || len(day.Name()) != 8 || !isDigits(day.Name()) {
			continue
		}
		matches, globErr := filepath.Glob(filepath.Join(dir, day.Name(), pattern))
		if globErr != nil {
			continue
		}
		out = append(out, matches...)
	}
	return out, nil
}
