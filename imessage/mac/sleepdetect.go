//go:build darwin

// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package mac

//#cgo CFLAGS: -x objective-c -Wno-incompatible-pointer-types
//#cgo LDFLAGS: -framework IOKit -framework Foundation
//#include "meowSleep.h"
import "C"
import (
	"runtime"
	"runtime/debug"
	"sync"

	log "maunium.net/go/maulogger/v2"
)

var sleepLog log.Logger

var actualWakeupCallback = make(chan struct{})

//export meowWakeupCallback
func meowWakeupCallback() {
	sleepLog.Infoln("Received wakeup notification")
	select {
	case actualWakeupCallback <- struct{}{}:
	default:
		sleepLog.Debugln("Not sending wakeup call as there's nobody waiting for it")
	}
}

func meowRunWakeupListener(wg *sync.WaitGroup) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	sleepLog.Debugln("Starting wakeup listener")
	ok := C.meowListenWakeup()
	wg.Done()
	if ok != 0 {
		sleepLog.Errorln("Failed to start wakeup listener")
	} else {
		sleepLog.Debugln("Wakeup listener stopped")
	}
}

func (mac *macOSDatabase) ListenWakeup() {
	sleepLog = log.Sub("SleepDetect")
	defer func() {
		err := recover()
		if err != nil {
			sleepLog.Errorln("Panic in wakeup listen thread: %v\n%s", err, string(debug.Stack()))
		}
	}()
	stop := make(chan struct{})
	mac.stopWakeupDetecting = stop

	go meowRunWakeupListener(&mac.stopWait)

	sleepLog.Debugln("Starting wakeup listen loop")
	for {
		select {
		case <-actualWakeupCallback:
			mac.bridge.PingServer()
		case <-stop:
			sleepLog.Debugln("Stopping wakeup listener")
			C.meowStopListeningWakeup()
			return
		}
	}
}
