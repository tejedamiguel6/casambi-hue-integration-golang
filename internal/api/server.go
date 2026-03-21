package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/migueltejeda/casambi-go/internal/audio"
	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/protocol"
	"github.com/migueltejeda/casambi-go/internal/spotify"
)

// Server wraps the BLE connection and Hue bridge in an HTTP API.
type Server struct {
	conn     *ble.Connection
	creds    *NetworkCredentials
	hue      *HueClient
	beatSync *spotify.BeatSync
	reactive *audio.ReactiveEngine
	mux      *http.ServeMux
	port     string
	seq      uint16 // command sequence counter
}

// SpotifyAPIURL can be overridden via environment variable SPOTIFY_NOW_PLAYING_URL
var spotifyAPIURL = "https://api-spotify-tracks.mtejeda.co/now-listening-to"

func init() {
	if url := os.Getenv("SPOTIFY_NOW_PLAYING_URL"); url != "" {
		spotifyAPIURL = url
	}
}

func NewServer(conn *ble.Connection, creds *NetworkCredentials, hue *HueClient, port string) *Server {
	// Casambi send helpers for the reactive engine
	casambiSend := func(level uint8, unitID uint16) {
		cmd := protocol.NewSetLevelCommand(level, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}
	casambiState := func(state []byte, unitID uint16) {
		cmd := protocol.NewSetRawStateCommand(state, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}

	s := &Server{
		conn:     conn,
		creds:    creds,
		hue:      hue,
		beatSync: spotify.NewBeatSync(conn, hue, spotifyAPIURL),
		reactive: audio.NewReactiveEngine(hue, casambiSend, casambiState, spotifyAPIURL),
		port:     port,
		seq:      1,
	}

	mux := http.NewServeMux()

	// Help — shows all endpoints
	mux.HandleFunc("GET /", s.handleHelp)

	// Status
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// Casambi units (BLE)
	mux.HandleFunc("GET /api/units", s.handleListUnits)
	mux.HandleFunc("POST /api/units/{id}/on", s.handleUnitOn)
	mux.HandleFunc("POST /api/units/{id}/off", s.handleUnitOff)
	mux.HandleFunc("POST /api/units/{id}/level", s.handleSetLevel)
	mux.HandleFunc("POST /api/units/{id}/state", s.handleSetState)

	// Casambi scenes (BLE)
	mux.HandleFunc("GET /api/scenes", s.handleListScenes)
	mux.HandleFunc("POST /api/scenes/{id}/on", s.handleSceneOn)
	mux.HandleFunc("POST /api/scenes/{id}/off", s.handleSceneOff)

	// Spotify beat sync
	mux.HandleFunc("GET /api/spotify/sync/status", s.handleSyncStatus)
	mux.HandleFunc("POST /api/spotify/sync/start", s.handleSyncStart)
	mux.HandleFunc("POST /api/spotify/sync/stop", s.handleSyncStop)
	mux.HandleFunc("POST /api/spotify/sync/bpm", s.handleSyncBPM)
	mux.HandleFunc("POST /api/spotify/sync/tap", s.handleSyncTap)
	mux.HandleFunc("POST /api/spotify/sync/save", s.handleSyncSave)

	// Audio-reactive lighting
	mux.HandleFunc("GET /api/reactive/status", s.handleReactiveStatus)
	mux.HandleFunc("POST /api/reactive/start", s.handleReactiveStart)
	mux.HandleFunc("POST /api/reactive/stop", s.handleReactiveStop)
	mux.HandleFunc("POST /api/reactive/gain", s.handleReactiveGain)
	mux.HandleFunc("POST /api/reactive/bpm", s.handleReactiveBPM)

	// Hue lights (HTTP to bridge)
	mux.HandleFunc("GET /api/hue/lights", s.handleListHueLights)
	mux.HandleFunc("POST /api/hue/lights/{id}/on", s.handleHueLightOn)
	mux.HandleFunc("POST /api/hue/lights/{id}/off", s.handleHueLightOff)
	mux.HandleFunc("POST /api/hue/lights/{id}/color", s.handleHueLightColor)
	mux.HandleFunc("POST /api/hue/lights/{id}/level", s.handleHueLightLevel)

	s.mux = mux
	return s
}

func (s *Server) Start() error {
	addr := ":" + s.port
	fmt.Printf("\nAPI server listening on http://localhost%s\n", addr)
	fmt.Println("Endpoints:")
	fmt.Println("  GET  /api/status")
	fmt.Println("  GET  /api/units                              Casambi units")
	fmt.Println("  GET  /api/scenes                             Casambi scenes")
	fmt.Println("  POST /api/units/{id}/on")
	fmt.Println("  POST /api/units/{id}/off")
	fmt.Println("  POST /api/units/{id}/level   {\"level\": 0-255}")
	fmt.Println("  POST /api/units/{id}/state   {\"state\": \"f2ff6097b8\"}")
	fmt.Println("  POST /api/scenes/{id}/on")
	fmt.Println("  POST /api/scenes/{id}/off")
	fmt.Println("  GET  /api/spotify/sync/status                 Spotify sync")
	fmt.Println("  POST /api/spotify/sync/start   {\"bpm\": 120}")
	fmt.Println("  POST /api/spotify/sync/stop")
	fmt.Println("  POST /api/spotify/sync/bpm     {\"bpm\": 140}")
	fmt.Println("  POST /api/spotify/sync/tap                    Tap tempo")
	fmt.Println("  GET  /api/reactive/status                     Audio-reactive")
	fmt.Println("  POST /api/reactive/start")
	fmt.Println("  POST /api/reactive/stop")
	fmt.Println("  POST /api/spotify/sync/save                   Save BPM to cache")
	fmt.Println("  GET  /api/hue/lights                         Hue lights")
	fmt.Println("  POST /api/hue/lights/{id}/on")
	fmt.Println("  POST /api/hue/lights/{id}/off")
	fmt.Println("  POST /api/hue/lights/{id}/color  {\"hue\":0-65535,\"sat\":0-254,\"bri\":0-254}")
	fmt.Println("  POST /api/hue/lights/{id}/level  {\"level\":0-254}")
	return http.ListenAndServe(addr, s.logMiddleware(s.mux))
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func (s *Server) nextSeq() uint16 {
	s.seq++
	return s.seq
}

func (s *Server) writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) parseUnitID(r *http.Request) (uint16, error) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, fmt.Errorf("invalid unit ID: %s", idStr)
	}
	return uint16(id), nil
}

func (s *Server) parseSceneID(r *http.Request) (uint16, error) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, fmt.Errorf("invalid scene ID: %s", idStr)
	}
	return uint16(id), nil
}

// ── Handlers ──────────────────────────────────────────────

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	help := map[string]any{
		"quickstart": map[string]string{
			"1_start_reactive": "curl -X POST localhost:" + s.port + "/api/reactive/start",
			"2_set_gain":       "curl -X POST localhost:" + s.port + "/api/reactive/gain -d '{\"gain\": 30}'",
			"3_check_status":   "curl localhost:" + s.port + "/api/reactive/status",
			"4_stop":           "curl -X POST localhost:" + s.port + "/api/reactive/stop",
		},
		"endpoints": map[string]any{
			"status": map[string]string{
				"GET /api/status": "Connection info",
			},
			"casambi_lights": map[string]string{
				"GET  /api/units":             "List all units",
				"POST /api/units/{id}/on":     "Turn on",
				"POST /api/units/{id}/off":    "Turn off",
				"POST /api/units/{id}/level":  "{\"level\": 0-255}",
				"POST /api/units/{id}/state":  "{\"state\": \"f2ff6097b8\"}",
			},
			"casambi_scenes": map[string]string{
				"GET  /api/scenes":          "List all scenes",
				"POST /api/scenes/{id}/on":  "Activate scene",
				"POST /api/scenes/{id}/off": "Deactivate scene",
			},
			"audio_reactive": map[string]string{
				"GET  /api/reactive/status": "Audio analysis + colors",
				"POST /api/reactive/start":  "Start mic listening",
				"POST /api/reactive/stop":   "Stop",
				"POST /api/reactive/gain":   "{\"gain\": 1-500}",
			},
			"hue_lights": map[string]string{
				"GET  /api/hue/lights":            "List all Hue lights",
				"POST /api/hue/lights/{id}/on":    "Turn on",
				"POST /api/hue/lights/{id}/off":   "Turn off",
				"POST /api/hue/lights/{id}/color": "{\"hue\":0-65535, \"sat\":0-254, \"bri\":0-254}",
				"POST /api/hue/lights/{id}/level": "{\"level\": 0-254}",
			},
			"spotify_sync": map[string]string{
				"GET  /api/spotify/sync/status": "Sync state + BPM",
				"POST /api/spotify/sync/start":  "{\"bpm\": 120}",
				"POST /api/spotify/sync/stop":   "Stop sync",
				"POST /api/spotify/sync/bpm":    "{\"bpm\": 140}",
				"POST /api/spotify/sync/tap":    "Tap tempo",
				"POST /api/spotify/sync/save":   "Save BPM to cache",
			},
		},
	}
	s.writeJSON(w, help)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, map[string]any{
		"status":    "connected",
		"network":   s.creds.NetworkID,
		"units":     len(s.creds.Units),
		"scenes":    len(s.creds.Scenes),
		"device":    s.conn.Device.Name,
		"mtu":       s.conn.MTU,
		"commandSeq": s.seq,
	})
}

func (s *Server) handleListUnits(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.creds.Units)
}

func (s *Server) handleListScenes(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.creds.Scenes)
}

func (s *Server) handleUnitOn(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetLevelCommand(255, id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleUnitOff(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetLevelCommand(0, id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSetLevel(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Level int `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Level < 0 || body.Level > 255 {
		s.writeError(w, "level must be 0-255", http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetLevelCommand(uint8(body.Level), id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	stateBytes, err := hexDecode(body.State)
	if err != nil {
		s.writeError(w, "state must be hex string (e.g. \"f2ff6097b8\")", http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetRawStateCommand(stateBytes, id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSceneOn(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseSceneID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetLevelCommand(255, id, protocol.TargetScene, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSceneOff(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseSceneID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := protocol.NewSetLevelCommand(0, id, protocol.TargetScene, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

// ── Spotify Sync Handlers ─────────────────────────────────

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.beatSync.Status())
}

func (s *Server) handleSyncStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BPM float64 `json:"bpm"`
	}
	body.BPM = 120 // default
	json.NewDecoder(r.Body).Decode(&body)
	if body.BPM < 30 || body.BPM > 300 {
		s.writeError(w, "bpm must be 30-300", http.StatusBadRequest)
		return
	}
	if err := s.beatSync.Start(body.BPM); err != nil {
		s.writeError(w, err.Error(), http.StatusConflict)
		return
	}
	s.writeJSON(w, map[string]any{"status": "started", "bpm": body.BPM})
}

func (s *Server) handleSyncStop(w http.ResponseWriter, r *http.Request) {
	s.beatSync.Stop()
	s.writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) handleSyncBPM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BPM float64 `json:"bpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.BPM < 30 || body.BPM > 300 {
		s.writeError(w, "bpm must be 30-300", http.StatusBadRequest)
		return
	}
	s.beatSync.SetBPM(body.BPM)
	s.writeJSON(w, map[string]any{"status": "ok", "bpm": body.BPM})
}

// ── Audio-Reactive Handlers ───────────────────────────────

func (s *Server) handleReactiveStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, s.reactive.Status())
}

func (s *Server) handleReactiveStart(w http.ResponseWriter, r *http.Request) {
	// Stop BPM sync if running
	s.beatSync.Stop()

	if err := s.reactive.Start(); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) handleReactiveStop(w http.ResponseWriter, r *http.Request) {
	s.reactive.Stop()
	s.writeJSON(w, map[string]string{"status": "stopped"})
}


func (s *Server) handleReactiveBPM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BPM float64 `json:"bpm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.BPM < 30 || body.BPM > 300 {
		s.writeError(w, "bpm must be 30-300", http.StatusBadRequest)
		return
	}
	s.reactive.SetBPM(body.BPM)
	s.writeJSON(w, map[string]any{"status": "ok", "bpm": body.BPM})
}

func (s *Server) handleReactiveGain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Gain float64 `json:"gain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Gain < 1 || body.Gain > 500 {
		s.writeError(w, "gain must be 1-500", http.StatusBadRequest)
		return
	}
	s.reactive.SetGain(body.Gain)
	s.writeJSON(w, map[string]any{"status": "ok", "gain": body.Gain})
}

func (s *Server) handleSyncTap(w http.ResponseWriter, r *http.Request) {
	bpm := s.beatSync.Tap()
	s.writeJSON(w, map[string]any{"status": "ok", "bpm": bpm})
}

func (s *Server) handleSyncSave(w http.ResponseWriter, r *http.Request) {
	s.beatSync.SaveCurrentBPM()
	s.writeJSON(w, map[string]string{"status": "saved"})
}

// ── Hue Handlers ──────────────────────────────────────────

func (s *Server) handleListHueLights(w http.ResponseWriter, r *http.Request) {
	lights, err := s.hue.ListLights()
	if err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, lights)
}

func (s *Server) handleHueLightOn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.hue.SetState(id, map[string]any{"on": true, "bri": 254}); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHueLightOff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.hue.SetState(id, map[string]any{"on": false}); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHueLightColor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Hue int `json:"hue"` // 0-65535
		Sat int `json:"sat"` // 0-254
		Bri int `json:"bri"` // 0-254
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	state := map[string]any{"on": true}
	if body.Hue > 0 {
		state["hue"] = body.Hue
	}
	if body.Sat > 0 {
		state["sat"] = body.Sat
	}
	if body.Bri > 0 {
		state["bri"] = body.Bri
	}
	if err := s.hue.SetState(id, state); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHueLightLevel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Level int `json:"level"` // 0-254
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Level == 0 {
		s.hue.SetState(id, map[string]any{"on": false})
	} else {
		s.hue.SetState(id, map[string]any{"on": true, "bri": body.Level})
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd length hex string")
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		n, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			return nil, err
		}
		b[i] = byte(n)
	}
	return b, nil
}
