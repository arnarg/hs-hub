package hub

import (
	"strings"
	"testing"
)

func TestValidHostname(t *testing.T) {
	for name, want := range map[string]bool{
		"reader.example.com":             true,
		"a.b-c.example.com":              true,
		"x1.example.com":                 true,
		"":                               false,
		"*.example.com":                  false,
		"under_score.example.com":        false,
		"-lead.example.com":              false,
		"trail-.example.com":             false,
		"double..example.com":            false,
		"UPPER.example.com":              false, // callers normalize first
		strings.Repeat("a", 64) + ".com": false,
		strings.Repeat("a.", 127) + "a":  false, // 255 characters
	} {
		if got := validHostname(name); got != want {
			t.Errorf("validHostname(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	if got := normalizeName(" Reader.Example.COM. "); got != "reader.example.com" {
		t.Fatalf("got %q", got)
	}
}
