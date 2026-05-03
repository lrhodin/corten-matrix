// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// CLI subcommand for setting up external CardDAV contacts.
// Auto-discovers the CardDAV URL and encrypts the password.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog"

	"github.com/lrhodin/imessage/pkg/connector"
)

// cardDAVSetupResult is the JSON output of the carddav-setup command.
type cardDAVSetupResult struct {
	URL               string `json:"url"`
	PasswordEncrypted string `json:"password_encrypted"`
}

// runCardDAVSetup handles the carddav-setup subcommand.
// Discovers the CardDAV URL and encrypts the password.
// Outputs JSON to stdout for install scripts to parse.
func runCardDAVSetup() {
	fs := flag.NewFlagSet("carddav-setup", flag.ExitOnError)
	email := fs.String("email", "", "Email address for CardDAV auto-discovery")
	password := fs.String("password", "", "App password for CardDAV authentication")
	username := fs.String("username", "", "Username (defaults to email if empty)")
	url := fs.String("url", "", "CardDAV URL (skip auto-discovery)")
	stripSpaces := fs.Bool("strip-spaces", false, "Strip spaces from password (for Google app passwords)")

	fs.Parse(os.Args[2:])

	if *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "Usage: carddav-setup --email <email> --password <password> [--username <user>] [--url <url>] [--strip-spaces]")
		os.Exit(1)
	}

	cleanPassword := *password
	if *stripSpaces {
		// Google app passwords are displayed with spaces (e.g. "abcd efgh ijkl mnop").
		// Strip them so users can paste as-is without auth failures.
		cleanPassword = strings.ReplaceAll(cleanPassword, " ", "")
	}

	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()

	effectiveUsername := *username
	if effectiveUsername == "" {
		effectiveUsername = *email
	}

	// Auto-discover URL if not provided
	discoveredURL := *url
	if discoveredURL == "" {
		var err error
		discoveredURL, err = connector.DiscoverCardDAVURL(*email, effectiveUsername, cleanPassword, log)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: CardDAV auto-discovery failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "✓ Discovered CardDAV URL: %s\n", discoveredURL)
	}

	// Encrypt the password
	encrypted, err := connector.EncryptCardDAVPassword(cleanPassword)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to encrypt password: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "✓ Password encrypted")

	// Output JSON to stdout
	result := cardDAVSetupResult{
		URL:               discoveredURL,
		PasswordEncrypted: encrypted,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
}
