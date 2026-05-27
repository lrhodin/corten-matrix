package connector

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCardDAVConfig_IsConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  CardDAVConfig
		want bool
	}{
		{"both set", CardDAVConfig{Email: "a@b.com", PasswordEncrypted: "enc"}, true},
		{"no email", CardDAVConfig{PasswordEncrypted: "enc"}, false},
		{"no password", CardDAVConfig{Email: "a@b.com"}, false},
		{"empty", CardDAVConfig{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsConfigured(); got != tt.want {
				t.Errorf("IsConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCardDAVConfig_GetUsername(t *testing.T) {
	t.Run("explicit username", func(t *testing.T) {
		cfg := CardDAVConfig{Email: "a@b.com", Username: "user"}
		if got := cfg.GetUsername(); got != "user" {
			t.Errorf("GetUsername() = %q, want %q", got, "user")
		}
	})
	t.Run("falls back to email", func(t *testing.T) {
		cfg := CardDAVConfig{Email: "a@b.com"}
		if got := cfg.GetUsername(); got != "a@b.com" {
			t.Errorf("GetUsername() = %q, want %q", got, "a@b.com")
		}
	})
}

func TestIMConfig_PostProcess(t *testing.T) {
	c := &IMConfig{DisplaynameTemplate: "{{.FirstName}} {{.LastName}}"}
	if err := c.PostProcess(); err != nil {
		t.Fatalf("PostProcess() error: %v", err)
	}
	if c.displaynameTemplate == nil {
		t.Fatal("displaynameTemplate should not be nil after PostProcess")
	}
}

func TestIMConfig_PostProcess_InvalidTemplate(t *testing.T) {
	c := &IMConfig{DisplaynameTemplate: "{{.Bad"}
	if err := c.PostProcess(); err == nil {
		t.Error("PostProcess() should return error for invalid template")
	}
}

func TestIMConfig_FormatDisplayname(t *testing.T) {
	c := &IMConfig{DisplaynameTemplate: "{{.FirstName}} {{.LastName}}"}
	c.PostProcess()

	tests := []struct {
		name   string
		params DisplaynameParams
		want   string
	}{
		{"full name", DisplaynameParams{FirstName: "Alice", LastName: "Smith", ID: "id1"}, "Alice Smith"},
		{"first only", DisplaynameParams{FirstName: "Alice", ID: "id2"}, "Alice"},
		{"empty falls back to ID", DisplaynameParams{ID: "fallback-id"}, "fallback-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.FormatDisplayname(tt.params)
			if got != tt.want {
				t.Errorf("FormatDisplayname() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIMConfig_FormatDisplayname_DefaultTemplate(t *testing.T) {
	tmpl := `{{if .FirstName}}{{.FirstName}}{{if .LastName}} {{.LastName}}{{end}}{{else if .Nickname}}{{.Nickname}}{{else if .Phone}}{{.Phone}}{{else if .Email}}{{.Email}}{{else}}{{.ID}}{{end}}`
	c := &IMConfig{DisplaynameTemplate: tmpl}
	c.PostProcess()

	tests := []struct {
		name   string
		params DisplaynameParams
		want   string
	}{
		{"first+last", DisplaynameParams{FirstName: "Alice", LastName: "Smith"}, "Alice Smith"},
		{"first only", DisplaynameParams{FirstName: "Alice"}, "Alice"},
		{"nickname", DisplaynameParams{Nickname: "Al"}, "Al"},
		{"phone", DisplaynameParams{Phone: "+1555"}, "+1555"},
		{"email", DisplaynameParams{Email: "a@b.com"}, "a@b.com"},
		{"id fallback", DisplaynameParams{ID: "some-id"}, "some-id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.FormatDisplayname(tt.params)
			if got != tt.want {
				t.Errorf("FormatDisplayname() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIMConfig_UseChatDBBackfill(t *testing.T) {
	tests := []struct {
		name    string
		cfg     IMConfig
		want    bool
	}{
		{"enabled chatdb", IMConfig{CloudKitBackfill: true, BackfillSource: "chatdb"}, true},
		{"enabled cloudkit", IMConfig{CloudKitBackfill: true, BackfillSource: "cloudkit"}, false},
		{"disabled chatdb", IMConfig{CloudKitBackfill: false, BackfillSource: "chatdb"}, false},
		{"disabled empty", IMConfig{CloudKitBackfill: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.UseChatDBBackfill(); got != tt.want {
				t.Errorf("UseChatDBBackfill() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIMConfig_UseCloudKitBackfill(t *testing.T) {
	tests := []struct {
		name string
		cfg  IMConfig
		want bool
	}{
		{"enabled cloudkit", IMConfig{CloudKitBackfill: true, BackfillSource: "cloudkit"}, true},
		{"enabled empty source", IMConfig{CloudKitBackfill: true}, true},
		{"enabled chatdb", IMConfig{CloudKitBackfill: true, BackfillSource: "chatdb"}, false},
		{"disabled", IMConfig{CloudKitBackfill: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.UseCloudKitBackfill(); got != tt.want {
				t.Errorf("UseCloudKitBackfill() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIMConfig_UnmarshalYAML(t *testing.T) {
	yamlData := `
displayname_template: "{{.FirstName}}"
cloudkit_backfill: true
backfill_source: chatdb
`
	var c IMConfig
	if err := yaml.Unmarshal([]byte(yamlData), &c); err != nil {
		t.Fatalf("UnmarshalYAML error: %v", err)
	}
	if c.DisplaynameTemplate != "{{.FirstName}}" {
		t.Errorf("DisplaynameTemplate = %q, want %q", c.DisplaynameTemplate, "{{.FirstName}}")
	}
	if !c.CloudKitBackfill {
		t.Error("CloudKitBackfill should be true")
	}
	if c.BackfillSource != "chatdb" {
		t.Errorf("BackfillSource = %q, want %q", c.BackfillSource, "chatdb")
	}
	if c.displaynameTemplate == nil {
		t.Error("displaynameTemplate should be set after unmarshal (PostProcess called)")
	}
}

func TestIMConfig_URLPreviewsInBackfill(t *testing.T) {
	t.Run("explicit true", func(t *testing.T) {
		var c IMConfig
		if err := yaml.Unmarshal([]byte("url_previews_in_backfill: true\n"), &c); err != nil {
			t.Fatalf("UnmarshalYAML error: %v", err)
		}
		if !c.URLPreviewsInBackfill {
			t.Error("URLPreviewsInBackfill should be true")
		}
	})
	t.Run("explicit false", func(t *testing.T) {
		var c IMConfig
		if err := yaml.Unmarshal([]byte("url_previews_in_backfill: false\n"), &c); err != nil {
			t.Fatalf("UnmarshalYAML error: %v", err)
		}
		if c.URLPreviewsInBackfill {
			t.Error("URLPreviewsInBackfill should be false")
		}
	})
}
