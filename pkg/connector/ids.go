// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// makeUserID creates a networkid.UserID from an iMessage identifier
// (e.g., "tel:+1234567890" or "mailto:user@example.com").
func makeUserID(identifier string) networkid.UserID {
	return networkid.UserID(identifier)
}

// makeMessageID creates a networkid.MessageID from an iMessage message UUID.
func makeMessageID(guid string) networkid.MessageID {
	return networkid.MessageID(guid)
}
