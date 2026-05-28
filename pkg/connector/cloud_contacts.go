// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// Cloud-based contact sync via Apple's CardDAV (iCloud Contacts).
// Uses DSID + mmeAuthToken credentials obtained from the MobileMe delegate
// during login to access iCloud Contacts without a Mac relay.

package connector

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/lrhodin/imessage/imessage"
	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// contactSource is the interface for contact name resolution.
// Both iCloud CardDAV and external CardDAV servers implement this.
type contactSource interface {
	SyncContacts(log zerolog.Logger) error
	GetContactInfo(identifier string) (*imessage.Contact, error)
	// GetAllContacts returns a snapshot of all cached contacts for bulk search.
	GetAllContacts() []*imessage.Contact
}

// cloudContactsClient fetches contacts from iCloud via CardDAV and caches
// them locally for fast phone/email lookups.
type cloudContactsClient struct {
	baseURL    string             // CardDAV URL from MobileMe delegate
	dsid       string             // cached DSID for URL construction
	rustClient *rustpushgo.Client // for getting auth headers via TokenProvider
	httpClient *http.Client

	mu       sync.RWMutex
	byPhone  map[string]*imessage.Contact // normalized phone → contact
	byEmail  map[string]*imessage.Contact // lowercase email → contact
	contacts []*imessage.Contact          // all contacts
	lastSync time.Time
}

// newCloudContactsClient creates a CardDAV contacts client using the rust Client's
// TokenProvider for authentication. Returns nil if the token provider is unavailable
// or the contacts URL can't be retrieved.
func newCloudContactsClient(rustClient *rustpushgo.Client, log zerolog.Logger) *cloudContactsClient {
	if rustClient == nil {
		return nil
	}

	contactsURL, err := rustClient.GetContactsUrl()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get contacts URL from TokenProvider")
		return nil
	}
	if contactsURL == nil || *contactsURL == "" {
		log.Warn().Msg("No contacts CardDAV URL available from TokenProvider")
		return nil
	}

	dsidPtr, err := rustClient.GetDsid()
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get DSID from TokenProvider")
		return nil
	}

	return &cloudContactsClient{
		baseURL:    strings.TrimRight(*contactsURL, "/"),
		dsid:       *dsidPtr,
		rustClient: rustClient,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		byPhone: make(map[string]*imessage.Contact),
		byEmail: make(map[string]*imessage.Contact),
	}
}

// doRequest performs an authenticated request to the CardDAV server.
// Gets fresh auth + anisette headers from the TokenProvider on each call.
func (c *cloudContactsClient) doRequest(method, url, body string, depth string) (*http.Response, error) {
	// Get auth headers from Rust (includes Authorization + anisette, auto-refreshes)
	headersPtr, err := c.rustClient.GetIcloudAuthHeaders()
	if err != nil {
		return nil, fmt.Errorf("failed to get iCloud auth headers: %w", err)
	}
	if headersPtr == nil {
		return nil, fmt.Errorf("no iCloud auth headers available (no token provider)")
	}
	headers := *headersPtr

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	return c.httpClient.Do(req)
}

// SyncContacts fetches all contacts from iCloud via CardDAV and rebuilds the cache.
func (c *cloudContactsClient) SyncContacts(log zerolog.Logger) error {
	// Step 1: Get the principal URL
	principalURL, err := c.discoverPrincipal(log)
	if err != nil {
		log.Warn().Err(sanitizeURLError(err, c.baseURL+"/")).Msg("CardDAV: failed to discover principal URL")
		return sanitizeURLError(err, c.baseURL+"/")
	}
	log.Debug().Str("principal_host", logSafeURL(principalURL)).Msg("CardDAV: discovered principal URL")

	// Step 2: Get the address book home set
	homeSetURL, err := c.discoverAddressBookHome(log, principalURL)
	if err != nil {
		log.Warn().Err(sanitizeURLError(err, principalURL)).Msg("CardDAV: failed to discover address book home")
		return sanitizeURLError(err, principalURL)
	}
	log.Debug().Str("home_set_host", logSafeURL(homeSetURL)).Msg("CardDAV: discovered address book home")

	// Step 3: List address books
	addressBooks, err := c.listAddressBooks(log, homeSetURL)
	if err != nil {
		log.Warn().Err(sanitizeURLError(err, homeSetURL)).Msg("CardDAV: failed to list address books")
		return sanitizeURLError(err, homeSetURL)
	}
	log.Debug().Int("count", len(addressBooks)).Msg("CardDAV: found address books")

	// Step 4: Fetch all vCards from each address book
	var allContacts []*imessage.Contact
	for _, abURL := range addressBooks {
		contacts, fetchErr := c.fetchAllVCards(log, abURL)
		if fetchErr != nil {
			log.Warn().Err(sanitizeURLError(fetchErr, abURL)).Str("address_book_host", logSafeURL(abURL)).Msg("CardDAV: failed to fetch vCards")
			continue
		}
		allContacts = append(allContacts, contacts...)
	}

	// Step 4.5: Download any photo URLs — use authenticated fetcher for iCloud URLs
	downloadContactPhotos(allContacts, log, c.downloadAuthURL)

	// Step 5: Build lookup caches
	c.mu.Lock()
	defer c.mu.Unlock()

	c.byPhone = make(map[string]*imessage.Contact, len(allContacts)*2)
	c.byEmail = make(map[string]*imessage.Contact, len(allContacts))
	c.contacts = allContacts

	for _, contact := range allContacts {
		for _, phone := range contact.Phones {
			for _, suffix := range phoneSuffixes(phone) {
				c.byPhone[suffix] = contact
			}
		}
		for _, email := range contact.Emails {
			c.byEmail[strings.ToLower(email)] = contact
		}
	}
	c.lastSync = time.Now()

	log.Info().
		Int("contacts", len(allContacts)).
		Int("phone_keys", len(c.byPhone)).
		Int("email_keys", len(c.byEmail)).
		Msg("Contact cache synced from iCloud CardDAV")
	return nil
}

// GetContactInfo looks up a contact by phone number or email.
func (c *cloudContactsClient) GetContactInfo(identifier string) (*imessage.Contact, error) {
	if c == nil {
		return nil, nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// Try email first
	if !strings.HasPrefix(identifier, "+") && strings.Contains(identifier, "@") {
		if contact, ok := c.byEmail[strings.ToLower(identifier)]; ok {
			return contact, nil
		}
		return nil, nil
	}

	// Phone number: try all suffix variations
	for _, suffix := range phoneSuffixes(identifier) {
		if contact, ok := c.byPhone[suffix]; ok {
			return contact, nil
		}
	}

	return nil, nil
}

// GetAllContacts returns a snapshot of the full contact list for bulk search.
func (c *cloudContactsClient) GetAllContacts() []*imessage.Contact {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	// Return a copy of the slice header so callers can iterate safely.
	result := make([]*imessage.Contact, len(c.contacts))
	copy(result, c.contacts)
	return result
}

// ============================================================================
// CardDAV Protocol Implementation
// ============================================================================

// discoverPrincipal finds the principal URL via PROPFIND on the base URL.
func (c *cloudContactsClient) discoverPrincipal(log zerolog.Logger) (string, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:current-user-principal/>
  </d:prop>
</d:propfind>`

	resp, err := c.doRequest("PROPFIND", c.baseURL+"/", body, "0")
	if err != nil {
		return "", fmt.Errorf("PROPFIND failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PROPFIND returned %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	href := extractPropValue(data, "current-user-principal")
	if href == "" {
		log.Debug().Int("body_bytes", len(data)).Msg("CardDAV: PROPFIND response (no principal found)")
		return "", fmt.Errorf("no current-user-principal in response")
	}

	return c.resolveURL(href), nil
}

// discoverAddressBookHome finds the address book home set from the principal.
func (c *cloudContactsClient) discoverAddressBookHome(log zerolog.Logger, principalURL string) (string, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <card:addressbook-home-set/>
  </d:prop>
</d:propfind>`

	resp, err := c.doRequest("PROPFIND", principalURL, body, "0")
	if err != nil {
		return "", fmt.Errorf("PROPFIND failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("PROPFIND returned %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	href := extractPropValue(data, "addressbook-home-set")
	if href == "" {
		log.Debug().Int("body_bytes", len(data)).Msg("CardDAV: PROPFIND response (no home set found)")
		return "", fmt.Errorf("no addressbook-home-set in response")
	}

	return c.resolveURL(href), nil
}

// listAddressBooks returns the URLs of all address books in the home set.
func (c *cloudContactsClient) listAddressBooks(log zerolog.Logger, homeSetURL string) ([]string, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<d:propfind xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:resourcetype/>
    <d:displayname/>
  </d:prop>
</d:propfind>`

	resp, err := c.doRequest("PROPFIND", homeSetURL, body, "1")
	if err != nil {
		return nil, fmt.Errorf("PROPFIND failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("PROPFIND returned %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	log.Debug().
		Int("body_bytes", len(data)).
		Msg("CardDAV: listAddressBooks PROPFIND response")
	return c.parseAddressBookList(data, homeSetURL, log), nil
}

// fetchAllVCards fetches all vCards from an address book using REPORT addressbook-query.
func (c *cloudContactsClient) fetchAllVCards(log zerolog.Logger, addressBookURL string) ([]*imessage.Contact, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:getetag/>
    <card:address-data/>
  </d:prop>
</card:addressbook-query>`

	resp, err := c.doRequest("REPORT", addressBookURL, body, "1")
	if err != nil {
		return nil, fmt.Errorf("REPORT failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 207 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("REPORT returned %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 500)]))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	log.Debug().
		Int("body_bytes", len(data)).
		Str("address_book_host", logSafeURL(addressBookURL)).
		Msg("CardDAV: REPORT response received")

	return c.parseVCardMultistatus(data, log), nil
}

// resolveURL converts a relative href to an absolute URL.
func (c *cloudContactsClient) resolveURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	// Extract scheme + host from baseURL
	base := c.baseURL
	if idx := strings.Index(base, "://"); idx >= 0 {
		schemeHost := base[:idx+3]
		rest := base[idx+3:]
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			base = schemeHost + rest[:slashIdx]
		}
	}
	return base + href
}

// ============================================================================
// XML Parsing helpers
// ============================================================================

// multistatus represents a WebDAV multistatus response.
type multistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string        `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ResourceType davResourceType `xml:"resourcetype"`
	DisplayName  string          `xml:"displayname"`
	GetETag      string          `xml:"getetag"`
	AddressData  string          `xml:"address-data"`
	Principal    davHref         `xml:"current-user-principal"`
	HomeSet      davHref         `xml:"addressbook-home-set"`
}

type davResourceType struct {
	AddressBook *struct{} `xml:"addressbook"`
	Collection  *struct{} `xml:"collection"`
}

type davHref struct {
	Href string `xml:"href"`
}

// extractPropValue extracts a property href from a multistatus response.
func extractPropValue(data []byte, propName string) string {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		return ""
	}

	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			switch propName {
			case "current-user-principal":
				if ps.Prop.Principal.Href != "" {
					return ps.Prop.Principal.Href
				}
			case "addressbook-home-set":
				if ps.Prop.HomeSet.Href != "" {
					return ps.Prop.HomeSet.Href
				}
			}
		}
	}
	return ""
}

// parseAddressBookList extracts address book URLs from a PROPFIND response.
func (c *cloudContactsClient) parseAddressBookList(data []byte, homeSetURL string, log zerolog.Logger) []string {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		log.Warn().Err(err).Msg("CardDAV: failed to parse address book list XML")
		return nil
	}

	var addressBooks []string
	for _, resp := range ms.Responses {
		href := c.resolveURL(resp.Href)
		// Skip the home set itself
		if href == homeSetURL || resp.Href == homeSetURL {
			continue
		}
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			if ps.Prop.ResourceType.AddressBook != nil {
				log.Debug().
					Str("address_book_host", logSafeURL(href)).
					Str("name", ps.Prop.DisplayName).
					Msg("CardDAV: found address book")
				addressBooks = append(addressBooks, href)
			}
		}
	}

	// Fallback: if no address books found with proper resourcetype,
	// try the default Apple path
	if len(addressBooks) == 0 {
		defaultURL := c.baseURL + "/" + c.dsid + "/carddavhome/card/"
		log.Debug().Str("url_host", logSafeURL(defaultURL)).Msg("CardDAV: no address books found via PROPFIND, trying default path")
		addressBooks = append(addressBooks, defaultURL)
	}

	return addressBooks
}

// parseVCardMultistatus extracts contacts from a REPORT multistatus response.
func (c *cloudContactsClient) parseVCardMultistatus(data []byte, log zerolog.Logger) []*imessage.Contact {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		log.Warn().Err(err).Msg("CardDAV: failed to parse REPORT XML")
		return nil
	}

	var contacts []*imessage.Contact
	skippedNoData := 0
	skippedNoInfo := 0
	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			vcardData := strings.TrimSpace(ps.Prop.AddressData)
			if vcardData == "" {
				skippedNoData++
				continue
			}
			contact := parseVCard(vcardData)
			if contact != nil && (contact.HasName() || len(contact.Phones) > 0 || len(contact.Emails) > 0) {
				contacts = append(contacts, contact)
			} else {
				skippedNoInfo++
			}
		}
	}
	log.Debug().
		Int("responses", len(ms.Responses)).
		Int("parsed", len(contacts)).
		Int("skipped_no_data", skippedNoData).
		Int("skipped_no_info", skippedNoInfo).
		Msg("CardDAV REPORT parsing stats")
	return contacts
}

// ============================================================================
// vCard Parser
// ============================================================================

// parseVCard parses a vCard string into a Contact struct.
// Handles vCard 3.0 and 4.0 format, including folded lines and quoted-printable.
func parseVCard(vcardData string) *imessage.Contact {
	contact := &imessage.Contact{}

	// Unfold continuation lines (RFC 6350 §3.2): a line starting with a
	// space or tab is a continuation of the previous logical line.
	vcardData = strings.ReplaceAll(vcardData, "\r\n ", "")
	vcardData = strings.ReplaceAll(vcardData, "\r\n\t", "")
	vcardData = strings.ReplaceAll(vcardData, "\n ", "")
	vcardData = strings.ReplaceAll(vcardData, "\n\t", "")

	lines := strings.Split(vcardData, "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		// Split into property name (with params) and value
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		nameWithParams := line[:colonIdx]
		value := line[colonIdx+1:]

		// Extract property name (before any ;parameters)
		propName := nameWithParams
		if semiIdx := strings.Index(nameWithParams, ";"); semiIdx >= 0 {
			propName = nameWithParams[:semiIdx]
		}
		// Strip Apple vCard group prefix (e.g. "item1.TEL" → "TEL")
		if dotIdx := strings.Index(propName, "."); dotIdx >= 0 {
			propName = propName[dotIdx+1:]
		}
		propName = strings.ToUpper(propName)

		switch propName {
		case "N":
			// N:Last;First;Middle;Prefix;Suffix
			parts := strings.Split(value, ";")
			if len(parts) >= 1 {
				contact.LastName = decodeVCardValue(parts[0])
			}
			if len(parts) >= 2 {
				contact.FirstName = decodeVCardValue(parts[1])
			}
		case "FN":
			// Full formatted name — use as fallback if N didn't provide names
			if contact.FirstName == "" && contact.LastName == "" {
				fn := decodeVCardValue(value)
				parts := strings.SplitN(fn, " ", 2)
				if len(parts) == 2 {
					contact.FirstName = parts[0]
					contact.LastName = parts[1]
				} else if len(parts) == 1 {
					contact.FirstName = parts[0]
				}
			}
		case "NICKNAME":
			contact.Nickname = decodeVCardValue(value)
		case "TEL":
			phone := decodeVCardValue(value)
			// Strip tel: URI prefix if present (vCard 4.0)
			phone = strings.TrimPrefix(phone, "tel:")
			phone = strings.TrimSpace(phone)
			if phone != "" {
				contact.Phones = append(contact.Phones, phone)
			}
		case "EMAIL":
			email := decodeVCardValue(value)
			email = strings.TrimSpace(email)
			if email != "" {
				contact.Emails = append(contact.Emails, email)
			}
		case "PHOTO":
			// Try to extract inline base64 photo data, or capture URL for deferred download
			photo, photoURL := extractVCardPhoto(nameWithParams, value)
			if photo != nil {
				contact.Avatar = photo
			} else if photoURL != "" {
				contact.AvatarURL = photoURL
			}
		}
	}

	return contact
}

// decodeVCardValue handles basic vCard value decoding (escaped characters).
func decodeVCardValue(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\N", "\n")
	s = strings.ReplaceAll(s, "\\,", ",")
	s = strings.ReplaceAll(s, "\\;", ";")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return strings.TrimSpace(s)
}

// extractVCardPhoto tries to decode a base64-encoded PHOTO value.
// Returns (photoBytes, photoURL). If the photo is a URL reference,
// photoBytes is nil and photoURL is set for deferred download.
func extractVCardPhoto(nameWithParams, value string) ([]byte, string) {
	params := strings.ToUpper(nameWithParams)
	// Check for base64 encoding (vCard 3.0: ENCODING=b or ENCODING=BASE64)
	if !strings.Contains(params, "ENCODING=B") && !strings.Contains(params, "ENCODING=BASE64") {
		// vCard 4.0 uses data: URIs
		if strings.HasPrefix(value, "data:") {
			// data:image/jpeg;base64,/9j/4AAQ...
			if idx := strings.Index(value, ","); idx >= 0 {
				value = value[idx+1:]
			} else {
				return nil, ""
			}
		} else if strings.HasPrefix(value, "http") {
			// URL reference — return for deferred download
			return nil, value
		} else {
			// Assume base64 if it looks like it
			if len(value) < 100 {
				return nil, ""
			}
		}
	}

	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, ""
	}
	return data, ""
}

// downloadAuthURL fetches a URL using iCloud auth headers.
func (c *cloudContactsClient) downloadAuthURL(ctx context.Context, targetURL string) ([]byte, error) {
	resp, err := c.doRequest("GET", targetURL, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, logSafeURL(targetURL))
	}
	return io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
}

// photoFetcher downloads a URL with authentication. Nil means use generic unauthenticated fetch.
type photoFetcher func(ctx context.Context, url string) ([]byte, error)

// downloadContactPhotos downloads photo URLs for contacts that have AvatarURL
// set but no Avatar bytes. Uses bounded concurrency to avoid overwhelming
// the server. If an authenticated fetcher is provided, iCloud URLs use it;
// all other URLs use the generic unauthenticated downloader.
func downloadContactPhotos(contacts []*imessage.Contact, log zerolog.Logger, authFetch ...photoFetcher) {
	var needsDownload []*imessage.Contact
	for _, c := range contacts {
		if c.AvatarURL != "" && c.Avatar == nil {
			needsDownload = append(needsDownload, c)
		}
	}
	if len(needsDownload) == 0 {
		return
	}

	log.Info().Int("count", len(needsDownload)).Msg("Downloading contact photo URLs")

	var authDL photoFetcher
	if len(authFetch) > 0 {
		authDL = authFetch[0]
	}

	const maxConcurrency = 10
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, contact := range needsDownload {
		wg.Add(1)
		go func(c *imessage.Contact) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var data []byte
			var err error
			if authDL != nil && (strings.Contains(c.AvatarURL, ".icloud.com") || strings.Contains(c.AvatarURL, ".apple.com")) {
				data, err = authDL(ctx, c.AvatarURL)
			} else {
				data, _, err = downloadURL(ctx, c.AvatarURL)
			}
			if err != nil {
				log.Debug().Err(sanitizeURLError(err, c.AvatarURL)).
					Str("name", c.Name()).
					Str("url_host", logSafeURL(c.AvatarURL)).
					Msg("Failed to download contact photo URL")
				return
			}
			if len(data) > 0 {
				c.Avatar = data
				c.AvatarURL = ""
			}
		}(contact)
	}

	wg.Wait()

	downloaded := 0
	for _, c := range needsDownload {
		if c.Avatar != nil {
			downloaded++
		}
	}
	log.Info().
		Int("attempted", len(needsDownload)).
		Int("downloaded", downloaded).
		Msg("Contact photo URL downloads complete")
}
