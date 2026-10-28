package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxOnline is the build-time online capacity limit for the FREE edition.
// It is injected via ldflags (-X dnf90ops/internal/server.MaxOnline=8).
// 0 means unlimited and is the PAID-edition default, so paid builds keep the
// stock ldflags value and behave exactly like today.
var MaxOnline = "0"

// onlineCapacityState keeps the latest online count reported by dnfbridge via
// the loopback /internal/online-report heartbeat.
type onlineCapacityState struct {
	mu        sync.Mutex
	online    int
	updatedAt time.Time
}

var capacityState onlineCapacityState

// reportStaleAfter is the window after which a heartbeat is considered stale.
const reportStaleAfter = 90 * time.Second

func configuredMaxOnline() int {
	if v, err := strconv.Atoi(strings.TrimSpace(MaxOnline)); err == nil && v > 0 {
		return v
	}
	return 0
}

func currentReportedOnline() int {
	capacityState.mu.Lock()
	defer capacityState.mu.Unlock()
	if time.Since(capacityState.updatedAt) > reportStaleAfter {
		return 0
	}
	return capacityState.online
}

// handleOnlineReport accepts the dnfbridge heartbeat:
//
//	POST /internal/online-report
//	X-Internal-Token: <token>
//	{"online": 7}
func (s *Service) handleOnlineReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false})
		return
	}
	token := s.cfg.Internal.Token
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(strings.TrimSpace(r.Header.Get("X-Internal-Token")))) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false})
		return
	}
	var req struct {
		Online int `json:"online"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Online < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false})
		return
	}
	capacityState.mu.Lock()
	capacityState.online = req.Online
	capacityState.updatedAt = time.Now()
	capacityState.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// rejectByOnlineCapacity returns a non-empty deny reason when the online
// capacity limit is reached. reqOnline is the online count the game server
// observed at connection time; the fresher heartbeat value wins when present.
func (s *Service) rejectByOnlineCapacity(reqOnline int) string {
	limit := configuredMaxOnline()
	if limit <= 0 {
		return ""
	}
	effective := reqOnline
	if reported := currentReportedOnline(); reported > effective {
		effective = reported
	}
	if effective >= limit {
		return "server-full"
	}
	return ""
}
