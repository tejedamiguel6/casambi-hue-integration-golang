package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HueClient controls Philips Hue lights directly via the bridge HTTP API.
type HueClient struct {
	BridgeIP string
	Username string
}

// HueLight represents a light on the Hue bridge.
type HueLight struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	On        bool   `json:"on"`
	Bri       int    `json:"bri"`
	Hue       int    `json:"hue"`
	Sat       int    `json:"sat"`
	Reachable bool   `json:"reachable"`
}

// ListLights returns all lights from the Hue bridge.
func (h *HueClient) ListLights() ([]HueLight, error) {
	url := fmt.Sprintf("http://%s/api/%s/lights", h.BridgeIP, h.Username)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	var lights []HueLight
	for id, data := range raw {
		var light struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			State struct {
				On        bool `json:"on"`
				Bri       int  `json:"bri"`
				Hue       int  `json:"hue"`
				Sat       int  `json:"sat"`
				Reachable bool `json:"reachable"`
			} `json:"state"`
		}
		if err := json.Unmarshal(data, &light); err != nil {
			continue
		}
		lights = append(lights, HueLight{
			ID:        id,
			Name:      light.Name,
			Type:      light.Type,
			On:        light.State.On,
			Bri:       light.State.Bri,
			Hue:       light.State.Hue,
			Sat:       light.State.Sat,
			Reachable: light.State.Reachable,
		})
	}
	return lights, nil
}

// SetState sends a state update to a Hue light.
// Accepts any combination: on, bri (0-254), hue (0-65535), sat (0-254).
func (h *HueClient) SetState(lightID string, state map[string]any) error {
	url := fmt.Sprintf("http://%s/api/%s/lights/%s/state", h.BridgeIP, h.Username, lightID)
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return nil
}
