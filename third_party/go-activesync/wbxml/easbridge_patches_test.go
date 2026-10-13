// Copyright (C) 2026 Henry Stern
// SPDX-License-Identifier: MIT
//
// Tests for the easbridge patches that touch this package (see ../PATCHES.md).

package wbxml

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// provisionSpec is code page 14 as listed in MS-ASWBXML section 2.1.2.1.15
// ("Code Page 14: Provision"), transcribed token by token. Token 0x12 is not
// assigned.
var provisionSpec = []struct {
	tok  byte
	name string
}{
	{0x05, "Provision"}, {0x06, "Policies"}, {0x07, "Policy"}, {0x08, "PolicyType"}, {0x09, "PolicyKey"},
	{0x0A, "Data"}, {0x0B, "Status"}, {0x0C, "RemoteWipe"}, {0x0D, "EASProvisionDoc"},
	{0x0E, "DevicePasswordEnabled"}, {0x0F, "AlphanumericDevicePasswordRequired"},
	{0x10, "RequireStorageCardEncryption"}, {0x11, "PasswordRecoveryEnabled"},
	{0x13, "AttachmentsEnabled"}, {0x14, "MinDevicePasswordLength"}, {0x15, "MaxInactivityTimeDeviceLock"},
	{0x16, "MaxDevicePasswordFailedAttempts"}, {0x17, "MaxAttachmentSize"}, {0x18, "AllowSimpleDevicePassword"},
	{0x19, "DevicePasswordExpiration"}, {0x1A, "DevicePasswordHistory"}, {0x1B, "AllowStorageCard"},
	{0x1C, "AllowCamera"}, {0x1D, "RequireDeviceEncryption"}, {0x1E, "AllowUnsignedApplications"},
	{0x1F, "AllowUnsignedInstallationPackages"}, {0x20, "MinDevicePasswordComplexCharacters"},
	{0x21, "AllowWiFi"}, {0x22, "AllowTextMessaging"}, {0x23, "AllowPOPIMAPEmail"}, {0x24, "AllowBluetooth"},
	{0x25, "AllowIrDA"}, {0x26, "RequireManualSyncWhenRoaming"}, {0x27, "AllowDesktopSync"},
	{0x28, "MaxCalendarAgeFilter"}, {0x29, "AllowHTMLEmail"}, {0x2A, "MaxEmailAgeFilter"},
	{0x2B, "MaxEmailBodyTruncationSize"}, {0x2C, "MaxEmailHTMLBodyTruncationSize"},
	{0x2D, "RequireSignedSMIMEMessages"}, {0x2E, "RequireEncryptedSMIMEMessages"},
	{0x2F, "RequireSignedSMIMEAlgorithm"}, {0x30, "RequireEncryptionSMIMEAlgorithm"},
	{0x31, "AllowSMIMEEncryptionAlgorithmNegotiation"}, {0x32, "AllowSMIMESoftCerts"},
	{0x33, "AllowBrowser"}, {0x34, "AllowConsumerEmail"}, {0x35, "AllowRemoteDesktop"},
	{0x36, "AllowInternetSharing"}, {0x37, "UnapprovedInROMApplicationList"}, {0x38, "ApplicationName"},
	{0x39, "ApprovedApplicationList"}, {0x3A, "Hash"}, {0x3B, "AccountOnlyRemoteWipe"},
}

func TestPatch8_provisionCodePageMatchesTheSpecificationTokenForToken(t *testing.T) {
	r := DefaultRegistry()
	want := map[byte]string{}
	for _, s := range provisionSpec {
		want[s.tok] = s.name
		if got, _ := r.TagName(PageProvision, s.tok); got != s.name {
			t.Errorf("token 0x%02X is %q, the specification says %q", s.tok, got, s.name)
		}
		if id, ok := r.TagID(PageProvision, s.name); !ok || id != s.tok {
			t.Errorf("%s encodes as 0x%02X (%v), the specification says 0x%02X", s.name, id, ok, s.tok)
		}
	}
	// Nothing besides the specification: no invented names, no token 0x12.
	for tok := 0; tok < 256; tok++ {
		if name, ok := r.TagName(PageProvision, byte(tok)); ok && want[byte(tok)] != name {
			t.Errorf("token 0x%02X is %q but the specification has no such tag", tok, name)
		}
	}
	if len(provisionSpec) != 0x3B-0x05+1-1 { // 0x05..0x3B without 0x12
		t.Errorf("the transcription has %d tags", len(provisionSpec))
	}
}

func TestPatch8_accountOnlyRemoteWipeIsToken3BOnTheWire(t *testing.T) {
	// <Provision><AccountOnlyRemoteWipe/></Provision> as a 16.1 server sends it.
	raw := []byte{0x03, 0x01, 0x6A, 0x00, 0x00, 0x0E, 0x45, 0x3B, 0x01}
	doc, err := Unmarshal(raw, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Root.Name != "Provision" || doc.Root.Find("AccountOnlyRemoteWipe") == nil || doc.Root.Find("Hash") != nil {
		t.Fatalf("decoded as %+v", doc.Root)
	}
	out, err := Marshal(doc, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte{0x45, 0x3B}) {
		t.Errorf("re-encoded as % X", out)
	}
}

// settingsSpec is code page 18 as listed in MS-ASWBXML section 2.1.2.1.19 ("Code
// Page 18: Settings"), transcribed token by token. Token 0x2A is not assigned.
// The specification writes 0x1F as SMTPAddress; only the token travels on the wire.
var settingsSpec = []struct {
	tok  byte
	name string
}{
	{0x05, "Settings"}, {0x06, "Status"}, {0x07, "Get"}, {0x08, "Set"}, {0x09, "Oof"}, {0x0A, "OofState"},
	{0x0B, "StartTime"}, {0x0C, "EndTime"}, {0x0D, "OofMessage"}, {0x0E, "AppliesToInternal"},
	{0x0F, "AppliesToExternalKnown"}, {0x10, "AppliesToExternalUnknown"}, {0x11, "Enabled"},
	{0x12, "ReplyMessage"}, {0x13, "BodyType"}, {0x14, "DevicePassword"}, {0x15, "Password"},
	{0x16, "DeviceInformation"}, {0x17, "Model"}, {0x18, "IMEI"}, {0x19, "FriendlyName"}, {0x1A, "OS"},
	{0x1B, "OSLanguage"}, {0x1C, "PhoneNumber"}, {0x1D, "UserInformation"}, {0x1E, "EmailAddresses"},
	{0x1F, "SMTPAddress"}, {0x20, "UserAgent"}, {0x21, "EnableOutboundSMS"}, {0x22, "MobileOperator"},
	{0x23, "PrimarySmtpAddress"}, {0x24, "Accounts"}, {0x25, "Account"}, {0x26, "AccountId"},
	{0x27, "AccountName"}, {0x28, "UserDisplayName"}, {0x29, "SendDisabled"}, {0x2B, "RightsManagementInformation"},
}

func TestPatch10_settingsCodePageMatchesTheSpecificationTokenForToken(t *testing.T) {
	// The device information of the initial Provision request is written on this page.
	if PageSettings != 18 || PageProvision != 14 {
		t.Fatalf("code pages: Settings %d (want 18), Provision %d (want 14)", PageSettings, PageProvision)
	}
	r := DefaultRegistry()
	want := map[byte]string{}
	for _, s := range settingsSpec {
		want[s.tok] = s.name
		if got, _ := r.TagName(PageSettings, s.tok); !strings.EqualFold(got, s.name) {
			t.Errorf("token 0x%02X is %q, the specification says %q", s.tok, got, s.name)
		}
		if got, _ := r.TagName(PageSettings, s.tok); got != "" {
			if id, ok := r.TagID(PageSettings, got); !ok || id != s.tok {
				t.Errorf("%s encodes as 0x%02X (%v), the specification says 0x%02X", got, id, ok, s.tok)
			}
		}
	}
	for tok := 0; tok < 256; tok++ {
		if name, ok := r.TagName(PageSettings, byte(tok)); ok && want[byte(tok)] == "" {
			t.Errorf("token 0x%02X is %q but the specification has no such tag", tok, name)
		}
	}
	if len(settingsSpec) != 0x2B-0x05+1-1 { // 0x05..0x2B without 0x2A
		t.Errorf("the transcription has %d tags", len(settingsSpec))
	}
}

func TestPatch10_aProvisionRequestSwitchesToTheSettingsPageAndBack(t *testing.T) {
	// <Provision><settings:DeviceInformation><settings:Set><settings:Model>m</settings:Model></settings:Set>
	// </settings:DeviceInformation><Policies/></Provision>: the encoder must switch to page 18 for the
	// device information and back to page 14 for Policies.
	doc := &Document{Root: E(PageProvision, "Provision",
		E(PageSettings, "DeviceInformation", E(PageSettings, "Set", E(PageSettings, "Model", Text("m")))),
		E(PageProvision, "Policies"))}
	got, err := Marshal(doc, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x03, 0x01, 0x6A, 0x00,
		0x00, 0x0E, 0x45, // page 14, Provision (with content)
		0x00, 0x12, 0x56, 0x48, 0x57, 0x03, 'm', 0x00, 0x01, 0x01, 0x01, // page 18, DeviceInformation, Set, Model "m", ends
		0x00, 0x0E, 0x06, // back to page 14, Policies (no content)
		0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("got  % X\nwant % X", got, want)
	}
	back, err := Unmarshal(got, DefaultRegistry())
	if err != nil || back.Root.Find("Model") == nil || back.Root.Find("Model").Codepage != PageSettings || back.Root.Find("Policies").Codepage != PageProvision {
		t.Errorf("round trip: %+v %v", back, err)
	}
}

// ---- patch 13: the tables of the pages a 16.1 Sync, Fetch, Ping and SendMail use ----

// specTable is one code page as MS-ASWBXML lists it, token by token.
type specTable struct {
	page   byte
	title  string
	tokens string // "0xNN Name" per line
}

// specTables are the pages the bridge decodes responses of, transcribed from
// MS-ASWBXML 2.1.2.1 ("Code Pages"). The library's tables were written for 14.1
// and lacked the 16.0 and 16.1 tags of AirSyncBase (a meeting request in a 16.x
// Sync carries airsyncbase:Location, 0x20) and of ComposeMail. Provision (14) and
// Settings (18) have their own tests above.
var specTables = []specTable{
	{PageAirSync, "Code Page 0: AirSync (2.1.2.1.1)", `0x05 Sync
0x06 Responses
0x07 Add
0x08 Change
0x09 Delete
0x0A Fetch
0x0B SyncKey
0x0C ClientId
0x0D ServerId
0x0E Status
0x0F Collection
0x10 Class
0x12 CollectionId
0x13 GetChanges
0x14 MoreAvailable
0x15 WindowSize
0x16 Commands
0x17 Options
0x18 FilterType
0x19 Truncation
0x1B Conflict
0x1C Collections
0x1D ApplicationData
0x1E DeletesAsMoves
0x20 Supported
0x21 SoftDelete
0x22 MIMESupport
0x23 MIMETruncation
0x24 Wait
0x25 Limit
0x26 Partial
0x27 ConversationMode
0x28 MaxItems
0x29 HeartbeatInterval`},
	{PageEmail, "Code Page 2: Email (2.1.2.1.3)", `0x05 Attachment
0x06 Attachments
0x07 AttName
0x08 AttSize
0x09 Att0id
0x0A AttMethod
0x0C Body
0x0D BodySize
0x0E BodyTruncated
0x0F DateReceived
0x10 DisplayName
0x11 DisplayTo
0x12 Importance
0x13 MessageClass
0x14 Subject
0x15 Read
0x16 To
0x17 Cc
0x18 From
0x19 ReplyTo
0x1A AllDayEvent
0x1B Categories
0x1C Category
0x1D DtStamp
0x1E EndTime
0x1F InstanceType
0x20 BusyStatus
0x21 Location
0x22 MeetingRequest
0x23 Organizer
0x24 RecurrenceId
0x25 Reminder
0x26 ResponseRequested
0x27 Recurrences
0x28 Recurrence
0x29 Type
0x2A Until
0x2B Occurrences
0x2C Interval
0x2D DayOfWeek
0x2E DayOfMonth
0x2F WeekOfMonth
0x30 MonthOfYear
0x31 StartTime
0x32 Sensitivity
0x33 TimeZone
0x34 GlobalObjId
0x35 ThreadTopic
0x36 MIMEData
0x37 MIMETruncated
0x38 MIMESize
0x39 InternetCPID
0x3A Flag
0x3B Status
0x3C ContentClass
0x3D FlagType
0x3E CompleteTime
0x3F DisallowNewTimeProposal`},
	{PageCalendar, "Code Page 4: Calendar (2.1.2.1.5)", `0x05 Timezone
0x06 AllDayEvent
0x07 Attendees
0x08 Attendee
0x09 Email
0x0A Name
0x0B Body
0x0C BodyTruncated
0x0D BusyStatus
0x0E Categories
0x0F Category
0x11 DtStamp
0x12 EndTime
0x13 Exception
0x14 Exceptions
0x15 Deleted
0x16 ExceptionStartTime
0x17 Location
0x18 MeetingStatus
0x19 OrganizerEmail
0x1A OrganizerName
0x1B Recurrence
0x1C Type
0x1D Until
0x1E Occurrences
0x1F Interval
0x20 DayOfWeek
0x21 DayOfMonth
0x22 WeekOfMonth
0x23 MonthOfYear
0x24 Reminder
0x25 Sensitivity
0x26 Subject
0x27 StartTime
0x28 UID
0x29 AttendeeStatus
0x2A AttendeeType
0x33 DisallowNewTimeProposal
0x34 ResponseRequested
0x35 AppointmentReplyTime
0x36 ResponseType
0x37 CalendarType
0x38 IsLeapMonth
0x39 FirstDayOfWeek
0x3A OnlineMeetingConfLink
0x3B OnlineMeetingExternalLink
0x3C ClientUid`},
	{PageMove, "Code Page 5: Move (2.1.2.1.6)", `0x05 MoveItems
0x06 Move
0x07 SrcMsgId
0x08 SrcFldId
0x09 DstFldId
0x0A Response
0x0B Status
0x0C DstMsgId`},
	{PageFolderHierarchy, "Code Page 7: FolderHierarchy (2.1.2.1.8)", `0x05 Folders
0x06 Folder
0x07 DisplayName
0x08 ServerId
0x09 ParentId
0x0A Type
0x0C Status
0x0E Changes
0x0F Add
0x10 Delete
0x11 Update
0x12 SyncKey
0x13 FolderCreate
0x14 FolderDelete
0x15 FolderUpdate
0x16 FolderSync
0x17 Count`},
	{PagePing, "Code Page 13: Ping (2.1.2.1.14)", `0x05 Ping
0x07 Status
0x08 HeartbeatInterval
0x09 Folders
0x0A Folder
0x0B Id
0x0C Class
0x0D MaxFolders`},
	{PageAirSyncBase, "Code Page 17: AirSyncBase (2.1.2.1.18)", `0x05 BodyPreference
0x06 Type
0x07 TruncationSize
0x08 AllOrNone
0x0A Body
0x0B Data
0x0C EstimatedDataSize
0x0D Truncated
0x0E Attachments
0x0F Attachment
0x10 DisplayName
0x11 FileReference
0x12 Method
0x13 ContentId
0x14 ContentLocation
0x15 IsInline
0x16 NativeBodyType
0x17 ContentType
0x18 Preview
0x19 BodyPartPreference
0x1A BodyPart
0x1B Status
0x1C Add
0x1D Delete
0x1E ClientId
0x1F Content
0x20 Location
0x21 Annotation
0x22 Street
0x23 City
0x24 State
0x25 Country
0x26 PostalCode
0x27 Latitude
0x28 Longitude
0x29 Accuracy
0x2A Altitude
0x2B AltitudeAccuracy
0x2C LocationUri
0x2D InstanceId`},
	{PageItemOperations, "Code Page 20: ItemOperations (2.1.2.1.21)", `0x05 ItemOperations
0x06 Fetch
0x07 Store
0x08 Options
0x09 Range
0x0A Total
0x0B Properties
0x0C Data
0x0D Status
0x0E Response
0x0F Version
0x10 Schema
0x11 Part
0x12 EmptyFolderContents
0x13 DeleteSubFolders
0x14 UserName
0x15 Password
0x16 Move
0x17 DstFldId
0x18 ConversationId
0x19 MoveAlways`},
	{PageComposeMail, "Code Page 21: ComposeMail (2.1.2.1.22)", `0x05 SendMail
0x06 SmartForward
0x07 SmartReply
0x08 SaveInSentItems
0x09 ReplaceMime
0x0B Source
0x0C FolderId
0x0D ItemId
0x0E LongId
0x0F InstanceId
0x10 Mime
0x11 ClientId
0x12 Status
0x13 AccountId
0x15 Forwardees
0x16 Forwardee
0x17 Name
0x18 Email`},
	{PageEmail2, "Code Page 22: Email2 (2.1.2.1.23)", `0x05 UmCallerID
0x06 UmUserNotes
0x07 UmAttDuration
0x08 UmAttOrder
0x09 ConversationId
0x0A ConversationIndex
0x0B LastVerbExecuted
0x0C LastVerbExecutionTime
0x0D ReceivedAsBcc
0x0E Sender
0x0F CalendarType
0x10 IsLeapMonth
0x11 AccountId
0x12 FirstDayOfWeek
0x13 MeetingMessageType
0x15 IsDraft
0x16 Bcc
0x17 Send`},
}

// Tags of the specification that the library leaves out on purpose: they exist
// for protocol 2.5 (and 12.x) only, which the bridge does not speak.
var absentOnPurpose = map[byte]map[byte]string{
	PageAirSync:         {0x19: "Truncation (2.5)"},
	PageEmail:           {0x05: "Attachment", 0x06: "Attachments", 0x07: "AttName", 0x08: "AttSize", 0x09: "Att0id", 0x0A: "AttMethod", 0x0C: "Body", 0x0D: "BodySize", 0x0E: "BodyTruncated", 0x10: "DisplayName", 0x36: "MIMEData", 0x37: "MIMETruncated", 0x38: "MIMESize"},
	PageCalendar:        {0x0B: "Body (2.5)", 0x0C: "BodyTruncated (2.5)"},
	PageFolderHierarchy: {0x05: "Folders (2.5, 12.x)", 0x06: "Folder (2.5, 12.x)"},
}

// Names the library gives differently (the token is what travels).
var renamed = map[byte]map[byte]string{
	PageEmail: {0x29: "Recurrence_Type", 0x2A: "Recurrence_Until", 0x2B: "Recurrence_Occurrences", 0x2C: "Recurrence_Interval",
		0x2D: "Recurrence_DayOfWeek", 0x2E: "Recurrence_DayOfMonth", 0x2F: "Recurrence_WeekOfMonth", 0x30: "Recurrence_MonthOfYear", 0x3B: "FlagStatus"},
	PageCalendar: {0x1C: "Recurrence_Type", 0x1D: "Recurrence_Until", 0x1E: "Recurrence_Occurrences", 0x1F: "Recurrence_Interval",
		0x20: "Recurrence_DayOfWeek", 0x21: "Recurrence_DayOfMonth", 0x22: "Recurrence_WeekOfMonth", 0x23: "Recurrence_MonthOfYear"},
}

// Tags the library has that the specification does not list.
var notInSpec = map[byte]map[byte]string{PagePing: {0x06: "AutdState"}}

func TestPatch13_codePagesMatchTheSpecificationTokenForToken(t *testing.T) {
	r := DefaultRegistry()
	for _, st := range specTables {
		spec := map[byte]string{}
		for _, ln := range strings.Split(strings.TrimSpace(st.tokens), "\n") {
			f := strings.Fields(ln)
			var tok int
			if len(f) != 2 {
				t.Fatalf("%s: bad line %q", st.title, ln)
			}
			if _, err := fmt.Sscanf(f[0], "0x%X", &tok); err != nil {
				t.Fatalf("%s: bad token in %q", st.title, ln)
			}
			spec[byte(tok)] = f[1]
		}
		for tok, name := range spec {
			if _, skip := absentOnPurpose[st.page][tok]; skip {
				if got, ok := r.TagName(st.page, tok); ok {
					t.Errorf("%s: token 0x%02X is %q, left out on purpose until now", st.title, tok, got)
				}
				continue
			}
			want := name
			if alt, ok := renamed[st.page][tok]; ok {
				want = alt
			}
			got, ok := r.TagName(st.page, tok)
			if !ok {
				t.Errorf("%s: token 0x%02X (%s) is missing from the library", st.title, tok, name)
				continue
			}
			if !strings.EqualFold(got, want) {
				t.Errorf("%s: token 0x%02X is %q, the specification says %q", st.title, tok, got, name)
			}
			if id, ok := r.TagID(st.page, got); !ok || id != tok {
				t.Errorf("%s: %s encodes as 0x%02X (%v), the specification says 0x%02X", st.title, got, id, ok, tok)
			}
		}
		for tok := 0; tok < 256; tok++ {
			got, ok := r.TagName(st.page, byte(tok))
			if _, inSpec := spec[byte(tok)]; ok && !inSpec {
				if _, extra := notInSpec[st.page][byte(tok)]; !extra {
					t.Errorf("%s: token 0x%02X is %q but the specification has no such tag", st.title, tok, got)
				}
			}
		}
	}
}

// A meeting request in a 16.x Sync is the case that made the tables matter:
// airsyncbase:Location (0x20) sits inside email:MeetingRequest.
func TestPatch13_aMeetingRequestOfProtocol16DecodesStrictly(t *testing.T) {
	doc := &Document{Root: E(PageAirSync, "Sync", E(PageAirSync, "Collections", E(PageAirSync, "Collection",
		E(PageAirSync, "Commands", E(PageAirSync, "Add",
			E(PageAirSync, "ServerId", Text("5:1")),
			E(PageAirSync, "ApplicationData",
				E(PageEmail, "MeetingRequest",
					E(PageAirSyncBase, "Location",
						E(PageAirSyncBase, "Annotation", Text("Room 4")), E(PageAirSyncBase, "City", Text("Riyadh"))),
					E(PageCalendar, "UID", Text("abc")),
					E(PageEmail2, "MeetingMessageType", Text("1")))))))))}
	b, err := Marshal(doc, DefaultRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte{0x00, 0x11, 0x60}) { // SWITCH_PAGE 17, Location (0x20 | content)
		t.Errorf("airsyncbase:Location is not 0x20 on page 17: % X", b)
	}
	back, err := Unmarshal(b, DefaultRegistry()) // the strict registry, no tolerance needed
	if err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if loc := back.Root.Find("MeetingRequest").Find("Location"); loc == nil || loc.Codepage != PageAirSyncBase || loc.Find("City").TextContent() != "Riyadh" {
		t.Errorf("Location lost: %+v", loc)
	}
}

func TestPatch13_unknownTagsAreKeptWhenTheRegistryTolerates(t *testing.T) {
	// <Sync><Collections><Collection><SyncKey>K</SyncKey>
	//   [page 17, token 0x3E, unknown: with content: [a Type "x" child] and text]
	//   [page 99, token 0x08, an unregistered page, no content]
	//   <CollectionId>5</CollectionId></Collection></Collections></Sync>
	raw := []byte{0x03, 0x01, 0x6A, 0x00,
		0x45, 0x5C, 0x4F, // Sync, Collections, Collection (page 0)
		0x4B, 0x03, 'K', 0x00, 0x01, // SyncKey "K"
		0x00, 0x11, 0x7E, 0x03, 'h', 'i', 0x00, 0x46, 0x03, 'x', 0x00, 0x01, 0x01, // page 17: unknown 0x3E with text and a Type child
		0x00, 0x63, 0x08, // page 99: unknown 0x08, no content
		0x00, 0x00, 0x52, 0x03, '5', 0x00, 0x01, // page 0 again: CollectionId "5"
		0x01, 0x01, 0x01}
	if _, err := Unmarshal(raw, DefaultRegistry()); err == nil || !strings.Contains(err.Error(), "unknown tag") {
		t.Fatalf("the default registry must still refuse an unknown tag: %v", err)
	}
	doc, err := Unmarshal(raw, DefaultRegistry().TolerateUnknownTags())
	if err != nil {
		t.Fatalf("tolerant decode: %v", err)
	}
	coll := doc.Root.Find("Collection")
	var names []string
	for _, k := range coll.Children {
		if e, ok := k.(*Element); ok {
			names = append(names, e.Name)
		}
	}
	if got, want := strings.Join(names, ","), "SyncKey,Unknown_17_0x3E,Unknown_99_0x08,CollectionId"; got != want {
		t.Fatalf("children = %s, want %s", got, want)
	}
	u := coll.Find("Unknown_17_0x3E")
	if u.TextContent() != "hi" || u.Find("Type") == nil || u.Find("Type").TextContent() != "x" || u.Codepage != PageAirSyncBase {
		t.Errorf("the unknown element lost its content: %+v", u)
	}
	if coll.Find("CollectionId").TextContent() != "5" || coll.Find("SyncKey").TextContent() != "K" {
		t.Errorf("the known elements around it were disturbed")
	}
	// Marshal stays strict.
	if _, err := Marshal(&Document{Root: E(PageAirSync, "Sync", E(PageAirSync, "Unknown_0_0x3E"))}, DefaultRegistry().TolerateUnknownTags()); err == nil {
		t.Errorf("Marshal encoded a tag it does not know")
	}
	// Attributes are still not EAS.
	if _, err := Unmarshal([]byte{0x03, 0x01, 0x6A, 0x00, 0x85}, DefaultRegistry().TolerateUnknownTags()); err == nil {
		t.Errorf("an element with attributes was accepted")
	}
}
