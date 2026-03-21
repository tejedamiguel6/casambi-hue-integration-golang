package spotify

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/protocol"
)

// HueSetter abstracts the Hue bridge API to avoid import cycles.
type HueSetter interface {
	SetState(lightID string, state map[string]any) error
}

// BeatSync coordinates music-reactive lighting across Casambi and Hue.
type BeatSync struct {
	mu       sync.Mutex
	running  bool
	stopCh   chan struct{}
	conn   *ble.Connection
	hue    HueSetter
	apiURL string
	bpm    float64
	beatNum  int
	track    string
	artist   string
	progress int // ms — estimated, interpolated between polls

	// Timing for progress interpolation
	lastPollProgress int       // progress_ms from last API poll
	lastPollTime     time.Time // local time of last API poll

	// Tap tempo
	taps []time.Time

	// BPM cache
	bpmCache *BPMCache

	// Config
	CasambiUnits []uint16 // Casambi unit IDs to pulse
	HueLights    []string // Hue light IDs to pulse
	BrightHigh   int      // peak brightness (Hue: 0-254, Casambi: 0-255)
	BrightLow    int      // trough brightness
}

func NewBeatSync(conn *ble.Connection, hue HueSetter, apiURL string) *BeatSync {
	return &BeatSync{
		conn:         conn,
		hue:          hue,
		apiURL:       apiURL,
		bpmCache:     LoadBPMCache(),
		bpm:          120,
		BrightHigh:   254,
		BrightLow:    30,
		CasambiUnits: []uint16{1, 4},
		HueLights:    []string{"19", "20", "23", "31"},
	}
}

type nowPlayingResponse struct {
	Data struct {
		ProgressMS int `json:"progress_ms"`
		Item       struct {
			Name  string `json:"name"`
			Album struct {
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
			} `json:"album"`
		} `json:"item"`
	} `json:"data"`
}

// Status returns the current sync state.
func (bs *BeatSync) Status() map[string]any {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return map[string]any{
		"running":  bs.running,
		"bpm":      bs.bpm,
		"track":    bs.track,
		"artist":   bs.artist,
		"progress": bs.estimateProgress(),
		"beats":    bs.beatNum,
	}
}

// estimateProgress interpolates between API polls. Must be called with mu held.
func (bs *BeatSync) estimateProgress() int {
	if bs.lastPollTime.IsZero() {
		return bs.progress
	}
	elapsed := time.Since(bs.lastPollTime).Milliseconds()
	return bs.lastPollProgress + int(elapsed)
}

// Start begins beat-reactive lighting.
func (bs *BeatSync) Start(bpm float64) error {
	bs.mu.Lock()
	if bs.running {
		bs.mu.Unlock()
		return fmt.Errorf("already running")
	}
	bs.bpm = bpm
	bs.running = true
	bs.beatNum = 0
	bs.stopCh = make(chan struct{})
	bs.mu.Unlock()

	go bs.run()
	return nil
}

// Stop halts the sync loop.
func (bs *BeatSync) Stop() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.running {
		close(bs.stopCh)
		bs.running = false
	}
}

// SetBPM updates the tempo on the fly.
func (bs *BeatSync) SetBPM(bpm float64) {
	bs.mu.Lock()
	bs.bpm = bpm
	bs.mu.Unlock()
}

func (bs *BeatSync) run() {
	log.Printf("Beat sync started at %.0f BPM", bs.bpm)

	// Poll track info in background
	go bs.pollTrack()

	// Use a ticker slightly faster than beat resolution
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	var lastBeat int

	for {
		select {
		case <-bs.stopCh:
			log.Println("Beat sync stopped")
			bs.resetLights()
			return
		case <-ticker.C:
			bs.mu.Lock()
			bpm := bs.bpm
			progress := bs.estimateProgress()
			bs.mu.Unlock()

			if progress == 0 || bpm == 0 {
				continue
			}

			// Calculate which beat we're on
			beatInterval := 60000.0 / bpm
			currentBeat := int(math.Floor(float64(progress) / beatInterval))

			if currentBeat != lastBeat {
				lastBeat = currentBeat
				bs.mu.Lock()
				bs.beatNum++
				bs.mu.Unlock()
				bs.fireBeat()
			}
		}
	}
}

func (bs *BeatSync) pollTrack() {
	for {
		select {
		case <-bs.stopCh:
			return
		default:
		}

		resp, err := http.Get(bs.apiURL)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var np nowPlayingResponse
		if err := json.Unmarshal(body, &np); err == nil && np.Data.ProgressMS > 0 {
			trackName := np.Data.Item.Name
			artistName := ""
			if len(np.Data.Item.Album.Artists) > 0 {
				artistName = np.Data.Item.Album.Artists[0].Name
			}

			bs.mu.Lock()
			bs.lastPollProgress = np.Data.ProgressMS
			bs.lastPollTime = time.Now()
			bs.progress = np.Data.ProgressMS
			trackChanged := trackName != "" && trackName != bs.track
			if trackChanged {
				bs.track = trackName
				bs.artist = artistName
			}
			bs.mu.Unlock()

			// Check BPM cache on track change
			if trackChanged {
				log.Printf("Now playing: %s — %s", trackName, artistName)
				if cached := bs.bpmCache.Get(trackName, artistName); cached > 0 {
					bs.mu.Lock()
					bs.bpm = cached
					bs.mu.Unlock()
					log.Printf("Cached BPM: %s — %.0f BPM", trackName, cached)
				} else {
					log.Printf("No cached BPM for %q — use tap tempo or POST /api/spotify/sync/bpm", trackName)
				}
				// Reset taps for new song
				bs.mu.Lock()
				bs.taps = nil
				bs.mu.Unlock()
			}
		}

		time.Sleep(2 * time.Second)
	}
}

// Tap records a beat tap and recalculates BPM from recent taps.
// Returns the calculated BPM.
func (bs *BeatSync) Tap() float64 {
	now := time.Now()
	bs.mu.Lock()
	defer bs.mu.Unlock()

	// Discard taps older than 5 seconds (user stopped tapping)
	cutoff := now.Add(-5 * time.Second)
	fresh := bs.taps[:0]
	for _, t := range bs.taps {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	fresh = append(fresh, now)
	bs.taps = fresh

	if len(fresh) < 2 {
		return bs.bpm
	}

	// Average interval between taps
	var totalInterval time.Duration
	for i := 1; i < len(fresh); i++ {
		totalInterval += fresh[i].Sub(fresh[i-1])
	}
	avgInterval := totalInterval / time.Duration(len(fresh)-1)
	bpm := 60.0 / avgInterval.Seconds()

	// Clamp to reasonable range
	if bpm >= 30 && bpm <= 300 {
		bs.bpm = bpm
		// Auto-save to cache
		if bs.track != "" {
			bs.bpmCache.Set(bs.track, bs.artist, bpm)
		}
	}

	return bs.bpm
}

// SaveCurrentBPM saves the current BPM to the cache for the current track.
func (bs *BeatSync) SaveCurrentBPM() {
	bs.mu.Lock()
	track := bs.track
	artist := bs.artist
	bpm := bs.bpm
	bs.mu.Unlock()

	if track != "" {
		bs.bpmCache.Set(track, artist, bpm)
		log.Printf("Saved BPM: %s — %.0f BPM", track, bpm)
	}
}

func (bs *BeatSync) fireBeat() {
	bs.mu.Lock()
	bpm := bs.bpm
	high := bs.BrightHigh
	low := bs.BrightLow
	bs.mu.Unlock()

	// Flash bright — all lights in parallel
	bs.setAllLightsParallel(high, 0)

	// Fade down after half a beat
	halfBeat := time.Duration(30000/bpm) * time.Millisecond
	time.Sleep(halfBeat)

	// Fade transition time for Hue (in 100ms units)
	fadeTime := int(halfBeat.Milliseconds() / 100)
	if fadeTime < 1 {
		fadeTime = 1
	}
	bs.setAllLightsParallel(low, fadeTime)
}

func (bs *BeatSync) setAllLightsParallel(brightness int, hueTransition int) {
	var wg sync.WaitGroup

	// Casambi lights — send in parallel
	seq := uint16(bs.beatNum%30000) + 100
	for _, unitID := range bs.CasambiUnits {
		wg.Add(1)
		go func(id uint16, s uint16) {
			defer wg.Done()
			cmd := protocol.NewSetLevelCommand(uint8(min(brightness, 255)), id, protocol.TargetUnit, s)
			bs.conn.SendCommand(cmd)
		}(unitID, seq)
		seq++
	}

	// Hue lights — send in parallel
	for _, lightID := range bs.HueLights {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			bs.hue.SetState(id, map[string]any{
				"on":             true,
				"bri":            min(brightness, 254),
				"transitiontime": hueTransition,
			})
		}(lightID)
	}

	wg.Wait()
}

func (bs *BeatSync) resetLights() {
	bs.setAllLightsParallel(bs.BrightHigh, 5)
}
