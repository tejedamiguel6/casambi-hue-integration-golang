package main

// ╔══════════════════════════════════════════════════════════════╗
// ║  PHASE 1: BLE Scanner                                       ║
// ║  Goal: Find Casambi devices nearby using Bluetooth           ║
// ║                                                              ║
// ║  This is your starting point. Run this on your Raspberry Pi  ║
// ║  while standing near your Casambi lights. You should see     ║
// ║  them appear in the scan results.                            ║
// ║                                                              ║
// ║  What you'll learn:                                          ║
// ║  - How tinygo-org/bluetooth works                            ║
// ║  - BLE scanning and advertisement parsing                    ║
// ║  - Filtering devices by service UUID                         ║
// ╚══════════════════════════════════════════════════════════════╝

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/migueltejeda/casambi-go/internal/api"
	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/crypto"
)

func main() {
	password := flag.String("password", "", "Casambi network password")
	mac := flag.String("mac", "", "Network UUID from Casambi app (e.g. e280ab018da7)")
	network := flag.String("network", "", "Casambi API network ID (if already known)")
	fetchCreds := flag.Bool("fetch-creds", false, "Fetch and cache cloud credentials only (no BLE)")
	serve := flag.Bool("serve", false, "Start REST API server after connecting")
	port := flag.String("port", "8080", "REST API port")
	flag.Parse()

	fmt.Println("Casambi BLE Controller")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Cloud-only mode: fetch and cache credentials, then exit.
	// Usage: Open Casambi app on phone, then immediately run:
	//   go run . --fetch-creds --password <pw> --network <id>
	if *fetchCreds {
		if *password == "" || *network == "" {
			fmt.Println("Usage: go run . --fetch-creds --password <pw> --network <network-id>")
			return
		}
		creds, err := api.FetchCredentialsWithNetworkID(*network, *password)
		if err != nil {
			fmt.Println("Cloud API error:", err)
			return
		}
		if err := api.SaveCredentials(creds); err != nil {
			fmt.Println("Failed to save credentials:", err)
			return
		}
		fmt.Println("\nCredentials saved to casambi_credentials.json!")
		fmt.Printf("  AES Key: %x\n", creds.AESKey)
		fmt.Printf("  Key ID:  %d\n", creds.KeyID)
		fmt.Printf("  Units:   %d\n", len(creds.Units))
		return
	}

	// Phase 1: Scan for Casambi devices
	fmt.Println("Scanning for Casambi devices (5 seconds)...")
	fmt.Println()

	devices := ble.ScanAll(5 * time.Second)
	if len(devices) == 0 {
		fmt.Println("No Casambi devices found")
		return
	}
	fmt.Printf("Found %d Casambi device(s)\n\n", len(devices))

	// Use the first device
	device := devices[0]
	fmt.Printf("── Connecting to: %s ──\n", device.Name)

	// Phase 2: Connect and read device info
	conn, err := ble.Connect(device)
	if err != nil {
		fmt.Println("Connect error:", err)
		return
	}
	fmt.Printf("Connected! Device nonce: %x\n\n", conn.DeviceNonce)

	// Phase 3: ECDH key exchange
	ke, err := crypto.NewKeyExchange()
	if err != nil {
		fmt.Println("Key exchange init error:", err)
		return
	}

	err = conn.PerformKeyExchange(ke)
	if err != nil {
		fmt.Println("Key exchange error:", err)
		return
	}
	fmt.Printf("Transport key: %x\n\n", ke.TransportKey)

	// Phase 5a: Load credentials (cached or from cloud API)
	creds := api.LoadCachedCredentials()
	if creds != nil {
		fmt.Println("Loaded cached credentials from casambi_credentials.json")
		fmt.Printf("  AES Key: %x\n", creds.AESKey)
		fmt.Printf("  Key ID:  %d\n", creds.KeyID)
		fmt.Printf("  Units:   %d\n", len(creds.Units))
	} else if *password != "" && (*network != "" || *mac != "") {
		if *network != "" {
			creds, err = api.FetchCredentialsWithNetworkID(*network, *password)
		} else {
			creds, err = api.FetchCredentials(*mac, *password)
		}
		if err != nil {
			fmt.Println("Cloud API error:", err)
			return
		}
		api.SaveCredentials(creds)
		fmt.Println("Credentials fetched and cached!")
	} else {
		fmt.Println("\nNo cached credentials found.")
		fmt.Println("Run with --fetch-creds first (with Casambi app open):")
		fmt.Println("  go run . --fetch-creds --password <pw> --network <network-id>")
		return
	}

	// Phase 5b: BLE Authentication
	fmt.Println("\nAuthenticating...")
	err = conn.AuthenticateWithKey(creds.AESKey, creds.KeyID, ke.TransportKey)
	if err != nil {
		fmt.Println("Auth error:", err)
		return
	}
	fmt.Println("Authenticated! Ready for light control.")

	if *serve {
		bridgeIP := os.Getenv("HUE_BRIDGE_IP")
		username := os.Getenv("HUE_USERNAME")
		clientKey := os.Getenv("HUE_CLIENTKEY")
		entertainmentArea := os.Getenv("HUE_ENTERTAINMENT_AREA")

		if bridgeIP == "" || username == "" {
			fmt.Println("Set HUE_BRIDGE_IP and HUE_USERNAME environment variables")
			fmt.Println("  export HUE_BRIDGE_IP=192.168.x.x")
			fmt.Println("  export HUE_USERNAME=your-hue-username")
			return
		}

		// REST client for manual control endpoints
		hue := &api.HueClient{
			BridgeIP: bridgeIP,
			Username: username,
		}

		// Entertainment streamer for reactive mode (if clientkey available)
		var streamer *api.HueStreamer
		if clientKey != "" {
			if entertainmentArea == "" {
				fmt.Println("HUE_CLIENTKEY is set but HUE_ENTERTAINMENT_AREA is not")
				fmt.Println("  export HUE_ENTERTAINMENT_AREA=<entertainment-configuration-id>")
				fmt.Println("  (find it via: curl -sk https://$HUE_BRIDGE_IP/clip/v2/resource/entertainment_configuration -H \"hue-application-key: $HUE_USERNAME\")")
			} else {
				channels := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
				s, err := api.NewHueStreamer(bridgeIP, username, clientKey, entertainmentArea, channels)
				if err != nil {
					fmt.Println("Hue Entertainment setup error:", err)
				} else {
					streamer = s
				}
			}
		}

		server := api.NewServer(conn, creds, hue, *port)
		server.HueStreamer = streamer
		if err := server.Start(); err != nil {
			fmt.Println("Server error:", err)
		}
	}
}
