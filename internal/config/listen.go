package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// Default ports of the three listeners (v0.3 install defaults, docs/DESIGN-v0.3.md).
const (
	DefaultIMAPPort    = "993"
	DefaultSMTPPort    = "465"
	DefaultCardDAVPort = "8443"
)

// Iface is one network interface of the namespace the bridge runs in.
type Iface struct {
	Name     string
	Loopback bool
	Addrs    []netip.Addr
}

// InterfaceLister lists the interfaces of the namespace. Injectable, so that the checks are
// tested with a plain container, a host and everything between.
type InterfaceLister func() ([]Iface, error)

// SystemInterfaces lists the real interfaces.
func SystemInterfaces() ([]Iface, error) {
	nis, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, ni := range nis {
		f := Iface{Name: ni.Name, Loopback: ni.Flags&net.FlagLoopback != 0}
		addrs, _ := ni.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if p, ok := netip.AddrFromSlice(ip); ok {
				f.Addrs = append(f.Addrs, p.Unmap())
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// containerIface is the heuristic of the wildcard guard: inside a container (Docker, Podman
// with a network, Kubernetes) every interface besides the loopback is a veth end named eth0,
// eth1 ... One that is not (wlan0, enp3s0, eno1, docker0, br-..., veth..., tailscale0, wg0)
// means the namespace is the host's, or shares the host's networks.
var containerIface = regexp.MustCompile(`^eth[0-9]+$`)

// hostLike returns the names of the interfaces that make a namespace look like a host's:
// every non-loopback interface whose name is not eth<N>.
func hostLike(ifaces []Iface) []string {
	var out []string
	for _, f := range ifaces {
		if f.Loopback || f.Name == "lo" {
			continue
		}
		if !containerIface.MatchString(f.Name) {
			out = append(out, f.Name)
		}
	}
	sort.Strings(out)
	return out
}

func describeIfaces(ifaces []Iface) string {
	var parts []string
	for _, f := range ifaces {
		var as []string
		for _, a := range f.Addrs {
			as = append(as, a.String())
		}
		if len(as) == 0 {
			as = []string{"no address"}
		}
		parts = append(parts, f.Name+" "+strings.Join(as, ", "))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

// parseListenIP reads EASBRIDGE_LISTEN_IP: an IP literal (v4 or v6), no port, no zone, not the
// unspecified address (to listen on every address, leave the variable unset).
func parseListenIP(v string) (string, error) {
	ip, err := netip.ParseAddr(v)
	if err != nil || ip.Zone() != "" {
		return "", fmt.Errorf("want an IP address (v4 or v6) without a port or a host name, got %q", v)
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		return "", fmt.Errorf("%q is the unspecified address; to listen on every address of the container leave the variable unset", v)
	}
	return ip.String(), nil
}

// usesDerivedListener reports whether at least one of the three listeners took its address from
// the defaults (the port numbers and EASBRIDGE_LISTEN_IP) and not from its own variable.
func (c *Config) usesDerivedListener() bool {
	return c.imapDerived || c.smtpDerived || c.carddavDerived
}

// WildcardListeners reports whether a listener binds every address of the namespace because of
// the defaults (no EASBRIDGE_LISTEN_IP): the case the host-network guard is about.
func (c *Config) WildcardListeners() bool { return c.ListenIP == "" && c.usesDerivedListener() }

// CheckListenNetwork runs the two checks that need the interfaces of the namespace:
//
//   - EASBRIDGE_LISTEN_IP must be an address of one of them (when a listener uses it);
//   - with the wildcard default, the namespace must look like a container's (hostLike), unless
//     EASBRIDGE_ALLOW_ANY_BIND is set.
//
// Listeners with their own EASBRIDGE_*_ADDR are not checked here (their validation is the one
// of 0.2.2, which never looked at the interfaces).
func (c *Config) CheckListenNetwork(list InterfaceLister) error {
	if !c.usesDerivedListener() {
		return nil
	}
	if c.ListenIP == "" && c.AllowAnyBind {
		return nil
	}
	if list == nil {
		list = SystemInterfaces
	}
	ifaces, err := list()
	if err != nil {
		return fmt.Errorf("cannot list the network interfaces to check where to listen: %w", err)
	}
	if c.ListenIP != "" {
		want, _ := netip.ParseAddr(c.ListenIP)
		for _, f := range ifaces {
			for _, a := range f.Addrs {
				if a == want {
					return nil
				}
			}
		}
		return fmt.Errorf("%s=%s is not an address of this container; the addresses here are: %s", EnvListenIP, c.ListenIP, describeIfaces(ifaces))
	}
	if bad := hostLike(ifaces); len(bad) > 0 {
		return fmt.Errorf("this network namespace does not look like a container's (interface %s; a container has lo and eth0, eth1 ...), "+
			"so listening on every address would expose the bridge to the host's networks. Set %s to the IP address the bridge should listen on, "+
			"or give the addresses of the listeners (%s, %s, %s), or set %s=true if listening on every address here is what you want",
			strings.Join(bad, ", "), EnvListenIP, EnvIMAPAddr, EnvSMTPAddr, EnvCardDAVAddr, EnvAllowAnyBind)
	}
	return nil
}

// ListenMode is the sentence the start log gives for where the bridge listens.
func (c *Config) ListenMode() string {
	switch {
	case !c.usesDerivedListener():
		return "listening on the addresses given in " + EnvIMAPAddr + ", " + EnvSMTPAddr + " and " + EnvCardDAVAddr
	case c.ListenIP != "":
		return "listening on " + c.ListenIP + " only (" + EnvListenIP + ")"
	case c.AllowAnyBind:
		return "listening on all addresses of this namespace (set " + EnvListenIP + " to restrict; " + EnvAllowAnyBind + " is set, so the namespace was not checked)"
	}
	return "listening on all container addresses (set " + EnvListenIP + " to restrict)"
}
