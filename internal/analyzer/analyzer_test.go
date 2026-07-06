package analyzer

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// synth builds seconds of test audio at SampleRate.
func synth(seconds float64) []float64 {
	return make([]float64, int(seconds*SampleRate))
}

// addClickTrack overlays short noise-burst "drum hits" at the given BPM,
// starting at offsetMS into the buffer.
func addClickTrack(buf []float64, bpm, offsetMS float64) {
	period := 60.0 / bpm * SampleRate
	start := offsetMS / 1000 * SampleRate
	for pos := start; int(pos) < len(buf); pos += period {
		// 30ms decaying burst with a bassy 80 Hz thump + noise-ish partials
		for i := 0; i < SampleRate*30/1000 && int(pos)+i < len(buf); i++ {
			t := float64(i) / SampleRate
			envelope := math.Exp(-t * 60)
			s := 0.9*math.Sin(2*math.Pi*80*t) +
				0.3*math.Sin(2*math.Pi*913*t) +
				0.2*math.Sin(2*math.Pi*2731*t)
			buf[int(pos)+i] += envelope * s
		}
	}
}

// addChord overlays a sustained chord built from the given fundamental
// frequencies (with a couple of harmonics each).
func addChord(buf []float64, freqs []float64, amp float64) {
	for i := range buf {
		t := float64(i) / SampleRate
		var s float64
		for _, f := range freqs {
			s += math.Sin(2*math.Pi*f*t) + 0.4*math.Sin(2*math.Pi*2*f*t) + 0.15*math.Sin(2*math.Pi*3*f*t)
		}
		buf[i] += amp * s / float64(len(freqs))
	}
}

func analyze(t *testing.T, buf []float64) *TrackProfile {
	t.Helper()
	s := NewSession("test track", "test artist", 0)
	// Feed in capture-sized chunks like the real engine does
	for i := 0; i+1024 <= len(buf); i += 1024 {
		s.Feed(buf[i : i+1024])
	}
	p, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return p
}

func TestTempoDetection(t *testing.T) {
	for _, bpm := range []float64{90, 120, 128, 150, 174} {
		buf := synth(30)
		addClickTrack(buf, bpm, 0)
		p := analyze(t, buf)

		// Accept the estimate or a half/double-time folding of it.
		ok := false
		for _, target := range []float64{bpm, bpm / 2, bpm * 2} {
			if math.Abs(p.BPM-target) < 2.5 {
				ok = true
			}
		}
		if !ok {
			t.Errorf("BPM %.0f: got %.1f (confidence %.2f)", bpm, p.BPM, p.TempoConfidence)
		}
		if p.TempoConfidence < 0.1 {
			t.Errorf("BPM %.0f: confidence too low: %.2f", bpm, p.TempoConfidence)
		}
	}
}

func TestKeyDetectionMajorMinor(t *testing.T) {
	// C major: C E G (261.63, 329.63, 392.00)
	buf := synth(25)
	addChord(buf, []float64{261.63, 329.63, 392.00, 523.25}, 0.3)
	addClickTrack(buf, 120, 0) // some rhythm so tempo analysis has material
	p := analyze(t, buf)
	if p.Mode != "major" {
		t.Errorf("C major chord: detected %q (%s)", p.Key, p.Mode)
	}

	// A minor: A C E (220, 261.63, 329.63)
	buf = synth(25)
	addChord(buf, []float64{220, 261.63, 329.63, 440}, 0.3)
	addClickTrack(buf, 120, 0)
	p = analyze(t, buf)
	if p.Mode != "minor" {
		t.Errorf("A minor chord: detected %q (%s)", p.Key, p.Mode)
	}
}

func TestEnergyOrdering(t *testing.T) {
	loud := synth(25)
	addClickTrack(loud, 128, 0)
	addChord(loud, []float64{220, 330}, 0.5)

	quiet := synth(25)
	addChord(quiet, []float64{220, 330}, 0.02)
	addClickTrack(quiet, 128, 0) // clicks at full scale but sparse

	pl := analyze(t, loud)
	pq := analyze(t, quiet)
	if pl.Energy <= pq.Energy {
		t.Errorf("loud energy %.2f should exceed quiet %.2f", pl.Energy, pq.Energy)
	}
	if pl.LoudnessDB <= pq.LoudnessDB {
		t.Errorf("loud %.1f dB should exceed quiet %.1f dB", pl.LoudnessDB, pq.LoudnessDB)
	}
}

func TestValenceModeInfluence(t *testing.T) {
	major := valenceScore("major", 120, 0.5, 0.5)
	minor := valenceScore("minor", 120, 0.5, 0.5)
	if major <= minor {
		t.Errorf("major valence %.2f should exceed minor %.2f", major, minor)
	}
}

func TestFinalizeRejectsShortAudio(t *testing.T) {
	s := NewSession("x", "y", 0)
	s.Feed(synth(5))
	if _, err := s.Finalize(); err == nil {
		t.Error("expected error for 5s of audio")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := NewSession("track", "artist", 12000)
	if !s.Active() {
		t.Error("new session should be active")
	}
	buf := synth(22)
	addClickTrack(buf, 120, 0)
	for i := 0; i+1024 <= len(buf); i += 1024 {
		s.Feed(buf[i : i+1024])
	}
	if !s.Ready() {
		t.Errorf("22s fed, Ready() false (got %.1fs)", s.Seconds())
	}
	if s.Done() {
		t.Error("22s fed, Done() should need 45s")
	}
	p, err := s.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if s.Active() {
		t.Error("finalized session should be inactive")
	}
	if p.Track != "track" || p.Artist != "artist" {
		t.Errorf("profile identity: %q / %q", p.Track, p.Artist)
	}
	period := 60000.0 / p.BPM
	if p.BeatPhaseMS < 0 || p.BeatPhaseMS >= period {
		t.Errorf("beat phase %.0fms outside period %.0fms", p.BeatPhaseMS, period)
	}
}

func TestStoreRoundTripAndLegacyImport(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "profiles.json")
	legacyPath := filepath.Join(dir, "bpm_cache.json")
	os.WriteFile(legacyPath, []byte(`{"old song — old artist": 97.0}`), 0600)

	store := LoadStore(storePath, legacyPath)
	if p := store.Get("old song", "old artist"); p == nil || p.BPM != 97 || p.Source != "legacy-bpm-cache" {
		t.Fatalf("legacy import failed: %+v", p)
	}

	store.Set(&TrackProfile{Track: "New Song", Artist: "Artist", BPM: 124.5, Key: "F# minor"})

	// Reload from disk
	store2 := LoadStore(storePath, legacyPath)
	p := store2.Get("new song", "ARTIST") // case-insensitive
	if p == nil || p.BPM != 124.5 || p.Key != "F# minor" || p.Source != "audio-analysis" {
		t.Fatalf("round trip failed: %+v", p)
	}
	if store2.Len() != 2 {
		t.Errorf("expected 2 profiles, got %d", store2.Len())
	}
	if !store2.Delete("new song", "artist") {
		t.Error("delete failed")
	}
	if store2.Get("new song", "Artist") != nil {
		t.Error("profile still present after delete")
	}
}
