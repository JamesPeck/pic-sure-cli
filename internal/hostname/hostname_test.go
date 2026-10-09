package hostname

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	long := strings.Repeat("a", 63)
	for _, tt := range []struct {
		s  string
		ok bool
	}{
		{"localhost", true},
		{"picsure.example.org", true},
		{"PicSure.Example.ORG", true},
		{"a-b.c", true},
		{"host1", true},
		{"10.0.0.5", true},
		{"::1", true},
		{"2001:db8::1", true},
		{long + ".example", true},
		{strings.Repeat(long+".", 3) + strings.Repeat("a", 61), true}, // 253 bytes

		{"", false},
		{"my_host", false},
		{"_srv.example.org", false},
		{"10.1.2.300", false},
		{"host.123", false},
		{"picsure.0x1f", false},
		{"picsure.0X1F", false},
		{"a..b", false},
		{".example.org", false},
		{"example.org.", false},
		{"-a.example", false},
		{"a-.example", false},
		{"bad host", false},
		{"localhost:8443", false},
		{"[::1]", false},
		{long + "a.example", false},
		{strings.Repeat(long+".", 3) + strings.Repeat("a", 62), false}, // 254 bytes
	} {
		err := Check(tt.s)
		if (err == nil) != tt.ok {
			t.Errorf("Check(%q) = %v, want ok %t", tt.s, err, tt.ok)
		}
	}
	if err := Check("10.1.2.300"); err == nil || !strings.Contains(err.Error(), "numeric label") {
		t.Errorf("Check(10.1.2.300) = %v, want the numeric label explained", err)
	}
	if ValidName("10.0.0.5") {
		t.Error("ValidName accepted an IP address")
	}
}

func TestValidMatchName(t *testing.T) {
	for _, tt := range []struct {
		s  string
		ok bool
	}{
		{"internal_host", true},
		{"corp_example.org", true},
		{"_srv.example.org", true},
		{"a_", true},
		{"picsure.example.org", true},

		{"", false},
		{"a_b-.example", false},
		{"-a_b.example", false},
		{"a..b", false},
		{"bad host", false},
		{"10.1.2.300", false},
		{"a_b.123", false},
		{strings.Repeat("a_", 32) + ".example", false}, // 64-byte label
	} {
		if got := ValidMatchName(tt.s); got != tt.ok {
			t.Errorf("ValidMatchName(%q) = %t, want %t", tt.s, got, tt.ok)
		}
		if tt.ok && strings.Contains(tt.s, "_") && ValidName(tt.s) {
			t.Errorf("ValidName(%q) = true, want false", tt.s)
		}
	}
}
