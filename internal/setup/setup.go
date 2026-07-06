// Package setup implements the interactive first-run wizard:
// scan for the Casambi network, fetch and cache cloud credentials, pick the
// lights that participate in reactive mode, optionally pair a Philips Hue
// bridge, and write the config file.
package setup

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/migueltejeda/casambi-go/internal/api"
	"github.com/migueltejeda/casambi-go/internal/ble"
	"github.com/migueltejeda/casambi-go/internal/config"
)

// Run walks the user through configuration and writes the config file.
// An existing config's values are kept as defaults, so re-running is safe.
func Run(cfg *config.Config, configPath string) error {
	in := bufio.NewReader(os.Stdin)

	fmt.Println("casambi-go setup")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("This wizard writes %s.\nPress Ctrl-C at any time to abort; nothing is saved until the end.\n\n", displayPath(configPath))

	creds, err := setupCasambi(in, cfg)
	if err != nil {
		return err
	}

	if creds != nil {
		selectReactiveUnits(in, cfg, creds)
	}

	fmt.Println()
	if askYesNo(in, "Configure a Philips Hue bridge?", cfg.Hue.Configured()) {
		if err := setupHue(in, cfg); err != nil {
			fmt.Printf("  Hue setup failed: %v\n", err)
			fmt.Println("  Continuing without Hue — re-run 'casambi-go setup' or edit the config later.")
		}
	}

	fmt.Println()
	setupSpotify(in, cfg)

	fmt.Println()
	setupServer(in, cfg)

	if err := cfg.Save(configPath); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Config written to %s\n\n", displayPath(configPath))
	fmt.Println("Start the server with:")
	fmt.Printf("  %s serve\n", Invocation())
	return nil
}

// Invocation returns how to invoke this program again: "go run ." when
// running under `go run` (the executable lives in the go-build temp dir),
// otherwise the binary name.
func Invocation() string {
	exe, err := os.Executable()
	if err == nil && strings.Contains(exe, "go-build") {
		return "go run ."
	}
	if err != nil || exe == "" {
		return "casambi-go"
	}
	return filepath.Base(exe)
}

// ── Casambi ───────────────────────────────────────────────

func setupCasambi(in *bufio.Reader, cfg *config.Config) (*api.NetworkCredentials, error) {
	fmt.Println("── Casambi network ──")

	// Reuse cached credentials when they exist and the user wants them
	// (including a legacy casambi_credentials.json in the working directory).
	if cached := api.LoadCachedCredentials(config.CredentialsPath(), "casambi_credentials.json"); cached != nil && len(cached.Units) > 0 {
		fmt.Printf("Found cached credentials for network %s (%d units, %d scenes).\n",
			cached.NetworkID, len(cached.Units), len(cached.Scenes))
		if askYesNo(in, "Use them?", true) {
			return saveCreds(cfg, cached)
		}
	}

	// A BLE scan confirms the lights are in range. On Linux the scanned
	// address is the real MAC and can be resolved to a network ID via the
	// cloud; macOS reports a CoreBluetooth UUID that cannot.
	var scanned []ble.CasambiDevice
	if askYesNo(in, "Scan for nearby Casambi networks (5s)?", true) {
		fmt.Println()
		scanned = ble.ScanAll(5 * time.Second)
		if len(scanned) == 0 {
			fmt.Println("No Casambi devices found. Make sure you're within Bluetooth range.")
			if !askYesNo(in, "Continue anyway?", false) {
				return nil, fmt.Errorf("aborted: no Casambi devices in range")
			}
		}
	}

	fmt.Println()
	fmt.Println("Casambi's cloud only answers while your phone acts as the network gateway.")
	ask(in, "Open the Casambi app on your phone now, then press Enter", "")

	password := askPassword("Casambi network password")

	// Try resolving the network ID from a scanned device address first
	// (works on Linux where the real MAC is visible).
	if len(scanned) > 0 {
		addr := scanned[0].Address.String()
		if strings.Contains(addr, ":") {
			fmt.Printf("Trying to look up the network via device %s...\n", addr)
			creds, err := api.FetchCredentials(addr, password)
			if err == nil {
				return saveCreds(cfg, creds)
			}
			fmt.Printf("  Lookup via device address failed (%v) — falling back to network ID.\n", err)
		}
	}

	fmt.Println()
	fmt.Println("Enter your network ID: a hex string like a1b2c3d4e5f6, shown in the")
	fmt.Println("Casambi app under Network Setup, or in the URL when you open your")
	fmt.Println("network on https://casambi.com.")
	for {
		networkID := ask(in, "Network ID", cfg.Casambi.NetworkID)
		if networkID == "" {
			fmt.Println("  A network ID is required.")
			continue
		}
		creds, err := api.FetchCredentialsWithNetworkID(networkID, password)
		if err != nil {
			fmt.Printf("  Fetching credentials failed: %v\n", err)
			fmt.Println("  Check that the Casambi app is open and the password is correct.")
			if askYesNo(in, "Try again?", true) {
				continue
			}
			return nil, fmt.Errorf("could not fetch Casambi credentials")
		}
		return saveCreds(cfg, creds)
	}
}

func saveCreds(cfg *config.Config, creds *api.NetworkCredentials) (*api.NetworkCredentials, error) {
	if err := api.SaveCredentials(creds, config.CredentialsPath()); err != nil {
		return nil, fmt.Errorf("save credentials: %w", err)
	}
	cfg.Casambi.NetworkID = creds.NetworkID
	fmt.Printf("Credentials cached in %s — future runs work offline.\n", displayPath(config.CredentialsPath()))
	if len(creds.Units) == 0 {
		fmt.Println()
		fmt.Println("WARNING: no units came back from the Casambi cloud — the phone gateway")
		fmt.Println("likely dropped mid-fetch. Light control will not work until this is fixed:")
		fmt.Println("re-run setup with the Casambi app open in the foreground on your phone.")
	}
	return creds, nil
}

func selectReactiveUnits(in *bufio.Reader, cfg *config.Config, creds *api.NetworkCredentials) {
	if len(creds.Units) == 0 {
		fmt.Println("Skipping unit selection — no units known (see warning above).")
		return
	}
	units := make([]api.Unit, len(creds.Units))
	copy(units, creds.Units)
	sort.Slice(units, func(i, j int) bool { return units[i].ID < units[j].ID })

	fmt.Println()
	fmt.Println("Which Casambi units should music-reactive mode drive?")
	for _, u := range units {
		fmt.Printf("  [%d] %s (%s)\n", u.ID, u.Name, u.Type)
	}
	def := formatUnitIDs(cfg.Reactive.CasambiUnits)
	for {
		answer := ask(in, "Unit IDs, comma-separated (Enter = all)", def)
		if answer == "" {
			cfg.Reactive.CasambiUnits = nil
			for _, u := range units {
				cfg.Reactive.CasambiUnits = append(cfg.Reactive.CasambiUnits, uint16(u.ID))
			}
			return
		}
		ids, err := parseUnitIDs(answer)
		if err != nil {
			fmt.Printf("  %v\n", err)
			continue
		}
		cfg.Reactive.CasambiUnits = ids
		return
	}
}

// ── Hue ───────────────────────────────────────────────────

func setupHue(in *bufio.Reader, cfg *config.Config) error {
	fmt.Println()
	fmt.Println("── Philips Hue ──")

	// Bridge IP: try automatic discovery, fall back to manual entry.
	defIP := cfg.Hue.BridgeIP
	if defIP == "" {
		fmt.Println("Looking for Hue bridges on your network...")
		if bridges := discoverBridges(); len(bridges) > 0 {
			for _, b := range bridges {
				fmt.Printf("  Found bridge %s at %s\n", b.ID, b.IP)
			}
			defIP = bridges[0].IP
		} else {
			fmt.Println("  None found automatically (the discovery service needs internet).")
			fmt.Println("  You can find the IP in the Hue app under Settings → My hue system → bridge.")
		}
	}
	bridgeIP := ask(in, "Bridge IP", defIP)
	if bridgeIP == "" {
		return fmt.Errorf("no bridge IP given")
	}
	cfg.Hue.BridgeIP = bridgeIP

	// Registration (link button dance) — skipped when we already have a key.
	if cfg.Hue.Username != "" && askYesNo(in, "Keep the existing bridge key?", true) {
		fmt.Println("  Keeping existing credentials.")
	} else {
		registered := false
		for attempt := 1; attempt <= 5; attempt++ {
			ask(in, "Press the round link button on the bridge, then press Enter", "")
			username, clientKey, ok, err := registerWithBridge(bridgeIP)
			if err != nil {
				return fmt.Errorf("register with bridge: %w", err)
			}
			if !ok {
				fmt.Println("  Bridge says the link button wasn't pressed — try again.")
				continue
			}
			cfg.Hue.Username = username
			cfg.Hue.ClientKey = clientKey
			registered = true
			fmt.Println("  Registered with the bridge.")
			if clientKey != "" {
				fmt.Println("  Got an Entertainment streaming key too (25 Hz reactive streaming).")
			}
			break
		}
		if !registered {
			return fmt.Errorf("bridge registration not confirmed after 5 attempts")
		}
	}

	// Entertainment area — needed for DTLS streaming.
	if cfg.Hue.ClientKey != "" {
		areas, err := listEntertainmentAreas(bridgeIP, cfg.Hue.Username)
		switch {
		case err != nil:
			fmt.Printf("  Could not list entertainment areas: %v\n", err)
			fmt.Println("  Reactive mode will use the (slower) REST API for Hue.")
		case len(areas) == 0:
			fmt.Println("  No entertainment areas on this bridge. Create one in the Hue app")
			fmt.Println("  (Settings → Entertainment areas) and re-run setup to enable streaming.")
		default:
			fmt.Println("Entertainment areas (used for low-latency streaming):")
			for i, a := range areas {
				fmt.Printf("  [%d] %s (%d channels)\n", i+1, a.Name, len(a.Channels))
			}
			choice := askInt(in, "Pick one (0 = skip streaming)", 1, 0, len(areas))
			if choice > 0 {
				cfg.Hue.EntertainmentArea = areas[choice-1].ID
				cfg.Hue.EntertainmentChannels = areas[choice-1].Channels
			}
		}
	}

	// Which Hue lights participate in reactive mode.
	hue := &api.HueClient{BridgeIP: bridgeIP, Username: cfg.Hue.Username}
	lights, err := hue.ListLights()
	if err != nil || len(lights) == 0 {
		fmt.Println("  Could not list lights; set reactive.hue_lights in the config by hand.")
		return nil
	}
	sort.Slice(lights, func(i, j int) bool { return lights[i].ID < lights[j].ID })
	fmt.Println()
	fmt.Println("Which Hue lights should music-reactive mode drive?")
	for _, l := range lights {
		note := ""
		if !l.Reachable {
			note = " (unreachable)"
		}
		fmt.Printf("  [%s] %s%s\n", l.ID, l.Name, note)
	}
	def := strings.Join(cfg.Reactive.HueLights, ",")
	answer := ask(in, "Light IDs, comma-separated (Enter = all reachable)", def)
	if answer == "" {
		cfg.Reactive.HueLights = nil
		for _, l := range lights {
			if l.Reachable {
				cfg.Reactive.HueLights = append(cfg.Reactive.HueLights, l.ID)
			}
		}
	} else {
		cfg.Reactive.HueLights = splitIDs(answer)
	}
	return nil
}

// ── Spotify + server ──────────────────────────────────────

func setupSpotify(in *bufio.Reader, cfg *config.Config) {
	fmt.Println("── Spotify (optional) ──")
	fmt.Println("Reactive mode works standalone with just audio capture. If you run a")
	fmt.Println("now-playing endpoint for your Spotify account, casambi-go can also pull")
	fmt.Println("album-art colors and pause detection from it (see README for the JSON shape).")
	cfg.Spotify.NowPlayingURL = ask(in, "Now-playing URL (Enter to skip)", cfg.Spotify.NowPlayingURL)
}

func setupServer(in *bufio.Reader, cfg *config.Config) {
	fmt.Println("── Server ──")
	cfg.Server.Port = askInt(in, "API port", cfg.Server.Port, 1, 65535)
	lan := askYesNo(in, "Allow other devices on your network to open the dashboard? (no auth!)", cfg.Server.Bind == "0.0.0.0")
	if lan {
		cfg.Server.Bind = "0.0.0.0"
	} else {
		cfg.Server.Bind = "127.0.0.1"
	}
}

// ── Prompt helpers ────────────────────────────────────────

func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askYesNo(in *bufio.Reader, prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	answer := ask(in, fmt.Sprintf("%s [%s]", prompt, hint), "")
	if answer == "" {
		return def
	}
	answer = strings.ToLower(answer)
	return answer == "y" || answer == "yes"
}

func askInt(in *bufio.Reader, prompt string, def, min, max int) int {
	for {
		answer := ask(in, prompt, strconv.Itoa(def))
		n, err := strconv.Atoi(answer)
		if err != nil || n < min || n > max {
			fmt.Printf("  Enter a number between %d and %d.\n", min, max)
			continue
		}
		return n
	}
}

// askPassword reads without echoing when stdin is a terminal.
func askPassword(prompt string) string {
	fmt.Printf("%s: ", prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err == nil {
			return strings.TrimSpace(string(pw))
		}
	}
	// Not a terminal (piped input) — read a plain line.
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func parseUnitIDs(s string) ([]uint16, error) {
	var ids []uint16
	for _, part := range splitIDs(s) {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 65535 {
			return nil, fmt.Errorf("invalid unit ID %q", part)
		}
		ids = append(ids, uint16(n))
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no unit IDs given")
	}
	return ids, nil
}

func splitIDs(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func formatUnitIDs(ids []uint16) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(int(id))
	}
	return strings.Join(parts, ",")
}

func displayPath(path string) string {
	if path == "" {
		return config.Path()
	}
	return path
}
