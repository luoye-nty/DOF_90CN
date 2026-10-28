// yhverify.go —— client.yh 授权上收接口（V13 改造第①步，服务端侧）
//
// 新增路由（仅配置了 -yh-pubkey/-yh-aeskey 时启用，不影响 CDK 老接口）：
//
//	POST /api/yh/verify  登录器启动上报 client.yh 原始字节：
//	                       Ed25519 验签 -> AES-256-GCM 解密 -> target 校验
//	                       -> 内嵌签名 server_ip 与上报目标 IP 比对 -> 签发短时票据
//	POST /api/yh/nonce   登录器在连接服主转发链路前申请一次性 nonce（防重放）
//	POST /api/yh/check   服主 launcher 回调验证 ticket+nonce（X-Yh-Callback-Key 鉴权）
//
// client.yh 二进制布局（对齐 C# ClientYhFormat）：
//
//	0..4  魔数 E8 0D 46 4C
//	4     格式版本(1)
//	5     载荷算法(1 = AES-256-GCM)
//	6..18 GCM Nonce(12)
//	18..22 载荷块长度 L（含16B tag，大端）
//	22..22+L AES-256-GCM 密文+tag
//	22+L..+64 Ed25519 签名（覆盖 [0,22+L)）
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------- client.yh 二进制解析 ----------------

var (
	errYhMagic    = errors.New("yh: bad magic")
	errYhLength   = errors.New("yh: bad length")
	errYhVersion  = errors.New("yh: unsupported version")
	errYhAlg      = errors.New("yh: unsupported algorithm")
	errYhSig      = errors.New("yh: ed25519 signature invalid")
	errYhOpen     = errors.New("yh: aes-gcm open failed")
	errYhNoCipher = errors.New("yh: cipher empty")
)

// yhPayload 与 C# ClientYhPayload JSON 字段严格对齐。
type yhPayload struct {
	Target    string `json:"target"`
	ServerIp  string `json:"server_ip"`
	AuthPort  int    `json:"auth_port"`
	RemoteUrl string `json:"remote_url"`
	Invite    string `json:"invite"`
	IssuedAt  string `json:"issued_at"`
}

var yhMagic = []byte{0xE8, 0x0D, 0x46, 0x4C}

// parseYhFile 验签并解密 client.yh 原文，返回明文载荷。
func parseYhFile(file []byte, pub ed25519.PublicKey, aesKey []byte) (*yhPayload, error) {
	if len(file) < 22+16+64 {
		return nil, errYhLength
	}
	if string(file[0:4]) != string(yhMagic) {
		return nil, errYhMagic
	}
	if file[4] != 1 {
		return nil, errYhVersion
	}
	if file[5] != 1 {
		return nil, errYhAlg
	}
	bodyLen := int(binary.BigEndian.Uint32(file[18:22]))
	total := 22 + bodyLen
	if total < 22+16 || total+64 != len(file) {
		return nil, errYhLength
	}
	// Ed25519 验签：签名覆盖 [0,total)
	if len(pub) != ed25519.PublicKeySize {
		return nil, errYhNoCipher
	}
	if !ed25519.Verify(pub, file[:total], file[total:]) {
		return nil, errYhSig
	}
	// AES-256-GCM 解密
	if len(aesKey) != 32 {
		return nil, errYhNoCipher
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := file[6:18]
	ct := file[22:total]
	if len(ct) < gcm.Overhead() {
		return nil, errYhOpen
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errYhOpen
	}
	var p yhPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, fmt.Errorf("yh: payload json: %w", err)
	}
	return &p, nil
}

// ---------------- Yh 服务 ----------------

type YhTicket struct {
	YhHash    string
	Target    string
	TargetIp  string
	MachineId string
	IssuedAt  time.Time
	Exp       time.Time
	NonceUsed map[string]bool
}

type YhStore struct {
	mu sync.Mutex
	m  map[string]*YhTicket
}

func NewYhStore() *YhStore {
	return &YhStore{m: map[string]*YhTicket{}}
}

func (s *YhStore) issue(p *yhPayload, machineID string, ttl time.Duration) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	tok := hex.EncodeToString(b)
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(p.ServerIp)) + "\x00" + p.Target))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if now.After(v.Exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = &YhTicket{
		YhHash:    hex.EncodeToString(h[:]),
		Target:    p.Target,
		TargetIp:  strings.TrimSpace(p.ServerIp),
		MachineId: machineID,
		IssuedAt:  now,
		Exp:       now.Add(ttl),
		NonceUsed: map[string]bool{},
	}
	return tok
}

func (s *YhStore) valid(tok string) *YhTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[tok]
	if !ok {
		return nil
	}
	if time.Now().After(t.Exp) {
		delete(s.m, tok)
		return nil
	}
	return t
}

// claimNonce 原子领取一次性 nonce；返回 false 表示已用过/票据无效。
func (s *YhStore) claimNonce(tok, nonce string) *YhTicket {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[tok]
	if !ok {
		return nil
	}
	if time.Now().After(t.Exp) {
		delete(s.m, tok)
		return nil
	}
	if t.NonceUsed[nonce] {
		return nil
	}
	t.NonceUsed[nonce] = true
	return t
}

// YhService 携带配置，挂载到 App。
type YhService struct {
	store       *YhStore
	pubKey      ed25519.PublicKey
	aesKey      []byte
	callbackKey string
	ttl         time.Duration
}

// parseHexOrB64Key 兼容 hex(64) 与 base64(44/32B 等长) 两种表达，返回原始 32B。
func parseHexOrB64Key(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty key")
	}
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	return nil, fmt.Errorf("key must be 64-hex or base64 of 32 bytes")
}

// NewYhService pubHex/aesHex 可为空（返回 nil 表示 yh 能力关闭）。
func NewYhService(pubHex, aesHex, callbackKey string, ttl time.Duration) (*YhService, error) {
	if pubHex == "" || aesHex == "" {
		return nil, nil // 未配置：保持关闭，CDK 老接口不受影响
	}
	pub, err := parseHexOrB64Key(pubHex)
	if err != nil {
		return nil, fmt.Errorf("yh pubkey: %w", err)
	}
	aesKey, err := parseHexOrB64Key(aesHex)
	if err != nil {
		return nil, fmt.Errorf("yh aeskey: %w", err)
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &YhService{
		store:       NewYhStore(),
		pubKey:      ed25519.PublicKey(pub),
		aesKey:      aesKey,
		callbackKey: strings.TrimSpace(callbackKey),
		ttl:         ttl,
	}, nil
}

// ipEquivalent 判定 yh 内嵌 IP 与上报目标等价：字符串相等；或一方为 IP 一方为域名时做 DNS 解析比对。
func ipEquivalent(want, got string) bool {
	want = strings.TrimSpace(want)
	got = strings.TrimSpace(got)
	if want == "" || got == "" {
		return false
	}
	if strings.EqualFold(want, got) {
		return true
	}
	// 域名归一（去端口/斜杠）
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "https://")
		s = strings.TrimPrefix(s, "http://")
		if i := strings.IndexAny(s, "/:"); i >= 0 {
			s = s[:i]
		}
		return strings.TrimSpace(s)
	}
	w, g := norm(want), norm(got)
	if w == g {
		return true
	}
	// 双方均需至少一方为域名：把非 IP 的一方做解析后互比
	resolve := func(host string) []net.IP {
		if ip := net.ParseIP(host); ip != nil {
			return []net.IP{ip}
		}
		addrs, err := net.LookupIP(host)
		if err != nil {
			return nil
		}
		return addrs
	}
	wIPs := resolve(w)
	gIPs := resolve(g)
	if len(wIPs) == 0 || len(gIPs) == 0 {
		return false
	}
	for _, a := range wIPs {
		for _, b := range gIPs {
			if a.Equal(b) {
				return true
			}
		}
	}
	return false
}

// ---------------- HTTP handlers ----------------

func (s *YhService) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, resp{Ok: false, Code: "ERR_METHOD", Msg: "POST only"})
		return
	}
	var req struct {
		YhB64     string `json:"yh_b64"`
		Target    string `json:"target"`
		TargetIP  string `json:"target_ip"`
		MachineID string `json:"machine_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	file, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.YhB64))
	if err != nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_MALFORMED", Msg: "client.yh 编码错误"})
		return
	}
	payload, err := parseYhFile(file, s.pubKey, s.aesKey)
	if err != nil {
		code := "ERR_YH_CRYPTO"
		msg := "client.yh 校验失败"
		switch {
		case errors.Is(err, errYhSig):
			code, msg = "ERR_YH_SIG", "client.yh 签名无效"
		case errors.Is(err, errYhMagic):
			code, msg = "ERR_YH_MALFORMED", "client.yh 魔数错误"
		}
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: code, Msg: msg})
		return
	}
	// target 必须匹配
	if strings.TrimSpace(req.Target) != "" && !strings.EqualFold(strings.TrimSpace(payload.Target), strings.TrimSpace(req.Target)) {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_TARGET", Msg: fmt.Sprintf("client.yh 绑定版本(%s)与当前(%s)不一致", payload.Target, req.Target)})
		return
	}
	// 内嵌签名 IP 与上报目标 IP 比对
	if !ipEquivalent(payload.ServerIp, req.TargetIP) {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_IP_MISMATCH", Msg: fmt.Sprintf("client.yh 绑定的服务器(%s)与当前目标(%s)不一致", payload.ServerIp, req.TargetIP)})
		return
	}
	tok := s.store.issue(payload, strings.TrimSpace(req.MachineID), s.ttl)
	if tok == "" {
		writeJSON(w, http.StatusInternalServerError, resp{Ok: false, Code: "ERR_YH_ISSUE", Msg: "票据签发失败"})
		return
	}
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"ticket":      tok,
		"expires_in":  int(s.ttl.Seconds()),
		"server_time": nowStr(),
		"payload": map[string]interface{}{
			"target":     payload.Target,
			"server_ip":  payload.ServerIp,
			"auth_port":  payload.AuthPort,
			"remote_url": payload.RemoteUrl,
			"invite":     payload.Invite,
			"issued_at":  payload.IssuedAt,
		},
	}})
}

func (s *YhService) handleNonce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, resp{Ok: false, Code: "ERR_METHOD", Msg: "POST only"})
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	if s.store.valid(strings.TrimSpace(req.Ticket)) == nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_TICKET_INVALID", Msg: "yh 票据无效或已过期，请重新启动登录器"})
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeJSON(w, http.StatusInternalServerError, resp{Ok: false, Code: "ERR_YH_NONCE", Msg: "nonce 生成失败"})
		return
	}
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"nonce": hex.EncodeToString(b),
	}})
}

func (s *YhService) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, resp{Ok: false, Code: "ERR_METHOD", Msg: "POST only"})
		return
	}
	if s.callbackKey == "" {
		writeJSON(w, http.StatusForbidden, resp{Ok: false, Code: "ERR_YH_CALLBACK_AUTH", Msg: "回调未配置"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-Yh-Callback-Key"))), []byte(s.callbackKey)) != 1 {
		writeJSON(w, http.StatusUnauthorized, resp{Ok: false, Code: "ERR_YH_CALLBACK_AUTH", Msg: "回调鉴权失败"})
		return
	}
	var req struct {
		Ticket   string `json:"ticket"`
		Nonce    string `json:"nonce"`
		TargetIP string `json:"target_ip"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	t := s.store.claimNonce(strings.TrimSpace(req.Ticket), strings.TrimSpace(req.Nonce))
	if t == nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_NONCE_USED", Msg: "ticket 无效、已过期或 nonce 已使用"})
		return
	}
	if !ipEquivalent(t.TargetIp, req.TargetIP) {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_YH_IP_MISMATCH", Msg: fmt.Sprintf("yh 绑定服务器(%s)与本次目标(%s)不一致", t.TargetIp, req.TargetIP)})
		return
	}
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"yh_hash": t.YhHash,
		"target":  t.Target,
	}})
}

// register 挂载 /api/yh/* 路由。
func (s *YhService) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/yh/verify", s.handleVerify)
	mux.HandleFunc("/api/yh/nonce", s.handleNonce)
	mux.HandleFunc("/api/yh/check", s.handleCheck)
}
