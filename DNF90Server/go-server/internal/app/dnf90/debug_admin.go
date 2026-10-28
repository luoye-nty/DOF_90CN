package dnf90

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
)

type debugDungeonRequest struct {
	CharacterID uint16 `json:"character_id,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	ClientPID   uint32 `json:"client_pid,omitempty"`
	NextX       *int   `json:"next_x,omitempty"`
	NextY       *int   `json:"next_y,omitempty"`
}

func registerDNFDebugRoutes(env *appkit.Env, bridge *dnfbridge.Service) {
	if env == nil || bridge == nil || env.AdminMux == nil || env.AdminAuth == nil ||
		!strings.EqualFold(env.Core().Config.Service.Environment, "local") {
		return
	}

	env.AdminMux.HandleFunc("/local/dnf-debug/sessions", env.AdminAuth("dnf-debug", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			writeDebugJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		writeDebugJSON(w, http.StatusOK, map[string]any{
			"environment": "local",
			"sessions":    bridge.DebugSessionsSnapshot(),
		})
	}))

	env.AdminMux.HandleFunc("/local/dnf-debug/kill-current-room", env.AdminAuth("dnf-debug", debugActionHandler(bridge, "kill")))
	env.AdminMux.HandleFunc("/local/dnf-debug/move-room", env.AdminAuth("dnf-debug", debugActionHandler(bridge, "move")))
}

func debugActionHandler(bridge *dnfbridge.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeDebugJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		request, err := decodeDebugDungeonRequest(w, r)
		if err != nil {
			writeDebugJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		characterID, err := bridge.DebugResolveCharacterID(request.CharacterID, request.AccountID, request.ClientPID)
		if err != nil {
			writeDebugJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		var result dnfbridge.DebugDungeonResult
		switch action {
		case "kill":
			result, err = bridge.DebugKillCurrentRoom(ctx, characterID)
		case "move":
			if request.NextX == nil || request.NextY == nil || *request.NextX < 0 || *request.NextX > 255 || *request.NextY < 0 || *request.NextY > 255 {
				writeDebugJSON(w, http.StatusBadRequest, map[string]string{"error": "next_x and next_y must be bytes"})
				return
			}
			result, err = bridge.DebugMoveCurrentRoom(ctx, characterID, byte(*request.NextX), byte(*request.NextY))
		default:
			writeDebugJSON(w, http.StatusNotFound, map[string]string{"error": "unknown debug action"})
			return
		}
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				status = http.StatusGatewayTimeout
			}
			writeDebugJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeDebugJSON(w, http.StatusOK, result)
	}
}

func decodeDebugDungeonRequest(w http.ResponseWriter, r *http.Request) (debugDungeonRequest, error) {
	request := debugDungeonRequest{}
	if r.Body != nil {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			return debugDungeonRequest{}, fmt.Errorf("invalid request: %w", err)
		}
	}
	if request.CharacterID == 0 {
		request.CharacterID = uint16(queryUint(r, "character_id"))
	}
	if request.AccountID == "" {
		request.AccountID = strings.TrimSpace(r.URL.Query().Get("account_id"))
	}
	if request.ClientPID == 0 {
		request.ClientPID = uint32(queryUint(r, "client_pid"))
	}
	if request.NextX == nil {
		if value, ok := queryInt(r, "next_x"); ok {
			request.NextX = &value
		}
	}
	if request.NextY == nil {
		if value, ok := queryInt(r, "next_y"); ok {
			request.NextY = &value
		}
	}
	return request, nil
}

func queryUint(r *http.Request, key string) uint64 {
	value, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get(key)), 10, 32)
	if err != nil {
		return 0
	}
	return value
}

func queryInt(r *http.Request, key string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	return value, err == nil
}

func writeDebugJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
