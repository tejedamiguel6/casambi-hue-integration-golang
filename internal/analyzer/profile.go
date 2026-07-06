package analyzer

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// TrackProfile is the analyzed fingerprint of one track — the local
// replacement for Spotify's audio-features object.
type TrackProfile struct {
	Track  string `json:"track"`
	Artist string `json:"artist"`

	// Rhythm
	BPM             float64 `json:"bpm"`
	BeatPhaseMS     float64 `json:"beat_phase_ms"`    // first-beat offset within a beat period, relative to track time 0
	TempoConfidence float64 `json:"tempo_confidence"` // 0-1

	// Energy
	Energy     float64 `json:"energy"`      // 0-1
	LoudnessDB float64 `json:"loudness_db"` // mean RMS in dBFS
	Dynamics   float64 `json:"dynamics"`    // 0-1, loudness variation

	// Mood
	Danceability  float64 `json:"danceability"`   // 0-1
	Valence       float64 `json:"valence"`        // 0-1, musical positivity
	Key           string  `json:"key"`            // e.g. "F# minor"
	Mode          string  `json:"mode"`           // "major" / "minor"
	KeyConfidence float64 `json:"key_confidence"` // 0-1
	Brightness    float64 `json:"brightness"`     // 0-1, spectral centroid

	AnalyzedSeconds float64   `json:"analyzed_seconds"`
	AnalyzedAt      time.Time `json:"analyzed_at"`
	Source          string    `json:"source"` // "audio-analysis" or "legacy-bpm-cache"

	// AI is the Claude-generated layer: genre, mood, refined scores, and
	// lighting direction. nil until enrichment runs (requires an API key).
	AI *Enrichment `json:"ai,omitempty"`
}

// Store persists track profiles to a JSON file. It transparently imports
// entries from the old bpm_cache.json as BPM-only profiles, so nothing
// learned via tap-tempo is lost.
type Store struct {
	mu       sync.Mutex
	path     string
	profiles map[string]*TrackProfile // "track — artist" (lowercased) → profile
}

// LoadStore reads the profile store at path. legacyBPMPath (optional) points
// at the old bpm_cache.json; its entries are imported once as BPM-only
// profiles unless a full profile already exists.
func LoadStore(path, legacyBPMPath string) *Store {
	s := &Store{path: path, profiles: make(map[string]*TrackProfile)}

	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &s.profiles); err != nil {
			log.Printf("Track profile store %s unreadable (%v) — starting fresh", path, err)
			s.profiles = make(map[string]*TrackProfile)
		}
	}

	if legacyBPMPath != "" {
		s.importLegacy(legacyBPMPath)
	}

	if len(s.profiles) > 0 {
		log.Printf("Loaded %d track profiles", len(s.profiles))
	}
	return s
}

func (s *Store) importLegacy(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var entries map[string]float64
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	imported := 0
	for k, bpm := range entries {
		key := strings.ToLower(k)
		if _, exists := s.profiles[key]; exists || bpm <= 0 {
			continue
		}
		track, artist := k, ""
		if parts := strings.SplitN(k, " — ", 2); len(parts) == 2 {
			track, artist = parts[0], parts[1]
		}
		s.profiles[key] = &TrackProfile{
			Track:  track,
			Artist: artist,
			BPM:    bpm,
			Source: "legacy-bpm-cache",
		}
		imported++
	}
	if imported > 0 {
		log.Printf("Imported %d BPM entries from legacy cache %s", imported, path)
		s.saveLocked()
	}
}

func key(track, artist string) string {
	return strings.ToLower(track + " — " + artist)
}

// Get returns the profile for a track, or nil.
func (s *Store) Get(track, artist string) *TrackProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profiles[key(track, artist)]
}

// Set saves a profile (stamping AnalyzedAt/Source) and persists the store.
func (s *Store) Set(p *TrackProfile) {
	if p == nil {
		return
	}
	if p.Source == "" {
		p.Source = "audio-analysis"
	}
	if p.AnalyzedAt.IsZero() {
		p.AnalyzedAt = time.Now()
	}
	s.mu.Lock()
	s.profiles[key(p.Track, p.Artist)] = p
	s.saveLocked()
	s.mu.Unlock()
}

// Delete removes a track's profile (used by re-analyze). Reports whether
// an entry existed.
func (s *Store) Delete(track, artist string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(track, artist)
	if _, ok := s.profiles[k]; !ok {
		return false
	}
	delete(s.profiles, k)
	s.saveLocked()
	return true
}

// All returns a copy of every stored profile.
func (s *Store) All() map[string]*TrackProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*TrackProfile, len(s.profiles))
	for k, v := range s.profiles {
		out[k] = v
	}
	return out
}

// Len returns the number of stored profiles.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.profiles)
}

func (s *Store) saveLocked() {
	data, err := json.MarshalIndent(s.profiles, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(s.path, data, 0600); err != nil {
		log.Printf("Failed to save track profiles: %v", err)
	}
}
