package carddav_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"easbridge/internal/carddav"
	"easbridge/internal/store"
)

func TestHostileXMLIsRejected(t *testing.T) {
	h := newHarness(t)
	h.seedBook("c1", "Contacts", store.TypeContacts, map[string]string{"1": "A"})
	billion := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">]><propfind xmlns="DAV:"><prop><displayname>&lol2;</displayname></prop></propfind>`
	for name, body := range map[string]string{
		"a billion laughs":          billion,
		"a DOCTYPE":                 `<!DOCTYPE propfind><propfind xmlns="DAV:"><allprop/></propfind>`,
		"a lower case doctype":      `<!doctype x><propfind xmlns="DAV:"><allprop/></propfind>`,
		"an external entity":        `<?xml version="1.0"?><!DOCTYPE d [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><propfind xmlns="DAV:"><prop><displayname>&xxe;</displayname></prop></propfind>`,
		"an ENTITY after a comment": `<propfind xmlns="DAV:"><!-- <!ENTITY a "b"> --><allprop/></propfind>`,
		"in CDATA":                  `<propfind xmlns="DAV:"><prop><![CDATA[<!DOCTYPE x>]]></prop></propfind>`,
		"another encoding":          `<?xml version="1.0" encoding="UTF-16"?><propfind xmlns="DAV:"><allprop/></propfind>`,
		"two roots":                 `<propfind xmlns="DAV:"><allprop/></propfind><propfind xmlns="DAV:"><allprop/></propfind>`,
		"invalid UTF-8":             "<propfind xmlns=\"DAV:\"><prop>\xff\xfe</prop></propfind>",
		"a wrong root":              `<D:x xmlns:D="DAV:"/>`,
		"no children":               `<propfind xmlns="DAV:"/>`,
		"unterminated":              `<propfind xmlns="DAV:"><prop>`,
	} {
		r := h.propfind("/dav/addressbooks/me/contacts/", "0", body)
		if r.code != 400 {
			t.Errorf("%s: %d", name, r.code)
		}
		if strings.Contains(r.body, "root:") || strings.Contains(r.body, "lollol") {
			t.Errorf("%s: the answer shows expanded content", name)
		}
		r = h.report("/dav/addressbooks/me/contacts/", "1", body)
		if r.code != 400 && r.code != 403 {
			t.Errorf("%s as a REPORT: %d", name, r.code)
		}
	}
}

func TestDeeplyNestedAndHugeDocumentsAreRefused(t *testing.T) {
	h := newHarness(t)
	deep := strings.Repeat("<a>", 100) + strings.Repeat("</a>", 100)
	if r := h.propfind("/dav/addressbooks/me/", "0", `<propfind xmlns="DAV:"><prop>`+deep+`</prop></propfind>`); r.code != 400 {
		t.Errorf("deep nesting: %d", r.code)
	}
	wide := strings.Repeat("<a/>", 6000)
	if r := h.propfind("/dav/addressbooks/me/", "0", `<propfind xmlns="DAV:"><prop>`+wide+`</prop></propfind>`); r.code != 400 {
		t.Errorf("many elements: %d", r.code)
	}
	big := `<propfind xmlns="DAV:"><prop><displayname>` + strings.Repeat("x", 70<<10) + `</displayname></prop></propfind>`
	if r := h.propfind("/dav/addressbooks/me/", "0", big); r.code != 413 {
		t.Errorf("a body over 64 KiB: %d", r.code)
	}
	if r := h.propfind("/dav/addressbooks/me/", "0", `<propfind xmlns="DAV:"><prop>`+strings.Repeat(" ", 60<<10)+`<resourcetype/></prop></propfind>`); r.code != 207 {
		t.Errorf("a body under the limit: %d", r.code)
	}
	// A big body on a method that does not read it is not a way to make the server work.
	if r := h.do("PUT", "/dav/addressbooks/me/contacts/x.vcf", strings.Repeat("x", 1<<20)); r.code != 405 && r.code != 413 {
		t.Errorf("big PUT: %d", r.code)
	}
}

func TestAtMostEightRequestsAtOnce(t *testing.T) {
	var mu sync.Mutex
	inLogin, maxIn := 0, 0
	release := make(chan struct{})
	entered := make(chan struct{}, 64)
	h := newHarness(t)
	h.login = func(context.Context, string, string) (string, error) {
		mu.Lock()
		inLogin++
		if inLogin > maxIn {
			maxIn = inLogin
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		inLogin--
		mu.Unlock()
		return testUser, nil
	}
	codes := make(chan int, 16)
	for i := 0; i < 8; i++ {
		go func() { codes <- h.do("PROPFIND", "/", "", hdr("Depth", "0")).code }()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("eight requests did not get in")
		}
	}
	r := h.do("PROPFIND", "/", "", hdr("Depth", "0"))
	if r.code != 503 || r.header.Get("Retry-After") == "" {
		t.Errorf("the ninth request: %d, Retry-After %q", r.code, r.header.Get("Retry-After"))
	}
	close(release)
	for i := 0; i < 8; i++ {
		if c := <-codes; c != 207 {
			t.Errorf("a request that was admitted: %d", c)
		}
	}
	if maxIn != 8 {
		t.Errorf("%d requests in flight at most", maxIn)
	}
	// And afterwards the server takes requests again.
	if r := h.do("PROPFIND", "/", "", hdr("Depth", "0")); r.code != 207 {
		t.Errorf("after the burst: %d", r.code)
	}
}

func TestServerLimitsAreTheSpecifiedOnes(t *testing.T) {
	s := carddav.New(carddav.Options{Login: nil, DB: func() *store.DB { return nil }})
	hs := carddav.HTTPServerForTest(s)
	if hs.ReadHeaderTimeout != 10*time.Second || hs.ReadTimeout != 30*time.Second || hs.WriteTimeout != 60*time.Second ||
		hs.IdleTimeout != 120*time.Second || hs.MaxHeaderBytes != 16<<10 {
		t.Errorf("timeouts and limits: %+v", hs)
	}
	if len(hs.TLSNextProto) != 0 {
		t.Errorf("HTTP/2 must stay off")
	}
}

func TestHeadersOverTheLimitAreRefused(t *testing.T) {
	h := newHarness(t)
	r := h.do("PROPFIND", "/", "", hdr("Depth", "0"), hdr("X-Big", strings.Repeat("a", 20<<10)))
	if r.code != 431 {
		t.Errorf("a header block over 16 KiB: %d", r.code)
	}
}

func TestSlowClientsAreCutOff(t *testing.T) {
	// ReadHeaderTimeout is 10 s: not worth waiting for here; the value is checked above and
	// net/http enforces it. This test only shows a stalled header read does not hold a slot
	// for ever by closing the connection from the client side.
	h := newHarness(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "PROPFIND / HTTP/1.1\r\nHost: x\r\n")
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	if r := h.do("PROPFIND", "/", "", hdr("Depth", "0")); r.code != 207 {
		t.Errorf("%d", r.code)
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "easbridge"}, DNSNames: []string{"easbridge", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestOverTLS12AndLaterOnlyAndHTTP11(t *testing.T) {
	h := newHarness(t)
	cert := selfSigned(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := carddav.New(carddav.Options{Login: h.srv.LoginForTest(), DB: func() *store.DB { return h.db }, DirectoryEnabled: true})
	go srv.ServeTLS(ln, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
	defer srv.Close()
	pool := x509.NewCertPool()
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	pool.AddCert(leaf)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "easbridge"}, ForceAttemptHTTP2: true}}
	req, _ := http.NewRequest("OPTIONS", "https://"+ln.Addr().String()+"/", nil)
	req.Header.Set("Authorization", basic(testUser, testPass))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 || resp.ProtoMajor != 1 || resp.StatusCode != 200 {
		t.Errorf("TLS %v, HTTP/%d, %d (HTTP/2 stays off even when the client offers it)", resp.TLS, resp.ProtoMajor, resp.StatusCode)
	}
	old := &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}
	if c, err := tls.Dial("tcp", ln.Addr().String(), old); err == nil {
		c.Close()
		t.Errorf("TLS 1.1 was accepted")
	}
}

func TestLogsHoldNeitherCredentialsNorNamesNorPaths(t *testing.T) {
	h := newHarness(t)
	f := h.seedBook("c1", "Secret Clients", store.TypeUserContacts, map[string]string{"1": "Noorhan Alsulami"})
	h.putDirectory("noorhan@corp.example", "Noorhan Alsulami")
	c := h.mustCards(f)[0]
	h.do("GET", "/dav/addressbooks/me/f-"+"x/"+c.UID+".vcf", "")
	h.do("PROPFIND", "/dav/addressbooks/me/", "", hdr("Depth", "1"))
	h.report("/dav/addressbooks/me/directory/", "1", queryAll)
	h.do("PUT", "/dav/addressbooks/me/directory/x.vcf", "x")
	h.do("PROPFIND", "/", "", hdr("Authorization", basic(testUser, "wrong-password-xyz")), hdr("Depth", "0"))
	logs := h.logs.String()
	if logs == "" {
		t.Fatal("nothing was logged at debug level")
	}
	for _, secret := range []string{testPass, "wrong-password-xyz", "pw:with", "Noorhan", "Alsulami", "corp.example", "Secret Clients", c.UID, "alice", "BEGIN:VCARD", "Authorization", "Basic"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log contains %q:\n%s", secret, logs)
		}
	}
}
