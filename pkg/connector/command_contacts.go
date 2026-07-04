// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

// contacts command — search loaded contacts by name, confirm iMessage
// reachability, and let the user pick from a numbered list to start a DM.
//
// Flow:
//   !im contacts John
//   → Searches contacts, batch-validates against iMessage, shows numbered list
//   2
//   → Resolves identifier and creates (or opens) the Matrix room

import (
	"fmt"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/provisionutil"

	"github.com/lrhodin/corten-matrix/imessage"
)

// contactMatch holds one iMessage-reachable identifier for a matching contact.
type contactMatch struct {
	identifier  string // portal ID: "tel:+15551234567" or "mailto:user@example.com"
	displayName string // contact's full name
	label       string // human-readable identifier: "📞 +1 (555) 234-5678"
}

// maxContactValidate caps how many contact identifiers a single `contacts`
// search validates against Apple's identity service (IDS). A broad query
// (e.g. "David" against a 6,000-entry address book) can match thousands of
// phone/email identifiers; validating them all hammers IDS. Only the
// top-ranked identifiers are checked — the user is told to narrow the search
// if there were more.
const maxContactValidate = 25

// Name-match score tiers, ranked most- to least-relevant.
const (
	matchNone = iota
	matchFuzzy
	matchExact
)

// cmdContacts is the !im contacts command handler.
var cmdContacts = &commands.FullHandler{
	Name:    "contacts",
	Aliases: []string{"find"},
	Func:    fnContacts,
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Search contacts by name, see who's on iMessage, then reply with a number to open a chat",
		Args:        "<name>",
	},
	RequiresLogin: true,
}

func fnContacts(ce *commands.Event) {
	if len(ce.Args) == 0 {
		ce.Reply("**Usage:** `$cmdprefix contacts <name>`\n\nSearches your synced contacts and shows which ones are reachable on iMessage.\n\nExample: `$cmdprefix contacts John`")
		return
	}

	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return
	}
	if client.contacts == nil {
		ce.Reply("Contacts have not synced yet. Try again in a moment.")
		return
	}

	query := strings.ToLower(strings.Join(ce.Args, " "))

	// --- Phase 1: name search across all loaded contacts ---
	all := client.contacts.GetAllContacts()

	type candidate struct {
		identifier string // tel:+ or mailto: form used for validation / portal key
		rawLabel   string // phone/email as stored in the contact record
		name       string // contact display name
	}
	// Rank matching contacts so exact name matches come before fuzzy ones —
	// this matters once we cap the IDS validation below, so the most relevant
	// matches stay within the cap.
	var exact, fuzzy []*imessage.Contact
	for _, contact := range all {
		switch contactMatchScore(contact, query) {
		case matchExact:
			exact = append(exact, contact)
		case matchFuzzy:
			fuzzy = append(fuzzy, contact)
		}
	}
	ranked := append(exact, fuzzy...)

	var candidates []candidate
	seen := make(map[string]bool)
	for _, contact := range ranked {
		name := contact.Name()

		for _, phone := range contact.Phones {
			norm := normalizePhoneForPortalID(phone)
			if norm == "" {
				continue
			}
			id := "tel:" + norm
			if seen[id] {
				continue
			}
			seen[id] = true
			candidates = append(candidates, candidate{identifier: id, rawLabel: phone, name: name})
		}
		for _, email := range contact.Emails {
			email = strings.ToLower(strings.TrimSpace(email))
			if email == "" {
				continue
			}
			id := "mailto:" + email
			if seen[id] {
				continue
			}
			seen[id] = true
			candidates = append(candidates, candidate{identifier: id, rawLabel: email, name: name})
		}
	}

	if len(candidates) == 0 {
		ce.Reply("No contacts found matching **\"%s\"**.", strings.Join(ce.Args, " "))
		return
	}

	// Cap how many identifiers are validated against Apple's identity service.
	// Building candidates above is cheap (string ops only); the IDS round-trip
	// is the expensive part, so we cap here — keeping the top-ranked matches —
	// rather than spamming IDS with every match for a broad query.
	totalCandidates := len(candidates)
	truncated := false
	if len(candidates) > maxContactValidate {
		candidates = candidates[:maxContactValidate]
		truncated = true
	}

	// --- Phase 2: batch-validate the (capped) identifiers against Apple iMessage ---
	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.identifier
	}
	valid := client.validateTargetsSafe(ids)
	validSet := make(map[string]bool, len(valid))
	for _, v := range valid {
		validSet[v] = true
	}

	// Filter to only iMessage-reachable identifiers.
	var matches []contactMatch
	for _, cand := range candidates {
		if !validSet[cand.identifier] {
			continue
		}
		var label string
		if strings.HasPrefix(cand.identifier, "tel:") {
			label = "📞 " + cand.rawLabel
		} else {
			label = "📧 " + cand.rawLabel
		}
		matches = append(matches, contactMatch{
			identifier:  cand.identifier,
			displayName: cand.name,
			label:       label,
		})
	}

	if len(matches) == 0 {
		if truncated {
			ce.Reply("None of the first %d of %d identifiers matching **\"%s\"** are reachable on iMessage. Add a last name (e.g. `$cmdprefix contacts David Smith`) to narrow your search.", maxContactValidate, totalCandidates, strings.Join(ce.Args, " "))
		} else {
			ce.Reply("No contacts matching **\"%s\"** are reachable on iMessage.", strings.Join(ce.Args, " "))
		}
		return
	}

	// --- Phase 3: show numbered list, wait for selection ---
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found **%d** iMessage contact(s) matching \"%s\":\n\n", len(matches), strings.Join(ce.Args, " "))
	for i, m := range matches {
		fmt.Fprintf(&sb, "%d. **%s** — %s\n", i+1, m.displayName, m.label)
	}
	if truncated {
		fmt.Fprintf(&sb, "\n_Your search matched %d possible numbers/emails; only the first %d were checked against iMessage to avoid overloading Apple. Add a last name (e.g. `$cmdprefix contacts David Smith`) to narrow it down._\n", totalCandidates, maxContactValidate)
	}
	sb.WriteString("\nReply with a number to start a chat, or `$cmdprefix cancel` to cancel.")
	ce.Reply(sb.String())

	// Store state so the next message from this user is treated as a selection.
	commands.StoreCommandState(ce.User, &commands.CommandState{
		Action: "select contact to message",
		Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
			n, err := strconv.Atoi(strings.TrimSpace(ce.RawArgs))
			if err != nil || n < 1 || n > len(matches) {
				ce.Reply("Please reply with a number between 1 and %d, or `$cmdprefix cancel` to cancel.", len(matches))
				return
			}

			// Clear state immediately before the (potentially slow) portal create.
			commands.StoreCommandState(ce.User, nil)

			chosen := matches[n-1]
			login := ce.User.GetDefaultLogin()
			if login == nil {
				ce.Reply("No active login found.")
				return
			}

			resp, err := provisionutil.ResolveIdentifier(ce.Ctx, login, chosen.identifier, true)
			if err != nil {
				ce.Reply("Failed to start chat with **%s**: %v", chosen.displayName, err)
				return
			}
			if resp == nil || resp.Portal == nil {
				ce.Reply("Could not start a chat with **%s** (%s).", chosen.displayName, chosen.label)
				return
			}

			roomName := resp.Portal.Name
			if roomName == "" {
				roomName = resp.Portal.MXID.String()
			}
			if resp.JustCreated {
				ce.Reply("Started chat with **%s**: [%s](%s)", chosen.displayName, roomName, resp.Portal.MXID.URI().MatrixToURL())
			} else {
				ce.Reply("You already have a chat with **%s**: [%s](%s)", chosen.displayName, roomName, resp.Portal.MXID.URI().MatrixToURL())
			}
		}),
		Cancel: func() {}, // nothing to clean up; cancel reply is handled by the framework
	})
}

// contactMatchScore scores how well a contact's name matches the query:
// matchExact for an exact substring of the name/nickname, matchFuzzy for a
// word-by-word fuzzy match, matchNone for no match.
//
// Strategy (applied in order, short-circuits on first hit):
//  1. Fast exact substring on the full name / nickname  → matchExact.
//  2. Fuzzy: split both query and name into words; every query word must
//     fuzzily match at least one name word via prefix match OR Levenshtein
//     distance within the per-word threshold              → matchFuzzy.
//
// Threshold table (Levenshtein):
//
//	≤2 chars → 0 (short tokens must prefix-match exactly)
//	3–5 chars → 1 edit  ("jon"→"John", "smth"→"Smith")
//	6+ chars  → 2 edits ("johnsen"→"Johnson")
func contactMatchScore(contact *imessage.Contact, query string) int {
	name := strings.ToLower(contact.Name())
	nick := strings.ToLower(contact.Nickname)

	// 1. Fast path: exact substring anywhere in name or nickname.
	if strings.Contains(name, query) {
		return matchExact
	}
	if nick != "" && strings.Contains(nick, query) {
		return matchExact
	}

	// 2. Fuzzy word-by-word match.
	queryWords := strings.Fields(query)
	if len(queryWords) == 0 {
		return matchNone
	}
	nameWords := strings.Fields(name)
	if nick != "" {
		nameWords = append(nameWords, strings.Fields(nick)...)
	}
	for _, qw := range queryWords {
		if !anyNameWordFuzzyMatches(qw, nameWords) {
			return matchNone
		}
	}
	return matchFuzzy
}

// anyNameWordFuzzyMatches returns true if qw fuzzy-matches any word in nameWords.
func anyNameWordFuzzyMatches(qw string, nameWords []string) bool {
	threshold := fuzzyEditThreshold(len(qw))
	for _, nw := range nameWords {
		// Prefix match: "jo" matches "john", "john" matches "johnson".
		if strings.HasPrefix(nw, qw) {
			return true
		}
		// Edit-distance match for longer query words.
		if threshold > 0 && levenshtein(qw, nw) <= threshold {
			return true
		}
	}
	return false
}

// fuzzyEditThreshold returns the maximum Levenshtein edits allowed for a
// query word of the given length.
func fuzzyEditThreshold(n int) int {
	switch {
	case n <= 2:
		return 0 // must prefix-match exactly
	case n <= 5:
		return 1 // "jon"→"john", "smth"→"smith"
	default:
		return 2 // "johnsen"→"johnson"
	}
}

// levenshtein computes the edit distance between two strings (two-row DP).
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
