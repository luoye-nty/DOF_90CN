//go:build paid

// 数据库备份远程管理端点（设计 P1 三端备份闭环）：
//
//	GET  /manage/v1/backup/config   查询自动备份配置+备份目录+运行态快照
//	POST /manage/v1/backup/config   应用自动备份配置（enabled / interval_hours，基准=now）
//	GET  /manage/v1/backup/list     列出服务器端备份目录全部 zip
//	POST /manage/v1/backup/restore  覆盖还原指定备份（运行中先自动停服，幂等拉起 MySQL 后还原）
//
// 还原属覆盖操作：管理端 UI 在调用前负责二次确认（检测到服务端运行会先弹窗征询）。
package manage

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"dnf90ops/internal/config"
)

// backupApplyRequest POST /manage/v1/backup/config 入参。
type backupApplyRequest struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
	// Keep 保留份数；0 表示"用默认值"（兼容只发两个字段的老管理工具）。
	Keep int `json:"keep"`
}

// backupRestoreRequest POST /manage/v1/backup/restore 入参。
// File 仅接受文件名（服务端拼备份目录），禁止路径分隔符 / 反斜杠 / ..，防路径穿越。
type backupRestoreRequest struct {
	File string `json:"file"`
}

// backupIntervalError 间隔非法（与主程序共用的硬性合法区间 1~168）。
var backupIntervalError = errors.New("invalid-backup-interval")

// handleBackupConfigGet 返回自动备份配置快照。
func (s *Server) handleBackupConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BackupStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup-status-unavailable")
		return
	}
	writeOK(w, s.bridge.BackupStatus())
}

// handleBackupConfigApply 校验并应用自动备份配置（与 GUI「保存设置」语义一致：基准=now）。
func (s *Server) handleBackupConfigApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BackupApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup-apply-unavailable")
		return
	}
	var req backupApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	if req.IntervalHours < 1 || req.IntervalHours > 168 {
		writeErr(w, http.StatusBadRequest, backupIntervalError.Error())
		return
	}
	// keep=0 视为"未指定"，用默认值（老管理工具不会发这个字段）。
	keep := req.Keep
	if keep == 0 {
		keep = config.AutoBackupKeepDefault
	}
	if keep < 1 || keep > config.AutoBackupKeepMax {
		writeErr(w, http.StatusBadRequest, "invalid-backup-keep")
		return
	}
	status, err := s.bridge.BackupApply(r.Context(), req.Enabled, req.IntervalHours, keep)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "apply-failed")
		return
	}
	writeOK(w, status)
}

// handleBackupList 返回服务器端备份目录下全部备份文件（新→旧）。
func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BackupList == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup-list-unavailable")
		return
	}
	entries, err := s.bridge.BackupList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list-failed")
		return
	}
	if entries == nil {
		entries = []BackupFileEntry{}
	}
	writeOK(w, entries)
}

// handleBackupNow 立即执行一次在线备份（不停止服务端，与运维工具 GUI「立即备份」同链路）。
func (s *Server) handleBackupNow(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BackupNow == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup-now-unavailable")
		return
	}
	res, err := s.bridge.BackupNow(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "backup-failed: "+err.Error())
		return
	}
	writeOK(w, res)
}

// handleBackupRestore 覆盖还原：入参 fileName 仅允许 base 名。
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BackupRestore == nil {
		writeErr(w, http.StatusServiceUnavailable, "backup-restore-unavailable")
		return
	}
	var req backupRestoreRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	name := strings.TrimSpace(req.File)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		writeErr(w, http.StatusBadRequest, "bad-file-name")
		return
	}
	res, err := s.bridge.BackupRestore(r.Context(), name, func(line string) {
		s.log.Info("backup restore", "line", line)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "restore-failed: "+err.Error())
		return
	}
	writeOK(w, res)
}
