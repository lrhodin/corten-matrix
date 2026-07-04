// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
	"maunium.net/go/mautrix/id"
)

// Global reader so buffered input isn't lost between prompts (important when
// input is piped rather than typed interactively).
var stdinReader = bufio.NewReader(os.Stdin)

func prompt(label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	line, _ := stdinReader.ReadString('\n')
	return sanitizeInput(line)
}

// sanitizeInput strips terminal bracketed-paste markers (ESC[200~ … ESC[201~)
// and any control characters a paste/terminal can inject but that are never part
// of a typed Apple ID or password, then trims surrounding whitespace. Stray
// bytes here corrupt the SRP proof and surface as a misleading GSA -22406
// ("wrong password"). It reports to stderr whenever it removes anything, so a
// failed login tells us the input was the problem.
func sanitizeInput(s string) string {
	hadPaste := strings.Contains(s, "\x1b[200~") || strings.Contains(s, "\x1b[201~")
	s = strings.ReplaceAll(s, "\x1b[200~", "")
	s = strings.ReplaceAll(s, "\x1b[201~", "")
	var b strings.Builder
	ctrl := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			if r != '\n' && r != '\r' && r != '\t' {
				ctrl++
			}
			continue
		}
		b.WriteRune(r)
	}
	if hadPaste || ctrl > 0 {
		fmt.Fprintf(os.Stderr, "  [input sanitized: paste-markers=%v, control-bytes-removed=%d]\n", hadPaste, ctrl)
	}
	return strings.TrimSpace(b.String())
}

// promptSelect displays numbered options and returns the selected value.
func promptSelect(label string, options []string) string {
	fmt.Fprintf(os.Stderr, "%s:\n", label)
	for i, opt := range options {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, opt)
	}
	for {
		fmt.Fprintf(os.Stderr, "Enter number (1-%d): ", len(options))
		line, _ := stdinReader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		var idx int
		if _, err := fmt.Sscanf(trimmed, "%d", &idx); err == nil && idx >= 1 && idx <= len(options) {
			return options[idx-1]
		}
		// Also accept the option value directly
		for _, opt := range options {
			if strings.EqualFold(trimmed, opt) {
				return opt
			}
		}
		fmt.Fprintf(os.Stderr, "  Invalid choice, try again.\n")
	}
}

// promptMultiline reads lines until an empty line, concatenating and stripping
// all whitespace. Used for fields like hardware keys that are long base64
// strings which get split across lines when pasted.
func promptMultiline(label string) string {
	fmt.Fprintf(os.Stderr, "%s (paste, then press Enter on a blank line):\n", label)
	var parts []string
	for {
		line, _ := stdinReader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, "")
}

// runInteractiveLogin drives the bridge's login flow from the terminal.
// It reuses the exact same CreateLogin → SubmitUserInput code path as the
// Matrix bot, but reads input from stdin instead of Matrix messages.
func runInteractiveLogin(br *mxmain.BridgeMain) {
	// Initialize the bridge (DB, connector, etc.) without starting Matrix.
	br.PreInit()
	repairPermissions(br)
	br.Init()

	ctx := br.Log.WithContext(context.Background())

	// Run database migrations (normally done in Start → StartConnectors,
	// but we don't call Start because we don't need the Matrix connection).
	if err := br.DB.Upgrade(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[!] Database migration failed: %v\n", err)
		os.Exit(1)
	}

	// Initialize BackgroundCtx (normally set in StartConnectors).
	// NewLogin needs this for LoadUserLogin.
	br.Bridge.BackgroundCtx, _ = context.WithCancel(context.Background())
	br.Bridge.BackgroundCtx = br.Log.WithContext(br.Bridge.BackgroundCtx)

	// Find the admin user from permissions config.
	userMXID := findAdminUser(br)
	if userMXID == "" {
		fmt.Fprintln(os.Stderr, "[!] No admin user found in config permissions. Cannot log in.")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[*] Logging in as %s\n", userMXID)

	user, err := br.Bridge.GetUserByMXID(ctx, userMXID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to get user: %v\n", err)
		os.Exit(1)
	}

	// Pick login flow: prefer external-key on Linux, apple-id on macOS.
	flows := br.Bridge.Network.GetLoginFlows()
	var flowID string
	for _, f := range flows {
		if f.ID == "apple-id" {
			flowID = f.ID // prefer if available (macOS)
		}
	}
	if flowID == "" && len(flows) > 0 {
		flowID = flows[0].ID
	}
	if flowID == "" {
		fmt.Fprintln(os.Stderr, "[!] No login flows available")
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[*] Using login flow: %s\n", flowID)

	login, err := br.Bridge.Network.CreateLogin(ctx, user, flowID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to create login: %v\n", err)
		os.Exit(1)
	}

	step, err := login.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[!] Failed to start login: %v\n", err)
		os.Exit(1)
	}

	// Drive the multi-step login flow interactively.
	userInput, ok := login.(bridgev2.LoginProcessUserInput)
	if !ok {
		fmt.Fprintln(os.Stderr, "[!] Login flow does not support user input")
		os.Exit(1)
	}

	for step.Type != bridgev2.LoginStepTypeComplete {
		if step.Instructions != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n\n", step.Instructions)
		}

		switch step.Type {
		case bridgev2.LoginStepTypeUserInput:
			input := make(map[string]string)
			for _, field := range step.UserInputParams.Fields {
				if strings.Contains(field.ID, "key") {
					// Long base64 values get line-wrapped when pasted.
					input[field.ID] = promptMultiline(field.Name)
				} else if len(field.Options) > 0 {
					input[field.ID] = promptSelect(field.Name, field.Options)
				} else {
					input[field.ID] = prompt(field.Name)
				}
			}
			step, err = userInput.SubmitUserInput(ctx, input)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[!] Login step failed: %v\n", err)
				os.Exit(1)
			}
		default:
			fmt.Fprintf(os.Stderr, "[!] Unsupported login step type: %s\n", step.Type)
			os.Exit(1)
		}
	}

	fmt.Fprintf(os.Stderr, "\n[+] %s\n", step.Instructions)
	fmt.Fprintf(os.Stderr, "[+] Login ID: %s\n", step.CompleteParams.UserLoginID)

	// Clean shutdown.
	os.Exit(0)
}

// findAdminUser returns the first user MXID with admin permissions.
func findAdminUser(br *mxmain.BridgeMain) id.UserID {
	for userID, perm := range br.Config.Bridge.Permissions {
		if perm.Admin {
			return id.UserID(userID)
		}
	}
	return ""
}
