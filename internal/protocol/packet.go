package protocol

// ╔══════════════════════════════════════════════════════════════╗
// ║  Packet Construction — Phase 5                               ║
// ║                                                              ║
// ║  This is where you build the actual command packets          ║
// ║  that tell your lights what to do.                           ║
// ║                                                              ║
// ║  Before implementing this, you need Phase 3 & 4 working     ║
// ║  (key exchange + encryption), because all command packets    ║
// ║  must be encrypted before sending over BLE.                  ║
// ╚══════════════════════════════════════════════════════════════╝

import "encoding/binary"

// CommandPacket represents a Casambi control command.
//
// Wire format (before encryption):
//   [Flags: 2B] [OpCode: 1B] [Origin: 2B] [Target: 2B] [Reserved: 2B] [Payload: 0-63B]
//
// All header fields are big-endian.
type CommandPacket struct {
	Lifetime uint8      // 0-15, usually 0 (no expiry)
	OpCode   OpCode     // What to do (dim, color, on/off, etc.)
	Origin   uint16     // Auto-incrementing sequence number
	TargetID uint16     // Device/group/scene ID
	Target   TargetType // Unit, group, or scene
	Payload  []byte     // 0-63 bytes, depends on the OpCode
}

// Encode serializes the packet into bytes ready for encryption.
//
// TODO (Phase 5): Implement this method.
// Use encoding/binary.BigEndian for header fields.
func (p *CommandPacket) Encode() []byte {
	payloadLen := len(p.Payload)
	if payloadLen > MaxPayloadSize {
		payloadLen = MaxPayloadSize
	}

	// Encode the flags field:
	// Bits 15-11: lifetime (4 bits)
	// Bits 10-0:  payload length
	flags := (uint16(p.Lifetime&0x0F) << 11) | uint16(payloadLen&0x3F)

	// Encode the target field:
	// target = (id << 8) | type
	target := (p.TargetID << 8) | uint16(p.Target)

	buf := make([]byte, PacketHeaderSize+payloadLen)

	// Write header (big-endian)
	binary.BigEndian.PutUint16(buf[0:2], flags)
	buf[2] = byte(p.OpCode)
	binary.BigEndian.PutUint16(buf[3:5], p.Origin)
	binary.BigEndian.PutUint16(buf[5:7], target)
	binary.BigEndian.PutUint16(buf[7:9], 0x0000) // reserved

	// Append payload
	copy(buf[PacketHeaderSize:], p.Payload[:payloadLen])

	return buf
}

// ─── Convenience Constructors ────────────────────────────────
// These build common command packets.
// Each returns a CommandPacket ready to be encoded and encrypted.

// NewSetLevelCommand creates a brightness command.
// level: 0 (off) to 255 (full brightness)
// targetID: the unit or group ID
// targetType: TargetUnit, TargetGroup, or TargetScene
//
// TODO (Phase 5): The payload format for SetLevel needs the level
// byte packed according to the unit's control resolution.
// For now, we use a simple 1-byte payload.
func NewSetLevelCommand(level uint8, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetLevel,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  []byte{level},
	}
}

// NewSetStateCommand creates an on/off command.
// on: true = turn on, false = turn off
func NewSetStateCommand(on bool, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	state := byte(0)
	if on {
		state = 1
	}
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetState,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  []byte{state},
	}
}

// NewSetRawStateCommand sends pre-packed state bytes to a unit.
// Use this with state bytes from scenes or modes (e.g. "f2ff6097b8" for Purple).
func NewSetRawStateCommand(state []byte, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetState,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  state,
	}
}

// NewSetColorCommand creates an RGB color command using hue and saturation.
// hue: 0-1023 (maps to 0-360 degrees), sat: 0-255
// Uses OpCode 7 (SetColor) — 3-byte payload: [hue_lo, hue_hi, sat]
func NewSetColorCommand(hue uint16, sat uint8, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetColor,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  []byte{byte(hue & 0xFF), byte(hue >> 8), sat},
	}
}

// PackState5ch packs dimmer, hue, saturation, white, and temperature into
// 5 bytes matching the Casambi fixture type 25227 (LINE RGB+TW) state format.
//
// Bit layout (little-endian packing, matching casambi-bt):
//   Offset 0,  8 bits: Dimmer (0-255)
//   Offset 8,  18 bits: RGB = (hue << 8) | saturation
//                        hue: 10 bits (0-1023, maps to 0-360°)
//                        sat: 8 bits (0-255)
//   Offset 26, 6 bits: White color balance (0-63)
//   Offset 32, 8 bits: Color temperature (0-255)
//
// Verified against known cloud API states:
//   PackState5ch(255, 1023, 255, 31, 127) → ffffff7f7f (white)
//   PackState5ch(242, 864, 255, 37, 184)  → f2ff6097b8 (purple)
func PackState5ch(dimmer uint8, hue uint16, sat uint8, white uint8, temp uint8) []byte {
	state := make([]byte, 5)

	// Dimmer: 8 bits at offset 0
	state[0] = dimmer

	// RGB: 18 bits at offset 8 — (hue << 8) | saturation
	if hue > 1023 {
		hue = 1023
	}
	rgb := (uint32(hue) << 8) | uint32(sat)
	state[1] = byte(rgb)
	state[2] = byte(rgb >> 8)
	state[3] = byte(rgb >> 16) & 0x03 // top 2 bits of 18-bit RGB

	// White color balance: 6 bits at offset 26 (byte 3, bit 2)
	wcb := white
	if wcb > 63 {
		wcb = 63
	}
	state[3] |= (wcb & 0x3F) << 2

	// Color temperature: 8 bits at offset 32
	state[4] = temp

	return state
}

// NewSetFullStateCommand creates a full-state command for a 5-channel fixture.
// This sets dimmer, color, white, and temperature atomically in one BLE command.
// Use white=0 to display pure RGB colors without white channel wash-out.
func NewSetFullStateCommand(dimmer uint8, hue uint16, sat uint8, white uint8, temp uint8, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetState,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  PackState5ch(dimmer, hue, sat, white, temp),
	}
}

// NewSetTemperatureCommand creates a color temperature command.
// The actual Kelvin encoding depends on the unit's min/max range
// and control bit resolution — you'll need to normalize it.
//
// TODO (Phase 6): Implement proper Kelvin normalization.
func NewSetTemperatureCommand(kelvin uint16, targetID uint16, targetType TargetType, origin uint16) *CommandPacket {
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, kelvin)
	return &CommandPacket{
		Lifetime: DefaultLifetime,
		OpCode:   OpSetTemperature,
		Origin:   origin,
		TargetID: targetID,
		Target:   targetType,
		Payload:  payload,
	}
}
