package mimeutil

import (
	"mime"
	"net/mail"
	"strings"
	"testing"
)

func TestHasField(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"present", "To: a@x.test\r\nBcc: b@x.test\r\n\r\nbody", true},
		{"any case", "To: a@x.test\r\nBCC: b@x.test\r\n\r\nbody", true},
		{"empty field", "To: a@x.test\r\nBcc:\r\n\r\nbody", true},
		{"folded", "To: a@x.test\r\nBcc: b@x.test,\r\n c@x.test\r\n\r\nbody", true},
		{"absent", "To: a@x.test\r\n\r\nbody", false},
		{"in the body only", "To: a@x.test\r\n\r\nBcc: b@x.test\r\n", false},
		{"a longer name", "To: a@x.test\r\nX-Bcc: b@x.test\r\n\r\n", false},
		{"inside a continuation line", "To: a@x.test,\r\n Bcc: b@x.test\r\n\r\n", false},
	} {
		if got := HasField([]byte(tc.raw), "bcc"); got != tc.want {
			t.Errorf("%s: HasField = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func bccOf(t *testing.T, raw []byte) []string {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("the result is not a message: %v\n%q", err, raw)
	}
	list, err := msg.Header.AddressList("Bcc")
	if err != nil {
		t.Fatalf("the Bcc header does not parse: %v\n%q", err, raw)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}

func TestAddBccFromProperty(t *testing.T) {
	base := "From: a@x.test\r\nTo: Bob <bob@x.test>\r\nSubject: s\r\n\r\nbody\r\n"
	t.Run("a list with names, as the To element is written", func(t *testing.T) {
		out, ok := AddBccFromProperty([]byte(base), `"Eve Example" <eve@x.test>, frank@x.test`)
		want := "From: a@x.test\r\nTo: Bob <bob@x.test>\r\nSubject: s\r\nBcc: \"Eve Example\" <eve@x.test>, frank@x.test\r\n\r\nbody\r\n"
		if !ok || string(out) != want {
			t.Fatalf("ok=%v\n got %q\nwant %q", ok, out, want)
		}
	})
	t.Run("semicolons, which some servers use", func(t *testing.T) {
		out, ok := AddBccFromProperty([]byte(base), `"Eve" <eve@x.test>; frank@x.test`)
		if !ok {
			t.Fatal("not added")
		}
		if got := bccOf(t, out); strings.Join(got, ",") != "eve@x.test,frank@x.test" {
			t.Fatalf("recipients %v", got)
		}
	})
	t.Run("a comma inside a quoted name", func(t *testing.T) {
		out, _ := AddBccFromProperty([]byte(base), `"Doe, Jane" <jane@x.test>, joe@x.test`)
		if got := bccOf(t, out); strings.Join(got, ",") != "jane@x.test,joe@x.test" {
			t.Fatalf("recipients %v in %q", got, out)
		}
	})
	t.Run("an Arabic name becomes an encoded-word and nothing raw reaches the header", func(t *testing.T) {
		out, ok := AddBccFromProperty([]byte(base), `"محمد العتيبي" <mohammed@x.test>`)
		if !ok {
			t.Fatal("not added")
		}
		hdr, _ := SplitHeader(out)
		for _, c := range hdr {
			if c >= 0x80 {
				t.Fatalf("a non-ASCII byte in the header: %q", hdr)
			}
		}
		msg, _ := mail.ReadMessage(strings.NewReader(string(out)))
		list, err := (&mail.AddressParser{WordDecoder: &mime.WordDecoder{}}).ParseList(msg.Header.Get("Bcc"))
		if err != nil || len(list) != 1 || list[0].Name != "محمد العتيبي" || list[0].Address != "mohammed@x.test" {
			t.Fatalf("the name does not survive: %v %v (%q)", list, err, out)
		}
	})
	t.Run("a message with a Bcc header is left alone", func(t *testing.T) {
		have := "To: bob@x.test\r\nBcc: old@x.test\r\n\r\nbody"
		out, ok := AddBccFromProperty([]byte(have), "eve@x.test")
		if ok || string(out) != have {
			t.Fatalf("ok=%v %q", ok, out)
		}
	})
	t.Run("nothing to add", func(t *testing.T) {
		for _, prop := range []string{"", "   ", "no addresses here", "<>", ",;,"} {
			out, ok := AddBccFromProperty([]byte(base), prop)
			if ok || string(out) != base {
				t.Errorf("property %q: ok=%v %q", prop, ok, out)
			}
		}
	})
	t.Run("addresses are taken from text net/mail cannot read", func(t *testing.T) {
		out, ok := AddBccFromProperty([]byte(base), `eve@x.test and <frank@x.test> (the boss) plus nobody`)
		if !ok {
			t.Fatal("not added")
		}
		if got := bccOf(t, out); strings.Join(got, ",") != "eve@x.test,frank@x.test" {
			t.Fatalf("recipients %v", got)
		}
	})
	t.Run("a repeated address is written once", func(t *testing.T) {
		out, _ := AddBccFromProperty([]byte(base), `eve@x.test, "Eve" <EVE@x.test>, frank@x.test`)
		if got := bccOf(t, out); len(got) != 2 {
			t.Fatalf("recipients %v", got)
		}
	})
	t.Run("control characters cannot start another header", func(t *testing.T) {
		prop := "\"Eve\r\nX-Evil: yes\" <eve@x.test>, frank@x.test\r\nSubject: hijacked"
		out, ok := AddBccFromProperty([]byte(base), prop)
		if ok {
			hdr, _ := SplitHeader(out)
			if strings.Contains(string(hdr), "\r\nX-Evil") || strings.Count(string(hdr), "Subject:") != 1 {
				t.Fatalf("the property started a header: %q", hdr)
			}
		}
		// Whatever was written, the message still has one header block and the same body.
		if _, body := SplitHeader(out); string(body) != "body\r\n" {
			t.Fatalf("the body changed: %q", body)
		}
	})
	t.Run("a long name is dropped, the address stays", func(t *testing.T) {
		out, _ := AddBccFromProperty([]byte(base), `"`+strings.Repeat("N", 5000)+`" <eve@x.test>`)
		hdr, _ := SplitHeader(out)
		if !strings.Contains(string(hdr), "Bcc: eve@x.test\r\n") {
			t.Fatalf("header %q", hdr)
		}
	})
	t.Run("a long list is folded, not cut", func(t *testing.T) {
		var addrs []string
		for i := 0; i < 400; i++ {
			addrs = append(addrs, "recipient-number-"+strings.Repeat("x", 10)+itoaForTest(i)+"@example.test")
		}
		out, ok := AddBccFromProperty([]byte(base), strings.Join(addrs, ", "))
		if !ok {
			t.Fatal("not added")
		}
		hdr, _ := SplitHeader(out)
		for _, line := range strings.Split(string(hdr), "\r\n") {
			if len(line) > 998 {
				t.Fatalf("a line of %d characters", len(line))
			}
		}
		if got := bccOf(t, out); len(got) != 400 {
			t.Fatalf("%d recipients survive, want 400", len(got))
		}
	})
	t.Run("at most maxBccAddresses are written", func(t *testing.T) {
		var addrs []string
		for i := 0; i < maxBccAddresses+50; i++ {
			addrs = append(addrs, "r"+itoaForTest(i)+"@example.test")
		}
		out, _ := AddBccFromProperty([]byte(base), strings.Join(addrs, ", "))
		if got := bccOf(t, out); len(got) != maxBccAddresses {
			t.Fatalf("%d recipients", len(got))
		}
	})
	t.Run("LF line endings stay LF", func(t *testing.T) {
		out, ok := AddBccFromProperty([]byte("To: bob@x.test\nSubject: s\n\nbody\n"), "eve@x.test")
		if want := "To: bob@x.test\nSubject: s\nBcc: eve@x.test\n\nbody\n"; !ok || string(out) != want {
			t.Fatalf("got %q", out)
		}
	})
	t.Run("the body is never touched", func(t *testing.T) {
		body := "Bcc: not-a-header@x.test\r\n\r\n--\r\n"
		out, _ := AddBccFromProperty([]byte("To: a@x.test\r\n\r\n"+body), "z@x.test")
		if !strings.HasSuffix(string(out), "\r\n\r\n"+body) {
			t.Fatalf("body changed: %q", out)
		}
	})
}

func itoaForTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
