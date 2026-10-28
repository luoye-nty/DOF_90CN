// online_report.go pushes the real online account count to the ops tool's
// loopback /internal/online-report endpoint on a fixed interval. The ops tool
// uses that count both for /launcher/v1/health display and for the FREE
// edition's hard online limit, so the game server must be the authority.
package dnfbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// onlineReportMaxBody bounds the report response body.
const onlineReportMaxBody = 4 << 10

// countOnlineAccounts returns the number of distinct non-empty account IDs
// that currently hold an online seat. onlineSeats is the authoritative seat
// table guarded by s.mu: a connection takes its seat as soon as it is bound
// to an account (launcher verify OK or a local PID/WireGuard/Wine binding),
// before it selects a character, and keeps it until the connection closes.
// Duplicates (same account on several characters/sockets) collapse into one
// seat. Counting connecting/role-select connections here is deliberate: the
// capacity gate must treat an in-progress login as occupying the server, so
// a full server rejects the next connection instead of overselling a seat.
func (s *Service) countOnlineAccounts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countOnlineAccountsLocked()
}

func (s *Service) countOnlineAccountsLocked() int {
	seen := make(map[string]struct{}, len(s.onlineSeats))
	for session := range s.onlineSeats {
		if session == nil {
			continue
		}
		id := strings.TrimSpace(session.accountID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	return len(seen)
}

// occupyOnlineSeat records an account-bound connection as occupying one
// online seat and nudges the heartbeat so capacity decisions see the change
// immediately. It is a no-op for seats without an account or already seated.
func (s *Service) occupyOnlineSeat(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	if strings.TrimSpace(session.accountID) == "" {
		return
	}
	s.mu.Lock()
	if s.onlineSeats == nil {
		s.onlineSeats = make(map[*gameSession]struct{})
	}
	if _, ok := s.onlineSeats[session]; ok {
		s.mu.Unlock()
		return
	}
	s.onlineSeats[session] = struct{}{}
	s.mu.Unlock()
	s.notifyOnlineChanged()
}

// releaseOnlineSeat removes an account-bound connection from the seat table
// when its transport closes, freeing its capacity slot immediately. Safe to
// call for sessions that never took a seat.
func (s *Service) releaseOnlineSeat(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	s.mu.Lock()
	if _, ok := s.onlineSeats[session]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.onlineSeats, session)
	s.mu.Unlock()
	s.notifyOnlineChanged()
}

// notifyOnlineChanged nudges the heartbeat goroutine to push the seat count
// right away instead of waiting for the next interval tick. The non-blocking
// send collapses bursts of bind/unbind events into a single report.
func (s *Service) notifyOnlineChanged() {
	trigger := s.onlineReportTrigger
	if trigger == nil {
		return
	}
	select {
	case trigger <- struct{}{}:
	default:
	}
}

// launcherOnlineReportURL derives the report endpoint from the verify URL by
// replacing the trailing path segment. Both live on the same loopback port.
func launcherOnlineReportURL(verifyURL string) string {
	verifyURL = strings.TrimSpace(verifyURL)
	if verifyURL == "" {
		verifyURL = defaultLauncherAuthVerifyURL
	}
	slash := strings.LastIndexByte(verifyURL, '/')
	if slash < 0 {
		return verifyURL + "/internal/online-report"
	}
	return verifyURL[:slash] + "/online-report"
}

// startLauncherOnlineReport launches the heartbeat loop; it is a no-op when
// the report is disabled by configuration.
func (s *Service) startLauncherOnlineReport(ctx context.Context) {
	if !s.options.launcherOnlineReportEnabled {
		return
	}
	interval := s.options.launcherOnlineReportInterval
	if interval <= 0 {
		interval = defaultLauncherOnlineReportInterval
	}
	go s.runLauncherOnlineReport(ctx, interval)
}

func (s *Service) runLauncherOnlineReport(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	reportURL := launcherOnlineReportURL(s.options.launcherAuthVerifyURL)
	s.reportOnlineOnce(ctx, reportURL)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.onlineReportTrigger:
			// Event-driven nudge: a connection occupied or released a seat.
			// Push immediately so capacity decisions never lag behind a full
			// tick window after a login or disconnect.
			s.reportOnlineOnce(ctx, reportURL)
		case <-ticker.C:
			s.reportOnlineOnce(ctx, reportURL)
		}
	}
}

func (s *Service) reportOnlineOnce(ctx context.Context, reportURL string) {
	payload, err := json.Marshal(map[string]any{
		"online": s.countOnlineAccounts(),
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reportURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(s.options.launcherAuthInternalToken); token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	client := s.launcherAuthHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logWarn("dnfbridge online report request failed", "url", reportURL, "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, onlineReportMaxBody))
	if resp.StatusCode != http.StatusOK {
		s.logWarn("dnfbridge online report rejected", "url", reportURL, "status", resp.StatusCode)
	}
}
