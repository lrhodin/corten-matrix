package imessage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	log "maunium.net/go/maulogger/v2"

	"github.com/lrhodin/corten-matrix/ipc"
)

type testBridge struct {
	cfg *PlatformConfig
}

func (tb *testBridge) GetIPC() *ipc.Processor { return nil }

func (tb *testBridge) GetLog() log.Logger { return nil }

func (tb *testBridge) GetZLog() *zerolog.Logger { return nil }

func (tb *testBridge) GetConnectorConfig() *PlatformConfig { return tb.cfg }

func (tb *testBridge) PingServer() (start, serverTs, end time.Time) {
	return time.Time{}, time.Time{}, time.Time{}
}

func (tb *testBridge) SendBridgeStatus(state BridgeStatus) {}

func (tb *testBridge) ReIDPortal(oldGUID, newGUID string, mergeExisting bool) bool { return false }

func (tb *testBridge) GetMessagesSince(chatGUID string, since time.Time) []string { return nil }

func (tb *testBridge) SetPushKey(req *PushKeyRequest) {}

// ---------------------------------------------------------------------------
// Message.SenderText
// ---------------------------------------------------------------------------

func TestMessage_SenderText_FromMe(t *testing.T) {
	msg := &Message{IsFromMe: true, Sender: Identifier{LocalID: "alice"}}
	if got := msg.SenderText(); got != "self" {
		t.Errorf("SenderText() = %q, want %q", got, "self")
	}
}

func TestMessage_SenderText_FromOther(t *testing.T) {
	msg := &Message{IsFromMe: false, Sender: Identifier{LocalID: "+15551234567"}}
	if got := msg.SenderText(); got != "+15551234567" {
		t.Errorf("SenderText() = %q, want %q", got, "+15551234567")
	}
}

func TestMessage_SenderText_Empty(t *testing.T) {
	msg := &Message{IsFromMe: false, Sender: Identifier{}}
	if got := msg.SenderText(); got != "" {
		t.Errorf("SenderText() = %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// Contact.HasName
// ---------------------------------------------------------------------------

func TestContact_HasName(t *testing.T) {
	tests := []struct {
		name    string
		contact *Contact
		want    bool
	}{
		{"nil", nil, false},
		{"empty", &Contact{}, false},
		{"first only", &Contact{FirstName: "Alice"}, true},
		{"last only", &Contact{LastName: "Smith"}, true},
		{"nickname only", &Contact{Nickname: "Al"}, true},
		{"full name", &Contact{FirstName: "Alice", LastName: "Smith"}, true},
		{"phones but no name", &Contact{Phones: []string{"+1555"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.contact.HasName(); got != tt.want {
				t.Errorf("HasName() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Contact.Name
// ---------------------------------------------------------------------------

func TestContact_Name(t *testing.T) {
	tests := []struct {
		name    string
		contact *Contact
		want    string
	}{
		{"nil", nil, ""},
		{"empty", &Contact{}, ""},
		{"first and last", &Contact{FirstName: "Alice", LastName: "Smith"}, "Alice Smith"},
		{"first only", &Contact{FirstName: "Alice"}, "Alice"},
		{"last only", &Contact{LastName: "Smith"}, "Smith"},
		{"nickname only", &Contact{Nickname: "Al"}, "Al"},
		{"email fallback", &Contact{Emails: []string{"alice@example.com"}}, "alice@example.com"},
		{"phone fallback", &Contact{Phones: []string{"+15551234567"}}, "+15551234567"},
		{"first takes priority over nickname", &Contact{FirstName: "Alice", Nickname: "Al"}, "Alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.contact.Name(); got != tt.want {
				t.Errorf("Name() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Identifier / ParseIdentifier / String
// ---------------------------------------------------------------------------

func TestParseIdentifier(t *testing.T) {
	tests := []struct {
		guid    string
		want    Identifier
	}{
		{"", Identifier{}},
		{"simple-guid", Identifier{LocalID: "simple-guid"}},
		{"iMessage;-;+15551234567", Identifier{Service: "iMessage", IsGroup: false, LocalID: "+15551234567"}},
		{"iMessage;+;chat123456", Identifier{Service: "iMessage", IsGroup: true, LocalID: "chat123456"}},
		{"SMS;-;+15551234567", Identifier{Service: "SMS", IsGroup: false, LocalID: "+15551234567"}},
		{"SMS;+;hexuuid-1234", Identifier{Service: "SMS", IsGroup: true, LocalID: "hexuuid-1234"}},
		// "+" separator makes it a group even without "chat" prefix
		{"iMessage;+;some-uuid", Identifier{Service: "iMessage", IsGroup: true, LocalID: "some-uuid"}},
		// chat prefix makes it a group even with "-" separator
		{"iMessage;-;chat999", Identifier{Service: "iMessage", IsGroup: true, LocalID: "chat999"}},
		// only two parts
		{"nodots", Identifier{LocalID: "nodots"}},
	}
	for _, tt := range tests {
		t.Run(tt.guid, func(t *testing.T) {
			got := ParseIdentifier(tt.guid)
			if got != tt.want {
				t.Errorf("ParseIdentifier(%q) = %+v, want %+v", tt.guid, got, tt.want)
			}
		})
	}
}

func TestIdentifier_String(t *testing.T) {
	tests := []struct {
		id   Identifier
		want string
	}{
		{Identifier{}, ""},
		{Identifier{Service: "iMessage", IsGroup: false, LocalID: "+15551234567"}, "iMessage;-;+15551234567"},
		{Identifier{Service: "iMessage", IsGroup: true, LocalID: "chat123"}, "iMessage;+;chat123"},
		{Identifier{Service: "SMS", IsGroup: false, LocalID: "+1555"}, "SMS;-;+1555"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.id.String(); got != tt.want {
				t.Errorf("Identifier.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIdentifier_RoundTrip(t *testing.T) {
	original := Identifier{Service: "iMessage", IsGroup: false, LocalID: "+15551234567"}
	s := original.String()
	parsed := ParseIdentifier(s)
	if parsed != original {
		t.Errorf("round-trip failed: %+v -> %q -> %+v", original, s, parsed)
	}
}

// ---------------------------------------------------------------------------
// Attachment.GetMimeType
// ---------------------------------------------------------------------------

func TestAttachment_GetMimeType_Set(t *testing.T) {
	a := &Attachment{MimeType: "image/png"}
	if got := a.GetMimeType(); got != "image/png" {
		t.Errorf("GetMimeType() = %q, want %q", got, "image/png")
	}
}

func TestAttachment_GetMimeType_DetectsFromFile(t *testing.T) {
	// Create a real PNG file for mimetype detection
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.png")
	// Minimal PNG header
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	os.WriteFile(f, pngHeader, 0600)

	a := &Attachment{PathOnDisk: f}
	got := a.GetMimeType()
	if got != "image/png" {
		t.Errorf("GetMimeType() = %q, want %q", got, "image/png")
	}
	// Second call should return cached value
	got2 := a.GetMimeType()
	if got2 != got {
		t.Errorf("second call returned %q, want %q", got2, got)
	}
}

func TestAttachment_GetMimeType_NoFile(t *testing.T) {
	a := &Attachment{PathOnDisk: "/nonexistent/path"}
	got := a.GetMimeType()
	if got != "" {
		t.Errorf("GetMimeType() = %q, want empty string for missing file", got)
	}
	// triedMagic should be set, so second call won't retry
	if !a.triedMagic {
		t.Error("triedMagic should be true after failed detection")
	}
	// Second call returns early (triedMagic branch)
	got2 := a.GetMimeType()
	if got2 != "" {
		t.Errorf("GetMimeType() second call = %q, want empty", got2)
	}
}

func TestAttachment_GetFileName(t *testing.T) {
	a := &Attachment{FileName: "photo.jpg"}
	if got := a.GetFileName(); got != "photo.jpg" {
		t.Errorf("GetFileName() = %q, want %q", got, "photo.jpg")
	}
}

// ---------------------------------------------------------------------------
// Attachment.Read
// ---------------------------------------------------------------------------

func TestAttachment_Read(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "testfile.txt")
	content := []byte("hello world")
	os.WriteFile(f, content, 0600)

	a := &Attachment{PathOnDisk: f}
	got, err := a.Read()
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("Read() = %q, want %q", string(got), string(content))
	}
}

func TestAttachment_Read_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("can't get home dir")
	}
	tmp, err := os.MkdirTemp(home, "imessage-test-*")
	if err != nil {
		t.Skip("can't create temp dir in home")
	}
	defer os.RemoveAll(tmp)

	rel, _ := filepath.Rel(home, tmp)
	f := filepath.Join(tmp, "testfile.txt")
	os.WriteFile(f, []byte("tilde"), 0600)

	a := &Attachment{PathOnDisk: "~/" + filepath.Join(rel, "testfile.txt")}
	got, err := a.Read()
	if err != nil {
		t.Fatalf("Read() with tilde error: %v", err)
	}
	if string(got) != "tilde" {
		t.Errorf("Read() = %q, want %q", string(got), "tilde")
	}
}

func TestAttachment_Read_MissingFile(t *testing.T) {
	a := &Attachment{PathOnDisk: "/definitely/not/a/real/file"}
	if _, err := a.Read(); err == nil {
		t.Fatal("Read() expected error for missing file, got nil")
	}
}

func TestAttachment_Read_TildeMissingFile(t *testing.T) {
	a := &Attachment{PathOnDisk: "~/definitely-not-a-real-file-imessage-test"}
	if _, err := a.Read(); err == nil {
		t.Fatal("Read() expected error for missing ~/ file, got nil")
	}
}

func TestAttachment_Read_HomeDirError(t *testing.T) {
	orig := userHomeDir
	userHomeDir = func() (string, error) {
		return "", errors.New("boom")
	}
	t.Cleanup(func() { userHomeDir = orig })

	a := &Attachment{PathOnDisk: "~/any-file"}
	if _, err := a.Read(); err == nil {
		t.Fatal("Read() expected home directory error, got nil")
	}
}

func TestAttachment_Delete(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "deleteme.txt")
	os.WriteFile(f, []byte("bye"), 0600)

	a := &Attachment{PathOnDisk: f}
	if err := a.Delete(); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file should not exist after Delete()")
	}
}

// ---------------------------------------------------------------------------
// SendFilePrepare
// ---------------------------------------------------------------------------

func TestSendFilePrepare(t *testing.T) {
	data := []byte("test file content")
	dir, filePath, err := SendFilePrepare("test.txt", data)
	if err != nil {
		t.Fatalf("SendFilePrepare() error: %v", err)
	}
	defer os.RemoveAll(dir)

	// Verify file was written
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("can't read written file: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("file content = %q, want %q", string(got), string(data))
	}

	// Verify the file is inside the directory
	if filepath.Dir(filePath) != dir {
		t.Errorf("file not inside temp dir: %q not in %q", filePath, dir)
	}
}

func TestSendFilePrepare_WriteError(t *testing.T) {
	// Nested path without parent dir inside temp upload dir should fail write.
	_, _, err := SendFilePrepare(filepath.Join("no-such-dir", "test.txt"), []byte("x"))
	if err == nil {
		t.Fatal("SendFilePrepare() expected write error, got nil")
	}
}

func TestSendFilePrepare_TempDirError(t *testing.T) {
	// Force TempDir() to fail by pointing TMPDIR to a regular file.
	tmpFile, err := os.CreateTemp("", "imessage-upload-file-*")
	if err != nil {
		t.Fatalf("CreateTemp() error: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	t.Setenv("TMPDIR", tmpPath)
	if _, _, err := SendFilePrepare("test.txt", []byte("x")); err == nil {
		t.Fatal("SendFilePrepare() expected temp dir error, got nil")
	}
}

// ---------------------------------------------------------------------------
// GroupActionType / ItemType constants
// ---------------------------------------------------------------------------

func TestConstants(t *testing.T) {
	if GroupActionAddUser != 0 {
		t.Errorf("GroupActionAddUser = %d, want 0", GroupActionAddUser)
	}
	if GroupActionRemoveUser != 1 {
		t.Errorf("GroupActionRemoveUser = %d, want 1", GroupActionRemoveUser)
	}
	if ItemTypeMessage != 0 {
		t.Errorf("ItemTypeMessage = %d, want 0", ItemTypeMessage)
	}
	if ItemTypeMember != 1 {
		t.Errorf("ItemTypeMember = %d, want 1", ItemTypeMember)
	}
	if ItemTypeName != 2 {
		t.Errorf("ItemTypeName = %d, want 2", ItemTypeName)
	}
	if ItemTypeAvatar != 3 {
		t.Errorf("ItemTypeAvatar = %d, want 3", ItemTypeAvatar)
	}
	if ItemTypeError != -100 {
		t.Errorf("ItemTypeError = %d, want -100", ItemTypeError)
	}
}

// ---------------------------------------------------------------------------
// PlatformConfig.BridgeName
// ---------------------------------------------------------------------------

func TestBridgeName(t *testing.T) {
	tests := []struct {
		platform string
		want     string
	}{
		{"android", "Android SMS Bridge"},
		{"mac", "iMessage Bridge"},
		{"", "iMessage Bridge"},
	}
	for _, tt := range tests {
		pc := &PlatformConfig{Platform: tt.platform}
		if got := pc.BridgeName(); got != tt.want {
			t.Errorf("BridgeName(%q) = %q, want %q", tt.platform, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TempDir
// ---------------------------------------------------------------------------

func TestTempDir(t *testing.T) {
	dir, err := TempDir("test-imessage")
	if err != nil {
		t.Fatalf("TempDir() error: %v", err)
	}
	defer os.RemoveAll(dir)

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("TempDir() result is not a directory")
	}
}

func TestTempDir_MkdirAllError(t *testing.T) {
	// Force os.TempDir() to return a regular file path so MkdirAll fails.
	tmpFile, err := os.CreateTemp("", "imessage-tempdir-file-*")
	if err != nil {
		t.Fatalf("CreateTemp() error: %v", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	orig := os.Getenv("TMPDIR")
	t.Setenv("TMPDIR", tmpPath)
	defer func() {
		if orig == "" {
			os.Unsetenv("TMPDIR")
		} else {
			os.Setenv("TMPDIR", orig)
		}
	}()

	if _, err := TempDir("test-imessage"); err == nil {
		t.Fatal("TempDir() expected error when TMPDIR points to file, got nil")
	}
}

func TestTempDir_MkdirTempError(t *testing.T) {
	origPerm := TempDirPermissions
	TempDirPermissions = 0400
	t.Cleanup(func() { TempDirPermissions = origPerm })

	base := filepath.Join(t.TempDir(), "no-write-temp-root")
	t.Setenv("TMPDIR", base)

	if _, err := TempDir("test-imessage"); err == nil {
		t.Fatal("TempDir() expected error from MkdirTemp in non-writable TMPDIR, got nil")
	}
}

// ---------------------------------------------------------------------------
// NewAPI (error path only — no real implementations registered in test)
// ---------------------------------------------------------------------------

func TestNewAPI_UnknownPlatform(t *testing.T) {
	bridge := &testBridge{cfg: &PlatformConfig{Platform: "__bogus_platform__"}}
	api, err := NewAPI(bridge)
	if err == nil {
		t.Fatal("NewAPI() expected error for unknown platform, got nil")
	}
	if api != nil {
		t.Fatal("NewAPI() expected nil API on unknown platform")
	}
}

func TestNewAPI_KnownPlatform(t *testing.T) {
	const platform = "__unit_test_platform__"
	origImpl, hadOrig := Implementations[platform]
	t.Cleanup(func() {
		if hadOrig {
			Implementations[platform] = origImpl
		} else {
			delete(Implementations, platform)
		}
	})

	called := false
	Implementations[platform] = func(b Bridge) (API, error) {
		called = true
		if b == nil {
			t.Fatal("implementation received nil bridge")
		}
		return nil, nil
	}

	bridge := &testBridge{cfg: &PlatformConfig{Platform: platform}}
	api, err := NewAPI(bridge)
	if err != nil {
		t.Fatalf("NewAPI() unexpected error: %v", err)
	}
	if api != nil {
		t.Fatal("NewAPI() expected nil API from test implementation")
	}
	if !called {
		t.Fatal("expected implementation to be called")
	}
}
