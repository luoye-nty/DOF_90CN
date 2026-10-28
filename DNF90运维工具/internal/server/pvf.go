// PVF 加密 key 下发（GET /launcher/v1/pvf/key）与最新发布信息（GET /launcher/v1/pvf/latest）：
// 登录器登录拿到 ticket 后，带本机 Script.pvf 的 SHA-256 来换取管理端最新下发的 key_set；
// 最新发布信息接口无 ticket 依赖（文件哈希未加密/未启用校验时按 enabled 语义放行）。
// 仅校验 ticket 会话有效性（不验封禁，与 redeem 同策略），再由 pvf_keys 表判定。
package server

import (
	"errors"
	"net/http"
	"strings"

	"dnf90ops/internal/store"
)

// latestPvfData GET /launcher/v1/pvf/latest 成功响应 data 结构。
// 注意：latest 接口无鉴权，仅下发更新判断所需字段，绝不下发 key_set（防密钥公开绕过授权墙）。
type latestPvfData struct {
	PVFVersion        string `json:"pvf_version"`
	FileSHA256        string `json:"file_sha256"`
	DownloadURL       string `json:"download_url"`
	ClientDownloadURL string `json:"client_download_url"`
}

// pvfResponseData /key 命中时 data 结构。
type pvfResponseData struct {
	PVFVersion  string `json:"pvf_version"`
	FileSHA256  string `json:"file_sha256"`
	KeySet      string `json:"key_set"`
	DownloadURL string `json:"download_url"`
}

// handlePvfKey GET /launcher/v1/pvf/key?ticket=xxx&file_sha256=<64hex>：
// 服务端已启用加密且客户端 sha 命中 200 {ok:true, enabled:true, data:{pvf_version,file_sha256,key_set,download_url}}；
// 加密从未启用或最新发布 enabled=false 200 {ok:true, enabled:false, data:null}，登录器放行不注入；
// ticket 无效 401 ticket-not-found；sha 非法 400 bad-request；
// 加密已启用但无该 pvf 记录 404 pvf-not-found（登录器据此判定“客户端与服务端不一致”阻断启动）。
func (s *Service) handlePvfKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	shaRaw := strings.TrimSpace(r.URL.Query().Get("file_sha256"))
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "ticket required")
		return
	}
	sha, err := store.NormalizeSHA256Hex(shaRaw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}

	accountID, err := s.resolveAccountIDByTicket(r.Context(), ticket)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("pvf key resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}

	// 最新发布总开关：从未发布或 enabled=false 一律放行（客户端无需换钥/更新）
	latest, err := s.store.GetLatestActivePvfKey(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrPvfNotFound) {
			// 加密从未启用：放行。登录器不注入 key_set，客户端用内置标准 key 正常启动。
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "data": nil})
			return
		}
		s.log.Error("pvf latest lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	if !latest.PvfEnabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "data": nil})
		return
	}

	info, err := s.store.GetPvfKeyBySHA(r.Context(), sha)
	if err != nil {
		if errors.Is(err, store.ErrPvfNotFound) {
			writeError(w, http.StatusNotFound, "pvf-not-found", "client pvf not published, please update client")
			return
		}
		s.log.Error("pvf key lookup failed", "account_id", accountID, "file_sha256", sha, "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "data": pvfResponseData{
		PVFVersion:  info.PVFVersion,
		FileSHA256:  info.FileSHA256,
		KeySet:      info.KeySet,
		DownloadURL: info.DownloadURL,
	}})
}

// handlePvfLatest GET /launcher/v1/pvf/latest：
// 供登录器自检/轮询“是否需要更新客户端”。无需 ticket（不泄露密钥敏感信息）：
// 200 {ok:true, enabled:true, data:{pvf_version,file_sha256,download_url}}；
// 未发布或 enabled=false 200 {ok:true, enabled:false, data:null}。
func (s *Service) handlePvfLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	latest, err := s.store.GetLatestActivePvfKey(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrPvfNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "data": nil})
			return
		}
		s.log.Error("pvf latest lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	if !latest.PvfEnabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false, "data": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": true, "data": latestPvfData{
		PVFVersion:        latest.PVFVersion,
		FileSHA256:        latest.FileSHA256,
		DownloadURL:       latest.DownloadURL,
		ClientDownloadURL: latest.ClientDownloadURL,
	}})
}
