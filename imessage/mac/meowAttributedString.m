// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

#include <Foundation/Foundation.h>
#include "meowAttributedString.h"

id jsonSafeObject(id obj);

NSArray* jsonSafeArray(NSArray* input) {
	NSMutableArray* output = [[NSMutableArray alloc] initWithCapacity:input.count];
	[output enumerateObjectsUsingBlock:^(id object, NSUInteger idx, BOOL *stop) {
		[output setObject:jsonSafeObject(object) atIndexedSubscript:idx];
	}];
	return output;
}

NSDictionary* jsonSafeDict(NSDictionary* input) {
	NSMutableDictionary* output = [[NSMutableDictionary alloc] init];
	[input enumerateKeysAndObjectsUsingBlock:^(id key, id obj, BOOL *stop) {
		[output setObject:jsonSafeObject(obj) forKey:key];
	}];
	return output;
}

id jsonSafeObject(id obj) {
	if ([obj isKindOfClass:[NSString class]] || [obj isKindOfClass:[NSNumber class]] || [obj isKindOfClass:[NSNull class]]) {
		return obj;
	} else if ([obj isKindOfClass:[NSData class]]) {
		return [obj base64EncodedStringWithOptions:0];
	} else if ([obj isKindOfClass:[NSURL class]]) {
		return ((NSURL*)obj).absoluteString;
	} else if ([obj isKindOfClass:[NSDictionary class]]) {
		return jsonSafeDict(obj);
	} else if ([obj isKindOfClass:[NSArray class]]) {
		return jsonSafeArray(obj);
	}
	return @"unknown object";
}

char* meowUnsafeDecodeAttributedString(char* input) {
    NSString* nsInput = @(input);
    NSData* data = [[NSData alloc] initWithBase64EncodedString:nsInput options:0];
    NSUnarchiver* arch = [[NSUnarchiver alloc] initForReadingWithData:data];
    NSAttributedString* str = [arch decodeObject];

    NSMutableArray* attrs = [[NSMutableArray alloc] init];
    [str enumerateAttributesInRange:NSMakeRange(0, [str length]) options:NSAttributedStringEnumerationLongestEffectiveRangeNotRequired usingBlock:
        ^(NSDictionary *attributes, NSRange range, BOOL *stop) {
            [attrs addObject:@{
                @"location": [NSNumber numberWithUnsignedInteger:range.location],
                @"length": [NSNumber numberWithUnsignedInteger:range.length],
                @"values": jsonSafeDict(attributes),
            }];
        }
    ];
    NSDictionary* outputDict = @{
        @"content": str.string,
        @"attributes": attrs,
    };

    NSError *error = NULL;
    NSData *jsonData = [NSJSONSerialization dataWithJSONObject:outputDict options:0 error:&error];
    if (!jsonData && error) {
        NSString* fancyError = [[error.localizedDescription stringByAppendingString:@". "] stringByAppendingString:error.localizedFailureReason];
        return [fancyError UTF8String];
    }
    NSString *jsonString = [[NSString alloc] initWithData:jsonData encoding:NSUTF8StringEncoding];
    return [jsonString UTF8String];
}

char* meowDecodeAttributedString(char* input) {
	@try {
		return meowUnsafeDecodeAttributedString(input);
	}
	@catch (NSException* err) {
		return [[[err.name stringByAppendingString:@": "] stringByAppendingString:err.reason] UTF8String];
	}
}
