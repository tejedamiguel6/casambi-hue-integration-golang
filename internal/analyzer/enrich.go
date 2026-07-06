package analyzer

// AI enrichment: the DSP analyzer measures what's physically in the audio
// (BPM, key, energy); this file adds what a music-knowledgeable model knows
// about the song — genre, mood, structure, and lighting direction. The two
// are complementary: DSP numbers are exact but shallow, the model's read is
// rich but approximate, and the profile stores both.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// DefaultModel balances cost and quality for one small call per new track.
const DefaultModel = "claude-haiku-4-5"

const anthropicURL = "https://api.anthropic.com/v1/messages"

// PaletteColor is one AI-suggested light color.
type PaletteColor struct {
	Name string  `json:"name"`           // e.g. "deep violet"
	Hue  float64 `json:"hue"`            // degrees 0-360
	Sat  float64 `json:"sat"`            // 0-1
	Role string  `json:"role,omitempty"` // "primary", "secondary", "accent"
}

// Section is a rough song-structure landmark ("drop", "bridge", ...).
// Timestamps come from the model's knowledge of the song and are approximate.
type Section struct {
	AtMS  int    `json:"at_ms"`
	Label string `json:"label"`          // "intro", "build", "drop", "bridge", "outro"
	Note  string `json:"note,omitempty"` // lighting direction for this section
}

// Enrichment is the AI layer of a track profile.
type Enrichment struct {
	KnownTrack bool     `json:"known_track"` // false = model didn't recognize it, derived from DSP only
	Genre      []string `json:"genre,omitempty"`
	Moods      []string `json:"moods,omitempty"`

	// Refined scores (0-1) blending the model's knowledge of the song with
	// the DSP measurements. Zero values mean "not provided".
	Valence      float64 `json:"valence,omitempty"`
	Danceability float64 `json:"danceability,omitempty"`
	Energy       float64 `json:"energy,omitempty"`

	// Lighting direction
	Palette       []PaletteColor `json:"palette,omitempty"`
	PulseStyle    string         `json:"pulse_style,omitempty"`    // "strobe", "pulse", "wash", "breathe"
	IntensityBias float64        `json:"intensity_bias,omitempty"` // 0.5-1.5 brightness multiplier
	Notes         string         `json:"notes,omitempty"`          // one-line lighting direction

	Structure []Section `json:"structure,omitempty"`

	Model      string    `json:"model,omitempty"`
	EnrichedAt time.Time `json:"enriched_at,omitempty"`
}

// Enricher calls the Claude API to enrich track profiles.
type Enricher struct {
	APIKey string
	Model  string
	URL    string // overridable for tests; defaults to the Anthropic API
	Client *http.Client
}

// NewEnricher builds an Enricher; model "" selects DefaultModel.
func NewEnricher(apiKey, model string) *Enricher {
	if model == "" {
		model = DefaultModel
	}
	return &Enricher{
		APIKey: apiKey,
		Model:  model,
		URL:    anthropicURL,
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

const enrichSystemPrompt = `You are the music director for a beat-synced stage-lighting rig driving Casambi and Philips Hue fixtures. Given a track name, artist, and DSP measurements of the actual audio, return lighting-relevant knowledge about the song as STRICT JSON (no markdown, no prose) with this shape:

{
  "known_track": bool,        // do you actually recognize this specific song?
  "genre": ["..."],           // 1-3 tags, most specific first
  "moods": ["..."],           // 2-5 adjectives, e.g. "dark", "euphoric", "hypnotic"
  "valence": 0.0-1.0,         // musical positivity, refined from your knowledge
  "danceability": 0.0-1.0,
  "energy": 0.0-1.0,
  "palette": [                // 2-3 colors that fit the song's character
    {"name": "...", "hue": 0-360, "sat": 0.0-1.0, "role": "primary|secondary|accent"}
  ],
  "pulse_style": "strobe|pulse|wash|breathe",  // strobe = hard EDM hits, pulse = clear beats, wash = smooth blend, breathe = slow ambient swell
  "intensity_bias": 0.5-1.5,  // overall brightness multiplier for this song
  "notes": "...",             // one sentence of lighting direction
  "structure": [              // only if you genuinely know this song's arrangement; else []
    {"at_ms": 72000, "label": "drop", "note": "full brightness, snap to accent color"}
  ]
}

Rules:
- If you do not recognize the track, set known_track=false, structure=[], and derive everything else from the DSP measurements and genre hints in the metadata alone. Never invent structure timestamps.
- Trust the DSP BPM and key over your memory — they were measured from the actual audio.
- Palette hues are color-wheel degrees: 0=red, 60=yellow, 120=green, 240=blue, 280=purple.
- Output ONLY the JSON object.`

// Enrich asks the model about the track and returns the parsed enrichment.
func (e *Enricher) Enrich(p *TrackProfile) (*Enrichment, error) {
	if e.APIKey == "" {
		return nil, fmt.Errorf("no API key configured")
	}

	user := fmt.Sprintf(
		`Track: %q by %q

DSP measurements of the actual audio:
- BPM: %.1f (confidence %.2f)
- Key: %s
- Energy: %.2f (0-1), mean loudness %.1f dBFS, dynamics %.2f (0-1)
- Brightness (spectral centroid): %.2f (0-1)
- Heuristic danceability: %.2f, heuristic valence: %.2f
- Analyzed from %.0fs of audio`,
		p.Track, p.Artist, p.BPM, p.TempoConfidence, p.Key,
		p.Energy, p.LoudnessDB, p.Dynamics, p.Brightness,
		p.Danceability, p.Valence, p.AnalyzedSeconds)

	reqBody, err := json.Marshal(map[string]any{
		"model":      e.Model,
		"max_tokens": 1024,
		"system":     enrichSystemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", e.URL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", e.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode API response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error (%s): %s", apiResp.Error.Type, apiResp.Error.Message)
	}
	var text string
	for _, c := range apiResp.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return nil, fmt.Errorf("empty API response (HTTP %d)", resp.StatusCode)
	}

	enr, err := parseEnrichment(text)
	if err != nil {
		return nil, err
	}
	enr.Model = e.Model
	enr.EnrichedAt = time.Now()
	return enr, nil
}

// parseEnrichment extracts and validates the JSON object from model output,
// tolerating markdown fences or stray prose around it.
func parseEnrichment(text string) (*Enrichment, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in model output")
	}
	var enr Enrichment
	if err := json.Unmarshal([]byte(text[start:end+1]), &enr); err != nil {
		return nil, fmt.Errorf("parse enrichment JSON: %w", err)
	}

	// Clamp everything the lighting engine consumes.
	enr.Valence = clamp01(enr.Valence)
	enr.Danceability = clamp01(enr.Danceability)
	enr.Energy = clamp01(enr.Energy)
	switch enr.PulseStyle {
	case "strobe", "pulse", "wash", "breathe":
	default:
		enr.PulseStyle = ""
	}
	if enr.IntensityBias != 0 {
		enr.IntensityBias = clampRange(enr.IntensityBias, 0.5, 1.5)
	}
	palette := enr.Palette[:0]
	for _, c := range enr.Palette {
		if c.Hue < 0 || c.Hue > 360 {
			c.Hue = math.Mod(math.Mod(c.Hue, 360)+360, 360)
		}
		c.Sat = clamp01(c.Sat)
		palette = append(palette, c)
	}
	enr.Palette = palette
	structure := enr.Structure[:0]
	for _, sec := range enr.Structure {
		if sec.AtMS >= 0 && sec.Label != "" {
			structure = append(structure, sec)
		}
	}
	enr.Structure = structure
	// A model that doesn't know the track must not claim structure knowledge.
	if !enr.KnownTrack {
		enr.Structure = nil
	}
	return &enr, nil
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
