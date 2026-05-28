package connector

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// sanitizeURLError returns err with every occurrence of the given URLs in
// the error message replaced by their scheme+host form. Go's net/http wraps
// transport failures in *url.Error whose Error() embeds the full URL
// (e.g. `Get "https://x.com/secret/path": dial tcp …`), so .Err(err) re-leaks
// the path/query that logSafeURL was meant to strip from the same log line.
func sanitizeURLError(err error, urls ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	original := msg
	for _, u := range urls {
		if u == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, u, logSafeURL(u))
	}
	if msg == original {
		return err
	}
	return errors.New(msg)
}

// logSafeURL returns a scheme+host representation of a URL for log fields, so
// the path and query (which carry user-content references like article slugs
// or search terms) don't end up in logs. Falls back to "url:redacted" when
// the URL is unparseable.
func logSafeURL(u string) string {
	if u == "" {
		return ""
	}
	if debugDisablePrivacy {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return "url:redacted"
	}
	if parsed.Scheme == "" {
		return parsed.Host
	}
	return parsed.Scheme + "://" + parsed.Host
}

// metaTagRegex generically matches all <meta> tags with property/name and content
// attributes, in either attribute order. Captures the full attribute name (including
// any namespace prefix like og:, twitter:, etc.) and the content value.
// Double-quoted values only to avoid apostrophe truncation.
var metaTagRegex = regexp.MustCompile(
	`(?i)<meta\s+[^>]*(?:property|name)="([^"]+)"[^>]*content="([^"]*)"[^>]*/?\s*>` +
		`|` +
		`(?i)<meta\s+[^>]*content="([^"]*)"[^>]*(?:property|name)="([^"]+)"[^>]*/?\s*>`,
)

// titleRegex extracts the <title> tag content as a last-resort fallback.
var titleRegex = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)

// fetchURLPreview builds a BeeperLinkPreview by fetching the target URL's
// metadata. It tries the homeserver's /preview_url first, then falls back to
// fetching the HTML and parsing meta tags directly (og:, twitter:, standard
// HTML meta, <title>). If an image is found, it is downloaded and uploaded
// via the provided intent.
func fetchURLPreview(ctx context.Context, bridge *bridgev2.Bridge, intent bridgev2.MatrixAPI, roomID id.RoomID, targetURL string) *event.BeeperLinkPreview {
	log := zerolog.Ctx(ctx)

	fetchURL := normalizeURL(targetURL)

	preview := &event.BeeperLinkPreview{
		MatchedURL: targetURL,
		LinkPreview: event.LinkPreview{
			CanonicalURL: fetchURL,
			Title:        targetURL,
		},
	}

	// Try homeserver preview first
	if mc, ok := bridge.Matrix.(bridgev2.MatrixConnectorWithURLPreviews); ok {
		lp, err := mc.GetURLPreview(ctx, fetchURL)
		if err != nil {
			log.Debug().Err(sanitizeURLError(err, fetchURL, targetURL)).Str("url_host", logSafeURL(fetchURL)).Msg("Homeserver URL preview failed, falling back to meta scraping")
		}
		if err == nil && lp != nil {
			preview.LinkPreview = *lp
			if preview.CanonicalURL == "" {
				preview.CanonicalURL = targetURL
			}
			if preview.Title == "" {
				preview.Title = targetURL
			}
			// If homeserver already returned an image, we're done
			if preview.ImageURL != "" {
				return preview
			}
		}
	}

	// Fetch the page ourselves and parse metadata from all meta tag formats
	meta := fetchPageMetadata(ctx, fetchURL)
	if meta["title"] != "" && preview.Title == targetURL {
		preview.Title = meta["title"]
	}
	if meta["description"] != "" && preview.Description == "" {
		preview.Description = meta["description"]
	}

	// Download and upload image
	imageURL := meta["image"]
	if imageURL == "" {
		imageURL = meta["image:secure_url"]
	}
	if imageURL != "" && intent != nil {
		// Resolve relative URLs against the normalized (https://) URL
		if !strings.HasPrefix(imageURL, "http") {
			if base, err := url.Parse(fetchURL); err == nil {
				if ref, err := url.Parse(imageURL); err == nil {
					imageURL = base.ResolveReference(ref).String()
				}
			}
		}
		data, mime, err := downloadURL(ctx, imageURL)
		if err == nil && len(data) > 0 {
			mxcURL, encFile, err := intent.UploadMedia(ctx, roomID, data, "preview", mime)
			if err == nil {
				if encFile != nil {
					preview.ImageEncryption = encFile
					preview.ImageURL = encFile.URL
				} else {
					preview.ImageURL = mxcURL
				}
				preview.ImageType = mime
				preview.ImageSize = event.IntOrString(len(data))
				log.Debug().Str("image_url_host", logSafeURL(imageURL)).Msg("Uploaded URL preview image")
			} else {
				log.Debug().Err(err).Msg("Failed to upload URL preview image")
			}
		} else if err != nil {
			log.Debug().Err(sanitizeURLError(err, imageURL)).Str("image_url_host", logSafeURL(imageURL)).Msg("Failed to download URL preview image")
		}
	}

	return preview
}

// userAgents lists User-Agent strings to try when fetching page metadata.
// Many modern sites (JS SPAs) only serve meta tags to recognized social media
// crawlers — regular browser UAs get a JS shell with no metadata. We lead
// with crawler UAs that are universally whitelisted for link unfurling, then
// fall back to browser UAs for traditional sites.
//
//  1. facebookexternalhit — most widely whitelisted crawler; virtually every
//     site serves og: tags to Facebook's crawler for link unfurling.
//  2. WhatsApp — also very widely whitelisted for the same reason.
//  3. iPhone Safari — for sites that serve metadata to real mobile browsers.
//  4. Googlebot — last resort; some sites verify by reverse DNS but many don't.
var userAgents = []string{
	"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatype.html)",
	"WhatsApp/2.23.2 A",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
	"Googlebot/2.1 (+http://www.google.com/bot.html)",
}

// fetchPageMetadata fetches a URL and extracts preview metadata from the HTML.
// It tries multiple User-Agent strings and returns a unified map with keys
// "title", "description", "image", "image:secure_url" resolved from all meta
// tag formats (og:, twitter:, standard HTML meta, <title> fallback).
func fetchPageMetadata(ctx context.Context, targetURL string) map[string]string {
	log := zerolog.Ctx(ctx)

	for _, ua := range userAgents {
		result := fetchPageMetadataWithUA(ctx, targetURL, ua)
		if result["image"] != "" || result["title"] != "" {
			log.Debug().Str("ua", ua).Str("url_host", logSafeURL(targetURL)).
				Bool("has_title", result["title"] != "").
				Bool("has_image", result["image"] != "").Msg("Found page metadata")
			return result
		}
		log.Debug().Str("ua", ua).Str("url_host", logSafeURL(targetURL)).Msg("No metadata found with this UA, trying next")
	}

	return make(map[string]string)
}

// ogHTTPClient uses TLS 1.3 to avoid bot detection via TLS fingerprinting.
// Sites like Reddit reject Go's default TLS 1.2 handshake with HTTP 403.
var ogHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			MaxVersion: tls.VersionTLS13,
		},
	},
}

// fetchPageMetadataWithUA fetches a URL with a specific User-Agent and extracts
// metadata from ALL meta tag formats. It parses every <meta> tag generically,
// then resolves the best title/description/image using a priority cascade:
//
//	og: > twitter: > standard HTML meta (name="description") > <title> tag
//
// This ensures we get metadata from any site regardless of which meta tag
// convention it uses.
func fetchPageMetadataWithUA(ctx context.Context, targetURL string, ua string) map[string]string {
	result := make(map[string]string)
	log := zerolog.Ctx(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return result
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Note: Do NOT set Accept-Encoding manually. Go's Transport automatically
	// handles gzip and transparently decompresses. Setting it explicitly
	// disables auto-decompression, causing us to read raw gzip bytes.

	resp, err := ogHTTPClient.Do(req)
	if err != nil {
		log.Debug().Err(sanitizeURLError(err, targetURL)).Str("url_host", logSafeURL(targetURL)).Str("ua", ua).Msg("HTTP request failed for meta scraping")
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Debug().Int("status", resp.StatusCode).Str("url_host", logSafeURL(targetURL)).Str("ua", ua).Msg("HTTP error for meta scraping")
		return result
	}

	// Read first 512KB — meta tags should be in <head> but some sites
	// inline hundreds of KB of CSS/JS before their meta tags.
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if len(data) == 0 {
		return result
	}
	log.Debug().Int("status", resp.StatusCode).Int("body_bytes", len(data)).Str("url_host", logSafeURL(targetURL)).Str("ua", ua).Msg("Fetched page for meta scraping")
	htmlStr := string(data)

	// Parse ALL <meta> tags generically into namespaced buckets.
	// e.g. "og:title" -> ogTags["title"], "twitter:image" -> twitterTags["image"],
	//       "description" -> stdTags["description"]
	ogTags := make(map[string]string)
	twitterTags := make(map[string]string)
	stdTags := make(map[string]string)

	for _, match := range metaTagRegex.FindAllStringSubmatch(htmlStr, -1) {
		var name, content string
		if match[1] != "" {
			name, content = match[1], match[2]
		} else {
			name, content = match[4], match[3]
		}
		name = strings.ToLower(name)
		content = html.UnescapeString(content)

		switch {
		case strings.HasPrefix(name, "og:"):
			key := name[3:] // strip "og:" prefix
			if _, exists := ogTags[key]; !exists {
				ogTags[key] = content
			}
		case strings.HasPrefix(name, "twitter:"):
			key := name[8:] // strip "twitter:" prefix
			if _, exists := twitterTags[key]; !exists {
				twitterTags[key] = content
			}
		default:
			if _, exists := stdTags[name]; !exists {
				stdTags[name] = content
			}
		}
	}

	// Resolve with priority cascade: og: > twitter: > standard meta
	// Title
	switch {
	case ogTags["title"] != "":
		result["title"] = ogTags["title"]
	case twitterTags["title"] != "":
		result["title"] = twitterTags["title"]
	case stdTags["title"] != "":
		result["title"] = stdTags["title"]
	}

	// Description
	switch {
	case ogTags["description"] != "":
		result["description"] = ogTags["description"]
	case twitterTags["description"] != "":
		result["description"] = twitterTags["description"]
	case stdTags["description"] != "":
		result["description"] = stdTags["description"]
	}

	// Image
	switch {
	case ogTags["image"] != "":
		result["image"] = ogTags["image"]
	case ogTags["image:secure_url"] != "":
		result["image"] = ogTags["image:secure_url"]
	case twitterTags["image"] != "":
		result["image"] = twitterTags["image"]
	case twitterTags["image:src"] != "":
		result["image"] = twitterTags["image:src"]
	}

	// Also expose image:secure_url if present (used by fetchURLPreview)
	if ogTags["image:secure_url"] != "" {
		result["image:secure_url"] = ogTags["image:secure_url"]
	}

	// Fall back to <title> tag if no title was found from any meta tags
	if result["title"] == "" {
		if m := titleRegex.FindStringSubmatch(htmlStr); m != nil {
			title := strings.TrimSpace(html.UnescapeString(m[1]))
			if title != "" {
				result["title"] = title
			}
		}
	}

	return result
}

// downloadURL fetches a URL and returns the body bytes and content type.
// Uses the shared TLS 1.3 client to avoid bot detection.
func downloadURL(ctx context.Context, targetURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15")

	resp, err := ogHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, logSafeURL(targetURL))
	}

	// Limit to 5MB
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
	if err != nil {
		return nil, "", err
	}

	mime := resp.Header.Get("Content-Type")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	if mime == "" {
		mime = "image/jpeg"
	}

	return data, mime, nil
}
