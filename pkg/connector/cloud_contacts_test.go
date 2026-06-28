// corten-matrix - A Matrix-iMessage puppeting bridge.
//
// Tests for contact-photo failure classification.

package connector

import (
	"errors"
	"testing"
)

// TestPhotoFailurePermanentForHost pins the host-aware classification: a 401/403
// from Apple's own photo CDN is transient (rate-limiting), so it is NOT cached as
// dead — that prevents a restart-time download storm from permanently blanking
// every iCloud contact photo. A 404/410 is a genuinely gone photo for every host,
// and a third-party 401/403 is a genuine block, so both stay permanent.
func TestPhotoFailurePermanentForHost(t *testing.T) {
	appleURL := "https://gateway.icloud.com/contacts/photo/abc"
	otherURL := "https://img.contactsplus.com/photo/abc"

	cases := []struct {
		name string
		err  error
		url  string
		want bool
	}{
		{"apple 403 transient", errors.New("HTTP 403 fetching " + appleURL), appleURL, false},
		{"apple 401 transient", errors.New("HTTP 401 fetching " + appleURL), appleURL, false},
		{"apple 404 permanent", errors.New("HTTP 404 fetching " + appleURL), appleURL, true},
		{"apple 410 permanent", errors.New("HTTP 410 fetching " + appleURL), appleURL, true},
		{"third-party 403 permanent", errors.New("HTTP 403 fetching " + otherURL), otherURL, true},
		{"third-party 404 permanent", errors.New("HTTP 404 fetching " + otherURL), otherURL, true},
		{"apple 500 transient", errors.New("HTTP 500 fetching " + appleURL), appleURL, false},
		{"apple 429 transient", errors.New("HTTP 429 fetching " + appleURL), appleURL, false},
		{"nil error", nil, appleURL, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := photoFailurePermanentForHost(tc.err, tc.url); got != tc.want {
				t.Errorf("photoFailurePermanentForHost(%q, host=%s) = %v, want %v", tc.err, tc.url, got, tc.want)
			}
		})
	}
}
