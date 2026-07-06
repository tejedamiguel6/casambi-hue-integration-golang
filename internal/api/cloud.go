package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SaveCredentials writes credentials to a local JSON file so we
// never need to call the cloud API again.
func SaveCredentials(creds *NetworkCredentials, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// LoadCachedCredentials loads credentials from the first path that has them.
// Returns nil if none exists.
func LoadCachedCredentials(paths ...string) *NetworkCredentials {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var creds NetworkCredentials
		if err := json.Unmarshal(data, &creds); err != nil {
			continue
		}
		return &creds
	}
	return nil
}

const casambiAPIBase = "https://api.casambi.com"

// casambiUserAgent mimics the casambi-bt Python client. The Casambi API
// rejects certain User-Agent strings (like "CasambiGo/1.0") with 403 Forbidden.
const casambiUserAgent = "Python/3.11 aiohttp/3.8"

// NetworkCredentials holds everything we need from the Cloud API
// to authenticate over BLE. Cache this — you only need to fetch it once.
type NetworkCredentials struct {
	NetworkID    string
	SessionToken string
	KeyID        int
	Role         int
	AESKey       [16]byte
	Units        []Unit
	Scenes       []Scene
}

// Unit represents a light fixture on the Casambi network.
type Unit struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Scene represents a saved lighting preset.
type Scene struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// FetchCredentials runs all 3 Cloud API steps and returns the credentials
// needed for BLE authentication.
//
// deviceAddr: the BLE address of the Casambi device (e.g. "AA:BB:CC:DD:EE:FF")
// password:   your Casambi network password
func FetchCredentials(deviceAddr string, password string) (*NetworkCredentials, error) {
	uuid := bleAddrToUUID(deviceAddr)
	fmt.Printf("Step 1: Looking up network for device %s...\n", uuid)

	networkID, err := getNetworkID(uuid)
	if err != nil {
		return nil, fmt.Errorf("get network ID: %w", err)
	}
	fmt.Printf("  Network ID: %s\n", networkID)

	return FetchCredentialsWithNetworkID(networkID, password)
}

// FetchCredentialsWithNetworkID skips the device lookup and goes straight
// to login + config fetch. Use this when you already know the network ID.
func FetchCredentialsWithNetworkID(networkID string, password string) (*NetworkCredentials, error) {
	fmt.Println("Step 2: Logging in...")
	session, err := login(networkID, password)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	fmt.Printf("  Session token: %s...\n", session.Token[:16])
	fmt.Printf("  Key ID: %d, Role: %d\n", session.KeyID, session.Role)

	// Step 3: Fetch network config (AES keys + units).
	// The phone-gateway-backed config endpoint sometimes returns a partial
	// response with an empty unit list — retry a few times before accepting it.
	fmt.Println("Step 3: Fetching network config...")
	var config *networkConfig
	for attempt := 1; attempt <= 3; attempt++ {
		config, err = getNetworkConfig(networkID, session.Token)
		if err != nil {
			return nil, fmt.Errorf("get network config: %w", err)
		}
		if len(config.Units) > 0 {
			break
		}
		if attempt < 3 {
			fmt.Printf("  Config came back with no units (gateway hiccup?) — retry %d/3...\n", attempt+1)
			time.Sleep(3 * time.Second)
		}
	}
	if len(config.Units) == 0 {
		fmt.Println("  WARNING: the network config lists no units. Make sure the Casambi")
		fmt.Println("  app is open in the foreground and re-run if your lights are missing.")
	}

	// Find the AES key matching our keyID
	var aesKey [16]byte
	found := false
	for _, k := range config.Keys {
		if k.ID == session.KeyID {
			keyBytes, err := hex.DecodeString(k.Key)
			if err != nil {
				return nil, fmt.Errorf("decode AES key: %w", err)
			}
			copy(aesKey[:], keyBytes)
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no AES key found for keyID %d", session.KeyID)
	}

	fmt.Printf("  AES key: %x\n", aesKey)
	fmt.Printf("  Units: %d\n", len(config.Units))
	for _, u := range config.Units {
		fmt.Printf("    [%d] %s (%s)\n", u.ID, u.Name, u.Type)
	}
	fmt.Printf("  Scenes: %d\n", len(config.Scenes))
	for _, s := range config.Scenes {
		fmt.Printf("    [%d] %s\n", s.ID, s.Name)
	}

	return &NetworkCredentials{
		NetworkID:    networkID,
		SessionToken: session.Token,
		KeyID:        session.KeyID,
		Role:         session.Role,
		AESKey:       aesKey,
		Units:        config.Units,
		Scenes:       config.Scenes,
	}, nil
}

// ── Step 1: Get Network ID ──────────────────────────────────

// escapeSpecialChars Unicode-escapes non-alphanumeric characters in a string
// for use in manually-built JSON. Works around a Casambi API bug where
// certain characters (like !) cause 400 errors in the password field.
func escapeSpecialChars(s string) string {
	var buf strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			buf.WriteRune(r)
		} else {
			fmt.Fprintf(&buf, "\\u%04x", r)
		}
	}
	return buf.String()
}

func bleAddrToUUID(addr string) string {
	// Convert BLE address to Casambi UUID format:
	// "AA:BB:CC:DD:EE:FF" → "aabbccddeeff"
	// macOS-style UUIDs like "cb631840-8014-..." → strip dashes, lowercase
	cleaned := strings.ReplaceAll(addr, ":", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	return strings.ToLower(cleaned)
}

type networkIDResponse struct {
	ID string `json:"id"`
}

func getNetworkID(uuid string) (string, error) {
	url := fmt.Sprintf("%s/network/uuid/%s", casambiAPIBase, uuid)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", casambiUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	var result networkIDResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, body)
	}

	return result.ID, nil
}

// ── Step 2: Login ───────────────────────────────────────────

type loginRequest struct {
	Password   string `json:"password"`
	DeviceName string `json:"deviceName"`
}

type loginResponse struct {
	Token   string `json:"session"`
	Network string `json:"network"`
	Manager bool   `json:"manager"`
	KeyID   int    `json:"keyID"`
	Expires int64  `json:"expires"`
	Role    int    `json:"role"`
}

func login(networkID, password string) (*loginResponse, error) {
	url := fmt.Sprintf("%s/network/%s/session", casambiAPIBase, networkID)

	// Build JSON manually with Unicode-escaped password.
	// The Casambi API has a bug where certain characters (like !)
	// cause 400 errors unless they're Unicode-escaped (\uXXXX).
	jsonBody := fmt.Sprintf(`{"password":"%s","deviceName":"Casambi Go"}`, escapeSpecialChars(password))

	// Retry for up to 30 seconds — the phone gateway is intermittent
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		if attempt > 1 {
			fmt.Printf("  Retry %d/10...\n", attempt)
			time.Sleep(3 * time.Second)
		}

		req, err := http.NewRequest("POST", url, strings.NewReader(jsonBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", casambiUserAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == 200 {
			var result loginResponse
			if err := json.Unmarshal(body, &result); err != nil {
				return nil, fmt.Errorf("parse response: %w (body: %s)", err, body)
			}
			return &result, nil
		}

		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)

		// 403/404 = gateway down or auth issue, worth retrying
		// Other errors = probably not transient
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			return nil, lastErr
		}
	}

	return nil, fmt.Errorf("login failed after 10 attempts: %w", lastErr)
}

// ── Step 3: Fetch Network Config ────────────────────────────

type configRequest struct {
	FormatVersion int    `json:"formatVersion"`
	DeviceName    string `json:"deviceName"`
	Revision      int    `json:"revision"`
}

type networkConfig struct {
	Keys   []keyEntry
	Units  []Unit
	Scenes []Scene
}

type keyEntry struct {
	ID   int    `json:"id"`
	Key  string `json:"key"`
	Role int    `json:"role"`
}

func getNetworkConfig(networkID, sessionToken string) (*networkConfig, error) {
	// NOTE: No trailing slash! The API returns 404 with a trailing slash.
	url := fmt.Sprintf("%s/network/%s", casambiAPIBase, networkID)

	reqBody, _ := json.Marshal(configRequest{
		FormatVersion: 1,
		DeviceName:    "Casambi Go",
		Revision:      0,
	})

	req, err := http.NewRequest("PUT", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", casambiUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Casambi-Session", sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	// The response is deeply nested JSON. Parse what we need.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	config := &networkConfig{}

	// Parse keyStore.keys
	if networkJSON, ok := raw["network"]; ok {
		var network map[string]json.RawMessage
		json.Unmarshal(networkJSON, &network)

		if ksJSON, ok := network["keyStore"]; ok {
			var keyStore map[string]json.RawMessage
			json.Unmarshal(ksJSON, &keyStore)

			if keysJSON, ok := keyStore["keys"]; ok {
				json.Unmarshal(keysJSON, &config.Keys)
			}
		}

		// Parse units
		if unitsJSON, ok := network["units"]; ok {
			// units is a map of ID → unit object
			var unitsMap map[string]json.RawMessage
			json.Unmarshal(unitsJSON, &unitsMap)

			for _, unitJSON := range unitsMap {
				var u Unit
				json.Unmarshal(unitJSON, &u)
				if u.ID != 0 {
					config.Units = append(config.Units, u)
				}
			}
		}

		// Parse scenes
		if scenesJSON, ok := network["scenes"]; ok {
			var scenesMap map[string]json.RawMessage
			json.Unmarshal(scenesJSON, &scenesMap)

			for _, sceneJSON := range scenesMap {
				var s Scene
				json.Unmarshal(sceneJSON, &s)
				if s.ID != 0 {
					config.Scenes = append(config.Scenes, s)
				}
			}
		}
	}

	return config, nil
}

// go run . --fetch-creds --password '<your-password>' --network '<your-network-id>'
