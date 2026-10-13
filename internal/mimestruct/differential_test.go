package mimestruct

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"
)

// go-imap's own helpers are the specification for the output of well-formed
// mail. These tests compare them with this package on generated messages.

func eqMap(a, b map[string]string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func eqStrs(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func eqDisp(a, b *imap.BodyStructureDisposition) string {
	if (a == nil) != (b == nil) {
		return fmt.Sprintf("disposition nil-ness: %+v vs %+v", a, b)
	}
	if a != nil && (a.Value != b.Value || !eqMap(a.Params, b.Params)) {
		return fmt.Sprintf("disposition %+v vs %+v", a, b)
	}
	return ""
}

func eqEnv(a, b *imap.Envelope) string {
	if a == nil || b == nil {
		if a != b {
			return "envelope nil-ness"
		}
		return ""
	}
	if !a.Date.Equal(b.Date) || a.Subject != b.Subject || a.MessageID != b.MessageID || !eqStrs(a.InReplyTo, b.InReplyTo) {
		return fmt.Sprintf("envelope %+v vs %+v", a, b)
	}
	for _, p := range [][2][]imap.Address{{a.From, b.From}, {a.Sender, b.Sender}, {a.ReplyTo, b.ReplyTo}, {a.To, b.To}, {a.Cc, b.Cc}, {a.Bcc, b.Bcc}} {
		if len(p[0]) != len(p[1]) || (len(p[0]) > 0 && !reflect.DeepEqual(p[0], p[1])) {
			return fmt.Sprintf("addresses %+v vs %+v", p[0], p[1])
		}
	}
	return ""
}

// diffBS returns "" when a and b are the same body structure (nil and empty
// maps and slices count as equal: both are NIL on the wire).
func diffBS(a, b imap.BodyStructure) string {
	switch x := a.(type) {
	case *imap.BodyStructureMultiPart:
		y, ok := b.(*imap.BodyStructureMultiPart)
		if !ok {
			return fmt.Sprintf("%T vs %T", a, b)
		}
		if x.Subtype != y.Subtype || len(x.Children) != len(y.Children) {
			return fmt.Sprintf("multipart %q/%d vs %q/%d", x.Subtype, len(x.Children), y.Subtype, len(y.Children))
		}
		for i := range x.Children {
			if d := diffBS(x.Children[i], y.Children[i]); d != "" {
				return fmt.Sprintf("child %d: %s", i+1, d)
			}
		}
		if (x.Extended == nil) != (y.Extended == nil) {
			return "multipart extension nil-ness"
		}
		if x.Extended != nil {
			if !eqMap(x.Extended.Params, y.Extended.Params) || x.Extended.Location != y.Extended.Location ||
				!eqStrs(x.Extended.Language, y.Extended.Language) {
				return fmt.Sprintf("multipart ext %+v vs %+v", x.Extended, y.Extended)
			}
			if d := eqDisp(x.Extended.Disposition, y.Extended.Disposition); d != "" {
				return d
			}
		}
	case *imap.BodyStructureSinglePart:
		y, ok := b.(*imap.BodyStructureSinglePart)
		if !ok {
			return fmt.Sprintf("%T vs %T", a, b)
		}
		if x.Type != y.Type || x.Subtype != y.Subtype || !eqMap(x.Params, y.Params) || x.ID != y.ID ||
			x.Description != y.Description || x.Encoding != y.Encoding || x.Size != y.Size {
			return fmt.Sprintf("single part %+v vs %+v", *x, *y)
		}
		if (x.Text == nil) != (y.Text == nil) || (x.Text != nil && *x.Text != *y.Text) {
			return fmt.Sprintf("text lines %+v vs %+v", x.Text, y.Text)
		}
		if (x.MessageRFC822 == nil) != (y.MessageRFC822 == nil) {
			return "message/rfc822 nil-ness"
		}
		if x.MessageRFC822 != nil {
			if x.MessageRFC822.NumLines != y.MessageRFC822.NumLines {
				return fmt.Sprintf("rfc822 lines %d vs %d", x.MessageRFC822.NumLines, y.MessageRFC822.NumLines)
			}
			if d := eqEnv(x.MessageRFC822.Envelope, y.MessageRFC822.Envelope); d != "" {
				return d
			}
			if d := diffBS(x.MessageRFC822.BodyStructure, y.MessageRFC822.BodyStructure); d != "" {
				return "embedded: " + d
			}
		}
		if (x.Extended == nil) != (y.Extended == nil) {
			return "single part extension nil-ness"
		}
		if x.Extended != nil {
			if x.Extended.Location != y.Extended.Location || !eqStrs(x.Extended.Language, y.Extended.Language) {
				return fmt.Sprintf("single ext %+v vs %+v", x.Extended, y.Extended)
			}
			if d := eqDisp(x.Extended.Disposition, y.Extended.Disposition); d != "" {
				return d
			}
		}
	default:
		return fmt.Sprintf("unknown type %T", a)
	}
	return ""
}

// paths lists the section paths worth asking for, from go-imap's structure,
// plus a few that do not exist.
func paths(bs imap.BodyStructure, prefix []int) [][]int {
	out := [][]int{append([]int(nil), prefix...)}
	switch x := bs.(type) {
	case *imap.BodyStructureMultiPart:
		for i, c := range x.Children {
			out = append(out, paths(c, append(append([]int(nil), prefix...), i+1))...)
		}
	case *imap.BodyStructureSinglePart:
		if x.MessageRFC822 != nil {
			switch inner := x.MessageRFC822.BodyStructure.(type) {
			case *imap.BodyStructureMultiPart:
				for i, c := range inner.Children {
					out = append(out, paths(c, append(append([]int(nil), prefix...), i+1))...)
				}
			default:
				out = append(out, append(append([]int(nil), prefix...), 1))
			}
		}
	}
	return out
}

func items(path []int) []*imap.FetchItemBodySection {
	all := []*imap.FetchItemBodySection{
		{Part: path},
		{Part: path, Specifier: imap.PartSpecifierHeader},
		{Part: path, Specifier: imap.PartSpecifierText},
		{Part: path, Specifier: imap.PartSpecifierMIME},
		{Part: path, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "content-type"}},
		{Part: path, Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{"Subject", "To"}},
		{Part: path, Partial: &imap.SectionPartial{Offset: 3, Size: 17}},
	}
	return all
}

func describe(it *imap.FetchItemBodySection) string {
	return fmt.Sprintf("part=%v spec=%q fields=%v not=%v partial=%v", it.Part, it.Specifier, it.HeaderFields, it.HeaderFieldsNot, it.Partial)
}

func TestDifferentialAgainstGoIMAP(t *testing.T) {
	const perStyle = 300
	for _, lf := range []bool{false, true} {
		for seed := int64(1); seed <= perStyle; seed++ {
			g := newGen(seed*7919, lf)
			raw := g.message(0)
			ix := Build(raw)
			if ix.Flat {
				t.Fatalf("seed %d lf=%v: a normal message was flattened:\n%s", seed, lf, raw)
			}
			want := imapserver.ExtractBodyStructure(bytes.NewReader(raw))
			if d := diffBS(want, ix.BodyStructure()); d != "" {
				t.Fatalf("seed %d lf=%v: BODYSTRUCTURE differs: %s\n--- message:\n%s", seed, lf, d, raw)
			}
			// The stored form must render the same.
			dec, err := Decode(ix.Encode())
			if err != nil || !dec.Check(len(raw)) {
				t.Fatalf("seed %d: decode: %v", seed, err)
			}
			if d := diffBS(want, dec.BodyStructure()); d != "" {
				t.Fatalf("seed %d lf=%v: decoded BODYSTRUCTURE differs: %s", seed, lf, d)
			}
			th, _ := textproto.ReadHeader(bufioReader(raw))
			if d := eqEnv(imapserver.ExtractEnvelope(th), ix.Env.IMAP()); d != "" {
				t.Fatalf("seed %d: ENVELOPE differs: %s", seed, d)
			}
			for _, p := range paths(want, nil) {
				for _, it := range items(p) {
					exp := imapserver.ExtractBodySection(bytes.NewReader(raw), it)
					got := Section(raw, dec, it)
					// Original bytes instead of a re-serialised header: for LF-only mail
					// compare the header-bearing sections to the raw slices instead.
					if lf && (it.Specifier == imap.PartSpecifierHeader || it.Specifier == imap.PartSpecifierMIME ||
						(it.Specifier == imap.PartSpecifierNone && len(it.Part) == 0)) && len(it.HeaderFields) == 0 && len(it.HeaderFieldsNot) == 0 {
						continue
					}
					if exp == nil && got != nil && g.unclosed {
						continue // go-imap drops the data of a part inside an unclosed multipart
					}
					if !bytes.Equal(exp, got) {
						t.Fatalf("seed %d lf=%v: %s\n got  %q\n want %q\n--- message:\n%s", seed, lf, describe(it), got, exp, raw)
					}
				}
			}
			// Parts that do not exist.
			for _, p := range [][]int{{9}, {1, 9}, {2, 2, 2, 2}, {0}} {
				it := &imap.FetchItemBodySection{Part: p}
				exp := imapserver.ExtractBodySection(bytes.NewReader(raw), it)
				if got := Section(raw, dec, it); !bytes.Equal(exp, got) && !(exp == nil && g.unclosed) {
					t.Fatalf("seed %d: missing part %v: got %q want %q", seed, p, got, exp)
				}
			}
		}
	}
}

func TestSectionsReturnTheOriginalBytes(t *testing.T) {
	for _, lf := range []bool{false, true} {
		g := newGen(99, lf)
		raw := g.message(0)
		ix := Build(raw)
		he := headerLen(raw)
		if got := Section(raw, ix, &imap.FetchItemBodySection{}); !bytes.Equal(got, raw) {
			t.Errorf("lf=%v: BODY[] is not the message", lf)
		}
		if got := Section(raw, ix, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader}); !bytes.Equal(got, raw[:he]) {
			t.Errorf("lf=%v: BODY[HEADER] is not the header block", lf)
		}
		if got := Section(raw, ix, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText}); !bytes.Equal(got, raw[he:]) {
			t.Errorf("lf=%v: BODY[TEXT] is not the body", lf)
		}
		// Header only: the stored header block is enough for BODY[HEADER...].
		hdrOnly := Section(raw[:he], nil, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject"}})
		if !bytes.HasPrefix(hdrOnly, []byte("Subject:")) {
			t.Errorf("lf=%v: HEADER.FIELDS from the header block alone: %q", lf, hdrOnly)
		}
	}
}

func TestStoredIndexRendersLikeTheFreshOne(t *testing.T) {
	for seed := int64(1); seed <= 100; seed++ {
		raw := newGen(seed, false).message(0)
		fresh := Build(raw)
		dec, err := Decode(fresh.Encode())
		if err != nil {
			t.Fatal(err)
		}
		if d := diffBS(fresh.BodyStructure(), dec.BodyStructure()); d != "" {
			t.Fatalf("seed %d: %s", seed, d)
		}
		if !fresh.Env.Date.Equal(dec.Env.Date) || fresh.Env.Subject != dec.Env.Subject {
			t.Fatalf("seed %d: envelope changed in storage", seed)
		}
	}
}

func TestDigestPartsDefaultToMessageRFC822(t *testing.T) {
	raw := []byte("Content-Type: multipart/digest; boundary=D\r\n\r\n--D\r\n\r\nSubject: inner\r\n\r\ninner body\r\n--D--\r\n")
	ix := Build(raw)
	mp := ix.BodyStructure().(*imap.BodyStructureMultiPart)
	sp, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || sp.Type != "message" || sp.Subtype != "rfc822" || sp.MessageRFC822 == nil || sp.MessageRFC822.Envelope.Subject != "inner" {
		t.Fatalf("digest child: %+v", mp.Children[0])
	}
	if got := Section(raw, ix, &imap.FetchItemBodySection{Part: []int{1}, Specifier: imap.PartSpecifierText}); string(got) != "inner body" {
		t.Errorf("BODY[1.TEXT] = %q", got)
	}
}

func TestMatchTextFollowsTheOldSearch(t *testing.T) {
	raw := []byte("Subject: =?utf-8?q?caf=C3=A9?= menu\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\nQ2lhbyBtb25kbw==\r\n" + // "Ciao mondo"
		"--B\r\nContent-Type: application/pdf\r\nContent-Description: Quarterly report\r\n\r\nsecret-binary-needle\r\n--B--\r\n")
	ix := Build(raw)
	cases := []struct {
		pat    string
		header bool
		want   bool
	}{
		{"ciao", false, true},     // decoded base64 text part
		{"mondo", true, true},     // TEXT also finds body text
		{"café", true, true},      // RFC 2047 decoded header
		{"café", false, false},    // BODY does not search headers
		{"quarterly", true, true}, // headers of every part
		{"quarterly", false, false},
		{"secret-binary-needle", true, false}, // binary part bodies are not searched
		{"absent", true, false},
	}
	for _, c := range cases {
		if got := MatchText(raw, ix, c.pat, c.header); got != c.want {
			t.Errorf("MatchText(%q, header=%v) = %v, want %v", c.pat, c.header, got, c.want)
		}
	}
}

func TestCheckRejectsDamagedIndexes(t *testing.T) {
	raw := newGen(5, false).message(0)
	ix := Build(raw)
	if !ix.Check(len(raw)) {
		t.Fatal("a fresh index must check out")
	}
	if ix.Check(len(raw) - 5) {
		t.Error("an index for a longer message must not check out against a shorter one")
	}
	bad := Build(raw)
	bad.Root.BE = len(raw) + 10
	if bad.Check(len(raw)) {
		t.Error("offsets beyond the message must be rejected")
	}
	bad = Build(raw)
	bad.Root.HE = bad.Root.BE + 1
	if bad.Check(len(raw)) {
		t.Error("inverted offsets must be rejected")
	}
	// A damaged stored index never panics a section request: it is rebuilt.
	bad = Build(raw)
	bad.Root.BE = len(raw) + 10
	_ = Section(raw, bad, &imap.FetchItemBodySection{Part: []int{1}})
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Error("garbage decoded")
	}
	if _, err := Decode([]byte(`{"v":99}`)); err == nil {
		t.Error("an unknown version decoded")
	}
}
