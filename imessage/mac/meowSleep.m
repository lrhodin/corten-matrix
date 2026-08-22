// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Based on https://developer.apple.com/library/archive/qa/qa1340/_index.html

#include <ctype.h>
#include <stdlib.h>
#include <stdio.h>

#include <mach/mach_port.h>
#include <mach/mach_interface.h>
#include <mach/mach_init.h>

#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>

#include "meowSleep.h"

io_connect_t rootPort;
id runLoop;

void meowObjCSleepCallback(void *refCon, io_service_t service, natural_t messageType, void *messageArgument) {
    switch (messageType) {
        case kIOMessageCanSystemSleep:
            IOAllowPowerChange(rootPort, (long)messageArgument);
            break;
        case kIOMessageSystemWillSleep:
            IOAllowPowerChange(rootPort, (long)messageArgument);
            break;
        case kIOMessageSystemWillPowerOn:
            break;
        case kIOMessageSystemHasPoweredOn:
        	meowWakeupCallback();
			break;
        default:
            break;
    }
}

int meowListenWakeup() {
    IONotificationPortRef notifyPortRef;
    io_object_t notifierObject;
    void* refCon;

    runLoop = CFRunLoopGetCurrent();

    rootPort = IORegisterForSystemPower(refCon, &notifyPortRef, meowObjCSleepCallback, &notifierObject);
    if (rootPort == 0) {
        return 1;
    }

    CFRunLoopAddSource(runLoop, IONotificationPortGetRunLoopSource(notifyPortRef), kCFRunLoopCommonModes);

    CFRunLoopRun();

    CFRunLoopRemoveSource(runLoop, IONotificationPortGetRunLoopSource(notifyPortRef), kCFRunLoopCommonModes);
    IODeregisterForSystemPower(&notifierObject);
    IOServiceClose(rootPort);
    IONotificationPortDestroy(notifyPortRef);
    return 0;
}

void meowStopListeningWakeup() {
    CFRunLoopStop(runLoop);
}
