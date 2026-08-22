//go:build darwin

// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package mac

import (
	"fmt"
)

const groupMemberQuery = `
SELECT handle.id FROM chat
JOIN chat_handle_join ON chat_handle_join.chat_id = chat.ROWID
JOIN handle ON chat_handle_join.handle_id = handle.ROWID
WHERE chat.guid=$1
`

func (mac *macOSDatabase) prepareGroups() error {
	var err error
	mac.groupMemberQuery, err = mac.chatDB.Prepare(groupMemberQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare legacy group query: %w", err)
	}
	return nil
}

func (mac *macOSDatabase) GetGroupMembers(chatID string) ([]string, error) {
	res, err := mac.groupMemberQuery.Query(chatID)
	if err != nil {
		return nil, fmt.Errorf("error querying group members: %w", err)
	}
	var users []string
	for res.Next() {
		var user string
		err = res.Scan(&user)
		if err != nil {
			return users, fmt.Errorf("error scanning row: %w", err)
		} else if len(user) == 0 {
			continue
		}
		users = append(users, user)
	}
	return users, nil
}
