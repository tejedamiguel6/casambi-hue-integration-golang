package api

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
)

// HueStreamer streams color data to Hue lights via the Entertainment API.
// Uses DTLS/UDP for ~25Hz updates instead of REST API's ~10 req/sec.
type HueStreamer struct {
	bridgeIP  string
	username  string
	clientKey []byte // decoded from hex
	areaID    string // entertainment configuration UUID
	channels  []int  // channel IDs to control

	mu   sync.Mutex
	conn net.Conn
	active bool

	// A dead DTLS connection blocks Write inside pion's handshake mutex,
	// which no deadline can interrupt. writeMu admits one in-flight write;
	// concurrent frames are dropped (fine at 25 Hz). If the in-flight write
	// is stuck past stuckAfter, the stream is declared lost, the connection
	// closed (unblocking the writer), and SetState falls back to REST.
	writeMu    sync.Mutex
	writeStart time.Time // guarded by mu; zero = no write in flight
	writeErrs  int       // guarded by mu; consecutive write failures
	rest       *HueClient
}

const (
	streamWriteDeadline = time.Second
	streamStuckAfter    = 3 * time.Second
	streamMaxErrs       = 5
)

// NewHueStreamer creates a streamer. clientKeyHex is the hex-encoded PSK from bridge registration.
func NewHueStreamer(bridgeIP, username, clientKeyHex, areaID string, channels []int) (*HueStreamer, error) {
	key, err := hex.DecodeString(clientKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid client key hex: %w", err)
	}
	return &HueStreamer{
		bridgeIP:  bridgeIP,
		username:  username,
		clientKey: key,
		areaID:    areaID,
		channels:  channels,
	}, nil
}

// Start activates the entertainment area and establishes the DTLS connection.
func (s *HueStreamer) Start() error {
	// Step 1: Activate entertainment area via REST API
	url := fmt.Sprintf("https://%s/clip/v2/resource/entertainment_configuration/%s", s.bridgeIP, s.areaID)
	body := strings.NewReader(`{"action":"start"}`)
	req, err := http.NewRequest("PUT", url, body)
	if err != nil {
		return err
	}
	req.Header.Set("hue-application-key", s.username)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to activate entertainment area: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("entertainment activation failed (%d): %s", resp.StatusCode, respBody)
	}
	log.Printf("Entertainment area activated: %s", s.areaID)

	// Step 2: Establish DTLS connection
	addr := &net.UDPAddr{IP: net.ParseIP(s.bridgeIP), Port: 2100}

	psk := s.clientKey
	identity := s.username
	config := &dtls.Config{
		PSK: func(hint []byte) ([]byte, error) {
			return psk, nil
		},
		PSKIdentityHint:    []byte(identity),
		CipherSuites:       []dtls.CipherSuiteID{dtls.TLS_PSK_WITH_AES_128_GCM_SHA256},
		InsecureSkipVerify: true,
	}

	conn, err := dtls.Dial("udp4", addr, config)
	if err != nil {
		return fmt.Errorf("DTLS connection failed: %w", err)
	}

	s.mu.Lock()
	s.conn = conn
	s.active = true
	s.mu.Unlock()

	log.Println("Hue Entertainment DTLS connection established")
	return nil
}

// Stop closes the DTLS connection and deactivates the entertainment area.
func (s *HueStreamer) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	s.active = false

	// Deactivate entertainment area
	url := fmt.Sprintf("https://%s/clip/v2/resource/entertainment_configuration/%s", s.bridgeIP, s.areaID)
	body := strings.NewReader(`{"action":"stop"}`)
	req, _ := http.NewRequest("PUT", url, body)
	req.Header.Set("hue-application-key", s.username)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	client.Do(req)
	log.Println("Hue Entertainment area deactivated")
}

// SendRGB sends RGB color data to all channels simultaneously.
// r, g, b are 0.0-1.0 float values.
func (s *HueStreamer) SendRGB(r, g, b float64) error {
	s.mu.Lock()
	conn := s.conn
	channels := s.channels
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	packet := s.buildPacket(channels, r, g, b)
	return s.write(conn, packet)
}

// write sends one packet with single-writer admission and stuck detection.
func (s *HueStreamer) write(conn net.Conn, packet []byte) error {
	if !s.writeMu.TryLock() {
		// Another write is in flight — drop this frame. If that write has
		// been stuck for a while the stream is dead; tear it down so the
		// stuck writer unblocks and callers fall back to REST.
		s.mu.Lock()
		stuck := !s.writeStart.IsZero() && time.Since(s.writeStart) > streamStuckAfter
		s.mu.Unlock()
		if stuck {
			s.failStream("write stuck")
		}
		return nil
	}
	defer s.writeMu.Unlock()

	s.mu.Lock()
	s.writeStart = time.Now()
	s.mu.Unlock()

	conn.SetWriteDeadline(time.Now().Add(streamWriteDeadline))
	_, err := conn.Write(packet)

	s.mu.Lock()
	s.writeStart = time.Time{}
	if err != nil {
		s.writeErrs++
		errs := s.writeErrs
		s.mu.Unlock()
		if errs >= streamMaxErrs {
			s.failStream(fmt.Sprintf("%d consecutive write errors", errs))
		}
		return err
	}
	s.writeErrs = 0
	s.mu.Unlock()
	return nil
}

// failStream tears down a dead DTLS connection so SetState reverts to REST.
func (s *HueStreamer) failStream(reason string) {
	s.mu.Lock()
	if s.conn == nil {
		s.mu.Unlock()
		return
	}
	s.conn.Close() // also unblocks a writer stuck inside pion
	s.conn = nil
	s.active = false
	s.writeErrs = 0
	s.mu.Unlock()
	log.Printf("Hue Entertainment stream lost (%s) — falling back to REST until reactive mode restarts", reason)
}

// SendChannelColors sends individual RGB colors per channel.
// colors is a map of channel_id -> [r, g, b] (0.0-1.0).
func (s *HueStreamer) SendChannelColors(colors map[int][3]float64) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	packet := s.buildMultiColorPacket(colors)
	return s.write(conn, packet)
}

// SetState implements the same interface as HueClient for the reactive engine.
// Converts HSV-style state to RGB and sends via streaming. When the DTLS
// stream is down (never started or declared lost), it transparently falls
// back to the REST API so reactive lights keep working at a lower rate.
func (s *HueStreamer) SetState(lightID string, state map[string]any) error {
	s.mu.Lock()
	streaming := s.conn != nil
	if s.rest == nil {
		s.rest = &HueClient{BridgeIP: s.bridgeIP, Username: s.username}
	}
	rest := s.rest
	s.mu.Unlock()
	if !streaming {
		return rest.SetState(lightID, state)
	}
	// Extract brightness — the main reactive parameter
	bri := 0.0
	if v, ok := state["bri"]; ok {
		switch val := v.(type) {
		case int:
			bri = float64(val) / 254.0
		case float64:
			bri = val / 254.0
		}
	}

	// Extract hue and saturation for color
	hue := 0.0
	sat := 0.0
	if v, ok := state["hue"]; ok {
		switch val := v.(type) {
		case int:
			hue = float64(val) / 65535.0
		case float64:
			hue = val / 65535.0
		}
	}
	if v, ok := state["sat"]; ok {
		switch val := v.(type) {
		case int:
			sat = float64(val) / 254.0
		case float64:
			sat = val / 254.0
		}
	}

	// HSV to RGB
	r, g, b := hsvToRGBf(hue*360, sat, bri)

	return s.SendRGB(r, g, b)
}

func (s *HueStreamer) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *HueStreamer) buildPacket(channels []int, r, g, b float64) []byte {
	// Header: "HueStream" + version + flags + color space
	header := []byte{
		'H', 'u', 'e', 'S', 't', 'r', 'e', 'a', 'm', // protocol name
		0x02, 0x00, // API version 2.0
		0x00,       // sequence number
		0x00, 0x00, // reserved
		0x00,       // color space: 0x00 = RGB
		0x00,       // reserved
	}

	// Entertainment configuration ID as ASCII
	areaBytes := []byte(s.areaID)

	// Channel data: 7 bytes per channel
	ri := uint16(r * 65535)
	gi := uint16(g * 65535)
	bi := uint16(b * 65535)

	packet := make([]byte, 0, 16+36+7*len(channels))
	packet = append(packet, header...)
	packet = append(packet, areaBytes...)

	for _, ch := range channels {
		packet = append(packet,
			byte(ch),
			byte(ri>>8), byte(ri),
			byte(gi>>8), byte(gi),
			byte(bi>>8), byte(bi),
		)
	}

	return packet
}

func (s *HueStreamer) buildMultiColorPacket(colors map[int][3]float64) []byte {
	header := []byte{
		'H', 'u', 'e', 'S', 't', 'r', 'e', 'a', 'm',
		0x02, 0x00,
		0x00,
		0x00, 0x00,
		0x00,
		0x00,
	}

	areaBytes := []byte(s.areaID)

	packet := make([]byte, 0, 16+36+7*len(colors))
	packet = append(packet, header...)
	packet = append(packet, areaBytes...)

	for ch, rgb := range colors {
		ri := uint16(rgb[0] * 65535)
		gi := uint16(rgb[1] * 65535)
		bi := uint16(rgb[2] * 65535)
		packet = append(packet,
			byte(ch),
			byte(ri>>8), byte(ri),
			byte(gi>>8), byte(gi),
			byte(bi>>8), byte(bi),
		)
	}

	return packet
}

// hsvToRGBf converts HSV (h: 0-360, s: 0-1, v: 0-1) to RGB (0-1).
func hsvToRGBf(h, s, v float64) (float64, float64, float64) {
	if s == 0 {
		return v, v, v
	}
	h = h - float64(int(h/360))*360
	h /= 60
	i := int(h)
	f := h - float64(i)
	p := v * (1 - s)
	q := v * (1 - s*f)
	t := v * (1 - s*(1-f))
	switch i {
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
