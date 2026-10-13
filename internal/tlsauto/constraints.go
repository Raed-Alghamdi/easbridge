package tlsauto

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// Name constraints of the automatic CA.
//
// MailFlow's backend trusts the CA through NODE_EXTRA_CA_CERTS, and Node applies that to every outbound
// TLS connection the backend makes, not only to the connection to EASBridge. A CA that could certify any
// name would let whoever gets hold of its key (it lives in the data volume) impersonate any site to the
// backend. So the CA is made with critical name constraints: it may certify only the DNS names it was
// created for (easbridge, the host name, the entries of EASBRIDGE_TLS_NAMES) and addresses in the
// private ranges below. OpenSSL and Go both enforce them; a leaf for another name is refused by the
// client whoever signed it.

// permittedIPRanges are the only addresses the CA may certify: loopback, RFC 1918, unique local
// IPv6 (fc00::/7) and link-local.
var permittedIPRanges = []string{
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7",
	"169.254.0.0/16", "fe80::/10",
}

func permittedNets() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range permittedIPRanges {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

func ipPermitted(ip netip.Addr, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip.AsSlice()) {
			return true
		}
	}
	return false
}

// dnsPermitted follows X.509: a constraint "example.com" permits "example.com" and "a.example.com".
func dnsPermitted(name string, domains []string) bool {
	name = strings.ToLower(name)
	for _, d := range domains {
		d = strings.ToLower(d)
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// constraintsOf are the permitted DNS domains and IP networks of a CA certificate. constrained is false
// for a CA made without any (an early build): it certifies anything.
func constraintsOf(ca *x509.Certificate) (domains []string, nets []*net.IPNet, constrained bool) {
	return ca.PermittedDNSDomains, ca.PermittedIPRanges, len(ca.PermittedDNSDomains)+len(ca.PermittedIPRanges) > 0
}

// effective returns the names to put into the server certificate: every required name, and the best
// effort names the CA may certify. A required name outside the constraints is an error that says what to
// do.
func effective(ca *x509.Certificate, n Names, dir string) (Names, error) {
	domains, nets, constrained := constraintsOf(ca)
	if !constrained {
		return Names{DNS: append(append([]string(nil), n.DNS...), n.HostDNS...), IPs: append(append([]netip.Addr(nil), n.IPs...), n.AutoIPs...)}, nil
	}
	var out Names
	var bad []string
	for _, d := range n.DNS {
		if dnsPermitted(d, domains) {
			out.DNS = append(out.DNS, d)
		} else {
			bad = append(bad, d)
		}
	}
	for _, d := range n.HostDNS {
		if dnsPermitted(d, domains) {
			out.DNS = append(out.DNS, d)
		}
	}
	for _, ip := range n.IPs {
		if ipPermitted(ip, nets) {
			out.IPs = append(out.IPs, ip)
		} else {
			bad = append(bad, ip.String())
		}
	}
	for _, ip := range n.AutoIPs {
		if ipPermitted(ip, nets) {
			out.IPs = append(out.IPs, ip)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return Names{}, fmt.Errorf("the server certificate is to name %s, which the automatic CA in %s may not certify (it was made for %s and for private addresses only). "+
			"To use these names, stop EASBridge, remove the directory %s (a new CA is made at the next start) and restart MailFlow's backend so that it trusts the new CA",
			strings.Join(bad, ", "), dir, strings.Join(domains, ", "), dir)
	}
	sort.Strings(out.DNS)
	sort.Slice(out.IPs, func(i, j int) bool { return out.IPs[i].Less(out.IPs[j]) })
	return out, nil
}

// checkNew refuses a first start whose required names no CA made now could certify: the addresses must be
// in the private ranges (a DNS name is always permitted, the CA is made for it).
func checkNew(n Names) error {
	nets := permittedNets()
	var bad []string
	for _, ip := range n.IPs {
		if !ipPermitted(ip, nets) {
			bad = append(bad, ip.String())
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("the automatic certificate can name private addresses only (loopback, 10/8, 172.16/12, 192.168/16, fc00::/7, link-local); %s is outside them. "+
			"Name the host with a DNS name in EASBRIDGE_TLS_NAMES, or provide your own certificate", strings.Join(bad, ", "))
	}
	return nil
}
