package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// ─── Phase 3: ECDH Key Exchange ──────────────────────────────

// Simulate a full key exchange between two parties (client + device).
// Both should derive the same transport key.
func TestKeyExchange_BothSidesDerivesSameKey(t *testing.T) {
	// Create two key pairs (simulating client and device)
	client, err := NewKeyExchange()
	if err != nil {
		t.Fatal("client NewKeyExchange:", err)
	}

	device, err := NewKeyExchange()
	if err != nil {
		t.Fatal("device NewKeyExchange:", err)
	}

	// Exchange public keys (in little-endian wire format)
	clientPub := client.PublicKeyBytes()
	devicePub := device.PublicKeyBytes()

	if len(clientPub) != 64 {
		t.Fatalf("client public key should be 64 bytes, got %d", len(clientPub))
	}
	if len(devicePub) != 64 {
		t.Fatalf("device public key should be 64 bytes, got %d", len(devicePub))
	}

	// Each side derives the transport key from the other's public key
	err = client.DeriveTransportKey(devicePub)
	if err != nil {
		t.Fatal("client DeriveTransportKey:", err)
	}

	err = device.DeriveTransportKey(clientPub)
	if err != nil {
		t.Fatal("device DeriveTransportKey:", err)
	}

	// Both sides must arrive at the same transport key
	if client.TransportKey != device.TransportKey {
		t.Fatalf("transport keys don't match!\n  client: %x\n  device: %x",
			client.TransportKey, device.TransportKey)
	}

	// Sanity: the key shouldn't be all zeros
	var zero [16]byte
	if client.TransportKey == zero {
		t.Fatal("transport key is all zeros")
	}
}

func TestPublicKeyBytes_RoundTrip(t *testing.T) {
	ke, err := NewKeyExchange()
	if err != nil {
		t.Fatal(err)
	}

	pub := ke.PublicKeyBytes()

	// Should be 64 bytes (32-byte X + 32-byte Y, little-endian)
	if len(pub) != 64 {
		t.Fatalf("expected 64 bytes, got %d", len(pub))
	}

	// Calling it again should return the same bytes
	pub2 := ke.PublicKeyBytes()
	if !bytes.Equal(pub, pub2) {
		t.Fatal("PublicKeyBytes not deterministic")
	}
}

func TestDeriveTransportKey_RejectsBadInput(t *testing.T) {
	ke, err := NewKeyExchange()
	if err != nil {
		t.Fatal(err)
	}

	// 63 bytes is too short (need 64)
	err = ke.DeriveTransportKey(make([]byte, 63))
	if err == nil {
		t.Fatal("expected error for short input")
	}

	// 64 bytes of zeros is not a valid point on P-256
	err = ke.DeriveTransportKey(make([]byte, 64))
	if err == nil {
		t.Fatal("expected error for zero-point input")
	}
}

// ─── Phase 4a: AES-CTR Encryption ───────────────────────────

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	// Use a fixed key and nonce so the test is deterministic
	var transportKey [16]byte
	var deviceNonce [16]byte
	copy(transportKey[:], []byte("0123456789abcdef"))
	copy(deviceNonce[:], []byte("nonce-for-test!!"))

	// Encrypt side (outgoing counter starts at 2)
	encryptor := NewEncryptor(transportKey, deviceNonce)

	plaintext := []byte("hello casambi lights!")
	ciphertext := encryptor.Encrypt(plaintext)

	// Ciphertext should differ from plaintext
	if bytes.Equal(plaintext, ciphertext) {
		t.Fatal("ciphertext equals plaintext — encryption did nothing")
	}

	// Decrypt side needs to use the OUTGOING nonce format with counter=2
	// since we're decrypting what was encrypted with outgoing counter 2.
	// In real usage, the device would decrypt with its incoming counter.
	// For this test, create a fresh encryptor and manually verify.
	decryptor := NewEncryptor(transportKey, deviceNonce)
	// The encryptor's outgoing counter is 2, so to decrypt we need
	// a fresh encryptor whose outgoing counter is also 2.
	decrypted := decryptor.Encrypt(ciphertext) // CTR is symmetric: encrypt == decrypt with same nonce

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatalf("round-trip failed:\n  plaintext:  %x\n  decrypted:  %x", plaintext, decrypted)
	}
}

func TestEncrypt_CounterIncrements(t *testing.T) {
	var transportKey [16]byte
	var deviceNonce [16]byte
	copy(transportKey[:], []byte("0123456789abcdef"))
	copy(deviceNonce[:], []byte("nonce-for-test!!"))

	enc := NewEncryptor(transportKey, deviceNonce)

	msg := []byte("same message")

	// Encrypt the same plaintext twice — different counters means different ciphertext
	ct1 := enc.Encrypt(msg)
	ct2 := enc.Encrypt(msg)

	if bytes.Equal(ct1, ct2) {
		t.Fatal("same plaintext produced same ciphertext — counter not incrementing")
	}
}

// ─── Phase 4b: CMAC (RFC 4493 Test Vectors) ─────────────────
//
// Test vectors from RFC 4493, Section 4.
// Key: 2b7e1516 28aed2a6 abf71588 09cf4f3c

func rfc4493Key() [16]byte {
	b, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	var key [16]byte
	copy(key[:], b)
	return key
}

func TestCMAC_EmptyMessage(t *testing.T) {
	// RFC 4493 Example 1: len = 0
	key := rfc4493Key()
	expected, _ := hex.DecodeString("bb1d6929e95937287fa37d129b756746")

	tag := ComputeCMAC(key, []byte{})

	if !bytes.Equal(tag[:], expected) {
		t.Fatalf("CMAC empty message:\n  got:      %x\n  expected: %x", tag, expected)
	}
}

func TestCMAC_16Bytes(t *testing.T) {
	// RFC 4493 Example 2: len = 16
	key := rfc4493Key()
	msg, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	expected, _ := hex.DecodeString("070a16b46b4d4144f79bdd9dd04a287c")

	tag := ComputeCMAC(key, msg)

	if !bytes.Equal(tag[:], expected) {
		t.Fatalf("CMAC 16-byte message:\n  got:      %x\n  expected: %x", tag, expected)
	}
}

func TestCMAC_40Bytes(t *testing.T) {
	// RFC 4493 Example 3: len = 40
	key := rfc4493Key()
	msg, _ := hex.DecodeString(
		"6bc1bee22e409f96e93d7e117393172a" +
			"ae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411",
	)
	expected, _ := hex.DecodeString("dfa66747de9ae63030ca32611497c827")

	tag := ComputeCMAC(key, msg)

	if !bytes.Equal(tag[:], expected) {
		t.Fatalf("CMAC 40-byte message:\n  got:      %x\n  expected: %x", tag, expected)
	}
}

func TestCMAC_64Bytes(t *testing.T) {
	// RFC 4493 Example 4: len = 64
	key := rfc4493Key()
	msg, _ := hex.DecodeString(
		"6bc1bee22e409f96e93d7e117393172a" +
			"ae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411e5fbc1191a0a52ef" +
			"f69f2445df4f9b17ad2b417be66c3710",
	)
	expected, _ := hex.DecodeString("51f0bebf7e3b9d92fc49741779363cfe")

	tag := ComputeCMAC(key, msg)

	if !bytes.Equal(tag[:], expected) {
		t.Fatalf("CMAC 64-byte message:\n  got:      %x\n  expected: %x", tag, expected)
	}
}

func TestVerifyCMAC(t *testing.T) {
	key := rfc4493Key()
	msg, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	expected, _ := hex.DecodeString("070a16b46b4d4144f79bdd9dd04a287c")

	var tag [16]byte
	copy(tag[:], expected)

	if !VerifyCMAC(key, msg, tag) {
		t.Fatal("VerifyCMAC rejected a valid tag")
	}

	// Flip a bit — should fail
	tag[0] ^= 0x01
	if VerifyCMAC(key, msg, tag) {
		t.Fatal("VerifyCMAC accepted a corrupted tag")
	}
}
