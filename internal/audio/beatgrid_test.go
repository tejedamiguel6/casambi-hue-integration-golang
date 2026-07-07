package audio

import (
	"math"
	"testing"
)

func TestBeatGridDelayMS(t *testing.T) {
	const period = 500.0 // 120 BPM

	cases := []struct {
		name    string
		posMS   float64
		phaseMS float64
		bpm     float64
		want    float64
	}{
		{"exactly on a beat, next is one period away", 10000, 0, 120, period},
		{"halfway between beats", 10250, 0, 120, 250},
		{"just before a beat", 10490, 0, 120, 10},
		{"phase offset shifts the grid", 10000, 200, 120, 200},
		{"position before the phase anchor (negative mod)", 100, 350, 120, 250},
		{"phase larger than one period folds", 10000, 1200, 120, 200},
		{"non-integer BPM", 60000, 0, 124.9, 60000.0 / 124.9 * (1 - math.Mod(60000/(60000.0/124.9), 1))},
	}

	for _, c := range cases {
		got := beatGridDelayMS(c.posMS, c.phaseMS, c.bpm)
		if math.Abs(got-c.want) > 0.01 {
			t.Errorf("%s: beatGridDelayMS(%v, %v, %v) = %.3f, want %.3f",
				c.name, c.posMS, c.phaseMS, c.bpm, got, c.want)
		}
		period := 60000.0 / c.bpm
		if got <= 0 || got > period+0.01 {
			t.Errorf("%s: delay %.3f outside (0, period=%.3f]", c.name, got, period)
		}
	}
}

// The delay must walk backwards in lockstep with playback: advancing the
// position by X ms shortens the delay by X (mod period).
func TestBeatGridDelayMonotonic(t *testing.T) {
	const bpm, phase = 128.0, 137.0
	period := 60000.0 / bpm
	prev := beatGridDelayMS(20000, phase, bpm)
	for step := 1.0; step < period; step += 7 {
		got := beatGridDelayMS(20000+step, phase, bpm)
		want := math.Mod(prev-step, period)
		if want <= 0 {
			want += period
		}
		if math.Abs(got-want) > 0.01 {
			t.Fatalf("at +%.0fms: delay %.3f, want %.3f", step, got, want)
		}
	}
}

func TestSyntheticLevels(t *testing.T) {
	for _, energy := range []float64{0, 0.2, 0.5, 1.0} {
		rms, bass, mid, treble := syntheticLevels(energy)
		if rms < 0.35 || rms > 0.8 {
			t.Errorf("energy %v: rms %.3f outside [0.35, 0.8]", energy, rms)
		}
		if bass <= 0 || mid <= 0 || treble <= 0 {
			t.Errorf("energy %v: bands must be positive, got %v %v %v", energy, bass, mid, treble)
		}
		if bass < treble {
			t.Errorf("energy %v: bass %.3f should dominate treble %.3f for Hue blending", energy, bass, treble)
		}
	}
	// Zero energy (quiet-capture artifact) must not produce dark lights.
	rms0, _, _, _ := syntheticLevels(0)
	rmsHalf, _, _, _ := syntheticLevels(0.5)
	if rms0 != rmsHalf {
		t.Errorf("zero energy should fall back to 0.5: got %.3f, want %.3f", rms0, rmsHalf)
	}
}
