package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/migueltejeda/casambi-go/internal/audio"
	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/config"
	"github.com/migueltejeda/casambi-go/internal/protocol"
	"github.com/migueltejeda/casambi-go/internal/spotify"
)

// Server wraps the BLE connection and Hue bridge in an HTTP API.
type Server struct {
	conn        *ble.Connection
	creds       *NetworkCredentials
	hue         *HueClient // nil when no Hue bridge is configured
	HueStreamer *HueStreamer
	beatSync    *spotify.BeatSync
	reactive    *audio.ReactiveEngine
	mux         *http.ServeMux
	opts        ServerOptions
	cfgMu       sync.Mutex // guards opts.Config across settings handlers
	seq         uint16     // command sequence counter
}

// ServerOptions carries the user configuration the server needs.
type ServerOptions struct {
	Bind                 string // listen address, e.g. "127.0.0.1"
	Port                 int
	SpotifyURL           string // now-playing endpoint; "" disables Spotify features
	ReactiveCasambiUnits []uint16
	ReactiveHueLights    []string
	BPMCachePath         string
	Config               *config.Config // full config, for the settings API (may be nil)
	ConfigPath           string         // where PUT /api/config persists ("" = default)
}

func NewServer(conn *ble.Connection, creds *NetworkCredentials, hue *HueClient, opts ServerOptions) *Server {
	// Casambi send helpers for the reactive engine
	casambiSend := func(level uint8, unitID uint16) {
		cmd := protocol.NewSetLevelCommand(level, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}
	casambiState := func(state []byte, unitID uint16) {
		cmd := protocol.NewSetRawStateCommand(state, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}
	casambiColor := func(hue uint16, sat uint8, unitID uint16) {
		cmd := protocol.NewSetColorCommand(hue, sat, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}
	casambiFullState := func(dimmer uint8, hue uint16, sat uint8, white uint8, temp uint8, unitID uint16) {
		cmd := protocol.NewSetFullStateCommand(dimmer, hue, sat, white, temp, unitID, protocol.TargetUnit, 0)
		conn.SendCommand(cmd)
	}

	// A nil *HueClient must not become a non-nil interface value downstream,
	// so only assign it when a bridge is actually configured.
	var hueForSync spotify.HueSetter
	var hueForReactive audio.HueSetter
	if hue != nil {
		hueForSync = hue
		hueForReactive = hue
	}

	s := &Server{
		conn:     conn,
		creds:    creds,
		hue:      hue,
		beatSync: spotify.NewBeatSync(conn, hueForSync, opts.SpotifyURL, opts.BPMCachePath, opts.ReactiveCasambiUnits, opts.ReactiveHueLights),
		reactive: audio.NewReactiveEngine(hueForReactive, casambiSend, casambiState, casambiColor, casambiFullState, opts.SpotifyURL, opts.ReactiveCasambiUnits, opts.ReactiveHueLights),
		opts:     opts,
		seq:      1,
	}

	// The now-playing poller runs for the server's lifetime so the dashboard
	// shows the current track even while reactive mode is off.
	s.reactive.StartSpotifyPoller()

	mux := http.NewServeMux()

	// Web UI dashboard ({$} = exact root match only)
	mux.HandleFunc("GET /{$}", s.handleDashboard)

	// Settings (config view/update)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handleUpdateConfig)

	// Help — shows all endpoints
	mux.HandleFunc("GET /api/help", s.handleHelp)

	// Status
	mux.HandleFunc("GET /api/status", s.handleStatus)

	// Casambi units (BLE)
	mux.HandleFunc("GET /api/units", s.handleListUnits)
	mux.HandleFunc("POST /api/units/{id}/on", s.handleUnitOn)
	mux.HandleFunc("POST /api/units/{id}/off", s.handleUnitOff)
	mux.HandleFunc("POST /api/units/{id}/level", s.handleSetLevel)
	mux.HandleFunc("POST /api/units/{id}/state", s.handleSetState)
	mux.HandleFunc("POST /api/units/{id}/color", s.handleSetColor)
	mux.HandleFunc("POST /api/units/{id}/colorxy", s.handleSetColorXY)
	mux.HandleFunc("POST /api/units/{id}/fullstate", s.handleSetFullState)

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
	mux.HandleFunc("GET /api/reactive/stream", s.handleReactiveStream)
	mux.HandleFunc("POST /api/reactive/start", s.handleReactiveStart)
	mux.HandleFunc("POST /api/reactive/stop", s.handleReactiveStop)
	mux.HandleFunc("POST /api/reactive/gain", s.handleReactiveGain)
	mux.HandleFunc("POST /api/reactive/autogain", s.handleReactiveAutoGain)
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
	addr := fmt.Sprintf("%s:%d", s.opts.Bind, s.opts.Port)
	fmt.Printf("\nWeb dashboard:  http://%s/\n", addr)
	fmt.Printf("API reference:  http://%s/api/help\n", addr)
	if s.opts.Bind == "127.0.0.1" || s.opts.Bind == "localhost" {
		fmt.Println("(listening on this machine only — set server.bind: 0.0.0.0 in the config for LAN access)")
	}
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
	port := strconv.Itoa(s.opts.Port)
	help := map[string]any{
		"quickstart": map[string]string{
			"1_start_reactive": "curl -X POST localhost:" + port + "/api/reactive/start",
			"2_set_gain":       "curl -X POST localhost:" + port + "/api/reactive/gain -d '{\"gain\": 30}'",
			"3_check_status":   "curl localhost:" + port + "/api/reactive/status",
			"4_stop":           "curl -X POST localhost:" + port + "/api/reactive/stop",
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
				"GET  /api/reactive/status":   "Audio analysis + colors",
				"GET  /api/reactive/stream":   "Live status via SSE (~12Hz)",
				"POST /api/reactive/start":    "Start mic listening",
				"POST /api/reactive/stop":     "Stop",
				"POST /api/reactive/gain":     "{\"gain\": 1-500}",
				"POST /api/reactive/autogain": "{\"enabled\": true}",
			},
			"dashboard": map[string]string{
				"GET /": "Web UI dashboard",
			},
			"settings": map[string]string{
				"GET /api/config": "Current configuration",
				"PUT /api/config": "Update configuration (reactive lights + Spotify URL apply live)",
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

func (s *Server) handleSetColor(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Hue int `json:"hue"` // 0-1023 (maps to 0-360 degrees)
		Sat int `json:"sat"` // 0-255
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Hue < 0 || body.Hue > 1023 {
		s.writeError(w, "hue must be 0-1023", http.StatusBadRequest)
		return
	}
	if body.Sat < 0 || body.Sat > 255 {
		s.writeError(w, "sat must be 0-255", http.StatusBadRequest)
		return
	}
	log.Printf("SetColor unit %d: hue=%d (%.0f°), sat=%d", id, body.Hue, float64(body.Hue)/1023*360, body.Sat)
	cmd := protocol.NewSetColorCommand(uint16(body.Hue), uint8(body.Sat), id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"status": "ok", "opcode": 7, "hue": body.Hue, "sat": body.Sat})
}

func (s *Server) handleSetColorXY(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		X float64 `json:"x"` // CIE x: 0.0-1.0
		Y float64 `json:"y"` // CIE y: 0.0-1.0
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	// Pack x,y into 3 bytes: (x_scaled << coordLen) | y_scaled, coordLen=11 bits
	coordLen := 11
	xyMask := (1 << coordLen) - 1 // 2047
	xScaled := uint32(body.X * float64(xyMask))
	yScaled := uint32(body.Y * float64(xyMask))
	packed := (xScaled << uint(coordLen)) | yScaled
	payload := []byte{byte(packed), byte(packed >> 8), byte(packed >> 16)}
	log.Printf("SetColorXY unit %d: x=%.4f y=%.4f packed=%06x", id, body.X, body.Y, packed)
	cmd := &protocol.CommandPacket{
		Lifetime: 5,
		OpCode:   protocol.OpSetColorXY,
		Origin:   s.nextSeq(),
		TargetID: id,
		Target:   protocol.TargetUnit,
		Payload:  payload,
	}
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"status": "ok", "opcode": 54, "x": body.X, "y": body.Y})
}

func (s *Server) handleSetFullState(w http.ResponseWriter, r *http.Request) {
	id, err := s.parseUnitID(r)
	if err != nil {
		s.writeError(w, err.Error(), http.StatusBadRequest)
		return
	}
	var body struct {
		Dimmer int `json:"dimmer"` // 0-255
		Hue    int `json:"hue"`    // 0-1023
		Sat    int `json:"sat"`    // 0-255
		White  int `json:"white"`  // 0-63 (white color balance)
		Temp   int `json:"temp"`   // 0-255 (color temperature)
	}
	body.Dimmer = 255
	body.Hue = 0
	body.Sat = 255
	body.White = 0
	body.Temp = 127
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	state := protocol.PackState5ch(uint8(body.Dimmer), uint16(body.Hue), uint8(body.Sat), uint8(body.White), uint8(body.Temp))
	log.Printf("SetFullState unit %d: dimmer=%d hue=%d sat=%d white=%d temp=%d → %x",
		id, body.Dimmer, body.Hue, body.Sat, body.White, body.Temp, state)
	cmd := protocol.NewSetRawStateCommand(state, id, protocol.TargetUnit, s.nextSeq())
	if err := s.conn.SendCommand(cmd); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]any{"status": "ok", "opcode": 48, "state": fmt.Sprintf("%x", state)})
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

	// Wake reactive Hue lights via REST — the Entertainment API streams RGB
	// to lights that are already on but does not turn them on. Fire all wakes
	// concurrently and don't block the handler; if the bridge is slow or
	// unreachable, the handler still returns immediately and streaming will
	// reach any lights that come online.
	if s.hue != nil {
		for _, lightID := range s.reactive.HueTargets() {
			go func(id string) {
				if err := s.hue.SetState(id, map[string]any{
					"on":             true,
					"bri":             100,
					"transitiontime": 0,
				}); err != nil {
					log.Printf("Hue wake %s failed: %v", id, err)
				}
			}(lightID)
		}
	}

	// Start Hue Entertainment streaming if available
	if s.HueStreamer != nil {
		if err := s.HueStreamer.Start(); err != nil {
			log.Printf("Hue Entertainment start error (falling back to REST): %v", err)
		} else {
			// Swap the reactive engine's Hue setter to use streaming
			s.reactive.SetHueSetter(s.HueStreamer)
		}
	}

	if err := s.reactive.Start(); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) handleReactiveStop(w http.ResponseWriter, r *http.Request) {
	s.reactive.Stop()

	// Stop Hue Entertainment streaming
	if s.HueStreamer != nil && s.HueStreamer.IsActive() {
		s.HueStreamer.Stop()
		// Restore REST API for manual control
		if s.hue != nil {
			s.reactive.SetHueSetter(s.hue)
		}
	}

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

// ── Settings Handlers ─────────────────────────────────────

// handleGetConfig returns the config, with the reactive light targets
// overlaid with what the engine is actually driving right now (they can
// differ when the config leaves them empty and defaults were applied).
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.opts.Config == nil {
		s.writeError(w, "settings unavailable (no config attached)", http.StatusServiceUnavailable)
		return
	}
	view := *s.opts.Config
	view.Reactive.CasambiUnits = s.reactive.CasambiTargets()
	view.Reactive.HueLights = s.reactive.HueTargets()
	s.writeJSON(w, view)
}

// handleUpdateConfig merges the request body over the current config,
// persists it, and applies what it can without a restart (reactive light
// targets, Spotify URL). Server bind/port and Hue bridge changes are saved
// but only take effect on restart.
func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.opts.Config == nil {
		s.writeError(w, "settings unavailable (no config attached)", http.StatusServiceUnavailable)
		return
	}

	updated := *s.opts.Config
	if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
		s.writeError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if updated.Server.Port < 1 || updated.Server.Port > 65535 {
		s.writeError(w, "server.port must be 1-65535", http.StatusBadRequest)
		return
	}
	if updated.Server.Bind == "" {
		updated.Server.Bind = "127.0.0.1"
	}

	restart := updated.Server != s.opts.Config.Server ||
		!reflect.DeepEqual(updated.Hue, s.opts.Config.Hue)

	if err := updated.Save(s.opts.ConfigPath); err != nil {
		s.writeError(w, "save config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	*s.opts.Config = updated

	// Live-apply what doesn't need a restart.
	s.reactive.SetTargets(updated.Reactive.CasambiUnits, updated.Reactive.HueLights)
	s.reactive.SetSpotifyURL(updated.Spotify.NowPlayingURL)

	log.Printf("Config updated via API (restart required: %v)", restart)
	s.writeJSON(w, map[string]any{"status": "saved", "restartRequired": restart})
}

// ── Hue Handlers ──────────────────────────────────────────

// requireHue writes a 503 and returns false when no Hue bridge is configured.
func (s *Server) requireHue(w http.ResponseWriter) bool {
	if s.hue == nil {
		s.writeError(w, "Hue bridge not configured — run 'casambi-go setup'", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (s *Server) handleListHueLights(w http.ResponseWriter, r *http.Request) {
	if !s.requireHue(w) {
		return
	}
	lights, err := s.hue.ListLights()
	if err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, lights)
}

func (s *Server) handleHueLightOn(w http.ResponseWriter, r *http.Request) {
	if !s.requireHue(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.hue.SetState(id, map[string]any{"on": true, "bri": 254}); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHueLightOff(w http.ResponseWriter, r *http.Request) {
	if !s.requireHue(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.hue.SetState(id, map[string]any{"on": false}); err != nil {
		s.writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleHueLightColor(w http.ResponseWriter, r *http.Request) {
	if !s.requireHue(w) {
		return
	}
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
	if !s.requireHue(w) {
		return
	}
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
