// DNF90运维工具在线授权服务端（部署于独立公网 Linux 服务器，无 GUI 常驻）。
//
// 功能：
//   - /api/verify    客户端启动校验：CDK(哈希存储) + TCP 对端 IP 与预设绑定 IP 一致 → 发放短期 token
//   - /api/heartbeat 客户端周期心跳：续期 token；CDK 被 revoke/unbind/rebind/delete 或换 IP 时
//     立即失效（服务端清会话 + 状态/IP 权威校验），客户端收到致命 code 即退出
//   - /admin/*       管理接口（作者本机 ClientYhSigner 调用，X-Admin-Token 鉴权）：
//     gen 生成CDK(绑定预设IP) / list 列表 / revoke 封禁 / unbind 解绑 / rebind 改绑换机
//
// 数据：全部落盘 data.json（CDK 仅存 sha256，明文只在 gen 时返回一次）。
// 会话：token 存内存（重启后客户端心跳失败会自动重新 verify，无需持久化）。
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------- 常量 ----------------

const (
	cdkAlphabet   = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去 0O1I
	cdkLen        = 20
	tokenTTL      = 2 * time.Hour
	adminTokBytes = 24
)

// ---------------- 数据模型 ----------------

type CDK struct {
	ID          string `json:"id"`
	CDKHash     string `json:"cdk_hash"`
	CDKCipher   string `json:"cdk_cipher,omitempty"` // 明文CDK的AES-GCM密文(hex)，仅作者管理端可解出展示
	IP          string `json:"ip"`                   // 预设/绑定公网IP
	Note        string `json:"note"`
	Status      string `json:"status"` // active / revoked / unbound（存储态；"已到期"是派生态，不回写此字段）
	CreatedAt   string `json:"created_at"`
	ActivatedAt string `json:"activated_at,omitempty"` // 语义：最后一次 verify 时间
	LastSeen    string `json:"last_seen,omitempty"`

	// 档位与期限（见 license_tier.go）。老记录这些字段为空 → 视为永久卡，无需数据迁移。
	Kind             string `json:"kind,omitempty"`               // ""/"forever"=永久；month/quarter/year=限时
	Days             int    `json:"days,omitempty"`               // 累计有效时长（限时卡）
	FirstActivatedAt string `json:"first_activated_at,omitempty"` // 首次激活时间＝起算点，只写一次
	ExpireAt         string `json:"expire_at,omitempty"`          // 到期时间(UTC RFC3339)；永久卡为空
}

type DB struct {
	AdminTokens  []string `json:"admin_tokens"`
	ServerSecret string   `json:"server_secret,omitempty"` // AES 密钥(hex 32B)，用于 CDK 明文加密展示
	CDKs         []*CDK   `json:"cdks"`
}

type Store struct {
	mu   sync.Mutex
	path string
	db   *DB
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.db = &DB{}
			return nil
		}
		return err
	}
	var db DB
	if err := json.Unmarshal(raw, &db); err != nil {
		return err
	}
	if db.AdminTokens == nil {
		db.AdminTokens = []string{}
	}
	s.db = &db
	return nil
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) ensureAdminToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.db.AdminTokens) > 0 {
		return s.db.AdminTokens[0], nil
	}
	b := make([]byte, adminTokBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.db.AdminTokens = append(s.db.AdminTokens, tok)
	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Store) findCDKByHash(h string) *CDK {
	for _, c := range s.db.CDKs {
		if c.CDKHash == h {
			return c
		}
	}
	return nil
}

func (s *Store) findCDKByID(id string) *CDK {
	for _, c := range s.db.CDKs {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// ---------------- 会话 ----------------

type Session struct {
	CDKID string
	IP    string // verify 时的对端 IP，用于防 token 跨 IP 复用
	Exp   time.Time
}

type Sessions struct {
	mu sync.Mutex
	m  map[string]*Session
}

func NewSessions() *Sessions {
	return &Sessions{m: map[string]*Session{}}
}

func (s *Sessions) Put(cdkID, ip string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 清理过期
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.Exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = &Session{CDKID: cdkID, IP: ip, Exp: now.Add(tokenTTL)}
	return tok
}

// Lookup 只读查询 token 对应的会话（过期即删），返回 CDKID 与来源 IP。
func (s *Sessions) Lookup(tok string) (cdkID, ip string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	se, ok := s.m[tok]
	if !ok {
		return "", "", false
	}
	if time.Now().After(se.Exp) {
		delete(s.m, tok)
		return "", "", false
	}
	return se.CDKID, se.IP, true
}

func (s *Sessions) Refresh(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	se, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(se.Exp) {
		delete(s.m, tok)
		return false
	}
	se.Exp = time.Now().Add(tokenTTL)
	return true
}

// KillByCDKID 清空某 CDK 的全部会话（revoke/unbind/rebind/delete 后调用，
// 使已持有旧 token 的客户端立即失效，无法续活）。
func (s *Sessions) KillByCDKID(cdkID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if v.CDKID == cdkID {
			delete(s.m, k)
		}
	}
}

// ---------------- 工具 ----------------

func hashCDK(cdk string) string {
	sum := sha256.Sum256([]byte(cdk))
	return hex.EncodeToString(sum[:])
}

// ensureServerSecret 首次启动生成 AES-256 密钥并落盘；用于加密 CDK 明文以便管理端回显。
func (s *Store) ensureServerSecret() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.db.ServerSecret) == 64 {
		return nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	s.db.ServerSecret = hex.EncodeToString(b)
	return s.saveLocked()
}

// sealCDK AES-256-GCM 加密明文 CDK，返回 hex；secretHex 为 64 位 hex。
func sealCDK(plain, secretHex string) (string, error) {
	key, err := hex.DecodeString(secretHex)
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("bad server secret")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// openCDK 解密 CDK 明文；secretHex 为 64 位 hex。
func openCDK(cipherHex, secretHex string) (string, error) {
	if cipherHex == "" || len(secretHex) != 64 {
		return "", fmt.Errorf("no cipher")
	}
	key, err := hex.DecodeString(secretHex)
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("bad server secret")
	}
	raw, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("bad cipher len")
	}
	nonce, data := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func normalizeCDK(s string) string {
	s = strings.ToUpper(s)
	s = strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	return s
}

func genCDK() string {
	b := make([]byte, cdkLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(cdkAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = cdkAlphabet[n.Int64()]
	}
	return string(b)
}

func formatCDK(s string) string {
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && i%5 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func nowStr() string { return time.Now().Format(time.RFC3339) }

// ---------------- HTTP 应用 ----------------

type App struct {
	st       *Store
	sessions *Sessions
	minProto int // 允许登录的最低客户端协议版本（见 license_tier.go clientProtoCurrent）
}

type resp struct {
	Ok   bool        `json:"ok"`
	Code string      `json:"code,omitempty"`
	Msg  string      `json:"msg,omitempty"`
	Data interface{} `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, code int, r resp) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(r)
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) adminOK(r *http.Request) bool {
	tok := r.Header.Get("X-Admin-Token")
	if tok == "" {
		return false
	}
	a.st.mu.Lock()
	defer a.st.mu.Unlock()
	for _, t := range a.st.db.AdminTokens {
		if t == tok {
			return true
		}
	}
	return false
}

func (a *App) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, resp{Ok: false, Code: "ERR_METHOD", Msg: "POST only"})
		return
	}
	var req struct {
		CDK   string `json:"cdk"`
		Proto int    `json:"proto"` // 客户端协议版本；老客户端不发此字段（=0）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	// 版本闸门：老版本赞助版运维工具一律拒绝登录（在 CDK 校验之前，永久卡老客户端同样被拦）。
	if req.Proto < a.minProto {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CLIENT_OUTDATED",
			Msg: "当前工具版本已停止授权服务，请下载最新版运维工具后重试（原授权卡继续有效）"})
		return
	}
	cdk := normalizeCDK(req.CDK)
	if len(cdk) != cdkLen {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_NOT_FOUND", Msg: "CDK 格式不正确"})
		return
	}
	h := hashCDK(cdk)
	ip := peerIP(r)

	a.st.mu.Lock()
	c := a.st.findCDKByHash(h)
	if c == nil {
		a.st.mu.Unlock()
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_NOT_FOUND", Msg: "CDK 不存在"})
		return
	}
	if c.Status == "revoked" {
		a.st.mu.Unlock()
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_REVOKED", Msg: "CDK 已被封禁，请联系作者"})
		return
	}
	if c.Status != "active" {
		a.st.mu.Unlock()
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_INACTIVE", Msg: "CDK 未激活，请联系作者"})
		return
	}
	// 到期/误配判定（限时卡）：与 heartbeat 共用 deny()，到期立即失效、不给宽限。
	now := time.Now()
	if code, msg := c.deny(now); code != "" {
		cid := c.ID
		a.st.mu.Unlock()
		log.Printf("拒绝登录：id=%s [%s] %s", cid, code, msg)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: code, Msg: msg})
		return
	}
	if c.IP != ip {
		a.st.mu.Unlock()
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_IP_MISMATCH", Msg: fmt.Sprintf("CDK 绑定的服务器 IP(%s) 与当前 IP(%s) 不一致", c.IP, ip)})
		return
	}
	// 首次激活：写入起算点与到期时间（仅一次；重启/换绑不重置）。
	firstActivate := c.activateIfNeeded(now)
	c.ActivatedAt = nowStr()
	c.LastSeen = nowStr()
	_ = a.st.saveLocked()
	// 出锁前取齐响应字段，避免并发读写。
	kind := normalizeKind(c.Kind)
	kindName := kindLabel(kind)
	expireAt := c.ExpireAt
	remain := c.remainDays(now)
	firstAt := c.FirstActivatedAt
	expireText := c.expireText()
	cid := c.ID
	a.st.mu.Unlock()

	if firstActivate {
		log.Printf("CDK 首次激活：id=%s 档位=%s 到期=%s", cid, kindName, expireText)
	}

	tok := a.sessions.Put(cid, ip)
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"token":              tok,
		"expires_in":         int(tokenTTL.Seconds()),
		"server_time":        nowStr(),
		"kind":               kind,
		"kind_label":         kindName,
		"expire_at":          expireAt,
		"expire_text":        expireText,
		"remain_days":        remain,
		"first_activated_at": firstAt,
	}})
	return
}

func (a *App) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, resp{Ok: false, Code: "ERR_METHOD", Msg: "POST only"})
		return
	}
	var req struct {
		Token string `json:"token"`
		CDK   string `json:"cdk"`
		Proto int    `json:"proto"` // 客户端协议版本；老客户端不发此字段（=0）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	// 版本闸门：老版本客户端的心跳一律拒绝（“永不返回成功”即可断开其授权）。
	if req.Proto < a.minProto {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CLIENT_OUTDATED",
			Msg: "当前工具版本已停止授权服务，请下载最新版运维工具后重试（原授权卡继续有效）"})
		return
	}
	cdk := normalizeCDK(req.CDK)
	h := hashCDK(cdk)
	ip := peerIP(r)

	a.st.mu.Lock()
	c := a.st.findCDKByHash(h)
	if c == nil {
		a.st.mu.Unlock()
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_NOT_FOUND", Msg: "CDK 不存在"})
		return
	}
	switch c.Status {
	case "revoked":
		a.st.mu.Unlock()
		a.sessions.KillByCDKID(c.ID) // 清会话，兜底不识别新 code 的旧客户端
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "REVOKED", Msg: "CDK 已被封禁，请联系作者"})
		return
	case "unbound":
		a.st.mu.Unlock()
		a.sessions.KillByCDKID(c.ID)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_INACTIVE", Msg: "CDK 已解绑，请联系作者重新绑定"})
		return
	}
	if c.Status != "active" {
		a.st.mu.Unlock()
		a.sessions.KillByCDKID(c.ID)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_INACTIVE", Msg: "CDK 未激活，请联系作者"})
		return
	}
	// 到期/误配判定（限时卡）：与 verify 共用 deny()。到期即清会话 + 拒绝续期，
	// 是"客户端长期不重启也不会白嫖"的关键一环。
	now := time.Now()
	if code, msg := c.deny(now); code != "" {
		cid := c.ID
		a.st.mu.Unlock()
		a.sessions.KillByCDKID(cid)
		log.Printf("心跳拒绝：id=%s [%s] %s", cid, code, msg)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: code, Msg: msg})
		return
	}
	if c.IP != ip {
		a.st.mu.Unlock()
		a.sessions.KillByCDKID(c.ID)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_IP_MISMATCH", Msg: fmt.Sprintf("CDK 绑定 IP(%s) 与当前 IP(%s) 不一致，请重新验证", c.IP, ip)})
		return
	}
	c.LastSeen = nowStr()
	_ = a.st.saveLocked()
	a.st.mu.Unlock()

	// 会话校验：token 必须存在、属于该 CDK、且来源 IP 与 verify 时一致。
	cdkID, sip, ok := a.sessions.Lookup(req.Token)
	if !ok || cdkID != c.ID || sip != ip {
		a.sessions.KillByCDKID(c.ID)
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_TOKEN_INVALID", Msg: "会话已失效，请重新验证"})
		return
	}
	if !a.sessions.Refresh(req.Token) {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_TOKEN_INVALID", Msg: "会话已失效，请重新验证"})
		return
	}
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{"server_time": nowStr()}})
}

// ---------------- 管理接口 ----------------

func (a *App) handleAdminGen(w http.ResponseWriter, r *http.Request) {
	if !a.adminOK(r) {
		writeJSON(w, http.StatusUnauthorized, resp{Ok: false, Code: "ERR_ADMIN_AUTH", Msg: "管理鉴权失败"})
		return
	}
	var req struct {
		IP   string `json:"ip"`
		Note string `json:"note"`
		Kind string `json:"kind"` // month/quarter/year/forever；缺省（含老管理端不发此字段）= forever
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	ip := strings.TrimSpace(req.IP)
	if net.ParseIP(ip) == nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_IP", Msg: "IP 不合法"})
		return
	}
	kind := normalizeKind(req.Kind)
	if !kindValid(kind) {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_KIND",
			Msg: fmt.Sprintf("卡类型不支持：%s（可用：永久卡/月卡/季卡/年卡）", req.Kind)})
		return
	}
	days := cardDays[kind] // 永久卡为 0
	plain := genCDK()
	cipherHex, err := sealCDK(plain, a.st.db.ServerSecret)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, resp{Ok: false, Code: "ERR_SEAL", Msg: "CDK 加密失败"})
		return
	}
	c := &CDK{
		ID:        genCDK()[:8] + hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano()%100000)))[:4],
		CDKHash:   hashCDK(plain),
		CDKCipher: cipherHex,
		IP:        ip,
		Note:      strings.TrimSpace(req.Note),
		Status:    "active",
		CreatedAt: nowStr(),
		Kind:      kind,
		Days:      days,
	}
	a.st.mu.Lock()
	a.st.db.CDKs = append(a.st.db.CDKs, c)
	_ = a.st.saveLocked()
	a.st.mu.Unlock()
	log.Printf("生成 CDK：id=%s 档位=%s 时长=%d天 绑定IP=%s", c.ID, kindLabel(kind), days, ip)
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"id":         c.ID,
		"cdk":        formatCDK(plain),
		"ip":         ip,
		"kind":       kind,
		"kind_label": kindLabel(kind),
		"days":       days,
	}})
}

func (a *App) handleAdminList(w http.ResponseWriter, r *http.Request) {
	if !a.adminOK(r) {
		writeJSON(w, http.StatusUnauthorized, resp{Ok: false, Code: "ERR_ADMIN_AUTH", Msg: "管理鉴权失败"})
		return
	}
	a.st.mu.Lock()
	defer a.st.mu.Unlock()
	now := time.Now()
	list := make([]map[string]interface{}, 0, len(a.st.db.CDKs))
	for _, c := range a.st.db.CDKs {
		cdk := ""
		if plain, err := openCDK(c.CDKCipher, a.st.db.ServerSecret); err == nil {
			cdk = formatCDK(plain)
		}
		list = append(list, map[string]interface{}{
			"id":           c.ID,
			"cdk":          cdk,
			"ip":           c.IP,
			"note":         c.Note,
			"status":       c.Status,
			"created_at":   c.CreatedAt,
			"activated_at": c.ActivatedAt,
			"last_seen":    c.LastSeen,
			// 档位与期限（老管理端忽略未知字段，安全）
			"kind":               normalizeKind(c.Kind),
			"kind_label":         kindLabel(c.Kind),
			"days":               c.Days,
			"first_activated_at": c.FirstActivatedAt,
			"expire_at":          c.ExpireAt,
			"expire_text":        c.expireText(),
			"remain_days":        c.remainDays(now),
			"expired":            c.expired(now),
			"misconfigured":      c.misconfigured(),
		})
	}
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{"list": list}})
}

func (a *App) handleAdminAction(w http.ResponseWriter, r *http.Request) {
	if !a.adminOK(r) {
		writeJSON(w, http.StatusUnauthorized, resp{Ok: false, Code: "ERR_ADMIN_AUTH", Msg: "管理鉴权失败"})
		return
	}
	var req struct {
		ID   string `json:"id"`
		IP   string `json:"ip"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	a.st.mu.Lock()
	defer a.st.mu.Unlock()
	c := a.st.findCDKByID(strings.TrimSpace(req.ID))
	if c == nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_NOT_FOUND", Msg: "记录不存在"})
		return
	}
	action := strings.TrimSpace(r.URL.Path)
	switch {
	case strings.HasSuffix(action, "/revoke"):
		c.Status = "revoked"
		a.sessions.KillByCDKID(c.ID) // 清会话：已登录客户端下个心跳即被踢
	case strings.HasSuffix(action, "/unbind"):
		c.Status = "unbound"
		a.sessions.KillByCDKID(c.ID)
	case strings.HasSuffix(action, "/activate"):
		if c.Status == "revoked" {
			writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_CDK_REVOKED", Msg: "封禁状态不可直接激活，请先解封"})
			return
		}
		c.Status = "active"
	case strings.HasSuffix(action, "/rebind"):
		if net.ParseIP(strings.TrimSpace(req.IP)) == nil {
			writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_IP", Msg: "IP 不合法"})
			return
		}
		c.IP = strings.TrimSpace(req.IP)
		c.Status = "active"
		a.sessions.KillByCDKID(c.ID) // 改绑后旧 IP 会话全部失效
	case strings.HasSuffix(action, "/note"):
		c.Note = strings.TrimSpace(req.Note)
	case strings.HasSuffix(action, "/delete"):
		// 删除记录（不可恢复）
		a.sessions.KillByCDKID(c.ID)
		for i, it := range a.st.db.CDKs {
			if it.ID == c.ID {
				a.st.db.CDKs = append(a.st.db.CDKs[:i], a.st.db.CDKs[i+1:]...)
				break
			}
		}
	default:
		writeJSON(w, http.StatusNotFound, resp{Ok: false, Code: "ERR_ACTION", Msg: "未知操作"})
		return
	}
	_ = a.st.saveLocked()
	writeJSON(w, http.StatusOK, resp{Ok: true})
}

// ---------------- 续期 ----------------

// handleAdminRenew 续期（作者管理端调用）：POST /admin/renew {id, kind 或 days}。
//
// 口径：
//   - 未到期 → 从原到期日叠加（客户不亏）；
//   - 已到期 → 从当前时刻起算；
//   - 尚未激活的限时卡 → 顺延总时长（起算点仍是首次激活，避免囤卡期间被算掉）；
//   - 永久卡 → 拒绝（无意义）。
//
// 只改 ExpireAt/Days，绝不改 Status：封禁/解绑的卡续期后依然不可用，需单独解封，
// 防止"续个费把封禁卡顺手放出来"。
func (a *App) handleAdminRenew(w http.ResponseWriter, r *http.Request) {
	if !a.adminOK(r) {
		writeJSON(w, http.StatusUnauthorized, resp{Ok: false, Code: "ERR_ADMIN_AUTH", Msg: "管理鉴权失败"})
		return
	}
	var req struct {
		ID   string `json:"id"`
		Kind string `json:"kind"` // 续期档位 month/quarter/year
		Days int    `json:"days"` // 直接指定天数（>0 时优先于 kind）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, resp{Ok: false, Code: "ERR_BODY", Msg: "bad body"})
		return
	}
	days := req.Days
	if days <= 0 {
		kind := normalizeKind(req.Kind)
		if kind == kindForever || cardDays[kind] <= 0 {
			writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_KIND",
				Msg: "请指定续期档位（月卡/季卡/年卡）或正数天数"})
			return
		}
		days = cardDays[kind]
	}
	if days > 3650 {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_DAYS", Msg: "单次续期天数过大（上限 3650 天）"})
		return
	}

	a.st.mu.Lock()
	defer a.st.mu.Unlock()
	c := a.st.findCDKByID(strings.TrimSpace(req.ID))
	if c == nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_NOT_FOUND", Msg: "记录不存在"})
		return
	}
	if c.isForever() {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_KIND", Msg: "永久卡无需续期"})
		return
	}
	now := time.Now()
	wasExpired := c.expired(now)
	if err := c.renew(now, days); err != nil {
		writeJSON(w, http.StatusOK, resp{Ok: false, Code: "ERR_RENEW", Msg: err.Error()})
		return
	}
	_ = a.st.saveLocked()
	log.Printf("续期：id=%s +%d天 原状态=%s 到期=%s", c.ID, days, map[bool]string{true: "已到期", false: "未到期"}[wasExpired], c.expireText())
	writeJSON(w, http.StatusOK, resp{Ok: true, Data: map[string]interface{}{
		"id":          c.ID,
		"kind":        normalizeKind(c.Kind),
		"kind_label":  kindLabel(c.Kind),
		"days":        c.Days,
		"expire_at":   c.ExpireAt,
		"expire_text": c.expireText(),
		"remain_days": c.remainDays(now),
		"added_days":  days,
		"was_expired": wasExpired,
		"status":      c.Status, // 提示调用方：续期不改状态，封禁/解绑需另行处理
	}})
}

// ---------------- 证书 ----------------

func ensureCert(certPath, keyPath string) error {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return nil
		}
	}
	log.Printf("未找到证书，自动生成自签证书: %s", certPath)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   "dnf90-auth",
			Organization: []string{"DNF90-Auth"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}
	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		certOut.Close()
		return err
	}
	certOut.Close()
	keyDer, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer keyOut.Close()
	return pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})
}

// ---------------- main ----------------

// flagOrEnv 优先 flag，为空时回退环境变量。
func flagOrEnv(fv *string, env string) string {
	if fv != nil && *fv != "" {
		return *fv
	}
	return os.Getenv(env)
}

func main() {
	addr := flag.String("addr", ":8443", "监听地址")
	data := flag.String("data", "data.json", "数据文件路径")
	cert := flag.String("cert", "cert.pem", "证书路径")
	key := flag.String("key", "key.pem", "私钥路径")
	// 客户端最低协议版本：默认 2（拒绝 2026-09 之前的老版赞助版运维工具登录）。
	// 紧急放行老客户端：改 systemd ExecStart 追加 -min-proto=1 或在命令行传该参数。
	minProto := flag.Int("min-proto", clientProtoCurrent, "允许登录的最低客户端协议版本（1=放行老客户端，2=强制升级）")
	// client.yh 授权上收（V13）：不配置则 /api/yh/* 保持关闭
	yhPubKey := flag.String("yh-pubkey", "", "yh Ed25519 验签公钥(hex64)；环境变量 YH_PUBKEY_HEX")
	yhAesKey := flag.String("yh-aeskey", "", "yh AES-256 解密密钥(hex64)；环境变量 YH_AESKEY_HEX")
	yhCallbackKey := flag.String("yh-callback-key", "", "服主 launcher 回调密钥；环境变量 YH_CALLBACK_KEY")
	yhTTL := flag.Duration("yh-ticket-ttl", 24*time.Hour, "yh 票据有效期；环境变量 YH_TICKET_TTL(如 24h)")
	flag.Parse()

	dir := filepath.Dir(*data)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := ensureCert(*cert, *key); err != nil {
		log.Fatalf("证书准备失败: %v", err)
	}

	st := &Store{path: *data}
	if err := st.load(); err != nil {
		log.Fatalf("加载数据失败: %v", err)
	}
	tok, err := st.ensureAdminToken()
	if err != nil {
		log.Fatalf("初始化管理令牌失败: %v", err)
	}
	log.Printf("管理令牌: %s （请保存，用于 ClientYhSigner 授权管理）", tok)
	if err := st.ensureServerSecret(); err != nil {
		log.Fatalf("初始化 CDK 加密密钥失败: %v", err)
	}

	app := &App{st: st, sessions: NewSessions(), minProto: *minProto}
	if app.minProto < 1 {
		app.minProto = 1
	}
	if app.minProto > clientProtoCurrent {
		log.Printf("警告：-min-proto=%d 高于当前客户端协议版本 %d，最新版客户端也将无法登录", app.minProto, clientProtoCurrent)
	}
	log.Printf("客户端版本闸门：最低协议版本 proto>=%d（%s）", app.minProto,
		map[bool]string{true: "老版本运维工具将被拒绝登录", false: "已放宽，老版本仍可登录"}[app.minProto >= clientProtoCurrent])

	// yh 授权服务：flag 优先，回退环境变量
	yhPub := flagOrEnv(yhPubKey, "YH_PUBKEY_HEX")
	yhAes := flagOrEnv(yhAesKey, "YH_AESKEY_HEX")
	yhCb := flagOrEnv(yhCallbackKey, "YH_CALLBACK_KEY")
	ttlStr := os.Getenv("YH_TICKET_TTL")
	if ttlStr != "" {
		if d, err := time.ParseDuration(ttlStr); err == nil {
			*yhTTL = d
		}
	}
	yhSvc, err := NewYhService(yhPub, yhAes, yhCb, *yhTTL)
	if err != nil {
		log.Fatalf("yh 服务初始化失败: %v", err)
	}
	if yhSvc == nil {
		log.Printf("yh 授权接口未配置密钥，保持关闭（不影响 CDK 接口）")
	} else {
		log.Printf("yh 授权接口已启用，票据 TTL=%s", yhSvc.ttl)
	}

	certPEM, err := os.ReadFile(*cert)
	if err != nil {
		log.Fatalf("读证书失败: %v", err)
	}
	_ = certPEM

	mux := http.NewServeMux()
	mux.HandleFunc("/api/verify", app.handleVerify)
	mux.HandleFunc("/api/heartbeat", app.handleHeartbeat)
	if yhSvc != nil {
		yhSvc.register(mux)
	}
	mux.HandleFunc("/admin/gen", app.handleAdminGen)
	mux.HandleFunc("/admin/list", app.handleAdminList)
	mux.HandleFunc("/admin/revoke", app.handleAdminAction)
	mux.HandleFunc("/admin/unbind", app.handleAdminAction)
	mux.HandleFunc("/admin/activate", app.handleAdminAction)
	mux.HandleFunc("/admin/rebind", app.handleAdminAction)
	mux.HandleFunc("/admin/note", app.handleAdminAction)
	mux.HandleFunc("/admin/renew", app.handleAdminRenew)
	mux.HandleFunc("/admin/delete", app.handleAdminAction)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, resp{Ok: true, Msg: "alive"})
	})

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("auth-server 启动，监听 %s", *addr)
	if err := srv.ListenAndServeTLS(*cert, *key); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
