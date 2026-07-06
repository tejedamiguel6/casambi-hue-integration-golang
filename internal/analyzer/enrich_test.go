package analyzer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseEnrichment(t *testing.T) {
	// Typical clean response
	raw := `{
		"known_track": true,
		"genre": ["melodic techno"],
		"moods": ["dark", "hypnotic"],
		"valence": 0.3,
		"danceability": 0.85,
		"energy": 0.75,
		"palette": [
			{"name": "deep violet", "hue": 280, "sat": 0.9, "role": "primary"},
			{"name": "ice blue", "hue": 200, "sat": 0.7, "role": "accent"}
		],
		"pulse_style": "pulse",
		"intensity_bias": 1.1,
		"notes": "Keep it moody; save full brightness for the drop.",
		"structure": [{"at_ms": 92000, "label": "drop", "note": "snap to accent"}]
	}`
	enr, err := parseEnrichment(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !enr.KnownTrack || enr.Genre[0] != "melodic techno" || enr.PulseStyle != "pulse" {
		t.Errorf("basic fields wrong: %+v", enr)
	}
	if len(enr.Structure) != 1 || enr.Structure[0].AtMS != 92000 {
		t.Errorf("structure wrong: %+v", enr.Structure)
	}

	// Markdown fences + prose around the JSON
	fenced := "Here is the analysis:\n```json\n" + raw + "\n```\nHope that helps!"
	if _, err := parseEnrichment(fenced); err != nil {
		t.Errorf("fenced parse: %v", err)
	}

	// Out-of-range values get clamped/dropped
	dirty := `{"known_track": false, "valence": 1.7, "intensity_bias": 9,
		"pulse_style": "explode",
		"palette": [{"name": "x", "hue": 400, "sat": 2.0}],
		"structure": [{"at_ms": 5000, "label": "drop"}]}`
	enr, err = parseEnrichment(dirty)
	if err != nil {
		t.Fatalf("dirty parse: %v", err)
	}
	if enr.Valence != 1.0 {
		t.Errorf("valence not clamped: %v", enr.Valence)
	}
	if enr.IntensityBias != 1.5 {
		t.Errorf("intensity bias not clamped: %v", enr.IntensityBias)
	}
	if enr.PulseStyle != "" {
		t.Errorf("invalid pulse style kept: %q", enr.PulseStyle)
	}
	if enr.Palette[0].Hue != 40 || enr.Palette[0].Sat != 1.0 {
		t.Errorf("palette not normalized: %+v", enr.Palette[0])
	}
	if enr.Structure != nil {
		t.Errorf("unknown track kept structure: %+v", enr.Structure)
	}

	// Garbage
	if _, err := parseEnrichment("I can't help with that."); err == nil {
		t.Error("expected error for non-JSON output")
	}
}

func TestEnrichAgainstMockAPI(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing api key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		json.NewDecoder(r.Body).Decode(&gotReq)
		json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"known_track": true, "genre": ["house"], "moods": ["warm"], "pulse_style": "wash", "valence": 0.8}`},
			},
		})
	}))
	defer srv.Close()

	e := NewEnricher("test-key", "")
	e.URL = srv.URL
	p := &TrackProfile{Track: "Test Song", Artist: "Test Artist", BPM: 124, Key: "F# minor", Energy: 0.7}

	enr, err := e.Enrich(p)
	if err != nil {
		t.Fatalf("Enrich: %v", err)
	}
	if enr.Genre[0] != "house" || enr.PulseStyle != "wash" || enr.Valence != 0.8 {
		t.Errorf("enrichment wrong: %+v", enr)
	}
	if enr.Model != DefaultModel {
		t.Errorf("model not stamped: %q", enr.Model)
	}
	if enr.EnrichedAt.IsZero() {
		t.Error("EnrichedAt not stamped")
	}

	// The request must carry the track identity and DSP measurements
	if gotReq["model"] != DefaultModel {
		t.Errorf("request model: %v", gotReq["model"])
	}
	msgs := gotReq["messages"].([]any)
	userContent := msgs[0].(map[string]any)["content"].(string)
	for _, want := range []string{"Test Song", "Test Artist", "124.0", "F# minor"} {
		if !contains(userContent, want) {
			t.Errorf("request missing %q", want)
		}
	}
}

func TestEnrichAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	}))
	defer srv.Close()

	e := NewEnricher("bad-key", "")
	e.URL = srv.URL
	if _, err := e.Enrich(&TrackProfile{Track: "x"}); err == nil {
		t.Error("expected error from API error response")
	}

	if _, err := NewEnricher("", "").Enrich(&TrackProfile{Track: "x"}); err == nil {
		t.Error("expected error with no API key")
	}
}

func TestProfileJSONRoundTripWithAI(t *testing.T) {
	p := &TrackProfile{
		Track: "t", Artist: "a", BPM: 120,
		AI: &Enrichment{KnownTrack: true, Genre: []string{"techno"}, PulseStyle: "strobe"},
	}
	data, _ := json.Marshal(p)
	var back TrackProfile
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.AI == nil || back.AI.Genre[0] != "techno" {
		t.Errorf("AI layer lost in round trip: %+v", back.AI)
	}

	// Profiles without AI must not serialize an "ai" key
	p.AI = nil
	data, _ = json.Marshal(p)
	if contains(string(data), `"ai"`) {
		t.Error("nil AI serialized")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
