package logcrypt

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ReadFilePlaintext 用私钥解开一个容器文件。
//
// 返回 (明文, 记录数, 末尾是否被截断, error)。truncated 为 true 表示文件最后一条
// 记录没写完（掉电/强杀），前面的记录仍然有效。
func ReadFilePlaintext(privateKey []byte, path string) ([]byte, int, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304：路径来自调用方（运维本机或本机回读）。
	if err != nil {
		return nil, 0, false, err
	}
	return DecryptAll(privateKey, raw)
}

// OptionalPrivateKey 从可选路径加载私钥，用于"服务端本机可选持有私钥"的场景
// （例如 GM 记录查询需要回读自己写的记录）。
//
// 路径为空、或文件不存在时返回 (nil, nil) —— 这不是错误：分发包里**不放私钥**，
// 此时服务端只能写、不能读自己的日志，回读接口应当明确报"已加密"而不是报故障。
func OptionalPrivateKey(path string) ([]byte, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, nil
	}
	key, err := LoadPrivateKey(trimmed)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("logcrypt: optional private key %s: %w", trimmed, err)
	}
	return key, nil
}
