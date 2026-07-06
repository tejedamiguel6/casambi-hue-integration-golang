package config

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	want := Default()
	want.Casambi.NetworkID = "abc123"
	want.Hue.BridgeIP = "192.168.1.50"
	want.Hue.Username = "hue-user"
	want.Hue.ClientKey = "deadbeef"
	want.Hue.EntertainmentArea = "area-1"
	want.Hue.EntertainmentChannels = []int{0, 1, 2}
	want.Spotify.NowPlayingURL = "http://example.com/now"
	want.Reactive.CasambiUnits = []uint16{1, 4}
	want.Reactive.HueLights = []string{"6", "19"}
	want.Server.Port = 9090

	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, found, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("Load: found=false for existing file")
	}
	if got.Casambi.NetworkID != want.Casambi.NetworkID ||
		got.Hue.BridgeIP != want.Hue.BridgeIP ||
		got.Hue.EntertainmentArea != want.Hue.EntertainmentArea ||
		got.Spotify.NowPlayingURL != want.Spotify.NowPlayingURL ||
		got.Server.Port != 9090 ||
		len(got.Reactive.CasambiUnits) != 2 || got.Reactive.CasambiUnits[1] != 4 ||
		len(got.Reactive.HueLights) != 2 || got.Reactive.HueLights[0] != "6" ||
		len(got.Hue.EntertainmentChannels) != 3 {
		t.Errorf("round trip mismatch:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, found, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found {
		t.Error("found=true for missing file")
	}
	if cfg.Server.Bind != "127.0.0.1" || cfg.Server.Port != 8080 {
		t.Errorf("defaults wrong: %+v", cfg.Server)
	}
	if cfg.Hue.Configured() {
		t.Error("empty Hue config reports Configured")
	}
}
