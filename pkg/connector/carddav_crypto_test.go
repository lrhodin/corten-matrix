package connector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	// Set up a temp directory for the key file
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	password := "my-secret-app-password"
	encrypted, err := EncryptCardDAVPassword(password)
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}
	if encrypted == "" {
		t.Fatal("encrypted should not be empty")
	}
	if encrypted == password {
		t.Fatal("encrypted should differ from plaintext")
	}

	decrypted, err := DecryptCardDAVPassword(encrypted)
	if err != nil {
		t.Fatalf("DecryptCardDAVPassword error: %v", err)
	}
	if decrypted != password {
		t.Errorf("decrypted = %q, want %q", decrypted, password)
	}
}

func TestEncryptDecryptRoundTrip_LongPassword(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	password := "this-is-a-very-long-password-with-special-chars-!@#$%^&*()"
	encrypted, err := EncryptCardDAVPassword(password)
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}

	decrypted, err := DecryptCardDAVPassword(encrypted)
	if err != nil {
		t.Fatalf("DecryptCardDAVPassword error: %v", err)
	}
	if decrypted != password {
		t.Errorf("decrypted = %q, want %q", decrypted, password)
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	encrypted, err := EncryptCardDAVPassword("my-password")
	if err != nil {
		t.Fatalf("EncryptCardDAVPassword error: %v", err)
	}

	// Overwrite key file with different key
	keyPath := filepath.Join(tmpDir, "corten-matrix", cardDAVKeyFileName)
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(i)
	}
	os.WriteFile(keyPath, newKey, 0600)

	_, err = DecryptCardDAVPassword(encrypted)
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with wrong key")
	}
}

func TestDecryptInvalidBase64(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// Generate a key so loadCardDAVKey succeeds
	_, err := generateCardDAVKey()
	if err != nil {
		t.Fatalf("generateCardDAVKey error: %v", err)
	}

	_, err = DecryptCardDAVPassword("not-valid-base64!!!")
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with invalid base64")
	}
}

func TestDecryptTooShort(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	_, err := generateCardDAVKey()
	if err != nil {
		t.Fatalf("generateCardDAVKey error: %v", err)
	}

	// Very short base64 (1 byte decoded)
	_, err = DecryptCardDAVPassword("AA==")
	if err == nil {
		t.Error("DecryptCardDAVPassword should fail with too-short ciphertext")
	}
}

func TestLoadCardDAVKey_WrongSize(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	dir := filepath.Join(tmpDir, "corten-matrix")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, cardDAVKeyFileName), []byte("too-short"), 0600)

	_, err := loadCardDAVKey()
	if err == nil {
		t.Error("loadCardDAVKey should fail with wrong-size key file")
	}
}

func TestLoadCardDAVKey_Missing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	_, err := loadCardDAVKey()
	if err == nil {
		t.Error("loadCardDAVKey should fail when key file is missing")
	}
}

func TestCardDAVKeyDir_XDGSet(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/test-xdg")
	got := cardDAVKeyDir()
	want := "/tmp/test-xdg/corten-matrix"
	if got != want {
		t.Errorf("cardDAVKeyDir() = %q, want %q", got, want)
	}
}

func TestCardDAVKeyDir_XDGUnset(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	got := cardDAVKeyDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "share", "corten-matrix")
	if got != want {
		t.Errorf("cardDAVKeyDir() = %q, want %q", got, want)
	}
}

func TestGenerateCardDAVKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	key, err := generateCardDAVKey()
	if err != nil {
		t.Fatalf("generateCardDAVKey error: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}

	// Verify file was written
	keyPath := filepath.Join(tmpDir, "corten-matrix", cardDAVKeyFileName)
	fileKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("key file read error: %v", err)
	}
	if string(fileKey) != string(key) {
		t.Error("key file content doesn't match returned key")
	}
}
