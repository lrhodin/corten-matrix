// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package connector

import (
	"io"
	"time"

	"github.com/rs/zerolog"
	log "maunium.net/go/maulogger/v2"

	"github.com/lrhodin/corten-matrix/imessage"
	"github.com/lrhodin/corten-matrix/ipc"
)

// bridgeAdapter satisfies the legacy imessage.Bridge interface so the
// existing mac connector code can be used unmodified.
type bridgeAdapter struct {
	maulog  log.Logger
	zlog    *zerolog.Logger
	ipcProc *ipc.Processor
	config  *imessage.PlatformConfig
}

func newBridgeAdapter(zlog *zerolog.Logger) *bridgeAdapter {
	maulog := log.Create()
	// Create a no-op IPC processor (mac connector uses it only for debug tracking)
	ipcProc := ipc.NewCustomProcessor(io.Discard, &emptyReader{}, maulog, false)
	return &bridgeAdapter{
		maulog:  maulog,
		zlog:    zlog,
		ipcProc: ipcProc,
		config: &imessage.PlatformConfig{
			Platform: "mac",
		},
	}
}

type emptyReader struct{}

func (e *emptyReader) Read(p []byte) (n int, err error) {
	// Block forever - the IPC processor won't actually read from this
	select {}
}

func (ba *bridgeAdapter) GetIPC() *ipc.Processor                      { return ba.ipcProc }
func (ba *bridgeAdapter) GetLog() log.Logger                           { return ba.maulog }
func (ba *bridgeAdapter) GetZLog() *zerolog.Logger                     { return ba.zlog }
func (ba *bridgeAdapter) GetConnectorConfig() *imessage.PlatformConfig { return ba.config }

func (ba *bridgeAdapter) PingServer() (start, serverTs, end time.Time) {
	now := time.Now()
	return now, now, now
}

func (ba *bridgeAdapter) SendBridgeStatus(state imessage.BridgeStatus)                        {}
func (ba *bridgeAdapter) ReIDPortal(oldGUID, newGUID string, mergeExisting bool) bool          { return false }
func (ba *bridgeAdapter) GetMessagesSince(chatGUID string, since time.Time) []string           { return nil }
func (ba *bridgeAdapter) SetPushKey(req *imessage.PushKeyRequest)                              {}
