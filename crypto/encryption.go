package crypto

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Magic key used by Bosch/Nefit protocol
const magicHex = "58f18d70f667c9c79ef7de435bf0f9b1553bbb6e61816212ab80e5b0d351fbb1"

// Encryptor handles AES-256-ECB encryption/decryption for the Nefit Easy protocol.
type Encryptor struct {
	key []byte
}

// NewEncryptor creates an encryptor initialized with a key derived from the provided credentials.
func NewEncryptor(serialNumber, accessKey, password string) (*Encryptor, error) {
	magic, err := hex.DecodeString(magicHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode magic key: %w", err)
	}

	key := generateKey(magic, accessKey, password)

	return &Encryptor{
		key: key,
	}, nil
}

// generateKey creates the encryption key by concatenating two MD5 hashes:
// MD5(accessKey + MAGIC) + MD5(MAGIC + password)
func generateKey(magic []byte, accessKey, password string) []byte {
	// First part: MD5(accessKey + MAGIC)
	h1 := md5.New()
	h1.Write([]byte(accessKey))
	h1.Write(magic)
	part1 := h1.Sum(nil)

	// Second part: MD5(MAGIC + password)
	h2 := md5.New()
	h2.Write(magic)
	h2.Write([]byte(password))
	part2 := h2.Sum(nil)

	// Concatenate both parts (16 + 16 = 32 bytes for AES-256)
	key := make([]byte, 32)
	copy(key[:16], part1)
	copy(key[16:], part2)

	return key
}

// Encrypt encrypts data using AES-256-ECB and returns a base64-encoded result.
func (e *Encryptor) Encrypt(data string) (string, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	plaintext := []byte(data)

	// Apply manual PKCS#7-style padding to 16-byte blocks
	padding := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	if padding > 0 && padding < aes.BlockSize {
		plaintext = append(plaintext, make([]byte, padding)...)
	}

	// Encrypt using ECB mode (encrypt each block independently)
	ciphertext := make([]byte, len(plaintext))
	for i := 0; i < len(plaintext); i += aes.BlockSize {
		block.Encrypt(ciphertext[i:i+aes.BlockSize], plaintext[i:i+aes.BlockSize])
	}

	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts base64-encoded data using AES-256-ECB.
func (e *Encryptor) Decrypt(data string) (string, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	// Add zero-padding if needed (from JS implementation)
	paddingLength := len(ciphertext) % 8
	if paddingLength != 0 {
		padding := make([]byte, paddingLength)
		ciphertext = append(ciphertext, padding...)
	}

	plaintext := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plaintext[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}

	return string(plaintext), nil
}

// DecryptAndStrip decrypts data and removes trailing null byte padding.
func (e *Encryptor) DecryptAndStrip(data string) (string, error) {
	decrypted, err := e.Decrypt(data)
	if err != nil {
		return "", err
	}

	for i := len(decrypted) - 1; i >= 0; i-- {
		if decrypted[i] != 0 {
			return decrypted[:i+1], nil
		}
	}

	return decrypted, nil
}
