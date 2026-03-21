package audio

import (
	"encoding/json"
	"log"
	"math"
	"math/cmplx"
	"net/http"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
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

	hue             HueSetter
	casambiSend     func(level uint8, unitID uint16)
	casambiState    func(state []byte, unitID uint16)
	CasambiUnits    []uint16
	HueLights       []string

	// Audio state (read by status endpoint)
	rms       float64 // overall volume 0.0-1.0
	bass      float64 // bass energy 0.0-1.0
	mid       float64 // mid energy 0.0-1.0
	treble    float64 // treble energy 0.0-1.0
	beatCount int
	onBeat    bool

	// Beat detection state
	bassHistory  [43]float64 // ~1 second of bass energy at 43 fps (1024 samples @ 44100Hz)
	bassIdx      int
	lastBeatTime time.Time

	// Smoothing
	smoothBri float64 // smoothed brightness for Casambi

	// Gain — multiplier for mic sensitivity. Higher = brighter.
	Gain float64

	// Album art colors
	colors    *DominantColors
	trackName string

	// Hybrid beat sync: Spotify progress + BPM for timing, mic for intensity
	spotifyURL       string
	bpm              float64
	progress         int       // ms from Spotify
	lastPollProgress int
	lastPollTime     time.Time
	lastBeatNum      int
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

func NewReactiveEngine(hue HueSetter, casambiSend func(level uint8, unitID uint16), casambiState func(state []byte, unitID uint16), spotifyURL string) *ReactiveEngine {
	return &ReactiveEngine{
		hue:          hue,
		casambiSend:  casambiSend,
		casambiState: casambiState,
		CasambiUnits: []uint16{1, 4},
		HueLights:    []string{"19", "20", "23", "25", "31"},
		Gain:         10,
		spotifyURL:   spotifyURL,
		bpm:          120,
	}
}

func (re *ReactiveEngine) Status() map[string]any {
	re.mu.Lock()
	defer re.mu.Unlock()
	return map[string]any{
		"running":  re.running,
		"gain":     re.Gain,
		"bpm":      re.bpm,
		"rms":      math.Round(re.rms*100) / 100,
		"bass":     math.Round(re.bass*100) / 100,
		"mid":      math.Round(re.mid*100) / 100,
		"treble":   math.Round(re.treble*100) / 100,
		"beats":    re.beatCount,
		"onBeat":   re.onBeat,
		"track":    re.trackName,
		"progress": re.estimateProgress(),
		"colors":   re.colors,
	}
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
	re.mu.Unlock()
	log.Printf("Album colors: %s → primary hue=%d sat=%d, secondary hue=%d sat=%d",
		trackName, colors.Primary.Hue, colors.Primary.Sat, colors.Secondary.Hue, colors.Secondary.Sat)
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
	re.mu.Unlock()
}

func (re *ReactiveEngine) Start() error {
	re.mu.Lock()
	if re.running {
		re.mu.Unlock()
		return nil
	}
	re.running = true
	re.stopCh = make(chan struct{})
	re.mu.Unlock()

	go re.run()
	return nil
}

func (re *ReactiveEngine) Stop() {
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.running {
		close(re.stopCh)
		re.running = false
	}
}

func (re *ReactiveEngine) run() {
	if err := portaudio.Initialize(); err != nil {
		log.Printf("portaudio init error: %v", err)
		return
	}
	defer portaudio.Terminate()

	buf := make([]float32, bufSize)
	stream, err := portaudio.OpenDefaultStream(1, 0, sampleRate, bufSize, buf)
	if err != nil {
		log.Printf("open audio stream error: %v", err)
		return
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		log.Printf("start audio stream error: %v", err)
		return
	}
	defer stream.Stop()

	log.Println("Audio-reactive engine started (hybrid: Spotify timing + mic intensity)")

	// Start Spotify poller for progress + album art
	go re.pollSpotify()

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

		// Convert to float64 for analysis
		samples := make([]float64, len(buf))
		for i, s := range buf {
			samples[i] = float64(s)
		}

		// Analyze mic for intensity/frequency
		rms := computeRMS(samples)
		bass, mid, treble := computeFrequencyBands(samples)

		// Hybrid beat detection: use Spotify progress + BPM for timing
		re.mu.Lock()
		bpm := re.bpm
		progress := re.estimateProgress()
		re.mu.Unlock()

		isBeat := false
		if progress > 0 && bpm > 0 {
			beatInterval := 60000.0 / bpm
			currentBeatNum := int(math.Floor(float64(progress) / beatInterval))
			if currentBeatNum != re.lastBeatNum {
				re.lastBeatNum = currentBeatNum
				isBeat = true
			}
		} else {
			// Fallback to mic-based beat detection if no Spotify data
			isBeat = re.detectBeat(bass)
		}

		re.mu.Lock()
		re.rms = rms
		re.bass = bass
		re.mid = mid
		re.treble = treble
		re.onBeat = isBeat
		if isBeat {
			re.beatCount++
		}
		re.mu.Unlock()

		// Drive lights — mic intensity modulates the beat flash
		re.updateLights(rms, bass, mid, treble, isBeat)
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

	var lastTrack string
	for {
		select {
		case <-re.stopCh:
			return
		default:
		}

		if re.spotifyURL == "" {
			time.Sleep(5 * time.Second)
			continue
		}

		resp, err := http.Get(re.spotifyURL)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		var np spotifyResponse
		json.NewDecoder(resp.Body).Decode(&np)
		resp.Body.Close()

		if np.Data.ProgressMS > 0 {
			re.mu.Lock()
			re.lastPollProgress = np.Data.ProgressMS
			re.lastPollTime = time.Now()
			re.progress = np.Data.ProgressMS
			re.mu.Unlock()
		}

		trackName := np.Data.Item.Name
		if trackName != "" && trackName != lastTrack {
			lastTrack = trackName
			log.Printf("Now playing: %s", trackName)

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

func (re *ReactiveEngine) detectBeat(bassEnergy float64) bool {
	// Store bass energy in circular buffer
	re.bassHistory[re.bassIdx%len(re.bassHistory)] = bassEnergy
	re.bassIdx++

	// Need at least half a second of history
	if re.bassIdx < len(re.bassHistory)/2 {
		return false
	}

	// Compute average bass energy
	var avg float64
	for _, v := range re.bassHistory {
		avg += v
	}
	avg /= float64(len(re.bassHistory))

	// Beat = bass energy above average + minimum interval (80ms)
	// Lower threshold = more sensitive to beats
	threshold := avg*1.15 + 0.005
	now := time.Now()
	if bassEnergy > threshold && now.Sub(re.lastBeatTime) > 80*time.Millisecond {
		re.lastBeatTime = now
		return true
	}
	return false
}

func (re *ReactiveEngine) updateLights(rms, bass, mid, treble float64, isBeat bool) {
	// Apply gain to boost mic input
	rms = rms * re.Gain
	bass = bass * re.Gain
	mid = mid * re.Gain
	treble = treble * re.Gain

	// Casambi: 5-channel color fixture [Master, ch1, ch2, ch3, ch4]
	// Use album art colors blended by frequency balance, same as Hue lights
	targetBri := clamp(rms*255, 0, 255)
	if isBeat {
		targetBri = 255
	}
	// Asymmetric smoothing: instant attack on beats, slow decay
	if targetBri > re.smoothBri {
		re.smoothBri = targetBri // instant jump up
	} else {
		re.smoothBri = re.smoothBri*0.85 + targetBri*0.15 // slow fade down
	}
	casambiBri := uint8(clamp(re.smoothBri, 0, 255))

	re.mu.Lock()
	colors := re.colors
	re.mu.Unlock()

	// Convert album art HSV colors to RGB for the 5-channel state
	var r1, g1, b1, r2, g2, b2 float64
	if colors != nil {
		r1, g1, b1 = hsvToRGB(float64(colors.Primary.Hue)/65535.0*360, float64(colors.Primary.Sat)/254.0, 1.0)
		r2, g2, b2 = hsvToRGB(float64(colors.Secondary.Hue)/65535.0*360, float64(colors.Secondary.Sat)/254.0, 1.0)
	} else {
		r1, g1, b1 = 1.0, 0.2, 0.0 // warm default
		r2, g2, b2 = 0.0, 0.2, 1.0 // cool default
	}

	// Blend primary/secondary — primary dominates, secondary is accent
	// Primary gets 70% minimum, secondary shifts in on treble-heavy moments
	total := bass + mid + treble
	primaryWeight := 0.8
	if total > 0.001 {
		bassRatio := bass / total
		primaryWeight = 0.7 + bassRatio*0.3 // ranges from 0.7 (no bass) to 1.0 (all bass)
	}
	r := r1*primaryWeight + r2*(1-primaryWeight)
	g := g1*primaryWeight + g2*(1-primaryWeight)
	b := b1*primaryWeight + b2*(1-primaryWeight)

	// 5-byte state: [Master, ch1, ch2, ch3, ch4]
	// Master controls overall intensity, color channels control the hue
	// Boost color values and scale by brightness
	colorScale := clamp(float64(casambiBri)/255.0+0.3, 0, 1) // never fully dark
	state := []byte{
		casambiBri,
		uint8(clamp(r*255*colorScale, 0, 255)),
		uint8(clamp(g*255*colorScale, 0, 255)),
		uint8(clamp(b*255*colorScale, 0, 255)),
		uint8(clamp(float64(casambiBri)*0.15, 0, 80)), // minimal white to avoid washing out colors
	}

	for _, unitID := range re.CasambiUnits {
		go re.casambiState(state, unitID)
	}

	// Hue: use album art colors, blend between primary (bass) and secondary (treble)
	primary := HSV{Hue: 0, Sat: 200, Val: 254}       // red
	secondary := HSV{Hue: 43690, Sat: 200, Val: 254}  // blue
	if colors != nil {
		primary = colors.Primary
		secondary = colors.Secondary
	}

	// Blend between primary and secondary — primary dominates (same weighting as Casambi)
	if total < 0.001 {
		return // silence
	}
	hueColor := int(float64(primary.Hue)*primaryWeight + float64(secondary.Hue)*(1-primaryWeight))
	hueSat := int(float64(primary.Sat)*primaryWeight + float64(secondary.Sat)*(1-primaryWeight))
	hueBri := int(clamp(rms*254, 30, 254))

	if isBeat {
		hueBri = 254
		hueSat = min(hueSat+50, 254)
		hueColor = primary.Hue // snap to primary color on beat
	}

	transition := 3 // 300ms smooth fade between beats
	if isBeat {
		transition = 0 // instant on beat
	}

	for _, lightID := range re.HueLights {
		go re.hue.SetState(lightID, map[string]any{
			"on":             true,
			"hue":            hueColor % 65536,
			"sat":            hueSat,
			"bri":            hueBri,
			"transitiontime": transition,
		})
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
	// Simple FFT using DFT for the frequency bands we care about
	spectrum := fft(samples)
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
