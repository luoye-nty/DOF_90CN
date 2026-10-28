package dnfbridge

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
)

const (
	PaymentWebOrdinaryPath     = "/comm-htdocs/pay/client/dnf/"
	PaymentWebBlackDiamondPath = "/comm-htdocs/pay/client/dnf/diamond/"
	currentPremiumWebTicketTTL = 2 * time.Minute
	currentPremiumWebResultTTL = 5 * time.Minute
	currentBlackDiamondTerm    = 30 * 24 * time.Hour
	currentCeraTicketMaxURL    = 63
)

type currentPremiumWebPortal struct {
	baseURL string
	random  io.Reader
	mu      sync.Mutex
	tickets map[string]*currentPremiumWebTicket
}

type currentPremiumWebTicket struct {
	token               string
	remoteIP            string
	accountID           string
	characterID         uint16
	characterGeneration uint64
	session             *gameSession
	expiresAt           time.Time
	completed           bool
	activating          bool
	endsAt              time.Time
}

type currentPremiumWebPage struct {
	Title, Heading, Message, Action, Button, Token string
	ShowButton, Success                            bool
}

var currentPremiumWebTemplate = template.Must(template.New("dnf-black-diamond").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta http-equiv="Cache-Control" content="no-store">
<title>{{.Title}}</title><style>
html,body{margin:0;min-height:100%;background:#f3f5f7;color:#20242a;font-family:"Microsoft YaHei",sans-serif}
main{max-width:720px;margin:0 auto;padding:52px 40px}h1{margin:0 0 18px;font-size:28px;font-weight:600}
p{margin:0 0 28px;color:#59616c;font-size:15px;line-height:1.8}button{min-width:180px;height:44px;border:0;border-radius:4px;background:#1677c8;color:#fff;font-size:16px;cursor:pointer}.success{color:#177245}
</style></head><body><main><h1{{if .Success}} class="success"{{end}}>{{.Heading}}</h1><p>{{.Message}}</p>
{{if .ShowButton}}<form method="{{if .Token}}post{{else}}get{{end}}" action="{{.Action}}">{{if .Token}}<input type="hidden" name="ticket" value="{{.Token}}">{{end}}<button type="submit">{{.Button}}</button></form>{{end}}
</main></body></html>`))

func (s *Service) ConfigurePaymentWeb(baseURL string) error {
	if s == nil {
		return errors.New("black diamond payment Web service is nil")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("black diamond payment Web base URL %q is invalid", baseURL)
	}
	if len(baseURL+PaymentWebOrdinaryPath) > currentCeraTicketMaxURL {
		return fmt.Errorf("black diamond payment Web URL is longer than %d bytes", currentCeraTicketMaxURL)
	}
	s.premiumWeb = &currentPremiumWebPortal{
		baseURL: baseURL,
		random:  cryptorand.Reader,
		tickets: make(map[string]*currentPremiumWebTicket),
	}
	return nil
}

func (s *Service) PaymentWebHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PaymentWebOrdinaryPath, s.serveCurrentPremiumWebRoot)
	mux.HandleFunc(PaymentWebBlackDiamondPath, s.serveCurrentBlackDiamondWeb)
	return mux
}

func (s *Service) handleCurrentCeraTicket(session *gameSession, body []byte) error {
	if len(body) != 0 {
		return fmt.Errorf("cera ticket generation body length=%d, want 0", len(body))
	}
	if s == nil || s.premiumWeb == nil || session == nil || session.selectedCharacterID == 0 ||
		session.townSceneReadyCharacterID != session.selectedCharacterID {
		return nil
	}
	session.dungeon.mu.Lock()
	inDungeon := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	if inDungeon {
		return nil
	}
	if err := s.premiumWeb.issue(s, session, s.gameplayNow().UTC()); err != nil {
		return err
	}
	response, err := buildCurrentCeraTicketSuccess(s.premiumWeb.baseURL+PaymentWebOrdinaryPath, 0)
	if err != nil {
		s.premiumWeb.revoke(session)
		return err
	}
	if err := s.sendGameUpperRawClass(session, 65, response, 1); err != nil {
		s.premiumWeb.revoke(session)
		return err
	}
	s.logGameEvent(session, "game-cera-ticket-black-diamond-web-sent",
		"char_id", session.selectedCharacterID,
		"web_origin", s.premiumWeb.baseURL,
		"wire_type", premium.TypeBlackDiamondWire)
	return nil
}

func buildCurrentCeraTicketSuccess(webURL string, opaque uint32) ([]byte, error) {
	if len(webURL) == 0 || len(webURL) > currentCeraTicketMaxURL {
		return nil, fmt.Errorf("cera ticket URL length=%d", len(webURL))
	}
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint32(uint32(len(webURL)))
	writer.writeBytes([]byte(webURL))
	writer.writeUint32(opaque)
	return writer.bytes(), nil
}

func (p *currentPremiumWebPortal) issue(service *Service, session *gameSession, now time.Time) error {
	if p == nil || service == nil || session == nil || session.conn == nil || session.selectedCharacterID == 0 {
		return errors.New("black diamond Web session is not authenticated")
	}
	accountID := strings.TrimSpace(service.accountIDForSession(session))
	if accountID == "" {
		return errors.New("black diamond Web account is not authenticated")
	}
	remoteIP, err := normalizedCurrentPremiumRemoteIP(session.conn.RemoteAddr().String())
	if err != nil {
		return err
	}
	var random [32]byte
	if _, err := io.ReadFull(p.random, random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	for key, ticket := range p.tickets {
		if ticket.session == session {
			delete(p.tickets, key)
		}
	}
	p.tickets[token] = &currentPremiumWebTicket{
		token: token, remoteIP: remoteIP,
		accountID:           accountID,
		characterID:         session.selectedCharacterID,
		characterGeneration: session.characterGeneration,
		session:             session, expiresAt: now.Add(currentPremiumWebTicketTTL),
	}
	return nil
}

func (p *currentPremiumWebPortal) pruneLocked(now time.Time) {
	for token, ticket := range p.tickets {
		if ticket == nil || !ticket.expiresAt.After(now) {
			delete(p.tickets, token)
		}
	}
}

func (p *currentPremiumWebPortal) revoke(session *gameSession) {
	if p == nil || session == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for token, ticket := range p.tickets {
		if ticket.session == session {
			delete(p.tickets, token)
		}
	}
}

func (s *Service) disconnectCurrentPremiumWeb(session *gameSession) {
	if s != nil && s.premiumWeb != nil {
		s.premiumWeb.revoke(session)
	}
}

func (p *currentPremiumWebPortal) ticketForRemote(remoteIP string, now time.Time) (*currentPremiumWebTicket, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	loopback := net.ParseIP(remoteIP).IsLoopback()
	var found *currentPremiumWebTicket
	for _, ticket := range p.tickets {
		if ticket.remoteIP != remoteIP && !loopback {
			continue
		}
		if found != nil {
			return nil, errors.New("multiple Black Diamond Web sessions share this address")
		}
		found = ticket
	}
	if found == nil {
		return nil, errors.New("Black Diamond Web session was not found")
	}
	snapshot := *found
	return &snapshot, nil
}

func (p *currentPremiumWebPortal) ticketByToken(token, remoteIP string, now time.Time) (*currentPremiumWebTicket, error) {
	if len(token) != 64 {
		return nil, errors.New("Black Diamond Web ticket is malformed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneLocked(now)
	ticket := p.tickets[token]
	if ticket == nil || ticket.remoteIP != remoteIP && !net.ParseIP(remoteIP).IsLoopback() {
		return nil, errors.New("Black Diamond Web ticket is invalid")
	}
	return ticket, nil
}

func (s *Service) serveCurrentPremiumWebRoot(w http.ResponseWriter, r *http.Request) {
	setCurrentPremiumWebHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderCurrentPremiumWebPage(w, http.StatusOK, currentPremiumWebPage{
		Title: "充值中心", Heading: "点券充值", Message: "请选择当前可用的会员服务。",
		Action: PaymentWebBlackDiamondPath, Button: "黑钻会员", ShowButton: true,
	})
}

func (s *Service) serveCurrentBlackDiamondWeb(w http.ResponseWriter, r *http.Request) {
	setCurrentPremiumWebHeaders(w)
	if s == nil || s.premiumWeb == nil {
		s.renderCurrentPremiumWebPage(w, http.StatusServiceUnavailable, currentPremiumWebPage{Title: "黑钻会员", Heading: "服务暂不可用", Message: "会员服务尚未就绪。"})
		return
	}
	remoteIP, err := normalizedCurrentPremiumRemoteIP(r.RemoteAddr)
	if err != nil {
		s.renderCurrentPremiumWebPage(w, http.StatusBadRequest, currentPremiumWebPage{Title: "黑钻会员", Heading: "请求无效", Message: "无法确认当前连接。"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		ticket, err := s.premiumWeb.ticketForRemote(remoteIP, s.gameplayNow().UTC())
		if err != nil {
			s.renderCurrentPremiumWebPage(w, http.StatusConflict, currentPremiumWebPage{Title: "黑钻会员", Heading: "会话已失效", Message: "请从游戏内重新打开会员页面。"})
			return
		}
		if ticket.completed {
			s.renderCurrentPremiumWebPage(w, http.StatusOK, currentPremiumWebPage{Title: "黑钻会员", Heading: "开通成功", Message: "黑钻会员已生效。", Success: true})
			return
		}
		s.renderCurrentPremiumWebPage(w, http.StatusOK, currentPremiumWebPage{
			Title: "黑钻会员", Heading: "黑钻会员", Message: "开通后将为当前账号增加 30 天黑钻会员。",
			Action: PaymentWebBlackDiamondPath, Button: "立即开通", Token: ticket.token, ShowButton: true,
		})
	case http.MethodPost:
		s.activateCurrentBlackDiamondWeb(w, r, remoteIP)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) activateCurrentBlackDiamondWeb(w http.ResponseWriter, r *http.Request, remoteIP string) {
	r.Body = http.MaxBytesReader(w, r.Body, 2048)
	if err := r.ParseForm(); err != nil {
		s.renderCurrentPremiumWebPage(w, http.StatusBadRequest, currentPremiumWebPage{Title: "黑钻会员", Heading: "请求无效", Message: "提交内容无法识别。"})
		return
	}
	ticket, err := s.premiumWeb.ticketByToken(strings.TrimSpace(r.PostFormValue("ticket")), remoteIP, s.gameplayNow().UTC())
	if err != nil {
		s.renderCurrentPremiumWebPage(w, http.StatusConflict, currentPremiumWebPage{Title: "黑钻会员", Heading: "会话已失效", Message: "请从游戏内重新打开会员页面。"})
		return
	}
	s.premiumWeb.mu.Lock()
	if ticket.completed {
		s.premiumWeb.mu.Unlock()
		s.renderCurrentPremiumWebPage(w, http.StatusOK, currentPremiumWebPage{Title: "黑钻会员", Heading: "开通成功", Message: "黑钻会员已生效。", Success: true})
		return
	}
	if ticket.activating {
		s.premiumWeb.mu.Unlock()
		s.renderCurrentPremiumWebPage(w, http.StatusConflict, currentPremiumWebPage{Title: "黑钻会员", Heading: "正在处理", Message: "请稍候刷新页面。"})
		return
	}
	ticket.activating = true
	s.premiumWeb.mu.Unlock()

	result, err := s.activateCurrentBlackDiamondEntitlement(r.Context(), ticket)
	s.premiumWeb.mu.Lock()
	ticket.activating = false
	if err == nil {
		ticket.completed = true
		ticket.endsAt = result.ExpiresAt
		ticket.expiresAt = s.gameplayNow().UTC().Add(currentPremiumWebResultTTL)
	}
	s.premiumWeb.mu.Unlock()
	if err != nil {
		s.logWarn("black diamond Web activation failed", "account", ticket.accountID, "character", ticket.characterID, "error", err)
		s.renderCurrentPremiumWebPage(w, http.StatusInternalServerError, currentPremiumWebPage{Title: "黑钻会员", Heading: "开通失败", Message: "会员权益暂未写入，请稍后重试。"})
		return
	}
	s.notifyCurrentBlackDiamondActivation(ticket, result)
	s.renderCurrentPremiumWebPage(w, http.StatusOK, currentPremiumWebPage{Title: "黑钻会员", Heading: "开通成功", Message: "黑钻会员已生效。", Success: true})
}

func (s *Service) activateCurrentBlackDiamondEntitlement(ctx context.Context, ticket *currentPremiumWebTicket) (currentBlackDiamondSelectionResult, error) {
	if s == nil || ticket == nil || ticket.accountID == "" || ticket.characterID == 0 ||
		ticket.session == nil || ticket.session.selectedCharacterID != ticket.characterID ||
		ticket.session.characterGeneration != ticket.characterGeneration {
		return currentBlackDiamondSelectionResult{}, errors.New("Black Diamond game session changed")
	}
	return s.activateBlackDiamondForCharacter(
		ctx,
		ticket.accountID,
		fmt.Sprintf("%d", ticket.characterID),
		currentBlackDiamondTerm,
	)
}

func (s *Service) notifyCurrentBlackDiamondActivation(ticket *currentPremiumWebTicket, result currentBlackDiamondSelectionResult) {
	if ticket == nil || ticket.session == nil || !result.Found {
		return
	}
	session := ticket.session
	if session.selectedCharacterID != ticket.characterID || session.characterGeneration != ticket.characterGeneration {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	_, err := s.sendCurrentBlackDiamondActivationState(ctx, session, result.ExpiresAt, "payment_web_activation")
	cancel()
	if err != nil {
		s.logWarn("black diamond live refresh failed", "character", ticket.characterID, "error", err)
	}
}

func (s *Service) renderCurrentPremiumWebPage(w http.ResponseWriter, status int, page currentPremiumWebPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = currentPremiumWebTemplate.Execute(w, page)
}

func setCurrentPremiumWebHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
}

func normalizedCurrentPremiumRemoteIP(address string) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", errors.New("remote host is not an IP address")
	}
	return ip.String(), nil
}
