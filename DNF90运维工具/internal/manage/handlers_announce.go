//go:build paid

// 全服公告发布：POST /manage/v1/announce。
// 内容校验（rune ≤ 100 且 UTF-8 字节 ≤ 400）+ 颜色格式 + 同会话 3 秒限频。
package manage

import (
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var announceColorRE = regexp.MustCompile(`^0[xX][0-9A-Fa-f]{8}$`)

const (
	announceMaxRunes     = 100
	announceMaxBytes     = 400
	announceDefaultColor = "0xFF0000FF"
	announceMinInterval  = 3 * time.Second
)

type announceRequest struct {
	Content string `json:"content"`
	Color   string `json:"color"`
}

func (s *Server) handleAnnounce(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.PublishAnnounce == nil {
		writeErr(w, http.StatusServiceUnavailable, "announce-hook-unavailable")
		return
	}
	var req announceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeErr(w, http.StatusBadRequest, "content-required")
		return
	}
	if utf8.RuneCountInString(content) > announceMaxRunes || len(content) > announceMaxBytes {
		writeErr(w, http.StatusBadRequest, "content-too-long")
		return
	}
	color := strings.TrimSpace(req.Color)
	if color == "" {
		color = announceDefaultColor
	} else if !announceColorRE.MatchString(color) {
		writeErr(w, http.StatusBadRequest, "bad-color")
		return
	}

	// 同会话限频（3 秒）。
	s.announceMu.Lock()
	if time.Since(s.lastAnnounce) < announceMinInterval {
		s.announceMu.Unlock()
		writeErr(w, http.StatusTooManyRequests, "too-frequent")
		return
	}
	s.lastAnnounce = time.Now()
	s.announceMu.Unlock()

	id, err := s.bridge.PublishAnnounce(r.Context(), content, color)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "announce-failed: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"id": id})
}
