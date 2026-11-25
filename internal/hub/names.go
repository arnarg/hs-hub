package hub

import "strings"

// normalizeName lowercases a DNS name and strips a trailing dot.
func normalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

// validHostname reports whether name is a syntactically valid DNS hostname
// (LDH labels, 1-63 characters each, at most 253 characters in total).
// Wildcards are not valid.
func validHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// isSubdomain reports whether name is strictly below domain.
func isSubdomain(name, domain string) bool {
	return strings.HasSuffix(name, "."+domain)
}

// isWithin reports whether name is domain itself or below it.
func isWithin(name, domain string) bool {
	return name == domain || isSubdomain(name, domain)
}

// checkName returns an empty string if name may be published, otherwise a
// short reason why it was rejected. name must already be normalized.
func (c *Config) checkName(name string) string {
	if !validHostname(name) {
		return "invalid hostname"
	}
	if _, ok := c.reserved[name]; ok {
		return "reserved name"
	}
	for _, ex := range c.ExcludeDomains {
		if isWithin(name, ex) {
			return "inside excluded domain " + ex
		}
	}
	for _, d := range c.Domains {
		if isSubdomain(name, d) {
			return ""
		}
	}
	return "not below an allowed domain"
}
