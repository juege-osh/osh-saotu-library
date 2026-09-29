package main

import (
	"net/http"
	"os"
)

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.appDB.PingContext(r.Context()); err != nil {
		apiError(w, http.StatusServiceUnavailable, "数据库暂时不可用")
		return
	}
	info, err := os.Stat(s.mediaDir)
	if err != nil || !info.IsDir() {
		apiError(w, http.StatusServiceUnavailable, "图片存储暂时不可用")
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "service": "saotu-osh", "version": "1.0.0"})
}
