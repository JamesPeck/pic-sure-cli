package netproxy

import (
	"encoding/binary"
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

// labelRE is one label of a lower-case host or domain name: letters,
// digits, - and _, with no - at either end.
var labelRE = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?$`)

// validHostName reports whether s, in lower case, is a host or domain name.
// A last label of only digits is refused, because the name is then most
// likely a mistyped IP address such as 10.1.2.300.
func validHostName(s string) bool {
	if len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if len(l) > 63 || !labelRE.MatchString(l) {
			return false
		}
	}
	_, err := strconv.Atoi(labels[len(labels)-1])
	return err != nil
}

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
	if !validHostName(e.domain) {
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

// nonProxyHosts is the no-proxy list as a JVM's or Maven's nonProxyHosts:
// "|"-separated patterns whose only wildcard is a leading or trailing "*".
// Ports are dropped, so an entry with a port covers every port of its
// host. An IPv4 range becomes prefix patterns. IPv6 ranges can't be
// written and are left out.
//
// Maven gets no IPv6 entries: it makes each pattern a regular expression
// with only "." escaped, so brackets would be a character class, and it
// can't parse an IPv6 host from a repository URL anyway.
func (p *Proxy) nonProxyHosts(maven bool) string {
	var out []string
	seen := map[string]bool{}
	add := func(patterns ...string) {
		for _, pattern := range patterns {
			if !seen[pattern] {
				seen[pattern] = true
				out = append(out, pattern)
			}
		}
	}
	for _, e := range p.noProxy {
		switch {
		case e.all:
			add("*")
		case e.cidr != nil:
			add(ipv4Patterns(e.cidr)...)
		case e.ip != nil && e.ip.To4() == nil:
			// The JVM matches an IPv6 host in brackets, as in its default
			// nonProxyHosts.
			if !maven {
				add("[" + e.ip.String() + "]")
			}
		case e.ip != nil:
			add(e.ip.String())
		case e.subOnly:
			add("*." + e.domain)
		default:
			add(e.domain, "*."+e.domain)
		}
	}
	// Setting nonProxyHosts replaces the JVM's default list, which sends
	// all of loopback direct, as ProxyURL does.
	add("127.*")
	if !maven {
		add("[::1]")
	}
	return strings.Join(out, "|")
}

// ipv4Patterns writes an IPv4 range as nonProxyHosts patterns, one for each
// block at the next octet boundary: 10.0.0.0/8 is 10.*, 172.16.0.0/12 is
// 172.16.* to 172.31.*, and a range of /25 or longer is its addresses. It
// returns nil for an IPv6 range.
func ipv4Patterns(n *net.IPNet) []string {
	ones, bits := n.Mask.Size()
	ip := n.IP.To4()
	if ip == nil || bits != 32 {
		return nil
	}
	octets := max(1, (ones+7)/8) // the octets each pattern spells out
	step := uint64(1) << (32 - 8*octets)
	base := uint64(binary.BigEndian.Uint32(ip))
	var out []string
	for i := range uint64(1) << (8*octets - ones) {
		a := base + i*step
		parts := make([]string, octets)
		for j := range parts {
			parts[j] = strconv.FormatUint(a>>(24-8*j)&0xff, 10)
		}
		pattern := strings.Join(parts, ".")
		if octets < 4 {
			pattern += ".*"
		}
		out = append(out, pattern)
	}
	return out
}
