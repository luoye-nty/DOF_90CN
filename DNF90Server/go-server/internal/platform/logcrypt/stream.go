package logcrypt

import (
	"io"
	"sync"
)

// StreamSink 是"把任意字节流加密后写进一个已打开的文件"的最简形态。
//
// 与 StreamWriter 的区别：**每次 Write 直接落一条记录，不做任何行缓冲**。
// 这是为 os/exec 的 cmd.Stdout / cmd.Stderr 准备的：
//
//   - 子进程退出时我们拿不到"最后一行写完了"的通知，缓冲会让尾部内容永远留在内存里；
//   - 逐次落盘没有缓冲，掉电/强杀也不丢已经写出的字节；
//   - 解密时记录是顺序拼接的，因此这样切分**不改变字节流本身**——
//     "abc" + "def\n" 两条记录解开后仍是 "abcdef\n"。
//
// 它不负责关闭底层文件：调用方在子进程生命周期内必须保持句柄有效。
type StreamSink struct {
	mu    sync.Mutex
	core  *encoderCore
	fail  error
	bytes int64
}

// NewStreamSink 用收件人公钥为 w 写文件头，并返回可直接接给 cmd.Stdout 的写入器。
func NewStreamSink(recipient *Recipient, w io.Writer, random io.Reader) (*StreamSink, error) {
	core, err := newEncoderCore(recipient, w, random)
	if err != nil {
		return nil, err
	}
	return &StreamSink{core: core}, nil
}

// Write 实现 io.Writer：把本次写入的内容整体作为一条记录落盘。
//
// 永远返回 len(p)：调用方是 os/exec 的管道拷贝协程，报短写会让它丢弃子进程输出。
func (s *StreamSink) Write(p []byte) (int, error) {
	if s == nil || s.core == nil {
		return 0, ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return len(p), s.fail
	}
	if err := s.core.writeRecord(p); err != nil {
		// 记下第一个错误，后续写入继续返回它但不重复做 IO。
		s.fail = err
		s.bytes = s.core.written
		return len(p), err
	}
	s.bytes = s.core.written
	return len(p), nil
}

// Bytes 已写入容器的总字节数（含文件头）。
func (s *StreamSink) Bytes() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Err 返回第一次写入失败的错误（没有失败返回 nil）。
// 注意：写入失败不会向上抛给子进程，只在这里留痕，供调用方在停服时检查。
func (s *StreamSink) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

// KeyID 本流使用的收件人公钥标识。
func (s *StreamSink) KeyID() string {
	if s == nil || s.core == nil {
		return ""
	}
	return s.core.keyID
}
