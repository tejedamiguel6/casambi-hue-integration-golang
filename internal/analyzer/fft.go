package analyzer

import "math"

// Iterative radix-2 FFT with cached twiddle factors and Hann windows.
// The recursive FFT in internal/audio is fine for one 1024-point transform
// per frame; the analyzer runs several sizes concurrently, so this version
// avoids the per-call allocations.

type fftPlan struct {
	n       int
	twiddle []complex128 // e^(-2πik/n) for k in [0, n/2)
	rev     []int        // bit-reversal permutation
	hann    []float64
}

func newFFTPlan(n int) *fftPlan {
	p := &fftPlan{n: n}
	p.twiddle = make([]complex128, n/2)
	for k := 0; k < n/2; k++ {
		angle := -2 * math.Pi * float64(k) / float64(n)
		p.twiddle[k] = complex(math.Cos(angle), math.Sin(angle))
	}
	p.rev = make([]int, n)
	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		p.rev[i] = r
	}
	p.hann = make([]float64, n)
	for i := range p.hann {
		p.hann[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return p
}

// magnitudes computes the windowed FFT of samples (len must equal plan size)
// and writes the magnitude spectrum for bins [0, n/2) into out.
// out must have length n/2. Scratch is reused across calls.
func (p *fftPlan) magnitudes(samples []float64, scratch []complex128, out []float64) {
	n := p.n
	for i := 0; i < n; i++ {
		scratch[p.rev[i]] = complex(samples[i]*p.hann[i], 0)
	}
	for size := 2; size <= n; size *= 2 {
		half := size / 2
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				w := p.twiddle[k*step]
				a := scratch[start+k]
				b := scratch[start+k+half] * w
				scratch[start+k] = a + b
				scratch[start+k+half] = a - b
			}
		}
	}
	for i := 0; i < n/2; i++ {
		re := real(scratch[i])
		im := imag(scratch[i])
		out[i] = math.Sqrt(re*re + im*im)
	}
}
