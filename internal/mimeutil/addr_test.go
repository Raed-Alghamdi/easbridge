package mimeutil

import (
	"strconv"
	"strings"
	"testing"
)

func TestHeaderAddressesTakesFromToAndCcOnly(t *testing.T) {
	h := []byte("From: =?UTF-8?B?2LnZhdixINin2YTYs9mE2YXZig==?= <Omar@Example.com>\r\nTo: a@example.com, \"Last, First\" <B@Example.com>\r\n" +
		"To: c@example.com\r\nCc: d@example.com\r\nBcc: hidden@example.com\r\nReply-To: r@example.com\r\nSubject: x\r\n\r\n")
	var got []string
	for _, a := range HeaderAddresses(h) {
		got = append(got, a.Name+"|"+a.Address)
	}
	want := []string{"عمر السلمي|omar@example.com", "|a@example.com", "Last, First|b@example.com", "|c@example.com", "|d@example.com"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestHeaderAddressesSurvivesGarbageAndIsBounded(t *testing.T) {
	if got := HeaderAddresses([]byte("From: <<<>>>\r\nTo: not an address\r\n\r\n")); len(got) != 0 {
		t.Errorf("garbage gave %v", got)
	}
	var b strings.Builder
	b.WriteString("To: ")
	for i := 0; i < 1000; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("u" + strconv.Itoa(i) + "@example.com")
	}
	if got := HeaderAddresses([]byte(b.String() + "\r\n\r\n")); len(got) != maxCandidateAddrs {
		t.Errorf("got %d addresses, want at most %d", len(got), maxCandidateAddrs)
	}
}
