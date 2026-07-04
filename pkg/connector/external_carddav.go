// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// External CardDAV contact source for non-iCloud servers (Google, Nextcloud,
// Radicale, Fastmail, etc.). Uses HTTP Basic auth and standard CardDAV
// protocol. Reuses the vCard parser and XML types from cloud_contacts.go.

package connector

import (
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/imessage"
)

// externalCardDAVClient fetches contacts from an external CardDAV server
// (Google, Nextcloud, Fastmail, etc.) using HTTP Basic authentication.
type externalCardDAVClient struct {
	baseURL    string // CardDAV server URL (discovered or manual)
	username   string
	password   string
	httpClient *http.Client

	mu       sync.RWMutex
	byPhone  map[string]*imessage.Contact
	byEmail  map[string]*imessage.Contact
	contacts []*imessage.Contact
	lastSync time.Time
}

// newExternalCardDAVClient creates an external CardDAV client.
// Performs auto-discovery if url is empty. Returns nil if configuration is insufficient.
func newExternalCardDAVClient(cfg CardDAVConfig, log zerolog.Logger) *externalCardDAVClient {
	if !cfg.IsConfigured() {
		return nil
	}

	// Decrypt password
	password, err := DecryptCardDAVPassword(cfg.PasswordEncrypted)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to decrypt CardDAV password")
		return nil
	}

	username := cfg.GetUsername()
	url := cfg.URL

	// Auto-discover if no URL provided
	if url == "" {
		discovered, err := DiscoverCardDAVURL(cfg.Email, username, password, log)
		if err != nil {
			log.Warn().Err(err).Str("email", logSafeHandle(cfg.Email)).Msg("CardDAV auto-discovery failed")
			return nil
		}
		url = discovered
		log.Info().Str("url_host", logSafeURL(url)).Msg("CardDAV URL auto-discovered")
	}

	return &externalCardDAVClient{
		baseURL:  strings.TrimRight(url, "/"),
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		byPhone: make(map[string]*imessage.Contact),
		byEmail: make(map[string]*imessage.Contact),
	}
}

// doRequest performs an authenticated CardDAV request with HTTP Basic auth.
func (c *externalCardDAVClient) doRequest(method, url, body string, depth string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	if depth != "" {
		req.Header.Set("Depth", depth)
	}
	return c.httpClient.Do(req)
}

// SyncContacts fetches all contacts from the external CardDAV server.
func (c *externalCardDAVClient) SyncContacts(log zerolog.Logger) error {
	// Step 1: Discover principal
	principalURL, err := c.discoverPrincipal(log)
	if err != nil {
		return fmt.Errorf("discover principal: %w", sanitizeURLError(err, c.baseURL+"/"))
	}
	log.Debug().Str("principal_host", logSafeURL(principalURL)).Msg("External CardDAV: discovered principal")

	// Step 2: Get address book home set
	homeSetURL, err := c.discoverAddressBookHome(log, principalURL)
	if err != nil {
		return fmt.Errorf("discover address book home: %w", sanitizeURLError(err, principalURL))
	}
	log.Debug().Str("home_set_host", logSafeURL(homeSetURL)).Msg("External CardDAV: discovered address book home")

	// Step 3: List address books
	addressBooks, err := c.listAddressBooks(log, homeSetURL)
	if err != nil {
		return fmt.Errorf("list address books: %w", sanitizeURLError(err, homeSetURL))
	}
	log.Debug().Int("count", len(addressBooks)).Msg("External CardDAV: found address books")

	// Step 4: Fetch all vCards
	var allContacts []*imessage.Contact
	for _, abURL := range addressBooks {
		contacts, fetchErr := c.fetchAllVCards(log, abURL)
		if fetchErr != nil {
			log.Warn().Err(sanitizeURLError(fetchErr, abURL)).Str("address_book_host", logSafeURL(abURL)).Msg("External CardDAV: failed to fetch vCards")
			continue
		}
		allContacts = append(allContacts, contacts...)
	}

	// Step 4.5: Download any photo URLs (e.g. Google uses URL-based PHOTO fields)
	downloadContactPhotos(allContacts, log)

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
		Msg("Contact cache synced from external CardDAV")
	return nil
}

// GetContactInfo looks up a contact by phone number or email.
func (c *externalCardDAVClient) GetContactInfo(identifier string) (*imessage.Contact, error) {
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
func (c *externalCardDAVClient) GetAllContacts() []*imessage.Contact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*imessage.Contact, len(c.contacts))
	copy(result, c.contacts)
	return result
}

// ============================================================================
// CardDAV Protocol (same as cloud_contacts.go but with Basic auth)
// ============================================================================

func (c *externalCardDAVClient) discoverPrincipal(log zerolog.Logger) (string, error) {
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
		log.Debug().Int("body_bytes", len(data)).Msg("External CardDAV: PROPFIND response (no principal)")
		return "", fmt.Errorf("no current-user-principal in response")
	}

	return c.resolveURL(href), nil
}

func (c *externalCardDAVClient) discoverAddressBookHome(log zerolog.Logger, principalURL string) (string, error) {
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
		return "", fmt.Errorf("no addressbook-home-set in response")
	}

	return c.resolveURL(href), nil
}

func (c *externalCardDAVClient) listAddressBooks(log zerolog.Logger, homeSetURL string) ([]string, error) {
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

	return c.parseAddressBookList(data, homeSetURL, log), nil
}

func (c *externalCardDAVClient) fetchAllVCards(log zerolog.Logger, addressBookURL string) ([]*imessage.Contact, error) {
	body := `<?xml version="1.0" encoding="UTF-8"?>
<card:addressbook-query xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:getetag/>
    <card:address-data/>
  </d:prop>
  <card:filter>
    <card:prop-filter name="FN"/>
  </card:filter>
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
		Int("response_bytes", len(data)).
		Str("address_book_host", logSafeURL(addressBookURL)).
		Msg("External CardDAV: REPORT response received")

	return parseVCardMultistatusStandalone(data, log), nil
}

// resolveURL converts a relative href to an absolute URL.
func (c *externalCardDAVClient) resolveURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
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

// parseAddressBookList extracts address book URLs from a PROPFIND response.
func (c *externalCardDAVClient) parseAddressBookList(data []byte, homeSetURL string, log zerolog.Logger) []string {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		log.Warn().Err(err).Msg("External CardDAV: failed to parse address book list")
		return nil
	}

	var addressBooks []string
	for _, resp := range ms.Responses {
		href := c.resolveURL(resp.Href)
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
					Msg("External CardDAV: found address book")
				addressBooks = append(addressBooks, href)
			}
		}
	}
	return addressBooks
}

// parseVCardMultistatusStandalone extracts contacts from a REPORT multistatus
// response. Standalone version that doesn't depend on cloudContactsClient.
func parseVCardMultistatusStandalone(data []byte, log zerolog.Logger) []*imessage.Contact {
	var ms multistatus
	if err := xml.Unmarshal(data, &ms); err != nil {
		log.Warn().Err(err).Msg("External CardDAV: failed to parse REPORT XML")
		return nil
	}

	var contacts []*imessage.Contact
	for _, resp := range ms.Responses {
		for _, ps := range resp.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			vcardData := strings.TrimSpace(ps.Prop.AddressData)
			if vcardData == "" {
				continue
			}
			contact := parseVCard(vcardData)
			if contact != nil && (contact.HasName() || len(contact.Phones) > 0 || len(contact.Emails) > 0) {
				contacts = append(contacts, contact)
			}
		}
	}
	return contacts
}

// ============================================================================
// CardDAV Auto-Discovery (RFC 6764) + Known Providers
// ============================================================================

// KnownCardDAVProvider maps a provider name to its URL pattern.
// {email} is replaced with the user's email address.
type KnownCardDAVProvider struct {
	Name    string
	Domains []string // email domains that match this provider
	URL     string   // URL pattern ({email} replaced)
}

// KnownProviders lists well-known CardDAV providers with their URL patterns.
var KnownProviders = []KnownCardDAVProvider{
	{
		Name:    "Google",
		Domains: []string{"gmail.com", "googlemail.com"},
		URL:     "https://www.googleapis.com/carddav/v1/principals/{email}/lists/default/",
	},
	{
		Name:    "Fastmail",
		Domains: []string{"fastmail.com", "fastmail.fm", "messagingengine.com"},
		URL:     "https://carddav.fastmail.com/dav/addressbooks/user/{email}/Default/",
	},
	{
		Name:    "iCloud",
		Domains: []string{"icloud.com", "me.com", "mac.com"},
		URL:     "https://contacts.icloud.com",
	},
	{
		Name:    "Yahoo",
		Domains: []string{"yahoo.com", "yahoo.co.uk", "ymail.com"},
		URL:     "https://carddav.address.yahoo.com/dav/{email}/",
	},
}

// ResolveProviderURL returns the CardDAV URL for a known provider, or empty string.
func ResolveProviderURL(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	domain := strings.ToLower(parts[1])

	for _, p := range KnownProviders {
		for _, d := range p.Domains {
			if domain == d {
				return strings.ReplaceAll(p.URL, "{email}", email)
			}
		}
	}
	// Google Workspace: any domain could be Google-hosted
	// but we can't detect that — return empty, let auto-discovery try
	return ""
}

// DiscoverCardDAVURL attempts to find the CardDAV server URL for an email address.
// Tries in order: known providers, .well-known/carddav (GET + PROPFIND), DNS SRV.
func DiscoverCardDAVURL(email, username, password string, log zerolog.Logger) (string, error) {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("invalid email address: %s", email)
	}
	domain := parts[1]

	// Try known providers first
	if url := ResolveProviderURL(email); url != "" {
		log.Info().Str("url_host", logSafeURL(url)).Msg("CardDAV URL resolved from known provider")
		return url, nil
	}

	// Try .well-known/carddav (HTTPS)
	wellKnownURL := fmt.Sprintf("https://%s/.well-known/carddav", domain)
	if url, err := tryWellKnown(wellKnownURL, username, password, log); err == nil {
		return url, nil
	}

	// Try HTTP fallback
	wellKnownURL = fmt.Sprintf("http://%s/.well-known/carddav", domain)
	if url, err := tryWellKnown(wellKnownURL, username, password, log); err == nil {
		return url, nil
	}

	// Try DNS SRV records
	if url, err := trySRVDiscovery(domain, log); err == nil {
		return url, nil
	}

	return "", fmt.Errorf("CardDAV auto-discovery failed for %s (tried known providers, .well-known, and SRV)", domain)
}

// tryWellKnown attempts CardDAV discovery via .well-known/carddav.
// Tries GET first (most servers redirect), then PROPFIND.
func tryWellKnown(wellKnownURL, username, password string, log zerolog.Logger) (string, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Try GET first — most servers return a redirect
	for _, method := range []string{"GET", "PROPFIND"} {
		req, err := http.NewRequest(method, wellKnownURL, nil)
		if err != nil {
			continue
		}
		req.SetBasicAuth(username, password)
		if method == "PROPFIND" {
			req.Header.Set("Depth", "0")
			req.Header.Set("Content-Type", "application/xml; charset=utf-8")
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Debug().Err(sanitizeURLError(err, wellKnownURL)).Str("method", method).Str("url_host", logSafeURL(wellKnownURL)).Msg("CardDAV .well-known request failed")
			continue
		}
		resp.Body.Close()

		// 3xx redirect: follow the Location header
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			if location != "" {
				log.Debug().Str("location_host", logSafeURL(location)).Str("method", method).Msg("CardDAV .well-known redirected")
				return resolveRedirect(wellKnownURL, location), nil
			}
		}

		// 207 Multi-Status: the URL itself is the CardDAV endpoint
		if resp.StatusCode == 207 {
			return wellKnownURL, nil
		}
	}

	return "", fmt.Errorf(".well-known not available at %s", logSafeURL(wellKnownURL))
}

// resolveRedirect resolves a potentially relative redirect Location.
func resolveRedirect(baseURL, location string) string {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location
	}
	if strings.HasPrefix(location, "/") {
		if idx := strings.Index(baseURL, "://"); idx >= 0 {
			rest := baseURL[idx+3:]
			if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
				return baseURL[:idx+3+slashIdx] + location
			}
		}
	}
	return location
}

// trySRVDiscovery looks up _carddavs._tcp and _carddav._tcp SRV records.
func trySRVDiscovery(domain string, log zerolog.Logger) (string, error) {
	// Try TLS first
	_, addrs, err := net.LookupSRV("carddavs", "tcp", domain)
	if err == nil && len(addrs) > 0 {
		target := strings.TrimRight(addrs[0].Target, ".")
		port := addrs[0].Port
		url := fmt.Sprintf("https://%s:%d", target, port)
		log.Debug().Str("url_host", logSafeURL(url)).Msg("CardDAV SRV record found (_carddavs._tcp)")
		return url, nil
	}

	// Fall back to plain
	_, addrs, err = net.LookupSRV("carddav", "tcp", domain)
	if err == nil && len(addrs) > 0 {
		target := strings.TrimRight(addrs[0].Target, ".")
		port := addrs[0].Port
		url := fmt.Sprintf("http://%s:%d", target, port)
		log.Debug().Str("url_host", logSafeURL(url)).Msg("CardDAV SRV record found (_carddav._tcp)")
		return url, nil
	}

	return "", fmt.Errorf("no CardDAV SRV records for %s", domain)
}
