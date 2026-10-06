package netproxy

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// entry is one parsed no-proxy entry. Exactly one of all, cidr, ip and
// domain is set.
type entry struct {
	text    string // its NO_PROXY form
	all     bool   // "*": every host
	cidr    *net.IPNet
	ip      net.IP
	domain  string // without a leading dot
	subOnly bool   // ".example.com" or "*.example.com": subdomains, not the domain
	port    string // only this port; empty for any
}

// domainRE is a host or domain name: dot-separated labels of letters,
// digits, - and _, with no - at either end of a label.
var domainRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*$`)

// ParseNoProxy parses the user's no_proxy setting: comma-separated entries,
// each "*" (every host), a host or domain name (it and its subdomains), a
// domain with a leading "." or "*." (its subdomains only), an IP address or
// a CIDR range. A name or address may end in :port, which limits it to that
// port. It returns the entries in their NO_PROXY form: lower case, with
// "*.example.com" as ".example.com". Errors are phrased to follow the
// config key.
func ParseNoProxy(s string) ([]string, error) {
	entries, err := parseList(s)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.text)
	}
	return out, nil
}

func parseList(s string) ([]entry, error) {
	var out []entry
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		e, err := parseEntry(f)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseEntry(s string) (entry, error) {
	s = strings.ToLower(s)
	if s == "*" {
		return entry{text: s, all: true}, nil
	}
	if _, n, err := net.ParseCIDR(s); err == nil {
		return entry{text: n.String(), cidr: n}, nil
	}
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return entry{}, fmt.Errorf("has an entry with an invalid port: %q", s)
		}
		host, port = h, p
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		host = s[1 : len(s)-1]
	}
	withPort := func(h string) string {
		if port == "" {
			return h
		}
		return net.JoinHostPort(h, port)
	}
	if ip := net.ParseIP(host); ip != nil {
		return entry{text: withPort(ip.String()), ip: ip, port: port}, nil
	}
	e := entry{domain: host, port: port}
	if d, ok := strings.CutPrefix(host, "*."); ok {
		e.domain, e.subOnly = d, true
	} else if d, ok := strings.CutPrefix(host, "."); ok {
		e.domain, e.subOnly = d, true
	}
	if !domainRE.MatchString(e.domain) {
		return entry{}, fmt.Errorf("has an entry that isn't a host, domain, IP address or CIDR range: %q", s)
	}
	e.text = e.domain
	if e.subOnly {
		e.text = "." + e.domain
	}
	e.text = withPort(e.text)
	return e, nil
}

// bypass reports whether u's host is reached directly: localhost, a
// loopback address, or a match on the no-proxy list.
func (p *Proxy) bypass(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	ip := net.ParseIP(host)
	if host == "localhost" || ip != nil && ip.IsLoopback() {
		return true
	}
	for _, e := range p.noProxy {
		if e.matches(host, ip, port) {
			return true
		}
	}
	return false
}

func (e entry) matches(host string, ip net.IP, port string) bool {
	if e.port != "" && e.port != port {
		return false
	}
	switch {
	case e.all:
		return true
	case e.cidr != nil:
		return ip != nil && e.cidr.Contains(ip)
	case e.ip != nil:
		return ip != nil && e.ip.Equal(ip)
	default:
		return (host == e.domain && !e.subOnly) || strings.HasSuffix(host, "."+e.domain)
	}
}

// javaNonProxyHosts is the no-proxy list as the JVM's and Maven's
// nonProxyHosts: "|"-separated patterns whose only wildcard is a leading or
// trailing "*". The ports go, so an entry with a port covers every port of
// its host. IPv4 ranges become prefix patterns (10.0.0.0/8 is 10.*); ranges
// on other than an octet boundary and IPv6 ranges can't be written and are
// left out.
func (p *Proxy) javaNonProxyHosts() string {
	var out []string
	seen := map[string]bool{}
	add := func(pattern string) {
		if !seen[pattern] {
			seen[pattern] = true
			out = append(out, pattern)
		}
	}
	for _, e := range p.noProxy {
		switch {
		case e.all:
			add("*")
		case e.cidr != nil:
			if pattern, ok := ipv4Prefix(e.cidr); ok {
				add(pattern)
			}
		case e.ip != nil && e.ip.To4() == nil:
			// The JVM matches an IPv6 host in brackets, as in its default
			// nonProxyHosts, [::1].
			add("[" + e.ip.String() + "]")
		case e.ip != nil:
			add(e.ip.String())
		case e.subOnly:
			add("*." + e.domain)
		default:
			add(e.domain)
			// A bare single-label name such as a service has no subdomains
			// worth listing.
			if strings.Contains(e.domain, ".") {
				add("*." + e.domain)
			}
		}
	}
	return strings.Join(out, "|")
}

// ipv4Prefix writes an IPv4 range on an octet boundary, /8 to /32, as a
// nonProxyHosts pattern.
func ipv4Prefix(n *net.IPNet) (string, bool) {
	ones, bits := n.Mask.Size()
	ip := n.IP.To4()
	if ip == nil || bits != 32 || ones == 0 || ones%8 != 0 {
		return "", false
	}
	octets := strings.Split(ip.String(), ".")
	if ones == 32 {
		return ip.String(), true
	}
	return strings.Join(octets[:ones/8], ".") + ".*", true
}
