package connector

import (
	"testing"

	"github.com/lrhodin/corten-matrix/imessage"
)

func TestResolveProviderURL(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"user@gmail.com", "https://www.googleapis.com/carddav/v1/principals/user@gmail.com/lists/default/"},
		{"user@googlemail.com", "https://www.googleapis.com/carddav/v1/principals/user@googlemail.com/lists/default/"},
		{"user@fastmail.com", "https://carddav.fastmail.com/dav/addressbooks/user/user@fastmail.com/Default/"},
		{"user@fastmail.fm", "https://carddav.fastmail.com/dav/addressbooks/user/user@fastmail.fm/Default/"},
		{"user@icloud.com", "https://contacts.icloud.com"},
		{"user@me.com", "https://contacts.icloud.com"},
		{"user@mac.com", "https://contacts.icloud.com"},
		{"user@yahoo.com", "https://carddav.address.yahoo.com/dav/user@yahoo.com/"},
		{"user@unknown-provider.com", ""},
		{"invalid-email", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			got := ResolveProviderURL(tt.email)
			if got != tt.want {
				t.Errorf("ResolveProviderURL(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

func TestResolveRedirect(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		location string
		want     string
	}{
		{"absolute URL", "https://example.com/.well-known/carddav", "https://other.com/dav/", "https://other.com/dav/"},
		{"http absolute", "https://example.com/path", "http://other.com/dav/", "http://other.com/dav/"},
		{"relative path", "https://example.com/.well-known/carddav", "/dav/principals/", "https://example.com/dav/principals/"},
		{"just path no match", "https://example.com", "relative", "relative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRedirect(tt.baseURL, tt.location)
			if got != tt.want {
				t.Errorf("resolveRedirect(%q, %q) = %q, want %q", tt.baseURL, tt.location, got, tt.want)
			}
		})
	}
}

func TestExternalCardDAVClient_ResolveURL(t *testing.T) {
	client := &externalCardDAVClient{
		baseURL: "https://carddav.example.com/dav",
	}

	tests := []struct {
		href string
		want string
	}{
		{"https://other.com/path", "https://other.com/path"},
		{"http://other.com/path", "http://other.com/path"},
		{"/dav/addressbooks/user/", "https://carddav.example.com/dav/addressbooks/user/"},
		{"/path", "https://carddav.example.com/path"},
	}
	for _, tt := range tests {
		t.Run(tt.href, func(t *testing.T) {
			got := client.resolveURL(tt.href)
			if got != tt.want {
				t.Errorf("resolveURL(%q) = %q, want %q", tt.href, got, tt.want)
			}
		})
	}
}

func TestExternalCardDAVClient_GetContactInfo_Empty(t *testing.T) {
	client := &externalCardDAVClient{
		byPhone: make(map[string]*imessage.Contact),
		byEmail: make(map[string]*imessage.Contact),
	}
	contact, err := client.GetContactInfo("+15551234567")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact != nil {
		t.Error("expected nil contact for empty directory")
	}
}

func TestExternalCardDAVClient_GetContactInfo_ByPhone(t *testing.T) {
	alice := &imessage.Contact{FirstName: "Alice", Phones: []string{"+15551234567"}}
	client := &externalCardDAVClient{
		byPhone: map[string]*imessage.Contact{
			"+15551234567": alice,
			"15551234567":  alice,
			"5551234567":   alice,
			"1234567":      alice,
		},
		byEmail: make(map[string]*imessage.Contact),
	}

	contact, err := client.GetContactInfo("+15551234567")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact == nil || contact.FirstName != "Alice" {
		t.Error("expected Alice contact")
	}
}

func TestExternalCardDAVClient_GetContactInfo_ByEmail(t *testing.T) {
	bob := &imessage.Contact{FirstName: "Bob", Emails: []string{"bob@example.com"}}
	client := &externalCardDAVClient{
		byPhone: make(map[string]*imessage.Contact),
		byEmail: map[string]*imessage.Contact{
			"bob@example.com": bob,
		},
	}

	contact, err := client.GetContactInfo("bob@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if contact == nil || contact.FirstName != "Bob" {
		t.Error("expected Bob contact")
	}
}

func TestExternalCardDAVClient_GetAllContacts(t *testing.T) {
	alice := &imessage.Contact{FirstName: "Alice"}
	bob := &imessage.Contact{FirstName: "Bob"}
	client := &externalCardDAVClient{
		contacts: []*imessage.Contact{alice, bob},
	}

	all := client.GetAllContacts()
	if len(all) != 2 {
		t.Fatalf("got %d contacts, want 2", len(all))
	}
}
