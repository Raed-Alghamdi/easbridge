package mimestruct

import (
	"fmt"
	"math/rand"
	"strings"
)

// gen builds random but well-formed MIME messages for the differential tests.
type gen struct {
	r  *rand.Rand
	nl string
	n  int // boundary counter
	// unclosed is set when a multipart was left without its closing delimiter.
	// go-imap loses the data of such parts (io.ErrUnexpectedEOF), this package
	// returns what is there.
	unclosed bool
}

func newGen(seed int64, lf bool) *gen {
	g := &gen{r: rand.New(rand.NewSource(seed)), nl: "\r\n"}
	if lf {
		g.nl = "\n"
	}
	return g
}

func (g *gen) pick(opts ...string) string { return opts[g.r.Intn(len(opts))] }
func (g *gen) chance(p float64) bool      { return g.r.Float64() < p }

// topHeader is the header of a whole message.
func (g *gen) topHeader(extra string) string {
	return "Message-ID: <m" + fmt.Sprint(g.r.Intn(1e6)) + "@gen.test>" + g.nl +
		"From: Alice <alice@example.test>" + g.nl +
		"To: Bob <bob@example.test>, carol@example.test" + g.nl +
		"Subject: sample " + fmt.Sprint(g.r.Intn(1000)) + g.nl +
		"Date: Fri, 09 Oct 2026 12:00:00 +0000" + g.nl +
		"MIME-Version: 1.0" + g.nl + extra
}

// message returns a complete message.
func (g *gen) message(depth int) []byte {
	h, body := g.entity(depth, true)
	return []byte(h + g.nl + body)
}

// entity returns the header lines (without the blank line) and the body.
func (g *gen) entity(depth int, top bool) (header, body string) {
	extra := func(h string) string {
		if top {
			return g.topHeader(h)
		}
		return h
	}
	switch {
	case depth < 4 && g.chance(0.5):
		g.n++
		boundary := fmt.Sprintf("=_b%d_%d", g.n, g.r.Intn(1000))
		sub := g.pick("mixed", "alternative", "related", "signed")
		ct := "Content-Type: multipart/" + sub + "; boundary=\"" + boundary + "\"" + g.nl
		if g.chance(0.3) {
			ct = "Content-Type: multipart/" + sub + ";" + g.nl + "\tboundary=" + boundary + g.nl
		}
		if g.chance(0.2) {
			ct += "Content-Disposition: inline" + g.nl
		}
		var b strings.Builder
		if g.chance(0.4) {
			b.WriteString("This is a multi-part message in MIME format." + g.nl)
		}
		kids := 1 + g.r.Intn(4)
		for i := 0; i < kids; i++ {
			kh, kb := g.entity(depth+1, false)
			b.WriteString("--" + boundary + g.nl)
			b.WriteString(kh + g.nl + kb)
			b.WriteString(g.nl) // the line break that belongs to the next delimiter
		}
		if g.chance(0.9) {
			b.WriteString("--" + boundary + "--" + g.nl)
			if g.chance(0.3) {
				b.WriteString("epilogue text" + g.nl)
			}
		} else {
			g.unclosed = true
		}
		return extra(ct), b.String()
	case depth < 3 && g.chance(0.25):
		ih, ib := g.entity(depth+1, true)
		ct := "Content-Type: message/rfc822" + g.nl
		if g.chance(0.3) {
			ct += "Content-Disposition: attachment; filename=\"fwd.eml\"" + g.nl
		}
		return extra(ct), ih + g.nl + ib
	default:
		var ct, enc, body string
		switch g.r.Intn(6) {
		case 0:
			ct, enc, body = "text/plain; charset=utf-8", "7bit", "Hello world "+fmt.Sprint(g.r.Intn(99))+g.nl+"second line"+g.nl
		case 1:
			ct, enc, body = "text/html; charset=\"iso-8859-1\"", "quoted-printable", "<p>Caf=E9</p>"+g.nl
		case 2:
			ct, enc, body = "application/pdf; name=\"r.pdf\"", "base64", "JVBERi0xLjQKJcfsj6IK"+g.nl+"AAAA"+g.nl
		case 3:
			ct, enc, body = "image/png", "base64", strings.Repeat("iVBORw0KGgo=", 3)+g.nl
		case 4:
			ct, enc, body = "text/plain", "", "" // empty part
		default:
			ct, enc, body = "", "", "no content type header: defaults to text/plain"+g.nl
		}
		var h strings.Builder
		if ct != "" {
			h.WriteString("Content-Type: " + ct + g.nl)
		}
		if enc != "" {
			h.WriteString("Content-Transfer-Encoding: " + enc + g.nl)
		}
		if g.chance(0.3) {
			h.WriteString("Content-ID: <cid" + fmt.Sprint(g.r.Intn(100)) + "@gen>" + g.nl)
		}
		if g.chance(0.3) {
			h.WriteString("Content-Disposition: attachment; filename=\"f" + fmt.Sprint(g.r.Intn(100)) + ".bin\"" + g.nl)
		}
		if g.chance(0.15) {
			h.WriteString("Content-Language: en, de" + g.nl)
		}
		if g.chance(0.1) {
			h.WriteString("Content-Location: http://example.test/x" + g.nl)
		}
		if g.chance(0.1) {
			h.WriteString("Content-Description: a description" + g.nl)
		}
		return extra(h.String()), body
	}
}
