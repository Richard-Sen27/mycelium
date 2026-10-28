package egress

import (
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	ok := []struct{ in, want string }{
		{"PyPI.org.", "pypi.org"},
		{" pypi.org ", "pypi.org"},
		{"*.GitHub.com", "*.github.com"},
		{"*.github.com:22", "*.github.com:22"},
		{"Example.COM:8443", "example.com:8443"},
		{"10.0.0.5", "10.0.0.5"},
		{"10.0.0.5:9000", "10.0.0.5:9000"},
		{"::1", "::1"},
		{"[::1]", "::1"},
		{"[::1]:8080", "[::1]:8080"},
		{"::ffff:10.0.0.5", "10.0.0.5"},
		{"1e100.net", "1e100.net"},
		{"cafe.example", "cafe.example"},
		{"a_b-c.example", "a_b-c.example"},
	}
	for _, tt := range ok {
		got, err := ParseRule(tt.in)
		if err != nil || string(got) != tt.want {
			t.Errorf("ParseRule(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
		// The normalised form parses to itself and matches identically.
		again, err := ParseRule(string(got))
		if err != nil || again != got {
			t.Errorf("ParseRule(%q) not idempotent: %q, %v", got, again, err)
		}
	}
	bad := []string{
		"", "*", "*.", "*.com", "*.com:443", "ex*ample.com", "example.com:", "example.com:0",
		"example.com:70000", "example.com:abc", "bad host", "10.0.0.0/8", "[10.0.0.5]",
		"[pypi.org]", "[::1", "[::1]x", "[::1]:", "a..b", "127.1", "2130706433",
		"host.0x1f", "host.123", "*.0.0.1", "0x7f.0x1", "fe80::1::2",
	}
	// Numeric labels that are not the last label are ordinary host names.
	for _, in := range []string{"0x7f.example", "123.example.com", "*.123.com", "1.pool.ntp.org"} {
		if _, err := ParseRule(in); err != nil {
			t.Errorf("ParseRule(%q) = %v, want ok", in, err)
		}
	}
	for _, in := range bad {
		if got, err := ParseRule(in); err == nil {
			t.Errorf("ParseRule(%q) = %q, want an error", in, got)
		}
		if Match([]Rule{Rule(in)}, "example.com", 443) {
			t.Errorf("invalid rule %q matched", in)
		}
	}
}

func TestValidateRules(t *testing.T) {
	if err := ValidateRules([]string{"pypi.org", "*.github.com:22", "[::1]:80"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRules(nil); err != nil {
		t.Fatal(err)
	}
	err := ValidateRules([]string{"pypi.org", "*.com", "bad host"})
	if err == nil {
		t.Fatal("invalid rules accepted")
	}
	for _, s := range []string{`"*.com"`, `"bad host"`} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not name %s", err, s)
		}
	}
}
