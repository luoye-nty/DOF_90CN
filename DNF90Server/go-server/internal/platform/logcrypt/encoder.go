package logcrypt

import (
	"crypto/cipher"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Encoder 把明文写成一个 logcrypt 容器的记录区；文件头（含被公钥封装的 DEK）由它负责写。
//
// 它**不含轮转、清理、队列**——那是 Writer 的职责。给两类调用方用：
//
//   - 自己管文件、序号与保留策略的调用方（例如 records 的 activityRecorder）；
//   - 需要把字节流当"行"写的调用方（StreamWriter 包在它外面）。
//
// 一个 Encoder 对应一个文件；换文件就新建一个。
type Encoder struct {
	core *encoderCore
}

// NewEncoder 用收件人公钥为 w 生成一把随机 DEK、写文件头，并返回可继续写记录的编码器。
func NewEncoder(recipient *Recipient, w io.Writer, random io.Reader) (*Encoder, error) {
	core, err := newEncoderCore(recipient, w, random)
	if err != nil {
		return nil, err
	}
	return &Encoder{core: core}, nil
}

// WriteRecord 把一段明文作为一条记录写入。
func (e *Encoder) WriteRecord(plain []byte) error {
	if e == nil || e.core == nil {
		return errors.New("logcrypt: encoder is not initialized")
	}
	return e.core.writeRecord(plain)
}

// WriteLine 写一行（自动补换行）。
func (e *Encoder) WriteLine(line []byte) error {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line = append(append([]byte(nil), line...), '\n')
	}
	return e.WriteRecord(line)
}

// Bytes 已写入容器的总字节数（含文件头）。调用方用它判断轮转。
func (e *Encoder) Bytes() int64 {
	if e == nil || e.core == nil {
		return 0
	}
	return e.core.written
}

// KeyID 本文件使用的收件人公钥标识。
func (e *Encoder) KeyID() string {
	if e == nil || e.core == nil {
		return ""
	}
	return e.core.keyID
}

// StreamWriter 把任意字节流按行切成加密记录，实现 io.Writer。
//
// 用于 os/exec 的 cmd.Stdout / cmd.Stderr：子进程写出来的是字节流，行边界可能被切开，
// 这里缓冲到 '\n' 再落一条记录；超过 maxLineBytes 时也先落一条，避免内存被一行撑爆。
type StreamWriter struct {
	mu           sync.Mutex
	core         *encoderCore
	buf          []byte
	maxLineBytes int
	closed       bool
	closeErr     error
}

// NewStreamWriter 创建行切分 + 加密的 io.Writer。
func NewStreamWriter(recipient *Recipient, w io.Writer, random io.Reader) (*StreamWriter, error) {
	core, err := newEncoderCore(recipient, w, random)
	if err != nil {
		return nil, err
	}
	return &StreamWriter{core: core, maxLineBytes: 1 << 20}, nil
}

// Write 实现 io.Writer。永远返回 len(p)：调用方是 os/exec 的管道拷贝协程，
// 报短写会让它把子进程的输出丢掉。
func (s *StreamWriter) Write(p []byte) (int, error) {
	if s == nil || s.core == nil {
		return 0, errors.New("logcrypt: stream writer is not initialized")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	s.buf = append(s.buf, p...)
	for {
		index := indexByte(s.buf, '\n')
		if index < 0 {
			if len(s.buf) >= s.maxLineBytes {
				if err := s.core.writeRecord(s.buf); err != nil {
					return len(p), err
				}
				s.buf = s.buf[:0]
			}
			break
		}
		if err := s.core.writeRecord(s.buf[:index+1]); err != nil {
			return len(p), err
		}
		s.buf = append(s.buf[:0], s.buf[index+1:]...)
	}
	return len(p), nil
}

// Flush 把缓冲区里最后一段没有换行的内容落一条记录。
func (s *StreamWriter) Flush() error {
	if s == nil || s.core == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *StreamWriter) flushLocked() error {
	if len(s.buf) == 0 {
		return nil
	}
	err := s.core.writeRecord(s.buf)
	s.buf = s.buf[:0]
	return err
}

// Close 落盘残余内容并封闭；可重复调用。
func (s *StreamWriter) Close() error {
	if s == nil || s.core == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.flushLocked()
	return s.closeErr
}

// Bytes 已写入字节数。
func (s *StreamWriter) Bytes() int64 {
	if s == nil || s.core == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.core.written
}

// KeyID 本流使用的收件人公钥标识。
func (s *StreamWriter) KeyID() string {
	if s == nil || s.core == nil {
		return ""
	}
	return s.core.keyID
}

// encoderCore 是 Encoder 与 StreamWriter 共用的"一个文件/一个流"的加密核心。
type encoderCore struct {
	aead    cipher.AEAD
	header  []byte
	random  io.Reader
	out     io.Writer
	written int64
	keyID   string
}

func newEncoderCore(recipient *Recipient, w io.Writer, random io.Reader) (*encoderCore, error) {
	if recipient == nil || recipient.pub == nil {
		return nil, ErrRecipientMissing
	}
	if w == nil {
		return nil, errors.New("logcrypt: encoder needs a writer")
	}
	if random == nil {
		random = defaultRandom()
	}
	dek, ephemeralPublic, wrapNonce, wrappedDEK, err := recipient.wrapDEK(random)
	if err != nil {
		return nil, err
	}
	header := buildHeader(recipient, ephemeralPublic, wrapNonce, wrappedDEK)
	aead, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	core := &encoderCore{aead: aead, header: header, random: random, out: w, keyID: recipient.keyID}
	n, err := writeFull(w, header)
	core.written += int64(n)
	if err != nil {
		return nil, fmt.Errorf("logcrypt: write container header: %w", err)
	}
	return core, nil
}

func (c *encoderCore) writeRecord(plain []byte) error {
	record, err := appendRecord(nil, c.aead, c.header, plain, c.random)
	if err != nil {
		return err
	}
	n, err := writeFull(c.out, record)
	c.written += int64(n)
	return err
}

func writeFull(w io.Writer, payload []byte) (int, error) {
	total := 0
	for total < len(payload) {
		n, err := w.Write(payload[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func indexByte(data []byte, target byte) int {
	for index, value := range data {
		if value == target {
			return index
		}
	}
	return -1
}

func defaultRandom() io.Reader { return cryptorand.Reader }
