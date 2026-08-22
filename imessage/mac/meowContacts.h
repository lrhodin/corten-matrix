// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
#import <Contacts/Contacts.h>

const char* nsstring2cstring(NSString* s);

extern void meowAuthCallback(int granted, char* errorDescription, char* errorReason);
CNAuthorizationStatus meowCheckAuth();
CNContactStore* meowCreateStore();
void meowRequestAuth(CNContactStore* store);

NSArray<CNContact*>* meowGetContactList(CNContactStore* store);
CNContact* meowGetContactArrayItem(NSArray<CNContact*>* arr, unsigned long i);
CNContact* meowGetContactByEmail(CNContactStore* store, char* emailAddressC);
CNContact* meowGetContactByPhone(CNContactStore* store, char* phoneNumberC);

NSString* meowGetGivenNameFromContact(CNContact* contact);
NSString* meowGetFamilyNameFromContact(CNContact* contact);
NSString* meowGetNicknameFromContact(CNContact* contact);

const void* meowGetImageDataFromContact(CNContact* contact);
unsigned long meowGetImageDataLengthFromContact(CNContact* contact);

NSArray<CNLabeledValue<NSString*>*>* meowGetEmailAddressesFromContact(CNContact* contact);
NSArray<CNLabeledValue<CNPhoneNumber*>*>* meowGetPhoneNumbersFromContact(CNContact* contact);
NSString* meowGetPhoneArrayItem(NSArray<CNLabeledValue<CNPhoneNumber*>*>* arr, unsigned long i);
NSString* meowGetEmailArrayItem(NSArray<CNLabeledValue<NSString*>*>* arr, unsigned long i);
unsigned long meowGetArrayLength(NSArray* arr);
int meowTestContactQuery(CNContactStore* store);
