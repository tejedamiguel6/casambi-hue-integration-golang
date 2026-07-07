package audio

// Beat grid: once a track has a full analyzer profile, its beats are a known
// timetable — BPM plus a phase offset anchored to Spotify progress_ms. This
// file *computes* beat times from playback position instead of detecting
// them from microphone onsets, and fires each light command a configurable
// lead ahead of the audible beat so BLE/Hue transport latency lands the
// flash on the beat instead of after it.
//
// In mic-free mode the grid loop also renders the ambient frames between
// beats from the profile's energy, so lights stay musical with no usable
// audio capture at all. The mic keeps one job: analyzing tracks that don't
// have a profile yet.

import (
	"log"
	"math"
	"time"

	"github.com/migueltejeda/casambi-go/internal/analyzer"
)

const (
	// gridAutoMinConfidence gates "auto" beat source: below this tempo
	// confidence the profile's BPM is too shaky to extrapolate a whole
	// track from, and mic onsets keep driving beats. "grid" mode overrides.
	gridAutoMinConfidence = 0.08

	// gridMaxPollAge: if Spotify hasn't reported progress this recently,
	// the extrapolated playback position is too stale to trust.
	gridMaxPollAge = 10 * time.Second

	// defaultBeatLeadMS fires grid beats this many ms early — roughly one
	// Casambi BLE write round-trip — so the flash lands on the beat.
	defaultBeatLeadMS = 90

	// gridIdleInterval is how often the loop rechecks while the grid is
	// unusable (no profile, paused, mic mode, stale progress).
	gridIdleInterval = 150 * time.Millisecond

	// gridMaxSleep bounds each sleep so pause/track-change/settings changes
	// are picked up quickly even mid-bar.
	gridMaxSleep = 60 * time.Millisecond
)

// SetBeatConfig applies the beat-timing settings (config file / settings API).
// source: "auto" (grid when the profile is trustworthy), "mic", or "grid".
func (re *ReactiveEngine) SetBeatConfig(source string, leadMS int, micFree bool) {
	switch source {
	case "mic", "grid", "auto":
	default:
		source = "auto"
	}
	if leadMS <= 0 {
		leadMS = defaultBeatLeadMS
	}
	re.mu.Lock()
	re.beatSource = source
	re.beatLead = time.Duration(leadMS) * time.Millisecond
	re.micFree = micFree
	re.mu.Unlock()
}

// beatGridDelayMS returns milliseconds until the next beat for a track at
// position posMS, given the profile's BPM and beat phase offset.
func beatGridDelayMS(posMS, phaseMS, bpm float64) float64 {
	period := 60000.0 / bpm
	since := math.Mod(posMS-phaseMS, period)
	if since < 0 {
		since += period
	}
	return period - since
}

// syntheticLevels fabricates gain-normalized band levels from a profile's
// energy for mic-free driving. The split only affects Hue color blending.
func syntheticLevels(energy float64) (rms, bass, mid, treble float64) {
	if energy <= 0 {
		// Profiles analyzed from a quiet mic can report energy 0; a dead
		// floor would leave the lights ambient-dark for the whole song.
		energy = 0.5
	}
	rms = 0.35 + 0.45*energy
	return rms, rms * 0.5, rms * 0.3, rms * 0.2
}

// gridSnapshot captures everything one grid iteration needs under the lock.
type gridSnapshot struct {
	on          bool    // grid owns beat timing right now
	synthetic   bool    // mic-free: grid loop renders ambient frames too
	idleAmbient bool    // mic-free without a grid: hold a steady ambient level
	bpm         float64 // profile BPM driving the grid
	phaseMS     float64
	energy      float64
	posMS       float64 // estimated playback position
	lead        time.Duration
}

func (re *ReactiveEngine) gridSnapshot() gridSnapshot {
	re.mu.Lock()
	defer re.mu.Unlock()

	var snap gridSnapshot
	snap.lead = re.beatLead

	p := re.profile
	usable := p != nil && p.Source != "legacy-bpm-cache" &&
		p.BPM >= 40 && p.BPM <= 220 &&
		!re.paused &&
		!re.lastPollTime.IsZero() && time.Since(re.lastPollTime) < gridMaxPollAge

	switch re.beatSource {
	case "mic":
		usable = false
	case "grid":
		// any full profile qualifies
	default: // "auto"
		usable = usable && p.TempoConfidence >= gridAutoMinConfidence
	}

	if wasOn := re.gridActive; usable != wasOn {
		if usable {
			log.Printf("Beat grid locked: %.1f BPM, phase %.0fms, lead %dms (%s)",
				p.BPM, p.BeatPhaseMS, snap.lead.Milliseconds(), p.Track)
		} else {
			log.Println("Beat grid released — mic beat detection driving")
		}
	}
	re.gridActive = usable
	re.syntheticDrive = usable && re.micFree

	snap.on = usable
	snap.synthetic = re.syntheticDrive
	// Mic-free with no usable grid (track not profiled yet, or the profile
	// is legacy/BPM-only): hold a steady ambient level while a track plays
	// so the lights aren't frozen dark while the analyzer listens.
	snap.idleAmbient = re.micFree && !usable && re.running &&
		re.currentTrack != "" && !re.paused &&
		!re.lastPollTime.IsZero() && time.Since(re.lastPollTime) < gridMaxPollAge
	if usable {
		snap.bpm = p.BPM
		snap.phaseMS = p.BeatPhaseMS
		snap.energy = p.Energy
		if p.AI != nil && p.AI.Energy > 0 {
			// The AI's energy blends song knowledge with the DSP number and
			// isn't skewed by how loud the mic capture happened to be.
			snap.energy = p.AI.Energy
		}
		snap.posMS = float64(re.estimateProgress())
	}
	return snap
}

// gridLoop runs for the life of one reactive session. Whenever the beat grid
// is usable it fires beat frames at predicted beat times minus the configured
// lead; in mic-free mode it also renders the decay/ambient frames between
// beats (the capture loop stands down from light driving in that case).
func (re *ReactiveEngine) gridLoop(stopCh chan struct{}) {
	var lastFire time.Time
	for {
		snap := re.gridSnapshot()

		if !snap.on {
			if snap.idleAmbient {
				re.driveSynthetic(snap.energy) // energy 0 → 0.5 fallback level
			}
			select {
			case <-stopCh:
				return
			case <-time.After(gridIdleInterval):
			}
			continue
		}

		periodMS := 60000.0 / snap.bpm
		delayMS := beatGridDelayMS(snap.posMS, snap.phaseMS, snap.bpm)
		fireInMS := delayMS - float64(snap.lead.Milliseconds())

		var sleep time.Duration
		if fireInMS <= 3 {
			// Guard against double-firing one beat (position jitter between
			// Spotify polls can move the grid under us by a few ms).
			if time.Since(lastFire) >= time.Duration(0.55*periodMS)*time.Millisecond {
				re.fireGridBeat(snap)
				lastFire = time.Now()
			}
			// Step past the audible beat so the next delay is a full period.
			sleep = time.Duration(delayMS+8) * time.Millisecond
		} else {
			if snap.synthetic {
				re.driveSynthetic(snap.energy)
			}
			sleep = time.Duration(math.Min(fireInMS, float64(gridMaxSleep.Milliseconds()))) * time.Millisecond
		}

		select {
		case <-stopCh:
			return
		case <-time.After(sleep):
		}
	}
}

// fireGridBeat sends one on-beat light frame. Levels come from the live
// audio when the mic has signal, otherwise from the profile's energy.
func (re *ReactiveEngine) fireGridBeat(snap gridSnapshot) {
	re.mu.Lock()
	re.beatCount++
	re.lastGridFire = time.Now()
	gain := re.effectiveGain
	rms, bass, mid, treble := re.rms*gain, re.bass*gain, re.mid*gain, re.treble*gain
	re.mu.Unlock()

	if snap.synthetic || rms < 0.05 {
		rms, bass, mid, treble = syntheticLevels(snap.energy)
	}
	re.updateLights(rms, bass, mid, treble, true)
}

// driveSynthetic renders one ambient (non-beat) frame from profile energy —
// the mic-free replacement for the capture loop's per-frame light driving.
func (re *ReactiveEngine) driveSynthetic(energy float64) {
	rms, bass, mid, treble := syntheticLevels(energy)
	re.updateLights(rms, bass, mid, treble, false)
}

// GridStatus reports whether the beat grid is currently driving beats and
// with which profile — used by Status() and surfaced on the dashboard.
func (re *ReactiveEngine) GridStatus() (active, synthetic bool, p *analyzer.TrackProfile) {
	re.mu.Lock()
	defer re.mu.Unlock()
	return re.gridActive, re.syntheticDrive, re.profile
}
