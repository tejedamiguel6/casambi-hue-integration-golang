package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// ╔══════════════════════════════════════════════════════════════╗
// ║  Crypto Package — Phase 3 & 4                                ║
// ║                                                              ║
// ║  This is where the real protocol work happens.               ║
// ║  You'll implement three things here:                         ║
// ║                                                              ║
// ║  1. ECDH Key Exchange (Phase 3)                              ║
// ║     - Generate a P-256 key pair                              ║
// ║     - Exchange public keys with the device                   ║
// ║     - Derive the 16-byte transport key                       ║
// ║                                                              ║
// ║  2. AES-CTR Encryption (Phase 4a)                            ║
// ║     - Encrypt outgoing command packets                       ║
// ║     - Decrypt incoming response packets                      ║
// ║     - Manage nonce counters                                  ║
// ║                                                              ║
// ║  3. CMAC Authentication (Phase 4b)                           ║
// ║     - Compute message authentication codes                   ║
// ║     - Verify incoming message integrity                      ║
// ╚══════════════════════════════════════════════════════════════╝

// ─── Phase 3: ECDH Key Exchange ──────────────────────────────
//
// Research these Go packages before implementing:
//   - crypto/ecdh          (ECDH key agreement, added in Go 1.20)
//   - crypto/elliptic      (P-256 curve, if you need raw coordinates)
//   - crypto/sha256        (for hashing the shared secret)
//
// The flow:
//   1. Generate key pair:  privateKey, _ := ecdh.P256().GenerateKey(rand.Reader)
//   2. Get your public key bytes (X, Y as 32-byte little-endian each)
//   3. Send your public key to the device via BLE characteristic write
//   4. Read the device's public key from BLE characteristic
//   5. Compute shared secret:  secret, _ := privateKey.ECDH(devicePublicKey)
//   6. Derive transport key:
//      hash := sha256.Sum256(secret)
//      transportKey[i] = hash[i] ^ hash[i+16]   for i in 0..15
//
// IMPORTANT: The device sends/expects public key coordinates in
// LITTLE-ENDIAN byte order. Go's crypto/ecdh uses big-endian.
// You'll need to reverse the byte order of each 32-byte coordinate.

// TODO (Phase 3): Implement KeyExchange
//
// type KeyExchange struct {
//     PrivateKey    *ecdh.PrivateKey
//     TransportKey  [16]byte
//     DeviceNonce   [16]byte
// }
//
// func NewKeyExchange() (*KeyExchange, error) { ... }
// func (ke *KeyExchange) PublicKeyBytes() []byte { ... }
// func (ke *KeyExchange) DeriveTransportKey(devicePubKeyBytes []byte) error { ... }

// ─── Phase 4a: AES-CTR Encryption ───────────────────────────
//
// Research these Go packages:
//   - crypto/aes           (AES block cipher)
//   - crypto/cipher        (CTR mode: cipher.NewCTR)
//
// Nonce construction for OUTGOING packets (16 bytes total):
//   Bytes 0-3:   DeviceNonce[0:4]
//   Bytes 4-7:   OutgoingCounter (little-endian uint32)
//   Bytes 8-15:  DeviceNonce[8:16]
//
// Nonce construction for INCOMING packets (16 bytes total):
//   Bytes 0-3:   IncomingCounter (little-endian uint32)
//   Bytes 4-15:  DeviceNonce[4:16]
//
// Counters:
//   Outgoing starts at 2, increments per packet sent
//   Incoming starts at 1, increments per packet received

// TODO (Phase 4a): Implement Encryptor
//
// type Encryptor struct {
//     transportKey    [16]byte
//     deviceNonce     [16]byte
//     outgoingCounter uint32
//     incomingCounter uint32
// }
//
// func NewEncryptor(transportKey, deviceNonce [16]byte) *Encryptor { ... }
// func (e *Encryptor) Encrypt(plaintext []byte) []byte { ... }
// func (e *Encryptor) Decrypt(ciphertext []byte) []byte { ... }

// ─── Phase 4b: CMAC (RFC 4493) ──────────────────────────────
//
// CMAC is a message authentication code built on AES.
// It proves that a message hasn't been tampered with.
//
// Research:
//   - RFC 4493 (the CMAC spec — it's short and readable!)
//   - Go doesn't have CMAC in stdlib, so you'll implement it yourself.
//     This is a great learning exercise. It's ~50 lines of code.
//
// Algorithm summary:
//   1. Generate subkeys K1, K2 from AES(key, zero_block)
//   2. Process message in 16-byte blocks using CBC-MAC
//   3. Final block: XOR with K1 (complete) or pad + XOR with K2 (incomplete)
//   4. Output: 16-byte authentication tag
//
// Constant: Rb = 0x87 (used in subkey derivation)
//
// The pattern is: encrypt the command, then compute CMAC over
// the ciphertext, and append the 16-byte tag.

// TODO (Phase 4b): Implement CMAC
//
// func ComputeCMAC(key [16]byte, message []byte) [16]byte { ... }
// func VerifyCMAC(key [16]byte, message []byte, expectedTag [16]byte) bool { ... }

type KeyExchange struct {
	PrivateKey   *ecdh.PrivateKey
	TransportKey [16]byte
}

func NewKeyExchange() (*KeyExchange, error) {
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	return &KeyExchange{
		PrivateKey: privateKey,
	}, nil
}

func (ke *KeyExchange) PublicKeyBytes() []byte {
	raw := ke.PrivateKey.PublicKey().Bytes() // 65 bytes: [0x04, X(32), Y(32)]

	x := make([]byte, 32)
	y := make([]byte, 32)
	copy(x, raw[1:33]) // skip the 0x04 prefix
	copy(y, raw[33:65])

	// Reverse both to little-endian
	reverseBytes(x)
	reverseBytes(y)

	// Return 64 bytes: little-endian X + little-endian Y
	return append(x, y...)
}

func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

func (ke *KeyExchange) DeriveTransportKey(devicePubKeyBytes []byte) error {
	if len(devicePubKeyBytes) < 64 {
		return fmt.Errorf("device public key too short: got %d bytes, need 64", len(devicePubKeyBytes))
	}

	// devicePubKeyBytes is 64 bytes: little-endian X(32) + Y(32)
	// We need to convert to big-endian and add the 0x04 prefix for Go

	x := make([]byte, 32)
	y := make([]byte, 32)
	copy(x, devicePubKeyBytes[0:32])
	copy(y, devicePubKeyBytes[32:64])

	// Reverse from little-endian (device) to big-endian (Go)
	reverseBytes(x)
	reverseBytes(y)

	// Build the uncompressed point: [0x04, X, Y]
	raw := make([]byte, 65)
	raw[0] = 0x04
	copy(raw[1:33], x)
	copy(raw[33:65], y)

	// Parse it into a Go public key
	devicePubKey, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return err
	}

	// Compute the shared secret (the magic of Diffie-Hellman!)
	sharedSecret, err := ke.PrivateKey.ECDH(devicePubKey)
	if err != nil {
		return err
	}

	// Reverse the shared secret before hashing (casambi-bt does secret.reverse())
	reversed := make([]byte, len(sharedSecret))
	copy(reversed, sharedSecret)
	reverseBytes(reversed)

	// Derive the 16-byte transport key:
	// SHA-256 the reversed shared secret, then XOR the two halves together
	hash := sha256.Sum256(reversed)
	for i := 0; i < 16; i++ {
		ke.TransportKey[i] = hash[i] ^ hash[i+16]
	}

	return nil
}

// ─── Phase 4a: AES-CTR Encryption ─────────────────────────────

type Encryptor struct {
	transportKey    [16]byte
	deviceNonce     [16]byte
	outgoingCounter uint32
	incomingCounter uint32
}

func NewEncryptor(transportKey, deviceNonce [16]byte) *Encryptor {
	return &Encryptor{
		transportKey:    transportKey,
		deviceNonce:     deviceNonce,
		outgoingCounter: 2, // starts at 2
		incomingCounter: 1, // starts at 1
	}
}

func (e *Encryptor) Encrypt(plaintext []byte) []byte {
	// Build the 16-byte nonce for outgoing packets:
	// [DeviceNonce 0:4] [OutCounter LE] [DeviceNonce 8:16]
	var nonce [16]byte
	copy(nonce[0:4], e.deviceNonce[0:4])
	binary.LittleEndian.PutUint32(nonce[4:8], e.outgoingCounter)
	copy(nonce[8:16], e.deviceNonce[8:16])

	block, _ := aes.NewCipher(e.transportKey[:])
	stream := cipher.NewCTR(block, nonce[:])

	ciphertext := make([]byte, len(plaintext))
	stream.XORKeyStream(ciphertext, plaintext)

	e.outgoingCounter++
	return ciphertext
}

func (e *Encryptor) Decrypt(ciphertext []byte) []byte {
	// Build the 16-byte nonce for incoming packets:
	// [InCounter LE] [DeviceNonce 4:16]
	var nonce [16]byte
	binary.LittleEndian.PutUint32(nonce[0:4], e.incomingCounter)
	copy(nonce[4:16], e.deviceNonce[4:16])

	block, _ := aes.NewCipher(e.transportKey[:])
	stream := cipher.NewCTR(block, nonce[:])

	plaintext := make([]byte, len(ciphertext))
	stream.XORKeyStream(plaintext, ciphertext)

	e.incomingCounter++
	return plaintext
}

// EncryptWithNonce encrypts using Casambi's custom CTR mode.
//
// Casambi's CTR differs from standard AES-CTR:
//   - Standard: increments the entire 16-byte nonce as a big-endian number
//   - Casambi: replaces the LAST 4 bytes of the nonce with a little-endian
//     block counter (0, 1, 2, ...) for each 16-byte block
//
// This matches the _encryptInternal method in casambi-bt's _encryption.py.
func EncryptWithNonce(key [16]byte, nonce [16]byte, plaintext []byte) []byte {
	block, _ := aes.NewCipher(key[:])
	ciphertext := make([]byte, len(plaintext))

	var counterBlock [16]byte
	var keystream [16]byte

	for i := 0; i < len(plaintext); i += 16 {
		// Build counter block: nonce[0:12] + blockCounter as LE uint32
		copy(counterBlock[:12], nonce[:12])
		binary.LittleEndian.PutUint32(counterBlock[12:16], uint32(i/16))

		// Encrypt counter block to get keystream
		block.Encrypt(keystream[:], counterBlock[:])

		// XOR keystream with plaintext
		end := i + 16
		if end > len(plaintext) {
			end = len(plaintext)
		}
		for j := i; j < end; j++ {
			ciphertext[j] = plaintext[j] ^ keystream[j-i]
		}
	}

	return ciphertext
}

// ─── Phase 4b: CMAC (RFC 4493) ────────────────────────────────

func ComputeCMAC(key [16]byte, message []byte) [16]byte {
	block, _ := aes.NewCipher(key[:])

	// Step 1: Generate subkeys K1 and K2
	var zeroBlock [16]byte
	var L [16]byte
	block.Encrypt(L[:], zeroBlock[:])

	K1 := generateSubkey(L)
	K2 := generateSubkey(K1)

	// Step 2: Process message
	n := len(message)
	numBlocks := (n + 15) / 16
	if numBlocks == 0 {
		numBlocks = 1
	}
	lastBlockComplete := (n > 0) && (n%16 == 0)

	// Step 3: Prepare the last block
	var lastBlock [16]byte
	if lastBlockComplete {
		// XOR last 16 bytes with K1
		copy(lastBlock[:], message[(numBlocks-1)*16:])
		xorBlock(&lastBlock, K1)
	} else {
		// Pad and XOR with K2
		remaining := n - (numBlocks-1)*16
		copy(lastBlock[:remaining], message[(numBlocks-1)*16:])
		lastBlock[remaining] = 0x80 // padding: 1 bit then zeros
		xorBlock(&lastBlock, K2)
	}

	// Step 4: CBC-MAC
	var mac [16]byte
	for i := 0; i < numBlocks-1; i++ {
		var blk [16]byte
		copy(blk[:], message[i*16:(i+1)*16])
		xorBlock(&mac, blk)
		block.Encrypt(mac[:], mac[:])
	}
	xorBlock(&mac, lastBlock)
	block.Encrypt(mac[:], mac[:])

	return mac
}

func VerifyCMAC(key [16]byte, message []byte, expectedTag [16]byte) bool {
	computed := ComputeCMAC(key, message)
	return computed == expectedTag
}

func generateSubkey(input [16]byte) [16]byte {
	var output [16]byte
	// Left shift by 1
	var carry byte
	for i := 15; i >= 0; i-- {
		output[i] = (input[i] << 1) | carry
		carry = (input[i] >> 7) & 1
	}
	// If MSB of input was 1, XOR with Rb (0x87)
	if input[0]&0x80 != 0 {
		output[15] ^= 0x87
	}
	return output
}

func xorBlock(dst *[16]byte, src [16]byte) {
	for i := 0; i < 16; i++ {
		dst[i] ^= src[i]
	}
}
