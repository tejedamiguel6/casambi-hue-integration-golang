# casambi-go

Music-reactive lighting controller for **Casambi** professional fixtures and **Philips Hue** lights, written in Go.

casambi-go bridges two lighting ecosystems into one system: it speaks the (reverse-engineered) Casambi Bluetooth Low Energy protocol to control commercial fixtures directly, and streams to Philips Hue lights over the Entertainment API (DTLS/UDP at 25 Hz). On top of that sits a real-time audio engine that makes your lights react to whatever is playing — with colors pulled from the album art of the current Spotify track.

## Features

- **Casambi BLE control** — full protocol implementation: ECDH P-256 key exchange, Casambi's custom AES-CTR encryption, RFC 4493 CMAC authentication, and command packets (brightness, color, scenes). No official API key required (Casambi discontinued their consumer API program in 2025).
- **Philips Hue control** — REST API for manual control, plus the Entertainment API (DTLS/UDP streaming) for low-latency reactive mode.
- **Real-time audio analysis** — microphone or system-audio capture via portaudio, pure-Go FFT, spectral flux beat detection (real onsets, not a fixed BPM metronome), predictive beat timing that fires commands early to compensate for BLE latency, and auto-gain that adapts to volume and mic distance.
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

### 1. Casambi credentials

Fetch and cache your network credentials from the Casambi cloud (one time; open the Casambi app on your phone first so the network gateway is awake):

```bash
go run . --fetch-creds --password '<your-network-password>' --network '<your-network-id>'
```

Credentials are cached in `casambi_credentials.json` (gitignored) — subsequent runs work fully offline.

### 2. Philips Hue (optional)

Register with the bridge (press the link button, then request a username with `generateclientkey: true` to also get the Entertainment PSK), and create an Entertainment Area in the Hue app (Settings → Entertainment Areas).

Put the results in a `.env` file (gitignored):

```bash
HUE_BRIDGE_IP=192.168.x.x
HUE_USERNAME=<hue-application-key>
HUE_CLIENTKEY=<entertainment-psk-hex>          # optional, enables 25 Hz streaming
HUE_ENTERTAINMENT_AREA=<entertainment-config-id>  # required with HUE_CLIENTKEY
SPOTIFY_NOW_PLAYING_URL=<url>                  # optional, enables album art colors
```

To find your entertainment configuration ID:

```bash
curl -sk https://$HUE_BRIDGE_IP/clip/v2/resource/entertainment_configuration \
  -H "hue-application-key: $HUE_USERNAME"
```

### 3. Run

```bash
export $(cat .env | xargs) && go run . --serve --port 8080
```

The server scans for your Casambi network, performs the key exchange and authentication, and starts the REST API.

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
│   └── spotify/             Now-playing polling, BPM cache
```

## Disclaimer

This project is not affiliated with or endorsed by Casambi Technologies or Signify (Philips Hue). The Casambi BLE protocol support is the result of interoperability-focused reverse engineering. Use it with your own lighting networks.

## License

MIT
