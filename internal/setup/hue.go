package setup

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type discoveredBridge struct {
	ID string `json:"id"`
	IP string `json:"internalipaddress"`
}

// discoverBridges asks Philips' discovery service for Hue bridges on this
// network. Returns an empty slice on any failure — the caller falls back to
// manual IP entry.
func discoverBridges() []discoveredBridge {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://discovery.meethue.com/")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var bridges []discoveredBridge
	if err := json.NewDecoder(resp.Body).Decode(&bridges); err != nil {
		return nil
	}
	return bridges
}

// registerWithBridge performs one registration attempt against the bridge.
// The bridge returns error type 101 until its link button has been pressed;
// that case is reported as linkPressed=false with no error.
func registerWithBridge(ip string) (username, clientKey string, linkPressed bool, err error) {
	host, _ := os.Hostname()
	if host == "" {
		host = "host"
	}
	body, _ := json.Marshal(map[string]any{
		"devicetype":        "casambi-go#" + host,
		"generateclientkey": true,
	})

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://%s/api", ip), "application/json", bytes.NewReader(body))
	if err != nil {
		return "", "", false, err
	}
	defer resp.Body.Close()

	var results []struct {
		Error *struct {
			Type        int    `json:"type"`
			Description string `json:"description"`
		} `json:"error"`
		Success *struct {
			Username  string `json:"username"`
			ClientKey string `json:"clientkey"`
		} `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return "", "", false, fmt.Errorf("unexpected bridge response: %w", err)
	}
	if len(results) == 0 {
		return "", "", false, fmt.Errorf("empty bridge response")
	}
	if e := results[0].Error; e != nil {
		if e.Type == 101 { // link button not pressed
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("bridge error %d: %s", e.Type, e.Description)
	}
	if s := results[0].Success; s != nil {
		return s.Username, s.ClientKey, true, nil
	}
	return "", "", false, fmt.Errorf("bridge response had neither success nor error")
}

type entertainmentArea struct {
	ID       string
	Name     string
	Channels []int
}

// listEntertainmentAreas fetches the bridge's entertainment configurations
// via the CLIP v2 API. The bridge uses a self-signed certificate, hence
// InsecureSkipVerify (same as the Entertainment streamer).
func listEntertainmentAreas(ip, username string) ([]entertainmentArea, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/clip/v2/resource/entertainment_configuration", ip), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("hue-application-key", username)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Data []struct {
			ID       string `json:"id"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Channels []struct {
				ChannelID int `json:"channel_id"`
			} `json:"channels"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	var areas []entertainmentArea
	for _, d := range payload.Data {
		area := entertainmentArea{ID: d.ID, Name: d.Metadata.Name}
		for _, ch := range d.Channels {
			area.Channels = append(area.Channels, ch.ChannelID)
		}
		areas = append(areas, area)
	}
	return areas, nil
}
