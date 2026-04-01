package captive

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestECDHKeypair_RoundTrip(t *testing.T) {
	// Server generates keypair (like AP startup)
	serverKP, err := NewECDHKeypair()
	if err != nil {
		t.Fatalf("NewECDHKeypair: %v", err)
	}

	// Browser generates ephemeral keypair (like TweetNaCl.js nacl.box.keyPair())
	clientPub, clientPriv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("client keygen: %v", err)
	}

	// Browser encrypts passphrase
	passphrase := []byte("my-wifi-password-123!")
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	ciphertext := box.Seal(nil, passphrase, &nonce, &serverKP.PublicKey, clientPriv)

	// Server decrypts
	plaintext, err := serverKP.Decrypt(*clientPub, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if string(plaintext) != string(passphrase) {
		t.Fatalf("round-trip mismatch: got %q, want %q", plaintext, passphrase)
	}
}

func TestECDHKeypair_WrongKey(t *testing.T) {
	serverKP, _ := NewECDHKeypair()
	wrongKP, _ := NewECDHKeypair()

	clientPub, clientPriv, _ := box.GenerateKey(rand.Reader)
	var nonce [24]byte
	rand.Read(nonce[:])
	ciphertext := box.Seal(nil, []byte("secret"), &nonce, &wrongKP.PublicKey, clientPriv)

	_, err := serverKP.Decrypt(*clientPub, nonce, ciphertext)
	if err == nil {
		t.Fatal("expected decryption to fail with wrong key")
	}
}

func TestECDHKeypair_Zero(t *testing.T) {
	kp, _ := NewECDHKeypair()

	allZero := true
	for _, b := range kp.PrivateKey {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("private key should not be all zeros after generation")
	}

	kp.Zero()

	for i, b := range kp.PrivateKey {
		if b != 0 {
			t.Fatalf("private key byte %d is %d after Zero(), want 0", i, b)
		}
	}
}

func TestDecryptConnectRequest_RoundTrip(t *testing.T) {
	serverKP, _ := NewECDHKeypair()
	clientPub, clientPriv, _ := box.GenerateKey(rand.Reader)

	passphrase := []byte("test-pass")
	var nonce [24]byte
	rand.Read(nonce[:])
	ciphertext := box.Seal(nil, passphrase, &nonce, &serverKP.PublicKey, clientPriv)

	plaintext, err := decryptConnectRequest(serverKP,
		base64.StdEncoding.EncodeToString(clientPub[:]),
		base64.StdEncoding.EncodeToString(nonce[:]),
		base64.StdEncoding.EncodeToString(ciphertext),
	)
	if err != nil {
		t.Fatalf("decryptConnectRequest: %v", err)
	}
	if string(plaintext) != "test-pass" {
		t.Fatalf("got %q, want %q", plaintext, "test-pass")
	}
}
