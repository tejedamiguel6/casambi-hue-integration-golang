package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

//go:embed static/index.html
var staticFS embed.FS

// handleDashboard serves the embedded single-page web UI at /.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		s.writeError(w, "dashboard not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

// handleReactiveStream pushes the reactive engine status over Server-Sent
// Events at ~12 Hz so the dashboard can render live without polling.
func (s *Server) handleReactiveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			data, err := json.Marshal(s.reactive.Status())
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleReactiveAutoGain re-enables auto-gain (manual SetGain disables it).
func (s *Server) handleReactiveAutoGain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	body.Enabled = true
	json.NewDecoder(r.Body).Decode(&body)
	s.reactive.SetAutoGain(body.Enabled)
	s.writeJSON(w, map[string]any{"status": "ok", "autoGain": body.Enabled})
}
