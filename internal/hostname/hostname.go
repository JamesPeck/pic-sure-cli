// Package hostname is the one rule for the host names pic-sure accepts:
// network.hostname, auth.auth0.tenant, db.remote.host, a proxy's host and
// the no-proxy list's names.
package hostname

import (
	"fmt"
	"net"
	"strings"
)

// Check returns nil if s is an IP address or a valid name (ValidName), and
// otherwise an error saying why not.
func Check(s string) error {
	if net.ParseIP(s) != nil {
		return nil
	}
	if !ValidName(s) {
		if numericLast(strings.ToLower(s)) {
			return fmt.Errorf("%q isn't an IP address, and a host name can't end in a numeric label, which browsers read as part of an IPv4 address", s)
		}
		return fmt.Errorf("want a host name or IP address, got %q", s)
	}
	return nil
}

// ValidName reports whether s is a host or domain name, in any case:
// dot-separated labels of 1 to 63 letters, digits and hyphens, none
// starting or ending with a hyphen, 253 bytes at most (RFC 1123). No
// underscores: Java's URI, which the services parse their URLs with, finds
// no host in a name that has one. A name whose
// last label is a number (10.1.2.300, x.0x1f) is refused, because it is most
// likely a mistyped IP address and browsers parse it as an IPv4 address.
func ValidName(s string) bool {
	return validName(s, false)
}

// ValidMatchName is ValidName that also allows underscores in labels. It is
// for names that are only matched against a destination's host, such as
// no_proxy entries, and never connected to or parsed as a URL's host.
func ValidMatchName(s string) bool {
	return validName(s, true)
}

func validName(s string, underscore bool) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	s = strings.ToLower(s)
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && (!underscore || c != '_') {
				return false
			}
		}
	}
	return !numericLast(s)
}

// numericLast reports whether the last label of the lower-case s is a
// decimal or 0x hexadecimal number.
func numericLast(s string) bool {
	last := s[strings.LastIndex(s, ".")+1:]
	if last == "" {
		return false
	}
	if hex, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return strings.Trim(last, "0123456789") == ""
}
