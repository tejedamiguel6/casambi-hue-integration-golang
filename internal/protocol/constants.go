package protocol

// Casambi BLE Protocol Constants
// Reverse-engineered from casambi-bt (Python) and esp32-casambi (C++)
// Reference: https://github.com/lkempf/casambi-bt
// Reference: https://github.com/lian/esp32-casambi

// ─── BLE UUIDs ───────────────────────────────────────────────

// CasambiServiceUUID is the primary BLE service advertised by Casambi devices.
// This is what you scan for to find Casambi networks nearby.
const CasambiServiceUUID = "0000fe4d-0000-1000-8000-00805f9b34fb"

// CasambiCharacteristicUUID is the GATT characteristic used for all
// communication (key exchange, auth, commands).
const CasambiCharacteristicUUID = "c9ffde48-ca5a-0001-ab83-8f519b482f77"

// CasambiManufacturerID appears in BLE advertisement data.
const CasambiManufacturerID = 0x03C3

// ─── Protocol Versions ───────────────────────────────────────

const ProtocolVersionMin = 10
const ProtocolVersionMax = 11

// ─── OpCodes ─────────────────────────────────────────────────
// These are the command types you send to control lights.
// Each one goes into the OpCode byte of a command packet.

type OpCode uint8

const (
	OpResponse       OpCode = 0  // Status/acknowledgment from device
	OpSetLevel       OpCode = 1  // Brightness: 0-255
	OpSetTemperature OpCode = 3  // Color temperature (Kelvin range)
	OpSetVertical    OpCode = 4  // Motor/vertical positioning: 0-255
	OpSetWhite       OpCode = 5  // White channel intensity: 0-255
	OpSetColor       OpCode = 7  // RGB color mode
	OpSetSlider      OpCode = 12 // Generic slider: 0-255
	OpSetState       OpCode = 48 // Power on/off toggle
	OpSetColorXY     OpCode = 54 // CIE 1931 xy color coordinates
)

// ─── Target Types ────────────────────────────────────────────
// The target field in a command packet encodes both an ID and a type.
// Formula: target = (id << 8) | targetType

type TargetType uint8

const (
	TargetUnit  TargetType = 0x01 // Individual light fixture
	TargetGroup TargetType = 0x02 // Group of fixtures
	TargetScene TargetType = 0x04 // Preset scene
)

// ─── Control Types ───────────────────────────────────────────
// Each light unit has a set of controls (dimmer, color, etc.)
// with varying bit resolutions.

type ControlType uint8

const (
	ControlDimmer      ControlType = 0
	ControlWhite       ControlType = 1
	ControlRGB         ControlType = 2
	ControlOnOff       ControlType = 3
	ControlTemperature ControlType = 4
	ControlVertical    ControlType = 5
	ControlColorSource ControlType = 6
	ControlXY          ControlType = 7
	ControlSlider      ControlType = 8
)

// ─── Auth Packet Types ───────────────────────────────────────
// During the authentication phase after key exchange.

const (
	AuthRequestType  = 0x04 // Client sends auth digest
	AuthSuccessType  = 0x05 // Device confirms authentication
	AuthRejectedType = 0x06 // Device rejects credentials
)

// ─── Connection States ───────────────────────────────────────

type ConnectionState int

const (
	StateDisconnected  ConnectionState = iota // No BLE connection
	StateConnected                            // BLE connected, not yet secure
	StateKeyExchanged                         // ECDH complete, transport key derived
	StateAuthenticated                        // Fully authenticated, ready for commands
)

// ─── Encryption Constants ────────────────────────────────────

const (
	AESKeySize   = 16 // 128-bit AES key
	NonceSize    = 16 // Full nonce/counter block size
	CMACSize     = 16 // CMAC tag size
	CMACConstRb  = 0x87 // CMAC subkey derivation constant
	ECDHKeySize  = 32 // 32 bytes per coordinate (X, Y)
	SharedKeyLen = 16 // XOR-folded from 32-byte SHA256
)

// ─── Packet Structure ────────────────────────────────────────
// Command packets have this layout:
//
//   [Flags: 2B] [OpCode: 1B] [Origin: 2B] [Target: 2B] [Reserved: 2B] [Payload: 0-63B]
//
// Flags encoding:
//   Bits 15-11: Lifetime (4 bits, shifted left 11)
//   Bits 10-0:  Payload length (6 bits)
//   Formula: flags = ((lifetime & 0x0F) << 11) | (payloadLen & 0x3F)
//
// Header fields are big-endian.
// Counter values in encryption nonces are little-endian.

const (
	PacketHeaderSize  = 9  // Flags(2) + OpCode(1) + Origin(2) + Target(2) + Reserved(2)
	MaxPayloadSize    = 63
	DefaultLifetime   = 5  // matches casambi-bt default
)
