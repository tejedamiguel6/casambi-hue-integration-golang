# casambi-go

Music-reactive lighting controller for **Casambi** professional fixtures and **Philips Hue** lights, written in Go.

casambi-go bridges two lighting ecosystems into one system: it speaks the (reverse-engineered) Casambi Bluetooth Low Energy protocol to control commercial fixtures directly, and streams to Philips Hue lights over the Entertainment API (DTLS/UDP at 25 Hz). On top of that sits a real-time audio engine that makes your lights react to whatever is playing — with colors pulled from the album art of the current Spotify track.

## Features

- **Casambi BLE control** — full protocol implementation: ECDH P-256 key exchange, Casambi's custom AES-CTR encryption, RFC 4493 CMAC authentication, and command packets (brightness, color, scenes). No official API key required (Casambi discontinued their consumer API program in 2025).
- **Philips Hue control** — REST API for manual control, plus the Entertainment API (DTLS/UDP streaming) for low-latency reactive mode.
- **Real-time audio analysis** — microphone or system-audio capture via portaudio, pure-Go FFT, spectral flux beat detection (real onsets, not a fixed BPM metronome), predictive beat timing that fires commands early to compensate for BLE latency, and auto-gain that adapts to volume and mic distance.
- **Song analyzer** — a pure-Go replacement for Spotify's dead audio-features API. The first time a track plays, its audio is analyzed offline: tempo + beat grid (autocorrelation over the onset envelope, anchored to Spotify `progress_ms`), energy and dynamics, key/mode (Krumhansl-Schmuckler chromagram matching), plus danceability, valence, and brightness scores. The profile is cached in `track_profiles.json`, so every later play gets instant BPM and mood without re-listening. Legacy `bpm_cache.json` entries (tap tempo) are imported automatically and upgraded to full profiles as tracks replay.
- **AI track enrichment (optional)** — with an Anthropic API key configured, each analyzed track gets one small Claude API call adding what DSP can't measure: genre and mood tags, refined valence/danceability/energy, a color palette matched to the song's character, a pulse style (strobe / pulse / wash / breathe) that tunes how hard the lights hit, an intensity bias, and — for tracks Claude knows — song structure notes (builds, drops). The engine applies the lighting direction live: pulse style sets brightness decay and color-flash pacing, and the AI palette covers tracks whose album art yields no usable colors. Everything is cached in the same profile, so it's one API call per new song, ever.
- **Album art color extraction** — when the track changes, the current album cover is downloaded and its two dominant vibrant colors drive the light palette. A dark hip-hop record gets deep purples; a bright pop album gets warm oranges.
- **REST API** — everything is controllable over HTTP from any device on your network. `GET /` returns a self-documenting endpoint list with curl examples.
- **Single binary** — cross-compiles to a Raspberry Pi sitting next to your lights.

## Requirements

- Go 1.22+
- macOS or Linux with Bluetooth LE
- [portaudio](https://www.portaudio.com/) for audio capture (`brew install portaudio` on macOS, `apt install portaudio19-dev` on Debian/Ubuntu)
- A Casambi network with **sharing enabled** in the Casambi app, and its network password
- Optionally: a Philips Hue bridge for Hue lights

## Setup

One command:

```bash
go run . setup
```

The wizard scans for your Casambi network, fetches and caches your credentials from the Casambi cloud (open the Casambi app on your phone first so the network gateway is awake), lets you pick which lights react to music, optionally pairs a Philips Hue bridge (automatic discovery + link-button registration + Entertainment area selection), and writes everything to `~/.config/casambi-go/config.yaml`:

```yaml
server:
  bind: 127.0.0.1   # 0.0.0.0 exposes the dashboard to your LAN (no auth!)
  port: 8080
casambi:
  network_id: <your-network-id>
hue:
  bridge_ip: 192.168.x.x            # empty = Casambi-only, Hue disabled
  username: <hue-application-key>
  clientkey: <entertainment-psk>    # enables 25 Hz DTLS streaming
  entertainment_area: <config-id>
spotify:
  now_playing_url: <url>            # optional, enables album-art colors
reactive:
  casambi_units: [1, 4]             # which lights react to music
  hue_lights: ["6", "19"]
ai:
  anthropic_api_key: <key>          # optional, enables AI track enrichment
  model: claude-haiku-4-5           # default; any Claude model works
```

Credentials are cached next to the config in `credentials.json` — after setup, light control works fully offline. The config file is safe to edit by hand, and the old `HUE_*` / `SPOTIFY_NOW_PLAYING_URL` environment variables still override it.

### Run

```bash
go run . serve
```

The server scans for your Casambi network, performs the key exchange and authentication, and starts the REST API + web dashboard.

## Usage

```bash
# Start music-reactive mode:
curl -X POST localhost:8080/api/reactive/start

# See levels, detected BPM, extracted colors, current track:
curl localhost:8080/api/reactive/status

# Adjust mic sensitivity (auto-gain is on by default):
curl -X POST localhost:8080/api/reactive/gain -d '{"gain": 30}'

# Stop:
curl -X POST localhost:8080/api/reactive/stop

# Song analyzer — profiling progress + the current track's profile
# (BPM, beat phase, energy, key, danceability, valence):
curl localhost:8080/api/analyzer/status

# All cached track profiles:
curl localhost:8080/api/analyzer/profiles

# Re-analyze the current track from scratch:
curl -X POST localhost:8080/api/analyzer/reanalyze

# Re-run AI enrichment for the current track (needs ai.anthropic_api_key):
curl -X POST localhost:8080/api/analyzer/enrich

# Direct control:
curl -X POST localhost:8080/api/units/1/on
curl -X POST localhost:8080/api/units/1/level -d '{"level": 128}'
curl -X POST localhost:8080/api/units/1/color -d '{"hue": 864, "sat": 255}'
curl -X POST localhost:8080/api/scenes/2/on
curl -X POST localhost:8080/api/hue/lights/19/color -d '{"hue": 46000, "sat": 254, "bri": 200}'

# Full endpoint list with examples:
curl localhost:8080/
```

## How it works

```
┌─────────────┐   44.1 kHz    ┌──────────────────────────────┐
│ mic / system│ ───────────▶  │ ReactiveEngine               │
│ audio       │               │ FFT → spectral flux beats    │
└─────────────┘               │ auto-gain · BPM prediction   │
                              └──────┬───────────────┬───────┘
┌─────────────┐  album art           │ BLE           │ DTLS/UDP 25 Hz
│ Spotify     │ ───────────▶ colors  ▼               ▼
│ now playing │           ┌──────────────┐   ┌──────────────┐
└─────────────┘           │ Casambi mesh │   │ Hue bridge   │
                          │ (encrypted)  │   │ Entertainment│
                          └──────────────┘   └──────────────┘
```

The Casambi protocol work follows the trail blazed by [casambi-bt](https://github.com/lkempf/casambi-bt) (Python) — packet formats, the custom CTR mode, and the cloud API quirks were verified against its source. This is, to our knowledge, the only Go implementation.

## Project structure

```
├── main.go                  Entry point: scan → connect → key exchange → auth → serve
├── internal/
│   ├── ble/                 BLE scanning, connection, key exchange, authenticated commands
│   ├── crypto/              ECDH, AES-CTR (standard + Casambi custom), RFC 4493 CMAC
│   ├── protocol/            Packet encoding, opcodes, fixture state packing
│   ├── api/                 Casambi cloud API, REST server, Hue REST + Entertainment clients
│   ├── audio/               Reactive engine: capture, FFT, beat detection, color mapping
│   ├── analyzer/            Song analyzer: tempo/beat grid, energy, key/mood, profile cache
│   └── spotify/             Now-playing polling, BPM cache
```

## Disclaimer

This project is not affiliated with or endorsed by Casambi Technologies or Signify (Philips Hue). The Casambi BLE protocol support is the result of interoperability-focused reverse engineering. Use it with your own lighting networks.

## License

MIT
