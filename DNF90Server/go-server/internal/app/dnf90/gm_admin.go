package dnf90

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
)

const (
	gmCharactersPath            = "/local/dnf-gm/characters"
	gmSystemMailPath            = "/local/dnf-gm/mail"
	gmMailboxClearPath          = "/local/dnf-gm/mail/clear"
	gmWalletQueryPath           = "/local/dnf-gm/wallet/query"
	gmWalletUpdatePath          = "/local/dnf-gm/wallet/update"
	gmWalletRepairHappyPath     = "/local/dnf-gm/wallet/repair-happy-token-cera"
	gmCharacterSkillPath        = "/local/dnf-gm/character/sptp"
	gmCharacterLevelPath        = "/local/dnf-gm/character/level"
	gmCharacterJobPath          = "/local/dnf-gm/character/job"
	gmCharacterBlackDiamondPath = "/local/dnf-gm/character/black-diamond"
	gmDungeonsUnlockAllPath     = "/local/dnf-gm/dungeons/unlock-all"
	gmEquipSlotsUnlockAllPath   = "/local/dnf-gm/equip-slots/unlock-all"
	gmKickSessionsPath          = "/local/dnf-gm/kick-sessions"
)

func registerDNFGMRoutes(env *appkit.Env, bridge *dnfbridge.Service) {
	if env == nil || bridge == nil || env.AdminMux == nil || env.AdminAuth == nil ||
		!strings.EqualFold(env.Core().Config.Service.Environment, "local") {
		return
	}
	env.AdminMux.HandleFunc(gmCharactersPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			writeGMJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		accountID := strings.TrimSpace(r.URL.Query().Get("account_id"))
		characterName := strings.TrimSpace(r.URL.Query().Get("character_name"))
		username := strings.TrimSpace(r.URL.Query().Get("username"))
		var characters []dnfbridge.GMCharacterInfo
		var err error
		if username != "" {
			if accountID != "" || characterName != "" {
				writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "only one character selector is allowed"})
				return
			}
			characters, err = bridge.GMFindCharactersByUsername(ctx, username)
		} else if accountID != "" || characterName != "" {
			characters, err = bridge.GMFindCharacters(ctx, accountID, characterName)
		} else {
			characters, err = bridge.GMCharacters(ctx)
		}
		if err != nil {
			writeGMError(w, err)
			return
		}
		message := "character list loaded"
		if username != "" || accountID != "" || characterName != "" {
			message = "character query completed"
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": characters})
	}))
	env.AdminMux.HandleFunc(gmSystemMailPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeGMJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
			return
		}
		request := dnfbridge.GMSystemMailRequest{}
		if !decodeGMRequest(w, r, &request, 64<<10) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		result, err := bridge.GMSendSystemMail(ctx, request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		message := "mail committed"
		if result.Refreshed {
			message = "mail committed and online mailbox refreshed"
		} else if result.Online {
			message = "mail committed; online refresh will retry on next mailbox open"
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
	}))
	env.AdminMux.HandleFunc(gmMailboxClearPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		request := dnfbridge.GMMailboxClearRequest{}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		result, err := bridge.GMClearMailbox(ctx, request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		message := "mailbox cleared"
		if result.Refreshed {
			message = "mailbox cleared and online client refreshed"
		} else if result.Online {
			message = "mailbox cleared; online refresh will retry on next mailbox open"
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
	}))
	registerDNFGMWalletRoutes(env, bridge)
	registerDNFGMCharacterRoutes(env, bridge)
	registerDNFGKickRoutes(env, bridge)
}

// registerDNFGKickRoutes exposes the ban-driven kick endpoint used by the
// launcher ops tool: after persisting a ban it POSTs the same ban target here
// so every matching online session is disconnected immediately.
func registerDNFGKickRoutes(env *appkit.Env, bridge *dnfbridge.Service) {
	env.AdminMux.HandleFunc(gmKickSessionsPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			BanType string `json:"ban_type"`
			BanValue string `json:"ban_value"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		var banType dnfbridge.KickBanType
		switch strings.ToLower(strings.TrimSpace(request.BanType)) {
		case string(dnfbridge.KickBanAccount):
			banType = dnfbridge.KickBanAccount
		case string(dnfbridge.KickBanIP):
			banType = dnfbridge.KickBanIP
		case string(dnfbridge.KickBanMachine):
			banType = dnfbridge.KickBanMachine
		default:
			writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid ban_type"})
			return
		}
		if strings.TrimSpace(request.BanValue) == "" {
			writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "ban_value required"})
			return
		}
		kicked, err := bridge.KickSessions(banType, request.BanValue)
		if err != nil {
			writeGMError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"message": "kick request completed",
			"kicked":  kicked,
		})
	}))
}

func registerDNFGMWalletRoutes(env *appkit.Env, bridge *dnfbridge.Service) {
	env.AdminMux.HandleFunc(gmWalletQueryPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfbridge.GMWalletRequest
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMReadWallet(r.Context(), request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "wallet loaded", "data": result})
	}))
	env.AdminMux.HandleFunc(gmWalletUpdatePath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfbridge.GMWalletRequest
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMUpdateWallet(r.Context(), request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		message := "wallet committed"
		if result.Refreshed {
			message = "wallet committed and online client refreshed"
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
	}))
	env.AdminMux.HandleFunc(gmWalletRepairHappyPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfbridge.GMWalletRequest
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMRepairHappyTokenCeraVoucher(r.Context(), request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		message := "happy-token Cera voucher repair completed"
		if result.Refreshed {
			message = "happy-token Cera voucher repaired and online client refreshed"
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
	}))
}

func registerDNFGMCharacterRoutes(env *appkit.Env, bridge *dnfbridge.Service) {
	env.AdminMux.HandleFunc(gmCharacterSkillPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			result, err := bridge.GMReadCharacterSkill(r.Context(), strings.TrimSpace(r.URL.Query().Get("id")))
			if err != nil {
				writeGMError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": result})
		case http.MethodPost:
			var request dnfbridge.GMCharacterSkillUpdate
			if !decodeGMRequest(w, r, &request, 16<<10) {
				return
			}
			result, err := bridge.GMUpdateCharacterSkill(r.Context(), request)
			if err != nil {
				writeGMError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": gmNativeStateMessage("skill points committed", result.GMNativeRefreshResult), "data": result})
		default:
			gmMethodNotAllowed(w, "GET, POST")
		}
	}))
	env.AdminMux.HandleFunc(gmCharacterLevelPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			CharacterID string `json:"character_id"`
			Level       int    `json:"level"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMSetCharacterLevel(r.Context(), request.CharacterID, request.Level)
		if err != nil {
			writeGMError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": gmNativeStateMessage("character level committed", result.GMNativeRefreshResult), "data": result})
	}))
	env.AdminMux.HandleFunc(gmCharacterJobPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			result, err := bridge.GMReadCharacterJob(r.Context(), strings.TrimSpace(r.URL.Query().Get("id")))
			if err != nil {
				writeGMError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": result})
		case http.MethodPost:
			var request dnfbridge.GMCharacterJobUpdate
			if !decodeGMRequest(w, r, &request, 16<<10) {
				return
			}
			result, err := bridge.GMSetCharacterJob(r.Context(), request)
			if err != nil {
				writeGMError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": gmNativeStateMessage("character profession committed", result.GMNativeRefreshResult), "data": result})
		default:
			gmMethodNotAllowed(w, "GET, POST")
		}
	}))
	env.AdminMux.HandleFunc(gmCharacterBlackDiamondPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// 只读查询：?id=<character_id> 或 ?name=<角色名>，不修改任何数据。
			result, err := bridge.GMBlackDiamondStatus(r.Context(), dnfbridge.GMBlackDiamondQuery{
				CharacterID:   strings.TrimSpace(r.URL.Query().Get("id")),
				CharacterName: strings.TrimSpace(r.URL.Query().Get("name")),
			})
			if err != nil {
				writeGMError(w, err)
				return
			}
			message := "Black Diamond expired"
			switch {
			case result.Active && result.Online:
				message = "Black Diamond active (character online)"
			case result.Active:
				message = "Black Diamond active"
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
		case http.MethodPost:
			var request dnfbridge.GMBlackDiamondRequest
			if !decodeGMRequest(w, r, &request, 16<<10) {
				return
			}
			result, err := bridge.GMActivateBlackDiamond(r.Context(), request)
			if err != nil {
				writeGMError(w, err)
				return
			}
			message := "Black Diamond entitlement committed; character is offline and will load it on next login"
			switch {
			case result.Deferred:
				message = "Black Diamond entitlement committed; online refresh is deferred until town"
			case result.Refreshed:
				message = "Black Diamond entitlement committed and native online client packets sent"
			case result.Online:
				message = "Black Diamond entitlement committed; native online client packet refresh failed"
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message, "data": result})
		default:
			gmMethodNotAllowed(w, "GET, POST")
		}
	}))
	env.AdminMux.HandleFunc(gmDungeonsUnlockAllPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfbridge.GMDungeonUnlockRequest
		if !decodeGMRequest(w, r, &request, 128<<10) {
			return
		}
		result, err := bridge.GMUnlockDungeons(r.Context(), request)
		if err != nil {
			writeGMError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": gmNativeStateMessage("dungeon permissions committed", result.GMNativeRefreshResult), "data": result})
	}))
	env.AdminMux.HandleFunc(gmEquipSlotsUnlockAllPath, env.AdminAuth("dnf-gm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			CharacterID string `json:"character_id"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMUnlockEquipSlots(r.Context(), request.CharacterID)
		if err != nil {
			writeGMError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": gmNativeStateMessage("equipment slots committed", result.GMNativeRefreshResult), "data": result})
	}))
}

func gmNativeStateMessage(committed string, result dnfbridge.GMNativeRefreshResult) string {
	if result.Refreshed {
		return committed + " and native online client packets sent"
	}
	if result.Online {
		return committed + "; native online client packet refresh failed"
	}
	return committed + "; character is offline and will load it on next login"
}

func decodeGMRequest(w http.ResponseWriter, r *http.Request, target any, maxBytes int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request: " + err.Error()})
		return false
	}
	if err := rejectTrailingGMJSON(decoder); err != nil {
		writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request: " + err.Error()})
		return false
	}
	return true
}

func gmMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeGMJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
}

func rejectTrailingGMJSON(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func writeGMError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, dnfbridge.ErrGMRequestInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, dnfbridge.ErrGMCharacterNotFound):
		status = http.StatusNotFound
	case errors.Is(err, dnfbridge.ErrGMRepositoryUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status = http.StatusGatewayTimeout
	}
	writeGMJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func writeGMJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
