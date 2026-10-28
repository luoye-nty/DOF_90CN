package main

import (
	"embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/dungeons_max_difficulty.json
var dungeonsMaxDifficultyRaw []byte

//go:embed web
var webAssets embed.FS

func (s *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(indexHTML)
}
