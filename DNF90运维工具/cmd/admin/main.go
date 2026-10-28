package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
)

const (
	sessionCookieName = "dnf90admin"
	sessionTTL        = 12 * time.Hour
	maxRequestBody    = 16 << 20
)

// MySQLConfig 与 admin.json 的 mysql 段保持一致。
type MySQLConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
}

// Config 是 admin.json 的完整结构；字段名必须与线上部署保持一致。
type Config struct {
	Listen         string      `json:"listen"`
	Password       string      `json:"password"`
	SessionSecret  string      `json:"session_secret"`
	GameAdminURL   string      `json:"game_admin_url"`
	GameAdminToken string      `json:"game_admin_token"`
	MySQL          MySQLConfig `json:"mysql"`
	Title          string      `json:"title"`
	TrustProxy     bool        `json:"trust_proxy"`
}

func defaultConfig() Config {
	return Config{
		Listen:       ":18114",
		GameAdminURL: "http://127.0.0.1:18111",
		MySQL: MySQLConfig{
			Host:   "127.0.0.1",
			Port:   3306,
			User:   "dnf90",
			DBName: "dnf_local",
		},
		Title: "DNF90 管理后台",
	}
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = ":18114"
	}
	if strings.TrimSpace(cfg.GameAdminURL) == "" {
		cfg.GameAdminURL = "http://127.0.0.1:18111"
	}
	if strings.TrimSpace(cfg.Title) == "" {
		cfg.Title = "DNF90 管理后台"
	}
	if cfg.MySQL.Host == "" {
		cfg.MySQL.Host = "127.0.0.1"
	}
	if cfg.MySQL.Port <= 0 {
		cfg.MySQL.Port = 3306
	}
	if cfg.MySQL.DBName == "" {
		cfg.MySQL.DBName = "dnf_local"
	}
	if strings.TrimSpace(cfg.Password) == "" {
		return nil, errors.New("配置 password 不能为空")
	}
	if strings.TrimSpace(cfg.GameAdminToken) == "" {
		return nil, errors.New("配置 game_admin_token 不能为空")
	}
	if strings.TrimSpace(cfg.SessionSecret) == "" {
		secret, err := randomHex(32)
		if err != nil {
			return nil, err
		}
		cfg.SessionSecret = secret
		log.Printf("配置缺少 session_secret，本次启动使用临时密钥（重启后需重新登录）")
	}
	return &cfg, nil
}

type loginState struct {
	count int
	until time.Time
}

type webServer struct {
	cfg   *Config
	store *store.Store
	gm    *gmClient

	loginMu    sync.Mutex
	loginFails map[string]*loginState

	allMu      sync.Mutex
	allCache   *allCharactersResponse
	allScanned time.Time

	batches *batchRegistry
}

func main() {
	configPath := flag.String("config", "admin.json", "admin.json 路径")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}

	var st *store.Store
	st, err = store.Open(config.MySQLConfig{
		Host:     cfg.MySQL.Host,
		Port:     cfg.MySQL.Port,
		User:     cfg.MySQL.User,
		Password: cfg.MySQL.Password,
		DBName:   cfg.MySQL.DBName,
	})
	if err != nil {
		log.Printf("警告：MySQL 连接失败，账号/封禁功能不可用: %v", err)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if schemaErr := st.EnsureBansSchema(ctx); schemaErr != nil {
			log.Printf("警告：检查封禁表失败: %v", schemaErr)
		}
		cancel()
	}

	s := &webServer{
		cfg:        cfg,
		store:      st,
		gm:         newGMClient(cfg.GameAdminURL, cfg.GameAdminToken),
		loginFails: make(map[string]*loginState),
		batches:    newBatchRegistry(),
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("DNF90 管理后台启动：listen=%s game_admin=%s title=%s",
		cfg.Listen, cfg.GameAdminURL, cfg.Title)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
}

func (s *webServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	protected := func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, s.auth(fn))
	}

	protected("GET /api/status", s.handleStatus)
	protected("GET /api/accounts", s.handleSearchAccounts)
	protected("GET /api/accounts/{id}/characters", s.handleAccountCharacters)
	protected("GET /api/accounts/{id}", s.handleAccountDetail)
	protected("POST /api/account/black-diamond", s.handleAccountBlackDiamond)
	protected("GET /api/account/black-diamond", s.handleAccountBlackDiamondGet)
	protected("POST /api/account/value", s.handleAccountValue)
	protected("GET /api/characters", s.handleFindCharacter)
	protected("GET /api/all-characters", s.handleAllCharacters)
	protected("GET /api/class-tree", s.handleClassTree)

	protected("GET /api/items", s.handleItems)
	protected("GET /api/items/categories", s.handleItemCategories)
	protected("GET /api/items/{id}/mail-options", s.handleItemMailOptions)
	protected("GET /api/items/{id}", s.handleItemDetail)
	protected("GET /api/avatar-sets", s.handleAvatarSets)

	protected("GET /api/bans", s.handleBanList)
	protected("POST /api/bans", s.handleBanCreate)
	protected("DELETE /api/bans", s.handleBanDelete)
	protected("POST /api/bans/kick", s.handleBanKick)

	protected("GET /api/records/meta", s.handleRecordsMeta)
	protected("POST /api/records/query", s.handleRecordsQuery)
	protected("GET /api/raid-reward-pools", s.handleRaidRewardPools)
	protected("GET /api/raid-reward-rate", s.handleRaidRewardRateGet)
	protected("POST /api/raid-reward-rate", s.handleRaidRewardRateSet)
	protected("POST /api/channels/reload", s.handleChannelsReload)

	protected("POST /api/mail", s.handleSendMail)
	protected("POST /api/mail/rich", s.handleRichMail)
	protected("POST /api/cera", s.handleGrantCera)
	protected("POST /api/wallet/query", s.handleWalletQuery)

	protected("GET /api/character/black-diamond", s.handleBlackDiamondGet)
	protected("POST /api/character/black-diamond", s.handleBlackDiamondPost)
	protected("POST /api/character/level", s.handleSetLevel)
	protected("POST /api/character/sptp", s.handleSetSPTP)
	protected("POST /api/character/job", s.handleSetJob)
	protected("POST /api/character/career", s.handleSetCareer)
	protected("POST /api/character/skill-bonus", s.handleSetSkillBonus)
	protected("POST /api/character/wallet/repair-happy", s.handleRepairHappy)
	protected("POST /api/character/dungeons-unlock", s.handleUnlockDungeons)
	protected("POST /api/character/equip-slots-unlock", s.handleUnlockEquipSlots)
	protected("POST /api/character/dungeons/unlock-difficulties", s.handleUnlockDungeonDifficulties)
	protected("POST /api/character/dungeons/unlock-hell", s.handleUnlockHell)
	protected("GET /api/character/dungeons", s.handleDungeonAccess)
	protected("GET /api/character/equipment-slots", s.handleEquipmentSlots)
	protected("GET /api/character/inventory", s.handleInventory)
	protected("GET /api/character/mailbox", s.handleMailbox)
	protected("GET /api/character/quests", s.handleQuests)
	protected("GET /api/character/quests/{qid}", s.handleQuestDetail)
	protected("GET /api/character/job", s.handleCharacterJob)
	protected("GET /api/character/detail", s.handleCharacterDetail)
	protected("GET /api/character/raid-counters", s.handleRaidAdmissionCounters)
	protected("POST /api/character/raid-counters/reset", s.handleResetRaidAdmissionCounter)
	protected("POST /api/character/gold", s.handleSetGold)
	protected("POST /api/character/fatigue", s.handleSetFatigue)
	protected("POST /api/character/account-cubes", s.handleSetAccountCubes)
	protected("POST /api/character/delete", s.handleDeleteCharacter)
	protected("POST /api/character/inventory/delete", s.handleInventoryDelete)
	protected("POST /api/character/inventory/clear", s.handleInventoryClear)
	protected("POST /api/character/mailbox/clear", s.handleMailboxClear)
	protected("POST /api/character/mailbox/delete", s.handleMailboxDelete)
	protected("POST /api/character/equipment/unequip", s.handleEquipUnequip)
	protected("POST /api/character/equipment/delete", s.handleEquipDelete)
	protected("POST /api/character/quest/action", s.handleQuestAction)
	protected("POST /api/character/quests/complete-batch", s.handleQuestBatch)

	protected("POST /api/batch/cera", s.handleBatchCera)
	protected("POST /api/batch/black-diamond", s.handleBatchBlackDiamond)
	protected("POST /api/batch/mail", s.handleBatchMail)
	protected("GET /api/batch", s.handleBatchList)
	protected("GET /api/batch/{id}", s.handleBatchGet)

	return s.withRecover(s.withLogging(mux))
}

func (s *webServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessionValid(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok":    false,
				"error": "未登录或登录已过期",
			})
			return
		}
		next(w, r)
	}
}

func (s *webServer) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *webServer) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"ok":    false,
					"error": fmt.Sprintf("服务器内部错误: %v", rec),
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if left := s.loginLockLeft(ip); left > 0 {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("尝试次数过多，请 %d 秒后再试", int(left.Seconds())+1),
		})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.cfg.Password)) != 1 {
		s.recordLoginFailure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "密码错误",
		})
		return
	}
	s.clearLoginFailures(ip)
	expires := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.signSession(expires.Unix()),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"data": map[string]any{
			"title": s.cfg.Title,
		},
	})
}

func (s *webServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *webServer) signSession(expires int64) string {
	payload := strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, []byte(s.cfg.SessionSecret))
	_, _ = mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *webServer) sessionValid(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() >= expires {
		return false
	}
	expected := s.signSession(expires)
	return hmac.Equal([]byte(cookie.Value), []byte(expected))
}

func (s *webServer) loginLockLeft(ip string) time.Duration {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	state := s.loginFails[ip]
	if state == nil {
		return 0
	}
	if time.Now().After(state.until) {
		delete(s.loginFails, ip)
		return 0
	}
	return time.Until(state.until)
}

func (s *webServer) recordLoginFailure(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	state := s.loginFails[ip]
	if state == nil {
		state = &loginState{}
		s.loginFails[ip] = state
	}
	state.count++
	if state.count >= 5 {
		state.until = time.Now().Add(60 * time.Second)
		state.count = 0
	}
}

func (s *webServer) clearLoginFailures(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	delete(s.loginFails, ip)
}

func (s *webServer) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
				forwarded = forwarded[:idx]
			}
			if value := strings.TrimSpace(forwarded); value != "" {
				return value
			}
		}
		if value := strings.TrimSpace(r.Header.Get("X-Real-IP")); value != "" {
			return value
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

func writeRawOK(w http.ResponseWriter, data json.RawMessage) {
	if len(data) == 0 || string(data) == "null" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}{true, data})
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	return dec.Decode(dst)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *webServer) dbRequired(w http.ResponseWriter) bool {
	if s.store == nil {
		fail(w, http.StatusServiceUnavailable, "账号数据库不可用")
		return false
	}
	return true
}

func asString(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		if value {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func asInt64(v any) int64 {
	switch value := v.(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return n
	case json.Number:
		n, _ := value.Int64()
		return n
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	default:
		return 0
	}
}

func asBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(value))
		return parsed
	case json.Number:
		return value.String() != "0"
	case float64:
		return value != 0
	default:
		return false
	}
}

func asStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		if list, ok := v.([]string); ok {
			return list
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if value := asString(item); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func queryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func queryInt64(r *http.Request, name string, fallback int64) int64 {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}
