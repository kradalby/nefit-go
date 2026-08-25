package crypto

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncryptorKeyGeneration(t *testing.T) {
	// Test that key generation produces a 32-byte key (for AES-256)
	enc, err := NewEncryptor("123456789", "abcdefghij", "testpass")
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	if len(enc.key) != 32 {
		t.Errorf("Expected key length 32, got %d", len(enc.key))
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	tests := []struct {
		name         string
		serialNumber string
		accessKey    string
		password     string
		plaintext    string
	}{
		{
			name:         "simple text",
			serialNumber: "123456789",
			accessKey:    "abcdefghij",
			password:     "secret",
			plaintext:    "Hello, World!",
		},
		{
			name:         "json data",
			serialNumber: "987654321",
			accessKey:    "keytest123",
			password:     "pass123",
			plaintext:    `{"temperature":21.5,"status":"on"}`,
		},
		{
			name:         "empty string",
			serialNumber: "111111111",
			accessKey:    "key",
			password:     "pwd",
			plaintext:    "",
		},
		{
			name:         "long text",
			serialNumber: "222222222",
			accessKey:    "longkey",
			password:     "longpass",
			plaintext:    "This is a much longer text that spans multiple AES blocks to test padding",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := NewEncryptor(tt.serialNumber, tt.accessKey, tt.password)
			if err != nil {
				t.Fatalf("Failed to create encryptor: %v", err)
			}

			// Encrypt
			encrypted, err := enc.Encrypt(tt.plaintext)
			if err != nil {
				t.Fatalf("Encryption failed: %v", err)
			}

			// Decrypt
			decrypted, err := enc.DecryptAndStrip(encrypted)
			if err != nil {
				t.Fatalf("Decryption failed: %v", err)
			}

			// Verify round trip
			if decrypted != tt.plaintext {
				t.Errorf("Round trip failed.\nOriginal:  %q\nDecrypted: %q", tt.plaintext, decrypted)
			}
		})
	}
}

func TestEncryptionDeterministic(t *testing.T) {
	// Same input should produce same output
	enc, err := NewEncryptor("123456789", "abcdefghij", "secret")
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	plaintext := "Test message"

	encrypted1, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("First encryption failed: %v", err)
	}

	encrypted2, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Second encryption failed: %v", err)
	}

	if encrypted1 != encrypted2 {
		t.Errorf("Encryption is not deterministic.\nFirst:  %s\nSecond: %s", encrypted1, encrypted2)
	}
}

func TestDecryptWithPadding(t *testing.T) {
	// Test that decryption properly handles null byte padding removal
	enc, err := NewEncryptor("123456789", "abcdefghij", "secret")
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	plaintext := "Short"
	encrypted, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encryption failed: %v", err)
	}

	// Decrypt without stripping
	decryptedRaw, err := enc.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decryption failed: %v", err)
	}

	// Should contain null bytes
	if len(decryptedRaw) == len(plaintext) {
		t.Error("Expected raw decryption to contain padding")
	}

	// Decrypt with stripping
	decryptedStripped, err := enc.DecryptAndStrip(encrypted)
	if err != nil {
		t.Fatalf("DecryptAndStrip failed: %v", err)
	}

	// Should match original
	if decryptedStripped != plaintext {
		t.Errorf("Stripped decryption failed.\nExpected: %q\nGot:      %q", plaintext, decryptedStripped)
	}
}

func TestDifferentCredentialsProduceDifferentKeys(t *testing.T) {
	enc1, _ := NewEncryptor("123456789", "key1", "pass1")
	enc2, _ := NewEncryptor("123456789", "key2", "pass1")
	enc3, _ := NewEncryptor("123456789", "key1", "pass2")

	// Different accessKey
	if string(enc1.key) == string(enc2.key) {
		t.Error("Different access keys produced same encryption key")
	}

	// Different password
	if string(enc1.key) == string(enc3.key) {
		t.Error("Different passwords produced same encryption key")
	}
}

func BenchmarkEncrypt(b *testing.B) {
	enc, _ := NewEncryptor("123456789", "abcdefghij", "secret")
	plaintext := `{"temperature":21.5,"status":"on","mode":"manual"}`

	for b.Loop() {
		_, err := enc.Encrypt(plaintext)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecrypt(b *testing.B) {
	enc, _ := NewEncryptor("123456789", "abcdefghij", "secret")
	plaintext := `{"temperature":21.5,"status":"on","mode":"manual"}`
	encrypted, _ := enc.Encrypt(plaintext)

	for b.Loop() {
		_, err := enc.Decrypt(encrypted)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestDecryptNonBlockMultipleLength guards the zero-padding in Decrypt.
// Ciphertext length arrives off the wire, so a length that is not a whole
// number of AES blocks must not panic the caller. Lengths 4, 20 and 24 all
// used to panic with a slice-bounds error.
func TestDecryptNonBlockMultipleLength(t *testing.T) {
	enc, err := NewEncryptor("123456789", "abcdefghij", "secret")
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	for _, n := range []int{1, 4, 8, 15, 16, 20, 24, 31, 33} {
		data := base64.StdEncoding.EncodeToString(make([]byte, n))
		if _, err := enc.Decrypt(data); err != nil {
			t.Errorf("Decrypt(%d bytes) returned error: %v", n, err)
		}
	}
}

// TestDecryptAndStripRemovesPadding documents why the push-notification path
// must use DecryptAndStrip: AES-ECB pads plaintext to a block boundary with
// NUL bytes, and JSON parsing fails on the trailing NULs.
func TestDecryptAndStripRemovesPadding(t *testing.T) {
	enc, err := NewEncryptor("123456789", "abcdefghij", "secret")
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	// 14 bytes, so AES-ECB pads it out to 16.
	plaintext := `{"value":21.5}`

	encrypted, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	padded, err := enc.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt() error: %v", err)
	}
	if padded == plaintext {
		t.Skip("plaintext happened to land on a block boundary; nothing to strip")
	}
	if !strings.HasPrefix(padded, plaintext) {
		t.Fatalf("Decrypt() = %q, want it to start with %q", padded, plaintext)
	}

	stripped, err := enc.DecryptAndStrip(encrypted)
	if err != nil {
		t.Fatalf("DecryptAndStrip() error: %v", err)
	}
	if stripped != plaintext {
		t.Errorf("DecryptAndStrip() = %q, want %q", stripped, plaintext)
	}

	// The padded form is what broke JSON parsing before the fix.
	var v any
	if err := json.Unmarshal([]byte(padded), &v); err == nil {
		t.Error("expected NUL-padded plaintext to fail JSON parsing")
	}
	if err := json.Unmarshal([]byte(stripped), &v); err != nil {
		t.Errorf("stripped plaintext should parse as JSON, got: %v", err)
	}
}
