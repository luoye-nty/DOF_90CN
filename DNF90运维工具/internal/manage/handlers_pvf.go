//go:build paid

// POST /manage/v1/pvf/info：管理工具完成本地加密后上报 EInfo（版本+sha256+key_set+直链+启用开关）。
// 落库策略：发布新版本先使旧 active 全部失效，再按 file_sha256 upsert 覆盖为当前生效记录
// （服务端只认最新发布，旧客户端 sha 一律 404 阻断要求更新）。
package manage

import (
	"net/http"
	"strings"

	"dnf90ops/internal/store"
)

// pvfInfoRequest 管理工具加密上报请求体（字段名与 EInfo 契约对齐）。
type pvfInfoRequest struct {
	PVFVersion        string `json:"pvf_version"`
	FileSHA256        string `json:"file_sha256"`
	KeySet            string `json:"key_set"`                       // 6 组 × 8B = 96 hex
	DownloadURL       string `json:"download_url,omitempty"`        // 加密后客户端更新包直链（zip/7z/rar），可为空
	ClientDownloadURL string `json:"client_download_url,omitempty"` // 整包客户端直链（zip/7z/rar），可为空
	Enabled           *bool  `json:"enabled"`                       // 是否启用 PVF 加密校验；nil 视为 true（旧版兼容）
}

func (s *Server) handlePvfInfo(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	var req pvfInfoRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	version := strings.TrimSpace(req.PVFVersion)
	if version == "" {
		writeErr(w, http.StatusBadRequest, "pvf_version-required")
		return
	}
	sha, err := store.NormalizeSHA256Hex(req.FileSHA256)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid-file_sha256: "+err.Error())
		return
	}
	keySet, err := store.NormalizeKeySetHex(req.KeySet)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid-key_set: "+err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	id, err := st.UpsertPvfKey(r.Context(), version, sha, keySet, req.DownloadURL, req.ClientDownloadURL, enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		return
	}
	writeOK(w, map[string]any{
		"id":                  id,
		"pvf_version":         version,
		"file_sha256":         sha,
		"key_set":             keySet,
		"download_url":        req.DownloadURL,
		"client_download_url": req.ClientDownloadURL,
		"enabled":             enabled,
	})
}

// handlePvfInfoList GET /manage/v1/pvf/info：管理工具 PVF 加密页已发布列表。
// 返回全部 active=1 记录（含 key_set，供管理端查看/二次加密引用）。
func (s *Server) handlePvfInfoList(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	list, err := st.ListActivePvfKeys(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		return
	}
	type item struct {
		ID                int64  `json:"id"`
		PVFVersion        string `json:"pvf_version"`
		FileSHA256        string `json:"file_sha256"`
		KeySet            string `json:"key_set"`
		DownloadURL       string `json:"download_url"`
		ClientDownloadURL string `json:"client_download_url"`
		Enabled           bool   `json:"enabled"`
		CreatedAt         string `json:"created_at"`
		UpdatedAt         string `json:"updated_at"`
	}
	out := make([]item, 0, len(list))
	for _, it := range list {
		out = append(out, item{
			ID:                it.ID,
			PVFVersion:        it.PVFVersion,
			FileSHA256:        it.FileSHA256,
			KeySet:            it.KeySet,
			DownloadURL:       it.DownloadURL,
			ClientDownloadURL: it.ClientDownloadURL,
			Enabled:           it.PvfEnabled,
			CreatedAt:         it.CreatedAt.Format("2006-01-02 15:04:05"),
			UpdatedAt:         it.UpdatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	writeOK(w, out)
}
