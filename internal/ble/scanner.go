package ble

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/migueltejeda/casambi-go/internal/crypto"
	"github.com/migueltejeda/casambi-go/internal/protocol"
	"tinygo.org/x/bluetooth"
)

// CasambiDevice represents a discovered Casambi light network.
type CasambiDevice struct {
	Name    string
	Address bluetooth.Address
	RSSI    int16
}

// Connection holds the state of an active BLE link to a Casambi device.
type Connection struct {
	Device         CasambiDevice
	Characteristic bluetooth.DeviceCharacteristic
	DeviceNonce    [16]byte
	TransportKey   [16]byte
	MTU            uint8
	UnitID         uint16
	Flags          uint16
	notifyCh       chan []byte // shared notification channel
	commandCounter uint32     // outgoing command counter, starts at 2 (1 is used by auth)
	cmdMu          sync.Mutex // protects commandCounter and serializes BLE writes
}

// This is the service UUID that all Casambi devices advertise.
var casambiServiceUUID = bluetooth.NewUUID([16]byte{
	0x00, 0x00, 0xfe, 0x4d,
	0x00, 0x00,
	0x10, 0x00,
	0x80, 0x00,
	0x00, 0x80, 0x5f, 0x9b, 0x34, 0xfb,
})

// ScanAll scans for Casambi devices for the given duration and returns all found.
func ScanAll(timeout time.Duration) []CasambiDevice {
	adapter := bluetooth.DefaultAdapter

	if err := adapter.Enable(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to enable Bluetooth adapter: %v\n", err)
		fmt.Fprintln(os.Stderr, "\nTroubleshooting:")
		fmt.Fprintln(os.Stderr, "  - Make sure Bluetooth is on: sudo systemctl start bluetooth")
		fmt.Fprintln(os.Stderr, "  - Try running with sudo: sudo go run main.go")
		fmt.Fprintln(os.Stderr, "  - Check adapter exists: hciconfig")
		os.Exit(1)
	}
	fmt.Println("Bluetooth adapter enabled")

	seen := make(map[string]bool)
	var devices []CasambiDevice

	go func() {
		time.Sleep(timeout)
		adapter.StopScan()
	}()

	err := adapter.Scan(func(adapter *bluetooth.Adapter, result bluetooth.ScanResult) {
		addr := result.Address.String()
		if seen[addr] {
			return
		}
		seen[addr] = true

		if !result.HasServiceUUID(casambiServiceUUID) {
			return
		}

		name := result.LocalName()
		if name == "" {
			name = "(unnamed)"
		}

		device := CasambiDevice{
			Name:    name,
			Address: result.Address,
			RSSI:    result.RSSI,
		}
		devices = append(devices, device)

		signalQuality := "weak"
		if result.RSSI > -60 {
			signalQuality = "strong"
		} else if result.RSSI > -80 {
			signalQuality = "medium"
		}

		fmt.Printf("  [%d] %s\n", len(devices), name)
		fmt.Printf("      Address: %s\n", addr)
		fmt.Printf("      RSSI:    %d dBm (%s)\n", result.RSSI, signalQuality)
		fmt.Println()
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "Scan failed: %v\n", err)
	}

	return devices
}

// Connect establishes a BLE connection to a Casambi device, discovers the
// service and characteristic, enables notifications, and reads the initial
// device info (nonce, MTU, etc.) from the first notification.
//
// This matches the casambi-bt flow exactly:
//  1. BLE connect
//  2. Discover service + characteristic
//  3. Enable notifications
//  4. First notification (type 0x01) = device info with nonce
func Connect(device CasambiDevice) (*Connection, error) {
	adapter := bluetooth.DefaultAdapter

	bleDevice, err := adapter.Connect(device.Address, bluetooth.ConnectionParams{})
	if err != nil {
		return nil, err
	}

	services, err := bleDevice.DiscoverServices([]bluetooth.UUID{casambiServiceUUID})
	if err != nil {
		return nil, err
	}
	fmt.Println("Found service:", services[0].UUID().String())

	charUUID := bluetooth.NewUUID([16]byte{
		0xc9, 0xff, 0xde, 0x48,
		0xca, 0x5a,
		0x00, 0x01,
		0xab, 0x83,
		0x8f, 0x51, 0x9b, 0x48, 0x2f, 0x77,
	})

	chars, err := services[0].DiscoverCharacteristics([]bluetooth.UUID{charUUID})
	if err != nil {
		return nil, err
	}
	fmt.Println("Found characteristic:", chars[0].UUID().String())

	connection := &Connection{
		Device:         device,
		Characteristic: chars[0],
		notifyCh:       make(chan []byte, 10),
	}

	// Read device info via Read() — the device sends nonce/MTU/flags this way.
	// (The device does NOT send a type 0x01 notification; that comes via Read.)
	buf := make([]byte, 128)
	n, err := chars[0].Read(buf)
	if err != nil {
		return nil, fmt.Errorf("read device info: %w", err)
	}
	fmt.Printf("Device info (%d bytes): %x\n", n, buf[:n])
	parseDeviceInfo(connection, buf[:n])

	// Enable notifications — used for key exchange (0x02, 0x03) and auth (0x05/0x06)
	err = chars[0].EnableNotifications(func(buf []byte) {
		data := make([]byte, len(buf))
		copy(data, buf)
		select {
		case connection.notifyCh <- data:
		default:
			fmt.Printf("  Warning: notification dropped (%d bytes)\n", len(data))
		}
	})
	if err != nil {
		return nil, fmt.Errorf("enable notifications: %w", err)
	}

	return connection, nil
}

// parseDeviceInfo extracts the nonce and other fields from the device info response.
//
// Layout from casambi-bt (struct.unpack_from(">BHH16s", resp, 2)):
//
//	[0]     Type byte (0x01)
//	[1]     Protocol info
//	[2]     MTU
//	[3-4]   Unit ID (big-endian uint16)
//	[5-6]   Flags (big-endian uint16)
//	[7-22]  Device Nonce (16 bytes)
//	[23+]   Additional info
func parseDeviceInfo(c *Connection, data []byte) {
	if len(data) < 23 {
		fmt.Printf("  Warning: device info too short (%d bytes), expected >= 23\n", len(data))
		fmt.Printf("  Raw: %x\n", data)
		return
	}

	c.MTU = data[2]
	c.UnitID = binary.BigEndian.Uint16(data[3:5])
	c.Flags = binary.BigEndian.Uint16(data[5:7])
	copy(c.DeviceNonce[:], data[7:23])

	fmt.Printf("  MTU: %d\n", c.MTU)
	fmt.Printf("  Unit ID: %d\n", c.UnitID)
	fmt.Printf("  Flags: 0x%04x\n", c.Flags)
	fmt.Printf("  Device Nonce: %x\n", c.DeviceNonce)
}

// PerformKeyExchange performs the ECDH key exchange with the device.
//
// Matches casambi-bt flow:
//  1. Wait for device's public key (notification type 0x02)
//  2. Compute shared secret
//  3. Send our public key
//  4. Wait for acknowledgment (notification type 0x03)
func (c *Connection) PerformKeyExchange(ke *crypto.KeyExchange) error {
	// Step 1: Wait for the device's public key via notification (type 0x02)
	// The device sends this proactively after the connection info.
	fmt.Println("Waiting for device public key...")

	var devicePubKey []byte
	timeout := time.After(10 * time.Second)

	for {
		select {
		case response := <-c.notifyCh:
			fmt.Printf("Notification (%d bytes, type 0x%02x): %x\n", len(response), response[0], response)

			if len(response) >= 65 && response[0] == 0x02 {
				// Type 0x02 = key exchange, skip header byte
				devicePubKey = response[1:65]
			} else if response[0] == 0x01 {
				// Type 0x01 = device info (might arrive again), skip it
				fmt.Println("  Skipping duplicate device info notification")
				continue
			} else {
				fmt.Printf("  Unexpected notification type 0x%02x, skipping\n", response[0])
				continue
			}

		case <-timeout:
			return fmt.Errorf("timeout waiting for device public key (10s)")
		}
		break
	}

	// Step 2: Derive the transport key from device's public key
	err := ke.DeriveTransportKey(devicePubKey)
	if err != nil {
		return fmt.Errorf("derive transport key: %w", err)
	}
	fmt.Println("Transport key derived successfully")

	// Step 3: Send OUR public key to the device (after computing shared secret)
	// Format: [0x02 type] [X:32 LE] [Y:32 LE] [0x01 suffix] = 66 bytes
	rawPubKey := ke.PublicKeyBytes() // 64 bytes: LE X + LE Y
	pubKeyPacket := make([]byte, 66)
	pubKeyPacket[0] = 0x02 // key exchange type
	copy(pubKeyPacket[1:65], rawPubKey)
	pubKeyPacket[65] = 0x01 // trailing byte
	fmt.Printf("Sending our public key (%d bytes)\n", len(pubKeyPacket))

	_, err = c.Characteristic.Write(pubKeyPacket)
	if err != nil {
		return fmt.Errorf("write public key: %w", err)
	}

	// Step 4: Wait for key exchange acknowledgment (type 0x03)
	fmt.Println("Waiting for key exchange acknowledgment...")
	select {
	case ack := <-c.notifyCh:
		fmt.Printf("Key exchange ack (%d bytes, type 0x%02x): %x\n", len(ack), ack[0], ack)
		if ack[0] == 0x03 {
			fmt.Println("Key exchange complete!")
		} else {
			fmt.Printf("  Warning: expected type 0x03, got 0x%02x\n", ack[0])
		}
	case <-time.After(10 * time.Second):
		fmt.Println("Warning: no key exchange ack received (continuing anyway)")
	}

	return nil
}

// AuthenticateWithKey sends the BLE auth packet using the AES key from the cloud API,
// the device nonce from the connection, and the transport key from the ECDH exchange.
//
// Phase 5b flow:
//  1. Build auth digest: SHA-256(aesKey + deviceNonce + transportKey)
//  2. Build auth packet: [Counter: 4B LE] [Type: 0x04] [KeyID: 1B] [Digest: 32B]
//  3. Encrypt payload (not header) with Casambi custom CTR
//  4. Append CMAC tag
//  5. Write to BLE characteristic
//  6. Wait for notification: 0x05 = success, 0x06 = rejected
func (c *Connection) AuthenticateWithKey(aesKey [16]byte, keyID int, transportKey [16]byte) error {
	// Step 1: Build auth digest = SHA-256(aesKey + deviceNonce + transportKey)
	h := sha256.New()
	h.Write(aesKey[:])
	h.Write(c.DeviceNonce[:])
	h.Write(transportKey[:])
	digest := h.Sum(nil) // 32 bytes

	fmt.Printf("Auth digest: %x\n", digest)

	// Step 2: Build auth packet (plaintext)
	// [Counter: 4B LE] [Type: 0x04] [KeyID: 1B] [Digest: 32B] = 38 bytes
	packet := make([]byte, 38)
	binary.LittleEndian.PutUint32(packet[0:4], 1) // counter = 1 (per casambi-bt)
	packet[4] = 0x04                               // auth request type
	packet[5] = byte(keyID)                        // key ID from cloud API
	copy(packet[6:38], digest)                     // 32-byte SHA-256 digest

	fmt.Printf("Auth packet (plaintext): %x\n", packet)

	// Step 3: Encrypt ONLY the payload (bytes 4+), header stays plaintext
	// Nonce: [DeviceNonce 0:4] [id=1 as LE uint32] [DeviceNonce 8:16]
	var nonce [16]byte
	copy(nonce[0:4], c.DeviceNonce[0:4])
	binary.LittleEndian.PutUint32(nonce[4:8], 1) // id=1 for auth
	copy(nonce[8:16], c.DeviceNonce[8:16])

	header := make([]byte, 4)
	copy(header, packet[:4])
	encryptedPayload := crypto.EncryptWithNonce(transportKey, nonce, packet[4:])
	partialPacket := append(header, encryptedPayload...)

	fmt.Printf("Auth packet (%d bytes): %x\n", len(partialPacket), partialPacket)

	// Step 4: Compute CMAC over header + ciphertext, then append
	cmacTag := crypto.ComputeCMAC(transportKey, partialPacket)
	fullPacket := append(partialPacket, cmacTag[:]...)
	fmt.Printf("Auth + CMAC (%d bytes): %x\n", len(fullPacket), fullPacket)

	// Step 5: Drain any stale notifications before sending
	drainCount := 0
	for {
		select {
		case stale := <-c.notifyCh:
			drainCount++
			fmt.Printf("  Drained stale notification (%d bytes, type 0x%02x)\n", len(stale), stale[0])
		default:
			goto drained
		}
	}
drained:
	if drainCount > 0 {
		fmt.Printf("  Drained %d stale notification(s)\n", drainCount)
	}

	// Step 6: Write the auth packet (must use Write with response, not WriteWithoutResponse,
	// because the 54-byte packet exceeds the default ATT MTU for WriteWithoutResponse)
	_, err := c.Characteristic.Write(fullPacket)
	if err != nil {
		return fmt.Errorf("write auth packet: %w", err)
	}

	// Step 7: Wait for auth response via notification
	fmt.Println("Waiting for auth response...")
	select {
	case response := <-c.notifyCh:
		return c.parseAuthResponse(response, transportKey)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for auth response (10s)")
	}
}

// parseAuthResponse decrypts the auth response and checks for success/failure.
// Response format: [4-byte header] [encrypted payload] [16-byte CMAC]
func (c *Connection) parseAuthResponse(response []byte, transportKey [16]byte) error {
	fmt.Printf("Auth response raw (%d bytes): %x\n", len(response), response)

	if len(response) < 21 { // 4 header + 1 type min + 16 CMAC
		return fmt.Errorf("auth response too short: %d bytes", len(response))
	}

	// Split: header (4) | encrypted payload | CMAC (16)
	header := response[:4]
	cmacTag := response[len(response)-16:]
	ciphertext := response[4 : len(response)-16]

	// Verify CMAC over header + ciphertext
	var expectedCMAC [16]byte
	copy(expectedCMAC[:], cmacTag)
	if !crypto.VerifyCMAC(transportKey, response[:len(response)-16], expectedCMAC) {
		fmt.Println("  Warning: CMAC verification failed on auth response")
	}

	// Build decryption nonce for INCOMING packets: [header bytes] [DeviceNonce 4:16]
	// This differs from outgoing which is: [DeviceNonce 0:4] [counter] [DeviceNonce 8:16]
	// Matches casambi-bt: data[:4] + self._nonce[4:]
	var decryptNonce [16]byte
	copy(decryptNonce[0:4], header[0:4])
	copy(decryptNonce[4:16], c.DeviceNonce[4:16])

	plaintext := crypto.EncryptWithNonce(transportKey, decryptNonce, ciphertext)
	fmt.Printf("Auth response decrypted: %x (header: %x)\n", plaintext, header)

	if len(plaintext) == 0 {
		return fmt.Errorf("empty decrypted auth response")
	}

	switch plaintext[0] {
	case 0x05:
		fmt.Println("Authentication SUCCESS!")
		c.TransportKey = transportKey
		c.commandCounter = 2 // auth used counter 1, commands start at 2
		return nil
	case 0x06:
		return fmt.Errorf("authentication REJECTED by device (0x06)")
	default:
		return fmt.Errorf("unexpected auth response type 0x%02x (decrypted: %x)", plaintext[0], plaintext)
	}
}

// SendCommand encrypts a command packet and sends it over BLE.
//
// Wire format: [Counter: 4B LE] [Encrypted command] [CMAC: 16B]
// Same encrypt-then-MAC pattern as auth, using outgoing counter (starts at 2).
func (c *Connection) SendCommand(cmd *protocol.CommandPacket) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	payload := cmd.Encode()

	// Build full packet: [counter: 4B LE] [0x07 type] [command data]
	// 0x07 = data/command type (like 0x04 = auth). Without this the device ignores the packet.
	packet := make([]byte, 4+1+len(payload))
	binary.LittleEndian.PutUint32(packet[0:4], c.commandCounter)
	packet[4] = 0x07 // command type
	copy(packet[5:], payload)

	// Encrypt bytes 4+ using outgoing nonce: [DeviceNonce 0:4] [counter LE] [DeviceNonce 8:16]
	var nonce [16]byte
	copy(nonce[0:4], c.DeviceNonce[0:4])
	binary.LittleEndian.PutUint32(nonce[4:8], c.commandCounter)
	copy(nonce[8:16], c.DeviceNonce[8:16])

	header := make([]byte, 4)
	copy(header, packet[:4])
	encryptedPayload := crypto.EncryptWithNonce(c.TransportKey, nonce, packet[4:])
	partialPacket := append(header, encryptedPayload...)

	// Compute CMAC over header + ciphertext, then append
	cmacTag := crypto.ComputeCMAC(c.TransportKey, partialPacket)
	fullPacket := append(partialPacket, cmacTag[:]...)

	fmt.Printf("Command (counter=%d, %d bytes): %x\n", c.commandCounter, len(fullPacket), fullPacket)

	_, err := c.Characteristic.Write(fullPacket)
	if err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	c.commandCounter++
	return nil
}
