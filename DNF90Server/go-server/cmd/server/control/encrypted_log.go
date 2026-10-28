package main

import (
	"os"
	"path/filepath"

	"longheng.io/server/internal/platform/logcrypt"
	"longheng.io/server/internal/platform/logkey"
)

// 服务端 stdout/stderr 自 2026-09-30 起**落盘即密文**（公钥信封）。
//
// 这两份日志曾是最大的明文来源（17 小时 101 MB），内容含客户端 IP、admin 请求路径、
// 角色名与聊天摘要——正是"服务端要分发出去"时最不该外泄的现场信息。
//
// 密钥：收件人公钥**固化在二进制里**（internal/platform/logkey），因此运维不需要
// 部署任何密钥文件；只有显式设了 DNFBRIDGE_LOG_PUBLIC_KEY_FILE 才用它覆盖（轮换/测试）。
// 能解开日志的私钥只在运营本机，分发包里没有。

// serverLogStream 是接给 cmd.Stdout / cmd.Stderr 的加密写入器。
//
// 刻意不在这里 Close：os/exec 会起一个拷贝协程，在子进程存活期间持续调用 Write，
// 提前关闭会让服务端输出全部丢失。每次 start 保留一对句柄到本进程退出，
// 相比服务端自身的运行时长可以忽略。
type serverLogStream struct {
	path string
	file *os.File
	sink *logcrypt.StreamSink
}

// openEncryptedLogStream 打开一个加密日志流并写入容器头。
func openEncryptedLogStream(path string) (*serverLogStream, error) {
	recipient, err := logkey.ResolveRecipient()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	sink, err := logcrypt.NewStreamSink(recipient, file, nil)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &serverLogStream{path: path, file: file, sink: sink}, nil
}

// Write 实现 io.Writer。
func (s *serverLogStream) Write(p []byte) (int, error) { return s.sink.Write(p) }

// Path 返回容器文件路径。
func (s *serverLogStream) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Err 返回第一次写入失败的错误（用于停服时留痕）。
func (s *serverLogStream) Err() error {
	if s == nil {
		return nil
	}
	return s.sink.Err()
}

// Close 关闭底层文件。只有在确认子进程已经退出之后才可调用。
func (s *serverLogStream) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}
