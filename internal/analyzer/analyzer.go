// Package analyzer builds per-track audio profiles — tempo, beat grid,
// energy, and mood — from raw PCM captured while a song plays.
//
// It replaces Spotify's dead audio-features/audio-analysis APIs: the first
// time a track plays, the reactive engine feeds its (BlackHole or mic) audio
// here; after enough material has accumulated the profile is computed once
// and cached, so every later play has instant BPM, beat phase, energy, and
// mood without listening again.
package analyzer

import (
	"fmt"
	"math"
	"sync"
)

const (
	// SampleRate is the PCM rate the analyzer expects (mono float64).
	SampleRate = 44100

	// Onset envelope: 1024-sample frames, 512 hop → ~86 fps.
	onsetFrame = 1024
	onsetHop   = 512

	// Chroma: 4096-sample frames give 10.8 Hz bins — enough to resolve
	// semitones above ~200 Hz for key/mode detection.
	chromaFrame = 4096
	chromaHop   = 2048

	// MinSeconds is the least audio Finalize accepts.
	MinSeconds = 20.0
	// TargetSeconds is when callers should finalize for a good profile.
	TargetSeconds = 45.0
	// maxSeconds caps memory; extra audio past this is ignored.
	maxSeconds = 90.0

	onsetFPS = float64(SampleRate) / float64(onsetHop)
)

// Session accumulates audio for one track and computes its profile.
type Session struct {
	mu sync.Mutex

	track           string
	artist          string
	startProgressMS int
	active          bool

	// Rolling PCM buffer with independent cursors for the two frame sizes.
	buf       []float64
	bufOffset int // absolute sample index of buf[0]
	onsetPos  int // absolute sample index of next onset frame
	chromaPos int // absolute sample index of next chroma frame
	total     int // absolute samples consumed

	// Onset analysis
	onsetPlan *fftPlan
	onsetTmp  []complex128
	onsetMag  []float64
	prevMag   []float64
	onsetEnv  []float64
	rmsPerFrm []float64
	centroids []float64

	// Chroma accumulation
	chromaPlan *fftPlan
	chromaTmp  []complex128
	chromaMag  []float64
	chroma     [12]float64
	chromaN    int
}

// NewSession starts profiling a track. progressMS is the Spotify playback
// position at the moment the first samples arrive — it anchors the beat
// grid to track time so beats can later be predicted from progress_ms alone.
func NewSession(track, artist string, progressMS int) *Session {
	return &Session{
		track:           track,
		artist:          artist,
		startProgressMS: progressMS,
		active:          true,
		onsetPlan:       newFFTPlan(onsetFrame),
		onsetTmp:        make([]complex128, onsetFrame),
		onsetMag:        make([]float64, onsetFrame/2),
		prevMag:         make([]float64, onsetFrame/2),
		chromaPlan:      newFFTPlan(chromaFrame),
		chromaTmp:       make([]complex128, chromaFrame),
		chromaMag:       make([]float64, chromaFrame/2),
	}
}

// Track returns the track/artist this session is profiling.
func (s *Session) Track() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.track, s.artist
}

// Active reports whether the session is still accepting audio.
func (s *Session) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Seconds returns how much audio has been analyzed so far.
func (s *Session) Seconds() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return float64(s.total) / SampleRate
}

// Ready reports whether enough audio has accumulated for a usable profile.
func (s *Session) Ready() bool { return s.Seconds() >= MinSeconds }

// Done reports whether the session has enough audio for a full profile.
func (s *Session) Done() bool { return s.Seconds() >= TargetSeconds }

// Feed appends mono 44.1 kHz samples and processes complete frames.
// Cheap enough to call from the real-time capture loop (~2 FFTs per call).
func (s *Session) Feed(samples []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || float64(s.total)/SampleRate >= maxSeconds {
		return
	}
	s.buf = append(s.buf, samples...)
	s.total += len(samples)

	end := s.bufOffset + len(s.buf)

	// Onset frames
	for s.onsetPos+onsetFrame <= end {
		start := s.onsetPos - s.bufOffset
		frame := s.buf[start : start+onsetFrame]
		s.processOnsetFrame(frame)
		s.onsetPos += onsetHop
	}
	// Chroma frames
	for s.chromaPos+chromaFrame <= end {
		start := s.chromaPos - s.bufOffset
		frame := s.buf[start : start+chromaFrame]
		s.processChromaFrame(frame)
		s.chromaPos += chromaHop
	}

	// Trim consumed prefix
	keepFrom := min(s.onsetPos, s.chromaPos)
	if drop := keepFrom - s.bufOffset; drop > 0 {
		s.buf = append(s.buf[:0], s.buf[drop:]...)
		s.bufOffset = keepFrom
	}
}

func (s *Session) processOnsetFrame(frame []float64) {
	s.onsetPlan.magnitudes(frame, s.onsetTmp, s.onsetMag)

	// Spectral flux (half-wave rectified) over the full band up to ~8 kHz,
	// plus RMS and spectral centroid for energy/brightness.
	maxBin := 8000 * onsetFrame / SampleRate
	var flux, num, den float64
	for i := 1; i < maxBin && i < len(s.onsetMag); i++ {
		d := s.onsetMag[i] - s.prevMag[i]
		if d > 0 {
			flux += d
		}
		freq := float64(i) * SampleRate / onsetFrame
		num += freq * s.onsetMag[i]
		den += s.onsetMag[i]
	}
	copy(s.prevMag, s.onsetMag)
	s.onsetEnv = append(s.onsetEnv, flux)

	var sum float64
	for _, v := range frame {
		sum += v * v
	}
	s.rmsPerFrm = append(s.rmsPerFrm, math.Sqrt(sum/float64(len(frame))))

	if den > 1e-9 {
		s.centroids = append(s.centroids, num/den)
	}
}

func (s *Session) processChromaFrame(frame []float64) {
	s.chromaPlan.magnitudes(frame, s.chromaTmp, s.chromaMag)

	// Map bins between 80 Hz and 5 kHz onto 12 pitch classes.
	minBin := 80 * chromaFrame / SampleRate
	maxBin := 5000 * chromaFrame / SampleRate
	for i := minBin; i < maxBin && i < len(s.chromaMag); i++ {
		freq := float64(i) * SampleRate / chromaFrame
		// Semitones above A440, folded to a pitch class (A=0 ... G#=11)
		semis := 12 * math.Log2(freq/440.0)
		pc := ((int(math.Round(semis)) % 12) + 12) % 12
		// 1/f weighting keeps the loud bass fundamentals from swamping
		// the harmonically-informative mids.
		s.chroma[pc] += s.chromaMag[i] * (440.0 / freq)
	}
	s.chromaN++
}

// Finalize computes the track profile and deactivates the session.
// Fails if fewer than MinSeconds of audio were fed.
func (s *Session) Finalize() (*TrackProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false

	secs := float64(s.total) / SampleRate
	if secs < MinSeconds {
		return nil, fmt.Errorf("only %.1fs of audio analyzed, need %.0fs", secs, MinSeconds)
	}

	tempo := estimateTempo(s.onsetEnv, onsetFPS)

	// Anchor the beat grid to track time: the phase offset is where the
	// first beat fell within the analyzed window, shifted by where in the
	// track the window started.
	period := 60000.0 / tempo.BPM
	phaseMS := tempo.PhaseFrames * 1000.0 / onsetFPS
	beatPhase := math.Mod(float64(s.startProgressMS)+phaseMS, period)

	key, mode, keyConf := detectKey(s.chroma)
	energy, loudness, dynamics := energyProfile(s.rmsPerFrm)
	brightness := brightnessScore(s.centroids)
	dance := danceability(tempo.BPM, tempo.Confidence, tempo.Regularity, energy)
	valence := valenceScore(mode, tempo.BPM, brightness, energy)

	return &TrackProfile{
		Track:           s.track,
		Artist:          s.artist,
		BPM:             math.Round(tempo.BPM*10) / 10,
		BeatPhaseMS:     math.Round(beatPhase),
		TempoConfidence: round3(tempo.Confidence),
		Energy:          round3(energy),
		LoudnessDB:      math.Round(loudness*10) / 10,
		Dynamics:        round3(dynamics),
		Danceability:    round3(dance),
		Valence:         round3(valence),
		Key:             key,
		Mode:            mode,
		KeyConfidence:   round3(keyConf),
		Brightness:      round3(brightness),
		AnalyzedSeconds: math.Round(secs*10) / 10,
	}, nil
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
