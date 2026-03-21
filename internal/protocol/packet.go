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
