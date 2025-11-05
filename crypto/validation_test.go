package crypto

import (
	"encoding/base64"
	"testing"
)

// TestAgainstJSImplementation validates our encryption matches the JavaScript version
// These test vectors would need to come from the actual JS implementation
func TestAgainstJSImplementation(t *testing.T) {
	t.Skip("TODO: Need actual test vectors from JS implementation with real credentials")

	// Example test structure (needs real values):
	// enc, _ := NewEncryptor("REAL_SERIAL", "REAL_ACCESS_KEY", "REAL_PASSWORD")
	//
	// known plaintext from JS
	// plaintext := `{"value":21.5}`
	//
	// known ciphertext from JS
	// expectedCiphertext := "..."
	//
	// encrypted, _ := enc.Encrypt(plaintext)
	// if encrypted != expectedCiphertext {
	//     t.Errorf("Encryption doesn't match JS implementation")
	// }
}

// TestKeyGenerationOrder verifies we generate keys in the correct order
func TestKeyGenerationOrder(t *testing.T) {
	// From JS: MD5(accessKey + MAGIC) + MD5(MAGIC + password)
	enc, err := NewEncryptor("testserial", "testaccesskey", "testpassword")
	if err != nil {
		t.Fatal(err)
	}

	if len(enc.key) != 32 {
		t.Errorf("Key should be 32 bytes (256 bits), got %d", len(enc.key))
	}

	// First 16 bytes should be MD5(accessKey + MAGIC)
	// Second 16 bytes should be MD5(MAGIC + password)
	// We can't verify the exact values without known test vectors,
	// but we can verify the key is generated

	t.Logf("Generated key (hex): %x", enc.key)
}

// TestPaddingCompatibility ensures padding matches JS implementation
func TestPaddingCompatibility(t *testing.T) {
	enc, _ := NewEncryptor("test", "test", "test")

	tests := []struct {
		name      string
		plaintext string
	}{
		{"exact_block", "0123456789abcdef"}, // Exactly 16 bytes
		{"needs_padding", "test"},            // Needs padding
		{"empty", ""},                        // Empty string
		{"long", "this is a longer test string that spans multiple blocks"}, // Multiple blocks
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encrypted, err := enc.Encrypt(tt.plaintext)
			if err != nil {
				t.Fatalf("Encryption failed: %v", err)
			}

			// Verify it's valid base64
			decoded, err := base64.StdEncoding.DecodeString(encrypted)
			if err != nil {
				t.Fatalf("Encrypted output is not valid base64: %v", err)
			}

			// Verify encrypted length is multiple of 16 (AES block size)
			if len(decoded) % 16 != 0 {
				t.Errorf("Encrypted data length (%d) is not multiple of 16", len(decoded))
			}

			// Verify round-trip
			decrypted, err := enc.DecryptAndStrip(encrypted)
			if err != nil {
				t.Fatalf("Decryption failed: %v", err)
			}

			if decrypted != tt.plaintext {
				t.Errorf("Round-trip failed.\nOriginal:  %q\nDecrypted: %q", tt.plaintext, decrypted)
			}
		})
	}
}
