// casambi-go — music-reactive lighting controller for Casambi BLE fixtures
// and Philips Hue lights.
//
// Usage:
//
//	casambi-go setup             interactive first-run wizard (writes config)
//	casambi-go serve             connect to the lights and start the REST API
//	casambi-go --fetch-creds     fetch + cache cloud credentials only (no BLE)
//
// Configuration lives in ~/.config/casambi-go/config.yaml (see 'setup').
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/migueltejeda/casambi-go/internal/api"
	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/config"
	"github.com/migueltejeda/casambi-go/internal/crypto"
	"github.com/migueltejeda/casambi-go/internal/setup"
)

func main() {
	// Subcommand-style CLI: "casambi-go setup", "casambi-go serve".
	// The pre-existing flag spellings (--serve, --fetch-creds) keep working.
	args := os.Args[1:]
	command := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("casambi-go", flag.ExitOnError)
	password := fs.String("password", "", "Casambi network password")
	mac := fs.String("mac", "", "Network UUID from Casambi app (e.g. a1b2c3d4e5f6)")
	network := fs.String("network", "", "Casambi API network ID (if already known)")
	fetchCreds := fs.Bool("fetch-creds", false, "Fetch and cache cloud credentials only (no BLE)")
	serve := fs.Bool("serve", false, "Start REST API server after connecting")
	port := fs.Int("port", 0, "REST API port (overrides config)")
	configPath := fs.String("config", "", "Path to config.yaml (default: ~/.config/casambi-go/config.yaml)")
	fs.Parse(args)

	cfg, configFound, err := config.Load(*configPath)
	if err != nil {
		fmt.Println("Config error:", err)
		os.Exit(1)
	}
	cfg.ApplyEnvOverrides()
	if *port != 0 {
		cfg.Server.Port = *port
	}

	switch command {
	case "setup":
		if err := setup.Run(cfg, *configPath); err != nil {
			fmt.Println("Setup failed:", err)
			os.Exit(1)
		}
		return
	case "serve":
		*serve = true
	case "":
		// flag-only invocation
	default:
		fmt.Printf("Unknown command %q\n\n", command)
		fmt.Println("Usage:")
		fmt.Printf("  %s setup    interactive configuration wizard\n", setup.Invocation())
		fmt.Printf("  %s serve    connect and start the REST API + dashboard\n", setup.Invocation())
		os.Exit(2)
	}

	fmt.Println("Casambi BLE Controller")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// Cloud-only mode: fetch and cache credentials, then exit.
	// (Open the Casambi app on your phone first so the network gateway is awake.)
	if *fetchCreds {
		if *password == "" || *network == "" {
			fmt.Println("Usage: casambi-go --fetch-creds --password <pw> --network <network-id>")
			return
		}
		creds, err := api.FetchCredentialsWithNetworkID(*network, *password)
		if err != nil {
			fmt.Println("Cloud API error:", err)
			return
		}
		if err := api.SaveCredentials(creds, config.CredentialsPath()); err != nil {
			fmt.Println("Failed to save credentials:", err)
			return
		}
		fmt.Printf("\nCredentials saved to %s\n", config.CredentialsPath())
		fmt.Printf("  Key ID: %d\n", creds.KeyID)
		fmt.Printf("  Units:  %d\n", len(creds.Units))
		return
	}

	if !configFound {
		fmt.Printf("No config file found — run '%s setup' for guided configuration.\n\n", setup.Invocation())
	}

	// Scan for Casambi devices
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

	// Connect and read device info
	conn, err := ble.Connect(device)
	if err != nil {
		fmt.Println("Connect error:", err)
		return
	}
	fmt.Println("Connected!")

	// ECDH key exchange
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

	// Load credentials (cached, with legacy working-directory fallback)
	creds := api.LoadCachedCredentials(config.CredentialsPath(), "casambi_credentials.json")
	if creds != nil {
		fmt.Printf("Loaded cached credentials for network %s (%d units, %d scenes)\n",
			creds.NetworkID, len(creds.Units), len(creds.Scenes))
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
		api.SaveCredentials(creds, config.CredentialsPath())
		fmt.Println("Credentials fetched and cached!")
	} else {
		fmt.Println("\nNo cached credentials found.")
		fmt.Printf("Run '%s setup' for guided configuration, or fetch credentials directly:\n", setup.Invocation())
		fmt.Printf("  %s --fetch-creds --password <pw> --network <network-id>\n", setup.Invocation())
		return
	}

	// BLE authentication
	fmt.Println("\nAuthenticating...")
	err = conn.AuthenticateWithKey(creds.AESKey, creds.KeyID, ke.TransportKey)
	if err != nil {
		fmt.Println("Auth error:", err)
		return
	}
	fmt.Println("Authenticated! Ready for light control.")

	if *serve {
		// Hue is optional — Casambi-only setups just skip it.
		var hue *api.HueClient
		var streamer *api.HueStreamer
		if cfg.Hue.Configured() {
			hue = &api.HueClient{
				BridgeIP: cfg.Hue.BridgeIP,
				Username: cfg.Hue.Username,
			}
			if cfg.Hue.StreamingConfigured() {
				channels := cfg.Hue.EntertainmentChannels
				if len(channels) == 0 {
					channels = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
				}
				s, err := api.NewHueStreamer(cfg.Hue.BridgeIP, cfg.Hue.Username, cfg.Hue.ClientKey, cfg.Hue.EntertainmentArea, channels)
				if err != nil {
					fmt.Println("Hue Entertainment setup error (falling back to REST):", err)
				} else {
					streamer = s
				}
			}
		} else {
			fmt.Println("No Hue bridge configured — running Casambi-only. ('casambi-go setup' adds one.)")
		}

		// Reactive mode drives the configured lights; with nothing configured,
		// default to every Casambi unit on the network.
		reactiveUnits := cfg.Reactive.CasambiUnits
		if len(reactiveUnits) == 0 {
			for _, u := range creds.Units {
				reactiveUnits = append(reactiveUnits, uint16(u.ID))
			}
			fmt.Println("No reactive.casambi_units configured — defaulting to all units.")
		}

		server := api.NewServer(conn, creds, hue, api.ServerOptions{
			Bind:                 cfg.Server.Bind,
			Port:                 cfg.Server.Port,
			SpotifyURL:           cfg.Spotify.NowPlayingURL,
			ReactiveCasambiUnits: reactiveUnits,
			ReactiveHueLights:    cfg.Reactive.HueLights,
			BPMCachePath:         config.BPMCachePath(),
			Config:               cfg,
			ConfigPath:           *configPath,
		})
		server.HueStreamer = streamer
		if err := server.Start(); err != nil {
			fmt.Println("Server error:", err)
		}
	}
}
