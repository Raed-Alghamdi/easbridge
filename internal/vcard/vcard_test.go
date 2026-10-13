package vcard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"easbridge/internal/upstream"
)

// unfold undoes the folding the way every reader does.
func unfold(b []byte) string {
	return strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n ", ""), "\r\n\t", "")
}

func TestMarshalIsVCard30WithCRLF(t *testing.T) {
	b := Card{UID: "eas-1", Name: "Ada Lovelace", Family: "Lovelace", Given: "Ada",
		Emails: []Email{{Value: "ada@example.com", Types: []string{"WORK"}}},
		Phones: []Phone{{Value: "+1 555 0100", Types: []string{"WORK", "VOICE"}}, {Value: "+1 555 0101", Types: []string{"CELL"}}},
		Org:    []string{"Analytical Engines", "Research"}, Title: "Countess"}.Marshal()
	want := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:eas-1\r\nFN:Ada Lovelace\r\nN:Lovelace;Ada;;;\r\n" +
		"EMAIL;TYPE=WORK:ada@example.com\r\nTEL;TYPE=WORK;TYPE=VOICE:+1 555 0100\r\nTEL;TYPE=CELL:+1 555 0101\r\n" +
		"ORG:Analytical Engines;Research\r\nTITLE:Countess\r\nEND:VCARD\r\n"
	if string(b) != want {
		t.Errorf("got\n%q\nwant\n%q", b, want)
	}
	if bytes.Contains(bytes.ReplaceAll(b, []byte("\r\n"), nil), []byte("\n")) {
		t.Error("a bare line feed in the output")
	}
}

func TestEscaping(t *testing.T) {
	for in, want := range map[string]string{
		`a\b`:          `a\\b`,
		`a,b;c`:        `a\,b\;c`,
		"line1\nline2": `line1\nline2`,
		"line1\r\nl2":  `line1\nl2`,
		"bare\rcr":     "barecr",
		"tab\there":    "tab here",
		"nul\x00byte":  "nul byte",
		"bad\xffutf8":  "bad�utf8",
		"ls sep":       "ls sep",
		"":             "",
		"عمر, أحمد; ":  `عمر\, أحمد\; `,
	} {
		if got := Escape(in); got != want {
			t.Errorf("Escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFoldingAt75OctetsNeverSplitsAUTF8Sequence(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("a", 300),
		strings.Repeat("محمد بن عبدالله ", 40),  // 2-octet characters
		strings.Repeat("€", 100),                // 3-octet
		strings.Repeat("😀", 60),                 // 4-octet
		"x" + strings.Repeat("ع", 80),           // odd alignment
		strings.Repeat("ab", 37) + "ع" + "tail", // the cut falls inside the character
	} {
		line := "FN:" + name
		folded := Fold(line)
		parts := strings.Split(strings.TrimSuffix(folded, "\r\n"), "\r\n")
		for i, p := range parts {
			if !utf8.ValidString(p) {
				t.Fatalf("part %d is not valid UTF-8: %q", i, p)
			}
			limit := 75
			if i > 0 {
				if p[0] != ' ' {
					t.Fatalf("continuation %d does not start with a space", i)
				}
				limit = 75 // the space counts: one octet of the 75
			}
			if len(p) > limit {
				t.Errorf("part %d is %d octets", i, len(p))
			}
		}
		if got := strings.ReplaceAll(folded, "\r\n ", ""); got != line+"\r\n" {
			t.Errorf("unfolding does not give the line back")
		}
	}
	if got := Fold("short"); got != "short\r\n" {
		t.Errorf("a short line is only terminated: %q", got)
	}
	exactly := strings.Repeat("x", 75)
	if got := Fold(exactly); got != exactly+"\r\n" {
		t.Errorf("75 octets stay on one line")
	}
	if got := Fold(exactly + "y"); got != exactly+"\r\n y\r\n" {
		t.Errorf("76 octets fold after 75: %q", got)
	}
}

func TestArabicCardRoundTripsThroughFolding(t *testing.T) {
	c := Card{UID: "dir-1", Name: "عبدالرحمن بن محمد السلمي الأستاذ الدكتور", Family: "السلمي", Given: "عبدالرحمن",
		Org: []string{"شركة الأمثلة المحدودة, قسم تقنية المعلومات"}, Title: "مدير إدارة الأنظمة والتطبيقات",
		Emails: []Email{{Value: "a@example.com", Types: []string{"WORK"}}}}
	b := c.Marshal()
	if !utf8.Valid(b) {
		t.Fatal("not UTF-8")
	}
	un := unfold(b)
	for _, want := range []string{"FN:" + c.Name, "N:السلمي;عبدالرحمن;;;", `ORG:شركة الأمثلة المحدودة\, قسم تقنية المعلومات`, "TITLE:" + c.Title} {
		if !strings.Contains(un, want) {
			t.Errorf("missing %q in\n%s", want, un)
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line of %d octets", len(line))
		}
	}
}

func TestEmptyFieldsAreLeftOut(t *testing.T) {
	b := string(Card{UID: "u", Emails: []Email{{Value: ""}}, Phones: []Phone{{}}, Addresses: []Address{{Types: []string{"HOME"}}}, Categories: []string{"", ""}}.Marshal())
	for _, no := range []string{"EMAIL", "TEL", "ADR", "ORG", "\r\nN:", "TITLE", "BDAY", "CATEGORIES", "NOTE", "URL", "NICKNAME"} {
		if strings.Contains(b, no) {
			t.Errorf("%s written for an empty field:\n%s", no, b)
		}
	}
	if !strings.Contains(b, "FN:u\r\n") {
		t.Errorf("FN is required and falls back to the UID:\n%s", b)
	}
}

func TestFNFallbackOrder(t *testing.T) {
	for _, tc := range []struct {
		c    Card
		want string
	}{
		{Card{Name: " Ada ", Given: "X"}, "Ada"},
		{Card{Given: "Ada", Family: "Lovelace", Prefix: "Dr."}, "Dr. Ada Lovelace"},
		{Card{Nickname: "Ads"}, "Ads"},
		{Card{Org: []string{"Acme"}}, "Acme"},
		{Card{Emails: []Email{{Value: "a@b.c"}}}, "a@b.c"},
		{Card{UID: "u1"}, "u1"},
	} {
		if got := tc.c.fn(); got != tc.want {
			t.Errorf("fn(%+v) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestParametersAreSanitised(t *testing.T) {
	b := string(Card{UID: "u", Emails: []Email{{Value: "a@b.c", Types: []string{"wo;rk:", "", "x\r\ny"}}}}.Marshal())
	if !strings.Contains(b, "EMAIL;TYPE=WORK;TYPE=OTHER;TYPE=XY:a@b.c\r\n") {
		t.Errorf("TYPE values must be letters and digits only:\n%s", b)
	}
}

func TestDatesAddressesCategoriesAndNote(t *testing.T) {
	b := unfold(Card{UID: "u", Name: "N", Birthday: time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC), Anniversary: time.Date(2015, 1, 2, 0, 0, 0, 0, time.UTC),
		Addresses:  []Address{{Types: []string{"WORK"}, Street: "1 Main St; Suite 2", City: "Riyadh", Country: "SA"}},
		Categories: []string{"VIP", "a,b"}, URL: "https://example.com/x", Note: "Office: B2\nfloor 3"}.Marshal())
	for _, want := range []string{"BDAY:1990-05-15\r\n", "X-ANNIVERSARY:2015-01-02\r\n", `ADR;TYPE=WORK:;;1 Main St\; Suite 2;Riyadh;;;SA` + "\r\n",
		`CATEGORIES:VIP,a\,b` + "\r\n", "URL:https://example.com/x\r\n", `NOTE:Office: B2\nfloor 3` + "\r\n"} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
}

func TestInjectionInValuesCannotAddProperties(t *testing.T) {
	b := string(Card{UID: "u", Name: "Evil\r\nEND:VCARD\r\nBEGIN:VCARD\r\nFN:x", Note: "a\nTEL:123"}.Marshal())
	begins, ends := 0, 0
	for _, l := range strings.Split(b, "\r\n") {
		switch l {
		case "BEGIN:VCARD":
			begins++
		case "END:VCARD":
			ends++
		}
	}
	if begins != 1 || ends != 1 {
		t.Errorf("a value must not end the card:\n%s", b)
	}
	for _, l := range strings.Split(b, "\r\n") {
		if strings.HasPrefix(l, "TEL:") {
			t.Errorf("a value added a property: %q", l)
		}
	}
}

func TestStableUIDsAndETag(t *testing.T) {
	a := PersonalUID("alice@example.com", "5", "5:12")
	if a != PersonalUID("alice@example.com", "5", "5:12") || len(a) != len("eas-")+32 || !strings.HasPrefix(a, "eas-") {
		t.Errorf("personal uid %q", a)
	}
	for _, other := range []string{PersonalUID("bob@example.com", "5", "5:12"), PersonalUID("alice@example.com", "6", "5:12"),
		PersonalUID("alice@example.com", "5", "5:13"), PersonalUID("alice@example.com", "55", ":12")} {
		if other == a {
			t.Errorf("uids collide: %q", other)
		}
	}
	d := DirectoryUID(" Ada@Example.COM ")
	if d != DirectoryUID("ada@example.com") || !strings.HasPrefix(d, "dir-") || len(d) != 4+32 {
		t.Errorf("directory uid %q", d)
	}
	sum := sha256.Sum256([]byte("ada@example.com"))
	if d != "dir-"+hex.EncodeToString(sum[:])[:32] {
		t.Errorf("the directory uid is the first 32 hex digits of the SHA-256 of the lower-case address, got %q", d)
	}
	e := ETag([]byte("x"))
	if !strings.HasPrefix(e, `"`) || !strings.HasSuffix(e, `"`) || len(e) != 66 || e == ETag([]byte("y")) {
		t.Errorf("etag %q", e)
	}
}

func TestFromContactMapsWhatMailFlowShows(t *testing.T) {
	c := upstream.Contact{ServerID: "5:9", FirstName: "Ada", LastName: "Lovelace", MiddleName: "King", Title: "Dr.", Suffix: "PhD", FileAs: "Lovelace, Ada",
		NickName: "Ads", Company: "Acme, Inc.", Department: "R&D", JobTitle: "Engineer", Office: "B2",
		Email1: "Ada Lovelace <ada@example.com>", Email2: "SMTP:ada2@example.com", Email3: "/o=legacy/cn=x",
		HomePhone: "111", BusinessPhone: "222", MobilePhone: "333",
		BusinessAddress: upstream.Address{Street: "1 Main", City: "Riyadh", Country: "SA"},
		Birthday:        time.Date(1990, 5, 15, 0, 0, 0, 0, time.UTC), WebPage: "https://example.com", Categories: []string{"VIP", " "}}
	card := FromContact("alice@example.com", "5", c)
	b := unfold(card.Marshal())
	for _, want := range []string{
		"UID:" + PersonalUID("alice@example.com", "5", "5:9"), "FN:Lovelace\\, Ada", "N:Lovelace;Ada;King;Dr.;PhD", "NICKNAME:Ads",
		"EMAIL;TYPE=OTHER:ada@example.com", "EMAIL;TYPE=OTHER:ada2@example.com",
		"TEL;TYPE=WORK;TYPE=VOICE:222", "TEL;TYPE=CELL:333", "TEL;TYPE=HOME;TYPE=VOICE:111",
		`ORG:Acme\, Inc.;R&D`, "TITLE:Engineer", "ADR;TYPE=WORK:;;1 Main;Riyadh;;;SA", "BDAY:1990-05-15", "URL:https://example.com",
		"CATEGORIES:VIP", "NOTE:Office: B2"} {
		if !strings.Contains(b, want+"\r\n") {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	if strings.Count(b, "EMAIL") != 2 {
		t.Errorf("an address that is not an address is left out:\n%s", b)
	}
	if strings.Contains(b, "ADR;TYPE=HOME") {
		t.Errorf("an empty address is left out")
	}
}

func TestFromDirectoryKeepsOnlyTheMappedFields(t *testing.T) {
	e := upstream.DirectoryEntry{DisplayName: "Ada Lovelace", FirstName: "Ada", LastName: "Lovelace", Title: "Engineer", Office: "B2",
		Company: "Acme", EmailAddress: "Ada@Example.com", Phone: "222", MobilePhone: "333"}
	b := unfold(FromDirectory(e).Marshal())
	for _, want := range []string{"UID:" + DirectoryUID("ada@example.com"), "FN:Ada Lovelace", "N:Lovelace;Ada;;;", "EMAIL;TYPE=WORK:ada@example.com",
		"TEL;TYPE=WORK;TYPE=VOICE:222", "TEL;TYPE=CELL:333", "TITLE:Engineer", "ORG:Acme", "NOTE:Office: B2"} {
		if !strings.Contains(b, want+"\r\n") {
			t.Errorf("missing %q in\n%s", want, b)
		}
	}
	if strings.Contains(b, "HOME") {
		t.Errorf("no home data in a directory card:\n%s", b)
	}
}

func TestBareAddress(t *testing.T) {
	for in, want := range map[string]string{
		"a@b.c": "a@b.c", " a@b.c ": "a@b.c", "Name <a@b.c>": "a@b.c", `"Last, First" <a@b.c>`: "a@b.c", "SMTP:a@b.c": "a@b.c",
		"smtp:a@b.c": "a@b.c", "/o=x/cn=y": "", "": "", "<>": "", "no address": "",
	} {
		if got := BareAddress(in); got != want {
			t.Errorf("BareAddress(%q) = %q, want %q", in, got, want)
		}
	}
}
