package analyzer

import "math"

// tempoEstimate is the result of offline tempo analysis.
type tempoEstimate struct {
	BPM         float64
	PhaseFrames float64 // offset of the first beat within the analyzed window, in onset frames
	Confidence  float64 // 0-1: how dominant the winning tempo was
	Regularity  float64 // 0-1: how consistently onsets land on the grid
}

// estimateTempo finds the dominant tempo in an onset-strength envelope via
// autocorrelation with a log-Gaussian tempo prior and harmonic reinforcement,
// then locks the beat phase with a comb filter.
//
// env is the spectral-flux envelope sampled at fps frames per second.
func estimateTempo(env []float64, fps float64) tempoEstimate {
	out := tempoEstimate{BPM: 120} // safe default
	if len(env) < int(4*fps) {     // need at least ~4 seconds
		return out
	}

	// 1. Condition the envelope: subtract a moving average so sustained
	// loudness doesn't correlate, keep only the onset spikes.
	e := detrend(env, int(fps/2))

	// 2. Autocorrelation over the 50-220 BPM lag range.
	minLag := int(fps * 60 / 220)
	maxLag := int(fps * 60 / 50)
	if maxLag >= len(e)/2 {
		maxLag = len(e)/2 - 1
	}
	if maxLag <= minLag {
		return out
	}

	ac := make([]float64, maxLag*3+1) // room for harmonic lookups at 2x/3x
	var ac0 float64
	for _, v := range e {
		ac0 += v * v
	}
	if ac0 < 1e-12 {
		return out
	}
	for lag := 1; lag < len(ac) && lag < len(e); lag++ {
		var sum float64
		for i := lag; i < len(e); i++ {
			sum += e[i] * e[i-lag]
		}
		ac[lag] = sum / ac0
	}

	// 3. Score each candidate lag: prior × (self + harmonics).
	// The log-Gaussian prior (centered 120 BPM) resolves octave ambiguity —
	// without it a 140 BPM track often scores highest at 70 BPM.
	bestLag, bestScore := 0, -1.0
	for lag := minLag; lag <= maxLag; lag++ {
		bpm := fps * 60 / float64(lag)
		prior := math.Exp(-0.5 * sq(math.Log2(bpm/120.0)/0.9))
		score := ac[lag]
		if 2*lag < len(ac) {
			score += 0.5 * ac[2*lag]
		}
		if 3*lag < len(ac) {
			score += 0.25 * ac[3*lag]
		}
		score *= prior
		if score > bestScore {
			bestScore = score
			bestLag = lag
		}
	}
	if bestLag == 0 {
		return out
	}

	// 4. Parabolic interpolation around the peak for sub-frame precision.
	lagF := float64(bestLag)
	if bestLag > minLag && bestLag < maxLag {
		y0, y1, y2 := ac[bestLag-1], ac[bestLag], ac[bestLag+1]
		den := y0 - 2*y1 + y2
		if math.Abs(den) > 1e-12 {
			lagF += 0.5 * (y0 - y2) / den
		}
	}
	bpm := fps * 60 / lagF

	// Fold into the 70-180 range most music lives in.
	for bpm < 70 {
		bpm *= 2
		lagF /= 2
	}
	for bpm > 180 {
		bpm /= 2
		lagF *= 2
	}

	// 5. Beat phase: comb-filter the envelope at the winning period and
	// pick the offset where onsets are strongest.
	period := lagF
	nPhases := int(period)
	if nPhases < 1 {
		nPhases = 1
	}
	bestPhase, bestPhaseSum := 0.0, -1.0
	for p := 0; p < nPhases; p++ {
		var sum float64
		var n int
		for i := float64(p); int(i) < len(env); i += period {
			sum += env[int(i)]
			n++
		}
		if n > 0 && sum/float64(n) > bestPhaseSum {
			bestPhaseSum = sum / float64(n)
			bestPhase = float64(p)
		}
	}

	// Confidence: normalized autocorrelation at the winning lag.
	conf := clamp01(ac[bestLag])

	// Regularity: how much stronger on-grid onsets are than the average —
	// a proxy for danceability's "steady beat" component.
	var meanEnv float64
	for _, v := range env {
		meanEnv += v
	}
	meanEnv /= float64(len(env))
	regularity := 0.0
	if meanEnv > 1e-12 {
		regularity = clamp01((bestPhaseSum/meanEnv - 1) / 3)
	}

	return tempoEstimate{
		BPM:         bpm,
		PhaseFrames: bestPhase,
		Confidence:  conf,
		Regularity:  regularity,
	}
}

// detrend subtracts a centered moving average (half-window w) and half-wave
// rectifies, leaving only local onset spikes.
func detrend(env []float64, w int) []float64 {
	if w < 1 {
		w = 1
	}
	prefix := make([]float64, len(env)+1)
	for i, v := range env {
		prefix[i+1] = prefix[i] + v
	}
	out := make([]float64, len(env))
	for i := range env {
		lo := max(i-w, 0)
		hi := min(i+w+1, len(env))
		mean := (prefix[hi] - prefix[lo]) / float64(hi-lo)
		if d := env[i] - mean; d > 0 {
			out[i] = d
		}
	}
	return out
}

func sq(x float64) float64 { return x * x }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
