// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// AES-256-GCM encryption for CardDAV credentials stored in config.
// The encryption key is a random 32-byte file stored alongside session data.

package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

const cardDAVKeyFileName = "carddav.key"

// cardDAVKeyDir returns the session data directory where the encryption key is stored.
func cardDAVKeyDir() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "corten-matrix")
}

// cardDAVKeyPath returns the full path to the CardDAV encryption key file.
func cardDAVKeyPath() string {
	return filepath.Join(cardDAVKeyDir(), cardDAVKeyFileName)
}

// generateCardDAVKey creates a new random 32-byte AES-256 key and saves it.
// Returns the key bytes. If the key file already exists, it is overwritten.
func generateCardDAVKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate random key: %w", err)
	}

	dir := cardDAVKeyDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	if err := os.WriteFile(cardDAVKeyPath(), key, 0600); err != nil {
		return nil, fmt.Errorf("failed to write key file: %w", err)
	}

	return key, nil
}

// loadCardDAVKey reads the AES-256 key from the session data directory.
func loadCardDAVKey() ([]byte, error) {
	key, err := os.ReadFile(cardDAVKeyPath())
	if err != nil {
		return nil, fmt.Errorf("failed to read CardDAV key file (%s): %w", cardDAVKeyPath(), err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("CardDAV key file has wrong size: %d (expected 32)", len(key))
	}
	return key, nil
}

// EncryptCardDAVPassword encrypts a password using AES-256-GCM.
// Generates a new key if one doesn't exist. Returns base64-encoded ciphertext.
func EncryptCardDAVPassword(password string) (string, error) {
	// Try to load existing key, generate if missing
	key, err := loadCardDAVKey()
	if err != nil {
		key, err = generateCardDAVKey()
		if err != nil {
			return "", err
		}
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Seal: nonce is prepended to ciphertext
	ciphertext := gcm.Seal(nonce, nonce, []byte(password), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptCardDAVPassword decrypts a base64-encoded AES-256-GCM ciphertext.
func DecryptCardDAVPassword(encrypted string) (string, error) {
	key, err := loadCardDAVKey()
	if err != nil {
		return "", err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decode base64: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt: %w", err)
	}

	return string(plaintext), nil
}
