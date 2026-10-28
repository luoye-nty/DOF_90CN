//go:build paid

// 配对码认证与短时管理 token（内存存储）：
//   - 配对码 sha256(salt+code) 哈希比对（常量时间），salt 由 GUI 开启远程管理时随机生成；
//   - 登录成功签发短时 token，只存 sha256 摘要，TTL 可配；
//   - 修改/清除配对码、主动作废、关闭远程管理时全量作废 token；
//   - 按来源 IP 记录失败次数，超过阈值锁定 lockout 时长。
package manage

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotEnabled   = errors.New("not-enabled")
	ErrInvalidPair  = errors.New("invalid-pairing-code")
	ErrPairLocked   = errors.New("pairing-locked")
	ErrTokenInvalid = errors.New("invalid-token")
)

// pairingCharset 配对码字符集：去掉易混淆 0/O/1/I，全大写。
const pairingCharset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GenPairingCode 生成 n 位去易混大写配对码（n<8 时按 8 处理）。
func GenPairingCode(n int) (string, error) {
	if n < 8 {
		n = 8
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = pairingCharset[int(buf[i])%len(pairingCharset)]
	}
	return string(out), nil
}

// PairingHash 计算配对码哈希 sha256(salt+code) hex。
func PairingHash(salt, code string) string {
	sum := sha256.Sum256([]byte(salt + normalizePairing(code)))
	return hex.EncodeToString(sum[:])
}

// GenSalt 生成随机盐（32 字节 hex）。
func GenSalt() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func normalizePairing(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// newToken 随机 32 字节 hex token。
func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type tokenRecord struct {
	expiresAt time.Time
}

type failRecord struct {
	fails int
	until time.Time
}

// Auth 配对码/token 内存认证状态。方法全部线程安全。
type Auth struct {
	mu sync.Mutex

	salt string
	hash string

	tokenTTL time.Duration
	maxFail  int
	lockout  time.Duration

	tokens map[string]tokenRecord // key = sha256(token)
	fails  map[string]failRecord  // key = 来源 IP
}

// NewAuth 以当前配对码创建认证器；salt/hash 为空表示未启用。
func NewAuth(salt, hash string, tokenTTL time.Duration, maxFail int, lockout time.Duration) *Auth {
	return &Auth{
		salt:     salt,
		hash:     hash,
		tokenTTL: tokenTTL,
		maxFail:  maxFail,
		lockout:  lockout,
		tokens:   make(map[string]tokenRecord),
		fails:    make(map[string]failRecord),
	}
}

// SetPairing 更新配对码（重新生成/自定义修改/关闭时调用）；任一项为空表示关闭。
// 变更后全量作废旧 token 并清空失败计数。
func (a *Auth) SetPairing(salt, hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.salt = salt
	a.hash = hash
	a.tokens = make(map[string]tokenRecord)
	a.fails = make(map[string]failRecord)
}

// Enabled 返回配对码是否已启用。
func (a *Auth) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.salt != "" && a.hash != ""
}

// SetLockout 仅用于配置运行时调整（保留）。
func (a *Auth) SetLockout(lockout time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lockout = lockout
}

// Login 校验配对码并签发 token；失败按来源 IP 计数/锁定。
func (a *Auth) Login(ip, code string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.salt == "" || a.hash == "" {
		return "", ErrNotEnabled
	}
	if rec, ok := a.fails[ip]; ok && time.Now().Before(rec.until) {
		return "", ErrPairLocked
	}

	want := sha256hex(a.salt + normalizePairing(code))
	if subtle.ConstantTimeCompare([]byte(want), []byte(a.hash)) != 1 {
		rec := a.fails[ip]
		rec.fails++
		if rec.fails >= a.maxFail {
			rec.until = time.Now().Add(a.lockout)
		}
		a.fails[ip] = rec
		return "", ErrInvalidPair
	}

	delete(a.fails, ip)

	tok, err := newToken()
	if err != nil {
		return "", err
	}
	a.tokens[sha256hex(tok)] = tokenRecord{expiresAt: time.Now().Add(a.tokenTTL)}
	return tok, nil
}

// Validate 校验 token 有效性（自动剔除过期项）。
func (a *Auth) Validate(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	key := sha256hex(token)

	a.mu.Lock()
	defer a.mu.Unlock()
	rec, ok := a.tokens[key]
	if !ok {
		return false
	}
	if time.Now().After(rec.expiresAt) {
		delete(a.tokens, key)
		return false
	}
	return true
}

// RevokeAll 主动作废全部 token（配对码不变也可调用）。
func (a *Auth) RevokeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens = make(map[string]tokenRecord)
	a.fails = make(map[string]failRecord)
}

// RevokeIP 解禁某 IP（登录成功后由 handler 一并清理）。
func (a *Auth) RevokeIP(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

// Revoke 主动作废单个 token（登出时调用）。
func (a *Auth) Revoke(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	key := sha256hex(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, key)
}
