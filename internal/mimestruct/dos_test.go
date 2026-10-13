package mimestruct

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// Regression tests for the BODYSTRUCTURE denial of service found by the security
// review of v0.1.0: go-imap's ExtractBodyStructure copies every leaf and, for
// message/rfc822, the whole remainder again at every nesting level, and recurses
// without a limit. 8,000 nested message/rfc822 parts in 528 KB peaked at 2.8 GB
// of heap, and 30,000 nested multiparts (2.2 MB) kept a CPU busy for two
// minutes. MailFlow retries after 30 s, so one message became a crash loop.

// measure runs f and reports the elapsed time and the bytes allocated.
func measure(f func()) (time.Duration, uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	f()
	d := time.Since(start)
	runtime.ReadMemStats(&after)
	return d, after.TotalAlloc - before.TotalAlloc
}

func nestedRFC822(levels int) []byte {
	var b bytes.Buffer
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&b, "Subject: level %d\r\nFrom: attacker@example.test\r\nContent-Type: message/rfc822\r\n\r\n", i)
	}
	b.WriteString("Subject: innermost\r\n\r\nbody\r\n")
	return b.Bytes()
}

func nestedMultipart(levels int) []byte {
	var b bytes.Buffer
	b.WriteString("Subject: nested\r\n")
	for i := 0; i < levels; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\nbody\r\n")
	return b.Bytes()
}

func TestNestedMessageRFC822IsBounded(t *testing.T) {
	raw := nestedRFC822(8000)
	if len(raw) < 500<<10 {
		t.Fatalf("test message is only %d bytes", len(raw))
	}
	var ix *Index
	d, alloc := measure(func() { ix = Build(raw) })
	t.Logf("8000 nested message/rfc822 (%d KB): %v, %d KB allocated", len(raw)>>10, d, alloc>>10)
	if !ix.Flat {
		t.Error("expected the flat fallback")
	}
	if d > 2*time.Second || alloc > 16<<20 {
		t.Errorf("too much work: %v, %d bytes", d, alloc)
	}
	bs, ok := ix.BodyStructure().(*imap.BodyStructureSinglePart)
	if !ok || bs.Type != "application" || bs.Subtype != "octet-stream" || int(bs.Size) != len(raw)-headerLen(raw) {
		t.Errorf("flat BODYSTRUCTURE: %+v", ix.BodyStructure())
	}
	if ix.Env.Subject != "level 0" {
		t.Errorf("the envelope must still describe the message: %q", ix.Env.Subject)
	}
}

func TestNestedMultipartIsBounded(t *testing.T) {
	raw := nestedMultipart(30000)
	if len(raw) < 1500<<10 {
		t.Fatalf("test message is only %d bytes", len(raw))
	}
	var ix *Index
	d, alloc := measure(func() { ix = Build(raw) })
	t.Logf("30000 nested multiparts (%d KB): %v, %d KB allocated", len(raw)>>10, d, alloc>>10)
	if !ix.Flat {
		t.Error("expected the flat fallback")
	}
	if d > 2*time.Second || alloc > 16<<20 {
		t.Errorf("too much work: %v, %d bytes", d, alloc)
	}
}

func TestEverySectionOfAFlatMessageIsServedQuickly(t *testing.T) {
	raw := nestedMultipart(30000)
	ix := Build(raw)
	start := time.Now()
	body := raw[headerLen(raw):]
	// The opaque part is the whole body, as BODY[1].
	if got := Section(raw, ix, &imap.FetchItemBodySection{Part: []int{1}}); !bytes.Equal(got, body) {
		t.Errorf("BODY[1] of a flat message: %d bytes, want %d", len(got), len(body))
	}
	if got := Section(raw, ix, &imap.FetchItemBodySection{Part: []int{1}, Specifier: imap.PartSpecifierMIME}); !bytes.HasPrefix(got, []byte("Subject: nested")) {
		t.Errorf("BODY[1.MIME]: %q", got)
	}
	for _, p := range [][]int{{2}, {1, 1, 1, 1}, {1, 2}, {0}} {
		_ = Section(raw, ix, &imap.FetchItemBodySection{Part: p})
	}
	if MatchText(raw, ix, "needle", true) {
		t.Error("nothing to find")
	}
	if time.Since(start) > time.Second {
		t.Errorf("sections of a flat message took %v", time.Since(start))
	}
}

func TestDepthLimit(t *testing.T) {
	// n multipart containers around one leaf: the leaf sits at depth n.
	build := func(n int) []byte {
		var b bytes.Buffer
		b.WriteString("Subject: x\r\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"b%d\"\r\n\r\n--b%d\r\n", i, i)
		}
		b.WriteString("Content-Type: text/plain\r\n\r\nleaf\r\n")
		for i := n - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "\r\n--b%d--\r\n", i)
		}
		return b.Bytes()
	}
	if ix := Build(build(MaxDepth)); ix.Flat {
		t.Errorf("%d levels of nesting must be allowed", MaxDepth)
	}
	if ix := Build(build(MaxDepth + 1)); !ix.Flat {
		t.Errorf("%d levels of nesting must be refused", MaxDepth+1)
	}
}

func TestPartLimit(t *testing.T) {
	build := func(parts int) []byte {
		var b bytes.Buffer
		b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
		for i := 0; i < parts; i++ {
			b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\nx\r\n")
		}
		b.WriteString("--B--\r\n")
		return b.Bytes()
	}
	// The root is a part as well.
	if ix := Build(build(MaxParts - 1)); ix.Flat {
		t.Errorf("%d parts in total must be allowed", MaxParts)
	}
	if ix := Build(build(MaxParts)); !ix.Flat {
		t.Errorf("%d parts in total must be refused", MaxParts+1)
	}
	var ix *Index
	raw := build(200000)
	d, alloc := measure(func() { ix = Build(raw) })
	t.Logf("200000 sibling parts (%d KB): %v, %d KB allocated", len(raw)>>10, d, alloc>>10)
	if !ix.Flat || d > 2*time.Second || alloc > 32<<20 {
		t.Errorf("flat=%v %v %d bytes", ix.Flat, d, alloc)
	}
}

func TestHugeHeaderIsFlatButTheEnvelopeSurvives(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("Subject: still readable\r\nFrom: a@example.test\r\n")
	for b.Len() < 3*MaxHeaderBytes {
		b.WriteString("X-Padding: " + strings.Repeat("p", 70) + "\r\n")
	}
	b.WriteString("\r\nbody\r\n")
	raw := b.Bytes()
	ix := Build(raw)
	if !ix.Flat {
		t.Error("a header over the limit must give the flat fallback")
	}
	if ix.Env.Subject != "still readable" || len(ix.Env.From) != 1 {
		t.Errorf("envelope from the capped header: %+v", ix.Env)
	}
	if got := Section(raw, ix, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader}); len(got) != headerLen(raw) {
		t.Errorf("BODY[HEADER] must still be the whole header block: %d of %d bytes", len(got), headerLen(raw))
	}
}

func TestDeepLargeMessageDoesNotMultiplyItsSizeInMemory(t *testing.T) {
	// 32 nested message/rfc822 levels around a 20 MB payload: go-imap would hold
	// a copy of the remainder per level.
	payload := strings.Repeat("0123456789abcdef", 1<<20) // 16 MiB
	var b bytes.Buffer
	for i := 0; i < MaxDepth; i++ {
		b.WriteString("Content-Type: message/rfc822\r\n\r\n")
	}
	b.WriteString("Content-Type: text/plain\r\n\r\n" + payload)
	raw := b.Bytes()
	var ix *Index
	d, alloc := measure(func() { ix = Build(raw) })
	t.Logf("32 nested levels around %d MiB: flat=%v %v, %d KB allocated", len(raw)>>20, ix.Flat, d, alloc>>10)
	if alloc > 8<<20 {
		t.Errorf("allocated %d bytes for a %d byte message", alloc, len(raw))
	}
	if d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestLegitimateDeepForwardIsStillIndexed(t *testing.T) {
	// 20 forwards of forwards around a big payload is unusual but real: it must
	// keep its structure, at a cost far below depth x size.
	payload := strings.Repeat("0123456789abcdef", 1<<20) // 16 MiB
	var b bytes.Buffer
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "Subject: fwd %d\r\nContent-Type: message/rfc822\r\n\r\n", i)
	}
	b.WriteString("Subject: original\r\nContent-Type: text/plain\r\n\r\n" + payload)
	raw := b.Bytes()
	var ix *Index
	d, alloc := measure(func() { ix = Build(raw) })
	t.Logf("20 forwards around %d MiB: flat=%v %v, %d KB allocated", len(raw)>>20, ix.Flat, d, alloc>>10)
	if ix.Flat {
		t.Fatal("a 20-level forward must not be flattened")
	}
	if alloc > 4<<20 || d > 5*time.Second {
		t.Errorf("%v, %d bytes", d, alloc)
	}
	n := ix.Root
	for i := 0; i < 20; i++ {
		if n.Msg == nil {
			t.Fatalf("level %d is not a message/rfc822 part", i)
		}
		n = n.Msg.Root
	}
	if n.Type != "text" || int(n.BE-n.HE) != len(payload) {
		t.Errorf("innermost part: %s/%s, %d bytes", n.Type, n.Sub, n.BE-n.HE)
	}
	// Its innermost text is addressable as BODY[1.1.1...].
	path := make([]int, 21)
	for i := range path {
		path[i] = 1
	}
	if got := Section(raw, ix, &imap.FetchItemBodySection{Part: path}); len(got) != len(payload) {
		t.Errorf("BODY[%v] has %d bytes, want %d", path[:3], len(got), len(payload))
	}
}
