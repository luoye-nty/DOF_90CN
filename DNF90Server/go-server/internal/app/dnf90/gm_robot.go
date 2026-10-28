package dnf90

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
	"longheng.io/server/internal/services/dnfrobot"
)

const (
	gmRobotsPath              = "/local/dnf-gm/robots"
	gmRobotStatusPath         = "/local/dnf-gm/robots/status"
	gmRobotBatchPath          = "/local/dnf-gm/robots/batch"
	gmRobotOperationsPath     = "/local/dnf-gm/robots/operations"
	gmRobotProfilePath        = "/local/dnf-gm/robots/profile"
	gmRobotProvisionPath      = "/local/dnf-gm/robots/provision"
	gmRobotProvisionBatchPath = "/local/dnf-gm/robots/provision/batch"
	gmRobotStockPath          = "/local/dnf-gm/robots/stock"
	gmRobotAutomationPath     = "/local/dnf-gm/robots/automation"
	gmRobotStartPath          = "/local/dnf-gm/robots/start"
	gmRobotStopPath           = "/local/dnf-gm/robots/stop"
	gmRobotMovePath           = "/local/dnf-gm/robots/move"
	gmRobotChatPath           = "/local/dnf-gm/robots/chat"
	gmRobotStoreOpenPath      = "/local/dnf-gm/robots/store/open"
	gmRobotStoreClosePath     = "/local/dnf-gm/robots/store/close"
)

const gmRobotMaximumProvisionBatch = 64

type gmRobotProvisionBatchRequest struct {
	Count         int    `json:"count"`
	StartIndex    int    `json:"start_index"`
	AccountPrefix string `json:"account_prefix"`
	NamePrefix    string `json:"name_prefix"`
	ProfilePrefix string `json:"profile_prefix"`
	ChannelID     int    `json:"channel_id"`
	AutoStart     bool   `json:"auto_start"`
	Job           int    `json:"job"`
	GrowType      int    `json:"grow_type"`
	Level         int    `json:"level"`
	Village       int    `json:"village"`
	Area          int    `json:"area"`
	X             int    `json:"x"`
	Y             int    `json:"y"`
}

type gmRobotProvisionBatchRow struct {
	Index       int    `json:"index"`
	ProfileID   string `json:"profile_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	CharacterID uint16 `json:"character_id,omitempty"`
	Configured  bool   `json:"configured"`
	Running     bool   `json:"running"`
	Error       string `json:"error,omitempty"`
}

type gmRobotProvisionBatchResult struct {
	Requested int                        `json:"requested"`
	Succeeded int                        `json:"succeeded"`
	Failed    int                        `json:"failed"`
	Rows      []gmRobotProvisionBatchRow `json:"rows"`
}

func registerDNFRobotGMRoutes(env *appkit.Env, bridge *dnfbridge.Service, manager *dnfrobot.Manager) {
	if env == nil || env.AdminMux == nil || env.AdminAuth == nil || bridge == nil || manager == nil {
		return
	}
	auth := func(handler http.HandlerFunc) http.HandlerFunc { return env.AdminAuth("dnf-gm", handler) }
	env.AdminMux.HandleFunc(gmRobotsPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			gmMethodNotAllowed(w, http.MethodGet)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": manager.List()})
	}))
	env.AdminMux.HandleFunc(gmRobotStatusPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			gmMethodNotAllowed(w, http.MethodGet)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": manager.Status()})
	}))
	env.AdminMux.HandleFunc(gmRobotBatchPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfrobot.BatchRequest
		if !decodeGMRequest(w, r, &request, 64<<10) {
			return
		}
		result, err := manager.Batch(r.Context(), request)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot batch action completed", "data": result})
	}))
	env.AdminMux.HandleFunc(gmRobotOperationsPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			gmMethodNotAllowed(w, http.MethodGet)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": manager.Operations()})
	}))
	env.AdminMux.HandleFunc(gmRobotProvisionPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request dnfbridge.GMRobotCharacterProvisionRequest
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		result, err := bridge.GMProvisionRobotCharacter(r.Context(), request)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot character provisioned", "data": result})
	}))
	env.AdminMux.HandleFunc(gmRobotProvisionBatchPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request gmRobotProvisionBatchRequest
		if !decodeGMRequest(w, r, &request, 32<<10) {
			return
		}
		request, err := normalizeGMRobotProvisionBatchRequest(request)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		operation := manager.BeginOperation("provision", fmt.Sprintf("%s count=%d start=%d", request.ProfilePrefix, request.Count, request.StartIndex))
		result := provisionGMRobotBatch(r.Context(), bridge, manager, request)
		manager.CompleteOperation(operation.ID, fmt.Sprintf("requested=%d succeeded=%d failed=%d", result.Requested, result.Succeeded, result.Failed), nil)
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"message": fmt.Sprintf("robot batch provision completed: %d succeeded, %d failed", result.Succeeded, result.Failed),
			"data":    result,
		})
	}))
	env.AdminMux.HandleFunc(gmRobotStockPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID    string                       `json:"id"`
			Items []dnfbridge.GMRobotStockItem `json:"items"`
		}
		if !decodeGMRequest(w, r, &request, 32<<10) {
			return
		}
		snapshot, err := manager.Snapshot(request.ID)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		operation := manager.BeginOperation("stock", snapshot.Profile.ID)
		result, err := bridge.GMStockRobotCharacter(r.Context(), dnfbridge.GMRobotStockRequest{
			AccountID: snapshot.Profile.AccountID, CharacterID: snapshot.Profile.CharacterID, Items: request.Items,
		})
		if err != nil {
			manager.CompleteOperation(operation.ID, "", err)
			writeGMRobotError(w, err)
			return
		}
		manager.CompleteOperation(operation.ID, fmt.Sprintf("items=%d slots=%d", len(result.Items), len(result.Slots)), nil)
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot private-store stock committed", "data": result})
	}))
	env.AdminMux.HandleFunc(gmRobotAutomationPath, auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": manager.Automation()})
		case http.MethodPost:
			var policy dnfrobotdomain.AutomationPolicy
			if !decodeGMRequest(w, r, &policy, 64<<10) {
				return
			}
			result, err := manager.UpdateAutomation(r.Context(), policy)
			if err != nil {
				writeGMRobotError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot automation saved", "data": result})
		default:
			gmMethodNotAllowed(w, "GET, POST")
		}
	}))
	env.AdminMux.HandleFunc(gmRobotProfilePath, auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var profile dnfrobotdomain.Profile
			if !decodeGMRequest(w, r, &profile, 16<<10) {
				return
			}
			result, err := manager.Upsert(r.Context(), profile)
			if err != nil {
				writeGMRobotError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot profile saved", "data": result})
		case http.MethodDelete:
			if err := manager.Delete(r.Context(), strings.TrimSpace(r.URL.Query().Get("id"))); err != nil {
				writeGMRobotError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot profile deleted"})
		default:
			gmMethodNotAllowed(w, "POST, DELETE")
		}
	}))
	env.AdminMux.HandleFunc(gmRobotStartPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		id, ok := decodeRobotID(w, r)
		if !ok {
			return
		}
		result, err := manager.StartProfile(id)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot start requested", "data": result})
	}))
	env.AdminMux.HandleFunc(gmRobotStopPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		id, ok := decodeRobotID(w, r)
		if !ok {
			return
		}
		result, err := manager.StopProfile(id)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot stopped", "data": result})
	}))
	env.AdminMux.HandleFunc(gmRobotMovePath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID       string `json:"id"`
			Village  uint8  `json:"village"`
			Area     uint8  `json:"area"`
			X        uint16 `json:"x"`
			Y        uint16 `json:"y"`
			MoveType uint8  `json:"move_type"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		if err := manager.Move(request.ID, request.Village, request.Area, request.X, request.Y, request.MoveType); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot move packet sent"})
	}))
	env.AdminMux.HandleFunc(gmRobotChatPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID      string `json:"id"`
			Mode    uint8  `json:"mode"`
			Message string `json:"message"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		if err := manager.Chat(request.ID, request.Mode, request.Message); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot chat packet sent"})
	}))
	env.AdminMux.HandleFunc(gmRobotStoreOpenPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Maximum   int    `json:"maximum_rows"`
			UnitPrice uint32 `json:"unit_price"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		if err := manager.OpenPrivateStore(r.Context(), request.ID, request.Title, request.Maximum, request.UnitPrice); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot private store opening requested"})
	}))
	env.AdminMux.HandleFunc(gmRobotStoreClosePath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		id, ok := decodeRobotID(w, r)
		if !ok {
			return
		}
		if err := manager.ClosePrivateStore(id); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot private store close requested"})
	}))
}

func normalizeGMRobotProvisionBatchRequest(request gmRobotProvisionBatchRequest) (gmRobotProvisionBatchRequest, error) {
	request.AccountPrefix = strings.TrimSpace(request.AccountPrefix)
	request.NamePrefix = strings.TrimSpace(request.NamePrefix)
	request.ProfilePrefix = strings.TrimSpace(request.ProfilePrefix)
	if request.Count < 1 || request.Count > gmRobotMaximumProvisionBatch || request.StartIndex < 0 || request.StartIndex > 999999 ||
		request.AccountPrefix == "" || request.NamePrefix == "" || request.ProfilePrefix == "" || request.ChannelID < 1 || request.ChannelID > 65535 {
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: invalid robot provision batch", dnfbridge.ErrGMRequestInvalid)
	}
	if request.StartIndex == 0 {
		request.StartIndex = 1
	}
	last := request.StartIndex + request.Count - 1
	if last > 999999 {
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: robot provision batch index exceeds 999999", dnfbridge.ErrGMRequestInvalid)
	}
	accountID := fmt.Sprintf("%s-%03d", request.AccountPrefix, last)
	profileID := fmt.Sprintf("%s-%03d", request.ProfilePrefix, last)
	if len(accountID) > 96 || len(profileID) > 64 || strings.IndexByte(accountID, 0) >= 0 || strings.IndexByte(profileID, 0) >= 0 {
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: generated robot account/profile id is too long", dnfbridge.ErrGMRequestInvalid)
	}
	return request, nil
}

func provisionGMRobotBatch(ctx context.Context, bridge *dnfbridge.Service, manager *dnfrobot.Manager, request gmRobotProvisionBatchRequest) gmRobotProvisionBatchResult {
	result := gmRobotProvisionBatchResult{Requested: request.Count, Rows: make([]gmRobotProvisionBatchRow, 0, request.Count)}
	for offset := 0; offset < request.Count; offset++ {
		index := request.StartIndex + offset
		row := gmRobotProvisionBatchRow{
			Index:     index,
			ProfileID: fmt.Sprintf("%s-%03d", request.ProfilePrefix, index),
			AccountID: fmt.Sprintf("%s-%03d", request.AccountPrefix, index),
			Name:      fmt.Sprintf("%s%d", request.NamePrefix, index),
		}
		if ctx != nil && ctx.Err() != nil {
			row.Error = ctx.Err().Error()
			result.Failed++
			result.Rows = append(result.Rows, row)
			continue
		}
		provisioned, err := bridge.GMProvisionRobotCharacter(ctx, dnfbridge.GMRobotCharacterProvisionRequest{
			AccountID: row.AccountID,
			Name:      row.Name,
			Job:       request.Job,
			GrowType:  request.GrowType,
			Level:     request.Level,
			Village:   request.Village,
			Area:      request.Area,
			X:         request.X,
			Y:         request.Y,
		})
		if err != nil {
			row.Error = err.Error()
			result.Failed++
			result.Rows = append(result.Rows, row)
			continue
		}
		row.CharacterID = provisioned.CharacterID
		runtime, err := manager.Upsert(ctx, dnfrobotdomain.Profile{
			ID:          row.ProfileID,
			AccountID:   row.AccountID,
			CharacterID: provisioned.CharacterID,
			ChannelID:   request.ChannelID,
			AutoStart:   request.AutoStart,
			Village:     uint8(request.Village),
			Area:        uint8(request.Area),
			X:           uint16(request.X),
			Y:           uint16(request.Y),
		})
		if err != nil {
			row.Error = fmt.Sprintf("character created but profile save failed: %v", err)
			result.Failed++
			result.Rows = append(result.Rows, row)
			continue
		}
		row.Configured = true
		row.Running = runtime.Running
		result.Succeeded++
		result.Rows = append(result.Rows, row)
	}
	return result
}

func decodeRobotID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var request struct {
		ID string `json:"id"`
	}
	if !decodeGMRequest(w, r, &request, 4096) {
		return "", false
	}
	request.ID = strings.TrimSpace(request.ID)
	if request.ID == "" {
		writeGMJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "robot profile id is required"})
		return "", false
	}
	return request.ID, true
}

func writeGMRobotError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, dnfbridge.ErrGMRequestInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, dnfbridge.ErrGMRepositoryUnavailable):
		status = http.StatusServiceUnavailable
	case errors.Is(err, dnfrobotdomain.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, dnfrobot.ErrProfileNotFound):
		status = http.StatusNotFound
	case errors.Is(err, dnfrobotdomain.ErrUnavailable), errors.Is(err, dnfrobot.ErrManagerStopped):
		status = http.StatusServiceUnavailable
	case errors.Is(err, dnfrobot.ErrNotReady):
		status = http.StatusConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	}
	writeGMJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}
