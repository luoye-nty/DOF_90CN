package dnf90

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
	appkit "longheng.io/server/internal/platform/app"
	"longheng.io/server/internal/services/dnfbridge"
	"longheng.io/server/internal/services/dnfrobot"
)

const (
	gmRobotsPath                  = "/local/dnf-gm/robots"
	gmRobotStatusPath             = "/local/dnf-gm/robots/status"
	gmRobotBatchPath              = "/local/dnf-gm/robots/batch"
	gmRobotOperationsPath         = "/local/dnf-gm/robots/operations"
	gmRobotProfilePath            = "/local/dnf-gm/robots/profile"
	gmRobotProvisionPath          = "/local/dnf-gm/robots/provision"
	gmRobotProvisionBatchPath     = "/local/dnf-gm/robots/provision/batch"
	gmRobotStockPath              = "/local/dnf-gm/robots/stock"
	gmRobotAutomationPath         = "/local/dnf-gm/robots/automation"
	gmRobotSpeechPath             = "/local/dnf-gm/robots/speech"
	gmRobotRecyclePath            = "/local/dnf-gm/robots/recycle"
	gmRobotStartPath              = "/local/dnf-gm/robots/start"
	gmRobotStopPath               = "/local/dnf-gm/robots/stop"
	gmRobotMovePath               = "/local/dnf-gm/robots/move"
	gmRobotRelocatePath           = "/local/dnf-gm/robots/relocate"
	gmRobotChatPath               = "/local/dnf-gm/robots/chat"
	gmRobotStoreOpenPath          = "/local/dnf-gm/robots/store/open"
	gmRobotStoreClosePath         = "/local/dnf-gm/robots/store/close"
	gmRobotPrivateStoreConfigPath = "/local/dnf-gm/robots/private-store/config"
	gmRobotExpertStoreOpenPath    = "/local/dnf-gm/robots/expert-store/open"
	gmRobotExpertStoreClosePath   = "/local/dnf-gm/robots/expert-store/close"
	gmRobotExpertStoreCostPath    = "/local/dnf-gm/robots/expert-store/cost"
	gmRobotReferencePath          = "/local/dnf-gm/robots/reference"
)

const gmRobotMaximumProvisionBatch = 64

const gmRobotCharacterNameAttempts = 32

var gmRobotNameCharacters = []rune("晨星月影风语霜华赤焰青岚星尘流光云歌夜航银羽苍穹花火山岚晴空远星剑客旅人游侠勇者行者守望逐风寻梦猎手冒险先锋")

type gmRobotProvisionBatchRequest struct {
	RobotType       string `json:"robot_type"`
	Count           int    `json:"count"`
	StartIndex      int    `json:"start_index"`
	AccountPrefix   string `json:"account_prefix"`
	NamePrefix      string `json:"name_prefix"`
	ProfilePrefix   string `json:"profile_prefix"`
	ChannelID       int    `json:"channel_id"`
	AutoStart       bool   `json:"auto_start"`
	Job             int    `json:"job"`
	GrowType        int    `json:"grow_type"`
	Level           int    `json:"level"`
	Village         int    `json:"village"`
	Area            int    `json:"area"`
	X               int    `json:"x"`
	Y               int    `json:"y"`
	ExpertStoreKind string `json:"expert_store_kind,omitempty"`
	ExpertStoreCost uint32 `json:"expert_store_cost,omitempty"`
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
	env.AdminMux.HandleFunc(gmRobotReferencePath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			gmMethodNotAllowed(w, http.MethodGet)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": bridge.RobotReferenceCatalog()})
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
	env.AdminMux.HandleFunc(gmRobotSpeechPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			Modes []dnfrobot.SpeechMode `json:"modes"`
		}
		if !decodeGMRequest(w, r, &request, 64<<10) {
			return
		}
		result, err := manager.UpdateSpeechModes(r.Context(), request.Modes)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok": true, "message": "robot speech modes saved", "data": result,
		})
	}))
	env.AdminMux.HandleFunc(gmRobotRecyclePath, auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				writeGMRobotError(w, fmt.Errorf("%w: robot id is required", dnfbridge.ErrGMRequestInvalid))
				return
			}
			snapshot, err := manager.Snapshot(id)
			if err != nil {
				writeGMRobotError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "data": snapshot.Profile.Recycle})
		case http.MethodPost:
			var request struct {
				ID string `json:"id"`
				dnfrobotdomain.RecyclePolicy
			}
			if !decodeGMRequest(w, r, &request, 512<<10) {
				return
			}
			request.ID = strings.TrimSpace(request.ID)
			if request.ID == "" {
				writeGMRobotError(w, fmt.Errorf("%w: robot id is required", dnfbridge.ErrGMRequestInvalid))
				return
			}
			result, err := manager.UpdateProfileRecyclePolicy(r.Context(), request.ID, request.RecyclePolicy)
			if err != nil {
				writeGMRobotError(w, err)
				return
			}
			writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot recycle policy saved", "data": result})
		default:
			gmMethodNotAllowed(w, "GET, POST")
		}
	}))
	env.AdminMux.HandleFunc(gmRobotProfilePath, auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var profile dnfrobotdomain.Profile
			if !decodeGMRequest(w, r, &profile, 512<<10) {
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
		bounds := dnfrobot.PlacementBounds{}
		if placement, ok := bridge.RobotMovementBoundsAround(int(request.Village), int(request.Area), int(request.X), int(request.Y)); ok {
			bounds = dnfrobot.PlacementBounds{
				Set: true, MinX: placement.MinX, MaxX: placement.MaxX,
				MinY: placement.MinY, MaxY: placement.MaxY,
			}
		}
		if err := manager.MoveAndPersist(r.Context(), request.ID, request.Village, request.Area, request.X, request.Y, request.MoveType, bounds); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot move packet sent"})
	}))
	env.AdminMux.HandleFunc(gmRobotRelocatePath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID        string `json:"id"`
			ChannelID int    `json:"channel_id"`
			Village   uint8  `json:"village"`
			Area      uint8  `json:"area"`
			X         uint16 `json:"x"`
			Y         uint16 `json:"y"`
			MoveType  uint8  `json:"move_type"`
			AutoStart *bool  `json:"auto_start,omitempty"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		request.ID = strings.TrimSpace(request.ID)
		if request.ID == "" || request.ChannelID <= 0 || request.ChannelID > 65535 {
			writeGMRobotError(w, errors.New("robot id or channel id is invalid"))
			return
		}
		if _, err := bridge.RobotGameEndpoint(request.ChannelID); err != nil {
			writeGMRobotError(w, fmt.Errorf("target channel is unavailable: %w", err))
			return
		}
		bounds := dnfrobot.PlacementBounds{}
		if placement, ok := bridge.RobotMovementBoundsAround(int(request.Village), int(request.Area), int(request.X), int(request.Y)); ok {
			bounds = dnfrobot.PlacementBounds{
				Set: true, MinX: placement.MinX, MaxX: placement.MaxX,
				MinY: placement.MinY, MaxY: placement.MaxY,
			}
		}
		operation := manager.BeginOperation("relocate", request.ID)
		result, err := manager.RelocateWithAutoStart(
			r.Context(), request.ID, request.ChannelID,
			request.Village, request.Area, request.X, request.Y, request.MoveType, bounds, request.AutoStart,
		)
		autoStart := "unchanged"
		if request.AutoStart != nil {
			autoStart = strconv.FormatBool(*request.AutoStart)
		}
		manager.CompleteOperation(operation.ID, fmt.Sprintf("channel=%d town=%d area=%d x=%d y=%d auto_start=%s",
			request.ChannelID, request.Village, request.Area, request.X, request.Y, autoStart), err)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok": true, "message": "robot location updated", "data": result,
		})
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
			ID    string                            `json:"id"`
			Title string                            `json:"title"`
			Items []dnfrobotdomain.PrivateStoreItem `json:"items"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		if _, err := manager.ConfigurePrivateStore(r.Context(), request.ID, request.Title, request.Items); err != nil {
			writeGMRobotError(w, err)
			return
		}
		if err := manager.OpenConfiguredPrivateStore(r.Context(), request.ID); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot private store opening requested"})
	}))
	env.AdminMux.HandleFunc(gmRobotPrivateStoreConfigPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID    string                            `json:"id"`
			Title string                            `json:"title"`
			Items []dnfrobotdomain.PrivateStoreItem `json:"items"`
		}
		if !decodeGMRequest(w, r, &request, 32<<10) {
			return
		}
		result, err := manager.ConfigurePrivateStore(r.Context(), request.ID, request.Title, request.Items)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok": true, "message": "robot private store configuration saved", "data": result,
		})
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
	env.AdminMux.HandleFunc(gmRobotExpertStoreOpenPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID   string `json:"id"`
			Cost uint32 `json:"cost"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		cost := request.Cost
		if err := manager.OpenExpertJobStore(r.Context(), strings.TrimSpace(request.ID), &cost); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot expert store opening requested"})
	}))
	env.AdminMux.HandleFunc(gmRobotExpertStoreClosePath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		id, ok := decodeRobotID(w, r)
		if !ok {
			return
		}
		if err := manager.CloseExpertJobStore(id); err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "robot expert store close requested"})
	}))
	env.AdminMux.HandleFunc(gmRobotExpertStoreCostPath, auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			gmMethodNotAllowed(w, http.MethodPost)
			return
		}
		var request struct {
			ID   string `json:"id"`
			Cost uint32 `json:"cost"`
		}
		if !decodeGMRequest(w, r, &request, 16<<10) {
			return
		}
		request.ID = strings.TrimSpace(request.ID)
		if request.ID == "" {
			writeGMRobotError(w, errors.New("robot id or expert store cost is invalid"))
			return
		}
		result, err := manager.SetExpertStoreCost(r.Context(), request.ID, request.Cost)
		if err != nil {
			writeGMRobotError(w, err)
			return
		}
		writeGMJSON(w, http.StatusOK, map[string]any{
			"ok": true, "message": "robot expert store cost updated", "data": result,
		})
	}))
}

func normalizeGMRobotProvisionBatchRequest(request gmRobotProvisionBatchRequest) (gmRobotProvisionBatchRequest, error) {
	request.AccountPrefix = strings.TrimSpace(request.AccountPrefix)
	request.NamePrefix = strings.TrimSpace(request.NamePrefix)
	request.ProfilePrefix = strings.TrimSpace(request.ProfilePrefix)
	request.RobotType = strings.ToLower(strings.TrimSpace(request.RobotType))
	if request.RobotType == "" {
		request.RobotType = string(dnfrobotdomain.RobotTypeNormal)
	}
	request.ExpertStoreKind = strings.ToLower(strings.TrimSpace(request.ExpertStoreKind))
	if request.Count < 1 || request.Count > gmRobotMaximumProvisionBatch || request.StartIndex < 0 || request.StartIndex > 999999 ||
		request.AccountPrefix == "" || request.NamePrefix == "" || request.ProfilePrefix == "" || request.ChannelID < 1 || request.ChannelID > 65535 {
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: invalid robot provision batch", dnfbridge.ErrGMRequestInvalid)
	}
	if request.ExpertStoreKind != "" && request.ExpertStoreKind != "disjoint" && request.ExpertStoreKind != "enchant" {
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: invalid robot expert store kind", dnfbridge.ErrGMRequestInvalid)
	}
	switch dnfrobotdomain.RobotType(request.RobotType) {
	case dnfrobotdomain.RobotTypeProfessional:
		if request.ExpertStoreKind == "" {
			return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: professional robot requires an expert store kind", dnfbridge.ErrGMRequestInvalid)
		}
	case dnfrobotdomain.RobotTypePersonalStore:
		request.ExpertStoreKind = ""
		request.ExpertStoreCost = 0
	case dnfrobotdomain.RobotTypeNormal:
		request.ExpertStoreKind = ""
		request.ExpertStoreCost = 0
	default:
		return gmRobotProvisionBatchRequest{}, fmt.Errorf("%w: invalid robot type", dnfbridge.ErrGMRequestInvalid)
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
			Name:      "",
		}
		if ctx != nil && ctx.Err() != nil {
			row.Error = ctx.Err().Error()
			result.Failed++
			result.Rows = append(result.Rows, row)
			continue
		}
		moveBounds, hasMoveBounds := bridge.RobotMovementBoundsAround(request.Village, request.Area, request.X, request.Y)
		var provisioned dnfbridge.GMRobotCharacterProvisionResult
		var err error
		for attempt := 0; attempt < gmRobotCharacterNameAttempts; attempt++ {
			name, nameErr := randomRobotCharacterName(request.NamePrefix)
			if nameErr != nil {
				err = nameErr
				break
			}
			row.Name = name
			provisioned, err = bridge.GMProvisionRobotCharacter(ctx, dnfbridge.GMRobotCharacterProvisionRequest{
				AccountID:       row.AccountID,
				Name:            name,
				Job:             request.Job,
				GrowType:        request.GrowType,
				Level:           request.Level,
				Village:         request.Village,
				Area:            request.Area,
				X:               request.X,
				Y:               request.Y,
				ExpertStoreKind: request.ExpertStoreKind,
				ExpertStoreCost: request.ExpertStoreCost,
			})
			if err == nil || !isRobotCharacterNameConflict(err) {
				break
			}
		}
		if err != nil {
			row.Error = err.Error()
			result.Failed++
			result.Rows = append(result.Rows, row)
			continue
		}
		row.CharacterID = provisioned.CharacterID
		var expertStoreCost *uint32
		if request.ExpertStoreKind != "" {
			cost := request.ExpertStoreCost
			expertStoreCost = &cost
		}
		profile := dnfrobotdomain.Profile{
			ID:                     row.ProfileID,
			AccountID:              row.AccountID,
			CharacterID:            provisioned.CharacterID,
			RobotType:              dnfrobotdomain.RobotType(request.RobotType),
			ChannelID:              request.ChannelID,
			AutoStart:              request.AutoStart,
			Village:                uint8(request.Village),
			Area:                   uint8(request.Area),
			X:                      uint16(request.X),
			Y:                      uint16(request.Y),
			MoveBoundsSet:          hasMoveBounds,
			MoveMinX:               moveBounds.MinX,
			MoveMaxX:               moveBounds.MaxX,
			MoveMinY:               moveBounds.MinY,
			MoveMaxY:               moveBounds.MaxY,
			ExpertStoreKind:        request.ExpertStoreKind,
			ExpertStoreCost:        expertStoreCost,
			AutomaticStoreDisabled: true,
		}
		if profile.RobotType == dnfrobotdomain.RobotTypePersonalStore {
			profile.PrivateStoreTitle = dnfrobotdomain.DefaultPersonalStoreTitle
			profile.PrivateStoreItems = []dnfrobotdomain.PrivateStoreItem{{
				ItemID: dnfrobotdomain.DefaultPersonalStoreItemID,
				Count:  dnfrobotdomain.DefaultPersonalStoreItemCount,
				Price:  dnfrobotdomain.DefaultPersonalStoreItemPrice,
			}}
		}
		runtime, err := manager.Upsert(ctx, profile)
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

func randomRobotCharacterName(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", fmt.Errorf("%w: robot character name prefix is empty", dnfbridge.ErrGMRequestInvalid)
	}
	var name strings.Builder
	name.Grow(len(prefix) + 9)
	name.WriteString(prefix)
	limit := big.NewInt(int64(len(gmRobotNameCharacters)))
	for index := 0; index < 3; index++ {
		value, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate robot character name: %w", err)
		}
		name.WriteRune(gmRobotNameCharacters[value.Int64()])
	}
	return name.String(), nil
}

func isRobotCharacterNameConflict(err error) bool {
	return err != nil && errors.Is(err, dnfbridge.ErrGMRequestInvalid) &&
		strings.Contains(strings.ToLower(err.Error()), "character name already exists")
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
