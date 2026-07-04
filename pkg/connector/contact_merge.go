// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

// Contact-based DM portal merging.
//
// When a contact has multiple phone numbers or emails, iMessage stores each
// as a separate conversation. Without merging, the bridge creates separate
// Matrix rooms for each number. This file provides helpers to redirect
// incoming messages from a secondary phone number to an existing primary portal.

import (
	"context"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/lrhodin/corten-matrix/imessage"
)

// resolveContactPortalID checks if the given DM identifier belongs to a contact
// that already has an existing portal under a different phone number or email.
// Returns the original identifier (as a PortalID) if no existing portal is found.
func (c *IMClient) resolveContactPortalID(identifier string) networkid.PortalID {
	defaultID := networkid.PortalID(identifier)

	if strings.Contains(identifier, ",") {
		return defaultID
	}

	contact := c.lookupContact(identifier)
	if contact == nil || !contact.HasName() {
		return defaultID
	}

	altIDs := contactPortalIDs(contact)
	if len(altIDs) <= 1 {
		return defaultID
	}

	ctx := context.Background()
	for _, altID := range altIDs {
		if altID == identifier {
			continue
		}
		portal, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
			ID:       networkid.PortalID(altID),
			Receiver: c.UserLogin.ID,
		})
		if err == nil && portal != nil && portal.MXID != "" {
			c.UserLogin.Log.Debug().
				Str("original", identifier).
				Str("resolved", altID).
				Msg("Resolved contact portal to existing portal")
			return networkid.PortalID(altID)
		}
	}

	return defaultID
}

// resolveSendTarget determines the best identifier to send to for a DM portal.
func (c *IMClient) resolveSendTarget(portalID string) (target string) {
	if c.client == nil || strings.Contains(portalID, ",") {
		return portalID
	}

	contact := c.lookupContact(portalID)
	altIDs := contactPortalIDs(contact)
	if contact == nil || len(altIDs) <= 1 {
		return portalID
	}
	// ValidateTargets crosses into the identity-manager FFI path. Upstream
	// has reachable panic sites (identity_manager.rs:249/335/542/555); fall
	// back to the original portalID if the FFI call panics rather than
	// crashing the send path.
	defer func() {
		if r := recover(); r != nil {
			c.UserLogin.Log.Error().Interface("panic", r).Str("portal_id", portalID).
				Msg("resolveSendTarget panicked in FFI path — falling back to original portalID")
			target = portalID
		}
	}()

	candidates := make([]string, 0, len(altIDs)+1)
	seen := make(map[string]struct{}, len(altIDs)+1)
	addCandidate := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	addCandidate(portalID)
	for _, altID := range altIDs {
		addCandidate(altID)
	}

	valid := c.client.ValidateTargets(candidates, c.handle)
	validSet := make(map[string]struct{}, len(valid))
	for _, id := range valid {
		validSet[id] = struct{}{}
	}
	if picked, ok := pickSendTarget(portalID, altIDs, validSet); ok {
		if picked == portalID {
			return portalID
		}
		c.UserLogin.Log.Info().
			Str("portal_id", portalID).
			Str("send_target", picked).
			Int("candidates", len(candidates)).
			Int("valid", len(valid)).
			Msg("Resolved send target to alternate contact number")
		return picked
	}

	if len(valid) == 0 {
		c.UserLogin.Log.Warn().
			Str("portal_id", portalID).
			Int("candidates", len(candidates)).
			Int("valid", len(valid)).
			Msg("Contact send-target IDS lookup returned no valid handles; falling back to original portal ID")
		return portalID
	}
	c.UserLogin.Log.Info().
		Str("portal_id", portalID).
		Int("candidates", len(candidates)).
		Int("valid", len(valid)).
		Msg("No reachable contact alias matched send target candidates; falling back to original portal ID")
	return portalID
}

func pickSendTarget(portalID string, altIDs []string, validSet map[string]struct{}) (string, bool) {
	if _, ok := validSet[portalID]; ok {
		return portalID, true
	}
	for _, altID := range altIDs {
		if altID == portalID {
			continue
		}
		if _, ok := validSet[altID]; ok {
			return altID, true
		}
	}
	return portalID, false
}

// lookupContact resolves a portal/identifier string to a Contact using
// cloud contacts (iCloud CardDAV), falling back to chat.db contacts.
func (c *IMClient) lookupContact(identifier string) *imessage.Contact {
	localID := stripIdentifierPrefix(identifier)
	if localID == "" {
		return nil
	}

	if c.contacts != nil {
		contact, _ := c.contacts.GetContactInfo(localID)
		if contact != nil {
			return contact
		}
	}
	if c.chatDB != nil {
		contact, _ := c.chatDB.api.GetContactInfo(localID)
		return contact
	}
	return nil
}

// countNonSelfMembers counts the unique non-self members of a conversation
// across the participant list and the sender, collapsing a contact's alternate
// handles so a multi-number contact isn't double-counted. The group/DM signal
// for inbound routing: self is implicit, so >=2 other members means a group.
// Sender is included because relayed carrier groups omit self from participants.
func (c *IMClient) countNonSelfMembers(participants []string, sender *string) int {
	seen := make(map[string]bool)
	count := 0
	add := func(raw string) {
		n := normalizeIdentifierForPortalID(raw)
		if n == "" || c.isMyHandle(n) || seen[n] {
			return
		}
		seen[n] = true
		count++
		if contact := c.lookupContact(n); contact != nil {
			for _, altID := range contactPortalIDs(contact) {
				seen[altID] = true
			}
		}
	}
	for _, p := range participants {
		add(p)
	}
	if sender != nil {
		add(*sender)
	}
	return count
}

// getContactChatGUIDs returns all possible chat.db GUIDs for a DM portal,
// including GUIDs for alternate phone numbers/emails belonging to the same contact.
func (c *IMClient) getContactChatGUIDs(portalID string) []string {
	guids := portalIDToChatGUIDs(portalID)

	contact := c.lookupContact(portalID)
	if contact == nil {
		return guids
	}

	for _, altID := range contactPortalIDs(contact) {
		if altID == portalID {
			continue
		}
		guids = append(guids, portalIDToChatGUIDs(altID)...)
	}

	return guids
}

// contactKeyFromContact returns a stable identity key for grouping a contact's
// DM entries during initial sync deduplication. Returns "" if no merging is
// needed (single phone, no name, etc.).
func contactKeyFromContact(contact *imessage.Contact) string {
	if contact == nil || !contact.HasName() {
		return ""
	}
	phones := make([]string, 0, len(contact.Phones))
	for _, p := range contact.Phones {
		n := normalizePhoneForPortalID(p)
		if n != "" {
			phones = append(phones, n)
		}
	}
	if len(phones) <= 1 {
		return ""
	}
	sort.Strings(phones)
	return strings.Join(phones, "|")
}

// contactPortalIDs returns all portal ID strings for a contact's phone numbers
// and emails.
func contactPortalIDs(contact *imessage.Contact) []string {
	if contact == nil {
		return nil
	}

	seen := make(map[string]bool)
	var ids []string

	for _, phone := range contact.Phones {
		normalized := normalizePhoneForPortalID(phone)
		if normalized == "" {
			continue
		}
		pid := "tel:" + normalized
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	for _, email := range contact.Emails {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		pid := "mailto:" + email
		if !seen[pid] {
			seen[pid] = true
			ids = append(ids, pid)
		}
	}

	return ids
}

// normalizePhoneForPortalID converts a phone number to E.164-like format.
func normalizePhoneForPortalID(phone string) string {
	n := normalizePhone(phone)
	if n == "" {
		return ""
	}
	if strings.HasPrefix(n, "+") {
		return n
	}
	if len(n) == 10 {
		return "+1" + n
	}
	if len(n) == 11 && n[0] == '1' {
		return "+" + n
	}
	return "+" + n
}

// canonicalContactHandle returns a deterministic canonical handle for a contact
// that has multiple iMessage handles (phone + email). This ensures CloudKit
// backfill creates a single portal per contact rather than one per handle.
// If the identifier doesn't resolve to a multi-handle contact, returns it unchanged.
func (c *IMClient) canonicalContactHandle(identifier string) string {
	contact := c.lookupContact(identifier)
	if contact == nil || !contact.HasName() {
		return identifier
	}
	altIDs := contactPortalIDs(contact)
	if len(altIDs) <= 1 {
		return identifier
	}
	sort.Strings(altIDs)
	for _, id := range altIDs {
		if strings.HasPrefix(id, "tel:") {
			return id
		}
	}
	return altIDs[0]
}

// canonicalizeDMSender remaps the sender identity for DM events so that the
// ghost matches the portal's canonical handle. Without this, a contact sending
// from their email handle into a phone-based DM portal causes a phantom ghost
// to briefly join the room.
func (c *IMClient) canonicalizeDMSender(portalKey networkid.PortalKey, sender bridgev2.EventSender) bridgev2.EventSender {
	if sender.IsFromMe {
		return sender
	}
	portalID := string(portalKey.ID)
	// Only remap for DM portals (not groups or gid: portals).
	if strings.Contains(portalID, ",") || strings.HasPrefix(portalID, "gid:") {
		return sender
	}
	canonicalUserID := makeUserID(portalID)
	if sender.Sender != canonicalUserID {
		return bridgev2.EventSender{
			IsFromMe: false,
			Sender:   canonicalUserID,
		}
	}
	return sender
}
