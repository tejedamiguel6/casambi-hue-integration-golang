package analyzer

import (
	"math"
	"sort"
)

// Krumhansl-Kessler key profiles: perceptual "fit" of each pitch class
// within a major/minor key, from probe-tone experiments. Index 0 = tonic.
var (
	majorProfile = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	minorProfile = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
)

// Chroma vector index 0 = A (the analyzer folds frequencies relative to A440).
var pitchNames = [12]string{"A", "A#", "B", "C", "C#", "D", "D#", "E", "F", "F#", "G", "G#"}

// detectKey correlates the accumulated chroma against all 24 major/minor
// rotations (Krumhansl-Schmuckler). Returns e.g. ("F# minor", "minor", 0.72).
// Confidence is the gap between the best and second-best key, scaled.
func detectKey(chroma [12]float64) (key, mode string, confidence float64) {
	var total float64
	for _, v := range chroma {
		total += v
	}
	if total < 1e-9 {
		return "unknown", "unknown", 0
	}

	best, second := -2.0, -2.0
	bestIdx, bestMajor := 0, true
	for tonic := 0; tonic < 12; tonic++ {
		for _, isMajor := range []bool{true, false} {
			profile := &minorProfile
			if isMajor {
				profile = &majorProfile
			}
			var rotated [12]float64
			for i := 0; i < 12; i++ {
				rotated[i] = profile[((i-tonic)%12+12)%12]
			}
			r := correlate(chroma[:], rotated[:])
			if r > best {
				second = best
				best = r
				bestIdx = tonic
				bestMajor = isMajor
			} else if r > second {
				second = r
			}
		}
	}

	mode = "minor"
	if bestMajor {
		mode = "major"
	}
	confidence = clamp01((best - second) * 8)
	return pitchNames[bestIdx] + " " + mode, mode, confidence
}

// correlate returns the Pearson correlation of two equal-length vectors.
func correlate(a, b []float64) float64 {
	n := float64(len(a))
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma /= n
	mb /= n
	var num, da, db float64
	for i := range a {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da < 1e-12 || db < 1e-12 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// energyProfile summarizes the RMS-per-frame series into an energy score
// (0-1), mean loudness in dBFS, and a dynamics score (0-1, how much the
// loudness moves around — compressed EDM ≈ 0.1, orchestral ≈ 0.8).
func energyProfile(rms []float64) (energy, loudnessDB, dynamics float64) {
	if len(rms) == 0 {
		return 0, -96, 0
	}
	var sum float64
	for _, v := range rms {
		sum += v
	}
	mean := sum / float64(len(rms))
	if mean < 1e-9 {
		return 0, -96, 0
	}
	loudnessDB = 20 * math.Log10(mean)

	// Energy: map mean loudness from [-36 dB, -10 dB] onto [0, 1].
	// System-audio capture is level-normalized enough for this to be a
	// meaningful (if approximate) scale.
	energy = clamp01((loudnessDB + 36) / 26)

	// Dynamics: ratio between the 95th and 20th percentile RMS, in dB,
	// mapped from [0 dB, 18 dB] onto [0, 1].
	p20 := percentile(rms, 0.20)
	p95 := percentile(rms, 0.95)
	if p20 > 1e-9 {
		rangeDB := 20 * math.Log10(p95/p20)
		dynamics = clamp01(rangeDB / 18)
	}
	return energy, loudnessDB, dynamics
}

func percentile(vals []float64, p float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// brightnessScore maps the mean spectral centroid from [500 Hz, 4 kHz]
// onto [0, 1]. Dark, bass-heavy mixes sit low; bright, hi-hat-forward
// mixes sit high.
func brightnessScore(centroids []float64) float64 {
	if len(centroids) == 0 {
		return 0.5
	}
	var sum float64
	for _, c := range centroids {
		sum += c
	}
	mean := sum / float64(len(centroids))
	return clamp01((mean - 500) / 3500)
}

// danceability blends beat strength, grid regularity, tempo suitability
// (peaking near 115 BPM), and energy — a heuristic stand-in for Spotify's
// metric of the same name.
func danceability(bpm, tempoConf, regularity, energy float64) float64 {
	tempoFit := math.Exp(-0.5 * sq((bpm-115)/45))
	return clamp01(0.35*tempoConf + 0.25*regularity + 0.25*tempoFit + 0.15*energy)
}

// valenceScore estimates musical positivity: major mode, brighter timbre,
// higher tempo, and higher energy all push it up.
func valenceScore(mode string, bpm, brightness, energy float64) float64 {
	modeScore := 0.35
	if mode == "major" {
		modeScore = 0.75
	}
	tempoScore := clamp01((bpm - 60) / 120)
	return clamp01(0.40*modeScore + 0.25*brightness + 0.20*tempoScore + 0.15*energy)
}
