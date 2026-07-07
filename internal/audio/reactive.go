package audio

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/cmplx"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
	"github.com/migueltejeda/casambi-go/internal/analyzer"
)

// HueSetter abstracts Hue bridge control.
type HueSetter interface {
	SetState(lightID string, state map[string]any) error
}

// CasambiSender abstracts Casambi BLE control.
type CasambiSender interface {
	SendCommand(cmd any) error
}

// ReactiveEngine captures audio from the microphone and drives lights
// based on real-time audio analysis: volume, bass energy, and frequency content.
type ReactiveEngine struct {
	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	lightCh chan lightUpdate // capture loop → light-writer goroutine

	hue              HueSetter
	casambiSend      func(level uint8, unitID uint16)
	casambiState     func(state []byte, unitID uint16)
	casambiColor     func(hue uint16, sat uint8, unitID uint16)
	casambiFullState func(dimmer uint8, hue uint16, sat uint8, white uint8, temp uint8, unitID uint16)
	CasambiUnits     []uint16
	HueLights        []string

	// Audio state (read by status endpoint)
	rms       float64 // overall volume 0.0-1.0
	bass      float64 // bass energy 0.0-1.0
	mid       float64 // mid energy 0.0-1.0
	treble    float64 // treble energy 0.0-1.0
	beatCount int
	onBeat    bool

	// Beat detection state — spectral flux based
	prevSpectrum [trebleMaxBin]float64 // previous frame's spectrum for flux calculation
	fluxHistory  [43]float64          // ~1 second of spectral flux at 43 fps
	fluxIdx      int
	lastBeatTime time.Time

	// Predictive beat timing
	beatIntervals [8]float64 // recent beat-to-beat intervals for BPM estimation
	beatIntIdx    int
	detectedBPM   float64    // BPM estimated from audio beat detection
	nextBeatTime  time.Time  // predicted time of next beat

	// Smoothing
	smoothBri float64 // smoothed brightness for Casambi

	// Command throttling — don't flood BLE
	lastCasambiBri     uint8
	lastHueBri         int
	lastCmdTime        time.Time
	lastBeatPulseTime  time.Time // last on-beat color shift for Casambi

	// Gain — multiplier for mic sensitivity. Higher = brighter.
	Gain float64

	// Auto-gain: tracks signal level and adapts automatically
	autoGain      bool
	rmsHistory    [172]float64 // ~4 seconds of RMS values for normalization
	rmsIdx        int
	peakRMS       float64 // rolling peak for normalization
	effectiveGain float64 // the computed gain value

	// Album art colors
	colors    *DominantColors
	trackName string

	// Spotify integration for album art + track info. The poller runs for
	// the server's lifetime (not the engine's) so the dashboard shows the
	// current track even when reactive mode is off.
	spotifyURL       string
	pollerOnce       sync.Once
	bpm              float64
	progress         int       // ms from Spotify
	lastPollProgress int
	lastPollTime     time.Time
	lastBeatNum      int
	paused           bool // true when Spotify progress_ms hasn't advanced between polls

	// Track profiling — local replacement for Spotify's dead
	// audio-features API. The first time a track plays, its audio is fed
	// to an analyzer.Session; the resulting profile (BPM, beat phase,
	// energy, mood) is cached so later plays get it instantly.
	profiles     *analyzer.Store        // persistent profile cache (nil = profiling disabled)
	profiler     *analyzer.Session      // in-flight analysis for the current track
	profile      *analyzer.TrackProfile // profile of the current track, if known
	currentTrack string
	artistName   string
	OnProfile    func(*analyzer.TrackProfile) // optional hook, called after a new profile is saved

	// AI enrichment (optional): Claude adds genre, mood, and lighting
	// direction to each profile; the engine translates that into how the
	// lights behave for this song.
	enricher  *analyzer.Enricher
	enriching map[string]bool // tracks with an enrichment call in flight

	// Per-song lighting behavior, set from the AI's lighting direction
	// (defaults when no enrichment is available).
	pulseDecay    float64       // brightness decay factor: lower = harder pulses
	intensityBias float64       // overall brightness multiplier (0.5-1.5)
	beatPulseGap  time.Duration // min gap between on-beat color pulses

	// Beat grid (see beatgrid.go): with a full profile, beats are computed
	// from BPM/phase + Spotify progress instead of detected from onsets,
	// and fired early to cover light-transport latency.
	beatSource     string        // "auto", "mic", or "grid"
	beatLead       time.Duration // fire grid beats this early
	micFree        bool          // drive lights from the profile alone when possible
	gridActive     bool          // grid currently owns beat timing
	syntheticDrive bool          // mic-free: grid loop renders ambient frames too
	lastGridFire   time.Time

	// lightMu serializes updateLights — both the capture loop and the grid
	// loop call it, and it mutates smoothing/throttle state.
	lightMu sync.Mutex
}

const (
	sampleRate = 44100
	bufSize    = 1024 // ~23ms per frame at 44100Hz
	fftSize    = 1024

	// Frequency band boundaries (FFT bin indices for 44100Hz, 1024 samples)
	// Each bin = 44100/1024 ≈ 43 Hz
	bassMaxBin   = 5   // 0-215 Hz
	midMinBin    = 5   // 215 Hz
	midMaxBin    = 30  // 1290 Hz
	trebleMinBin = 30  // 1290 Hz
	trebleMaxBin = 100 // 4300 Hz
)

// openBlackHoleStream finds the BlackHole 2ch device and opens it as a portaudio input stream.
// This captures system audio digitally instead of using the mic.
func openBlackHoleStream(buf []float32) (*portaudio.Stream, []float32, error) {
	devices, err := portaudio.Devices()
	if err != nil {
		return nil, nil, err
	}
	log.Println("Available audio input devices:")
	for _, d := range devices {
		if d.MaxInputChannels > 0 {
			log.Printf("  - %q (%d ch)", d.Name, d.MaxInputChannels)
		}
	}
	for _, d := range devices {
		if d.MaxInputChannels > 0 && (d.Name == "BlackHole 2ch" || d.Name == "BlackHole2ch") {
			log.Printf("Using BlackHole device: %s (%d input channels)", d.Name, d.MaxInputChannels)
			// BlackHole is stereo — capture both channels and mix to mono in the read loop
			stereoBuf := make([]float32, bufSize*2) // interleaved L,R samples
			params := portaudio.StreamParameters{
				Input: portaudio.StreamDeviceParameters{
					Device:   d,
					Channels: 2,
					Latency:  d.DefaultLowInputLatency,
				},
				SampleRate:      sampleRate,
				FramesPerBuffer: bufSize,
			}
			stream, err := portaudio.OpenStream(params, stereoBuf)
			if err != nil {
				return nil, nil, err
			}
			// Return the stereo buffer — caller will mix to mono
			return stream, stereoBuf, nil
		}
	}
	return nil, nil, fmt.Errorf("BlackHole 2ch device not found")
}

func NewReactiveEngine(hue HueSetter, casambiSend func(level uint8, unitID uint16), casambiState func(state []byte, unitID uint16), casambiColor func(hue uint16, sat uint8, unitID uint16), casambiFullState func(dimmer uint8, hue uint16, sat uint8, white uint8, temp uint8, unitID uint16), spotifyURL string, casambiUnits []uint16, hueLights []string) *ReactiveEngine {
	return &ReactiveEngine{
		hue:              hue,
		casambiSend:      casambiSend,
		casambiState:     casambiState,
		casambiColor:     casambiColor,
		casambiFullState: casambiFullState,
		CasambiUnits:     casambiUnits,
		HueLights:        hueLights,
		Gain:             10,
		autoGain:         true,
		effectiveGain:    10,
		spotifyURL:       spotifyURL,
		bpm:              120,
		pulseDecay:       0.5,
		intensityBias:    1.0,
		beatPulseGap:     450 * time.Millisecond,
		beatSource:       "auto",
		beatLead:         defaultBeatLeadMS * time.Millisecond,
	}
}

func (re *ReactiveEngine) Status() map[string]any {
	re.mu.Lock()
	defer re.mu.Unlock()
	st := map[string]any{
		"running":       re.running,
		"gain":          math.Round(re.effectiveGain*10) / 10,
		"autoGain":      re.autoGain,
		"bpm":           math.Round(re.detectedBPM*10) / 10,
		"rms":           math.Round(re.rms*re.effectiveGain*1000) / 1000,
		"bass":          math.Round(re.bass*re.effectiveGain*1000) / 1000,
		"mid":           math.Round(re.mid*re.effectiveGain*1000) / 1000,
		"treble":        math.Round(re.treble*re.effectiveGain*1000) / 1000,
		"beats":     re.beatCount,
		"onBeat":    re.onBeat,
		"track":     re.trackName,
		"artist":    re.artistName,
		"progress":  re.estimateProgress(),
		"paused":    re.paused,
		"colors":    re.colors,
		"profile":   re.profile,
		"analyzing": re.profiler != nil,
	}
	if re.profiler != nil {
		st["analyzedSeconds"] = math.Round(re.profiler.Seconds()*10) / 10
		st["targetSeconds"] = analyzer.TargetSeconds
	}

	// Beat grid: report what's timing the beats. Grid beats are instants,
	// not frames, so hold onBeat long enough for the ~12 Hz SSE stream.
	st["beatSource"] = "mic"
	if re.gridActive {
		st["beatSource"] = "grid"
		if re.profile != nil {
			st["bpm"] = re.profile.BPM // the BPM actually driving the lights
			st["micBPM"] = math.Round(re.detectedBPM*10) / 10
		}
	}
	st["micFree"] = re.syntheticDrive
	if !re.lastGridFire.IsZero() && time.Since(re.lastGridFire) < 150*time.Millisecond {
		st["onBeat"] = true
	}
	return st
}

// SetAlbumColors fetches album art and extracts dominant colors for the light palette.
func (re *ReactiveEngine) SetAlbumColors(trackName, imageURL string) {
	re.mu.Lock()
	if trackName == re.trackName {
		re.mu.Unlock()
		return // same track, skip
	}
	re.trackName = trackName
	re.mu.Unlock()

	colors, err := ExtractColorsFromURL(imageURL)
	if err != nil {
		log.Printf("Album color extraction failed: %v", err)
		return
	}

	re.mu.Lock()
	re.colors = colors
	running := re.running
	re.mu.Unlock()
	log.Printf("Album colors: %s → primary hue=%d sat=%d, secondary hue=%d sat=%d",
		trackName, colors.Primary.Hue, colors.Primary.Sat, colors.Secondary.Hue, colors.Secondary.Sat)

	// Push the new primary color to Casambi units once via SetColor (opcode 7).
	// Per-beat SetLevel handles brightness; color only changes on track change,
	// so back-to-back state writes (which the fixture silently drops) are avoided.
	// Only while reactive mode is running — the poller alone must not touch lights.
	if running {
		go re.pushCasambiColor(colors.Primary)
	}
}

// pushCasambiColor sends a SetColor (opcode 7) packet to each Casambi unit with
// a small inter-unit gap so the fixture doesn't drop one of two rapid writes.
func (re *ReactiveEngine) pushCasambiColor(c HSV) {
	if re.casambiColor == nil {
		return
	}
	hue1023 := uint16(float64(c.Hue) / 65535.0 * 1023)
	sat255 := uint8(float64(c.Sat) / 254.0 * 255)
	for i, unitID := range re.CasambiTargets() {
		if i > 0 {
			time.Sleep(80 * time.Millisecond)
		}
		re.casambiColor(hue1023, sat255, unitID)
	}
}

// beatColorPulse flashes Casambi units to a hue ~90° off the album primary,
// then reverts. Color shifts are more visible than brightness changes on the
// LINE 5xDIM, which has internal slew limiting on its dimmer channel.
// Spaced internally so the fixture doesn't drop any of the writes.
func (re *ReactiveEngine) beatColorPulse(c HSV) {
	if re.casambiColor == nil {
		return
	}
	primaryHue := uint16(float64(c.Hue) / 65535.0 * 1023)
	primarySat := uint8(float64(c.Sat) / 254.0 * 255)
	// Shift hue ~90° (256 of 1024 = 90° of 360°)
	shiftedHue := uint16((int(primaryHue) + 256) % 1024)

	units := re.CasambiTargets()
	// Pulse to shifted color
	for i, unitID := range units {
		if i > 0 {
			time.Sleep(80 * time.Millisecond)
		}
		re.casambiColor(shiftedHue, primarySat, unitID)
	}
	// Hold the flash briefly
	time.Sleep(180 * time.Millisecond)
	// Revert to album primary
	for i, unitID := range units {
		if i > 0 {
			time.Sleep(80 * time.Millisecond)
		}
		re.casambiColor(primaryHue, primarySat, unitID)
	}
}

func (re *ReactiveEngine) estimateProgress() int {
	if re.lastPollTime.IsZero() {
		return re.progress
	}
	elapsed := time.Since(re.lastPollTime).Milliseconds()
	return re.lastPollProgress + int(elapsed)
}

func (re *ReactiveEngine) SetBPM(bpm float64) {
	re.mu.Lock()
	re.bpm = bpm
	re.mu.Unlock()
}

func (re *ReactiveEngine) SetGain(gain float64) {
	re.mu.Lock()
	re.Gain = gain
	re.autoGain = false // manual gain disables auto-gain
	re.effectiveGain = gain
	re.mu.Unlock()
}

func (re *ReactiveEngine) SetHueSetter(hue HueSetter) {
	re.mu.Lock()
	re.hue = hue
	re.mu.Unlock()
}

// SetProfileStore enables first-play track profiling backed by the store.
func (re *ReactiveEngine) SetProfileStore(store *analyzer.Store) {
	re.mu.Lock()
	re.profiles = store
	re.mu.Unlock()
}

// SetEnricher enables AI enrichment of track profiles.
func (re *ReactiveEngine) SetEnricher(e *analyzer.Enricher) {
	re.mu.Lock()
	re.enricher = e
	re.mu.Unlock()
}

// onTrackChange runs when the Spotify poller sees a new track: apply a
// cached profile if one exists, otherwise start analyzing this play.
func (re *ReactiveEngine) onTrackChange(track, artist string) {
	re.mu.Lock()
	re.currentTrack = track
	re.artistName = artist
	// Reset per-song lighting behavior until this track's profile says otherwise
	re.pulseDecay = 0.5
	re.intensityBias = 1.0
	re.beatPulseGap = 450 * time.Millisecond
	prev := re.profiler
	re.profiler = nil
	re.profile = nil
	store := re.profiles
	running := re.running
	progress := re.estimateProgress()
	re.mu.Unlock()

	// A session from the previous track may already have enough audio.
	if prev != nil {
		if prev.Ready() {
			go re.finalizeProfiler(prev)
		} else if t, _ := prev.Track(); t != "" {
			log.Printf("Discarding partial analysis of %q (%.0fs — need %.0fs)", t, prev.Seconds(), analyzer.MinSeconds)
		}
	}

	if store == nil {
		return
	}

	p := store.Get(track, artist)
	if p != nil {
		re.applyProfile(p)
	}
	// Analyze when there's no profile, or only a BPM-only import from the
	// legacy tap-tempo cache (a full analysis upgrades it).
	if running && (p == nil || p.Source == "legacy-bpm-cache") {
		log.Printf("Analyzing %q — profile ready after ~%.0fs of playback", track, analyzer.TargetSeconds)
		sess := analyzer.NewSession(track, artist, progress)
		re.mu.Lock()
		re.profiler = sess
		re.mu.Unlock()
	}
}

// applyProfile makes a cached/new profile the engine's current one and
// seeds beat prediction with its BPM.
func (re *ReactiveEngine) applyProfile(p *analyzer.TrackProfile) {
	re.mu.Lock()
	re.profile = p
	re.bpm = p.BPM
	if re.detectedBPM == 0 {
		re.detectedBPM = p.BPM // predictive beats work from the first bar
	}
	re.mu.Unlock()

	if p.Source == "legacy-bpm-cache" {
		log.Printf("Track profile (BPM only, legacy): %s — %.0f BPM", p.Track, p.BPM)
	} else {
		log.Printf("Track profile: %s — %.1f BPM, %s, energy %.2f, dance %.2f, valence %.2f",
			p.Track, p.BPM, p.Key, p.Energy, p.Danceability, p.Valence)
	}

	if p.AI != nil {
		re.applyEnrichment(p.AI)
	} else {
		re.maybeEnrich(p)
	}
}

// maybeEnrich asks Claude about the track (async) unless enrichment is
// disabled, already present, or already in flight.
func (re *ReactiveEngine) maybeEnrich(p *analyzer.TrackProfile) {
	if p == nil || p.AI != nil || p.Source == "legacy-bpm-cache" {
		return // legacy entries lack DSP data; a replay upgrades them first
	}
	k := strings.ToLower(p.Track + " — " + p.Artist)
	re.mu.Lock()
	enricher := re.enricher
	if enricher == nil || re.enriching[k] {
		re.mu.Unlock()
		return
	}
	if re.enriching == nil {
		re.enriching = make(map[string]bool)
	}
	re.enriching[k] = true
	re.mu.Unlock()

	go func() {
		defer func() {
			re.mu.Lock()
			delete(re.enriching, k)
			re.mu.Unlock()
		}()

		ai, err := enricher.Enrich(p)
		if err != nil {
			log.Printf("AI enrichment failed for %q: %v", p.Track, err)
			return
		}

		enriched := *p
		enriched.AI = ai

		re.mu.Lock()
		store := re.profiles
		isCurrent := re.currentTrack == enriched.Track
		if isCurrent {
			re.profile = &enriched
		}
		re.mu.Unlock()

		if store != nil {
			store.Set(&enriched)
		}
		log.Printf("AI enrichment: %s — genre %v, moods %v, pulse %q, known=%v",
			enriched.Track, ai.Genre, ai.Moods, ai.PulseStyle, ai.KnownTrack)

		if isCurrent {
			re.applyEnrichment(ai)
		}
	}()
}

// applyEnrichment translates the AI's lighting direction into engine
// behavior: pulse hardness, overall intensity, and a palette fallback for
// tracks whose album art yielded no colors.
func (re *ReactiveEngine) applyEnrichment(ai *analyzer.Enrichment) {
	if ai == nil {
		return
	}
	decay, gap := 0.5, 450*time.Millisecond
	switch ai.PulseStyle {
	case "strobe":
		decay, gap = 0.35, 300*time.Millisecond
	case "pulse":
		decay, gap = 0.5, 450*time.Millisecond
	case "wash":
		decay, gap = 0.7, 700*time.Millisecond
	case "breathe":
		decay, gap = 0.85, 900*time.Millisecond
	}
	bias := 1.0
	if ai.IntensityBias != 0 {
		bias = ai.IntensityBias
	}

	re.mu.Lock()
	re.pulseDecay = decay
	re.beatPulseGap = gap
	re.intensityBias = bias
	haveColors := re.colors != nil
	running := re.running
	re.mu.Unlock()

	if ai.Notes != "" {
		log.Printf("Lighting direction: %s", ai.Notes)
	}

	// Album art colors stay authoritative; the AI palette covers tracks
	// where art extraction failed or produced nothing.
	if !haveColors && len(ai.Palette) > 0 {
		colors := paletteToColors(ai.Palette)
		re.mu.Lock()
		re.colors = colors
		re.mu.Unlock()
		log.Printf("Using AI palette: primary hue=%d sat=%d", colors.Primary.Hue, colors.Primary.Sat)
		if running {
			go re.pushCasambiColor(colors.Primary)
		}
	}
}

// paletteToColors converts AI palette colors (degrees, 0-1 sat) into the
// engine's Hue-scale DominantColors.
func paletteToColors(palette []analyzer.PaletteColor) *DominantColors {
	toHSV := func(c analyzer.PaletteColor) HSV {
		return HSV{
			Hue: int(c.Hue / 360 * 65535),
			Sat: int(c.Sat * 254),
			Val: 254,
		}
	}
	colors := &DominantColors{Primary: toHSV(palette[0]), Secondary: toHSV(palette[0])}
	// Prefer the explicitly-tagged roles when present
	for _, c := range palette {
		if c.Role == "primary" {
			colors.Primary = toHSV(c)
		}
	}
	for _, c := range palette {
		if c.Role == "secondary" || c.Role == "accent" {
			colors.Secondary = toHSV(c)
			break
		}
	}
	if len(palette) > 1 && colors.Secondary == colors.Primary {
		colors.Secondary = toHSV(palette[1])
	}
	return colors
}

// EnrichCurrent re-runs AI enrichment for the current track's profile.
func (re *ReactiveEngine) EnrichCurrent() error {
	re.mu.Lock()
	p := re.profile
	enricher := re.enricher
	re.mu.Unlock()
	if enricher == nil {
		return fmt.Errorf("AI enrichment not configured — set ai.anthropic_api_key in config.yaml")
	}
	if p == nil {
		return fmt.Errorf("no profile for the current track yet")
	}
	fresh := *p
	fresh.AI = nil
	re.mu.Lock()
	re.profile = &fresh
	re.mu.Unlock()
	re.maybeEnrich(&fresh)
	return nil
}

// finalizeProfiler computes, saves, and applies a finished analysis session.
func (re *ReactiveEngine) finalizeProfiler(sess *analyzer.Session) {
	profile, err := sess.Finalize()
	if err != nil {
		log.Printf("Track analysis failed: %v", err)
		return
	}

	re.mu.Lock()
	store := re.profiles
	current := re.currentTrack
	hook := re.OnProfile
	re.mu.Unlock()

	if store != nil {
		store.Set(profile)
	}
	log.Printf("Analyzed %q: %.1f BPM (conf %.2f), %s, energy %.2f, dance %.2f, valence %.2f [%.0fs of audio]",
		profile.Track, profile.BPM, profile.TempoConfidence, profile.Key,
		profile.Energy, profile.Danceability, profile.Valence, profile.AnalyzedSeconds)

	if profile.Track == current {
		re.applyProfile(profile) // also kicks off AI enrichment
	} else {
		re.maybeEnrich(profile) // enrich the saved profile for next play
	}
	if hook != nil {
		hook(profile)
	}
}

// maybeStartProfiling begins analysis for the current track when reactive
// mode starts mid-song and no (full) profile exists yet.
func (re *ReactiveEngine) maybeStartProfiling() {
	re.mu.Lock()
	track, artist := re.currentTrack, re.artistName
	store := re.profiles
	inFlight := re.profiler != nil
	progress := re.estimateProgress()
	re.mu.Unlock()

	if store == nil || track == "" || inFlight {
		return
	}
	if p := store.Get(track, artist); p != nil && p.Source != "legacy-bpm-cache" {
		return
	}
	log.Printf("Analyzing %q — profile ready after ~%.0fs of playback", track, analyzer.TargetSeconds)
	sess := analyzer.NewSession(track, artist, progress)
	re.mu.Lock()
	re.profiler = sess
	re.mu.Unlock()
}

// AnalyzerStatus reports profiling progress and the current track profile.
func (re *ReactiveEngine) AnalyzerStatus() map[string]any {
	re.mu.Lock()
	prof := re.profiler
	profile := re.profile
	track, artist := re.currentTrack, re.artistName
	store := re.profiles
	re.mu.Unlock()

	st := map[string]any{
		"enabled":   store != nil,
		"track":     track,
		"artist":    artist,
		"analyzing": prof != nil,
		"profile":   profile,
	}
	if prof != nil {
		st["analyzed_seconds"] = math.Round(prof.Seconds()*10) / 10
		st["target_seconds"] = analyzer.TargetSeconds
	}
	if store != nil {
		st["cached_profiles"] = store.Len()
	}
	return st
}

// ReanalyzeCurrent drops the current track's cached profile and analyzes
// this play from scratch.
func (re *ReactiveEngine) ReanalyzeCurrent() error {
	re.mu.Lock()
	track, artist := re.currentTrack, re.artistName
	store := re.profiles
	running := re.running
	progress := re.estimateProgress()
	re.mu.Unlock()

	if store == nil {
		return fmt.Errorf("track profiling not configured")
	}
	if track == "" {
		return fmt.Errorf("no track playing")
	}
	store.Delete(track, artist)
	if !running {
		return fmt.Errorf("profile cleared — start reactive mode so audio can be analyzed")
	}
	sess := analyzer.NewSession(track, artist, progress)
	re.mu.Lock()
	re.profile = nil
	re.profiler = sess
	re.mu.Unlock()
	log.Printf("Re-analyzing %q", track)
	return nil
}

// StartSpotifyPoller launches the now-playing poller for the process
// lifetime. Safe to call more than once; the loop idles while no URL is set.
func (re *ReactiveEngine) StartSpotifyPoller() {
	re.pollerOnce.Do(func() { go re.pollSpotify() })
}

// SetSpotifyURL updates the now-playing endpoint at runtime.
func (re *ReactiveEngine) SetSpotifyURL(url string) {
	re.mu.Lock()
	re.spotifyURL = url
	re.mu.Unlock()
}

// SetTargets replaces the set of lights reactive mode drives.
func (re *ReactiveEngine) SetTargets(casambiUnits []uint16, hueLights []string) {
	re.mu.Lock()
	re.CasambiUnits = append([]uint16(nil), casambiUnits...)
	re.HueLights = append([]string(nil), hueLights...)
	re.mu.Unlock()
}

// CasambiTargets returns a copy of the Casambi units reactive mode drives.
func (re *ReactiveEngine) CasambiTargets() []uint16 {
	re.mu.Lock()
	defer re.mu.Unlock()
	return append([]uint16(nil), re.CasambiUnits...)
}

// HueTargets returns a copy of the Hue lights reactive mode drives.
func (re *ReactiveEngine) HueTargets() []string {
	re.mu.Lock()
	defer re.mu.Unlock()
	return append([]string(nil), re.HueLights...)
}

func (re *ReactiveEngine) SetAutoGain(enabled bool) {
	re.mu.Lock()
	re.autoGain = enabled
	if enabled {
		re.peakRMS = 0
		re.rmsIdx = 0
	}
	re.mu.Unlock()
}

func (re *ReactiveEngine) Start() error {
	re.mu.Lock()
	if re.running {
		re.mu.Unlock()
		return nil
	}
	// Reset adaptive state so a stop/start cycle gets a fresh adaptation
	// window. Without this, peakRMS, beat history, and pause flags persist
	// from the previous session and the new run starts with stale calibration.
	re.peakRMS = 0
	re.rmsIdx = 0
	re.rmsHistory = [172]float64{}
	re.fluxIdx = 0
	re.fluxHistory = [43]float64{}
	re.prevSpectrum = [trebleMaxBin]float64{}
	re.beatIntIdx = 0
	re.beatIntervals = [8]float64{}
	re.detectedBPM = 0
	re.nextBeatTime = time.Time{}
	re.lastBeatTime = time.Time{}
	re.smoothBri = 0
	re.lastCasambiBri = 0
	re.lastHueBri = 0
	re.lastCmdTime = time.Time{}
	re.lastBeatPulseTime = time.Time{}
	re.beatCount = 0
	re.paused = false
	re.lastPollProgress = 0
	re.gridActive = false
	re.syntheticDrive = false
	re.lastGridFire = time.Time{}

	re.running = true
	re.stopCh = make(chan struct{})
	re.lightCh = make(chan lightUpdate, 1)
	colors := re.colors
	re.mu.Unlock()

	// The poller may already know the track (it runs even while stopped) —
	// apply its album color now, since color pushes are gated on running.
	if colors != nil {
		go re.pushCasambiColor(colors.Primary)
	}

	go re.lightWriter(re.stopCh, re.lightCh)
	go re.gridLoop(re.stopCh)
	go re.run()

	// If the poller already knows the track and it has no profile yet,
	// analyze this play.
	re.maybeStartProfiling()
	return nil
}

func (re *ReactiveEngine) Stop() {
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.running {
		close(re.stopCh)
		re.running = false
		re.gridActive = false
		re.syntheticDrive = false
	}
}

func (re *ReactiveEngine) run() {
	if err := portaudio.Initialize(); err != nil {
		log.Printf("portaudio init error: %v", err)
		return
	}
	defer portaudio.Terminate()

	buf := make([]float32, bufSize)

	// Try BlackHole first for digital audio, auto-fallback to mic if silent
	stream, stereoBuf, err := openBlackHoleStream(buf)
	useBlackHole := err == nil
	if useBlackHole {
		// Test for ~0.5 seconds — if all silence, BlackHole isn't receiving audio
		if err := stream.Start(); err != nil {
			log.Printf("BlackHole start error: %v", err)
			useBlackHole = false
		} else {
			silent := true
			for i := 0; i < 20; i++ { // 20 frames ≈ 0.5 seconds
				if err := stream.Read(); err != nil {
					break
				}
				for _, s := range stereoBuf {
					if s != 0 {
						silent = false
						break
					}
				}
				if !silent {
					break
				}
			}
			if silent {
				log.Println("BlackHole detected but signal is silent (audio likely on AirPlay/Sonos)")
				stream.Stop()
				stream.Close()
				useBlackHole = false
			} else {
				stream.Stop() // will be restarted below
				log.Println("BlackHole has signal — using digital audio capture")
			}
		}
	}

	if !useBlackHole {
		stereoBuf = nil
		log.Println("Using default mic for audio capture")
		stream, err = portaudio.OpenDefaultStream(1, 0, sampleRate, bufSize, buf)
		if err != nil {
			log.Printf("open audio stream error: %v", err)
			return
		}
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		log.Printf("start audio stream error: %v", err)
		return
	}
	defer stream.Stop()

	log.Println("Audio-reactive engine started (hybrid: Spotify timing + mic intensity)")

	for {
		select {
		case <-re.stopCh:
			log.Println("Audio-reactive engine stopped")
			return
		default:
		}

		if err := stream.Read(); err != nil {
			continue
		}

		// If using BlackHole (stereo), mix down to mono
		if stereoBuf != nil {
			for i := 0; i < bufSize; i++ {
				buf[i] = (stereoBuf[i*2] + stereoBuf[i*2+1]) / 2
			}
		}

		// Convert to float64 for analysis
		samples := make([]float64, len(buf))
		for i, s := range buf {
			samples[i] = float64(s)
		}

		// Feed the track profiler (first-play analysis). Skipped while
		// Spotify is paused so silence doesn't skew the profile.
		re.mu.Lock()
		prof := re.profiler
		profPaused := re.paused
		re.mu.Unlock()
		if prof != nil && !profPaused {
			prof.Feed(samples)
			if prof.Done() {
				re.mu.Lock()
				re.profiler = nil
				re.mu.Unlock()
				go re.finalizeProfiler(prof)
			}
		}

		// Analyze audio for intensity/frequency
		rms := computeRMS(samples)
		spectrum := fft(samples)
		bass, mid, treble := bandsFromSpectrum(spectrum)

		// Build magnitude spectrum for beat detection
		specMag := make([]float64, trebleMaxBin)
		for i := 0; i < trebleMaxBin && i < len(spectrum)/2; i++ {
			specMag[i] = cmplx.Abs(spectrum[i])
		}

		// Auto-gain: track signal level and compute gain to normalize
		re.mu.Lock()
		if re.autoGain {
			re.rmsHistory[re.rmsIdx%len(re.rmsHistory)] = rms
			re.rmsIdx++

			// Find peak RMS over the last ~4 seconds
			var peak float64
			count := min(re.rmsIdx, len(re.rmsHistory))
			for i := 0; i < count; i++ {
				if re.rmsHistory[i] > peak {
					peak = re.rmsHistory[i]
				}
			}

			// Slow decay on peak so gain doesn't jump around
			if peak > re.peakRMS {
				re.peakRMS = peak
			} else {
				re.peakRMS = re.peakRMS*0.999 + peak*0.001 // very slow decay
			}

			// Target: peak RMS should map to ~0.8 brightness (leave headroom for beats)
			if re.peakRMS > 0.0001 {
				re.effectiveGain = 0.8 / re.peakRMS
				// Clamp to reasonable range
				re.effectiveGain = clamp(re.effectiveGain, 5, 500)
			}
		} else {
			re.effectiveGain = re.Gain
		}
		re.mu.Unlock()

		// Beat detection: spectral flux based with predictive timing
		// Silence gate: ignore background noise / ambient room sound
		isBeat := false
		if rms > 0.0002 { // low threshold for mic picking up speakers across the room
			isBeat = re.detectBeat(specMag)
		}

		re.mu.Lock()
		re.rms = rms
		re.bass = bass
		re.mid = mid
		re.treble = treble
		gridOn := re.gridActive
		synthetic := re.syntheticDrive
		gain := re.effectiveGain
		paused := re.paused
		if gridOn {
			// The beat grid owns beat timing — mic onsets would double-fire.
			// detectBeat still ran above, so detectedBPM keeps tracking for
			// the dashboard comparison and for instant fallback.
			isBeat = false
		}
		re.onBeat = isBeat
		if isBeat {
			re.beatCount++
		}
		re.mu.Unlock()

		// Drive lights — only when there's actual audio AND Spotify isn't
		// paused. In mic-free (synthetic) drive the grid loop renders all
		// frames; mic audio then only feeds the profiler above.
		if !synthetic && rms > 0.0002 && !paused {
			re.updateLights(rms*gain, bass*gain, mid*gain, treble*gain, isBeat)
		}
	}
}

func (re *ReactiveEngine) pollSpotify() {
	type spotifyResponse struct {
		Data struct {
			ProgressMS int `json:"progress_ms"`
			Item       struct {
				Name  string `json:"name"`
				Album struct {
					Images []struct {
						URL    string `json:"url"`
						Height int    `json:"height"`
					} `json:"images"`
					Artists []struct {
						Name string `json:"name"`
					} `json:"artists"`
				} `json:"album"`
			} `json:"item"`
		} `json:"data"`
	}

	// Runs for the process lifetime (see StartSpotifyPoller); idles when no
	// URL is configured so the URL can be set later via the settings API.
	var lastTrack string
	for {
		re.mu.Lock()
		url := re.spotifyURL
		re.mu.Unlock()
		if url == "" {
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := http.Get(url)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		var np spotifyResponse
		json.NewDecoder(resp.Body).Decode(&np)
		resp.Body.Close()

		if np.Data.ProgressMS > 0 {
			re.mu.Lock()
			// Pause detection: progress_ms is ms-precise; identical polls = paused
			wasPaused := re.paused
			re.paused = re.lastPollProgress != 0 && np.Data.ProgressMS == re.lastPollProgress
			re.lastPollProgress = np.Data.ProgressMS
			re.lastPollTime = time.Now()
			re.progress = np.Data.ProgressMS
			paused := re.paused
			re.mu.Unlock()
			if paused && !wasPaused {
				log.Println("Spotify paused — holding lights")
			} else if !paused && wasPaused {
				log.Println("Spotify resumed")
			}
		}

		trackName := np.Data.Item.Name
		if trackName != "" && trackName != lastTrack {
			lastTrack = trackName
			log.Printf("Now playing: %s", trackName)

			artistName := ""
			if len(np.Data.Item.Album.Artists) > 0 {
				artistName = np.Data.Item.Album.Artists[0].Name
			}

			// Apply a cached track profile or start analyzing this play
			re.onTrackChange(trackName, artistName)

			// Extract album art colors
			var imageURL string
			for _, img := range np.Data.Item.Album.Images {
				if imageURL == "" || img.Height < 400 {
					imageURL = img.URL
				}
			}
			if imageURL != "" {
				re.SetAlbumColors(trackName, imageURL)
			}
		}

		time.Sleep(2 * time.Second)
	}
}

func (re *ReactiveEngine) detectBeat(spectrum []float64) bool {
	// Spectral flux: measure how much the LOW FREQUENCY spectrum changed
	// Only look at bass bins (0-215 Hz) — catches kick drums, not hi-hats
	var flux float64
	for i := 0; i < bassMaxBin && i < len(spectrum); i++ {
		diff := spectrum[i] - re.prevSpectrum[i]
		if diff > 0 { // only positive changes (energy increases = onsets)
			flux += diff * diff // square to emphasize strong onsets
		}
		re.prevSpectrum[i] = spectrum[i]
	}
	// Still track the rest of the spectrum for prevSpectrum updates
	for i := bassMaxBin; i < len(re.prevSpectrum) && i < len(spectrum); i++ {
		re.prevSpectrum[i] = spectrum[i]
	}

	// Store flux in circular buffer
	re.fluxHistory[re.fluxIdx%len(re.fluxHistory)] = flux
	re.fluxIdx++

	// Need at least half a second of history
	if re.fluxIdx < len(re.fluxHistory)/2 {
		return false
	}

	// Adaptive threshold: mean + 1.5 * stddev of recent flux
	count := min(re.fluxIdx, len(re.fluxHistory))
	var avg float64
	for i := 0; i < count; i++ {
		avg += re.fluxHistory[i]
	}
	avg /= float64(count)

	var variance float64
	for i := 0; i < count; i++ {
		diff := re.fluxHistory[i] - avg
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / float64(count))

	threshold := avg + stddev*2.0
	now := time.Now()

	// Check for predictive beat: if we have a reliable BPM, fire slightly early
	predictedBeat := false
	if re.detectedBPM > 30 && !re.nextBeatTime.IsZero() {
		// Fire 30ms early to compensate for BLE latency
		if now.Add(30 * time.Millisecond).After(re.nextBeatTime) && now.Sub(re.lastBeatTime) > 100*time.Millisecond {
			predictedBeat = true
		}
	}

	// Detected beat: flux spike above threshold + minimum interval (350ms = max ~170 BPM)
	detectedBeat := flux > threshold && now.Sub(re.lastBeatTime) > 350*time.Millisecond

	if detectedBeat || predictedBeat {
		// Track beat intervals for BPM estimation
		if !re.lastBeatTime.IsZero() {
			interval := now.Sub(re.lastBeatTime).Seconds()
			if interval > 0.2 && interval < 2.0 { // 30-300 BPM range
				re.beatIntervals[re.beatIntIdx%len(re.beatIntervals)] = interval
				re.beatIntIdx++

				// Estimate BPM from median interval
				if re.beatIntIdx >= 4 {
					re.detectedBPM = 60.0 / re.medianInterval()
				}
			}
		}

		re.lastBeatTime = now

		// Predict next beat
		if re.detectedBPM > 30 {
			beatDur := time.Duration(60.0 / re.detectedBPM * float64(time.Second))
			re.nextBeatTime = now.Add(beatDur)
		}

		return true
	}
	return false
}

func (re *ReactiveEngine) medianInterval() float64 {
	count := min(re.beatIntIdx, len(re.beatIntervals))
	vals := make([]float64, count)
	copy(vals, re.beatIntervals[:count])
	// Simple sort for small array
	for i := 0; i < len(vals); i++ {
		for j := i + 1; j < len(vals); j++ {
			if vals[j] < vals[i] {
				vals[i], vals[j] = vals[j], vals[i]
			}
		}
	}
	return vals[len(vals)/2]
}

// updateLights turns one frame of gain-normalized levels into light
// commands. Called from the capture loop and (on grid beats / mic-free
// frames) the grid loop — lightMu serializes the smoothing and throttle
// state they share.
func (re *ReactiveEngine) updateLights(rms, bass, mid, treble float64, isBeat bool) {
	re.lightMu.Lock()
	defer re.lightMu.Unlock()

	// Snapshot light targets (mutable via settings) and the per-song
	// lighting behavior (set by AI enrichment)
	re.mu.Lock()
	casambiUnits := append([]uint16(nil), re.CasambiUnits...)
	hueLights := append([]string(nil), re.HueLights...)
	pulseDecay := re.pulseDecay
	intensityBias := re.intensityBias
	beatPulseGap := re.beatPulseGap
	re.mu.Unlock()

	// Casambi: hold a low ambient baseline so the beat spike to 255 is dramatic.
	// The 5-channel fixture perceptually compresses brightness — going from 200
	// to 255 reads as "no change". Going from ~60 to 255 reads as a clear pulse.
	targetBri := clamp(rms*120*intensityBias, 0, 200) // ambient ceiling well below max
	if isBeat {
		targetBri = clamp(255*intensityBias, 0, 255)
	}
	// Asymmetric smoothing: instant attack on beats, tunable decay for
	// contrast. The AI's pulse style sets the decay: strobe = hard drops,
	// breathe = slow ambient swell.
	if targetBri > re.smoothBri {
		re.smoothBri = targetBri // instant jump up
	} else {
		re.smoothBri = re.smoothBri*pulseDecay + targetBri*(1-pulseDecay)
	}
	casambiBri := uint8(clamp(re.smoothBri, 0, 255))

	re.mu.Lock()
	colors := re.colors
	re.mu.Unlock()

	// Frequency balance for Hue color blending
	total := bass + mid + treble

	// Casambi: per-beat we only send SetLevel (1-byte payload, opcode 1).
	// Color is pushed once on track change via pushCasambiColor → SetColor
	// (opcode 7). Sending state-changing packets back-to-back caused the
	// fixture to silently drop one of them.
	briDelta := int(casambiBri) - int(re.lastCasambiBri)
	if briDelta < 0 {
		briDelta = -briDelta
	}
	now := time.Now()
	shouldSendCasambi := isBeat || briDelta > 15 || now.Sub(re.lastCmdTime) > 100*time.Millisecond
	if shouldSendCasambi && re.casambiSend != nil {
		re.lastCasambiBri = casambiBri
		re.lastCmdTime = now
	} else {
		shouldSendCasambi = false
	}

	// On-beat color flash: shift hue ~90° from album primary and revert.
	// More visible than brightness pulses on the LINE 5xDIM's slew-limited
	// dimmer channel. Debounced (gap set by the AI's pulse style) to keep
	// pulses distinct and avoid stacking goroutines at fast BPM.
	if isBeat && colors != nil && now.Sub(re.lastBeatPulseTime) > beatPulseGap {
		re.lastBeatPulseTime = now
		go re.beatColorPulse(colors.Primary)
	}

	// Hue: use album art colors, blend between primary (bass) and secondary (treble)
	primary := HSV{Hue: 0, Sat: 200, Val: 254}       // red
	secondary := HSV{Hue: 43690, Sat: 200, Val: 254}  // blue
	if colors != nil {
		primary = colors.Primary
		secondary = colors.Secondary
	}

	// Blend between primary and secondary — primary dominates
	if total < 0.001 {
		return // silence
	}
	primaryWeight := 0.8
	if total > 0.001 {
		bassRatio := bass / total
		primaryWeight = 0.7 + bassRatio*0.3
	}
	hueColor := int(float64(primary.Hue)*primaryWeight + float64(secondary.Hue)*(1-primaryWeight))
	hueSat := int(float64(primary.Sat)*primaryWeight + float64(secondary.Sat)*(1-primaryWeight))
	hueBri := int(clamp(rms*254*intensityBias, 5, 254))

	if isBeat {
		hueBri = int(clamp(254*intensityBias, 5, 254))
		hueSat = min(hueSat+50, 254)
		hueColor = primary.Hue // snap to primary color on beat
	}

	// Hue transition: instant on beat, smooth fade otherwise
	// Let the Hue bridge handle interpolation
	transition := 3 // 300ms smooth fade
	if isBeat {
		transition = 0 // instant
	}

	// Throttle Hue too: only on beats or significant change
	hueBriDelta := hueBri - re.lastHueBri
	if hueBriDelta < 0 {
		hueBriDelta = -hueBriDelta
	}
	re.mu.Lock()
	hue := re.hue
	re.mu.Unlock()
	shouldSendHue := hue != nil && (isBeat || hueBriDelta > 20 || now.Sub(re.lastCmdTime) > 150*time.Millisecond)
	if shouldSendHue {
		re.lastHueBri = hueBri
	}

	// Hand the actual I/O (BLE writes with inter-unit gaps, Hue sends) to the
	// light-writer goroutine. The capture loop must never sleep or block:
	// audio arrives every ~23ms and dropped frames starve beat detection and
	// the track analyzer (analysis crawled at ~10% of real time before this).
	if shouldSendCasambi || shouldSendHue {
		re.pushLights(lightUpdate{
			sendCasambi:  shouldSendCasambi,
			casambiBri:   casambiBri,
			casambiUnits: casambiUnits,
			beatSync:     isBeat && shouldSendCasambi,
			sendHue:      shouldSendHue,
			hue:          hue,
			hueLights:    hueLights,
			hueColor:     hueColor % 65536,
			hueSat:       hueSat,
			hueBri:       hueBri,
			transition:   transition,
		})
	}
}

// lightUpdate is one frame of light intent, computed by updateLights and
// executed by the light-writer goroutine.
type lightUpdate struct {
	sendCasambi  bool
	casambiBri   uint8
	casambiUnits []uint16
	beatSync     bool // pause between Casambi and Hue so the beat lands together
	sendHue      bool
	hue          HueSetter
	hueLights    []string
	hueColor     int
	hueSat       int
	hueBri       int
	transition   int
}

// pushLights hands an update to the writer, keeping only the newest frame
// when the writer is mid-send (latest-wins, never blocks the capture loop).
func (re *ReactiveEngine) pushLights(u lightUpdate) {
	re.mu.Lock()
	ch := re.lightCh
	re.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- u:
	default:
		select {
		case <-ch: // drop the stale frame
		default:
		}
		select {
		case ch <- u:
		default:
		}
	}
}

// lightWriter performs the slow light I/O — BLE writes with the inter-unit
// spacing the fixture needs, the Casambi→Hue beat-sync gap, and Hue sends —
// off the audio capture path.
func (re *ReactiveEngine) lightWriter(stopCh chan struct{}, ch chan lightUpdate) {
	for {
		select {
		case <-stopCh:
			return
		case u := <-ch:
			if u.sendCasambi && re.casambiSend != nil {
				for i, unitID := range u.casambiUnits {
					if i > 0 {
						time.Sleep(25 * time.Millisecond) // space writes across units
					}
					re.casambiSend(u.casambiBri, unitID)
				}
			}
			// Small delay before Hue to let Casambi commands get a head start:
			// BLE write takes ~50-100ms, Hue UDP ~40ms, so this syncs the beat.
			if u.beatSync {
				time.Sleep(50 * time.Millisecond)
			}
			if u.sendHue && u.hue != nil {
				for _, lightID := range u.hueLights {
					go u.hue.SetState(lightID, map[string]any{
						"on":             true,
						"hue":            u.hueColor,
						"sat":            u.hueSat,
						"bri":            u.hueBri,
						"transitiontime": u.transition,
					})
				}
			}
		}
	}
}

// ── Audio Analysis ────────────────────────────────────────

func computeRMS(samples []float64) float64 {
	var sum float64
	for _, s := range samples {
		sum += s * s
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func computeFrequencyBands(samples []float64) (bass, mid, treble float64) {
	spectrum := fft(samples)
	return bandsFromSpectrum(spectrum)
}

func bandsFromSpectrum(spectrum []complex128) (bass, mid, treble float64) {
	n := len(spectrum) / 2

	var bassSum, midSum, trebleSum float64
	for i := 1; i < n && i < trebleMaxBin; i++ {
		mag := cmplx.Abs(spectrum[i])
		if i < bassMaxBin {
			bassSum += mag
		} else if i >= midMinBin && i < midMaxBin {
			midSum += mag
		} else if i >= trebleMinBin && i < trebleMaxBin {
			trebleSum += mag
		}
	}

	// Normalize
	scale := float64(n)
	return bassSum / scale, midSum / scale, trebleSum / scale
}

// fft computes a simple radix-2 FFT. Input length must be a power of 2.
func fft(x []float64) []complex128 {
	n := len(x)
	if n == 1 {
		return []complex128{complex(x[0], 0)}
	}

	even := make([]float64, n/2)
	odd := make([]float64, n/2)
	for i := 0; i < n/2; i++ {
		even[i] = x[2*i]
		odd[i] = x[2*i+1]
	}

	fEven := fft(even)
	fOdd := fft(odd)

	result := make([]complex128, n)
	for k := 0; k < n/2; k++ {
		w := cmplx.Exp(complex(0, -2*math.Pi*float64(k)/float64(n)))
		result[k] = fEven[k] + w*fOdd[k]
		result[k+n/2] = fEven[k] - w*fOdd[k]
	}
	return result
}

func hsvToRGB(h, s, v float64) (r, g, b float64) {
	if s == 0 {
		return v, v, v
	}
	h = math.Mod(h, 360) / 60
	i := math.Floor(h)
	f := h - i
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	switch int(i) {
	case 0:
		return v, t, p
	case 1:
		return q, v, p
	case 2:
		return p, v, t
	case 3:
		return p, q, v
	case 4:
		return t, p, v
	default:
		return v, p, q
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
