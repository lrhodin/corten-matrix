// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

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
