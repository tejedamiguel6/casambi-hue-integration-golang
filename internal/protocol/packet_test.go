package protocol

import (
	"encoding/hex"
	"testing"
)

func TestPackState5ch(t *testing.T) {
	tests := []struct {
		name    string
		dimmer  uint8
		hue     uint16
		sat     uint8
		white   uint8
		temp    uint8
		wantHex string
	}{
		{
			name:    "white (from cloud API mode1)",
			dimmer:  255, hue: 1023, sat: 255, white: 31, temp: 127,
			wantHex: "ffffff7f7f",
		},
		{
			name:    "purple (from cloud API scene)",
			dimmer:  242, hue: 864, sat: 255, white: 37, temp: 184,
			wantHex: "f2ff6097b8",
		},
		{
			name:    "off (from cloud API mode2)",
			dimmer:  0, hue: 1023, sat: 255, white: 31, temp: 127,
			wantHex: "00ffff7f7f",
		},
		{
			name:    "pure red, no white",
			dimmer:  255, hue: 0, sat: 255, white: 0, temp: 127,
			wantHex: "ffff00007f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PackState5ch(tt.dimmer, tt.hue, tt.sat, tt.white, tt.temp)
			gotHex := hex.EncodeToString(got)
			if gotHex != tt.wantHex {
				t.Errorf("PackState5ch(%d, %d, %d, %d, %d) = %s, want %s",
					tt.dimmer, tt.hue, tt.sat, tt.white, tt.temp, gotHex, tt.wantHex)
			}
		})
	}
}
